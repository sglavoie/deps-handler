package github

import (
	"context"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/cli/go-gh/v2/pkg/api"
)

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
		  {"nameWithOwner": "o/zeta", "viewerPermission": "ADMIN", "yml": {"id": "1"}, "yaml": null},
		  {"nameWithOwner": "o/no-config", "viewerPermission": "ADMIN", "yml": null, "yaml": null},
		  {"nameWithOwner": "o/archived", "isArchived": true, "viewerPermission": "ADMIN", "yml": {"id": "2"}},
		  {"nameWithOwner": "o/fork", "isFork": true, "viewerPermission": "ADMIN", "yml": {"id": "3"}}
		]}}}}`,
		`{"data": {"viewer": {"repositories": {"pageInfo": {"hasNextPage": false}, "nodes": [
		  {"nameWithOwner": "O/Alpha", "viewerPermission": "WRITE", "yml": null, "yaml": {"id": "4"}},
		  {"nameWithOwner": "p/read-only", "viewerPermission": "READ", "yml": {"id": "5"}}
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
		  {"nameWithOwner": "me/a", "viewerPermission": "ADMIN", "yml": {"id": "1"}},
		  {"nameWithOwner": "someone/b", "viewerPermission": "WRITE", "yml": {"id": "2"}}
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
	  {"nameWithOwner": "me/a", "viewerPermission": "ADMIN", "yml": {"id": "1"}},
	  {"nameWithOwner": "someone/b", "viewerPermission": "WRITE", "yml": {"id": "2"}}
	]}}}}`}}
	got, err = gqlClient(t, seq).dependabotRepos(context.Background(), []string{"Me"})
	if err != nil || !slices.Equal(got, []string{"me/a"}) {
		t.Fatalf("repos = %q, %v; want only the owner's", got, err)
	}
}

func TestDependabotReposStopsAtThePageCap(t *testing.T) {
	page := `{"data": {"viewer": {"repositories": {"pageInfo": {"hasNextPage": true, "endCursor": "c"}, "nodes": []}}}}`
	seq := &sequence{responses: slices.Repeat([]string{page}, maxDiscoverPages)}
	if _, err := gqlClient(t, seq).dependabotRepos(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "name the owners") {
		t.Fatalf("err = %v", err)
	}
}
