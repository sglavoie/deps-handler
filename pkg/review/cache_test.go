package review

import (
	"path/filepath"
	"testing"

	"github.com/sglavoie/deps-merger/pkg/dependabot"
	"github.com/sglavoie/deps-merger/pkg/policy"
)

func TestForgetExactUpdatePreservesOtherUpdates(t *testing.T) {
	c, err := OpenCache(filepath.Join(t.TempDir(), "verdicts.json"))
	if err != nil {
		t.Fatal(err)
	}
	target := `o/r#1 base="main" lib@1`
	others := []string{
		`o/r#1 base="Main" lib@1`,
		`o/r#1 base="main" Lib@1`,
		`o/r#1 base="main" lib@1-RC`,
		target + " head", // A full key must not also act as a prefix.
	}
	for _, key := range append([]string{target}, others...) {
		if err := c.Put(key, "head", "agent", policy.Verdict{Approved: true}); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := c.Forget(`O/R#1 base="main" lib@1`); err != nil || n != 1 {
		t.Fatalf("Forget = %d, %v; want only the exact update removed", n, err)
	}
	for _, key := range others {
		if c.Get(key, "head") == nil {
			t.Errorf("unrelated verdict removed: %s", key)
		}
	}
}

func TestCacheDoesNotReuseUnescapedDependencyKeys(t *testing.T) {
	c, err := OpenCache(filepath.Join(t.TempDir(), "verdicts.json"))
	if err != nil {
		t.Fatal(err)
	}
	r := request()
	// This old key could represent two dependencies or one dependency
	// whose version contains ",other@2". Neither may reuse its approval.
	if err := c.Put(`o/r#7 base="main" lib@1,other@2`, r.HeadOID, "agent", policy.Verdict{Approved: true}); err != nil {
		t.Fatal(err)
	}
	for _, deps := range [][]dependabot.Dependency{
		{{Name: "lib", Version: "1,other@2"}},
		{{Name: "lib", Version: "1"}, {Name: "other", Version: "2"}},
	} {
		r.Dependencies = deps
		if c.Get(r.Key(), r.HeadOID) != nil {
			t.Fatalf("reused ambiguous legacy approval for %+v", deps)
		}
	}
}

func TestCacheDoesNotReuseKeysFromUnverifiedHosts(t *testing.T) {
	c, err := OpenCache(filepath.Join(t.TempDir(), "verdicts.json"))
	if err != nil {
		t.Fatal(err)
	}
	r := request()
	// Before host validation, this key could hold an Enterprise approval
	// for the same repository, PR number, branch and dependency versions.
	if err := c.Put(`o/r#7 v2 base="main" "lib"@"2.0.0"`, r.HeadOID, "agent", policy.Verdict{Approved: true}); err != nil {
		t.Fatal(err)
	}
	if c.Get(r.Key(), r.HeadOID) != nil {
		t.Fatal("reused approval whose GitHub host was not verified")
	}
	if err := c.Put(r.Key(), r.HeadOID, "agent", policy.Verdict{Approved: true}); err != nil {
		t.Fatal(err)
	}
	if c.Get(r.Key(), r.HeadOID) == nil {
		t.Fatal("did not reuse a fresh approval")
	}
}

func TestCacheKeysDistinguishDependencySeparators(t *testing.T) {
	for name, pair := range map[string][2][]dependabot.Dependency{
		"dependency boundary": {
			{{Name: "lib", Version: "1,other@2"}},
			{{Name: "lib", Version: "1"}, {Name: "other", Version: "2"}},
		},
		"version boundary": {
			{{Name: "lib@tag", Version: "2"}},
			{{Name: "lib", Version: "tag@2"}},
		},
		"literal quotes": {
			{{Name: `"lib@tag"`, Version: "2"}},
			{{Name: "lib@tag", Version: "2"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			a, b := request(), request()
			a.Dependencies, b.Dependencies = pair[0], pair[1]
			c, err := OpenCache(filepath.Join(t.TempDir(), "verdicts.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Put(a.Key(), a.HeadOID, "agent", policy.Verdict{Approved: true}); err != nil {
				t.Fatal(err)
			}
			if c.Get(b.Key(), b.HeadOID) != nil {
				t.Fatalf("different dependencies reused approval under %q", a.Key())
			}
		})
	}
}

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
