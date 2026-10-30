package review

import (
	"context"
	"fmt"
	"strings"
)

// FetchBase makes the PR's base available to a repair agent, including
// history for resolving conflicts. Use a disposable Workspace for repairs.
func (w *Workspace) FetchBase(ctx context.Context, dir, base string) error {
	return w.fetchBase(ctx, dir, "origin", base, "refs/remotes/origin/"+base)
}

func (w *Workspace) fetchBase(ctx context.Context, dir, remote, base, reference string) error {
	return w.fetchRef(ctx, dir, remote, "refs/heads/"+base, reference)
}

func (w *Workspace) fetchRef(ctx context.Context, dir, remote, source, reference string) error {
	if err := w.git(ctx, dir, "check-ref-format", source); err != nil {
		return err
	}
	args := []string{"fetch", "-q", "--no-tags"}
	shallow, err := w.output(ctx, dir, "rev-parse", "--is-shallow-repository")
	if err != nil {
		return err
	}
	if shallow == "true" {
		args = append(args, "--unshallow")
	}
	args = append(args, remote, "+"+source+":"+reference)
	return w.git(ctx, dir, args...)
}

// CommitAndPush commits the agent's worktree edits and updates only the
// explicitly selected PR branch. A lease rejects a concurrent remote push;
// the local HEAD must still be the checked-out PR commit.
func (w *Workspace) CommitAndPush(ctx context.Context, dir, owner, repo, branch, expected string) (string, error) {
	return w.commitAndPush(ctx, dir, owner, repo, branch, expected, expected)
}

// commitAndPush pins the agent's starting HEAD separately from the remote
// lease, so pre-existing local commits can be published without rewriting them.
func (w *Workspace) commitAndPush(ctx context.Context, dir, owner, repo, branch, localHead, remoteHead string) (string, error) {
	if err := w.git(ctx, dir, "check-ref-format", "refs/heads/"+branch); err != nil {
		return "", err
	}
	head, err := w.output(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	if head != localHead {
		return "", fmt.Errorf("agent changed HEAD; refusing to publish (expected %s, got %s)", localHead, head)
	}
	status, err := w.output(ctx, dir, "status", "--porcelain")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(status) == "" {
		if localHead == remoteHead {
			return "", fmt.Errorf("agent made no file changes; nothing to push")
		}
	} else {
		if err := w.git(ctx, dir, "add", "--all"); err != nil {
			return "", err
		}
		if err := w.git(ctx, dir, "-c", "user.name=deps-merger", "-c", "user.email=deps-merger@users.noreply.github.com", "-c", "commit.gpgSign=false", "commit", "-qm", "fix: address pull request checks"); err != nil {
			return "", err
		}
	}
	oid, err := w.output(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	if err := w.git(ctx, dir, "merge-base", "--is-ancestor", remoteHead, oid); err != nil {
		return "", fmt.Errorf("repair does not descend from PR head: %w", err)
	}
	ref := "refs/heads/" + branch
	if err := w.git(ctx, dir, "push", "--porcelain", "--force-with-lease="+ref+":"+remoteHead, w.url(owner, repo), "HEAD:"+ref); err != nil {
		return "", err
	}
	return oid, nil
}
