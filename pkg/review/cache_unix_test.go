//go:build unix

package review

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sglavoie/deps-merger/pkg/policy"
)

func TestCacheFailedWriteKeepsStoredVerdicts(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write to a read-only directory")
	}
	for _, operation := range []string{"insert", "replace", "forget"} {
		t.Run(operation, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "verdicts.json")
			c, err := OpenCache(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Put("existing", "head", "agent", policy.Verdict{Summary: "rejected"}); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dir, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
			switch operation {
			case "insert":
				err = c.Put("new", "head", "agent", policy.Verdict{Approved: true})
			case "replace":
				err = c.Put("existing", "head", "agent", policy.Verdict{Approved: true})
			case "forget":
				_, err = c.Forget("existing")
			}
			if err == nil {
				t.Fatal("write to read-only cache directory succeeded")
			}
			reopened, err := OpenCache(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, cache := range []*Cache{c, reopened} {
				if v := cache.Get("existing", "head"); v == nil || v.Approved || v.Summary != "rejected" {
					t.Errorf("failed write changed existing verdict: %+v", v)
				}
				if v := cache.Get("new", "head"); v != nil {
					t.Errorf("failed write installed new verdict: %+v", v)
				}
			}
		})
	}
}
