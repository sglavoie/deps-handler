package service

import (
	"errors"
	"net/http"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
	gh "github.com/google/go-github/v88/github"
	"github.com/sglavoie/deps-merger/pkg/github"
)

// checkRateLimit refuses requests during the installation's cooldown.
// The runner recognizes the reset time and can wait for a short pause.
func (g *installationGate) checkRateLimit() error {
	g.rateMu.Lock()
	defer g.rateMu.Unlock()
	if time.Now().Before(g.retryAt) {
		return &github.RateLimitError{Err: errors.New("not sent: GitHub requests are paused for this installation's rate limit"), Reset: g.retryAt}
	}
	return nil
}

func (g *installationGate) observeRateLimit(err error) {
	delay, limited := rateLimitDelay(err)
	if limited {
		g.rateMu.Lock()
		defer g.rateMu.Unlock()
		if until := time.Now().Add(delay); until.After(g.retryAt) {
			g.retryAt = until
		}
	}
}

// The engine and issue client use different GitHub libraries. Prefer the
// raw headers in both: the engine parser also handles HTTP-date and large
// Retry-After values, which go-github's parsed duration can lose.
func rateLimitDelay(err error) (time.Duration, bool) {
	if delay, limited := github.RateLimited(err); limited {
		return delay, true
	}
	var resp *http.Response
	var message string
	var primary *gh.RateLimitError
	var secondary *gh.AbuseRateLimitError
	var other *gh.ErrorResponse
	var fallback time.Duration
	known := false
	switch {
	case errors.As(err, &primary):
		resp, message = primary.Response, "rate limit: "+primary.Message
		fallback, known = time.Minute, true
		if !primary.Rate.Reset.IsZero() {
			fallback = max(0, time.Until(primary.Rate.Reset.Time))
		}
	case errors.As(err, &secondary):
		resp, message = secondary.Response, "secondary rate limit: "+secondary.Message
		fallback, known = time.Minute, true
		if secondary.RetryAfter != nil {
			fallback = max(0, *secondary.RetryAfter)
		}
	case errors.As(err, &other):
		resp, message = other.Response, other.Message
	}
	if resp == nil {
		return fallback, known
	}
	delay, limited := github.RateLimited(&api.HTTPError{StatusCode: resp.StatusCode, Headers: resp.Header, Message: message})
	return max(delay, fallback), limited || known
}

// Each issue sync gets a fresh client. Observe responses in the transport
// so even a best-effort pin failure is shared with the engine and the next
// repository. Cloning retains authentication, endpoints and timeouts.
func withRateLimit(c *gh.Client, g *installationGate) (*gh.Client, error) {
	httpClient := c.Client()
	base := httpClient.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	httpClient.Transport = rateLimitTransport{base: base, gate: g}
	return c.Clone(gh.WithHTTPClient(httpClient))
}

type rateLimitTransport struct {
	base http.RoundTripper
	gate *installationGate
}

func (t rateLimitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := t.gate.checkRateLimit(); err != nil {
		if req.Body != nil {
			req.Body.Close()
		}
		return nil, err
	}
	resp, err := t.base.RoundTrip(req)
	if err == nil && resp != nil && (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests) {
		body := resp.Body
		// CheckResponse restores the error body for the issue client.
		t.gate.observeRateLimit(gh.CheckResponse(resp))
		body.Close()
	}
	return resp, err
}
