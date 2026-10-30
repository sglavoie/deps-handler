package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// PRTarget identifies one explicitly selected PR on github.com.
type PRTarget struct {
	Owner  string `json:"owner"`
	Repo   string `json:"repo"`
	Number int    `json:"number"`
}

func (t PRTarget) String() string { return fmt.Sprintf("%s/%s#%d", t.Owner, t.Repo, t.Number) }

var prReference = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9-]*)/([A-Za-z0-9_.-]+)#([1-9][0-9]*)$`)

// ParsePRTarget accepts owner/repo#123 or a github.com pull request URL.
func ParsePRTarget(value string) (PRTarget, error) {
	original := value
	if strings.HasPrefix(value, "https://") {
		u, err := url.Parse(value)
		if err != nil || u.Host != "github.com" || u.User != nil || u.RawQuery != "" {
			return PRTarget{}, fmt.Errorf("invalid GitHub PR URL %q", original)
		}
		parts := strings.Split(strings.TrimSuffix(u.Path, "/"), "/")
		if len(parts) != 5 || parts[3] != "pull" {
			return PRTarget{}, fmt.Errorf("expected https://github.com/owner/repo/pull/123")
		}
		value = parts[1] + "/" + parts[2] + "#" + parts[4]
	}
	m := prReference.FindStringSubmatch(value)
	if m == nil || m[2] == "." || m[2] == ".." || strings.HasSuffix(m[2], ".git") {
		return PRTarget{}, fmt.Errorf("invalid PR %q; use owner/repo#123 or https://github.com/owner/repo/pull/123", original)
	}
	n, err := strconv.Atoi(m[3])
	if err != nil {
		return PRTarget{}, fmt.Errorf("invalid PR number: %w", err)
	}
	return PRTarget{Owner: m[1], Repo: m[2], Number: n}, nil
}

// PRCheck includes diagnostic evidence in addition to the check's outcome.
type PRCheck struct {
	Check
	ID            int64  `json:"id,omitempty"`
	URL           string `json:"url,omitempty"`
	Description   string `json:"description,omitempty"`
	Log           string `json:"log,omitempty"`
	EvidenceError string `json:"evidence_error,omitempty"`
}

// PRSnapshot is a head-pinned view of an explicitly selected PR, regardless
// of author. Incomplete check lists are reported and never considered green.
type PRSnapshot struct {
	Target         PRTarget  `json:"target"`
	Title          string    `json:"title"`
	URL            string    `json:"url"`
	State          string    `json:"state"`
	Draft          bool      `json:"draft"`
	Archived       bool      `json:"archived"`
	Permission     string    `json:"permission"`
	HeadOID        string    `json:"head_oid"`
	HeadRef        string    `json:"head_ref"`
	HeadRepo       string    `json:"head_repo"`
	BaseRef        string    `json:"base_ref"`
	BaseOID        string    `json:"base_oid"`
	Mergeable      string    `json:"mergeable"`
	MergeState     string    `json:"merge_state"`
	ReviewDecision string    `json:"review_decision"`
	ChecksComplete bool      `json:"checks_complete"`
	Checks         []PRCheck `json:"checks"`
}

// PRClient deliberately has no merge operation. Fixes update the selected
// head branch; merging remains a separate user action.
type PRClient interface {
	Snapshot(context.Context, PRTarget) (PRSnapshot, error)
	Evidence(context.Context, PRSnapshot) (*PullRequestDetails, []PRCheck, error)
}

type prClient struct{ *client }

// NewPRClient uses the same credentials and host restrictions as run.
func NewPRClient() (PRClient, error) {
	opts, err := githubComOptions()
	if err != nil {
		return nil, err
	}
	c, err := newClient(opts)
	if err != nil {
		return nil, err
	}
	return &prClient{c}, nil
}

const prQuery = `query($owner: String!, $name: String!, $number: Int!) {
 repository(owner: $owner, name: $name) {
  isArchived viewerPermission
  pullRequest(number: $number) {
   title url state isDraft headRefOid headRefName baseRefOid baseRefName
   headRepository { nameWithOwner }
   mergeable mergeStateStatus reviewDecision
   commits(last: 1) { nodes { commit { oid statusCheckRollup { contexts(first: 100) {
    totalCount nodes {
     __typename
     ... on CheckRun { databaseId name status conclusion detailsUrl
      checkSuite { databaseId app { databaseId } workflowRun { event workflow { databaseId } } }
     }
     ... on StatusContext { context state targetUrl description }
    }
   } } } } }
  }
 }
}`

func (c *prClient) Snapshot(ctx context.Context, target PRTarget) (PRSnapshot, error) {
	var out struct {
		Repository *struct {
			IsArchived       *bool
			ViewerPermission string
			PullRequest      *struct {
				Title, URL, State, HeadRefOid, HeadRefName, BaseRefOid, BaseRefName string
				IsDraft                                                             *bool
				HeadRepository                                                      *struct{ NameWithOwner string }
				Mergeable, MergeStateStatus, ReviewDecision                         string
				Commits                                                             struct {
					Nodes []struct {
						Commit struct {
							Oid               string
							StatusCheckRollup json.RawMessage
						}
					}
				}
			}
		}
	}
	vars := map[string]any{"owner": target.Owner, "name": target.Repo, "number": target.Number}
	if err := c.gql.DoWithContext(ctx, prQuery, vars, &out); err != nil {
		return PRSnapshot{}, fmt.Errorf("%s: %w", target, c.withReset(ctx, err))
	}
	if out.Repository == nil || out.Repository.PullRequest == nil {
		return PRSnapshot{}, fmt.Errorf("%s: pull request not found", target)
	}
	r, p := out.Repository, out.Repository.PullRequest
	if r.IsArchived == nil || p.IsDraft == nil || p.HeadRefOid == "" || p.BaseRefOid == "" || p.State == "" {
		return PRSnapshot{}, fmt.Errorf("%s: incomplete pull request state", target)
	}
	s := PRSnapshot{Target: target, Title: p.Title, URL: p.URL, State: p.State, Draft: *p.IsDraft, Archived: *r.IsArchived, Permission: r.ViewerPermission,
		HeadOID: p.HeadRefOid, HeadRef: p.HeadRefName, BaseRef: p.BaseRefName, BaseOID: p.BaseRefOid,
		Mergeable: p.Mergeable, MergeState: p.MergeStateStatus, ReviewDecision: p.ReviewDecision, Checks: []PRCheck{}}
	if p.HeadRepository != nil {
		s.HeadRepo = p.HeadRepository.NameWithOwner
	}
	if len(p.Commits.Nodes) != 1 || p.Commits.Nodes[0].Commit.Oid != s.HeadOID {
		return s, fmt.Errorf("%s: checks do not identify the PR head", target)
	}
	raw := p.Commits.Nodes[0].Commit.StatusCheckRollup
	if len(raw) == 0 {
		return s, fmt.Errorf("%s: missing check rollup", target)
	}
	var rollup *gqlCheckRollup
	if err := json.Unmarshal(raw, &rollup); err != nil {
		return s, err
	}
	s.ChecksComplete = true
	if rollup == nil {
		return s, nil
	}
	cx := rollup.Contexts
	if cx.TotalCount == nil || cx.Nodes == nil || *cx.TotalCount < len(cx.Nodes) {
		return s, fmt.Errorf("%s: incomplete check rollup", target)
	}
	s.ChecksComplete = *cx.TotalCount == len(cx.Nodes)
	for _, n := range cx.Nodes {
		check := PRCheck{ID: n.DatabaseID, URL: n.DetailsURL, Check: Check{Name: n.Name, State: CheckUnknown}}
		switch n.Typename {
		case "CheckRun":
			check.State = checkRunState(n.Status, n.Conclusion)
			if suite := n.CheckSuite; suite != nil {
				check.Suite = suite.DatabaseID
				if suite.App != nil {
					check.App = suite.App.DatabaseID
				}
				if suite.WorkflowRun != nil {
					check.Workflow = suite.WorkflowRun.Workflow.DatabaseID
					check.Event = suite.WorkflowRun.Event
				}
			}
		case "StatusContext":
			check.Name, check.State, check.Status = n.Context, statusState(n.State), true
			check.URL, check.Description = n.TargetURL, n.Description
		}
		s.Checks = append(s.Checks, check)
	}
	return s, nil
}

const maxEvidenceBytes = 80_000

var actionsJobPath = regexp.MustCompile(`^/([^/]+)/([^/]+)/actions/runs/[0-9]+/job/([0-9]+)/?$`)

// Evidence reads bounded failure logs and annotations. Unavailable logs
// are explicit gaps, so external CI and expired logs still allow diagnosis.
func (c *prClient) Evidence(ctx context.Context, s PRSnapshot) (*PullRequestDetails, []PRCheck, error) {
	d, err := c.PullRequestDetails(ctx, s.Target.Owner, s.Target.Repo, s.Target.Number)
	if err != nil {
		return nil, nil, err
	}
	checks := append([]PRCheck{}, s.Checks...)
	budget := maxEvidenceBytes * 3
	for i := range checks {
		check := &checks[i]
		if check.State != CheckFailed && check.State != CheckStale {
			continue
		}
		if budget <= 0 {
			check.EvidenceError = "failure evidence budget exhausted"
			continue
		}
		if check.ID > 0 && !check.Status {
			path := fmt.Sprintf("repos/%s/%s/check-runs/%d/annotations?per_page=30", s.Target.Owner, s.Target.Repo, check.ID)
			var annotations []struct {
				Path      string
				StartLine int `json:"start_line"`
				Message   string
			}
			if err := c.rest.DoWithContext(ctx, "GET", path, nil, &annotations); err != nil {
				check.EvidenceError = "annotations: " + err.Error()
			} else {
				for _, a := range annotations {
					check.Log += fmt.Sprintf("%s:%d: %s\n", a.Path, a.StartLine, a.Message)
				}
			}
		}
		u, err := url.Parse(check.URL)
		if err == nil && u.Scheme == "https" && u.Host == "github.com" && u.User == nil {
			m := actionsJobPath.FindStringSubmatch(u.Path)
			if m != nil && strings.EqualFold(m[1], s.Target.Owner) && strings.EqualFold(m[2], s.Target.Repo) {
				path := fmt.Sprintf("repos/%s/%s/actions/jobs/%s/logs", s.Target.Owner, s.Target.Repo, m[3])
				resp, err := c.rest.RequestWithContext(ctx, "GET", path, nil)
				if err != nil {
					check.EvidenceError += " logs: " + err.Error()
				} else {
					const maxLogBytes = 4 << 20
					data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxLogBytes+1))
					resp.Body.Close()
					if readErr != nil {
						check.EvidenceError += " logs: " + readErr.Error()
					} else {
						// Error details tend to be at the end of a job log.
						prefix := ""
						if len(data) > maxLogBytes {
							data = data[:maxLogBytes]
							prefix = "[download truncated at 4 MiB; excerpt is not the end of the full log]\n"
						}
						if len(data) > maxEvidenceBytes {
							data = append([]byte("[log tail; truncated]\n"), data[len(data)-maxEvidenceBytes:]...)
						}
						check.Log += prefix + string(data)
					}
				}
			}
		}
		if check.Log == "" && check.EvidenceError == "" {
			check.EvidenceError = "no downloadable GitHub Actions log or annotations; inspect the check URL"
		}
		limit := min(maxEvidenceBytes, budget)
		if len(check.Log) > limit {
			check.Log = check.Log[:limit] + "\n[evidence truncated]"
		}
		budget -= len(check.Log)
	}
	return d, checks, nil
}
