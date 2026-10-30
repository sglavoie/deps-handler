package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sglavoie/deps-handler/internal/app"
	"github.com/sglavoie/deps-handler/internal/store"
)

func TestInstallationSnapshotsRespectOtherProcesses(t *testing.T) {
	for _, snapshot := range []string{"active", "suspended", "absent"} {
		for _, current := range []string{"active", "suspended", "deleted"} {
			t.Run(snapshot+"/newer "+current, func(t *testing.T) {
				s, p := setup(t, false, "")
				ctx := context.Background()
				// A second connection and independent Service gates model a
				// server and -once process sharing the database and files.
				otherStore, err := store.Open(ctx, p.dbPath)
				if err != nil {
					t.Fatal(err)
				}
				defer otherStore.Close()
				s.ReviewDir = t.TempDir()
				other := &Service{Store: otherStore, Log: s.Log, ReviewDir: s.ReviewDir}
				if err := s.Store.InstallationAdded(ctx, 1, "o", t0); err != nil {
					t.Fatal(err)
				}
				if err := s.Store.SeenPR(ctx, 1, "o/r", 7, "update", t0, t0); err != nil {
					t.Fatal(err)
				}
				rv := s.reviewFiles()
				checkout := filepath.Join(rv.checkouts(1), "o", "r")
				if err := os.MkdirAll(checkout, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Dir(rv.verdicts(1)), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(rv.verdicts(1), []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
				switch snapshot {
				case "active":
					p.installations = []app.Installation{{ID: 1, Account: "stale-name"}}
				case "suspended":
					p.installations = []app.Installation{{ID: 1, Account: "stale-name", Suspended: true}}
				case "absent":
					p.installations = []app.Installation{}
				}
				p.onList = func() {
					var err error
					switch current {
					case "active":
						err = other.InstallationAdded(ctx, 1, "current-name")
					case "suspended":
						err = other.installationSuspendedAt(ctx, 1, "current-name", time.Now())
					case "deleted":
						err = other.InstallationRemoved(ctx, 1, true)
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				if _, err := s.SyncInstallations(ctx); err != nil {
					t.Fatal(err)
				}
				if inactive, err := s.Store.InstallationInactive(ctx, 1); err != nil || inactive != (current != "active") {
					t.Fatalf("stale snapshot changed current state: inactive = %t, %v", inactive, err)
				}
				prs, err := s.Store.OpenPRs(ctx, "o/r")
				if err != nil || (len(prs) == 1) != (current != "deleted") {
					t.Fatalf("stale snapshot changed retained PRs: %v, %v", prs, err)
				}
				for _, path := range []string{checkout, rv.verdicts(1)} {
					_, err := os.Stat(path)
					if current == "deleted" && !os.IsNotExist(err) || current != "deleted" && err != nil {
						t.Errorf("unexpected retained files: %s: %v", path, err)
					}
				}
				if current != "deleted" {
					ins, err := s.Store.Installations(ctx)
					if err != nil || len(ins) != 1 || ins[0].Account != "current-name" {
						t.Fatalf("stale snapshot changed current account: %+v, %v", ins, err)
					}
				}
			})
		}
	}
}

func TestStaleSweepKeepsNewInstallationFromAnotherProcess(t *testing.T) {
	s, p := setup(t, false, "")
	ctx := context.Background()
	otherStore, err := store.Open(ctx, p.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer otherStore.Close()
	other := &Service{Store: otherStore, Log: s.Log}
	p.installations = []app.Installation{}
	p.onList = func() {
		if err := other.InstallationAdded(ctx, 2, "new-owner"); err != nil {
			t.Fatal(err)
		}
		if err := other.Store.SeenPR(ctx, 2, "new-owner/r", 7, "update", t0, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.SyncInstallations(ctx); err != nil {
		t.Fatal(err)
	}
	if inactive, err := s.Store.InstallationInactive(ctx, 2); err != nil || inactive {
		t.Fatalf("new installation removed: inactive = %t, %v", inactive, err)
	}
	if prs, err := s.Store.OpenPRs(ctx, "new-owner/r"); err != nil || len(prs) != 1 {
		t.Fatalf("new installation records deleted: %v, %v", prs, err)
	}
}

func TestCleanupRechecksAnotherProcessesObservation(t *testing.T) {
	for _, inactive := range []bool{false, true} {
		t.Run(map[bool]string{false: "reactivated", true: "suspended"}[inactive], func(t *testing.T) {
			s, p := setup(t, false, "")
			ctx := context.Background()
			if err := s.Store.InstallationRemoved(ctx, 1, t0); err != nil {
				t.Fatal(err)
			}
			otherStore, err := store.Open(ctx, p.dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer otherStore.Close()
			if _, err := otherStore.ObserveInstallation(ctx, 1, "o", inactive, t0.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			if err := otherStore.SeenPR(ctx, 1, "o/r", 7, "update", t0, t0); err != nil {
				t.Fatal(err)
			}
			// Cleanup may have waited for an in-flight run or a file lock
			// while another process recorded this newer state.
			if err := s.forgetIfInactiveBefore(ctx, 1, t0); err != nil {
				t.Fatal(err)
			}
			if prs, err := s.Store.OpenPRs(ctx, "o/r"); err != nil || len(prs) != 1 {
				t.Fatalf("stale cleanup deleted current records: %v, %v", prs, err)
			}
		})
	}
}
