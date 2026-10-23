package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/sglavoie/deps-merger/internal/config"
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
	if !strings.Contains(out, "false           -  ") {
		t.Errorf("no-checks merge should not apply where checks are optional:\n%s", out)
	}
	if !strings.Contains(out, "agent for --review: claude (default model, 15m timeout, up to $2 per review)") {
		t.Errorf("agent line missing:\n%s", out)
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
		"bad repo name":   {[]string{"run", "-c", writeConfig(t, "repos: [acme/x.git]\n")}, 1, "drop the .git suffix"},
		"unexpected arg":  {[]string{"run", "extra"}, 2, "unknown command"},
		"unknown command": {[]string{"nope"}, 2, "unknown command"},
		"unknown flag":    {[]string{"run", "--nope"}, 2, "unknown flag"},
		"bad flag value":  {[]string{"run", "--max-reviews", "many"}, 2, "invalid argument"},
		"missing arg":     {[]string{"verdicts", "forget"}, 2, "requires at least 1 arg"},
		"unknown agent":   {[]string{"run", "-c", active, "--agent", "gemini"}, 2, `unknown agent "gemini" (want claude or codex)`},
		"bad limit":       {[]string{"run", "-c", active, "--review", "--max-reviews", "-1"}, 2, "--max-reviews must not be negative"},
		"limit alone":     {[]string{"run", "-c", active, "--max-reviews", "3"}, 2, "--max-reviews needs --review or --agent"},
		// The command line is checked before the config is read.
		"limit before config": {[]string{"run", "-c", missing, "--max-reviews", "3"}, 2, "--max-reviews needs --review or --agent"},
		"repo before config":  {[]string{"run", "-c", missing, "-r", ""}, 2, "--repo needs a repo name"},
	} {
		t.Run(name, func(t *testing.T) {
			code, _, stderr := run(c.args...)
			if code != c.code || !strings.Contains(stderr, c.stderr) {
				t.Fatalf("code %d, stderr %q; want %d containing %q", code, stderr, c.code, c.stderr)
			}
		})
	}
}

func TestNewAgent(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	cfg := config.Agent{Use: "claude", Codex: config.Codex{Model: "gpt-5"}}
	if _, err := newAgent(cfg, ""); err == nil || err.Error() != "claude is not installed or not on PATH" {
		t.Fatalf("err = %v", err)
	}
	a, err := newAgent(cfg, "Codex")
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := a.(review.Codex); !ok || c.Model != "gpt-5" {
		t.Fatalf("agent = %#v", a)
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
	if got, want := formatVersion(nil, "dev", "unknown", "unknown"), "dev (unknown, built unknown)"; got != want {
		t.Errorf("no build info: got %q, want %q", got, want)
	}
}
