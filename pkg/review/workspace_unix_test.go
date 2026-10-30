//go:build unix

package review

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWorkspaceUsesCurrentTokenForNetworkCommands(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nprintf '%s' \"$GIT_CONFIG_VALUE_0\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	token := "first"
	var sourceErr error
	w := &Workspace{Token: "stale", TokenSource: func(context.Context) (string, error) { return token, sourceErr }}
	for _, command := range []string{"fetch", "ls-remote"} {
		out, err := w.output(context.Background(), bin, command)
		want := "AUTHORIZATION: basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
		if err != nil || out != want {
			t.Fatalf("%s authentication = %q, %v; want %q", command, out, err, want)
		}
		token = "refreshed"
	}
	sourceErr = errors.New("cannot refresh installation token")
	if _, err := w.output(context.Background(), bin, "fetch"); !errors.Is(err, sourceErr) {
		t.Fatalf("token refresh error = %v", err)
	}
	token, sourceErr = "", nil
	if _, err := w.output(context.Background(), bin, "fetch"); err == nil {
		t.Fatal("empty refreshed token accepted")
	}
	// Local commands need neither a token nor a working token source.
	sourceErr = errors.New("network credentials unavailable")
	for _, local := range []*Workspace{w, {Token: "static-token"}} {
		for _, command := range []string{"init", "remote", "rev-parse", "checkout", "clean"} {
			out, err := local.output(context.Background(), bin, command)
			if err != nil || out != "" {
				t.Errorf("local %s received credentials or fetched a token: %q, %v", command, out, err)
			}
		}
	}
}

func TestWorkspaceDoesNotRunConfiguredFilters(t *testing.T) {
	origin, _ := gitOrigin(t)
	if err := os.WriteFile(filepath.Join(origin, ".gitattributes"), []byte("go.mod filter=audit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git := func(t *testing.T, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", origin, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git(t, "add", ".gitattributes")
	git(t, "commit", "-qm", "add filter attribute")
	head := git(t, "rev-parse", "HEAD")
	git(t, "update-ref", "refs/pull/7/head", head)
	for _, source := range []string{"global", "system"} {
		for _, driver := range []string{"smudge", "process"} {
			t.Run(source+"/"+driver, func(t *testing.T) {
				dir := t.TempDir()
				marker, script := filepath.Join(dir, "filter-ran"), filepath.Join(dir, "filter")
				filter := "cat"
				if driver == "smudge" {
					filter = "sed 's/module x/module filtered/'"
				}
				if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s' \"$GIT_CONFIG_VALUE_0\" > '"+marker+"'\n"+filter+"\n"), 0700); err != nil {
					t.Fatal(err)
				}
				cfg := filepath.Join(dir, "gitconfig")
				git(t, "config", "--file", cfg, "filter.audit."+driver, "'"+script+"'")
				git(t, "config", "--file", cfg, "filter.audit.clean", "cat")
				git(t, "config", "--file", cfg, "filter.audit.required", "true")
				if source == "global" {
					t.Setenv("GIT_CONFIG_GLOBAL", cfg)
				} else {
					t.Setenv("GIT_CONFIG_SYSTEM", cfg)
					t.Setenv("GIT_CONFIG_NOSYSTEM", "0")
				}
				w := &Workspace{Root: t.TempDir(), Token: "audit-token", URL: func(string, string) string { return origin }}
				checkout, err := w.Checkout(context.Background(), "o", "r", 7, head)
				if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
					t.Fatalf("%s filter ran outside the reviewer sandbox: %v", source, statErr)
				}
				if err != nil {
					t.Fatal(err)
				}
				if got := read(t, filepath.Join(checkout, "go.mod")); got != "module x\n" {
					t.Fatalf("review checkout content was transformed: %q", got)
				}
				if driver == "smudge" {
					// Simulate the cached checkout an earlier version left
					// after applying the user's filter to this same head.
					if err := os.Remove(filepath.Join(checkout, "go.mod")); err != nil {
						t.Fatal(err)
					}
					cmd := exec.Command("git", "-C", checkout, "checkout-index", "--force", "--all", "--index")
					if out, err := cmd.CombinedOutput(); err != nil {
						t.Fatalf("seed previously filtered checkout: %v\n%s", err, out)
					}
					if got := read(t, filepath.Join(checkout, "go.mod")); got != "module filtered\n" {
						t.Fatalf("fixture did not apply its filter: %q", got)
					}
					if err := os.Remove(marker); err != nil {
						t.Fatal(err)
					}
					if _, err := w.Checkout(context.Background(), "o", "r", 7, head); err != nil {
						t.Fatal(err)
					}
					if _, err := os.Stat(marker); !os.IsNotExist(err) {
						t.Fatalf("filter ran while reusing checkout: %v", err)
					}
					if got := read(t, filepath.Join(checkout, "go.mod")); got != "module x\n" {
						t.Fatalf("reused checkout retained filtered content: %q", got)
					}
				}
			})
		}
	}
}

func TestWorkspaceFetchKeepsUserNetworkConfiguration(t *testing.T) {
	origin, head := gitOrigin(t)
	global := filepath.Join(t.TempDir(), "gitconfig")
	cmd := exec.Command("git", "config", "--file", global, "url."+origin+".insteadOf", "https://github.com/o/r.git")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("configure URL rewrite: %v\n%s", err, out)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	w := &Workspace{Root: t.TempDir()}
	if _, err := w.Checkout(context.Background(), "o", "r", 7, head); err != nil {
		t.Fatalf("fetch did not retain the user's network configuration: %v", err)
	}
}

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

func TestWorkspaceIgnoresInheritedRepository(t *testing.T) {
	origin, head := gitOrigin(t)
	decoy, decoyHead := gitOrigin(t)
	if err := os.WriteFile(filepath.Join(decoy, "untracked"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// As a dotfiles setup leaves them, pointing at $HOME.
	t.Setenv("GIT_DIR", filepath.Join(decoy, ".git"))
	t.Setenv("GIT_WORK_TREE", decoy)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(decoy, ".git", "index"))
	t.Setenv("GIT_OBJECT_DIRECTORY", filepath.Join(decoy, ".git", "objects"))
	t.Setenv("GIT_CONFIG_PARAMETERS", "'remote.origin.url'='"+decoy+"'")
	w := &Workspace{Root: t.TempDir(), URL: func(string, string) string { return origin }}
	dir, err := w.Checkout(context.Background(), "o", "r", 7, head)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", "HEAD")); err != nil {
		t.Fatalf("the checkout has no repository of its own: %v", err)
	}
	for _, k := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_CONFIG_PARAMETERS"} {
		os.Unsetenv(k) // t.Setenv restores them
	}
	if _, err := os.Stat(filepath.Join(decoy, "untracked")); err != nil {
		t.Error("git cleaned the repository GIT_DIR named")
	}
	out, err := exec.Command("git", "-C", decoy, "remote").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "" {
		t.Errorf("remotes of the decoy = %q, %v; want none", out, err)
	}
	out, err = exec.Command("git", "-C", decoy, "rev-parse", "HEAD").Output()
	if err != nil || strings.TrimSpace(string(out)) != decoyHead {
		t.Errorf("decoy HEAD = %q, %v; want %s", out, err, decoyHead)
	}
}

func TestWorkspaceRunsNoHooks(t *testing.T) {
	origin, head := gitOrigin(t)
	tmp := t.TempDir()
	ran := filepath.Join(tmp, "ran")
	hook := "#!/bin/sh\necho \"$0\" >> '" + ran + "'\n"
	hooks, template := filepath.Join(tmp, "hooks"), filepath.Join(tmp, "template")
	for _, d := range []string{hooks, filepath.Join(template, "hooks")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, h := range []string{"post-checkout", "reference-transaction", "fsmonitor"} {
			if err := os.WriteFile(filepath.Join(d, h), []byte(hook), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	global := filepath.Join(tmp, "gitconfig")
	cfg := "[core]\n\thooksPath = " + hooks + "\n\tfsmonitor = " + filepath.Join(hooks, "fsmonitor") + "\n[init]\n\ttemplateDir = " + template + "\n"
	if err := os.WriteFile(global, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	w := &Workspace{Root: t.TempDir(), URL: func(string, string) string { return origin }}
	dir, err := w.Checkout(context.Background(), "o", "r", 7, head)
	if err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(ran); err == nil {
		t.Fatalf("a hook of the user's ran in the checkout:\n%s", data)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", "hooks", "post-checkout")); err == nil {
		t.Error("the user's template put its hooks in the checkout")
	}
}

func TestWorkspaceSSHAsksNothing(t *testing.T) {
	defer func(d time.Duration) { fetchRetryPause = d }(fetchRetryPause)
	fetchRetryPause = 0
	bin := t.TempDir()
	log := filepath.Join(bin, "log")
	// ssh as git runs it, for a remote the user's config rewrites to SSH.
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> '" + log + "'\nif (: < /dev/tty) 2>/dev/null; then echo HAS-TTY >> '" + log + "'; fi\nexit 255\n"
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GIT_SSH", "")
	for _, c := range []struct{ own, want string }{
		{"", "BatchMode=yes"},
		{"ssh -o ConnectTimeout=1", "ConnectTimeout=1"},
	} {
		os.Remove(log)
		t.Setenv("GIT_SSH_COMMAND", c.own)
		w := &Workspace{Root: t.TempDir(), URL: func(string, string) string { return "ssh://git@example.invalid/o/r.git" }}
		if _, err := w.Checkout(context.Background(), "o", "r", 7, "abc"); err == nil {
			t.Fatal("a fetch over a failing ssh must fail")
		}
		got := read(t, log)
		if !strings.Contains(got, c.want+"\n") || strings.Contains(got, "HAS-TTY") {
			t.Errorf("GIT_SSH_COMMAND %q: ssh ran with:\n%s", c.own, got)
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
