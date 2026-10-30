// Package cmd holds the deps-handler command line.
package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"

	"github.com/sglavoie/deps-handler/pkg/config"
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
		Use:   "deps-handler",
		Short: "Manage Dependabot updates and diagnose or repair pull requests",
		Long: `deps-handler manages Dependabot updates across repositories and helps
diagnose or repair any explicitly selected pull request.

Use dependabot to evaluate open Dependabot PRs in configured repositories
and, with --apply, merge those that are green and within policy. PRs not
opened by Dependabot, or touched by anyone else, are never changed by
this command.

Use pr diagnose for a repair handoff or pr fix to prepare local fixes to
a selected PR; add --push to publish them and verify CI. Use pr rerun to
preview failed Actions jobs; --apply reruns them and watches the new attempt.`,
		Version:       versionString(),
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	// An empty -c, as an unset variable gives, must not fall back on the
	// default config and act on its repos.
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		if f := cmd.Flag("config"); f != nil && f.Changed && strings.TrimSpace(configPath) == "" {
			return usageError{errors.New("--config needs a file path")}
		}
		return nil
	}
	root.PersistentFlags().StringVarP(&configPath, "config", "c", "", "config file (default $XDG_CONFIG_HOME/deps-handler/config.yaml, or ~/.config/deps-handler/config.yaml when XDG_CONFIG_HOME is unset)")
	root.AddCommand(newInitCmd(), newDependabotCmd(), newReposCmd(), newVerdictsCmd(), newPRCmd())
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError{err} })
	// Cobra's help prints the usage for an unknown topic and succeeds:
	// a topic that names no command is a usage error like any other.
	root.InitDefaultHelpCmd()
	if help, _, err := root.Find([]string{"help"}); err == nil {
		help.Args = func(cmd *cobra.Command, args []string) error {
			if _, _, err := cmd.Root().Find(args); err != nil {
				return fmt.Errorf("unknown help topic %q", strings.Join(args, " "))
			}
			return nil
		}
	}
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
	usageCompletion(root)
	return exitCode(root.Execute(), stderr)
}

// usageCompletion makes completion for a shell it has no script for a
// usage error, as help is for an unknown topic. Cobra only checks the
// arguments of a command that runs, so it runs to print its help. The
// command is made once the output is set: its scripts write where it was
// when they were made.
func usageCompletion(root *cobra.Command) {
	root.InitDefaultCompletionCmd()
	if completion, _, err := root.Find([]string{"completion"}); err == nil && completion != root {
		completion.Args = cobra.NoArgs
		completion.RunE = func(cmd *cobra.Command, _ []string) error { return cmd.Help() }
	}
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
		fmt.Fprintln(stderr, "deps-handler: interrupted")
		return 130
	case errors.As(err, new(usageError)) || strings.HasPrefix(err.Error(), "unknown command "):
		// Cobra reports an unknown subcommand before any hook can see it.
		fmt.Fprintln(stderr, "deps-handler:", err)
		return 2
	}
	fmt.Fprintln(stderr, "deps-handler:", err)
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
	if pseudoVersionOf(version, commit) {
		// The version names the commit already, as v0.2.1-0.20261024141842-3b9316762033 does.
		if dirty && !strings.Contains(version, "+dirty") {
			version += "+dirty"
		}
		return fmt.Sprintf("%s (built %s)", version, built)
	}
	if dirty {
		commit += "-dirty"
	}
	return fmt.Sprintf("%s (%s, built %s)", version, commit, built)
}

// pseudoVersionOf reports whether version is a Go pseudo-version, which
// ends in the first 12 hex digits of the commit it was built from.
func pseudoVersionOf(version, commit string) bool {
	version, _, _ = strings.Cut(version, "+")
	i := strings.LastIndex(version, "-")
	if i < 0 || len(commit) < 7 || commit == "unknown" {
		return false
	}
	rev := version[i+1:]
	return len(rev) == 12 && strings.HasPrefix(rev, commit)
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
	cfg, err := config.Load(path)
	initCmd := command("init")
	if nf, ok := errors.AsType[*config.NotFoundError](err); ok {
		return nil, fmt.Errorf("config not found at %s; create one with `%s`", nf.Path, initCmd)
	}
	if errors.Is(err, config.ErrNoRepos) {
		// As init leaves it: say where to fill it in.
		return nil, fmt.Errorf("%s lists no repos yet; list yours under repos:, or fill them in with `%s --discover --force`", path, initCmd)
	}
	return cfg, err
}

// command is the deps-handler command line to suggest for sub, naming the
// config given with -c, as following the advice must not act on another.
func command(sub string) string {
	if configPath == "" {
		return "deps-handler " + sub
	}
	return "deps-handler " + sub + " -c " + shellQuote(configPath)
}

// shellQuote quotes s for a POSIX shell when it holds anything but the
// characters of a plain path.
func shellQuote(s string) string {
	plain := strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/._-+=:,@%", r))
	}) < 0
	if plain && s != "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
