package pr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/sglavoie/deps-handler/pkg/github"
)

type handleClient struct {
	*rerunClient
	state                    github.HandleState
	stateReads, merges       int
	mergedHead, mergedMethod string
	mergeErr                 error
	mergedState              github.PRState
	onState                  func(*handleClient)
}

func (c *handleClient) HandleState(ctx context.Context, _ github.PRTarget) (github.HandleState, error) {
	if err := ctx.Err(); err != nil {
		return github.HandleState{}, err
	}
	c.stateReads++
	if c.onState != nil {
		c.onState(c)
	}
	return c.state, nil
}
func (c *handleClient) OpenPRs(context.Context, string, string) ([]github.PRTarget, error) {
	return []github.PRTarget{c.state.Snapshot.Target}, nil
}
func (c *handleClient) Evidence(context.Context, github.PRSnapshot) (github.PREvidence, error) {
	return github.PREvidence{Details: &github.PullRequestDetails{Body: c.state.Body, Diff: "-old\n+new"}, Checks: c.state.Snapshot.Checks}, nil
}
func (c *handleClient) Merge(_ context.Context, _, _ string, _ int, method, head string) error {
	c.merges++
	c.mergedHead, c.mergedMethod = head, method
	return c.mergeErr
}
func (c *handleClient) PullRequestState(context.Context, string, string, int) (github.PRState, error) {
	return c.mergedState, nil
}

type handleWorkspace struct {
	testWorkspace
	client     *handleClient
	beforePush func()
}

func (w *handleWorkspace) CommitAndPush(ctx context.Context, dir, owner, name, branch, expected string) (string, error) {
	if w.beforePush != nil {
		w.beforePush()
	}
	oid, err := w.testWorkspace.CommitAndPush(ctx, dir, owner, name, branch, expected)
	if err == nil {
		w.client.state.Snapshot.HeadOID = oid
		w.client.state.UpdatedAt = "after-push"
		w.client.snapshots = []github.PRSnapshot{w.client.state.Snapshot}
		w.client.reads = 0
	}
	return oid, err
}

func handleFixture() (*HandleService, *handleClient, *testAgent, *handleWorkspace) {
	p := snapshot(github.CheckPassed)
	c := &handleClient{rerunClient: &rerunClient{snapshots: []github.PRSnapshot{p}}, state: github.HandleState{
		Snapshot: p, Author: "contributor", UpdatedAt: "before-push", Body: "Fix an edge case", DiscussionsComplete: true, MergeMethods: []string{"squash", "merge", "rebase"},
	}}
	a := &testAgent{raw: handleAnswer("approve", "")}
	w := &handleWorkspace{client: c}
	s := &HandleService{Client: c, Agent: a, Workspace: w, MaxAttempts: 2, WaitTimeout: 30 * time.Millisecond, PollInterval: time.Millisecond, AgentTimeout: time.Second}
	return s, c, a, w
}

func handleAnswer(decision, recommendation string) string {
	return fmt.Sprintf(`{"decision":%q,"analysis":{"summary":"Reviewed edge case behavior","causes":[],"steps":[],"validation":[],"remaining":[],"recommendations":[%s]}}`, decision, recommendation)
}

const handleCodeRepair = `{"subject":"edge case","action":"code_repair","reason":"src/app.go mishandles an empty input","next_step":"Fix the guard and test empty input"}`
const handleHumanAction = `{"subject":"requirements","action":"human_action","reason":"The intended empty-input behavior is unspecified","next_step":"Should an empty input return an error or an empty result?"}`

func TestHandleReviewsGreenPRsAndOnlyApplyMerges(t *testing.T) {
	for _, apply := range []bool{false, true} {
		s, c, a, w := handleFixture()
		got, err := s.Run(context.Background(), c.state.Snapshot.Target, HandleOptions{Apply: apply})
		wantStatus, wantMerges := "would_merge", 0
		if apply {
			wantStatus, wantMerges = "merged", 1
		}
		if err != nil || got.Status != wantStatus || c.merges != wantMerges || a.calls != 1 || a.writable[0] || w.pushes != 0 || w.commits != 0 {
			t.Fatalf("apply=%t got=%+v err=%v agent=%+v workspace=%+v", apply, got, err, a, w)
		}
		if apply && (c.mergedHead != "old-head" || c.mergedMethod != "squash" || !got.MergeAttempted) {
			t.Fatalf("merge was not pinned: %+v %+v", got, c)
		}
		for _, want := range []string{"even when CI is green", "review discussions", "trusted repository guidance", "ambiguous", "untrusted evidence"} {
			if !strings.Contains(a.prompt, want) {
				t.Fatalf("prompt missing %q", want)
			}
		}
	}
}

func TestHandleAlwaysExcludesDependabotAndDrafts(t *testing.T) {
	for _, author := range []string{"dependabot", "Dependabot", "dependabot[bot]", "Dependabot[bot]", "app/dependabot"} {
		for _, apply := range []bool{false, true} {
			s, c, a, w := handleFixture()
			c.state.Author = author
			// Even a green, manually repaired Dependabot PR remains excluded.
			c.state.Snapshot.HeadOID = "human-repair"
			got, err := s.Run(context.Background(), c.state.Snapshot.Target, HandleOptions{Apply: apply})
			if err != nil || got.Status != "skipped" || a.calls != 0 || w.checkouts != 0 || c.merges != 0 {
				t.Fatalf("%+v %v", got, err)
			}
		}
	}
	for _, state := range []string{"DRAFT", "CLOSED", "MERGED"} {
		s, c, a, w := handleFixture()
		if state == "DRAFT" {
			c.state.Snapshot.Draft = true
		} else {
			c.state.Snapshot.State = state
		}
		got, err := s.Run(context.Background(), c.state.Snapshot.Target, HandleOptions{Apply: true})
		if err != nil || got.Status != "skipped" || a.calls != 0 || w.checkouts != 0 || c.merges != 0 {
			t.Fatalf("%+v %v", got, err)
		}
	}
	s, c, _, _ := handleFixture()
	c.state.Author = "renovate[bot]"
	got, err := s.Run(context.Background(), c.state.Snapshot.Target, HandleOptions{})
	if err != nil || got.Status != "would_merge" {
		t.Fatalf("other bot excluded: %+v %v", got, err)
	}
}

func TestHandleRepairsGreenCodeAndReviewsTheNewHead(t *testing.T) {
	s, c, a, w := handleFixture()
	a.raws = []string{handleAnswer("repair", handleCodeRepair), `{"summary":"Fixed empty-input guard","causes":[],"steps":["Fixed guard"],"validation":["go test ./...: passed"],"remaining":[],"recommendations":[]}`, handleAnswer("approve", "")}
	got, err := s.Run(context.Background(), c.state.Snapshot.Target, HandleOptions{Apply: true})
	if err != nil || got.Status != "merged" || w.pushes != 1 || a.calls != 3 || len(got.Reviews) != 2 || len(got.Repairs) != 1 || c.mergedHead != "new-head" {
		t.Fatalf("got=%+v err=%v agent=%+v client=%+v", got, err, a, c)
	}
	if fmt.Sprint(a.writable) != "[false true false]" || len(got.Commits) != 1 {
		t.Fatal(a.writable, got.Commits)
	}
}

func TestHandlePreviewDoesNotEditRepairablePR(t *testing.T) {
	s, c, a, w := handleFixture()
	a.raw = handleAnswer("repair", handleCodeRepair)
	got, err := s.Run(context.Background(), c.state.Snapshot.Target, HandleOptions{})
	if err != nil || got.Status != "would_repair" || w.pushes != 0 || w.commits != 0 || c.merges != 0 || fmt.Sprint(a.writable) != "[false]" {
		t.Fatalf("%+v %v %+v", got, err, w)
	}
}

func TestHandleHumanDecisionIsActionableAndNeverEdits(t *testing.T) {
	s, c, a, w := handleFixture()
	a.raw = handleAnswer("blocked", handleHumanAction)
	got, err := s.Run(context.Background(), c.state.Snapshot.Target, HandleOptions{Apply: true})
	if err != nil || got.Status != "blocked" || w.pushes != 0 || c.merges != 0 || a.calls != 1 {
		t.Fatalf("%+v %v", got, err)
	}
	var b strings.Builder
	if err := HandleReport(&b, []HandleResult{got}, true, false); err != nil || !strings.Contains(b.String(), "Should an empty input return") {
		t.Fatal(b.String(), err)
	}
}

func TestHandleDoesNotRepairWhileHumanDecisionIsPending(t *testing.T) {
	for _, apply := range []bool{false, true} {
		s, c, a, w := handleFixture()
		a.raw = handleAnswer("repair", handleCodeRepair+","+handleHumanAction)
		got, err := s.Run(context.Background(), c.state.Snapshot.Target, HandleOptions{Apply: apply})
		if err != nil || got.Status != "blocked" || a.calls != 1 || w.pushes != 0 || c.merges != 0 {
			t.Fatalf("apply=%t got=%+v err=%v", apply, got, err)
		}
	}
}

func TestHandlePreservesGitHubGatesDespiteAIApproval(t *testing.T) {
	for _, gate := range []string{"review", "discussion", "merge-state", "mergeability", "permission", "method", "incomplete-discussions", "author", "archive"} {
		t.Run(gate, func(t *testing.T) {
			s, c, _, w := handleFixture()
			opts := HandleOptions{Apply: true}
			switch gate {
			case "review":
				c.state.Snapshot.ReviewDecision = "REVIEW_REQUIRED"
			case "discussion":
				resolved := false
				c.state.Threads = []github.DiscussionThread{{Path: "src/app.go", IsResolved: &resolved}}
			case "merge-state":
				c.state.Snapshot.MergeState = "BLOCKED"
			case "mergeability":
				c.state.Snapshot.Mergeable = "UNKNOWN"
			case "permission":
				c.state.Snapshot.Permission = "READ"
			case "method":
				opts.MergeMethod = "rebase"
				c.state.MergeMethods = []string{"merge"}
			case "incomplete-discussions":
				c.state.DiscussionsComplete = false
			case "author":
				c.state.Author = ""
			case "archive":
				c.state.Snapshot.Archived = true
			}
			got, err := s.Run(context.Background(), c.state.Snapshot.Target, opts)
			if c.merges != 0 || w.pushes != 0 || (got.Status != "blocked" && err == nil) {
				t.Fatalf("%+v %v", got, err)
			}
		})
	}
}

func TestHandleInvalidatesReviewBeforeAnyWrite(t *testing.T) {
	for _, phase := range []string{"review", "repair"} {
		for _, change := range []string{"head", "base", "body", "author", "discussion", "draft", "target-branch"} {
			t.Run(phase+"/"+change, func(t *testing.T) {
				s, c, a, w := handleFixture()
				if phase == "repair" {
					a.raws = []string{handleAnswer("repair", handleCodeRepair), `{"summary":"Fixed guard","causes":[],"steps":[],"validation":[],"remaining":[],"recommendations":[]}`}
				}
				read := 2
				if phase == "repair" {
					read = 3
				}
				c.onState = func(c *handleClient) {
					if c.stateReads != read {
						return
					}
					switch change {
					case "head":
						c.state.Snapshot.HeadOID = "other"
					case "base":
						c.state.Snapshot.BaseOID = "other"
					case "body":
						c.state.Body = "New requirements"
					case "author":
						c.state.Author = "dependabot[bot]"
					case "discussion":
						c.state.UpdatedAt = "new-comment"
					case "draft":
						c.state.Snapshot.Draft = true
					case "target-branch":
						c.state.Snapshot.BaseRef = "release"
					}
				}
				got, err := s.Run(context.Background(), c.state.Snapshot.Target, HandleOptions{Apply: true})
				// A draft observed at the final gate is blocked without a merge.
				if w.pushes != 0 || c.merges != 0 || (err == nil && got.Status != "blocked") {
					t.Fatalf("%+v %v", got, err)
				}
			})
		}
	}
}

func TestHandleAttemptsAreBoundedAndFinalReviewStillRuns(t *testing.T) {
	s, c, a, w := handleFixture()
	s.MaxAttempts = 1
	a.raws = []string{handleAnswer("repair", handleCodeRepair), `{"summary":"Attempted guard fix","causes":[],"steps":[],"validation":[],"remaining":[],"recommendations":[]}`, handleAnswer("repair", handleCodeRepair)}
	got, err := s.Run(context.Background(), c.state.Snapshot.Target, HandleOptions{Apply: true})
	if err != nil || got.Status != "blocked" || w.pushes != 1 || c.merges != 0 || len(got.Reviews) != 2 || !strings.Contains(got.Summary, "after 1") {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestHandleRejectsMalformedAndContradictoryVerdicts(t *testing.T) {
	for _, answer := range []string{
		`{"decision":"approve"}`,
		strings.Replace(handleAnswer("approve", ""), `"decision":"approve"`, `"decision":"blocked","decision":"approve"`, 1),
		handleAnswer("approve", handleHumanAction),
		handleAnswer("repair", handleHumanAction),
		handleAnswer("blocked", ""),
		handleAnswer("yes", ""),
		strings.Replace(handleAnswer("approve", ""), `"remaining":[]`, `"remaining":["not sure"]`, 1),
	} {
		s, c, a, w := handleFixture()
		a.raw = answer
		got, err := s.Run(context.Background(), c.state.Snapshot.Target, HandleOptions{Apply: true})
		if err == nil || c.merges != 0 || w.pushes != 0 || got.Status != "error" {
			t.Fatalf("accepted %s: %+v %v", answer, got, err)
		}
	}
	for _, checks := range [][]github.PRCheck{nil, {{Check: github.Check{Name: "test", State: github.CheckSkipped}}}, {{Check: github.Check{Name: "test", State: github.CheckUnknown}}}} {
		s, c, _, _ := handleFixture()
		c.state.Snapshot.Checks = checks
		got, err := s.Run(context.Background(), c.state.Snapshot.Target, HandleOptions{Apply: true})
		if err == nil || c.merges != 0 {
			t.Fatalf("approved missing/unknown checks: %+v %v", got, err)
		}
	}
}

func TestHandleReconcilesAmbiguousMergeWithoutRetry(t *testing.T) {
	for _, final := range []github.PRState{github.PRMerged, github.PROpen, github.PRClosed, ""} {
		s, c, _, _ := handleFixture()
		c.mergeErr, c.mergedState = errors.New("connection lost"), final
		got, err := s.Run(context.Background(), c.state.Snapshot.Target, HandleOptions{Apply: true})
		if c.merges != 1 || (final == github.PRMerged && (err != nil || got.Status != "merged")) || (final != github.PRMerged && (err == nil || got.Status != "error")) {
			t.Fatalf("%s: %+v %v", final, got, err)
		}
	}
}

func TestHandleReportCleansUntrustedTextAndPropagatesWriteErrors(t *testing.T) {
	result := HandleResult{Target: github.PRTarget{Owner: "o", Repo: "r", Number: 7}, Status: "blocked", Summary: "bad\x1b[2Jtext", Blockers: []string{"needs review"}}
	for _, list := range []bool{false, true} {
		var b strings.Builder
		if err := HandleReport(&b, []HandleResult{result}, false, list); err != nil || strings.Contains(b.String(), "\x1b[2J") || !strings.Contains(b.String(), "needs review") {
			t.Fatal(b.String(), err)
		}
		if err := HandleReport(errorHandleWriter{}, []HandleResult{result}, false, list); !errors.Is(err, io.ErrClosedPipe) {
			t.Fatal(err)
		}
	}
}

type errorHandleWriter struct{}

func (errorHandleWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestHandleRerunsOnlySupportedTransientFailures(t *testing.T) {
	s, c, a, _ := handleFixture()
	_, rc, p, run := rerunFixture()
	c.rerunClient, c.state.Snapshot = rc, p
	check := p.Checks[0]
	check.JobID, check.RunID, check.RunAttempt = 99, 20, 1
	c.state.Snapshot.Checks = []github.PRCheck{check}
	done := run
	done.Attempt, done.Conclusion = 2, "success"
	rc.runs[20] = []github.ActionsRun{run, run, done}
	passing := p
	passing.Checks = append([]github.PRCheck{}, p.Checks...)
	passing.Checks[0].State = github.CheckPassed
	rc.snapshots = []github.PRSnapshot{p, p, p, passing}
	a.raws = []string{handleAnswer("blocked", `{"subject":"test","action":"rerun_candidate","reason":"Runner log reports a temporary service outage","next_step":"Rerun test once"}`), handleAnswer("approve", "")}
	c.onState = func(c *handleClient) {
		if len(c.posts) > 0 {
			c.state.Snapshot = passing
		}
	}
	got, err := s.Run(context.Background(), p.Target, HandleOptions{Apply: true})
	if err != nil || got.Status != "merged" || len(c.posts) != 1 || len(got.Reruns) != 1 || a.calls != 2 {
		t.Fatalf("%+v %v reads=%d runs=%v posts=%v", got, err, rc.reads, rc.runReads, c.posts)
	}
}

func TestHandleCanceledAgentCannotAuthorizeMerge(t *testing.T) {
	s, c, _, _ := handleFixture()
	ctx, cancel := context.WithCancel(context.Background())
	s.Agent = cancelHandleAgent{cancel}
	got, err := s.Run(ctx, c.state.Snapshot.Target, HandleOptions{Apply: true})
	if !errors.Is(err, context.Canceled) || got.Status != "error" || c.merges != 0 {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestHandleWaitsForPendingChecksBeforeReview(t *testing.T) {
	s, c, a, w := handleFixture()
	pending := c.state.Snapshot
	pending.Checks = []github.PRCheck{{Check: github.Check{Name: "test", State: github.CheckPending}}}
	c.snapshots = []github.PRSnapshot{pending, c.state.Snapshot}
	passing := c.state.Snapshot
	c.state.Snapshot = pending
	c.onState = func(c *handleClient) {
		if c.stateReads == 2 {
			c.state.Snapshot = passing
		}
	}
	got, err := s.Run(context.Background(), pending.Target, HandleOptions{Apply: true})
	if err != nil || got.Status != "merged" || a.calls != 1 || w.pushes != 0 || c.reads < 3 {
		t.Fatalf("%+v %v reads=%d", got, err, c.reads)
	}
}

func TestHandleNeverMergesAfterCITimeoutOrFailedPublication(t *testing.T) {
	for _, failure := range []string{"push", "pending", "base-after-push"} {
		s, c, a, w := handleFixture()
		if failure == "pending" {
			c.state.Snapshot.Checks[0].State = github.CheckPending
		} else {
			a.raws = []string{handleAnswer("repair", handleCodeRepair), `{"summary":"Fixed guard","causes":[],"steps":[],"validation":[],"remaining":[],"recommendations":[]}`}
			if failure == "push" {
				w.pushErr = errors.New("lease rejected")
			}
			if failure == "base-after-push" {
				c.onState = func(c *handleClient) {
					if c.stateReads == 4 {
						c.state.Snapshot.BaseOID = "new-base"
					}
				}
			}
		}
		got, err := s.Run(context.Background(), c.state.Snapshot.Target, HandleOptions{Apply: true})
		if err == nil || got.Status != "error" || c.merges != 0 {
			t.Fatalf("%s: %+v %v", failure, got, err)
		}
		if failure != "pending" && got.Workspace == "" {
			t.Fatal("lost repair checkout", got)
		}
	}
}

func TestHandleRequestedChangesNeedReviewerApprovalOrDismissal(t *testing.T) {
	for _, later := range []string{"", "COMMENTED", "APPROVED", "DISMISSED"} {
		s, c, _, _ := handleFixture()
		author := &struct{ Login string }{"maintainer"}
		c.state.Reviews.Nodes = []github.DiscussionComment{{Author: author, State: "CHANGES_REQUESTED"}}
		if later != "" {
			c.state.Reviews.Nodes = append(c.state.Reviews.Nodes, github.DiscussionComment{Author: author, State: later})
		}
		got, err := s.Run(context.Background(), c.state.Snapshot.Target, HandleOptions{Apply: true})
		want := "blocked"
		if later == "APPROVED" || later == "DISMISSED" {
			want = "merged"
		}
		if err != nil || got.Status != want {
			t.Fatalf("later=%s got=%+v err=%v", later, got, err)
		}
	}
}

func TestHandleRerunsAtMostOnceAndStopsOnUncertainWrites(t *testing.T) {
	for _, kind := range []string{"failed-again", "unconfirmed-post", "external", "human-action"} {
		s, c, a, _ := handleFixture()
		_, rc, p, run := rerunFixture()
		c.rerunClient, c.state.Snapshot = rc, p
		check := p.Checks[0]
		check.JobID, check.RunID, check.RunAttempt = 99, 20, 1
		c.state.Snapshot.Checks = []github.PRCheck{check}
		done := run
		done.Attempt = 2 // A completed failed rerun should get fresh analysis.
		rc.runs[20] = []github.ActionsRun{run, run, done}
		recommendation := `{"subject":"test","action":"rerun_candidate","reason":"Runner log reports a temporary service outage","next_step":"Rerun test once"}`
		if kind == "unconfirmed-post" {
			rc.postErr = errors.New("response lost")
		}
		if kind == "external" {
			c.state.Snapshot.Checks[0].URL = "https://external.example/job"
		}
		if kind == "human-action" {
			recommendation += "," + handleHumanAction
		}
		a.raw = handleAnswer("blocked", recommendation)
		got, err := s.Run(context.Background(), p.Target, HandleOptions{Apply: true})
		wantPosts, wantCalls := 0, 1
		if kind == "failed-again" {
			wantPosts, wantCalls = 1, 2
		}
		if kind == "unconfirmed-post" {
			wantPosts = 1
		}
		if len(c.posts) != wantPosts || a.calls != wantCalls || c.merges != 0 || (kind == "unconfirmed-post" && err == nil) || (kind != "unconfirmed-post" && (err != nil || got.Status != "blocked")) {
			t.Fatalf("%s: %+v %v posts=%v calls=%d", kind, got, err, c.posts, a.calls)
		}
	}
}

type cancelHandleAgent struct{ cancel context.CancelFunc }

func (cancelHandleAgent) Name() string { return "cancel" }
func (a cancelHandleAgent) Task(context.Context, string, string, string, bool) (json.RawMessage, error) {
	a.cancel()
	return json.RawMessage(handleAnswer("approve", "")), nil
}
