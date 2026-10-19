package config

import (
	"strings"
	"testing"
	"time"
)

func TestParseAppliesDefaultsAndOverrides(t *testing.T) {
	cfg, err := Parse([]byte(`
defaults:
  merge_method: merge
repos:
  - acme/Billing
  - repo: acme/auth-worker
    require_checks: false
    max_update_type: major
    agent_review: false
`))
	if err != nil {
		t.Fatal(err)
	}
	a, b := cfg.Repos[0].Policy, cfg.Repos[1].Policy
	if a != (Policy{MergeMethod: "merge", MaxUpdateType: UpdateMinor, RequireChecks: true, AgentReview: true}) {
		t.Errorf("Billing policy = %+v", a)
	}
	if b != (Policy{MergeMethod: "merge", MaxUpdateType: UpdateMajor, RequireChecks: false, AgentReview: false}) {
		t.Errorf("worker policy = %+v", b)
	}
}

func TestParseRejectsBadInput(t *testing.T) {
	for name, in := range map[string]string{
		"no repos":      "repos: []",
		"bad name":      "repos: [acme]",
		"duplicate":     "repos: [a/b, A/B]",
		"bad method":    "repos: [{repo: a/b, merge_method: yolo}]",
		"bad update":    "repos: [{repo: a/b, max_update_type: huge}]",
		"unknown field": "repos: [{repo: a/b, auto: true}]",
		"unknown top":   "repo: [a/b]",
		"empty":         "",
		"only comments": "# nothing here\n",
		"bad agent":     "agent: {use: gemini}\nrepos: [a/b]",
		"bad timeout":   "agent: {timeout: soon}\nrepos: [a/b]",
		"zero timeout":  "agent: {timeout: 0s}\nrepos: [a/b]",
		"agent field":   "agent: {claude: {config: x}}\nrepos: [a/b]",
		"bad budget":    "agent: {claude: {max_budget_usd: -1}}\nrepos: [a/b]",
	} {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestParseAcceptsEveryEntryField(t *testing.T) {
	in := "repos:\n  - {repo: a/b, merge_method: merge, max_update_type: patch, require_checks: false, agent_review: false, skip: true}\n"
	if _, err := Parse([]byte(in)); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"repo", "skip", "merge_method", "max_update_type", "require_checks", "agent_review"} {
		if !entryKeys[k] {
			t.Errorf("entryKeys lacks %q", k)
		}
	}
	if len(entryKeys) != 6 {
		t.Errorf("entryKeys = %v", entryKeys)
	}
}

func TestFilter(t *testing.T) {
	cfg, err := Parse([]byte("repos: [acme/Billing, acme/Storefront]"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := cfg.Filter([]string{"storefront"})
	if err != nil || len(got) != 1 || got[0].Name != "Storefront" {
		t.Fatalf("Filter(storefront) = %v, %v", got, err)
	}
	if _, err := cfg.Filter([]string{"nope"}); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("Filter(nope) err = %v", err)
	}
}

func TestFilterSkip(t *testing.T) {
	cfg, err := Parse([]byte("repos: [a/one, {repo: a/two, skip: true}]"))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := cfg.Filter(nil)
	if len(got) != 1 || got[0].Name != "one" {
		t.Fatalf("Filter(nil) = %v", got)
	}
	got, _ = cfg.Filter([]string{"two"})
	if len(got) != 1 || !got[0].Skip {
		t.Fatalf("naming a skipped repo should still select it: %v", got)
	}
}

func TestParseEmptySaysSo(t *testing.T) {
	if _, err := Parse(nil); err == nil || !strings.Contains(err.Error(), "config is empty") {
		t.Fatalf("err = %v", err)
	}
}

func TestFilterDeduplicates(t *testing.T) {
	cfg, err := Parse([]byte("repos: [acme/Billing, acme/Storefront]"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := cfg.Filter([]string{"Storefront", "acme/storefront", "billing"})
	if err != nil || len(got) != 2 || got[0].Name != "Billing" || got[1].Name != "Storefront" {
		t.Fatalf("Filter = %v, %v", got, err)
	}
}

func TestParseAgent(t *testing.T) {
	cfg, err := Parse([]byte("repos: [a/b]"))
	if err != nil {
		t.Fatal(err)
	}
	if a := cfg.Agent; a.Use != "claude" || a.Timeout != DefaultAgentTimeout {
		t.Errorf("default agent = %+v", a)
	}

	t.Setenv("HOME", "/home/me")
	cfg, err = Parse([]byte(`
agent:
  use: Codex
  timeout: 5m
  claude:
    model: opus
    config_dir: ~/.claude-alt
    max_budget_usd: 2.5
  codex:
    model: gpt-5
    home: /opt/codex
repos: [a/b]
`))
	if err != nil {
		t.Fatal(err)
	}
	want := Agent{
		Use: "codex", Timeout: 5 * time.Minute,
		Claude: Claude{Model: "opus", ConfigDir: "/home/me/.claude-alt", MaxBudgetUSD: 2.5},
		Codex:  Codex{Model: "gpt-5", Home: "/opt/codex"},
	}
	if cfg.Agent != want {
		t.Errorf("agent = %+v\nwant %+v", cfg.Agent, want)
	}
}
