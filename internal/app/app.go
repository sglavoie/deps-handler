// Package app authenticates as the GitHub App and as each of its
// installations.
package app

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	gh "github.com/google/go-github/v88/github"
	"github.com/sglavoie/deps-merger/pkg/github"
)

// App is the GitHub App, holding a transport per installation so their
// tokens are minted once and refreshed as they expire.
type App struct {
	ID   int64
	Slug string
	// Events are the webhook events the App subscribes to, as its
	// settings were when New read them.
	Events []string
	atr    *ghinstallation.AppsTransport
	api    *gh.Client

	mu       sync.Mutex
	installs map[int64]*ghinstallation.Transport
}

// New authenticates as App id with its private key, and learns its slug.
func New(ctx context.Context, id int64, key []byte) (*App, error) {
	atr, err := ghinstallation.NewAppsTransport(http.DefaultTransport, id, key)
	if err != nil {
		return nil, err
	}
	api, err := newClient(atr)
	if err != nil {
		return nil, err
	}
	a := &App{ID: id, atr: atr, api: api, installs: map[int64]*ghinstallation.Transport{}}
	app, _, err := a.api.Apps.Get(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("GitHub App %d: %w", id, err)
	}
	a.Slug, a.Events = app.GetSlug(), app.Events
	return a, nil
}

// Login is the App's own login, as its comments and issues carry it.
func (a *App) Login() string { return a.Slug + "[bot]" }

// Installation is one installation of the App, on an account.
type Installation struct {
	ID        int64
	Account   string
	Suspended bool
}

// Installations lists the App's installations.
func (a *App) Installations(ctx context.Context) ([]Installation, error) {
	var out []Installation
	for in, err := range a.api.Apps.ListInstallationsIter(ctx, &gh.ListOptions{PerPage: 100}) {
		if err != nil {
			return nil, err
		}
		out = append(out, Installation{ID: in.GetID(), Account: in.GetAccount().GetLogin(), Suspended: in.SuspendedAt != nil})
	}
	return out, nil
}

func (a *App) transport(id int64) *ghinstallation.Transport {
	a.mu.Lock()
	defer a.mu.Unlock()
	t, ok := a.installs[id]
	if !ok {
		t = ghinstallation.NewFromAppsTransport(a.atr, id)
		a.installs[id] = t
	}
	return t
}

func newClient(t http.RoundTripper) (*gh.Client, error) {
	return gh.NewClient(gh.WithHTTPClient(&http.Client{Transport: t, Timeout: github.RequestTimeout}))
}

// REST is a go-github client acting as installation id.
func (a *App) REST(id int64) (*gh.Client, error) {
	return newClient(a.transport(id))
}

// Repos lists the full names of the repositories installation id can
// reach.
func (a *App) Repos(ctx context.Context, id int64) ([]string, error) {
	c, err := a.REST(id)
	if err != nil {
		return nil, err
	}
	var out []string
	for r, err := range c.Apps.ListReposIter(ctx, &gh.ListOptions{PerPage: 100}) {
		if err != nil {
			return nil, fmt.Errorf("installation %d: %w", id, err)
		}
		if !r.GetArchived() {
			out = append(out, r.GetFullName())
		}
	}
	return out, nil
}

// Token is an access token of installation id, minted or refreshed as
// needed.
func (a *App) Token(ctx context.Context, id int64) (string, error) {
	return a.transport(id).Token(ctx)
}

// Engine is a deps-merger client acting as installation id. GitHub
// reports no viewerPermission to an installation, so what it was granted
// stands in: write when it may write contents and pull requests.
func (a *App) Engine(ctx context.Context, id int64) (github.Client, error) {
	t := a.transport(id)
	_, err := t.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("installation %d token: %w", id, err)
	}
	c, err := github.NewWithTokenSource(t.Token)
	if err != nil {
		return nil, err
	}
	perms, _ := t.Permissions()
	permission := "READ"
	if perms.GetContents() == "write" && perms.GetPullRequests() == "write" {
		permission = "WRITE"
	}
	return github.WithPermission(c, permission), nil
}

// SetWebhook points the App's webhook at url, signed with secret. GitHub
// offers no API to turn the webhook on or choose its events: those stay
// in the App's settings.
func (a *App) SetWebhook(ctx context.Context, url, secret string) error {
	_, _, err := a.api.Apps.UpdateHookConfig(ctx, &gh.HookConfig{URL: gh.Ptr(url), Secret: gh.Ptr(secret), ContentType: gh.Ptr("json"), InsecureSSL: gh.Ptr("0")})
	return err
}

// Delivery is one webhook delivery GitHub made, or tried to.
type Delivery struct {
	At         time.Time
	Event      string
	Action     string
	StatusCode int
	Status     string
	Redelivery bool
}

// Deliveries lists the App's latest webhook deliveries, newest first.
func (a *App) Deliveries(ctx context.Context, n int) ([]Delivery, error) {
	ds, _, err := a.api.Apps.ListHookDeliveries(ctx, &gh.ListCursorOptions{PerPage: n})
	if err != nil {
		return nil, err
	}
	out := make([]Delivery, len(ds))
	for i, d := range ds {
		out[i] = Delivery{At: d.GetDeliveredAt().Time, Event: d.GetEvent(), Action: d.GetAction(), StatusCode: d.GetStatusCode(), Status: d.GetStatus(), Redelivery: d.GetRedelivery()}
	}
	return out, nil
}
