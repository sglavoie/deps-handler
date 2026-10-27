package policy

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sglavoie/deps-merger/internal/testutil"
	"github.com/sglavoie/deps-merger/pkg/config"
	"github.com/sglavoie/deps-merger/pkg/github"
)

func findings(repo *github.Repo) map[FindingKind][]Finding {
	ds := Evaluate(repo, defaultPolicy, "me", nil)
	out := map[FindingKind][]Finding{}
	for _, f := range Diagnose(repo, defaultPolicy, ds) {
		out[f.Kind] = append(out[f.Kind], f)
	}
	return out
}

func failing(pr github.PullRequest, names ...string) github.PullRequest {
	pr.Checks = nil
	for _, n := range names {
		pr.Checks = append(pr.Checks, github.Check{Name: n, State: github.CheckFailed})
	}
	pr.Checks = append(pr.Checks, github.Check{Name: "lint", State: github.CheckPassed})
	return pr
}

func TestDiagnoseRepoWideFailure(t *testing.T) {
	// Like Storefront: one check fails on every Dependabot PR.
	f := findings(testutil.Repo(
		failing(testutil.PR(1), "i18n"),
		failing(testutil.PR(2), "i18n"),
		failing(testutil.PR(3), "i18n"),
	))
	if len(f[FindingRepoCheck]) != 1 || len(f[FindingRepoCheck][0].PRs) != 3 {
		t.Fatalf("repo-wide = %+v", f[FindingRepoCheck])
	}
	if len(f[FindingIncompatible]) != 0 {
		t.Fatalf("a repo-wide failure must not be blamed on the updates: %+v", f[FindingIncompatible])
	}
}

func TestDiagnoseRepoWideFailuresByBaseBranch(t *testing.T) {
	pr := func(number int, base string, fails bool) github.PullRequest {
		p := testutil.PR(number)
		p.BaseRef = base
		if fails {
			p = failing(p, "build")
		}
		return p
	}
	t.Run("passing branch does not hide a broken branch", func(t *testing.T) {
		f := findings(testutil.Repo(pr(1, "release", true), pr(2, "release", true), pr(3, "main", false)))
		shared := f[FindingRepoCheck]
		if len(shared) != 1 || len(shared[0].PRs) != 2 || !strings.Contains(shared[0].Summary, `"release"`) {
			t.Fatalf("branch failure = %+v", shared)
		}
		if len(f[FindingIncompatible]) != 0 {
			t.Fatalf("broken branch blamed on updates: %+v", f[FindingIncompatible])
		}
	})
	t.Run("unrelated failures do not establish a broken branch", func(t *testing.T) {
		f := findings(testutil.Repo(pr(1, "release", true), pr(2, "main", true)))
		if len(f[FindingRepoCheck]) != 0 || len(f[FindingIncompatible]) != 2 {
			t.Fatalf("findings = %+v", f)
		}
	})
	t.Run("coverage threshold counts only the same branch", func(t *testing.T) {
		other := func(number int) github.PullRequest {
			p := pr(number, "main", false)
			p.Checks[0].Name = "other"
			return p
		}
		f := findings(testutil.Repo(pr(1, "release", true), pr(2, "release", true),
			other(3), other(4), other(5)))
		if len(f[FindingRepoCheck]) != 1 || len(f[FindingIncompatible]) != 0 {
			t.Fatalf("findings = %+v", f)
		}
	})
	t.Run("shared failure does not hide an isolated failure on another branch", func(t *testing.T) {
		f := findings(testutil.Repo(pr(1, "release", true), pr(2, "release", true), pr(3, "main", true)))
		shared, own := f[FindingRepoCheck], f[FindingIncompatible]
		if len(shared) != 1 || len(shared[0].PRs) != 2 || len(own) != 1 || own[0].PRs[0] != 3 {
			t.Fatalf("findings = %+v", f)
		}
	})
	t.Run("separate broken branches have separate findings", func(t *testing.T) {
		f := findings(testutil.Repo(pr(1, "release", true), pr(2, "release", true),
			pr(3, "Release", true), pr(4, "Release", true)))
		shared := f[FindingRepoCheck]
		if len(shared) != 2 || shared[0].PRs[0] != 3 || shared[1].PRs[0] != 1 ||
			len(shared[0].PRs) != 2 || len(shared[1].PRs) != 2 || len(f[FindingIncompatible]) != 0 {
			t.Fatalf("findings = %+v", f)
		}
	})
}

func TestDiagnoseRepoWideFailureBesideAPendingRun(t *testing.T) {
	pending := testutil.PR(3)
	pending.Checks = []github.Check{{Name: "i18n", State: github.CheckPending}}
	f := findings(testutil.Repo(failing(testutil.PR(1), "i18n"), failing(testutil.PR(2), "i18n"), pending))
	if len(f[FindingRepoCheck]) != 1 || len(f[FindingRepoCheck][0].PRs) != 2 {
		t.Fatalf("repo-wide = %+v", f[FindingRepoCheck])
	}
	if len(f[FindingIncompatible]) != 0 {
		t.Fatalf("a check still running elsewhere must not blame the updates: %+v", f[FindingIncompatible])
	}
}

func TestDiagnoseRepoWideCheckSources(t *testing.T) {
	check := func(workflow int64, state github.CheckState) github.Check {
		return github.Check{Name: "test", State: state, Suite: 1, App: 10, Workflow: workflow, Event: "pull_request"}
	}
	pr := func(number int, checks ...github.Check) github.PullRequest {
		p := testutil.PR(number)
		p.Checks = checks
		return p
	}
	for _, field := range []string{"workflow", "app", "event", "status context"} {
		t.Run("unrelated failures from different "+field, func(t *testing.T) {
			a, b := check(1, github.CheckFailed), check(1, github.CheckFailed)
			switch field {
			case "workflow":
				b.Workflow++
			case "app":
				b.App++
			case "event":
				b.Event = "push"
			case "status context":
				b = github.Check{Name: "test", State: github.CheckFailed, Status: true}
			}
			f := findings(testutil.Repo(pr(1, a), pr(2, b)))
			if len(f[FindingRepoCheck]) != 0 || len(f[FindingIncompatible]) != 2 {
				t.Fatalf("unrelated checks were treated as a shared failure: %+v", f)
			}
		})
	}
	t.Run("another workflow passing does not hide a shared failure", func(t *testing.T) {
		f := findings(testutil.Repo(pr(1, check(1, github.CheckFailed)),
			pr(2, check(1, github.CheckFailed)), pr(3, check(2, github.CheckPassed))))
		if len(f[FindingRepoCheck]) != 1 || len(f[FindingRepoCheck][0].PRs) != 2 || len(f[FindingIncompatible]) != 0 {
			t.Fatalf("findings = %+v", f)
		}
	})
	t.Run("shared failure does not hide an isolated same-named failure", func(t *testing.T) {
		f := findings(testutil.Repo(pr(1, check(1, github.CheckFailed), check(2, github.CheckFailed)),
			pr(2, check(1, github.CheckFailed), check(2, github.CheckPassed))))
		shared, own := f[FindingRepoCheck], f[FindingIncompatible]
		if len(shared) != 1 || len(shared[0].PRs) != 2 || len(own) != 1 || own[0].PRs[0] != 1 {
			t.Fatalf("findings = %+v", f)
		}
	})
	t.Run("suites differ between PRs and repeated jobs count once", func(t *testing.T) {
		a, b := check(1, github.CheckFailed), check(1, github.CheckFailed)
		b.Suite = 2
		f := findings(testutil.Repo(pr(1, a, a), pr(2, b, b)))
		if len(f[FindingRepoCheck]) != 1 || len(f[FindingRepoCheck][0].PRs) != 2 || len(f[FindingIncompatible]) != 0 {
			t.Fatalf("findings = %+v", f)
		}
	})
}

func TestDiagnoseCancelledRunIsNotRepoWide(t *testing.T) {
	// Like ExcelAddIn: concurrency cancels CI on every PR in favour of a
	// later run that passes.
	rerun := func(pr github.PullRequest) github.PullRequest {
		pr.Checks = []github.Check{
			{Name: "Quality", State: github.CheckFailed, Suite: 1, App: 15368, Workflow: 7, Event: "pull_request"},
			{Name: "Quality", State: github.CheckPassed, Suite: 2, App: 15368, Workflow: 7, Event: "pull_request"},
		}
		return pr
	}
	f := findings(testutil.Repo(rerun(testutil.PR(1)), rerun(testutil.PR(2))))
	if len(f) != 0 {
		t.Fatalf("findings = %+v", f)
	}
}

func TestDiagnoseIncompatibleMajor(t *testing.T) {
	f := findings(testutil.Repo(
		failing(testutil.PR(1, "pydantic:major"), "build"),
		testutil.PR(2),
	))
	inc := f[FindingIncompatible]
	if len(inc) != 1 || len(inc[0].Suggestions) != 1 || inc[0].Suggestions[0] != "@dependabot ignore this major version" {
		t.Fatalf("incompatible = %+v", inc)
	}
}

func TestDiagnoseGroupSuggestsPerDependency(t *testing.T) {
	f := findings(testutil.Repo(
		failing(testutil.PR(1, "a:patch", "b:major", "c:major"), "build"),
		testutil.PR(2),
	))
	inc := f[FindingIncompatible]
	want := []string{"@dependabot ignore b major version", "@dependabot ignore c major version"}
	if len(inc) != 1 || len(inc[0].Suggestions) != 2 || inc[0].Suggestions[0] != want[0] || inc[0].Suggestions[1] != want[1] {
		t.Fatalf("incompatible = %+v", inc)
	}
}

func TestDiagnoseHeldMajorAndNoChecks(t *testing.T) {
	noCI := testutil.PR(2)
	noCI.Checks = nil
	f := findings(testutil.Repo(testutil.PR(1, "lib:major"), noCI))
	if len(f[FindingHeldMajor]) != 1 {
		t.Errorf("held-major = %+v", f[FindingHeldMajor])
	}
	if len(f[FindingNoChecks]) != 1 || f[FindingNoChecks][0].PRs[0] != 2 {
		t.Errorf("no-checks = %+v", f[FindingNoChecks])
	}
}

func TestDiagnoseHeldOnlyForSize(t *testing.T) {
	conflicting := testutil.PR(2, "lib:major")
	conflicting.Mergeable = "CONFLICTING"
	pinned := testutil.PR(3, "actions/checkout:unknown")
	f := findings(testutil.Repo(testutil.PR(1, "lib:major"), conflicting, pinned))
	if held := f[FindingHeldMajor]; len(held) != 1 || held[0].PRs[0] != 1 || !strings.Contains(held[0].Summary, "--review") {
		t.Errorf("held-major = %+v; a conflicting major is not held by policy alone", held)
	}
	if held := f[FindingHeldUnknown]; len(held) != 1 || held[0].PRs[0] != 3 {
		t.Errorf("held-unknown = %+v", held)
	}

	p := defaultPolicy
	p.AgentReview = false
	repo := testutil.Repo(testutil.PR(1, "lib:major"))
	got := Diagnose(repo, p, Evaluate(repo, p, "me", nil))
	if len(got) != 1 || got[0].Kind != FindingHeldMajor || strings.Contains(got[0].Summary, "--review") {
		t.Errorf("findings = %+v; --review cannot help where agent_review is off", got)
	}
}

func TestDiagnoseHeldMinorUnderPatchLimit(t *testing.T) {
	p := defaultPolicy
	p.MaxUpdateType = config.UpdatePatch
	repo := testutil.Repo(testutil.PR(1, "lib:minor"))
	got := Diagnose(repo, p, Evaluate(repo, p, "me", nil))
	if len(got) != 1 || got[0].Kind != FindingHeldMinor || !strings.Contains(got[0].Summary, "max_update_type: minor") {
		t.Errorf("findings = %+v; a minor held under a patch limit needs attention", got)
	}
}

func TestDiagnoseNoChecksIgnoresConflictingPRs(t *testing.T) {
	conflicting := testutil.PR(1)
	conflicting.Checks = nil
	conflicting.Mergeable, conflicting.MergeState = "CONFLICTING", "DIRTY"
	if f := findings(testutil.Repo(conflicting)); len(f[FindingNoChecks]) != 0 {
		t.Fatalf("no-checks = %+v; GitHub runs no workflow on a PR in conflict", f[FindingNoChecks])
	}
}

func TestDiagnoseNoChecksIgnoresWaitingPRs(t *testing.T) {
	fresh := testutil.PR(1)
	fresh.Checks = nil
	fresh.Commits[0].CommittedDate = testutil.Fetched
	if f := findings(testutil.Repo(fresh)); len(f[FindingNoChecks]) != 0 {
		t.Fatalf("no-checks = %+v; CI may not have started yet", f[FindingNoChecks])
	}
}

// A PR skipped for its size never reaches the wait for CI to start, but
// its CI may not have started yet all the same.
func TestDiagnoseNoChecksIgnoresNewOversizedPRs(t *testing.T) {
	fresh := testutil.PR(1, "lib:major")
	fresh.Checks = nil
	fresh.Commits[0].CommittedDate = testutil.Fetched.Add(-2 * time.Minute)
	f := findings(testutil.Repo(fresh))
	if held := f[FindingHeldMajor]; len(held) != 1 || held[0].PRs[0] != 1 {
		t.Errorf("held-major = %+v", held)
	}
	if len(f[FindingNoChecks]) != 0 {
		t.Errorf("no-checks = %+v; CI may not have started yet", f[FindingNoChecks])
	}
}

func TestDiagnoseNoChecksIgnoresUnassessedPRs(t *testing.T) {
	noCI := func(pr github.PullRequest) github.PullRequest {
		pr.Checks = nil
		return pr
	}
	draft := noCI(testutil.PR(1))
	draft.IsDraft = true
	edited := noCI(testutil.PR(2))
	edited.Commits[0].Authors = append(edited.Commits[0].Authors, github.Author{Login: "sglavoie"})
	if f := findings(testutil.Repo(draft, edited)); len(f[FindingNoChecks]) != 0 {
		t.Fatalf("no-checks = %+v; neither PR was judged on its checks", f[FindingNoChecks])
	}
	archived := testutil.Repo(noCI(testutil.PR(3)))
	archived.Archived = true
	if f := findings(archived); len(f[FindingNoChecks]) != 0 {
		t.Fatalf("no-checks = %+v; an archived repository's PRs were not judged", f[FindingNoChecks])
	}
}

func TestFindingListsAreNeverNull(t *testing.T) {
	b, err := json.Marshal(Finding{Kind: FindingReadOnly, Summary: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"kind":"read-only","summary":"s","prs":[],"details":[],"suggestions":[]}`; string(b) != want {
		t.Fatalf("json = %s, want %s", b, want)
	}
}

func TestDiagnoseReviewRequired(t *testing.T) {
	blocked := func(n int) github.PullRequest {
		pr := testutil.PR(n)
		pr.MergeState, pr.ReviewDecision = "BLOCKED", "REVIEW_REQUIRED"
		return pr
	}
	f := findings(testutil.Repo(blocked(1), blocked(2), testutil.PR(3)))
	if r := f[FindingReviewRequired]; len(r) != 1 || len(r[0].PRs) != 2 || r[0].PRs[1] != 2 {
		t.Fatalf("review-required = %+v", r)
	}
}

func TestDiagnoseStalledRebase(t *testing.T) {
	pr := testutil.PR(1)
	pr.MergeState = "BEHIND"
	pr.Comments = []github.Comment{{Author: "me", Body: RebaseCommand, CreatedAt: testutil.T0.Add(1)}}
	repo := testutil.Repo(pr)
	repo.FetchedAt = testutil.T0.Add(30 * time.Hour)
	f := findings(repo)
	if s := f[FindingStalledRebase]; len(s) != 1 || s[0].Suggestions[0] != "@dependabot recreate" {
		t.Fatalf("stalled-rebase = %+v", s)
	}
}

func TestDiagnoseStaleCheckIsNotRepoWide(t *testing.T) {
	// Like Billing: an old job fails on two old PRs; newer PRs never ran it.
	f := findings(testutil.Repo(
		failing(testutil.PR(1, "pydantic:major"), "build (3.11)"),
		failing(testutil.PR(2, "protobuf:major"), "build (3.11)"),
		testutil.PR(3), testutil.PR(4), testutil.PR(5),
	))
	if len(f[FindingRepoCheck]) != 0 {
		t.Fatalf("repo-wide = %+v", f[FindingRepoCheck])
	}
	if len(f[FindingIncompatible]) != 2 {
		t.Fatalf("incompatible = %+v", f[FindingIncompatible])
	}
}

func TestDiagnoseTruncatedSearch(t *testing.T) {
	repo := testutil.Repo(testutil.PR(1))
	repo.OpenPRCount, repo.SearchCount = 140, 100
	f := findings(repo)
	if len(f[FindingTruncated]) != 1 || !strings.Contains(f[FindingTruncated][0].Summary, "100 of 140") {
		t.Fatalf("truncated = %+v", f[FindingTruncated])
	}
}

func TestDiagnoseClosedPRsInSearchAreNotTruncation(t *testing.T) {
	// GitHub's search still counts two PRs closed since it indexed them;
	// it returned them, and FetchRepo dropped them.
	repo := testutil.Repo(testutil.PR(1))
	repo.OpenPRCount, repo.SearchCount = 3, 3
	if f := findings(repo); len(f[FindingTruncated]) != 0 {
		t.Fatalf("truncated = %+v", f[FindingTruncated])
	}
}

func TestDiagnoseEmptyIsNotNil(t *testing.T) {
	if f := Diagnose(testutil.Repo(), defaultPolicy, nil); f == nil {
		t.Fatal("Diagnose returned nil; JSON would print null")
	}
}

func TestDiagnoseIgnoresUnassessedFailures(t *testing.T) {
	edited := failing(testutil.PR(1, "lib:major"), "build")
	edited.Commits[0].Authors = append(edited.Commits[0].Authors, github.Author{Login: "sglavoie"})
	moved := failing(testutil.PR(2), "build")
	moved.ChecksOID = "older"
	f := findings(testutil.Repo(edited, moved, testutil.PR(3), testutil.PR(4)))
	if len(f[FindingIncompatible]) != 0 {
		t.Fatalf("incompatible = %+v; a failure on a human's change or another commit is not the update's", f[FindingIncompatible])
	}
}

func TestDiagnoseAgentVerdicts(t *testing.T) {
	repo := testutil.Repo(testutil.PR(1, "lib:major"), testutil.PR(2, "lib:major"), testutil.PR(3, "lib:major"), testutil.PR(4, "lib:major"))
	verdicts := map[int]*Verdict{
		1: {Summary: "drops Python 3.9", Concerns: []string{"setup.cfg pins 3.9"}},
		2: {Error: "timed out"},
		3: {Approved: true},
	}
	ds := Evaluate(repo, defaultPolicy, "me", func(d Decision) *Verdict { return verdicts[d.Number] })
	got := map[FindingKind][]Finding{}
	for _, f := range Diagnose(repo, defaultPolicy, ds) {
		got[f.Kind] = append(got[f.Kind], f)
	}
	rej := got[FindingAgentRejected]
	if len(rej) != 1 || rej[0].PRs[0] != 1 || !strings.Contains(rej[0].Summary, "drops Python 3.9") || len(rej[0].Details) != 1 {
		t.Errorf("rejected = %+v", rej)
	}
	if f := got[FindingAgentFailed]; len(f) != 1 || f[0].PRs[0] != 2 || !strings.Contains(f[0].Summary, "timed out") {
		t.Errorf("failed = %+v", f)
	}
	// Neither the approved PR nor the one awaiting review is "held".
	if f := got[FindingHeldMajor]; len(f) != 0 {
		t.Errorf("held majors = %+v", f)
	}
}

func TestDiagnoseArchived(t *testing.T) {
	repo := testutil.Repo(testutil.PR(1))
	repo.Archived = true
	f := findings(repo)
	if len(f[FindingArchived]) != 1 || !strings.Contains(f[FindingArchived][0].Suggestions[0], "skip: true") {
		t.Fatalf("findings = %+v", f)
	}
	if f := findings(testutil.Repo(testutil.PR(1))); len(f[FindingArchived]) != 0 {
		t.Fatalf("an active repo is not archived: %+v", f)
	}
}

func TestDiagnoseNoChecksCountsOnlyChecksThatRan(t *testing.T) {
	skipped := testutil.PR(1)
	skipped.Checks = []github.Check{{Name: "build", State: github.CheckSkipped}}
	f := findings(testutil.Repo(skipped, testutil.PR(2)))
	if n := f[FindingNoChecks]; len(n) != 1 || len(n[0].PRs) != 1 || n[0].PRs[0] != 1 {
		t.Fatalf("no-checks = %+v; a PR whose every check was skipped ran none", n)
	}
}

func TestDiagnoseRepoWideCountsOnlyAssessedPRs(t *testing.T) {
	moved := func(pr github.PullRequest) github.PullRequest {
		pr.ChecksOID = "older"
		return pr
	}
	edited := failing(testutil.PR(1), "i18n")
	edited.Commits[0].Authors = append(edited.Commits[0].Authors, github.Author{Login: "sglavoie"})
	f := findings(testutil.Repo(edited, moved(failing(testutil.PR(2), "i18n"))))
	if len(f[FindingRepoCheck]) != 0 {
		t.Fatalf("repo-wide = %+v; neither failure ran on Dependabot's own head commit", f[FindingRepoCheck])
	}

	// PRs whose checks ran on another commit do not count towards the half.
	f = findings(testutil.Repo(
		failing(testutil.PR(1), "i18n"), failing(testutil.PR(2), "i18n"),
		moved(testutil.PR(3)), moved(testutil.PR(4)), moved(testutil.PR(5)),
	))
	if r := f[FindingRepoCheck]; len(r) != 1 || len(r[0].PRs) != 2 {
		t.Fatalf("repo-wide = %+v", r)
	}
}
