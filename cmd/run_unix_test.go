//go:build unix

package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sglavoie/deps-merger/internal/review"
)

// reviewEnv gives a --review run its own state and cache directories and
// a claude on PATH, and returns those directories.
func reviewEnv(t *testing.T) (state, cache string) {
	t.Helper()
	state, cache, bin := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("GH_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", bin)
	return filepath.Join(state, "deps-merger"), filepath.Join(cache, "deps-merger")
}

func TestRunReviewFailsWhileAnotherHoldsTheLock(t *testing.T) {
	_, cfg := runFixture(t)
	state, _ := reviewEnv(t)
	release, err := review.Lock(filepath.Join(state, "review.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	code, out, stderr := run("run", "-c", cfg, "--review")
	if code != 1 || out != "" || !strings.Contains(stderr, "another deps-merger run is reviewing updates") {
		t.Fatalf("code %d, out %q, stderr %q", code, out, stderr)
	}
}

func TestRunReviewWarnsWhenPruneFails(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads a directory whatever its mode")
	}
	_, cfg := runFixture(t)
	_, cache := reviewEnv(t)
	locked := filepath.Join(cache, "checkouts", "gone")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(locked, 0o755)
	code, out, stderr := run("run", "-c", cfg, "--review", "-r", "quiet")
	if code != 0 || !strings.Contains(out, "prune checkouts:") || !strings.Contains(out, "no open Dependabot PRs") {
		t.Fatalf("code %d, out %q, stderr %q; a checkout that cannot be pruned must not stop the run", code, out, stderr)
	}
	_, out, _ = run("run", "-c", cfg, "--review", "-r", "quiet", "--json")
	var doc struct{ Results []struct{ Warnings []string } }
	if err := json.Unmarshal([]byte(out), &doc); err != nil || len(doc.Results) != 1 || len(doc.Results[0].Warnings) != 1 || !strings.HasPrefix(doc.Results[0].Warnings[0], "prune checkouts: ") {
		t.Fatalf("json: %v:\n%s", err, out)
	}
}
