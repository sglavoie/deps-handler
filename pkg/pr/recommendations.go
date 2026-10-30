package pr

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/sglavoie/deps-handler/pkg/github"
	"github.com/sglavoie/deps-handler/pkg/review"
)

// RecommendationAction describes the kind of work supported by the evidence.
// It never authorizes a CI rerun, account change or other external action.
type RecommendationAction string

const (
	CodeRepair           RecommendationAction = "code_repair"
	RerunCandidate       RecommendationAction = "rerun_candidate"
	HumanAction          RecommendationAction = "human_action"
	InsufficientEvidence RecommendationAction = "insufficient_evidence"
)

// Recommendation keeps each failure's next action separate in mixed cases.
// Subject names the check, merge conflict or other problem being assessed.
type Recommendation struct {
	Subject  string               `json:"subject"`
	Action   RecommendationAction `json:"action"`
	Reason   string               `json:"reason"`
	NextStep string               `json:"next_step"`
}

func (action RecommendationAction) label() string {
	switch action {
	case CodeRepair:
		return "Code repair"
	case RerunCandidate:
		return "Rerun candidate"
	case HumanAction:
		return "Human action"
	default:
		return "Insufficient evidence"
	}
}

func (a Analysis) supportsCodeRepair() bool {
	for _, r := range a.Recommendations {
		if r.Action == CodeRepair {
			return true
		}
	}
	return false
}

// completeRecommendations preserves uncertainty when the agent omits a check.
// An empty assessment must never turn a failed or missing CI result into an
// implicit authorization to edit. Merge conflicts are independently observed.
func (a *Analysis) completeRecommendations(p github.PRSnapshot) {
	covered := make(map[string]bool)
	for _, r := range a.Recommendations {
		covered[r.Subject] = true
	}
	allSkipped := true
	for _, c := range p.Checks {
		allSkipped = allSkipped && c.State == github.CheckSkipped
		if c.State == github.CheckPassed || c.State == github.CheckSkipped || covered[c.Name] {
			continue
		}
		a.Recommendations = append(a.Recommendations, Recommendation{c.Name, InsufficientEvidence,
			"No supported next action was identified for this check.", "Inspect the current check and its failure evidence before editing code."})
		covered[c.Name] = true
	}
	if allSkipped || !p.ChecksComplete {
		a.Recommendations = append(a.Recommendations, Recommendation{"CI evidence", InsufficientEvidence,
			"Checks are absent, all skipped, or the check list is incomplete.", "Obtain a complete CI result; missing checks do not identify a code repair."})
	}
	if needsBaseRepair(p) && !covered["base branch"] {
		a.Recommendations = append(a.Recommendations, Recommendation{"base branch", CodeRepair,
			"GitHub reports merge conflicts or a branch behind its base.", "Update the PR branch from its base, resolve any conflicts and validate the result."})
	}
}

// analysisObject rejects duplicate, unknown, case-varied and missing keys.
// Structured-output schemas are hints to agents, not runtime validation.
func analysisObject(data []byte, keys ...string) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	if token, err := dec.Token(); err != nil || token != json.Delim('{') {
		return nil, errors.New("expected a JSON object")
	}
	allowed := make(map[string]bool, len(keys))
	for _, key := range keys {
		allowed[key] = true
	}
	fields := make(map[string]json.RawMessage, len(keys))
	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok || !allowed[key] || fields[key] != nil {
			return nil, fmt.Errorf("unknown or duplicate field %q", token)
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, err
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, fmt.Errorf("%s must not be null", key)
		}
		fields[key] = value
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("expected a single JSON object")
	}
	if len(fields) != len(keys) {
		return nil, fmt.Errorf("required fields: %s", strings.Join(keys, ", "))
	}
	return fields, nil
}

func decodeAnalysis(data []byte) (Analysis, error) {
	fields, err := analysisObject(data, "summary", "causes", "steps", "validation", "remaining", "recommendations")
	if err != nil {
		return Analysis{}, fmt.Errorf("invalid analysis: %w", err)
	}
	var a Analysis
	if err := json.Unmarshal(data, &a); err != nil {
		return Analysis{}, fmt.Errorf("invalid analysis: %w", err)
	}
	if strings.TrimSpace(a.Summary) == "" {
		return Analysis{}, errors.New("analysis requires a non-empty summary")
	}
	for _, field := range []string{"causes", "steps", "validation", "remaining"} {
		var items []json.RawMessage
		if err := json.Unmarshal(fields[field], &items); err != nil {
			return Analysis{}, err
		}
		for _, item := range items {
			if len(item) == 0 || item[0] != '"' {
				return Analysis{}, fmt.Errorf("%s must contain only strings", field)
			}
		}
	}
	var recommendations []json.RawMessage
	if err := json.Unmarshal(fields["recommendations"], &recommendations); err != nil {
		return Analysis{}, err
	}
	for i, raw := range recommendations {
		if _, err := analysisObject(raw, "subject", "action", "reason", "next_step"); err != nil {
			return Analysis{}, fmt.Errorf("invalid recommendation: %w", err)
		}
		r := &a.Recommendations[i]
		switch r.Action {
		case CodeRepair, RerunCandidate, HumanAction, InsufficientEvidence:
		default:
			return Analysis{}, fmt.Errorf("invalid recommendation action %q", r.Action)
		}
		r.Subject, r.Reason, r.NextStep = review.Clean(r.Subject), review.Clean(r.Reason), review.Clean(r.NextStep)
		if strings.TrimSpace(r.Subject) == "" || strings.TrimSpace(r.Reason) == "" || strings.TrimSpace(r.NextStep) == "" {
			return Analysis{}, errors.New("recommendations require non-empty subject, reason and next_step")
		}
	}
	return a, nil
}
