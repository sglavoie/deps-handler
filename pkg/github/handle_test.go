package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/cli/go-gh/v2/pkg/api"
)

type handleTransport struct {
	bodies    []string
	queries   []string
	variables []map[string]any
}

func (r *handleTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	data, _ := io.ReadAll(req.Body)
	var payload struct {
		Query     string
		Variables map[string]any
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, err
	}
	r.queries = append(r.queries, payload.Query)
	r.variables = append(r.variables, payload.Variables)
	i := min(len(r.queries)-1, len(r.bodies)-1)
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(r.bodies[i])), Request: req}, nil
}

func handleAPIClient(t *testing.T, bodies ...string) (*prClient, *handleTransport) {
	t.Helper()
	rt := &handleTransport{bodies: bodies}
	c, err := newClient(api.ClientOptions{Host: "github.com", AuthToken: "x", Transport: rt})
	if err != nil {
		t.Fatal(err)
	}
	return &prClient{c}, rt
}

func openPRPage(numbers string, next bool, cursor string) string {
	return fmt.Sprintf(`{"data":{"repository":{"selectionBoundary":{"nodes":[{"number":100}]},"pullRequests":{"nodes":[%s],"pageInfo":{"hasNextPage":%t,"endCursor":%q}}}}}`, numbers, next, cursor)
}

func TestOpenPRsCompletesPaginationInCreatedOrder(t *testing.T) {
	c, rt := handleAPIClient(t, openPRPage(`{"number":2},{"number":9}`, true, "next"), openPRPage(`{"number":14}`, false, ""))
	targets, err := c.OpenPRs(context.Background(), "o", "r")
	if err != nil || fmt.Sprint(targets) != "[o/r#2 o/r#9 o/r#14]" || len(rt.queries) != 2 || rt.variables[1]["after"] != "next" || !strings.Contains(rt.queries[0], "CREATED_AT, direction: ASC") {
		t.Fatalf("%v %v %+v", targets, err, rt)
	}
	c, _ = handleAPIClient(t, openPRPage("", false, ""))
	if targets, err = c.OpenPRs(context.Background(), "o", "r"); err != nil || targets == nil || len(targets) != 0 {
		t.Fatal(targets, err)
	}
}

func TestOpenPRsRejectsPartialAndInvalidSelections(t *testing.T) {
	for _, pages := range [][]string{
		{`{"data":{"repository":null}}`},
		{`{"data":{"repository":{"pullRequests":{"nodes":[],"pageInfo":{}}}}}`},
		{strings.Replace(openPRPage("", false, ""), `"nodes":[]`, `"nodes":null`, 1)},
		{openPRPage(`{"number":0}`, false, "")},
		{openPRPage(`{"number":2}`, true, "")},
		{openPRPage("", true, "next")},
		{openPRPage(`{"number":2}`, true, "next"), openPRPage(`{"number":2}`, false, "")},
		{openPRPage(`{"number":2}`, true, "next"), openPRPage(`{"number":3}`, true, "next")},
		{openPRPage(`{"number":2}`, true, "next"), `{"errors":[{"message":"API unavailable"}]}`},
	} {
		c, _ := handleAPIClient(t, pages...)
		if targets, err := c.OpenPRs(context.Background(), "o", "r"); err == nil || targets != nil {
			t.Fatalf("accepted partial listing: %v %v", targets, err)
		}
	}
}

func TestOpenPRsDoesNotAddPRsOpenedDuringDiscovery(t *testing.T) {
	first := strings.Replace(openPRPage(`{"number":2}`, true, "next"), `"number":100`, `"number":9`, 1)
	c, _ := handleAPIClient(t, first, openPRPage(`{"number":9},{"number":14}`, false, ""))
	if got, err := c.OpenPRs(context.Background(), "o", "r"); err != nil || fmt.Sprint(got) != "[o/r#2 o/r#9]" {
		t.Fatal(got, err)
	}
}

const handleMetadata = `{"data":{"repository":{"mergeCommitAllowed":true,"squashMergeAllowed":true,"rebaseMergeAllowed":false,"pullRequest":{"author":{"login":"contributor"},"updatedAt":"2026-11-03T00:00:00Z","body":"Intent","headRefOid":"head","baseRefOid":"base","comments":{"totalCount":1,"nodes":[{"author":{"login":"maintainer"},"body":"Please preserve old behavior","url":"comment"}]},"reviews":{"totalCount":0,"nodes":[]},"reviewThreads":{"totalCount":1,"nodes":[{"path":"app.go","isResolved":false,"comments":{"totalCount":1,"nodes":[{"author":{"login":"maintainer"},"body":"Empty input?","url":"thread"}]}}]}}}}}`

func TestHandleStateReadsAuthorDiscussionsAndMergeMethods(t *testing.T) {
	c, _ := handleAPIClient(t, prResponse("null"), handleMetadata)
	got, err := c.HandleState(context.Background(), PRTarget{"o", "r", 7})
	if err != nil || got.Author != "contributor" || !got.DiscussionsComplete || len(got.Threads) != 1 || got.Threads[0].IsResolved == nil || *got.Threads[0].IsResolved || len(got.MergeMethods) != 2 || got.MergeMethods[0] != "squash" || got.Comments.Nodes[0].Body != "Please preserve old behavior" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestHandleStateRejectsMissingIdentityAndChangesBetweenReads(t *testing.T) {
	for _, meta := range []string{
		strings.Replace(handleMetadata, `"author":{"login":"contributor"}`, `"author":null`, 1),
		strings.Replace(handleMetadata, `"updatedAt":"2026-11-03T00:00:00Z",`, "", 1),
		strings.Replace(handleMetadata, `"squashMergeAllowed":true,`, "", 1),
		strings.Replace(handleMetadata, `"headRefOid":"head"`, `"headRefOid":"other"`, 1),
		strings.Replace(handleMetadata, `"baseRefOid":"base"`, `"baseRefOid":"other"`, 1),
		`{"data":{"repository":{"pullRequest":null}}}`,
	} {
		c, _ := handleAPIClient(t, prResponse("null"), meta)
		if got, err := c.HandleState(context.Background(), PRTarget{"o", "r", 7}); err == nil {
			t.Fatalf("accepted: %+v", got)
		}
	}
}

func TestHandleStateNeverTreatsIncompleteDiscussionsAsComplete(t *testing.T) {
	for _, meta := range []string{
		strings.Replace(handleMetadata, `"comments":{"totalCount":1`, `"comments":{"totalCount":101`, 1),
		strings.Replace(handleMetadata, `"reviews":{"totalCount":0`, `"reviews":{"totalCount":100`, 1),
		strings.Replace(handleMetadata, `"reviewThreads":{"totalCount":1`, `"reviewThreads":{"totalCount":101`, 1),
		strings.Replace(handleMetadata, `"isResolved":false,`, "", 1),
		strings.Replace(handleMetadata, `"isResolved":false,"comments":{"totalCount":1`, `"isResolved":false,"comments":{"totalCount":101`, 1),
		strings.Replace(handleMetadata, `"reviews":{"totalCount":0,"nodes":[]}`, `"reviews":{"totalCount":0,"nodes":null}`, 1),
	} {
		c, _ := handleAPIClient(t, prResponse("null"), meta)
		if got, err := c.HandleState(context.Background(), PRTarget{"o", "r", 7}); err != nil || got.DiscussionsComplete {
			t.Fatalf("%+v %v", got, err)
		}
	}
}

func TestParseHandleRepository(t *testing.T) {
	for _, value := range []string{"", "repo", "o/r#7", "o/..", "o/r.git", "o/r extra", "https://github.com/o/r", "o/r/x"} {
		if _, _, err := ParseRepository(value); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
	if owner, repo, err := ParseRepository("Org/Project-name"); err != nil || owner != "Org" || repo != "Project-name" {
		t.Fatal(owner, repo, err)
	}
}
