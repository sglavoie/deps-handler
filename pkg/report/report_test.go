package report

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/sglavoie/deps-merger/pkg/config"
	"github.com/sglavoie/deps-merger/pkg/policy"
	"github.com/sglavoie/deps-merger/pkg/runner"
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
	repo(&b, false, res, style{links: true})
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
	// applied holds a failed merge.
	if want := "o/r: failed (2 PRs)\no/x: error: boom\n"; b.String() != want {
		t.Fatalf("got %q", b.String())
	}
}

func TestHasPRs(t *testing.T) {
	for name, c := range map[string]struct {
		res  runner.RepoResult
		want bool
	}{
		"no PRs":       {runner.RepoResult{Repo: "o/r"}, false},
		"error":        {runner.RepoResult{Repo: "o/r", Error: "boom"}, true},
		"decisions":    {runner.RepoResult{Repo: "o/r", Decisions: applied.Decisions}, true},
		"events only":  {runner.RepoResult{Repo: "o/r", Events: applied.Events}, true},
		"warning only": {runner.RepoResult{Repo: "o/r", Warnings: []string{"verdict cache unreadable"}}, true},
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

func TestRepoErrorStillShowsWhatWasDone(t *testing.T) {
	res := applied
	res.Error = "o/r: HTTP 502"
	res.Warnings = []string{"#4: the rebase request may not have been posted: HTTP 504"}
	var b bytes.Buffer
	Repo(&b, true, res)
	want := `o/r  (squash, up to minor, checks required, write access)
  error: o/r: HTTP 502
  #1  patch  merged        squash             bump lib
  #2  patch  merge-failed  405 not mergeable  bump lib
  warning: #4: the rebase request may not have been posted: HTTP 504

`
	if b.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", b.String(), want)
	}
}

func TestRepoWithOnlyAWarning(t *testing.T) {
	var b bytes.Buffer
	Repo(&b, true, runner.RepoResult{Repo: "o/r", Policy: config.Builtin, Warnings: []string{"verdict cache unreadable"}})
	want := "o/r  (squash, up to minor, checks required)\n  no open Dependabot PRs\n  warning: verdict cache unreadable\n\n"
	if b.String() != want {
		t.Fatalf("got %q\nwant %q", b.String(), want)
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
	Summary(&b, true, []runner.RepoResult{applied, failed, rebase}, 0)
	want := "3 repos: 1 merged, 1 merge failed, 1 rebase failed, 1 waiting; 1 needs attention; 3 repos failed\n"
	if b.String() != want {
		t.Fatalf("got %q\nwant %q", b.String(), want)
	}
}

func TestSummaryLeavesOutWhatAFailureHid(t *testing.T) {
	failed := applied
	failed.Error = "HTTP 502"
	var b bytes.Buffer
	Summary(&b, true, []runner.RepoResult{failed}, 0)
	want := "1 repo: 1 merged, 1 merge failed; 1 repo failed\n"
	if b.String() != want {
		t.Fatalf("got %q\nwant %q", b.String(), want)
	}
}

func TestSummaryDryRun(t *testing.T) {
	res := runner.RepoResult{Repo: "o/r", Decisions: []policy.Decision{
		decision(1, policy.ActionMerge, ""), decision(2, policy.ActionRebase, ""), decision(3, policy.ActionSkip, ""),
	}}
	var b bytes.Buffer
	Summary(&b, false, []runner.RepoResult{res}, 0)
	want := "1 repo: 1 would merge, 1 would rebase, 1 skipped\ndry run: nothing was changed; rerun with --apply to merge and rebase\n"
	if b.String() != want {
		t.Fatalf("got %q\nwant %q", b.String(), want)
	}
}

// --apply would change nothing in a repository in report mode: no rerun
// is suggested for it.
func TestSummaryDryRunInReportMode(t *testing.T) {
	p := config.Builtin
	p.Mode = config.ModeReport
	res := runner.RepoResult{Repo: "o/r", Policy: p, Decisions: []policy.Decision{decision(1, policy.ActionMerge, "")}}
	var b bytes.Buffer
	Summary(&b, false, []runner.RepoResult{res}, 0)
	if want := "1 repo: 1 would merge\n"; b.String() != want {
		t.Fatalf("got %q\nwant %q", b.String(), want)
	}
}

func TestSummaryNothingToDo(t *testing.T) {
	var b bytes.Buffer
	Summary(&b, false, []runner.RepoResult{{Repo: "o/r"}}, 0)
	if want := "1 repo: nothing to do\n"; b.String() != want {
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
	Summary(&b, true, []runner.RepoResult{res}, 0)
	if got := b.String(); got != "1 repo: 1 awaiting review, 1 skipped; 1 needs attention\n" {
		t.Errorf("summary = %q", got)
	}
}

func TestRepoColorsKeepColumnsAligned(t *testing.T) {
	var b bytes.Buffer
	repo(&b, true, applied, style{color: true})
	plain := regexp.MustCompile("\x1b\\[[0-9;]*m").ReplaceAllString(b.String(), "")
	var want bytes.Buffer
	Repo(&want, true, applied)
	if plain != want.String() {
		t.Fatalf("colored report differs once stripped:\n%s\nwant:\n%s", plain, want.String())
	}
	for _, s := range []string{"\x1b[1mo/r\x1b[0m", "\x1b[32mmerged\x1b[0m", "\x1b[1;31mmerge-failed\x1b[0m", "\x1b[33mwait\x1b[0m", "\x1b[32mpatch\x1b[0m"} {
		if !strings.Contains(b.String(), s) {
			t.Errorf("report lacks %q:\n%q", s, b.String())
		}
	}
}

func TestSummaryColors(t *testing.T) {
	var b bytes.Buffer
	summary(&b, true, []runner.RepoResult{applied, {Repo: "o/x", Error: "boom"}}, 0, style{color: true})
	want := "\x1b[1m2 repos:\x1b[0m \x1b[32m1 merged\x1b[0m, \x1b[1;31m1 merge failed\x1b[0m, \x1b[33m1 waiting\x1b[0m; \x1b[33m1 needs attention\x1b[0m; \x1b[31m2 repos failed\x1b[0m\n"
	if b.String() != want {
		t.Fatalf("got %q\nwant %q", b.String(), want)
	}
}

func TestRepoShowsWarningsAndAgentReviewOff(t *testing.T) {
	res := applied
	res.Policy.AgentReview = false
	res.Warnings = []string{"#1: verdict not cached: disk full"}
	var b bytes.Buffer
	Repo(&b, true, res)
	for _, want := range []string{"checks required, no agent review, write access)", "\n  warning: #1: verdict not cached: disk full\n"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing %q in:\n%s", want, b.String())
		}
	}
}

func TestProgressSingular(t *testing.T) {
	var b bytes.Buffer
	Progress(&b, runner.RepoResult{Repo: "o/r", Decisions: []policy.Decision{decision(1, policy.ActionWait, "")}})
	if b.String() != "o/r: done (1 PR)\n" {
		t.Fatalf("got %q", b.String())
	}
}

func TestSummaryCountsHiddenRepos(t *testing.T) {
	var b bytes.Buffer
	Summary(&b, true, []runner.RepoResult{applied}, 10)
	if want := "1 repo (10 without open PRs hidden): 1 merged, 1 merge failed, 1 waiting; 1 needs attention; 1 repo failed\n"; b.String() != want {
		t.Fatalf("got %q\nwant %q", b.String(), want)
	}
}

func TestReportDropsControlCharacters(t *testing.T) {
	d := decision(1, policy.ActionSkip, "major update exceeds limit (minor); agent review failed: claude: \x1b]52;c;aGk=\x07oops\nnext")
	res := runner.RepoResult{Repo: "o/r", Policy: config.Builtin, Decisions: []policy.Decision{d},
		Findings: []policy.Finding{{Kind: policy.FindingAgentRejected, Summary: "rejected: \x1b[2Jgone", Details: []string{"uses \x1b]8;;https://evil\x1b\\v1"}}}}
	var b bytes.Buffer
	repo(&b, false, res, style{color: true, links: true})
	out := b.String()
	for _, bad := range []string{"\x1b]52", "\x1b[2J", "\x1b]8;;https://evil", "\x07", "oops\nnext"} {
		if strings.Contains(out, bad) {
			t.Errorf("report passes %q through:\n%q", bad, out)
		}
	}
	if !strings.Contains(out, "oops next") {
		t.Errorf("a line break in a reason should read as a space:\n%q", out)
	}
}

func TestPlainDropsFormatCharacters(t *testing.T) {
	// Bidirectional overrides and isolates, and zero-width characters,
	// would let an agent's answer reorder or hide what the report shows.
	in := "safe\u202e,detacerped\u202c \u2066x\u2069 a\u200bb\u200dc\ufeffd\u00ade \u00e9"
	if got, want := Plain(in), "safe,detacerped x abcde \u00e9"; got != want {
		t.Errorf("Plain(%q) = %q, want %q", in, got, want)
	}
}

func TestNoColorWinsOverForce(t *testing.T) {
	var b bytes.Buffer
	t.Setenv("NO_COLOR", "")
	t.Setenv("CLICOLOR_FORCE", "1")
	if !styleFor(&b).color {
		t.Error("CLICOLOR_FORCE should color piped output")
	}
	t.Setenv("NO_COLOR", "1")
	if styleFor(&b).color {
		t.Error("NO_COLOR must win over CLICOLOR_FORCE")
	}
}

func TestSummaryDryRunRebaseOnly(t *testing.T) {
	res := runner.RepoResult{Repo: "o/r", Decisions: []policy.Decision{decision(1, policy.ActionRebase, "")}}
	var b bytes.Buffer
	Summary(&b, false, []runner.RepoResult{res, res}, 0)
	want := "2 repos: 2 would rebase\ndry run: nothing was changed; rerun with --apply to rebase\n"
	if b.String() != want {
		t.Fatalf("got %q\nwant %q", b.String(), want)
	}
}

func TestSummaryAllHidden(t *testing.T) {
	var b bytes.Buffer
	Summary(&b, false, nil, 1)
	if want := "no open Dependabot PRs in 1 repo\n"; b.String() != want {
		t.Fatalf("got %q\nwant %q", b.String(), want)
	}
}

func TestRepoInReportModeDuringApply(t *testing.T) {
	p := config.Builtin
	p.Mode, p.DependabotCommands = config.ModeReport, false
	res := runner.RepoResult{
		Repo:        "o/r",
		Policy:      p,
		ConfigFiles: []string{".github/deps-merger.yml"},
		Permission:  "WRITE",
		Decisions:   []policy.Decision{decision(1, policy.ActionMerge, "checks green, within policy")},
	}
	var b bytes.Buffer
	Repo(&b, true, res)
	want := `o/r  (squash, up to minor, checks required, no Dependabot commands, report only, write access)
  policy from .github/deps-merger.yml
  #1  patch  would merge  checks green, within policy  bump lib

`
	if b.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", b.String(), want)
	}
	b.Reset()
	Summary(&b, true, []runner.RepoResult{res}, 0)
	if want := "1 repo: 1 would merge\nrepos in report mode (mode: report) were only reported on\n"; b.String() != want {
		t.Fatalf("summary %q, want %q", b.String(), want)
	}
}

func TestMarkdown(t *testing.T) {
	p := config.Builtin
	p.DependabotCommands = false
	res := runner.RepoResult{
		Repo:        "o/r",
		Policy:      p,
		ConfigFiles: []string{"o/.github:deps-merger.yml"},
		Permission:  "WRITE",
		Decisions: []policy.Decision{
			{Number: 2, Title: "bump a|b for @octocat <b>now</b>", URL: "https://github.com/o/r/pull/2", Grade: config.UpdateMinor, Action: policy.ActionMerge, Reason: "checks green, within policy"},
			{Number: 3, Title: "bump `x`", URL: "javascript:alert(1)", Grade: config.UpdatePatch, Action: policy.ActionSkip, Reason: "behind base branch; rebase left to a human (dependabot_commands: false)"},
		},
		Events: []runner.Event{{Number: 1, Title: "bump lib", URL: "https://github.com/o/r/pull/1", Grade: config.UpdatePatch, Kind: runner.EventMerged, Detail: "squash"}},
		Findings: []policy.Finding{{
			Kind: policy.FindingBehind, Summary: "behind the base branch", PRs: []int{3},
			Suggestions: []string{"@dependabot rebase", "ask @team"},
		}},
	}
	var b bytes.Buffer
	Markdown(&b, true, res)
	want := "## o/r\n\n" +
		"Squash, up to minor, checks required, no Dependabot commands, write access\n" +
		"Policy from `o/.github:deps-merger.yml`.\n\n" +
		"| PR | Update | Status | Reason |\n| --- | --- | --- | --- |\n" +
		"| [#1 bump lib](https://github.com/o/r/pull/1) | patch | merged | squash |\n" +
		"| [#2 bump a\\|b for @\u200boctocat &lt;b&gt;now&lt;/b&gt;](https://github.com/o/r/pull/2) | minor | merge | checks green, within policy |\n" +
		"| #\u200b3 bump \\`x\\` | patch | skip | behind base branch; rebase left to a human (dependabot\\_commands: false) |\n\n" +
		"### Needs attention\n\n" +
		"- behind the base branch (o/r#3)\n" +
		"  - Suggested: `@dependabot rebase`\n" +
		"  - Suggested: ask @\u200bteam\n\n"
	if b.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", b.String(), want)
	}
}

func TestMarkdownReportModeAndError(t *testing.T) {
	p := config.Builtin
	p.Mode = config.ModeReport
	var b bytes.Buffer
	Markdown(&b, true, runner.RepoResult{Repo: "o/r", Policy: p, Decisions: []policy.Decision{decision(1, policy.ActionMerge, "ok")}})
	if !strings.Contains(b.String(), "| would merge |") || !strings.Contains(b.String(), "report only") {
		t.Errorf("report mode:\n%s", b.String())
	}
	b.Reset()
	Markdown(&b, false, runner.RepoResult{Repo: "o/r", Policy: config.Builtin, Error: "line\nbreak | pipe"})
	if want := "**Error:** line break \\| pipe\n\n"; !strings.Contains(b.String(), want) || strings.Contains(b.String(), "| PR |") {
		t.Errorf("error:\n%s", b.String())
	}
	b.Reset()
	Markdown(&b, false, runner.RepoResult{Repo: "o/r", Policy: config.Builtin, Decisions: []policy.Decision{}})
	if !strings.Contains(b.String(), "No open Dependabot PRs.") {
		t.Errorf("no PRs:\n%s", b.String())
	}
}

func TestMdLeadNamesTheRepo(t *testing.T) {
	for in, want := range map[string]string{
		"#58 (major) rejected by the agent: see #12": "o/r#58 (major) rejected by the agent: see #\u200b12",
		"#4: verdict not cached":                     "o/r#4: verdict not cached",
		"no checks ran":                              "no checks ran",
	} {
		if got := mdLead("o/r", in); got != want {
			t.Errorf("mdLead(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMdTextOpensNoBlock(t *testing.T) {
	for in, want := range map[string]string{
		"# Breaking: foo removed": `\# Breaking: foo removed`,
		"###":                     `\###`,
		"- a":                     `\- a`,
		"---":                     `\---`,
		"+ a":                     `\+ a`,
		"1. step":                 `1\. step`,
		"12) step":                `12\) step`,
		"    indented":            "indented",
		"#12 bump lib":            "#\u200b12 bump lib",
		"fixes #345 upstream":     "fixes #\u200b345 upstream",
		"C# 12":                   "C# 12",
		"see GH-123 upstream":     "see GH-\u200b123 upstream",
		"-1 is less":              "-1 is less",
		"1.2.3 is out":            "1.2.3 is out",
		"reads $GOPATH/$GOBIN":    `reads \$GOPATH/\$GOBIN`,
		// GitHub links no URL but the PR's own.
		"see https://evil.example/x": "see https:/\u200b/evil.example/x",
		"see WWW.evil.example":       "see WWW\u200b.evil.example",
		"_www.evil.example":          "\\_www\u200b.evil.example",
	} {
		if got := mdText(in); got != want {
			t.Errorf("mdText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMdPRLinksOnlyAPlainGitHubURL(t *testing.T) {
	for url, linked := range map[string]bool{
		"https://github.com/o/r/pull/1":         true,
		"https://github.com.evil.example/x":     false,
		"https://github.com/o/r/pull/1|x":       false,
		"https://github.com/o/r/pull/1\nx":      false,
		"https://github.com/o/r/pull/1\x1b[31m": false,
		"https://github.com/o/r/pull/1)[x](y":   false,
	} {
		if got := mdPR(1, "t", url); strings.Contains(got, "](") != linked {
			t.Errorf("mdPR(%q) = %q", url, got)
		}
	}
}

func TestTruncateCountsOnlyWhatIsShown(t *testing.T) {
	in := strings.Repeat("\u200b", 59) + "Bump lodash from 1 to 2"
	if got := truncate(in); got != "Bump lodash from 1 to 2" {
		t.Errorf("truncate = %q", got)
	}
}

func TestMdCode(t *testing.T) {
	for in, want := range map[string]string{
		"@dependabot rebase": "`@dependabot rebase`",
		"a`b":                "``a`b``",
		"`x`":                "`` `x` ``",
		"a\nb":               "`a b`",
	} {
		if got := mdCode(in); got != want {
			t.Errorf("mdCode(%q) = %q, want %q", in, got, want)
		}
	}
}
