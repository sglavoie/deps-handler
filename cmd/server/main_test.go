package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sglavoie/deps-merger/internal/jobs"
	"github.com/sglavoie/deps-merger/pkg/review"
)

func TestListenerFailureStopsQueuedWorkers(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q := &jobs.Queues{Run: func(context.Context, jobs.Job) error { return nil }, Debounce: time.Hour}
	q.Start(ctx)
	q.Enqueue(jobs.Job{Installation: 1, Repo: "o/r"})
	done := make(chan error, 1)
	go func() {
		done <- listenAndDrain(&http.Server{Addr: occupied.Addr().String()}, q, cancel)
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "address already in use") {
			t.Fatalf("listener error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("listener failure left a queued worker running")
	}
}

func TestLoadReviewBackends(t *testing.T) {
	for _, backend := range []string{"claude", "codex"} {
		t.Run(backend, func(t *testing.T) {
			bin := t.TempDir()
			if err := os.WriteFile(filepath.Join(bin, backend), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin)
			t.Setenv("DMC_REVIEW_AGENT", backend)
			// A local API configuration in dev.env must not select its model
			// or require its key when a CLI backend was requested.
			t.Setenv("DMC_REVIEW_URL", "http://127.0.0.1:8081/v1")
			t.Setenv("DMC_REVIEW_MODEL", "local-model")
			t.Setenv("DMC_REVIEW_KEY_ENV", "UNSET_REVIEW_KEY")
			t.Setenv("DMC_REVIEWS_PER_DAY", "3")
			t.Setenv("DMC_REVIEW_TIMEOUT", "5m")
			r, err := loadReview(filepath.Join(t.TempDir(), "app.db"))
			if err != nil {
				t.Fatal(err)
			}
			if r == nil || r.Model != backend || r.Available != nil || r.PerDay != 3 || r.Timeout != 5*time.Minute {
				t.Fatalf("review = %+v", r)
			}
			a := r.Agent(nil)
			if a.Name() != backend {
				t.Fatalf("agent = %q", a.Name())
			}
			switch backend {
			case "claude":
				if _, ok := a.(review.Claude); !ok {
					t.Fatalf("agent = %T", a)
				}
			case "codex":
				if _, ok := a.(review.Codex); !ok {
					t.Fatalf("agent = %T", a)
				}
			}
		})
	}
}

func TestLoadReviewBackendValidation(t *testing.T) {
	t.Setenv("DMC_REVIEW_URL", "")
	t.Setenv("DMC_REVIEW_MODEL", "")
	t.Setenv("DMC_REVIEW_AGENT", "api")
	r, err := loadReview("app.db")
	if err != nil || r != nil {
		t.Fatalf("unconfigured API review = %v, %v", r, err)
	}
	t.Setenv("DMC_REVIEW_AGENT", "other")
	if _, err := loadReview("app.db"); err == nil || !strings.Contains(err.Error(), "DMC_REVIEW_AGENT") {
		t.Fatalf("unknown backend error = %v", err)
	}
	t.Setenv("DMC_REVIEW_AGENT", "claude")
	t.Setenv("PATH", t.TempDir())
	if _, err := loadReview("app.db"); err == nil || !strings.Contains(err.Error(), "claude is not installed") {
		t.Fatalf("missing CLI error = %v", err)
	}
}

func TestLoadReviewDefaultAPI(t *testing.T) {
	t.Setenv("DMC_REVIEW_AGENT", "")
	t.Setenv("DMC_REVIEW_URL", "http://127.0.0.1:8081/v1")
	t.Setenv("DMC_REVIEW_MODEL", "local-model")
	t.Setenv("DMC_REVIEW_KEY_ENV", "TEST_MODEL_KEY")
	t.Setenv("TEST_MODEL_KEY", "key")
	t.Setenv("DMC_REVIEW_TOOLS", "false")
	r, err := loadReview("app.db")
	if err != nil {
		t.Fatal(err)
	}
	if r == nil || r.Model != "local-model" || r.Available == nil {
		t.Fatalf("review = %+v", r)
	}
	a, ok := r.Agent(nil).(review.API)
	if !ok || a.URL != "http://127.0.0.1:8081/v1" || a.Model != "local-model" || a.Key != "key" || a.Tools {
		t.Fatalf("API agent = %+v", a)
	}
}

func TestPingPreservesModelURLQuery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.URL.RawQuery != "api-key=secret" {
			t.Errorf("request URL = %s", r.URL.String())
			http.Error(w, "wrong URL", http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	if err := ping(context.Background(), server.URL+"/v1?api-key=secret", "token"); err != nil {
		t.Fatal(err)
	}
}
