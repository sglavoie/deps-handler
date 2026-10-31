package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// outputFormat is shared by report commands. eval keeps --list as a case
// selector, so its legacy layout is selected with --format list instead.
type outputFormat struct {
	name              string
	list, table, json bool
}

func addOutputFlags(c *cobra.Command, listSelector bool) *outputFormat {
	f := &outputFormat{}
	c.Flags().StringVar(&f.name, "format", "table", "output format: table, list, json")
	c.Flags().BoolVar(&f.table, "table", false, "print colored tables (default)")
	c.Flags().BoolVar(&f.json, "json", false, "print results as JSON")
	if !listSelector {
		c.Flags().BoolVar(&f.list, "list", false, "print the original list output")
	}
	_ = c.RegisterFlagCompletionFunc("format", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return []string{"table", "list", "json"}, cobra.ShellCompDirectiveNoFileComp
	})
	return f
}

func (f *outputFormat) resolve(c *cobra.Command) (string, error) {
	name := f.name
	switch name {
	case "table", "list", "json":
	default:
		return "", usageError{fmt.Errorf("unknown output format %q (want table, list, json)", name)}
	}
	selected := ""
	for _, flag := range []struct {
		name    string
		enabled bool
	}{{"list", f.list}, {"table", f.table}, {"json", f.json}} {
		if !flag.enabled {
			continue
		}
		if selected != "" || c.Flags().Changed("format") && name != flag.name {
			return "", usageError{fmt.Errorf("output modes conflict; choose one of --list, --table, --json or --format")}
		}
		selected = flag.name
	}
	if selected != "" {
		name = selected
	}
	if markdown := c.Flags().Lookup("markdown"); markdown != nil && markdown.Value.String() == "true" {
		if selected != "" || c.Flags().Changed("format") {
			return "", usageError{fmt.Errorf("--markdown cannot be used with --list, --table, --json or --format")}
		}
		return "markdown", nil
	}
	return name, nil
}
