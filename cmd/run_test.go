package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sglavoie/deps-merger/internal/testutil"
	"github.com/sglavoie/deps-merger/pkg/github"
	"github.com/sglavoie/deps-merger/pkg/policy"
	"github.com/sglavoie/deps-merger/pkg/review"
)

// fakeClient serves fixed repos by name; a name missing from repos fails.
type fakeClient struct {
	repos  map[string]*github.Repo
	merges []int
}

func (f *fakeClient) Viewer(context.Context) (string, error) { return "me", nil }

func (f *fakeClient) FetchRepo(_ context.Context, owner, name string) (*github.Repo, error) {
	r, ok := f.repos[owner+"/"+name]
	if !ok {
		return nil, errors.New(owner + "/" + name + ": repository not found or not accessible")
	}
	cp := *r
	cp.FullName = owner + "/" + name
	cp.PullRequests = nil
	for _, pr := range r.PullRequests {
		if !contains(f.merges, pr.Number) {
			cp.PullRequests = append(cp.PullRequests, pr)
		}
	}
	return &cp, nil
}

func contains(ns []int, n int) bool {
	return slices.Contains(ns, n)
}

func (f *fakeClient) Merge(_ context.Context, _, _ string, n int, _, _ string) error {
	f.merges = append(f.merges, n)
	return nil
}

func (f *fakeClient) FileText(context.Context, string, string, string) ([]byte, error) {
	return nil, nil
}

func (f *fakeClient) PullRequestState(_ context.Context, _, _ string, n int) (github.PRState, error) {
	if contains(f.merges, n) {
		return github.PRMerged, nil
	}
	return github.PROpen, nil
}

func (f *fakeClient) Comment(context.Context, string, string, int, string) error { return nil }

func (f *fakeClient) PullRequestDetails(context.Context, string, string, int) (*github.PullRequestDetails, error) {
	return &github.PullRequestDetails{}, nil
}

// useClient makes run talk to fc for the rest of the test.
func useClient(t *testing.T, fc *fakeClient) {
	t.Helper()
	old := newClient
	newClient = func() (github.Client, error) { return fc, nil }
	t.Cleanup(func() { newClient = old })
}

func runFixture(t *testing.T) (*fakeClient, string) {
	fc := &fakeClient{repos: map[string]*github.Repo{
		"o/busy":  testutil.Repo(testutil.PR(1), testutil.PR(2, "lib:major")),
		"o/quiet": testutil.Repo(),
	}}
	useClient(t, fc)
	return fc, writeConfig(t, "repos:\n  - o/busy\n  - o/quiet\n")
}

func TestRunDryRun(t *testing.T) {
	fc, cfg := runFixture(t)
	code, out, _ := run("run", "-c", cfg)
	if code != 0 || len(fc.merges) != 0 {
		t.Fatalf("code %d, merges %v:\n%s", code, fc.merges, out)
	}
	for _, want := range []string{"would merge", "no open Dependabot PRs", "2 repos: 1 would merge, 1 skipped", "dry run: nothing was changed"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestRunApply(t *testing.T) {
	fc, cfg := runFixture(t)
	code, out, _ := run("run", "-c", cfg, "--apply", "-r", " busy ")
	if code != 0 || len(fc.merges) != 1 || fc.merges[0] != 1 || !strings.Contains(out, "1 repo: 1 merged, 1 skipped") {
		t.Fatalf("code %d, merges %v:\n%s", code, fc.merges, out)
	}
}

func TestRunIncludeSkipped(t *testing.T) {
	fc, _ := runFixture(t)
	cfg := writeConfig(t, "repos:\n  - o/quiet\n  - repo: o/busy\n    skip: true\n")
	if _, out, _ := run("run", "-c", cfg, "--owner", "o"); strings.Contains(out, "o/busy") {
		t.Fatalf("--owner ran a skipped repo:\n%s", out)
	}
	code, out, _ := run("run", "-c", cfg, "--owner", "o", "--include-skipped")
	if code != 0 || len(fc.merges) != 0 || !strings.Contains(out, "o/busy") || !strings.Contains(out, "2 repos:") {
		t.Fatalf("code %d, merges %v:\n%s", code, fc.merges, out)
	}
}

func TestRunOpenLeavesOutQuietRepos(t *testing.T) {
	_, cfg := runFixture(t)
	if _, out, _ := run("run", "-c", cfg, "--open"); strings.Contains(out, "o/quiet") || !strings.Contains(out, "o/busy") {
		t.Fatalf("output:\n%s", out)
	}
	_, out, _ := run("run", "-c", cfg, "--open", "--json")
	var doc struct {
		Apply   bool
		Results []struct {
			Repo      string
			Decisions []struct{ Number int }
		}
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("%v:\n%s", err, out)
	}
	if doc.Apply || len(doc.Results) != 1 || doc.Results[0].Repo != "o/busy" || len(doc.Results[0].Decisions) != 2 {
		t.Fatalf("json = %+v", doc)
	}
}

func TestRunMarkdown(t *testing.T) {
	_, cfg := runFixture(t)
	code, out, _ := run("run", "-c", cfg, "--markdown", "--open")
	if code != 0 {
		t.Fatalf("code %d:\n%s", code, out)
	}
	for _, want := range []string{"## o/busy\n", "| PR | Update | Status | Reason |", "| would merge |", "### Needs attention"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// --open leaves the quiet repo out, and no summary follows.
	if strings.Contains(out, "o/quiet") || strings.Contains(out, "dry run:") {
		t.Errorf("output:\n%s", out)
	}
}

func TestRunOpenSummaryCountsHiddenRepos(t *testing.T) {
	_, cfg := runFixture(t)
	if _, out, _ := run("run", "-c", cfg, "--open"); !strings.Contains(out, "1 repo (1 without open PRs hidden): 1 would merge, 1 skipped") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestRunReviewSkipsSetupWithoutAgentReview(t *testing.T) {
	_, _ = runFixture(t)
	cfg := writeConfig(t, "repos:\n  - {repo: o/busy, agent_review: false}\n")
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir()) // no agent installed
	code, out, stderr := run("run", "-c", cfg, "--review", "--max-reviews", "3")
	if code != 0 || !strings.Contains(stderr, "every selected repo sets agent_review: false") || !strings.Contains(out, "would merge") {
		t.Fatalf("code %d, stderr %q:\n%s", code, stderr, out)
	}
	if _, err := os.Stat(filepath.Join(state, "deps-merger")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("review setup ran anyway: %v", err)
	}
	// Off only in the defaults, reviews may be turned on by the repo's
	// files, so the agent is set up, and here found missing.
	off := writeConfig(t, "defaults:\n  agent_review: false\nrepos:\n  - o/busy\n")
	if code, _, stderr := run("run", "-c", off, "--review"); code == 0 || !strings.Contains(stderr, "claude is not installed") {
		t.Errorf("off in the defaults: code %d, %q", code, stderr)
	}
	if code, _, stderr := run("run", "-c", cfg, "--agent", "gemini"); code != 2 || !strings.Contains(stderr, `unknown agent "gemini"`) {
		t.Errorf("unknown agent: code %d, %q", code, stderr)
	}
}

func TestRunFailedRepoExitsOne(t *testing.T) {
	_, _ = runFixture(t)
	cfg := writeConfig(t, "repos:\n  - o/busy\n  - o/missing\n")
	code, out, _ := run("run", "-c", cfg)
	if code != 1 || !strings.Contains(out, "error: o/missing: repository not found") || !strings.Contains(out, "1 repo failed") {
		t.Fatalf("code %d:\n%s", code, out)
	}
}

func TestVerdictsFilterAndJSON(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	cache, err := review.OpenCache(verdictsPath(filepath.Join(state, "deps-merger")))
	if err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("word ", 30)
	for _, k := range []string{"o/a#1 lib@2.0.0", "o/b#2 x@3.0.0"} {
		if err := cache.Put(k, "sha", "claude", policy.Verdict{Summary: long, Concerns: []string{"src/x.go"}}); err != nil {
			t.Fatal(err)
		}
	}
	code, out, _ := run("verdicts", "o/b")
	if code != 0 || strings.Contains(out, "o/a#1") || !strings.Contains(out, "o/b#2") || !strings.Contains(out, "…") || !strings.Contains(out, "EXPIRES") {
		t.Fatalf("filtered: code %d:\n%s", code, out)
	}
	code, out, _ = run("verdicts", "--json", "o/a#1")
	var doc struct {
		Verdicts []struct {
			Update, Verdict, Summary string
			Concerns                 []string
		}
	}
	if err := json.Unmarshal([]byte(out), &doc); code != 0 || err != nil {
		t.Fatalf("json: code %d, %v:\n%s", code, err, out)
	}
	got := doc.Verdicts
	if len(got) != 1 || got[0].Update != "o/a#1 lib@2.0.0" || got[0].Verdict != "rejected" || got[0].Summary != long || got[0].Concerns[0] != "src/x.go" {
		t.Fatalf("json = %+v", got)
	}
}

func TestVerdictsForgetCarriesOn(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	cache, _ := review.OpenCache(verdictsPath(filepath.Join(state, "deps-merger")))
	if err := cache.Put("o/a#1 lib@2.0.0", "sha", "claude", policy.Verdict{}); err != nil {
		t.Fatal(err)
	}
	code, out, stderr := run("verdicts", "forget", "o/zzz", "o/a")
	if code != 1 || out != "o/a: forgot 1 verdict(s)\n" || !strings.Contains(stderr, `no cached verdict matches "o/zzz"`) {
		t.Fatalf("code %d, out %q, stderr %q", code, out, stderr)
	}
}
