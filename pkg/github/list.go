package github

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// PRListClient reads repository and open pull request listings without a
// deps-handler config or the heavier state needed for reviews.
type PRListClient interface {
	ListRepositories(context.Context, string) ([]string, error)
	ListOpenPRs(context.Context, string, string) ([]ListedPR, error)
}

type ListedPR struct {
	Repository string    `json:"repository"`
	Number     int       `json:"number"`
	Title      string    `json:"title"`
	URL        string    `json:"url"`
	Author     string    `json:"author"`
	Draft      bool      `json:"draft"`
	CreatedAt  time.Time `json:"created_at"`
}

func NewPRListClient() (PRListClient, error) {
	c, err := NewPRClient()
	if err != nil {
		return nil, err
	}
	return c.(*prClient), nil
}

const listViewerReposQuery = `query($after: String) {
 viewer { repositories(first: 100, after: $after, ownerAffiliations: [OWNER, COLLABORATOR, ORGANIZATION_MEMBER], orderBy: {field: NAME, direction: ASC}) {
  pageInfo { hasNextPage endCursor } nodes { nameWithOwner }
 } }
}`

const listOwnerReposQuery = `query($owner: String!, $after: String) {
 repositoryOwner(login: $owner) { repositories(first: 100, after: $after, orderBy: {field: NAME, direction: ASC}) {
  pageInfo { hasNextPage endCursor } nodes { nameWithOwner }
 } }
}`

// ListRepositories lists every repository reachable through the account's
// owner, collaborator or organization membership affiliations. With owner,
// it lists that owner's repositories. An incomplete page fails the command.
func (c *prClient) ListRepositories(ctx context.Context, owner string) ([]string, error) {
	type page struct {
		PageInfo *struct {
			HasNextPage *bool
			EndCursor   string
		}
		Nodes []struct{ NameWithOwner string }
	}
	repos := []string{}
	seenRepos, seenCursors := map[string]bool{}, map[string]bool{}
	var after any
	for range 100 {
		var out struct {
			Viewer          *struct{ Repositories *page }
			RepositoryOwner *struct{ Repositories *page }
		}
		query := listViewerReposQuery
		vars := map[string]any{"after": after}
		if owner != "" {
			query = listOwnerReposQuery
			vars["owner"] = owner
		}
		if err := c.gql.DoWithContext(ctx, query, vars, &out); err != nil {
			return nil, c.withReset(ctx, err)
		}
		var p *page
		if owner == "" && out.Viewer != nil {
			p = out.Viewer.Repositories
		} else if owner != "" && out.RepositoryOwner != nil {
			p = out.RepositoryOwner.Repositories
		}
		if p == nil || p.PageInfo == nil || p.PageInfo.HasNextPage == nil || p.Nodes == nil {
			return nil, errors.New("incomplete repository listing or owner not found")
		}
		for _, n := range p.Nodes {
			o, _, err := ParseRepository(n.NameWithOwner)
			if err != nil {
				return nil, errors.New("invalid repository in listing")
			}
			// A user's repositoryOwner listing also includes repositories
			// they collaborate on under other owners.
			if owner != "" && !strings.EqualFold(o, owner) {
				continue
			}
			key := strings.ToLower(n.NameWithOwner)
			if !seenRepos[key] {
				seenRepos[key] = true
				repos = append(repos, n.NameWithOwner)
			}
		}
		if !*p.PageInfo.HasNextPage {
			slices.SortFunc(repos, func(a, b string) int { return strings.Compare(strings.ToLower(a), strings.ToLower(b)) })
			return repos, nil
		}
		cursor := p.PageInfo.EndCursor
		if cursor == "" || seenCursors[cursor] || len(p.Nodes) == 0 {
			return nil, errors.New("repository listing pagination did not advance")
		}
		seenCursors[cursor], after = true, cursor
	}
	return nil, errors.New("repository listing exceeded 100 pages")
}

const listOpenPRsQuery = `query($owner: String!, $name: String!, $after: String) {
 repository(owner: $owner, name: $name) { pullRequests(first: 100, after: $after, states: OPEN, orderBy: {field: CREATED_AT, direction: ASC}) {
  pageInfo { hasNextPage endCursor }
  nodes { number title url isDraft createdAt author { login } }
 } }
}`

// ListOpenPRs returns every open PR in a repository, including drafts and
// Dependabot PRs. No partial listing is returned after a failed page.
func (c *prClient) ListOpenPRs(ctx context.Context, owner, name string) ([]ListedPR, error) {
	if _, _, err := ParseRepository(owner + "/" + name); err != nil {
		return nil, err
	}
	prs := []ListedPR{}
	seenPRs, seenCursors := map[int]bool{}, map[string]bool{}
	var after any
	for range 100 {
		var out struct {
			Repository *struct {
				PullRequests *struct {
					PageInfo *struct {
						HasNextPage *bool
						EndCursor   string
					}
					Nodes []struct {
						Number    int
						Title     string
						URL       string
						IsDraft   *bool
						CreatedAt time.Time
						Author    *struct{ Login string }
					}
				}
			}
		}
		if err := c.gql.DoWithContext(ctx, listOpenPRsQuery, map[string]any{"owner": owner, "name": name, "after": after}, &out); err != nil {
			return nil, fmt.Errorf("%s/%s: %w", owner, name, c.withReset(ctx, err))
		}
		if out.Repository == nil || out.Repository.PullRequests == nil {
			return nil, fmt.Errorf("%s/%s: repository or open PR list is missing", owner, name)
		}
		p := out.Repository.PullRequests
		if p.PageInfo == nil || p.PageInfo.HasNextPage == nil || p.Nodes == nil {
			return nil, fmt.Errorf("%s/%s: incomplete open PR listing", owner, name)
		}
		for _, n := range p.Nodes {
			if n.Number <= 0 || seenPRs[n.Number] || n.Title == "" || n.URL == "" || n.IsDraft == nil || n.CreatedAt.IsZero() {
				return nil, fmt.Errorf("%s/%s: invalid or duplicate PR in listing", owner, name)
			}
			seenPRs[n.Number] = true
			author := "(deleted)"
			if n.Author != nil && n.Author.Login != "" {
				author = n.Author.Login
			}
			prs = append(prs, ListedPR{owner + "/" + name, n.Number, n.Title, n.URL, author, *n.IsDraft, n.CreatedAt})
		}
		if !*p.PageInfo.HasNextPage {
			return prs, nil
		}
		cursor := p.PageInfo.EndCursor
		if cursor == "" || seenCursors[cursor] || len(p.Nodes) == 0 {
			return nil, fmt.Errorf("%s/%s: open PR pagination did not advance", owner, name)
		}
		seenCursors[cursor], after = true, cursor
	}
	return nil, fmt.Errorf("%s/%s: open PR listing exceeded 100 pages", owner, name)
}
