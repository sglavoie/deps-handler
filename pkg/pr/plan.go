package pr

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/sglavoie/deps-handler/pkg/github"
)

// MaxPlanBytes bounds a saved handoff passed to the agent.
const MaxPlanBytes = 1 << 20

// RepairPlan preserves a saved diagnosis, including any user edits. Metadata
// is optional when the caller explicitly supplies a target PR.
type RepairPlan struct {
	Source  string          `json:"source,omitempty"`
	Text    string          `json:"text"`
	Target  github.PRTarget `json:"target"`
	HeadOID string          `json:"head_oid,omitempty"`
	BaseOID string          `json:"base_oid,omitempty"`
	BaseRef string          `json:"base_ref,omitempty"`
}

// LoadPlan reads Markdown without modifying it or executing its contents.
func LoadPlan(path string) (*RepairPlan, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("read repair plan: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("repair plan must be a regular Markdown file")
	}
	if info.Size() > MaxPlanBytes {
		return nil, fmt.Errorf("repair plan exceeds %d bytes", MaxPlanBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read repair plan: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, MaxPlanBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read repair plan: %w", err)
	}
	plan, err := ParsePlan(data)
	if err != nil {
		return nil, fmt.Errorf("repair plan %q: %w", path, err)
	}
	plan.Source = path
	return plan, nil
}

var planHead = regexp.MustCompile("^Head: `([^`]+)`$")
var planBase = regexp.MustCompile("^Base: `([^`]+)` \\(`([^`]+)`\\)$")

// ParsePlan recognizes the preamble of a diagnose Markdown report. Free-form
// Markdown is also accepted, but cannot select a PR without metadata.
func ParsePlan(data []byte) (*RepairPlan, error) {
	if len(data) > MaxPlanBytes {
		return nil, fmt.Errorf("repair plan exceeds %d bytes", MaxPlanBytes)
	}
	if !utf8.Valid(data) || strings.ContainsRune(string(data), 0) {
		return nil, errors.New("repair plan must be UTF-8 text without NUL bytes")
	}
	text := string(data)
	if strings.TrimSpace(strings.TrimPrefix(text, "\ufeff")) == "" {
		return nil, errors.New("repair plan is empty")
	}
	plan := &RepairPlan{Text: text}
	for _, line := range strings.Split(strings.TrimPrefix(text, "\ufeff"), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "## ") {
			break
		}
		var reference string
		switch {
		case strings.HasPrefix(line, "# "):
			candidate, _, _ := strings.Cut(strings.TrimPrefix(line, "# "), ":")
			if _, err := github.ParsePRTarget(candidate); err == nil {
				reference = candidate
			}
		case strings.HasPrefix(line, "PR: "):
			reference = strings.TrimSpace(strings.TrimPrefix(line, "PR: "))
		case strings.HasPrefix(line, "Head: "):
			match := planHead.FindStringSubmatch(line)
			if match == nil {
				return nil, errors.New("invalid Head metadata in repair plan")
			}
			if plan.HeadOID != "" && plan.HeadOID != match[1] {
				return nil, errors.New("conflicting Head metadata in repair plan")
			}
			plan.HeadOID = match[1]
		case strings.HasPrefix(line, "Base: "):
			match := planBase.FindStringSubmatch(line)
			if match == nil {
				return nil, errors.New("invalid Base metadata in repair plan")
			}
			if plan.BaseOID != "" && (plan.BaseOID != match[2] || plan.BaseRef != match[1]) {
				return nil, errors.New("conflicting Base metadata in repair plan")
			}
			plan.BaseRef, plan.BaseOID = match[1], match[2]
		}
		if reference == "" {
			continue
		}
		target, err := github.ParsePRTarget(reference)
		if err != nil {
			return nil, fmt.Errorf("invalid PR metadata: %w", err)
		}
		if err := plan.ValidateTarget(target); err != nil {
			return nil, err
		}
		plan.Target = target
	}
	return plan, nil
}

// ValidateTarget rejects using one PR's report to authorize updates to another.
func (p *RepairPlan) ValidateTarget(target github.PRTarget) error {
	if p.Target.Number != 0 && (p.Target.Number != target.Number || !strings.EqualFold(p.Target.Owner, target.Owner) || !strings.EqualFold(p.Target.Repo, target.Repo)) {
		return fmt.Errorf("repair plan targets %s, not %s", p.Target, target)
	}
	return nil
}

// MatchesSnapshot reports whether the saved evidence still identifies the
// current code. A stale plan or one without commit metadata gets fresh
// evidence without discarding the saved instructions.
func (p *RepairPlan) MatchesSnapshot(snapshot github.PRSnapshot) bool {
	return p.HeadOID != "" && p.HeadOID == snapshot.HeadOID &&
		p.BaseOID != "" && p.BaseOID == snapshot.BaseOID &&
		p.BaseRef != "" && p.BaseRef == snapshot.BaseRef
}
