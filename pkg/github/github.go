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
	"math"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/cli/go-gh/v2/pkg/auth"
)

// CheckState is the normalised outcome of one check run or status context.
type CheckState string

const (
	// CheckUnknown preserves missing or unrecognized API states so policy
	// blocks the PR even if a newer check would otherwise supersede it.
	CheckUnknown CheckState = "unknown"
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
	// BaseMovedAt is when the latest commit on the base branch was made, as
	// a merge into it does; zero when GitHub cannot say.
	BaseMovedAt time.Time `json:"base_moved_at"`
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

// WithPermission returns c with permission filled in as the viewer's
// permission on every repository GitHub reports none for. GitHub reports
// none for a GitHub App installation, whose permissions the caller knows
// instead. A permission GitHub does report is kept.
func WithPermission(c Client, permission string) Client {
	return permissionClient{Client: c, permission: permission}
}

type permissionClient struct {
	Client
	permission string
}

func (c permissionClient) FetchRepo(ctx context.Context, owner, name string) (*Repo, error) {
	r, err := c.Client.FetchRepo(ctx, owner, name)
	if r != nil && r.Permission == "" {
		r.Permission = c.permission
	}
	return r, err
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
	// FileText reads the file at path on the repository's default branch.
	// It returns nil, and no error, when the repository or the file does
	// not exist or cannot be seen.
	FileText(ctx context.Context, owner, name, path string) ([]byte, error)
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

// New builds a github.com client from the gh CLI's stored credentials or
// GH_TOKEN. Other hosts are rejected: checkouts and verdict keys assume
// github.com, so accepting another host could reuse an unrelated approval.
func New() (Client, error) {
	opts, err := githubComOptions()
	if err != nil {
		return nil, err
	}
	c, err := newClient(opts)
	if err != nil {
		return nil, fmt.Errorf("github auth (run `gh auth login`): %w", err)
	}
	return c, nil
}

// githubComOptions keeps discovery and runs on the same supported host as
// review checkouts. Reject an unsupported selection instead of silently
// acting on a same-named repository on github.com.
func githubComOptions() (api.ClientOptions, error) {
	host, source := auth.DefaultHost()
	if !strings.EqualFold(host, "github.com") {
		return api.ClientOptions{}, fmt.Errorf("unsupported GitHub host %q from %s: deps-merger supports only github.com; set GH_HOST=github.com to use it", host, source)
	}
	return api.ClientOptions{Host: "github.com", Timeout: RequestTimeout}, nil
}

// NewWithToken builds a github.com client that authenticates with token,
// such as a GitHub App installation token, and reads nothing of the gh
// CLI's: neither its credentials nor its config. go-gh would otherwise
// send the token to an api_host set in that config, and log it when
// GH_DEBUG is set.
func NewWithToken(token string) (Client, error) {
	if token == "" {
		return nil, errors.New("github auth: empty token")
	}
	return newClient(api.ClientOptions{Host: "github.com", APIHost: "api.github.com", AuthToken: token, Transport: http.DefaultTransport, Timeout: RequestTimeout, LogIgnoreEnv: true})
}

func newClient(opts api.ClientOptions) (*client, error) {
	gql, err := api.NewGraphQLClient(opts)
	if err != nil {
		return nil, err
	}
	rest, err := api.NewRESTClient(opts)
	if err != nil {
		return nil, err
	}
	opts.Headers = map[string]string{"Accept": "application/vnd.github.diff"}
	diff, err := api.NewRESTClient(opts)
	if err != nil {
		return nil, err
	}
	return &client{gql: gql, rest: rest, diff: diff}, nil
}

func (c *client) Viewer(ctx context.Context) (string, error) {
	var resp struct {
		Viewer struct{ Login string }
	}
	if err := c.gql.DoWithContext(ctx, `query{viewer{login}}`, nil, &resp); err != nil {
		return "", c.withReset(ctx, err)
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
        baseRef { target { ... on Commit { committedDate } } }
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
			BaseRef *struct {
				Target *struct{ CommittedDate time.Time }
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
	err := c.gql.DoWithContext(ctx, repoQuery, vars, &resp)
	if err != nil && !repoNotFound(err) {
		return nil, fmt.Errorf("%s/%s: %w", owner, name, c.withReset(ctx, err))
	}
	// GitHub answers for a repository that does not exist, or that the
	// token cannot see, with a NOT_FOUND error rather than a null one.
	if err != nil || resp.Repository == nil {
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
		if n.BaseRef != nil && n.BaseRef.Target != nil {
			pr.BaseMovedAt = n.BaseRef.Target.CommittedDate
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
				switch cx.Typename {
				case "StatusContext":
					pr.Checks = append(pr.Checks, Check{Name: cx.Context, State: statusState(cx.State), Status: true})
				case "CheckRun":
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
				default:
					pr.Checks = append(pr.Checks, Check{Name: cx.Name, State: CheckUnknown})
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
		// Git, and GitHub with it, allows space before the colon.
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "co-authored-by") {
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
	for i, line := range slices.Backward(lines) {
		if strings.TrimSpace(line) == "" {
			return lines[i+1:]
		}
	}
	return lines
}

func checkRunState(status, conclusion string) CheckState {
	switch status {
	case "IN_PROGRESS", "PENDING", "QUEUED", "REQUESTED", "WAITING":
		return CheckPending
	case "COMPLETED":
	default:
		return CheckUnknown
	}
	switch conclusion {
	case "SUCCESS":
		return CheckPassed
	case "NEUTRAL", "SKIPPED":
		return CheckSkipped
	case "STALE":
		return CheckStale
	case "ACTION_REQUIRED", "CANCELLED", "FAILURE", "STARTUP_FAILURE", "TIMED_OUT":
		return CheckFailed
	}
	return CheckUnknown
}

func statusState(state string) CheckState {
	switch state {
	case "SUCCESS":
		return CheckPassed
	case "PENDING", "EXPECTED":
		return CheckPending
	case "ERROR", "FAILURE":
		return CheckFailed
	}
	return CheckUnknown
}

func (c *client) Merge(ctx context.Context, owner, name string, number int, method, headOID string) error {
	body, _ := json.Marshal(map[string]string{"merge_method": method, "sha": headOID})
	path := fmt.Sprintf("repos/%s/%s/pulls/%d/merge", owner, name, number)
	var result struct {
		Merged  *bool
		Message string
	}
	err := c.rest.DoWithContext(ctx, "PUT", path, bytes.NewReader(body), &result)
	if he, ok := errors.AsType[*api.HTTPError](err); ok && he.StatusCode == http.StatusForbidden {
		// GitHub words it as a scope for a user's token, and as a
		// permission for a GitHub App's.
		switch {
		case strings.Contains(he.Message, "without `workflow` scope"):
			return &WorkflowScopeError{Err: err}
		case strings.Contains(he.Message, "without `workflows` permission"):
			return &WorkflowScopeError{Err: err, App: true}
		}
	}
	if err != nil {
		// A successful HTTP response with unreadable JSON does not say
		// whether the write went through. Reconcile it like a timeout.
		if errors.As(err, new(*json.SyntaxError)) || errors.As(err, new(*json.UnmarshalTypeError)) {
			return fmt.Errorf("%w: %w", errMergeUnconfirmed, err)
		}
		return err
	}
	if result.Merged == nil {
		return errMergeUnconfirmed
	}
	if !*result.Merged {
		if result.Message == "" {
			result.Message = "GitHub reported merged=false"
		}
		return fmt.Errorf("merge refused: %s", result.Message)
	}
	return nil
}

// errMergeUnconfirmed asks the runner to check the PR's state before
// reporting the outcome of a write whose response supplied no answer.
var errMergeUnconfirmed = errors.New("GitHub did not confirm the merge outcome")

// errCommentUnconfirmed preserves the possibility that a comment was
// posted even though its successful HTTP response could not be decoded.
var errCommentUnconfirmed = errors.New("GitHub did not confirm the comment outcome")

func (c *client) Comment(ctx context.Context, owner, name string, number int, text string) error {
	body, _ := json.Marshal(map[string]string{"body": text})
	path := fmt.Sprintf("repos/%s/%s/issues/%d/comments", owner, name, number)
	err := c.rest.DoWithContext(ctx, "POST", path, bytes.NewReader(body), nil)
	// A successful POST with unreadable JSON may already have created the
	// comment. Keep its place in the rebase queue and warn about the
	// uncertainty, as for a connection lost while reading the response.
	if errors.As(err, new(*json.SyntaxError)) || errors.As(err, new(*json.UnmarshalTypeError)) {
		return fmt.Errorf("%w: %w", errCommentUnconfirmed, err)
	}
	return err
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
		he, ok := errors.AsType[*api.HTTPError](err)
		if ctx.Err() != nil || !ok || (he.StatusCode != http.StatusNotAcceptable && he.StatusCode != http.StatusUnprocessableEntity) {
			return nil, fmt.Errorf("%s/%s#%d: diff: %w", owner, name, number, err)
		}
		d.DiffError = err.Error()
		return d, nil
	}
	defer func() {
		// Drain a little of what a truncated diff left unread, so the
		// connection can be reused when that is all there is; a huge
		// remainder costs more to read than a new connection.
		_, _ = io.CopyN(io.Discard, resp.Body, 64<<10)
		_ = resp.Body.Close()
	}()
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
	case pr.State == "CLOSED":
		return PRClosed, nil
	}
	return "", fmt.Errorf("%s/%s#%d: unrecognized or inconsistent pull request state %q", owner, name, number, resp.Repository.PullRequest.State)
}

const fileQuery = `
query($owner: String!, $name: String!, $expression: String!) {
  repository(owner: $owner, name: $name) {
    object(expression: $expression) {
      __typename
      ... on Blob { text isBinary isTruncated }
    }
  }
}`

func (c *client) FileText(ctx context.Context, owner, name, path string) ([]byte, error) {
	var resp struct {
		Repository *struct {
			Object *struct {
				Typename    string `json:"__typename"`
				Text        *string
				IsBinary    bool
				IsTruncated bool
			}
		}
	}
	vars := map[string]any{"owner": owner, "name": name, "expression": "HEAD:" + path}
	if err := c.gql.DoWithContext(ctx, fileQuery, vars, &resp); err != nil {
		if repoNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("%s/%s:%s: %w", owner, name, path, c.withReset(ctx, err))
	}
	if resp.Repository == nil || resp.Repository.Object == nil {
		return nil, nil
	}
	switch o := resp.Repository.Object; {
	case o.Typename != "Blob":
		return nil, fmt.Errorf("%s/%s:%s is not a file", owner, name, path)
	case o.IsBinary || o.Text == nil:
		return nil, fmt.Errorf("%s/%s:%s is not a text file", owner, name, path)
	case o.IsTruncated:
		return nil, fmt.Errorf("%s/%s:%s is too large", owner, name, path)
	default:
		return []byte(*o.Text), nil
	}
}

// repoNotFound reports whether err is GitHub saying the repository a
// query asked for does not exist or cannot be seen, with no other errors.
// A missing nested object does not establish that its policy file is
// absent, and another error (such as a rate limit) must not be swallowed.
func repoNotFound(err error) bool {
	if ge, ok := errors.AsType[*api.GraphQLError](err); ok && len(ge.Errors) > 0 {
		for _, e := range ge.Errors {
			if e.Type != "NOT_FOUND" || len(e.Path) != 1 || e.Path[0] != "repository" {
				return false
			}
		}
		return true
	}
	return false
}

// Transient reports whether a failed call may succeed if made again:
// GitHub can answer with a 500, 502, 503 or 504, or with a GraphQL error
// saying something went wrong, and a connection can time out, or be closed
// or reset, on the way. For a merge or comment, it means the write may have
// gone through all the same. A
// call cut short by the run being cancelled can fail in these ways too:
// callers rule that out first.
func Transient(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errMergeUnconfirmed) || errors.Is(err, errCommentUnconfirmed) {
		return true
	}
	if he, ok := errors.AsType[*api.HTTPError](err); ok {
		return he.StatusCode == http.StatusInternalServerError || he.StatusCode == http.StatusBadGateway || he.StatusCode == http.StatusServiceUnavailable || he.StatusCode == http.StatusGatewayTimeout
	}
	// The connection was closed or reset before the whole response came
	// back: the request may have reached GitHub, and nothing says it failed.
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) {
		return true
	}
	// The same over HTTP/2, as GitHub speaks it: a stream reset, or the
	// connection gone away under it. net/http keeps these errors' types
	// to itself, so only their text tells them apart.
	if msg := err.Error(); strings.Contains(msg, "stream error: stream ID") || strings.Contains(msg, "http2: server sent GOAWAY") || strings.Contains(msg, "http2: client connection lost") {
		return true
	}
	if ge, ok := errors.AsType[*api.GraphQLError](err); ok {
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

// MaxRateLimitWait is the longest a read (through Backoff) or a write waits
// for a rate limit to lift; a longer one fails the call rather than stall
// the run.
const MaxRateLimitWait = 2 * time.Minute

// RateLimited reports whether GitHub refused a call for exceeding a rate
// limit and, if so, how long it asked to wait before the next one.
func RateLimited(err error) (time.Duration, bool) {
	if he, ok := errors.AsType[*api.HTTPError](err); ok {
		if he.StatusCode != http.StatusForbidden && he.StatusCode != http.StatusTooManyRequests {
			return 0, false
		}
		if d, ok := retryAfter(he.Headers.Get("Retry-After")); ok {
			return d, true
		}
		// GitHub sends the quota on every response: one refused for another
		// reason may also have used up the last of it, and is no rate limit.
		msg := strings.ToLower(he.Message)
		if he.Headers.Get("X-Ratelimit-Remaining") == "0" && (he.StatusCode == http.StatusTooManyRequests || msg == "" || strings.Contains(msg, "rate limit")) {
			if reset, err := strconv.ParseInt(he.Headers.Get("X-Ratelimit-Reset"), 10, 64); err == nil {
				return max(0, time.Until(time.Unix(reset, 0))), true
			}
		}
		// A secondary rate limit may come without either header; GitHub
		// then asks for at least a minute's pause.
		if he.StatusCode == http.StatusTooManyRequests || strings.Contains(msg, "rate limit") {
			return time.Minute, true
		}
		return 0, false
	}
	if rl, ok := errors.AsType[*RateLimitError](err); ok {
		return max(0, time.Until(rl.Reset)), true
	}
	if graphQLRateLimited(err) {
		// GraphQL's limit is an hourly budget, and its error does not say
		// when it lifts: assume the worst rather than retry blindly.
		return time.Hour, true
	}
	return 0, false
}

// retryAfter accepts the HTTP date and non-negative decimal seconds forms
// of Retry-After. Saturate large delays before converting to nanoseconds:
// wrapping a long pause into a negative duration would retry immediately.
func retryAfter(value string) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value != "" && !strings.ContainsFunc(value, func(r rune) bool { return r < '0' || r > '9' }) {
		seconds, err := strconv.ParseUint(value, 10, 64)
		if err != nil || seconds > uint64(math.MaxInt64/int64(time.Second)) {
			return time.Duration(math.MaxInt64), true
		}
		return time.Duration(seconds) * time.Second, true
	}
	if reset, err := http.ParseTime(value); err == nil {
		return max(0, time.Until(reset)), true
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

// WorkflowScopeError is a merge refused because the PR changes a file under
// .github/workflows and the token lacks the workflow scope GitHub asks for
// to write one, as gh's default login does. App is true for a GitHub App's
// token, which lacks the Workflows permission instead.
type WorkflowScopeError struct {
	Err error
	App bool
}

func (e *WorkflowScopeError) Error() string {
	if e.App {
		return "the GitHub App lacks the Workflows permission needed to merge changes to workflow files: grant it Workflows: read and write"
	}
	return "token lacks the `workflow` scope needed to merge changes to workflow files: run `gh auth refresh -h github.com -s workflow`, or give GH_TOKEN that scope"
}

func (e *WorkflowScopeError) Unwrap() error { return e.Err }

func graphQLRateLimited(err error) bool {
	if ge, ok := errors.AsType[*api.GraphQLError](err); ok {
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
		return v, NotWaiting(err)
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

// NotWaiting adds to a rate limit error that it was not waited out, and
// for how long it holds, when GitHub says. Any other error is returned as
// it is.
func NotWaiting(err error) error {
	d, ok := RateLimited(err)
	switch {
	case !ok:
		return err
	case graphQLRateLimited(err) && !errors.As(err, new(*RateLimitError)):
		return fmt.Errorf("%w; not waiting, as GitHub does not say when the limit lifts", err)
	}
	return fmt.Errorf("%w; not waiting up to %s for the rate limit to lift", err, d.Round(time.Second))
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

// Refused reports whether GitHub answered a call with an error status that
// says it was not carried out: any but the 500, 502, 503 and 504 Transient
// counts, after which a write may have gone through all the same.
func Refused(err error) bool {
	_, ok := errors.AsType[*api.HTTPError](err)
	return ok && !Transient(err)
}

// Retryable reports whether a failed merge may succeed once GitHub catches
// up: 405 (not mergeable yet) and 409 (head moved) are often transient
// right after another merge into the base branch.
func Retryable(err error) bool {
	he, ok := errors.AsType[*api.HTTPError](err)
	return ok && (he.StatusCode == http.StatusMethodNotAllowed || he.StatusCode == http.StatusConflict)
}
