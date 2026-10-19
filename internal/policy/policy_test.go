package policy

import (
	"strings"
	"testing"

	"github.com/sglavoie/deps-merger/internal/config"
	"github.com/sglavoie/deps-merger/internal/github"
	"github.com/sglavoie/deps-merger/internal/testutil"
)

var defaultPolicy = config.Builtin

func one(t *testing.T, repo *github.Repo, p config.Policy) Decision {
	t.Helper()
	ds := Evaluate(repo, p, "me")
	if len(ds) != 1 {
		t.Fatalf("got %d decisions", len(ds))
	}
	return ds[0]
}

func TestEvaluate(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*github.Repo, *github.PullRequest)
		policy func(*config.Policy)
		deps   []string
		action Action
		reason string
	}{
		{name: "clean patch merges", action: ActionMerge},
		{name: "minor merges", deps: []string{"lib:minor"}, action: ActionMerge},
		{name: "major held", deps: []string{"lib:major"}, action: ActionSkip, reason: "major update exceeds limit"},
		{name: "major allowed by override", deps: []string{"lib:major"},
			policy: func(p *config.Policy) { p.MaxUpdateType = config.UpdateMajor }, action: ActionMerge},
		{name: "group with one major is major", deps: []string{"a:patch", "b:major"}, action: ActionSkip, reason: "major update exceeds limit"},
		{name: "failing check", action: ActionSkip, reason: "failing checks: build",
			mutate: func(_ *github.Repo, pr *github.PullRequest) { pr.Checks[0].State = github.CheckFailed }},
		{name: "pending check", action: ActionWait, reason: "still running",
			mutate: func(_ *github.Repo, pr *github.PullRequest) { pr.Checks[0].State = github.CheckPending }},
		{name: "no checks skipped by default", action: ActionSkip, reason: "no checks ran",
			mutate: func(_ *github.Repo, pr *github.PullRequest) { pr.Checks = nil }},
		{name: "no checks with opt-in merges", action: ActionMerge,
			mutate: func(_ *github.Repo, pr *github.PullRequest) { pr.Checks = nil },
			policy: func(p *config.Policy) { p.RequireChecks = false }},
		{name: "human commit", action: ActionSkip, reason: "modified by sglavoie",
			mutate: func(_ *github.Repo, pr *github.PullRequest) {
				pr.Commits = append(pr.Commits, github.Commit{OID: "abcdef123", Authors: []github.Author{{Login: "sglavoie"}}})
				pr.CommitCount = 2
			}},
		{name: "unknown update type", action: ActionSkip, reason: "cannot determine",
			mutate: func(_ *github.Repo, pr *github.PullRequest) { pr.Commits[0].Message = "bump" }},
		{name: "draft", action: ActionSkip, reason: "draft",
			mutate: func(_ *github.Repo, pr *github.PullRequest) { pr.IsDraft = true }},
		{name: "conflicting", action: ActionSkip, reason: "conflicts",
			mutate: func(_ *github.Repo, pr *github.PullRequest) { pr.Mergeable = "CONFLICTING" }},
		{name: "mergeability unknown", action: ActionWait, reason: "mergeability",
			mutate: func(_ *github.Repo, pr *github.PullRequest) { pr.Mergeable = "UNKNOWN" }},
		{name: "behind asks rebase", action: ActionRebase,
			mutate: func(_ *github.Repo, pr *github.PullRequest) { pr.MergeState = "BEHIND" }},
		{name: "behind with our request waits", action: ActionWait, reason: "rebase already requested",
			mutate: func(_ *github.Repo, pr *github.PullRequest) {
				pr.MergeState = "BEHIND"
				pr.Comments = []github.Comment{{Author: "me", Body: RebaseCommand, CreatedAt: testutil.T0.Add(1)}}
			}},
		{name: "behind with stale request asks again", action: ActionRebase,
			mutate: func(_ *github.Repo, pr *github.PullRequest) {
				pr.MergeState = "BEHIND"
				pr.Comments = []github.Comment{{Author: "me", Body: RebaseCommand, CreatedAt: testutil.T0.Add(-1)}}
			}},
		{name: "review required", action: ActionSkip, reason: "approving review",
			mutate: func(_ *github.Repo, pr *github.PullRequest) {
				pr.MergeState = "BLOCKED"
				pr.ReviewDecision = "REVIEW_REQUIRED"
			}},
		{name: "unstable is not merged", action: ActionSkip, reason: "merge state UNSTABLE",
			mutate: func(_ *github.Repo, pr *github.PullRequest) { pr.MergeState = "UNSTABLE" }},
		{name: "read-only repo", action: ActionSkip, reason: "would merge, but you only have read access",
			mutate: func(r *github.Repo, _ *github.PullRequest) { r.Permission = "READ" }},
		{name: "archived repo", action: ActionSkip, reason: "archived",
			mutate: func(r *github.Repo, _ *github.PullRequest) { r.Archived = true }},
		{name: "unsigned commit", action: ActionSkip, reason: "not signed by GitHub",
			mutate: func(_ *github.Repo, pr *github.PullRequest) { pr.Commits[0].SignedByGitHub = false }},
		{name: "forged dependabot author without signature", action: ActionSkip, reason: "not signed by GitHub",
			mutate: func(_ *github.Repo, pr *github.PullRequest) {
				pr.Commits[0].SignedByGitHub = false
				pr.Commits[0].Authors = []github.Author{{Name: "dependabot[bot]", Email: "49699333+dependabot[bot]@users.noreply.github.com"}}
			}},
		{name: "authors truncated", action: ActionSkip, reason: "too many authors",
			mutate: func(_ *github.Repo, pr *github.PullRequest) { pr.Commits[0].AuthorCount = 6 }},
		{name: "checks truncated", action: ActionSkip, reason: "too many checks to verify (101)",
			mutate: func(_ *github.Repo, pr *github.PullRequest) { pr.CheckCount = 101 }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repo := testutil.Repo(testutil.PR(1, c.deps...))
			if c.mutate != nil {
				c.mutate(repo, &repo.PullRequests[0])
			}
			p := defaultPolicy
			if c.policy != nil {
				c.policy(&p)
			}
			d := one(t, repo, p)
			if d.Action != c.action || !strings.Contains(d.Reason, c.reason) {
				t.Fatalf("got %s %q, want %s containing %q", d.Action, d.Reason, c.action, c.reason)
			}
		})
	}
}

func TestEvaluateIgnoresNonDependabotPRs(t *testing.T) {
	human := testutil.PR(2)
	human.Author = "sglavoie"
	ds := Evaluate(testutil.Repo(testutil.PR(1), human), defaultPolicy, "me")
	if len(ds) != 1 || ds[0].Number != 1 {
		t.Fatalf("decisions = %+v", ds)
	}
}

func TestMergeMethodFallsBack(t *testing.T) {
	repo := &github.Repo{FullName: "o/r", MergeAllowed: true}
	if m, err := MergeMethod(repo, "squash"); err != nil || m != "merge" {
		t.Fatalf("got %q %v", m, err)
	}
	if _, err := MergeMethod(&github.Repo{FullName: "o/r"}, "squash"); err == nil {
		t.Fatal("expected error")
	}
}
