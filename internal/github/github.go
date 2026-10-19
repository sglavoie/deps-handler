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
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
)

// CheckState is the normalised outcome of one check run or status context.
type CheckState string

const (
	CheckPassed  CheckState = "passed"
	CheckPending CheckState = "pending"
	CheckFailed  CheckState = "failed"
)

type Check struct {
	Name  string     `json:"name"`
	State CheckState `json:"state"`
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
	Authors       []Author  `json:"authors"`
	// AuthorCount is the total number of authors, which can exceed the
	// number fetched into Authors.
	AuthorCount int `json:"author_count"`
	// SignedByGitHub is true when the commit carries a valid signature made
	// with GitHub's own key, as Dependabot's commits do.
	SignedByGitHub bool `json:"signed_by_github"`
}

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
	// CheckCount is the total number of checks, which can exceed the number
	// fetched into Checks.
	CheckCount int       `json:"check_count"`
	Comments   []Comment `json:"-"`
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
}

type client struct {
	gql  *api.GraphQLClient
	rest *api.RESTClient
}

// New builds a client from the gh CLI's stored credentials or GH_TOKEN.
func New() (Client, error) {
	gql, err := api.DefaultGraphQLClient()
	if err != nil {
		return nil, fmt.Errorf("github auth (run `gh auth login`): %w", err)
	}
	rest, err := api.DefaultRESTClient()
	if err != nil {
		return nil, fmt.Errorf("github auth (run `gh auth login`): %w", err)
	}
	return &client{gql: gql, rest: rest}, nil
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
            authors(first: 5) { totalCount nodes { name email user { login } } }
            signature { isValid wasSignedByGitHub }
          } }
        }
        last: commits(last: 1) {
          nodes { commit { statusCheckRollup { contexts(first: 100) { totalCount nodes {
            __typename
            ... on CheckRun { name status conclusion }
            ... on StatusContext { context state }
          } } } } }
        }
        comments(last: 30) { nodes { body createdAt author { login } } }
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
						Authors       struct {
							TotalCount int
							Nodes      []struct {
								Name  string
								Email string
								User  *struct{ Login string }
							}
						}
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
						StatusCheckRollup *struct {
							Contexts struct {
								TotalCount int
								Nodes      []struct {
									Typename   string `json:"__typename"`
									Name       string
									Status     string
									Conclusion string
									Context    string
									State      string
								}
							}
						}
					}
				}
			}
			Comments struct {
				Nodes []struct {
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
		"search": fmt.Sprintf("repo:%s/%s is:pr is:open author:app/dependabot", owner, name),
	}
	var resp gqlRepo
	if err := c.gql.DoWithContext(ctx, repoQuery, vars, &resp); err != nil {
		return nil, err
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
		}
		if n.Author != nil {
			pr.Author = n.Author.Login
		}
		for _, cn := range n.Commits.Nodes {
			cm := Commit{
				OID:           cn.Commit.Oid,
				Message:       cn.Commit.Message,
				CommittedDate: cn.Commit.CommittedDate,
				AuthorCount:   cn.Commit.Authors.TotalCount,
			}
			if sig := cn.Commit.Signature; sig != nil {
				cm.SignedByGitHub = sig.IsValid && sig.WasSignedByGitHub
			}
			for _, a := range cn.Commit.Authors.Nodes {
				au := Author{Name: a.Name, Email: a.Email}
				if a.User != nil {
					au.Login = a.User.Login
				}
				cm.Authors = append(cm.Authors, au)
			}
			pr.Commits = append(pr.Commits, cm)
		}
		for _, ln := range n.Last.Nodes {
			if ln.Commit.StatusCheckRollup == nil {
				continue
			}
			pr.CheckCount = ln.Commit.StatusCheckRollup.Contexts.TotalCount
			for _, cx := range ln.Commit.StatusCheckRollup.Contexts.Nodes {
				if cx.Typename == "StatusContext" {
					pr.Checks = append(pr.Checks, Check{Name: cx.Context, State: statusState(cx.State)})
				} else {
					pr.Checks = append(pr.Checks, Check{Name: cx.Name, State: checkRunState(cx.Status, cx.Conclusion)})
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

func checkRunState(status, conclusion string) CheckState {
	if status != "COMPLETED" {
		return CheckPending
	}
	switch conclusion {
	case "SUCCESS", "NEUTRAL", "SKIPPED":
		return CheckPassed
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

// Retryable reports whether a failed merge may succeed once GitHub catches
// up: 405 (not mergeable yet) and 409 (head moved) are often transient
// right after another merge into the base branch.
func Retryable(err error) bool {
	var he *api.HTTPError
	return errors.As(err, &he) && (he.StatusCode == 405 || he.StatusCode == 409)
}
