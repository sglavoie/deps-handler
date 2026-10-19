// Package dependabot reads the metadata Dependabot embeds in its commit
// messages and recognises Dependabot as an author.
package dependabot

import (
	"strings"

	"github.com/sglavoie/deps-merger/internal/config"
	"gopkg.in/yaml.v3"
)

// Dependency is one entry of a commit's "updated-dependencies" block.
type Dependency struct {
	Name       string            `json:"name"`
	Version    string            `json:"version,omitempty"`
	Type       string            `json:"type,omitempty"`
	Group      string            `json:"group,omitempty"`
	UpdateType config.UpdateType `json:"update_type"`
}

type rawDependency struct {
	Name       string `yaml:"dependency-name"`
	Version    string `yaml:"dependency-version"`
	Type       string `yaml:"dependency-type"`
	Group      string `yaml:"dependency-group"`
	UpdateType string `yaml:"update-type"`
}

// ParseCommitMessage extracts the dependencies listed after the "---" line
// of a Dependabot commit message. It returns nil when there is no block.
func ParseCommitMessage(msg string) []Dependency {
	msg = strings.ReplaceAll(msg, "\r\n", "\n")
	start := strings.Index(msg, "\nupdated-dependencies:")
	if start < 0 {
		return nil
	}
	block := msg[start+1:]
	if end := strings.Index(block, "\n...\n"); end >= 0 {
		block = block[:end]
	} else {
		block = strings.TrimSuffix(block, "\n...")
	}
	var doc struct {
		Deps []rawDependency `yaml:"updated-dependencies"`
	}
	if err := yaml.Unmarshal([]byte(block), &doc); err != nil {
		return nil
	}
	out := make([]Dependency, 0, len(doc.Deps))
	for _, d := range doc.Deps {
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

// IsCommitAuthor reports whether a commit author identity is Dependabot. A
// linked login decides; without one, the name or email Dependabot uses is
// accepted, which only suits a Co-authored-by trailer: such trailers link
// no account. A commit's own author must be linked to Dependabot's account
// (see IsLogin), since a name or email alone is anyone's to type.
func IsCommitAuthor(login, name, email string) bool {
	if login != "" {
		return IsLogin(login)
	}
	return name == "dependabot[bot]" || strings.HasSuffix(email, "+dependabot[bot]@users.noreply.github.com")
}
