package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/sglavoie/deps-handler/pkg/config"
	"github.com/sglavoie/deps-handler/pkg/github"
	"github.com/sglavoie/deps-handler/pkg/pr"
	"github.com/sglavoie/deps-handler/pkg/review"
)

type commandActionsClient struct {
	requested bool
	jobID     int64
	postErr   error
}

func (c *commandActionsClient) Snapshot(context.Context, github.PRTarget) (github.PRSnapshot, error) {
	state := github.CheckFailed
	if c.requested {
		state = github.CheckPassed
	}
	return github.PRSnapshot{Target: github.PRTarget{Owner: "o", Repo: "r", Number: 7}, State: "OPEN", HeadOID: "abc123", HeadRef: "update", HeadRepo: "o/r", BaseRef: "main", BaseOID: "def456", Permission: "WRITE", ChecksComplete: true, Mergeable: "MERGEABLE", MergeState: "CLEAN", Checks: []github.PRCheck{{
		Check: github.Check{Name: "test", State: state, Suite: 4, Workflow: 5, Event: "pull_request"}, ID: 42, URL: "https://github.com/o/r/actions/runs/20/job/99",
	}}}, nil
}
func (*commandActionsClient) ActionsJob(context.Context, github.PRTarget, int64) (github.ActionsJob, error) {
	return github.ActionsJob{ID: 99, RunID: 20, RunAttempt: 1, HeadSHA: "abc123", Name: "test", Status: "completed", Conclusion: "failure", CheckURL: "https://api.github.com/repos/o/r/check-runs/42"}, nil
}
func (c *commandActionsClient) ActionsRun(context.Context, github.PRTarget, int64) (github.ActionsRun, error) {
	r := github.ActionsRun{ID: 20, Attempt: 1, HeadSHA: "abc123", SuiteID: 4, WorkflowID: 5, Event: "pull_request", Status: "completed", Conclusion: "failure"}
	r.Repository.FullName = "o/r"
	if c.requested {
		r.Attempt, r.Conclusion = 2, "success"
	}
	return r, nil
}
func (c *commandActionsClient) RerunJob(_ context.Context, _ github.PRTarget, id int64) error {
	c.jobID = id
	c.requested = c.postErr == nil
	return c.postErr
}
func (c *commandActionsClient) RerunFailedJobs(context.Context, github.PRTarget, int64) error {
	c.requested = c.postErr == nil
	return c.postErr
}

func mockRerunCommand(t *testing.T) *commandActionsClient {
	t.Helper()
	mockPRCommands(t)
	old := newPRRerunClient
	t.Cleanup(func() { newPRRerunClient = old })
	c := &commandActionsClient{}
	newPRRerunClient = func() (github.ActionsClient, error) { return c, nil }
	newTaskAgent = func(config.Agent, string) (review.TaskAgent, error) {
		t.Fatal("rerun started an AI agent")
		return nil, nil
	}
	return c
}

func TestPRRerunPreviewAndApplyWithoutConfigOrAgent(t *testing.T) {
	for _, apply := range []bool{false, true} {
		for _, asJSON := range []bool{false, true} {
			t.Run(strings.Join([]string{map[bool]string{false: "preview", true: "apply"}[apply], map[bool]string{false: "table", true: "json"}[asJSON]}, "/"), func(t *testing.T) {
				c := mockRerunCommand(t)
				t.Setenv("XDG_CONFIG_HOME", t.TempDir())
				args := []string{"pr", "rerun", "o/r#7", "--job", "99", "--head", "abc123", "--attempt", "1"}
				if apply {
					args = append(args, "--apply")
				}
				if asJSON {
					args = append(args, "--json")
				}
				code, out, stderr := run(args...)
				if code != 0 || c.requested != apply {
					t.Fatalf("code=%d out=%s stderr=%s client=%+v", code, out, stderr, c)
				}
				if apply && c.jobID != 99 {
					t.Fatal("did not use selected Actions job ID", c)
				}
				if asJSON {
					var result pr.RerunResult
					if err := json.Unmarshal([]byte(out), &result); err != nil || result.Apply != apply || len(result.Runs) != 1 || result.Runs[0].Requested != apply || result.ChecksPassing != apply {
						t.Fatalf("result=%+v err=%v out=%s", result, err, out)
					}
				} else if !strings.Contains(out, "│ test │ failed │ 99") || !strings.Contains(out, "│ CHECK ID │") {
					t.Fatal("lost job identity", out)
				}
			})
		}
	}
}

func TestPRRerunInvalidArgumentsAndDemoNeverReachGitHub(t *testing.T) {
	for _, flags := range [][]string{{"--job", "0"}, {"--job", "-1"}, {"--attempt", "0"}, {"--attempt", "1"}, {"--head", " "}, {"--wait-timeout", "0s"}, {"--agent", "codex"}} {
		t.Run(strings.Join(flags, " "), func(t *testing.T) {
			mockRerunCommand(t)
			newPRRerunClient = func() (github.ActionsClient, error) {
				t.Fatal("invalid args reached GitHub")
				return nil, nil
			}
			code, _, stderr := run(append([]string{"pr", "rerun", "o/r#7"}, flags...)...)
			if code != 2 {
				t.Fatalf("code=%d stderr=%s", code, stderr)
			}
		})
	}
	mockRerunCommand(t)
	prCommandsAvailable = false
	newPRRerunClient = func() (github.ActionsClient, error) {
		t.Fatal("demo reached GitHub")
		return nil, nil
	}
	code, _, stderr := run("pr", "rerun", "o/r#7", "--apply")
	if code != 1 || !strings.Contains(stderr, "unavailable in the demo build") {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
}

func TestPRRerunReportsAmbiguousRequestAndOutputErrors(t *testing.T) {
	c := mockRerunCommand(t)
	c.postErr = errors.New("connection reset")
	code, out, stderr := run("pr", "rerun", "o/r#7", "--apply", "--json")
	var result pr.RerunResult
	if err := json.Unmarshal([]byte(out), &result); err != nil || code != 1 || result.Error == "" || len(result.Runs) != 1 || !result.Runs[0].RequestAttempted || result.Runs[0].Requested || !strings.Contains(stderr, "inspect Actions before retrying") {
		t.Fatalf("code=%d result=%+v err=%v stderr=%s", code, result, err, stderr)
	}
	for _, asJSON := range []bool{false, true} {
		args := []string{"pr", "rerun", "o/r#7"}
		if asJSON {
			args = append(args, "--json")
		}
		var stderr strings.Builder
		code := execute(args, &failOutput{err: errors.New("output full")}, &stderr)
		if code != 1 {
			t.Fatalf("lost output error: code=%d stderr=%s", code, &stderr)
		}
	}
}

func TestPRRerunInterruptionUsesExit130(t *testing.T) {
	mockRerunCommand(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	root := newRoot()
	root.SetContext(ctx)
	root.SetArgs([]string{"pr", "rerun", "o/r#7", "--json"})
	var out, stderr bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&stderr)
	if code := exitCode(root.Execute(), &stderr); code != 130 {
		t.Fatalf("code=%d out=%s stderr=%s", code, &out, &stderr)
	}
}
