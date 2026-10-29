package app

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestEngineRefreshesInstallationToken(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	private := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	minted := 0
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body string
		switch r.URL.Path {
		case "/app":
			body = `{"slug":"deps-merger","events":["pull_request"]}`
		case "/app/installations/7/access_tokens":
			minted++
			token, expiry := "near-expiry", time.Now().Add(30*time.Second)
			if minted > 1 {
				token, expiry = "fresh", time.Now().Add(time.Hour)
			}
			data, err := json.Marshal(map[string]any{"token": token, "expires_at": expiry,
				"permissions": map[string]string{"contents": "write", "pull_requests": "write"}})
			if err != nil {
				t.Fatal(err)
			}
			body = string(data)
		case "/graphql":
			if got := r.Header.Get("Authorization"); got != "token fresh" {
				t.Errorf("engine kept an expiring token: Authorization = %q", got)
			}
			body = `{"data":{"viewer":{"login":"deps-merger[bot]"}}}`
		default:
			return nil, fmt.Errorf("unexpected request: %s", r.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	a, err := New(context.Background(), 1, private)
	if err != nil {
		t.Fatal(err)
	}
	c, err := a.Engine(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Viewer(context.Background()); err != nil {
		t.Fatal(err)
	}
	if minted != 2 {
		t.Fatalf("minted %d tokens; want a refresh before the API request", minted)
	}
}
