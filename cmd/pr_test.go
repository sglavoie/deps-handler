package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sglavoie/deps-handler/pkg/config"
	"github.com/sglavoie/deps-handler/pkg/github"
	"github.com/sglavoie/deps-handler/pkg/pr"
	"github.com/sglavoie/deps-handler/pkg/review"
)

type commandPRClient struct{ snapshot github.PRSnapshot }

func (c commandPRClient) Snapshot(context.Context, github.PRTarget) (github.PRSnapshot, error) {
	return c.snapshot, nil
}
func (commandPRClient) Evidence(context.Context, github.PRSnapshot) (*github.PullRequestDetails, []github.PRCheck, error) {
	return &github.PullRequestDetails{Diff: "diff"}, nil, nil
}

type commandTaskAgent struct{ err error }

func (commandTaskAgent) Name() string { return "fake" }
func (a commandTaskAgent) Task(context.Context, string, string, string, bool) (json.RawMessage, error) {
	return json.RawMessage(`{"summary":"Repair plan","causes":["old API"],"steps":["edit src/app.go"],"validation":["run tests"],"remaining":[]}`), a.err
}

type commandWorkspace struct{ root string }

func (w commandWorkspace) Checkout(context.Context, string, string, int, string) (string, error) {
	return w.root, nil
}
func (commandWorkspace) FetchBase(context.Context, string, string) error { return nil }
func (commandWorkspace) Commit(context.Context, string, string) (string, error) {
	return "local-head", nil
}
func (commandWorkspace) DiffSummary(context.Context, string, string) (string, error) {
	return " src/app.go | 2 +-", nil
}
func (commandWorkspace) CommitAndPush(context.Context, string, string, string, string, string) (string, error) {
	return "", errors.New("unexpected push")
}

func mockPRCommands(t *testing.T) {
	t.Helper()
	oldClient, oldAgent, oldWorkspace, oldLocalWorkspace, oldAvailable := newPRClient, newTaskAgent, newPRWorkspace, newLocalPRWorkspace, prCommandsAvailable
	t.Cleanup(func() {
		newPRClient, newTaskAgent, newPRWorkspace, newLocalPRWorkspace, prCommandsAvailable = oldClient, oldAgent, oldWorkspace, oldLocalWorkspace, oldAvailable
	})
	prCommandsAvailable = true
	newPRClient = func() (github.PRClient, error) {
		return commandPRClient{github.PRSnapshot{Target: github.PRTarget{Owner: "o", Repo: "r", Number: 7}, State: "OPEN", HeadOID: "head", HeadRepo: "o/r", HeadRef: "update", BaseOID: "base", BaseRef: "main", Permission: "WRITE", Mergeable: "MERGEABLE", MergeState: "CLEAN", ChecksComplete: true, Checks: []github.PRCheck{{Check: github.Check{Name: "test", State: github.CheckFailed}}}}}, nil
	}
	newTaskAgent = func(config.Agent, string) (review.TaskAgent, error) { return commandTaskAgent{}, nil }
	newPRWorkspace = func(root string) pr.Workspace {
		if err := os.MkdirAll(root, 0700); err != nil {
			t.Fatal(err)
		}
		return commandWorkspace{root: root}
	}
}

func TestPRDiagnosisUsesExplicitTargetWithoutConfig(t *testing.T) {
	mockPRCommands(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	code, out, stderr := run("pr", "diagnose", "o/r#7", "--focus", "compile", "--json")
	var result pr.Result
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(out, err)
	}
	if code != 0 || result.Analysis.Summary != "Repair plan" || result.Snapshot.Target.Number != 7 || result.ChecksPassing || result.Workspace != "" || result.RepairCommand != "deps-handler pr fix o/r#7 --plan repair.md --agent codex" {
		t.Fatalf("code %d out %s stderr %s", code, out, stderr)
	}
}
func TestPRConfigCanHaveOnlyAgentSettings(t *testing.T) {
	mockPRCommands(t)
	cfg := writeConfig(t, "agent:\n  use: codex\n  codex:\n    model: model-for-test\n")
	newTaskAgent = func(cfg config.Agent, name string) (review.TaskAgent, error) {
		if name != "codex" || cfg.Codex.Model != "model-for-test" {
			t.Fatalf("name %s cfg %+v", name, cfg)
		}
		return commandTaskAgent{}, nil
	}
	code, out, stderr := run("pr", "diagnose", "o/r#7", "-c", cfg)
	if code != 0 || !strings.Contains(out, "Repair plan") {
		t.Fatalf("code %d out %s stderr %s", code, out, stderr)
	}
}
func TestPRUsageErrorsBeforeSetup(t *testing.T) {
	for _, args := range [][]string{
		{"pr", "diagnose"}, {"pr", "diagnose", "7"}, {"pr", "diagnose", "o/r#7", "--agent", ""},
		{"pr", "fix", "o/r#7", "--max-attempts", "0"}, {"pr", "fix", "o/r#7", "--wait-timeout", "0"},
		{"pr", "fix", "o/r#7", "--agent", "unknown"}, {"pr", "diagnose", "o/r#7", "--config", ""},
	} {
		code, _, stderr := run(args...)
		if code != 2 {
			t.Fatalf("%v: code %d stderr %s", args, code, stderr)
		}
	}
}
func TestPRFixReportsFailureAndKeepsWorkspace(t *testing.T) {
	mockPRCommands(t)
	cfg := writeConfig(t, "agent:\n  use: codex\n")
	newTaskAgent = func(config.Agent, string) (review.TaskAgent, error) {
		return commandTaskAgent{err: errors.New("agent timeout")}, nil
	}
	code, out, stderr := run("pr", "fix", "o/r#7", "-c", cfg, "--json")
	var result pr.Result
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(out, err)
	}
	if code != 1 || result.Error != "agent timeout" || result.Workspace == "" || result.RepairCommand != "" || !strings.Contains(stderr, "checkout kept at") {
		t.Fatalf("code %d out %s stderr %s", code, out, stderr)
	}
	if _, err := os.Stat(result.Workspace); err != nil {
		t.Fatal("checkout lost", err)
	}
	// The production command keeps this directory for recovery; clean the test's.
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(result.Workspace)) })
}
func TestPRExplicitMissingConfigFails(t *testing.T) {
	mockPRCommands(t)
	code, _, stderr := run("pr", "diagnose", "o/r#7", "-c", filepath.Join(t.TempDir(), "absent"))
	if code != 1 || !strings.Contains(stderr, "no such file") {
		t.Fatalf("code %d stderr %s", code, stderr)
	}
}
func TestPRFixRejectsAPIAgent(t *testing.T) {
	mockPRCommands(t)
	cfg := writeConfig(t, "agent:\n  use: api\n  api:\n    url: http://localhost:8081/v1\n    model: local\n")
	code, _, stderr := run("pr", "fix", "o/r#7", "-c", cfg)
	if code != 2 || !strings.Contains(stderr, "api supports pr diagnose only") {
		t.Fatalf("code %d stderr %s", code, stderr)
	}
}
func TestPRDemoCannotReachGitHub(t *testing.T) {
	mockPRCommands(t)
	prCommandsAvailable = false
	cfg := writeConfig(t, "agent: {}\n")
	newPRClient = func() (github.PRClient, error) { t.Fatal("demo reached GitHub"); return nil, nil }
	code, _, stderr := run("pr", "diagnose", "o/r#7", "-c", cfg)
	if code != 1 || !strings.Contains(stderr, "unavailable in the demo build") {
		t.Fatalf("code %d stderr %s", code, stderr)
	}
}

type planCommandAgent struct {
	t     *testing.T
	want  string
	calls *int
}

func (planCommandAgent) Name() string { return "fake" }
func (a planCommandAgent) Task(_ context.Context, _, prompt, _ string, writable bool) (json.RawMessage, error) {
	*a.calls++
	if !writable || !strings.Contains(prompt, a.want) || !strings.Contains(prompt, "Do not start a new diagnosis") {
		a.t.Errorf("saved plan missing from writable task: %s", prompt)
	}
	return nil, errors.New("stop after checking the saved plan")
}

func TestPRFixAcceptsSavedDiagnosisAndInfersTarget(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "inferred", true: "explicit"}[explicit], func(t *testing.T) {
			mockPRCommands(t)
			cfg := writeConfig(t, "agent:\n  use: codex\n")
			path := filepath.Join(t.TempDir(), "repair.md")
			plan := "# o/r#7: repair\nPR: https://github.com/o/r/pull/7\nHead: `head`\nBase: `main` (`base`)\n\n## Repair steps\nUse my saved fix for src/app.go.\n"
			if err := os.WriteFile(path, []byte(plan), 0600); err != nil {
				t.Fatal(err)
			}
			calls := 0
			newTaskAgent = func(config.Agent, string) (review.TaskAgent, error) {
				return planCommandAgent{t: t, want: "Use my saved fix for src/app.go.", calls: &calls}, nil
			}
			baseClient := newPRClient
			newPRClient = func() (github.PRClient, error) {
				c, err := baseClient()
				if err != nil {
					return nil, err
				}
				return targetCheckingPRClient{t: t, PRClient: c}, nil
			}
			args := []string{"pr", "fix", "--plan", path, "--agent", "codex", "-c", cfg, "--json"}
			if explicit {
				args = append(args, "o/r#7")
			}
			code, out, stderr := run(args...)
			var result pr.Result
			if err := json.Unmarshal([]byte(out), &result); err != nil {
				t.Fatal(out, err)
			}
			if code != 1 || calls != 1 || result.PlanSource != path || result.Workspace == "" || !strings.Contains(stderr, "Using saved repair plan") {
				t.Fatalf("code %d calls %d out %s stderr %s", code, calls, out, stderr)
			}
			t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(result.Workspace)) })
			data, err := os.ReadFile(path)
			if err != nil || string(data) != plan {
				t.Fatal("plan changed", err)
			}
		})
	}
}

type targetCheckingPRClient struct {
	t *testing.T
	github.PRClient
}

func (c targetCheckingPRClient) Snapshot(ctx context.Context, target github.PRTarget) (github.PRSnapshot, error) {
	if target != (github.PRTarget{Owner: "o", Repo: "r", Number: 7}) {
		c.t.Fatalf("wrong target: %+v", target)
	}
	return c.PRClient.Snapshot(ctx, target)
}

func TestPRFixPlanInputErrorsBeforeAgentSetup(t *testing.T) {
	for _, kind := range []string{"empty flag", "missing file", "empty file", "mismatched PR", "no target", "no target or plan"} {
		t.Run(kind, func(t *testing.T) {
			mockPRCommands(t)
			newTaskAgent = func(config.Agent, string) (review.TaskAgent, error) {
				t.Fatal("agent setup ran for invalid plan input")
				return nil, nil
			}
			newPRClient = func() (github.PRClient, error) { t.Fatal("GitHub setup ran for invalid plan input"); return nil, nil }
			path := filepath.Join(t.TempDir(), "repair.md")
			text := "PR: https://github.com/o/r/pull/7\n\n## Repair steps\nFix src/app.go.\n"
			args := []string{"pr", "fix", "--plan", path}
			wantCode := 2
			switch kind {
			case "empty flag":
				args = []string{"pr", "fix", "--plan", ""}
			case "missing file":
				wantCode = 1
			case "empty file":
				text = ""
				wantCode = 1
			case "mismatched PR":
				args = append(args, "o/other#7")
			case "no target":
				text = "# My plan\n\nUpdate src/app.go.\n"
			case "no target or plan":
				args = []string{"pr", "fix"}
			}
			if kind != "missing file" {
				if err := os.WriteFile(path, []byte(text), 0600); err != nil {
					t.Fatal(err)
				}
			}
			code, _, stderr := run(args...)
			if code != wantCode {
				t.Fatalf("code %d want %d stderr %s", code, wantCode, stderr)
			}
		})
	}
}

type localDirectoryAgent struct {
	t         *testing.T
	directory string
}

func (localDirectoryAgent) Name() string { return "fake" }
func (a localDirectoryAgent) Task(_ context.Context, dir, _, _ string, writable bool) (json.RawMessage, error) {
	if dir != a.directory || !writable {
		a.t.Fatalf("agent directory=%s writable=%v", dir, writable)
	}
	return nil, errors.New("stop after checking working directory")
}

func TestPRFixUsesAndPreservesExplicitWorkingDirectory(t *testing.T) {
	for _, passing := range []bool{false, true} {
		t.Run(map[bool]string{false: "repair failure", true: "already passing"}[passing], func(t *testing.T) {
			mockPRCommands(t)
			cfg := writeConfig(t, "agent: {}\n")
			dir := t.TempDir()
			marker := filepath.Join(dir, "cached-dependency")
			if err := os.WriteFile(marker, []byte("keep me"), 0600); err != nil {
				t.Fatal(err)
			}
			released := false
			newLocalPRWorkspace = func(_ context.Context, path string) (pr.Workspace, func(), error) {
				if path != dir {
					t.Fatal("wrong selected directory", path)
				}
				return commandWorkspace{root: dir}, func() { released = true }, nil
			}
			newPRWorkspace = func(string) pr.Workspace {
				t.Fatal("made temporary checkout for explicit working directory")
				return nil
			}
			newTaskAgent = func(config.Agent, string) (review.TaskAgent, error) {
				return localDirectoryAgent{t: t, directory: dir}, nil
			}
			if passing {
				oldFactory := newPRClient
				newPRClient = func() (github.PRClient, error) {
					client, err := oldFactory()
					if err != nil {
						return nil, err
					}
					c := client.(commandPRClient)
					c.snapshot.Checks[0].State = github.CheckPassed
					return c, nil
				}
			}
			code, out, stderr := run("pr", "fix", "o/r#7", "--working-directory", dir, "-c", cfg, "--json")
			var result pr.Result
			if err := json.Unmarshal([]byte(out), &result); err != nil {
				t.Fatal(out, err)
			}
			expected := 1
			if passing {
				expected = 0
			}
			if code != expected || !released {
				t.Fatalf("code=%d released=%v out=%s stderr=%s", code, released, out, stderr)
			}
			if !passing && result.Workspace != dir {
				t.Fatalf("missing retained path: %+v", result)
			}
			data, err := os.ReadFile(marker)
			if err != nil || string(data) != "keep me" {
				t.Fatal("directory was cleaned", err)
			}
		})
	}
}

func TestPRFixRejectsEmptyWorkingDirectoryFlag(t *testing.T) {
	for _, path := range []string{"", " "} {
		code, _, stderr := run("pr", "fix", "o/r#7", "--working-directory", path)
		if code != 2 || !strings.Contains(stderr, "--working-directory needs a directory path") {
			t.Fatalf("code=%d stderr=%s", code, stderr)
		}
	}
}

func TestPRFixDefaultsToRetainedLocalRepair(t *testing.T) {
	for _, format := range []string{"json", "markdown", "explicit false"} {
		t.Run(format, func(t *testing.T) {
			mockPRCommands(t)
			cfg := writeConfig(t, "agent:\n  use: codex\n")
			var checkout string
			newPRWorkspace = func(root string) pr.Workspace {
				checkout = root
				if err := os.MkdirAll(root, 0700); err != nil {
					t.Fatal(err)
				}
				return commandWorkspace{root: root} // Any push fails this test.
			}
			args := []string{"pr", "fix", "o/r#7", "-c", cfg}
			if format != "markdown" {
				args = append(args, "--json")
			}
			if format == "explicit false" {
				args = append(args, "--push=false")
			}
			code, out, stderr := run(args...)
			if checkout != "" {
				t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(checkout)) })
			}
			if code != 0 || checkout == "" {
				t.Fatalf("code=%d out=%s stderr=%s", code, out, stderr)
			}
			if _, err := os.Stat(checkout); err != nil {
				t.Fatal("successful local repair was deleted", err)
			}
			if format == "markdown" {
				for _, want := range []string{"prepared locally", "## Local commits", "src/app.go", "## Publish after review", "--push --working-directory", checkout} {
					if !strings.Contains(out, want) {
						t.Fatalf("missing %q in %s", want, out)
					}
				}
				return
			}
			var result pr.Result
			if err := json.Unmarshal([]byte(out), &result); err != nil {
				t.Fatal(err, out)
			}
			wantCommand := "deps-handler pr fix o/r#7 --push --working-directory " + shellQuote(checkout) + " --agent codex --config " + shellQuote(cfg)
			if !result.Prepared || result.ChecksPassing || result.Workspace != checkout || len(result.LocalCommits) != 1 || len(result.Commits) != 0 || result.PublishCommand != wantCommand {
				t.Fatalf("unexpected local result: %+v", result)
			}
		})
	}
}

type publishingCommandWorkspace struct {
	commandWorkspace
	pushes *int
}

func (w publishingCommandWorkspace) CommitAndPush(context.Context, string, string, string, string, string) (string, error) {
	*w.pushes++
	return "", errors.New("test publication reached")
}

func TestPRFixPushFlagEnablesPublication(t *testing.T) {
	mockPRCommands(t)
	cfg := writeConfig(t, "agent:\n  use: codex\n")
	pushes := 0
	newPRWorkspace = func(root string) pr.Workspace {
		if err := os.MkdirAll(root, 0700); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(root)) })
		return publishingCommandWorkspace{commandWorkspace{root: root}, &pushes}
	}
	code, out, stderr := run("pr", "fix", "o/r#7", "--push", "-c", cfg, "--json")
	var result pr.Result
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err, out)
	}
	if code != 1 || pushes != 1 || result.Prepared || len(result.LocalCommits) != 0 || result.Error != "test publication reached" || result.Workspace == "" {
		t.Fatalf("code=%d pushes=%d out=%s stderr=%s", code, pushes, out, stderr)
	}
}
