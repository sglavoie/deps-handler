// Package github fetches repository and Dependabot pull request state and
// performs the few writes deps-merger makes (merges and comments). It reuses
// the gh CLI's authentication.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
)

// CheckState is the normalised outcome of one check run or status context.
type CheckState string

const (
	CheckPassed  CheckState = "passed"
	CheckPending CheckState = "pending"
	CheckFailed  CheckState = "failed"
	// CheckSkipped is a check run that completed without judging anything:
	// skipped, as a job whose condition was false is, or neutral. It blocks
	// nothing, but neither does it count as a check that ran.
	CheckSkipped CheckState = "skipped"
	// CheckStale is a check run GitHub gave up on after it sat unfinished
	// for 14 days: it will never pass, but says nothing about the update.
	CheckStale CheckState = "stale"
)

type Check struct {
	Name  string     `json:"name"`
	State CheckState `json:"state"`
	// Suite identifies the check suite (one workflow run) a check run
	// belongs to; a newer suite of the same app, workflow and event with
	// the same check name supersedes it, as when concurrency cancels a run
	// in favour of a later one. Zero for status contexts.
	Suite int64 `json:"-"`
	// App and Workflow identify what produced a check run: the GitHub App
	// behind its suite and, for GitHub Actions, the workflow. Two workflows
	// can each have a job called "test"; neither says anything about the
	// other's.
	App      int64 `json:"-"`
	Workflow int64 `json:"-"`
	// Event is what triggered a GitHub Actions workflow run, such as
	// "push" or "pull_request". A workflow listening to both runs twice on
	// the same commit, and a run for one event never supersedes the other.
	Event string `json:"-"`
	// Status is true for a commit status context rather than a check run.
	// GitHub reports only the latest status per context, and a status
	// never stands in for a check run of the same name or the reverse.
	Status bool `json:"-"`
}

type Author struct {
	Login string `json:"login,omitempty"`
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
}

type Commit struct {
	OID           string    `json:"oid"`
	Message       string    `json:"-"`
	CommittedDate time.Time `json:"committed_date"`
	// Authors is the commit author followed by every Co-authored-by
	// trailer. The first is always the author GitHub recorded, empty when
	// it recorded none; only it can carry a Login, since a trailer links no
	// account.
	Authors []Author `json:"authors"`
	// SignedByGitHub is true when the commit carries a valid signature made
	// with GitHub's own key, as Dependabot's commits do.
	SignedByGitHub bool `json:"signed_by_github"`
	// CommittedByGitHub is true when GitHub itself is the committer, as on
	// every commit Dependabot makes. An API call that names its own
	// committer gets no such commit, even when GitHub signs it.
	CommittedByGitHub bool `json:"committed_by_github"`
}

// gitHubCommitter is the committer email GitHub writes on the commits it
// creates, Dependabot's among them.
const gitHubCommitter = "noreply@github.com"

type Comment struct {
	Author    string    `json:"author"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

type PullRequest struct {
	Number         int       `json:"number"`
	Title          string    `json:"title"`
	URL            string    `json:"url"`
	Author         string    `json:"author"`
	IsDraft        bool      `json:"is_draft"`
	CreatedAt      time.Time `json:"created_at"`
	HeadOID        string    `json:"head_oid"`
	BaseRef        string    `json:"base_ref"`
	Mergeable      string    `json:"mergeable"`       // MERGEABLE, CONFLICTING, UNKNOWN
	MergeState     string    `json:"merge_state"`     // CLEAN, BEHIND, BLOCKED, DIRTY, ...
	ReviewDecision string    `json:"review_decision"` // APPROVED, REVIEW_REQUIRED, ...
	CommitCount    int       `json:"commit_count"`
	Commits        []Commit  `json:"commits"`
	Checks         []Check   `json:"checks"`
	// ChecksOID is the commit Checks were read from; checks for any commit
	// but HeadOID say nothing about what would be merged.
	ChecksOID string `json:"checks_oid"`
	// CheckCount is the total number of checks, which can exceed the number
	// fetched into Checks.
	CheckCount int `json:"check_count"`
	// Comments are the latest comments, oldest first; CommentCount is how
	// many the PR has, which can exceed the number fetched.
	Comments     []Comment `json:"-"`
	CommentCount int       `json:"-"`
}

type Repo struct {
	FullName      string        `json:"full_name"`
	Archived      bool          `json:"archived"`
	Permission    string        `json:"permission"` // ADMIN, MAINTAIN, WRITE, TRIAGE, READ
	SquashAllowed bool          `json:"squash_allowed"`
	MergeAllowed  bool          `json:"merge_allowed"`
	RebaseAllowed bool          `json:"rebase_allowed"`
	PullRequests  []PullRequest `json:"pull_requests"`
	// OpenPRCount is how many open Dependabot PRs GitHub reports, which can
	// exceed the number fetched into PullRequests.
	OpenPRCount int `json:"open_pr_count"`
	// SearchCount is how many results the search returned, before closed
	// PRs still in its index were dropped. Only when it falls short of
	// OpenPRCount were some PRs left unread: GitHub counts those closed PRs
	// too.
	SearchCount int `json:"search_count"`
	// FetchedAt is when this state was read, the "now" that the age of
	// commits and comments is measured against.
	FetchedAt time.Time `json:"-"`
}

// CanWrite reports whether the viewer can merge and comment as a collaborator.
func (r *Repo) CanWrite() bool {
	switch r.Permission {
	case "ADMIN", "MAINTAIN", "WRITE":
		return true
	}
	return false
}

// Client is the subset of GitHub operations deps-merger needs.
type Client interface {
	Viewer(ctx context.Context) (string, error)
	FetchRepo(ctx context.Context, owner, name string) (*Repo, error)
	Merge(ctx context.Context, owner, name string, number int, method, headOID string) error
	Comment(ctx context.Context, owner, name string, number int, body string) error
	PullRequestDetails(ctx context.Context, owner, name string, number int) (*PullRequestDetails, error)
	PullRequestState(ctx context.Context, owner, name string, number int) (PRState, error)
}

// PRState is whether a pull request is open, merged, or closed without
// being merged.
type PRState string

const (
	PROpen   PRState = "OPEN"
	PRMerged PRState = "MERGED"
	PRClosed PRState = "CLOSED"
)

// PullRequestDetails is what an agent review reads about a PR beyond what
// FetchRepo returns, fetched only for the PRs it reviews.
type PullRequestDetails struct {
	// Body is the PR description, where Dependabot puts release notes,
	// changelog entries and commits.
	Body string
	// Diff is the PR's unified diff, cut at MaxDiffBytes.
	Diff      string
	Truncated bool
	// DiffError says why Diff is empty, such as GitHub refusing a diff too
	// large to render.
	DiffError string
}

// MaxDiffBytes caps the diff read for a review: lockfile churn can run to
// megabytes and says little.
const MaxDiffBytes = 200_000

type client struct {
	gql  *api.GraphQLClient
	rest *api.RESTClient
	// diff is a REST client asking for diffs rather than JSON.
	diff *api.RESTClient
}

// RequestTimeout bounds each request to GitHub, reading the response
// included. Without it one request that hangs would hold the lock that
// serialises writes, and with it every repository, for good. A merge that
// times out may still have gone through: the error is Transient, so it is
// treated like a 504.
const RequestTimeout = time.Minute

// New builds a client from the gh CLI's stored credentials or GH_TOKEN.
func New() (Client, error) {
	gql, err := api.NewGraphQLClient(api.ClientOptions{Timeout: RequestTimeout})
	if err != nil {
		return nil, fmt.Errorf("github auth (run `gh auth login`): %w", err)
	}
	rest, err := api.NewRESTClient(api.ClientOptions{Timeout: RequestTimeout})
	if err != nil {
		return nil, fmt.Errorf("github auth (run `gh auth login`): %w", err)
	}
	diff, err := api.NewRESTClient(api.ClientOptions{Timeout: RequestTimeout, Headers: map[string]string{"Accept": "application/vnd.github.diff"}})
	if err != nil {
		return nil, fmt.Errorf("github auth (run `gh auth login`): %w", err)
	}
	return &client{gql: gql, rest: rest, diff: diff}, nil
}

func (c *client) Viewer(ctx context.Context) (string, error) {
	var resp struct {
		Viewer struct{ Login string }
	}
	if err := c.gql.DoWithContext(ctx, `query{viewer{login}}`, nil, &resp); err != nil {
		return "", err
	}
	return resp.Viewer.Login, nil
}

const repoQuery = `
query($owner: String!, $name: String!, $search: String!) {
  repository(owner: $owner, name: $name) {
    nameWithOwner
    isArchived
    viewerPermission
    squashMergeAllowed
    mergeCommitAllowed
    rebaseMergeAllowed
  }
  search(query: $search, type: ISSUE, first: 100) {
    issueCount
    nodes {
      ... on PullRequest {
        number state title url isDraft createdAt headRefOid baseRefName
        mergeable mergeStateStatus reviewDecision
        author { login }
        commits(first: 100) {
          totalCount
          nodes { commit {
            oid message committedDate
            author { name email user { login } }
            committer { email }
            signature { isValid wasSignedByGitHub }
          } }
        }
        last: commits(last: 1) {
          nodes { commit { oid statusCheckRollup { contexts(first: 100) { totalCount nodes {
            __typename
            ... on CheckRun { name status conclusion checkSuite {
              databaseId app { databaseId } workflowRun { event workflow { databaseId } }
            } }
            ... on StatusContext { context state }
          } } } } }
        }
        comments(last: 50) { totalCount nodes { body createdAt author { login } } }
      }
    }
  }
}`

type gqlRepo struct {
	Repository *struct {
		NameWithOwner      string
		IsArchived         bool
		ViewerPermission   string
		SquashMergeAllowed bool
		MergeCommitAllowed bool
		RebaseMergeAllowed bool
	}
	Search struct {
		IssueCount int
		Nodes      []struct {
			Number           int
			State            string
			Title            string
			URL              string
			IsDraft          bool
			CreatedAt        time.Time
			HeadRefOid       string
			BaseRefName      string
			Mergeable        string
			MergeStateStatus string
			ReviewDecision   string
			Author           *struct{ Login string }
			Commits          struct {
				TotalCount int
				Nodes      []struct {
					Commit struct {
						Oid           string
						Message       string
						CommittedDate time.Time
						Author        *struct {
							Name  string
							Email string
							User  *struct{ Login string }
						}
						Committer *struct{ Email string }
						Signature *struct {
							IsValid           bool
							WasSignedByGitHub bool
						}
					}
				}
			}
			Last struct {
				Nodes []struct {
					Commit struct {
						Oid               string
						StatusCheckRollup *struct {
							Contexts struct {
								TotalCount int
								Nodes      []struct {
									Typename   string `json:"__typename"`
									Name       string
									Status     string
									Conclusion string
									CheckSuite *struct {
										DatabaseID  int64
										App         *struct{ DatabaseID int64 }
										WorkflowRun *struct {
											Event    string
											Workflow struct{ DatabaseID int64 }
										}
									}
									Context string
									State   string
								}
							}
						}
					}
				}
			}
			Comments struct {
				TotalCount int
				Nodes      []struct {
					Body      string
					CreatedAt time.Time
					Author    *struct{ Login string }
				}
			}
		}
	}
}

func (c *client) FetchRepo(ctx context.Context, owner, name string) (*Repo, error) {
	vars := map[string]any{
		"owner":  owner,
		"name":   name,
		"search": fmt.Sprintf("repo:%s/%s is:pr is:open author:app/dependabot sort:created-asc", owner, name),
	}
	var resp gqlRepo
	if err := c.gql.DoWithContext(ctx, repoQuery, vars, &resp); err != nil {
		return nil, fmt.Errorf("%s/%s: %w", owner, name, c.withReset(ctx, err))
	}
	if resp.Repository == nil {
		return nil, fmt.Errorf("%s/%s: repository not found or not accessible", owner, name)
	}
	r := &Repo{
		FullName:      resp.Repository.NameWithOwner,
		Archived:      resp.Repository.IsArchived,
		Permission:    resp.Repository.ViewerPermission,
		SquashAllowed: resp.Repository.SquashMergeAllowed,
		MergeAllowed:  resp.Repository.MergeCommitAllowed,
		RebaseAllowed: resp.Repository.RebaseMergeAllowed,
		OpenPRCount:   resp.Search.IssueCount,
		SearchCount:   len(resp.Search.Nodes),
		FetchedAt:     time.Now(),
	}
	for _, n := range resp.Search.Nodes {
		if n.Number == 0 || n.State != "OPEN" {
			continue // non-PR hit, or closed since the search index was built
		}
		pr := PullRequest{
			Number:         n.Number,
			Title:          n.Title,
			URL:            n.URL,
			IsDraft:        n.IsDraft,
			CreatedAt:      n.CreatedAt,
			HeadOID:        n.HeadRefOid,
			BaseRef:        n.BaseRefName,
			Mergeable:      n.Mergeable,
			MergeState:     n.MergeStateStatus,
			ReviewDecision: n.ReviewDecision,
			CommitCount:    n.Commits.TotalCount,
			CommentCount:   n.Comments.TotalCount,
		}
		if n.Author != nil {
			pr.Author = n.Author.Login
		}
		for _, cn := range n.Commits.Nodes {
			cm := Commit{
				OID:           cn.Commit.Oid,
				Message:       cn.Commit.Message,
				CommittedDate: cn.Commit.CommittedDate,
			}
			if sig := cn.Commit.Signature; sig != nil {
				cm.SignedByGitHub = sig.IsValid && sig.WasSignedByGitHub
			}
			if cr := cn.Commit.Committer; cr != nil {
				cm.CommittedByGitHub = strings.EqualFold(cr.Email, gitHubCommitter)
			}
			// The author goes first even when GitHub recorded none, so a
			// trailer can never pass for it.
			var au Author
			if a := cn.Commit.Author; a != nil {
				au = Author{Name: a.Name, Email: a.Email}
				if a.User != nil {
					au.Login = a.User.Login
				}
			}
			cm.Authors = append(cm.Authors, au)
			cm.Authors = append(cm.Authors, coAuthors(cn.Commit.Message)...)
			pr.Commits = append(pr.Commits, cm)
		}
		for _, ln := range n.Last.Nodes {
			pr.ChecksOID = ln.Commit.Oid
			if ln.Commit.StatusCheckRollup == nil {
				continue
			}
			pr.CheckCount = ln.Commit.StatusCheckRollup.Contexts.TotalCount
			for _, cx := range ln.Commit.StatusCheckRollup.Contexts.Nodes {
				if cx.Typename == "StatusContext" {
					pr.Checks = append(pr.Checks, Check{Name: cx.Context, State: statusState(cx.State), Status: true})
				} else {
					c := Check{Name: cx.Name, State: checkRunState(cx.Status, cx.Conclusion)}
					if s := cx.CheckSuite; s != nil {
						c.Suite = s.DatabaseID
						if s.App != nil {
							c.App = s.App.DatabaseID
						}
						if s.WorkflowRun != nil {
							c.Workflow = s.WorkflowRun.Workflow.DatabaseID
							c.Event = s.WorkflowRun.Event
						}
					}
					pr.Checks = append(pr.Checks, c)
				}
			}
		}
		for _, cm := range n.Comments.Nodes {
			c := Comment{Body: cm.Body, CreatedAt: cm.CreatedAt}
			if cm.Author != nil {
				c.Author = cm.Author.Login
			}
			pr.Comments = append(pr.Comments, c)
		}
		r.PullRequests = append(r.PullRequests, pr)
	}
	return r, nil
}

// coAuthors reads the Co-authored-by trailers GitHub would list as extra
// authors. Fetching them from the message instead of the commit's authors
// connection keeps the query cheap: that connection, nested under every
// commit of every PR, made up nearly all of its rate-limit cost.
func coAuthors(msg string) []Author {
	var out []Author
	for _, line := range trailers(msg) {
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok || !strings.EqualFold(key, "co-authored-by") {
			continue
		}
		value = strings.TrimSpace(value)
		name, email := value, ""
		if i := strings.LastIndex(value, "<"); i >= 0 && strings.HasSuffix(value, ">") {
			name, email = strings.TrimSpace(value[:i]), value[i+1:len(value)-1]
		}
		out = append(out, Author{Name: name, Email: email})
	}
	return out
}

// trailers returns the lines of msg's last paragraph, the only one GitHub
// reads trailers from: a Co-authored-by line quoted in the release notes
// Dependabot embeds higher up names no author of the commit.
func trailers(msg string) []string {
	lines := strings.Split(strings.TrimSpace(strings.ReplaceAll(msg, "\r\n", "\n")), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) == "" {
			return lines[i+1:]
		}
	}
	return lines
}

func checkRunState(status, conclusion string) CheckState {
	if status != "COMPLETED" {
		return CheckPending
	}
	switch conclusion {
	case "SUCCESS":
		return CheckPassed
	case "NEUTRAL", "SKIPPED":
		return CheckSkipped
	case "STALE":
		return CheckStale
	}
	return CheckFailed
}

func statusState(state string) CheckState {
	switch state {
	case "SUCCESS":
		return CheckPassed
	case "PENDING", "EXPECTED":
		return CheckPending
	}
	return CheckFailed
}

func (c *client) Merge(ctx context.Context, owner, name string, number int, method, headOID string) error {
	body, _ := json.Marshal(map[string]string{"merge_method": method, "sha": headOID})
	path := fmt.Sprintf("repos/%s/%s/pulls/%d/merge", owner, name, number)
	return c.rest.DoWithContext(ctx, "PUT", path, bytes.NewReader(body), nil)
}

func (c *client) Comment(ctx context.Context, owner, name string, number int, text string) error {
	body, _ := json.Marshal(map[string]string{"body": text})
	path := fmt.Sprintf("repos/%s/%s/issues/%d/comments", owner, name, number)
	return c.rest.DoWithContext(ctx, "POST", path, bytes.NewReader(body), nil)
}

func (c *client) PullRequestDetails(ctx context.Context, owner, name string, number int) (*PullRequestDetails, error) {
	path := fmt.Sprintf("repos/%s/%s/pulls/%d", owner, name, number)
	var pr struct{ Body string }
	if err := c.rest.DoWithContext(ctx, "GET", path, nil, &pr); err != nil {
		return nil, fmt.Errorf("%s/%s#%d: %w", owner, name, number, err)
	}
	d := &PullRequestDetails{Body: pr.Body}
	resp, err := c.diff.RequestWithContext(ctx, "GET", path, nil)
	if err != nil {
		// Only GitHub declining to render the diff, as it does past its
		// size limits, leaves the review to go ahead without it. Anything
		// else, a 5xx or a rate limit above all, fails the call so that it
		// is made again or the review fails: an approval given without the
		// diff is cached as long as any other.
		var he *api.HTTPError
		if ctx.Err() != nil || !errors.As(err, &he) || (he.StatusCode != 406 && he.StatusCode != 422) {
			return nil, fmt.Errorf("%s/%s#%d: diff: %w", owner, name, number, err)
		}
		d.DiffError = err.Error()
		return d, nil
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxDiffBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%s/%s#%d: read diff: %w", owner, name, number, err)
	}
	d.Truncated = len(data) > MaxDiffBytes
	d.Diff = strings.ToValidUTF8(string(data[:min(len(data), MaxDiffBytes)]), "")
	return d, nil
}

const stateQuery = `
query($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) { pullRequest(number: $number) { state merged } }
}`

// PullRequestState reads whether a PR is open, merged or closed. The search
// FetchRepo makes lists open PRs only, so a PR missing from it may have
// been merged or just closed; this tells which.
func (c *client) PullRequestState(ctx context.Context, owner, name string, number int) (PRState, error) {
	var resp struct {
		Repository *struct {
			PullRequest *struct {
				State  string
				Merged bool
			}
		}
	}
	vars := map[string]any{"owner": owner, "name": name, "number": number}
	if err := c.gql.DoWithContext(ctx, stateQuery, vars, &resp); err != nil {
		return "", fmt.Errorf("%s/%s#%d: %w", owner, name, number, c.withReset(ctx, err))
	}
	if resp.Repository == nil || resp.Repository.PullRequest == nil {
		return "", fmt.Errorf("%s/%s#%d: pull request not found", owner, name, number)
	}
	switch pr := resp.Repository.PullRequest; {
	case pr.Merged:
		return PRMerged, nil
	case pr.State == "OPEN":
		return PROpen, nil
	}
	return PRClosed, nil
}

// Transient reports whether a failed read may succeed if made again:
// GitHub answers a heavy query it could not finish in time with a 502 or
// 504, or with a GraphQL error saying something went wrong.
func Transient(err error) bool {
	var he *api.HTTPError
	if errors.As(err, &he) {
		return he.StatusCode == 502 || he.StatusCode == 503 || he.StatusCode == 504
	}
	var ge *api.GraphQLError
	if errors.As(err, &ge) {
		for _, e := range ge.Errors {
			if strings.Contains(e.Message, "Something went wrong") {
				return true
			}
		}
		return false
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// MaxRateLimitWait is the longest Backoff waits for a rate limit to lift;
// a longer one fails the call rather than stall the run.
const MaxRateLimitWait = 2 * time.Minute

// RateLimited reports whether GitHub refused a call for exceeding a rate
// limit and, if so, how long it asked to wait before the next one.
func RateLimited(err error) (time.Duration, bool) {
	var he *api.HTTPError
	if errors.As(err, &he) {
		if he.StatusCode != 403 && he.StatusCode != 429 {
			return 0, false
		}
		if s, err := strconv.Atoi(he.Headers.Get("Retry-After")); err == nil {
			return time.Duration(s) * time.Second, true
		}
		if he.Headers.Get("X-Ratelimit-Remaining") == "0" {
			if reset, err := strconv.ParseInt(he.Headers.Get("X-Ratelimit-Reset"), 10, 64); err == nil {
				return max(0, time.Until(time.Unix(reset, 0))), true
			}
		}
		// A secondary rate limit may come without either header; GitHub
		// then asks for at least a minute's pause.
		if he.StatusCode == 429 || strings.Contains(strings.ToLower(he.Message), "rate limit") {
			return time.Minute, true
		}
		return 0, false
	}
	var rl *RateLimitError
	if errors.As(err, &rl) {
		return max(0, time.Until(rl.Reset)), true
	}
	if graphQLRateLimited(err) {
		// GraphQL's limit is an hourly budget, and its error does not say
		// when it lifts: assume the worst rather than retry blindly.
		return time.Hour, true
	}
	return 0, false
}

// RateLimitError is a GraphQL query refused for the rate limit, with when
// the limit lifts: unlike REST, GitHub's GraphQL error does not say.
type RateLimitError struct {
	Err   error
	Reset time.Time
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("%v (the GraphQL rate limit resets at %s)", e.Err, e.Reset.Local().Format("15:04:05"))
}

func (e *RateLimitError) Unwrap() error { return e.Err }

func graphQLRateLimited(err error) bool {
	var ge *api.GraphQLError
	if errors.As(err, &ge) {
		for _, e := range ge.Errors {
			if e.Type == "RATE_LIMITED" {
				return true
			}
		}
	}
	return false
}

// withReset adds to a GraphQL rate limit error when the limit lifts, read
// from the REST rate_limit endpoint, which does not count against it. Any
// other error, or one whose reset cannot be read, is returned as it is.
func (c *client) withReset(ctx context.Context, err error) error {
	if !graphQLRateLimited(err) || c.rest == nil {
		return err
	}
	var limits struct {
		Resources struct {
			GraphQL struct{ Reset int64 } `json:"graphql"`
		}
	}
	if c.rest.DoWithContext(ctx, "GET", "rate_limit", nil, &limits) != nil || limits.Resources.GraphQL.Reset == 0 {
		return err
	}
	return &RateLimitError{Err: err, Reset: time.Unix(limits.Resources.GraphQL.Reset, 0)}
}

// Backoff says how long to pause before making a failed call again, and
// whether to make it again at all: pause after a transient failure, and as
// long as GitHub asks after a rate limit, unless that is over
// MaxRateLimitWait.
func Backoff(err error, pause time.Duration) (time.Duration, bool) {
	if d, ok := RateLimited(err); ok {
		return d, d <= MaxRateLimitWait
	}
	return pause, Transient(err)
}

// Again calls read, and once more after the pause Backoff gives when it
// fails in a way that may pass, so one hiccup does not fail a scheduled
// run. sleep waits for a duration unless ctx ends first; nil uses a timer.
func Again[T any](ctx context.Context, pause time.Duration, sleep func(context.Context, time.Duration) error, read func() (T, error)) (T, error) {
	v, err := read()
	if err == nil || ctx.Err() != nil {
		return v, err
	}
	d, ok := Backoff(err, pause)
	if !ok {
		if _, limited := RateLimited(err); limited {
			return v, fmt.Errorf("%w; not waiting up to %s for the rate limit to lift", err, d.Round(time.Second))
		}
		return v, err
	}
	if sleep == nil {
		sleep = Sleep
	}
	if err := sleep(ctx, d); err != nil {
		var zero T
		return zero, err
	}
	return read()
}

// Sleep waits for d, or until ctx ends.
func Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Retryable reports whether a failed merge may succeed once GitHub catches
// up: 405 (not mergeable yet) and 409 (head moved) are often transient
// right after another merge into the base branch.
func Retryable(err error) bool {
	var he *api.HTTPError
	return errors.As(err, &he) && (he.StatusCode == 405 || he.StatusCode == 409)
}
