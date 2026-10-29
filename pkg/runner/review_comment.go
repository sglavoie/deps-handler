package runner

import (
	"crypto/sha256"
	"fmt"
	"html"
	"regexp"
	"strings"

	"github.com/sglavoie/deps-merger/pkg/config"
	"github.com/sglavoie/deps-merger/pkg/policy"
	"github.com/sglavoie/deps-merger/pkg/review"
)

// ReviewCommentPrefix identifies explanations the runner posts on reviewed
// PRs, allowing the App to distinguish them from Dependabot commands.
const ReviewCommentPrefix = "<!-- deps-merger-review:"

// reviewComment explains the verdict without claiming that a merge has
// already happened. Its identity follows the verdict's cache lifetime:
// approvals survive rebases, while rejections and failures pin the head.
func (r *Runner) reviewComment(rc config.Repo, d policy.Decision) string {
	v := d.Verdict
	key := review.NewRequest(rc, d).Key()
	if !v.Approved || v.Error != "" {
		key += "\n" + d.PR.HeadOID
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s%x -->\n\n## deps-merger agent review\n\n", ReviewCommentPrefix, sha256.Sum256([]byte(key)))
	switch {
	case v.Error != "":
		b.WriteString("**Review failed — leaving this PR unmerged.**\n\n")
	case v.Approved:
		b.WriteString("**Approved for merging once all policy checks pass.**\n\n")
	default:
		b.WriteString("**Rejected — leaving this PR unmerged for human review.**\n\n")
	}
	name := v.Agent
	if name == "" {
		name = r.AgentName
	}
	if name != "" {
		b.WriteString("Reviewer:\n" + quoteReview(name) + "\n\n")
	}
	if v.Error != "" {
		b.WriteString(quoteReview(v.Error) + "\n")
	} else {
		b.WriteString(quoteReview(v.Summary) + "\n")
		if len(v.Concerns) > 0 {
			b.WriteString("\n### Concerns and breaking changes\n\n")
			for _, concern := range v.Concerns {
				b.WriteString(quoteReview(concern) + "\n\n")
			}
		}
	}
	// GitHub bounds comment size. Keep even an unusually verbose verdict
	// publishable, without cutting a UTF-8 sequence in half.
	body := b.String()
	if len(body) > 60_000 {
		body = strings.ToValidUTF8(body[:60_000], "") + "\n\n[Review text truncated.]\n"
	}
	return body
}

var reviewMarkup = strings.NewReplacer(
	`\`, `\\`, "`", "\\`", "*", `\*`, "_", `\_`, "[", `\[`, "]", `\]`,
	"!", `\!`, "~", `\~`, "$", `\$`, "|", `\|`, "#", `\#`,
)

var reviewAutolink = regexp.MustCompile(`(?i)://|www\.|#[0-9]|GH-[0-9]`)

// Quote untrusted agent text and redact tokens, including older cached
// verdicts. Escape Markdown links and images and break bare autolinks;
// blockquotes alone would still publish external resources. Mentions must
// neither notify users nor command bots.
func quoteReview(s string) string {
	s = html.EscapeString(review.Clean(s))
	s = reviewMarkup.Replace(s)
	s = reviewAutolink.ReplaceAllStringFunc(s, func(m string) string {
		return m[:len(m)-1] + "\u200b" + m[len(m)-1:]
	})
	s = strings.ReplaceAll(s, "@", "&#64;")
	return "> " + strings.ReplaceAll(s, "\n", "\n> ")
}
