package report

import (
	"io"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/cli/go-gh/v2/pkg/term"
	"github.com/sglavoie/deps-merger/internal/config"
)

// SGR codes used by the report.
const (
	bold    = "1"
	dim     = "2"
	red     = "31"
	green   = "32"
	yellow  = "33"
	magenta = "35"
	cyan    = "36"
)

// style decides which escape codes a writer gets: OSC 8 links on a
// terminal, and colors on a terminal unless NO_COLOR or CLICOLOR=0 turns
// them off. CLICOLOR_FORCE turns them on anywhere, except that NO_COLOR
// always wins, as no-color.org asks.
type style struct {
	links, color bool
}

func styleFor(w io.Writer) style {
	f, ok := w.(*os.File)
	tty := ok && term.IsTerminal(f)
	noColor := os.Getenv("NO_COLOR") != ""
	return style{links: tty, color: !noColor && (term.IsColorForced() || (tty && !term.IsColorDisabled()))}
}

// paint colors text, which is printed as Plain makes it.
func (s style) paint(code, text string) string {
	text = Plain(text)
	if !s.color || code == "" || text == "" {
		return text
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

func (s style) link(url, text string) string {
	if !s.links || url == "" {
		return text
	}
	return "\x1b]8;;" + Plain(url) + "\x1b\\" + text + "\x1b]8;;\x1b\\"
}

// Plain makes text safe to print on a terminal as one line: control
// characters, such as the escape sequences an agent's answer or a reason
// quoting it could carry, are dropped, as are invisible format characters,
// such as the bidirectional overrides that would reorder the text around
// them, and line breaks and tabs become spaces.
func Plain(text string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return ' '
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
			return -1
		}
		return r
	}, text)
}

// labelColor colors an action or event by what it means for the PR: done
// or about to be (green), asked of Dependabot (cyan), not yet (yellow),
// for the agent (magenta), left alone (dim) or failed (red).
func labelColor(l string) string {
	switch l {
	case "merged", "merge", "would merge":
		return green
	case "rebase-requested", "rebase", "would rebase":
		return cyan
	case "wait":
		return yellow
	case "review":
		return magenta
	case "skip":
		return dim
	case "merge-failed", "rebase-failed":
		return bold + ";" + red
	}
	return ""
}

func gradeColor(g config.UpdateType) string {
	switch g {
	case config.UpdatePatch:
		return green
	case config.UpdateMinor:
		return yellow
	case config.UpdateMajor:
		return red
	}
	return dim
}

// cell is a table cell: its text, the color to paint it and, optionally,
// the URL it links to.
type cell struct{ text, color, url string }

// table writes rows with columns aligned on their plain text, so escape
// codes do not count toward the widths as they would with tabwriter. The
// last column is not padded.
func table(w io.Writer, s style, indent string, rows [][]cell) {
	var widths []int
	for _, r := range rows {
		for i, c := range r {
			if i == len(widths) {
				widths = append(widths, 0)
			}
			widths[i] = max(widths[i], utf8.RuneCountInString(Plain(c.text)))
		}
	}
	for _, r := range rows {
		var b strings.Builder
		b.WriteString(indent)
		for i, c := range r {
			b.WriteString(s.link(c.url, s.paint(c.color, c.text)))
			if i < len(r)-1 {
				b.WriteString(strings.Repeat(" ", widths[i]-utf8.RuneCountInString(Plain(c.text))+2))
			}
		}
		b.WriteString("\n")
		io.WriteString(w, b.String())
	}
}
