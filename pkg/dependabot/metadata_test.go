package dependabot

import (
	"testing"

	"github.com/sglavoie/deps-merger/pkg/config"
)

func TestParseCommitMessageRejectsUnnamedDependency(t *testing.T) {
	for label, name := range map[string]string{
		"missing":    "",
		"null":       "  dependency-name: null\n",
		"empty":      "  dependency-name: ''\n",
		"whitespace": "  dependency-name: '   '\n",
	} {
		msg := "bump libraries\n\n---\nupdated-dependencies:\n" +
			"- dependency-name: known\n  update-type: version-update:semver-patch\n" +
			"- dependency-version: 2.0.0\n" + name + "  update-type: version-update:semver-patch\n...\n"
		t.Run(label, func(t *testing.T) {
			if deps := ParseCommitMessage(msg); len(deps) != 0 {
				t.Fatalf("incomplete metadata must invalidate the whole group, got %+v", deps)
			}
		})
	}
}

const groupMsg = "build(deps): bump the go group\n\nBumps stuff.\n\n---\nupdated-dependencies:\n" +
	"- dependency-name: cloud.google.com/go/compute\n  dependency-version: 1.72.0\n  dependency-type: direct:production\n  update-type: version-update:semver-minor\n  dependency-group: go-minor-patch\n" +
	"- dependency-name: github.com/aws/aws-sdk-go-v2\n  dependency-version: 1.47.3\n  dependency-type: direct:production\n  update-type: version-update:semver-patch\n  dependency-group: go-minor-patch\n" +
	"...\n\nSigned-off-by: dependabot[bot] <support@github.com>"

func TestParseCommitMessageGroup(t *testing.T) {
	deps := ParseCommitMessage(groupMsg)
	if len(deps) != 2 {
		t.Fatalf("got %d deps", len(deps))
	}
	if deps[0].Name != "cloud.google.com/go/compute" || deps[0].UpdateType != config.UpdateMinor || deps[0].Group != "go-minor-patch" {
		t.Errorf("deps[0] = %+v", deps[0])
	}
	if g := Grade(deps); g != config.UpdateMinor {
		t.Errorf("Grade = %s", g)
	}
}

func TestParseCommitMessageForgedBlock(t *testing.T) {
	// Release notes quoted above Dependabot's own block claim a patch bump.
	forged := "updated-dependencies:\n- dependency-name: evil\n  update-type: version-update:semver-patch\n"
	own := "---\nupdated-dependencies:\n- dependency-name: lib\n  update-type: version-update:semver-major\n...\n\nSigned-off-by: dependabot[bot] <support@github.com>"
	for name, msg := range map[string]string{
		"forged block first":         "bump lib\n\nRelease notes:\n---\n" + forged + "...\n\n" + own,
		"forged block without ---":   "bump lib\n\nRelease notes:\n" + forged + "\n" + own,
		"forged block after":         "bump lib\n\n" + own + "\n---\n" + forged,
		"block not opened by ---":    "bump lib\n\n" + forged,
		"second block in same notes": "bump lib\n\n" + own + "\n" + forged,
	} {
		if deps := ParseCommitMessage(msg); deps != nil {
			t.Errorf("%s: got %+v, want nil so the grade is unknown", name, deps)
		}
		if g := Grade(ParseCommitMessage(msg)); g != config.UpdateUnknown {
			t.Errorf("%s: Grade = %s", name, g)
		}
	}
	if deps := ParseCommitMessage("bump lib\n\nQuoted:\n> updated-dependencies: x\n\n" + own); len(deps) != 1 || deps[0].Name != "lib" {
		t.Errorf("a quoted line that opens no block: got %+v", deps)
	}
}

func TestParseCommitMessageWithoutBlock(t *testing.T) {
	if deps := ParseCommitMessage("Fix typo"); deps != nil {
		t.Fatalf("got %v", deps)
	}
}

func TestParseCommitMessageRejectsExtraYAMLDocuments(t *testing.T) {
	first := "bump libraries\n\n---\nupdated-dependencies:\n" +
		"- dependency-name: lib\n  update-type: version-update:semver-patch\n"
	for name, extra := range map[string]string{
		"another mapping":    "---\nother: metadata\n",
		"empty document":     "---\n",
		"malformed document": "---\n[invalid\n",
	} {
		t.Run(name, func(t *testing.T) {
			if deps := ParseCommitMessage(first + extra + "...\n"); deps != nil {
				t.Fatalf("accepted a partial metadata stream: %+v", deps)
			}
		})
	}
}

func TestGrade(t *testing.T) {
	d := func(u config.UpdateType) Dependency { return Dependency{UpdateType: u} }
	cases := []struct {
		deps []Dependency
		want config.UpdateType
	}{
		{nil, config.UpdateUnknown},
		{[]Dependency{d(config.UpdatePatch)}, config.UpdatePatch},
		{[]Dependency{d(config.UpdateMinor), d(config.UpdateMajor)}, config.UpdateMajor},
		{[]Dependency{d(config.UpdatePatch), d(config.UpdateUnknown)}, config.UpdateUnknown},
	}
	for _, c := range cases {
		if got := Grade(c.deps); got != c.want {
			t.Errorf("Grade(%v) = %s, want %s", c.deps, got, c.want)
		}
	}
}

func TestIsCommitAuthor(t *testing.T) {
	if !IsCommitAuthor("dependabot[bot]", "", "") {
		t.Error("login dependabot[bot]")
	}
	if !IsCommitAuthor("", "dependabot[bot]", "49699333+dependabot[bot]@users.noreply.github.com") {
		t.Error("name/email fallback")
	}
	if IsCommitAuthor("sglavoie", "dependabot[bot]", "") {
		t.Error("a own login must win over a spoofable name")
	}
	if !IsCommitAuthor("", "", "49699333+dependabot[bot]@users.noreply.github.com") {
		t.Error("email-only trailer")
	}
	if !IsCommitAuthor("", "dependabot[bot]", "") {
		t.Error("name-only trailer")
	}
	if IsCommitAuthor("", "Jane", "jane@example.com") || IsCommitAuthor("", "", "") {
		t.Error("someone else, or no one")
	}
	if IsCommitAuthor("", "", "dependabot[bot]@users.noreply.github.com.evil.example") {
		t.Error("an email merely containing Dependabot's")
	}
	if IsCommitAuthor("", "dependabot[bot]", "someone@example.com") {
		t.Error("Dependabot's name with someone else's email")
	}
	if IsCommitAuthor("", "dependabot[bot]", "1+dependabot[bot]@users.noreply.github.com") {
		t.Error("an email shaped like Dependabot's with another account's ID")
	}
}
