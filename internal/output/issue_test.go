package output

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	gh "github.com/google/go-github/v88/github"
	"github.com/sglavoie/deps-handler/internal/ghfake"
	"github.com/sglavoie/deps-handler/internal/testutil"
	"github.com/sglavoie/deps-handler/pkg/config"
	"github.com/sglavoie/deps-handler/pkg/github"
	"github.com/sglavoie/deps-handler/pkg/policy"
	"github.com/sglavoie/deps-handler/pkg/runner"
)

const login = "dmc[bot]"

func TestHealthIssueOffersLocalDiagnosis(t *testing.T) {
	pr := testutil.PR(7)
	pr.Checks[0].State = github.CheckFailed
	repo := testutil.Repo(pr)
	decisions := policy.Evaluate(repo, config.Builtin, login, nil)
	res := runner.RepoResult{Repo: repo.FullName, Policy: config.Builtin, Decisions: decisions,
		Findings: policy.Diagnose(repo, config.Builtin, decisions)}
	body := Body(res, false)
	if !strings.Contains(body, "Suggested: `deps-handler pr diagnose o/r#7 > repair.md`") || strings.Contains(body, "deps-handler pr fix") {
		t.Fatalf("health issue guidance:\n%s", body)
	}
}

func fake() *ghfake.Issues { return &ghfake.Issues{Repo: "o/r", Login: login} }

func result(prs int) runner.RepoResult {
	p := config.Builtin
	p.Mode, p.DependabotCommands = config.ModeReport, false
	res := runner.RepoResult{Repo: "o/r", Policy: p, Decisions: []policy.Decision{}}
	for n := 1; n <= prs; n++ {
		res.Decisions = append(res.Decisions, policy.Decision{Number: n, Title: "bump lib", Grade: config.UpdatePatch, Action: policy.ActionMerge, Reason: "checks green"})
	}
	return res
}

func TestSyncCreatesUpdatesClosesAndReopens(t *testing.T) {
	f := fake()
	c, ctx := f.Client(t), context.Background()

	// Nothing to report and no issue: nothing is written.
	if n, ch, err := Sync(ctx, c, login, "o", "r", 0, Body(result(0), false), Wanted(result(0)), true); err != nil || n != 0 || ch != Unchanged {
		t.Fatalf("quiet: %d %q %v", n, ch, err)
	}

	n, ch, err := Sync(ctx, c, login, "o", "r", 0, Body(result(1), false), true, true)
	if err != nil || n != 1 || ch != Created || f.Pins() != 1 {
		t.Fatalf("create: %d %q %v, pins %d", n, ch, err, f.Pins())
	}
	if body := f.Get(1).Body; !strings.Contains(body, Marker) || !strings.Contains(body, "| would merge |") {
		t.Fatalf("body:\n%s", body)
	}

	// The same report again: not even an edit.
	f.TakeCalls()
	if _, ch, err := Sync(ctx, c, login, "o", "r", n, Body(result(1), false), true, true); err != nil || ch != Unchanged {
		t.Fatalf("same: %q %v", ch, err)
	}
	for _, call := range f.TakeCalls() {
		if !strings.HasPrefix(call, "GET ") {
			t.Fatalf("unchanged report wrote: %v", call)
		}
	}

	if _, ch, err := Sync(ctx, c, login, "o", "r", n, Body(result(2), false), true, true); err != nil || ch != Updated {
		t.Fatalf("update: %q %v", ch, err)
	}
	if _, ch, err := Sync(ctx, c, login, "o", "r", n, Body(result(0), false), false, true); err != nil || ch != Closed || f.Get(1).State != "closed" {
		t.Fatalf("close: %q %v, state %s", ch, err, f.Get(1).State)
	}
	// Found again by its marker once the recorded number is lost.
	if m, ch, err := Sync(ctx, c, login, "o", "r", 0, Body(result(1), false), true, true); err != nil || m != n || ch != Reopened || f.Len() != 1 {
		t.Fatalf("reopen: %d %q %v, %d issues", m, ch, err, f.Len())
	}
}

func TestSyncLeavesOtherIssuesAlone(t *testing.T) {
	f := fake()
	// Someone copied the marker into an issue of their own, and the App
	// once opened an issue without it.
	f.Add("mallory", Title, Marker+"\nfake", "open")
	f.Add(login, Title, "something else", "open")
	n, ch, err := Sync(context.Background(), f.Client(t), login, "o", "r", 1, Body(result(1), false), true, true)
	if err != nil || ch != Created || n != 3 {
		t.Fatalf("got %d %q %v", n, ch, err)
	}
	if f.Get(1).Body != Marker+"\nfake" || f.Get(2).Body != "something else" {
		t.Fatal("another issue was edited")
	}
}

func TestSyncReusesIssuesFromBeforeRename(t *testing.T) {
	for _, known := range []int{0, 2} {
		t.Run(fmt.Sprintf("known=%d", known), func(t *testing.T) {
			f := fake()
			f.Add("mallory", Title, legacyMarker+"\nfake", "open")
			f.Add(login, Title, legacyMarker+"\nold report", "open")
			body := Body(result(1), false)
			n, change, err := Sync(context.Background(), f.Client(t), login, "o", "r", known, body, true, true)
			if err != nil || n != 2 || change != Updated || f.Len() != 2 {
				t.Fatalf("got %d %q %v, %d issues", n, change, err, f.Len())
			}
			if f.Get(2).Body != body || f.Get(1).Body != legacyMarker+"\nfake" {
				t.Fatal("legacy report was not migrated or another author's issue was edited")
			}
		})
	}
}

func TestBodyEscapesAndIsStable(t *testing.T) {
	res := result(1)
	res.Decisions[0].Title = "bump @octocat | lib"
	a, b := Body(res, false), Body(res, false)
	if a != b {
		t.Fatal("body differs between renders of the same result")
	}
	if strings.Contains(a, "@octocat") || !strings.Contains(a, `\|`) {
		t.Fatalf("body not escaped:\n%s", a)
	}
}

func TestBodyBoundsLargeReports(t *testing.T) {
	for name, text := range map[string]string{
		"ASCII":   strings.Repeat("long reason ", 30_000),
		"Unicode": strings.Repeat("更新が必要です。", 30_000),
	} {
		t.Run(name, func(t *testing.T) {
			res := result(2)
			res.Decisions[0].URL = "https://github.com/o/r/pull/1"
			res.Decisions[1].Reason = text
			body := Body(res, false)
			if len(body) > 60_000 || !utf8.ValidString(body) {
				t.Fatalf("issue body must remain publishable: %d bytes, valid UTF-8: %v", len(body), utf8.ValidString(body))
			}
			if !strings.HasPrefix(body, Marker+"\n") || !strings.Contains(body, "#1 bump lib") {
				t.Fatal("truncation discarded the issue marker or the preceding PR")
			}
			if !strings.Contains(body, "Report truncated") {
				t.Fatal("large report was cut off without telling the reader")
			}
			if body != Body(res, false) {
				t.Fatal("truncated body differs between renders of the same result")
			}
		})
	}
	if body := Body(result(2), false); strings.Contains(body, "Report truncated") {
		t.Fatal("ordinary report was marked as truncated")
	}
}

func TestBodySaysWhetherTheAppMerges(t *testing.T) {
	report := result(1)
	merge := result(1)
	merge.Policy.Mode = config.ModeMerge
	for _, c := range []struct {
		name    string
		res     runner.RepoResult
		merging bool
		want    []string
	}{
		{"report mode", report, false, []string{"report mode", "add `mode: merge`", "| would merge |"}},
		{"report mode on a merging deployment", report, true, []string{"report mode", "| would merge |"}},
		{"merge mode", merge, true, []string{"merges the Dependabot PRs it finds safe", "| merge |"}},
		{"merge mode, merging off", merge, false, []string{"`mode: merge` is set, but merging is paused or not enabled", "| would merge |"}},
	} {
		body := Body(c.res, c.merging)
		for _, w := range c.want {
			if !strings.Contains(body, w) {
				t.Errorf("%s: missing %q in:\n%s", c.name, w, body)
			}
		}
	}
}

func TestSyncDoesNotOpenAnIssueForAFailure(t *testing.T) {
	f := fake()
	c, ctx := f.Client(t), context.Background()
	res := result(0)
	res.Error = "GraphQL: Resource not accessible by integration"
	if n, ch, err := Sync(ctx, c, login, "o", "r", 0, Body(res, false), Wanted(res), false); err != nil || n != 0 || ch != Unchanged || f.Len() != 0 {
		t.Fatalf("got %d %q %v, %d issues", n, ch, err, f.Len())
	}
	// An existing issue does show the failure.
	n, _, _ := Sync(ctx, c, login, "o", "r", 0, Body(result(1), false), true, true)
	if _, ch, err := Sync(ctx, c, login, "o", "r", n, Body(res, false), Wanted(res), false); err != nil || ch != Updated || !strings.Contains(f.Get(1).Body, "Resource not accessible") {
		t.Fatalf("update: %q %v", ch, err)
	}
}

func TestSyncWithIssuesDisabled(t *testing.T) {
	f := fake()
	f.Disable()
	if _, _, err := Sync(context.Background(), f.Client(t), login, "o", "r", 0, Body(result(1), false), true, true); !errors.Is(err, ErrIssuesDisabled) {
		t.Fatalf("err = %v", err)
	}
}

func TestSyncRequiresConfirmedIssueCreation(t *testing.T) {
	valid := `{"number":7,"node_id":"I_7","user":{"login":"dmc[bot]"},"body":"` + Marker + `","state":"open"}`
	for _, tc := range []struct {
		name string
		body string
	}{
		{"null", `null`},
		{"empty object", `{}`},
		{"missing number", strings.Replace(valid, `"number":7,`, "", 1)},
		{"zero number", strings.Replace(valid, `"number":7`, `"number":0`, 1)},
		{"missing author", strings.Replace(valid, `"user":{"login":"dmc[bot]"},`, "", 1)},
		{"wrong author", strings.Replace(valid, `dmc[bot]`, `someone-else`, 1)},
		{"missing state", strings.Replace(valid, `,"state":"open"`, "", 1)},
		{"unknown state", strings.Replace(valid, `"state":"open"`, `"state":"unknown"`, 1)},
		{"missing marker", strings.Replace(valid, Marker, "", 1)},
		{"pull request", strings.Replace(valid, `"number":7`, `"pull_request":{},"number":7`, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			creates, pins := 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet:
					fmt.Fprint(w, "[]")
				case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues":
					creates++
					w.WriteHeader(http.StatusCreated)
					fmt.Fprint(w, tc.body)
				case r.URL.Path == "/graphql":
					pins++
					fmt.Fprint(w, `{"data":{}}`)
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
					http.Error(w, "unexpected request", http.StatusInternalServerError)
				}
			}))
			defer srv.Close()
			base := srv.URL + "/"
			c, err := gh.NewClient(gh.WithHTTPClient(srv.Client()), gh.WithURLs(&base, &base))
			if err != nil {
				t.Fatal(err)
			}
			n, change, err := Sync(context.Background(), c, login, "o", "r", 0, Marker, true, true)
			if err == nil || !strings.Contains(err.Error(), "confirm") || change != Unchanged || n != 0 || creates != 1 || pins != 0 {
				t.Fatalf("unconfirmed creation: number %d, change %q, err %v, creates %d, pins %d", n, change, err, creates, pins)
			}
		})
	}
}

func TestSyncReconcilesUnconfirmedCreationWithoutDuplicatingIssue(t *testing.T) {
	f := fake()
	creates := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues" {
			creates++
			f.Add(login, Title, Marker, "open")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, "{}")
			return
		}
		f.ServeHTTP(w, r)
	}))
	defer srv.Close()
	base := srv.URL + "/"
	c, err := gh.NewClient(gh.WithHTTPClient(srv.Client()), gh.WithURLs(&base, &base))
	if err != nil {
		t.Fatal(err)
	}
	if n, change, err := Sync(context.Background(), c, login, "o", "r", 0, Marker, true, true); err == nil || n != 0 || change != Unchanged || f.Pins() != 0 {
		t.Fatalf("unconfirmed creation = %d, %q, %v; pins %d", n, change, err, f.Pins())
	}
	if n, change, err := Sync(context.Background(), c, login, "o", "r", 0, Marker, true, true); err != nil || n != 1 || change != Unchanged || creates != 1 {
		t.Fatalf("reconciliation = %d, %q, %v; creates %d", n, change, err, creates)
	}
}

func TestSyncSkipsPinWhenCreatedIssueHasNoNodeID(t *testing.T) {
	pins := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet:
			fmt.Fprint(w, "[]")
		case r.URL.Path == "/graphql":
			pins++
			fmt.Fprint(w, `{"data":{}}`)
		default:
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"number":7,"user":{"login":"dmc[bot]"},"body":%q,"state":"open"}`, Marker)
		}
	}))
	defer srv.Close()
	base := srv.URL + "/"
	c, err := gh.NewClient(gh.WithHTTPClient(srv.Client()), gh.WithURLs(&base, &base))
	if err != nil {
		t.Fatal(err)
	}
	if n, change, err := Sync(context.Background(), c, login, "o", "r", 0, Marker, true, true); err != nil || n != 7 || change != Created || pins != 0 {
		t.Fatalf("creation = %d, %q, %v; pins %d", n, change, err, pins)
	}
}

func TestSyncRejectsIncompleteIssueReads(t *testing.T) {
	issue := `{"number":7,"user":{"login":"dmc[bot]"},"body":"` + Marker + `","state":"open"}`
	for _, tc := range []struct {
		name  string
		known int
		body  string
	}{
		{"null list", 0, `null`},
		{"null item", 0, `[null]`},
		{"unidentified item", 0, `[{}]`},
		{"missing number", 0, `[` + strings.Replace(issue, `"number":7,`, "", 1) + `]`},
		{"missing author", 0, `[` + strings.Replace(issue, `"user":{"login":"dmc[bot]"},`, "", 1) + `]`},
		{"missing state", 0, `[` + strings.Replace(issue, `,"state":"open"`, "", 1) + `]`},
		{"unknown state", 0, `[` + strings.Replace(issue, `"open"`, `"unknown"`, 1) + `]`},
		{"null known issue", 7, `null`},
		{"empty known issue", 7, `{}`},
		{"wrong known number", 7, strings.Replace(issue, `"number":7`, `"number":8`, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writes := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method != http.MethodGet {
					writes++
					fmt.Fprint(w, issue)
					return
				}
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			base := srv.URL + "/"
			c, err := gh.NewClient(gh.WithHTTPClient(srv.Client()), gh.WithURLs(&base, &base))
			if err != nil {
				t.Fatal(err)
			}
			n, change, err := Sync(context.Background(), c, login, "o", "r", tc.known, "new report", true, true)
			if err == nil || change != Unchanged || n != tc.known || writes != 0 {
				t.Fatalf("incomplete read: number %d, change %q, err %v, writes %d", n, change, err, writes)
			}
		})
	}
}

func TestSyncIssuePagination(t *testing.T) {
	other := `[{"number":8,"user":{"login":"dmc[bot]"},"body":"another issue","state":"open"}]`
	body := Marker + "\nreport"
	ours := `[{"number":7,"user":{"login":"dmc[bot]"},"body":"` + Marker + `\nreport","state":"open"}]`
	for _, tc := range []struct {
		name   string
		first  string
		link   string
		second string
		bad    bool
	}{
		{"page numbers", other, "page=2", ours, false},
		{"cursors", other, "after=next", ours, false},
		{"null later page", other, "page=2", "null", true},
		{"empty page with continuation", "[]", "page=2", ours, true},
		{"repeated page", other, "page=1", ours, true},
		{"skipped page", other, "page=3", ours, true},
		{"repeated cursor", other, "after=next", other, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads, writes := 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method != http.MethodGet {
					writes++
					http.Error(w, "unexpected write", http.StatusInternalServerError)
					return
				}
				reads++
				if reads > 3 {
					http.Error(w, "pagination did not stop", http.StatusInternalServerError)
					return
				}
				if reads == 1 || tc.name == "repeated cursor" {
					w.Header().Set("Link", "<"+"http://"+r.Host+r.URL.Path+"?"+tc.link+">; rel=\"next\"")
				}
				if reads == 1 {
					fmt.Fprint(w, tc.first)
				} else {
					if !strings.Contains(r.URL.RawQuery, tc.link) {
						t.Errorf("next request lost pagination: %s", r.URL)
					}
					fmt.Fprint(w, tc.second)
				}
			}))
			defer srv.Close()
			base := srv.URL + "/"
			c, err := gh.NewClient(gh.WithHTTPClient(srv.Client()), gh.WithURLs(&base, &base))
			if err != nil {
				t.Fatal(err)
			}
			n, change, err := Sync(context.Background(), c, login, "o", "r", 0, body, true, true)
			if (err != nil) != tc.bad || change != Unchanged || writes != 0 {
				t.Fatalf("number %d, change %q, err %v, writes %d", n, change, err, writes)
			}
			if !tc.bad && (n != 7 || reads != 2) {
				t.Fatalf("did not find the existing health issue: number %d, reads %d", n, reads)
			}
			if tc.name == "repeated cursor" && reads != 2 {
				t.Fatalf("did not stop at the repeated cursor: %d reads", reads)
			}
		})
	}
}
