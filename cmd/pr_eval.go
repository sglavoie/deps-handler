package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/sglavoie/deps-handler/internal/diagnosiseval"
	"github.com/sglavoie/deps-handler/pkg/config"
	"github.com/sglavoie/deps-handler/pkg/review"
	"github.com/spf13/cobra"
)

func newPREvalCmd() *cobra.Command {
	var agentName string
	var asJSON, list bool
	var timeout time.Duration
	c := &cobra.Command{
		Use: "eval [case ...]", Short: "Evaluate diagnosis against frozen real-world PR cases",
		Long: `Replay frozen PR snapshots, logs, base checks, artifacts and source files
through the production diagnosis service, without reading live GitHub state.
Runs all cases unless case IDs are supplied. --list needs no agent or login.

This is an opt-in model evaluation: the configured agent backend may use
network access and incur its normal costs. Expected answers stay outside the
agent's temporary checkout. Cases run serially and are never edited or pushed.

Scores check actions, unsupported code repairs, missed blockers and selected
evidence references. Text-reference assertions are lexical checks, not a proof
of causal reasoning. JSON includes each diagnosis for human review.
Exit 0 means every selected case passed, 1 means a case failed or errored,
2 means invalid usage, and 130 means interrupted.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cases, err := diagnosiseval.Load()
			if err != nil {
				return err
			}
			if len(args) > 0 {
				selected := make([]diagnosiseval.Case, 0, len(args))
				seen := map[string]bool{}
				for _, id := range args {
					found := false
					for _, c := range cases {
						if c.ID == id {
							if seen[id] {
								return usageError{fmt.Errorf("case %q was selected twice", id)}
							}
							seen[id], found = true, true
							selected = append(selected, c)
							break
						}
					}
					if !found {
						return usageError{fmt.Errorf("unknown case %q; use pr eval --list", id)}
					}
				}
				cases = selected
			}
			if cmd.Flags().Changed("timeout") && timeout <= 0 {
				return usageError{errors.New("--timeout must be positive")}
			}
			name := strings.ToLower(strings.TrimSpace(agentName))
			if cmd.Flags().Changed("agent") && !config.ValidAgent(name) {
				return usageError{fmt.Errorf("--agent needs %s", config.AgentList())}
			}
			if list {
				type entry struct {
					ID          string `json:"id"`
					Description string `json:"description"`
				}
				entries := []entry{}
				for _, c := range cases {
					entries = append(entries, entry{c.ID, c.Description})
				}
				if asJSON {
					return json.NewEncoder(cmd.OutOrStdout()).Encode(entries)
				}
				for _, entry := range entries {
					if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", entry.ID, entry.Description); err != nil {
						return err
					}
				}
				return nil
			}
			if !prCommandsAvailable {
				return errors.New("pr commands are unavailable in the demo build")
			}
			cfg, err := loadPRConfig()
			if err != nil {
				return err
			}
			if name == "" {
				name = cfg.Use
			}
			if !cmd.Flags().Changed("timeout") {
				timeout = cfg.Timeout
			}
			agent, err := newTaskAgent(cfg, name)
			if err != nil {
				return err
			}
			model := cfg.Claude.Model
			if name == "codex" {
				model = cfg.Codex.Model
			} else if name == "api" {
				model = cfg.API.Model
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
			defer stop()
			report := diagnosiseval.Run(ctx, agent, cases, diagnosiseval.Options{Timeout: timeout, Model: model, Version: versionString(),
				Progress: func(message string) { fmt.Fprintln(cmd.ErrOrStderr(), message) }})
			if asJSON {
				data, err := json.MarshalIndent(report, "", "  ")
				if err != nil {
					return err
				}
				if _, err := fmt.Fprintln(cmd.OutOrStdout(), review.Clean(string(data))); err != nil {
					return err
				}
			} else {
				if err := writeEvalReport(cmd, report); err != nil {
					return err
				}
			}
			if ctx.Err() != nil {
				return errInterrupted
			}
			if !report.Passed() {
				return exitError{code: 1}
			}
			return nil
		},
	}
	c.Flags().StringVar(&agentName, "agent", "", "agent: "+config.AgentList()+" (default from config, otherwise claude)")
	c.Flags().BoolVar(&asJSON, "json", false, "print scores and full diagnoses as JSON")
	c.Flags().BoolVar(&list, "list", false, "list frozen cases without running an agent")
	c.Flags().DurationVar(&timeout, "timeout", 0, "maximum time per case (default agent timeout from config)")
	return c
}

func writeEvalReport(cmd *cobra.Command, report diagnosiseval.Report) error {
	var b strings.Builder
	for _, c := range report.Results {
		status := "FAIL"
		if c.Passed {
			status = "PASS"
		}
		fmt.Fprintf(&b, "%s %s: %d/%d actions, %d unsupported code repairs, %d missed blockers\n", status, c.ID,
			c.Score.CorrectActions, c.Score.ExpectedActions, c.Score.UnsupportedCodeRepairs, c.Score.MissedBlockers)
		if c.Error != "" {
			fmt.Fprintf(&b, "  error: %s\n", c.Error)
		}
		for _, finding := range c.Findings {
			fmt.Fprintf(&b, "  %s [%s]: %s\n", finding.Kind, finding.Subject, finding.Message)
		}
	}
	s := report.Summary
	fmt.Fprintf(&b, "\n%d/%d cases passed; %d/%d actions correct; %d unsupported code repairs; %d missed blockers; %d/%d evidence checks failed; %d errors\n",
		s.PassedCases, len(report.Selected), s.CorrectActions, s.ExpectedActions, s.UnsupportedCodeRepairs, s.MissedBlockers, s.FailedEvidenceChecks, s.EvidenceChecks, s.Errors)
	if report.Cancelled {
		b.WriteString("Evaluation interrupted; remaining cases were not run.\n")
	}
	_, err := fmt.Fprint(cmd.OutOrStdout(), review.Clean(b.String()))
	return err
}
