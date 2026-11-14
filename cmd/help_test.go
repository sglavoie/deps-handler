package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestCommandHelpExamples(t *testing.T) {
	root := newRoot()
	usageCompletion(root)
	var visit func(*cobra.Command)
	visit = func(cmd *cobra.Command) {
		if cmd.Hidden {
			return
		}
		t.Run(cmd.CommandPath(), func(t *testing.T) {
			args := strings.Fields(cmd.CommandPath())[1:]
			code, out, stderr := run(append(args, "--help")...)
			if code != 0 || stderr != "" || !strings.Contains(out, "Examples:") {
				t.Fatalf("help = %d, %q, %q", code, out, stderr)
			}
			if n := len(strings.Split(cmd.Example, "\n")); n < 2 || n > 4 {
				t.Errorf("got %d examples, want 2–4", n)
			}
			if strings.Contains(out, "[=true|false]") {
				t.Errorf("boolean syntax still appears in help: %s", out)
			}
			if !strings.Contains(out, "config file (default ~/.config/deps-handler/config.yaml)") {
				t.Error("missing concise config help")
			}
		})
		for _, sub := range cmd.Commands() {
			visit(sub)
		}
	}
	visit(root)
}

func TestHelpPreservesBooleanParsing(t *testing.T) {
	for _, value := range []string{"", "=true", "=false"} {
		t.Run(value, func(t *testing.T) {
			root := newRoot()
			var out bytes.Buffer
			root.SetOut(&out)
			cmd, _, err := root.Find([]string{"dependabot"})
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Help(); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "--apply ") || strings.Contains(out.String(), "[=true|false]") {
				t.Fatalf("unexpected apply help: %s", out.String())
			}
			if err := cmd.ParseFlags([]string{"--apply" + value}); err != nil {
				t.Fatal(err)
			}
			got, err := cmd.Flags().GetBool("apply")
			if err != nil || got != (value != "=false") || cmd.Flag("apply").NoOptDefVal != "true" {
				t.Fatalf("--apply%s = %v, %v; flag = %+v", value, got, err, cmd.Flag("apply"))
			}
		})
	}
}
