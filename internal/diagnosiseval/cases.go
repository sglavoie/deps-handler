// Package diagnosiseval replays frozen PR evidence through the production
// diagnosis service. Only the selected model backend needs network access.
package diagnosiseval

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"strings"

	"github.com/sglavoie/deps-handler/pkg/github"
	"github.com/sglavoie/deps-handler/pkg/pr"
)

// all: includes .github workflows in the frozen checkouts. testdata prevents
// Go from treating the upstream source excerpts as packages in this module.
//
//go:embed all:testdata/cases
var fixtures embed.FS

// Case keeps grading data separate from evidence and checkout files. Neither
// expectations nor provenance notes are passed to the diagnosis agent.
type Case struct {
	ID           string       `json:"id"`
	Description  string       `json:"description"`
	Provenance   Provenance   `json:"provenance"`
	Snapshot     snapshot     `json:"snapshot"`
	Evidence     evidence     `json:"evidence"`
	Expectations Expectations `json:"expectations"`
}

type Provenance struct {
	SourceReport string   `json:"source_report"`
	CapturedAt   string   `json:"captured_at"`
	Notes        []string `json:"notes"`
}

// Checks use an explicit wire type: github.Check deliberately omits producer
// identities from normal JSON unmarshalling. Losing those would change the
// base comparisons during replay.
type check struct {
	Name          string            `json:"name"`
	State         github.CheckState `json:"state"`
	App           int64             `json:"app_id,omitempty"`
	Workflow      int64             `json:"workflow_id,omitempty"`
	Suite         int64             `json:"suite_id,omitempty"`
	Event         string            `json:"event,omitempty"`
	Status        bool              `json:"status_context"`
	ID            int64             `json:"id,omitempty"`
	JobID         int64             `json:"job_id,omitempty"`
	RunID         int64             `json:"run_id,omitempty"`
	RunAttempt    int               `json:"run_attempt,omitempty"`
	URL           string            `json:"url,omitempty"`
	Description   string            `json:"description,omitempty"`
	Log           string            `json:"log,omitempty"`
	LogFile       string            `json:"log_file,omitempty"`
	EvidenceError string            `json:"evidence_error,omitempty"`
}

func checks(records []check) []github.PRCheck {
	out := make([]github.PRCheck, 0, len(records))
	for _, c := range records {
		out = append(out, github.PRCheck{
			Check: github.Check{Name: c.Name, State: c.State, App: c.App, Workflow: c.Workflow, Suite: c.Suite, Event: c.Event, Status: c.Status},
			ID:    c.ID, JobID: c.JobID, RunID: c.RunID, RunAttempt: c.RunAttempt,
			URL: c.URL, Description: c.Description, Log: c.Log, EvidenceError: c.EvidenceError,
		})
	}
	return out
}

type snapshot struct {
	github.PRSnapshot
	Checks []check `json:"checks"`
}

func (s snapshot) value() github.PRSnapshot {
	p := s.PRSnapshot
	p.Checks = checks(s.Checks)
	return p
}

type evidence struct {
	Details *github.PullRequestDetails `json:"details"`
	Checks  []check                    `json:"checks"`
	Base    struct {
		OID            string  `json:"oid"`
		ChecksComplete bool    `json:"checks_complete"`
		Checks         []check `json:"checks"`
		EvidenceError  string  `json:"evidence_error,omitempty"`
	} `json:"base"`
	Artifacts []github.RunArtifacts `json:"artifacts"`
}

func (e evidence) value() github.PREvidence {
	return github.PREvidence{Details: e.Details, Checks: checks(e.Checks), Artifacts: e.Artifacts,
		Base: github.BaseChecks{OID: e.Base.OID, ChecksComplete: e.Base.ChecksComplete, Checks: checks(e.Base.Checks), EvidenceError: e.Base.EvidenceError}}
}

type Expectations struct {
	Recommendations []ExpectedAction `json:"recommendations"`
	Assertions      []Assertion      `json:"assertions"`
}

type ExpectedAction struct {
	Subject string                    `json:"subject"`
	Actions []pr.RecommendationAction `json:"actions"`
}

// Assertion is a case-insensitive substring check. Subject limits its scope to
// that recommendation's reason and next step; otherwise it checks the entire
// raw analysis. These are evidence-reference checks, not semantic judgments.
type Assertion struct {
	ID      string   `json:"id"`
	Subject string   `json:"subject,omitempty"`
	AnyOf   []string `json:"any_of"`
}

// Load reads and validates the embedded, versioned benchmark in stable order.
func Load() ([]Case, error) {
	entries, err := fs.ReadDir(fixtures, "testdata/cases")
	if err != nil {
		return nil, err
	}
	var cases []Case
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		data, err := fixtures.ReadFile("testdata/cases/" + entry.Name() + "/case.json")
		if err != nil {
			return nil, err
		}
		var c Case
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return nil, fmt.Errorf("case %s: %w", entry.Name(), err)
		}
		if err := dec.Decode(new(any)); err != io.EOF {
			return nil, fmt.Errorf("case %s: expected one JSON object", entry.Name())
		}
		if c.ID != entry.Name() {
			return nil, fmt.Errorf("case %s: directory and ID differ", entry.Name())
		}
		for _, group := range [][]check{c.Evidence.Checks, c.Evidence.Base.Checks} {
			for i := range group {
				if group[i].LogFile == "" {
					continue
				}
				name := group[i].LogFile
				if !fs.ValidPath(name) || !strings.HasPrefix(name, "logs/") || group[i].Log != "" {
					return nil, fmt.Errorf("case %s: invalid log file %q", c.ID, name)
				}
				data, err := fixtures.ReadFile("testdata/cases/" + c.ID + "/" + name)
				if err != nil {
					return nil, err
				}
				group[i].Log = string(data)
			}
		}
		if err := c.validate(); err != nil {
			return nil, fmt.Errorf("case %s: %w", c.ID, err)
		}
		cases = append(cases, c)
	}
	if len(cases) == 0 {
		return nil, fmt.Errorf("no evaluation cases")
	}
	return cases, nil
}

func (c Case) validate() error {
	p := c.Snapshot.value()
	if _, err := github.ParsePRTarget(p.Target.String()); err != nil {
		return err
	}
	if c.Description == "" || c.Provenance.SourceReport == "" || c.Provenance.CapturedAt == "" || p.HeadOID == "" || p.BaseOID == "" || c.Evidence.Details == nil || c.Evidence.Base.OID != p.BaseOID {
		return fmt.Errorf("missing description, provenance or pinned evidence")
	}
	seen := map[string]bool{}
	for _, want := range c.Expectations.Recommendations {
		if strings.TrimSpace(want.Subject) == "" || seen[want.Subject] || len(want.Actions) == 0 {
			return fmt.Errorf("missing or repeated expectation subject %q", want.Subject)
		}
		seen[want.Subject] = true
		for _, action := range want.Actions {
			switch action {
			case pr.CodeRepair, pr.RerunCandidate, pr.HumanAction, pr.InsufficientEvidence:
			default:
				return fmt.Errorf("unknown expected action %q", action)
			}
		}
	}
	if len(seen) == 0 {
		return fmt.Errorf("no expected actions")
	}
	for _, check := range p.Checks {
		if check.State == github.CheckFailed && !seen[check.Name] {
			return fmt.Errorf("failed check %q has no expected action", check.Name)
		}
	}
	assertions := map[string]bool{}
	for _, a := range c.Expectations.Assertions {
		if a.ID == "" || assertions[a.ID] || len(a.AnyOf) == 0 || (a.Subject != "" && !seen[a.Subject]) {
			return fmt.Errorf("invalid assertion %q", a.ID)
		}
		assertions[a.ID] = true
		for _, text := range a.AnyOf {
			if strings.TrimSpace(text) == "" {
				return fmt.Errorf("assertion %q contains an empty alternative", a.ID)
			}
		}
	}
	_, err := fs.Stat(fixtures, "testdata/cases/"+c.ID+"/checkout")
	return err
}
