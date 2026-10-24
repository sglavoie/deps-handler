package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sglavoie/deps-merger/internal/config"
	"github.com/spf13/cobra"
)

// ExampleConfig is config.example.yaml, which init writes; main sets it
// from the copy embedded in the binary.
var ExampleConfig []byte

func newInitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Write an example config to fill in",
		Long: `Write the commented example config to the default config path, or to the
file -c names, for you to list your repositories in. An existing file is
never overwritten.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path := configPath
			if path == "" {
				p, err := config.DefaultPath()
				if err != nil {
					return err
				}
				path = p
			}
			if len(ExampleConfig) == 0 {
				return errors.New("this build carries no example config")
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			// O_EXCL: a config created since the check cannot be lost either.
			f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if errors.Is(err, os.ErrExist) {
				return fmt.Errorf("%s already exists; edit it, or remove it to start over", path)
			} else if err != nil {
				return err
			}
			if _, err := f.Write(ExampleConfig); err != nil {
				f.Close()
				os.Remove(path)
				return err
			}
			if err := f.Close(); err != nil {
				os.Remove(path)
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\nlist your repositories under repos:, then check them with `deps-merger repos`\n", path)
			return nil
		},
	}
}
