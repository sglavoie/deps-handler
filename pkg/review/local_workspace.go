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
	Directory           string
	git                 Workspace
	owner, repo, branch string
	release             func()
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
	release, err := Lock(filepath.Join(gitDir, "deps-merger-pr.lock"))
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

func (w *LocalWorkspace) Checkout(ctx context.Context, owner, repo string, _ int, expected string) (string, error) {
	head, err := w.git.output(ctx, w.Directory, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	if head != expected {
		return "", fmt.Errorf("working directory HEAD is %s; selected PR head is %s; check out the PR first (no files changed)", head, expected)
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
	w.owner, w.repo, w.branch = owner, repo, branch
	return w.Directory, nil
}

// BaseReference is separate from the user's origin tracking refs, including
// when origin points at a fork and the PR targets its upstream repository.
func (*LocalWorkspace) BaseReference(string) string { return "refs/worktree/deps-merger/base" }

func (w *LocalWorkspace) FetchBase(ctx context.Context, dir, base string) error {
	if dir != w.Directory {
		return errors.New("unexpected repair working directory")
	}
	if w.branch == base {
		return fmt.Errorf("working directory is on base branch %q; check out the PR branch or detach at its head before pr fix", base)
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
	return w.git.CommitAndPush(ctx, dir, owner, repo, branch, expected)
}
