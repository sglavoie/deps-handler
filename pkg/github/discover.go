package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/cli/go-gh/v2/pkg/api"
)

// maxDiscoverPages bounds how many pages of 100 repositories one listing
// reads, so an account that reaches thousands cannot hold init for minutes.
const maxDiscoverPages = 20

// repoFields are the fields DependabotRepos reads of each repository. A
// Dependabot config is .github/dependabot.yml, or .yaml, on the default
// branch.
const repoFields = `pageInfo { hasNextPage endCursor }
      nodes {
        nameWithOwner isArchived isFork viewerPermission
        yml: object(expression: "HEAD:.github/dependabot.yml") { id }
        yaml: object(expression: "HEAD:.github/dependabot.yaml") { id }
      }`

const viewerReposQuery = `
query($after: String) {
  viewer {
    repositories(first: 100, after: $after, ownerAffiliations: [OWNER, COLLABORATOR, ORGANIZATION_MEMBER], orderBy: {field: NAME, direction: ASC}) {
      ` + repoFields + `
    }
  }
}`

const ownerReposQuery = `
query($owner: String!, $after: String) {
  repositoryOwner(login: $owner) {
    repositories(first: 100, after: $after, orderBy: {field: NAME, direction: ASC}) {
      ` + repoFields + `
    }
  }
}`

type repoPage struct {
	PageInfo *struct {
		HasNextPage *bool
		EndCursor   string
	}
	Nodes []discoveredRepo
}

type discoveredRepo struct {
	NameWithOwner    string
	IsArchived       bool
	IsFork           bool
	ViewerPermission string
	Yml, Yaml        *struct{ ID string }
}

// UnmarshalJSON checks each item before filtering can discard it. Missing
// fields cannot establish that a repository has no Dependabot config or
// is eligible for discovery. Permission and file lookups may explicitly
// be null, but archive and fork flags must always be known.
func (r *discoveredRepo) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for _, key := range []string{"nameWithOwner", "isArchived", "isFork", "viewerPermission", "yml", "yaml"} {
		value, ok := fields[key]
		if !ok || (key == "nameWithOwner" || key == "isArchived" || key == "isFork") && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("incomplete repository listing: missing %s", key)
		}
	}
	type plain discoveredRepo
	if err := json.Unmarshal(data, (*plain)(r)); err != nil {
		return err
	}
	owner, name, ok := strings.Cut(r.NameWithOwner, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return errors.New("incomplete repository listing: invalid nameWithOwner")
	}
	if r.Yml != nil && r.Yml.ID == "" || r.Yaml != nil && r.Yaml.ID == "" {
		return errors.New("incomplete repository listing: unidentified Dependabot config")
	}
	return nil
}

// DependabotRepos lists, as owner/name, the repositories of owners, or
// those the gh login owns, collaborates on or can reach as an organisation
// member when owners is empty, that hold a Dependabot config and that it
// can write to, leaving out archived repositories and forks. The list is
// sorted, case aside, each repository once.
func DependabotRepos(ctx context.Context, owners []string) ([]string, error) {
	opts, err := githubComOptions()
	if err != nil {
		return nil, err
	}
	gql, err := api.NewGraphQLClient(opts)
	if err != nil {
		return nil, fmt.Errorf("github auth (run `gh auth login`): %w", err)
	}
	return (&client{gql: gql}).dependabotRepos(ctx, owners)
}

func (c *client) dependabotRepos(ctx context.Context, owners []string) ([]string, error) {
	var out []string
	// owner, when set, is the owner whose listing is read: a user's also
	// holds the repositories they collaborate on, which are not theirs.
	owner := ""
	add := func(p repoPage) {
		for _, n := range p.Nodes {
			if n.IsArchived || n.IsFork || n.Yml == nil && n.Yaml == nil {
				continue
			}
			if o, _, _ := strings.Cut(n.NameWithOwner, "/"); owner != "" && !strings.EqualFold(o, owner) {
				continue
			}
			if (&Repo{Permission: n.ViewerPermission}).CanWrite() {
				out = append(out, n.NameWithOwner)
			}
		}
	}
	if len(owners) == 0 {
		if err := pages(ctx, c, viewerReposQuery, map[string]any{}, func(raw *struct {
			Viewer struct{ Repositories repoPage }
		}) repoPage {
			return raw.Viewer.Repositories
		}, add); err != nil {
			return nil, err
		}
	}
	for _, owner = range owners {
		found := true
		err := pages(ctx, c, ownerReposQuery, map[string]any{"owner": owner}, func(raw *struct {
			RepositoryOwner *struct{ Repositories repoPage }
		}) repoPage {
			if raw.RepositoryOwner == nil {
				found = false
				return repoPage{}
			}
			return raw.RepositoryOwner.Repositories
		}, add)
		if !found {
			err = errors.New("no such user or organisation")
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", owner, err)
		}
	}
	slices.SortFunc(out, func(a, b string) int { return strings.Compare(strings.ToLower(a), strings.ToLower(b)) })
	return slices.CompactFunc(out, strings.EqualFold), nil
}

// pages runs query for each page of a repository listing, passing each
// page that page picks out of the response to add. A listing longer than
// maxDiscoverPages is an error rather than cut short without a word.
func pages[T any](ctx context.Context, c *client, query string, vars map[string]any, page func(*T) repoPage, add func(repoPage)) error {
	seen := map[string]bool{}
	for range maxDiscoverPages {
		var raw T
		if err := c.gql.DoWithContext(ctx, query, vars, &raw); err != nil {
			return c.withReset(ctx, err)
		}
		p := page(&raw)
		if p.PageInfo == nil || p.PageInfo.HasNextPage == nil || p.Nodes == nil {
			return errors.New("incomplete repository listing")
		}
		add(p)
		if !*p.PageInfo.HasNextPage {
			return nil
		}
		if p.PageInfo.EndCursor == "" || seen[p.PageInfo.EndCursor] {
			return errors.New("invalid repository listing cursor: missing or repeated")
		}
		seen[p.PageInfo.EndCursor] = true
		vars["after"] = p.PageInfo.EndCursor
	}
	return fmt.Errorf("more than %d repositories to look through; name the owners to look at", maxDiscoverPages*100)
}
