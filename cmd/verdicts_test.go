package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sglavoie/deps-handler/pkg/policy"
	"github.com/sglavoie/deps-handler/pkg/review"
)

// verdictsState points the state directory at a new one and returns the
// verdict cache in it.
func verdictsState(t *testing.T) (*review.Cache, string) {
	t.Helper()
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	path := verdictsPath(filepath.Join(state, "deps-handler"))
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
		if code != 2 || out != "" || !strings.Contains(stderr, "is neither a command nor a target") || !strings.Contains(stderr, `"verdicts forget"`) {
			t.Errorf("%v: code %d, out %q, stderr %q", args, code, out, stderr)
		}
	}
}

func TestForgetRejectsWhatIsNoTarget(t *testing.T) {
	cache, _ := verdictsState(t)
	if err := cache.Put("o/a#1 lib@2.0.0", "sha", "claude", policy.Verdict{Approved: true}); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"Storefront#1", "o/a#seven", "o/a lib@2.0.0", "o/a#1 "} {
		code, out, stderr := run("verdicts", "forget", target)
		if code != 2 || out != "" || !strings.Contains(stderr, "is not a target") {
			t.Errorf("%q: code %d, out %q, stderr %q", target, code, out, stderr)
		}
	}
	if code, out, stderr := run("verdicts", "forget", "o/a#1 lib@2.0.0"); code != 0 || !strings.Contains(out, "forgot 1 verdict") {
		t.Fatalf("update key: code %d, out %q, stderr %q", code, out, stderr)
	}
}

func TestVerdictsCompleteTargets(t *testing.T) {
	cache, _ := verdictsState(t)
	for _, key := range []string{"o/a#1 lib@2.0.0", "o/a#2 x@1.1.0", "p/b#7 y@3.0.0"} {
		if err := cache.Put(key, "sha", "claude", policy.Verdict{Approved: true}); err != nil {
			t.Fatal(err)
		}
	}
	// The forget subcommand comes first, with its description.
	if _, out, _ := run("__complete", "verdicts", ""); !strings.HasPrefix(out, "forget\t") || !strings.HasSuffix(out, "\no/a\no/a#1\no/a#2\np/b\np/b#7\n:4\n") {
		t.Errorf("verdicts: %q", out)
	}
	if _, out, _ := run("__complete", "verdicts", "forget", "o/a", ""); !strings.HasPrefix(out, "o/a#1\no/a#2\np/b\np/b#7\n:4\n") {
		t.Errorf("forget, o/a given: %q", out)
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
	code, out, _ = run("verdicts", "--json")
	var doc struct {
		Verdicts []struct {
			HeadOID  string `json:"head_oid"`
			Approved bool
		}
	}
	if err := json.Unmarshal([]byte(out), &doc); code != 0 || err != nil || len(doc.Verdicts) != 2 {
		t.Fatalf("json: code %d, %v:\n%s", code, err, out)
	}
	got := doc.Verdicts
	if got[0].Approved || !got[1].Approved {
		t.Fatalf("approved = %t, %t", got[0].Approved, got[1].Approved)
	}
	// An approval survives a rebase: it holds for any head.
	if got[0].HeadOID != "0123456789abcdef" || got[1].HeadOID != "any" {
		t.Fatalf("head_oid = %q, %q; want the rejected commit, then any", got[0].HeadOID, got[1].HeadOID)
	}
}

func TestVerdictsShowHeadOfApprovalWithoutVersions(t *testing.T) {
	cache, _ := verdictsState(t)
	// The key of an update without versions, as review.Request.Key makes
	// it: the head stands in for them, so the approval is for that head.
	if err := cache.Put("o/a#3 actions/checkout@ 0123456789abcdef", "0123456789abcdef", "claude", policy.Verdict{Approved: true}); err != nil {
		t.Fatal(err)
	}
	code, out, _ := run("verdicts")
	if code != 0 || !strings.Contains(out, "approved  claude  0123456") {
		t.Fatalf("code %d:\n%s", code, out)
	}
	code, out, _ = run("verdicts", "--json")
	if code != 0 || !strings.Contains(out, `"head_oid": "0123456789abcdef"`) {
		t.Fatalf("json: code %d:\n%s", code, out)
	}
}

func TestVerdictsPrintNoControlCharacters(t *testing.T) {
	cache, _ := verdictsState(t)
	if err := cache.Put("o/a#1 lib\x1b]8;;https://evil\x07@2.0.0", "sha", "cla\x1b[2Jude", policy.Verdict{Approved: true, Summary: "ok"}); err != nil {
		t.Fatal(err)
	}
	code, out, _ := run("verdicts")
	if code != 0 || strings.ContainsAny(out, "\x1b\x07") || !strings.Contains(out, "o/a#1 lib]8;;https://evil@2.0.0") || !strings.Contains(out, "cla[2Jude") {
		t.Fatalf("code %d:\n%q", code, out)
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
	if code != 0 || out != "no cached verdicts\n" || !strings.Contains(stderr, "deps-handler: warning: verdict cache") || !strings.Contains(stderr, ".corrupt-") {
		t.Fatalf("code %d, out %q, stderr %q", code, out, stderr)
	}
}
