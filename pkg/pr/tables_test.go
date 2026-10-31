package pr

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/sglavoie/deps-handler/pkg/github"
)

func TestPRTablesPreserveColorsAndCleanUntrustedText(t *testing.T) {
	t.Setenv("CLICOLOR_FORCE", "1")
	t.Setenv("NO_COLOR", "")
	snapshot := github.PRSnapshot{Target: github.PRTarget{Owner: "o", Repo: "r", Number: 7}, Title: "unsafe\x1b[2J", HeadOID: "abc123", Checks: []github.PRCheck{{Check: github.Check{Name: "test\x1b[2J", State: github.CheckFailed}}}}
	result := Result{Snapshot: snapshot, Analysis: Analysis{Summary: "assessment\x1b[2J", Recommendations: []Recommendation{{Subject: "test", Action: HumanAction, Reason: "billing\x1b[2J", NextStep: "Resolve billing"}}}}
	var out bytes.Buffer
	if err := Table(&out, result); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"\x1b[1;36mCHECK", "\x1b[31mfailed", "Resolve billing", "assessment"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in %q", want, out.String())
		}
	}
	if strings.Contains(out.String(), "\x1b[2J") || strings.Contains(out.String(), "DEPS_HANDLER_TABLE_") {
		t.Fatalf("untrusted escapes or render marker: %q", out.String())
	}
	out.Reset()
	if err := RerunTable(&out, RerunResult{Snapshot: snapshot, Warnings: []string{"warn\x1b[2J"}, Runs: []Rerun{{Jobs: snapshot.Checks}}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "\x1b[31mfailed") || strings.Contains(out.String(), "\x1b[2J") || strings.Contains(out.String(), "DEPS_HANDLER_TABLE_") {
		t.Fatalf("bad rerun table: %q", out.String())
	}
	if err := Table(brokenWriter{}, result); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("handoff error: %v", err)
	}
	if err := RerunTable(brokenWriter{}, RerunResult{Snapshot: snapshot}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("rerun error: %v", err)
	}
}
