package pr

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/sglavoie/deps-handler/pkg/github"
)

type MergeOptions struct {
	Apply       bool
	MergeMethod string
}

type MergeResult struct {
	Target         github.PRTarget   `json:"target"`
	Snapshot       github.PRSnapshot `json:"snapshot"`
	Status         string            `json:"status"`
	Blockers       []string          `json:"blockers"`
	MergeMethod    string            `json:"merge_method,omitempty"`
	MergeAttempted bool              `json:"merge_attempted"`
	Error          string            `json:"error,omitempty"`
}

// Merge reads each PR immediately before merging and pins the write to its head.
// A failed write is reconciled by reading state, never retried automatically.
func Merge(ctx context.Context, client github.MergeClient, target github.PRTarget, opts MergeOptions) (result MergeResult, err error) {
	result = MergeResult{Target: target, Status: "blocked", Blockers: []string{}}
	defer func() {
		if err != nil {
			result.Status, result.Error = "error", err.Error()
		}
	}()
	if opts.MergeMethod != "" && !slices.Contains([]string{"squash", "merge", "rebase"}, opts.MergeMethod) {
		return result, errors.New("merge method must be squash, merge or rebase")
	}
	methods, err := client.MergeMethods(ctx, target.Owner, target.Repo)
	if err != nil {
		return result, err
	}
	p, err := client.Snapshot(ctx, target)
	if err != nil {
		return result, err
	}
	if p.Target != target || p.HeadOID == "" || p.BaseOID == "" {
		return result, errors.New("merge requires a matching PR target and known head and base commits")
	}
	p.Checks = latestPRChecks(p.Checks)
	result.Snapshot = p
	if p.State == "MERGED" || p.State == "CLOSED" || p.Draft {
		result.Status = "skipped"
		result.Blockers = mergeBlockers(p, false)
		return result, nil
	}
	result.Blockers = mergeBlockers(p, false)
	result.MergeMethod = opts.MergeMethod
	if result.MergeMethod == "" && len(methods) > 0 {
		result.MergeMethod = methods[0]
	}
	if !slices.Contains(methods, result.MergeMethod) {
		result.Blockers = append(result.Blockers, "Requested merge method is not enabled for this repository.")
	}
	if len(result.Blockers) > 0 {
		return result, nil
	}
	result.Status = "would_merge"
	if !opts.Apply {
		return result, nil
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	result.MergeAttempted = true
	if err := client.Merge(ctx, target.Owner, target.Repo, target.Number, result.MergeMethod, p.HeadOID); err != nil {
		state, confirmErr := client.PullRequestState(ctx, target.Owner, target.Repo, target.Number)
		if confirmErr != nil || state != github.PRMerged {
			return result, fmt.Errorf("merge was not confirmed; inspect GitHub before retrying: %w", err)
		}
	}
	result.Status = "merged"
	return result, nil
}
