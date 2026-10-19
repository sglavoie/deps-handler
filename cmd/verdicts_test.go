package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sglavoie/deps-merger/internal/policy"
	"github.com/sglavoie/deps-merger/internal/review"
)

// verdictsState points the state directory at a new one and returns the
// verdict cache in it.
func verdictsState(t *testing.T) (*review.Cache, string) {
	t.Helper()
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	path := verdictsPath(filepath.Join(state, "deps-merger"))
	cache, err := review.OpenCache(path)
	if err != nil {
		t.Fatal(err)
	}
	return cache, path
}

func TestVerdictsRejectsUnknownWords(t *testing.T) {
	verdictsState(t)
	for _, args := range [][]string{
		{"verdicts", "forgte", "acme/x"},
		{"verdicts", "Storefront"},
		{"verdicts", "acme/x#seven"},
	} {
		code, out, stderr := run(args...)
		if code != 1 || out != "" || !strings.Contains(stderr, "is neither a command nor a target") || !strings.Contains(stderr, `"verdicts forget"`) {
			t.Errorf("%v: code %d, out %q, stderr %q", args, code, out, stderr)
		}
	}
}

func TestVerdictsUnmatchedTargetExitsOne(t *testing.T) {
	cache, _ := verdictsState(t)
	if err := cache.Put("o/a#1 lib@2.0.0", "sha", "claude", policy.Verdict{Approved: true}); err != nil {
		t.Fatal(err)
	}
	code, out, stderr := run("verdicts", "o/zzz")
	if code != 1 || out != "no cached verdicts\n" || !strings.Contains(stderr, `no cached verdict matches "o/zzz"`) {
		t.Fatalf("code %d, out %q, stderr %q", code, out, stderr)
	}
	code, out, stderr = run("verdicts", "O/A#1", "o/zzz")
	if code != 1 || !strings.Contains(out, "o/a#1 lib@2.0.0") || strings.Contains(stderr, "O/A#1") {
		t.Fatalf("code %d, out %q, stderr %q", code, out, stderr)
	}
}

func TestVerdictsShowHeadOfRejection(t *testing.T) {
	cache, _ := verdictsState(t)
	if err := cache.Put("o/a#1 lib@2.0.0", "0123456789abcdef", "claude", policy.Verdict{Summary: "breaks"}); err != nil {
		t.Fatal(err)
	}
	if err := cache.Put("o/a#2 x@3.0.0", "fedcba9876543210", "codex", policy.Verdict{Approved: true}); err != nil {
		t.Fatal(err)
	}
	code, out, _ := run("verdicts")
	if code != 0 || !strings.Contains(out, "HEAD") || !strings.Contains(out, "rejected  claude  0123456") || !strings.Contains(out, "approved  codex   any") {
		t.Fatalf("code %d:\n%s", code, out)
	}
	code, out, _ = run("verdicts", "--json", "o/a#1")
	var got []struct {
		HeadOID string `json:"head_oid"`
	}
	if err := json.Unmarshal([]byte(out), &got); code != 0 || err != nil || len(got) != 1 || got[0].HeadOID != "0123456789abcdef" {
		t.Fatalf("json: code %d, %v:\n%s", code, err, out)
	}
}

func TestVerdictsWarnOnCorruptCache(t *testing.T) {
	_, path := verdictsState(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, stderr := run("verdicts")
	if code != 0 || out != "no cached verdicts\n" || !strings.Contains(stderr, "deps-merger: warning: verdict cache") || !strings.Contains(stderr, ".corrupt-") {
		t.Fatalf("code %d, out %q, stderr %q", code, out, stderr)
	}
}
