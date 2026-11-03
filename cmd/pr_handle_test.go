package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/sglavoie/deps-handler/pkg/config"
	"github.com/sglavoie/deps-handler/pkg/github"
	"github.com/sglavoie/deps-handler/pkg/pr"
	"github.com/sglavoie/deps-handler/pkg/review"
)

type commandHandleClient struct {
	github.HandleClient // Unexpected operations fail the test instead of hitting GitHub.
	targets             []github.PRTarget
	reads, merges       []int
	listed              int
	listErr             error
	base                string
}

func (c *commandHandleClient) OpenPRs(_ context.Context, owner, name string) ([]github.PRTarget, error) {
	if owner != "o" || name != "r" {
		return nil, errors.New("unexpected repository")
	}
	c.listed++
	return c.targets, c.listErr
}
func (c *commandHandleClient) HandleState(_ context.Context, target github.PRTarget) (github.HandleState, error) {
	c.reads = append(c.reads, target.Number)
	author := "contributor"
	if target.Number == 7 {
		author = "dependabot"
	}
	p := github.PRSnapshot{Target: target, Title: "Fix edge case", State: "OPEN", HeadOID: fmt.Sprintf("head%d", target.Number), HeadRepo: target.Owner + "/" + target.Repo, HeadRef: "fix", BaseOID: c.base, BaseRef: "main", Permission: "WRITE", Mergeable: "MERGEABLE", MergeState: "CLEAN", ChecksComplete: true, Checks: []github.PRCheck{{Check: github.Check{Name: "test", State: github.CheckPassed}}}}
	if target.Number == 8 {
		p.ReviewDecision = "REVIEW_REQUIRED"
	}
	return github.HandleState{Snapshot: p, Author: author, UpdatedAt: "now", Body: "Intent", DiscussionsComplete: true, MergeMethods: []string{"squash"}}, nil
}
func (*commandHandleClient) Evidence(context.Context, github.PRSnapshot) (github.PREvidence, error) {
	return github.PREvidence{Details: &github.PullRequestDetails{Body: "Intent", Diff: "-old\n+new"}}, nil
}
func (c *commandHandleClient) Merge(_ context.Context, _, _ string, number int, method, head string) error {
	if head != fmt.Sprintf("head%d", number) || method != "squash" {
		return errors.New("unexpected merge")
	}
	c.merges = append(c.merges, number)
	c.base = fmt.Sprintf("after%d", number)
	return nil
}

type commandHandleAgent struct {
	t       *testing.T
	prompts []string
}

func (*commandHandleAgent) Name() string { return "fake" }
func (a *commandHandleAgent) Task(_ context.Context, _, prompt, _ string, writable bool) (json.RawMessage, error) {
	if writable {
		a.t.Fatal("unexpected repair")
	}
	a.prompts = append(a.prompts, prompt)
	return json.RawMessage(`{"decision":"approve","analysis":{"summary":"Code reviewed","causes":[],"steps":[],"validation":[],"remaining":[],"recommendations":[]}}`), nil
}

func mockHandleCommand(t *testing.T) (*commandHandleClient, *commandHandleAgent) {
	t.Helper()
	mockPRCommands(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	old := newPRHandleClient
	t.Cleanup(func() { newPRHandleClient = old })
	c := &commandHandleClient{base: "initial-base", targets: []github.PRTarget{{Owner: "o", Repo: "r", Number: 7}, {Owner: "o", Repo: "r", Number: 8}, {Owner: "o", Repo: "r", Number: 9}, {Owner: "o", Repo: "r", Number: 10}}}
	a := &commandHandleAgent{t: t}
	newPRHandleClient = func() (github.HandleClient, error) { return c, nil }
	newTaskAgent = func(config.Agent, string) (review.TaskAgent, error) { return a, nil }
	return c, a
}

func TestHandleCommandSelectionPreviewAndSerialApply(t *testing.T) {
	for _, apply := range []bool{false, true} {
		t.Run(fmt.Sprint(apply), func(t *testing.T) {
			c, a := mockHandleCommand(t)
			args := []string{"pr", "handle", "--repo", "o/r", "--agent", "claude", "--json"}
			if apply {
				args = append(args, "--apply")
			}
			code, out, stderr := run(args...)
			var results []pr.HandleResult
			if err := json.Unmarshal([]byte(out), &results); err != nil {
				t.Fatal(out, err)
			}
			wantCode := 0
			if apply {
				wantCode = 1
			} // #8 requires a human approval.
			if code != wantCode || len(results) != 4 || results[0].Status != "skipped" || results[1].Status != "blocked" || len(a.prompts) != 3 || c.listed != 1 {
				t.Fatalf("code=%d out=%s stderr=%s", code, out, stderr)
			}
			if apply {
				if fmt.Sprint(c.merges) != "[9 10]" || results[2].Status != "merged" || results[3].Status != "merged" {
					t.Fatal(out, c.merges)
				}
				if !strings.Contains(a.prompts[2], `"base_oid":"after9"`) {
					t.Fatal("next PR did not use fresh base", a.prompts[2])
				}
			} else if len(c.merges) != 0 || results[2].Status != "would_merge" {
				t.Fatal(out, c.merges)
			}
			for _, result := range results {
				if result.Workspace != "" {
					if _, err := os.Stat(result.Workspace); err != nil {
						t.Fatal("blocked apply checkout was lost", err)
					}
					t.Cleanup(func() { _ = os.RemoveAll(result.Workspace) })
				}
			}
		})
	}
}

func TestHandleCommandExplicitTargetsDoNotDiscoverRepositories(t *testing.T) {
	c, _ := mockHandleCommand(t)
	code, out, stderr := run("pr", "handle", "o/r#10", "https://github.com/o/r/pull/9", "--apply", "--list")
	if code != 0 || fmt.Sprint(c.merges) != "[10 9]" || c.listed != 0 || !strings.Contains(out, "o/r#10") || !strings.Contains(out, "merged") {
		t.Fatal(code, out, stderr)
	}
	for _, number := range c.reads {
		if number != 9 && number != 10 {
			t.Fatal("unselected PR was processed", c.reads)
		}
	}
}

func TestHandleCommandValidatesBeforeCallingClientsOrAgents(t *testing.T) {
	for _, args := range [][]string{
		{}, {"bad"}, {"--repo", ""}, {"--repo", "r"}, {"o/r#9", "--repo", "o/r"},
		{"o/r#9", "O/R#9"}, {"o/r#9", "--max-attempts", "0"}, {"o/r#9", "--wait-timeout", "0s"},
		{"o/r#9", "--agent", ""}, {"o/r#9", "--agent", "unknown"}, {"o/r#9", "--apply", "--agent", "api"},
		{"o/r#9", "--merge-method", ""}, {"o/r#9", "--merge-method", "bad"}, {"o/r#9", "--json", "--list"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			c, a := mockHandleCommand(t)
			code, out, stderr := run(append([]string{"pr", "handle"}, args...)...)
			if code != 2 || len(c.reads) != 0 || c.listed != 0 || len(a.prompts) != 0 {
				t.Fatal(code, out, stderr, c.reads)
			}
		})
	}
}

func TestHandleCommandEmptyDiscoveryAndFailureDoNotProcessPRs(t *testing.T) {
	for _, fail := range []bool{false, true} {
		c, a := mockHandleCommand(t)
		c.targets = []github.PRTarget{}
		want := 0
		if fail {
			c.listErr, want = errors.New("incomplete listing"), 1
		}
		code, out, stderr := run("pr", "handle", "--repo", "o/r", "--json")
		if code != want || len(a.prompts) != 0 || len(c.reads) != 0 || len(c.merges) != 0 {
			t.Fatal(code, out, stderr)
		}
		if !fail && strings.TrimSpace(out) != "[]" {
			t.Fatal(out)
		}
	}
}

func TestHandleCommandDemoCannotReachGitHub(t *testing.T) {
	c, _ := mockHandleCommand(t)
	prCommandsAvailable = false
	code, _, stderr := run("pr", "handle", "--repo", "o/r", "--apply")
	if code != 1 || !strings.Contains(stderr, "unavailable in the demo") || c.listed != 0 || len(c.reads) != 0 {
		t.Fatal(code, stderr)
	}
}

func TestHandleCommandUsesConfiguredAgentAndMethod(t *testing.T) {
	c, a := mockHandleCommand(t)
	newTaskAgent = func(cfg config.Agent, name string) (review.TaskAgent, error) {
		if name != "codex" || cfg.Codex.Model != "test-model" {
			t.Fatalf("agent=%s cfg=%+v", name, cfg)
		}
		return a, nil
	}
	path := writeConfig(t, "agent:\n  use: codex\n  codex:\n    model: test-model\n")
	code, out, stderr := run("pr", "handle", "o/r#9", "--config", path, "--merge-method", "squash", "--apply")
	if code != 0 || len(c.merges) != 1 {
		t.Fatal(code, out, stderr)
	}
}

func TestHandleCommandExcludedPRNeedsNoAgent(t *testing.T) {
	c, _ := mockHandleCommand(t)
	newTaskAgent = func(config.Agent, string) (review.TaskAgent, error) {
		t.Fatal("excluded PR started agent setup")
		return nil, errors.New("no agent installed")
	}
	code, out, stderr := run("pr", "handle", "o/r#7", "--apply", "--json")
	if code != 0 || !strings.Contains(out, "Dependabot PRs are always excluded") || len(c.merges) != 0 {
		t.Fatal(code, out, stderr)
	}
}
