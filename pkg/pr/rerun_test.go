package pr

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/sglavoie/deps-handler/pkg/github"
)

type rerunClient struct {
	snapshots []github.PRSnapshot
	reads     int
	jobs      map[int64]github.ActionsJob
	runs      map[int64][]github.ActionsRun
	runReads  map[int64]int
	posts     []string
	postErr   error
	jobErr    error
	runErr    error
	snapErr   error
}

func (c *rerunClient) Snapshot(ctx context.Context, _ github.PRTarget) (github.PRSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return github.PRSnapshot{}, err
	}
	p := c.snapshots[min(c.reads, len(c.snapshots)-1)]
	c.reads++
	return p, c.snapErr
}
func (c *rerunClient) ActionsJob(_ context.Context, _ github.PRTarget, id int64) (github.ActionsJob, error) {
	return c.jobs[id], c.jobErr
}
func (c *rerunClient) ActionsRun(_ context.Context, _ github.PRTarget, id int64) (github.ActionsRun, error) {
	i := c.runReads[id]
	c.runReads[id]++
	states := c.runs[id]
	if len(states) == 0 {
		return github.ActionsRun{}, errors.New("unknown run")
	}
	return states[min(i, len(states)-1)], c.runErr
}
func (c *rerunClient) RerunJob(_ context.Context, _ github.PRTarget, id int64) error {
	c.posts = append(c.posts, fmt.Sprintf("job %d", id))
	return c.postErr
}
func (c *rerunClient) RerunFailedJobs(_ context.Context, _ github.PRTarget, id int64) error {
	c.posts = append(c.posts, fmt.Sprintf("run %d", id))
	return c.postErr
}

func rerunFixture() (*RerunService, *rerunClient, github.PRSnapshot, github.ActionsRun) {
	p := snapshot(github.CheckFailed)
	p.HeadOID = "abc123"
	p.Checks[0].ID = 42 // The check ID differs from the job ID.
	p.Checks[0].URL = "https://github.com/o/r/actions/runs/20/job/99"
	p.Checks[0].Suite, p.Checks[0].Workflow, p.Checks[0].Event = 4, 5, "pull_request"
	run := github.ActionsRun{ID: 20, Attempt: 1, HeadSHA: p.HeadOID, SuiteID: 4, WorkflowID: 5, Event: "pull_request", Status: "completed", Conclusion: "failure"}
	run.Repository.FullName = "o/r"
	c := &rerunClient{snapshots: []github.PRSnapshot{p},
		jobs: map[int64]github.ActionsJob{99: {ID: 99, RunID: 20, RunAttempt: 1, HeadSHA: p.HeadOID, Name: "test", Status: "completed", Conclusion: "failure", CheckURL: "https://api.github.com/repos/o/r/check-runs/42"}},
		runs: map[int64][]github.ActionsRun{20: {run}}, runReads: map[int64]int{}, posts: []string{},
	}
	return &RerunService{Client: c, WaitTimeout: 30 * time.Millisecond, PollInterval: time.Millisecond}, c, p, run
}

func TestRerunPreviewDoesNotWrite(t *testing.T) {
	s, c, p, _ := rerunFixture()
	got, err := s.Run(context.Background(), p.Target, RerunOptions{})
	if err != nil || got.Apply || len(c.posts) != 0 || len(got.Runs) != 1 || len(got.Runs[0].Jobs) != 1 || got.Runs[0].Jobs[0].JobID != 99 || got.Runs[0].Jobs[0].ID != 42 || got.Runs[0].PreviousAttempt != 1 || got.Runs[0].Requested {
		t.Fatalf("preview=%+v err=%v client=%+v", got, err, c)
	}
	var b strings.Builder
	if err := RerunMarkdown(&b, got); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Preview only", "dependent jobs", "job 99, check 42", "deps-handler pr rerun o/r#7 --job 99 --head abc123 --attempt 1 --apply"} {
		if !strings.Contains(b.String(), want) {
			t.Fatalf("missing %q: %s", want, &b)
		}
	}
}

func TestRerunApplyVerifiesNewAttempt(t *testing.T) {
	for _, jobID := range []int64{0, 99} {
		t.Run(fmt.Sprint(jobID), func(t *testing.T) {
			s, c, p, run := rerunFixture()
			queued, done := run, run
			queued.Attempt, queued.Status, queued.Conclusion = 2, "queued", ""
			done.Attempt, done.Conclusion = 2, "success"
			// Two reads precede the POST. Its old completed result must not end
			// observation, and the queued attempt must not count as success.
			c.runs[20] = []github.ActionsRun{run, run, run, queued, done}
			passing := p
			passing.Checks = append([]github.PRCheck{}, p.Checks...)
			passing.Checks[0].State = github.CheckPassed
			c.snapshots = []github.PRSnapshot{p, p, p, p, p, passing}
			got, err := s.Run(context.Background(), p.Target, RerunOptions{Apply: true, JobID: jobID})
			want := "run 20"
			if jobID > 0 {
				want = "job 99"
			}
			if err != nil || len(c.posts) != 1 || c.posts[0] != want || !got.ChecksPassing || got.Runs[0].ObservedAttempt != 2 || !got.Runs[0].Requested || got.Runs[0].Conclusion != "success" || c.runReads[20] != 6 {
				t.Fatalf("result=%+v err=%v client=%+v", got, err, c)
			}
		})
	}
}

func TestRerunGroupsJobsByRunAndSelectsDuplicateNamesByID(t *testing.T) {
	for _, jobID := range []int64{0, 100} {
		s, c, p, run := rerunFixture()
		check := p.Checks[0]
		check.ID, check.URL = 43, "https://github.com/o/r/actions/runs/20/job/100"
		p.Checks = append(p.Checks, check)
		c.snapshots = []github.PRSnapshot{p}
		job := c.jobs[99]
		job.ID, job.CheckURL = 100, "https://api.github.com/repos/o/r/check-runs/43"
		c.jobs[100] = job
		got, err := s.Run(context.Background(), p.Target, RerunOptions{JobID: jobID})
		count := 2
		if jobID > 0 {
			count = 1
		}
		if err != nil || len(got.Runs) != 1 || len(got.Runs[0].Jobs) != count || len(c.posts) != 0 {
			t.Fatalf("job=%d result=%+v err=%v", jobID, got, err)
		}
		if jobID > 0 && got.Runs[0].Jobs[0].JobID != jobID {
			t.Fatal("selected by name instead of ID", got)
		}
		// Apply sends one request for this workflow, despite duplicate names.
		c.reads, c.runReads[20] = 0, 0
		done := run
		done.Attempt, done.Conclusion = 2, "success"
		c.runs[20] = []github.ActionsRun{run, run, run, run, done}
		if jobID > 0 {
			c.runs[20] = []github.ActionsRun{run, run, done}
		}
		passing := p
		passing.Checks = append([]github.PRCheck{}, p.Checks...)
		for i := range passing.Checks {
			passing.Checks[i].State = github.CheckPassed
		}
		c.snapshots = []github.PRSnapshot{p, p, p, passing}
		got, err = s.Run(context.Background(), p.Target, RerunOptions{Apply: true, JobID: jobID})
		if err != nil || len(c.posts) != 1 || !got.ChecksPassing {
			t.Fatalf("job=%d result=%+v err=%v posts=%v", jobID, got, err, c.posts)
		}
	}
}

func TestRerunProcessesWorkflowsSeriallyAndPreservesPartialResults(t *testing.T) {
	for _, firstFails := range []bool{false, true} {
		t.Run(fmt.Sprint(firstFails), func(t *testing.T) {
			s, c, p, run := rerunFixture()
			otherCheck := p.Checks[0]
			otherCheck.Name, otherCheck.ID, otherCheck.Suite, otherCheck.Workflow = "build", 43, 6, 7
			otherCheck.URL = "https://github.com/o/r/actions/runs/21/job/100"
			p.Checks = append(p.Checks, otherCheck)
			otherJob := c.jobs[99]
			otherJob.ID, otherJob.RunID, otherJob.Name, otherJob.CheckURL = 100, 21, "build", "https://api.github.com/repos/o/r/check-runs/43"
			c.jobs[100] = otherJob
			otherRun := run
			otherRun.ID, otherRun.SuiteID, otherRun.WorkflowID = 21, 6, 7
			done, otherDone := run, otherRun
			done.Attempt, done.Conclusion = 2, "success"
			otherDone.Attempt, otherDone.Conclusion = 2, "success"
			if firstFails {
				done.Conclusion = "failure"
			}
			c.runs[20] = []github.ActionsRun{run, run, done}
			c.runs[21] = []github.ActionsRun{otherRun, otherRun, otherDone}
			afterFirst, passing := p, p
			afterFirst.Checks, passing.Checks = append([]github.PRCheck{}, p.Checks...), append([]github.PRCheck{}, p.Checks...)
			afterFirst.Checks[0].State = github.CheckPassed
			passing.Checks[0].State, passing.Checks[1].State = github.CheckPassed, github.CheckPassed
			c.snapshots = []github.PRSnapshot{p, p, p, afterFirst, afterFirst, passing}
			got, err := s.Run(context.Background(), p.Target, RerunOptions{Apply: true})
			if len(got.Runs) != 2 || !got.Runs[0].Requested {
				t.Fatalf("result=%+v err=%v", got, err)
			}
			if firstFails {
				if err == nil || len(c.posts) != 1 || got.Runs[1].RequestAttempted || got.Runs[0].Error == "" {
					t.Fatalf("result=%+v err=%v posts=%v", got, err, c.posts)
				}
			} else if err != nil || !got.ChecksPassing || len(c.posts) != 2 || c.posts[0] != "run 20" || c.posts[1] != "run 21" || !got.Runs[1].Requested {
				t.Fatalf("result=%+v err=%v posts=%v", got, err, c.posts)
			}
		})
	}
}

func TestRerunWaitsForCheckRollupAndRejectsLaterAttempts(t *testing.T) {
	for _, concurrent := range []bool{false, true} {
		t.Run(fmt.Sprint(concurrent), func(t *testing.T) {
			s, c, p, run := rerunFixture()
			done := run
			done.Attempt, done.Conclusion = 2, "success"
			c.runs[20] = []github.ActionsRun{run, run, done, done, done}
			passing := p
			passing.Checks = append([]github.PRCheck{}, p.Checks...)
			passing.Checks[0].State = github.CheckPassed
			// The first final read still reports the selected job's old failure.
			c.snapshots = []github.PRSnapshot{p, p, p, p, passing}
			if concurrent {
				next := done
				next.Attempt, next.Status, next.Conclusion = 3, "queued", ""
				c.runs[20][4] = next
			}
			got, err := s.Run(context.Background(), p.Target, RerunOptions{Apply: true})
			if len(c.posts) != 1 || (err != nil) != concurrent || got.ChecksPassing == concurrent {
				t.Fatalf("result=%+v err=%v posts=%v", got, err, c.posts)
			}
			if concurrent && !strings.Contains(got.Error, "changed after verification") {
				t.Fatal("accepted another attempt", got)
			}
		})
	}
}

func TestRerunRefusesUnsafeOrStaleSelections(t *testing.T) {
	for _, kind := range []string{"head guard", "attempt guard", "unknown job", "incomplete checks", "closed", "archived", "read-only", "mismatched job", "mismatched check", "wrong SHA", "wrong run", "wrong attempt", "run active", "wrong workflow", "wrong suite", "wrong event", "PR moves", "job passes", "concurrent rerun", "job read error", "run read error"} {
		t.Run(kind, func(t *testing.T) {
			s, c, p, run := rerunFixture()
			opts := RerunOptions{Apply: true}
			job := c.jobs[99]
			switch kind {
			case "head guard":
				opts.HeadOID = "old"
			case "attempt guard":
				opts.JobID, opts.Attempt = 99, 2
			case "unknown job":
				opts.JobID = 101
			case "incomplete checks":
				p.ChecksComplete = false
			case "closed":
				p.State = "CLOSED"
			case "archived":
				p.Archived = true
			case "read-only":
				p.Permission = "READ"
			case "mismatched job":
				job.ID = 42
			case "mismatched check":
				job.CheckURL = "https://api.github.com/repos/o/r/check-runs/99"
			case "wrong SHA":
				job.HeadSHA = "other"
			case "wrong run":
				job.RunID = 21
			case "wrong attempt":
				run.Attempt = 2
			case "run active":
				run.Status, run.Conclusion = "in_progress", ""
			case "wrong workflow":
				run.WorkflowID++
			case "wrong suite":
				run.SuiteID++
			case "wrong event":
				run.Event = "push"
			case "job read error":
				c.jobErr = errors.New("403")
			case "run read error":
				c.runErr = errors.New("429")
			}
			c.snapshots, c.jobs[99], c.runs[20] = []github.PRSnapshot{p}, job, []github.ActionsRun{run}
			switch kind {
			case "PR moves":
				next := p
				next.HeadOID = "new"
				c.snapshots = []github.PRSnapshot{p, next}
			case "job passes":
				next := p
				next.Checks = append([]github.PRCheck{}, p.Checks...)
				next.Checks[0].State = github.CheckPassed
				c.snapshots = []github.PRSnapshot{p, next}
			case "concurrent rerun":
				next := run
				next.Attempt++
				c.runs[20] = []github.ActionsRun{run, next}
			}
			got, err := s.Run(context.Background(), p.Target, opts)
			if err == nil || got.Error == "" || len(c.posts) != 0 {
				t.Fatalf("result=%+v err=%v posts=%v", got, err, c.posts)
			}
		})
	}
}

func TestRerunNeverAcceptsOldAttemptOrRetriesWrites(t *testing.T) {
	for _, kind := range []string{"old success", "old failure", "new failure", "unexpected attempt", "new pending", "wrong identity", "head moves", "POST error", "PR still failing", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			s, c, p, run := rerunFixture()
			next := run
			next.Attempt, next.Conclusion = 2, "success"
			switch kind {
			case "old success":
				next.Attempt = 1
			case "old failure":
				next = run
			case "new failure":
				next.Conclusion = "failure"
			case "unexpected attempt":
				next.Attempt = 3
			case "new pending":
				next.Status, next.Conclusion = "queued", ""
			case "wrong identity":
				next.HeadSHA = "other"
			case "head moves":
				changed := p
				changed.HeadOID = "new"
				c.snapshots = []github.PRSnapshot{p, p, changed}
			case "POST error":
				c.postErr = errors.New("connection closed after POST")
			}
			c.runs[20] = []github.ActionsRun{run, run, next}
			ctx := context.Background()
			if kind == "cancelled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			got, err := s.Run(ctx, p.Target, RerunOptions{Apply: true})
			wantPosts := 1
			if kind == "cancelled" {
				wantPosts = 0
			}
			if err == nil || got.Error == "" || got.ChecksPassing || len(c.posts) != wantPosts {
				t.Fatalf("result=%+v err=%v posts=%v", got, err, c.posts)
			}
			if kind == "POST error" && (!got.Runs[0].RequestAttempted || got.Runs[0].Requested || !strings.Contains(got.Error, "inspect Actions before retrying")) {
				t.Fatal("lost ambiguous write outcome", got)
			}
		})
	}
}

func TestRerunReportsExternalChecksAndNoEligibleJobs(t *testing.T) {
	s, c, p, _ := rerunFixture()
	p.Checks[0].Status = true
	c.snapshots = []github.PRSnapshot{p}
	got, err := s.Run(context.Background(), p.Target, RerunOptions{})
	if err != nil || len(got.Runs) != 0 || len(got.Warnings) != 1 || len(c.posts) != 0 {
		t.Fatalf("result=%+v err=%v", got, err)
	}
	if _, err := s.Run(context.Background(), p.Target, RerunOptions{Apply: true}); err == nil || len(c.posts) != 0 {
		t.Fatal("applied an external check", err)
	}
	if _, err := s.Run(context.Background(), p.Target, RerunOptions{JobID: 99}); err == nil {
		t.Fatal("selected external job")
	}
}

func TestRerunPreviewAllowsReadOnlyAccess(t *testing.T) {
	s, c, p, _ := rerunFixture()
	p.Permission = "READ"
	c.snapshots = []github.PRSnapshot{p}
	got, err := s.Run(context.Background(), p.Target, RerunOptions{})
	if err != nil || len(got.Runs) != 1 || len(c.posts) != 0 {
		t.Fatalf("preview=%+v err=%v posts=%v", got, err, c.posts)
	}
	if _, err := s.Run(context.Background(), p.Target, RerunOptions{Apply: true}); err == nil || len(c.posts) != 0 {
		t.Fatal("read-only access authorized a rerun", err)
	}
}

func TestDiagnosisSuggestsOnlyIdentifiedRerunCandidates(t *testing.T) {
	for _, kind := range []string{"candidate", "code", "human", "missing identity", "duplicate name", "completed", "unsafe SHA", "incomplete", "closed"} {
		t.Run(kind, func(t *testing.T) {
			_, _, p, _ := rerunFixture()
			evidence := p.Checks[0]
			evidence.JobID, evidence.RunID, evidence.RunAttempt = 99, 20, 1
			action := RerunCandidate
			switch kind {
			case "code":
				action = CodeRepair
			case "human":
				action = HumanAction
			case "missing identity":
				evidence.RunAttempt = 0
			case "duplicate name":
				p.Checks = append(p.Checks, p.Checks[0])
			case "completed":
				p.Checks[0].State = github.CheckPassed
			case "unsafe SHA":
				p.HeadOID = "abc;touch /tmp/file"
			case "incomplete":
				p.ChecksComplete = false
			case "closed":
				p.State = "CLOSED"
			}
			a := Analysis{Recommendations: []Recommendation{{Subject: "test", Action: action}}}
			commands := rerunSuggestions(p, []github.PRCheck{evidence}, a)
			if (len(commands) == 1) != (kind == "candidate") {
				t.Fatalf("commands=%v", commands)
			}
		})
	}
	_, _, p, _ := rerunFixture()
	p.Checks[0].JobID, p.Checks[0].RunID, p.Checks[0].RunAttempt = 99, 20, 1
	s, c, a, _ := service(p)
	c.checks = p.Checks
	a.raw = recommendationAnswer(t, Recommendation{"test", RerunCandidate, "Checkout failed during a network interruption.", "Rerun this job once."})
	got, err := s.Diagnose(context.Background(), p.Target, "")
	if err != nil || got.RepairCommand != "" || len(got.RerunCommands) != 1 || got.Snapshot.Checks[0].RunAttempt != 1 {
		t.Fatalf("result=%+v err=%v", got, err)
	}
	var b strings.Builder
	if err := Markdown(&b, got); err != nil || !strings.Contains(b.String(), "## Suggested CI reruns") || !strings.Contains(b.String(), got.RerunCommands[0]) {
		t.Fatalf("markdown=%s err=%v", &b, err)
	}
	if err := RerunMarkdown(failingWriter{}, RerunResult{}); err == nil {
		t.Fatal("lost writer error")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
