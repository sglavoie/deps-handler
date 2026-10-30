package github

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// ActionsClient exposes only the reads and explicit reruns needed by pr rerun.
type ActionsClient interface {
	Snapshot(context.Context, PRTarget) (PRSnapshot, error)
	ActionsJob(context.Context, PRTarget, int64) (ActionsJob, error)
	ActionsRun(context.Context, PRTarget, int64) (ActionsRun, error)
	RerunJob(context.Context, PRTarget, int64) error
	RerunFailedJobs(context.Context, PRTarget, int64) error
}

type ActionsJob struct {
	ID         int64  `json:"id"`
	RunID      int64  `json:"run_id"`
	RunAttempt int    `json:"run_attempt"`
	HeadSHA    string `json:"head_sha"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	CheckURL   string `json:"check_run_url"`
}

type ActionsRun struct {
	ID         int64  `json:"id"`
	Attempt    int    `json:"run_attempt"`
	HeadSHA    string `json:"head_sha"`
	SuiteID    int64  `json:"check_suite_id"`
	WorkflowID int64  `json:"workflow_id"`
	Event      string `json:"event"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	StartedAt  string `json:"run_started_at"`
	UpdatedAt  string `json:"updated_at"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

func NewActionsClient() (ActionsClient, error) {
	c, err := NewPRClient()
	if err != nil {
		return nil, err
	}
	return c.(*prClient), nil
}

var actionsIdentityPath = regexp.MustCompile(`^/([^/]+)/([^/]+)/actions/runs/([1-9][0-9]*)/job/([1-9][0-9]*)/?$`)

// ActionsJobIDs reads identity hints from a same-repository URL. A URL alone
// is never sufficient authorization: VerifyPRJob confirms it with the API.
func ActionsJobIDs(target PRTarget, check PRCheck) (jobID, runID int64, ok bool) {
	if check.Status || check.ID <= 0 {
		return 0, 0, false
	}
	u, err := url.Parse(check.URL)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return 0, 0, false
	}
	m := actionsIdentityPath.FindStringSubmatch(u.Path)
	if m == nil || !strings.EqualFold(m[1], target.Owner) || !strings.EqualFold(m[2], target.Repo) {
		return 0, 0, false
	}
	runID, err = strconv.ParseInt(m[3], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	jobID, err = strconv.ParseInt(m[4], 10, 64)
	return jobID, runID, err == nil
}

func (c *prClient) ActionsJob(ctx context.Context, t PRTarget, id int64) (ActionsJob, error) {
	var job ActionsJob
	if id <= 0 {
		return job, fmt.Errorf("invalid Actions job ID")
	}
	err := c.rest.DoWithContext(ctx, "GET", fmt.Sprintf("repos/%s/%s/actions/jobs/%d", t.Owner, t.Repo, id), nil, &job)
	if err != nil {
		return job, c.withReset(ctx, err)
	}
	if job.ID != id || job.RunID <= 0 || job.RunAttempt <= 0 || job.HeadSHA == "" || job.Name == "" || !knownActionsState(job.Status, job.Conclusion) {
		return job, fmt.Errorf("incomplete or unidentified Actions job %d", id)
	}
	return job, nil
}

func (c *prClient) ActionsRun(ctx context.Context, t PRTarget, id int64) (ActionsRun, error) {
	var run ActionsRun
	if id <= 0 {
		return run, fmt.Errorf("invalid Actions run ID")
	}
	err := c.rest.DoWithContext(ctx, "GET", fmt.Sprintf("repos/%s/%s/actions/runs/%d", t.Owner, t.Repo, id), nil, &run)
	if err != nil {
		return run, c.withReset(ctx, err)
	}
	if run.ID != id || run.Attempt <= 0 || run.HeadSHA == "" || run.SuiteID <= 0 || run.WorkflowID <= 0 || run.Event == "" ||
		!strings.EqualFold(run.Repository.FullName, t.Owner+"/"+t.Repo) || !knownActionsState(run.Status, run.Conclusion) {
		return run, fmt.Errorf("incomplete or unidentified Actions run %d", id)
	}
	return run, nil
}

func knownActionsState(status, conclusion string) bool {
	if status != "completed" {
		switch status {
		case "queued", "in_progress", "requested", "waiting", "pending":
			return conclusion == ""
		}
		return false
	}
	switch conclusion {
	case "success", "failure", "neutral", "cancelled", "skipped", "timed_out", "action_required", "stale", "startup_failure":
		return true
	}
	return false
}

func failedActionsConclusion(conclusion string) bool {
	switch conclusion {
	case "failure", "cancelled", "timed_out", "action_required", "stale", "startup_failure":
		return true
	}
	return false
}

// VerifyPRJob ties a failed check to its actual Actions job and latest run
// attempt. Check run IDs and Actions job IDs are deliberately kept separate.
func VerifyPRJob(ctx context.Context, c ActionsClient, p PRSnapshot, check PRCheck) (ActionsJob, ActionsRun, error) {
	jobID, runID, ok := ActionsJobIDs(p.Target, check)
	if !ok || (check.State != CheckFailed && check.State != CheckStale) {
		return ActionsJob{}, ActionsRun{}, fmt.Errorf("check is not an identified failed GitHub Actions job")
	}
	job, err := c.ActionsJob(ctx, p.Target, jobID)
	if err != nil {
		return job, ActionsRun{}, err
	}
	expectedURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/check-runs/%d", p.Target.Owner, p.Target.Repo, check.ID)
	if job.ID != jobID || job.RunID != runID || job.RunAttempt <= 0 || job.HeadSHA != p.HeadOID || job.Name != check.Name ||
		!strings.EqualFold(job.CheckURL, expectedURL) || job.Status != "completed" || !failedActionsConclusion(job.Conclusion) {
		return job, ActionsRun{}, fmt.Errorf("job %d does not identify the failed check at PR head %s", jobID, p.HeadOID)
	}
	run, err := c.ActionsRun(ctx, p.Target, runID)
	if err != nil {
		return job, run, err
	}
	if run.ID != runID || run.HeadSHA != p.HeadOID || run.Attempt != job.RunAttempt || run.Status != "completed" || !failedActionsConclusion(run.Conclusion) ||
		check.Suite <= 0 || run.SuiteID != check.Suite || check.Workflow <= 0 || run.WorkflowID != check.Workflow || check.Event == "" || run.Event != check.Event {
		return job, run, fmt.Errorf("run %d no longer identifies the failed check's completed attempt", runID)
	}
	return job, run, nil
}

func (c *prClient) RerunJob(ctx context.Context, t PRTarget, id int64) error {
	return c.requestRerun(ctx, fmt.Sprintf("repos/%s/%s/actions/jobs/%d/rerun", t.Owner, t.Repo, id), id)
}

func (c *prClient) RerunFailedJobs(ctx context.Context, t PRTarget, id int64) error {
	return c.requestRerun(ctx, fmt.Sprintf("repos/%s/%s/actions/runs/%d/rerun-failed-jobs", t.Owner, t.Repo, id), id)
}

func (c *prClient) requestRerun(ctx context.Context, path string, id int64) error {
	if id <= 0 {
		return fmt.Errorf("invalid Actions ID")
	}
	// Never retry a write: an ambiguous response may already have scheduled CI.
	resp, err := c.rest.RequestWithContext(ctx, "POST", path, nil)
	if err != nil {
		return c.withReset(ctx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		return fmt.Errorf("rerun was not confirmed (HTTP %d); inspect Actions before retrying", resp.StatusCode)
	}
	return nil
}
