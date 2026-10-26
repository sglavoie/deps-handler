package config

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
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
    dependabot_commands: false
    merge_without_checks_after: "0"
`))
	if err != nil {
		t.Fatal(err)
	}
	a, b := cfg.Repos[0].Policy, cfg.Repos[1].Policy
	if a != (Policy{MergeMethod: "merge", MaxUpdateType: UpdateMinor, RequireChecks: true, AgentReview: true, DependabotCommands: true, MergeWithoutChecksAfter: 45 * time.Minute}) {
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
		"relative config_dir":          "agent: {claude: {config_dir: claude-cfg}}\nrepos: [a/b]",
		"relative codex home":          "agent: {codex: {home: ./codex}}\nrepos: [a/b]",
		"other user's home":            "agent: {claude: {config_dir: ~bob/.claude}}\nrepos: [a/b]",
	} {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestParseAcceptsEveryEntryField(t *testing.T) {
	in := "repos:\n  - {repo: a/b, merge_method: merge, max_update_type: patch, require_checks: true, agent_review: false, dependabot_commands: false, merge_without_checks_after: 5m, skip: true}\n"
	if _, err := Parse([]byte(in)); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"repo", "skip", "merge_method", "max_update_type", "require_checks", "agent_review", "dependabot_commands", "merge_without_checks_after"} {
		if !entryKeys[k] {
			t.Errorf("entryKeys lacks %q", k)
		}
	}
	if len(entryKeys) != 8 {
		t.Errorf("entryKeys = %v", entryKeys)
	}
}

func TestParseFollowsMergeKeys(t *testing.T) {
	in := `repos:
  - &strict {repo: a/b, max_update_type: patch, agent_review: false}
  - {<<: *strict, repo: c/d}
`
	cfg, err := Parse([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if p := cfg.Repos[1].Policy; cfg.Repos[1].Name != "d" || p.MaxUpdateType != UpdatePatch || p.AgentReview {
		t.Fatalf("repos = %+v", cfg.Repos)
	}

	// The config's own key check would catch an unknown field before it
	// could be merged anywhere, so check the merge is followed directly.
	var n yaml.Node
	if err := yaml.Unmarshal([]byte("x: &s {auto: true}\ny: {repo: a/b, <<: *s}"), &n); err != nil {
		t.Fatal(err)
	}
	if err := checkEntryKeys(n.Content[0].Content[3]); err == nil || !strings.Contains(err.Error(), `"auto"`) {
		t.Errorf("err = %v; an unknown merged field must be rejected", err)
	}
}

func TestFilter(t *testing.T) {
	cfg, err := Parse([]byte("repos: [acme/Billing, acme/Storefront]"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := cfg.Filter([]string{"storefront"}, nil, false)
	if err != nil || len(got) != 1 || got[0].Name != "Storefront" {
		t.Fatalf("Filter(storefront) = %v, %v", got, err)
	}
	if _, err := cfg.Filter([]string{"nope"}, nil, false); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("Filter(nope) err = %v", err)
	}
}

func TestFilterSkip(t *testing.T) {
	cfg, err := Parse([]byte("repos: [a/one, {repo: a/two, skip: true}]"))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := cfg.Filter(nil, nil, false)
	if len(got) != 1 || got[0].Name != "one" {
		t.Fatalf("Filter(nil) = %v", got)
	}
	got, _ = cfg.Filter([]string{"two"}, nil, false)
	if len(got) != 1 || !got[0].Skip {
		t.Fatalf("naming a skipped repo should still select it: %v", got)
	}
}

func TestFilterOwner(t *testing.T) {
	cfg, err := Parse([]byte("repos: [acme/one, other/two, {repo: acme/three, skip: true}, Acme/four]"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := cfg.Filter(nil, []string{" ACME "}, false)
	if err != nil || len(got) != 2 || got[0].Name != "one" || got[1].Name != "four" {
		t.Fatalf("Filter(owner acme) = %v, %v; want acme/one and Acme/four, skipping acme/three", got, err)
	}
	// Repos and owners add up, in config order; a skipped repo named by
	// --repo still runs.
	got, err = cfg.Filter([]string{"three", "acme/one"}, []string{"other"}, false)
	if err != nil || len(got) != 3 || got[0].Name != "one" || got[1].Name != "two" || got[2].Name != "three" {
		t.Fatalf("Filter(three, acme/one; other) = %v, %v", got, err)
	}
	if _, err := cfg.Filter(nil, []string{"nobody"}, false); err == nil || err.Error() != `owner "nobody" has no repo in the config` {
		t.Fatalf("Filter(owner nobody) err = %v", err)
	}
	// An owner whose every repo is skipped selects nothing, without error.
	cfg, err = Parse([]byte("repos: [a/one, {repo: b/two, skip: true}]"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := cfg.Filter(nil, []string{"b"}, false); err != nil || len(got) != 0 {
		t.Fatalf("Filter(owner b) = %v, %v", got, err)
	}
}

func TestFilterIncludeSkipped(t *testing.T) {
	cfg, err := Parse([]byte("repos: [a/one, {repo: a/two, skip: true}, {repo: b/three, skip: true}]"))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := cfg.Filter(nil, nil, true)
	if len(got) != 3 {
		t.Fatalf("Filter(include skipped) = %v; want every repo", got)
	}
	got, _ = cfg.Filter(nil, []string{"a"}, true)
	if len(got) != 2 || got[0].Name != "one" || got[1].Name != "two" {
		t.Fatalf("Filter(owner a, include skipped) = %v", got)
	}
	got, _ = cfg.Filter([]string{"one"}, nil, true)
	if len(got) != 1 || got[0].Name != "one" {
		t.Fatalf("Filter(one, include skipped) = %v; --repo alone still picks only what it names", got)
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
	got, err := cfg.Filter([]string{"Storefront", "acme/storefront", " billing "}, nil, false)
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
	want := `{"merge_method":"squash","max_update_type":"minor","require_checks":true,"agent_review":true,"dependabot_commands":true,"merge_without_checks_after":"10m"}`
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
	if _, err := cfg.Filter([]string{"docs"}, nil, false); err == nil || err.Error() != "ambiguous: docs matches acme/docs, other/docs; use owner/name" {
		t.Fatalf("Filter(docs) err = %v", err)
	}
	got, err := cfg.Filter([]string{"other/docs", "api"}, nil, false)
	if err != nil || len(got) != 2 || got[0].FullName() != "other/docs" || got[1].FullName() != "acme/api" {
		t.Fatalf("Filter = %v, %v", got, err)
	}
}

func TestRepoErrorsGiveLines(t *testing.T) {
	for in, want := range map[string]string{
		"repos:\n  - o/a\n  -\n":                                      "repos: line 3: empty entry",
		"repos:\n  - o/a\n  - o/a.git\n":                              "repos: line 3: \"o/a.git\": drop the .git suffix",
		"repos:\n  - o/a\n  - O/A\n":                                  "repos: line 3: O/A listed twice (first on line 2)",
		"repos:\n  - o/a\n  - repo: o/b\n    merge_method: rebased\n": "repos: line 3: o/b: invalid merge_method",
		"repos:\n  - o/a\n  - repo: o/b\n    color: blue\n":           "line 4: unknown field \"color\"",
	} {
		if _, err := Parse([]byte(in)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", in, err, want)
		}
	}
}
