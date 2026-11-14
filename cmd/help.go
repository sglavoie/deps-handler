package cmd

import (
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func init() {
	cobra.AddTemplateFunc("plainFlagUsages", plainFlagUsages)
}

func configureHelp(root *cobra.Command) {
	template := root.UsageTemplate()
	for _, flags := range []string{"LocalFlags", "InheritedFlags"} {
		template = strings.ReplaceAll(template, "."+flags+".FlagUsages", "plainFlagUsages ."+flags)
	}
	root.SetUsageTemplate(template)
}

// helpValue hides pflag's optional-bool interface only in the display copy,
// so help omits [=true|false] while parsing still accepts both forms.
type helpValue struct{ pflag.Value }

func plainFlagUsages(flags *pflag.FlagSet) string {
	display := pflag.NewFlagSet("help", pflag.ContinueOnError)
	display.SortFlags = flags.SortFlags
	flags.VisitAll(func(flag *pflag.Flag) {
		copy := *flag
		if copy.Value.Type() == "bool" {
			copy.Value = helpValue{copy.Value}
		}
		display.AddFlag(&copy)
	})
	return display.FlagUsages()
}
