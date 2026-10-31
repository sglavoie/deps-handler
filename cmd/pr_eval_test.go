package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/sglavoie/deps-handler/internal/diagnosiseval"
	"github.com/sglavoie/deps-handler/pkg/config"
	"github.com/sglavoie/deps-handler/pkg/github"
	"github.com/sglavoie/deps-handler/pkg/review"
)

type evalTaskAgent struct {
	answer string
	err    error
}

func (evalTaskAgent) Name() string { return "test" }
func (a evalTaskAgent) Task(context.Context, string, string, string, bool) (json.RawMessage, error) {
	return json.RawMessage(a.answer), a.err
}

const evalBillingAnswer = `{"summary":"Billing prevented the runner from starting","causes":["billing failure"],"steps":["Resolve billing"],"validation":[],"remaining":[],"recommendations":[{"subject":"check","action":"human_action","reason":"Account billing prevented the job from starting","next_step":"Fix billing and rerun"}]}`

func mockEvalCommand(t *testing.T, agent evalTaskAgent) {
	t.Helper()
	mockPRCommands(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	newPRClient = func() (github.PRClient, error) {
		t.Fatal("evaluation reached the live GitHub client factory")
		return nil, errors.New("unexpected GitHub access")
	}
	newTaskAgent = func(config.Agent, string) (review.TaskAgent, error) { return agent, nil }
}

func TestPREvalListNeedsNoConfigAgentOrGitHub(t *testing.T) {
	mockEvalCommand(t, evalTaskAgent{})
	newTaskAgent = func(config.Agent, string) (review.TaskAgent, error) {
		t.Fatal("listing cases created an agent")
		return nil, errors.New("unexpected agent")
	}
	for _, asJSON := range []bool{false, true} {
		args := []string{"pr", "eval", "--list", "--config", "/nonexistent/eval-config.yaml"}
		if asJSON {
			args = append(args, "--json")
		}
		code, out, stderr := run(args...)
		if code != 0 || stderr != "" {
			t.Fatalf("code %d output %s stderr %s", code, out, stderr)
		}
		if asJSON {
			var entries []struct{ ID, Description string }
			if err := json.Unmarshal([]byte(out), &entries); err != nil || len(entries) != 4 || entries[0].ID != "agent-session-loop" {
				t.Fatalf("bad listing: %s (%v)", out, err)
			}
		} else if !strings.Contains(out, "│ lyzma") || !strings.Contains(out, "│ zoomflow") {
			t.Fatal("missing case descriptions", out)
		}
	}
}

func TestPREvalExitCodesAndStructuredResults(t *testing.T) {
	for _, tc := range []struct {
		name  string
		agent evalTaskAgent
		code  int
	}{
		{"pass", evalTaskAgent{answer: evalBillingAnswer}, 0},
		{"unsafe action", evalTaskAgent{answer: strings.Replace(evalBillingAnswer, "human_action", "code_repair", 1)}, 1},
		{"invalid answer", evalTaskAgent{answer: `{"summary":"partial"}`}, 1},
		{"backend failure", evalTaskAgent{err: errors.New("agent unavailable")}, 1},
	} {
		for _, asJSON := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{true: " JSON"}[asJSON], func(t *testing.T) {
				mockEvalCommand(t, tc.agent)
				args := []string{"pr", "eval", "lyzma", "--agent", "claude", "--timeout", "1s"}
				if asJSON {
					args = append(args, "--json")
				}
				code, out, stderr := run(args...)
				if code != tc.code || !strings.Contains(stderr, "Evaluating lyzma") {
					t.Fatalf("code %d out %s stderr %s", code, out, stderr)
				}
				if asJSON {
					var report diagnosiseval.Report
					if err := json.Unmarshal([]byte(out), &report); err != nil || len(report.Results) != 1 || report.Selected[0] != "lyzma" || report.Passed() != (tc.code == 0) {
						t.Fatalf("bad report: %s (%v)", out, err)
					}
					if tc.name == "unsafe action" && report.Summary.UnsupportedCodeRepairs != 1 {
						t.Fatal("lost unsafe repair score", report.Summary)
					}
				} else if !strings.Contains(out, "cases passed") || !strings.Contains(out, "actions correct") {
					t.Fatal("missing readable scores", out)
				}
			})
		}
	}
}

func TestPREvalInvalidArgumentsDoNotStartAnAgent(t *testing.T) {
	for _, args := range [][]string{
		{"missing"}, {"lyzma", "lyzma"}, {"--agent", ""}, {"--agent", "other"}, {"--timeout", "0s"}, {"--timeout", "-1s"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			mockEvalCommand(t, evalTaskAgent{})
			newTaskAgent = func(config.Agent, string) (review.TaskAgent, error) {
				t.Fatal("invalid arguments created an agent")
				return nil, errors.New("unexpected agent")
			}
			code, _, stderr := run(append([]string{"pr", "eval"}, args...)...)
			if code != 2 {
				t.Fatalf("code %d stderr %s", code, stderr)
			}
		})
	}
}

func TestPREvalUnavailableInDemo(t *testing.T) {
	mockEvalCommand(t, evalTaskAgent{})
	prCommandsAvailable = false
	code, _, stderr := run("pr", "eval", "lyzma")
	if code != 1 || !strings.Contains(stderr, "unavailable in the demo build") {
		t.Fatalf("code %d stderr %s", code, stderr)
	}
}
