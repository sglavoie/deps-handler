package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"testing"

	"github.com/sglavoie/deps-merger/internal/policy"
	"github.com/sglavoie/deps-merger/internal/review"
)

func TestExitCode(t *testing.T) {
	for _, c := range []struct {
		err    error
		want   int
		stderr string
	}{
		{nil, 0, ""},
		{exitError{code: 1}, 1, ""},
		{fmt.Errorf("run: %w", errInterrupted), 130, "deps-merger: interrupted\n"},
		{fmt.Errorf("boom"), 1, "deps-merger: boom\n"},
	} {
		var stderr bytes.Buffer
		if got := exitCode(c.err, &stderr); got != c.want || stderr.String() != c.stderr {
			t.Errorf("exitCode(%v) = %d, %q; want %d, %q", c.err, got, stderr.String(), c.want, c.stderr)
		}
	}
}

func writeConfig(t *testing.T, yaml string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func run(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := execute(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestReposListsPolicy(t *testing.T) {
	cfg := writeConfig(t, "agent:\n  claude:\n    max_budget_usd: 2\nrepos:\n  - o/a\n  - repo: o/b\n    skip: true\n    require_checks: false\n")
	code, out, _ := run("repos", "-c", cfg)
	if code != 0 || !strings.Contains(out, "o/a   squash  minor") || !strings.Contains(out, "o/b") {
		t.Fatalf("code %d, output:\n%s", code, out)
	}
	if !strings.Contains(out, "no              -  ") {
		t.Errorf("no-checks merge should not apply where checks are optional:\n%s", out)
	}
	if !strings.Contains(out, "agent for --review: claude (default model, 15m timeout, up to $2 per review)") {
		t.Errorf("agent line missing:\n%s", out)
	}
}

func TestReposFilters(t *testing.T) {
	cfg := writeConfig(t, "repos:\n  - o/a\n  - repo: o/b\n    skip: true\n  - p/c\n")
	code, out, _ := run("repos", "-c", cfg, "--owner", "o")
	if code != 0 || !strings.Contains(out, "o/a") || !strings.Contains(out, "o/b") || strings.Contains(out, "p/c") {
		t.Fatalf("--owner: code %d, output:\n%s", code, out)
	}
	if code, out, _ := run("repos", "-c", cfg, "-r", "c"); code != 0 || strings.Contains(out, "o/a") || !strings.Contains(out, "p/c") {
		t.Fatalf("-r: code %d, output:\n%s", code, out)
	}
	if code, _, stderr := run("repos", "-c", cfg, "-r", "nope"); code != 2 || !strings.Contains(stderr, `repo "nope" is not in the config`) {
		t.Fatalf("unknown repo: code %d, %q", code, stderr)
	}
	if code, _, stderr := run("repos", "-c", cfg, "--owner", "o/a"); code != 2 || !strings.Contains(stderr, "use --repo") {
		t.Fatalf("owner/name: code %d, %q", code, stderr)
	}
}

func TestReposJSON(t *testing.T) {
	cfg := writeConfig(t, "agent:\n  timeout: 20m\n  claude:\n    max_budget_usd: 2\nrepos:\n  - o/a\n  - repo: o/b\n    skip: true\n    merge_without_checks_after: 2h\n")
	code, out, _ := run("repos", "-c", cfg, "--json")
	var doc struct {
		Repos []struct {
			Repo   string
			Skip   bool
			Policy struct {
				MergeMethod             string `json:"merge_method"`
				MaxUpdateType           string `json:"max_update_type"`
				MergeWithoutChecksAfter string `json:"merge_without_checks_after"`
			}
		}
		Agent struct {
			Use, Timeout string
			Claude       struct {
				MaxBudgetUSD float64 `json:"max_budget_usd"`
			}
		}
	}
	if err := json.Unmarshal([]byte(out), &doc); code != 0 || err != nil {
		t.Fatalf("code %d, %v:\n%s", code, err, out)
	}
	if len(doc.Repos) != 2 || doc.Repos[0].Repo != "o/a" || doc.Repos[0].Skip || doc.Repos[0].Policy.MergeMethod != "squash" || doc.Repos[0].Policy.MaxUpdateType != "minor" {
		t.Errorf("repos = %+v", doc.Repos)
	}
	if !doc.Repos[1].Skip || doc.Repos[1].Policy.MergeWithoutChecksAfter != "2h" {
		t.Errorf("skipped repo = %+v", doc.Repos[1])
	}
	if doc.Agent.Use != "claude" || doc.Agent.Timeout != "20m" || doc.Agent.Claude.MaxBudgetUSD != 2 {
		t.Errorf("agent = %+v", doc.Agent)
	}
}

func TestVerdicts(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	if code, out, _ := run("verdicts"); code != 0 || out != "no cached verdicts\n" {
		t.Fatalf("empty: code %d, %q", code, out)
	}
	cache, err := review.OpenCache(verdictsPath(filepath.Join(state, "deps-merger")))
	if err != nil {
		t.Fatal(err)
	}
	for k, approved := range map[string]bool{"o/a#1 lib@2.0.0": true, "o/a#2 x@3.0.0": false} {
		if err := cache.Put(k, "sha", "claude", policy.Verdict{Approved: approved, Summary: "because"}); err != nil {
			t.Fatal(err)
		}
	}
	code, out, _ := run("verdicts")
	if code != 0 || !strings.Contains(out, "o/a#1 lib@2.0.0  approved  claude") || !strings.Contains(out, "o/a#2 x@3.0.0    rejected") {
		t.Fatalf("list: code %d:\n%s", code, out)
	}
	if code, out, _ := run("verdicts", "forget", "o/a#2"); code != 0 || out != "o/a#2: forgot 1 verdict(s)\n" {
		t.Fatalf("forget: code %d, %q", code, out)
	}
	if code, _, stderr := run("verdicts", "forget", "o/a#2"); code != 1 || !strings.Contains(stderr, `no cached verdict matches "o/a#2"`) {
		t.Fatalf("forget again: code %d, %q", code, stderr)
	}
	if _, out, _ := run("verdicts"); strings.Contains(out, "o/a#2") {
		t.Fatalf("forgotten verdict still listed:\n%s", out)
	}
}

func TestRunErrorsExitCodes(t *testing.T) {
	cfg := writeConfig(t, "repos:\n  - repo: o/a\n    skip: true\n")
	active := writeConfig(t, "repos:\n  - o/a\n")
	missing := filepath.Join(t.TempDir(), "nope.yaml")
	for name, c := range map[string]struct {
		args   []string
		code   int
		stderr string
	}{
		"missing config": {[]string{"run", "-c", missing}, 1, "config not found"},
		"all skipped":    {[]string{"run", "-c", cfg}, 1, "every configured repo has skip: true"},
		"unknown repo":   {[]string{"run", "-c", cfg, "-r", "zzz"}, 2, `repo "zzz" is not in the config`},
		"ambiguous repo": {[]string{"run", "-c", writeConfig(t, "repos: [a/x, b/x]\n"), "-r", "x"}, 2, "ambiguous: x matches a/x, b/x"},
		// An unset variable in `-r "$REPO"` must not select every repo.
		"empty repo":      {[]string{"run", "-c", active, "-r", ""}, 2, "--repo needs a repo name"},
		"blank repo":      {[]string{"run", "-c", active, "-r", "a, "}, 2, "--repo has an empty repo name"},
		"blank --repo":    {[]string{"run", "-c", active, "--repo", " "}, 2, "--repo has an empty repo name"},
		"unknown owner":   {[]string{"run", "-c", cfg, "--owner", "zzz"}, 2, `owner "zzz" has no repo in the config`},
		"owner skipped":   {[]string{"run", "-c", cfg, "--owner", "o"}, 1, "every repo of the named owners has skip: true"},
		"empty owner":     {[]string{"run", "-c", active, "--owner", ""}, 2, "--owner needs an owner name"},
		"blank owner":     {[]string{"run", "-c", active, "--owner", "o, "}, 2, "--owner has an empty owner name"},
		"owner/name":      {[]string{"run", "-c", active, "--owner", "o/a"}, 2, `--owner takes an owner, not "o/a"; use --repo for one repo`},
		"bad repo name":   {[]string{"run", "-c", writeConfig(t, "repos: [acme/x.git]\n")}, 1, "drop the .git suffix"},
		"unexpected arg":  {[]string{"run", "extra"}, 2, "unknown command"},
		"unknown command": {[]string{"nope"}, 2, "unknown command"},
		"unknown flag":    {[]string{"run", "--nope"}, 2, "unknown flag"},
		"bad flag value":  {[]string{"run", "--max-reviews", "many"}, 2, "invalid argument"},
		"missing arg":     {[]string{"verdicts", "forget"}, 2, "requires at least 1 arg"},
		"unknown agent":   {[]string{"run", "-c", active, "--agent", "gemini"}, 2, `unknown agent "gemini" (want claude or codex)`},
		"bad limit":       {[]string{"run", "-c", active, "--review", "--max-reviews", "-1"}, 2, "--max-reviews must not be negative"},
		"limit alone":     {[]string{"run", "-c", active, "--max-reviews", "3"}, 2, "--max-reviews needs --review or --agent"},
		"no parallel":     {[]string{"run", "-c", active, "--parallel", "0"}, 2, "--parallel must be at least 1"},
		"no review slots": {[]string{"run", "-c", active, "--review", "--review-parallel", "0"}, 2, "--review-parallel must be at least 1"},
		"slots alone":     {[]string{"run", "-c", active, "--review-parallel", "3"}, 2, "--review-parallel needs --review or --agent"},
		// The command line is checked before the config is read.
		"limit before config": {[]string{"run", "-c", missing, "--max-reviews", "3"}, 2, "--max-reviews needs --review or --agent"},
		"repo before config":  {[]string{"run", "-c", missing, "-r", ""}, 2, "--repo needs a repo name"},
		"owner before config": {[]string{"run", "-c", missing, "--owner", ""}, 2, "--owner needs an owner name"},
	} {
		t.Run(name, func(t *testing.T) {
			code, _, stderr := run(c.args...)
			if code != c.code || !strings.Contains(stderr, c.stderr) {
				t.Fatalf("code %d, stderr %q; want %d containing %q", code, stderr, c.code, c.stderr)
			}
		})
	}
}

func TestFormatVersion(t *testing.T) {
	info := &debug.BuildInfo{Main: debug.Module{Version: "v1.2.3"}, Settings: []debug.BuildSetting{
		{Key: "vcs.revision", Value: "0123456789abcdef"},
		{Key: "vcs.time", Value: "2026-01-02T03:04:05Z"},
		{Key: "vcs.modified", Value: "true"},
	}}
	if got, want := formatVersion(info, "dev", "unknown", "unknown"), "v1.2.3 (0123456-dirty, built 2026-01-02T03:04:05Z)"; got != want {
		t.Errorf("dirty: got %q, want %q", got, want)
	}
	info.Settings[2].Value = "false"
	if got, want := formatVersion(info, "v9", "abc1234", "today"), "v9 (abc1234, built today)"; got != want {
		t.Errorf("ldflags: got %q, want %q", got, want)
	}
	pseudo := &debug.BuildInfo{Main: debug.Module{Version: "v0.2.1-0.20261024141842-3b9316762033+dirty"}, Settings: []debug.BuildSetting{
		{Key: "vcs.revision", Value: "3b9316762033aaaa"},
		{Key: "vcs.time", Value: "2026-10-24T14:18:42Z"},
		{Key: "vcs.modified", Value: "true"},
	}}
	if got, want := formatVersion(pseudo, "dev", "unknown", "unknown"), "v0.2.1-0.20261024141842-3b9316762033+dirty (built 2026-10-24T14:18:42Z)"; got != want {
		t.Errorf("pseudo-version: got %q, want %q", got, want)
	}
	if got, want := formatVersion(nil, "dev", "unknown", "unknown"), "dev (unknown, built unknown)"; got != want {
		t.Errorf("no build info: got %q, want %q", got, want)
	}
}

func TestInitWritesExampleOnce(t *testing.T) {
	old := ExampleConfig
	t.Cleanup(func() { ExampleConfig = old })
	ExampleConfig = []byte("repos:\n  - o/a\n")
	path := filepath.Join(t.TempDir(), "sub", "config.yaml")
	if code, out, stderr := run("init", "-c", path); code != 0 || !strings.Contains(out, "wrote "+path) {
		t.Fatalf("init: code %d, %q, %q", code, out, stderr)
	}
	if code, out, _ := run("repos", "-c", path); code != 0 || !strings.Contains(out, "o/a") {
		t.Fatalf("repos after init: code %d:\n%s", code, out)
	}
	if err := os.WriteFile(path, []byte("repos: [o/mine]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := run("init", "-c", path); code != 1 || !strings.Contains(stderr, "already exists") {
		t.Fatalf("second init: code %d, %q", code, stderr)
	}
	if data, _ := os.ReadFile(path); string(data) != "repos: [o/mine]\n" {
		t.Fatalf("init overwrote the config: %q", data)
	}
}

func TestInitStdoutWritesNothing(t *testing.T) {
	old := ExampleConfig
	t.Cleanup(func() { ExampleConfig = old })
	ExampleConfig = []byte("repos:\n  - o/a\n")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if code, out, _ := run("init", "-c", path, "--stdout"); code != 0 || out != "repos:\n  - o/a\n" {
		t.Fatalf("code %d, out %q", code, out)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("--stdout wrote %s: %v", path, err)
	}
}

func TestInitForceMovesConfigAside(t *testing.T) {
	old := ExampleConfig
	t.Cleanup(func() { ExampleConfig = old })
	ExampleConfig = []byte("repos:\n  - o/a\n")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("repos: [o/mine]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, stderr := run("init", "-c", path, "--force")
	if code != 0 || !strings.Contains(out, "moved the existing config to ") {
		t.Fatalf("code %d, out %q, stderr %q", code, out, stderr)
	}
	if data, _ := os.ReadFile(path); string(data) != "repos:\n  - o/a\n" {
		t.Fatalf("config = %q", data)
	}
	backups, _ := filepath.Glob(filepath.Join(dir, "config.yaml.*.bak"))
	if len(backups) != 1 {
		t.Fatalf("backups = %q", backups)
	}
	if data, _ := os.ReadFile(backups[0]); string(data) != "repos: [o/mine]\n" {
		t.Fatalf("backup = %q", data)
	}
	// With nothing to move aside, --force just writes.
	fresh := filepath.Join(dir, "fresh.yaml")
	if code, out, _ := run("init", "-c", fresh, "--force"); code != 0 || strings.Contains(out, "moved") {
		t.Fatalf("fresh: code %d, out %q", code, out)
	}
}

func TestInitDiscoverFillsRepos(t *testing.T) {
	old, oldDiscover := ExampleConfig, discoverRepos
	t.Cleanup(func() { ExampleConfig, discoverRepos = old, oldDiscover })
	ExampleConfig = []byte("defaults:\n  merge_method: squash\nrepos:\n  # - acme/example\n")
	var asked []string
	discoverRepos = func(_ context.Context, owners []string) ([]string, error) {
		asked = owners
		return []string{"o/a", "o/b"}, nil
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if code, _, stderr := run("init", "-c", path, "--discover", "--owner", " o "); code != 0 || !slices.Equal(asked, []string{"o"}) {
		t.Fatalf("code %d, asked %q, stderr %q", code, asked, stderr)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "acme/example") || !strings.HasSuffix(string(data), "  - o/a\n  - o/b\n") {
		t.Fatalf("config:\n%s", data)
	}
	if code, out, _ := run("repos", "-c", path); code != 0 || !strings.Contains(out, "o/b") {
		t.Fatalf("repos: code %d:\n%s", code, out)
	}

	discoverRepos = func(context.Context, []string) ([]string, error) { return nil, nil }
	if code, _, stderr := run("init", "--discover", "--stdout"); code != 1 || !strings.Contains(stderr, "found no repository") {
		t.Fatalf("none found: code %d, stderr %q", code, stderr)
	}
}

func TestInitFlagMistakes(t *testing.T) {
	for args, want := range map[string]string{
		"--stdout --force":       "--force needs a file to replace",
		"--owner o":              "--owner needs --discover",
		"--discover --owner o/r": "--owner takes an owner",
	} {
		if code, _, stderr := run(append([]string{"init"}, strings.Fields(args)...)...); code != 2 || !strings.Contains(stderr, want) {
			t.Errorf("%s: code %d, stderr %q", args, code, stderr)
		}
	}
}

func TestMissingConfigSuggestsInit(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.yaml")
	if _, _, stderr := run("repos", "-c", missing); !strings.Contains(stderr, "create one with `deps-merger init -c "+missing+"`") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestEmptyConfigSaysWhereToListRepos(t *testing.T) {
	cfg := writeConfig(t, "defaults:\n  max_update_type: patch\nrepos:\n  # - o/a\n")
	if code, _, stderr := run("run", "-c", cfg); code != 1 || !strings.Contains(stderr, cfg+" lists no repos yet; list yours under repos:, or fill them in with `deps-merger init -c "+cfg+" --discover --force`") {
		t.Fatalf("code %d, stderr %q", code, stderr)
	}
}

func TestCompletesRepoAndOwner(t *testing.T) {
	cfg := writeConfig(t, "repos:\n  - o/a\n  - o/b\n  - p/c\n")
	if _, out, _ := run("__complete", "-c", cfg, "run", "-r", ""); !strings.HasPrefix(out, "o/a\no/b\np/c\n:4\n") {
		t.Errorf("--repo: %q", out)
	}
	if _, out, _ := run("__complete", "-c", cfg, "repos", "--owner", ""); !strings.HasPrefix(out, "o\np\n:4\n") {
		t.Errorf("--owner: %q", out)
	}
}
