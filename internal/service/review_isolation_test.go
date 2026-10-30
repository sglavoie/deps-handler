package service

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sglavoie/deps-handler/internal/app"
	"github.com/sglavoie/deps-handler/pkg/review"
)

func TestReviewCannotReplaceAnotherServicesCheckout(t *testing.T) {
	origin, heads := remote(t, 2)
	s1, p1 := setup(t, true, "mode: merge\nagent_review: true\n", major(1, heads[0]))
	s2, p2 := setup(t, true, "mode: merge\nagent_review: true\n", major(2, heads[1]))
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a1, a2 := &agent{decision: "approve"}, &agent{decision: "approve"}
	withReview(t, s1, a1, origin, 10)
	withReview(t, s2, a2, origin, 10)
	// Independent Services (and even different databases) must cooperate
	// when configured to use the same checkout directory.
	s2.Review.Dir = s1.Review.Dir
	a1.onRun = func() {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
		}
		data, err := os.ReadFile(filepath.Join(s1.Review.checkouts(1), "o", "r", "package.json"))
		if err != nil || !strings.Contains(string(data), "^1.0.0") {
			t.Errorf("PR #1 reviewer read another checkout: %s, %v", data, err)
		}
	}
	done := make(chan error, 1)
	go func() { done <- s1.RunRepo(ctx, 1, "o/r", "server") }()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("first run stopped before review: %v", err)
	case <-ctx.Done():
		t.Fatal("first review did not start")
	}
	err := s2.RunRepo(ctx, 1, "o/r", "once")
	if err == nil || !strings.Contains(err.Error(), "lock installation review files") || len(a2.prompts) != 0 || len(p2.engine.merged) != 0 {
		t.Errorf("overlapping run: %v; reviews %d; merges %v", err, len(a2.prompts), p2.engine.merged)
	}
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p1.engine.merged, []int{1}) {
		t.Fatalf("first run merged %v", p1.engine.merged)
	}
	// The lock must be released when the run ends so the refused run can retry.
	if err := s2.RunRepo(ctx, 1, "o/r", "retry"); err != nil {
		t.Fatal(err)
	}
	if len(a2.prompts) != 1 || !slices.Equal(p2.engine.merged, []int{2}) {
		t.Fatalf("retry: reviews %d; merges %v", len(a2.prompts), p2.engine.merged)
	}
}

func TestUninstallRemovesAllReviewFilesWithReviewsDisabledOrEnabled(t *testing.T) {
	for _, trigger := range []string{"webhook", "sweep"} {
		for _, disabled := range []bool{false, true} {
			name := trigger + "/reviews enabled"
			if disabled {
				name = trigger + "/reviews disabled"
			}
			t.Run(name, func(t *testing.T) {
				origin, heads := remote(t, 1)
				s, p := setup(t, false, "agent_review: true\n", major(1, heads[0]))
				withReview(t, s, &agent{decision: "reject"}, origin, 10)
				ctx := context.Background()
				if err := s.Store.InstallationAdded(ctx, 1, "o", time.Now()); err != nil {
					t.Fatal(err)
				}
				if err := s.RunRepo(ctx, 1, "o/r", "test"); err != nil {
					t.Fatal(err)
				}
				rv := s.Review
				path := rv.verdicts(1)
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				// Recover a cache whose write was cut short, retaining private
				// review text in the archived file, then create a fresh cache.
				if err := os.WriteFile(path, data[:len(data)-1], 0600); err != nil {
					t.Fatal(err)
				}
				if _, err := review.OpenCache(path); err != nil {
					t.Fatal(err)
				}
				entries, err := os.ReadDir(filepath.Dir(path))
				if err != nil {
					t.Fatal(err)
				}
				removed := []string{rv.checkouts(1), path}
				for _, entry := range entries {
					if strings.HasPrefix(entry.Name(), "1.json.corrupt-") {
						removed = append(removed, filepath.Join(filepath.Dir(path), entry.Name()))
					}
				}
				if len(removed) != 3 {
					t.Fatal("corrupt cache was not archived")
				}
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				kept := []string{rv.verdicts(10), rv.verdicts(10) + ".corrupt-old"}
				for _, other := range kept {
					if err := os.WriteFile(other, []byte("another installation"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				s.ReviewDir = rv.Dir
				if disabled {
					s.Review = nil
				}
				if trigger == "webhook" {
					err = s.InstallationRemoved(ctx, 1, true)
				} else {
					p.installations = []app.Installation{{ID: 10, Account: "other"}}
					_, err = s.SyncInstallations(ctx)
				}
				if err != nil {
					t.Fatal(err)
				}
				for _, file := range removed {
					if _, err := os.Stat(file); !os.IsNotExist(err) {
						t.Errorf("private review files kept after uninstall: %s: %v", file, err)
					}
				}
				for _, file := range kept {
					if _, err := os.Stat(file); err != nil {
						t.Errorf("another installation's data removed: %s: %v", file, err)
					}
				}
			})
		}
	}
}

func TestUninstallRetriesWhileAnotherProcessHoldsReviewFiles(t *testing.T) {
	s, p := setup(t, false, "")
	s.ReviewDir = t.TempDir()
	rv := s.reviewFiles()
	dir := rv.checkouts(1)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.Store.SeenPR(ctx, 1, "o/r", 1, "update", t0, t0); err != nil {
		t.Fatal(err)
	}
	release, err := review.Lock(dir + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := s.InstallationRemoved(ctx, 1, true); err == nil || !strings.Contains(err.Error(), "lock installation review files") {
		t.Fatalf("uninstall did not refuse active review files: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("active checkout removed: %v", err)
	}
	if prs, err := s.Store.OpenPRs(ctx, "o/r"); err != nil || !slices.Equal(prs, []int{1}) {
		t.Fatalf("records deleted before file lock acquired: %v, %v", prs, err)
	}
	release()
	p.installations = []app.Installation{{ID: 10, Account: "other"}}
	if _, err := s.SyncInstallations(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("checkout survived sweep retry: %v", err)
	}
	if prs, err := s.Store.OpenPRs(ctx, "o/r"); err != nil || len(prs) != 0 {
		t.Fatalf("records survived sweep retry: %v, %v", prs, err)
	}
}

func TestReviewDoesNotRecreateFilesAfterConcurrentRemoval(t *testing.T) {
	for _, removal := range []string{"installation", "repository"} {
		t.Run(removal, func(t *testing.T) {
			origin, heads := remote(t, 1)
			s, p := setup(t, true, "mode: merge\nagent_review: true\n", major(1, heads[0]))
			a := &agent{decision: "approve"}
			withReview(t, s, a, origin, 10)
			other := &Service{Store: s.Store, ReviewDir: s.Review.Dir}
			ctx := context.Background()
			var once sync.Once
			// RunRepo checked access already. Simulate another process
			// removing it while the repository's policy is being read.
			p.engine.onFile = func() {
				once.Do(func() {
					var err error
					if removal == "installation" {
						err = other.InstallationRemoved(ctx, 1, true)
					} else {
						err = other.RepositoryRemoved(ctx, 1, "o/r")
					}
					if err != nil {
						t.Fatal(err)
					}
				})
			}
			if err := s.RunRepo(ctx, 1, "o/r", "test"); err == nil || !strings.Contains(err.Error(), "agent review setup") {
				t.Fatalf("review setup ignored removal: %v", err)
			}
			if len(a.prompts) != 0 || len(p.engine.merged) != 0 {
				t.Fatalf("removed access: reviews %d; merges %v", len(a.prompts), p.engine.merged)
			}
			for _, path := range []string{s.Review.checkouts(1), s.Review.verdicts(1)} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Errorf("review files created after access was removed: %s: %v", path, err)
				}
			}
		})
	}
}
