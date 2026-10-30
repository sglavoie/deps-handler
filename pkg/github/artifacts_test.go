package github

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/cli/go-gh/v2/pkg/api"
)

type artifactEntry struct {
	name, body string
	mode       os.FileMode
}

func artifactZIP(t *testing.T, entries ...artifactEntry) []byte {
	t.Helper()
	var out bytes.Buffer
	z := zip.NewWriter(&out)
	for _, entry := range entries {
		h := &zip.FileHeader{Name: entry.name, Method: zip.Deflate}
		if entry.mode != 0 {
			h.SetMode(entry.mode)
		}
		w, err := z.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, entry.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

const artifactRunResponse = `{"id":20,"run_attempt":1,"head_sha":"abc123","check_suite_id":4,"workflow_id":5,"event":"pull_request","status":"completed","conclusion":"failure","repository":{"full_name":"o/r"},"run_started_at":"2026-10-30T12:00:00Z","updated_at":"2026-10-30T12:10:00Z"}`
const artifactMetadata = `{"id":51,"name":"browser-report","size_in_bytes":100,"expired":false,"created_at":"2026-10-30T12:05:00Z","updated_at":"2026-10-30T12:06:00Z","workflow_run":{"id":20,"head_sha":"abc123"}}`

type artifactFixture struct {
	t                              *testing.T
	list                           string
	archive                        []byte
	attempt, run, job              string
	listStatus, downloadStatus     int
	listReads, downloads, runReads int
	moveRun                        bool
	unverified                     bool
	secondJob                      bool
}

func newArtifactFixture(t *testing.T) *artifactFixture {
	return &artifactFixture{t: t, list: `{"total_count":1,"artifacts":[` + artifactMetadata + `]}`,
		archive: artifactZIP(t, artifactEntry{name: "test-results/login/error-context.md", body: "Expected dashboard heading to be visible; received login form"},
			artifactEntry{name: "test-results/login/trace.zip", body: "trace"}, artifactEntry{name: "test-results/login/screenshot.png", body: "image"}),
		attempt: artifactRunResponse, run: artifactRunResponse, job: actionsJobResponse, listStatus: 200, downloadStatus: 200}
}

func (f *artifactFixture) RoundTrip(req *http.Request) (*http.Response, error) {
	f.t.Helper()
	var resp *http.Response
	key := req.Method + " " + req.URL.Path
	if req.Header.Get("Accept") == "application/vnd.github.diff" {
		key += " diff"
	}
	switch key {
	case "GET /repos/o/r/pulls/7":
		resp = reply(200, `{"body":"PR description"}`)
	case "GET /repos/o/r/pulls/7 diff":
		resp = reply(200, "diff")
	case "POST /graphql":
		resp = reply(200, baseResponse("base", "null"))
	case "GET /repos/o/r/check-runs/42/annotations", "GET /repos/o/r/check-runs/43/annotations":
		resp = reply(200, `[]`)
	case "GET /repos/o/r/actions/jobs/99/logs", "GET /repos/o/r/actions/jobs/100/logs":
		resp = reply(410, `{"message":"logs expired"}`)
	case "GET /repos/o/r/actions/jobs/99", "GET /repos/o/r/actions/jobs/100":
		body := f.job
		if strings.HasSuffix(req.URL.Path, "/100") {
			body = strings.NewReplacer(`"id":99`, `"id":100`, "check-runs/42", "check-runs/43", `"name":"test"`, `"name":"test two"`).Replace(body)
		}
		if f.unverified {
			body = strings.Replace(body, "abc123", "wrong", 1)
		}
		resp = reply(200, body)
	case "GET /repos/o/r/actions/runs/20":
		f.runReads++
		body := f.run
		if f.moveRun && f.runReads > 1 {
			body = strings.Replace(body, `"run_attempt":1`, `"run_attempt":2`, 1)
		}
		resp = reply(200, body)
	case "GET /repos/o/r/actions/runs/20/attempts/1", "GET /repos/o/r/actions/runs/20/attempts/2":
		resp = reply(200, f.attempt)
	case "GET /repos/o/r/actions/runs/20/artifacts":
		f.listReads++
		if req.URL.RawQuery != "per_page=20" {
			f.t.Fatal("unbounded listing", req.URL)
		}
		resp = reply(f.listStatus, f.list)
	case "GET /repos/o/r/actions/artifacts/51/zip":
		f.downloads++
		resp = reply(f.downloadStatus, string(f.archive))
		resp.Header.Set("Content-Type", "application/zip")
	default:
		f.t.Fatalf("unexpected request %s %s", req.Method, req.URL)
		return nil, nil
	}
	resp.Request = req
	return resp, nil
}

func (f *artifactFixture) evidence() (PREvidence, error) {
	c, err := newClient(api.ClientOptions{Host: "github.com", AuthToken: "x", Transport: f})
	if err != nil {
		return PREvidence{}, err
	}
	p := PRSnapshot{Target: PRTarget{"o", "r", 7}, HeadOID: "abc123", BaseOID: "base", Checks: []PRCheck{{
		Check: Check{Name: "test", State: CheckFailed, Suite: 4, Workflow: 5, Event: "pull_request"}, ID: 42, URL: "https://github.com/o/r/actions/runs/20/job/99"}}}
	if f.secondJob {
		check := p.Checks[0]
		check.Name, check.ID, check.URL = "test two", 43, "https://github.com/o/r/actions/runs/20/job/100"
		p.Checks = append(p.Checks, check)
	}
	return (&prClient{c}).Evidence(context.Background(), p)
}

func TestArtifactEvidenceFillsExpiredLogGap(t *testing.T) {
	f := newArtifactFixture(t)
	got, err := f.evidence()
	if err != nil || len(got.Artifacts) != 1 {
		t.Fatalf("%+v %v", got, err)
	}
	run := got.Artifacts[0]
	if run.EvidenceError != "" || run.RunID != 20 || run.RunAttempt != 1 || run.HeadOID != "abc123" || len(run.Artifacts) != 1 {
		t.Fatalf("lost provenance: %+v", run)
	}
	a := run.Artifacts[0]
	if a.EvidenceError != "" || a.ID != 51 || len(a.Files) != 3 || !strings.Contains(a.Files[0].Content, "received login form") ||
		!strings.Contains(a.Files[1].EvidenceError, "not inspected") || a.Files[1].Content != "" || a.Files[2].Content != "" ||
		!strings.Contains(a.DownloadCommand, "/artifacts/51/zip") || f.listReads != 1 || f.downloads != 1 {
		t.Fatalf("missing artifact evidence: %+v", a)
	}
	if !strings.Contains(got.Checks[0].EvidenceError, "410") || got.Checks[0].Log != "" {
		t.Fatal("lost log gap", got.Checks[0])
	}
}

func TestArtifactGapsStayNonfatalAndDoNotDownloadExcludedEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, old, replacement, gap string
		status                      int
		attempt                     bool
	}{
		{name: "expired", old: `"expired":false`, replacement: `"expired":true`, gap: "expired"},
		{name: "wrong commit", old: "abc123", replacement: "other", gap: "PR commit"},
		{name: "wrong run", old: `"id":20`, replacement: `"id":21`, gap: "verified workflow"},
		{name: "missing run", old: `"workflow_run":{"id":20,"head_sha":"abc123"}`, replacement: `"workflow_run":null`, gap: "verified workflow"},
		{name: "earlier attempt", old: "12:05:00", replacement: "11:05:00", gap: "earlier or ambiguous"},
		{name: "missing timestamps", old: `"created_at":"2026-10-30T12:05:00Z",`, gap: "timestamps"},
		{name: "after completion", old: "12:06:00", replacement: "13:06:00", gap: "timestamps"},
		{name: "oversized", old: `"size_in_bytes":100`, replacement: fmt.Sprintf(`"size_in_bytes":%d`, maxArtifactArchive+1), gap: "8 MiB"},
		{name: "missing list", old: `"artifacts":[` + artifactMetadata + `]`, replacement: `"artifacts":null`, gap: "incomplete"},
		{name: "missing expiry", old: `"expired":false,`, gap: "missing or duplicate"},
		{name: "duplicate ID", old: artifactMetadata, replacement: artifactMetadata + "," + artifactMetadata, gap: "incomplete"},
		{name: "access denied", status: 403, gap: "listing unavailable"},
		{name: "attempt timestamps missing", attempt: true, old: `"run_started_at":"2026-10-30T12:00:00Z",`, gap: "attempt timestamps unavailable"},
		{name: "attempt mismatch", attempt: true, old: `"run_attempt":1`, replacement: `"run_attempt":2`, gap: "attempt identity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newArtifactFixture(t)
			if tc.attempt {
				f.attempt = strings.Replace(f.attempt, tc.old, tc.replacement, 1)
			} else if tc.old != "" {
				f.list = strings.Replace(f.list, tc.old, tc.replacement, 1)
			}
			if tc.status != 0 {
				f.listStatus = tc.status
			}
			got, err := f.evidence()
			data, _ := json.Marshal(got.Artifacts)
			if err != nil || !strings.Contains(string(data), tc.gap) || f.downloads != 0 || got.Details.Diff != "diff" {
				t.Fatalf("evidence=%s err=%v downloads=%d", data, err, f.downloads)
			}
		})
	}
}

func TestArtifactCollectionRejectsUnverifiedAndRacingRuns(t *testing.T) {
	for _, moving := range []bool{false, true} {
		f := newArtifactFixture(t)
		f.unverified, f.moveRun = !moving, moving
		got, err := f.evidence()
		if err != nil {
			t.Fatal(err)
		}
		if !moving {
			if len(got.Artifacts) != 0 || f.listReads != 0 || f.downloads != 0 {
				t.Fatal("read unverified artifacts", got)
			}
		} else if len(got.Artifacts) != 1 || !strings.Contains(got.Artifacts[0].EvidenceError, "discarded") || len(got.Artifacts[0].Artifacts[0].Files) != 0 {
			t.Fatal("accepted racing evidence", got.Artifacts)
		}
	}
}

func TestArtifactsCollectedOnceForMultipleFailedJobsInOneRun(t *testing.T) {
	f := newArtifactFixture(t)
	f.secondJob = true
	got, err := f.evidence()
	if err != nil || len(got.Checks) != 2 || got.Checks[1].JobID != 100 || len(got.Artifacts) != 1 || f.listReads != 1 || f.downloads != 1 {
		t.Fatalf("duplicate or missing workflow evidence: %+v %v; listings=%d downloads=%d", got, err, f.listReads, f.downloads)
	}
}

func TestArtifactsExcludeAmbiguousRerunBoundary(t *testing.T) {
	f := newArtifactFixture(t)
	f.job = strings.Replace(f.job, `"run_attempt":1`, `"run_attempt":2`, 1)
	f.run = strings.Replace(f.run, `"run_attempt":1`, `"run_attempt":2`, 1)
	f.attempt = f.run
	f.list = strings.Replace(f.list, "12:05:00", "12:00:00", 1)
	got, err := f.evidence()
	if err != nil || len(got.Artifacts) != 1 || len(got.Artifacts[0].Artifacts) != 1 || !strings.Contains(got.Artifacts[0].Artifacts[0].EvidenceError, "earlier or ambiguous") || f.downloads != 0 {
		t.Fatalf("accepted ambiguous timestamp: %+v %v", got.Artifacts, err)
	}
}

func TestArtifactArchivesAreBoundedAndNeverExtracted(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []artifactEntry
		gap     string
	}{
		{"traversal", []artifactEntry{{name: "../error-context.md", body: "unsafe"}}, "unsafe"},
		{"absolute", []artifactEntry{{name: "/error-context.md", body: "unsafe"}}, "unsafe"},
		{"windows path", []artifactEntry{{name: `C:\error-context.md`, body: "unsafe"}}, "unsafe"},
		{"duplicate", []artifactEntry{{name: "a.txt"}, {name: "a.txt"}}, "duplicate"},
		{"symlink", []artifactEntry{{name: "a.txt", body: "/etc/passwd", mode: os.ModeSymlink | 0777}}, "unsafe"},
		{"expanded bomb", []artifactEntry{{name: "a.txt", body: strings.Repeat("a", maxArtifactExpanded+1)}}, "expanded size"},
		{"unsupported", []artifactEntry{{name: "index.html", body: "<script>bad</script>"}}, "no supported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files, gap := artifactFiles(context.Background(), artifactZIP(t, tc.entries...), "test-report", newArtifactBudget())
			if len(files) != 0 || !strings.Contains(gap, tc.gap) {
				t.Fatalf("files=%+v gap=%q", files, gap)
			}
		})
	}
	files, gap := artifactFiles(context.Background(), []byte("not zip"), "test", newArtifactBudget())
	if len(files) != 0 || !strings.Contains(gap, "invalid") {
		t.Fatal(files, gap)
	}
	entries := make([]artifactEntry, maxArtifactEntries+1)
	for i := range entries {
		entries[i].name = fmt.Sprintf("%d.txt", i)
	}
	if _, gap := artifactFiles(context.Background(), artifactZIP(t, entries...), "test", newArtifactBudget()); !strings.Contains(gap, "200 entries") {
		t.Fatal(gap)
	}
}

func TestArtifactTextAndGlobalBudgets(t *testing.T) {
	data := artifactZIP(t, artifactEntry{name: "junit.xml", body: `<testsuite><testcase name="login"><failure>Expected heading</failure></testcase></testsuite>`},
		artifactEntry{name: "results.json", body: `{"failed":1}`}, artifactEntry{name: "full.log", body: strings.Repeat("x", maxEvidenceBytes+1)},
		artifactEntry{name: "binary.txt", body: "bad\x00data"})
	files, gap := artifactFiles(context.Background(), data, "reports", newArtifactBudget())
	if gap != "" || len(files) != 4 || !strings.Contains(files[0].Content, "Expected heading") || files[1].Content != `{"failed":1}` ||
		len(files[2].Content) != maxEvidenceBytes || !files[2].Truncated || files[3].Content != "" || files[3].EvidenceError == "" {
		t.Fatalf("%+v %s", files, gap)
	}
	for _, kind := range []string{"text", "files", "expanded"} {
		b := newArtifactBudget()
		switch kind {
		case "text":
			b.text = 0
		case "files":
			b.files = 0
		case "expanded":
			b.expanded = 0
		}
		files, gap := artifactFiles(context.Background(), data, "reports", b)
		encoded, _ := json.Marshal(files)
		if !strings.Contains(string(encoded)+gap, "budget exhausted") {
			t.Fatalf("%s: %s %s", kind, encoded, gap)
		}
	}
}

func TestArtifactListingAndDownloadLimits(t *testing.T) {
	for _, kind := range []string{"truncated", "empty", "duplicate", "download denied", "actual archive too large", "bad ZIP"} {
		t.Run(kind, func(t *testing.T) {
			f := newArtifactFixture(t)
			want := ""
			switch kind {
			case "truncated":
				f.list = strings.Replace(f.list, `"total_count":1`, `"total_count":21`, 1)
				want = "listing truncated"
			case "empty":
				f.list = `{"total_count":0,"artifacts":[]}`
				want = "no workflow artifacts"
			case "duplicate":
				f.list = `{"total_count":2,"artifacts":[` + artifactMetadata + "," + artifactMetadata + `]}`
				want = "duplicate identities"
			case "download denied":
				f.downloadStatus = 403
				want = "HTTP 403"
			case "actual archive too large":
				f.archive = []byte(strings.Repeat("x", maxArtifactArchive+1))
				want = "download limit"
			case "bad ZIP":
				f.archive = []byte("bad ZIP")
				want = "invalid artifact ZIP"
			}
			got, err := f.evidence()
			data, _ := json.Marshal(got.Artifacts)
			if err != nil || !strings.Contains(string(data), want) {
				t.Fatalf("%s %v", data, err)
			}
			if kind == "truncated" && (f.downloads != 1 || len(got.Artifacts[0].Artifacts[0].Files) == 0) {
				t.Fatal("discarded bounded partial listing")
			}
		})
	}
	f := newArtifactFixture(t)
	c := &prClient{restClient(t, f)}
	var run ActionsRun
	if err := json.Unmarshal([]byte(artifactRunResponse), &run); err != nil {
		t.Fatal(err)
	}
	b := newArtifactBudget()
	b.runs = 0
	got := c.runArtifacts(context.Background(), PRTarget{"o", "r", 7}, run, b)
	if !strings.Contains(got.EvidenceError, "workflow budget") || f.listReads != 0 {
		t.Fatal(got)
	}
	b = newArtifactBudget()
	b.download = 1
	a := Artifact{ID: 51, Size: 100}
	c.readArtifact(context.Background(), PRTarget{"o", "r", 7}, &a, b)
	if a.EvidenceError == "" || f.downloads != 0 {
		t.Fatal(a)
	}
}

func TestArtifactRedirectsNeverSendCredentialsOrExposeSignedURLs(t *testing.T) {
	for _, destination := range []string{"https://results.blob.core.windows.net/archive?sig=secret", "http://results.blob.core.windows.net/archive?sig=secret",
		"https://evil.example/archive?sig=secret", "https://api.github.com/other?sig=secret", "https://user:pass@results.blob.core.windows.net/archive?sig=secret"} {
		t.Run(destination, func(t *testing.T) {
			storageReads := 0
			transport := actionsTransport(func(req *http.Request) (*http.Response, error) {
				var resp *http.Response
				if req.URL.Host == "api.github.com" && strings.HasSuffix(req.URL.Path, "/zip") {
					if req.Header.Get("Authorization") == "" {
						t.Fatal("missing API auth")
					}
					resp = reply(302, "")
					resp.Header.Set("Location", destination)
				} else {
					storageReads++
					if req.Header.Get("Authorization") != "" || req.Header.Get("Cookie") != "" {
						t.Fatal("credentials reached storage")
					}
					resp = reply(403, `{"message":"denied"}`)
				}
				resp.Request = req
				return resp, nil
			})
			c := &prClient{restClient(t, transport)}
			a := Artifact{ID: 51, Size: 100}
			c.readArtifact(context.Background(), PRTarget{"o", "r", 7}, &a, newArtifactBudget())
			if strings.Contains(a.EvidenceError, "secret") || strings.Contains(a.EvidenceError, "sig=") || a.EvidenceError == "" {
				t.Fatal("leaked signed URL", a)
			}
			if (storageReads == 1) != (destination == "https://results.blob.core.windows.net/archive?sig=secret") {
				t.Fatal("unsafe redirect followed", storageReads)
			}
		})
	}
}

func TestArtifactBudgetsApplyAcrossArchivesAndCancellation(t *testing.T) {
	b := newArtifactBudget()
	data := artifactZIP(t, artifactEntry{name: "full.log", body: strings.Repeat("x", maxEvidenceBytes)})
	total := 0
	for range 4 {
		files, gap := artifactFiles(context.Background(), data, "logs", b)
		if gap != "" || len(files) != 1 {
			t.Fatal(files, gap)
		}
		total += len(files[0].Content)
	}
	if total != 3*maxEvidenceBytes || b.text != 0 {
		t.Fatal("text budget exceeded", total)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, gap := artifactFiles(ctx, data, "logs", newArtifactBudget()); !strings.Contains(gap, "interrupted") {
		t.Fatal(gap)
	}
}
