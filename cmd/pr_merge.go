package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"

	"github.com/sglavoie/deps-handler/pkg/github"
	"github.com/sglavoie/deps-handler/pkg/pr"
	"github.com/sglavoie/deps-handler/pkg/report"
	"github.com/sglavoie/deps-handler/pkg/review"
	"github.com/spf13/cobra"
)

var newPRMergeClient = github.NewMergeClient

func newPRMergeCmd() *cobra.Command {
	var repos, owners []string
	var opts pr.MergeOptions
	var output *outputFormat
	c := &cobra.Command{
		Use:   "merge [owner/repo#number | PR-URL ...]",
		Short: "Preview merges for any PR author; --apply merges eligible PRs",
		Long: `Merge explicitly named PRs, or discover open PRs using the same filters as
pr list: --owner selects one user or organization; repeat --repo for full
owner/name references or bare repository names. Both filters intersect.
With no targets or filters, select all accessible repositories. Explicit PR
references cannot be combined with filters. A PR number is accepted with
one --repo owner/name, or --owner plus one bare --repo.

Preview is the default; --apply performs merges. Any author is eligible,
including Dependabot. No agent, checkout or project config is required.
Discovery completes before any merge, oldest PR first. PRs are processed
serially with fresh state, and blocked PRs do not stop the remaining work.
Drafts and closed PRs are skipped. GitHub must report clean mergeability,
write access and satisfied review requirements. Existing checks must pass;
a complete empty check list is allowed. Pending or incomplete CI blocks.
This command does not repair, review, rerun CI or bypass branch protection.

Merges pin the current head SHA. Failed requests are checked against GitHub
state and never retried automatically. --merge-method requires squash,
merge or rebase; otherwise prefer the first enabled method in that order.
Exit 0 means preview completed or all eligible PRs merged; exit 1 means an
error or blocked apply; invalid arguments exit 2 and interrupts exit 130.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := output.resolve(cmd)
			if err != nil {
				return err
			}
			owner, err := validatePRSelection(cmd, repos, owners)
			if err != nil {
				return err
			}
			if cmd.Flags().Changed("merge-method") && !slices.Contains([]string{"squash", "merge", "rebase"}, opts.MergeMethod) {
				return usageError{errors.New("--merge-method needs squash, merge or rebase")}
			}
			targets := []github.PRTarget{}
			seen := map[string]bool{}
			for _, arg := range args {
				reference := arg
				if len(repos) > 0 || owner != "" {
					// Numeric PRs need exactly one fully determined repository.
					if len(repos) != 1 || strings.ContainsAny(arg, "/#") {
						return usageError{errors.New("provide PR references or repository filters, exclusively; PR numbers require one explicit repository")}
					}
					repository := repos[0]
					if !strings.Contains(repository, "/") && owner != "" {
						repository = owner + "/" + repository
					}
					reference = repository + "#" + arg
				}
				target, err := github.ParsePRTarget(reference)
				if err != nil {
					return usageError{err}
				}
				key := strings.ToLower(target.String())
				if seen[key] {
					return usageError{fmt.Errorf("PR %s was selected twice", target)}
				}
				seen[key] = true
				targets = append(targets, target)
			}
			if !prCommandsAvailable {
				return errors.New("pr commands are unavailable in the demo build")
			}
			client, err := newPRMergeClient()
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
			defer stop()
			if len(args) == 0 {
				selected, err := selectPRListRepositories(ctx, client, owner, repos)
				if err != nil {
					if ctx.Err() != nil {
						return errInterrupted
					}
					return err
				}
				listed := []github.ListedPR{}
				for _, repository := range selected {
					o, name, _ := github.ParseRepository(repository)
					prs, err := client.ListOpenPRs(ctx, o, name)
					if err != nil {
						if ctx.Err() != nil {
							return errInterrupted
						}
						return err
					}
					listed = append(listed, prs...)
				}
				slices.SortFunc(listed, func(a, b github.ListedPR) int {
					if n := a.CreatedAt.Compare(b.CreatedAt); n != 0 {
						return n
					}
					if n := strings.Compare(strings.ToLower(a.Repository), strings.ToLower(b.Repository)); n != 0 {
						return n
					}
					return a.Number - b.Number
				})
				for _, p := range listed {
					target, err := github.ParsePRTarget(fmt.Sprintf("%s#%d", p.Repository, p.Number))
					if err != nil {
						return err
					}
					key := strings.ToLower(target.String())
					if !seen[key] {
						targets = append(targets, target)
						seen[key] = true
					}
				}
			}
			results := []pr.MergeResult{}
			failed := false
			for _, target := range targets {
				if ctx.Err() != nil {
					break
				}
				result, mergeErr := pr.Merge(ctx, client, target, opts)
				results = append(results, result)
				failed = failed || mergeErr != nil || (opts.Apply && result.Status == "blocked")
			}
			if err := writePRMerge(cmd, results, opts.Apply, format); err != nil {
				return err
			}
			if ctx.Err() != nil {
				return errInterrupted
			}
			if failed {
				return errors.New("some PRs could not be merged; see their reported blockers and errors")
			}
			return nil
		},
	}
	c.Flags().StringArrayVarP(&repos, "repo", "r", nil, "select repositories (owner/name or bare name; repeatable)")
	c.Flags().StringArrayVar(&owners, "owner", nil, "select repositories of this owner (once)")
	keepEmpty(c, "repo", "owner")
	c.Flags().BoolVar(&opts.Apply, "apply", false, "merge eligible PRs (default: preview only)")
	c.Flags().StringVar(&opts.MergeMethod, "merge-method", "", "require squash, merge or rebase (default: first enabled in that order)")
	output = addOutputFlags(c, false)
	return c
}

func writePRMerge(cmd *cobra.Command, results []pr.MergeResult, apply bool, format string) error {
	if format == "json" {
		data, err := json.MarshalIndent(results, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), review.Clean(string(data)))
		return err
	}
	if !apply {
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), "Preview only. Use --apply to merge eligible PRs."); err != nil {
			return err
		}
	}
	if len(results) == 0 {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), "No open PRs selected.")
		return err
	}
	rows := [][]string{}
	for _, r := range results {
		reason := strings.Join(r.Blockers, "; ")
		if r.Error != "" {
			reason = r.Error
		}
		rows = append(rows, []string{r.Target.String(), r.Status, r.MergeMethod, reason})
	}
	if format == "table" {
		return report.Table(cmd.OutOrStdout(), []string{"PR", "OUTCOME", "METHOD", "REASON"}, rows)
	}
	for _, row := range rows {
		for i := range row {
			row[i] = report.Plain(row[i])
		}
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), strings.Join(row, "\t")); err != nil {
			return err
		}
	}
	return nil
}
