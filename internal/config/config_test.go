package config

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestParseAppliesDefaultsAndOverrides(t *testing.T) {
	cfg, err := Parse([]byte(`
defaults:
  merge_method: merge
  merge_without_checks_after: 45m
repos:
  - acme/Billing
  - repo: acme/auth-worker
    require_checks: false
    max_update_type: major
    agent_review: false
    merge_without_checks_after: "0"
`))
	if err != nil {
		t.Fatal(err)
	}
	a, b := cfg.Repos[0].Policy, cfg.Repos[1].Policy
	if a != (Policy{MergeMethod: "merge", MaxUpdateType: UpdateMinor, RequireChecks: true, AgentReview: true, MergeWithoutChecksAfter: 45 * time.Minute}) {
		t.Errorf("Billing policy = %+v", a)
	}
	if b != (Policy{MergeMethod: "merge", MaxUpdateType: UpdateMajor, RequireChecks: false, AgentReview: false}) {
		t.Errorf("worker policy = %+v", b)
	}
}

func TestParseRejectsBadInput(t *testing.T) {
	for name, in := range map[string]string{
		"no repos":                     "repos: []",
		"bad name":                     "repos: [acme]",
		"duplicate":                    "repos: [a/b, A/B]",
		"bad method":                   "repos: [{repo: a/b, merge_method: yolo}]",
		"bad update":                   "repos: [{repo: a/b, max_update_type: huge}]",
		"unknown field":                "repos: [{repo: a/b, auto: true}]",
		"unknown top":                  "repo: [a/b]",
		"empty":                        "",
		"only comments":                "# nothing here\n",
		"bad agent":                    "agent: {use: gemini}\nrepos: [a/b]",
		"bad timeout":                  "agent: {timeout: soon}\nrepos: [a/b]",
		"zero timeout":                 "agent: {timeout: 0s}\nrepos: [a/b]",
		"agent field":                  "agent: {claude: {config: x}}\nrepos: [a/b]",
		"bad budget":                   "agent: {claude: {max_budget_usd: -1}}\nrepos: [a/b]",
		"bad no-checks":                "repos: [{repo: a/b, merge_without_checks_after: soon}]",
		"neg no-checks":                "repos: [{repo: a/b, merge_without_checks_after: -1m}]",
		"space in name":                `repos: ["acme/ z"]`,
		"space owner":                  `repos: ["ac me/z"]`,
		"bad owner":                    "repos: [ac_me/z]",
		"dash owner":                   "repos: [-acme/z]",
		"bad char":                     "repos: [acme/z!]",
		"dot name":                     "repos: [acme/..]",
		"git suffix":                   "repos: [acme/x.git]",
		"no-checks unused here":        "repos: [{repo: a/b, require_checks: false, merge_without_checks_after: 10m}]",
		"no-checks unused in defaults": "defaults: {require_checks: false, merge_without_checks_after: 10m}\nrepos: [a/b]",
		"no-checks unused by default":  "defaults: {require_checks: false}\nrepos: [{repo: a/b, merge_without_checks_after: 10m}]",
		"second document":              "repos: [a/b]\n---\nrepos: [c/d]\n",
		"null repo":                    "repos: [a/b, ~]",
		"empty repo item":              "repos:\n  - a/b\n  -\n",
		"nan budget":                   "agent: {claude: {max_budget_usd: .nan}}\nrepos: [a/b]",
		"inf budget":                   "agent: {claude: {max_budget_usd: .inf}}\nrepos: [a/b]",
	} {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestParseAcceptsEveryEntryField(t *testing.T) {
	in := "repos:\n  - {repo: a/b, merge_method: merge, max_update_type: patch, require_checks: true, agent_review: false, merge_without_checks_after: 5m, skip: true}\n"
	if _, err := Parse([]byte(in)); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"repo", "skip", "merge_method", "max_update_type", "require_checks", "agent_review", "merge_without_checks_after"} {
		if !entryKeys[k] {
			t.Errorf("entryKeys lacks %q", k)
		}
	}
	if len(entryKeys) != 7 {
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

func TestPolicyJSONWritesDurations(t *testing.T) {
	p := Builtin
	p.MergeWithoutChecksAfter = 10 * time.Minute
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"merge_method":"squash","max_update_type":"minor","require_checks":true,"agent_review":true,"merge_without_checks_after":"10m"}`
	if string(data) != want {
		t.Fatalf("got %s\nwant %s", data, want)
	}
}

func TestParseRepoNames(t *testing.T) {
	cfg, err := Parse([]byte("repos: [\"  acme/x.y_z-1  \", my-org/.github, a/b.gitignore]"))
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"acme/x.y_z-1", "my-org/.github", "a/b.gitignore"} {
		if got := cfg.Repos[i].FullName(); got != want {
			t.Errorf("repos[%d] = %q, want %q", i, got, want)
		}
	}
	if _, err := Parse([]byte("repos: [acme/x.git]")); err == nil || !strings.Contains(err.Error(), "drop the .git suffix") {
		t.Errorf(".git suffix err = %v", err)
	}
}

func TestParseNormalizesMergeMethod(t *testing.T) {
	cfg, err := Parse([]byte(`repos: [{repo: a/b, merge_method: " Rebase ", max_update_type: " Major "}]`))
	if err != nil {
		t.Fatal(err)
	}
	if p := cfg.Repos[0].Policy; p.MergeMethod != "rebase" || p.MaxUpdateType != UpdateMajor {
		t.Fatalf("policy = %+v", p)
	}
}

func TestParseNoChecksErrorSaysWhy(t *testing.T) {
	_, err := Parse([]byte("repos: [{repo: a/b, require_checks: false, merge_without_checks_after: 10m}]"))
	if err == nil || !strings.Contains(err.Error(), "a/b: merge_without_checks_after \"10m\" has no effect with require_checks: false") {
		t.Fatalf("err = %v", err)
	}
	// A default meant for the other repos does not stop one turning checks
	// off, and that repo's policy does not show it.
	cfg, err := Parse([]byte("defaults: {merge_without_checks_after: 45m}\nrepos: [a/b, {repo: a/c, require_checks: false}]"))
	if err != nil {
		t.Fatalf("inherited default: %v", err)
	}
	if got := cfg.Repos[1].Policy.MergeWithoutChecksAfter; got != 0 {
		t.Errorf("a/c merge_without_checks_after = %s, want 0 with checks off", got)
	}
	if got := cfg.Repos[0].Policy.MergeWithoutChecksAfter; got != 45*time.Minute {
		t.Errorf("a/b merge_without_checks_after = %s, want 45m", got)
	}
}

func TestParseNoChecksWaitsAtLeastChecksGrace(t *testing.T) {
	cfg, err := Parse([]byte("defaults: {merge_without_checks_after: 10m}\nrepos: [a/b, {repo: a/c, merge_without_checks_after: 1h}, {repo: a/d, merge_without_checks_after: \"0\"}]"))
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []time.Duration{ChecksGrace, time.Hour, 0} {
		if got := cfg.Repos[i].Policy.MergeWithoutChecksAfter; got != want {
			t.Errorf("%s: merge_without_checks_after = %s, want %s", cfg.Repos[i].FullName(), got, want)
		}
	}
}

func TestParseRejectsSecondDocument(t *testing.T) {
	_, err := Parse([]byte("repos: [a/b]\n---\nrepos: [c/d]\n"))
	if err == nil || !strings.Contains(err.Error(), "more than one YAML document") {
		t.Fatalf("err = %v", err)
	}
}

func TestFilterAmbiguousName(t *testing.T) {
	cfg, err := Parse([]byte("repos: [acme/docs, other/docs, acme/api]"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.Filter([]string{"docs"}); err == nil || err.Error() != "ambiguous: docs matches acme/docs, other/docs; use owner/name" {
		t.Fatalf("Filter(docs) err = %v", err)
	}
	got, err := cfg.Filter([]string{"other/docs", "api"})
	if err != nil || len(got) != 2 || got[0].FullName() != "other/docs" || got[1].FullName() != "acme/api" {
		t.Fatalf("Filter = %v, %v", got, err)
	}
}
