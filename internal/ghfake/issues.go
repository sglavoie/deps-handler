// Package ghfake serves the GitHub issue endpoints the App uses, for one
// repository, for tests.
package ghfake

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	gh "github.com/google/go-github/v88/github"
)

// Issue is an issue as the fake keeps it.
type Issue struct {
	Number int    `json:"number"`
	NodeID string `json:"node_id"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	State  string `json:"state"`
	User   struct {
		Login string `json:"login"`
	} `json:"user"`
}

// Issues serves the issues of repository Repo. Issues created through it
// are authored by Login.
type Issues struct {
	Repo  string
	Login string

	mu       sync.Mutex
	issues   []*Issue
	calls    []string
	pins     int
	disabled bool
}

// Add adds an issue, as author, and returns it.
func (f *Issues) Add(author, title, body, state string) *Issue {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.add(author, title, body, state)
}

func (f *Issues) add(author, title, body, state string) *Issue {
	i := &Issue{Number: len(f.issues) + 1, Title: title, Body: body, State: state}
	i.NodeID = fmt.Sprintf("I_%d", i.Number)
	i.User.Login = author
	f.issues = append(f.issues, i)
	return i
}

// Get returns a copy of issue n.
func (f *Issues) Get(n int) Issue {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *f.issues[n-1]
}

// Len counts the issues.
func (f *Issues) Len() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.issues)
}

// Pins counts the pinIssue mutations received.
func (f *Issues) Pins() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pins
}

// TakeCalls returns the requests made since the last call, as "METHOD path".
func (f *Issues) TakeCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.calls
	f.calls = nil
	return c
}

// Disable answers every request as a repository with issues turned off.
func (f *Issues) Disable() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disabled = true
}

func (f *Issues) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	w.Header().Set("Content-Type", "application/json")
	if f.disabled {
		http.Error(w, `{"message":"Issues has been disabled in this repository."}`, http.StatusGone)
		return
	}
	prefix := "/repos/" + f.Repo + "/issues"
	switch {
	case r.Method == "POST" && r.URL.Path == "/graphql":
		f.pins++
		w.Write([]byte(`{"data":{}}`))
	case r.Method == "GET" && r.URL.Path == prefix:
		creator := r.URL.Query().Get("creator")
		out := []*Issue{}
		for i := len(f.issues) - 1; i >= 0; i-- {
			if creator == "" || f.issues[i].User.Login == creator {
				out = append(out, f.issues[i])
			}
		}
		json.NewEncoder(w).Encode(out)
	case r.Method == "POST" && r.URL.Path == prefix:
		var req struct{ Title, Body string }
		json.NewDecoder(r.Body).Decode(&req)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(f.add(f.Login, req.Title, req.Body, "open"))
	case strings.HasPrefix(r.URL.Path, prefix+"/"):
		var n int
		fmt.Sscanf(strings.TrimPrefix(r.URL.Path, prefix+"/"), "%d", &n)
		if n < 1 || n > len(f.issues) {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		i := f.issues[n-1]
		if r.Method == "PATCH" {
			var req struct {
				Body  *string
				State *string
			}
			json.NewDecoder(r.Body).Decode(&req)
			if req.Body != nil {
				i.Body = *req.Body
			}
			if req.State != nil {
				i.State = *req.State
			}
		}
		json.NewEncoder(w).Encode(i)
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusTeapot)
	}
}

// Client serves f for the rest of the test and returns a go-github client
// talking to it.
func (f *Issues) Client(t testing.TB) *gh.Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	base := srv.URL + "/"
	c, err := gh.NewClient(gh.WithHTTPClient(srv.Client()), gh.WithURLs(&base, &base))
	if err != nil {
		t.Fatal(err)
	}
	return c
}
