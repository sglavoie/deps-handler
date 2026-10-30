package pr

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sglavoie/deps-handler/pkg/github"
)

func TestPlanRoundTripsDiagnosisMarkdown(t *testing.T) {
	result := newResult(snapshot(github.CheckFailed))
	result.Analysis.Steps = []string{"Replace the removed API in src/app.go"}
	var markdown strings.Builder
	if err := Markdown(&markdown, result); err != nil {
		t.Fatal(err)
	}
	plan, err := ParsePlan([]byte(markdown.String()))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Text != markdown.String() || plan.Target != result.Snapshot.Target || plan.HeadOID != result.Snapshot.HeadOID || plan.BaseOID != result.Snapshot.BaseOID || plan.BaseRef != result.Snapshot.BaseRef {
		t.Fatalf("parsed plan: %+v", plan)
	}
	if !plan.MatchesSnapshot(result.Snapshot) {
		t.Fatal("fresh plan reported stale")
	}
	current := result.Snapshot
	current.HeadOID = "new-head"
	if plan.MatchesSnapshot(current) {
		t.Fatal("changed head reported fresh")
	}
	current = result.Snapshot
	current.BaseOID = "new-base"
	if plan.MatchesSnapshot(current) {
		t.Fatal("changed base reported fresh")
	}
	current = result.Snapshot
	current.BaseRef = "release"
	if plan.MatchesSnapshot(current) {
		t.Fatal("changed target branch reported fresh")
	}
}

func TestPlanPreservesUserEditsAndIgnoresBodyReferences(t *testing.T) {
	text := "\ufeff# O/R#7: repair\r\nPR: https://github.com/o/r/pull/7\r\nHead: `head`\r\nBase: `main` (`base`)\r\n\r\n## Repair steps\r\n1. Use my revised approach.\r\nPR: https://github.com/other/project/pull/99\r\n"
	plan, err := ParsePlan([]byte(text))
	if err != nil || plan.Text != text || plan.Target.Number != 7 {
		t.Fatalf("%+v %v", plan, err)
	}
	if err := plan.ValidateTarget(github.PRTarget{Owner: "O", Repo: "R", Number: 7}); err != nil {
		t.Fatal(err)
	}
	if err := plan.ValidateTarget(github.PRTarget{Owner: "o", Repo: "r", Number: 8}); err == nil {
		t.Fatal("accepted different PR")
	}
}

func TestPlanAcceptsFreeFormMarkdownWithExplicitTarget(t *testing.T) {
	plan, err := ParsePlan([]byte("# My repair\n\nUpdate src/app.go and run go test ./...\n"))
	if err != nil || plan.Target.Number != 0 {
		t.Fatalf("%+v %v", plan, err)
	}
	if err := plan.ValidateTarget(snapshot(github.CheckFailed).Target); err != nil {
		t.Fatal(err)
	}
}

func TestPlanRejectsBadInput(t *testing.T) {
	for name, text := range map[string]string{
		"empty":           " \n\t",
		"binary":          "hello\x00world",
		"invalid UTF8":    "hello\xff",
		"too large":       strings.Repeat("a", MaxPlanBytes+1),
		"mismatched PRs":  "# o/r#7: repair\nPR: https://github.com/o/r/pull/8\n",
		"foreign host":    "PR: https://example.com/o/r/pull/7\n",
		"bad head":        "Head: not formatted\n",
		"duplicate heads": "Head: `one`\nHead: `two`\n",
		"bad base":        "Base: main\n",
		"duplicate bases": "Base: `main` (`base`)\nBase: `release` (`base`)\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParsePlan([]byte(text)); err == nil {
				t.Fatal("accepted bad input")
			}
		})
	}
}

func TestLoadPlanReadsWithoutChangingTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "repair plan.md")
	text := "PR: https://github.com/o/r/pull/7\n\n## Repair steps\n1. Apply my edits.\n"
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	plan, err := LoadPlan(path)
	if err != nil || plan.Source != path || plan.Text != text {
		t.Fatalf("%+v %v", plan, err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != text {
		t.Fatalf("source was modified: %s %v", data, err)
	}
	if _, err := LoadPlan(t.TempDir()); err == nil {
		t.Fatal("accepted a directory")
	}
	if _, err := LoadPlan(filepath.Join(t.TempDir(), "missing.md")); err == nil {
		t.Fatal("accepted missing file")
	}
}

func savedPlan(p github.PRSnapshot) *RepairPlan {
	return &RepairPlan{Source: "repair.md", Target: p.Target, HeadOID: p.HeadOID, BaseOID: p.BaseOID, BaseRef: p.BaseRef, Text: "## Repair steps\nReplace old call with the user-edited approach in src/app.go.\n"}
}

func TestFixFollowsSavedPlanWithoutNewFailureInvestigation(t *testing.T) {
	old := snapshot(github.CheckFailed)
	passing := snapshot(github.CheckPassed)
	passing.HeadOID = "new-head"
	s, c, a, w := service(old, old, passing, passing)
	s.Plan = savedPlan(old)
	result, err := s.Fix(context.Background(), old.Target, "compiler")
	if err != nil || !result.ChecksPassing || c.evidenceReads != 0 || a.calls != 2 || w.pushes != 1 || result.PlanSource != "repair.md" {
		t.Fatalf("result %+v err %v evidence=%d agent=%d pushes=%d", result, err, c.evidenceReads, a.calls, w.pushes)
	}
	for _, want := range []string{"saved_repair_plan", "user-edited approach", "Do not start a new diagnosis", "User focus: compiler"} {
		if !strings.Contains(a.prompt, want) {
			t.Fatalf("missing %q in %s", want, a.prompt)
		}
	}
}

func TestFixRefreshesEvidenceForStalePlansAndRetries(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(map[bool]string{false: "retry", true: "stale"}[stale], func(t *testing.T) {
			old := snapshot(github.CheckFailed)
			failed := old
			failed.HeadOID = "new-head"
			passing := snapshot(github.CheckPassed)
			passing.HeadOID = "new-head"
			states := []github.PRSnapshot{old, old, failed, failed, passing, passing}
			if stale {
				states = []github.PRSnapshot{old, old, passing, passing}
			}
			s, c, a, _ := service(states...)
			s.Plan = savedPlan(old)
			var progress []string
			s.Progress = func(message string) { progress = append(progress, message) }
			if stale {
				s.Plan.HeadOID = "earlier-head"
			}
			result, err := s.Fix(context.Background(), old.Target, "")
			if err != nil || !result.ChecksPassing || c.evidenceReads != 1 || !strings.Contains(a.prompt, "compiler: missing symbol") || !strings.Contains(a.prompt, "user-edited approach") {
				t.Fatalf("result %+v err %v evidence=%d prompt=%s", result, err, c.evidenceReads, a.prompt)
			}
			if stale && !strings.Contains(strings.Join(progress, " "), "adapting the plan") {
				t.Fatal("stale plan was not reported")
			}
		})
	}
}

func TestFixRejectsPlanForAnotherPRBeforeReadingGitHub(t *testing.T) {
	old := snapshot(github.CheckFailed)
	s, c, a, w := service(old)
	s.Plan = savedPlan(old)
	s.Plan.Target.Number++
	if _, err := s.Fix(context.Background(), old.Target, ""); err == nil || c.reads != 0 || a.calls != 0 || w.checkouts != 0 || w.pushes != 0 {
		t.Fatalf("err %v reads=%d calls=%d workspace=%+v", err, c.reads, a.calls, w)
	}
}

func TestDiagnoseDoesNotUseServiceRepairPlan(t *testing.T) {
	old := snapshot(github.CheckFailed)
	s, c, a, _ := service(old)
	s.Plan = savedPlan(old)
	if _, err := s.Diagnose(context.Background(), old.Target, ""); err != nil {
		t.Fatal(err)
	}
	if c.evidenceReads != 1 || strings.Contains(a.prompt, "saved_repair_plan") {
		t.Fatalf("evidence=%d prompt=%s", c.evidenceReads, a.prompt)
	}
}

func TestFixRefreshesEvidenceForPlanWithoutCommitMetadata(t *testing.T) {
	old := snapshot(github.CheckFailed)
	passing := snapshot(github.CheckPassed)
	passing.HeadOID = "new-head"
	s, c, a, _ := service(old, old, passing, passing)
	s.Plan = &RepairPlan{Source: "custom.md", Text: "Replace the old API call in src/app.go and run tests."}
	result, err := s.Fix(context.Background(), old.Target, "")
	if err != nil || !result.ChecksPassing || c.evidenceReads != 1 || !strings.Contains(a.prompt, s.Plan.Text) || !strings.Contains(a.prompt, "compiler: missing symbol") {
		t.Fatalf("result %+v err %v evidence=%d prompt=%s", result, err, c.evidenceReads, a.prompt)
	}
}
