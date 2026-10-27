package review

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// checkout makes a checkout with a few files, a .git directory, and
// symlinks leading inside and outside it.
func checkout(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	secret := filepath.Join(t.TempDir(), "secret.pem")
	for path, body := range map[string]string{
		"package.json":    `{"dependencies": {"express": "^5.0.0"}}`,
		"src/app.js":      "const express = require('express');\nconst app = express();\napp.del('/x', h);\n",
		".git/config":     "[remote]\n\turl = https://x-access-token:ghs_secret@github.com/o/r\n",
		"docs/README.md":  "express is used for routing\n",
		"assets/logo.bin": "PNG\x00\x01",
	} {
		p := filepath.Join(root, filepath.FromSlash(path))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(secret, []byte("-----BEGIN PRIVATE KEY-----"), 0o600)
	os.Symlink(secret, filepath.Join(root, "key.pem"))
	os.Symlink(filepath.Dir(secret), filepath.Join(root, "outside"))
	os.Symlink("src/app.js", filepath.Join(root, "app-link.js"))
	return root
}

func TestToolsStayInTheCheckout(t *testing.T) {
	root := checkout(t)
	for _, c := range []struct{ tool, args, wantErr string }{
		{"read_file", `{"path": "/etc/passwd"}`, "outside the checkout"},
		{"read_file", `{"path": "../../../etc/passwd"}`, "outside the checkout"},
		{"read_file", `{"path": "src/../../x"}`, "outside the checkout"},
		{"read_file", `{"path": "key.pem"}`, "outside the checkout"},
		{"read_file", `{"path": "outside/secret.pem"}`, "outside the checkout"},
		{"list_files", `{"path": "outside"}`, "outside the checkout"},
		{"search", `{"pattern": "PRIVATE", "path": "outside"}`, "outside the checkout"},
		{"read_file", `{"path": ".git/config"}`, ".git directory"},
		{"list_files", `{"path": ".git"}`, ".git directory"},
		{"read_file", `{"path": "assets/logo.bin"}`, "binary"},
		{"read_file", `{"path": "nope.txt"}`, "does not exist"},
		{"nope", `{}`, "no tool"},
		{"read_file", `not json`, "not JSON"},
	} {
		if got := runTool(root, c.tool, c.args); !strings.HasPrefix(got, "error:") || !strings.Contains(got, c.wantErr) {
			t.Errorf("%s %s = %q, want an error containing %q", c.tool, c.args, got, c.wantErr)
		}
	}
	// Searching the whole checkout reaches neither .git nor the key.
	if got := runTool(root, "search", `{"pattern": "ghs_|PRIVATE"}`); got != "(no matches)" {
		t.Errorf("search found secrets: %q", got)
	}
}

func TestTools(t *testing.T) {
	root := checkout(t)
	list := runTool(root, "list_files", `{"path": "."}`)
	for _, want := range []string{"package.json\n", "src/\n", "key.pem (symlink)\n"} {
		if !strings.Contains(list, want) {
			t.Errorf("list missing %q:\n%s", want, list)
		}
	}
	if strings.Contains(list, ".git") {
		t.Errorf("list shows .git:\n%s", list)
	}
	if got := runTool(root, "read_file", `{"path": "src/app.js", "offset": 2, "limit": 1}`); got != "2\tconst app = express();\n... more lines follow; read from offset 3\n" {
		t.Errorf("read = %q", got)
	}
	// A symlink to a file inside the checkout reads like the file.
	if got := runTool(root, "read_file", `{"path": "app-link.js"}`); !strings.Contains(got, "app.del") {
		t.Errorf("read through link = %q", got)
	}
	got := runTool(root, "search", `{"pattern": "express\\(|app\\.del"}`)
	if got != "src/app.js:2: const app = express();\nsrc/app.js:3: app.del('/x', h);\n" {
		t.Errorf("search = %q", got)
	}
	if got := runTool(root, "search", `{"pattern": "("}`); !strings.Contains(got, "bad pattern") {
		t.Errorf("bad pattern = %q", got)
	}
}

// A minified bundle is one huge line: reading it gives the start of the
// line, numbered as the others, rather than megabytes or an error.
func TestReadFileCutsLongLines(t *testing.T) {
	root := t.TempDir()
	bundle := "first\n" + strings.Repeat("x", 2<<20) + "\nlast"
	if err := os.WriteFile(filepath.Join(root, "dist.js"), []byte(bundle), 0o644); err != nil {
		t.Fatal(err)
	}
	got := runTool(root, "read_file", `{"path": "dist.js"}`)
	want := "1\tfirst\n2\t" + strings.Repeat("x", maxLineBytes) + " … [line cut at 2000 bytes]\n3\tlast\n"
	if got != want {
		t.Errorf("read = %.200q… (%d bytes), want %d bytes", got, len(got), len(want))
	}
}

// fakeModel is an OpenAI-compatible endpoint answering with replies, in
// order, and keeping the requests.
type fakeModel struct {
	mu      sync.Mutex
	replies []string
	// cut holds the indexes of the replies cut short at max_tokens.
	cut      map[int]bool
	requests []chatRequest
	bodies   []string
	auth     []string
}

func (f *fakeModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path != "/v1/chat/completions" {
		http.NotFound(w, r)
		return
	}
	var req chatRequest
	body, _ := io.ReadAll(r.Body)
	json.Unmarshal(body, &req)
	f.requests = append(f.requests, req)
	f.bodies = append(f.bodies, string(body))
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	if len(f.replies) == 0 {
		http.Error(w, `{"error": "no more replies"}`, http.StatusInternalServerError)
		return
	}
	reply := f.replies[0]
	f.replies = f.replies[1:]
	finish := "stop"
	if f.cut[len(f.requests)-1] {
		finish = "length"
	}
	w.Write([]byte(`{"choices": [{"message": ` + reply + `, "finish_reason": "` + finish + `"}], "usage": {"prompt_tokens": 100, "completion_tokens": 10}}`))
}

func serve(t *testing.T, f *fakeModel) string {
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return srv.URL + "/v1"
}

const approve = `{"decision": "approve", "summary": "only app.del changed, unused", "breaking_changes": ["app.del removed: not used"], "concerns": []}`

func content(s string) string {
	b, _ := json.Marshal(map[string]string{"role": "assistant", "content": s})
	return string(b)
}

func TestAPIUsesToolsThenAnswers(t *testing.T) {
	root := checkout(t)
	f := &fakeModel{replies: []string{
		`{"role": "assistant", "content": "", "tool_calls": [{"id": "c1", "type": "function", "function": {"name": "search", "arguments": "{\"pattern\": \"app\\\\.del\"}"}}]}`,
		content("<think>app.del is used once</think>\n```json\n" + approve + "\n```"),
	}}
	var used Usage
	a := API{URL: serve(t, f), Model: "qwen", Key: "k", Tools: true, Usage: func(u Usage) { used = u }}
	ans, err := a.Run(context.Background(), root, "review this")
	if err != nil {
		t.Fatal(err)
	}
	if ans.Decision != "approve" || len(ans.BreakingChanges) != 1 {
		t.Fatalf("answer = %+v", ans)
	}
	if len(f.requests) != 2 || len(f.requests[0].Tools) != 3 || f.requests[0].Model != "qwen" {
		t.Fatalf("requests = %+v", f.requests)
	}
	// The tool's result went back with the call's id.
	last := f.requests[1].Messages[len(f.requests[1].Messages)-1]
	if last.Role != "tool" || last.ToolCallID != "c1" || !strings.Contains(last.Content, "src/app.js:3: app.del") {
		t.Fatalf("tool message = %+v", last)
	}
	if f.auth[0] != "Bearer k" {
		t.Errorf("authorization = %q", f.auth[0])
	}
	if used != (Usage{Requests: 2, PromptTokens: 200, CompletionTokens: 20}) {
		t.Errorf("usage = %+v", used)
	}
}

func TestAPIAsksForTheSchemaWhenTheReplyIsNotAnAnswer(t *testing.T) {
	f := &fakeModel{replies: []string{content("I think this looks fine."), content(approve)}}
	a := API{URL: serve(t, f), Model: "m"}
	ans, err := a.Run(context.Background(), checkout(t), "review this")
	if err != nil || ans.Decision != "approve" {
		t.Fatalf("answer %+v, %v", ans, err)
	}
	second := f.requests[1]
	if second.ResponseFormat == nil || len(second.Tools) != 0 {
		t.Fatalf("second request: format %v, %d tools", second.ResponseFormat, len(second.Tools))
	}
	// Without tools, the model is told it cannot read the checkout.
	if len(f.requests[0].Tools) != 0 || !strings.Contains(f.requests[0].Messages[0].Content, "cannot read the checkout") {
		t.Fatalf("first request: %+v", f.requests[0])
	}
}

func TestAPIBoundsEachReplyAndLeavesSamplingToTheServer(t *testing.T) {
	// Thinking cut short at the bound: no tool call and no answer.
	cut := `{"role": "assistant", "content": "", "reasoning_content": "wait, let me check again. wait, let me check again."}`
	f := &fakeModel{replies: []string{cut, content(approve)}}
	a := API{URL: serve(t, f), Model: "m", Tools: true, MaxTokens: 1000}
	if ans, err := a.Run(context.Background(), checkout(t), "p"); err != nil || ans.Decision != "approve" {
		t.Fatalf("answer %+v, %v", ans, err)
	}
	if len(f.requests) != 2 || f.requests[1].ResponseFormat == nil {
		t.Fatalf("%d requests; last %+v", len(f.requests), f.requests[len(f.requests)-1])
	}
	for i, r := range f.requests {
		if r.MaxTokens != 1000 || strings.Contains(f.bodies[i], "temperature") {
			t.Fatalf("request %d: max_tokens %d, body %s", i, r.MaxTokens, f.bodies[i])
		}
	}
	f = &fakeModel{replies: []string{content(approve)}}
	(API{URL: serve(t, f), Model: "m"}).Run(context.Background(), checkout(t), "p")
	if f.requests[0].MaxTokens != DefaultMaxTokens {
		t.Fatalf("default max_tokens %d", f.requests[0].MaxTokens)
	}
}

func TestAPITakesNoDraftForTheAnswer(t *testing.T) {
	reject := `{"decision": "reject", "summary": "app.del is used", "breaking_changes": [], "concerns": ["app.del removed"]}`
	draft := "Draft: " + approve + " but wait, app.del is used, so I must rej"
	for name, f := range map[string]*fakeModel{
		// Thinking left open by a reply cut short, on a server that keeps
		// it in the content.
		"open thinking": {replies: []string{content("<think>" + draft), content(reject)}, cut: map[int]bool{0: true}},
		// A draft outside any thinking, in a reply cut short.
		"cut short": {replies: []string{content(draft), content(reject)}, cut: map[int]bool{0: true}},
	} {
		ans, err := (API{URL: serve(t, f), Model: "m"}).Run(context.Background(), checkout(t), "p")
		if err != nil || ans.Decision != "reject" || len(f.requests) != 2 {
			t.Errorf("%s: answer %+v, %v after %d requests", name, ans, err, len(f.requests))
		}
	}
	// An answer cut short is no answer either.
	f := &fakeModel{replies: []string{content("no"), content(approve)}, cut: map[int]bool{1: true}}
	if _, err := (API{URL: serve(t, f), Model: "m"}).Run(context.Background(), checkout(t), "p"); err == nil || !strings.Contains(err.Error(), "cut short") {
		t.Errorf("cut answer: err = %v", err)
	}
}

func TestAPIStopsCallingToolsAfterMaxTurns(t *testing.T) {
	call := `{"role": "assistant", "content": "", "tool_calls": [{"id": "c", "type": "function", "function": {"name": "list_files", "arguments": "{\"path\": \".\"}"}}]}`
	f := &fakeModel{replies: []string{call, call, content(approve)}}
	a := API{URL: serve(t, f), Model: "m", Tools: true, MaxTurns: 2}
	if ans, err := a.Run(context.Background(), checkout(t), "p"); err != nil || ans.Decision != "approve" {
		t.Fatalf("answer %+v, %v", ans, err)
	}
	if len(f.requests) != 3 || f.requests[2].ResponseFormat == nil || len(f.requests[2].Tools) != 0 {
		t.Fatalf("%d requests; last %+v", len(f.requests), f.requests[len(f.requests)-1])
	}
}

func TestAPIErrors(t *testing.T) {
	var used Usage
	f := &fakeModel{}
	a := API{URL: serve(t, f), Model: "m", Usage: func(u Usage) { used = u }}
	if _, err := a.Run(context.Background(), checkout(t), "p"); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v", err)
	}
	f = &fakeModel{replies: []string{content("no"), content(`{"decision": "maybe", "summary": "", "breaking_changes": [], "concerns": []}`)}}
	a = API{URL: serve(t, f), Model: "m", Usage: func(u Usage) { used = u }}
	if _, err := a.Run(context.Background(), checkout(t), "p"); err == nil || !strings.Contains(err.Error(), "unreadable answer") {
		t.Fatalf("err = %v", err)
	}
	if used.Requests != 2 {
		t.Errorf("usage not reported on failure: %+v", used)
	}
}
