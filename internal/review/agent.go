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

// Claude runs `claude -p` with only the read-only file tools, no MCP
// servers and no user or project settings.
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
	args := []string{
		"-p", "--restricted", "--strict-mcp-config", "--no-session-persistence",
		"--output-format", "json", "--tools", "Read,Grep,Glob", "--json-schema", Schema,
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
	out, err := run(cmd)
	if err != nil {
		return Answer{}, err
	}
	var res struct {
		IsError          bool    `json:"is_error"`
		Subtype          string  `json:"subtype"`
		Result           string  `json:"result"`
		StructuredOutput *Answer `json:"structured_output"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return Answer{}, fmt.Errorf("claude: unreadable output: %w", err)
	}
	if res.IsError {
		return Answer{}, fmt.Errorf("claude: %s: %s", res.Subtype, clip(res.Result))
	}
	if res.StructuredOutput == nil {
		return Answer{}, errors.New("claude: no structured output")
	}
	return *res.StructuredOutput, nil
}

// Codex runs `codex exec` in its read-only sandbox.
type Codex struct {
	Model string
	// Home, when set, is passed as CODEX_HOME.
	Home string
	// Command is the executable, "codex" when empty.
	Command string
}

func (c Codex) Name() string { return "codex" }

func (c Codex) Run(ctx context.Context, dir, prompt string) (Answer, error) {
	tmp, err := os.MkdirTemp("", "deps-merger-codex-")
	if err != nil {
		return Answer{}, err
	}
	defer os.RemoveAll(tmp)
	schema, last := filepath.Join(tmp, "schema.json"), filepath.Join(tmp, "answer.json")
	if err := os.WriteFile(schema, []byte(Schema), 0o600); err != nil {
		return Answer{}, err
	}
	args := []string{
		"exec", "--sandbox", "read-only", "--skip-git-repo-check", "--ephemeral", "--color", "never",
		"--output-schema", schema, "--output-last-message", last, "--cd", dir,
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
		return Answer{}, err
	}
	data, err := os.ReadFile(last)
	if err != nil {
		return Answer{}, fmt.Errorf("codex: no answer: %w", err)
	}
	var a Answer
	if err := json.Unmarshal(data, &a); err != nil {
		return Answer{}, fmt.Errorf("codex: unreadable answer: %w", err)
	}
	return a, nil
}

// githubTokens are the environment variables gh and other GitHub tools
// read a token from. An agent has no use for one, so it does not get it.
var githubTokens = []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"}

// agentEnv is this process's environment without the GitHub tokens.
func agentEnv() []string {
	env := os.Environ()
	return slices.DeleteFunc(env, func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return slices.ContainsFunc(githubTokens, func(t string) bool { return strings.EqualFold(name, t) })
	})
}

// waitDelay is how long run waits for output pipes once the process has
// exited or been killed.
var waitDelay = 5 * time.Second

// run runs cmd and returns its stdout, folding the end of stderr into the
// error when it fails.
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
			return nil, fmt.Errorf("%s: %w: %s", name, err, msg)
		}
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return stdout.Bytes(), nil
}

// clip keeps the last few hundred bytes of an agent's message, where the
// cause of a failure usually is, without splitting a character.
func clip(s string) string {
	s = strings.TrimSpace(s)
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
