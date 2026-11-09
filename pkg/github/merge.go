package github

import (
	"context"
	"errors"
)

// MergeClient exposes direct merging without AI review or repair operations.
type MergeClient interface {
	PRListClient
	Snapshot(context.Context, PRTarget) (PRSnapshot, error)
	MergeMethods(context.Context, string, string) ([]string, error)
	Merge(context.Context, string, string, int, string, string) error
	PullRequestState(context.Context, string, string, int) (PRState, error)
}

func NewMergeClient() (MergeClient, error) {
	c, err := NewPRClient()
	if err != nil {
		return nil, err
	}
	return c.(*prClient), nil
}

func (c *prClient) MergeMethods(ctx context.Context, owner, name string) ([]string, error) {
	var out struct {
		Repository *struct {
			SquashMergeAllowed, MergeCommitAllowed, RebaseMergeAllowed *bool
		}
	}
	err := c.gql.DoWithContext(ctx, `query($owner: String!, $name: String!) {
 repository(owner: $owner, name: $name) { squashMergeAllowed mergeCommitAllowed rebaseMergeAllowed }
}`, map[string]any{"owner": owner, "name": name}, &out)
	if err != nil {
		return nil, c.withReset(ctx, err)
	}
	r := out.Repository
	if r == nil || r.SquashMergeAllowed == nil || r.MergeCommitAllowed == nil || r.RebaseMergeAllowed == nil {
		return nil, errors.New("repository merge methods are missing")
	}
	methods := []string{}
	for _, m := range []struct {
		name    string
		allowed bool
	}{{"squash", *r.SquashMergeAllowed}, {"merge", *r.MergeCommitAllowed}, {"rebase", *r.RebaseMergeAllowed}} {
		if m.allowed {
			methods = append(methods, m.name)
		}
	}
	return methods, nil
}
