package pr

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/sglavoie/deps-handler/pkg/github"
	"github.com/sglavoie/deps-handler/pkg/review"
)

type RerunOptions struct {
	Apply   bool
	JobID   int64
	HeadOID string
	Attempt int
}

// Rerun records one write per workflow run. JobID is zero when all failed
// jobs in that run are selected; Jobs lists the verified failed jobs.
type Rerun struct {
	RunID            int64            `json:"run_id"`
	JobID            int64            `json:"job_id,omitempty"`
	PreviousAttempt  int              `json:"previous_attempt"`
	ObservedAttempt  int              `json:"observed_attempt"`
	Jobs             []github.PRCheck `json:"jobs"`
	RequestAttempted bool             `json:"request_attempted"`
	Requested        bool             `json:"requested"`
	Status           string           `json:"status,omitempty"`
	Conclusion       string           `json:"conclusion,omitempty"`
	Error            string           `json:"error,omitempty"`
}

type RerunResult struct {
	Snapshot      github.PRSnapshot `json:"snapshot"`
	Apply         bool              `json:"apply"`
	Runs          []Rerun           `json:"runs"`
	Warnings      []string          `json:"warnings"`
	Blockers      []string          `json:"blockers"`
	ChecksPassing bool              `json:"checks_passing"`
	Error         string            `json:"error,omitempty"`
}

type RerunService struct {
	Client       github.ActionsClient
	WaitTimeout  time.Duration
	PollInterval time.Duration
	Progress     func(string)
}

func rerunSnapshot(p github.PRSnapshot) error {
	if p.State != "OPEN" || p.Archived || p.HeadOID == "" || p.HeadRef == "" || p.HeadRepo == "" {
		return errors.New("rerun requires an open PR in an active repository with an existing head branch")
	}
	if !p.ChecksComplete {
		return errors.New("rerun requires a complete check list")
	}
	return nil
}

func (s *RerunService) snapshot(ctx context.Context, target github.PRTarget) (github.PRSnapshot, error) {
	p, err := s.Client.Snapshot(ctx, target)
	if err != nil {
		return p, err
	}
	p.Checks = latestPRChecks(p.Checks)
	return p, rerunSnapshot(p)
}

// Run previews by default. Apply authorizes exactly one rerun per selected
// workflow, including GitHub's dependent jobs. It never calls an AI agent.
func (s *RerunService) Run(ctx context.Context, target github.PRTarget, opts RerunOptions) (result RerunResult, err error) {
	result = RerunResult{Apply: opts.Apply, Runs: []Rerun{}, Warnings: []string{}, Blockers: []string{}}
	defer func() {
		if err != nil {
			result.Error = review.Clean(err.Error())
		}
	}()
	if opts.JobID < 0 || opts.Attempt < 0 || (opts.Attempt > 0 && opts.JobID == 0) || s.WaitTimeout <= 0 || s.PollInterval <= 0 {
		return result, errors.New("rerun needs positive timeouts and IDs; --attempt requires --job")
	}
	p, err := s.snapshot(ctx, target)
	result.Snapshot = p
	if err != nil {
		return result, err
	}
	result.Blockers = Blockers(p)
	result.ChecksPassing, _ = CheckState(p)
	if opts.HeadOID != "" && opts.HeadOID != p.HeadOID {
		return result, fmt.Errorf("PR head changed from %s to %s; diagnose again", opts.HeadOID, p.HeadOID)
	}
	if opts.Apply && p.Permission != "WRITE" && p.Permission != "MAINTAIN" && p.Permission != "ADMIN" {
		return result, errors.New("rerun requires write permission on the PR repository and Actions write access")
	}
	baselines := make(map[int64]github.ActionsRun)
	indices := make(map[int64]int)
	seenJobs := make(map[int64]bool)
	for _, check := range p.Checks {
		if check.State != github.CheckFailed && check.State != github.CheckStale {
			continue
		}
		jobID, _, ok := github.ActionsJobIDs(target, check)
		if !ok {
			result.Warnings = append(result.Warnings, "Cannot rerun "+check.Name+": no identified GitHub Actions job; inspect its check URL.")
			continue
		}
		if opts.JobID > 0 && jobID != opts.JobID {
			continue
		}
		job, run, verifyErr := github.VerifyPRJob(ctx, s.Client, p, check)
		if verifyErr != nil {
			return result, fmt.Errorf("cannot verify %s (job %d): %w", check.Name, jobID, verifyErr)
		}
		if seenJobs[jobID] {
			return result, fmt.Errorf("duplicate Actions job %d in check list", jobID)
		}
		seenJobs[jobID] = true
		if opts.Attempt > 0 && opts.Attempt != run.Attempt {
			return result, fmt.Errorf("run %d attempt changed from %d to %d; preview again", run.ID, opts.Attempt, run.Attempt)
		}
		check.JobID, check.RunID, check.RunAttempt = job.ID, run.ID, run.Attempt
		i, exists := indices[run.ID]
		if !exists {
			i = len(result.Runs)
			indices[run.ID], baselines[run.ID] = i, run
			result.Runs = append(result.Runs, Rerun{RunID: run.ID, JobID: opts.JobID, PreviousAttempt: run.Attempt, ObservedAttempt: run.Attempt, Jobs: []github.PRCheck{}})
		} else if baselines[run.ID] != run {
			return result, fmt.Errorf("run %d changed during preview; preview again", run.ID)
		}
		result.Runs[i].Jobs = append(result.Runs[i].Jobs, check)
	}
	if len(result.Runs) == 0 {
		if opts.JobID > 0 {
			return result, fmt.Errorf("job %d is not a current failed GitHub Actions job on this PR", opts.JobID)
		}
		if opts.Apply {
			return result, errors.New("no eligible failed GitHub Actions jobs to rerun")
		}
		return result, nil
	}
	if !opts.Apply {
		return result, nil
	}
	for i := range result.Runs {
		r := &result.Runs[i]
		// Validate the head and the selected jobs again immediately before each
		// write. GitHub has no conditional rerun endpoint; never retry a POST.
		current, readErr := s.snapshot(ctx, target)
		if readErr == nil && !sameTarget(p, current) {
			readErr = fmt.Errorf("PR changed before rerun: %s; stopped", changedFields(p, current))
		}
		if readErr == nil && current.Permission != p.Permission {
			readErr = errors.New("repository permission changed before rerun; stopped")
		}
		if readErr == nil {
			for _, original := range r.Jobs {
				found := false
				for _, check := range current.Checks {
					if check.ID != original.ID || check.URL != original.URL || check.Name != original.Name {
						continue
					}
					_, run, verifyErr := github.VerifyPRJob(ctx, s.Client, current, check)
					if verifyErr != nil {
						readErr = verifyErr
					} else if run != baselines[r.RunID] {
						readErr = fmt.Errorf("run %d changed before rerun; preview again", r.RunID)
					}
					found = true
					break
				}
				if !found {
					readErr = fmt.Errorf("selected job %d changed before rerun; preview again", original.JobID)
				}
				if readErr != nil {
					break
				}
			}
		}
		if readErr != nil {
			r.Error = review.Clean(readErr.Error())
			return result, readErr
		}
		if s.Progress != nil {
			s.Progress(fmt.Sprintf("Rerunning workflow run %d after attempt %d", r.RunID, r.PreviousAttempt))
		}
		r.RequestAttempted = true
		if r.JobID > 0 {
			err = s.Client.RerunJob(ctx, target, r.JobID)
		} else {
			err = s.Client.RerunFailedJobs(ctx, target, r.RunID)
		}
		if err != nil {
			r.Error = "Rerun request was not confirmed; inspect Actions before retrying: " + review.Clean(err.Error())
			return result, errors.New(r.Error)
		}
		r.Requested = true
		if err = s.wait(ctx, p, baselines[r.RunID], r); err != nil {
			result.ChecksPassing = false
			r.Error = review.Clean(err.Error())
			return result, err
		}
	}
	err = s.confirmChecks(ctx, p, baselines, &result)
	return result, err
}

func (s *RerunService) wait(ctx context.Context, p github.PRSnapshot, baseline github.ActionsRun, r *Rerun) error {
	ctx, cancel := context.WithTimeout(ctx, s.WaitTimeout)
	defer cancel()
	for {
		current, err := s.snapshot(ctx, p.Target)
		if err != nil {
			return err
		}
		if !sameTarget(p, current) {
			return errors.New("PR changed while waiting for rerun; stopped")
		}
		run, err := s.Client.ActionsRun(ctx, p.Target, r.RunID)
		if err != nil {
			return err
		}
		r.ObservedAttempt, r.Status, r.Conclusion = run.Attempt, run.Status, run.Conclusion
		if !sameRunIdentity(run, baseline) {
			return fmt.Errorf("run %d identity changed while waiting; stopped", r.RunID)
		}
		if run.Attempt < baseline.Attempt || run.Attempt > baseline.Attempt+1 {
			return fmt.Errorf("run %d has unexpected attempt %d; another rerun may have occurred", r.RunID, run.Attempt)
		}
		if run.Attempt == baseline.Attempt+1 && run.Status == "completed" {
			if run.Conclusion != "success" {
				return fmt.Errorf("run %d attempt %d finished with %s; run pr diagnose for current evidence", r.RunID, run.Attempt, run.Conclusion)
			}
			return nil
		}
		if err := github.Sleep(ctx, s.PollInterval); err != nil {
			return fmt.Errorf("new attempt for run %d not confirmed within %s: %w", r.RunID, s.WaitTimeout, err)
		}
	}
}

func sameRunIdentity(a, b github.ActionsRun) bool {
	return a.ID == b.ID && a.HeadSHA == b.HeadSHA && a.WorkflowID == b.WorkflowID && a.SuiteID == b.SuiteID && a.Event == b.Event && strings.EqualFold(a.Repository.FullName, b.Repository.FullName)
}

// The run API and PR check rollup can propagate at different times. Allow
// pending checks and the selected jobs' old failures to catch up, while
// rechecking that nobody has started another attempt of a completed rerun.
func (s *RerunService) confirmChecks(ctx context.Context, p github.PRSnapshot, baselines map[int64]github.ActionsRun, result *RerunResult) error {
	ctx, cancel := context.WithTimeout(ctx, s.WaitTimeout)
	defer cancel()
	oldChecks := make(map[int64]bool)
	for _, r := range result.Runs {
		for _, job := range r.Jobs {
			oldChecks[job.ID] = true
		}
	}
	for {
		current, err := s.snapshot(ctx, p.Target)
		if err != nil {
			return err
		}
		result.Snapshot, result.Blockers = current, Blockers(current)
		if !sameTarget(p, current) {
			return errors.New("PR changed after rerun; diagnose the current head")
		}
		for i := range result.Runs {
			r := &result.Runs[i]
			run, err := s.Client.ActionsRun(ctx, p.Target, r.RunID)
			if err != nil {
				return err
			}
			r.ObservedAttempt, r.Status, r.Conclusion = run.Attempt, run.Status, run.Conclusion
			if !sameRunIdentity(run, baselines[r.RunID]) || run.Attempt != r.PreviousAttempt+1 || run.Status != "completed" || run.Conclusion != "success" {
				return fmt.Errorf("run %d changed after verification (attempt %d, %s, %s); inspect Actions", r.RunID, run.Attempt, run.Status, run.Conclusion)
			}
		}
		result.ChecksPassing, _ = CheckState(current)
		if result.ChecksPassing {
			return nil
		}
		for _, check := range current.Checks {
			if check.State == github.CheckPassed || check.State == github.CheckSkipped || check.State == github.CheckPending {
				continue
			}
			if !oldChecks[check.ID] || check.State == github.CheckUnknown {
				return errors.New("rerun completed, but PR checks are not confirmed passing; run pr diagnose for the remaining failures or evidence gaps")
			}
		}
		if err := github.Sleep(ctx, s.PollInterval); err != nil {
			return fmt.Errorf("rerun completed, but PR checks were not confirmed within %s; run pr diagnose: %w", s.WaitTimeout, err)
		}
	}
}

// RerunMarkdown distinguishes a preview, an unconfirmed request and an
// observed new attempt, including when an operation stops partway through.
func RerunMarkdown(w io.Writer, result RerunResult) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# CI rerun: %s\n\nHead: `%s`\n\n", result.Snapshot.Target, result.Snapshot.HeadOID)
	if !result.Apply {
		b.WriteString("Preview only. Add `--apply` to request reruns and watch their new attempts.\n\n")
	}
	b.WriteString("GitHub also reruns dependent jobs. Without `--job`, all failed jobs in each listed workflow run are selected.\n\n")
	if len(result.Runs) == 0 {
		b.WriteString("No eligible failed GitHub Actions jobs.\n\n")
	}
	for _, r := range result.Runs {
		fmt.Fprintf(&b, "## Run %d (previous attempt %d)\n\n", r.RunID, r.PreviousAttempt)
		for _, job := range r.Jobs {
			fmt.Fprintf(&b, "- %s: job %d, check %d\n", job.Name, job.JobID, job.ID)
		}
		if !result.Apply {
			for _, job := range r.Jobs {
				if command := rerunCommand(result.Snapshot, job); command != "" {
					fmt.Fprintf(&b, "\nSelect this job:\n\n```sh\n%s --apply\n```\n", command)
				}
			}
		} else {
			fmt.Fprintf(&b, "\nRequest attempted: %t. Confirmed: %t. Observed attempt: %d. Status: %s. Conclusion: %s.\n", r.RequestAttempted, r.Requested, r.ObservedAttempt, r.Status, r.Conclusion)
		}
		if r.Error != "" {
			fmt.Fprintf(&b, "\nStopped: %s\n", r.Error)
		}
		b.WriteString("\n")
	}
	for _, warning := range result.Warnings {
		fmt.Fprintf(&b, "Warning: %s\n\n", warning)
	}
	if result.Apply {
		fmt.Fprintf(&b, "PR checks confirmed passing: %t. Reviews and other merge gates still apply.\n\n", result.ChecksPassing)
	}
	for _, blocker := range result.Blockers {
		fmt.Fprintf(&b, "- %s\n", blocker)
	}
	if result.Error != "" {
		fmt.Fprintf(&b, "\nOperation stopped: %s\n", result.Error)
	}
	_, err := io.WriteString(w, review.Clean(b.String()))
	return err
}

func rerunCommand(p github.PRSnapshot, check github.PRCheck) string {
	// Snapshot targets and SHAs come from GitHub; validate before emitting a
	// shell command, including for custom library clients.
	if _, err := github.ParsePRTarget(p.Target.String()); err != nil || !shellOID(p.HeadOID) || check.JobID <= 0 || check.RunID <= 0 || check.RunAttempt <= 0 {
		return ""
	}
	return fmt.Sprintf("deps-handler pr rerun %s --job %d --head %s --attempt %d", p.Target, check.JobID, p.HeadOID, check.RunAttempt)
}

func shellOID(oid string) bool {
	if oid == "" {
		return false
	}
	for _, c := range oid {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// rerunSuggestions excludes duplicate names: an agent's name-only subject
// cannot safely select between two producers with the same check name.
func rerunSuggestions(p github.PRSnapshot, evidence []github.PRCheck, a Analysis) []string {
	commands := []string{}
	if rerunSnapshot(p) != nil {
		return commands
	}
	counts := make(map[string]int)
	for _, c := range p.Checks {
		counts[c.Name]++
	}
	seen := make(map[int64]bool)
	for _, r := range a.Recommendations {
		if r.Action != RerunCandidate || counts[r.Subject] != 1 {
			continue
		}
		for _, c := range evidence {
			if c.Name != r.Subject || seen[c.JobID] || (c.State != github.CheckFailed && c.State != github.CheckStale) {
				continue
			}
			current := false
			for _, check := range p.Checks {
				if check.ID == c.ID && check.URL == c.URL && check.Name == c.Name && check.State == c.State {
					current = true
				}
			}
			if !current {
				continue
			}
			if command := rerunCommand(p, c); command != "" {
				commands = append(commands, command)
				seen[c.JobID] = true
			}
		}
	}
	return commands
}
