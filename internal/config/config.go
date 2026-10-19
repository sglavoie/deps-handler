// Package config loads the list of repositories deps-merger handles and the
// merge policy that applies to each of them.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"

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
}

// overrides holds optional policy fields; nil means "inherit".
type overrides struct {
	MergeMethod   *string `yaml:"merge_method"`
	MaxUpdateType *string `yaml:"max_update_type"`
	RequireChecks *bool   `yaml:"require_checks"`
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
	if n.Kind == yaml.MappingNode {
		for i := 0; i < len(n.Content); i += 2 {
			if k := n.Content[i]; !entryKeys[k.Value] {
				return fmt.Errorf("line %d: unknown field %q in repo entry", k.Line, k.Value)
			}
		}
	}
	type plain repoEntry
	return n.Decode((*plain)(e))
}

type file struct {
	Defaults overrides   `yaml:"defaults"`
	Repos    []repoEntry `yaml:"repos"`
}

var validMergeMethods = map[string]bool{"squash": true, "merge": true, "rebase": true}

// Builtin is the policy used when neither defaults nor a repo override it.
var Builtin = Policy{MergeMethod: "squash", MaxUpdateType: UpdateMinor, RequireChecks: true}

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
	defaults, err := apply(Builtin, f.Defaults)
	if err != nil {
		return nil, fmt.Errorf("defaults: %w", err)
	}
	cfg := &Config{}
	seen := map[string]bool{}
	for i, e := range f.Repos {
		owner, name, ok := strings.Cut(strings.TrimSpace(e.Repo), "/")
		if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
			return nil, fmt.Errorf("repos[%d]: %q is not owner/name", i, e.Repo)
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

func apply(base Policy, o overrides) (Policy, error) {
	p := base
	if o.MergeMethod != nil {
		m := strings.ToLower(*o.MergeMethod)
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
	return p, nil
}

// Filter keeps only the repos named in only (case-insensitive owner/name or
// bare name), including skipped ones, each once and in config order. An
// empty filter keeps every repo not marked skip.
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
		found := false
		for i, r := range c.Repos {
			if strings.ToLower(r.FullName()) == w || strings.ToLower(r.Name) == w {
				picked[i] = true
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("repo %q is not in the config", want)
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
