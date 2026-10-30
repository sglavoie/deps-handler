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
	"slices"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// listingApp keeps both listing endpoints on a fake transport, including
// the installation token exchange used by Repos.
func listingApp(t *testing.T, path string, pages []string, links []int) *App {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	private := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		header := http.Header{"Content-Type": {"application/json"}}
		var body string
		switch r.URL.Path {
		case "/app":
			body = `{"slug":"deps-handler"}`
		case "/app/installations/7/access_tokens":
			body = fmt.Sprintf(`{"token":"test","expires_at":%q}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		case path:
			page := r.URL.Query().Get("page")
			index := 0
			if page != "" {
				if _, err := fmt.Sscanf(page, "%d", &index); err != nil {
					return nil, err
				}
				index--
			}
			if index < 0 || index >= len(pages) {
				return nil, fmt.Errorf("unexpected page: %q", page)
			}
			body = pages[index]
			if index < len(links) && links[index] > 0 {
				header.Set("Link", fmt.Sprintf(`<https://api.github.com%s?page=%d>; rel="next"`, path, links[index]))
			}
		default:
			return nil, fmt.Errorf("unexpected request: %s", r.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: header,
			Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	original := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = original })
	a, err := New(t.Context(), 1, private)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestInstallationsRejectsIncompleteListings(t *testing.T) {
	valid := `[{"id":7,"account":{"login":"o"},"suspended_at":null}]`
	for _, tc := range []struct {
		name  string
		pages []string
	}{
		{"null list", []string{`null`}},
		{"empty response", []string{``}},
		{"null entry", []string{`[null]`}},
		{"missing ID", []string{`[{"account":{"login":"o"}}]`}},
		{"invalid ID", []string{`[{"id":-1,"account":{"login":"o"}}]`}},
		{"missing account", []string{`[{"id":7}]`}},
		{"blank account", []string{`[{"id":7,"account":{"login":" "}}]`}},
		{"incomplete later page", []string{valid, `null`}},
		{"repeated installation", []string{valid, valid}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var links []int
			if len(tc.pages) > 1 {
				links = []int{2}
			}
			a := listingApp(t, "/app/installations", tc.pages, links)
			got, err := a.Installations(t.Context())
			if err == nil || got != nil {
				t.Fatalf("Installations = %+v, %v; want no partial result and an error", got, err)
			}
		})
	}
}

func TestInstallationsReadsAllPages(t *testing.T) {
	a := listingApp(t, "/app/installations", []string{
		`[{"id":7,"account":{"login":"o"},"suspended_at":null}]`,
		`[{"id":8,"account":{"login":"p"},"suspended_at":"2026-10-01T12:00:00Z"}]`,
	}, []int{2})
	got, err := a.Installations(t.Context())
	want := []Installation{{ID: 7, Account: "o"}, {ID: 8, Account: "p", Suspended: true}}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("Installations = %+v, %v; want %+v", got, err, want)
	}
	a = listingApp(t, "/app/installations", []string{`[]`}, nil)
	if got, err := a.Installations(t.Context()); err != nil || len(got) != 0 {
		t.Fatalf("empty installations = %+v, %v", got, err)
	}
}

func TestReposRejectsIncompleteListings(t *testing.T) {
	valid := `{"total_count":2,"repositories":[{"id":1,"full_name":"o/r","archived":false}]}`
	for _, tc := range []struct {
		name  string
		pages []string
	}{
		{"null response", []string{`null`}},
		{"empty response", []string{``}},
		{"missing list", []string{`{"total_count":0}`}},
		{"null list", []string{`{"total_count":0,"repositories":null}`}},
		{"missing count", []string{`{"repositories":[]}`}},
		{"negative count", []string{`{"total_count":-1,"repositories":[]}`}},
		{"null entry", []string{`{"total_count":1,"repositories":[null]}`}},
		{"missing ID", []string{`{"total_count":1,"repositories":[{"full_name":"o/r","archived":false}]}`}},
		{"missing name", []string{`{"total_count":1,"repositories":[{"id":1,"archived":false}]}`}},
		{"invalid name", []string{`{"total_count":1,"repositories":[{"id":1,"full_name":"o/","archived":false}]}`}},
		{"missing archive flag", []string{`{"total_count":1,"repositories":[{"id":1,"full_name":"o/r"}]}`}},
		{"null archive flag", []string{`{"total_count":1,"repositories":[{"id":1,"full_name":"o/r","archived":null}]}`}},
		{"incomplete later page", []string{valid, `{"total_count":2}`}},
		{"count smaller than list", []string{`{"total_count":0,"repositories":[{"id":1,"full_name":"o/r","archived":false}]}`}},
		{"missing next page", []string{valid}},
		{"changing count", []string{valid, `{"total_count":3,"repositories":[{"id":2,"full_name":"o/s","archived":false}]}`}},
		{"repeated repository ID", []string{valid, `{"total_count":2,"repositories":[{"id":1,"full_name":"o/s","archived":false}]}`}},
		{"repeated repository name", []string{valid, `{"total_count":2,"repositories":[{"id":2,"full_name":"O/R","archived":false}]}`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var links []int
			if len(tc.pages) > 1 {
				links = []int{2}
			}
			a := listingApp(t, "/installation/repositories", tc.pages, links)
			got, err := a.Repos(t.Context(), 7)
			if err == nil || got != nil {
				t.Fatalf("Repos = %+v, %v; want no partial result and an error", got, err)
			}
		})
	}
}

func TestReposReadsAllPagesAndFiltersArchived(t *testing.T) {
	a := listingApp(t, "/installation/repositories", []string{
		`{"total_count":3,"repositories":[{"id":1,"full_name":"o/r","archived":false}]}`,
		`{"total_count":3,"repositories":[{"id":2,"full_name":"o/archive","archived":true},{"id":3,"full_name":"o/other","archived":false}]}`,
	}, []int{2})
	got, err := a.Repos(t.Context(), 7)
	if err != nil || !slices.Equal(got, []string{"o/r", "o/other"}) {
		t.Fatalf("Repos = %+v, %v", got, err)
	}
	a = listingApp(t, "/installation/repositories", []string{`{"total_count":0,"repositories":[]}`}, nil)
	if got, err := a.Repos(t.Context(), 7); err != nil || len(got) != 0 {
		t.Fatalf("empty repositories = %+v, %v", got, err)
	}
}

func TestListingsRejectInvalidPagination(t *testing.T) {
	for _, path := range []string{"/app/installations", "/installation/repositories"} {
		for _, next := range []int{1, 3} {
			t.Run(fmt.Sprintf("%s/next=%d", path, next), func(t *testing.T) {
				body := `[{"id":7,"account":{"login":"o"},"suspended_at":null}]`
				if path == "/installation/repositories" {
					body = `{"total_count":2,"repositories":[{"id":1,"full_name":"o/r","archived":false}]}`
				}
				a := listingApp(t, path, []string{body}, []int{next})
				var err error
				if path == "/app/installations" {
					_, err = a.Installations(t.Context())
				} else {
					_, err = a.Repos(t.Context(), 7)
				}
				if err == nil || !strings.Contains(err.Error(), "invalid pagination") {
					t.Fatalf("listing error = %v; want invalid pagination", err)
				}
			})
		}
	}
}

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
			body = `{"slug":"deps-handler","events":["pull_request"]}`
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
			body = `{"data":{"viewer":{"login":"deps-handler[bot]"}}}`
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
