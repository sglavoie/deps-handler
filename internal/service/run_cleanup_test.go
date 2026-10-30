package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	gh "github.com/google/go-github/v88/github"
	"github.com/sglavoie/deps-handler/internal/store"
	"github.com/sglavoie/deps-handler/pkg/github"
)

type snapshotEngine struct {
	github.Client
	afterRead func()
}

func TestIssueResponseCannotRestoreRecordsDeletedByAnotherProcess(t *testing.T) {
	s, p := setup(t, false, "", pr(7))
	ctx := context.Background()
	if err := s.InstallationAdded(ctx, 1, "o"); err != nil {
		t.Fatal(err)
	}
	otherStore, err := store.Open(ctx, p.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer otherStore.Close()
	other := &Service{Store: otherStore, Log: s.Log}
	removed := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues" {
			var request struct{ Title, Body string }
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}
			issue := p.issues.Add(login, request.Title, request.Body, "open")
			if err := other.InstallationRemoved(ctx, 1, true); err != nil {
				t.Error(err)
				http.Error(w, "cleanup failed", http.StatusInternalServerError)
				return
			}
			removed = true
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(issue)
			return
		}
		p.issues.ServeHTTP(w, r)
	}))
	defer srv.Close()
	base := srv.URL + "/"
	p.rest, err = gh.NewClient(gh.WithHTTPClient(srv.Client()), gh.WithURLs(&base, &base))
	if err != nil {
		t.Fatal(err)
	}
	// The audit write can fail because its run was deleted. Neither it nor
	// remembering the new issue may recreate repository records.
	_ = s.RunRepo(ctx, 1, "o/r", "test")
	if !removed {
		t.Fatal("issue creation did not exercise cleanup")
	}
	if n, err := s.Store.IssueNumber(ctx, "o/r"); err != nil || n != 0 {
		t.Fatalf("late issue response restored issue number %d, %v", n, err)
	}
	metrics, err := s.Store.Metrics(ctx, time.Now())
	if err != nil || len(metrics) != 1 || metrics[0].Repos != 0 || metrics[0].PRs != 0 || metrics[0].Writes != 0 {
		t.Fatalf("late issue response restored records: %+v, %v", metrics, err)
	}
}

func (e snapshotEngine) FetchRepo(ctx context.Context, owner, name string) (*github.Repo, error) {
	r, err := e.Client.FetchRepo(ctx, owner, name)
	if err == nil {
		e.afterRead()
	}
	return r, err
}

type snapshotPlatform struct {
	*platform
	client github.Client
}

func (p snapshotPlatform) Engine(context.Context, int64) (github.Client, error) {
	return p.client, nil
}

func TestRunCannotRestoreRecordsDeletedByAnotherProcess(t *testing.T) {
	for _, during := range []string{"repository read", "PR reconciliation"} {
		t.Run(during, func(t *testing.T) {
			s, p := setup(t, false, "", pr(7))
			ctx := context.Background()
			if err := s.InstallationAdded(ctx, 1, "o"); err != nil {
				t.Fatal(err)
			}
			// Separate connections and Service gates model a server and
			// -once process sharing a database, with reviews disabled.
			otherStore, err := store.Open(ctx, p.dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer otherStore.Close()
			other := &Service{Store: otherStore, Log: s.Log}
			uninstall := func() {
				if err := other.InstallationRemoved(ctx, 1, true); err != nil {
					t.Fatal(err)
				}
			}
			if during == "repository read" {
				s.Platform = snapshotPlatform{platform: p, client: snapshotEngine{Client: p.engine, afterRead: uninstall}}
			} else {
				if err := s.Store.SeenPR(ctx, 1, "o/r", 3, "previous PR", t0, t0); err != nil {
					t.Fatal(err)
				}
				p.engine.gone = map[int]bool{3: true}
				p.engine.onState = uninstall
			}
			if err := s.RunRepo(ctx, 1, "o/r", "test"); err != nil {
				t.Fatal(err)
			}
			metrics, err := s.Store.Metrics(ctx, time.Now())
			if err != nil || len(metrics) != 1 || metrics[0].PRs != 0 || metrics[0].Repos != 0 {
				t.Fatalf("uninstalled repository data recreated: %+v, %v", metrics, err)
			}
			if calls := p.issues.TakeCalls(); len(calls) != 0 {
				t.Fatalf("uninstalled run reported to GitHub: %v", calls)
			}
		})
	}
}
