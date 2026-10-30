package review

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitAt(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "core.hooksPath=" + os.DevNull, "-c", "commit.gpgSign=false", "-C", dir}, args...)...)
	cmd.Env = append(agentEnv(), "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestRepairPublishesDescendantAndHonorsLease(t *testing.T) {
	for _, concurrent := range []bool{false, true} {
		t.Run(map[bool]string{false: "publish", true: "concurrent"}[concurrent], func(t *testing.T) {
			origin, head := gitOrigin(t)
			gitAt(t, origin, "update-ref", "refs/heads/repair", head)
			main := gitAt(t, origin, "symbolic-ref", "--short", "HEAD")
			w := &Workspace{Root: t.TempDir(), URL: func(string, string) string { return origin }}
			ctx := context.Background()
			dir, err := w.Checkout(ctx, "o", "r", 7, head)
			if err != nil {
				t.Fatal(err)
			}
			if err := w.FetchBase(ctx, dir, main); err != nil {
				t.Fatal(err)
			}
			if got := gitAt(t, dir, "rev-parse", "refs/remotes/origin/"+main); got != head {
				t.Fatal(got)
			}
			if err := os.WriteFile(filepath.Join(dir, "fixed.txt"), []byte("repair\n"), 0600); err != nil {
				t.Fatal(err)
			}
			expectedRemote := head
			if concurrent {
				if err := os.WriteFile(filepath.Join(origin, "human.txt"), []byte("human\n"), 0600); err != nil {
					t.Fatal(err)
				}
				gitAt(t, origin, "add", "human.txt")
				gitAt(t, origin, "commit", "-qm", "human")
				expectedRemote = gitAt(t, origin, "rev-parse", "HEAD")
				gitAt(t, origin, "update-ref", "refs/heads/repair", expectedRemote)
			}
			oid, err := w.CommitAndPush(ctx, dir, "o", "r", "repair", head)
			got := gitAt(t, origin, "rev-parse", "refs/heads/repair")
			if concurrent {
				if err == nil || got != expectedRemote {
					t.Fatalf("overwrote concurrent push: oid %s remote %s want %s err %v", oid, got, expectedRemote, err)
				}
			} else {
				if err != nil || oid == head || got != oid {
					t.Fatalf("oid %s remote %s err %v", oid, got, err)
				}
				if parent := gitAt(t, dir, "rev-parse", oid+"^"); parent != head {
					t.Fatalf("rewrote PR history: parent %s", parent)
				}
				if content := gitAt(t, origin, "show", "refs/heads/repair:fixed.txt"); content != "repair" {
					t.Fatal(content)
				}
				if gotBase := gitAt(t, origin, "rev-parse", "refs/heads/"+main); gotBase != head {
					t.Fatalf("base moved: %s", gotBase)
				}
			}
		})
	}
}

func TestRepairRejectsNoChangesAndAgentCommits(t *testing.T) {
	origin, head := gitOrigin(t)
	w := &Workspace{Root: t.TempDir(), URL: func(string, string) string { return origin }}
	ctx := context.Background()
	dir, err := w.Checkout(ctx, "o", "r", 7, head)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.CommitAndPush(ctx, dir, "o", "r", "repair", head); err == nil || !strings.Contains(err.Error(), "no file changes") {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("edit"), 0600); err != nil {
		t.Fatal(err)
	}
	gitAt(t, dir, "add", "file")
	gitAt(t, dir, "commit", "-qm", "agent committed")
	if _, err := w.CommitAndPush(ctx, dir, "o", "r", "repair", head); err == nil || !strings.Contains(err.Error(), "changed HEAD") {
		t.Fatal(err)
	}
}

func TestTaskAdaptersKeepReviewsReadOnly(t *testing.T) {
	const analysis = `{"summary":"fixed","steps":[]}`
	body := `echo '{"type":"result","subtype":"success","is_error":false,"structured_output":` + analysis + `}'
while [ $# -gt 0 ]; do [ "$1" = --output-last-message ] && out=$2; shift; done
if [ -n "$out" ]; then echo '` + analysis + `' > "$out"; fi`
	for _, name := range []string{"claude", "codex"} {
		t.Run(name, func(t *testing.T) {
			path, record := fakeAgent(t, body)
			var agent TaskAgent = Claude{Command: path}
			if name == "codex" {
				agent = Codex{Command: path}
			}
			if _, err := agent.Task(context.Background(), t.TempDir(), "fix", "{}", true); err != nil {
				t.Fatal(err)
			}
			args := read(t, filepath.Join(record, "args"))
			if name == "codex" && !strings.Contains(args, "--sandbox\nworkspace-write\n") {
				t.Fatal(args)
			}
			if name == "claude" && !strings.Contains(args, "--allowedTools\nRead,Grep,Glob,Edit,Write,Bash\n") {
				t.Fatal(args)
			}
			if _, err := agent.Task(context.Background(), t.TempDir(), "diagnose", "{}", false); err != nil {
				t.Fatal(err)
			}
			args = read(t, filepath.Join(record, "args"))
			if strings.Contains(args, "workspace-write") || strings.Contains(args, "Bash") {
				t.Fatal("diagnosis was writable", args)
			}
		})
	}
	if _, err := (API{}).Task(context.Background(), t.TempDir(), "fix", "{}", true); err == nil {
		t.Fatal("API allowed edits")
	}
}

func TestRepairCanPublishResolvedMergeConflicts(t *testing.T) {
	origin, _ := gitOrigin(t)
	base := gitAt(t, origin, "symbolic-ref", "--short", "HEAD")
	gitAt(t, origin, "checkout", "-qb", "repair")
	if err := os.WriteFile(filepath.Join(origin, "go.mod"), []byte("module updated\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitAt(t, origin, "add", "go.mod")
	gitAt(t, origin, "commit", "-qm", "update")
	head := gitAt(t, origin, "rev-parse", "HEAD")
	gitAt(t, origin, "update-ref", "refs/pull/7/head", head)
	gitAt(t, origin, "checkout", "-q", base)
	if err := os.WriteFile(filepath.Join(origin, "go.mod"), []byte("module basechanged\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitAt(t, origin, "add", "go.mod")
	gitAt(t, origin, "commit", "-qm", "base changes")
	baseOID := gitAt(t, origin, "rev-parse", "HEAD")
	ctx := context.Background()
	w := &Workspace{Root: t.TempDir(), URL: func(string, string) string { return origin }}
	dir, err := w.Checkout(ctx, "o", "r", 7, head)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.FetchBase(ctx, dir, base); err != nil {
		t.Fatal(err)
	}
	if err := w.git(ctx, dir, "-c", "user.name=test", "-c", "user.email=t@t", "merge", "--no-commit", "--no-ff", "refs/remotes/origin/"+base); err == nil {
		t.Fatal("expected a merge conflict")
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module resolved\n"), 0600); err != nil {
		t.Fatal(err)
	}
	oid, err := w.CommitAndPush(ctx, dir, "o", "r", "repair", head)
	if err != nil {
		t.Fatal(err)
	}
	if parents := gitAt(t, dir, "show", "-s", "--format=%P", oid); parents != head+" "+baseOID {
		t.Fatalf("merge parents %s", parents)
	}
}

func TestRepairCommitDoesNotPublish(t *testing.T) {
	origin, head := gitOrigin(t)
	gitAt(t, origin, "update-ref", "refs/heads/repair", head)
	w := &Workspace{Root: t.TempDir(), URL: func(string, string) string { return origin }}
	ctx := context.Background()
	dir, err := w.Checkout(ctx, "o", "r", 7, head)
	if err != nil {
		t.Fatal(err)
	}
	w.URL = func(string, string) string { t.Fatal("local commit attempted network access"); return "" }
	if err := os.WriteFile(filepath.Join(dir, "fixed.txt"), []byte("repair\n"), 0600); err != nil {
		t.Fatal(err)
	}
	oid, err := w.Commit(ctx, dir, head)
	if err != nil || oid == head {
		t.Fatalf("oid=%s err=%v", oid, err)
	}
	if parent := gitAt(t, dir, "rev-parse", oid+"^"); parent != head {
		t.Fatal("rewrote PR history", parent)
	}
	if status := gitAt(t, dir, "status", "--porcelain"); status != "" {
		t.Fatal("repair not committed", status)
	}
	if remote := gitAt(t, origin, "rev-parse", "refs/heads/repair"); remote != head {
		t.Fatal("local preparation moved remote", remote)
	}
	stat, err := w.DiffSummary(ctx, dir, head)
	if err != nil || !strings.Contains(stat, "fixed.txt") || !strings.Contains(stat, "1 insertion") {
		t.Fatalf("stat=%s err=%v", stat, err)
	}
}
