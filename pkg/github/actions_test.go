package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

const actionsJobResponse = `{"id":99,"run_id":20,"run_attempt":1,"head_sha":"abc123","name":"test","status":"completed","conclusion":"failure","check_run_url":"https://api.github.com/repos/o/r/check-runs/42"}`
const actionsRunResponse = `{"id":20,"run_attempt":1,"head_sha":"abc123","check_suite_id":4,"workflow_id":5,"event":"pull_request","status":"completed","conclusion":"failure","repository":{"full_name":"o/r"}}`

func actionsFixture(t *testing.T, job, run string) (*prClient, PRSnapshot) {
	t.Helper()
	rt := &routes{answers: map[string]*http.Response{
		"GET /repos/o/r/actions/jobs/99": reply(200, job),
		"GET /repos/o/r/actions/runs/20": reply(200, run),
	}, bodies: map[string]string{}}
	p := PRSnapshot{Target: PRTarget{"o", "r", 7}, HeadOID: "abc123", Checks: []PRCheck{{
		Check: Check{Name: "test", State: CheckFailed, Suite: 4, Workflow: 5, Event: "pull_request"},
		ID:    42, URL: "https://github.com/o/r/actions/runs/20/job/99",
	}}}
	return &prClient{restClient(t, rt)}, p
}

func TestVerifyActionsJobSeparatesCheckAndJobIDs(t *testing.T) {
	c, p := actionsFixture(t, actionsJobResponse, actionsRunResponse)
	job, run, err := VerifyPRJob(context.Background(), c, p, p.Checks[0])
	if err != nil || job.ID != 99 || run.ID != 20 || run.Attempt != 1 {
		t.Fatalf("job=%+v run=%+v err=%v", job, run, err)
	}
}

func TestActionsRejectsIncompleteAndUnknownResponses(t *testing.T) {
	for _, test := range []struct {
		name, job, run string
	}{
		{"missing job", `{}`, actionsRunResponse},
		{"missing attempt", strings.Replace(actionsJobResponse, `"run_attempt":1,`, "", 1), actionsRunResponse},
		{"wrong check", strings.Replace(actionsJobResponse, "check-runs/42", "check-runs/99", 1), actionsRunResponse},
		{"wrong job head", strings.Replace(actionsJobResponse, "abc123", "other", 1), actionsRunResponse},
		{"unknown job status", strings.Replace(actionsJobResponse, "completed", "future", 1), actionsRunResponse},
		{"unknown conclusion", strings.Replace(actionsJobResponse, "failure", "future", 1), actionsRunResponse},
		{"missing run", actionsJobResponse, `{}`},
		{"wrong repository", actionsJobResponse, strings.Replace(actionsRunResponse, "o/r", "o/other", 1)},
		{"missing repository", actionsJobResponse, strings.Replace(actionsRunResponse, `"full_name":"o/r"`, "", 1)},
		{"wrong run ID", actionsJobResponse, strings.Replace(actionsRunResponse, `"id":20`, `"id":21`, 1)},
		{"missing workflow", actionsJobResponse, strings.Replace(actionsRunResponse, `"workflow_id":5,`, "", 1)},
		{"missing suite", actionsJobResponse, strings.Replace(actionsRunResponse, `"check_suite_id":4,`, "", 1)},
		{"wrong run head", actionsJobResponse, strings.Replace(actionsRunResponse, "abc123", "other", 1)},
		{"run already retried", actionsJobResponse, strings.Replace(actionsRunResponse, `"run_attempt":1`, `"run_attempt":2`, 1)},
		{"unknown run status", actionsJobResponse, strings.Replace(actionsRunResponse, "completed", "future", 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, p := actionsFixture(t, test.job, test.run)
			if _, _, err := VerifyPRJob(context.Background(), c, p, p.Checks[0]); err == nil {
				t.Fatal("accepted incomplete or mismatched identity")
			}
		})
	}
}

func TestActionsJobURLValidation(t *testing.T) {
	for _, raw := range []string{
		"http://github.com/o/r/actions/runs/20/job/99", "https://example.com/o/r/actions/runs/20/job/99",
		"https://user@github.com/o/r/actions/runs/20/job/99", "https://github.com/other/r/actions/runs/20/job/99",
		"https://github.com/o/other/actions/runs/20/job/99", "https://github.com/o/r/actions/runs/0/job/99",
		"https://github.com/o/r/actions/runs/20/job/0", "https://github.com/o/r/actions/runs/20/job/99999999999999999999999",
		"https://github.com/o/r/actions/runs/20/job/99?x=1", "https://github.com/o/r/actions/runs/20/job/99#step:1",
	} {
		check := PRCheck{ID: 42, URL: raw}
		if _, _, ok := ActionsJobIDs(PRTarget{"o", "r", 7}, check); ok {
			t.Fatalf("accepted %q", raw)
		}
	}
	job, run, ok := ActionsJobIDs(PRTarget{"o", "r", 7}, PRCheck{ID: 42, URL: "https://github.com/O/R/actions/runs/20/job/99"})
	if !ok || job != 99 || run != 20 {
		t.Fatal("rejected case variation", job, run, ok)
	}
}

func TestActionsReadsInProgressRunWithoutConclusion(t *testing.T) {
	for _, status := range []string{"queued", "in_progress", "waiting", "pending", "requested"} {
		body := strings.Replace(actionsRunResponse, `"status":"completed","conclusion":"failure"`, `"status":"`+status+`","conclusion":null`, 1)
		c, p := actionsFixture(t, actionsJobResponse, body)
		got, err := c.ActionsRun(context.Background(), p.Target, 20)
		if err != nil || got.Status != status || got.Conclusion != "" {
			t.Fatalf("run=%+v err=%v", got, err)
		}
	}
}

type actionsTransport func(*http.Request) (*http.Response, error)

func (f actionsTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestActionsRerunUsesExactEndpointOnce(t *testing.T) {
	for _, job := range []bool{false, true} {
		for _, status := range []int{201, 200, 403, 500} {
			t.Run(fmt.Sprintf("job=%t/status=%d", job, status), func(t *testing.T) {
				requests := 0
				c := &prClient{restClient(t, actionsTransport(func(req *http.Request) (*http.Response, error) {
					requests++
					want := "/repos/o/r/actions/runs/20/rerun-failed-jobs"
					if job {
						want = "/repos/o/r/actions/jobs/99/rerun"
					}
					if req.Method != "POST" || req.URL.Path != want {
						t.Fatalf("unexpected request %s %s", req.Method, req.URL.Path)
					}
					resp := reply(status, `{}`)
					resp.Request = req
					return resp, nil
				}))}
				var err error
				if job {
					err = c.RerunJob(context.Background(), PRTarget{"o", "r", 7}, 99)
				} else {
					err = c.RerunFailedJobs(context.Background(), PRTarget{"o", "r", 7}, 20)
				}
				if requests != 1 || (err == nil) != (status == 201) {
					t.Fatalf("requests=%d err=%v", requests, err)
				}
			})
		}
	}
}

func TestEvidenceRetainsVerifiedRunAttemptAndExposesIdentityGaps(t *testing.T) {
	for _, available := range []bool{false, true} {
		_, p := actionsFixture(t, actionsJobResponse, actionsRunResponse)
		transport := actionsTransport(func(req *http.Request) (*http.Response, error) {
			body, status := "", 200
			switch req.URL.Path {
			case "/repos/o/r/pulls/7":
				if req.Header.Get("Accept") == "application/vnd.github.diff" {
					body = "diff"
				} else {
					body = `{"body":"description"}`
				}
			case "/repos/o/r/check-runs/42/annotations":
				body = `[]`
			case "/repos/o/r/actions/jobs/99/logs":
				body = "network connection lost"
			case "/repos/o/r/actions/jobs/99":
				body = actionsJobResponse
			case "/repos/o/r/actions/runs/20":
				body = actionsRunResponse
				if !available {
					status, body = 403, `{"message":"forbidden"}`
				}
			default:
				t.Fatalf("unexpected request %s", req.URL)
			}
			resp := reply(status, body)
			resp.Request = req
			return resp, nil
		})
		c := &prClient{restClient(t, transport)}
		evidence, err := c.Evidence(context.Background(), p)
		if err != nil || len(evidence.Checks) != 1 || evidence.Checks[0].Log != "network connection lost" {
			t.Fatalf("evidence=%+v err=%v", evidence, err)
		}
		check := evidence.Checks[0]
		if available {
			data, err := json.Marshal(check)
			if err != nil || check.JobID != 99 || check.RunID != 20 || check.RunAttempt != 1 || !strings.Contains(string(data), `"run_attempt":1`) {
				t.Fatalf("check=%+v JSON=%s err=%v", check, data, err)
			}
		} else if check.JobID != 0 || check.RunID != 0 || check.RunAttempt != 0 || !strings.Contains(check.EvidenceError, "rerun identity") {
			t.Fatal("unverified rerun metadata escaped", check)
		}
	}
}
