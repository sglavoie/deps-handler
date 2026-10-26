package github

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
)

func TestCheckRunState(t *testing.T) {
	for _, c := range []struct {
		status, conclusion string
		want               CheckState
	}{
		{"COMPLETED", "SUCCESS", CheckPassed},
		{"COMPLETED", "NEUTRAL", CheckSkipped},
		{"COMPLETED", "SKIPPED", CheckSkipped},
		{"COMPLETED", "FAILURE", CheckFailed},
		{"COMPLETED", "CANCELLED", CheckFailed},
		{"COMPLETED", "TIMED_OUT", CheckFailed},
		{"COMPLETED", "ACTION_REQUIRED", CheckFailed},
		{"COMPLETED", "STALE", CheckStale},
		{"IN_PROGRESS", "", CheckPending},
		{"QUEUED", "", CheckPending},
	} {
		if got := checkRunState(c.status, c.conclusion); got != c.want {
			t.Errorf("checkRunState(%s, %s) = %s, want %s", c.status, c.conclusion, got, c.want)
		}
	}
}

func TestStatusState(t *testing.T) {
	for state, want := range map[string]CheckState{
		"SUCCESS": CheckPassed, "PENDING": CheckPending, "EXPECTED": CheckPending,
		"FAILURE": CheckFailed, "ERROR": CheckFailed,
	} {
		if got := statusState(state); got != want {
			t.Errorf("statusState(%s) = %s, want %s", state, got, want)
		}
	}
}

func TestRetryable(t *testing.T) {
	for err, want := range map[error]bool{
		&api.HTTPError{StatusCode: 405}:                         true,
		fmt.Errorf("wrap: %w", &api.HTTPError{StatusCode: 409}): true,
		&api.HTTPError{StatusCode: 403}:                         false,
		errors.New("405 not mergeable"):                         false,
	} {
		if got := Retryable(err); got != want {
			t.Errorf("Retryable(%v) = %t, want %t", err, got, want)
		}
	}
}

func TestTransient(t *testing.T) {
	for err, want := range map[error]bool{
		&api.HTTPError{StatusCode: 502}:                         true,
		fmt.Errorf("wrap: %w", &api.HTTPError{StatusCode: 504}): true,
		&api.HTTPError{StatusCode: 404}:                         false,
		&api.GraphQLError{Errors: []api.GraphQLErrorItem{{Message: "Something went wrong while executing your query."}}}: true,
		&api.GraphQLError{Errors: []api.GraphQLErrorItem{{Message: "Could not resolve to a Repository"}}}:                false,
		&net.DNSError{IsTimeout: true}: true,
		errors.New("502"):              false,
		// The connection broke before the whole answer came back.
		&url.Error{Op: "Put", URL: "u", Err: io.EOF}: true,
		&url.Error{Op: "Put", URL: "u", Err: &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}}: true,
		io.ErrUnexpectedEOF: true,
		&url.Error{Op: "Put", URL: "u", Err: context.Canceled}: false,
	} {
		if got := Transient(err); got != want {
			t.Errorf("Transient(%v) = %t, want %t", err, got, want)
		}
	}
}

func TestRefused(t *testing.T) {
	for err, want := range map[error]bool{
		&api.HTTPError{StatusCode: 422}:                         true,
		fmt.Errorf("wrap: %w", &api.HTTPError{StatusCode: 403}): true,
		&api.HTTPError{StatusCode: 504}:                         false,
		&url.Error{Op: "Put", URL: "u", Err: io.EOF}:            false,
		context.Canceled:                                        false,
	} {
		if got := Refused(err); got != want {
			t.Errorf("Refused(%v) = %t, want %t", err, got, want)
		}
	}
}

func TestRateLimited(t *testing.T) {
	soon := strconv.FormatInt(time.Now().Add(30*time.Second).Unix(), 10)
	for name, c := range map[string]struct {
		err  error
		want time.Duration
		ok   bool
	}{
		"retry-after": {&api.HTTPError{StatusCode: 403, Headers: http.Header{"Retry-After": {"7"}}}, 7 * time.Second, true},
		"429":         {&api.HTTPError{StatusCode: 429}, time.Minute, true},
		"secondary":   {&api.HTTPError{StatusCode: 403, Message: "You have exceeded a secondary rate limit"}, time.Minute, true},
		"forbidden":   {&api.HTTPError{StatusCode: 403, Message: "Resource not accessible by integration"}, 0, false},
		// Without its reset, GraphQL's hourly limit is not waited for.
		"graphql": {&api.GraphQLError{Errors: []api.GraphQLErrorItem{{Type: "RATE_LIMITED"}}}, time.Hour, true},
		"other":   {errors.New("boom"), 0, false},
	} {
		if got, ok := RateLimited(c.err); got != c.want || ok != c.ok {
			t.Errorf("%s: RateLimited = %s, %t; want %s, %t", name, got, ok, c.want, c.ok)
		}
	}
	reset := &api.HTTPError{StatusCode: 403, Headers: http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {soon}}}
	if d, ok := RateLimited(reset); !ok || d <= 0 || d > 30*time.Second {
		t.Errorf("reset: RateLimited = %s, %t", d, ok)
	}
	gqlReset := fmt.Errorf("o/r: %w", &RateLimitError{Err: &api.GraphQLError{}, Reset: time.Now().Add(30 * time.Second)})
	if d, ok := RateLimited(gqlReset); !ok || d <= 0 || d > 30*time.Second {
		t.Errorf("graphql reset: RateLimited = %s, %t", d, ok)
	}
}

func TestBackoff(t *testing.T) {
	for name, c := range map[string]struct {
		err  error
		want time.Duration
		ok   bool
	}{
		"transient":  {&api.HTTPError{StatusCode: 502}, time.Second, true},
		"rate limit": {&api.HTTPError{StatusCode: 429, Headers: http.Header{"Retry-After": {"5"}}}, 5 * time.Second, true},
		"too long":   {&api.HTTPError{StatusCode: 429, Headers: http.Header{"Retry-After": {"3600"}}}, time.Hour, false},
		"permanent":  {&api.HTTPError{StatusCode: 404}, time.Second, false},
	} {
		if got, ok := Backoff(c.err, time.Second); got != c.want || ok != c.ok {
			t.Errorf("%s: Backoff = %s, %t; want %s, %t", name, got, ok, c.want, c.ok)
		}
	}
}

func TestAgain(t *testing.T) {
	var slept []time.Duration
	sleep := func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	calls := 0
	read := func(errs ...error) func() (int, error) {
		calls = 0
		return func() (int, error) {
			calls++
			if calls <= len(errs) {
				return 0, errs[calls-1]
			}
			return calls, nil
		}
	}
	ctx := context.Background()
	if v, err := Again(ctx, time.Second, sleep, read(&api.HTTPError{StatusCode: 503})); v != 2 || err != nil || fmt.Sprint(slept) != "[1s]" {
		t.Fatalf("transient: %d, %v, slept %v", v, err, slept)
	}
	if _, err := Again(ctx, time.Second, sleep, read(&api.HTTPError{StatusCode: 503}, &api.HTTPError{StatusCode: 503})); err == nil || calls != 2 {
		t.Fatalf("twice: %v after %d calls", err, calls)
	}
	if _, err := Again(ctx, time.Second, sleep, read(&api.HTTPError{StatusCode: 404})); err == nil || calls != 1 {
		t.Fatalf("permanent: %v after %d calls", err, calls)
	}
}

type canned string

func (c canned) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(string(c))),
	}, nil
}

const fetchResponse = `{"data": {
  "repository": {"nameWithOwner": "o/r", "isArchived": false, "viewerPermission": "WRITE",
    "squashMergeAllowed": true, "mergeCommitAllowed": false, "rebaseMergeAllowed": true},
  "search": {"issueCount": 3, "nodes": [
    {},
    {"number": 7, "state": "CLOSED"},
    {"number": 5, "state": "OPEN", "title": "bump lib", "url": "u", "isDraft": false,
     "createdAt": "2026-10-01T12:00:00Z", "headRefOid": "abc", "baseRefName": "main",
     "mergeable": "MERGEABLE", "mergeStateStatus": "CLEAN", "reviewDecision": null,
     "baseRef": {"target": {"committedDate": "2026-10-01T14:00:00Z"}},
     "author": {"login": "dependabot"},
     "commits": {"totalCount": 1, "nodes": [{"commit": {
       "oid": "c1", "message": "bump\n\nCo-authored-by: Jane <jane@example.com>", "committedDate": "2026-10-01T12:00:00Z",
       "author": {"name": "dependabot[bot]", "email": "e", "user": {"login": "dependabot[bot]"}},
       "committer": {"email": "noreply@github.com"},
       "signature": {"isValid": true, "wasSignedByGitHub": true}}}]},
     "last": {"nodes": [{"commit": {"oid": "abc", "statusCheckRollup": {"contexts": {"totalCount": 120, "nodes": [
       {"__typename": "CheckRun", "name": "build", "status": "COMPLETED", "conclusion": "SUCCESS", "checkSuite": {"databaseId": 42,
         "app": {"databaseId": 15368}, "workflowRun": {"event": "pull_request", "workflow": {"databaseId": 7}}}},
       {"__typename": "CheckRun", "name": "sonar", "status": "COMPLETED", "conclusion": "SUCCESS", "checkSuite": {"databaseId": 43,
         "app": {"databaseId": 99}, "workflowRun": null}},
       {"__typename": "StatusContext", "context": "ci/legacy", "state": "PENDING"}]}}}}]},
     "comments": {"totalCount": 64, "nodes": [{"body": "@dependabot rebase", "createdAt": "2026-10-01T13:00:00Z", "author": {"login": "me"}}]}}
  ]}
}}`

func TestFetchRepoMapsResponse(t *testing.T) {
	gql, err := api.NewGraphQLClient(api.ClientOptions{Host: "github.com", AuthToken: "x", Transport: canned(fetchResponse)})
	if err != nil {
		t.Fatal(err)
	}
	r, err := (&client{gql: gql}).FetchRepo(context.Background(), "o", "r")
	if err != nil {
		t.Fatal(err)
	}
	if r.FullName != "o/r" || !r.CanWrite() || !r.SquashAllowed || r.MergeAllowed || r.OpenPRCount != 3 || r.SearchCount != 3 {
		t.Fatalf("repo = %+v", r)
	}
	if len(r.PullRequests) != 1 {
		t.Fatalf("want only the open PR, got %+v", r.PullRequests)
	}
	pr := r.PullRequests[0]
	if pr.Number != 5 || pr.Author != "dependabot" || pr.HeadOID != "abc" || pr.ChecksOID != "abc" || pr.MergeState != "CLEAN" || pr.CheckCount != 120 ||
		!pr.BaseMovedAt.Equal(time.Date(2026, 10, 1, 14, 0, 0, 0, time.UTC)) {
		t.Fatalf("pr = %+v", pr)
	}
	c := pr.Commits[0]
	if !c.SignedByGitHub || !c.CommittedByGitHub || len(c.Authors) != 2 || c.Authors[0].Login != "dependabot[bot]" || c.Authors[1] != (Author{Name: "Jane", Email: "jane@example.com"}) {
		t.Fatalf("commit = %+v", c)
	}
	want := []Check{
		{Name: "build", State: CheckPassed, Suite: 42, App: 15368, Workflow: 7, Event: "pull_request"},
		{Name: "sonar", State: CheckPassed, Suite: 43, App: 99},
		{Name: "ci/legacy", State: CheckPending, Status: true},
	}
	if len(pr.Checks) != len(want) {
		t.Fatalf("checks = %+v", pr.Checks)
	}
	for i := range want {
		if pr.Checks[i] != want[i] {
			t.Errorf("checks[%d] = %+v, want %+v", i, pr.Checks[i], want[i])
		}
	}
	if len(pr.Comments) != 1 || pr.Comments[0].Author != "me" || pr.CommentCount != 64 {
		t.Fatalf("comments = %+v of %d", pr.Comments, pr.CommentCount)
	}
}

func TestFetchRepoUnsignedCommit(t *testing.T) {
	resp := strings.Replace(fetchResponse, `"signature": {"isValid": true, "wasSignedByGitHub": true}`, `"signature": null`, 1)
	gql, err := api.NewGraphQLClient(api.ClientOptions{Host: "github.com", AuthToken: "x", Transport: canned(resp)})
	if err != nil {
		t.Fatal(err)
	}
	r, err := (&client{gql: gql}).FetchRepo(context.Background(), "o", "r")
	if err != nil {
		t.Fatal(err)
	}
	if r.PullRequests[0].Commits[0].SignedByGitHub {
		t.Fatal("a commit without a signature must not count as signed")
	}
}

func TestFetchRepoCommitWithoutAuthor(t *testing.T) {
	resp := strings.Replace(fetchResponse, `"author": {"name": "dependabot[bot]", "email": "e", "user": {"login": "dependabot[bot]"}}`, `"author": null`, 1)
	gql, err := api.NewGraphQLClient(api.ClientOptions{Host: "github.com", AuthToken: "x", Transport: canned(resp)})
	if err != nil {
		t.Fatal(err)
	}
	r, err := (&client{gql: gql}).FetchRepo(context.Background(), "o", "r")
	if err != nil {
		t.Fatal(err)
	}
	// The trailer must not move into the author's place.
	if a := r.PullRequests[0].Commits[0].Authors; len(a) != 2 || a[0] != (Author{}) || a[1].Name != "Jane" {
		t.Fatalf("authors = %+v", a)
	}
}

func TestCoAuthors(t *testing.T) {
	msg := "bump\n\nSigned-off-by: dependabot[bot] <support@github.com>\n" +
		"Co-authored-by: Jane Doe <jane@example.com>\n" +
		"  co-authored-by:   dependabot[bot] <49699333+dependabot[bot]@users.noreply.github.com>\r\n" +
		"Co-Authored-By: no email here\n" +
		"Co-authored-by : Sam <sam@example.com>"
	want := []Author{
		{Name: "Jane Doe", Email: "jane@example.com"},
		{Name: "dependabot[bot]", Email: "49699333+dependabot[bot]@users.noreply.github.com"},
		{Name: "no email here"},
		{Name: "Sam", Email: "sam@example.com"},
	}
	got := coAuthors(msg)
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestCoAuthorsReadsOnlyTheLastParagraph(t *testing.T) {
	msg := "Bumps lib.\n\nRelease notes:\nCo-authored-by: Upstream Dev <dev@example.com>\n" +
		"\n---\nupdated-dependencies:\n- dependency-name: lib\n...\n" +
		"  \r\nSigned-off-by: dependabot[bot] <support@github.com>\n"
	if got := coAuthors(msg); len(got) != 0 {
		t.Fatalf("got %+v; a trailer outside the last paragraph is not one", got)
	}
}

// recorder answers like canned and keeps the last request body.
type recorder struct {
	canned
	body string
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	b, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	r.body = string(b)
	return r.canned.RoundTrip(req)
}

func TestFetchRepoSearchesOldestFirst(t *testing.T) {
	rec := &recorder{canned: canned(fetchResponse)}
	gql, err := api.NewGraphQLClient(api.ClientOptions{Host: "github.com", AuthToken: "x", Transport: rec})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&client{gql: gql}).FetchRepo(context.Background(), "o", "r"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.body, `repo:o/r is:pr is:open author:app/dependabot sort:created-asc`) {
		t.Fatalf("search not sorted oldest first: %s", rec.body)
	}
}

// routes answers REST requests by "METHOD path", recording each request
// body; a path it does not know gets a 404.
type routes struct {
	answers map[string]*http.Response
	bodies  map[string]string
}

func (r *routes) RoundTrip(req *http.Request) (*http.Response, error) {
	key := req.Method + " " + req.URL.Path
	if req.Header.Get("Accept") == "application/vnd.github.diff" {
		key += " diff"
	}
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		r.bodies[key] = string(b)
	}
	resp, ok := r.answers[key]
	if !ok {
		resp = reply(404, `{"message": "Not Found"}`)
	}
	resp.Request = req
	return resp, nil
}

func reply(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func restClient(t *testing.T, rt http.RoundTripper) *client {
	t.Helper()
	rest, err := api.NewRESTClient(api.ClientOptions{Host: "github.com", AuthToken: "x", Transport: rt})
	if err != nil {
		t.Fatal(err)
	}
	diff, err := api.NewRESTClient(api.ClientOptions{Host: "github.com", AuthToken: "x", Transport: rt,
		Headers: map[string]string{"Accept": "application/vnd.github.diff"}})
	if err != nil {
		t.Fatal(err)
	}
	return &client{rest: rest, diff: diff}
}

func TestMergePinsHeadAndMethod(t *testing.T) {
	rt := &routes{answers: map[string]*http.Response{"PUT /repos/o/r/pulls/5/merge": reply(200, `{"merged": true}`)}, bodies: map[string]string{}}
	if err := restClient(t, rt).Merge(context.Background(), "o", "r", 5, "squash", "abc"); err != nil {
		t.Fatal(err)
	}
	if got := rt.bodies["PUT /repos/o/r/pulls/5/merge"]; got != `{"merge_method":"squash","sha":"abc"}` {
		t.Fatalf("body = %s", got)
	}
	rt.answers["PUT /repos/o/r/pulls/5/merge"] = reply(405, `{"message": "Pull Request is not mergeable"}`)
	if err := restClient(t, rt).Merge(context.Background(), "o", "r", 5, "squash", "abc"); !Retryable(err) {
		t.Fatalf("err = %v; a 405 should come back retryable", err)
	}
	rt.answers["PUT /repos/o/r/pulls/5/merge"] = reply(403, `{"message": "refusing to allow an OAuth App to create or update workflow `+"`.github/workflows/ci.yml` without `workflow` scope"+`"}`)
	err := restClient(t, rt).Merge(context.Background(), "o", "r", 5, "squash", "abc")
	var we *WorkflowScopeError
	if !errors.As(err, &we) || !Refused(err) || !strings.Contains(err.Error(), "gh auth refresh") {
		t.Fatalf("err = %v; a missing workflow scope should come back refused, with how to fix it", err)
	}
}

func TestCommentPostsBody(t *testing.T) {
	rt := &routes{answers: map[string]*http.Response{"POST /repos/o/r/issues/5/comments": reply(201, `{}`)}, bodies: map[string]string{}}
	if err := restClient(t, rt).Comment(context.Background(), "o", "r", 5, "@dependabot rebase"); err != nil {
		t.Fatal(err)
	}
	if got := rt.bodies["POST /repos/o/r/issues/5/comments"]; got != `{"body":"@dependabot rebase"}` {
		t.Fatalf("body = %s", got)
	}
}

func TestPullRequestDetails(t *testing.T) {
	diff := strings.Repeat("a", MaxDiffBytes-1) + "é"
	rt := &routes{answers: map[string]*http.Response{
		"GET /repos/o/r/pulls/5":      reply(200, `{"body": "release notes"}`),
		"GET /repos/o/r/pulls/5 diff": reply(200, diff),
	}, bodies: map[string]string{}}
	d, err := restClient(t, rt).PullRequestDetails(context.Background(), "o", "r", 5)
	if err != nil {
		t.Fatal(err)
	}
	if d.Body != "release notes" || !d.Truncated || d.Diff != strings.Repeat("a", MaxDiffBytes-1) {
		t.Fatalf("details = body %q, truncated %t, diff of %d bytes", d.Body, d.Truncated, len(d.Diff))
	}

	rt.answers["GET /repos/o/r/pulls/5"] = reply(200, `{"body": "notes"}`)
	rt.answers["GET /repos/o/r/pulls/5 diff"] = reply(406, `{"message": "diff too large"}`)
	d, err = restClient(t, rt).PullRequestDetails(context.Background(), "o", "r", 5)
	if err != nil || d.Diff != "" || !strings.Contains(d.DiffError, "406") {
		t.Fatalf("details = %+v, %v; a refused diff is not a failed review", d, err)
	}

	rt.answers["GET /repos/o/r/pulls/5"] = reply(200, `{"body": "notes"}`)
	rt.answers["GET /repos/o/r/pulls/5 diff"] = reply(422, `{"message": "diff too large"}`)
	if d, err = restClient(t, rt).PullRequestDetails(context.Background(), "o", "r", 5); err != nil || !strings.Contains(d.DiffError, "422") {
		t.Fatalf("details = %+v, %v; a 422 is GitHub refusing the diff too", d, err)
	}

	// A diff GitHub failed to send, or would not for a rate limit, must
	// fail the call, for Again to make it once more: a review without the
	// diff would be cached like any other.
	rt.answers["GET /repos/o/r/pulls/5"] = reply(200, `{"body": "notes"}`)
	rt.answers["GET /repos/o/r/pulls/5 diff"] = reply(502, `{"message": "Server Error"}`)
	if d, err = restClient(t, rt).PullRequestDetails(context.Background(), "o", "r", 5); !Transient(err) || !strings.HasPrefix(err.Error(), "o/r#5: ") {
		t.Fatalf("details = %+v, %v; a 502 should come back transient", d, err)
	}
	rt.answers["GET /repos/o/r/pulls/5"] = reply(200, `{"body": "notes"}`)
	limited := reply(403, `{"message": "You have exceeded a secondary rate limit"}`)
	limited.Header.Set("Retry-After", "30")
	rt.answers["GET /repos/o/r/pulls/5 diff"] = limited
	if d, err = restClient(t, rt).PullRequestDetails(context.Background(), "o", "r", 5); err == nil {
		t.Fatalf("details = %+v; a rate-limited diff should fail the call", d)
	}
	if wait, ok := RateLimited(err); !ok || wait != 30*time.Second {
		t.Fatalf("RateLimited = %s, %t for %v", wait, ok, err)
	}
}

// hang answers no request, until the request is cancelled.
type hang struct{}

func (hang) RoundTrip(req *http.Request) (*http.Response, error) {
	<-req.Context().Done()
	return nil, req.Context().Err()
}

func TestRequestTimeoutIsTransient(t *testing.T) {
	// A write that hangs must end, and say it may have gone through.
	opts := api.ClientOptions{Host: "github.com", AuthToken: "x", Transport: hang{}, Timeout: 10 * time.Millisecond}
	rest, err := api.NewRESTClient(opts)
	if err != nil {
		t.Fatal(err)
	}
	gql, err := api.NewGraphQLClient(opts)
	if err != nil {
		t.Fatal(err)
	}
	c := &client{gql: gql, rest: rest, diff: rest}
	if err := c.Merge(context.Background(), "o", "r", 5, "squash", "abc"); !Transient(err) {
		t.Fatalf("merge err = %v; a timeout should be transient", err)
	}
	if _, err := c.FetchRepo(context.Background(), "o", "r"); !Transient(err) {
		t.Fatalf("fetch err = %v; a timeout should be transient", err)
	}
}

func TestPullRequestState(t *testing.T) {
	for body, want := range map[string]PRState{
		`{"data": {"repository": {"pullRequest": {"state": "OPEN", "merged": false}}}}`:   PROpen,
		`{"data": {"repository": {"pullRequest": {"state": "MERGED", "merged": true}}}}`:  PRMerged,
		`{"data": {"repository": {"pullRequest": {"state": "CLOSED", "merged": false}}}}`: PRClosed,
	} {
		gql, err := api.NewGraphQLClient(api.ClientOptions{Host: "github.com", AuthToken: "x", Transport: canned(body)})
		if err != nil {
			t.Fatal(err)
		}
		if got, err := (&client{gql: gql}).PullRequestState(context.Background(), "o", "r", 5); got != want || err != nil {
			t.Errorf("state = %q, %v; want %q", got, err, want)
		}
	}
	gql, err := api.NewGraphQLClient(api.ClientOptions{Host: "github.com", AuthToken: "x", Transport: canned(`{"data": {"repository": {"pullRequest": null}}}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&client{gql: gql}).PullRequestState(context.Background(), "o", "r", 5); err == nil || !strings.HasPrefix(err.Error(), "o/r#5: ") {
		t.Fatalf("err = %v", err)
	}
}

func TestFetchRepoWithoutChecksOrRepository(t *testing.T) {
	resp := strings.Replace(fetchResponse, `"statusCheckRollup": {`, `"statusCheckRollup": null, "x": {`, 1)
	gql, err := api.NewGraphQLClient(api.ClientOptions{Host: "github.com", AuthToken: "x", Transport: canned(resp)})
	if err != nil {
		t.Fatal(err)
	}
	r, err := (&client{gql: gql}).FetchRepo(context.Background(), "o", "r")
	if err != nil {
		t.Fatal(err)
	}
	if pr := r.PullRequests[0]; len(pr.Checks) != 0 || pr.CheckCount != 0 || pr.ChecksOID != "abc" {
		t.Fatalf("pr = %+v; no rollup means no checks yet on abc", pr)
	}

	gql, err = api.NewGraphQLClient(api.ClientOptions{Host: "github.com", AuthToken: "x", Transport: canned(`{"data": {"repository": null, "search": {"issueCount": 0, "nodes": []}}}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&client{gql: gql}).FetchRepo(context.Background(), "o", "gone"); err == nil || !strings.Contains(err.Error(), "not found or not accessible") {
		t.Fatalf("err = %v", err)
	}

	// What GitHub actually answers for a repository it cannot find.
	notFound := `{"data": {"repository": null, "search": {"issueCount": 0, "nodes": []}}, "errors": [{"type": "NOT_FOUND", "path": ["repository"],
	  "message": "Could not resolve to a Repository with the name 'o/gone'."}]}`
	gql, err = api.NewGraphQLClient(api.ClientOptions{Host: "github.com", AuthToken: "x", Transport: canned(notFound)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&client{gql: gql}).FetchRepo(context.Background(), "o", "gone"); err == nil || err.Error() != "o/gone: repository not found or not accessible" {
		t.Fatalf("err = %v", err)
	}
}

const rateLimitedResponse = `{"data": null, "errors": [{"type": "RATE_LIMITED", "message": "API rate limit exceeded for user ID 1."}]}`

func TestFetchRepoRateLimitedSaysWhenItLifts(t *testing.T) {
	limited := func(reset time.Time) *client {
		rt := &routes{answers: map[string]*http.Response{
			"POST /graphql":   reply(200, rateLimitedResponse),
			"GET /rate_limit": reply(200, fmt.Sprintf(`{"resources": {"core": {"reset": 1}, "graphql": {"reset": %d}}}`, reset.Unix())),
		}, bodies: map[string]string{}}
		c := restClient(t, rt)
		gql, err := api.NewGraphQLClient(api.ClientOptions{Host: "github.com", AuthToken: "x", Transport: rt})
		if err != nil {
			t.Fatal(err)
		}
		c.gql = gql
		return c
	}
	ctx := context.Background()
	_, err := limited(time.Now().Add(30*time.Second)).FetchRepo(ctx, "o", "r")
	if d, ok := RateLimited(err); !ok || d <= 0 || d > 30*time.Second {
		t.Fatalf("RateLimited = %s, %t for %v", d, ok, err)
	}
	_, verr := limited(time.Now().Add(30 * time.Second)).Viewer(ctx)
	if d, ok := RateLimited(verr); !ok || d <= 0 || d > 30*time.Second {
		t.Fatalf("Viewer: RateLimited = %s, %t for %v", d, ok, verr)
	}
	if !strings.HasPrefix(err.Error(), "o/r: ") || !strings.Contains(err.Error(), "resets at") {
		t.Fatalf("err = %v", err)
	}

	slept := 0
	sleep := func(context.Context, time.Duration) error { slept++; return nil }
	// Half a minute over, as the reset GitHub gives drops the fraction of
	// a second the wait would otherwise be printed short by.
	c := limited(time.Now().Add(40*time.Minute + 30*time.Second))
	_, err = Again(ctx, time.Second, sleep, func() (*Repo, error) { return c.FetchRepo(ctx, "o", "r") })
	if slept != 0 || err == nil || !strings.Contains(err.Error(), "not waiting up to 40m") {
		t.Fatalf("slept %d times, err = %v; a limit lifting in 40 minutes should fail at once", slept, err)
	}
}

func TestNotWaitingSaysWhenItCannotTell(t *testing.T) {
	err := NotWaiting(&api.GraphQLError{Errors: []api.GraphQLErrorItem{{Type: "RATE_LIMITED", Message: "limited"}}})
	if !strings.Contains(err.Error(), "does not say when") || strings.Contains(err.Error(), "1h") {
		t.Errorf("err = %v; a reset GitHub did not give must not be made up", err)
	}
	plain := errors.New("boom")
	if NotWaiting(plain) != plain {
		t.Error("an error that is no rate limit must be returned as it is")
	}
}

func TestErrorsNameTheirPullRequest(t *testing.T) {
	rt := &routes{answers: map[string]*http.Response{}, bodies: map[string]string{}}
	if _, err := restClient(t, rt).PullRequestDetails(context.Background(), "o", "r", 5); err == nil || !strings.HasPrefix(err.Error(), "o/r#5: ") {
		t.Fatalf("err = %v", err)
	}
}

func TestFetchRepoCommitterNotGitHub(t *testing.T) {
	resp := strings.Replace(fetchResponse, `"committer": {"email": "noreply@github.com"}`, `"committer": {"email": "jane@example.com"}`, 1)
	gql, err := api.NewGraphQLClient(api.ClientOptions{Host: "github.com", AuthToken: "x", Transport: canned(resp)})
	if err != nil {
		t.Fatal(err)
	}
	r, err := (&client{gql: gql}).FetchRepo(context.Background(), "o", "r")
	if err != nil {
		t.Fatal(err)
	}
	if r.PullRequests[0].Commits[0].CommittedByGitHub {
		t.Fatal("a commit someone else committed must not count as GitHub's")
	}
}

// authRecorder answers like canned and keeps the last request's headers.
type authRecorder struct {
	canned
	header http.Header
}

func (r *authRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.header = req.Header.Clone()
	return r.canned.RoundTrip(req)
}

func TestNewWithToken(t *testing.T) {
	// Without gh credentials or config, so the token is all it can use.
	t.Setenv("GH_CONFIG_DIR", t.TempDir())
	for _, v := range []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_HOST"} {
		t.Setenv(v, "")
	}
	if _, err := NewWithToken(""); err == nil {
		t.Error("NewWithToken(\"\") succeeded, want an error")
	}
	rec := &authRecorder{canned: canned(`{"data": {"viewer": {"login": "deps-merger[bot]"}}}`)}
	orig := http.DefaultTransport
	http.DefaultTransport = rec
	defer func() { http.DefaultTransport = orig }()
	c, err := NewWithToken("ghs_install")
	if err != nil {
		t.Fatal(err)
	}
	login, err := c.Viewer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if login != "deps-merger[bot]" {
		t.Errorf("Viewer = %q, want deps-merger[bot]", login)
	}
	if got := rec.header.Get("Authorization"); got != "token ghs_install" {
		t.Errorf("Authorization = %q, want token ghs_install", got)
	}
}
