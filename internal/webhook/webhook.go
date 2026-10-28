// Package webhook checks the App's webhook deliveries and turns the ones
// that matter into runs.
package webhook

import (
	"context"
	"log/slog"
	"net/http"

	gh "github.com/google/go-github/v88/github"
	"github.com/sglavoie/deps-merger/internal/jobs"
)

// maxBody is the largest payload GitHub sends.
const maxBody = 25 << 20

// dependabot is the login Dependabot's pull requests carry.
const dependabot = "dependabot[bot]"

// Handler serves the App's webhook.
type Handler struct {
	// Secret is the webhook secret. A handler without one refuses every
	// delivery: go-github checks no signature when no secret is given.
	Secret []byte
	// Enqueue asks for a run.
	Enqueue func(jobs.Job)
	// Installed and Uninstalled record an installation coming and going:
	// deleted when it was uninstalled, rather than suspended. An error
	// rejects the delivery so it can be inspected and redelivered.
	Installed   func(ctx context.Context, id int64, account string) error
	Uninstalled func(ctx context.Context, id int64, deleted bool) error
	Log         *slog.Logger
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if len(h.Secret) == 0 {
		http.Error(w, "webhook secret not configured", http.StatusInternalServerError)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	payload, err := gh.ValidatePayload(r, h.Secret)
	if err != nil {
		h.Log.Warn("webhook rejected", "err", err, "delivery", r.Header.Get("X-GitHub-Delivery"))
		http.Error(w, "invalid signature or payload", http.StatusUnauthorized)
		return
	}
	kind := gh.WebHookType(r)
	event, err := gh.ParseWebHook(kind, payload)
	if err != nil {
		// An event the App does not subscribe to, or cannot read.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := h.handle(r.Context(), kind, event); err != nil {
		h.Log.Error("webhook processing failed", "err", err, "delivery", r.Header.Get("X-GitHub-Delivery"))
		http.Error(w, "webhook processing failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (h *Handler) handle(ctx context.Context, kind string, event any) error {
	enqueue := func(installation int64, repo string) {
		if installation == 0 {
			return
		}
		h.Log.Info("run queued", "installation", installation, "repo", repo, "trigger", kind)
		h.Enqueue(jobs.Job{Installation: installation, Repo: repo, Trigger: kind})
	}
	switch e := event.(type) {
	case *gh.InstallationEvent:
		id := e.GetInstallation().GetID()
		switch e.GetAction() {
		case "created", "unsuspend", "new_permissions_accepted":
			if err := h.Installed(ctx, id, e.GetInstallation().GetAccount().GetLogin()); err != nil {
				return err
			}
			enqueue(id, "")
		case "deleted", "suspend":
			if err := h.Uninstalled(ctx, id, e.GetAction() == "deleted"); err != nil {
				return err
			}
		}
	case *gh.InstallationRepositoriesEvent:
		if e.GetAction() == "added" {
			for _, r := range e.RepositoriesAdded {
				enqueue(e.GetInstallation().GetID(), r.GetFullName())
			}
		}
	case *gh.PullRequestEvent:
		switch e.GetAction() {
		case "opened", "reopened", "synchronize", "closed":
			if e.GetPullRequest().GetUser().GetLogin() == dependabot {
				enqueue(e.GetInstallation().GetID(), e.GetRepo().GetFullName())
			}
		}
	case *gh.CheckSuiteEvent:
		// A suite on a pull request's head: one of its checks may decide
		// whether a Dependabot PR is safe.
		if e.GetAction() == "completed" && len(e.GetCheckSuite().PullRequests) > 0 {
			enqueue(e.GetInstallation().GetID(), e.GetRepo().GetFullName())
		}
	case *gh.PushEvent:
		// The default branch moved: PRs may be behind, and the policy file
		// may have changed.
		if e.GetRef() == "refs/heads/"+e.GetRepo().GetDefaultBranch() {
			enqueue(e.GetInstallation().GetID(), e.GetRepo().GetFullName())
		}
	}
	return nil
}
