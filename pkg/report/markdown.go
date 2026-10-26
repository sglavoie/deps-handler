package report

import (
	"fmt"
	"io"
	"strings"

	"github.com/sglavoie/deps-merger/pkg/policy"
	"github.com/sglavoie/deps-merger/pkg/runner"
)

// Markdown writes one repository's section as GitHub Markdown, such as the
// body of an issue tracking the repository's Dependabot PRs. Text from
// elsewhere (PR titles, reasons quoting GitHub or an agent) is escaped, so
// that it can neither change the layout nor mention anyone.
func Markdown(w io.Writer, apply bool, res runner.RepoResult) {
	apply = applies(apply, res)
	fmt.Fprintf(w, "## %s\n\n", mdText(res.Repo))
	fmt.Fprintf(w, "%s\n", mdText(upperFirst(header(res))))
	if len(res.ConfigFiles) > 0 {
		files := make([]string, len(res.ConfigFiles))
		for i, f := range res.ConfigFiles {
			files[i] = mdCode(f)
		}
		fmt.Fprintf(w, "Policy from %s.\n", strings.Join(files, ", "))
	}
	fmt.Fprintln(w)

	// As in the text report: a repository that failed part way still
	// shows the writes made before then.
	var decisions []policy.Decision
	if res.Error != "" {
		fmt.Fprintf(w, "**Error:** %s\n\n", mdText(res.Error))
	} else {
		decisions = pending(res)
		if len(res.Decisions) == 0 && len(res.Events) == 0 {
			fmt.Fprint(w, "No open Dependabot PRs.\n\n")
		}
	}

	if len(res.Events) > 0 || len(decisions) > 0 {
		fmt.Fprint(w, "| PR | Update | Status | Reason |\n| --- | --- | --- | --- |\n")
		for _, ev := range res.Events {
			fmt.Fprintf(w, "| %s | %s | %s | %s |\n", mdPR(ev.Number, ev.Title, ev.URL), ev.Grade, mdText(string(ev.Kind)), mdText(ev.Detail))
		}
		for _, d := range decisions {
			fmt.Fprintf(w, "| %s | %s | %s | %s |\n", mdPR(d.Number, d.Title, d.URL), d.Grade, mdText(label(d.Action, apply)), mdText(d.Reason))
		}
		fmt.Fprintln(w)
	}
	for _, warning := range res.Warnings {
		fmt.Fprintf(w, "**Warning:** %s\n\n", mdText(warning))
	}

	if res.Error == "" && len(res.Findings) > 0 {
		fmt.Fprint(w, "### Needs attention\n\n")
		for _, f := range res.Findings {
			line := mdText(f.Summary)
			if listsPRs(f.Kind) {
				line += " (" + prList(f.PRs) + ")"
			}
			fmt.Fprintf(w, "- %s\n", line)
			for _, d := range f.Details {
				fmt.Fprintf(w, "  - %s\n", mdText(d))
			}
			for _, sg := range f.Suggestions {
				fmt.Fprintf(w, "  - Suggested: %s\n", mdSuggestion(sg))
			}
		}
		fmt.Fprintln(w)
	}
}

// mdPR links a PR's number and title to it. Only a github.com URL is
// linked: the link is the one thing in a cell that is not escaped.
func mdPR(n int, title, url string) string {
	text := mdText(fmt.Sprintf("#%d %s", n, truncate(title)))
	if !strings.HasPrefix(url, "https://github.com/") || strings.ContainsAny(url, " ()<>\"'`") {
		return text
	}
	return "[" + text + "](" + url + ")"
}

// mdSpecial is what GitHub Markdown would read as markup in a line of
// text, or in a table cell.
var mdSpecial = strings.NewReplacer(
	`\`, `\\`, "`", "\\`", "*", `\*`, "_", `\_`, "[", `\[`, "]", `\]`,
	"<", "&lt;", ">", "&gt;", "&", "&amp;", "|", `\|`, "~", `\~`,
	// A zero-width space after @ stops GitHub reading a mention, which
	// would notify whoever is named.
	"@", "@\u200b",
)

// mdText makes text safe to show as one line of Markdown, or in a table
// cell. A # starting the text cannot make it a heading: it never starts a
// line on its own.
func mdText(text string) string {
	return mdSpecial.Replace(Plain(text))
}

// mdSuggestion shows a Dependabot command as code, to copy and comment
// as is (GitHub reads no mention in code), and anything else as text.
func mdSuggestion(s string) string {
	if strings.HasPrefix(s, "@dependabot ") {
		return mdCode(s)
	}
	return mdText(s)
}

// mdCode writes text as inline code, fenced by more backticks than any
// run of them it holds.
func mdCode(text string) string {
	text = Plain(text)
	run, longest := 0, 0
	for _, r := range text {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", longest+1)
	if strings.HasPrefix(text, "`") || strings.HasSuffix(text, "`") {
		text = " " + text + " "
	}
	return fence + text + fence
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
