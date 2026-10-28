package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/cli/go-gh/v2/pkg/api"
)

func TestDependabotReposRejectsIncompleteEntries(t *testing.T) {
	valid := `{"nameWithOwner":"o/a","isArchived":false,"isFork":false,"viewerPermission":"WRITE","yml":{"__typename":"Blob","id":"1"},"yaml":null}`
	invalid := map[string]string{"null node": "null", "empty node": "{}"}
	for _, field := range []string{"nameWithOwner", "isArchived", "isFork", "viewerPermission", "yml", "yaml"} {
		var node map[string]any
		if err := json.Unmarshal([]byte(valid), &node); err != nil {
			t.Fatal(err)
		}
		delete(node, field)
		data, _ := json.Marshal(node)
		invalid["missing "+field] = string(data)
		if field == "nameWithOwner" || field == "isArchived" || field == "isFork" {
			node[field] = nil
			data, _ = json.Marshal(node)
			invalid["null "+field] = string(data)
		}
	}
	invalid["empty name"] = strings.Replace(valid, `"o/a"`, `""`, 1)
	invalid["missing owner"] = strings.Replace(valid, `"o/a"`, `"/a"`, 1)
	invalid["missing repo"] = strings.Replace(valid, `"o/a"`, `"o/"`, 1)
	invalid["unidentified config"] = strings.Replace(valid, `{"__typename":"Blob","id":"1"}`, `{}`, 1)
	invalid["missing config type"] = strings.Replace(valid, `"__typename":"Blob",`, ``, 1)
	for label, node := range invalid {
		for _, root := range []string{"viewer", "repositoryOwner"} {
			t.Run(root+"/"+label, func(t *testing.T) {
				var owners []string
				if root == "repositoryOwner" {
					owners = []string{"o"}
				}
				seq := &sequence{responses: []string{
					fmt.Sprintf(`{"data":{%q:{"repositories":{"pageInfo":{"hasNextPage":true,"endCursor":"c1"},"nodes":[%s]}}}}`, root, valid),
					fmt.Sprintf(`{"data":{%q:{"repositories":{"pageInfo":{"hasNextPage":false},"nodes":[%s]}}}}`, root, node),
				}}
				got, err := gqlClient(t, seq).dependabotRepos(context.Background(), owners)
				if err == nil || got != nil {
					t.Fatalf("repos = %q, err = %v; incomplete entry must discard the entire listing", got, err)
				}
			})
		}
	}
}

func TestDependabotReposRequiresConfigBlob(t *testing.T) {
	seq := &sequence{responses: []string{`{"data":{"viewer":{"repositories":{"pageInfo":{"hasNextPage":false},"nodes":[
	  {"nameWithOwner":"o/tree","isArchived":false,"isFork":false,"viewerPermission":"WRITE","yml":{"__typename":"Tree","id":"1"},"yaml":null},
	  {"nameWithOwner":"o/blob","isArchived":false,"isFork":false,"viewerPermission":"WRITE","yml":{"__typename":"Tree","id":"2"},"yaml":{"__typename":"Blob","id":"3"}}
	]}}}}`}}
	got, err := gqlClient(t, seq).dependabotRepos(context.Background(), nil)
	if err != nil || !slices.Equal(got, []string{"o/blob"}) {
		t.Fatalf("repos = %q, err = %v; only a Blob config should qualify", got, err)
	}
}

func TestDependabotReposAcceptsExplicitNullPermissionAndConfigs(t *testing.T) {
	for _, node := range []string{
		`{"nameWithOwner":"o/a","isArchived":false,"isFork":false,"viewerPermission":null,"yml":{"__typename":"Blob","id":"1"},"yaml":null}`,
		`{"nameWithOwner":"o/a","isArchived":false,"isFork":false,"viewerPermission":"WRITE","yml":null,"yaml":null}`,
	} {
		seq := &sequence{responses: []string{
			fmt.Sprintf(`{"data":{"viewer":{"repositories":{"pageInfo":{"hasNextPage":false},"nodes":[%s]}}}}`, node),
		}}
		got, err := gqlClient(t, seq).dependabotRepos(context.Background(), nil)
		if err != nil || len(got) != 0 {
			t.Fatalf("repos = %q, err = %v; explicitly ineligible repository should be filtered", got, err)
		}
	}
}

// sequence answers each GraphQL request with the next of its responses,
// recording the request bodies.
type sequence struct {
	responses []string
	bodies    []string
}

func (s *sequence) RoundTrip(req *http.Request) (*http.Response, error) {
	b, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	s.bodies = append(s.bodies, string(b))
	resp := canned(s.responses[0])
	s.responses = s.responses[1:]
	return resp.RoundTrip(req)
}

func gqlClient(t *testing.T, rt http.RoundTripper) *client {
	t.Helper()
	gql, err := api.NewGraphQLClient(api.ClientOptions{Host: "github.com", AuthToken: "x", Transport: rt})
	if err != nil {
		t.Fatal(err)
	}
	return &client{gql: gql}
}

func TestDependabotReposFollowsPagesAndFilters(t *testing.T) {
	seq := &sequence{responses: []string{
		`{"data": {"viewer": {"repositories": {"pageInfo": {"hasNextPage": true, "endCursor": "c1"}, "nodes": [
		  {"nameWithOwner":"o/zeta","viewerPermission":"ADMIN","yml":{"__typename":"Blob","id":"1"},"yaml":null,"isArchived":false,"isFork":false},
		  {"nameWithOwner":"o/no-config","viewerPermission":"ADMIN","yml":null,"yaml":null,"isArchived":false,"isFork":false},
		  {"nameWithOwner":"o/archived","isArchived":true,"viewerPermission":"ADMIN","yml":{"__typename":"Blob","id":"2"},"isFork":false,"yaml":null},
		  {"nameWithOwner":"o/fork","isFork":true,"viewerPermission":"ADMIN","yml":{"__typename":"Blob","id":"3"},"isArchived":false,"yaml":null}
		]}}}}`,
		`{"data": {"viewer": {"repositories": {"pageInfo": {"hasNextPage": false}, "nodes": [
		  {"nameWithOwner":"O/Alpha","viewerPermission":"WRITE","yml":null,"yaml":{"__typename":"Blob","id":"4"},"isArchived":false,"isFork":false},
		  {"nameWithOwner":"p/read-only","viewerPermission":"READ","yml":{"__typename":"Blob","id":"5"},"isArchived":false,"isFork":false,"yaml":null}
		]}}}}`,
	}}
	got, err := gqlClient(t, seq).dependabotRepos(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"O/Alpha", "o/zeta"}; !slices.Equal(got, want) {
		t.Fatalf("repos = %q, want %q", got, want)
	}
	if len(seq.bodies) != 2 || !strings.Contains(seq.bodies[1], `"after":"c1"`) {
		t.Fatalf("second page not asked from the cursor: %q", seq.bodies)
	}
}

func TestDependabotReposOfOwners(t *testing.T) {
	seq := &sequence{responses: []string{
		// A user's listing holds the repositories they collaborate on too.
		`{"data": {"repositoryOwner": {"repositories": {"pageInfo": {"hasNextPage": false}, "nodes": [
		  {"nameWithOwner":"me/a","viewerPermission":"ADMIN","yml":{"__typename":"Blob","id":"1"},"isArchived":false,"isFork":false,"yaml":null},
		  {"nameWithOwner":"someone/b","viewerPermission":"WRITE","yml":{"__typename":"Blob","id":"2"},"isArchived":false,"isFork":false,"yaml":null}
		]}}}}`,
		`{"data": {"repositoryOwner": null}}`,
	}}
	got, err := gqlClient(t, seq).dependabotRepos(context.Background(), []string{"Me", "ghost"})
	if err == nil || err.Error() != "ghost: no such user or organisation" {
		t.Fatalf("unknown owner: %q, %v", got, err)
	}
	if !strings.Contains(seq.bodies[0], `"owner":"Me"`) {
		t.Fatalf("owner not asked for: %s", seq.bodies[0])
	}

	seq = &sequence{responses: []string{`{"data": {"repositoryOwner": {"repositories": {"pageInfo": {"hasNextPage": false}, "nodes": [
	  {"nameWithOwner":"me/a","viewerPermission":"ADMIN","yml":{"__typename":"Blob","id":"1"},"isArchived":false,"isFork":false,"yaml":null},
	  {"nameWithOwner":"someone/b","viewerPermission":"WRITE","yml":{"__typename":"Blob","id":"2"},"isArchived":false,"isFork":false,"yaml":null}
	]}}}}`}}
	got, err = gqlClient(t, seq).dependabotRepos(context.Background(), []string{"Me"})
	if err != nil || !slices.Equal(got, []string{"me/a"}) {
		t.Fatalf("repos = %q, %v; want only the owner's", got, err)
	}
}

func TestDependabotReposStopsAtThePageCap(t *testing.T) {
	seq := &sequence{}
	for i := range maxDiscoverPages {
		seq.responses = append(seq.responses, fmt.Sprintf(`{"data": {"viewer": {"repositories": {"pageInfo": {"hasNextPage": true, "endCursor": "c%d"}, "nodes": []}}}}`, i))
	}
	if _, err := gqlClient(t, seq).dependabotRepos(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "name the owners") {
		t.Fatalf("err = %v", err)
	}
}

func TestDependabotReposRejectsIncompletePages(t *testing.T) {
	for _, page := range []string{
		`null`,
		`{}`,
		`{"nodes": []}`,
		`{"pageInfo": null, "nodes": []}`,
		`{"pageInfo": {}, "nodes": []}`,
		`{"pageInfo": {"hasNextPage": null}, "nodes": []}`,
		`{"pageInfo": {"hasNextPage": false}}`,
		`{"pageInfo": {"hasNextPage": false}, "nodes": null}`,
	} {
		for _, owner := range []string{"", "o"} {
			t.Run(owner+page, func(t *testing.T) {
				var owners []string
				root := "viewer"
				if owner != "" {
					owners, root = []string{owner}, "repositoryOwner"
				}
				seq := &sequence{responses: []string{
					fmt.Sprintf(`{"data": {%q: {"repositories": {"pageInfo":{"hasNextPage":true,"endCursor":"c1"},"nodes":[{"nameWithOwner":"o/a","viewerPermission":"WRITE","yml":{"__typename":"Blob","id":"1"},"isArchived":false,"isFork":false,"yaml":null}]}}}}`, root),
					fmt.Sprintf(`{"data": {%q: {"repositories": %s}}}`, root, page),
				}}
				got, err := gqlClient(t, seq).dependabotRepos(context.Background(), owners)
				if err == nil || got != nil || !strings.Contains(err.Error(), "incomplete repository listing") {
					t.Fatalf("repos = %q, err = %v; incomplete discovery must fail", got, err)
				}
			})
		}
	}
}

func TestDependabotReposAcceptsExplicitEmptyPage(t *testing.T) {
	for _, owners := range [][]string{nil, {"o"}} {
		root := "viewer"
		if len(owners) > 0 {
			root = "repositoryOwner"
		}
		seq := &sequence{responses: []string{
			fmt.Sprintf(`{"data": {%q: {"repositories": {"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[]}}}}`, root),
		}}
		got, err := gqlClient(t, seq).dependabotRepos(context.Background(), owners)
		if err != nil || len(got) != 0 {
			t.Fatalf("owners = %q: repos = %q, err = %v", owners, got, err)
		}
	}
}

func TestDependabotReposRejectsInvalidCursors(t *testing.T) {
	for _, cursor := range []string{`null`, `""`, `"c1"`} {
		t.Run(cursor, func(t *testing.T) {
			seq := &sequence{responses: []string{
				`{"data":{"viewer":{"repositories":{"pageInfo":{"hasNextPage":true,"endCursor":"c1"},"nodes":[{"nameWithOwner":"o/a","viewerPermission":"WRITE","yml":{"__typename":"Blob","id":"1"},"isArchived":false,"isFork":false,"yaml":null}]}}}}`,
				fmt.Sprintf(`{"data":{"viewer":{"repositories":{"pageInfo":{"hasNextPage":true,"endCursor":%s},"nodes":[]}}}}`, cursor),
			}}
			// Extra responses keep the unfixed implementation bounded by its page cap.
			seq.responses = append(seq.responses, slices.Repeat(seq.responses[1:], maxDiscoverPages)...)
			got, err := gqlClient(t, seq).dependabotRepos(context.Background(), nil)
			if err == nil || got != nil || !strings.Contains(err.Error(), "cursor") || len(seq.bodies) != 2 {
				t.Fatalf("repos = %q, err = %v, requests = %d; invalid pagination must stop immediately", got, err, len(seq.bodies))
			}
		})
	}
}
