package review

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
		// .git in another case, which macOS reads as the same directory.
		{"read_file", `{"path": ".GIT/config"}`, ""},
		{"list_files", `{"path": ".Git"}`, ""},
		{"search", `{"pattern": "ghs_", "path": ".GIT"}`, ""},
		{"read_file", `{"path": "assets/logo.bin"}`, "binary"},
		{"read_file", `{"path": "nope.txt"}`, "does not exist"},
		{"nope", `{}`, "no tool"},
		{"read_file", `not json`, "not JSON"},
	} {
		if got := runTool(context.Background(), root, c.tool, c.args); !strings.HasPrefix(got, "error:") || !strings.Contains(got, c.wantErr) {
			t.Errorf("%s %s = %q, want an error containing %q", c.tool, c.args, got, c.wantErr)
		}
	}
	// Searching the whole checkout reaches neither .git nor the key.
	if got := runTool(context.Background(), root, "search", `{"pattern": "ghs_|PRIVATE"}`); got != "(no matches)" {
		t.Errorf("search found secrets: %q", got)
	}
}

func TestToolsWithCurrentDirectoryCheckout(t *testing.T) {
	root := checkout(t)
	t.Chdir(root)
	for _, c := range []struct{ tool, args, want string }{
		{"list_files", `{"path":"src"}`, "app.js\n"},
		{"read_file", `{"path":"src/app.js","limit":1}`, "1\tconst express"},
		{"read_file", `{"path":"app-link.js"}`, "app.del"},
		{"search", `{"path":"src","pattern":"app\\.del"}`, "src/app.js:3: app.del"},
		{"read_file", `{"path":"key.pem"}`, "error: path is outside the checkout"},
		{"read_file", `{"path":".git/config"}`, "error: the .git directory"},
	} {
		if got := runTool(context.Background(), ".", c.tool, c.args); !strings.Contains(got, c.want) {
			t.Errorf("%s %s = %q; want %q", c.tool, c.args, got, c.want)
		}
	}
	if got := runTool(context.Background(), ".", "list_files", `{}`); strings.Contains(got, ".git") || !strings.Contains(got, "src/") {
		t.Errorf("root listing = %q; want checkout files without .git", got)
	}
}

func TestSearchReportsUnreadablePaths(t *testing.T) {
	for _, rel := range []string{".", "src", "src/app.js"} {
		t.Run(rel, func(t *testing.T) {
			root := checkout(t)
			path := filepath.Join(root, rel)
			if err := os.Chmod(path, 0); err != nil {
				t.Skipf("cannot restrict permissions: %v", err)
			}
			t.Cleanup(func() { os.Chmod(path, 0o755) })
			if f, err := os.Open(path); err == nil {
				f.Close()
				t.Skip("current user can read paths without read permission")
			}
			got := runTool(context.Background(), root, "search", `{"pattern":"express"}`)
			if !strings.HasPrefix(got, "error:") {
				t.Fatalf("search = %q; unreadable code must not look like a complete search", got)
			}
		})
	}
}

func TestTools(t *testing.T) {
	root := checkout(t)
	list := runTool(context.Background(), root, "list_files", `{"path": "."}`)
	for _, want := range []string{"package.json\n", "src/\n", "key.pem (symlink)\n"} {
		if !strings.Contains(list, want) {
			t.Errorf("list missing %q:\n%s", want, list)
		}
	}
	if strings.Contains(list, ".git") {
		t.Errorf("list shows .git:\n%s", list)
	}
	if got := runTool(context.Background(), root, "read_file", `{"path": "src/app.js", "offset": 2, "limit": 1}`); got != "2\tconst app = express();\n... more lines follow; read from offset 3\n" {
		t.Errorf("read = %q", got)
	}
	// A symlink to a file inside the checkout reads like the file.
	if got := runTool(context.Background(), root, "read_file", `{"path": "app-link.js"}`); !strings.Contains(got, "app.del") {
		t.Errorf("read through link = %q", got)
	}
	got := runTool(context.Background(), root, "search", `{"pattern": "express\\(|app\\.del"}`)
	if got != "src/app.js:2: const app = express();\nsrc/app.js:3: app.del('/x', h);\n" {
		t.Errorf("search = %q", got)
	}
	// The newline ending a file starts no line of its own to match.
	if got := runTool(context.Background(), root, "search", `{"pattern": "^$", "path": "src"}`); got != "(no matches)" {
		t.Errorf("search for an empty line = %q", got)
	}
	if got := runTool(context.Background(), root, "search", `{"pattern": "("}`); !strings.Contains(got, "bad pattern") {
		t.Errorf("bad pattern = %q", got)
	}
}

func TestToolsPreserveWhitespaceInPaths(t *testing.T) {
	for _, name := range []string{" leading.txt", "trailing.txt ", " "} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, name), []byte("exact file\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if trimmed := strings.TrimSpace(name); trimmed != "" {
				if err := os.WriteFile(filepath.Join(root, trimmed), []byte("wrong file\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			args, err := json.Marshal(map[string]string{"path": name, "pattern": "exact"})
			if err != nil {
				t.Fatal(err)
			}
			for _, tool := range []string{"read_file", "search"} {
				if got := runTool(context.Background(), root, tool, string(args)); !strings.Contains(got, "exact file") {
					t.Errorf("%s for %q = %q", tool, name, got)
				}
			}
			if err := os.Remove(filepath.Join(root, name)); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
				t.Fatal(err)
			}
			if got := runTool(context.Background(), root, "list_files", string(args)); got != "(empty)" {
				t.Errorf("list_files for directory %q = %q", name, got)
			}
		})
	}
}

func TestAPIEndpointPreservesQuery(t *testing.T) {
	for _, suffix := range []string{"/v1?api-version=1", "/v1/?api-version=1", "/v1?route=a%2Fb#local"} {
		t.Run(suffix, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/chat/completions" {
					t.Errorf("request path = %q", r.URL.Path)
				}
				if strings.Contains(suffix, "api-version") && r.URL.Query().Get("api-version") != "1" {
					t.Errorf("request query = %q", r.URL.RawQuery)
				}
				if strings.Contains(suffix, "route=") && r.URL.Query().Get("route") != "a/b" {
					t.Errorf("request query = %q", r.URL.RawQuery)
				}
				fmt.Fprint(w, `{"choices":[{"message":{"content":"{\"decision\":\"approve\",\"summary\":\"ok\",\"breaking_changes\":[],\"concerns\":[]}"},"finish_reason":"stop"}]}`)
			}))
			defer server.Close()
			a := API{URL: server.URL + suffix, Model: "test"}
			if _, err := a.Run(context.Background(), t.TempDir(), "review"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A minified bundle is one huge line: reading it gives the start of the
// line, numbered as the others, rather than megabytes or an error.
func TestSearchStopsWhenTheReviewTimesOut(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("needle\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := runTool(ctx, root, "search", `{"pattern": "needle"}`); !strings.Contains(got, "context canceled") {
		t.Fatalf("search = %q; a cancelled review must not keep reading files", got)
	}
}

func TestReadFileStopsWhenTheReviewTimesOut(t *testing.T) {
	root := checkout(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := runTool(ctx, root, "read_file", `{"path":"src/app.js"}`); !strings.Contains(got, "context canceled") {
		t.Fatalf("read = %q; a cancelled review must not keep reading files", got)
	}
}

// cancelReader cancels the review as its first chunk is being read.
type cancelReader struct {
	io.Reader
	cancel context.CancelFunc
}

func (r cancelReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.cancel()
	return n, err
}

func TestReadLineStopsDuringLongLine(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	source := strings.NewReader(strings.Repeat("x", 2<<20))
	r := bufio.NewReaderSize(cancelReader{source, cancel}, 64<<10)
	if _, _, err := readLine(ctx, r, maxLineBytes); !errors.Is(err, context.Canceled) {
		t.Fatalf("read = %v; a cancelled review must stop discarding a long line", err)
	}
	if source.Len() == 0 {
		t.Fatal("read consumed the whole line after cancellation")
	}
}

func TestReadFileCutsLongLines(t *testing.T) {
	root := t.TempDir()
	bundle := "first\n" + strings.Repeat("x", 2<<20) + "\nlast"
	if err := os.WriteFile(filepath.Join(root, "dist.js"), []byte(bundle), 0o644); err != nil {
		t.Fatal(err)
	}
	got := runTool(context.Background(), root, "read_file", `{"path": "dist.js"}`)
	want := "1\tfirst\n2\t" + strings.Repeat("x", maxLineBytes) + " … [line cut at 2000 bytes]\n3\tlast\n"
	if got != want {
		t.Errorf("read = %.200q… (%d bytes), want %d bytes", got, len(got), len(want))
	}

	// A last line, without its newline, that fills the reader exactly.
	if err := os.WriteFile(filepath.Join(root, "dist.js"), []byte("first\n"+strings.Repeat("x", 64<<10)), 0o644); err != nil {
		t.Fatal(err)
	}
	got = runTool(context.Background(), root, "read_file", `{"path": "dist.js"}`)
	want = "1\tfirst\n2\t" + strings.Repeat("x", maxLineBytes) + " … [line cut at 2000 bytes]\n"
	if got != want {
		t.Errorf("last line of 64 KiB: read = %.200q… (%d bytes), want %d bytes", got, len(got), len(want))
	}
}

// fakeModel is an OpenAI-compatible endpoint answering with replies, in
// order, and keeping the requests.
type fakeModel struct {
	mu      sync.Mutex
	replies []string
	// cut holds the indexes of the replies cut short at max_tokens.
	cut      map[int]bool
	finish   map[int]string
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
	if reason, ok := f.finish[len(f.requests)-1]; ok {
		finish = reason
	}
	w.Write([]byte(`{"choices": [{"message": ` + reply + `, "finish_reason": "` + finish + `"}], "usage": {"prompt_tokens": 100, "completion_tokens": 10}}`))
}

func serve(t *testing.T, f *fakeModel) string {
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return srv.URL + "/v1"
}

const approve = `{"decision": "approve", "summary": "only app.del changed, unused", "breaking_changes": ["app.del removed: not used"], "concerns": []}`

func TestAPIResponseSizeLimit(t *testing.T) {
	const limit = 16 << 20
	response := `{"choices":[{"message":` + content(approve) + `,"finish_reason":"stop"}]}`
	response += strings.Repeat(" ", limit-len(response))
	for _, suffix := range []string{"", " ", `{"error":"response continued"}`} {
		t.Run(fmt.Sprintf("extra_bytes=%d", len(suffix)), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, response+suffix)
			}))
			defer server.Close()
			a := API{URL: server.URL}
			answer, err := a.Run(context.Background(), t.TempDir(), "review")
			if suffix == "" {
				if err != nil || answer.Decision != "approve" {
					t.Fatalf("response at limit: answer=%+v, err=%v", answer, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "too large") {
				t.Fatalf("oversized response: answer=%+v, err=%v; want size error", answer, err)
			}
		})
	}
}

func content(s string) string {
	b, _ := json.Marshal(map[string]string{"role": "assistant", "content": s})
	return string(b)
}

func TestAPIRejectsUnfinishedApprovals(t *testing.T) {
	for _, reason := range []string{"content_filter", "", "unknown", "function_call", "tool_calls"} {
		for _, final := range []bool{false, true} {
			name := reason
			if final {
				name += "/final"
			}
			t.Run(name, func(t *testing.T) {
				replies := []string{content(approve)}
				if final {
					replies = append([]string{content("please wait")}, replies...)
				}
				f := &fakeModel{replies: replies, finish: map[int]string{len(replies) - 1: reason}}
				_, err := (API{URL: serve(t, f)}).Run(context.Background(), t.TempDir(), "review")
				if err == nil {
					t.Fatal("accepted an approval without a completed answer")
				}
			})
		}
	}
}

func TestAPIUsesToolsThenAnswers(t *testing.T) {
	root := checkout(t)
	f := &fakeModel{finish: map[int]string{0: "tool_calls"}, replies: []string{
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

func TestAPIRejectsUnsolicitedToolCalls(t *testing.T) {
	call := `{"role":"assistant","content":` + string(mustJSON(t, approve)) + `,"tool_calls":[{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"src/app.js\"}"}}]}`
	for _, tc := range []struct {
		name    string
		tools   bool
		replies []string
	}{
		{"tools disabled", false, []string{call}},
		{"final answer", true, []string{content("please clarify"), call}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeModel{replies: append(append([]string{}, tc.replies...), content(approve))}
			a := API{URL: serve(t, f), Model: "m", Tools: tc.tools}
			if _, err := a.Run(context.Background(), checkout(t), "review this"); err == nil || !strings.Contains(err.Error(), "tool calls") {
				t.Errorf("error = %v; unsolicited tool calls must fail the review", err)
			}
			if len(f.requests) != len(tc.replies) {
				t.Fatalf("sent %d requests, want %d; tool output must not be sent", len(f.requests), len(tc.replies))
			}
		})
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestAPIRequiresOneAnswerObject(t *testing.T) {
	reject := `{"decision":"reject","summary":"needs review","breaking_changes":[],"concerns":[]}`
	for name, reply := range map[string]string{
		"array":                      "[" + approve + "]",
		"trailing value":             approve + " false",
		"leading value":              "null " + approve,
		"revised in prose":           approve + " On reflection, reject this update.",
		"unfinished reconsideration": approve + "<think>Wait, this update breaks the application",
		"duplicate decision":         `{"decision":"reject","decision":"approve","summary":"ambiguous","breaking_changes":[],"concerns":[]}`,
		"case variant decision":      `{"decision":"reject","Decision":"approve","summary":"ambiguous","breaking_changes":[],"concerns":[]}`,
		"missing fields":             `{"decision":"approve"}`,
		"null summary":               strings.Replace(approve, `"summary": "only app.del changed, unused"`, `"summary": null`, 1),
		"null concerns":              strings.Replace(approve, `"concerns": []`, `"concerns": null`, 1),
		"null concern item":          strings.Replace(approve, `"concerns": []`, `"concerns": [null]`, 1),
		"duplicate concerns":         strings.TrimSuffix(approve, "}") + `,"concerns":["unsafe"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeModel{replies: []string{content(reply), content(reject)}}
			ans, err := (API{URL: serve(t, f), Model: "m"}).Run(context.Background(), checkout(t), "review this")
			if err != nil || ans.Decision != "reject" || len(f.requests) != 2 {
				t.Fatalf("answer %+v, %v after %d requests; want a fresh answer", ans, err, len(f.requests))
			}
			// The final schema-constrained reply must obey the same rule.
			f = &fakeModel{replies: []string{content("please clarify"), content(reply)}}
			if _, err := (API{URL: serve(t, f), Model: "m"}).Run(context.Background(), checkout(t), "review this"); err == nil {
				t.Fatal("accepted an ambiguous final answer")
			}
		})
	}
}

func TestAPIKeepsThinkingTagsInsideTheAnswer(t *testing.T) {
	want := Answer{Decision: "reject", Summary: "The <think> and </think> tags changed", BreakingChanges: []string{}, Concerns: []string{"literal <think> marker"}}
	answer := string(mustJSON(t, want))
	// Use literal tags, as a model may, instead of encoding their brackets.
	answer = strings.ReplaceAll(strings.ReplaceAll(answer, `\u003c`, "<"), `\u003e`, ">")
	for _, prefix := range []string{"", "<think>checking compatibility</think>\n", "checking compatibility</think>\n"} {
		f := &fakeModel{replies: []string{content(prefix + answer)}}
		got, err := (API{URL: serve(t, f), Model: "m"}).Run(context.Background(), checkout(t), "review")
		if err != nil || got.Summary != want.Summary || len(got.Concerns) != 1 || got.Concerns[0] != want.Concerns[0] {
			t.Errorf("prefix %q: answer %+v, %v; JSON strings must remain intact", prefix, got, err)
		}
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
		// A complete reply holding a draft and the answer: neither is
		// taken, as the first could be either.
		"two answers": {replies: []string{content("Draft:\n" + approve + "\nOn reflection:\n" + reject), content(reject)}},
	} {
		ans, err := (API{URL: serve(t, f), Model: "m"}).Run(context.Background(), checkout(t), "p")
		if err != nil || ans.Decision != "reject" || len(f.requests) != 2 {
			t.Errorf("%s: answer %+v, %v after %d requests", name, ans, err, len(f.requests))
		}
	}
	// A chat template that opens the thinking in the prompt leaves only its
	// end in the reply: a draft before it is thinking all the same.
	f := &fakeModel{replies: []string{content("maybe " + approve + " but wait, app.del is used</think>\n" + reject)}}
	if ans, err := (API{URL: serve(t, f), Model: "m"}).Run(context.Background(), checkout(t), "p"); err != nil || ans.Decision != "reject" {
		t.Errorf("thinking opened in the prompt: answer %+v, %v", ans, err)
	}
	// An answer cut short is no answer either.
	f = &fakeModel{replies: []string{content("no"), content(approve)}, cut: map[int]bool{1: true}}
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
