//go:build unix

package review

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func localRepairCheckout(t *testing.T) (*LocalWorkspace, string, string, string) {
	t.Helper()
	origin, head := gitOrigin(t)
	base := gitAt(t, origin, "symbolic-ref", "--short", "HEAD")
	gitAt(t, origin, "update-ref", "refs/heads/repair", head)
	parent := t.TempDir()
	dir := filepath.Join(parent, "repo with spaces")
	gitAt(t, parent, "clone", "-q", "--branch", "repair", origin, dir)
	workspace, err := NewLocalWorkspace(context.Background(), dir, "")
	if err != nil {
		t.Fatal(err)
	}
	workspace.git.URL = func(string, string) string { return origin }
	t.Cleanup(workspace.Close)
	return workspace, origin, head, base
}

func TestLocalWorkspaceEditsExactCheckoutAndPreservesDependencies(t *testing.T) {
	w, origin, head, base := localRepairCheckout(t)
	ctx := context.Background()
	exclude := filepath.Join(w.git.gitDir, "info", "exclude")
	if err := os.WriteFile(exclude, []byte("node_modules/\n"), 0600); err != nil {
		t.Fatal(err)
	}
	dependency := filepath.Join(w.Directory, "node_modules", "cached-package")
	if err := os.MkdirAll(filepath.Dir(dependency), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dependency, []byte("installed dependency"), 0600); err != nil {
		t.Fatal(err)
	}
	// A fork remote is left alone; base reads and publication use explicit URLs.
	gitAt(t, w.Directory, "remote", "set-url", "origin", "https://github.com/other/fork.git")
	dir, err := w.Checkout(ctx, "o", "r", 7, head)
	if err != nil || dir != w.Directory {
		t.Fatalf("checkout %s %v", dir, err)
	}
	if err := w.FetchBase(ctx, dir, base); err != nil {
		t.Fatal(err)
	}
	if got := gitAt(t, dir, "rev-parse", w.BaseReference(base)); got != head {
		t.Fatal(got)
	}
	if got := gitAt(t, dir, "config", "--get", "remote.origin.url"); got != "https://github.com/other/fork.git" {
		t.Fatal("origin changed", got)
	}
	edit := filepath.Join(dir, "repair.txt")
	if err := os.WriteFile(edit, []byte("repair"), 0600); err != nil {
		t.Fatal(err)
	}
	oid, err := w.CommitAndPush(ctx, dir, "o", "r", "repair", head)
	if err != nil {
		t.Fatal(err)
	}
	if got := gitAt(t, origin, "rev-parse", "refs/heads/repair"); got != oid {
		t.Fatal(got)
	}
	if _, err := w.Checkout(ctx, "o", "r", 7, oid); err != nil {
		t.Fatal("retry checkout", err)
	}
	if got := read(t, dependency); got != "installed dependency" {
		t.Fatal("dependencies lost", got)
	}
	if got := gitAt(t, dir, "branch", "--show-current"); got != "repair" {
		t.Fatal("local branch changed", got)
	}
	w.Close()
	if got := read(t, edit); got != "repair" {
		t.Fatal("edits removed on close", got)
	}
}

func TestLocalWorkspaceRejectsExistingChangesWithoutCleaning(t *testing.T) {
	for _, kind := range []string{"staged", "unstaged", "untracked", "wrong-head", "unfinished-merge"} {
		t.Run(kind, func(t *testing.T) {
			w, _, head, _ := localRepairCheckout(t)
			path := filepath.Join(w.Directory, "go.mod")
			content := "module user-edited\n"
			switch kind {
			case "staged", "unstaged":
				if err := os.WriteFile(path, []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
				if kind == "staged" {
					gitAt(t, w.Directory, "add", "go.mod")
				}
			case "untracked":
				path = filepath.Join(w.Directory, "my-work.txt")
				if err := os.WriteFile(path, []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			case "wrong-head":
				head = "other-PR-head"
				content = read(t, path)
			case "unfinished-merge":
				content = read(t, path)
				if err := os.WriteFile(filepath.Join(w.git.gitDir, "MERGE_HEAD"), []byte(head+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := w.Checkout(context.Background(), "o", "r", 7, head); err == nil {
				t.Fatal("accepted invalid checkout")
			}
			if got := read(t, path); got != content {
				t.Fatal("overwrote local work", got)
			}
		})
	}
}

func TestLocalWorkspaceRefusesBaseBranchAndConcurrentBranchSwitch(t *testing.T) {
	w, _, head, base := localRepairCheckout(t)
	ctx := context.Background()
	gitAt(t, w.Directory, "checkout", "-q", base)
	if _, err := w.Checkout(ctx, "o", "r", 7, head); err != nil {
		t.Fatal(err)
	}
	if err := w.FetchBase(ctx, w.Directory, base); err == nil || !strings.Contains(err.Error(), "base branch") {
		t.Fatal(err)
	}
	gitAt(t, w.Directory, "checkout", "-q", "repair")
	if _, err := w.Checkout(ctx, "o", "r", 7, head); err != nil {
		t.Fatal(err)
	}
	gitAt(t, w.Directory, "checkout", "--detach", "-q", head)
	if _, err := w.CommitAndPush(ctx, w.Directory, "o", "r", "repair", head); err == nil || !strings.Contains(err.Error(), "local branch changed") {
		t.Fatal(err)
	}
}

func TestLocalWorkspaceSupportsGitWorktrees(t *testing.T) {
	origin, head := gitOrigin(t)
	base := gitAt(t, origin, "symbolic-ref", "--short", "HEAD")
	dir := filepath.Join(t.TempDir(), "linked checkout")
	gitAt(t, origin, "worktree", "add", "--detach", "-q", dir, head)
	w, err := NewLocalWorkspace(context.Background(), dir, "")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	w.git.URL = func(string, string) string { return origin }
	if _, err := w.Checkout(context.Background(), "o", "r", 7, head); err != nil {
		t.Fatal(err)
	}
	if err := w.FetchBase(context.Background(), w.Directory, base); err != nil {
		t.Fatal(err)
	}
	if got := gitAt(t, w.Directory, "rev-parse", w.BaseReference(base)); got != head {
		t.Fatal(got)
	}
}

func TestLocalWorkspaceLocksAndValidatesRoot(t *testing.T) {
	w, _, _, _ := localRepairCheckout(t)
	if other, err := NewLocalWorkspace(context.Background(), w.Directory, ""); err == nil {
		other.Close()
		t.Fatal("concurrent repair acquired same checkout")
	}
	w.Close()
	again, err := NewLocalWorkspace(context.Background(), w.Directory, "")
	if err != nil {
		t.Fatal(err)
	}
	again.Close()
	subdir := filepath.Join(w.Directory, "subdir")
	if err := os.Mkdir(subdir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{subdir, t.TempDir(), filepath.Join(t.TempDir(), "missing"), filepath.Join(w.Directory, "go.mod")} {
		if workspace, err := NewLocalWorkspace(context.Background(), dir, ""); err == nil {
			workspace.Close()
			t.Fatalf("accepted %s", dir)
		}
	}
}

func addLocalRepairCommit(t *testing.T, w *LocalWorkspace) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(w.Directory, "user-fix.txt"), []byte("my committed fix"), 0600); err != nil {
		t.Fatal(err)
	}
	gitAt(t, w.Directory, "add", "user-fix.txt")
	gitAt(t, w.Directory, "commit", "-qm", "user repair")
	return gitAt(t, w.Directory, "rev-parse", "HEAD")
}

func TestLocalWorkspaceAcceptsAndPublishesExistingCommits(t *testing.T) {
	for _, furtherEdits := range []bool{false, true} {
		t.Run(map[bool]string{false: "existing commits only", true: "additional agent edits"}[furtherEdits], func(t *testing.T) {
			w, origin, remoteHead, base := localRepairCheckout(t)
			localHead := addLocalRepairCommit(t, w)
			ctx := context.Background()
			if _, err := w.Checkout(ctx, "o", "r", 7, remoteHead); err != nil {
				t.Fatal(err)
			}
			if w.LocalHeadOID() != localHead {
				t.Fatal("lost starting commit", w.LocalHeadOID())
			}
			if err := w.FetchBase(ctx, w.Directory, base); err != nil {
				t.Fatal(err)
			}
			if furtherEdits {
				if err := os.WriteFile(filepath.Join(w.Directory, "agent-fix.txt"), []byte("additional fix"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			oid, err := w.CommitAndPush(ctx, w.Directory, "o", "r", "repair", remoteHead)
			if err != nil {
				t.Fatal(err)
			}
			if got := gitAt(t, origin, "rev-parse", "refs/heads/repair"); got != oid {
				t.Fatal("wrong remote head", got, oid)
			}
			if !furtherEdits && oid != localHead {
				t.Fatal("created unnecessary commit", oid, localHead)
			}
			if furtherEdits && gitAt(t, w.Directory, "rev-parse", oid+"^") != localHead {
				t.Fatal("rewrote user's commit")
			}
			if got := gitAt(t, origin, "show", "refs/heads/repair:user-fix.txt"); got != "my committed fix" {
				t.Fatal("user work lost", got)
			}
			if got := gitAt(t, w.Directory, "rev-parse", localHead); got != localHead {
				t.Fatal("user commit lost")
			}
		})
	}
}

func TestLocalWorkspaceRejectsDivergedHistoryWithoutRewriting(t *testing.T) {
	w, origin, _, _ := localRepairCheckout(t)
	localHead := addLocalRepairCommit(t, w)
	if err := os.WriteFile(filepath.Join(origin, "remote-fix.txt"), []byte("remote change"), 0600); err != nil {
		t.Fatal(err)
	}
	gitAt(t, origin, "add", "remote-fix.txt")
	gitAt(t, origin, "commit", "-qm", "remote repair")
	remoteHead := gitAt(t, origin, "rev-parse", "HEAD")
	gitAt(t, origin, "update-ref", "refs/pull/7/head", remoteHead)
	gitAt(t, origin, "update-ref", "refs/heads/repair", remoteHead)
	_, err := w.Checkout(context.Background(), "o", "r", 7, remoteHead)
	if err == nil || !strings.Contains(err.Error(), "does not descend") {
		t.Fatal(err)
	}
	if got := gitAt(t, w.Directory, "rev-parse", "HEAD"); got != localHead {
		t.Fatal("local history moved", got)
	}
	if got := read(t, filepath.Join(w.Directory, "user-fix.txt")); got != "my committed fix" {
		t.Fatal(got)
	}
}

func TestLocalCommittedRepairsStillHonorRemoteLeaseAndAgentHead(t *testing.T) {
	for _, kind := range []string{"remote push", "agent commit", "PR moved during fetch"} {
		t.Run(kind, func(t *testing.T) {
			w, origin, remoteHead, _ := localRepairCheckout(t)
			localHead := addLocalRepairCommit(t, w)
			ctx := context.Background()
			if kind != "PR moved during fetch" {
				if _, err := w.Checkout(ctx, "o", "r", 7, remoteHead); err != nil {
					t.Fatal(err)
				}
			}
			expectedRemote := remoteHead
			if kind == "agent commit" {
				gitAt(t, w.Directory, "commit", "--allow-empty", "-qm", "unexpected agent commit")
			} else {
				gitAt(t, origin, "commit", "--allow-empty", "-qm", "concurrent remote change")
				expectedRemote = gitAt(t, origin, "rev-parse", "HEAD")
				gitAt(t, origin, "update-ref", "refs/heads/repair", expectedRemote)
				gitAt(t, origin, "update-ref", "refs/pull/7/head", expectedRemote)
			}
			var err error
			if kind == "PR moved during fetch" {
				_, err = w.Checkout(ctx, "o", "r", 7, remoteHead)
			} else {
				_, err = w.CommitAndPush(ctx, w.Directory, "o", "r", "repair", remoteHead)
			}
			if err == nil {
				t.Fatal("accepted concurrent change")
			}
			if got := gitAt(t, origin, "rev-parse", "refs/heads/repair"); got != expectedRemote {
				t.Fatal("overwrote remote", got)
			}
			gitAt(t, w.Directory, "cat-file", "-e", localHead)
		})
	}
}
