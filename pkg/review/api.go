package review

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// API reviews through an OpenAI-compatible chat completions endpoint: a
// local model served by llama.cpp's llama-server, Ollama, LM Studio or
// vLLM, or a hosted provider. Unlike the CLI agents, it runs no program:
// the model reads the checkout only through three tools deps-merger
// implements, which cannot reach outside it, and sees nothing else of the
// machine.
type API struct {
	// URL is the API's base, such as http://127.0.0.1:8081/v1.
	URL   string
	Model string
	// Key, when set, is sent as a bearer token.
	Key string
	// Tools lets the model list, read and search the checkout. Without
	// them it judges from the prompt alone, which holds the release notes
	// and the diff: for models that handle tools badly.
	Tools bool
	// MaxTurns bounds the rounds of tool calls before the model must
	// answer; 0 is DefaultMaxTurns.
	MaxTurns int
	// MaxTokens bounds what the model writes in one request, its thinking
	// included, so a model caught in a loop gives up in minutes rather
	// than at the review's timeout; 0 is DefaultMaxTokens.
	MaxTokens int
	// Client makes the requests; nil uses one without a timeout, the
	// review's own bounding them.
	Client *http.Client
	// Usage, when set, is told the tokens a review used, once it ends,
	// whether it succeeded or not.
	Usage func(Usage)
}

// Usage is the tokens one review used, summed over its requests.
type Usage struct {
	Requests         int
	PromptTokens     int
	CompletionTokens int
}

// DefaultMaxTurns is API's MaxTurns when unset.
const DefaultMaxTurns = 30

// DefaultMaxTokens is API's MaxTokens when unset.
const DefaultMaxTokens = 8192

// The tools' limits keep what they return within a local model's context.
const (
	maxListEntries  = 500
	maxReadLines    = 400
	maxReadBytes    = 64 << 10
	maxLineBytes    = 2000
	maxSearchHits   = 200
	maxSearchedFile = 1 << 20
)

func (a API) Name() string { return "api" }

const apiSystem = `You review a Dependabot pull request as the user describes. The checkout is reachable only through the tools, with paths relative to its root: list_files, read_file and search. Use them to find how the repository uses the updated dependencies. When you are done, reply with only the JSON object the user asks for, and nothing else.`

const apiNoTools = `You review a Dependabot pull request as the user describes. You cannot read the checkout: judge from the release notes, description and diff the user gives you, and reject when they are not enough. Reply with only the JSON object the user asks for, and nothing else.`

const apiAnswerNow = `Give your answer now: only the JSON object, with "decision", "summary", "breaking_changes" and "concerns".`

func (a API) Run(ctx context.Context, dir, prompt string) (Answer, error) {
	var used Usage
	if a.Usage != nil {
		defer func() { a.Usage(used) }()
	}
	system := apiNoTools
	if a.Tools {
		system = apiSystem
	}
	msgs := []message{{Role: "system", Content: system}, {Role: "user", Content: prompt}}
	turns := a.MaxTurns
	if turns <= 0 {
		turns = DefaultMaxTurns
	}
	limit := a.MaxTokens
	if limit <= 0 {
		limit = DefaultMaxTokens
	}
	// No temperature is sent: the server's own sampling applies, as greedy
	// decoding sends thinking models into endless repetition.
	for range turns {
		req := chatRequest{Model: a.Model, Messages: msgs, MaxTokens: limit}
		if a.Tools {
			req.Tools = apiTools
		}
		reply, cut, err := a.complete(ctx, req, &used)
		if err != nil {
			return Answer{}, err
		}
		if !a.Tools && len(reply.ToolCalls) > 0 {
			return Answer{}, errors.New("api: model returned tool calls while tools are disabled")
		}
		msgs = append(msgs, reply)
		// A reply cut short at MaxTokens is no answer, even when it holds
		// one the model drafted while thinking: it is asked for the
		// answer below.
		if len(reply.ToolCalls) == 0 {
			if ans, err := parseAnswer(reply.Content); err == nil && !cut {
				return ans, nil
			}
			break
		}
		for _, call := range reply.ToolCalls {
			msgs = append(msgs, message{Role: "tool", ToolCallID: call.ID, Content: runTool(ctx, dir, call.Function.Name, call.Function.Arguments)})
		}
	}
	// Out of turns, or a reply that was not the answer: ask for it, held
	// to the schema, with no tools left to call.
	msgs = append(msgs, message{Role: "user", Content: apiAnswerNow})
	req := chatRequest{Model: a.Model, Messages: msgs, MaxTokens: limit, ResponseFormat: answerFormat}
	reply, cut, err := a.complete(ctx, req, &used)
	if err != nil {
		return Answer{}, err
	}
	if cut {
		return Answer{}, fmt.Errorf("api: answer cut short at %d tokens", limit)
	}
	if len(reply.ToolCalls) > 0 {
		return Answer{}, errors.New("api: model returned tool calls instead of a final answer")
	}
	ans, err := parseAnswer(reply.Content)
	if err != nil {
		return Answer{}, fmt.Errorf("api: unreadable answer: %w", err)
	}
	return ans, nil
}

type message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatRequest struct {
	Model          string    `json:"model"`
	Messages       []message `json:"messages"`
	Tools          []tool    `json:"tools,omitempty"`
	MaxTokens      int       `json:"max_tokens"`
	ResponseFormat any       `json:"response_format,omitempty"`
}

type tool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

func newTool(name, description, parameters string) tool {
	t := tool{Type: "function"}
	t.Function.Name, t.Function.Description, t.Function.Parameters = name, description, json.RawMessage(parameters)
	return t
}

var apiTools = []tool{
	newTool("list_files", "List the files and directories in a directory of the checkout. Directories end with /.",
		`{"type":"object","properties":{"path":{"type":"string","description":"directory relative to the checkout root; . for the root"}},"required":["path"]}`),
	newTool("read_file", fmt.Sprintf("Read a file of the checkout, with line numbers, at most %d lines from offset.", maxReadLines),
		`{"type":"object","properties":{"path":{"type":"string"},"offset":{"type":"integer","description":"first line, from 1"},"limit":{"type":"integer"}},"required":["path"]}`),
	newTool("search", fmt.Sprintf("Search the checkout's text files for a regular expression, returning at most %d matching lines as path:line: text.", maxSearchHits),
		`{"type":"object","properties":{"pattern":{"type":"string","description":"RE2 regular expression"},"path":{"type":"string","description":"directory to search; . for the root"}},"required":["pattern"]}`),
}

var answerFormat = map[string]any{
	"type": "json_schema",
	"json_schema": map[string]any{
		"name":   "verdict",
		"strict": true,
		"schema": json.RawMessage(Schema),
	},
}

// complete sends req and returns the model's reply, and whether it was
// cut short at req.MaxTokens.
func (a API) complete(ctx context.Context, req chatRequest, used *Usage) (message, bool, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return message{}, false, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(a.URL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return message{}, false, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	if a.Key != "" {
		hreq.Header.Set("Authorization", "Bearer "+a.Key)
	}
	c := a.Client
	if c == nil {
		c = http.DefaultClient
	}
	resp, err := c.Do(hreq)
	if err != nil {
		return message{}, false, fmt.Errorf("api: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return message{}, false, fmt.Errorf("api: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return message{}, false, fmt.Errorf("api: %s: %s", resp.Status, clip(string(raw)))
	}
	var out struct {
		Choices []struct {
			Message      message `json:"message"`
			FinishReason string  `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return message{}, false, fmt.Errorf("api: unreadable response: %w", err)
	}
	used.Requests++
	used.PromptTokens += out.Usage.PromptTokens
	used.CompletionTokens += out.Usage.CompletionTokens
	if len(out.Choices) == 0 {
		return message{}, false, errors.New("api: response holds no choice")
	}
	reply := out.Choices[0].Message
	reply.Role = "assistant"
	return reply, out.Choices[0].FinishReason == "length", nil
}

var fenced = regexp.MustCompile("(?s)^```(?:json)?\\s*(.*?)\\s*```$")

// parseAnswer reads the model's JSON answer, past any reasoning it wrote
// before it or a code fence around it. Many chat templates open the
// thinking in the prompt, so a reply can close it without opening it:
// everything before its closing </think> is thinking too. What remains must
// be one JSON object: surrounding prose or other JSON values may mean it
// is a draft the model went on to change its mind about.
func parseAnswer(content string) (Answer, error) {
	s := strings.TrimSpace(content)
	// Only remove reasoning before the answer. Tags in JSON strings are
	// data, and reasoning after an answer may be reconsidering that answer.
	// Neither may be stripped to turn an ambiguous reply into an approval.
	for !strings.HasPrefix(s, "{") && !strings.HasPrefix(s, "```") {
		i := strings.Index(s, "</think>")
		if i < 0 {
			break
		}
		s = strings.TrimSpace(s[i+len("</think>"):])
	}
	if m := fenced.FindStringSubmatch(s); m != nil {
		s = m[1]
	}
	return decodeAnswer([]byte(s))
}

// runTool runs one tool call and returns what the model is told: the
// result, or why there is none.
func runTool(ctx context.Context, root, name, arguments string) string {
	var args struct {
		Path    string `json:"path"`
		Offset  int    `json:"offset"`
		Limit   int    `json:"limit"`
		Pattern string `json:"pattern"`
	}
	if err := json.Unmarshal([]byte(or(arguments, "{}")), &args); err != nil {
		return "error: arguments are not JSON: " + err.Error()
	}
	var out string
	var err error
	switch name {
	case "list_files":
		out, err = listFiles(root, args.Path)
	case "read_file":
		out, err = readFile(ctx, root, args.Path, args.Offset, args.Limit)
	case "search":
		out, err = search(ctx, root, args.Path, args.Pattern)
	default:
		err = fmt.Errorf("no tool %q", name)
	}
	if err != nil {
		return "error: " + err.Error()
	}
	return out
}

// errOutside is a path that does not lead inside the checkout.
var errOutside = errors.New("path is outside the checkout")

// inside resolves rel, a path relative to the checkout root, to a path
// within it, following symlinks so that one cannot lead out of it.
func inside(root, rel string) (string, error) {
	rel = filepath.FromSlash(strings.TrimSpace(rel))
	if rel == "" {
		rel = "."
	}
	if !filepath.IsLocal(rel) {
		return "", errOutside
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	p, err := filepath.EvalSymlinks(filepath.Join(realRoot, rel))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("%s does not exist", filepath.ToSlash(rel))
		}
		return "", err
	}
	if p != realRoot && !strings.HasPrefix(p, realRoot+string(filepath.Separator)) {
		return "", errOutside
	}
	if isGitDir(realRoot, p) {
		return "", errors.New("the .git directory is not part of the checkout")
	}
	return p, nil
}

func isGitDir(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	// macOS ignores case, so .GIT is the same directory.
	first, _, _ := strings.Cut(rel, string(filepath.Separator))
	return strings.EqualFold(first, ".git")
}

func listFiles(root, rel string) (string, error) {
	dir, err := inside(root, rel)
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", errors.New("not a directory")
	}
	var b strings.Builder
	n := 0
	for _, e := range entries {
		if e.Name() == ".git" && dir == mustReal(root) {
			continue
		}
		if n == maxListEntries {
			fmt.Fprintf(&b, "... %d more\n", len(entries)-n)
			break
		}
		name := e.Name()
		switch {
		case e.Type()&fs.ModeSymlink != 0:
			name += " (symlink)"
		case e.IsDir():
			name += "/"
		}
		b.WriteString(name)
		b.WriteByte('\n')
		n++
	}
	if n == 0 {
		return "(empty)", nil
	}
	return b.String(), nil
}

func mustReal(root string) string {
	r, err := filepath.EvalSymlinks(root)
	if err != nil {
		return root
	}
	return r
}

func readFile(ctx context.Context, root, rel string, offset, limit int) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	p, err := inside(root, rel)
	if err != nil {
		return "", err
	}
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || st.IsDir() {
		return "", errors.New("not a file")
	}
	offset = max(offset, 1)
	if limit <= 0 || limit > maxReadLines {
		limit = maxReadLines
	}
	var b strings.Builder
	r := bufio.NewReaderSize(f, 64<<10)
	line, shown := 0, 0
	for {
		text, clipped, err := readLine(ctx, r, maxLineBytes)
		if errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return "", err
		}
		line++
		if line < offset {
			continue
		}
		if shown == limit || b.Len() > maxReadBytes {
			fmt.Fprintf(&b, "... more lines follow; read from offset %d\n", line)
			break
		}
		if strings.ContainsRune(text, 0) {
			return "", errors.New("binary file")
		}
		if clipped {
			text += fmt.Sprintf(" … [line cut at %d bytes]", maxLineBytes)
		}
		fmt.Fprintf(&b, "%d\t%s\n", line, text)
		shown++
	}
	if shown == 0 {
		return fmt.Sprintf("(no lines from offset %d; the file has %d)", offset, line), nil
	}
	return b.String(), nil
}

// readLine reads the next line of r, without its line ending, keeping at
// most limit bytes of it, and reports whether it held more. A minified
// bundle, as an action's dist/index.js, is one line of megabytes: whole,
// it would fill a local model's context, or fail to read at all.
// Cancellation is checked for every fragment, including the bytes thrown
// away from a long line and the lines skipped before a requested offset.
func readLine(ctx context.Context, r *bufio.Reader, limit int) (text string, clipped bool, err error) {
	var b []byte
	for {
		if err := ctx.Err(); err != nil {
			return "", false, err
		}
		chunk, more, err := r.ReadLine()
		if errors.Is(err, io.EOF) && len(b) > 0 {
			// The file ends a line that filled the reader exactly.
			return strings.ToValidUTF8(string(b), ""), clipped, nil
		}
		if err != nil {
			return "", false, err
		}
		keep := min(len(chunk), limit-len(b))
		clipped = clipped || keep < len(chunk)
		b = append(b, chunk[:keep]...)
		if !more {
			return strings.ToValidUTF8(string(b), ""), clipped, nil
		}
	}
}

// search stops when ctx is done: walking a large checkout must not hold
// the review past its timeout.
func search(ctx context.Context, root, rel, pattern string) (string, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", fmt.Errorf("bad pattern: %w", err)
	}
	dir, err := inside(root, rel)
	if err != nil {
		return "", err
	}
	realRoot := mustReal(root)
	var b strings.Builder
	hits := 0
	stop := errors.New("enough")
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != dir && (strings.EqualFold(d.Name(), ".git") || d.Name() == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		// WalkDir does not follow symlinks; neither does the search.
		if !d.Type().IsRegular() {
			return nil
		}
		if info, err := d.Info(); err != nil || info.Size() > maxSearchedFile {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil || bytes.IndexByte(data, 0) >= 0 {
			return nil
		}
		relp, _ := filepath.Rel(realRoot, p)
		// The newline ending the last line starts no line of its own.
		for i, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
			if re.MatchString(line) {
				if hits == maxSearchHits {
					return stop
				}
				fmt.Fprintf(&b, "%s:%d: %s\n", filepath.ToSlash(relp), i+1, clipLine(line))
				hits++
			}
		}
		return nil
	})
	if errors.Is(err, stop) {
		fmt.Fprintf(&b, "... stopped at %d matches; narrow the pattern or the path\n", maxSearchHits)
	} else if err != nil {
		return "", err
	}
	if hits == 0 {
		return "(no matches)", nil
	}
	return b.String(), nil
}

func clipLine(s string) string {
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}

var _ Agent = API{}
