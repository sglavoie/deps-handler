package review

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ErrHeadMoved means a pull request's head is no longer the commit the
// policy judged, as when Dependabot pushed since the repository was read.
var ErrHeadMoved = errors.New("head moved")

// Workspace keeps one shallow checkout per repository for agents to read,
// reused across runs so a large repository is not cloned for every review.
type Workspace struct {
	Root string
	// Token authenticates fetches from github.com; it is passed to git
	// through the environment, never on the command line. On Linux, a
	// process of the user's, such as Codex reading what its sandbox
	// allows, can read it there while git runs.
	Token string
	// URL returns the remote of a repository; github.com over HTTPS when
	// nil.
	URL func(owner, name string) string
}

// Checkout puts the head of pull request number into the repository's
// checkout and returns its directory. It fails if the head is no longer
// headOID, so the agent never reads other code than the policy judged.
// A fetch that fails is tried once more. A checkout git cannot work with,
// as one a killed review left half written, is made again from scratch;
// one that only failed to fetch is kept when the remote cannot be reached
// either, since cloning again would fail the same way and lose it.
func (w *Workspace) Checkout(ctx context.Context, owner, name string, number int, headOID string) (string, error) {
	dir := filepath.Join(w.Root, owner, name)
	err := w.checkout(ctx, dir, owner, name, number, headOID)
	if errors.Is(err, errFetch) && ctx.Err() == nil {
		select {
		case <-ctx.Done():
			return "", err
		case <-time.After(fetchRetryPause):
		}
		err = w.checkout(ctx, dir, owner, name, number, headOID)
	}
	if err != nil && !errors.Is(err, ErrHeadMoved) && ctx.Err() == nil && (!errors.Is(err, errFetch) || w.reachable(ctx, dir, number)) {
		if err := os.RemoveAll(dir); err != nil {
			return "", err
		}
		err = w.checkout(ctx, dir, owner, name, number, headOID)
	}
	if err != nil {
		return "", err
	}
	// Mark the checkout as used, for Prune.
	now := time.Now()
	if err := os.Chtimes(dir, now, now); err != nil {
		return "", err
	}
	return dir, nil
}

// errFetch marks a failed fetch, which the network or GitHub more often
// causes than the checkout.
var errFetch = errors.New("fetch failed")

// fetchRetryPause is how long Checkout waits to fetch again.
var fetchRetryPause = 2 * time.Second

// reachable reports whether the remote of the checkout in dir serves pull
// request number, without reading the checkout's objects: when it does, a
// fetch that failed twice points at the checkout itself.
func (w *Workspace) reachable(ctx context.Context, dir string, number int) bool {
	return w.git(ctx, dir, "ls-remote", "--exit-code", "origin", fmt.Sprintf("refs/pull/%d/head", number)) == nil
}

func (w *Workspace) checkout(ctx context.Context, dir, owner, name string, number int, headOID string) error {
	// Checkouts hold private code: no one else may read them. Root is
	// tightened even when it exists, as older runs made it readable.
	if err := os.MkdirAll(w.Root, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(w.Root, 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		// No template: the user's would put their hooks in the checkout.
		if err := w.git(ctx, dir, "init", "-q", "--template="); err != nil {
			return err
		}
	}
	// A review holds review.lock, so no other git works in the checkout: a
	// lock file left there is from a git killed when its review timed out.
	locks, err := filepath.Glob(filepath.Join(dir, ".git", "*.lock"))
	if err != nil {
		return err
	}
	for _, l := range locks {
		if err := os.Remove(l); err != nil {
			return err
		}
	}
	// A run killed between init and here left no remote.
	url := w.url(owner, name)
	if err := w.git(ctx, dir, "remote", "set-url", "origin", url); err != nil {
		if err := w.git(ctx, dir, "remote", "add", "origin", url); err != nil {
			return err
		}
	}
	ref := fmt.Sprintf("+refs/pull/%d/head", number)
	if err := w.git(ctx, dir, "fetch", "-q", "--depth=1", "--no-tags", "origin", ref); err != nil {
		return fmt.Errorf("%w: %w", errFetch, err)
	}
	got, err := w.output(ctx, dir, "rev-parse", "FETCH_HEAD")
	if err != nil {
		return err
	}
	if got != headOID {
		return fmt.Errorf("%w: #%d went from %.7s to %.7s", ErrHeadMoved, number, headOID, got)
	}
	if err := w.git(ctx, dir, "checkout", "-q", "--detach", "--force", "FETCH_HEAD"); err != nil {
		return err
	}
	return w.git(ctx, dir, "clean", "-q", "-ffdx")
}

// PruneAfter is how long a checkout of a repository not in the config is
// kept unused. Runs with other configs share the checkouts, so one run
// cannot tell a repository dropped from its config from another config's.
const PruneAfter = 30 * 24 * time.Hour

// Prune removes the checkouts of repositories not in keep (as
// "owner/name") that no review used for PruneAfter, such as ones dropped
// from the config, and the owner directories left empty. One it cannot
// remove does not stop it from removing the others: their errors are
// returned together.
func (w *Workspace) Prune(keep map[string]bool) error {
	owners, err := os.ReadDir(w.Root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, o := range owners {
		if !o.IsDir() {
			continue
		}
		ownerDir := filepath.Join(w.Root, o.Name())
		names, err := os.ReadDir(ownerDir)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		left := len(names)
		for _, n := range names {
			if keep[o.Name()+"/"+n.Name()] {
				continue
			}
			info, err := n.Info()
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if time.Since(info.ModTime()) < PruneAfter {
				continue
			}
			if err := os.RemoveAll(filepath.Join(ownerDir, n.Name())); err != nil {
				errs = append(errs, err)
				continue
			}
			left--
		}
		if left == 0 {
			if err := os.Remove(ownerDir); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func (w *Workspace) url(owner, name string) string {
	if w.URL != nil {
		return w.URL(owner, name)
	}
	return fmt.Sprintf("https://github.com/%s/%s.git", owner, name)
}

func (w *Workspace) git(ctx context.Context, dir string, args ...string) error {
	_, err := w.output(ctx, dir, args...)
	return err
}

// output runs git in the checkout in dir and returns what it printed.
// The checkout holds untrusted code and git has the token, so the user's
// hooks and fsmonitor do not run there, and git is told which repository
// to work on rather than read it from an inherited GIT_DIR.
func (w *Workspace) output(ctx context.Context, dir string, args ...string) (string, error) {
	opts := []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false"}
	if args[0] != "init" {
		// git init would record the work tree in the config.
		opts = append(opts, "--git-dir=.git", "--work-tree=.")
	}
	cmd := exec.CommandContext(ctx, "git", append(opts, args...)...)
	cmd.Dir = dir
	// Git LFS would download every large file at checkout, for an agent
	// that reads code; the pointer files say what changed.
	cmd.Env = append(agentEnv(), "GIT_TERMINAL_PROMPT=0", "GIT_LFS_SKIP_SMUDGE=1")
	// A remote the user's config rewrites to SSH must not wait on a
	// question no one can answer. An ssh command of the user's own is
	// kept; noTerminal stops it asking all the same.
	if os.Getenv("GIT_SSH_COMMAND") == "" && os.Getenv("GIT_SSH") == "" {
		cmd.Env = append(cmd.Env, "GIT_SSH_COMMAND=ssh -o BatchMode=yes")
	}
	noTerminal(cmd)
	if w.Token != "" {
		auth := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + w.Token))
		cmd.Env = append(cmd.Env,
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.https://github.com/.extraheader",
			"GIT_CONFIG_VALUE_0=AUTHORIZATION: basic "+auth,
		)
	}
	out, err := run(cmd)
	if err != nil {
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return strings.TrimSpace(string(out)), nil
}
