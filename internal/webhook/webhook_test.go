package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/sglavoie/deps-merger/internal/jobs"
)

var secret = []byte("s3cret")

type recorded struct {
	jobs        []jobs.Job
	installed   []string
	uninstalled []string
	added       []string
	removed     []string
}

func handler(rec *recorded, secret []byte) *Handler {
	return &Handler{
		Secret:  secret,
		Enqueue: func(j jobs.Job) error { rec.jobs = append(rec.jobs, j); return nil },
		Installed: func(_ context.Context, id int64, account string) error {
			rec.installed = append(rec.installed, account)
			return nil
		},
		Uninstalled: func(_ context.Context, id int64, deleted bool) error {
			rec.uninstalled = append(rec.uninstalled, fmt.Sprintf("%d deleted=%t", id, deleted))
			return nil
		},
		RepoAdded: func(_ context.Context, id int64, repo string) error {
			rec.added = append(rec.added, repo)
			return nil
		},
		RepoRemoved: func(_ context.Context, id int64, repo string) error {
			rec.removed = append(rec.removed, repo)
			return nil
		},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func deliver(h http.Handler, kind, body string, key []byte) int {
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", kind)
	if key != nil {
		mac := hmac.New(sha256.New, key)
		mac.Write([]byte(body))
		req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Code
}

const inst = `"installation": {"id": 7, "account": {"login": "acme"}}`
const repo = `"repository": {"full_name": "acme/app", "default_branch": "main"}`

func TestSignatures(t *testing.T) {
	rec := &recorded{}
	body := `{"action": "opened", "pull_request": {"user": {"login": "dependabot[bot]"}}, ` + repo + `, ` + inst + `}`
	if code := deliver(handler(rec, secret), "pull_request", body, nil); code != http.StatusUnauthorized {
		t.Errorf("unsigned: %d", code)
	}
	if code := deliver(handler(rec, secret), "pull_request", body, []byte("wrong")); code != http.StatusUnauthorized {
		t.Errorf("wrongly signed: %d", code)
	}
	// No secret configured: nothing gets through, signed or not.
	if code := deliver(handler(rec, nil), "pull_request", body, nil); code != http.StatusInternalServerError {
		t.Errorf("no secret: %d", code)
	}
	if len(rec.jobs) != 0 {
		t.Fatalf("jobs from rejected deliveries: %+v", rec.jobs)
	}
	if code := deliver(handler(rec, secret), "pull_request", body, secret); code != http.StatusAccepted || len(rec.jobs) != 1 {
		t.Fatalf("signed: %d, jobs %+v", code, rec.jobs)
	}
}

func TestMalformedSubscribedWebhookIsNotAcknowledged(t *testing.T) {
	rec := &recorded{}
	h := handler(rec, secret)
	if code := deliver(h, "installation", "{", secret); code != http.StatusInternalServerError {
		t.Fatalf("malformed installation delivery status = %d", code)
	}
	if code := deliver(h, "star", "{", secret); code != http.StatusNoContent {
		t.Fatalf("unsupported event status = %d", code)
	}
	if code := deliver(h, "", `{}`, secret); code != http.StatusBadRequest {
		t.Fatalf("missing event type status = %d", code)
	}
	if len(rec.jobs) != 0 || len(rec.installed) != 0 {
		t.Fatalf("malformed or unsupported deliveries changed state: %+v", rec)
	}
}

func TestRouting(t *testing.T) {
	for _, c := range []struct {
		name, kind, body string
		want             []string // repos queued; "*" for all of the installation
	}{
		{"dependabot PR opened", "pull_request", `{"action": "opened", "pull_request": {"user": {"login": "dependabot[bot]"}}, ` + repo + `, ` + inst + `}`, []string{"acme/app"}},
		{"dependabot PR merged", "pull_request", `{"action": "closed", "pull_request": {"user": {"login": "dependabot[bot]"}}, ` + repo + `, ` + inst + `}`, []string{"acme/app"}},
		{"human PR", "pull_request", `{"action": "opened", "pull_request": {"user": {"login": "sam"}}, ` + repo + `, ` + inst + `}`, nil},
		{"PR labeled", "pull_request", `{"action": "labeled", "pull_request": {"user": {"login": "dependabot[bot]"}}, ` + repo + `, ` + inst + `}`, nil},
		{"suite on a PR", "check_suite", `{"action": "completed", "check_suite": {"pull_requests": [{"number": 3}]}, ` + repo + `, ` + inst + `}`, []string{"acme/app"}},
		{"suite on a branch", "check_suite", `{"action": "completed", "check_suite": {"pull_requests": []}, ` + repo + `, ` + inst + `}`, nil},
		{"commit status changed", "status", `{"sha": "head", "state": "success", ` + repo + `, ` + inst + `}`, []string{"acme/app"}},
		{"status without installation", "status", `{"sha": "head", "state": "success", ` + repo + `}`, nil},
		{"push to default", "push", `{"ref": "refs/heads/main", ` + repo + `, ` + inst + `}`, []string{"acme/app"}},
		{"push elsewhere", "push", `{"ref": "refs/heads/feature", ` + repo + `, ` + inst + `}`, nil},
		{"installed", "installation", `{"action": "created", ` + inst + `}`, []string{"*"}},
		{"repos added", "installation_repositories", `{"action": "added", "repositories_added": [{"full_name": "acme/a"}, {"full_name": "acme/b"}], ` + inst + `}`, []string{"acme/a", "acme/b"}},
		{"repos removed", "installation_repositories", `{"action": "removed", "repositories_removed": [{"full_name": "acme/a"}], ` + inst + `}`, nil},
		{"unsubscribed event", "star", `{"action": "created", ` + repo + `, ` + inst + `}`, nil},
	} {
		rec := &recorded{}
		code := deliver(handler(rec, secret), c.kind, c.body, secret)
		var got []string
		for _, j := range rec.jobs {
			r := j.Repo
			if r == "" {
				r = "*"
			}
			if j.Installation != 7 || j.Trigger != c.kind {
				t.Errorf("%s: job %+v", c.name, j)
			}
			got = append(got, r)
		}
		if !slices.Equal(got, c.want) || (code != http.StatusAccepted && code != http.StatusNoContent) {
			t.Errorf("%s: %d, queued %v, want %v", c.name, code, got, c.want)
		}
		if c.name == "repos removed" && !slices.Equal(rec.removed, []string{"acme/a"}) {
			t.Errorf("removed repositories = %v", rec.removed)
		}
		if c.name == "repos added" && !slices.Equal(rec.added, []string{"acme/a", "acme/b"}) {
			t.Errorf("added repositories = %v", rec.added)
		}
	}
}

func TestInstallationLifecycle(t *testing.T) {
	rec := &recorded{}
	h := handler(rec, secret)
	deliver(h, "installation", `{"action": "created", `+inst+`}`, secret)
	deliver(h, "installation", `{"action": "suspend", `+inst+`}`, secret)
	deliver(h, "installation", `{"action": "deleted", `+inst+`}`, secret)
	if !slices.Equal(rec.installed, []string{"acme"}) || !slices.Equal(rec.uninstalled, []string{"7 deleted=false", "7 deleted=true"}) {
		t.Fatalf("installed %v, uninstalled %v", rec.installed, rec.uninstalled)
	}
}

func TestPullRequestStateChangesQueueRuns(t *testing.T) {
	for _, event := range []struct {
		kind, action string
	}{
		{"pull_request", "ready_for_review"},
		{"pull_request", "converted_to_draft"},
		{"pull_request", "edited"},
		{"pull_request_review", "submitted"},
		{"pull_request_review", "dismissed"},
	} {
		t.Run(event.kind+"/"+event.action, func(t *testing.T) {
			for _, author := range []string{"dependabot[bot]", "sam"} {
				rec := &recorded{}
				body := `{"action":"` + event.action + `","pull_request":{"user":{"login":"` + author + `"}},` + repo + `,` + inst + `}`
				code := deliver(handler(rec, secret), event.kind, body, secret)
				want := 0
				if author == "dependabot[bot]" {
					want = 1
				}
				if code != http.StatusAccepted || len(rec.jobs) != want {
					t.Fatalf("author %s: status %d, jobs %+v; want %d", author, code, rec.jobs, want)
				}
				if want == 1 && rec.jobs[0] != (jobs.Job{Installation: 7, Repo: "acme/app", Trigger: event.kind}) {
					t.Fatalf("queued job = %+v", rec.jobs[0])
				}
			}
		})
	}
}

func TestInstallationPersistenceFailureRejectsDelivery(t *testing.T) {
	for _, action := range []string{"created", "deleted", "suspend"} {
		t.Run(action, func(t *testing.T) {
			rec := &recorded{}
			h := handler(rec, secret)
			h.Installed = func(context.Context, int64, string) error { return fmt.Errorf("database unavailable") }
			h.Uninstalled = func(context.Context, int64, bool) error { return fmt.Errorf("database unavailable") }
			body := `{"action": "` + action + `", ` + inst + `}`
			if code := deliver(h, "installation", body, secret); code != http.StatusInternalServerError {
				t.Fatalf("status = %d", code)
			}
			if len(rec.jobs) != 0 {
				t.Fatalf("queued after persistence failure: %+v", rec.jobs)
			}
		})
	}
}

func TestRepositoryPersistenceFailureRejectsDelivery(t *testing.T) {
	for _, action := range []string{"added", "removed"} {
		t.Run(action, func(t *testing.T) {
			rec := &recorded{}
			h := handler(rec, secret)
			h.RepoAdded = func(context.Context, int64, string) error { return fmt.Errorf("database unavailable") }
			h.RepoRemoved = func(context.Context, int64, string) error { return fmt.Errorf("database unavailable") }
			body := `{"action": "` + action + `", "repositories_` + action + `": [{"full_name": "acme/a"}], ` + inst + `}`
			if code := deliver(h, "installation_repositories", body, secret); code != http.StatusInternalServerError {
				t.Fatalf("status = %d", code)
			}
			if len(rec.jobs) != 0 {
				t.Fatalf("queued after persistence failure: %+v", rec.jobs)
			}
		})
	}
}

func TestQueuePersistenceFailureRejectsDelivery(t *testing.T) {
	rec := &recorded{}
	h := handler(rec, secret)
	h.Enqueue = func(jobs.Job) error { return fmt.Errorf("database unavailable") }
	body := `{"action": "opened", "pull_request": {"user": {"login": "dependabot[bot]"}}, ` + repo + `, ` + inst + `}`
	if code := deliver(h, "pull_request", body, secret); code != http.StatusInternalServerError {
		t.Fatalf("status = %d", code)
	}
}
