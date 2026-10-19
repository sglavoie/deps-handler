// Package review asks an AI agent whether a Dependabot update the policy
// alone would not merge (a major bump, or one of unknown size) is safe for
// the repository. The agent only reads: it returns a verdict, and
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
	"slices"
	"strings"
	"text/template"
	"time"

	"github.com/sglavoie/deps-merger/internal/config"
	"github.com/sglavoie/deps-merger/internal/dependabot"
	"github.com/sglavoie/deps-merger/internal/github"
	"github.com/sglavoie/deps-merger/internal/policy"
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
}

// NewRequest describes the PR behind d in repository rc. Its checks are
// the ones the policy judged: runs a newer suite superseded are left out.
func NewRequest(rc config.Repo, d policy.Decision) Request {
	noChecks := "the repository does not require checks"
	if rc.Policy.RequireChecks {
		noChecks = fmt.Sprintf("no check started within %s of the latest commit, after which the repository's policy merges without them because its CI does not run for every change", config.ShortDuration(rc.Policy.MergeWithoutChecksAfter))
	}
	return Request{
		Owner: rc.Owner, Name: rc.Name, Number: d.Number, Title: d.Title, URL: d.URL,
		HeadOID: d.PR.HeadOID, BaseRef: d.PR.BaseRef, Grade: d.Grade, Limit: rc.Policy.MaxUpdateType,
		Reason: strings.TrimSuffix(d.Reason, policy.AwaitingReview), Dependencies: d.Dependencies, Checks: policy.LatestChecks(d.PR.Checks),
		NoChecks: noChecks,
	}
}

// Key identifies the update under review: the PR and the versions it moves
// to. A rebase changes the head commit but not the update, so an approval
// still holds (the cache drops a rejection then); when Dependabot does not
// record versions, the head commit stands in for them. The repository is
// lower-cased, as GitHub ignores its case, and the agent is left out: a
// verdict from one agent is reused by another.
func (r Request) Key() string {
	deps := make([]string, 0, len(r.Dependencies))
	versioned := len(r.Dependencies) > 0
	for _, d := range r.Dependencies {
		deps = append(deps, d.Name+"@"+d.Version)
		versioned = versioned && d.Version != ""
	}
	slices.Sort(deps)
	key := fmt.Sprintf("%s/%s#%d %s", strings.ToLower(r.Owner), strings.ToLower(r.Name), r.Number, strings.Join(deps, ","))
	if !versioned {
		key += " " + r.HeadOID
	}
	return key
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
	v, err := r.review(ctx, req)
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return v, fmt.Errorf("%s timed out after %s", r.Agent.Name(), r.Timeout)
	}
	return v, err
}

func (r *AgentReviewer) review(ctx context.Context, req Request) (policy.Verdict, error) {
	details, err := github.Again(ctx, 3*time.Second, nil, func() (*github.PullRequestDetails, error) {
		return r.Details.PullRequestDetails(ctx, req.Owner, req.Name, req.Number)
	})
	if err != nil {
		return policy.Verdict{}, fmt.Errorf("fetch PR details: %w", err)
	}
	dir, err := r.Workspace.Checkout(ctx, req.Owner, req.Name, req.Number, req.HeadOID)
	if err != nil {
		return policy.Verdict{}, fmt.Errorf("checkout: %w", err)
	}
	prompt, err := Prompt(req, details)
	if err != nil {
		return policy.Verdict{}, err
	}
	a, err := r.Agent.Run(ctx, dir, prompt)
	if err != nil {
		return policy.Verdict{}, err
	}
	return Verdict(a)
}

// Verdict turns an agent's answer into a verdict. An answer that is
// neither "approve" nor "reject" is an error, not a verdict.
func Verdict(a Answer) (policy.Verdict, error) {
	switch a.Decision {
	case "approve", "reject":
	default:
		return policy.Verdict{}, fmt.Errorf("agent answered %q instead of approve or reject", a.Decision)
	}
	concerns := append(append([]string{}, a.BreakingChanges...), a.Concerns...)
	return policy.Verdict{Approved: a.Decision == "approve", Summary: strings.TrimSpace(a.Summary), Concerns: concerns}, nil
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
