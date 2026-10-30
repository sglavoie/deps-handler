package review

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// TaskAgent runs a structured task. Writable tasks may edit and test the
// disposable checkout; callers retain responsibility for commits and pushes.
type TaskAgent interface {
	Name() string
	Task(ctx context.Context, dir, prompt, schema string, writable bool) (json.RawMessage, error)
}

// Agent runs an AI coding agent, read-only, in dir and returns its answer.
type Agent interface {
	Name() string
	Run(ctx context.Context, dir, prompt string) (Answer, error)
}

// Answer is the structured reply both agents are held to by Schema.
type Answer struct {
	Decision        string   `json:"decision"` // approve | reject
	Summary         string   `json:"summary"`
	BreakingChanges []string `json:"breaking_changes"`
	Concerns        []string `json:"concerns"`
}

// Schema is the JSON Schema of Answer. Every property is required and no
// other is allowed, as Codex's strict structured output demands.
const Schema = `{
  "type": "object",
  "properties": {
    "decision": {"type": "string", "enum": ["approve", "reject"]},
    "summary": {"type": "string"},
    "breaking_changes": {"type": "array", "items": {"type": "string"}},
    "concerns": {"type": "array", "items": {"type": "string"}}
  },
  "required": ["decision", "summary", "breaking_changes", "concerns"],
  "additionalProperties": false
}`

// Claude runs `claude -p`, using read-only file tools for reviews and
// diagnosis and allowing editing and Bash for explicit repair tasks. No MCP
// servers, no user or project settings and, with --safe-mode, none of the
// user's customizations: no CLAUDE.md (the user's, or one in the checkout
// or its vendored code), skills, plugins or hooks. The login is kept.
type Claude struct {
	Model string
	// ConfigDir, when set, is passed as CLAUDE_CONFIG_DIR.
	ConfigDir    string
	MaxBudgetUSD float64
	// Command is the executable, "claude" when empty.
	Command string
}

func (c Claude) Name() string { return "claude" }

func (c Claude) Run(ctx context.Context, dir, prompt string) (Answer, error) {
	data, err := c.Task(ctx, dir, prompt, Schema, false)
	if err != nil {
		return Answer{}, err
	}
	a, err := decodeAnswer(data)
	if err != nil {
		return Answer{}, fmt.Errorf("claude: unreadable answer: %w", err)
	}
	return a, nil
}

func (c Claude) Task(ctx context.Context, dir, prompt, schema string, writable bool) (json.RawMessage, error) {
	tools := "Read,Grep,Glob"
	if writable {
		tools = "Read,Grep,Glob,Edit,Write,Bash"
	}
	args := []string{
		"-p", "--restricted", "--safe-mode", "--strict-mcp-config", "--no-session-persistence",
		"--output-format", "json", "--tools", tools, "--json-schema", schema,
	}
	if writable {
		args = append(args, "--allowedTools", "Read,Grep,Glob,Edit,Write,Bash", "--permission-mode", "acceptEdits", "--permission-prompts", "none")
	}
	if c.Model != "" {
		args = append(args, "--model", c.Model)
	}
	if c.MaxBudgetUSD > 0 {
		args = append(args, "--max-budget-usd", strconv.FormatFloat(c.MaxBudgetUSD, 'f', -1, 64))
	}
	cmd := exec.CommandContext(ctx, or(c.Command, "claude"), args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Env = agentEnv()
	if c.ConfigDir != "" {
		cmd.Env = append(cmd.Env, "CLAUDE_CONFIG_DIR="+c.ConfigDir)
	}
	out, runErr := run(cmd)
	var res struct {
		Type             string          `json:"type"`
		IsError          *bool           `json:"is_error"`
		Subtype          string          `json:"subtype"`
		Result           string          `json:"result"`
		StructuredOutput json.RawMessage `json:"structured_output"`
	}
	perr := json.Unmarshal(out, &res)
	// claude exits 1 on an error it reports in its JSON, where the reason
	// is: prefer that to the bare exit status.
	if perr == nil && res.IsError != nil && *res.IsError {
		if msg := clip(res.Result); msg != "" {
			if strings.Contains(strings.ToLower(msg), "not logged in") {
				return nil, errors.New("claude: not logged in; run `claude auth login` with the server's CLAUDE_CONFIG_DIR")
			}
			if res.Subtype == "" || res.Subtype == "success" {
				return nil, fmt.Errorf("claude: %s", msg)
			}
			return nil, fmt.Errorf("claude: %s: %s", res.Subtype, msg)
		}
		if res.Subtype == "success" {
			return nil, errors.New("claude: error")
		}
		return nil, fmt.Errorf("claude: %s", or(res.Subtype, "error"))
	}
	if runErr != nil {
		return nil, runErr
	}
	if perr != nil {
		return nil, fmt.Errorf("claude: unreadable output: %w", perr)
	}
	// A structured verdict does not establish that the review completed.
	// Limit failures can still carry output; missing flags must not become
	// an implicit success and allow that output to authorize a merge.
	if res.Type != "result" || res.Subtype != "success" || res.IsError == nil {
		return nil, fmt.Errorf("claude: incomplete or unsuccessful result (type %q, subtype %q)", Clean(res.Type), Clean(res.Subtype))
	}
	if len(res.StructuredOutput) == 0 || bytes.Equal(res.StructuredOutput, []byte("null")) {
		return nil, errors.New("claude: no structured output")
	}
	return res.StructuredOutput, nil
}

// Codex runs `codex exec` in a read-only sandbox (workspace-write for
// explicit repair tasks), without the user's
// config.toml or execpolicy rules: they could add MCP servers, which run
// outside the sandbox, or let commands escape it. CODEX_HOME still
// provides the login.
type Codex struct {
	Model string
	// Home, when set, is passed as CODEX_HOME.
	Home string
	// Command is the executable, "codex" when empty.
	Command string
}

func (c Codex) Name() string { return "codex" }

func (c Codex) Run(ctx context.Context, dir, prompt string) (Answer, error) {
	data, err := c.Task(ctx, dir, prompt, Schema, false)
	if err != nil {
		return Answer{}, err
	}
	a, err := decodeAnswer(data)
	if err != nil {
		return Answer{}, fmt.Errorf("codex: unreadable answer: %w", err)
	}
	return a, nil
}

func (c Codex) Task(ctx context.Context, dir, prompt, schemaText string, writable bool) (json.RawMessage, error) {
	// --cd is resolved by the child after cmd.Dir has already changed its
	// working directory. Make it absolute so a relative checkout is not
	// resolved a second time inside itself.
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp("", "deps-merger-codex-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	schema, last := filepath.Join(tmp, "schema.json"), filepath.Join(tmp, "answer.json")
	if err := os.WriteFile(schema, []byte(schemaText), 0o600); err != nil {
		return nil, err
	}
	sandbox := "read-only"
	if writable {
		sandbox = "workspace-write"
	}
	args := []string{
		"exec", "--sandbox", sandbox, "--ignore-user-config", "--ignore-rules",
		"--skip-git-repo-check", "--ephemeral", "--color", "never",
		"--output-schema", schema, "--output-last-message", last, "--cd", dir,
	}
	if writable {
		args = append(args, "--config", `approval_policy="never"`)
	}
	if c.Model != "" {
		args = append(args, "--model", c.Model)
	}
	cmd := exec.CommandContext(ctx, or(c.Command, "codex"), append(args, "-")...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Env = agentEnv()
	if c.Home != "" {
		cmd.Env = append(cmd.Env, "CODEX_HOME="+c.Home)
	}
	if _, err := run(cmd); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(last)
	if err != nil {
		return nil, fmt.Errorf("codex: no answer: %w", err)
	}
	return json.RawMessage(data), nil
}

// githubTokens are the environment variables gh and other GitHub tools
// read a token from. An agent has no use for one, so it does not get it.
var githubTokens = []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"}

// gitRepoEnv are the environment variables that point git at another
// repository than the one in its working directory, or give it settings
// from outside the config files: those `git rev-parse --local-env-vars`
// lists, and the ones that change how it looks for the repository.
// Inherited from a dotfiles setup, GIT_DIR would have git work on $HOME
// instead of the checkout. GIT_CONFIG_KEY_<n> and GIT_CONFIG_VALUE_<n> go
// with GIT_CONFIG_COUNT.
var gitRepoEnv = []string{
	"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_CONFIG", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT",
	"GIT_OBJECT_DIRECTORY", "GIT_DIR", "GIT_WORK_TREE", "GIT_IMPLICIT_WORK_TREE", "GIT_GRAFT_FILE",
	"GIT_INDEX_FILE", "GIT_NO_REPLACE_OBJECTS", "GIT_REPLACE_REF_BASE", "GIT_PREFIX", "GIT_SHALLOW_FILE",
	"GIT_COMMON_DIR", "GIT_NAMESPACE", "GIT_CEILING_DIRECTORIES", "GIT_DISCOVERY_ACROSS_FILESYSTEM",
}

// agentEnv is this process's environment without GitHub tokens, App
// settings and its review API key. CLI agents need none of those. It also
// excludes settings that would point git outside the checkout.
func agentEnv() []string {
	env := os.Environ()
	reviewKey := os.Getenv("DMC_REVIEW_KEY_ENV")
	return slices.DeleteFunc(env, func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return slices.ContainsFunc(githubTokens, func(t string) bool { return strings.EqualFold(name, t) }) ||
			strings.HasPrefix(name, "DMC_") || name == reviewKey ||
			slices.Contains(gitRepoEnv, name) ||
			strings.HasPrefix(name, "GIT_CONFIG_KEY_") || strings.HasPrefix(name, "GIT_CONFIG_VALUE_")
	})
}

// waitDelay is how long run waits for output pipes once the process has
// exited or been killed.
var waitDelay = 5 * time.Second

// run runs cmd and returns its stdout, folding the end of stderr into the
// error when it fails. The stdout of a failed command is returned too, as
// it may say why.
func run(cmd *exec.Cmd) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	// Give up on output pipes held open by the agent's own children once
	// it has exited or been killed.
	cmd.WaitDelay = waitDelay
	ownGroup(cmd)
	name := filepath.Base(cmd.Path)
	err := cmd.Run()
	// Whatever the agent left running goes with it, even when it succeeded.
	killGroup(cmd)
	if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
		// The agent exited cleanly but a child of its kept the output
		// open: what it wrote before exiting is its answer.
		err = nil
	}
	if err != nil {
		if msg := clip(stderr.String()); msg != "" {
			return stdout.Bytes(), fmt.Errorf("%s: %w: %s", name, err, msg)
		}
		return stdout.Bytes(), fmt.Errorf("%s: %w", name, err)
	}
	return stdout.Bytes(), nil
}

// clip keeps the last few hundred bytes of an agent's message, where the
// cause of a failure usually is, without splitting a character. It cleans
// the whole message first: a cut through a token's prefix would hide the
// rest of it from the redaction.
func clip(s string) string {
	s = strings.TrimSpace(Clean(s))
	const keep = 300
	if len(s) > keep {
		s = "…" + strings.ToValidUTF8(s[len(s)-keep:], "")
	}
	return s
}

func or(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
