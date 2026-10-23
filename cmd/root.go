// Package cmd holds the deps-merger command line.
package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"

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
	root.PersistentFlags().StringVarP(&configPath, "config", "c", "", "config file (default $XDG_CONFIG_HOME/deps-merger/config.yaml, or ~/.config/deps-merger/config.yaml when XDG_CONFIG_HOME is unset)")
	root.AddCommand(newRunCmd(), newReposCmd(), newVerdictsCmd())
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError{err} })
	usageArgs(root)
	return root
}

// usageArgs makes the argument errors of c and its subcommands usage
// errors.
func usageArgs(c *cobra.Command) {
	if args := c.Args; args != nil {
		c.Args = func(cmd *cobra.Command, a []string) error {
			if err := args(cmd, a); err != nil {
				return usageError{err}
			}
			return nil
		}
	}
	for _, sub := range c.Commands() {
		usageArgs(sub)
	}
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
	case errors.As(err, new(usageError)) || strings.HasPrefix(err.Error(), "unknown command "):
		// Cobra reports an unknown subcommand before any hook can see it.
		fmt.Fprintln(stderr, "deps-merger:", err)
		return 2
	}
	fmt.Fprintln(stderr, "deps-merger:", err)
	return 1
}

// usageError is a command used wrongly, such as an unknown flag or a
// missing argument: it exits 2, so a script can tell it from a run that
// failed.
type usageError struct{ err error }

func (e usageError) Error() string { return e.err.Error() }
func (e usageError) Unwrap() error { return e.err }

// exitError signals a non-zero exit whose details were already printed.
type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

// versionString prefers the -ldflags values and falls back to what the Go
// toolchain recorded, so `go install` builds still identify themselves.
func versionString() string {
	info, _ := debug.ReadBuildInfo()
	return formatVersion(info, Version, GitCommit, BuildTime)
}

// formatVersion fills what -ldflags left unset from info, which may be nil,
// and marks the commit "-dirty" when the build had uncommitted changes.
func formatVersion(info *debug.BuildInfo, version, commit, built string) string {
	dirty := false
	if info != nil {
		if version == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
			version = info.Main.Version
		}
		for _, s := range info.Settings {
			switch {
			case s.Key == "vcs.revision" && commit == "unknown":
				commit = s.Value[:min(7, len(s.Value))]
			case s.Key == "vcs.time" && built == "unknown":
				built = s.Value
			case s.Key == "vcs.modified":
				dirty = s.Value == "true"
			}
		}
	}
	if dirty {
		commit += "-dirty"
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
