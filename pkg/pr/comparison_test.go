package pr

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sglavoie/deps-handler/pkg/github"
)

func comparisonCheck(state github.CheckState, event string, suite int64) github.PRCheck {
	return github.PRCheck{ID: suite * 10, Check: github.Check{Name: "test", State: state, App: 1, Workflow: 2, Suite: suite, Event: event}}
}

func TestBaseComparison(t *testing.T) {
	failed := comparisonCheck(github.CheckFailed, "pull_request", 20)
	passed := comparisonCheck(github.CheckPassed, "push", 10)
	for _, test := range []struct {
		name    string
		head    github.PRCheck
		base    []github.PRCheck
		outcome string
		note    string
	}{
		{"existing failure", failed, []github.PRCheck{comparisonCheck(github.CheckFailed, "push", 10)}, "both-failed", "root causes"},
		{"PR-only failure", failed, []github.PRCheck{passed}, "base-passed-pr-failed", "does not establish"},
		{"same trigger", failed, []github.PRCheck{comparisonCheck(github.CheckFailed, "pull_request", 10)}, "both-failed", "root causes"},
		{"passing", comparisonCheck(github.CheckPassed, "push", 20), []github.PRCheck{passed}, "same-outcome", "same observed outcome"},
		{"fixed on PR", comparisonCheck(github.CheckPassed, "push", 20), []github.PRCheck{comparisonCheck(github.CheckFailed, "push", 10)}, "different-outcomes", "different observed outcomes"},
		{"missing", failed, nil, "missing-base-check", "no matching"},
		{"pending base", failed, []github.PRCheck{comparisonCheck(github.CheckPending, "push", 10)}, "inconclusive", "pending"},
		{"skipped base", failed, []github.PRCheck{comparisonCheck(github.CheckSkipped, "push", 10)}, "inconclusive", "skipped"},
		{"stale base", failed, []github.PRCheck{comparisonCheck(github.CheckStale, "push", 10)}, "inconclusive", "stale"},
		{"unknown base", failed, []github.PRCheck{comparisonCheck(github.CheckUnknown, "push", 10)}, "inconclusive", "unknown"},
		{"pending PR", comparisonCheck(github.CheckPending, "push", 20), []github.PRCheck{passed}, "inconclusive", "pending"},
		{"missing head identity", github.PRCheck{Check: github.Check{Name: "test", State: github.CheckFailed}}, []github.PRCheck{passed}, "uncomparable", "identity is missing"},
		{"external status", github.PRCheck{Check: github.Check{Name: "test", State: github.CheckFailed, Status: true}}, []github.PRCheck{{Check: github.Check{Name: "test", State: github.CheckPassed, Status: true}}}, "uncomparable", "identity is missing"},
		{"missing base event", failed, []github.PRCheck{comparisonCheck(github.CheckPassed, "", 10)}, "inconclusive", "trigger identity is missing"},
		{"ambiguous", failed, []github.PRCheck{passed, comparisonCheck(github.CheckFailed, "push", 10)}, "inconclusive", "multiple base runs"},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := snapshot(test.head.State)
			p.Checks = []github.PRCheck{test.head}
			got := compareBase(p, github.BaseChecks{OID: p.BaseOID, ChecksComplete: true, Checks: test.base})
			if len(got.Checks) != 1 || got.Checks[0].Outcome != test.outcome || !strings.Contains(got.Checks[0].Note, test.note) {
				t.Fatalf("comparison: %+v", got)
			}
			if len(got.Checks[0].Base) == 1 && got.Checks[0].Base[0].Event == "push" && test.head.Event == "pull_request" && !strings.Contains(got.Checks[0].Note, "triggers differ") {
				t.Fatalf("trigger difference lost: %+v", got.Checks[0])
			}
		})
	}
}

func TestBaseComparisonProducerIdentityAndReruns(t *testing.T) {
	failed := comparisonCheck(github.CheckFailed, "pull_request", 20)
	p := snapshot(github.CheckFailed)
	p.Checks = []github.PRCheck{failed}
	base := github.BaseChecks{OID: p.BaseOID, ChecksComplete: true}
	for _, field := range []string{"app", "workflow", "status"} {
		t.Run(field, func(t *testing.T) {
			other := comparisonCheck(github.CheckPassed, "push", 10)
			switch field {
			case "app":
				other.App++
			case "workflow":
				other.Workflow++
			case "status":
				other.Status = true
			}
			base.Checks = []github.PRCheck{other}
			if got := compareBase(p, base); got.Checks[0].Outcome != "missing-base-check" {
				t.Fatalf("unrelated producer matched: %+v", got.Checks)
			}
		})
	}
	base.Checks = []github.PRCheck{comparisonCheck(github.CheckFailed, "push", 5), comparisonCheck(github.CheckPassed, "push", 10)}
	got := compareBase(p, base)
	if got.Checks[0].Outcome != "base-passed-pr-failed" || len(got.Base.Checks) != 1 || got.Base.Checks[0].Suite != 10 {
		t.Fatalf("superseded failure retained: %+v", got)
	}
	base.Checks = append(base.Checks, comparisonCheck(github.CheckSkipped, "push", 11))
	if got := compareBase(p, base); got.Checks[0].Outcome != "inconclusive" {
		t.Fatalf("skipped rerun hid prior run: %+v", got)
	}
	base.Checks = []github.PRCheck{comparisonCheck(github.CheckFailed, "push", 10), comparisonCheck(github.CheckPassed, "pull_request", 5)}
	got = compareBase(p, base)
	if got.Checks[0].Outcome != "base-passed-pr-failed" || got.Checks[0].Base[0].Event != "pull_request" || strings.Contains(got.Checks[0].Note, "triggers differ") {
		t.Fatalf("same trigger was not preferred: %+v", got)
	}
}

func TestBaseComparisonGaps(t *testing.T) {
	for _, test := range []string{"incomplete base", "incomplete head", "unavailable", "wrong commit", "missing commit", "no checks"} {
		t.Run(test, func(t *testing.T) {
			p := snapshot(github.CheckFailed)
			p.Checks = []github.PRCheck{comparisonCheck(github.CheckFailed, "pull_request", 20)}
			base := github.BaseChecks{OID: p.BaseOID, ChecksComplete: true, Checks: []github.PRCheck{comparisonCheck(github.CheckPassed, "push", 10)}}
			switch test {
			case "incomplete base":
				base.ChecksComplete = false
			case "incomplete head":
				p.ChecksComplete = false
			case "unavailable":
				base.EvidenceError = "access denied"
			case "wrong commit":
				base.OID = "later-main"
			case "missing commit":
				base.OID = ""
			case "no checks":
				p.Checks = nil
			}
			got := compareBase(p, base)
			if test == "no checks" {
				if got.Checks == nil || len(got.Checks) != 0 || len(got.Base.Checks) != 1 {
					t.Fatalf("empty PR checks lost base evidence: %+v", got)
				}
			} else if len(got.Gaps) == 0 || got.Checks[0].Outcome != "inconclusive" {
				t.Fatalf("gap did not prevent attribution: %+v", got)
			}
		})
	}
}

func TestComparisonReachesDiagnosisRepairAndReports(t *testing.T) {
	for _, repair := range []bool{false, true} {
		t.Run(map[bool]string{false: "diagnose", true: "repair"}[repair], func(t *testing.T) {
			p := snapshot(github.CheckFailed)
			p.Checks = []github.PRCheck{comparisonCheck(github.CheckFailed, "pull_request", 20)}
			s, c, a, _ := service(p)
			s.Push = false
			b := comparisonCheck(github.CheckFailed, "push", 10)
			b.URL, b.Log = "https://github.com/o/r/actions/runs/1/job/100", "base compiler failure\n```\nlog tail"
			b.EvidenceError = "annotations unavailable"
			c.base = github.BaseChecks{OID: p.BaseOID, ChecksComplete: true, Checks: []github.PRCheck{b}}
			var got Result
			var err error
			if repair {
				got, err = s.Fix(context.Background(), p.Target, "")
			} else {
				got, err = s.Diagnose(context.Background(), p.Target, "")
			}
			if err != nil || got.BaseComparison == nil || got.BaseComparison.Checks[0].Outcome != "both-failed" {
				t.Fatalf("result %+v, error %v", got, err)
			}
			for _, want := range []string{`"base_comparison"`, `"head_oid":"old-head"`, `"oid":"base"`, `"workflow_id":2`, `"event":"push"`, "base compiler failure", "Compare failure logs and code"} {
				if !strings.Contains(a.prompt, want) {
					t.Fatalf("prompt missing %q: %s", want, a.prompt)
				}
			}
			data, err := json.Marshal(got)
			if err != nil || !strings.Contains(string(data), `"base_comparison"`) || !strings.Contains(string(data), "base compiler failure") || !strings.Contains(strings.Join(got.Analysis.Remaining, " "), "annotations unavailable") {
				t.Fatalf("JSON or evidence gap missing: %s, %v", data, err)
			}
			var out strings.Builder
			if err := Markdown(&out, got); err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"## Base CI comparison", "**both-failed**", "workflow 2", "event push", b.URL, "base compiler failure", "    ```", "annotations unavailable", "triggers differ"} {
				if !strings.Contains(out.String(), want) {
					t.Fatalf("Markdown missing %q: %s", want, out.String())
				}
			}
			if !repair {
				plan, err := ParsePlan([]byte(out.String()))
				if err != nil || !plan.MatchesSnapshot(p) {
					t.Fatalf("comparison broke saved plan metadata: %+v, %v", plan, err)
				}
				s.Plan = plan
				if _, err := s.Fix(context.Background(), p.Target, ""); err != nil {
					t.Fatal(err)
				}
				if c.evidenceReads != 1 || !strings.Contains(a.prompt, "base compiler failure") || !strings.Contains(a.prompt, "both-failed") {
					t.Fatalf("saved comparison not reused: reads %d, prompt %s", c.evidenceReads, a.prompt)
				}
			}
		})
	}
}
