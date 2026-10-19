package policy

import (
	"strings"
	"testing"
	"time"

	"github.com/sglavoie/deps-merger/internal/github"
	"github.com/sglavoie/deps-merger/internal/testutil"
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

func TestDiagnoseCancelledRunIsNotRepoWide(t *testing.T) {
	// Like ExcelAddIn: concurrency cancels CI on every PR in favour of a
	// later run that passes.
	rerun := func(pr github.PullRequest) github.PullRequest {
		pr.Checks = []github.Check{
			{Name: "Quality", State: github.CheckFailed, Suite: 1},
			{Name: "Quality", State: github.CheckPassed, Suite: 2},
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

func TestDiagnoseNoChecksIgnoresWaitingPRs(t *testing.T) {
	fresh := testutil.PR(1)
	fresh.Checks = nil
	fresh.Commits[0].CommittedDate = testutil.Fetched
	if f := findings(testutil.Repo(fresh)); len(f[FindingNoChecks]) != 0 {
		t.Fatalf("no-checks = %+v; CI may not have started yet", f[FindingNoChecks])
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
	repo.OpenPRCount = 140
	f := findings(repo)
	if len(f[FindingTruncated]) != 1 || !strings.Contains(f[FindingTruncated][0].Summary, "1 of 140") {
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
