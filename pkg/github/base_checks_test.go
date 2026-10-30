package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cli/go-gh/v2/pkg/api"
)

func baseResponse(oid, rollup string) string {
	return fmt.Sprintf(`{"data":{"repository":{"object":{"oid":%q,"statusCheckRollup":%s}}}}`, oid, rollup)
}

func TestBaseChecksPinnedAndIncomplete(t *testing.T) {
	node := `{"__typename":"CheckRun","databaseId":42,"name":"test","status":"COMPLETED","conclusion":"FAILURE","detailsUrl":"https://github.com/o/r/actions/runs/20/job/43","checkSuite":{"databaseId":1,"app":{"databaseId":2},"workflowRun":{"event":"push","workflow":{"databaseId":3}}}}`
	for _, test := range []struct {
		name, response string
		complete       bool
		count          int
		gap            string
	}{
		{"checks", baseResponse("base", `{"contexts":{"totalCount":1,"nodes":[`+node+`]}}`), true, 1, ""},
		{"truncated", baseResponse("base", `{"contexts":{"totalCount":101,"nodes":[`+node+`]}}`), false, 1, "first 100"},
		{"none", baseResponse("base", "null"), true, 0, ""},
		{"empty", baseResponse("base", `{"contexts":{"totalCount":0,"nodes":[]}}`), true, 0, ""},
		{"wrong commit", baseResponse("new-main", "null"), false, 0, "recorded base commit"},
		{"missing repository", `{"data":{"repository":null}}`, false, 0, "recorded base commit"},
		{"missing commit", `{"data":{"repository":{"object":null}}}`, false, 0, "recorded base commit"},
		{"missing rollup", `{"data":{"repository":{"object":{"oid":"base"}}}}`, false, 0, "missing check rollup"},
		{"missing count", baseResponse("base", `{"contexts":{"nodes":[]}}`), false, 0, "incomplete check rollup"},
		{"missing nodes", baseResponse("base", `{"contexts":{"totalCount":1,"nodes":null}}`), false, 0, "incomplete check rollup"},
		{"access denied", `{"errors":[{"message":"access denied"}],"data":{"repository":null}}`, false, 0, "access denied"},
	} {
		t.Run(test.name, func(t *testing.T) {
			rt := &routes{answers: map[string]*http.Response{"POST /graphql": reply(200, test.response)}, bodies: map[string]string{}}
			client, err := newClient(api.ClientOptions{Host: "github.com", AuthToken: "x", Transport: rt})
			if err != nil {
				t.Fatal(err)
			}
			c := &prClient{client}
			got := c.baseChecks(context.Background(), PRSnapshot{Target: PRTarget{"o", "r", 7}, BaseOID: "base", BaseRef: "main"})
			if got.OID != "base" || got.ChecksComplete != test.complete || got.Checks == nil || len(got.Checks) != test.count || (test.gap == "" && got.EvidenceError != "") || !strings.Contains(got.EvidenceError, test.gap) {
				t.Fatalf("base checks: %+v", got)
			}
			var request struct {
				Query     string
				Variables map[string]any
			}
			if err := json.Unmarshal([]byte(rt.bodies["POST /graphql"]), &request); err != nil {
				t.Fatal(err)
			}
			if request.Variables["oid"] != "base" || request.Variables["owner"] != "o" || request.Variables["name"] != "r" || !strings.Contains(request.Query, "object(oid: $oid)") || strings.Contains(request.Query, "refs/heads/main") {
				t.Fatalf("query not pinned to base: %+v", request)
			}
			if test.count == 1 {
				b := got.Checks[0]
				if b.ID != 42 || b.App != 2 || b.Workflow != 3 || b.Event != "push" || b.State != CheckFailed {
					t.Fatalf("base source identity lost: %+v", b)
				}
			}
		})
	}
}

func TestEvidenceIncludesBaseFailureLogsAndNonfatalGaps(t *testing.T) {
	for _, available := range []bool{true, false} {
		t.Run(fmt.Sprint(available), func(t *testing.T) {
			rt := &routes{answers: map[string]*http.Response{}, bodies: map[string]string{}}
			rt.answers["GET /repos/o/r/pulls/7"] = reply(200, `{"body":"description"}`)
			rt.answers["GET /repos/o/r/pulls/7 diff"] = reply(200, "diff")
			rt.answers["POST /graphql"] = reply(200, baseResponse("base", `{"contexts":{"totalCount":1,"nodes":[{"__typename":"CheckRun","databaseId":42,"name":"test","status":"COMPLETED","conclusion":"FAILURE","detailsUrl":"https://github.com/o/r/actions/runs/20/job/43"}]}}`))
			rt.answers["GET /repos/o/r/check-runs/41/annotations"] = reply(200, `[]`)
			rt.answers["GET /repos/o/r/actions/jobs/40/logs"] = reply(200, "head failure")
			if available {
				rt.answers["GET /repos/o/r/check-runs/42/annotations"] = reply(200, `[{"path":"src/base.go","start_line":12,"message":"base annotation"}]`)
				rt.answers["GET /repos/o/r/actions/jobs/43/logs"] = reply(200, "base failure")
			}
			client, err := newClient(api.ClientOptions{Host: "github.com", AuthToken: "x", Transport: rt})
			if err != nil {
				t.Fatal(err)
			}
			c := &prClient{client}
			p := PRSnapshot{Target: PRTarget{"o", "r", 7}, BaseOID: "base", Checks: []PRCheck{{Check: Check{Name: "test", State: CheckFailed}, ID: 41, URL: "https://github.com/o/r/actions/runs/21/job/40"}}}
			got, err := c.Evidence(context.Background(), p)
			if err != nil || got.Details.Diff != "diff" || got.Checks[0].Log != "head failure" || !got.Base.ChecksComplete || len(got.Base.Checks) != 1 {
				t.Fatalf("evidence: %+v, %v", got, err)
			}
			b := got.Base.Checks[0]
			if available && (!strings.Contains(b.Log, "base failure") || !strings.Contains(b.Log, "src/base.go:12: base annotation")) {
				t.Fatalf("base logs missing: %+v", b)
			}
			if !available && (b.Log != "" || !strings.Contains(b.EvidenceError, "404")) {
				t.Fatalf("expired logs not explicit: %+v", b)
			}
		})
	}
}

func TestFailureEvidenceBounded(t *testing.T) {
	rt := &routes{answers: map[string]*http.Response{}, bodies: map[string]string{}}
	checks := []PRCheck{}
	for i := range 4 {
		id := int64(i + 1)
		checks = append(checks, PRCheck{Check: Check{Name: fmt.Sprint(i), State: CheckFailed}, ID: id, URL: fmt.Sprintf("https://github.com/o/r/actions/runs/1/job/%d", id)})
		rt.answers[fmt.Sprintf("GET /repos/o/r/check-runs/%d/annotations", id)] = reply(200, `[]`)
		rt.answers[fmt.Sprintf("GET /repos/o/r/actions/jobs/%d/logs", id)] = reply(200, strings.Repeat("x", maxEvidenceBytes*2)+"failure at tail")
	}
	c := &prClient{restClient(t, rt)}
	c.failureEvidence(context.Background(), PRTarget{"o", "r", 7}, checks)
	total := 0
	for _, check := range checks {
		if len(check.Log) > maxEvidenceBytes {
			t.Fatalf("excerpt exceeds bound: %d", len(check.Log))
		}
		total += len(check.Log)
	}
	if total > 3*maxEvidenceBytes || !strings.Contains(checks[0].Log, "truncated") || !strings.HasSuffix(checks[0].Log, "failure at tail") || !strings.Contains(checks[3].EvidenceError, "budget exhausted") {
		t.Fatalf("evidence budget not enforced: total %d, last %+v", total, checks[3])
	}
}

func TestEvidenceKeepsHeadWhenBaseUnavailable(t *testing.T) {
	rt := &routes{answers: map[string]*http.Response{
		"GET /repos/o/r/pulls/7":                   reply(200, `{"body":"description"}`),
		"GET /repos/o/r/pulls/7 diff":              reply(200, "diff"),
		"POST /graphql":                            reply(403, `{"message":"access denied"}`),
		"GET /repos/o/r/check-runs/41/annotations": reply(200, `[]`),
		"GET /repos/o/r/actions/jobs/40/logs":      reply(200, "head failure"),
	}, bodies: map[string]string{}}
	client, err := newClient(api.ClientOptions{Host: "github.com", AuthToken: "x", Transport: rt})
	if err != nil {
		t.Fatal(err)
	}
	c := &prClient{client}
	p := PRSnapshot{Target: PRTarget{"o", "r", 7}, BaseOID: "base", Checks: []PRCheck{{Check: Check{Name: "test", State: CheckFailed}, ID: 41, URL: "https://github.com/o/r/actions/runs/21/job/40"}}}
	got, err := c.Evidence(context.Background(), p)
	if err != nil || got.Details.Diff != "diff" || got.Checks[0].Log != "head failure" || got.Base.ChecksComplete || !strings.Contains(got.Base.EvidenceError, "403") {
		t.Fatalf("base error discarded head evidence: %+v, %v", got, err)
	}
}
