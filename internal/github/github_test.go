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
       "oid": "c1", "message": "m", "committedDate": "2026-10-01T12:00:00Z",
       "authors": {"totalCount": 7, "nodes": [{"name": "dependabot[bot]", "email": "e", "user": {"login": "dependabot[bot]"}}]},
       "signature": {"isValid": true, "wasSignedByGitHub": true}}}]},
     "last": {"nodes": [{"commit": {"statusCheckRollup": {"contexts": {"totalCount": 120, "nodes": [
       {"__typename": "CheckRun", "name": "build", "status": "COMPLETED", "conclusion": "SUCCESS"},
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
	if pr.Number != 5 || pr.Author != "dependabot" || pr.HeadOID != "abc" || pr.MergeState != "CLEAN" || pr.CheckCount != 120 {
		t.Fatalf("pr = %+v", pr)
	}
	c := pr.Commits[0]
	if !c.SignedByGitHub || c.AuthorCount != 7 || c.Authors[0].Login != "dependabot[bot]" {
		t.Fatalf("commit = %+v", c)
	}
	want := []Check{{Name: "build", State: CheckPassed}, {Name: "ci/legacy", State: CheckPending}}
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
