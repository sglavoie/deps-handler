package report

import (
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/sglavoie/deps-merger/pkg/policy"
	"github.com/sglavoie/deps-merger/pkg/runner"
)

// Markdown writes one repository's section as GitHub Markdown, such as the
// body of an issue tracking the repository's Dependabot PRs. Text from
// elsewhere (PR titles, reasons quoting GitHub or an agent) is escaped, so
// that it can neither change the layout nor mention anyone.
func Markdown(w io.Writer, apply bool, res runner.RepoResult) error {
	out := &errorWriter{w: w}
	markdown(out, apply, res)
	return out.err
}

func markdown(w io.Writer, apply bool, res runner.RepoResult) {
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
		fmt.Fprintf(w, "**Warning:** %s\n\n", mdLead(res.Repo, warning))
	}

	if res.Error == "" && len(res.Findings) > 0 {
		fmt.Fprint(w, "### Needs attention\n\n")
		for _, f := range res.Findings {
			line := mdLead(res.Repo, f.Summary)
			if listsPRs(f.Kind) {
				line += " (" + mdPRList(res.Repo, f.PRs) + ")"
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

// mdURL is a github.com URL with nothing in it that Markdown, a table or
// a terminal would read as more than a URL.
var mdURL = regexp.MustCompile(`^https://github\.com/[A-Za-z0-9._~/%#?=+-]+$`)

// mdPR links a PR's number and title to it. Only a github.com URL is
// linked: the link is the one thing in a cell that is not escaped.
func mdPR(n int, title, url string) string {
	if !mdURL.MatchString(url) {
		return mdText(fmt.Sprintf("#%d %s", n, truncate(title)))
	}
	// GitHub links no #N in a link's text: its number is left as is.
	return fmt.Sprintf("[#%d %s](%s)", n, mdText(truncate(title)), url)
}

// mdPRList names PRs of repo as GitHub links them from anywhere, unlike
// a bare #N, which it links to the issue's own repository.
func mdPRList(repo string, prs []int) string {
	parts := make([]string, len(prs))
	for i, n := range prs {
		parts[i] = fmt.Sprintf("%s#%d", repo, n)
	}
	return strings.Join(parts, ", ")
}

// mdLeadRef is the PR a finding or a warning starts by naming.
var mdLeadRef = regexp.MustCompile(`^#[0-9]+`)

// mdLead is mdText for a line deps-merger starts with the PR it is about,
// named so that GitHub links it to that PR of repo.
func mdLead(repo, text string) string {
	ref := mdLeadRef.FindString(text)
	if ref == "" {
		return mdText(text)
	}
	rest := text[len(ref):]
	trimmed := strings.TrimLeft(rest, " ")
	return repo + ref + rest[:len(rest)-len(trimmed)] + mdText(trimmed)
}

// mdSpecial is what GitHub Markdown would read as markup in a line of
// text, or in a table cell.
var mdSpecial = strings.NewReplacer(
	`\`, `\\`, "`", "\\`", "*", `\*`, "_", `\_`, "[", `\[`, "]", `\]`,
	"<", "&lt;", ">", "&gt;", "&", "&amp;", "|", `\|`, "~", `\~`,
	// GitHub reads text between dollar signs as math.
	"$", `\$`,
	// A zero-width space after @ stops GitHub reading a mention, which
	// would notify whoever is named.
	"@", "@\u200b",
)

// mdAutolink is what GitHub links in text with no markup around it: a
// URL, a domain starting with www., or #N or GH-N, which it takes for an
// issue of the repository the Markdown is posted in, whichever it was
// written for.
var mdAutolink = regexp.MustCompile(`(?i)://|www\.|#[0-9]|GH-[0-9]`)

// mdBlock is what, starting a list item, would open a block of its own
// in it: a heading, a list or a rule.
var mdBlock = regexp.MustCompile(`^(?:#{1,6}|[-+]|\d{1,9}[.)])(?:[\s-]|$)`)

// mdText makes text safe to show as one line of Markdown, or in a table
// cell. Text that starts a list item, as a finding's details do, cannot
// open a heading, a list, a rule or a code block in it.
func mdText(text string) string {
	s := mdSpecial.Replace(strings.TrimLeft(Plain(text), " "))
	// A zero-width space in it, as after @, stops GitHub linking text
	// from elsewhere, such as an agent's concerns a prompt injection
	// shaped, to a site of its choosing.
	s = mdAutolink.ReplaceAllStringFunc(s, func(m string) string { return m[:len(m)-1] + "\u200b" + m[len(m)-1:] })
	if m := mdBlock.FindString(s); m != "" {
		// A list number is escaped at its . or ), which follows its digits.
		i := strings.IndexAny(m, ".)")
		if i < 0 {
			i = 0
		}
		s = s[:i] + `\` + s[i:]
	}
	return s
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
