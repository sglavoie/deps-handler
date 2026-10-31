package diagnosiseval

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sglavoie/deps-handler/pkg/pr"
)

type taskFunc func(context.Context, string, string, string, bool) (json.RawMessage, error)

func (taskFunc) Name() string { return "test" }
func (f taskFunc) Task(ctx context.Context, dir, prompt, schema string, writable bool) (json.RawMessage, error) {
	return f(ctx, dir, prompt, schema, writable)
}

func loadCases(t *testing.T) []Case {
	t.Helper()
	cases, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	return cases
}

// This is only a harness self-test answer, not a substitute for the opt-in
// evaluation with a real model. It exercises fixture loading and scoring.
func passingAnalysis(c Case) pr.Analysis {
	a := pr.Analysis{Summary: "Harness self-test", Causes: []string{}, Steps: []string{}, Validation: []string{}, Remaining: []string{}, Recommendations: []pr.Recommendation{}}
	for _, e := range c.Expectations.Recommendations {
		r := pr.Recommendation{Subject: e.Subject, Action: e.Actions[0], Reason: "Frozen evidence", NextStep: "Inspect the observed failure"}
		for _, assertion := range c.Expectations.Assertions {
			if assertion.Subject == e.Subject {
				r.Reason += " " + assertion.AnyOf[0]
			}
		}
		a.Recommendations = append(a.Recommendations, r)
	}
	for _, assertion := range c.Expectations.Assertions {
		if assertion.Subject == "" {
			a.Remaining = append(a.Remaining, assertion.AnyOf[0])
		}
	}
	return a
}

func TestFrozenCasesReplayProductionDiagnosisWithoutLabelLeakage(t *testing.T) {
	cases := loadCases(t)
	if len(cases) != 4 {
		t.Fatalf("expected the four real-world cases, got %d", len(cases))
	}
	var dirs []string
	calls := 0
	agent := taskFunc(func(_ context.Context, dir, prompt, schema string, writable bool) (json.RawMessage, error) {
		c := cases[calls]
		calls++
		dirs = append(dirs, dir)
		if writable || !strings.Contains(prompt, "Only read the checkout") || !strings.Contains(schema, "recommendations") {
			t.Fatal("not using the production read-only diagnosis task")
		}
		for _, hidden := range []string{`"expectations"`, `"assertions"`, c.Provenance.SourceReport, c.Description} {
			if strings.Contains(prompt, hidden) {
				t.Fatalf("grading metadata leaked to prompt: %s", hidden)
			}
		}
		if err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.Name() == "case.json" || entry.Name() == "logs" || entry.Name() == ".git" {
				t.Fatalf("non-source benchmark data copied to agent checkout: %s", path)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(dir, ".github", "workflows")); err != nil && c.ID != "zoomflow" {
			t.Fatal("hidden workflow files were not embedded", err)
		}
		if c.ID == "muse" {
			data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
			if err != nil || !strings.Contains(string(data), "go 1.26.0") {
				t.Fatal("nested module fixture was excluded or not restored", err)
			}
		}
		data, err := json.Marshal(passingAnalysis(c))
		return data, err
	})
	report := Run(context.Background(), agent, cases, Options{Timeout: time.Second, Model: "self-test"})
	if !report.Passed() || report.Summary.CorrectActions != 7 || report.Summary.ExpectedActions != 7 || report.Summary.Errors != 0 {
		t.Fatalf("replay failed: %+v", report)
	}
	for _, dir := range dirs {
		if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("temporary source checkout was not removed: %s (%v)", dir, err)
		}
	}
	// A JSON round-trip must not discard producer IDs or change comparisons.
	asl := report.Results[0].Diagnosis
	if report.Results[0].ID != "agent-session-loop" || asl.BaseComparison == nil {
		t.Fatal("missing ASL comparison")
	}
	outcomes := map[string]string{}
	for _, row := range asl.BaseComparison.Checks {
		outcomes[row.Head.Name] = row.Outcome
		if row.Head.Name == "Linux (portable, Python 3.15)" && (row.Head.App == 0 || row.Head.Workflow == 0 || row.Head.Suite == 0 || row.Head.Event == "") {
			t.Fatal("producer identity was lost during fixture decoding")
		}
	}
	if outcomes["Linux (portable, Python 3.15)"] != "both-failed" || outcomes["Workbench browser (Playwright)"] != "both-failed" || outcomes["macOS (full 3/4)"] != "base-passed-pr-failed" {
		t.Fatal("frozen base comparisons changed", outcomes)
	}
	zoom := report.Results[3].Diagnosis
	// Check the concrete text evidence independently of grading output.
	found := false
	for _, run := range zoom.Artifacts {
		for _, artifact := range run.Artifacts {
			for _, file := range artifact.Files {
				found = found || (artifact.ID == 12805423400 && strings.HasSuffix(file.Path, "error-context.md") && file.Content != "")
			}
		}
	}
	if !found {
		t.Fatal("frozen Playwright evidence was lost")
	}
}

func TestGradeRejectsUnsafeOrIncompleteAnswers(t *testing.T) {
	want := Expectations{Recommendations: []ExpectedAction{
		{Subject: "billing", Actions: []pr.RecommendationAction{pr.HumanAction}},
		{Subject: "compiler", Actions: []pr.RecommendationAction{pr.CodeRepair}},
		{Subject: "browser", Actions: []pr.RecommendationAction{pr.RerunCandidate, pr.InsufficientEvidence}},
	}, Assertions: []Assertion{{ID: "compiler file", Subject: "compiler", AnyOf: []string{"src/api.go"}}}}
	for _, tc := range []struct {
		name    string
		recs    []pr.Recommendation
		correct int
		missed  int
		unsafe  int
		kinds   []string
	}{
		{"missing", nil, 0, 3, 0, []string{"missed_blocker", "missing_evidence_reference"}},
		{"billing edit", []pr.Recommendation{{Subject: "billing", Action: pr.CodeRepair}}, 0, 2, 1, []string{"unsupported_code_repair", "incorrect_action"}},
		{"unrecognized edit", []pr.Recommendation{{Subject: "passed job", Action: pr.CodeRepair}}, 0, 3, 1, []string{"unexpected_recommendation", "unsupported_code_repair"}},
		{"duplicate alternatives", []pr.Recommendation{{Subject: "browser", Action: pr.RerunCandidate}, {Subject: "browser", Action: pr.CodeRepair}}, 0, 2, 1, []string{"duplicate_subject", "unsupported_code_repair"}},
		{"accepted uncertainty", []pr.Recommendation{{Subject: "browser", Action: pr.InsufficientEvidence}}, 1, 2, 0, []string{"missed_blocker"}},
		{"case insensitive reference", []pr.Recommendation{{Subject: "compiler", Action: pr.CodeRepair, Reason: "Update SRC/API.GO"}}, 1, 2, 0, []string{"missed_blocker"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			score, findings := Grade(want, pr.Analysis{Recommendations: tc.recs})
			if score.CorrectActions != tc.correct || score.MissedBlockers != tc.missed || score.UnsupportedCodeRepairs != tc.unsafe {
				t.Fatalf("score %+v findings %+v", score, findings)
			}
			for _, kind := range tc.kinds {
				if !slices.ContainsFunc(findings, func(f Finding) bool { return f.Kind == kind }) {
					t.Fatalf("missing finding %s: %+v", kind, findings)
				}
			}
		})
	}
	// A filename elsewhere in the report cannot satisfy an assertion scoped
	// to a particular recommendation's evidence.
	score, _ := Grade(want, pr.Analysis{Summary: "src/api.go", Recommendations: []pr.Recommendation{{Subject: "compiler", Action: pr.CodeRepair, Reason: "Update something"}}})
	if score.FailedEvidenceChecks != 1 {
		t.Fatal("unrelated prose satisfied a scoped evidence assertion")
	}
}

func TestMissingRawRecommendationCannotEarnFallbackCredit(t *testing.T) {
	c := loadCases(t)[1] // lyzma
	c.Expectations = Expectations{Recommendations: []ExpectedAction{{Subject: "check", Actions: []pr.RecommendationAction{pr.InsufficientEvidence}}}}
	agent := taskFunc(func(context.Context, string, string, string, bool) (json.RawMessage, error) {
		return json.RawMessage(`{"summary":"Cannot diagnose","causes":[],"steps":[],"validation":[],"remaining":[],"recommendations":[]}`), nil
	})
	r := Run(context.Background(), agent, []Case{c}, Options{Timeout: time.Second})
	if r.Passed() || r.Summary.MissedBlockers != 1 || r.Summary.CorrectActions != 0 || len(r.Results[0].Diagnosis.Analysis.Recommendations) != 1 || len(r.Results[0].AgentAnalysis.Recommendations) != 0 {
		t.Fatalf("service fallback hid an omitted recommendation: %+v", r)
	}
}

func TestErrorsTimeoutAndCancellationPreserveResults(t *testing.T) {
	cases := loadCases(t)
	for _, kind := range []string{"backend error", "invalid schema", "timeout", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			agent := taskFunc(func(ctx context.Context, _, _, _ string, _ bool) (json.RawMessage, error) {
				calls++
				if calls == 1 {
					switch kind {
					case "backend error":
						return nil, errors.New("backend unavailable")
					case "invalid schema":
						return json.RawMessage(`{"summary":"incomplete"}`), nil
					case "timeout":
						<-ctx.Done()
						return nil, ctx.Err()
					case "cancelled":
						cancel()
						return nil, ctx.Err()
					}
				}
				return json.Marshal(passingAnalysis(cases[calls-1]))
			})
			r := Run(ctx, agent, cases[:2], Options{Timeout: 100 * time.Millisecond})
			if r.Passed() || r.Summary.Errors != 1 || r.Results[0].Error == "" {
				t.Fatalf("lost case error: %+v", r)
			}
			if kind == "cancelled" {
				if !r.Cancelled || calls != 1 || len(r.Results) != 1 {
					t.Fatal("cancellation did not stop the remaining case", r)
				}
			} else if r.Cancelled || calls != 2 || r.Summary.PassedCases != 1 {
				t.Fatal("individual case error prevented the next case", r)
			}
		})
	}
}
