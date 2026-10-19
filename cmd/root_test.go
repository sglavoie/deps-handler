package cmd

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	cfg := writeConfig(t, "repos:\n  - o/a\n  - repo: o/b\n    skip: true\n")
	code, out, _ := run("repos", "-c", cfg)
	if code != 0 || !strings.Contains(out, "o/a   squash  minor") || !strings.Contains(out, "o/b") {
		t.Fatalf("code %d, output:\n%s", code, out)
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
