// Package dependabot reads the metadata Dependabot embeds in its commit
// messages and recognises Dependabot as an author.
package dependabot

import (
	"errors"
	"io"
	"strings"

	"github.com/sglavoie/deps-merger/pkg/config"
	"gopkg.in/yaml.v3"
)

// Dependency is one entry of a commit's "updated-dependencies" block.
type Dependency struct {
	Name       string            `json:"name"`
	Version    string            `json:"version"`
	Type       string            `json:"type"`
	Group      string            `json:"group"`
	UpdateType config.UpdateType `json:"update_type"`
}

type rawDependency struct {
	Name       string `yaml:"dependency-name"`
	Version    string `yaml:"dependency-version"`
	Type       string `yaml:"dependency-type"`
	Group      string `yaml:"dependency-group"`
	UpdateType string `yaml:"update-type"`
}

// metadataStart opens the block Dependabot appends to its commit messages,
// after the release notes and changelog it quotes.
const metadataStart = "\n---\nupdated-dependencies:"

// ParseCommitMessage extracts the dependencies listed after the "---" line
// of a Dependabot commit message. It returns nil when there is no block, and
// also when there is more than one: the release notes Dependabot quotes
// above its own block are written by the dependency's authors, who can
// forge one there, and nil grades the update unknown rather than trusting
// either.
func ParseCommitMessage(msg string) []Dependency {
	msg = strings.ReplaceAll(msg, "\r\n", "\n")
	// Any line opening a block is a candidate, whether or not a "---"
	// line introduces it, so a forgery cannot slip past by omitting it.
	if strings.Count(msg, "\nupdated-dependencies:") != 1 {
		return nil
	}
	start := strings.Index(msg, metadataStart)
	if start < 0 {
		return nil
	}
	block := msg[start+len("\n---\n"):]
	if end := strings.Index(block, "\n...\n"); end >= 0 {
		block = block[:end]
	} else {
		block = strings.TrimSuffix(block, "\n...")
	}
	var doc struct {
		Deps []rawDependency `yaml:"updated-dependencies"`
	}
	dec := yaml.NewDecoder(strings.NewReader(block))
	if err := dec.Decode(&doc); err != nil {
		return nil
	}
	// Unmarshal reads only the first YAML document. Never grade a valid
	// prefix while ignoring extra or malformed metadata after it.
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil
	}
	out := make([]Dependency, 0, len(doc.Deps))
	for _, d := range doc.Deps {
		// An unnamed entry cannot establish what was updated. Invalidate
		// the whole block instead of grading just its readable members.
		if strings.TrimSpace(d.Name) == "" {
			return nil
		}
		out = append(out, Dependency{
			Name:       d.Name,
			Version:    d.Version,
			Type:       d.Type,
			Group:      d.Group,
			UpdateType: parseUpdateType(d.UpdateType),
		})
	}
	return out
}

func parseUpdateType(s string) config.UpdateType {
	switch strings.TrimSpace(s) {
	case "version-update:semver-patch":
		return config.UpdatePatch
	case "version-update:semver-minor":
		return config.UpdateMinor
	case "version-update:semver-major":
		return config.UpdateMajor
	}
	return config.UpdateUnknown
}

// Grade is the riskiest bump among deps. Any dependency whose bump size is
// unknown makes the whole grade unknown, so it can never pass a limit.
func Grade(deps []Dependency) config.UpdateType {
	if len(deps) == 0 {
		return config.UpdateUnknown
	}
	g := config.UpdatePatch
	for _, d := range deps {
		if d.UpdateType == config.UpdateUnknown {
			return config.UpdateUnknown
		}
		if d.UpdateType > g {
			g = d.UpdateType
		}
	}
	return g
}

// IsLogin reports whether a GitHub login is Dependabot. GraphQL reports the
// app as "dependabot", REST and commit authors as "dependabot[bot]".
func IsLogin(login string) bool {
	return login == "dependabot" || login == "dependabot[bot]"
}

// Email is the address Dependabot's commits carry, which GitHub links to
// its account.
const Email = "49699333+dependabot[bot]@users.noreply.github.com"

// IsCommitAuthor reports whether a commit author identity is Dependabot. A
// linked login decides; without one, Dependabot's email, or its name with
// no email, is accepted, which only suits a Co-authored-by trailer: such
// trailers link no login. GitHub credits a trailer to whoever owns its
// email, so a trailer with Dependabot's name and someone else's email, or
// with an email merely shaped like Dependabot's, names someone else. A
// commit's own author must be linked to Dependabot's account (see
// IsLogin), since a name or email alone is anyone's to type.
func IsCommitAuthor(login, name, email string) bool {
	if login != "" {
		return IsLogin(login)
	}
	if email == "" {
		return name == "dependabot[bot]"
	}
	return strings.EqualFold(email, Email)
}
