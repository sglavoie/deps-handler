// Package cmd holds the deps-merger command line.
package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"

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
		Version:       versionString(),
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVarP(&configPath, "config", "c", "", "config file (default $XDG_CONFIG_HOME/deps-merger/config.yaml)")
	root.AddCommand(newRunCmd(), newReposCmd())
	return root
}

// Execute runs the CLI and returns the process exit code.
func Execute() int {
	return execute(os.Args[1:], os.Stdout, os.Stderr)
}

func execute(args []string, stdout, stderr io.Writer) int {
	root := newRoot()
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	return exitCode(root.Execute(), stderr)
}

// exitCode maps the error a command returned to the documented exit status,
// printing it unless it was already reported.
func exitCode(err error, stderr io.Writer) int {
	var ee exitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee):
		return ee.code
	case errors.Is(err, errInterrupted):
		fmt.Fprintln(stderr, "deps-merger: interrupted")
		return 130
	}
	fmt.Fprintln(stderr, "deps-merger:", err)
	return 1
}

// exitError signals a non-zero exit whose details were already printed.
type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

// versionString prefers the -ldflags values and falls back to what the Go
// toolchain recorded, so `go install` builds still identify themselves.
func versionString() string {
	version, commit, built := Version, GitCommit, BuildTime
	if info, ok := debug.ReadBuildInfo(); ok {
		if version == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
			version = info.Main.Version
		}
		for _, s := range info.Settings {
			switch {
			case s.Key == "vcs.revision" && commit == "unknown":
				commit = s.Value[:min(7, len(s.Value))]
			case s.Key == "vcs.time" && built == "unknown":
				built = s.Value
			}
		}
	}
	return fmt.Sprintf("%s (%s, built %s)", version, commit, built)
}

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
