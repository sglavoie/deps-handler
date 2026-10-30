package pr

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sglavoie/deps-handler/pkg/github"
)

func recommendationAnswer(t *testing.T, recommendations ...Recommendation) string {
	t.Helper()
	a := newResult(snapshot(github.CheckFailed)).Analysis
	a.Summary = "Assess the observed failures"
	a.Recommendations = append(a.Recommendations, recommendations...)
	data, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestNonCodeRecommendationsDoNotStartEditing(t *testing.T) {
	cases := []Recommendation{
		{"test", HumanAction, "The runner never started: account payments failed.", "Resolve billing, then rerun the failed job."},
		{"test", RerunCandidate, "The runner lost its network connection before checkout.", "Rerun the failed job and inspect any repeated failure."},
		{"test", InsufficientEvidence, "Job logs expired; no cause is established.", "Obtain a fresh job log before proposing code edits."},
	}
	for _, recommendation := range cases {
		t.Run(string(recommendation.Action), func(t *testing.T) {
			p := snapshot(github.CheckFailed)
			s, c, a, _ := service(p)
			c.checks = []github.PRCheck{{Check: p.Checks[0].Check, Log: recommendation.Reason}}
			a.raw = recommendationAnswer(t, recommendation)
			got, err := s.Diagnose(context.Background(), p.Target, "")
			if err != nil || got.RepairCommand != "" || a.calls != 1 || a.writable[0] {
				t.Fatalf("diagnosis=%+v err=%v agent=%+v", got, err, a)
			}
			if !strings.Contains(a.prompt, recommendation.Reason) || !strings.Contains(a.prompt, "Never infer a code repair merely because CI failed") {
				t.Fatal("assessment did not receive the failure evidence and action guidance", a.prompt)
			}
			var out strings.Builder
			if err := Markdown(&out, got); err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"## Recommended actions", recommendation.Action.label(), recommendation.Reason, recommendation.NextStep} {
				if !strings.Contains(out.String(), want) {
					t.Fatalf("missing %q in %s", want, &out)
				}
			}
			if strings.Contains(out.String(), "deps-handler pr fix") {
				t.Fatal("suggested a code repair for an external blocker", &out)
			}
			data, err := json.Marshal(got)
			if err != nil || !strings.Contains(string(data), `"action":"`+string(recommendation.Action)+`"`) {
				t.Fatalf("JSON lost recommendation: %s, %v", data, err)
			}
			for _, push := range []bool{false, true} {
				s, c, a, w := service(p)
				c.checks = []github.PRCheck{{Check: p.Checks[0].Check, Log: recommendation.Reason}}
				s.Push = push
				a.raw = recommendationAnswer(t, recommendation)
				got, err = s.Fix(context.Background(), p.Target, "")
				if err == nil || !strings.Contains(err.Error(), "no supported code repair") || got.Error == "" || got.Prepared || got.ChecksPassing || got.Attempts != 0 || w.commits != 0 || w.pushes != 0 || a.calls != 1 || a.writable[0] || len(got.Analysis.Recommendations) != 1 {
					t.Fatalf("push=%v result=%+v err=%v workspace=%+v agent=%+v", push, got, err, w, a)
				}
			}
		})
	}
}

func TestMixedFailuresRepairCodeAndKeepOtherActions(t *testing.T) {
	p := snapshot(github.CheckFailed)
	for _, name := range []string{"billing", "browser", "external CI"} {
		p.Checks = append(p.Checks, github.PRCheck{Check: github.Check{Name: name, State: github.CheckFailed}})
	}
	code := Recommendation{"test", CodeRepair, "src/app.go uses a removed API; replace its call.", "Update src/app.go and run go test ./..."}
	others := []Recommendation{
		{"billing", HumanAction, "Account billing prevented the runner from starting.", "Resolve billing."},
		{"browser", RerunCandidate, "A timing-sensitive assertion passed on a retry; the cause is uncertain.", "Rerun and inspect the trace if it fails again."},
		{"external CI", InsufficientEvidence, "External logs are unavailable.", "Read the external build logs."},
	}
	s, c, a, w := service(p)
	s.Push = false
	// Deliberately omit external blockers from the editor's answer. The caller
	// must retain the read-only assessment's non-code actions independently.
	a.raws = []string{recommendationAnswer(t, append([]Recommendation{code}, others...)...), recommendationAnswer(t, code)}
	diagnosis, err := s.Diagnose(context.Background(), p.Target, "")
	if err != nil || diagnosis.RepairCommand == "" || len(diagnosis.Analysis.Recommendations) != 4 {
		t.Fatalf("mixed diagnosis=%+v err=%v", diagnosis, err)
	}
	a.calls, a.writable = 0, nil
	got, err := s.Fix(context.Background(), p.Target, "")
	if err != nil || !got.Prepared || got.ChecksPassing || got.Attempts != 1 || a.calls != 2 || a.writable[0] || !a.writable[1] || w.commits != 1 || w.pushes != 0 || c.evidenceReads != 2 {
		t.Fatalf("result=%+v err=%v agent=%+v workspace=%+v evidence=%d", got, err, a, w, c.evidenceReads)
	}
	for _, want := range []string{"repair_assessment", "Fix only the code repairs", "do not try to fix those by editing code", "compiler: missing symbol"} {
		if !strings.Contains(a.prompt, want) {
			t.Fatalf("editing task missing %q: %s", want, a.prompt)
		}
	}
	for _, want := range others {
		found := false
		for _, r := range got.Analysis.Recommendations {
			if r == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("lost external action %+v in %+v", want, got.Analysis.Recommendations)
		}
	}
}

func TestRepairRetryStopsWhenOnlyHumanActionRemains(t *testing.T) {
	p := snapshot(github.CheckFailed)
	next := p
	next.HeadOID = "new-head"
	s, _, a, w := service(p, p, next)
	code := recommendationAnswer(t, Recommendation{"test", CodeRepair, "Replace removed API in src/app.go.", "Update the call."})
	human := recommendationAnswer(t, Recommendation{"test", HumanAction, "Account quota prevented the new job from starting.", "Restore the Actions quota."})
	a.raws = []string{code, code, human}
	got, err := s.Fix(context.Background(), p.Target, "")
	if err == nil || got.ChecksPassing || got.Attempts != 1 || w.pushes != 1 || len(got.Commits) != 1 || a.calls != 3 || a.writable[0] || !a.writable[1] || a.writable[2] || got.Analysis.Recommendations[0].Action != HumanAction {
		t.Fatalf("result=%+v err=%v agent=%+v workspace=%+v", got, err, a, w)
	}
}

func TestSavedPlansAreAssessedBeforeAnyEditing(t *testing.T) {
	for _, kind := range []string{"matching", "stale", "custom"} {
		t.Run(kind, func(t *testing.T) {
			p := snapshot(github.CheckFailed)
			s, c, a, w := service(p)
			s.Plan = savedPlan(p)
			if kind == "stale" {
				s.Plan.HeadOID = "earlier-head"
			} else if kind == "custom" {
				s.Plan = &RepairPlan{Text: "Fix billing before changing code."}
			}
			a.raw = recommendationAnswer(t, Recommendation{"test", HumanAction, "The plan requires an account billing fix.", "Resolve billing before rerunning CI."})
			got, err := s.Fix(context.Background(), p.Target, "")
			wantEvidence := 1
			if kind == "matching" {
				wantEvidence = 0
			}
			if err == nil || got.Prepared || a.calls != 1 || a.writable[0] || w.commits != 0 || w.pushes != 0 || c.evidenceReads != wantEvidence || !strings.Contains(a.prompt, "saved_repair_plan") || !strings.Contains(a.prompt, s.Plan.Text[:10]) {
				t.Fatalf("result=%+v err=%v agent=%+v workspace=%+v evidence=%d", got, err, a, w, c.evidenceReads)
			}
		})
	}
}

func TestUnclassifiedFailuresStayUncertain(t *testing.T) {
	for _, kind := range []string{"failed", "unknown", "pending", "all skipped", "missing", "incomplete"} {
		t.Run(kind, func(t *testing.T) {
			p := snapshot(github.CheckFailed)
			switch kind {
			case "unknown":
				p.Checks[0].State = github.CheckUnknown
			case "pending":
				p.Checks[0].State = github.CheckPending
			case "all skipped":
				p.Checks[0].State = github.CheckSkipped
			case "missing":
				p.Checks = nil
			case "incomplete":
				p.ChecksComplete = false
			}
			s, _, a, _ := service(p)
			a.raw = recommendationAnswer(t)
			got, err := s.Diagnose(context.Background(), p.Target, "")
			if err != nil || got.RepairCommand != "" || len(got.Analysis.Recommendations) == 0 || got.Analysis.Recommendations[0].Action != InsufficientEvidence {
				t.Fatalf("result=%+v err=%v", got, err)
			}
		})
	}
}

func TestObservedBaseRepairDoesNotDependOnAgentClassification(t *testing.T) {
	p := snapshot(github.CheckPassed)
	p.MergeState, p.Mergeable = "DIRTY", "CONFLICTING"
	s, _, a, _ := service(p)
	a.raw = recommendationAnswer(t)
	got, err := s.Diagnose(context.Background(), p.Target, "")
	if err != nil || got.RepairCommand == "" || len(got.Analysis.Recommendations) != 1 || got.Analysis.Recommendations[0].Action != CodeRepair {
		t.Fatalf("result=%+v err=%v", got, err)
	}
}

func TestCompletedLocalCommitsCanPublishWithoutEditing(t *testing.T) {
	p := snapshot(github.CheckFailed)
	passing := snapshot(github.CheckPassed)
	passing.HeadOID = "user-commit"
	s, _, a, _ := service(p, p, passing, passing)
	w := &committedRepairWorkspace{head: "user-commit"}
	s.Workspace = w
	a.raw = recommendationAnswer(t)
	got, err := s.Fix(context.Background(), p.Target, "")
	if err != nil || !got.ChecksPassing || got.Attempts != 0 || a.calls != 1 || a.writable[0] || w.pushes != 1 || got.Commits[0] != "user-commit" {
		t.Fatalf("result=%+v err=%v agent=%+v workspace=%+v", got, err, a, w)
	}
	if !strings.Contains(strings.Join(got.Analysis.Remaining, " "), "local validation commands were not run") {
		t.Fatal("read-only inspection claimed local validation", got.Analysis)
	}
}

func TestLocalCommitsDoNotBypassExternalRecommendations(t *testing.T) {
	p := snapshot(github.CheckFailed)
	s, _, a, _ := service(p)
	w := &committedRepairWorkspace{head: "user-commit"}
	s.Workspace = w
	a.raw = recommendationAnswer(t, Recommendation{"test", HumanAction, "Billing still prevents CI from starting.", "Resolve billing before publication."})
	got, err := s.Fix(context.Background(), p.Target, "")
	if err == nil || got.Prepared || w.commits != 0 || w.pushes != 0 || a.calls != 1 || a.writable[0] {
		t.Fatalf("result=%+v err=%v agent=%+v workspace=%+v", got, err, a, w)
	}
}

func TestInvalidRecommendationsCannotStartEditingOrCommit(t *testing.T) {
	valid := recommendationAnswer(t, Recommendation{"test", CodeRepair, "Replace removed API in src/app.go.", "Update the call."})
	cases := map[string]string{
		"missing recommendations":   `{"summary":"ok","causes":[],"steps":[],"validation":[],"remaining":[]}`,
		"null recommendations":      `{"summary":"ok","causes":[],"steps":[],"validation":[],"remaining":[],"recommendations":null}`,
		"null string element":       strings.Replace(valid, `"causes":[]`, `"causes":[null]`, 1),
		"unknown action":            strings.Replace(valid, `"code_repair"`, `"try_anything"`, 1),
		"duplicate action":          strings.Replace(valid, `"action":"code_repair"`, `"action":"human_action","action":"code_repair"`, 1),
		"duplicate recommendations": strings.Replace(valid, `"recommendations":`, `"recommendations":[],"recommendations":`, 1),
		"case-varied action":        strings.Replace(valid, `"action":`, `"Action":`, 1),
		"missing reason":            strings.Replace(valid, `"reason":"Replace removed API in src/app.go.",`, "", 1),
		"empty reason":              strings.Replace(valid, `"Replace removed API in src/app.go."`, `" "`, 1),
		"null subject":              strings.Replace(valid, `"subject":"test"`, `"subject":null`, 1),
		"unknown field":             strings.Replace(valid, `"subject":"test"`, `"subject":"test","extra":true`, 1),
		"null recommendation":       `{"summary":"ok","causes":[],"steps":[],"validation":[],"remaining":[],"recommendations":[null]}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			for _, editing := range []bool{false, true} {
				p := snapshot(github.CheckFailed)
				s, _, a, w := service(p)
				a.raws = []string{raw}
				if editing {
					a.raws = []string{valid, raw}
				}
				_, err := s.Fix(context.Background(), p.Target, "")
				if err == nil || w.commits != 0 || w.pushes != 0 || (!editing && (a.calls != 1 || a.writable[0])) {
					t.Fatalf("accepted %s at editing=%v: err=%v agent=%+v workspace=%+v", raw, editing, err, a, w)
				}
			}
		})
	}
}
