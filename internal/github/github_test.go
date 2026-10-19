package github

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/cli/go-gh/v2/pkg/api"
)

func TestCheckRunState(t *testing.T) {
	for _, c := range []struct {
		status, conclusion string
		want               CheckState
	}{
		{"COMPLETED", "SUCCESS", CheckPassed},
		{"COMPLETED", "NEUTRAL", CheckPassed},
		{"COMPLETED", "SKIPPED", CheckPassed},
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
     "author": {"login": "dependabot"},
     "commits": {"totalCount": 1, "nodes": [{"commit": {
       "oid": "c1", "message": "bump\n\nCo-authored-by: Jane <jane@example.com>", "committedDate": "2026-10-01T12:00:00Z",
       "author": {"name": "dependabot[bot]", "email": "e", "user": {"login": "dependabot[bot]"}},
       "signature": {"isValid": true, "wasSignedByGitHub": true}}}]},
     "last": {"nodes": [{"commit": {"oid": "abc", "statusCheckRollup": {"contexts": {"totalCount": 120, "nodes": [
       {"__typename": "CheckRun", "name": "build", "status": "COMPLETED", "conclusion": "SUCCESS", "checkSuite": {"databaseId": 42}},
       {"__typename": "StatusContext", "context": "ci/legacy", "state": "PENDING"}]}}}}]},
     "comments": {"nodes": [{"body": "@dependabot rebase", "createdAt": "2026-10-01T13:00:00Z", "author": {"login": "me"}}]}}
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
	if r.FullName != "o/r" || !r.CanWrite() || !r.SquashAllowed || r.MergeAllowed || r.OpenPRCount != 3 {
		t.Fatalf("repo = %+v", r)
	}
	if len(r.PullRequests) != 1 {
		t.Fatalf("want only the open PR, got %+v", r.PullRequests)
	}
	pr := r.PullRequests[0]
	if pr.Number != 5 || pr.Author != "dependabot" || pr.HeadOID != "abc" || pr.ChecksOID != "abc" || pr.MergeState != "CLEAN" || pr.CheckCount != 120 {
		t.Fatalf("pr = %+v", pr)
	}
	c := pr.Commits[0]
	if !c.SignedByGitHub || len(c.Authors) != 2 || c.Authors[0].Login != "dependabot[bot]" || c.Authors[1] != (Author{Name: "Jane", Email: "jane@example.com"}) {
		t.Fatalf("commit = %+v", c)
	}
	want := []Check{{Name: "build", State: CheckPassed, Suite: 42}, {Name: "ci/legacy", State: CheckPending}}
	if len(pr.Checks) != 2 || pr.Checks[0] != want[0] || pr.Checks[1] != want[1] {
		t.Fatalf("checks = %+v", pr.Checks)
	}
	if len(pr.Comments) != 1 || pr.Comments[0].Author != "me" {
		t.Fatalf("comments = %+v", pr.Comments)
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

func TestCoAuthors(t *testing.T) {
	msg := "bump\n\nSigned-off-by: dependabot[bot] <support@github.com>\n" +
		"Co-authored-by: Jane Doe <jane@example.com>\n" +
		"  co-authored-by:   dependabot[bot] <49699333+dependabot[bot]@users.noreply.github.com>\r\n" +
		"Co-Authored-By: no email here"
	want := []Author{
		{Name: "Jane Doe", Email: "jane@example.com"},
		{Name: "dependabot[bot]", Email: "49699333+dependabot[bot]@users.noreply.github.com"},
		{Name: "no email here"},
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
