package pr

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/sglavoie/deps-merger/pkg/github"
)

type testClient struct {
	snapshots     []github.PRSnapshot
	reads         int
	evidenceErr   error
	evidenceReads int
}

func (c *testClient) Snapshot(ctx context.Context, _ github.PRTarget) (github.PRSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return github.PRSnapshot{}, err
	}
	i := min(c.reads, len(c.snapshots)-1)
	c.reads++
	return c.snapshots[i], nil
}
func (c *testClient) Evidence(context.Context, github.PRSnapshot) (*github.PullRequestDetails, []github.PRCheck, error) {
	c.evidenceReads++
	return &github.PullRequestDetails{Body: "release notes", Diff: "-old\n+new"}, []github.PRCheck{{Log: "compiler: missing symbol"}}, c.evidenceErr
}

type testAgent struct {
	calls    int
	writable []bool
	prompt   string
	err      error
	raw      string
}

func (*testAgent) Name() string { return "fake" }
func (a *testAgent) Task(_ context.Context, _, prompt, _ string, writable bool) (json.RawMessage, error) {
	a.calls++
	a.writable = append(a.writable, writable)
	a.prompt = prompt
	if a.raw == "" {
		a.raw = `{"summary":"Update removed API usage in src/app.go","causes":["removed API"],"steps":["replace old call"],"validation":["go test ./...: passed"],"remaining":[]}`
	}
	return json.RawMessage(a.raw), a.err
}

type testWorkspace struct {
	checkouts, pushes int
	pushErr           error
}

func (w *testWorkspace) Checkout(context.Context, string, string, int, string) (string, error) {
	w.checkouts++
	return "/private/checkout", nil
}
func (*testWorkspace) FetchBase(context.Context, string, string) error { return nil }
func (w *testWorkspace) CommitAndPush(context.Context, string, string, string, string, string) (string, error) {
	w.pushes++
	return "new-head", w.pushErr
}

func snapshot(state github.CheckState) github.PRSnapshot {
	return github.PRSnapshot{Target: github.PRTarget{Owner: "o", Repo: "r", Number: 7}, Title: "update", URL: "https://github.com/o/r/pull/7", HeadOID: "old-head", BaseOID: "base", HeadRef: "update", HeadRepo: "o/r", BaseRef: "main", State: "OPEN", Permission: "WRITE", Mergeable: "MERGEABLE", MergeState: "CLEAN", ChecksComplete: true, Checks: []github.PRCheck{{Check: github.Check{Name: "test", State: state}}}}
}
func service(states ...github.PRSnapshot) (*Service, *testClient, *testAgent, *testWorkspace) {
	c, a, w := &testClient{snapshots: states}, &testAgent{}, &testWorkspace{}
	return &Service{Client: c, Agent: a, Workspace: w, AgentTimeout: time.Second, WaitTimeout: time.Second, PollInterval: time.Millisecond, MaxAttempts: 2}, c, a, w
}

func TestDiagnoseAnyCheckOutcome(t *testing.T) {
	for _, state := range []github.CheckState{github.CheckPassed, github.CheckFailed, github.CheckPending, github.CheckUnknown, github.CheckSkipped} {
		t.Run(string(state), func(t *testing.T) {
			p := snapshot(state)
			s, _, a, w := service(p)
			got, err := s.Diagnose(context.Background(), p.Target, "compiler")
			if err != nil || got.Analysis.Summary == "" || w.pushes != 0 || a.calls != 1 || a.writable[0] {
				t.Fatalf("%+v, %v; agent %+v, ws %+v", got, err, a, w)
			}
			if !strings.Contains(a.prompt, "compiler: missing symbol") || !strings.Contains(a.prompt, "User focus: compiler") || !strings.Contains(a.prompt, "old-head") {
				t.Fatal("missing actionable evidence", a.prompt)
			}
			if got.ChecksPassing != (state == github.CheckPassed) {
				t.Fatalf("passing = %v for %s", got.ChecksPassing, state)
			}
		})
	}
}

func TestDiagnoseNoChecksAndMergeGates(t *testing.T) {
	p := snapshot(github.CheckPassed)
	p.Checks = nil
	p.Draft = true
	p.ReviewDecision = "REVIEW_REQUIRED"
	p.MergeState = "BLOCKED"
	s, _, _, _ := service(p)
	got, err := s.Diagnose(context.Background(), p.Target, "")
	if err != nil || got.ChecksPassing || !strings.Contains(strings.Join(got.Blockers, " "), "REVIEW_REQUIRED") || len(got.Blockers) < 3 {
		t.Fatalf("%+v, %v", got, err)
	}
}

func TestDiagnoseRejectsMovedEvidence(t *testing.T) {
	old := snapshot(github.CheckFailed)
	moved := old
	moved.HeadOID = "concurrent"
	s, _, _, w := service(old, moved)
	_, err := s.Diagnose(context.Background(), old.Target, "")
	if err == nil || w.pushes != 0 {
		t.Fatalf("err=%v pushes=%d", err, w.pushes)
	}
}

func TestFixRetriesThenConfirmsCI(t *testing.T) {
	old := snapshot(github.CheckFailed)
	failed := old
	failed.HeadOID = "new-head"
	passing := failed
	passing.Checks = []github.PRCheck{{Check: github.Check{Name: "test", State: github.CheckPassed}}}
	passing.ReviewDecision = "REVIEW_REQUIRED"
	passing.MergeState = "BLOCKED"
	s, _, a, w := service(old, old, failed, failed, passing)
	got, err := s.Fix(context.Background(), old.Target, "")
	if err != nil || !got.ChecksPassing || got.Attempts != 2 || len(got.Commits) != 2 || w.pushes != 2 || a.calls != 2 || !a.writable[0] || !a.writable[1] {
		t.Fatalf("%+v %v; agent %+v ws %+v", got, err, a, w)
	}
	if !strings.Contains(strings.Join(got.Blockers, " "), "REVIEW_REQUIRED") {
		t.Fatal("manual merge gate hidden")
	}
}

func TestFixDoesNotEditPassingPR(t *testing.T) {
	p := snapshot(github.CheckPassed)
	s, _, a, w := service(p)
	got, err := s.Fix(context.Background(), p.Target, "")
	if err != nil || !got.ChecksPassing || a.calls != 0 || w.pushes != 0 {
		t.Fatalf("%+v %v; agent %+v ws %+v", got, err, a, w)
	}
}

func TestFixWaitsForExistingCI(t *testing.T) {
	pending := snapshot(github.CheckPending)
	passing := snapshot(github.CheckPassed)
	s, _, a, w := service(pending, passing)
	got, err := s.Fix(context.Background(), pending.Target, "")
	if err != nil || !got.ChecksPassing || a.calls != 0 || w.pushes != 0 {
		t.Fatalf("%+v %v; agent %+v ws %+v", got, err, a, w)
	}
}

func TestFixRefusesConcurrentChanges(t *testing.T) {
	for _, field := range []string{"head", "base", "branch", "repo", "closed", "permission"} {
		t.Run(field, func(t *testing.T) {
			old := snapshot(github.CheckFailed)
			moved := old
			switch field {
			case "head":
				moved.HeadOID = "human"
			case "base":
				moved.BaseOID = "new-base"
			case "branch":
				moved.HeadRef = "other"
			case "repo":
				moved.HeadRepo = "o/fork"
			case "closed":
				moved.State = "CLOSED"
			case "permission":
				moved.Permission = "READ"
			}
			s, _, _, w := service(old, moved)
			got, err := s.Fix(context.Background(), old.Target, "")
			if err == nil || w.pushes != 0 || got.Workspace == "" || got.Error == "" {
				t.Fatalf("%+v %v; pushes %d", got, err, w.pushes)
			}
		})
	}
}

func TestFixStopsWhenChecksNeverStart(t *testing.T) {
	old := snapshot(github.CheckFailed)
	empty := old
	empty.HeadOID = "new-head"
	empty.Checks = nil
	s, _, a, w := service(old, old, empty)
	s.WaitTimeout = 20 * time.Millisecond
	got, err := s.Fix(context.Background(), old.Target, "")
	if err == nil || got.ChecksPassing || w.pushes != 1 || a.calls != 1 || len(got.Commits) != 1 {
		t.Fatalf("%+v %v; pushes %d calls %d", got, err, w.pushes, a.calls)
	}
}

func TestFixStopsAtAttemptLimit(t *testing.T) {
	old := snapshot(github.CheckFailed)
	failed := old
	failed.HeadOID = "new-head"
	s, _, a, w := service(old, old, failed, failed, failed)
	got, err := s.Fix(context.Background(), old.Target, "")
	if err == nil || !strings.Contains(err.Error(), "2 repair attempts") || got.ChecksPassing || w.pushes != 2 || a.calls != 2 {
		t.Fatalf("%+v %v; pushes %d calls %d", got, err, w.pushes, a.calls)
	}
}

func TestFixKeepsChangesOnAgentAndPushFailures(t *testing.T) {
	for _, stage := range []string{"agent", "push"} {
		t.Run(stage, func(t *testing.T) {
			old := snapshot(github.CheckFailed)
			s, _, a, w := service(old)
			if stage == "agent" {
				a.err = errors.New("limit")
			} else {
				w.pushErr = errors.New("lease rejected")
			}
			got, err := s.Fix(context.Background(), old.Target, "")
			if err == nil || got.Workspace == "" || got.ChecksPassing || len(got.Commits) != 0 {
				t.Fatalf("%+v %v", got, err)
			}
		})
	}
}

func TestFixRejectsInvalidTargets(t *testing.T) {
	for _, kind := range []string{"closed", "archived", "read-only", "base-branch", "incomplete", "deleted-head"} {
		t.Run(kind, func(t *testing.T) {
			p := snapshot(github.CheckFailed)
			switch kind {
			case "closed":
				p.State = "CLOSED"
			case "archived":
				p.Archived = true
			case "read-only":
				p.Permission = "READ"
			case "base-branch":
				p.HeadRef = p.BaseRef
			case "incomplete":
				p.ChecksComplete = false
			case "deleted-head":
				p.HeadRepo = ""
			}
			s, _, a, w := service(p)
			_, err := s.Fix(context.Background(), p.Target, "")
			if err == nil || a.calls != 0 || w.pushes != 0 {
				t.Fatalf("err=%v calls=%d pushes=%d", err, a.calls, w.pushes)
			}
		})
	}
}

func TestCheckStateKeepsUnknownAndMissingChecksBlocked(t *testing.T) {
	for _, kind := range []string{"missing", "skipped", "unknown", "incomplete"} {
		t.Run(kind, func(t *testing.T) {
			p := snapshot(github.CheckPassed)
			switch kind {
			case "missing":
				p.Checks = nil
			case "skipped":
				p.Checks[0].State = github.CheckSkipped
			case "unknown":
				p.Checks[0].State = github.CheckUnknown
			case "incomplete":
				p.ChecksComplete = false
			}
			if passing, _ := CheckState(p); passing {
				t.Fatal("invalid checks reported passing")
			}
		})
	}
}

func TestInvalidAgentAnswerCannotPush(t *testing.T) {
	for _, raw := range []string{`{}`, `{"summary":"ok"}`, `{"summary":"ok","causes":null,"steps":[],"validation":[],"remaining":[]}`, `garbage`} {
		s, _, a, w := service(snapshot(github.CheckFailed))
		a.raw = raw
		if _, err := s.Fix(context.Background(), snapshot(github.CheckFailed).Target, ""); err == nil || w.pushes != 0 {
			t.Fatalf("accepted %q: %v", raw, err)
		}
	}
}

func TestMarkdownIsStandaloneAndReportsWriterErrors(t *testing.T) {
	result := newResult(snapshot(github.CheckFailed))
	result.Analysis.Summary = "Replace removed API"
	result.Analysis.Steps = []string{"Edit src/app.go"}
	var out strings.Builder
	if err := Markdown(&out, result); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"o/r#7", "old-head", "Edit src/app.go", "not confirmed passing"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %s: %s", want, out.String())
		}
	}
	if err := Markdown(brokenWriter{}, result); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestSupersededFailureDoesNotShortcutPassingObservation(t *testing.T) {
	p := snapshot(github.CheckSkipped)
	p.Checks[0].Check = github.Check{Name: "test", State: github.CheckFailed, Suite: 1, App: 2, Workflow: 3, Event: "pull_request"}
	p.Checks = append(p.Checks, github.PRCheck{Check: github.Check{Name: "test", State: github.CheckPassed, Suite: 2, App: 2, Workflow: 3, Event: "pull_request"}})
	if hasFailure(p) {
		t.Fatal("superseded failure counted as current")
	}
}

func TestFixRetriesUnresolvedConflictsEvenWithGreenCI(t *testing.T) {
	old := snapshot(github.CheckPassed)
	old.Mergeable = "CONFLICTING"
	old.MergeState = "DIRTY"
	next := old
	next.HeadOID = "new-head"
	s, _, a, w := service(old, old, next, next, next)
	got, err := s.Fix(context.Background(), old.Target, "")
	if err == nil || w.pushes != 2 || a.calls != 2 || !got.ChecksPassing {
		t.Fatalf("%+v %v pushes=%d calls=%d", got, err, w.pushes, a.calls)
	}
}

func TestFixWaitToleratesPushPropagationAndSeesNewJobs(t *testing.T) {
	old := snapshot(github.CheckFailed)
	green := snapshot(github.CheckPassed)
	green.HeadOID = "new-head"
	pending := green
	pending.Checks = append([]github.PRCheck{}, green.Checks...)
	pending.Checks = append(pending.Checks, github.PRCheck{Check: github.Check{Name: "late-job", State: github.CheckPending}})
	final := pending
	final.Checks = append([]github.PRCheck{}, pending.Checks...)
	final.Checks[1].State = github.CheckPassed
	s, c, a, w := service(old, old, old, green, pending, final, final)
	got, err := s.Fix(context.Background(), old.Target, "")
	if err != nil || !got.ChecksPassing || c.reads != 7 || a.calls != 1 || w.pushes != 1 {
		t.Fatalf("%+v %v reads=%d calls=%d pushes=%d", got, err, c.reads, a.calls, w.pushes)
	}
}

func TestWaitStopsWhenHumanPushesAfterRepair(t *testing.T) {
	old := snapshot(github.CheckFailed)
	other := old
	other.HeadOID = "human"
	s, _, _, w := service(old, old, other)
	got, err := s.Fix(context.Background(), old.Target, "")
	if err == nil || !strings.Contains(err.Error(), "head changed") || w.pushes != 1 || len(got.Commits) != 1 {
		t.Fatalf("%+v %v pushes=%d", got, err, w.pushes)
	}
}

func TestDiagnosisOmitsSupersededFailures(t *testing.T) {
	p := snapshot(github.CheckPassed)
	p.Checks = []github.PRCheck{
		{Check: github.Check{Name: "test", State: github.CheckFailed, Suite: 1, App: 2, Workflow: 3, Event: "pull_request"}},
		{Check: github.Check{Name: "test", State: github.CheckPassed, Suite: 2, App: 2, Workflow: 3, Event: "pull_request"}},
	}
	s, _, a, _ := service(p)
	result, err := s.Diagnose(context.Background(), p.Target, "")
	if err != nil || !result.ChecksPassing || len(result.Snapshot.Checks) != 1 || strings.Contains(a.prompt, `"state":"failed"`) {
		t.Fatalf("result %+v err %v prompt %s", result, err, a.prompt)
	}
}

func TestRepairReportsSpecificConcurrentChanges(t *testing.T) {
	for _, kind := range []string{"head", "base", "branch", "repository", "state"} {
		t.Run(kind, func(t *testing.T) {
			before := snapshot(github.CheckFailed)
			after := before
			expected := ""
			switch kind {
			case "head":
				after.HeadOID = "new-head"
				expected = `head commit changed from "old-head" to "new-head"`
			case "base":
				after.BaseOID = "new-base"
				expected = `base commit changed from "base" to "new-base"`
			case "branch":
				after.BaseRef = "release"
				expected = `base branch changed from "main" to "release"`
			case "repository":
				after.HeadRepo = "other/fork"
				expected = `head repository changed from "o/r" to "other/fork"`
			case "state":
				after.State = "CLOSED"
				expected = `PR state changed from "OPEN" to "CLOSED"`
			}
			s, _, _, w := service(before, after)
			result, err := s.Fix(context.Background(), before.Target, "")
			if err == nil || !strings.Contains(err.Error(), expected) || result.Workspace == "" || w.pushes != 0 {
				t.Fatalf("result=%+v err=%v pushes=%d", result, err, w.pushes)
			}
		})
	}
}

type customBaseWorkspace struct{ testWorkspace }

func (*customBaseWorkspace) BaseReference(string) string { return "refs/deps-merger/base" }
func TestRepairAgentUsesSelectedWorkspaceBaseReference(t *testing.T) {
	p := snapshot(github.CheckFailed)
	s, _, agent, _ := service(p)
	s.Workspace = &customBaseWorkspace{}
	agent.err = errors.New("stop after checking prompt")
	_, _ = s.Fix(context.Background(), p.Target, "")
	if !strings.Contains(agent.prompt, "base branch is available as refs/deps-merger/base") {
		t.Fatal(agent.prompt)
	}
}
