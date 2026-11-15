package report

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sglavoie/deps-handler/pkg/config"
	"github.com/sglavoie/deps-handler/pkg/dependabot"
	"github.com/sglavoie/deps-handler/pkg/policy"
	"github.com/sglavoie/deps-handler/pkg/runner"
)

func TestCompactTitlesOnlyInTable(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	dep := dependabot.Dependency{Name: "lodash", PreviousVersion: "4.17.20", Version: "4.17.21"}
	original := "Bump lodash from 4.17.20 to 4.17.21"
	for _, tc := range []struct {
		name string
		deps []dependabot.Dependency
		want string
	}{
		{"complete", []dependabot.Dependency{dep}, "lodash 4.17.20 → 4.17.21"},
		{"absent", nil, original},
		{"no source", []dependabot.Dependency{{Name: dep.Name, Version: dep.Version}}, original},
		{"no target", []dependabot.Dependency{{Name: dep.Name, PreviousVersion: dep.PreviousVersion}}, original},
		{"no name", []dependabot.Dependency{{PreviousVersion: dep.PreviousVersion, Version: dep.Version}}, original},
		{"multiple", []dependabot.Dependency{dep, dep}, original},
		{"named group", []dependabot.Dependency{{Name: dep.Name, PreviousVersion: dep.PreviousVersion, Version: dep.Version, Group: "group"}}, original},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, applied := range []bool{false, true} {
				res := runner.RepoResult{Repo: "o/r", Policy: config.Builtin}
				if applied {
					res.Events = []runner.Event{{Number: 1, Title: original, Kind: runner.EventMerged, Dependencies: tc.deps}}
				} else {
					res.Decisions = []policy.Decision{{Number: 1, Title: original, Action: policy.ActionMerge, Dependencies: tc.deps}}
				}
				var table, list, md, js bytes.Buffer
				if err := RepoTable(&table, applied, res); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(table.String(), tc.want) {
					t.Fatalf("missing %q: %s", tc.want, &table)
				}
				Repo(&list, applied, res)
				Markdown(&md, applied, res)
				JSON(&js, applied, []runner.RepoResult{res})
				for name, out := range map[string]string{"list": list.String(), "markdown": md.String(), "json": js.String()} {
					if !strings.Contains(out, original) || strings.Contains(out, "previous_version") || strings.Contains(out, "PreviousVersion") {
						t.Fatalf("changed %s: %s", name, out)
					}
				}
			}
		})
	}
}

func TestTableFindingsGroupWithoutMutatingOrLosingDetails(t *testing.T) {
	holds := []policy.Finding{
		{Kind: policy.FindingHeldMinor, PRs: []int{57}, Summary: "#57 is a minor update held only by policy (allow with max_update_type: minor, or run with --review)"},
		{Kind: policy.FindingHeldMajor, PRs: []int{58}, Summary: "#58 is a major update held only by policy (allow with max_update_type: major, or run with --review)"},
		{Kind: policy.FindingAgentRejected, PRs: []int{60}, Summary: "#60 rejected", Details: []string{"own concern"}},
		{Kind: policy.FindingIncompatible, PRs: []int{61}, Summary: "#61 fails test"},
		{Kind: policy.FindingIncompatible, PRs: []int{62}, Summary: "#62 fails test"},
		{Kind: policy.FindingHeldMinor, PRs: []int{63}, Summary: "#63 custom hold", Details: []string{"own detail"}},
	}
	res := runner.RepoResult{Policy: config.Builtin, Findings: holds}
	before, _ := json.Marshal(res)
	got := tableFindings(res)
	after, _ := json.Marshal(res)
	if !bytes.Equal(before, after) {
		t.Fatal("mutated result")
	}
	if len(got) != 5 || got[0].Summary != "held by policy: #57 (minor), #58 (major) — allow with max_update_type, or run with --review" || !reflect.DeepEqual(got[1:], holds[2:]) {
		t.Fatalf("findings: %+v", got)
	}
	// Disabling agent review must not suggest it.
	res.Policy.AgentReview = false
	res.Findings = []policy.Finding{{Kind: policy.FindingHeldMajor, PRs: []int{58}, Summary: "#58 is a major update held only by policy (allow with max_update_type: major)"}}
	if strings.Contains(tableFindings(res)[0].Summary, "--review") {
		t.Fatal("suggested disabled review")
	}
}

func TestEquivalentSuggestionsKeepEachCommand(t *testing.T) {
	res := runner.RepoResult{Findings: []policy.Finding{
		{Kind: policy.FindingStalledRebase, PRs: []int{1}, Summary: "#1: rebase stalled", Suggestions: []string{"deps-handler pr diagnose o/r#1", "see #12"}},
		{Kind: policy.FindingStalledRebase, PRs: []int{2}, Summary: "#2: rebase stalled", Suggestions: []string{"deps-handler pr diagnose o/r#2", "see #12"}},
	}}
	got := tableFindings(res)
	if len(got) != 1 || got[0].Summary != "#1, #2: rebase stalled" || len(got[0].Suggestions) != 3 {
		t.Fatalf("grouped: %+v", got)
	}
}

func TestTableFindingsGroupedHeldUnknownGrammar(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	for _, review := range []string{"", ", or run with --review"} {
		for _, count := range []int{2, 3} {
			t.Run(fmt.Sprintf("count=%d/review=%t", count, review != ""), func(t *testing.T) {
				res := runner.RepoResult{Repo: "o/r", Policy: config.Builtin}
				res.Policy.AgentReview = review != ""
				for n := 204; n < 204+count; n++ {
					res.Findings = append(res.Findings, policy.Finding{
						Kind:    policy.FindingHeldUnknown,
						PRs:     []int{n},
						Summary: fmt.Sprintf("#%d is held only because Dependabot records no semver size for it (merge it by hand%s)", n, review),
					})
				}
				before, err := json.Marshal(res)
				if err != nil {
					t.Fatal(err)
				}
				var table, list, md bytes.Buffer
				if err := RepoTable(&table, false, res); err != nil {
					t.Fatal(err)
				}
				if err := Repo(&list, false, res); err != nil {
					t.Fatal(err)
				}
				if err := Markdown(&md, false, res); err != nil {
					t.Fatal(err)
				}
				for _, f := range res.Findings {
					for name, output := range map[string]string{"table": table.String(), "list": list.String(), "markdown": md.String()} {
						if !strings.Contains(output, f.Summary) {
							t.Errorf("%s missing separate grammatical hold %q: %s", name, f.Summary, output)
						}
					}
				}
				after, err := json.Marshal(res)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(before, after) {
					t.Fatal("rendering mutated the underlying findings or JSON")
				}
			})
		}
	}
}

func TestTableSuggestionsImmediatelyFollowFinding(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	for _, kind := range []policy.FindingKind{policy.FindingReviewRequired, policy.FindingBehind, policy.FindingIncompatible} {
		t.Run(string(kind), func(t *testing.T) {
			res := runner.RepoResult{Findings: []policy.Finding{{
				Kind: kind, Summary: "needs attention", PRs: []int{207, 208},
				Details: []string{"extra context"}, Suggestions: []string{"first suggestion", "second suggestion"},
			}}}
			var table, list bytes.Buffer
			if err := RepoTable(&table, false, res); err != nil {
				t.Fatal(err)
			}
			if err := Repo(&list, false, res); err != nil {
				t.Fatal(err)
			}
			prs := ""
			if kind != policy.FindingIncompatible {
				prs = "      PRs: #207, #208\n"
			}
			want := "    - needs attention — suggest:\n      first suggestion\n      second suggestion\n" + prs + "      - extra context\n"
			if !strings.Contains(table.String(), want) {
				t.Fatalf("suggestions must precede PRs and details: %s", table.String())
			}
			legacy := "    - needs attention\n" + prs + "      - extra context\n      suggest: first suggestion\n      suggest: second suggestion\n"
			if !strings.Contains(list.String(), legacy) {
				t.Fatalf("list layout changed: %s", list.String())
			}
		})
	}
}

func TestTableSuggestionInvocationAndSanitization(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	old := os.Args
	t.Cleanup(func() { os.Args = old })
	for _, name := range []string{"/bin/dh", "deps-handler", "dh.exe"} {
		os.Args = []string{name}
		res := runner.RepoResult{Findings: []policy.Finding{{Summary: "fails test\x1b\u202e", Suggestions: []string{"deps-handler pr diagnose o/r#1 --list > repair.md", "@dependabot recreate"}}}}
		var b bytes.Buffer
		RepoTable(&b, false, res)
		prefix := "deps-handler"
		if name != "deps-handler" {
			prefix = "dh"
		}
		want := "    - fails test — suggest:\n      " + prefix + " pr diagnose o/r#1 --list > repair.md\n      @dependabot recreate\n"
		if !strings.Contains(b.String(), want) {
			t.Fatalf("%s: %q", name, b.String())
		}
	}
}

func TestTableSummaryPayoff(t *testing.T) {
	old := os.Args
	os.Args = []string{"dh"}
	t.Cleanup(func() { os.Args = old })
	t.Setenv("NO_COLOR", "1")
	for _, tc := range []struct {
		name    string
		apply   bool
		results []runner.RepoResult
		hidden  int
		want    string
	}{
		{"apply", true, []runner.RepoResult{applied}, 0, "done in 41s · 1 needs you: dh dependabot --open\n"},
		{"dry", false, []runner.RepoResult{{Decisions: []policy.Decision{decision(1, policy.ActionMerge, "")}}}, 0, "dry run: nothing was changed; rerun with --apply to merge\nelapsed 41s\n"},
		{"empty", true, nil, 2, "no open Dependabot PRs in 2 repos\ndone in 41s\n"},
		{"no actions", false, nil, 0, "nothing to do\nelapsed 41s\n"},
		{"stale findings", true, []runner.RepoResult{{Error: "failed", Findings: applied.Findings}}, 0, "done in 41s\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			if err := Summary(&b, tc.apply, tc.results, tc.hidden, 41*time.Second); err != nil {
				t.Fatal(err)
			}
			if !strings.HasSuffix(b.String(), tc.want) {
				t.Fatalf("%q", b.String())
			}
		})
	}
	if err := Summary(shortTableWriter{}, true, nil, 0, time.Second); err == nil {
		t.Fatal("lost write failure")
	}
}

func TestStatusGlyphs(t *testing.T) {
	for label, glyph := range map[string]string{"merged": "✓", "would merge": "✓", "rebase-requested": "↻", "would rebase": "↻", "wait": "…", "review": "?", "skip": "·", "merge-failed": "✗", "rebase-failed": "✗"} {
		if got := StatusLabel(label); got != glyph+" "+label {
			t.Errorf("%s: %q", label, got)
		}
	}
}
