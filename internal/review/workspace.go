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
)

// ErrHeadMoved means a pull request's head is no longer the commit the
// policy judged, as when Dependabot pushed since the repository was read.
var ErrHeadMoved = errors.New("head moved")

// Workspace keeps one shallow checkout per repository for agents to read,
// reused across runs so a large repository is not cloned for every review.
type Workspace struct {
	Root string
	// Token authenticates fetches from github.com; it is passed to git
	// through the environment, never on the command line.
	Token string
	// URL returns the remote of a repository; github.com over HTTPS when
	// nil.
	URL func(owner, name string) string
}

// Checkout puts the head of pull request number into the repository's
// checkout and returns its directory. It fails if the head is no longer
// headOID, so the agent never reads other code than the policy judged.
func (w *Workspace) Checkout(ctx context.Context, owner, name string, number int, headOID string) (string, error) {
	dir := filepath.Join(w.Root, owner, name)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
		if err := w.git(ctx, dir, "init", "-q"); err != nil {
			return "", err
		}
		if err := w.git(ctx, dir, "remote", "add", "origin", w.url(owner, name)); err != nil {
			return "", err
		}
	}
	ref := fmt.Sprintf("+refs/pull/%d/head", number)
	if err := w.git(ctx, dir, "fetch", "-q", "--depth=1", "--no-tags", "origin", ref); err != nil {
		return "", err
	}
	got, err := w.output(ctx, dir, "rev-parse", "FETCH_HEAD")
	if err != nil {
		return "", err
	}
	if got != headOID {
		return "", fmt.Errorf("%w: #%d went from %.7s to %.7s", ErrHeadMoved, number, headOID, got)
	}
	if err := w.git(ctx, dir, "checkout", "-q", "--detach", "--force", "FETCH_HEAD"); err != nil {
		return "", err
	}
	if err := w.git(ctx, dir, "clean", "-q", "-ffdx"); err != nil {
		return "", err
	}
	return dir, nil
}

// Prune removes the checkouts of repositories not in keep (as
// "owner/name"), such as ones dropped from the config, and the owner
// directories left empty.
func (w *Workspace) Prune(keep map[string]bool) error {
	owners, err := os.ReadDir(w.Root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, o := range owners {
		if !o.IsDir() {
			continue
		}
		ownerDir := filepath.Join(w.Root, o.Name())
		names, err := os.ReadDir(ownerDir)
		if err != nil {
			return err
		}
		left := len(names)
		for _, n := range names {
			if keep[o.Name()+"/"+n.Name()] {
				continue
			}
			if err := os.RemoveAll(filepath.Join(ownerDir, n.Name())); err != nil {
				return err
			}
			left--
		}
		if left == 0 {
			if err := os.Remove(ownerDir); err != nil {
				return err
			}
		}
	}
	return nil
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

func (w *Workspace) output(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
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
