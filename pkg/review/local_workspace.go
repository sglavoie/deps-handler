package review

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// LocalWorkspace edits an existing checkout without resetting, cleaning or
// switching its branch. Ignored dependencies and build caches stay in place.
type LocalWorkspace struct {
	Directory             string
	git                   Workspace
	owner, repo, branch   string
	localHead, remoteHead string
	release               func()
}

// NewLocalWorkspace validates the repository root and holds an exclusive
// repair lock until Close. Checkout validates its HEAD and clean worktree.
func NewLocalWorkspace(ctx context.Context, directory, token string) (*LocalWorkspace, error) {
	dir, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, fmt.Errorf("working directory: %w", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("--working-directory must name an existing repository directory")
	}
	discover := func(arg string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", "-c", "core.fsmonitor=false", "-c", "core.hooksPath="+os.DevNull, "-C", dir, "rev-parse", arg)
		cmd.Env = append(agentEnv(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_SYSTEM="+os.DevNull, "GIT_CONFIG_GLOBAL="+os.DevNull)
		out, err := run(cmd)
		return strings.TrimSpace(string(out)), err
	}
	top, err := discover("--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("working directory must be a Git checkout: %w", err)
	}
	top, err = filepath.EvalSymlinks(top)
	if err != nil {
		return nil, err
	}
	if top != dir {
		return nil, fmt.Errorf("--working-directory must name the repository root: %s", top)
	}
	gitDir, err := discover("--absolute-git-dir")
	if err != nil {
		return nil, err
	}
	release, err := Lock(filepath.Join(gitDir, "deps-handler-pr.lock"))
	if err != nil {
		return nil, err
	}
	return &LocalWorkspace{Directory: dir, git: Workspace{Token: token, gitDir: gitDir}, release: release}, nil
}

// Close releases the repair lock. It never deletes the checkout.
func (w *LocalWorkspace) Close() {
	if w.release != nil {
		w.release()
		w.release = nil
	}
}

func (w *LocalWorkspace) Checkout(ctx context.Context, owner, repo string, number int, expected string) (string, error) {
	head, err := w.git.output(ctx, w.Directory, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}

	status, err := w.git.output(ctx, w.Directory, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return "", err
	}
	if status != "" {
		return "", errors.New("working directory has staged, unstaged or untracked changes; commit or stash existing work before pr fix (no files changed)")
	}
	for _, name := range []string{"MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "rebase-merge", "rebase-apply", "sequencer"} {
		if _, err := os.Stat(filepath.Join(w.git.gitDir, name)); err == nil {
			return "", fmt.Errorf("working directory has an unfinished Git operation (%s); finish or abort it before pr fix", name)
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	branch, err := w.git.output(ctx, w.Directory, "branch", "--show-current")
	if err != nil {
		return "", err
	}
	if head != expected {
		// Fetch into a private ref to obtain a missing PR commit or shallow history.
		// The working tree, local branches and origin tracking refs are untouched.
		source := fmt.Sprintf("refs/pull/%d/head", number)
		reference := "refs/worktree/deps-handler/pr-head"
		if err := w.git.fetchRef(ctx, w.Directory, w.git.url(owner, repo), source, reference); err != nil {
			return "", fmt.Errorf("fetch selected PR head to compare local history: %w", err)
		}
		fetched, err := w.git.output(ctx, w.Directory, "rev-parse", reference)
		if err != nil {
			return "", err
		}
		if fetched != expected {
			return "", fmt.Errorf("%w: selected PR head changed from %s to %s while preparing local history (no files changed)", ErrHeadMoved, expected, fetched)
		}
		if err := w.git.git(ctx, w.Directory, "merge-base", "--is-ancestor", expected, head); err != nil {
			return "", fmt.Errorf("working directory HEAD %s does not descend from selected PR head %s; check out the PR branch and merge, rebase or cherry-pick your commits onto it (no files changed)", head, expected)
		}
	}
	w.localHead, w.remoteHead = head, expected
	w.owner, w.repo, w.branch = owner, repo, branch
	return w.Directory, nil
}

// LocalHeadOID identifies the existing committed code the agent will edit.
func (w *LocalWorkspace) LocalHeadOID() string { return w.localHead }

// ValidateBaseBranch prevents publishing a local base branch's history as a
// PR repair. Callers can perform this before fetching or waiting for remote CI.
func (w *LocalWorkspace) ValidateBaseBranch(ctx context.Context, base string) error {
	branch, err := w.git.output(ctx, w.Directory, "branch", "--show-current")
	if err != nil {
		return err
	}
	if branch == base {
		return fmt.Errorf("working directory is on base branch %q; check out the PR branch, then merge, rebase or cherry-pick your committed edits onto it before pr fix (no files changed)", base)
	}
	return nil
}

// BaseReference is separate from the user's origin tracking refs, including
// when origin points at a fork and the PR targets its upstream repository.
func (*LocalWorkspace) BaseReference(string) string { return "refs/worktree/deps-handler/base" }

func (w *LocalWorkspace) FetchBase(ctx context.Context, dir, base string) error {
	if dir != w.Directory {
		return errors.New("unexpected repair working directory")
	}
	if err := w.ValidateBaseBranch(ctx, base); err != nil {
		return err
	}
	return w.git.fetchBase(ctx, dir, w.git.url(w.owner, w.repo), base, w.BaseReference(base))
}

func (w *LocalWorkspace) CommitAndPush(ctx context.Context, dir, owner, repo, branch, expected string) (string, error) {
	if dir != w.Directory {
		return "", errors.New("unexpected repair working directory")
	}
	currentBranch, err := w.git.output(ctx, dir, "branch", "--show-current")
	if err != nil {
		return "", err
	}
	if currentBranch != w.branch {
		return "", errors.New("local branch changed during repair; edits kept locally, nothing pushed")
	}
	if expected != w.remoteHead {
		return "", errors.New("remote PR head differs from the prepared lease; nothing pushed")
	}
	return w.git.commitAndPush(ctx, dir, owner, repo, branch, w.localHead, expected)
}
