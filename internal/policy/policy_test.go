package policy

import (
	"strings"
	"testing"
	"time"

	"github.com/sglavoie/deps-merger/internal/config"
	"github.com/sglavoie/deps-merger/internal/github"
	"github.com/sglavoie/deps-merger/internal/testutil"
)

var defaultPolicy = config.Builtin

func one(t *testing.T, repo *github.Repo, p config.Policy) Decision {
	t.Helper()
	ds := Evaluate(repo, p, "me", nil)
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
		{name: "no checks on a new commit waits for CI to start", action: ActionWait, reason: "no checks reported yet",
			mutate: func(_ *github.Repo, pr *github.PullRequest) {
				pr.Checks = nil
				pr.Commits[0].CommittedDate = testutil.Fetched.Add(-time.Minute)
			}},
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
		{name: "behind with an unanswered request is left for a human", action: ActionSkip, reason: "rebase requested 2 days ago and not done",
			mutate: func(r *github.Repo, pr *github.PullRequest) {
				pr.MergeState = "BEHIND"
				pr.Comments = []github.Comment{{Author: "me", Body: RebaseCommand, CreatedAt: testutil.T0.Add(1)}}
				r.FetchedAt = testutil.T0.Add(50 * time.Hour)
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
		{name: "human co-author", action: ActionSkip, reason: "modified by Jane",
			mutate: func(_ *github.Repo, pr *github.PullRequest) {
				pr.Commits[0].Authors = append(pr.Commits[0].Authors, github.Author{Name: "Jane", Email: "jane@example.com"})
			}},
		{name: "checks truncated", action: ActionSkip, reason: "too many checks to verify (101)",
			mutate: func(_ *github.Repo, pr *github.PullRequest) { pr.CheckCount = 101 }},
		{name: "stale check", action: ActionSkip, reason: "stale checks: build",
			mutate: func(_ *github.Repo, pr *github.PullRequest) { pr.Checks[0].State = github.CheckStale }},
		{name: "checks from another commit", action: ActionWait, reason: "checks not yet reported for head commit sha1",
			mutate: func(_ *github.Repo, pr *github.PullRequest) { pr.ChecksOID = "older" }},
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
	ds := Evaluate(testutil.Repo(testutil.PR(1), human), defaultPolicy, "me", nil)
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

func TestEvaluateQueuesRebases(t *testing.T) {
	behind := func(n int) github.PullRequest {
		pr := testutil.PR(n)
		pr.MergeState = "BEHIND"
		return pr
	}
	actions := func(ds []Decision) string {
		var parts []string
		for _, d := range ds {
			parts = append(parts, string(d.Action)+" "+d.Reason)
		}
		return strings.Join(parts, "; ")
	}

	ds := Evaluate(testutil.Repo(behind(1), behind(2), behind(3)), defaultPolicy, "me", nil)
	if got, want := actions(ds), "rebase behind base branch; wait queued behind #1; wait queued behind #1"; got != want {
		t.Errorf("all behind:\n got %s\nwant %s", got, want)
	}

	ds = Evaluate(testutil.Repo(behind(1), testutil.PR(2)), defaultPolicy, "me", nil)
	if got, want := actions(ds), "wait queued behind #2; merge checks green, within policy"; got != want {
		t.Errorf("merge pending:\n got %s\nwant %s", got, want)
	}

	requested := behind(1)
	requested.Comments = []github.Comment{{Author: "me", Body: RebaseCommand, CreatedAt: testutil.T0.Add(1)}}
	ds = Evaluate(testutil.Repo(requested, behind(2)), defaultPolicy, "me", nil)
	if got, want := actions(ds), "wait rebase already requested; wait queued behind #1"; got != want {
		t.Errorf("rebase in flight:\n got %s\nwant %s", got, want)
	}

	stalled := testutil.Repo(requested, behind(2))
	stalled.FetchedAt = testutil.T0.Add(25 * time.Hour)
	ds = Evaluate(stalled, defaultPolicy, "me", nil)
	if got, want := actions(ds), "skip rebase requested a day ago and not done; left for a human; rebase behind base branch"; got != want {
		t.Errorf("rebase stalled:\n got %s\nwant %s", got, want)
	}

	// Just rebased: up to date, CI running; it merges next.
	rebased := testutil.PR(1)
	rebased.Checks[0].State = github.CheckPending
	rebased.MergeState = "BLOCKED"
	ds = Evaluate(testutil.Repo(rebased, behind(2)), defaultPolicy, "me", nil)
	if got, want := actions(ds), "wait 1 check(s) still running; wait queued behind #1"; got != want {
		t.Errorf("CI running:\n got %s\nwant %s", got, want)
	}

	pushed := testutil.PR(1)
	pushed.ChecksOID = "older"
	ds = Evaluate(testutil.Repo(pushed, behind(2)), defaultPolicy, "me", nil)
	if got, want := actions(ds), "wait checks not yet reported for head commit sha1; wait queued behind #1"; got != want {
		t.Errorf("checks not reported:\n got %s\nwant %s", got, want)
	}

	// Neither a held major nor a PR that is itself behind will merge next.
	major := testutil.PR(1, "lib:major")
	major.ChecksOID = "older"
	stillBehind := behind(2)
	stillBehind.Checks[0].State = github.CheckPending
	ds = Evaluate(testutil.Repo(major, stillBehind, behind(3)), defaultPolicy, "me", nil)
	if got, want := actions(ds), "wait checks not yet reported for head commit sha1; wait 1 check(s) still running; rebase behind base branch"; got != want {
		t.Errorf("not in flight:\n got %s\nwant %s", got, want)
	}
}

func TestEvaluateAgentReview(t *testing.T) {
	approved := func(Decision) *Verdict { return &Verdict{Approved: true, Summary: "fine"} }
	rejected := func(Decision) *Verdict { return &Verdict{Summary: "breaks API"} }
	failed := func(Decision) *Verdict { return &Verdict{Error: "timed out"} }
	unreviewed := func(Decision) *Verdict { return nil }
	cases := []struct {
		name   string
		deps   []string
		mutate func(*github.Repo, *github.PullRequest)
		policy func(*config.Policy)
		judge  Judge
		action Action
		reason string
	}{
		{name: "major awaits review", deps: []string{"lib:major"}, judge: unreviewed,
			action: ActionReview, reason: "major update exceeds limit (minor); awaiting agent review"},
		{name: "unknown grade awaits review", judge: unreviewed, action: ActionReview, reason: "cannot determine update type",
			mutate: func(_ *github.Repo, pr *github.PullRequest) { pr.Commits[0].Message = "bump" }},
		{name: "approved major merges", deps: []string{"lib:major"}, judge: approved,
			action: ActionMerge, reason: "checks green, major update approved by agent"},
		{name: "approved unknown merges", judge: approved, action: ActionMerge, reason: "unknown update approved by agent",
			mutate: func(_ *github.Repo, pr *github.PullRequest) { pr.Commits[0].Message = "bump" }},
		{name: "approved behind rebases", deps: []string{"lib:major"}, judge: approved,
			action: ActionRebase, reason: "behind base branch (major update approved by agent)",
			mutate: func(_ *github.Repo, pr *github.PullRequest) { pr.MergeState = "BEHIND" }},
		{name: "rejected is skipped", deps: []string{"lib:major"}, judge: rejected,
			action: ActionSkip, reason: "major update exceeds limit (minor); rejected by agent"},
		{name: "failed review is skipped", deps: []string{"lib:major"}, judge: failed,
			action: ActionSkip, reason: "agent review failed: timed out"},
		{name: "repo opted out", deps: []string{"lib:major"}, judge: approved,
			policy: func(p *config.Policy) { p.AgentReview = false },
			action: ActionSkip, reason: "major update exceeds limit (minor)"},
		{name: "within policy ignores judge", judge: rejected, action: ActionMerge, reason: "checks green, within policy"},
		{name: "approval does not excuse failing checks", deps: []string{"lib:major"}, judge: approved,
			action: ActionSkip, reason: "failing checks: build",
			mutate: func(_ *github.Repo, pr *github.PullRequest) { pr.Checks[0].State = github.CheckFailed }},
		{name: "approval does not excuse a human commit", deps: []string{"lib:major"}, judge: approved,
			action: ActionSkip, reason: "not signed by GitHub",
			mutate: func(_ *github.Repo, pr *github.PullRequest) { pr.Commits[0].SignedByGitHub = false }},
		{name: "read-only repo is not reviewed", deps: []string{"lib:major"}, judge: unreviewed,
			action: ActionSkip, reason: "would review, but you only have read access",
			mutate: func(r *github.Repo, _ *github.PullRequest) { r.Permission = "READ" }},
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
			ds := Evaluate(repo, p, "me", c.judge)
			if d := ds[0]; d.Action != c.action || !strings.Contains(d.Reason, c.reason) {
				t.Fatalf("got %s %q, want %s containing %q", d.Action, d.Reason, c.action, c.reason)
			}
		})
	}
}

func TestEvaluateUnapprovedDoesNotHoldRebaseQueue(t *testing.T) {
	major := testutil.PR(1, "lib:major")
	major.Checks[0].State = github.CheckPending
	behind := testutil.PR(2)
	behind.MergeState = "BEHIND"
	unreviewed := func(Decision) *Verdict { return nil }
	ds := Evaluate(testutil.Repo(major, behind), defaultPolicy, "me", unreviewed)
	if ds[1].Action != ActionRebase {
		t.Fatalf("unreviewed major held the queue: %s %q", ds[1].Action, ds[1].Reason)
	}
	approved := func(Decision) *Verdict { return &Verdict{Approved: true} }
	ds = Evaluate(testutil.Repo(major, behind), defaultPolicy, "me", approved)
	if ds[1].Action != ActionWait || ds[1].Reason != "queued behind #1" {
		t.Fatalf("approved major should hold the queue: %s %q", ds[1].Action, ds[1].Reason)
	}
}
