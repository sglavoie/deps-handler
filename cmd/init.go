package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sglavoie/deps-merger/pkg/config"
	"github.com/sglavoie/deps-merger/pkg/github"
	"github.com/spf13/cobra"
)

// ExampleConfig is config.example.yaml, which init writes; main sets it
// from the copy embedded in the binary.
var ExampleConfig []byte

// discoverRepos lists the repositories init --discover writes; tests and
// the demo build replace it.
var discoverRepos = github.DependabotRepos

func newInitCmd() *cobra.Command {
	var (
		toStdout bool
		force    bool
		discover bool
		owners   []string
	)
	c := &cobra.Command{
		Use:   "init",
		Short: "Write an example config to fill in",
		Long: `Write the commented example config to the default config path, or to the
file -c names, for you to list your repositories in. Its repos are
commented out, so nothing runs until you do. An existing file is never
overwritten, unless --force moves it aside first.

With --discover, the repos are filled in instead with every repository
your gh login can write to that holds a Dependabot config, leaving out
archived repositories and forks: those you own, collaborate on or reach
through an organisation, or with --owner only those of the named owners.
Review the list before a run with --apply.

With --stdout, the config is printed rather than written.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if toStdout && force {
				return usageError{errors.New("--force needs a file to replace; drop it with --stdout")}
			}
			if cmd.Flags().Changed("owner") && !discover {
				return usageError{errors.New("--owner needs --discover")}
			}
			if err := checkSelection(cmd, nil, owners); err != nil {
				return err
			}
			if len(ExampleConfig) == 0 {
				return errors.New("this build carries no example config")
			}
			content := ExampleConfig
			if discover {
				repos, err := discoverRepos(cmd.Context(), owners)
				if err != nil {
					return fmt.Errorf("discover repos: %w", err)
				}
				if len(repos) == 0 {
					return errors.New("found no repository you can write to with a Dependabot config; run init without --discover and list yours by hand")
				}
				if content, err = withRepos(ExampleConfig, repos); err != nil {
					return err
				}
			}
			if toStdout {
				_, err := cmd.OutOrStdout().Write(content)
				return err
			}
			path := configPath
			if path == "" {
				p, err := config.DefaultPath()
				if err != nil {
					return err
				}
				path = p
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			backup := ""
			if force {
				var err error
				if backup, err = moveAside(path); err != nil {
					return err
				}
			}
			// Replacing it must write what this init would have.
			retry := "init --force"
			if discover {
				retry += " --discover"
				for _, o := range owners {
					retry += " --owner " + strings.TrimSpace(o)
				}
			}
			if err := writeNew(path, content, retry); err != nil {
				if backup != "" {
					if restoreErr := restoreBackup(path, backup); restoreErr != nil {
						return errors.Join(err, fmt.Errorf("could not restore config from %s: %w", backup, restoreErr))
					}
				}
				return err
			}
			out := cmd.OutOrStdout()
			if backup != "" {
				if _, err := fmt.Fprintf(out, "moved the existing config to %s\n", backup); err != nil {
					return err
				}
			}
			if _, err := fmt.Fprintf(out, "wrote %s\n", path); err != nil {
				return err
			}
			var err error
			if discover {
				_, err = fmt.Fprintf(out, "check the repositories it lists under repos: with `%s`, then try a dry run with `%s`\n", command("repos"), command("run"))
			} else {
				_, err = fmt.Fprintf(out, "list your repositories under repos:, then check them with `%s`\n", command("repos"))
			}
			return err
		},
	}
	c.Flags().BoolVar(&toStdout, "stdout", false, "print the config instead of writing it")
	c.Flags().BoolVar(&force, "force", false, "move an existing config aside, to a timestamped .bak file, and write a new one")
	c.Flags().BoolVar(&discover, "discover", false, "list the repositories you can write to that hold a Dependabot config")
	c.Flags().StringSliceVar(&owners, "owner", nil, "with --discover, only look at the repositories of these owners (repeatable)")
	keepEmpty(c, "owner")
	return c
}

// restoreBackup restores a config after a failed replacement without
// overwriting a file another process created meanwhile. The old file or
// symlink is restored only if path is absent; a failed restore keeps the backup.
func restoreBackup(path, backup string) error {
	info, err := os.Lstat(backup)
	if err != nil {
		return err
	}
	if err := linkNoReplace(backup, path, info); err != nil {
		return err
	}
	return os.Remove(backup)
}

// linkNoReplace creates to with the same file or symlink as from, only
// when to does not exist. Link follows symlinks on some systems, so those
// must be copied as symlinks explicitly.
func linkNoReplace(from, to string, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(from)
		if err != nil {
			return err
		}
		return os.Symlink(target, to)
	}
	return os.Link(from, to)
}

// writeNew writes data to path, which must not exist yet: O_EXCL, so a
// config created since any check cannot be lost either. retry is the init
// command to suggest that would replace it.
func writeNew(path string, data []byte, retry string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("%s already exists; edit it, or replace it with `%s`", path, command(retry))
	} else if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return err
	}
	return nil
}

// moveAside backs up the file at path, if there is one, under a name based
// on the time and returns that name: empty when there was nothing to move.
// Creating the backup only when its name is free, atomically, means
// concurrent init runs cannot overwrite an earlier backup.
func moveAside(path string) (string, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	} else if err != nil {
		return "", err
	} else if fi.IsDir() {
		// As -c naming the config's directory by mistake would.
		return "", fmt.Errorf("%s is a directory, not a config file", path)
	}
	backup := fmt.Sprintf("%s.%s.bak", path, time.Now().Format("20060102-150405"))
	err = linkNoReplace(path, backup, fi)
	if errors.Is(err, os.ErrExist) {
		return "", fmt.Errorf("%s already exists; try again in a second", backup)
	} else if err != nil {
		return "", err
	}
	if err := os.Remove(path); err != nil {
		// Keep the backup if removing the source failed: it may be the
		// only copy if another process changed the source meanwhile.
		return "", err
	}
	return backup, nil
}

// reposKey starts the repos list, which ends the example config.
var reposKey = []byte("\nrepos:\n")

// withRepos replaces the commented examples under repos: in example, which
// end it, with repos.
func withRepos(example []byte, repos []string) ([]byte, error) {
	head, _, ok := bytes.Cut(example, reposKey)
	if !ok {
		return nil, errors.New("the example config has no repos: to fill in")
	}
	var b bytes.Buffer
	b.Write(head)
	b.Write(reposKey)
	b.WriteString("  # Found by `deps-merger init --discover`: the repositories you can\n  # write to that hold a Dependabot config.\n")
	for _, r := range repos {
		fmt.Fprintf(&b, "  - %s\n", r)
	}
	return b.Bytes(), nil
}
