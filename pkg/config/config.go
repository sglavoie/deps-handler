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
	"net/url"
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
	// DependabotCommands lets deps-merger comment @dependabot rebase on a
	// PR that is behind. Dependabot takes commands only from users with
	// push access, not from a GitHub App or GITHUB_TOKEN: without it, such
	// a PR is left for a human to rebase.
	DependabotCommands bool `json:"dependabot_commands"`
	// Mode is ModeMerge, or ModeReport to only report on the repository,
	// as a dry run does, even in a run that merges.
	Mode string `json:"mode"`
}

// The modes a policy can set.
const (
	ModeMerge  = "merge"
	ModeReport = "report"
)

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

	// layered is set when base and entry say how Policy was made, so that
	// WithFiles can slot the repository's own files in between.
	layered bool
	// base is the policy before the repo's entry: the defaults.
	base Policy
	// entry is what the repo's entry overrides.
	entry overrides
}

// MayReview reports whether r's policy can allow agent reviews once its
// owner's and its own files are applied: only its config entry, the last
// layer, rules them out for certain.
func (r Repo) MayReview() bool {
	return r.Policy.AgentReview || r.entry.AgentReview == nil
}

// NewRepo returns a repository with policy base, as a config listing it
// with no overrides would, for callers that build their own defaults.
func NewRepo(owner, name string, base Policy) Repo {
	return Repo{Owner: owner, Name: name, Policy: base, layered: true, base: base}
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
	// Use is the agent run by default: "claude", "codex" or "api".
	Use     string
	Timeout time.Duration
	Claude  Claude
	Codex   Codex
	API     API
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

// API configures a model behind an OpenAI-compatible chat completions
// endpoint, local (llama.cpp's llama-server, Ollama, LM Studio, vLLM) or
// hosted.
type API struct {
	// URL is the API's base, such as http://127.0.0.1:8081/v1.
	URL   string `yaml:"url"`
	Model string `yaml:"model"`
	// KeyEnv names the environment variable holding the API key, if the
	// endpoint wants one: the key itself stays out of the config.
	KeyEnv string `yaml:"key_env"`
	// Tools lets the model list, read and search the checkout; true
	// unless set false, for models that handle tools badly.
	Tools *bool `yaml:"tools"`
	// MaxTurns bounds the rounds of tool calls; 0 is the default, 30.
	MaxTurns int `yaml:"max_turns"`
}

// AgentList names the supported agents for a message: "claude, codex or
// api".
func AgentList() string {
	return strings.Join(Agents[:len(Agents)-1], ", ") + " or " + Agents[len(Agents)-1]
}

// UsesTools reports whether the model may use the checkout tools.
func (a API) UsesTools() bool { return a.Tools == nil || *a.Tools }

// Agents lists the supported agents, the first being the default.
var Agents = []string{"claude", "codex", "api"}

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
	// DependabotCommands is dependabot_commands.
	DependabotCommands *bool `yaml:"dependabot_commands"`
	// MergeWithoutChecksAfter is a duration such as "10m"; "0" turns it off.
	MergeWithoutChecksAfter *string `yaml:"merge_without_checks_after"`
	Mode                    *string `yaml:"mode"`
}

type repoEntry struct {
	Repo      string `yaml:"repo"`
	Skip      bool   `yaml:"skip"`
	overrides `yaml:",inline"`
}

var (
	entryKeys  = yamlKeys(reflect.TypeFor[repoEntry]())
	policyKeys = yamlKeys(reflect.TypeFor[overrides]())
)

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

// policyFields is overrides as a mapping of its own, under defaults or in
// a policy file. A repo entry inlines overrides and checks them itself:
// yaml.v3 would hand an inlined field's unmarshaler the whole entry.
type policyFields struct{ overrides }

// UnmarshalYAML rejects what checkKeys does.
func (p *policyFields) UnmarshalYAML(n *yaml.Node) error {
	// Node.Decode does not inherit the decoder's KnownFields, so check here.
	if err := checkKeys(n, policyKeys, ""); err != nil {
		return err
	}
	return n.Decode(&p.overrides)
}

// checkEntryKeys is checkKeys for a repo entry.
func checkEntryKeys(n *yaml.Node) error { return checkKeys(n, entryKeys, " in repo entry") }

// checkKeys rejects keys of a mapping outside keys, and policy fields given
// no value ("max_update_type:" or "~"), which would decode as unset and
// inherit without a word, though an empty string is rejected as invalid.
// A null skip is rejected too: decoding it as false would include a
// repository whose entry never explicitly allowed it to run.
// It follows YAML merge keys ("<<: *shared") into the mappings they pull
// in. where follows "unknown field" in an error.
func checkKeys(n *yaml.Node, keys map[string]bool, where string) error {
	return checkKeysVisited(n, keys, where, make(map[*yaml.Node]bool))
}

// visited records nodes on the current path as false and fully checked
// nodes as true. Reject cycles before Node.Decode can see them, and check
// shared mappings only once even when many merge aliases reference them.
func checkKeysVisited(n *yaml.Node, keys map[string]bool, where string, visited map[*yaml.Node]bool) error {
	if n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	if done, seen := visited[n]; seen {
		if !done {
			return fmt.Errorf("line %d: recursive YAML merge alias", n.Line)
		}
		return nil
	}
	visited[n] = false
	switch n.Kind {
	case yaml.SequenceNode: // <<: [*a, *b]
		for _, c := range n.Content {
			if err := checkKeysVisited(c, keys, where, visited); err != nil {
				return err
			}
		}
	case yaml.MappingNode:
		for i := 0; i < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if v.Kind == yaml.AliasNode {
				v = v.Alias
			}
			switch {
			case k.Tag == "!!merge":
				if err := checkKeysVisited(v, keys, where, visited); err != nil {
					return err
				}
			case !keys[k.Value]:
				return fmt.Errorf("line %d: unknown field %q%s", k.Line, k.Value, where)
			case k.Value == "skip" && v.Tag == "!!null":
				return fmt.Errorf("line %d: skip has no value; use true or false, or remove it to include the repository", k.Line)
			case policyKeys[k.Value] && v.Tag == "!!null":
				return fmt.Errorf("line %d: %s has no value; give it one, or remove it to inherit", k.Line, k.Value)
			}
		}
	}
	visited[n] = true
	return nil
}

type file struct {
	Defaults policyFields `yaml:"defaults"`
	// Repos holds nodes, decoded one by one, so that each error can give
	// its line and an empty entry ("- " or "~"), which the decoder would
	// otherwise drop without a word, can be reported.
	Repos []yaml.Node `yaml:"repos"`
	Agent agentEntry  `yaml:"agent"`
}

type agentEntry struct {
	Use     string `yaml:"use"`
	Timeout string `yaml:"timeout"`
	Claude  Claude `yaml:"claude"`
	Codex   Codex  `yaml:"codex"`
	API     API    `yaml:"api"`
}

var validMergeMethods = map[string]bool{"squash": true, "merge": true, "rebase": true}

// ChecksGrace is how long a new commit may go without any check before it
// counts as having no CI: GitHub registers checks only once a workflow
// starts, which can take that long when runners are busy. It is also the
// least merge_without_checks_after can wait, so that setting never merges a
// PR whose CI simply has not started yet.
const ChecksGrace = 30 * time.Minute

// Builtin is the policy used when neither defaults nor a repo override it.
var Builtin = Policy{MergeMethod: "squash", MaxUpdateType: UpdateMinor, RequireChecks: true, AgentReview: true, DependabotCommands: true, Mode: ModeMerge}

// exampleURL is the annotated example config, which a `go install` leaves
// no copy of.
const exampleURL = "https://github.com/sglavoie/deps-merger/blob/main/config.example.yaml"

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

// ErrNoRepos is a config that lists no repository, as the example init
// writes does until its repos are filled in.
var ErrNoRepos = errors.New("config lists no repos (see " + exampleURL + ")")

// NotFoundError is a config file that does not exist.
type NotFoundError struct{ Path string }

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("config not found at %s (see %s)", e.Path, exampleURL)
}

func (e *NotFoundError) Unwrap() error { return os.ErrNotExist }

// Load reads and resolves the config file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, &NotFoundError{Path: path}
		}
		return nil, err
	}
	return Parse(data)
}

// Parse resolves raw YAML into a Config.
func Parse(data []byte) (*Config, error) { return parse(data, false) }

// LoadForPR reads agent settings without requiring configured repositories.
// Targeted PR commands select their repository explicitly.
func LoadForPR(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parse(data, true)
}

func parse(data []byte, allowNoRepos bool) (*Config, error) {
	var f file
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&f); errors.Is(err, io.EOF) {
		return nil, errors.New("config is empty (see " + exampleURL + ")")
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
	defaults, err := apply(Builtin, f.Defaults.overrides)
	if err != nil {
		return nil, fmt.Errorf("defaults: %w", err)
	}
	agent, err := parseAgent(f.Agent)
	if err != nil {
		return nil, fmt.Errorf("agent: %w", err)
	}
	cfg := &Config{Agent: agent}
	seen := map[string]int{}
	for _, n := range f.Repos {
		if n.Kind == yaml.ScalarNode && n.Tag == "!!null" {
			return nil, fmt.Errorf("repos: line %d: empty entry", n.Line)
		}
		var e repoEntry
		if err := n.Decode(&e); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
		owner, name, err := parseRepoName(e.Repo)
		if err != nil {
			return nil, fmt.Errorf("repos: line %d: %w", n.Line, err)
		}
		key := strings.ToLower(owner + "/" + name)
		if first, ok := seen[key]; ok {
			return nil, fmt.Errorf("repos: line %d: %s listed twice (first on line %d)", n.Line, e.Repo, first)
		}
		seen[key] = n.Line
		p, err := apply(defaults, e.overrides)
		if err != nil {
			return nil, fmt.Errorf("repos: line %d: %s: %w", n.Line, e.Repo, err)
		}
		cfg.Repos = append(cfg.Repos, Repo{Owner: owner, Name: name, Policy: settle(p), Skip: e.Skip, layered: true, base: defaults, entry: e.overrides})
	}
	if len(cfg.Repos) == 0 && !allowNoRepos {
		return nil, ErrNoRepos
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
// and settle drops it from the repo's policy once every layer is applied,
// so that a later layer turning checks back on still has it.
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
	if o.DependabotCommands != nil {
		p.DependabotCommands = *o.DependabotCommands
	}
	if o.Mode != nil {
		m := strings.ToLower(strings.TrimSpace(*o.Mode))
		if m != ModeMerge && m != ModeReport {
			return p, fmt.Errorf("invalid mode %q (want merge or report)", *o.Mode)
		}
		p.Mode = m
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
	return p, nil
}

// settle finishes a repo's policy, once every layer is applied: one that
// turns checks off drops the merge_without_checks_after it inherited, so
// that it shows nothing it does not use.
func settle(p Policy) Policy {
	if !p.RequireChecks {
		p.MergeWithoutChecksAfter = 0
	}
	return p
}

func parseAgent(e agentEntry) (Agent, error) {
	a := Agent{Use: strings.ToLower(strings.TrimSpace(e.Use)), Timeout: DefaultAgentTimeout, Claude: e.Claude, Codex: e.Codex, API: e.API}
	if a.Use == "" {
		a.Use = Agents[0]
	}
	if !ValidAgent(a.Use) {
		return a, fmt.Errorf("invalid use %q (want %s)", e.Use, AgentList())
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
	if err := checkAPI(a.API, a.Use == "api"); err != nil {
		return a, err
	}
	return a, nil
}

// checkAPI checks the api settings: a URL and model are needed only when
// api is the agent used by default, but whatever is set must be valid.
func checkAPI(a API, used bool) error {
	if used && (a.URL == "" || a.Model == "") {
		return errors.New("use: api needs api.url and api.model")
	}
	if a.URL != "" {
		u, err := url.Parse(a.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("api.url %q is not an http or https URL", a.URL)
		}
	}
	if a.MaxTurns < 0 {
		return fmt.Errorf("api.max_turns must not be negative, not %d", a.MaxTurns)
	}
	return nil
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

// Filter keeps the repos named in only (case-insensitive owner/name or
// bare name, around which spaces are ignored), including skipped ones, and
// the repos of the owners in owners (case-insensitive) not marked skip,
// each once and in config order. With neither, it keeps every repo not
// marked skip. With includeSkipped, repos marked skip are kept as the
// others are. A bare name shared by repos of several owners is an error
// rather than a pick of them all, as is an owner no repo has.
func (c *Config) Filter(only, owners []string, includeSkipped bool) ([]Repo, error) {
	if len(only) == 0 && len(owners) == 0 {
		var out []Repo
		for _, r := range c.Repos {
			if includeSkipped || !r.Skip {
				out = append(out, r)
			}
		}
		return out, nil
	}
	picked := make([]bool, len(c.Repos))
	for _, want := range only {
		w := strings.ToLower(strings.TrimSpace(want))
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
	for _, owner := range owners {
		o := strings.TrimSpace(owner)
		found := false
		for i, r := range c.Repos {
			if strings.EqualFold(r.Owner, o) {
				found = true
				picked[i] = picked[i] || includeSkipped || !r.Skip
			}
		}
		if !found {
			return nil, fmt.Errorf("owner %q has no repo in the config", owner)
		}
	}
	var out []Repo
	for i, r := range c.Repos {
		if picked[i] {
			out = append(out, r)
		}
	}
	return out, nil
}

// RepoFile is where a repository keeps its own policy, on its default
// branch. OrgFile, in the owner's .github repository (OrgRepo), holds
// policy for every repository of the owner.
const (
	RepoFile = ".github/deps-merger.yml"
	OrgRepo  = ".github"
	OrgFile  = "deps-merger.yml"
)

// WithFiles returns r with the policy fields set in its owner's OrgFile
// and its own RepoFile, either nil when absent, layered between the
// defaults and r's entry: the defaults, then org, then repo, then the
// entry. The files hold policy fields only, as under defaults.
func (r Repo) WithFiles(org, repo []byte) (Repo, error) {
	if org == nil && repo == nil {
		return r, nil
	}
	base, entry := r.Policy, overrides{}
	if r.layered {
		base, entry = r.base, r.entry
	}
	p := base
	for _, f := range []struct {
		name string
		data []byte
	}{{r.Owner + "/" + OrgRepo + ":" + OrgFile, org}, {r.FullName() + ":" + RepoFile, repo}} {
		if f.data == nil {
			continue
		}
		o, err := parseFile(f.data)
		if err == nil {
			p, err = apply(p, o)
		}
		if err != nil {
			return r, fmt.Errorf("%s: %w", f.name, err)
		}
	}
	p, err := apply(p, entry)
	if err != nil {
		return r, fmt.Errorf("config entry for %s, over its files: %w", r.FullName(), err)
	}
	r.Policy = settle(p)
	return r, nil
}

// parseFile reads a repository's or owner's policy file: policy fields
// only, as under defaults. An empty file sets nothing.
func parseFile(data []byte) (overrides, error) {
	var p policyFields
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&p); errors.Is(err, io.EOF) {
		return p.overrides, nil
	} else if err != nil {
		return p.overrides, err
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return p.overrides, fmt.Errorf("line %d: more than one YAML document", extra.Line)
	} else if !errors.Is(err, io.EOF) {
		return p.overrides, err
	}
	return p.overrides, nil
}
