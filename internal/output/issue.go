// Package output keeps each repository's "Dependency health" issue: the
// one thing the App writes while in report mode.
package output

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	gh "github.com/google/go-github/v88/github"
	"github.com/sglavoie/deps-merger/pkg/config"
	"github.com/sglavoie/deps-merger/pkg/report"
	"github.com/sglavoie/deps-merger/pkg/runner"
)

// Title is the issue's title; Marker, hidden in its body, is what finds
// it again, together with its author being the App.
const (
	Title  = "Dependency health"
	Marker = "<!-- deps-merger:dependency-health -->"
)

// Keep reports comfortably within GitHub's issue body limit, including
// escaped text and the notice. A byte cap also bounds Unicode characters.
const maxBodyBytes = 60_000

const truncatedNotice = "\n\n**Report truncated.** More results did not fit in this issue; check the repository's Dependabot pull requests for the remaining details.\n"

// Body renders res as the issue's body. merging says whether the run was
// allowed to merge: merging is enabled on the deployment and not paused
// for the installation. It holds nothing that changes from run to run
// unless the repository does, such as the time, so an unchanged
// repository leaves the issue untouched.
func Body(res runner.RepoResult, merging bool) string {
	var b bytes.Buffer
	b.WriteString(Marker + "\n")
	b.WriteString("deps-merger reports here on this repository's Dependabot PRs. ")
	switch {
	case res.Policy.Mode != config.ModeMerge:
		b.WriteString("It is in report mode: it changes nothing but this issue. To let it merge the PRs it finds safe, add `mode: merge` to `.github/deps-merger.yml`, where its other policy fields go too.")
	case merging:
		b.WriteString("It merges the Dependabot PRs it finds safe (`mode: merge`), one at a time, and lists the rest here. Policy fields go in `.github/deps-merger.yml`.")
	default:
		b.WriteString("`mode: merge` is set, but merging is paused or not enabled for this installation: it only reports. Policy fields go in `.github/deps-merger.yml`.")
	}
	b.WriteString("\n\n")
	report.Markdown(&b, merging, res)
	body := strings.TrimRight(b.String(), "\n") + "\n"
	if len(body) > maxBodyBytes {
		prefix := body[:maxBodyBytes-len(truncatedNotice)]
		// Retain complete Markdown lines rather than a partial table row,
		// escape sequence or UTF-8 character. The marker stays at the top.
		prefix = prefix[:strings.LastIndexByte(prefix, '\n')]
		body = prefix + truncatedNotice
	}
	return body
}

// Wanted reports whether res is worth an open issue: open PRs, findings
// or an error. Otherwise an open issue is closed, and none is created.
func Wanted(res runner.RepoResult) bool {
	return res.Error != "" || len(res.Decisions) > 0 || len(res.Findings) > 0
}

// Change is what Sync did to the issue.
type Change string

const (
	Unchanged Change = ""
	Created   Change = "issue-created"
	Updated   Change = "issue-updated"
	Closed    Change = "issue-closed"
	Reopened  Change = "issue-reopened"
)

// ErrIssuesDisabled is a repository with its issues turned off.
var ErrIssuesDisabled = errors.New("issues are disabled")

// Sync makes repo's issue show body, open when open is true. known is the
// issue's number as last recorded, or 0. Without create, an issue that
// does not exist yet is not created: a run that failed updates the issue,
// but a failure alone, which is the App's to fix, does not open one. It
// returns the issue's number (0 when there is none) and what it changed.
func Sync(ctx context.Context, c *gh.Client, login, owner, name string, known int, body string, open, create bool) (int, Change, error) {
	issue, err := find(ctx, c, login, owner, name, known)
	if err != nil {
		return known, Unchanged, err
	}
	if issue == nil {
		if !open || !create {
			return 0, Unchanged, nil
		}
		created, resp, err := c.Issues.Create(ctx, owner, name, &gh.IssueRequest{Title: gh.Ptr(Title), Body: gh.Ptr(body)})
		if resp != nil && resp.StatusCode == http.StatusGone {
			return 0, Unchanged, ErrIssuesDisabled
		}
		if err != nil {
			return 0, Unchanged, fmt.Errorf("create issue: %w", err)
		}
		// A successful status alone cannot identify the issue we created.
		// Leave its outcome unconfirmed for the next lookup to reconcile;
		// do not report issue #0 or pin an unidentified response.
		if err := validateIssue(created); err != nil {
			return 0, Unchanged, fmt.Errorf("create issue: could not confirm creation: %w", err)
		}
		if !ours(created, login) {
			return 0, Unchanged, errors.New("create issue: could not confirm creation of the App's health issue")
		}
		// Pinning is a courtesy: a repository pins at most three issues.
		if nodeID := created.GetNodeID(); strings.TrimSpace(nodeID) != "" {
			_ = pin(ctx, c, nodeID)
		}
		return created.GetNumber(), Created, nil
	}

	n := issue.GetNumber()
	isOpen := issue.GetState() == "open"
	if issue.GetBody() == body && isOpen == open {
		return n, Unchanged, nil
	}
	req := &gh.IssueRequest{Body: gh.Ptr(body)}
	change := Updated
	switch {
	case open && !isOpen:
		req.State, change = gh.Ptr("open"), Reopened
	case !open && isOpen:
		req.State, req.StateReason, change = gh.Ptr("closed"), gh.Ptr("completed"), Closed
	}
	if _, _, err := c.Issues.Edit(ctx, owner, name, n, req); err != nil {
		return n, Unchanged, fmt.Errorf("edit issue #%d: %w", n, err)
	}
	return n, change, nil
}

// find returns the App's issue in the repository: number known if it
// still is one, or else the newest one it opened that carries Marker.
func find(ctx context.Context, c *gh.Client, login, owner, name string, known int) (*gh.Issue, error) {
	if known > 0 {
		issue, resp, err := c.Issues.Get(ctx, owner, name, known)
		switch {
		case err == nil:
			if err := validateIssue(issue); err != nil {
				return nil, fmt.Errorf("issue #%d: %w", known, err)
			}
			if issue.GetNumber() != known {
				return nil, fmt.Errorf("issue #%d: response identifies issue #%d", known, issue.GetNumber())
			}
			if ours(issue, login) {
				return issue, nil
			}
		case err != nil && (resp == nil || (resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusGone)):
			return nil, fmt.Errorf("issue #%d: %w", known, err)
		}
	}
	opts := &gh.IssueListByRepoOptions{Creator: login, State: "all", Sort: "created", Direction: "desc", ListOptions: gh.ListOptions{PerPage: 100, Page: 1}}
	seenCursors := map[string]bool{}
	for {
		page, resp, err := c.Issues.ListByRepo(ctx, owner, name, opts)
		if er, ok := errors.AsType[*gh.ErrorResponse](err); ok && er.Response.StatusCode == http.StatusGone {
			return nil, ErrIssuesDisabled
		}
		if err != nil {
			return nil, fmt.Errorf("list issues: %w", err)
		}
		// The iterator discards null pages, which cannot establish that
		// the health issue is absent. Validate entries before looking for
		// ours so a missing list or unidentified issue cannot cause a
		// duplicate or edit.
		if page == nil {
			return nil, errors.New("list issues: missing issue list")
		}
		for _, issue := range page {
			if err := validateIssue(issue); err != nil {
				return nil, fmt.Errorf("list issues: %w", err)
			}
		}
		for _, issue := range page {
			if ours(issue, login) {
				return issue, nil
			}
		}
		if resp.After == "" && resp.NextPage == 0 {
			return nil, nil
		}
		if len(page) == 0 {
			return nil, errors.New("list issues: invalid pagination")
		}
		if resp.After != "" {
			if seenCursors[resp.After] {
				return nil, errors.New("list issues: repeated pagination cursor")
			}
			seenCursors[resp.After] = true
		} else if resp.NextPage != opts.ListOptions.Page+1 {
			return nil, errors.New("list issues: invalid pagination")
		}
		opts.ListCursorOptions.After = resp.After
		opts.ListOptions.Page = resp.NextPage
	}
}

func validateIssue(issue *gh.Issue) error {
	if issue == nil || issue.GetNumber() <= 0 || strings.TrimSpace(issue.GetUser().GetLogin()) == "" {
		return errors.New("incomplete issue identity")
	}
	switch issue.GetState() {
	case "open", "closed":
		return nil
	default:
		return fmt.Errorf("issue #%d: unrecognized state %q", issue.GetNumber(), issue.GetState())
	}
}

func ours(issue *gh.Issue, login string) bool {
	return !issue.IsPullRequest() && strings.EqualFold(issue.GetUser().GetLogin(), login) && strings.Contains(issue.GetBody(), Marker)
}

func pin(ctx context.Context, c *gh.Client, nodeID string) error {
	req, err := c.NewRequest(ctx, http.MethodPost, "graphql", map[string]any{
		"query":     `mutation($id: ID!) { pinIssue(input: {issueId: $id}) { issue { number } } }`,
		"variables": map[string]any{"id": nodeID},
	})
	if err != nil {
		return err
	}
	_, err = c.Do(req, nil)
	return err
}
