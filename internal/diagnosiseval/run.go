package diagnosiseval

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sglavoie/deps-handler/pkg/github"
	"github.com/sglavoie/deps-handler/pkg/pr"
	"github.com/sglavoie/deps-handler/pkg/review"
)

type CaseResult struct {
	ID            string        `json:"id"`
	Passed        bool          `json:"passed"`
	Duration      time.Duration `json:"duration_ns"`
	Score         Score         `json:"score"`
	Findings      []Finding     `json:"findings"`
	Error         string        `json:"error,omitempty"`
	AgentAnalysis *pr.Analysis  `json:"agent_analysis,omitempty"`
	Diagnosis     pr.Result     `json:"diagnosis"`
}

type Summary struct {
	Cases       int `json:"cases"`
	PassedCases int `json:"passed_cases"`
	Errors      int `json:"errors"`
	Score
}

type Report struct {
	Agent       string       `json:"agent"`
	Model       string       `json:"model"`
	Version     string       `json:"version"`
	SuiteSHA256 string       `json:"suite_sha256"`
	StartedAt   time.Time    `json:"started_at"`
	Selected    []string     `json:"selected"`
	Cancelled   bool         `json:"cancelled"`
	Summary     Summary      `json:"summary"`
	Results     []CaseResult `json:"results"`
}

func (r Report) Passed() bool {
	return !r.Cancelled && len(r.Selected) > 0 && r.Summary.PassedCases == len(r.Selected)
}

type Options struct {
	Timeout  time.Duration
	Model    string
	Version  string
	Progress func(string)
}

// Run processes cases serially using the actual pr.Service.Diagnose prompt,
// evidence comparison and output validation. Each case gets a fresh temporary
// checkout containing only frozen source files. No GitHub client is created.
func Run(ctx context.Context, agent review.TaskAgent, cases []Case, opts Options) Report {
	report := Report{Agent: agent.Name(), Model: opts.Model, Version: opts.Version, StartedAt: time.Now().UTC(), Selected: []string{}, Results: []CaseResult{}}
	// Fingerprint both evidence and grading rules so reports from different
	// fixture revisions cannot accidentally be compared as the same benchmark.
	hash := sha256.New()
	_ = fs.WalkDir(fixtures, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			data, err := fixtures.ReadFile(name)
			if err != nil {
				return err
			}
			fmt.Fprintf(hash, "%s\x00%d\x00", name, len(data))
			hash.Write(data)
		}
		return nil
	})
	report.SuiteSHA256 = fmt.Sprintf("%x", hash.Sum(nil))
	for _, c := range cases {
		report.Selected = append(report.Selected, c.ID)
	}
	for _, c := range cases {
		if ctx.Err() != nil {
			report.Cancelled = true
			break
		}
		if opts.Progress != nil {
			opts.Progress("Evaluating " + c.ID)
		}
		result := runCase(ctx, agent, c, opts)
		report.Results = append(report.Results, result)
		s := &report.Summary
		s.Cases++
		if result.Passed {
			s.PassedCases++
		}
		if result.Error != "" {
			s.Errors++
		}
		s.ExpectedActions += result.Score.ExpectedActions
		s.CorrectActions += result.Score.CorrectActions
		s.MissedBlockers += result.Score.MissedBlockers
		s.UnsupportedCodeRepairs += result.Score.UnsupportedCodeRepairs
		s.UnexpectedRecommendations += result.Score.UnexpectedRecommendations
		s.EvidenceChecks += result.Score.EvidenceChecks
		s.FailedEvidenceChecks += result.Score.FailedEvidenceChecks
	}
	report.Cancelled = report.Cancelled || ctx.Err() != nil
	return report
}

func runCase(ctx context.Context, agent review.TaskAgent, c Case, opts Options) (result CaseResult) {
	started := time.Now()
	result = CaseResult{ID: c.ID, Findings: []Finding{},
		Score: Score{ExpectedActions: len(c.Expectations.Recommendations), EvidenceChecks: len(c.Expectations.Assertions)}}
	defer func() { result.Duration = time.Since(started) }()
	fail := func(err error) CaseResult {
		result.Error = review.Clean(err.Error())
		return result
	}
	if opts.Timeout <= 0 {
		return fail(fmt.Errorf("evaluation timeout must be positive"))
	}
	dir, err := os.MkdirTemp("", "deps-handler-eval-")
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(dir)
	if err := copyCheckout(c.ID, dir); err != nil {
		return fail(err)
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	recorder := &recordingAgent{TaskAgent: agent}
	service := pr.Service{Client: frozenClient{c}, Workspace: frozenWorkspace{dir}, Agent: recorder, AgentTimeout: opts.Timeout}
	result.Diagnosis, err = service.Diagnose(ctx, c.Snapshot.Target, "")
	if err != nil {
		return fail(err)
	}
	// Diagnose already validated the raw schema; retain it before service-added
	// uncertainty recommendations can hide an omitted blocker from the score.
	var analysis pr.Analysis
	if err := json.Unmarshal(recorder.raw, &analysis); err != nil {
		return fail(err)
	}
	result.AgentAnalysis = &analysis
	result.Score, result.Findings = Grade(c.Expectations, analysis)
	result.Passed = len(result.Findings) == 0
	return result
}

func copyCheckout(id, dest string) error {
	checkout, err := fs.Sub(fixtures, "testdata/cases/"+id+"/checkout")
	if err != nil {
		return err
	}
	return fs.WalkDir(checkout, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// A literal go.mod would make go:embed exclude the entire upstream
		// checkout as a nested module, even with all:. Restore its real name.
		target := filepath.Join(dest, filepath.FromSlash(strings.TrimSuffix(name, ".snapshot")))
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		data, err := fs.ReadFile(checkout, name)
		if err != nil {
			return err
		}
		// Even shell source is copied as data, without an executable bit.
		return os.WriteFile(target, data, 0o600)
	})
}

type frozenClient struct{ c Case }

func (f frozenClient) Snapshot(ctx context.Context, target github.PRTarget) (github.PRSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return github.PRSnapshot{}, err
	}
	if target != f.c.Snapshot.Target {
		return github.PRSnapshot{}, fmt.Errorf("unexpected evaluation target")
	}
	return f.c.Snapshot.value(), nil
}

func (f frozenClient) Evidence(ctx context.Context, _ github.PRSnapshot) (github.PREvidence, error) {
	return f.c.Evidence.value(), ctx.Err()
}

type frozenWorkspace struct{ dir string }

func (w frozenWorkspace) Checkout(context.Context, string, string, int, string) (string, error) {
	return w.dir, nil
}

func (frozenWorkspace) FetchBase(context.Context, string, string) error {
	return fmt.Errorf("evaluation cannot fetch")
}

func (frozenWorkspace) Commit(context.Context, string, string) (string, error) {
	return "", fmt.Errorf("evaluation cannot commit")
}

func (frozenWorkspace) DiffSummary(context.Context, string, string) (string, error) {
	return "", fmt.Errorf("evaluation cannot diff")
}

func (frozenWorkspace) CommitAndPush(context.Context, string, string, string, string, string) (string, error) {
	return "", fmt.Errorf("evaluation cannot push")
}

type recordingAgent struct {
	review.TaskAgent
	raw json.RawMessage
}

func (a *recordingAgent) Task(ctx context.Context, dir, prompt, schema string, writable bool) (json.RawMessage, error) {
	if writable {
		return nil, fmt.Errorf("evaluation cannot run a writable task")
	}
	raw, err := a.TaskAgent.Task(ctx, dir, prompt, schema, false)
	a.raw = raw
	return raw, err
}
