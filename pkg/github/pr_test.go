package github

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cli/go-gh/v2/pkg/api"
)

func TestParsePRTarget(t *testing.T) {
	for _, input := range []string{"o/r#7", "https://github.com/o/r/pull/7", "https://github.com/o/r/pull/7/#discussion_r123"} {
		got, err := ParsePRTarget(input)
		if err != nil || got != (PRTarget{"o", "r", 7}) {
			t.Fatalf("%q: %+v %v", input, got, err)
		}
	}
	for _, input := range []string{"", "7", "o/r#0", "o/r#-1", "o/r#99999999999999999999999", "o/..#7", "../r#7", "o/r.git#7", "https://example.com/o/r/pull/7", "https://github.com:443/o/r/pull/7", "https://user@github.com/o/r/pull/7", "https://github.com/o/r/issues/7", "https://github.com/o/r/pull/7/files", "https://github.com/o/r/pull/7?x=y", "o/r#7;rm -rf /"} {
		if got, err := ParsePRTarget(input); err == nil {
			t.Fatalf("accepted %q: %+v", input, got)
		}
	}
}

func prResponse(rollup string) string {
	return fmt.Sprintf(`{"data":{"repository":{"isArchived":false,"viewerPermission":"WRITE","pullRequest":{"title":"any author","url":"https://github.com/o/r/pull/7","state":"OPEN","isDraft":false,"headRefOid":"head","headRefName":"update","baseRefOid":"base","baseRefName":"main","headRepository":{"nameWithOwner":"o/r"},"mergeable":"MERGEABLE","mergeStateStatus":"CLEAN","reviewDecision":"APPROVED","commits":{"nodes":[{"commit":{"oid":"head","statusCheckRollup":%s}}]}}}}}`, rollup)
}
func snapshotClient(t *testing.T, body string) *prClient {
	t.Helper()
	c, err := newClient(api.ClientOptions{Host: "github.com", AuthToken: "x", Transport: canned(body)})
	if err != nil {
		t.Fatal(err)
	}
	return &prClient{c}
}
func TestTargetedSnapshot(t *testing.T) {
	body := prResponse(`{"contexts":{"totalCount":3,"nodes":[{"__typename":"CheckRun","databaseId":42,"name":"test","status":"COMPLETED","conclusion":"FAILURE","detailsUrl":"https://github.com/o/r/actions/runs/20/job/42","checkSuite":{"databaseId":1,"app":{"databaseId":2},"workflowRun":{"event":"pull_request","workflow":{"databaseId":3}}}},{"__typename":"StatusContext","context":"external","state":"SUCCESS","targetUrl":"https://ci.example/job","description":"passed"},{"__typename":"FutureCheck","name":"future"}]}}`)
	got, err := snapshotClient(t, body).Snapshot(context.Background(), PRTarget{"o", "r", 7})
	if err != nil || !got.ChecksComplete || got.HeadOID != "head" || got.BaseOID != "base" || len(got.Checks) != 3 {
		t.Fatalf("%+v %v", got, err)
	}
	if got.Checks[0].State != CheckFailed || got.Checks[0].ID != 42 || got.Checks[0].App != 2 || got.Checks[0].Workflow != 3 || got.Checks[0].Event != "pull_request" || got.Checks[1].State != CheckPassed || !got.Checks[1].Status || got.Checks[2].State != CheckUnknown {
		t.Fatalf("checks: %+v", got.Checks)
	}
}
func TestSnapshotFailsClosedForMissingData(t *testing.T) {
	for _, body := range []string{
		`{"data":{"repository":null}}`,
		strings.Replace(prResponse("null"), `"isDraft":false,`, "", 1),
		strings.Replace(prResponse("null"), `"isArchived":false,`, "", 1),
		strings.Replace(prResponse("null"), `"oid":"head"`, `"oid":"old"`, 1),
		strings.Replace(prResponse("null"), `,"statusCheckRollup":null`, "", 1),
		prResponse(`{"contexts":{"nodes":[]}}`),
		prResponse(`{"contexts":{"totalCount":1,"nodes":null}}`),
	} {
		if _, err := snapshotClient(t, body).Snapshot(context.Background(), PRTarget{"o", "r", 7}); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
}
func TestSnapshotDistinguishesMissingAndTruncatedChecks(t *testing.T) {
	for _, test := range []struct {
		raw      string
		complete bool
	}{
		{"null", true}, {`{"contexts":{"totalCount":0,"nodes":[]}}`, true},
		{`{"contexts":{"totalCount":101,"nodes":[{"__typename":"StatusContext","context":"x","state":"SUCCESS"}]}}`, false},
	} {
		got, err := snapshotClient(t, prResponse(test.raw)).Snapshot(context.Background(), PRTarget{"o", "r", 7})
		if err != nil || got.ChecksComplete != test.complete {
			t.Fatalf("%+v %v", got, err)
		}
	}
}
func TestFailureEvidence(t *testing.T) {
	rt := &routes{answers: map[string]*http.Response{}, bodies: map[string]string{}}
	rt.answers["GET /repos/o/r/pulls/7"] = reply(200, `{"body":"description"}`)
	rt.answers["GET /repos/o/r/pulls/7 diff"] = reply(200, "diff")
	rt.answers["GET /repos/o/r/check-runs/42/annotations"] = reply(200, `[{"path":"src/app.go","start_line":12,"message":"removed API"}]`)
	rt.answers["GET /repos/o/r/actions/jobs/43/logs"] = reply(200, "compiler failure")
	c := &prClient{restClient(t, rt)}
	p := PRSnapshot{Target: PRTarget{"o", "r", 7}, Checks: []PRCheck{
		{Check: Check{Name: "test", State: CheckFailed}, ID: 42, URL: "https://github.com/o/r/actions/runs/20/job/43"},
		{Check: Check{Name: "external", State: CheckFailed, Status: true}, URL: "https://external.example/log"},
		{Check: Check{Name: "pass", State: CheckPassed}, ID: 99},
	}}
	d, checks, err := c.Evidence(context.Background(), p)
	if err != nil || d.Body != "description" || d.Diff != "diff" || !strings.Contains(checks[0].Log, "src/app.go:12: removed API") || !strings.Contains(checks[0].Log, "compiler failure") || checks[1].EvidenceError == "" || checks[2].Log != "" {
		t.Fatalf("details %+v checks %+v err %v", d, checks, err)
	}
}
