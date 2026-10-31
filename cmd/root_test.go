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

	"github.com/sglavoie/deps-handler/pkg/policy"
	"github.com/sglavoie/deps-handler/pkg/review"
)

func TestExitCode(t *testing.T) {
	for _, c := range []struct {
		err    error
		want   int
		stderr string
	}{
		{nil, 0, ""},
		{exitError{code: 1}, 1, ""},
		{fmt.Errorf("run: %w", errInterrupted), 130, "deps-handler: interrupted\n"},
		{fmt.Errorf("boom"), 1, "deps-handler: boom\n"},
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

func TestRestoreBackupPreservesConcurrentConfig(t *testing.T) {
	path := writeConfig(t, "original config")
	backup, err := moveAside(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeNew(path, []byte("concurrent config"), "init --force"); err != nil {
		t.Fatal(err)
	}
	if err := restoreBackup(path, backup); err == nil {
		t.Error("restoring over another process's config must fail")
	}
	for file, want := range map[string]string{path: "concurrent config", backup: "original config"} {
		if got, err := os.ReadFile(file); err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", file, got, err, want)
		}
	}
}

func TestRestoreBackup(t *testing.T) {
	path := writeConfig(t, "original config")
	backup, err := moveAside(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := restoreBackup(path, backup); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "original config" {
		t.Fatalf("restored config = %q, %v", got, err)
	}
	if _, err := os.Lstat(backup); !os.IsNotExist(err) {
		t.Fatalf("backup should have been removed: %v", err)
	}
}

func TestMoveAsideKeepsBackupWhenRunsOverlap(t *testing.T) {
	path := writeConfig(t, "original config")
	start := make(chan struct{})
	type result struct {
		backup string
		err    error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			<-start
			backup, err := moveAside(path)
			results <- result{backup, err}
		}()
	}
	close(start)
	a, b := <-results, <-results
	backup := a.backup
	if backup == "" {
		backup = b.backup
	}
	if backup == "" {
		t.Fatalf("neither run made a backup: %+v, %+v", a, b)
	}
	if a.backup != "" && b.backup != "" {
		t.Fatalf("both runs moved one config: %+v, %+v", a, b)
	}
	if data, err := os.ReadFile(backup); err != nil || string(data) != "original config" {
		t.Fatalf("backup = %q, %v; original config was lost", data, err)
	}
}

func TestMoveAsidePreservesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.yaml")
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(target, []byte("original config"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target.yaml", path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	backup, err := moveAside(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.Readlink(backup); err != nil || got != "target.yaml" {
		t.Fatalf("backup symlink = %q, %v", got, err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("original symlink still exists: %v", err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "original config" {
		t.Fatalf("target = %q, %v", got, err)
	}
	if err := restoreBackup(path, backup); err != nil {
		t.Fatal(err)
	}
	if got, err := os.Readlink(path); err != nil || got != "target.yaml" {
		t.Fatalf("restored symlink = %q, %v", got, err)
	}
}

func run(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := execute(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestReposListsPolicy(t *testing.T) {
	cfg := writeConfig(t, "agent:\n  claude:\n    max_budget_usd: 2\nrepos:\n  - o/a\n  - repo: o/b\n    skip: true\n    require_checks: false\n    mode: report\n")
	code, out, _ := run("repos", "-c", cfg, "--list")
	if code != 0 || !strings.Contains(out, "o/a   merge   squash  minor") || !strings.Contains(out, "o/b   report  squash") {
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
	cache, err := review.OpenCache(verdictsPath(filepath.Join(state, "deps-handler")))
	if err != nil {
		t.Fatal(err)
	}
	for k, approved := range map[string]bool{"o/a#1 lib@2.0.0": true, "o/a#2 x@3.0.0": false} {
		if err := cache.Put(k, "sha", "claude", policy.Verdict{Approved: approved, Summary: "because"}); err != nil {
			t.Fatal(err)
		}
	}
	code, out, _ := run("verdicts", "--list")
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
		"missing config": {[]string{"dependabot", "-c", missing}, 1, "config not found"},
		"all skipped":    {[]string{"dependabot", "-c", cfg}, 1, "every configured repo has skip: true"},
		"unknown repo":   {[]string{"dependabot", "-c", cfg, "-r", "zzz"}, 2, `repo "zzz" is not in the config`},
		"ambiguous repo": {[]string{"dependabot", "-c", writeConfig(t, "repos: [a/x, b/x]\n"), "-r", "x"}, 2, "ambiguous: x matches a/x, b/x"},
		// An unset variable in `-r "$REPO"` must not select every repo.
		"empty repo":      {[]string{"dependabot", "-c", active, "-r", ""}, 2, "--repo needs a repo name"},
		"empty agent":     {[]string{"dependabot", "-c", active, "--agent", ""}, 2, "--agent needs an agent name"},
		"empty config":    {[]string{"dependabot", "-c", ""}, 2, "--config needs a file path"},
		"empty init -c":   {[]string{"init", "--force", "-c", ""}, 2, "--config needs a file path"},
		"blank repo":      {[]string{"dependabot", "-c", active, "-r", "a, "}, 2, "--repo has an empty repo name"},
		"blank --repo":    {[]string{"dependabot", "-c", active, "--repo", " "}, 2, "--repo has an empty repo name"},
		"unknown owner":   {[]string{"dependabot", "-c", cfg, "--owner", "zzz"}, 2, `owner "zzz" has no repo in the config`},
		"owner skipped":   {[]string{"dependabot", "-c", cfg, "--owner", "o"}, 1, "every repo of the named owners has skip: true"},
		"empty owner":     {[]string{"dependabot", "-c", active, "--owner", ""}, 2, "--owner needs an owner name"},
		"blank owner":     {[]string{"dependabot", "-c", active, "--owner", "o, "}, 2, "--owner has an empty owner name"},
		"owner/name":      {[]string{"dependabot", "-c", active, "--owner", "o/a"}, 2, `--owner takes an owner, not "o/a"; use --repo for one repo`},
		"bad repo name":   {[]string{"dependabot", "-c", writeConfig(t, "repos: [acme/x.git]\n")}, 1, "drop the .git suffix"},
		"unexpected arg":  {[]string{"dependabot", "extra"}, 2, "unknown command"},
		"unknown command": {[]string{"nope"}, 2, "unknown command"},
		"removed alias":   {[]string{"run"}, 2, `unknown command "run"`},
		"unknown flag":    {[]string{"dependabot", "--nope"}, 2, "unknown flag"},
		"bad flag value":  {[]string{"dependabot", "--max-reviews", "many"}, 2, "invalid argument"},
		"missing arg":     {[]string{"verdicts", "forget"}, 2, "requires at least 1 arg"},
		"unknown agent":   {[]string{"dependabot", "-c", active, "--agent", "gemini"}, 2, `unknown agent "gemini" (want claude, codex or api)`},
		"bad limit":       {[]string{"dependabot", "-c", active, "--review", "--max-reviews", "-1"}, 2, "--max-reviews must not be negative"},
		"limit alone":     {[]string{"dependabot", "-c", active, "--max-reviews", "3"}, 2, "--max-reviews needs --review or --agent"},
		"no parallel":     {[]string{"dependabot", "-c", active, "--parallel", "0"}, 2, "--parallel must be at least 1"},
		"no review slots": {[]string{"dependabot", "-c", active, "--review", "--review-parallel", "0"}, 2, "--review-parallel must be at least 1"},
		"slots alone":     {[]string{"dependabot", "-c", active, "--review-parallel", "3"}, 2, "--review-parallel needs --review or --agent"},
		"two formats":     {[]string{"dependabot", "-c", missing, "--json", "--markdown"}, 2, "--markdown cannot be used with"},
		// The command line is checked before the config is read.
		"limit before config": {[]string{"dependabot", "-c", missing, "--max-reviews", "3"}, 2, "--max-reviews needs --review or --agent"},
		"repo before config":  {[]string{"dependabot", "-c", missing, "-r", ""}, 2, "--repo needs a repo name"},
		"owner before config": {[]string{"dependabot", "-c", missing, "--owner", ""}, 2, "--owner needs an owner name"},
		// pflag drops an empty value given beside others.
		"empty repo among others":  {[]string{"dependabot", "-c", missing, "-r", "", "-r", "a"}, 2, "--repo has an empty repo name"},
		"empty owner among others": {[]string{"dependabot", "-c", missing, "--owner", "o", "--owner", ""}, 2, "--owner has an empty owner name"},
		"repos empty repo":         {[]string{"repos", "-c", missing, "-r", "a", "-r", ""}, 2, "--repo has an empty repo name"},
		"unknown help topic":       {[]string{"help", "nope"}, 2, `unknown help topic "nope"`},
		"unknown completion shell": {[]string{"completion", "nope"}, 2, `unknown command "nope"`},
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
	if code, out, stderr := run("init", "-c", path); code != 0 || !strings.Contains(out, "wrote "+path) || !strings.Contains(out, "`deps-handler repos -c "+path+"`") {
		t.Fatalf("init: code %d, %q, %q", code, out, stderr)
	}
	if code, out, _ := run("repos", "-c", path); code != 0 || !strings.Contains(out, "o/a") {
		t.Fatalf("repos after init: code %d:\n%s", code, out)
	}
	if err := os.WriteFile(path, []byte("repos: [o/mine]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := run("init", "-c", path); code != 1 || !strings.Contains(stderr, "already exists") || !strings.Contains(stderr, "`deps-handler init --force -c "+path+"`") {
		t.Fatalf("second init: code %d, %q", code, stderr)
	}
	if data, _ := os.ReadFile(path); string(data) != "repos: [o/mine]\n" {
		t.Fatalf("init overwrote the config: %q", data)
	}
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		"/home/me/.config/deps-handler/config.yaml": "/home/me/.config/deps-handler/config.yaml",
		"/home/me/my configs/c.yaml":                "'/home/me/my configs/c.yaml'",
		"it's.yaml":                                 `'it'\''s.yaml'`,
		"$HOME/c.yaml":                              "'$HOME/c.yaml'",
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
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
	// A directory named by mistake is no config to move aside.
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(filepath.Join(sub, "keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := run("init", "-c", sub, "--force"); code != 1 || !strings.Contains(stderr, "is a directory") {
		t.Fatalf("directory: code %d, stderr %q", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(sub, "keep")); err != nil {
		t.Fatalf("directory moved aside: %v", err)
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
	if code, out, stderr := run("init", "-c", path, "--discover", "--owner", " o "); code != 0 || !slices.Equal(asked, []string{"o"}) || !strings.Contains(out, "`deps-handler dependabot -c "+path+"`") {
		t.Fatalf("code %d, asked %q, output %q, stderr %q", code, asked, out, stderr)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "acme/example") || !strings.HasSuffix(string(data), "  - o/a\n  - o/b\n") {
		t.Fatalf("config:\n%s", data)
	}
	if code, out, _ := run("repos", "-c", path); code != 0 || !strings.Contains(out, "o/b") {
		t.Fatalf("repos: code %d:\n%s", code, out)
	}
	// The advice to replace it must discover again, not write the example.
	if code, _, stderr := run("init", "-c", path, "--discover", "--owner", " o "); code != 1 || !strings.Contains(stderr, "`deps-handler init --force --discover --owner o -c "+path+"`") {
		t.Fatalf("again: code %d, stderr %q", code, stderr)
	}

	discoverRepos = func(context.Context, []string) ([]string, error) { return nil, nil }
	if code, _, stderr := run("init", "--discover", "--stdout"); code != 1 || !strings.Contains(stderr, "found no repository") {
		t.Fatalf("none found: code %d, stderr %q", code, stderr)
	}
}

func TestInitFlagMistakes(t *testing.T) {
	for args, want := range map[string]string{
		"--stdout --force":              "--force needs a file to replace",
		"--owner o":                     "--owner needs --discover",
		"--discover --owner o/r":        "--owner takes an owner",
		"--discover --owner o --owner=": "--owner has an empty owner name",
	} {
		if code, _, stderr := run(append([]string{"init"}, strings.Fields(args)...)...); code != 2 || !strings.Contains(stderr, want) {
			t.Errorf("%s: code %d, stderr %q", args, code, stderr)
		}
	}
}

func TestMissingConfigSuggestsInit(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.yaml")
	if _, _, stderr := run("repos", "-c", missing); !strings.Contains(stderr, "create one with `deps-handler init -c "+missing+"`") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestEmptyConfigSaysWhereToListRepos(t *testing.T) {
	cfg := writeConfig(t, "defaults:\n  max_update_type: patch\nrepos:\n  # - o/a\n")
	if code, _, stderr := run("dependabot", "-c", cfg); code != 1 || !strings.Contains(stderr, cfg+" lists no repos yet; list yours under repos:, or fill them in with `deps-handler init -c "+cfg+" --discover --force`") {
		t.Fatalf("code %d, stderr %q", code, stderr)
	}
}

func TestCompletesRepoAndOwner(t *testing.T) {
	cfg := writeConfig(t, "repos:\n  - o/a\n  - o/b\n  - p/c\n")
	if _, out, _ := run("__complete", "-c", cfg, "dependabot", "-r", ""); !strings.HasPrefix(out, "o/a\no/b\np/c\n:4\n") {
		t.Errorf("--repo: %q", out)
	}
	if _, out, _ := run("__complete", "-c", cfg, "repos", "--owner", ""); !strings.HasPrefix(out, "o\np\n:4\n") {
		t.Errorf("--owner: %q", out)
	}
}

func TestCompletionWritesToOutput(t *testing.T) {
	if code, out, stderr := run("completion", "bash"); code != 0 || !strings.Contains(out, "bash completion V2 for deps-handler") {
		t.Fatalf("code %d, stderr %q, output starts %.60q", code, stderr, out)
	}
}
