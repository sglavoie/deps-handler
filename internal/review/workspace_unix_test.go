//go:build unix

package review

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWorkspaceCheckoutIsPrivate(t *testing.T) {
	origin, head := gitOrigin(t)
	root := filepath.Join(t.TempDir(), "checkouts")
	// As an older run left it.
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	w := &Workspace{Root: root, URL: func(string, string) string { return origin }}
	dir, err := w.Checkout(context.Background(), "o", "r", 7, head)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{root, dir} {
		info, err := os.Stat(d)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Errorf("%s has mode %o; private code must not be readable by others", d, perm)
		}
	}
}

func TestWorkspacePruneGoesOnAfterAnError(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads any directory")
	}
	root := t.TempDir()
	stale := time.Now().Add(-PruneAfter - time.Hour)
	for _, d := range []string{"a/locked", "b/gone"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(filepath.Join(root, d), stale, stale); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(root, "a"), 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(filepath.Join(root, "a"), 0o755)
	if err := (&Workspace{Root: root}).Prune(nil); err == nil {
		t.Error("an owner directory that cannot be read must be reported")
	}
	if _, err := os.Stat(filepath.Join(root, "b")); err == nil {
		t.Error("one unreadable owner directory must not stop the others being pruned")
	}
}
