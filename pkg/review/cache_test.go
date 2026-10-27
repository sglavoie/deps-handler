package review

import (
	"path/filepath"
	"testing"

	"github.com/sglavoie/deps-merger/pkg/policy"
)

func TestCacheOwnsVerdictConcerns(t *testing.T) {
	for _, operation := range []string{"put", "get", "entries"} {
		t.Run(operation, func(t *testing.T) {
			c, err := OpenCache(filepath.Join(t.TempDir(), "verdicts.json"))
			if err != nil {
				t.Fatal(err)
			}
			v := policy.Verdict{Concerns: []string{"breaking change"}}
			if err := c.Put("key", "head", "agent", v); err != nil {
				t.Fatal(err)
			}
			switch operation {
			case "put":
				v.Concerns[0] = "changed by caller"
			case "get":
				c.Get("key", "head").Concerns[0] = "changed by caller"
			case "entries":
				c.Entries()[0].Verdict.Concerns[0] = "changed by caller"
			}
			if got := c.Get("key", "head").Concerns[0]; got != "breaking change" {
				t.Fatalf("caller changed cached concern: %q", got)
			}
		})
	}
}
