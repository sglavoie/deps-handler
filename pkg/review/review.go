// Package review asks an AI agent whether a Dependabot update the policy
// alone would not merge (one over max_update_type, or of unknown size) is
// safe for the repository. The agent only reads: it returns a verdict, and
// deps-merger merges an approved PR through the same checks as any other.
package review

import (
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"text/template"
	"time"
	"unicode"

	"github.com/sglavoie/deps-merger/pkg/config"
	"github.com/sglavoie/deps-merger/pkg/dependabot"
	"github.com/sglavoie/deps-merger/pkg/github"
	"github.com/sglavoie/deps-merger/pkg/policy"
)

// Request is one PR to review.
type Request struct {
	Owner, Name  string
	Number       int
	Title        string
	URL          string
	HeadOID      string
	BaseRef      string
	Grade        config.UpdateType
	Limit        config.UpdateType
	Reason       string
	Dependencies []dependabot.Dependency
	Checks       []github.Check
	// NoChecks explains to the agent why the PR may merge without checks,
	// should none have run.
	NoChecks string
	// Unread is set when a commit records no dependencies: what it changed
	// is known only from the head commit reviewed.
	Unread bool
}

// NewRequest describes the PR behind d in repository rc. Its checks are
// the ones the policy judged: runs a newer suite superseded are left out.
func NewRequest(rc config.Repo, d policy.Decision) Request {
	noChecks := "the repository does not require checks"
	if rc.Policy.RequireChecks {
		noChecks = fmt.Sprintf("no check started within %s of the latest commit, after which the repository's policy merges without them because its CI does not run for every change", config.ShortDuration(rc.Policy.MergeWithoutChecksAfter))
	}
	unread := false
	for _, c := range d.PR.Commits {
		unread = unread || len(dependabot.ParseCommitMessage(c.Message)) == 0
	}
	return Request{
		Owner: rc.Owner, Name: rc.Name, Number: d.Number, Title: d.Title, URL: d.URL,
		HeadOID: d.PR.HeadOID, BaseRef: d.PR.BaseRef, Grade: d.Grade, Limit: rc.Policy.MaxUpdateType,
		Reason: strings.TrimSuffix(d.Reason, policy.AwaitingReview), Dependencies: d.Dependencies, Checks: policy.LatestChecks(d.PR.Checks),
		NoChecks: noChecks, Unread: unread,
	}
}

// Key identifies the update under review: the PR and the versions it moves
// to. A rebase changes the head commit but not the update, so an approval
// still holds (the cache drops a rejection then); when Dependabot does not
// record versions, or a commit records no dependencies at all, the head
// commit stands in for them, so not even an approval survives a rebase
// (see Entry.PinsHead). The repository is lower-cased, as GitHub ignores
// its case, and the agent is left out: a verdict from one agent is reused
// by another.
func (r Request) Key() string {
	deps := make([]string, 0, len(r.Dependencies))
	versioned := len(r.Dependencies) > 0 && !r.Unread
	for _, d := range r.Dependencies {
		deps = append(deps, d.Name+"@"+d.Version)
		versioned = versioned && d.Version != ""
	}
	slices.Sort(deps)
	parts := []string{fmt.Sprintf("%s/%s#%d", strings.ToLower(r.Owner), strings.ToLower(r.Name), r.Number)}
	if len(deps) > 0 {
		parts = append(parts, strings.Join(deps, ","))
	}
	if !versioned {
		parts = append(parts, r.HeadOID)
	}
	return strings.Join(parts, " ")
}

// Reviewer judges one update.
type Reviewer interface {
	Review(ctx context.Context, req Request) (policy.Verdict, error)
}

// Details fetches what a review reads about a PR beyond FetchRepo.
type Details interface {
	PullRequestDetails(ctx context.Context, owner, name string, number int) (*github.PullRequestDetails, error)
}

// AgentReviewer checks the PR out and has an agent read it.
type AgentReviewer struct {
	Agent     Agent
	Details   Details
	Workspace *Workspace
	Timeout   time.Duration
}

func (r *AgentReviewer) Review(ctx context.Context, req Request) (policy.Verdict, error) {
	if r.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}
	v, stage, err := r.review(ctx, req)
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		// The review's time runs out in whichever stage was running.
		if stage == "" {
			return v, fmt.Errorf("%s timed out after %s", r.Agent.Name(), r.Timeout)
		}
		return v, fmt.Errorf("review timed out after %s while %s, before %s ran", r.Timeout, stage, r.Agent.Name())
	}
	if err != nil {
		// What an agent writes to stderr reaches the report and CI logs.
		err = cleanError{err}
	}
	return v, err
}

// review does the review and, should it fail before the agent ran, says
// in which stage.
func (r *AgentReviewer) review(ctx context.Context, req Request) (v policy.Verdict, stage string, err error) {
	details, err := github.Again(ctx, 3*time.Second, nil, func() (*github.PullRequestDetails, error) {
		return r.Details.PullRequestDetails(ctx, req.Owner, req.Name, req.Number)
	})
	if err != nil {
		return v, "fetching the PR details", fmt.Errorf("fetch PR details: %w", err)
	}
	dir, err := r.Workspace.Checkout(ctx, req.Owner, req.Name, req.Number, req.HeadOID)
	if err != nil {
		return v, "checking the PR out", fmt.Errorf("checkout: %w", err)
	}
	prompt, err := Prompt(req, details)
	if err != nil {
		return v, "", err
	}
	a, err := r.Agent.Run(ctx, dir, prompt)
	if err != nil {
		return v, "", err
	}
	v, err = Verdict(a)
	return v, "", err
}

// cleanError is an error whose text passes through Clean, still wrapping
// it for errors.Is.
type cleanError struct{ err error }

func (e cleanError) Error() string { return Clean(e.err.Error()) }
func (e cleanError) Unwrap() error { return e.err }

// Verdict turns an agent's answer into a verdict. An answer that is
// neither "approve" nor "reject" is an error, not a verdict.
func Verdict(a Answer) (policy.Verdict, error) {
	switch a.Decision {
	case "approve", "reject":
	default:
		// Cleaned before quoting: %q would spell an invisible character
		// splitting a token as visible text that Clean no longer drops.
		return policy.Verdict{}, fmt.Errorf("agent answered %q instead of approve or reject", Clean(a.Decision))
	}
	concerns := make([]string, 0, len(a.BreakingChanges)+len(a.Concerns))
	for _, c := range append(append([]string{}, a.BreakingChanges...), a.Concerns...) {
		concerns = append(concerns, Clean(c))
	}
	return policy.Verdict{Approved: a.Decision == "approve", Summary: Clean(strings.TrimSpace(a.Summary)), Concerns: concerns}, nil
}

// githubToken matches the GitHub token formats: classic and fine-grained
// personal, OAuth, app user, app installation and refresh tokens. A
// token is matched wherever it starts, even glued to a word, as an agent
// could quote it ("xghp_…") to slip past a match at a word boundary.
var githubToken = regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{30,}`)

// redact hides GitHub tokens in an agent's reasons, which are cached,
// printed and sent to CI logs: an agent can read files holding one, such
// as gh's hosts.yml, and a prompt injection could have it quote them.
func redact(s string) string { return githubToken.ReplaceAllString(s, "[redacted token]") }

// Clean makes an agent's text safe to cache and print: tokens redacted,
// and control characters other than newlines and tabs dropped. The agent
// reads untrusted release notes, and a prompt injection could have it
// answer with escape sequences that rewrite the terminal, such as links
// or clipboard writes. Invisible format characters are dropped before
// redacting too: one splitting a token would hide it from the match, and
// the report, which drops them, would print it whole.
func Clean(s string) string {
	s = strings.Map(func(r rune) rune {
		if (unicode.IsControl(r) && r != '\n' && r != '\t') || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
	return redact(s)
}

//go:embed prompt.tmpl
var promptText string

var promptTmpl = template.Must(template.New("prompt").Parse(promptText))

// maxBodyBytes caps the PR description in the prompt.
const maxBodyBytes = 60_000

// Prompt renders the instructions given to the agent. The untrusted
// material is fenced by markers carrying a random token, so text in it
// cannot close its own block and pass for instructions.
func Prompt(req Request, d *github.PullRequestDetails) (string, error) {
	body := d.Body
	if len(body) > maxBodyBytes {
		body = strings.ToValidUTF8(body[:maxBodyBytes], "") + "\n[description truncated]"
	}
	diff := d.Diff
	switch {
	case d.DiffError != "":
		diff = "[diff unavailable: " + d.DiffError + "]"
	case d.Truncated:
		diff += "\n[diff truncated]"
	}
	token := make([]byte, 8)
	if _, err := rand.Read(token); err != nil {
		return "", err
	}
	var b bytes.Buffer
	err := promptTmpl.Execute(&b, struct {
		Request
		Body, Diff, Token string
	}{req, body, diff, hex.EncodeToString(token)})
	return b.String(), err
}
