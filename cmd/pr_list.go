package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/sglavoie/deps-handler/pkg/github"
	"github.com/sglavoie/deps-handler/pkg/report"
	"github.com/spf13/cobra"
)

var newPRListClient = github.NewPRListClient

func newPRListCmd() *cobra.Command {
	var repos, owners []string
	var output *outputFormat
	c := &cobra.Command{
		Use:   "list",
		Short: "List open PRs across repositories",
		Long: `List all open pull requests, including drafts and Dependabot PRs, in
repositories accessible to your GitHub account. This does not require a
deps-handler config. --owner narrows the listing to one user or organization.
Repeat --repo to select several repositories; use owner/name directly, or a
bare name to match repositories of that name (within --owner when set).
With neither filter, all accessible repositories are read.
--owner and --repo together select repositories matching both filters.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := output.resolve(cmd)
			if err != nil {
				return err
			}
			owner, err := validatePRSelection(cmd, repos, owners)
			if err != nil {
				return err
			}
			if !prCommandsAvailable {
				return errors.New("pr commands are unavailable in the demo build")
			}
			client, err := newPRListClient()
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
			defer stop()
			selected, err := selectPRListRepositories(ctx, client, owner, repos)
			if err != nil {
				if ctx.Err() != nil {
					return errInterrupted
				}
				return err
			}
			prs := []github.ListedPR{}
			for _, repository := range selected {
				o, name, _ := github.ParseRepository(repository)
				listed, err := client.ListOpenPRs(ctx, o, name)
				if err != nil {
					if ctx.Err() != nil {
						return errInterrupted
					}
					return err
				}
				prs = append(prs, listed...)
			}
			if ctx.Err() != nil {
				return errInterrupted
			}
			slices.SortFunc(prs, func(a, b github.ListedPR) int {
				if n := a.CreatedAt.Compare(b.CreatedAt); n != 0 {
					return n
				}
				if n := strings.Compare(strings.ToLower(a.Repository), strings.ToLower(b.Repository)); n != 0 {
					return n
				}
				return a.Number - b.Number
			})
			return writePRList(cmd, prs, format)
		},
	}
	output = addOutputFlags(c, false)
	c.Flags().StringArrayVarP(&repos, "repo", "r", nil, "only list these repositories (owner/name, or name with --owner; repeatable)")
	c.Flags().StringArrayVar(&owners, "owner", nil, "only list repositories of this owner (once)")
	keepEmpty(c, "repo", "owner")
	return c
}

func selectPRListRepositories(ctx context.Context, client github.PRListClient, owner string, repos []string) ([]string, error) {
	selected := []string{}
	seen := map[string]bool{}
	bare := map[string]bool{}
	matched := map[string]bool{}
	for _, repo := range repos {
		full := repo
		if !strings.Contains(repo, "/") {
			if owner == "" {
				bare[strings.ToLower(repo)] = true
				continue
			}
			full = owner + "/" + repo
		}
		key := strings.ToLower(full)
		if !seen[key] {
			selected = append(selected, full)
			seen[key] = true
		}
	}
	if len(repos) == 0 || len(bare) > 0 {
		available, err := client.ListRepositories(ctx, owner)
		if err != nil {
			return nil, err
		}
		for _, full := range available {
			_, name, _ := github.ParseRepository(full)
			keyName := strings.ToLower(name)
			if len(repos) > 0 && !bare[keyName] {
				continue
			}
			matched[keyName] = true
			key := strings.ToLower(full)
			if !seen[key] {
				selected = append(selected, full)
				seen[key] = true
			}
		}
		for _, name := range slices.Sorted(maps.Keys(bare)) {
			if !matched[name] {
				return nil, usageError{fmt.Errorf("no accessible repository matches --repo %q", name)}
			}
		}
	}
	slices.SortFunc(selected, func(a, b string) int { return strings.Compare(strings.ToLower(a), strings.ToLower(b)) })
	return selected, nil
}

func writePRList(cmd *cobra.Command, prs []github.ListedPR, format string) error {
	if format == "json" {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(struct {
			PullRequests []github.ListedPR `json:"pull_requests"`
		}{prs})
	}
	if len(prs) == 0 {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), "no open PRs")
		return err
	}
	headers := []string{"PR", "TITLE", "AUTHOR", "DRAFT", "CREATED", "URL"}
	rows := make([][]string, 0, len(prs))
	for _, p := range prs {
		rows = append(rows, []string{fmt.Sprintf("%s#%d", p.Repository, p.Number), p.Title, p.Author, yesNo(p.Draft), p.CreatedAt.Local().Format("2006-01-02"), p.URL})
	}
	if format == "table" {
		return report.Table(cmd.OutOrStdout(), headers, rows)
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(headers, "\t"))
	for _, row := range rows {
		for i := range row {
			row[i] = report.Plain(row[i])
		}
		fmt.Fprintln(tw, strings.Join(row, "\t"))
	}
	return tw.Flush()
}

// validatePRSelection keeps repository filters identical across list and merge.
func validatePRSelection(cmd *cobra.Command, repos, owners []string) (string, error) {
	if err := checkSelection(cmd, repos, owners); err != nil {
		return "", err
	}
	if len(owners) > 1 {
		return "", usageError{errors.New("--owner accepts one value only")}
	}
	owner := ""
	if len(owners) == 1 {
		owner = owners[0]
		if _, _, err := github.ParseRepository(owner + "/valid"); err != nil {
			return "", usageError{fmt.Errorf("invalid owner %q", owner)}
		}
	}
	for _, repo := range repos {
		if strings.Contains(repo, "/") {
			o, _, err := github.ParseRepository(repo)
			if err != nil {
				return "", usageError{err}
			}
			if owner != "" && !strings.EqualFold(o, owner) {
				return "", usageError{fmt.Errorf("--repo %q is outside --owner %q", repo, owner)}
			}
		} else if _, _, err := github.ParseRepository("valid/" + repo); err != nil {
			return "", usageError{fmt.Errorf("invalid repository name %q", repo)}
		}
	}
	return owner, nil
}
