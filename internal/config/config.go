// Package config loads the list of repositories deps-merger handles and the
// merge policy that applies to each of them.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// UpdateType is a semver bump size, ordered so that a larger value is a
// riskier update.
type UpdateType int

const (
	UpdateUnknown UpdateType = iota
	UpdatePatch
	UpdateMinor
	UpdateMajor
)

func (u UpdateType) String() string {
	switch u {
	case UpdatePatch:
		return "patch"
	case UpdateMinor:
		return "minor"
	case UpdateMajor:
		return "major"
	}
	return "unknown"
}

// ParseUpdateType accepts "patch", "minor" or "major".
func ParseUpdateType(s string) (UpdateType, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "patch":
		return UpdatePatch, nil
	case "minor":
		return UpdateMinor, nil
	case "major":
		return UpdateMajor, nil
	}
	return UpdateUnknown, fmt.Errorf("invalid update type %q (want patch, minor or major)", s)
}

func (u UpdateType) MarshalText() ([]byte, error) { return []byte(u.String()), nil }

// Policy is the effective merge policy for one repository.
type Policy struct {
	MergeMethod   string     `json:"merge_method"`
	MaxUpdateType UpdateType `json:"max_update_type"`
	RequireChecks bool       `json:"require_checks"`
	// MergeWithoutChecksAfter lets a PR that required checks merge once its
	// latest commit has gone this long without any, as on a repo whose CI
	// only runs for some paths. Zero never merges such a PR.
	MergeWithoutChecksAfter time.Duration `json:"merge_without_checks_after"`
	// AgentReview lets the AI agent judge updates the policy alone would
	// not merge (over max_update_type, or of unknown size) when a run asks
	// for agent reviews.
	AgentReview bool `json:"agent_review"`
}

// MarshalJSON writes MergeWithoutChecksAfter as the config would, "10m",
// rather than as nanoseconds.
func (p Policy) MarshalJSON() ([]byte, error) {
	type plain Policy
	return json.Marshal(struct {
		plain
		MergeWithoutChecksAfter string `json:"merge_without_checks_after"`
	}{plain(p), ShortDuration(p.MergeWithoutChecksAfter)})
}

// ShortDuration formats d as the config would write it: 10m, not 10m0s.
func ShortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// Repo is one configured repository with its resolved policy.
type Repo struct {
	Owner  string `json:"owner"`
	Name   string `json:"name"`
	Policy Policy `json:"policy"`
	// Skip leaves the repo out of runs unless it is named explicitly.
	Skip bool `json:"skip,omitempty"`
}

func (r Repo) FullName() string { return r.Owner + "/" + r.Name }

// Config is the resolved configuration.
type Config struct {
	Repos []Repo
	Agent Agent
}

// Agent configures the AI agent that reviews updates the policy alone
// would not merge.
type Agent struct {
	// Use is the agent run by default: "claude" or "codex".
	Use     string
	Timeout time.Duration
	Claude  Claude
	Codex   Codex
}

// Claude configures `claude -p`.
type Claude struct {
	Model string `yaml:"model"`
	// ConfigDir is passed as CLAUDE_CONFIG_DIR, picking the account and
	// settings Claude Code uses.
	ConfigDir    string  `yaml:"config_dir"`
	MaxBudgetUSD float64 `yaml:"max_budget_usd"`
}

// Codex configures `codex exec`.
type Codex struct {
	Model string `yaml:"model"`
	// Home is passed as CODEX_HOME.
	Home string `yaml:"home"`
}

// Agents lists the supported agents, the first being the default.
var Agents = []string{"claude", "codex"}

// ValidAgent reports whether name is a supported agent.
func ValidAgent(name string) bool {
	return slices.Contains(Agents, name)
}

// DefaultAgentTimeout bounds one review when the config sets no timeout.
const DefaultAgentTimeout = 15 * time.Minute

// overrides holds optional policy fields; nil means "inherit".
type overrides struct {
	MergeMethod   *string `yaml:"merge_method"`
	MaxUpdateType *string `yaml:"max_update_type"`
	RequireChecks *bool   `yaml:"require_checks"`
	AgentReview   *bool   `yaml:"agent_review"`
	// MergeWithoutChecksAfter is a duration such as "10m"; "0" turns it off.
	MergeWithoutChecksAfter *string `yaml:"merge_without_checks_after"`
}

type repoEntry struct {
	Repo      string `yaml:"repo"`
	Skip      bool   `yaml:"skip"`
	overrides `yaml:",inline"`
}

var entryKeys = yamlKeys(reflect.TypeFor[repoEntry]())

// yamlKeys returns the mapping keys a struct accepts, following inlined
// fields, so the check below cannot drift from the struct tags.
func yamlKeys(t reflect.Type) map[string]bool {
	keys := map[string]bool{}
	for f := range t.Fields() {
		name, opts, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if opts == "inline" {
			maps.Copy(keys, yamlKeys(f.Type))
		} else {
			keys[name] = true
		}
	}
	return keys
}

// UnmarshalYAML lets an entry be either "owner/name" or a mapping with
// "repo:" plus overrides.
func (e *repoEntry) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		e.Repo = n.Value
		return nil
	}
	// Node.Decode does not inherit the decoder's KnownFields, so check here.
	if err := checkEntryKeys(n); err != nil {
		return err
	}
	type plain repoEntry
	return n.Decode((*plain)(e))
}

// checkEntryKeys rejects unknown keys in a repo entry, following YAML merge
// keys ("<<: *shared") into the mappings they pull in.
func checkEntryKeys(n *yaml.Node) error {
	if n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	switch n.Kind {
	case yaml.SequenceNode: // <<: [*a, *b]
		for _, c := range n.Content {
			if err := checkEntryKeys(c); err != nil {
				return err
			}
		}
	case yaml.MappingNode:
		for i := 0; i < len(n.Content); i += 2 {
			k := n.Content[i]
			if k.Tag == "!!merge" {
				if err := checkEntryKeys(n.Content[i+1]); err != nil {
					return err
				}
			} else if !entryKeys[k.Value] {
				return fmt.Errorf("line %d: unknown field %q in repo entry", k.Line, k.Value)
			}
		}
	}
	return nil
}

type file struct {
	Defaults overrides `yaml:"defaults"`
	// Repos holds pointers so that an empty entry ("- " or "~"), which
	// the decoder would otherwise drop without a word, can be reported.
	Repos []*repoEntry `yaml:"repos"`
	Agent agentEntry   `yaml:"agent"`
}

type agentEntry struct {
	Use     string `yaml:"use"`
	Timeout string `yaml:"timeout"`
	Claude  Claude `yaml:"claude"`
	Codex   Codex  `yaml:"codex"`
}

var validMergeMethods = map[string]bool{"squash": true, "merge": true, "rebase": true}

// ChecksGrace is how long a new commit may go without any check before it
// counts as having no CI: GitHub registers checks only once a workflow
// starts, which can take that long when runners are busy. It is also the
// least merge_without_checks_after can wait, so that setting never merges a
// PR whose CI simply has not started yet.
const ChecksGrace = 30 * time.Minute

// Builtin is the policy used when neither defaults nor a repo override it.
var Builtin = Policy{MergeMethod: "squash", MaxUpdateType: UpdateMinor, RequireChecks: true, AgentReview: true}

// DefaultPath returns $XDG_CONFIG_HOME/deps-merger/config.yaml, falling back
// to ~/.config/deps-merger/config.yaml.
func DefaultPath() (string, error) {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "deps-merger", "config.yaml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "deps-merger", "config.yaml"), nil
}

// StateDir returns $XDG_STATE_HOME/deps-merger, falling back to
// ~/.local/state/deps-merger.
func StateDir() (string, error) {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "deps-merger"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "deps-merger"), nil
}

// CacheDir returns $XDG_CACHE_HOME/deps-merger, falling back to
// ~/.cache/deps-merger.
func CacheDir() (string, error) {
	if dir := os.Getenv("XDG_CACHE_HOME"); dir != "" {
		return filepath.Join(dir, "deps-merger"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cache", "deps-merger"), nil
}

// Load reads and resolves the config file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("config not found at %s (see config.example.yaml)", path)
		}
		return nil, err
	}
	return Parse(data)
}

// Parse resolves raw YAML into a Config.
func Parse(data []byte) (*Config, error) {
	var f file
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&f); errors.Is(err, io.EOF) {
		return nil, errors.New("config is empty (see config.example.yaml)")
	} else if err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	// A second document would be silently ignored, and with it whatever
	// repos or policy it holds.
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return nil, fmt.Errorf("parse config: line %d: more than one YAML document; keep the whole config in one", extra.Line)
	} else if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	defaults, err := apply(Builtin, f.Defaults)
	if err != nil {
		return nil, fmt.Errorf("defaults: %w", err)
	}
	agent, err := parseAgent(f.Agent)
	if err != nil {
		return nil, fmt.Errorf("agent: %w", err)
	}
	cfg := &Config{Agent: agent}
	seen := map[string]bool{}
	for i, e := range f.Repos {
		if e == nil {
			return nil, fmt.Errorf("repos[%d]: empty entry", i)
		}
		owner, name, err := parseRepoName(e.Repo)
		if err != nil {
			return nil, fmt.Errorf("repos[%d]: %w", i, err)
		}
		key := strings.ToLower(owner + "/" + name)
		if seen[key] {
			return nil, fmt.Errorf("repos[%d]: %s listed twice", i, e.Repo)
		}
		seen[key] = true
		p, err := apply(defaults, e.overrides)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.Repo, err)
		}
		cfg.Repos = append(cfg.Repos, Repo{Owner: owner, Name: name, Policy: p, Skip: e.Skip})
	}
	if len(cfg.Repos) == 0 {
		return nil, errors.New("config lists no repos")
	}
	return cfg, nil
}

// parseRepoName splits an entry's "owner/name", ignoring surrounding
// whitespace as YAML would for an unquoted value, and checks both parts
// against the characters GitHub allows, so a typo fails here rather than
// as a "not found" from GitHub on every run.
func parseRepoName(s string) (owner, name string, err error) {
	owner, name, ok := strings.Cut(strings.TrimSpace(s), "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", "", fmt.Errorf("%q is not owner/name", s)
	}
	if !validOwner(owner) {
		return "", "", fmt.Errorf("%q: owner %q may only hold letters, digits and hyphens, and cannot start with a hyphen", s, owner)
	}
	if strings.HasSuffix(strings.ToLower(name), ".git") {
		return "", "", fmt.Errorf("%q: drop the .git suffix; write the repository as owner/name", s)
	}
	if !validName(name) {
		return "", "", fmt.Errorf("%q: name %q may only hold letters, digits, hyphens, underscores and dots", s, name)
	}
	return owner, name, nil
}

func validOwner(s string) bool {
	return !strings.HasPrefix(s, "-") && !strings.ContainsFunc(s, func(r rune) bool { return !isAlnum(r) && r != '-' })
}

func validName(s string) bool {
	return s != "." && s != ".." && !strings.ContainsFunc(s, func(r rune) bool { return !isAlnum(r) && !strings.ContainsRune("-_.", r) })
}

func isAlnum(r rune) bool {
	return 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9'
}

// apply layers o over base. A merge_without_checks_after that o sets but
// that has no effect, because checks are optional where it applies (set
// alongside require_checks: false, or inherited from the defaults), is an
// error rather than silently ignored. One inherited from the defaults by a
// repo that turns checks off is not: the default is meant for the others,
// and the repo's policy drops it so that it shows nothing it does not use.
// A setting shorter than ChecksGrace waits ChecksGrace, and the policy says
// so, since merging sooner could beat CI that has yet to start.
func apply(base Policy, o overrides) (Policy, error) {
	p := base
	if o.MergeMethod != nil {
		m := strings.ToLower(strings.TrimSpace(*o.MergeMethod))
		if !validMergeMethods[m] {
			return p, fmt.Errorf("invalid merge_method %q (want squash, merge or rebase)", *o.MergeMethod)
		}
		p.MergeMethod = m
	}
	if o.MaxUpdateType != nil {
		u, err := ParseUpdateType(*o.MaxUpdateType)
		if err != nil {
			return p, err
		}
		p.MaxUpdateType = u
	}
	if o.RequireChecks != nil {
		p.RequireChecks = *o.RequireChecks
	}
	if o.AgentReview != nil {
		p.AgentReview = *o.AgentReview
	}
	if o.MergeWithoutChecksAfter != nil {
		d, err := time.ParseDuration(*o.MergeWithoutChecksAfter)
		if err != nil || d < 0 {
			return p, fmt.Errorf("invalid merge_without_checks_after %q (want a duration such as 10m, or 0)", *o.MergeWithoutChecksAfter)
		}
		if d > 0 && !p.RequireChecks {
			return p, fmt.Errorf("merge_without_checks_after %q has no effect with require_checks: false, which merges PRs without checks anyway; remove one of them", *o.MergeWithoutChecksAfter)
		}
		if d > 0 {
			d = max(d, ChecksGrace)
		}
		p.MergeWithoutChecksAfter = d
	}
	if !p.RequireChecks {
		p.MergeWithoutChecksAfter = 0
	}
	return p, nil
}

func parseAgent(e agentEntry) (Agent, error) {
	a := Agent{Use: strings.ToLower(strings.TrimSpace(e.Use)), Timeout: DefaultAgentTimeout, Claude: e.Claude, Codex: e.Codex}
	if a.Use == "" {
		a.Use = Agents[0]
	}
	if !ValidAgent(a.Use) {
		return a, fmt.Errorf("invalid use %q (want %s)", e.Use, strings.Join(Agents, " or "))
	}
	if e.Timeout != "" {
		d, err := time.ParseDuration(e.Timeout)
		if err != nil || d <= 0 {
			return a, fmt.Errorf("invalid timeout %q (want a duration such as 15m)", e.Timeout)
		}
		a.Timeout = d
	}
	// A NaN would pass the sign check and reach claude as "NaN".
	if b := a.Claude.MaxBudgetUSD; b < 0 || math.IsNaN(b) || math.IsInf(b, 0) {
		return a, fmt.Errorf("claude.max_budget_usd must be a non-negative number, not %v", b)
	}
	var err error
	if a.Claude.ConfigDir, err = expandHome("claude.config_dir", a.Claude.ConfigDir); err != nil {
		return a, err
	}
	if a.Codex.Home, err = expandHome("codex.home", a.Codex.Home); err != nil {
		return a, err
	}
	return a, nil
}

// expandHome replaces a leading ~ with the home directory, as a shell
// would for a path written in the config. Any other path must be absolute:
// a relative one would depend on where deps-merger runs from, which under
// cron is rarely where the config is.
func expandHome(field, path string) (string, error) {
	if path == "" {
		return "", nil
	}
	if path != "~" && !strings.HasPrefix(path, "~/") {
		if !filepath.IsAbs(path) {
			return "", fmt.Errorf("%s %q must be an absolute path or start with ~/", field, path)
		}
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, path[1:]), nil
}

// Filter keeps only the repos named in only (case-insensitive owner/name or
// bare name), including skipped ones, each once and in config order. An
// empty filter keeps every repo not marked skip. A bare name shared by
// repos of several owners is an error rather than a pick of them all.
func (c *Config) Filter(only []string) ([]Repo, error) {
	if len(only) == 0 {
		var out []Repo
		for _, r := range c.Repos {
			if !r.Skip {
				out = append(out, r)
			}
		}
		return out, nil
	}
	picked := make([]bool, len(c.Repos))
	for _, want := range only {
		w := strings.ToLower(want)
		var matches []int
		for i, r := range c.Repos {
			if strings.ToLower(r.FullName()) == w || strings.ToLower(r.Name) == w {
				matches = append(matches, i)
			}
		}
		switch len(matches) {
		case 0:
			return nil, fmt.Errorf("repo %q is not in the config", want)
		case 1:
		default:
			// Only a bare name can match more than one repo.
			names := make([]string, len(matches))
			for j, i := range matches {
				names[j] = c.Repos[i].FullName()
			}
			return nil, fmt.Errorf("ambiguous: %s matches %s; use owner/name", want, strings.Join(names, ", "))
		}
		picked[matches[0]] = true
	}
	var out []Repo
	for i, r := range c.Repos {
		if picked[i] {
			out = append(out, r)
		}
	}
	return out, nil
}
