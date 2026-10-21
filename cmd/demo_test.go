//go:build demo

package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The demo recorded for the README merges, rebases, and has the agent
// approve and reject, all against the make-believe GitHub.
func TestDemo(t *testing.T) {
	os.RemoveAll(filepath.Join(os.Getenv("XDG_STATE_HOME"), "deps-merger", "verdicts.json"))
	code, out, errOut := run("run", "--review", "--apply")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, want := range []string{"merged", "rebase-requested", "approved by claude", "rejected by the agent", `check "lint" fails`} {
		if !strings.Contains(out, want) {
			t.Errorf("demo output lacks %q:\n%s", want, out)
		}
	}
}
