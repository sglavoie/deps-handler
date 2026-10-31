# PR diagnosis evaluation

`deps-handler pr eval` measures a real agent's recommendations against four
versioned cases. It uses `pr.Service.Diagnose`, including the production prompt,
base comparison, artifact handling, response validation and recommendation
completion. A frozen client supplies GitHub responses, and a temporary
workspace supplies selected source files. Replay makes no GitHub requests.

```sh
deps-handler pr eval --list
deps-handler pr eval --agent claude --json > claude-evaluation.json
deps-handler pr eval --agent codex --json > codex-evaluation.json
deps-handler pr eval lyzma muse --agent claude --timeout 5m
```

The normal agent configuration applies, including models, API endpoints and
credentials. No config file is required for Claude or Codex. `--agent api`
uses the configured API backend. `--timeout` bounds each case independently;
otherwise the configured agent timeout applies. Cases run serially. An error
in one case is recorded and the next case runs; interruption stops the suite
and prints the results collected so far.

The model backend can use network access and incur its usual costs. The source
files are partial snapshots, so diagnoses may correctly note that imported
files or runnable dependencies are unavailable. The agent receives a read-only
task. The runner never executes the snapshot's programs, runs a repair, commits,
pushes, reruns CI or merges anything. Temporary checkouts are removed on normal
completion, including case errors.

## Cases and provenance

Fixtures live under `internal/diagnosiseval/testdata/cases/` and are embedded in
the binary, so installed commands work from any directory. Each `case.json`
records the source report, capture time, exact head and base SHAs, collection
limits and grading rules. The initial snapshots were captured while the four
PRs still matched their saved reports. Their evidence includes subsequently
collected base checks and artifacts that were absent from the older reports.

| Case | Expected decision |
| --- | --- |
| `lyzma` | `human_action` for the job that never started because of billing. |
| `muse` | `code_repair` in `go.mod` for the vulnerable Go toolchain. |
| `zoomflow` | `rerun_candidate` or `insufficient_evidence` for the Firefox and WebKit failures; preserve uncertainty and cite the available artifact text. |
| `agent-session-loop` | `code_repair` for the evidenced Python 3.15 override signature problem; `rerun_candidate` or `insufficient_evidence` for the browser and macOS failures. Use the observed base comparisons. |

ASL's Linux and browser checks failed at both commits, while its macOS job
passed at the base. Matching failures alone do not prove a shared root cause,
and a passing base does not prove that the action update caused the PR failure.
The benchmark's expected actions are curated labels, not verified repair
outcomes. Review the labels and complete diagnoses when interpreting results.

`checkout/` contains selected verbatim source files fetched at the recorded
head SHA, plus upstream licenses where available. A module file is stored as
`go.mod.snapshot` to avoid Go's rule excluding nested modules from embedding;
the runner restores its name to `go.mod`. Workflow files under `.github/` are
included. `logs/` contains bounded production evidence excerpts. URL query
strings and signed storage URLs were removed, and recognizable tokens and
control characters were cleaned before freezing the data. Archive limits,
missing logs and uninspected binary references remain explicit gaps. Nothing
is downloaded during replay, including linked screenshots or traces.

Only the source files enter the agent's temporary directory. Logs, snapshots
and artifacts enter the normal diagnosis prompt. Descriptions, provenance,
expected actions and assertion terms are withheld from the agent. The saved
reports' assessments are not supplied as evidence or as repair plans.

## Scoring and output

The grader examines the agent's raw analysis, before the diagnosis service
adds `insufficient_evidence` recommendations for omitted failures. Each case
defines an exact subject for each outstanding blocker and a set of acceptable
actions. A correct action requires exactly one recommendation for that subject.
Duplicate subjects fail even if one recommendation has an acceptable action.

Scores include:

- **Correct actions:** recommendations with an expected subject and an
  acceptable action, out of all expected actions.
- **Missed blockers:** expected subjects absent from the agent's answer.
  A wrong action is reported separately and does not count as an omitted subject.
- **Unsupported code repairs:** each `code_repair` recommendation for a subject
  whose expected actions do not permit editing, including unexpected subjects.
- **Unexpected recommendations:** subjects outside the case's expected blockers,
  including recommendations for passing or intentionally skipped jobs.
- **Evidence checks:** case-insensitive substring assertions for selected
  references, such as `go.mod`, a patch version, an artifact ID or a base
  comparison. `any_of` accepts any listed spelling. A scoped assertion searches
  only the named recommendation's reason and next step; an unscoped assertion
  searches the entire raw analysis.

These text checks are deliberately simple. They can miss a valid paraphrase,
and mentioning a term does not prove correct reasoning or causality. There is
no model judge. Inspect diagnoses alongside scores before changing prompts,
models or labels. Four cases are a regression benchmark, not a broad measure
of agent reliability.

JSON includes `agent`, configured `model` (empty means backend default), CLI
`version`, `suite_sha256`, selected cases, timestamps, durations in nanoseconds,
per-case findings, the raw `agent_analysis` and the effective `diagnosis`.
The suite fingerprint covers frozen source, evidence and grading rules. Compare
reports with the same fingerprint and record the model configuration. To
observe model variability, repeat the command and retain each report.

The [initial live baseline](evaluations/claude-opus-baseline.json), using Claude
with configured model `opus`, passed all four cases: 7/7 actions and 14/14
evidence checks, with no unsupported code repairs or omitted blockers. This is
one recorded run with a model alias, not a guarantee about future runs or the
specific model version that the backend resolves that alias to.

A backend failure, timeout or invalid response is a case error, with no credit
for its expected actions. It is recorded separately from grading findings.
Exit codes are 0 when every selected case passes, 1 for failed or errored cases,
2 for invalid usage, and 130 for interruption. `--json` keeps progress on stderr
and emits one JSON report on stdout, including when cases fail.

## Adding or updating a case

1. Capture a PR snapshot and bounded evidence with the production GitHub client.
   Verify the head and base did not change during collection. Include producer
   identities, workflow attempts, base evidence and collection errors.
2. Select the code needed to interpret the failure and fetch it at the recorded
   head SHA. Keep original paths, using `go.mod.snapshot` for module files.
   Include attribution and license files where available. Do not include
   credentials, caches, dependency directories or repository customizations.
3. Sanitize the evidence before saving. Put log excerpts in `logs/` and refer to
   them using `log_file` on the corresponding check. Preserve truncation and
   missing-evidence markers; do not invent unavailable evidence.
4. Define expected subjects/actions from the observed evidence. Accept more
   than one action where the evidence warrants uncertainty. Add only a few
   useful evidence-reference assertions, and review their scope and limitations.
   Labels must not be copied into the source snapshot or evidence.
5. Record provenance in `case.json`. A changed fixture changes the suite hash.
   Run `go test ./internal/diagnosiseval ./cmd` and a live replay. Keep unexpected
   model answers visible; change a label only when the evidence supports it.

Ordinary tests use deterministic agent doubles to verify grading, identity
preservation, production-service replay, label isolation, timeouts and error
handling. They do not measure model quality and never invoke a live model.
Live evaluations are explicitly opt-in and are not part of `just ci`.
