package diagnosiseval

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/sglavoie/deps-handler/pkg/pr"
)

type Finding struct {
	Kind    string `json:"kind"`
	Subject string `json:"subject,omitempty"`
	Message string `json:"message"`
}

type Score struct {
	ExpectedActions           int `json:"expected_actions"`
	CorrectActions            int `json:"correct_actions"`
	MissedBlockers            int `json:"missed_blockers"`
	UnsupportedCodeRepairs    int `json:"unsupported_code_repairs"`
	UnexpectedRecommendations int `json:"unexpected_recommendations"`
	EvidenceChecks            int `json:"evidence_checks"`
	FailedEvidenceChecks      int `json:"failed_evidence_checks"`
}

// Grade uses the agent's raw analysis, before the production service fills in
// unclassified failures. Otherwise an omitted blocker could receive credit for
// an insufficient_evidence recommendation supplied by the service itself.
func Grade(want Expectations, analysis pr.Analysis) (Score, []Finding) {
	score := Score{ExpectedActions: len(want.Recommendations), EvidenceChecks: len(want.Assertions)}
	findings := []Finding{}
	bySubject := map[string][]pr.Recommendation{}
	expected := map[string]ExpectedAction{}
	for _, item := range want.Recommendations {
		expected[item.Subject] = item
	}
	for _, r := range analysis.Recommendations {
		bySubject[r.Subject] = append(bySubject[r.Subject], r)
		e, known := expected[r.Subject]
		if !known {
			score.UnexpectedRecommendations++
			findings = append(findings, Finding{"unexpected_recommendation", r.Subject, "No outstanding blocker with this exact subject is expected"})
		}
		if r.Action == pr.CodeRepair && (!known || !slices.Contains(e.Actions, pr.CodeRepair)) {
			score.UnsupportedCodeRepairs++
			findings = append(findings, Finding{"unsupported_code_repair", r.Subject, "The frozen evidence does not support a code repair for this subject"})
		}
	}
	for _, item := range want.Recommendations {
		got := bySubject[item.Subject]
		switch {
		case len(got) == 0:
			score.MissedBlockers++
			findings = append(findings, Finding{"missed_blocker", item.Subject, "The agent omitted this blocker"})
		case len(got) != 1:
			findings = append(findings, Finding{"duplicate_subject", item.Subject, "Expected one recommendation for this subject"})
		case !slices.Contains(item.Actions, got[0].Action):
			findings = append(findings, Finding{"incorrect_action", item.Subject, fmt.Sprintf("Got %s; acceptable actions: %v", got[0].Action, item.Actions)})
		default:
			score.CorrectActions++
		}
	}
	data, _ := json.Marshal(analysis)
	for _, assertion := range want.Assertions {
		text := string(data)
		if assertion.Subject != "" {
			text = ""
			for _, r := range bySubject[assertion.Subject] {
				text += r.Reason + "\n" + r.NextStep + "\n"
			}
		}
		if !slices.ContainsFunc(assertion.AnyOf, func(term string) bool {
			return strings.Contains(strings.ToLower(text), strings.ToLower(term))
		}) {
			score.FailedEvidenceChecks++
			findings = append(findings, Finding{"missing_evidence_reference", assertion.Subject, assertion.ID})
		}
	}
	return score, findings
}
