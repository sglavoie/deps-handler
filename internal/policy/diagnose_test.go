package policy

import (
	"testing"

	"github.com/sglavoie/deps-merger/internal/github"
	"github.com/sglavoie/deps-merger/internal/testutil"
)

func findings(repo *github.Repo) map[FindingKind][]Finding {
	ds := Evaluate(repo, defaultPolicy, "me")
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
