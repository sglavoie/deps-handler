// Package testutil builds GitHub fixtures for tests.
package testutil

import (
	"fmt"
	"strings"
	"time"

	"github.com/sglavoie/deps-merger/internal/github"
)

// T0 is the fixed reference time for fixtures.
var T0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// Message builds a Dependabot commit message; each dep is
// "name:patch|minor|major".
func Message(group string, deps ...string) string {
	var b strings.Builder
	b.WriteString("bump things\n\n---\nupdated-dependencies:\n")
	for _, d := range deps {
		name, kind, _ := strings.Cut(d, ":")
		fmt.Fprintf(&b, "- dependency-name: %s\n  dependency-type: direct:production\n  update-type: version-update:semver-%s\n", name, kind)
		if group != "" {
			fmt.Fprintf(&b, "  dependency-group: %s\n", group)
		}
	}
	b.WriteString("...\n\nSigned-off-by: dependabot[bot] <support@github.com>")
	return b.String()
}

var dependabotAuthor = github.Author{Login: "dependabot[bot]", Name: "dependabot[bot]"}

// PR returns a clean, green, single-dependency Dependabot PR.
func PR(number int, deps ...string) github.PullRequest {
	if len(deps) == 0 {
		deps = []string{"lib:patch"}
	}
	group := ""
	if len(deps) > 1 {
		group = "grp"
	}
	return github.PullRequest{
		Number:      number,
		Title:       fmt.Sprintf("bump %d", number),
		Author:      "dependabot",
		HeadOID:     fmt.Sprintf("sha%d", number),
		Mergeable:   "MERGEABLE",
		MergeState:  "CLEAN",
		CommitCount: 1,
		Commits: []github.Commit{{
			OID: fmt.Sprintf("c%d", number), Message: Message(group, deps...),
			CommittedDate: T0, Authors: []github.Author{dependabotAuthor},
		}},
		Checks: []github.Check{{Name: "build", State: github.CheckPassed}},
	}
}

// Repo returns a writable squash-only repo holding prs.
func Repo(prs ...github.PullRequest) *github.Repo {
	return &github.Repo{FullName: "o/r", Permission: "WRITE", SquashAllowed: true, PullRequests: prs}
}
