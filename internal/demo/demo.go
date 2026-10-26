// Package demo is a make-believe GitHub for recording and trying out
// deps-merger: a few fictional acme repositories with Dependabot PRs in
// every state the policy tells apart, and an agent with canned verdicts.
// Nothing here reaches the network. Only the binary built with
// `-tags demo` uses it.
package demo

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/sglavoie/deps-merger/pkg/github"
	"github.com/sglavoie/deps-merger/pkg/policy"
	"github.com/sglavoie/deps-merger/pkg/review"
)

// Config is the config file the demo runs with.
const Config = `defaults:
  merge_method: squash
  max_update_type: minor
agent:
  use: claude
repos:
  - acme/storefront
  - repo: acme/billing
    max_update_type: patch
  - acme/gateway
  - acme/search
  - acme/docs
`

// dep is one entry of a commit's updated-dependencies block.
type dep struct {
	name, from, to, kind string // kind: patch, minor, major or "" (unknown)
}

// spec is a PR as the demo describes it; pr turns it into what GitHub
// would return.
type spec struct {
	number int
	group  string
	deps   []dep
	state  string // merge state: CLEAN, BEHIND, BLOCKED
	review string // review decision
	failed []string
	age    time.Duration
}

var world = map[string][]spec{
	"acme/storefront": {
		{number: 412, deps: []dep{{"lodash", "4.17.20", "4.17.21", "patch"}}},
		{number: 415, group: "eslint", deps: []dep{{"eslint", "9.12.0", "9.14.0", "minor"}, {"@typescript-eslint/parser", "8.8.1", "8.13.0", "minor"}}},
		{number: 418, deps: []dep{{"axios", "1.7.7", "1.8.2", "minor"}}, state: "BEHIND"},
		{number: 421, deps: []dep{{"react-router", "6.27.0", "7.1.1", "major"}}},
	},
	"acme/billing": {
		{number: 57, deps: []dep{{"stripe", "17.2.0", "17.4.0", "minor"}}},
		{number: 58, deps: []dep{{"express", "4.21.1", "5.0.1", "major"}}},
		{number: 61, deps: []dep{{"jest", "29.7.0", "29.7.1", "patch"}}, failed: []string{"test"}},
	},
	"acme/gateway": {
		{number: 203, deps: []dep{{"golang.org/x/net", "0.30.0", "0.31.0", "minor"}}},
		{number: 204, deps: []dep{{"actions/checkout", "11bd719", "eef6144", ""}}},
		{number: 207, deps: []dep{{"github.com/redis/go-redis/v9", "9.6.1", "9.6.2", "patch"}}, state: "BLOCKED", review: "REVIEW_REQUIRED"},
	},
	"acme/search": {
		{number: 88, deps: []dep{{"pydantic", "2.9.2", "2.9.3", "patch"}}, failed: []string{"lint"}},
		{number: 90, deps: []dep{{"httpx", "0.27.2", "0.28.0", "minor"}}, failed: []string{"lint"}},
	},
	"acme/docs": nil,
}

func (s spec) pr(repo string, now time.Time) github.PullRequest {
	first := s.deps[0]
	title := fmt.Sprintf("Bump %s from %s to %s", first.name, first.from, first.to)
	if s.group != "" {
		title = fmt.Sprintf("Bump the %s group with %d updates", s.group, len(s.deps))
	}
	var msg strings.Builder
	fmt.Fprintf(&msg, "%s\n\n---\nupdated-dependencies:\n", title)
	for _, d := range s.deps {
		kind := "version-update:semver-" + d.kind
		if d.kind == "" {
			kind = ""
		}
		fmt.Fprintf(&msg, "- dependency-name: %q\n  dependency-version: %q\n  dependency-type: direct:production\n  update-type: %q\n", d.name, d.to, kind)
		if s.group != "" {
			fmt.Fprintf(&msg, "  dependency-group: %s\n", s.group)
		}
	}
	msg.WriteString("...\n\nSigned-off-by: dependabot[bot] <support@github.com>")

	head := fmt.Sprintf("%07x", s.number*2654435761%0xfffffff)
	age := s.age
	if age == 0 {
		age = 26 * time.Hour
	}
	state := s.state
	if state == "" {
		state = "CLEAN"
	}
	checks := []github.Check{{Name: "build", State: github.CheckPassed}, {Name: "test", State: github.CheckPassed}, {Name: "lint", State: github.CheckPassed}}
	for i, c := range checks {
		for _, f := range s.failed {
			if c.Name == f {
				checks[i].State = github.CheckFailed
				if state == "CLEAN" {
					state = "UNSTABLE"
				}
			}
		}
	}
	return github.PullRequest{
		Number:         s.number,
		Title:          title,
		URL:            fmt.Sprintf("https://github.com/%s/pull/%d", repo, s.number),
		Author:         "dependabot",
		CreatedAt:      now.Add(-age),
		HeadOID:        head,
		BaseRef:        "main",
		Mergeable:      "MERGEABLE",
		MergeState:     state,
		ReviewDecision: s.review,
		CommitCount:    1,
		Commits: []github.Commit{{
			OID: head, Message: msg.String(), CommittedDate: now.Add(-age),
			Authors:        []github.Author{{Login: "dependabot[bot]", Name: "dependabot[bot]"}},
			SignedByGitHub: true, CommittedByGitHub: true,
		}},
		Checks:     checks,
		ChecksOID:  head,
		CheckCount: len(checks),
	}
}

// Client is a GitHub holding the demo's repositories. Merges and comments
// change only its memory.
type Client struct {
	mu      sync.Mutex
	merged  map[string]bool
	rebased map[string]time.Time
}

// NewClient returns a Client in the demo's starting state.
func NewClient() *Client {
	return &Client{merged: map[string]bool{}, rebased: map[string]time.Time{}}
}

func key(owner, name string, n int) string { return fmt.Sprintf("%s/%s#%d", owner, name, n) }

func (c *Client) Viewer(context.Context) (string, error) { return "octocat", nil }

func (c *Client) FetchRepo(ctx context.Context, owner, name string) (*github.Repo, error) {
	full := owner + "/" + name
	specs, ok := world[full]
	if !ok {
		return nil, fmt.Errorf("%s: repository not found or not accessible", full)
	}
	pause(ctx, 250*time.Millisecond)
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	repo := &github.Repo{FullName: full, Permission: "WRITE", SquashAllowed: true, MergeAllowed: true, FetchedAt: now}
	for _, s := range specs {
		k := key(owner, name, s.number)
		if c.merged[k] {
			continue
		}
		pr := s.pr(full, now)
		if at, ok := c.rebased[k]; ok {
			pr.Comments = []github.Comment{{Author: "octocat", Body: "@dependabot rebase", CreatedAt: at}}
			pr.CommentCount = 1
		}
		repo.PullRequests = append(repo.PullRequests, pr)
	}
	repo.OpenPRCount, repo.SearchCount = len(repo.PullRequests), len(repo.PullRequests)
	return repo, nil
}

func (c *Client) Merge(ctx context.Context, owner, name string, number int, _, _ string) error {
	pause(ctx, 400*time.Millisecond)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.merged[key(owner, name, number)] = true
	return nil
}

func (c *Client) Comment(ctx context.Context, owner, name string, number int, _ string) error {
	pause(ctx, 200*time.Millisecond)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rebased[key(owner, name, number)] = time.Now()
	return nil
}

func (c *Client) PullRequestDetails(context.Context, string, string, int) (*github.PullRequestDetails, error) {
	return &github.PullRequestDetails{}, nil
}

// FileText finds no files: the demo's repositories keep no policy of
// their own.
func (c *Client) FileText(context.Context, string, string, string) ([]byte, error) {
	return nil, nil
}

func (c *Client) PullRequestState(_ context.Context, owner, name string, number int) (github.PRState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.merged[key(owner, name, number)] {
		return github.PRMerged, nil
	}
	return github.PROpen, nil
}

// Agent stands in for Claude Code; Reviewer gives its verdicts, so it is
// never run.
type Agent struct{}

func (Agent) Name() string { return "claude" }

func (Agent) Run(context.Context, string, string) (review.Answer, error) {
	return review.Answer{}, fmt.Errorf("the demo agent does not run")
}

// verdicts are the demo agent's answers, by PR; any other PR is approved.
var verdicts = map[string]policy.Verdict{
	"acme/storefront#421": {Approved: true, Summary: "Uses no v6 API that v7 removed or changed."},
	"acme/billing#58": {Summary: "Express 5 breaks routing the app relies on.", Concerns: []string{
		"src/server.ts uses app.del(), removed in Express 5",
		"routes/webhooks.ts registers a '/hooks/*' path; v5 requires named wildcards",
	}},
	"acme/gateway#204": {Approved: true, Summary: "A tagged release; the inputs ci.yml uses are kept."},
}

// Reviewer answers as a careful agent would, after a while.
type Reviewer struct{}

func (Reviewer) Review(ctx context.Context, req review.Request) (policy.Verdict, error) {
	if !pause(ctx, 1500*time.Millisecond) {
		return policy.Verdict{}, ctx.Err()
	}
	if v, ok := verdicts[key(req.Owner, req.Name, req.Number)]; ok {
		return v, nil
	}
	return policy.Verdict{Approved: true, Summary: "No breaking changes affect this repository."}, nil
}

// pause waits d, so the demo moves at the pace of a real run, and reports
// whether ctx let it.
func pause(ctx context.Context, d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}
