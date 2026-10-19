package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sglavoie/deps-merger/internal/config"
	"github.com/sglavoie/deps-merger/internal/policy"
	"github.com/sglavoie/deps-merger/internal/runner"
)

func decision(n int, a policy.Action, reason string) policy.Decision {
	return policy.Decision{Number: n, Title: "bump lib", Grade: config.UpdatePatch, Action: a, Reason: reason}
}

// applied is a repo after --apply: #1 merged, #2's merge failed, #3 waits.
var applied = runner.RepoResult{
	Repo:       "o/r",
	Policy:     config.Builtin,
	Permission: "WRITE",
	Decisions: []policy.Decision{
		decision(2, policy.ActionMerge, "checks green, within policy"),
		decision(3, policy.ActionWait, "1 check(s) still running"),
	},
	Findings: []policy.Finding{{Kind: policy.FindingNoChecks, Summary: "no checks ran", PRs: []int{4, 5}}},
	Events: []runner.Event{
		{Number: 1, Title: "bump lib", Grade: config.UpdatePatch, Kind: runner.EventMerged, Detail: "squash"},
		{Number: 2, Title: "bump lib", Grade: config.UpdatePatch, Kind: runner.EventMergeFailed, Detail: "405 not mergeable"},
	},
}

func TestRepoAfterApply(t *testing.T) {
	var b bytes.Buffer
	Repo(&b, true, applied)
	want := `o/r  (squash, up to minor, checks required, write access)
  #1  patch  merged        squash                    bump lib
  #2  patch  merge-failed  405 not mergeable         bump lib
  #3  patch  wait          1 check(s) still running  bump lib
  needs attention:
    - no checks ran
      PRs: #4, #5

`
	if b.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", b.String(), want)
	}
}

func TestRepoLinksTitlesOnATerminal(t *testing.T) {
	d := decision(1, policy.ActionWait, "1 check(s) still running")
	d.URL = "https://github.com/o/r/pull/1"
	res := runner.RepoResult{Repo: "o/r", Policy: config.Builtin, Decisions: []policy.Decision{d}}
	var b bytes.Buffer
	repo(&b, false, res, true)
	if want := "\x1b]8;;https://github.com/o/r/pull/1\x1b\\bump lib\x1b]8;;\x1b\\\n"; !strings.Contains(b.String(), want) {
		t.Fatalf("got %q", b.String())
	}
	b.Reset()
	Repo(&b, false, res)
	if strings.Contains(b.String(), "\x1b") {
		t.Fatalf("escape codes written to a non-terminal: %q", b.String())
	}
}

func TestProgress(t *testing.T) {
	var b bytes.Buffer
	Progress(&b, applied)
	Progress(&b, runner.RepoResult{Repo: "o/x", Error: "boom"})
	if want := "o/r: done (2 PRs)\no/x: error: boom\n"; b.String() != want {
		t.Fatalf("got %q", b.String())
	}
}

func TestHasPRs(t *testing.T) {
	for name, c := range map[string]struct {
		res  runner.RepoResult
		want bool
	}{
		"no PRs":      {runner.RepoResult{Repo: "o/r"}, false},
		"error":       {runner.RepoResult{Repo: "o/r", Error: "boom"}, true},
		"decisions":   {runner.RepoResult{Repo: "o/r", Decisions: applied.Decisions}, true},
		"events only": {runner.RepoResult{Repo: "o/r", Events: applied.Events}, true},
	} {
		if got := HasPRs(c.res); got != c.want {
			t.Errorf("%s: HasPRs = %t", name, got)
		}
	}
}

func TestRepoError(t *testing.T) {
	var b bytes.Buffer
	Repo(&b, false, runner.RepoResult{Repo: "o/r", Policy: config.Builtin, Error: "boom"})
	if want := "o/r  (squash, up to minor, checks required)\n  error: boom\n\n"; b.String() != want {
		t.Fatalf("got %q", b.String())
	}
}

func TestRepoTruncatesLongTitles(t *testing.T) {
	d := decision(1, policy.ActionSkip, "draft pull request")
	d.Title = strings.Repeat("x", 80)
	var b bytes.Buffer
	Repo(&b, false, runner.RepoResult{Repo: "o/r", Policy: config.Builtin, Decisions: []policy.Decision{d}})
	if !strings.Contains(b.String(), strings.Repeat("x", titleWidth-1)+"…\n") || strings.Contains(b.String(), strings.Repeat("x", titleWidth)) {
		t.Fatalf("title not truncated:\n%s", b.String())
	}
}

func TestSummaryAfterApply(t *testing.T) {
	failed := runner.RepoResult{Repo: "o/x", Error: "boom", Decisions: []policy.Decision{}}
	rebase := runner.RepoResult{Repo: "o/y", Events: []runner.Event{{Number: 9, Kind: runner.EventRebaseFailed}}}
	var b bytes.Buffer
	Summary(&b, true, []runner.RepoResult{applied, failed, rebase})
	want := "3 repos: 1 merged, 1 merge-failed, 1 rebase-failed, 1 wait; 1 need attention; 1 failed\n"
	if b.String() != want {
		t.Fatalf("got %q\nwant %q", b.String(), want)
	}
}

func TestSummaryDryRun(t *testing.T) {
	res := runner.RepoResult{Repo: "o/r", Decisions: []policy.Decision{
		decision(1, policy.ActionMerge, ""), decision(2, policy.ActionRebase, ""), decision(3, policy.ActionSkip, ""),
	}}
	var b bytes.Buffer
	Summary(&b, false, []runner.RepoResult{res})
	want := "1 repos: 1 would merge, 1 would rebase, 1 skip\ndry run: nothing was changed; rerun with --apply to merge\n"
	if b.String() != want {
		t.Fatalf("got %q\nwant %q", b.String(), want)
	}
}

func TestSummaryNothingToDo(t *testing.T) {
	var b bytes.Buffer
	Summary(&b, false, []runner.RepoResult{{Repo: "o/r"}})
	if want := "1 repos: nothing to do\n"; b.String() != want {
		t.Fatalf("got %q", b.String())
	}
}

func TestJSON(t *testing.T) {
	var b bytes.Buffer
	if err := JSON(&b, true, []runner.RepoResult{applied}); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Apply   bool
		Results []struct {
			Repo   string
			Policy struct {
				MaxUpdateType string `json:"max_update_type"`
			}
			Events []struct{ Kind string }
		}
	}
	if err := json.Unmarshal(b.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if !doc.Apply || len(doc.Results) != 1 || doc.Results[0].Policy.MaxUpdateType != "minor" || doc.Results[0].Events[1].Kind != "merge-failed" {
		t.Fatalf("doc = %+v", doc)
	}
}

func TestRepoHeaderShowsFallbackMergeMethod(t *testing.T) {
	var b bytes.Buffer
	Repo(&b, false, runner.RepoResult{Repo: "o/r", Policy: config.Builtin, MergeMethod: "merge"})
	if first, _, _ := strings.Cut(b.String(), "\n"); first != "o/r  (merge; squash not allowed, up to minor, checks required)" {
		t.Fatalf("header = %q", first)
	}
}

func TestRepoShowsAgentVerdict(t *testing.T) {
	d := decision(1, policy.ActionSkip, "major update exceeds limit (minor); rejected by agent")
	d.Grade = config.UpdateMajor
	res := runner.RepoResult{
		Repo: "o/r", Policy: config.Builtin, Permission: "WRITE",
		Decisions: []policy.Decision{d, decision(2, policy.ActionReview, "major update exceeds limit (minor); awaiting agent review (review limit of 1 reached)")},
		Findings: []policy.Finding{{
			Kind: policy.FindingAgentRejected, Summary: "#1 (major) rejected by the agent: drops Node 18",
			PRs: []int{1}, Details: []string{"package.json engines allows Node 18"},
		}},
	}
	var b bytes.Buffer
	Repo(&b, true, res)
	for _, want := range []string{"  needs attention:\n    - #1 (major) rejected by the agent: drops Node 18\n      - package.json engines allows Node 18\n", "  review  major update"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, b.String())
		}
	}
	b.Reset()
	Summary(&b, true, []runner.RepoResult{res})
	if got := b.String(); got != "1 repos: 1 review, 1 skip; 1 need attention\n" {
		t.Errorf("summary = %q", got)
	}
}
