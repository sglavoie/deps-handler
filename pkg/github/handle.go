package github

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// HandleClient is separate from Dependabot discovery and policy. Only the
// orchestrator, never an agent, can publish repairs, rerun jobs or merge.
type HandleClient interface {
	PRClient
	ActionsClient
	OpenPRs(context.Context, string, string) ([]PRTarget, error)
	HandleState(context.Context, PRTarget) (HandleState, error)
	Merge(context.Context, string, string, int, string, string) error
	PullRequestState(context.Context, string, string, int) (PRState, error)
}

func NewHandleClient() (HandleClient, error) {
	c, err := NewPRClient()
	if err != nil {
		return nil, err
	}
	return c.(*prClient), nil
}

// DependabotAuthor uses the original PR author, regardless of later commits.
func DependabotAuthor(login string) bool {
	return strings.EqualFold(login, "dependabot") || strings.EqualFold(login, "dependabot[bot]") || strings.EqualFold(login, "app/dependabot")
}

// ParseRepository validates an explicit selector without consulting config.
func ParseRepository(value string) (owner, name string, err error) {
	t, err := ParsePRTarget(value + "#1")
	if err != nil {
		return "", "", fmt.Errorf("invalid repository %q; use owner/repo", value)
	}
	return t.Owner, t.Repo, nil
}

const openPRsQuery = `query($owner: String!, $name: String!, $after: String) {
 repository(owner: $owner, name: $name) {
  selectionBoundary: pullRequests(first: 1, states: OPEN, orderBy: {field: CREATED_AT, direction: DESC}) { nodes { number } }
  pullRequests(first: 100, after: $after, states: OPEN, orderBy: {field: CREATED_AT, direction: ASC}) {
   pageInfo { hasNextPage endCursor } nodes { number }
  }
 }
}`

// OpenPRs completes discovery before any write, oldest first. Page failures
// return no partial selection. Processing never discovers more PRs mid-run.
func (c *prClient) OpenPRs(ctx context.Context, owner, name string) ([]PRTarget, error) {
	if _, _, err := ParseRepository(owner + "/" + name); err != nil {
		return nil, err
	}
	targets := []PRTarget{}
	seen, cursors := map[int]bool{}, map[string]bool{}
	var after any
	boundary := 0
	for range 100 {
		var out struct {
			Repository *struct {
				SelectionBoundary *struct{ Nodes []struct{ Number int } }
				PullRequests      *struct {
					PageInfo *struct {
						HasNextPage *bool
						EndCursor   string
					}
					Nodes []struct{ Number int }
				}
			}
		}
		if err := c.gql.DoWithContext(ctx, openPRsQuery, map[string]any{"owner": owner, "name": name, "after": after}, &out); err != nil {
			return nil, c.withReset(ctx, err)
		}
		if out.Repository == nil || out.Repository.PullRequests == nil {
			return nil, errors.New("repository or open PR list is missing")
		}
		page := out.Repository.PullRequests
		if page.PageInfo == nil || page.PageInfo.HasNextPage == nil || page.Nodes == nil {
			return nil, errors.New("incomplete open PR listing")
		}
		if after == nil {
			// PR numbers increase within a repository. Capture the newest
			// open PR on the first read so later pages cannot add new work.
			first := out.Repository.SelectionBoundary
			if first == nil || first.Nodes == nil || len(first.Nodes) > 1 || (len(first.Nodes) == 1 && first.Nodes[0].Number <= 0) {
				return nil, errors.New("open PR selection boundary is missing or invalid")
			}
			if len(first.Nodes) == 0 {
				return targets, nil
			}
			boundary = first.Nodes[0].Number
		}
		for _, n := range page.Nodes {
			if n.Number <= 0 || seen[n.Number] {
				return nil, errors.New("invalid or duplicate PR in repository listing; retry discovery")
			}
			seen[n.Number] = true
			if n.Number <= boundary {
				targets = append(targets, PRTarget{owner, name, n.Number})
			}
		}
		if !*page.PageInfo.HasNextPage {
			return targets, nil
		}
		cursor := page.PageInfo.EndCursor
		if cursor == "" || cursors[cursor] || len(page.Nodes) == 0 {
			return nil, errors.New("open PR pagination did not advance")
		}
		cursors[cursor], after = true, cursor
	}
	return nil, errors.New("open PR listing exceeded 100 pages; no PRs selected")
}

type DiscussionComment struct {
	Author *struct{ Login string } `json:"author"`
	Body   string                  `json:"body"`
	URL    string                  `json:"url"`
	State  string                  `json:"state,omitempty"`
}

type DiscussionThread struct {
	Path       string         `json:"path"`
	IsResolved *bool          `json:"isResolved"`
	Comments   DiscussionPage `json:"comments"`
}

type DiscussionPage struct {
	TotalCount *int                `json:"totalCount"`
	Nodes      []DiscussionComment `json:"nodes"`
}

func (p DiscussionPage) complete() bool {
	return p.TotalCount != nil && *p.TotalCount >= 0 && p.Nodes != nil && *p.TotalCount == len(p.Nodes)
}

// HandleState pins code and discussion evidence independently of the AI verdict.
// Incomplete discussions never establish merge readiness.
type HandleState struct {
	Snapshot            PRSnapshot         `json:"snapshot"`
	Author              string             `json:"author"`
	UpdatedAt           string             `json:"updated_at"`
	Body                string             `json:"body"`
	MergeMethods        []string           `json:"merge_methods"`
	Comments            DiscussionPage     `json:"comments"`
	Reviews             DiscussionPage     `json:"reviews"`
	Threads             []DiscussionThread `json:"threads"`
	DiscussionsComplete bool               `json:"discussions_complete"`
}

const handleStateQuery = `query($owner: String!, $name: String!, $number: Int!) {
 repository(owner: $owner, name: $name) {
  mergeCommitAllowed squashMergeAllowed rebaseMergeAllowed
  pullRequest(number: $number) {
   author { login } updatedAt body headRefOid baseRefOid
   comments(first: 100) { totalCount nodes { author { login } body url } }
   reviews(first: 100) { totalCount nodes { author { login } body url state } }
   reviewThreads(first: 100) { totalCount nodes {
    path isResolved comments(first: 100) { totalCount nodes { author { login } body url } }
   } }
  }
 }
}`

func (c *prClient) HandleState(ctx context.Context, target PRTarget) (HandleState, error) {
	snapshot, err := c.Snapshot(ctx, target)
	if err != nil {
		return HandleState{}, err
	}
	var out struct {
		Repository *struct {
			MergeCommitAllowed, SquashMergeAllowed, RebaseMergeAllowed *bool
			PullRequest                                                *struct {
				Author                                  *struct{ Login string }
				UpdatedAt, Body, HeadRefOid, BaseRefOid string
				Comments, Reviews                       DiscussionPage
				ReviewThreads                           *struct {
					TotalCount *int
					Nodes      []DiscussionThread
				}
			}
		}
	}
	if err := c.gql.DoWithContext(ctx, handleStateQuery, map[string]any{"owner": target.Owner, "name": target.Repo, "number": target.Number}, &out); err != nil {
		return HandleState{}, c.withReset(ctx, err)
	}
	if out.Repository == nil || out.Repository.PullRequest == nil {
		return HandleState{}, errors.New("PR handling metadata is missing")
	}
	r, p := out.Repository, out.Repository.PullRequest
	if p.Author == nil || p.Author.Login == "" || p.UpdatedAt == "" || r.MergeCommitAllowed == nil || r.SquashMergeAllowed == nil || r.RebaseMergeAllowed == nil {
		return HandleState{}, errors.New("PR author, update time or merge methods are missing")
	}
	if snapshot.HeadOID != p.HeadRefOid || snapshot.BaseOID != p.BaseRefOid {
		return HandleState{}, errors.New("PR or base changed while reading handling metadata; retry")
	}
	s := HandleState{Snapshot: snapshot, Author: p.Author.Login, UpdatedAt: p.UpdatedAt, Body: p.Body,
		Comments: p.Comments, Reviews: p.Reviews, MergeMethods: []string{}, Threads: []DiscussionThread{}}
	for _, method := range []struct {
		name    string
		allowed bool
	}{{"squash", *r.SquashMergeAllowed}, {"merge", *r.MergeCommitAllowed}, {"rebase", *r.RebaseMergeAllowed}} {
		if method.allowed {
			s.MergeMethods = append(s.MergeMethods, method.name)
		}
	}
	s.DiscussionsComplete = p.Comments.complete() && p.Reviews.complete()
	if p.ReviewThreads == nil || p.ReviewThreads.TotalCount == nil || *p.ReviewThreads.TotalCount < 0 || p.ReviewThreads.Nodes == nil {
		s.DiscussionsComplete = false
	} else {
		s.Threads = p.ReviewThreads.Nodes
		s.DiscussionsComplete = s.DiscussionsComplete && *p.ReviewThreads.TotalCount == len(s.Threads)
	}
	for _, thread := range s.Threads {
		s.DiscussionsComplete = s.DiscussionsComplete && thread.IsResolved != nil && thread.Comments.complete()
	}
	return s, nil
}
