// Package cmd holds the deps-merger command line.
package cmd

import (
	"fmt"
	"os"

	"github.com/sglavoie/deps-merger/internal/config"
	"github.com/spf13/cobra"
)

// Set at build time through -ldflags.
var (
	Version   = "dev"
	GitCommit = "unknown"
	BuildTime = "unknown"
)

var configPath string

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:   "deps-merger",
		Short: "Merge safe Dependabot pull requests across your repositories",
		Long: `deps-merger finds open Dependabot PRs in the repositories listed in its
config, merges those that are green and within policy, and reports what
needs fixing in each repository. PRs not opened by Dependabot, or touched
by anyone else, are never changed.`,
		Version:       fmt.Sprintf("%s (%s, built %s)", Version, GitCommit, BuildTime),
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVarP(&configPath, "config", "c", "", "config file (default $XDG_CONFIG_HOME/deps-merger/config.yaml)")
	root.AddCommand(newRunCmd(), newReposCmd())
	return root
}

// Execute runs the CLI and returns the process exit code.
func Execute() int {
	if err := newRoot().Execute(); err != nil {
		if _, ok := err.(exitError); !ok {
			fmt.Fprintln(os.Stderr, "deps-merger:", err)
		}
		return 1
	}
	return 0
}

// exitError signals a non-zero exit whose details were already printed.
type exitError struct{ msg string }

func (e exitError) Error() string { return e.msg }

func loadConfig() (*config.Config, error) {
	path := configPath
	if path == "" {
		p, err := config.DefaultPath()
		if err != nil {
			return nil, err
		}
		path = p
	}
	return config.Load(path)
}
