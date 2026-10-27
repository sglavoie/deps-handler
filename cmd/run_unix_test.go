//go:build unix

package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sglavoie/deps-merger/pkg/config"
	"github.com/sglavoie/deps-merger/pkg/review"
	"github.com/sglavoie/deps-merger/pkg/runner"
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

func TestReviewsCannotShareCheckoutsWithDifferentStateDirectories(t *testing.T) {
	reviewEnv(t)
	setup := func() (func(), error) {
		return setupReviews(runner.New(nil, "me", false), &config.Config{}, review.Claude{}, nil, 10)
	}
	release, err := setup()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	// Separate verdict caches still use the same checkout directory.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	secondRelease, err := setup()
	if err == nil {
		secondRelease()
		t.Fatal("two review runs acquired the same checkouts with different state directories")
	}
	if !strings.Contains(err.Error(), "another deps-merger run is reviewing updates") {
		t.Fatalf("unexpected error: %v", err)
	}
	// A failed acquisition must release the second state's lock, and
	// ending the first review must free the shared checkouts.
	release()
	thirdRelease, err := setup()
	if err != nil {
		t.Fatalf("locks remained held after release: %v", err)
	}
	thirdRelease()
}

func TestReviewSetupReleasesLocksWhenCacheCannotOpen(t *testing.T) {
	state, _ := reviewEnv(t)
	// XDG permits using the same directory for cache and state. The two
	// locks must still be distinct, and both must be released on failure.
	t.Setenv("XDG_CACHE_HOME", os.Getenv("XDG_STATE_HOME"))
	path := verdictsPath(state)
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	setup := func() (func(), error) {
		return setupReviews(runner.New(nil, "me", false), &config.Config{}, review.Claude{}, nil, 10)
	}
	if release, err := setup(); err == nil {
		release()
		t.Fatal("expected an unreadable verdict cache error")
	} else if strings.Contains(err.Error(), "another deps-merger run") {
		t.Fatalf("setup tried to acquire its own lock twice: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	release, err := setup()
	if err != nil {
		t.Fatalf("failed setup left locks held: %v", err)
	}
	release()
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
