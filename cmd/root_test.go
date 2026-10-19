package cmd

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
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
		if err := cache.Put(k, "claude", policy.Verdict{Approved: approved, Summary: "because"}); err != nil {
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

func TestRunErrorsExitOne(t *testing.T) {
	cfg := writeConfig(t, "repos:\n  - repo: o/a\n    skip: true\n")
	active := writeConfig(t, "repos:\n  - o/a\n")
	for name, c := range map[string]struct {
		args   []string
		stderr string
	}{
		"missing config": {[]string{"run", "-c", filepath.Join(t.TempDir(), "nope.yaml")}, "config not found"},
		"all skipped":    {[]string{"run", "-c", cfg}, "every configured repo has skip: true"},
		"unknown repo":   {[]string{"run", "-c", cfg, "-r", "zzz"}, `repo "zzz" is not in the config`},
		"unexpected arg": {[]string{"run", "extra"}, "unknown command"},
		"unknown flag":   {[]string{"run", "--nope"}, "unknown flag"},
		"unknown agent":  {[]string{"run", "-c", active, "--agent", "gemini"}, `unknown agent "gemini" (want claude or codex)`},
		"bad limit":      {[]string{"run", "-c", active, "--review", "--max-reviews", "-1"}, "--max-reviews must not be negative"},
	} {
		t.Run(name, func(t *testing.T) {
			code, _, stderr := run(c.args...)
			if code != 1 || !strings.Contains(stderr, c.stderr) {
				t.Fatalf("code %d, stderr %q; want 1 containing %q", code, stderr, c.stderr)
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
