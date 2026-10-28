package policy

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sglavoie/deps-merger/internal/testutil"
	"github.com/sglavoie/deps-merger/pkg/config"
	"github.com/sglavoie/deps-merger/pkg/github"
)

var defaultPolicy = config.Builtin

// actions is the GitHub Actions app's id, behind every workflow's checks.
const actions = 15368

// chatter is a page of other people's comments, the first made at from.
func chatter(from time.Time) []github.Comment {
	var out []github.Comment
	for i := range 50 {
		out = append(out, github.Comment{Author: "coverage-bot", Body: "coverage: 81%", CreatedAt: from.Add(time.Duration(i) * time.Minute)})
	}
	return out
}

func one(t *testing.T, repo *github.Repo, p config.Policy) Decision {
	t.Helper()
	ds := Evaluate(repo, p, "me", nil)
	if len(ds) != 1 {
		t.Fatalf("got %d decisions", len(ds))
	}
	return ds[0]
}

func TestRebaseRequestMatchesViewerIgnoringCase(t *testing.T) {
	for _, tc := range []struct {
		name, author, viewer string
		want                 Action
	}{
		{"same login", "me", "me", ActionWait},
		{"different capitalization", "Me", "me", ActionWait},
		{"another user", "someone-else", "me", ActionRebase},
		{"missing identities", "", "", ActionRebase},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pr := testutil.PR(1)
			pr.MergeState = "BEHIND"
			pr.Comments = []github.Comment{{Author: tc.author, Body: RebaseCommand, CreatedAt: testutil.T0.Add(time.Minute)}}
			d := Evaluate(testutil.Repo(pr), defaultPolicy, tc.viewer, nil)[0]
			if d.Action != tc.want {
				t.Fatalf("author %q, viewer %q: %+v; want %s", tc.author, tc.viewer, d, tc.want)
			}
		})
	}
}

func TestMissingCommitsCannotBeApprovedByAgent(t *testing.T) {
	pr := testutil.PR(1)
	pr.Commits = nil
	pr.CommitCount = 0
	for _, judge := range []Judge{approveAll, nil} {
		ds := Evaluate(testutil.Repo(pr), defaultPolicy, "me", judge)
		if len(ds) != 1 || ds[0].Action != ActionSkip || !strings.Contains(ds[0].Reason, "no commits") {
			t.Fatalf("missing commits must block the PR even with agent approval: %+v", ds)
		}
	}
}

func TestUnnamedDependencyCannotMergeWithoutReview(t *testing.T) {
	pr := testutil.PR(1)
	pr.Commits[0].Message = strings.ReplaceAll(pr.Commits[0].Message, "dependency-name: lib", "dependency-name: ''")
	d := one(t, testutil.Repo(pr), defaultPolicy)
	if d.Grade != config.UpdateUnknown || d.Action != ActionSkip {
		t.Fatalf("unnamed dependency authorized a merge: %+v", d)
	}
	ds := Evaluate(testutil.Repo(pr), defaultPolicy, "me", func(Decision) *Verdict { return nil })
	if ds[0].Action != ActionReview {
		t.Fatalf("unreadable metadata should require review: %+v", ds[0])
	}
}

func TestExtraMetadataDocumentCannotAuthorizeMerge(t *testing.T) {
	pr := testutil.PR(1)
	pr.Commits[0].Message = strings.Replace(pr.Commits[0].Message, "\n...", "\n---\nother: metadata\n...", 1)
	d := one(t, testutil.Repo(pr), defaultPolicy)
	if d.Grade != config.UpdateUnknown || d.Action != ActionSkip {
		t.Fatalf("partial metadata authorized a merge: %+v", d)
	}
	ds := Evaluate(testutil.Repo(pr), defaultPolicy, "me", func(Decision) *Verdict { return nil })
	if ds[0].Action != ActionReview {
		t.Fatalf("unreadable metadata should require review: %+v", ds[0])
	}
}

func TestNoChecksRequiresKnownHeadAge(t *testing.T) {
	for _, size := range []string{"patch", "major"} {
		for _, state := range []string{"CLEAN", "BEHIND"} {
			for _, skipped := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/skipped=%t", size, state, skipped), func(t *testing.T) {
					pr := testutil.PR(1, "lib:"+size)
					pr.MergeState = state
					pr.Commits[0].CommittedDate = time.Time{}
					pr.Checks = nil
					if skipped {
						pr.Checks = []github.Check{{Name: "test", State: github.CheckSkipped}}
					}
					p := defaultPolicy
					p.MergeWithoutChecksAfter = time.Hour
					d := Evaluate(testutil.Repo(pr), p, "me", approveAll)[0]
					if d.Action != ActionWait || !strings.Contains(d.Reason, "timestamp") {
						t.Fatalf("unknown head age must wait, even with approval: %+v", d)
					}
				})
			}
		}
	}
}

func TestNoChecksUsesHeadCommitAge(t *testing.T) {
	pr := testutil.PR(1)
	older := pr.Commits[0]
	older.OID = "older"
	pr.Commits[0].CommittedDate = testutil.Fetched.Add(-time.Minute)
	// The head must determine the grace period, regardless of list order.
	pr.Commits = append(pr.Commits, older)
	pr.CommitCount = len(pr.Commits)
	pr.Checks = nil
	p := defaultPolicy
	p.MergeWithoutChecksAfter = time.Hour
	if d := one(t, testutil.Repo(pr), p); d.Action != ActionWait {
		t.Fatalf("a fresh head must not use an older commit's age: %+v", d)
	}
}

func TestMissingHeadAgeDoesNotBlockTestedOrChecksOptionalPR(t *testing.T) {
	pr := testutil.PR(1)
	pr.Commits[0].CommittedDate = time.Time{}
	if d := one(t, testutil.Repo(pr), defaultPolicy); d.Action != ActionMerge {
		t.Fatalf("passing checks do not need an age exemption: %+v", d)
	}
	pr.Checks = nil
	p := defaultPolicy
	p.RequireChecks = false
	if d := one(t, testutil.Repo(pr), p); d.Action != ActionMerge {
		t.Fatalf("optional checks do not need an age exemption: %+v", d)
	}
}

func TestUnknownCheckStateCannotMerge(t *testing.T) {
	for _, state := range []github.CheckState{"", "unknown"} {
		for _, require := range []bool{true, false} {
			for _, size := range []string{"patch", "major"} {
				pr := testutil.PR(1, "lib:"+size)
				pr.Checks[0].State = state
				p := defaultPolicy
				p.RequireChecks = require
				ds := Evaluate(testutil.Repo(pr), p, "me", approveAll)
				if ds[0].Action != ActionSkip || !strings.Contains(ds[0].Reason, "unknown check state") {
					t.Errorf("state %q, require_checks %t, size %s: %+v", state, require, size, ds[0])
				}
			}
		}
	}
}

func TestUnknownMergeabilityCannotAuthorizeAction(t *testing.T) {
	for _, state := range []string{"CLEAN", "HAS_HOOKS", "BEHIND"} {
		for _, size := range []string{"patch", "major"} {
			t.Run(state+"/"+size, func(t *testing.T) {
				pr := testutil.PR(1, "lib:"+size)
				pr.Mergeable, pr.MergeState = "UNRECOGNIZED", state
				d := Evaluate(testutil.Repo(pr), defaultPolicy, "me", approveAll)[0]
				if d.Action != ActionSkip || !strings.Contains(d.Reason, "unknown mergeability") {
					t.Fatalf("unrecognized mergeability authorized an action: %+v", d)
				}
			})
		}
	}
}

func TestIncompleteCheckIdentityCannotHideFailure(t *testing.T) {
	cases := []struct {
		name string
		omit func(*github.Check)
	}{
		{"suite", func(c *github.Check) { c.Suite = 0 }},
		{"app", func(c *github.Check) { c.App = 0 }},
		{"workflow", func(c *github.Check) { c.Workflow = 0 }},
		{"event", func(c *github.Check) { c.Event = "" }},
		{"name", func(c *github.Check) { c.Name = "" }},
	}
	for _, tc := range cases {
		for _, size := range []string{"patch", "major"} {
			t.Run(tc.name+"/"+size, func(t *testing.T) {
				pr := testutil.PR(1, "lib:"+size)
				pr.Checks = []github.Check{
					{Name: "test", State: github.CheckFailed, Suite: 1, App: actions, Workflow: 7, Event: "pull_request"},
					{Name: "test", State: github.CheckPassed, Suite: 2, App: actions, Workflow: 7, Event: "pull_request"},
				}
				tc.omit(&pr.Checks[0])
				if tc.name != "suite" {
					tc.omit(&pr.Checks[1])
				}
				p := defaultPolicy
				p.RequireChecks = false
				d := Evaluate(testutil.Repo(pr), p, "me", approveAll)[0]
				if d.Action != ActionSkip || !strings.Contains(d.Reason, "failing checks") {
					t.Fatalf("incomplete check identity hid a failure: %+v", d)
				}
				if got := LatestChecks(pr.Checks); len(got) != 2 {
					t.Fatalf("review and diagnosis also need both checks: %+v", got)
				}
			})
		}
	}
}

func TestNewerCheckCannotHideUnknownState(t *testing.T) {
	for _, state := range []github.CheckState{"", github.CheckUnknown, "future-state"} {
		for _, size := range []string{"patch", "major"} {
			pr := testutil.PR(1, "lib:"+size)
			pr.Checks = []github.Check{
				{Name: "build", State: state, Suite: 1, App: actions, Workflow: 7, Event: "pull_request"},
				{Name: "build", State: github.CheckPassed, Suite: 2, App: actions, Workflow: 7, Event: "pull_request"},
			}
			if got := LatestChecks(pr.Checks); len(got) != 2 {
				t.Fatalf("unknown check must survive supersession: %+v", got)
			}
			p := defaultPolicy
			p.RequireChecks = false
			ds := Evaluate(testutil.Repo(pr), p, "me", approveAll)
			if ds[0].Action != ActionSkip || !strings.Contains(ds[0].Reason, "unknown check state") {
				t.Errorf("state %q, size %s: newer check hid an unknown state: %+v", state, size, ds[0])
			}
		}
	}
}

func TestUnverifiedHeadCannotMergeOrBeReviewed(t *testing.T) {
	for _, state := range []string{"CLEAN", "BEHIND"} {
		for _, size := range []string{"patch", "major"} {
			t.Run(state+"/"+size, func(t *testing.T) {
				pr := testutil.PR(1, "lib:"+size)
				pr.HeadOID, pr.ChecksOID = "new-head", "new-head"
				pr.MergeState = state
				called := false
				ds := Evaluate(testutil.Repo(pr), defaultPolicy, "me", func(Decision) *Verdict {
					called = true
					return &Verdict{Approved: true}
				})
				if ds[0].Action != ActionWait || ds[0].assessed || called {
					t.Fatalf("unverified head must wait before review or diagnosis: %+v, judge called: %t", ds[0], called)
				}
			})
		}
	}
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
		{name: "check cancelled in favour of a newer run merges", action: ActionMerge,
			mutate: func(_ *github.Repo, pr *github.PullRequest) {
				pr.Checks = []github.Check{
					{Name: "build", State: github.CheckFailed, Suite: 1, App: actions, Workflow: 7, Event: "pull_request"},
					{Name: "build", State: github.CheckPassed, Suite: 2, App: actions, Workflow: 7, Event: "pull_request"},
				}
			}},
		{name: "a newer run that was skipped does not hide a failure", action: ActionSkip, reason: "failing checks: test",
			mutate: func(_ *github.Repo, pr *github.PullRequest) {
				pr.Checks = []github.Check{
					{Name: "lint", State: github.CheckPassed, Suite: 1, App: actions, Workflow: 7, Event: "pull_request"},
					{Name: "test", State: github.CheckFailed, Suite: 1, App: actions, Workflow: 7, Event: "pull_request"},
					{Name: "test", State: github.CheckSkipped, Suite: 2, App: actions, Workflow: 7, Event: "pull_request"},
				}
			}},
		{name: "same job name in another workflow is not a rerun", action: ActionSkip, reason: "failing checks: test",
			mutate: func(_ *github.Repo, pr *github.PullRequest) {
				pr.Checks = []github.Check{
					{Name: "test", State: github.CheckFailed, Suite: 1, App: actions, Workflow: 7, Event: "pull_request"},
					{Name: "test", State: github.CheckPassed, Suite: 2, App: actions, Workflow: 8, Event: "pull_request"},
				}
			}},
		{name: "same job name pending in another workflow waits", action: ActionWait, reason: "1 check(s) still running",
			mutate: func(_ *github.Repo, pr *github.PullRequest) {
				pr.Checks = []github.Check{
					{Name: "test", State: github.CheckPending, Suite: 1, App: actions, Workflow: 7, Event: "pull_request"},
					{Name: "test", State: github.CheckPassed, Suite: 2, App: actions, Workflow: 8, Event: "pull_request"},
				}
			}},
		{name: "the same workflow run for another event is not a rerun", action: ActionSkip, reason: "failing checks: test",
			mutate: func(_ *github.Repo, pr *github.PullRequest) {
				pr.Checks = []github.Check{
					{Name: "test", State: github.CheckFailed, Suite: 1, App: actions, Workflow: 7, Event: "push"},
					{Name: "test", State: github.CheckPassed, Suite: 2, App: actions, Workflow: 7, Event: "pull_request"},
				}
			}},
		{name: "same check name from another app is not a rerun", action: ActionSkip, reason: "failing checks: lint",
			mutate: func(_ *github.Repo, pr *github.PullRequest) {
				pr.Checks = []github.Check{
					{Name: "lint", State: github.CheckFailed, Suite: 1, App: 99},
					{Name: "lint", State: github.CheckPassed, Suite: 2, App: actions, Workflow: 7, Event: "pull_request"},
				}
			}},
		{name: "a check run does not supersede a status of the same name", action: ActionSkip, reason: "failing checks: ci",
			mutate: func(_ *github.Repo, pr *github.PullRequest) {
				pr.Checks = []github.Check{
					{Name: "ci", State: github.CheckFailed, Status: true},
					{Name: "ci", State: github.CheckPassed, Suite: 2, App: actions, Workflow: 7, Event: "pull_request"},
				}
			}},
		{name: "a pending status is not superseded either", action: ActionWait, reason: "1 check(s) still running",
			mutate: func(_ *github.Repo, pr *github.PullRequest) {
				pr.Checks = []github.Check{
					{Name: "ci", State: github.CheckPending, Status: true},
					{Name: "ci", State: github.CheckPassed, Suite: 2, App: actions, Workflow: 7, Event: "pull_request"},
				}
			}},
		{name: "check failing in the newer run", action: ActionSkip, reason: "failing checks: build",
			mutate: func(_ *github.Repo, pr *github.PullRequest) {
				pr.Checks = []github.Check{
					{Name: "build", State: github.CheckPassed, Suite: 1},
					{Name: "build", State: github.CheckFailed, Suite: 2},
				}
			}},
		{name: "matrix job failing in the same run", action: ActionSkip, reason: "failing checks: build",
			mutate: func(_ *github.Repo, pr *github.PullRequest) {
				pr.Checks = []github.Check{
					{Name: "build", State: github.CheckPassed, Suite: 2},
					{Name: "build", State: github.CheckFailed, Suite: 2},
				}
			}},
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
		{name: "no checks waits for merge_without_checks_after", action: ActionWait, reason: "merging without them in 5m",
			mutate: func(_ *github.Repo, pr *github.PullRequest) {
				pr.Checks = nil
				pr.Commits[0].CommittedDate = testutil.Fetched.Add(-35 * time.Minute)
			},
			policy: func(p *config.Policy) { p.MergeWithoutChecksAfter = 40 * time.Minute }},
		{name: "no checks after merge_without_checks_after merges", action: ActionMerge, reason: "no checks ran",
			mutate: func(_ *github.Repo, pr *github.PullRequest) {
				pr.Checks = nil
				pr.Commits[0].CommittedDate = testutil.Fetched.Add(-41 * time.Minute)
			},
			policy: func(p *config.Policy) { p.MergeWithoutChecksAfter = 40 * time.Minute }},
		{name: "merge_without_checks_after countdown never reads 0s", action: ActionWait, reason: "merging without them in 1m",
			mutate: func(_ *github.Repo, pr *github.PullRequest) {
				pr.Checks = nil
				pr.Commits[0].CommittedDate = testutil.Fetched.Add(-40*time.Minute + 20*time.Second)
			},
			policy: func(p *config.Policy) { p.MergeWithoutChecksAfter = 40 * time.Minute }},
		{name: "merge_without_checks_after waits at least for CI to start", action: ActionWait, reason: "merging without them in 19m",
			mutate: func(_ *github.Repo, pr *github.PullRequest) {
				pr.Checks = nil
				pr.Commits[0].CommittedDate = testutil.Fetched.Add(-11 * time.Minute)
			},
			policy: func(p *config.Policy) { p.MergeWithoutChecksAfter = 10 * time.Minute }},
		{name: "only skipped checks is no checks", action: ActionSkip, reason: "no checks ran",
			mutate: func(_ *github.Repo, pr *github.PullRequest) {
				pr.Checks = []github.Check{{Name: "build", State: github.CheckSkipped}, {Name: "sonar", State: github.CheckSkipped}}
			}},
		{name: "only skipped checks on a new commit waits for CI to start", action: ActionWait, reason: "only skipped checks reported yet for a new commit",
			mutate: func(_ *github.Repo, pr *github.PullRequest) {
				pr.Checks[0].State = github.CheckSkipped
				pr.Commits[0].CommittedDate = testutil.Fetched.Add(-time.Minute)
			}},
		{name: "only skipped checks with opt-in merges", action: ActionMerge, reason: "no checks ran, within policy",
			mutate: func(_ *github.Repo, pr *github.PullRequest) { pr.Checks[0].State = github.CheckSkipped },
			policy: func(p *config.Policy) { p.RequireChecks = false }},
		{name: "a skipped check beside one that passed merges", action: ActionMerge, reason: "checks green, within policy",
			mutate: func(_ *github.Repo, pr *github.PullRequest) {
				pr.Checks = append(pr.Checks, github.Check{Name: "deploy", State: github.CheckSkipped})
			}},
		{name: "merge_without_checks_after ignores failing checks", action: ActionSkip, reason: "failing checks: build",
			mutate: func(_ *github.Repo, pr *github.PullRequest) { pr.Checks[0].State = github.CheckFailed },
			policy: func(p *config.Policy) { p.MergeWithoutChecksAfter = time.Minute }},
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
		{name: "conflicting after the base moved waits for Dependabot", action: ActionWait, reason: "merge conflicts since the base branch moved 2m ago; waiting for Dependabot to rebase",
			mutate: func(_ *github.Repo, pr *github.PullRequest) {
				pr.Mergeable = "CONFLICTING"
				pr.BaseMovedAt = testutil.Fetched.Add(-2 * time.Minute)
			}},
		{name: "dirty after the base moved waits for Dependabot", action: ActionWait, reason: "waiting for Dependabot to rebase",
			mutate: func(_ *github.Repo, pr *github.PullRequest) {
				pr.MergeState = "DIRTY"
				pr.BaseMovedAt = testutil.Fetched.Add(-2 * time.Minute)
			}},
		{name: "conflicting long after the base moved is left for a human", action: ActionSkip, reason: "merge conflicts (Dependabot normally rebases",
			mutate: func(_ *github.Repo, pr *github.PullRequest) {
				pr.Mergeable = "CONFLICTING"
				pr.BaseMovedAt = testutil.Fetched.Add(-31 * time.Minute)
			}},
		{name: "conflicting with a base older than the PR is left for a human", action: ActionSkip, reason: "merge conflicts",
			mutate: func(r *github.Repo, pr *github.PullRequest) {
				pr.Mergeable = "CONFLICTING"
				pr.BaseMovedAt = testutil.T0.Add(-time.Minute)
				r.FetchedAt = testutil.T0.Add(time.Minute)
			}},
		// GitHub runs no pull_request workflow on a PR in conflict.
		{name: "conflicting without checks is a conflict", action: ActionSkip, reason: "merge conflicts (Dependabot normally rebases",
			mutate: func(_ *github.Repo, pr *github.PullRequest) {
				pr.Checks = nil
				pr.Mergeable = "CONFLICTING"
			}},
		{name: "dirty without checks after the base moved waits for Dependabot", action: ActionWait, reason: "waiting for Dependabot to rebase",
			mutate: func(_ *github.Repo, pr *github.PullRequest) {
				pr.Checks = nil
				pr.MergeState = "DIRTY"
				pr.BaseMovedAt = testutil.Fetched.Add(-2 * time.Minute)
			},
			policy: func(p *config.Policy) { p.MergeWithoutChecksAfter = time.Hour }},
		{name: "a second commit without metadata makes the size unknown", action: ActionSkip, reason: "cannot determine",
			mutate: func(_ *github.Repo, pr *github.PullRequest) {
				c := pr.Commits[0]
				c.OID = "c1b"
				c.Message = "some other change\n\nno metadata here"
				pr.Commits = append(pr.Commits, c)
				pr.CommitCount = 2
			}},
		{name: "mergeability unknown", action: ActionWait, reason: "mergeability",
			mutate: func(_ *github.Repo, pr *github.PullRequest) { pr.Mergeable = "UNKNOWN" }},
		{name: "behind asks rebase", action: ActionRebase,
			mutate: func(_ *github.Repo, pr *github.PullRequest) { pr.MergeState = "BEHIND" }},
		{name: "behind with our request waits", action: ActionWait, reason: "rebase already requested",
			mutate: func(_ *github.Repo, pr *github.PullRequest) {
				pr.MergeState = "BEHIND"
				pr.Comments = []github.Comment{{Author: "me", Body: RebaseCommand, CreatedAt: testutil.T0.Add(1)}}
			}},
		{name: "behind with our request at the head timestamp waits", action: ActionWait, reason: "rebase already requested",
			mutate: func(_ *github.Repo, pr *github.PullRequest) {
				pr.MergeState = "BEHIND"
				pr.Comments = []github.Comment{{Author: "me", Body: RebaseCommand, CreatedAt: testutil.T0}}
			}},
		{name: "behind with an unanswered request is left for a human", action: ActionSkip, reason: "rebase requested 2 days ago and not done",
			mutate: func(r *github.Repo, pr *github.PullRequest) {
				pr.MergeState = "BEHIND"
				pr.Comments = []github.Comment{{Author: "me", Body: RebaseCommand, CreatedAt: testutil.T0.Add(1)}}
				r.FetchedAt = testutil.T0.Add(50 * time.Hour)
			}},
		{name: "behind with a request maybe among comments not read waits", action: ActionWait, reason: "rebase may have been requested (older comments not read)",
			mutate: func(_ *github.Repo, pr *github.PullRequest) {
				pr.MergeState = "BEHIND"
				pr.Comments, pr.CommentCount = chatter(testutil.T0.Add(time.Minute)), 80
			}},
		{name: "behind with comments cut off at the head timestamp waits", action: ActionWait, reason: "rebase may have been requested (older comments not read)",
			mutate: func(_ *github.Repo, pr *github.PullRequest) {
				pr.MergeState = "BEHIND"
				pr.Comments, pr.CommentCount = chatter(testutil.T0), 80
			}},
		{name: "behind with a request maybe not read for a day is left for a human", action: ActionSkip,
			reason: "rebase may have been requested (older comments not read) 2 days ago and not done",
			mutate: func(r *github.Repo, pr *github.PullRequest) {
				pr.MergeState = "BEHIND"
				pr.Comments, pr.CommentCount = chatter(testutil.T0.Add(time.Minute)), 80
				r.FetchedAt = testutil.T0.Add(50 * time.Hour)
			}},
		{name: "behind with comments cut off before the commit asks", action: ActionRebase,
			mutate: func(_ *github.Repo, pr *github.PullRequest) {
				pr.MergeState = "BEHIND"
				pr.Comments, pr.CommentCount = chatter(testutil.T0.Add(-time.Minute)), 80
			}},
		{name: "behind with all comments read and none ours asks", action: ActionRebase,
			mutate: func(_ *github.Repo, pr *github.PullRequest) {
				pr.MergeState = "BEHIND"
				pr.Comments = chatter(testutil.T0.Add(time.Minute))
				pr.CommentCount = len(pr.Comments)
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
		{name: "no reported permission", action: ActionSkip, reason: "would merge, but GitHub reports no access for you",
			mutate: func(r *github.Repo, _ *github.PullRequest) { r.Permission = "" }},
		{name: "archived repo", action: ActionSkip, reason: "archived",
			mutate: func(r *github.Repo, _ *github.PullRequest) { r.Archived = true }},
		{name: "unsigned commit", action: ActionSkip, reason: "not signed by GitHub",
			mutate: func(_ *github.Repo, pr *github.PullRequest) { pr.Commits[0].SignedByGitHub = false }},
		{name: "signed commit with another committer", action: ActionSkip, reason: "not committed by GitHub",
			mutate: func(_ *github.Repo, pr *github.PullRequest) { pr.Commits[0].CommittedByGitHub = false }},
		{name: "forged dependabot author without signature", action: ActionSkip, reason: "not linked to Dependabot's account",
			mutate: func(_ *github.Repo, pr *github.PullRequest) {
				pr.Commits[0].SignedByGitHub = false
				pr.Commits[0].Authors = []github.Author{{Name: "dependabot[bot]", Email: "49699333+dependabot[bot]@users.noreply.github.com"}}
			}},
		{name: "signed commit whose author is only named dependabot", action: ActionSkip, reason: `author "dependabot[bot]" is not linked`,
			mutate: func(_ *github.Repo, pr *github.PullRequest) {
				pr.Commits[0].Authors = []github.Author{{Name: "dependabot[bot]", Email: "someone@example.com"}}
			}},
		{name: "signed commit whose author has only dependabot's email", action: ActionSkip, reason: "commit sha1 author is not linked to Dependabot's account",
			mutate: func(_ *github.Repo, pr *github.PullRequest) {
				pr.Commits[0].Authors = []github.Author{{Email: "49699333+dependabot[bot]@users.noreply.github.com"}}
			}},
		{name: "commit without an author", action: ActionSkip, reason: "commit sha1 has no author",
			mutate: func(_ *github.Repo, pr *github.PullRequest) { pr.Commits[0].Authors = nil }},
		{name: "unrecorded author before a dependabot trailer", action: ActionSkip, reason: "not linked to Dependabot's account",
			mutate: func(_ *github.Repo, pr *github.PullRequest) {
				pr.Commits[0].Authors = []github.Author{{}, {Name: "dependabot[bot]", Email: "49699333+dependabot[bot]@users.noreply.github.com"}}
			}},
		{name: "dependabot co-author trailer merges", action: ActionMerge,
			mutate: func(_ *github.Repo, pr *github.PullRequest) {
				pr.Commits[0].Authors = append(pr.Commits[0].Authors, github.Author{Name: "dependabot[bot]", Email: "49699333+dependabot[bot]@users.noreply.github.com"})
			}},
		{name: "dependabot co-author trailer with only an email merges", action: ActionMerge,
			mutate: func(_ *github.Repo, pr *github.PullRequest) {
				pr.Commits[0].Authors = append(pr.Commits[0].Authors, github.Author{Email: "49699333+dependabot[bot]@users.noreply.github.com"})
			}},
		{name: "human co-author", action: ActionSkip, reason: "modified by Jane",
			mutate: func(_ *github.Repo, pr *github.PullRequest) {
				pr.Commits[0].Authors = append(pr.Commits[0].Authors, github.Author{Name: "Jane", Email: "jane@example.com"})
			}},
		{name: "checks truncated", action: ActionSkip, reason: "too many checks to verify (101)",
			mutate: func(_ *github.Repo, pr *github.PullRequest) { pr.CheckCount = 101 }},
		{name: "stale check", action: ActionSkip, reason: "stale checks: build",
			mutate: func(_ *github.Repo, pr *github.PullRequest) { pr.Checks[0].State = github.CheckStale }},
		{name: "failing check is not hidden by a stale one of the same name", action: ActionSkip, reason: "failing checks: test",
			mutate: func(_ *github.Repo, pr *github.PullRequest) {
				pr.Checks = []github.Check{
					{Name: "test", State: github.CheckStale, App: actions, Workflow: 7, Suite: 1},
					{Name: "test", State: github.CheckFailed, App: actions, Workflow: 8, Suite: 2},
				}
			}},
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

	ds = EvaluateAfter(testutil.Repo(testutil.PR(1), behind(2)), defaultPolicy, "me", nil, map[int]bool{1: true})
	if got, want := actions(ds), "merge checks green, within policy; rebase behind base branch"; got != want {
		t.Errorf("merge refused:\n got %s\nwant %s", got, want)
	}

	ds = EvaluateAfter(testutil.Repo(behind(1), behind(2)), defaultPolicy, "me", nil, map[int]bool{1: true})
	if got, want := actions(ds), "rebase behind base branch; rebase behind base branch"; got != want {
		t.Errorf("rebase refused:\n got %s\nwant %s", got, want)
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

	conflicting := testutil.PR(1)
	conflicting.Checks[0].State = github.CheckPending
	conflicting.Mergeable, conflicting.MergeState = "CONFLICTING", "DIRTY"
	ds = Evaluate(testutil.Repo(conflicting, behind(2)), defaultPolicy, "me", nil)
	if got, want := actions(ds), "wait 1 check(s) still running; rebase behind base branch"; got != want {
		t.Errorf("in conflict:\n got %s\nwant %s", got, want)
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

func TestRebaseQueuesAreIndependentByBaseBranch(t *testing.T) {
	for _, state := range []string{"merge", "pending checks", "missing checks", "rebase", "rebase requested", "unknown mergeability"} {
		t.Run(state, func(t *testing.T) {
			first := testutil.PR(1)
			first.BaseRef = "main"
			switch state {
			case "pending checks":
				first.Checks[0].State = github.CheckPending
			case "missing checks":
				first.ChecksOID = "older"
			case "rebase":
				first.MergeState = "BEHIND"
			case "rebase requested":
				first.MergeState = "BEHIND"
				first.Comments = []github.Comment{{Author: "me", Body: RebaseCommand, CreatedAt: testutil.T0.Add(time.Minute)}}
			case "unknown mergeability":
				first.Mergeable = "UNKNOWN"
			}
			behind := func(n int, base string) github.PullRequest {
				pr := testutil.PR(n)
				pr.BaseRef, pr.MergeState = base, "BEHIND"
				return pr
			}
			ds := Evaluate(testutil.Repo(first, behind(2, "release"), behind(3, "main"), behind(4, "release")), defaultPolicy, "me", nil)
			if ds[1].Action != ActionRebase {
				t.Errorf("independent branch: got %s (%s), want rebase", ds[1].Action, ds[1].Reason)
			}
			for _, pair := range []struct{ index, ahead int }{{2, 1}, {3, 2}} {
				d := ds[pair.index]
				if d.Action != ActionWait || d.Reason != fmt.Sprintf("queued behind #%d", pair.ahead) {
					t.Errorf("#%d: got %s (%s), want queued behind #%d", d.Number, d.Action, d.Reason, pair.ahead)
				}
			}
		})
	}
}

func TestRefusedPRCannotHoldRebaseQueueWhileWaiting(t *testing.T) {
	for _, state := range []string{"pending checks", "missing checks", "unknown mergeability", "unknown merge state"} {
		t.Run(state, func(t *testing.T) {
			pr := testutil.PR(1)
			switch state {
			case "pending checks":
				pr.Checks[0].State = github.CheckPending
			case "missing checks":
				pr.ChecksOID = "older"
			case "unknown mergeability":
				pr.Mergeable = "UNKNOWN"
			case "unknown merge state":
				pr.MergeState = "UNKNOWN"
			}
			behind := testutil.PR(2)
			behind.MergeState = "BEHIND"
			repo := testutil.Repo(pr, behind)
			// An update still eligible to merge holds the queue while waiting.
			if ds := Evaluate(repo, defaultPolicy, "me", nil); ds[1].Action != ActionWait {
				t.Fatalf("eligible PR must hold the queue: %+v", ds)
			}
			ds := EvaluateAfter(repo, defaultPolicy, "me", nil, map[int]bool{1: true})
			if ds[0].Action != ActionWait || ds[1].Action != ActionRebase {
				t.Fatalf("a PR whose write failed must release the queue even while waiting: %+v", ds)
			}
		})
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
		{name: "approved major without checks merges", deps: []string{"lib:major"}, judge: approved,
			action: ActionMerge, reason: "no checks ran, major update approved by agent",
			mutate: func(_ *github.Repo, pr *github.PullRequest) { pr.Checks[0].State = github.CheckSkipped },
			policy: func(p *config.Policy) { p.RequireChecks = false }},
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

func TestLatestChecks(t *testing.T) {
	checks := []github.Check{
		{Name: "test", State: github.CheckFailed, Suite: 1, App: actions, Workflow: 7, Event: "pull_request"},
		{Name: "test", State: github.CheckPassed, Suite: 3, App: actions, Workflow: 7, Event: "pull_request"},
		{Name: "test", State: github.CheckPending, Suite: 2, App: actions, Workflow: 8, Event: "pull_request"},
		{Name: "test", State: github.CheckFailed, Status: true},
		{Name: "matrix", State: github.CheckPassed, Suite: 3, App: actions, Workflow: 7, Event: "pull_request"},
		{Name: "matrix", State: github.CheckFailed, Suite: 3, App: actions, Workflow: 7, Event: "pull_request"},
		{Name: "both", State: github.CheckFailed, Suite: 4, App: actions, Workflow: 9, Event: "push"},
		{Name: "both", State: github.CheckPassed, Suite: 5, App: actions, Workflow: 9, Event: "pull_request"},
		{Name: "both", State: github.CheckFailed, Suite: 4, App: actions, Workflow: 9, Event: "pull_request"},
	}
	got := LatestChecks(checks)
	want := []github.Check{checks[1], checks[2], checks[3], checks[4], checks[5], checks[6], checks[7]}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestVerdictConcernsAreNeverNull(t *testing.T) {
	b, err := json.Marshal(Decision{Verdict: &Verdict{Error: "timed out"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"concerns":[]`) {
		t.Fatalf("json = %s", b)
	}
	b, _ = json.Marshal(Verdict{Concerns: []string{"x"}})
	if string(b) != `{"approved":false,"summary":"","concerns":["x"]}` {
		t.Fatalf("json = %s", b)
	}
}

func TestEvaluateWithoutDependabotCommands(t *testing.T) {
	behind := func(n int, deps ...string) github.PullRequest {
		pr := testutil.PR(n, deps...)
		pr.MergeState = "BEHIND"
		return pr
	}
	p := defaultPolicy
	p.DependabotCommands = false
	repo := testutil.Repo(behind(1), behind(2), behind(3, "lib:major"), testutil.PR(4))
	ds := Evaluate(repo, p, "me", nil)
	var got []string
	for _, d := range ds {
		got = append(got, string(d.Action))
	}
	// Nothing is asked of Dependabot, and nothing queues behind a rebase
	// that will not be requested.
	if want := "skip skip skip merge"; strings.Join(got, " ") != want {
		t.Fatalf("actions = %s, want %s", strings.Join(got, " "), want)
	}
	if !strings.Contains(ds[0].Reason, "left to a human") {
		t.Errorf("reason = %q", ds[0].Reason)
	}
	f := map[FindingKind][]Finding{}
	for _, x := range Diagnose(repo, p, ds) {
		f[x.Kind] = append(f[x.Kind], x)
	}
	if len(f[FindingBehind]) != 1 || len(f[FindingBehind][0].PRs) != 2 || !strings.Contains(f[FindingBehind][0].Suggestions[0], RebaseCommand) {
		t.Errorf("behind = %+v", f[FindingBehind])
	}
	// Nor is a request read into comments too many to see: none is made.
	busy := behind(1)
	busy.CommentCount = 80
	busy.Comments = []github.Comment{{Author: "someone", Body: "ping", CreatedAt: testutil.T0.Add(time.Hour)}}
	for _, at := range []time.Duration{0, 50 * time.Hour} {
		r := testutil.Repo(busy)
		r.FetchedAt = r.FetchedAt.Add(at)
		d := Evaluate(r, p, "me", nil)[0]
		if d.Action != ActionSkip || !d.behind || d.stalledRebase {
			t.Errorf("busy PR after %s = %s, %q", at, d.Action, d.Reason)
		}
	}
	// A major that is behind is still held for its size alone.
	if len(f[FindingHeldMajor]) != 1 || f[FindingHeldMajor][0].PRs[0] != 3 {
		t.Errorf("held-major = %+v", f[FindingHeldMajor])
	}
	// One the agent approved is left to a human too, who is told the
	// approval holds once it is rebased.
	approve := func(Decision) *Verdict { return &Verdict{Approved: true} }
	d := Evaluate(testutil.Repo(behind(3, "lib:major")), p, "me", approve)[0]
	if d.Action != ActionSkip || !d.behind || !strings.Contains(d.Reason, "left to a human") || !strings.Contains(d.Reason, "major update approved by agent") {
		t.Errorf("approved major behind = %s, %q", d.Action, d.Reason)
	}
}
