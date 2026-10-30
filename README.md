# deps-merger

[![CI](https://github.com/sglavoie/deps-merger/actions/workflows/ci.yml/badge.svg)](https://github.com/sglavoie/deps-merger/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/sglavoie/deps-merger)](https://github.com/sglavoie/deps-merger/releases)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Merges safe Dependabot pull requests across a list of repositories and
reports what needs fixing in each one. It uses your `gh` login.

The same repository also contains a free GitHub App server. It
reports into a Dependency health issue and merges only in repositories
that opt in. See [GitHub App setup and operation](docs/app/README.md).
The App also reruns when a Dependabot PR leaves draft state, is edited,
or receives an approval or review dismissal.
Queued webhook events and retries recognize repository names regardless
of capitalization, including when replayed after a restart.
Incomplete installation or repository lists stop reconciliation, preserving
stored data and access state until GitHub returns a complete list.
Large Dependency health reports keep complete lines within a 60,000-byte
budget and show a truncation notice, so they can still be published.
Dependency health issue lookups reject missing lists, unidentified entries,
unknown issue states and broken pagination before creating or editing an issue.
Issue creation also requires a valid response identifying the App's health
issue before reporting success or attempting to pin it. An unconfirmed
creation is looked up again on the next run.
Unknown PR states also fail tracking and preserve the previously open record
instead of counting the PR as closed.
Uninstall cleanup cannot be undone by a run finishing in another process:
tracking results and issue numbers are saved only while the original run exists.
Overlapping runs also preserve confirmed PR merges, their recorded times and
the App's merge attribution when a delayed run saves an older outcome.
The App shares GitHub's rate-limit cooldown across each installation's
repositories, retries, repository listings, review comments and health issues,
including issue pinning when GraphQL returns a rate limit with HTTP 200.
Other installations keep running with their own quotas.
Repository reconciliation records access as soon as a repository is discovered,
so older webhook responses cannot revoke that access before its first run.
Later sweeps can also revoke access to discovered repositories that have not run yet.
Repositories with reviews disabled skip review setup, so a broken review
cache or unavailable checkout credentials cannot fail their reports.

It is deliberately conservative. A PR merges only when Dependabot alone
wrote it, its checks are green and its update is within your policy. With
`--review`, an AI agent (Claude Code or Codex) reads a read-only checkout
to judge the bigger updates. Anything it can't merge is reported with the
fix: a check broken on the base branch, an update to ignore, a review
branch protection wants.

![deps-merger merging, rebasing and reviewing Dependabot PRs across fictional repositories](docs/demo.gif)

<sub>Recorded with the `demo` build against make-believe repositories: no
real PR was harmed. See [Try it without your repositories](#try-it-without-your-repositories).</sub>

## Install

```bash
brew install --cask sglavoie/tap/deps-merger           # macOS, Linux
go install github.com/sglavoie/deps-merger@latest      # Go 1.27+
```

Or download a binary from the [releases](https://github.com/sglavoie/deps-merger/releases).
deps-merger needs [`gh`](https://cli.github.com) logged in (`gh auth login`)
or `GH_TOKEN` set. Agent review also needs
[Claude Code](https://docs.claude.com/en/docs/claude-code) or
[Codex](https://github.com/openai/codex) on `PATH`, on Linux or macOS.

Only github.com is supported. If `GH_HOST` or your sole `gh` login selects
another host, runs and repository discovery stop before making requests:
review checkouts and cached verdicts assume github.com. Set
`GH_HOST=github.com` to select your github.com account explicitly.

The Homebrew cask installs shell completions for bash, zsh and fish,
which complete `-r` and `--owner` with your configured repos. Otherwise,
load `deps-merger completion bash` (or `zsh`, `fish`, `powershell`);
`deps-merger completion --help` says how.

## Usage

```bash
deps-merger init                  # write an example config, then list your repos in it
deps-merger init --discover       # or fill it with your repos that use Dependabot
deps-merger repos                 # effective policy per repo (--json, -r, --owner)
deps-merger run                   # dry run: report only
deps-merger run --apply           # merge safe PRs
deps-merger run --open            # only repos with open Dependabot PRs
deps-merger run -r Storefront --json  # one repo, machine-readable
deps-merger run --markdown        # GitHub Markdown, as for an issue body
deps-merger run --review --apply  # let an AI agent judge bigger updates too
deps-merger pr diagnose acme/Storefront#123 > repair.md  # AI repair handoff
deps-merger pr fix acme/Storefront#123 --agent codex    # edit, push and verify CI
deps-merger verdicts              # the agent's cached verdicts
deps-merger verdicts acme/Storefront --json  # one repo's, with concerns
```

### Diagnose or repair a specific PR

Point at any PR by URL or `owner/repo#number`, including PRs outside your
configured repositories and PRs opened by someone other than Dependabot:

```bash
# Get a plan to paste into your next coding session; nothing is pushed.
deps-merger pr diagnose https://github.com/acme/Storefront/pull/123 \
  --agent codex > repair.md

# Focus on a particular failure, or request structured output.
deps-merger pr diagnose acme/Storefront#123 --focus 'Linux build failure'
deps-merger pr diagnose acme/Storefront#123 --json

# Apply a saved report (edit repair.md first if you want to refine its steps).
# The report identifies the PR, so no PR argument is needed.
deps-merger pr fix --plan repair.md --agent codex

# Or provide the PR explicitly alongside the saved plan.
deps-merger pr fix acme/Storefront#123 --plan repair.md --agent codex

# Reuse an existing PR checkout and its installed dependencies.
deps-merger pr fix --plan repair.md --agent codex \
  --working-directory /path/to/Storefront

# Authorize a repair without an existing plan.
deps-merger pr fix acme/Storefront#123 --agent codex \
  --max-attempts 3 --wait-timeout 15m
```

`diagnose` reads the PR's current checks, merge state, description, diff and
available failed GitHub Actions job logs and annotations. Its Markdown handoff
identifies the PR and commit, explains likely causes, proposes concrete edits
and validation commands, and lists remaining work and evidence gaps. It also
works when checks pass, are pending, are missing, or only some fail. External
CI, expired logs and access failures are reported as evidence gaps.

With `--plan repair.md`, the AI implements the saved diagnosis and any edits
you made to its steps and validation commands. It reads the file without
modifying it. The PR is inferred from the report header or `PR:` line; an
explicit PR must match. A custom Markdown plan without PR metadata requires
an explicit PR argument. The command still reads current PR/check state and
checks for concurrent updates. When the saved head and base match, it reuses
the existing diagnosis without downloading failure logs again on the first
attempt. If the code has changed, commit metadata is missing, or another attempt is needed, it
refreshes failure evidence and adapts the saved steps rather than generating
a new plan. Plans must be non-empty UTF-8 Markdown files of at most 1 MiB.

`--working-directory /path/to/repo` runs the repair agent directly in an
existing checkout, including a Git worktree. The directory must be the
repository root, with no staged, unstaged or untracked changes, on a non-base
branch or detached HEAD. It can be at the selected PR head or contain local
commits on top of it. Those commits are preserved and included in the PR update.
If they already fix the issue, the AI validates them and the command pushes
them without creating an empty commit. The command never resets, cleans,
switches branches or deletes this directory; ignored dependencies such as `node_modules` are
preserved. Successful repairs commit to its current branch (or detached HEAD)
and push to the selected PR branch. An exclusive lock prevents two deps-merger
repairs from sharing the checkout. Existing remote URLs are left unchanged.
Directory selection does not change the agent's network permissions or fix
registry/DNS failures.

The remote PR head is fetched into a private ref when needed to check ancestry.
Diverged history is rejected with instructions to merge, rebase or cherry-pick
your work onto the PR. Commits made on `main` belong to the base branch: check
out the PR branch and merge/rebase `main` into it before rerunning. The command
keeps the base branch protected. Passing or pending CI on the older remote
commit never skips unpublished local commits; the tool validates and publishes
the local code, then observes CI for the resulting head.

`fix` uses a temporary checkout by default. It waits for pending CI before
editing, runs a writable Claude Code or Codex task, commits the resulting file
changes and pushes to the PR's head branch. It verifies the new commit's CI,
retrying failures up to the attempt limit. An already passing PR needs no
code repair unless it has merge conflicts or is behind its base. API models
support `diagnose` only. Agent selection, model, budget and per-task timeout
come from your existing `agent:` config; `--agent` overrides the selection.
Without a config, Claude is the default. A config containing only agent
settings is accepted by these commands.

The command checks for concurrent PR and base changes before pushing. If
anything moved, its error identifies the changed fields and old/new values;
that attempt's edits remain local. It uses a Git lease to avoid overwriting a concurrent push. It never merges the
PR. Reviews, draft status, branch protection and other merge gates remain
visible even when CI passes. An empty or all-skipped check list does not
confirm passing CI. The wait is bounded per commit; a timeout or incomplete
check list is a failure, with any repair checkout retained for recovery.
`diagnose` exits 0 when its report completes even if it finds blockers;
`fix` exits 0 when checks pass and code merge conflicts are resolved, and 1
on an unsuccessful repair.
Both use 2 for invalid arguments and 130 for interruption. `--json` includes
errors and the retained checkout path when available.

A repair adds commits that Dependabot did not author. The existing `run`
command therefore leaves that PR alone under its authorship policy; review
and merge the repaired PR yourself after its checks and remaining gates pass.
These commands are CLI-only and unavailable in the demo build.

### Try it without your repositories

The `demo` build replaces GitHub and the agent with fictional `acme`
repositories held in memory. It never reaches the network or reads your
config, token or verdicts, so `--apply` is safe to try:

```bash
git clone https://github.com/sglavoie/deps-merger && cd deps-merger
go run -tags demo . run                    # dry run
go run -tags demo . run --review --apply   # merge, rebase, agent verdicts
go run -tags demo . verdicts
```

## What counts as safe

A PR is merged only when all of these hold:

- Dependabot opened it and wrote every commit: authored by its GitHub
  account, committed and signed by GitHub, with no `Co-authored-by`
  anyone else. The verified commits must include the current head. A PR
  anyone else pushed to is left alone.
- It is not a draft, and the repository is not archived and you can write
  to it.
- Every dependency it moves has a known semver bump, and the largest is
  within `max_update_type` (a grouped PR is as risky as its riskiest bump).
  Missing or blank dependency names invalidate the metadata for the whole
  commit, requiring agent review or manual handling.
  Metadata containing multiple YAML documents or malformed trailing YAML
  is also rejected rather than graded from its first document alone.
- Every check on its head commit passed, and at least one ran, unless the
  repo sets `require_checks: false` or `merge_without_checks_after`.
- GitHub reports it mergeable and clean, and nothing about it was cut off
  (over 100 commits or checks).

An unrecognized mergeability value blocks action. Missing or unrecognized
check types, statuses and completed-check conclusions also block action,
even when a newer check passed. A newer check can replace
an older one only when both identify their name, suite, app, workflow and
event; checks with missing identifiers remain part of the decision.
An unknown check state cannot replace an earlier result, so known failures
remain visible in reports while the unknown state blocks merging.
The waiting period for `merge_without_checks_after` uses the head commit's
timestamp. When that timestamp is missing, the PR waits and is not diagnosed
as having no CI yet.

Missing mergeability or merge-state values get the same bounded retries as
`UNKNOWN`. If they remain unresolved, the PR waits without merging.

A safe PR that is only behind its base branch gets one `@dependabot rebase`
comment, one PR at a time per target branch, so that merges do not rerun CI
on every other PR targeting that branch. PRs targeting independent branches
do not hold each other's rebase queues. Merges are made one at a time with
the head SHA pinned, re-reading the repository after each; a merge GitHub
fails in a way that may hide a success is checked before it is reported. Writes are never sent
concurrently, and a rate limit stops them rather than push through it.
Merge responses must explicitly confirm success. Missing or malformed
confirmations are reconciled against the PR's state; an unknown state is
reported as an error rather than assumed to mean the PR was closed.
HTTP 500 responses also require reconciliation: an internal server error
does not establish whether a merge went through.
Rate-limit pauses accept `Retry-After` as seconds or an HTTP date; pauses
over two minutes stop further writes for that run.
A PR whose merge or rebase request failed stops holding the rebase queue
for that run, even if it later waits on CI or GitHub to compute mergeability.
A PR blocked on a required human review also leaves the rebase queue free
while it waits for CI. If GitHub reports existing comments but returns none,
the PR waits rather than risking a duplicate rebase request.
An unreadable response to a successful rebase-comment POST is reported as
possibly posted, with a warning, and keeps its place in the queue. A rebase
request with the same timestamp as the head commit also counts as already
requested, so timestamp ties do not trigger duplicate comments.
Rebase requests recognize your login regardless of capitalization.

The exact rules, and how each failure, retry, interruption and rate limit
is reported, are in [How deps-merger behaves](docs/behaviour.md#what-counts-as-safe).

## Agent review

Some updates are safe in every way the policy can check but their size: a
bump over `max_update_type`, or one Dependabot records no semver size for
(a GitHub Action pinned by commit, a Docker digest). With `--review`, an AI
agent judges those:

```bash
deps-merger run --review                # dry run: review, merge nothing
deps-merger run --review --apply
deps-merger run --agent codex --apply   # another agent; implies --review
deps-merger run --agent api --review --review-parallel 1   # a local model
```

The agent reads a shallow checkout of the PR, its release notes and its
diff, and answers `approve` or `reject` with its reasons. It runs
read-only, without your customizations and without a GitHub token:

- Claude Code with only the Read, Grep and Glob tools, no MCP servers,
  settings, `CLAUDE.md`, skills, plugins or hooks;
- Codex in its read-only sandbox, without your `config.toml` or rules;
- `api`: a model behind an OpenAI-compatible chat completions endpoint,
  such as one served locally by llama.cpp's `llama-server`, Ollama or LM
  Studio, which costs nothing per review. It runs no program: it reads
  the checkout only through three tools deps-merger implements (list,
  read and search), which cannot reach outside it. With `api.tools: false`,
  file access is disabled and any tool call fails the review. API verdicts
  must be a single JSON object after any thinking block or code fence;
  all verdict fields must be present with the expected types, with no
  duplicate fields. Ambiguous or incomplete replies require a fresh answer
  before they can approve a PR. Reasoning after the answer also requires
  a fresh answer. Filtered responses and missing or unsupported completion
  states fail the review, even when their text contains a valid verdict.
  Responses over 16 MiB also fail rather than accepting a truncated JSON prefix.
  Query parameters in `api.url` are preserved when requesting chat completions.
  Claude Code and Codex verdicts undergo the same field
  validation; malformed verdicts fail the review.
  Claude Code must also explicitly report a successful completed result;
  incomplete results and results reporting a limit or other failure cannot
  approve an update even if they contain a valid verdict. File reads and searches
  stop when the review is cancelled or its timeout expires.
  API file tools preserve leading and trailing whitespace in filenames,
  so they read the exact path requested.
  Searches report unreadable files or directories as errors instead of
  presenting partial results or claiming there were no matches.
  Search handles LF and CRLF line endings alike and reports no matches
  for empty files.

Read-only is not confined to the checkout for Claude Code and Codex: they
can read any file your user can, and what an agent reads goes to its
model provider (for `api`, wherever `api.url` points).
[Agent review](docs/behaviour.md#agent-review) says exactly what each
agent can reach.

With `--apply`, deps-merger posts the reviewer's decision, reasoning and
concerns as a PR comment before merging or leaving the reviewed PR alone.
Cached verdicts are posted too; an identical comment by the current GitHub
user among the PR's latest 50 comments is reused. Review failures get an
explanation as well. If posting fails, processing that repository stops
without merging the PR and the run exits `1`; a later run can retry using
the cached verdict. Dry runs and repositories in report mode post no comments.
Reviewer text is escaped so Markdown images, links and bare URLs cannot
publish external resources; mentions and bot commands are neutralized too.

The agent never merges anything: an approved PR goes through every check
again before it merges. Reviews run after their repository's other
merges, four at a time (`--review-parallel`), at most 10 per run
(`--max-reviews`). Verdicts
are cached for 14 days in `~/.local/state/deps-merger/verdicts.json`, so a
scheduled run does not pay twice for the same update; a new version or a
change of target branch is always reviewed again, and a rejection lasts
only until Dependabot rebases the PR. `deps-merger verdicts` lists them and
`deps-merger verdicts forget acme/Storefront#42` drops them. Set
`agent_review: false` on a repo to keep the agent out of it.
Missing or whitespace-only dependency versions pin the verdict to the
reviewed commit, so a changed head requires a fresh review.

Overlapping review runs cannot share checkouts, even when they use different
state directories for their verdict caches.
The App also locks each installation's review files across processes. A
conflicting server or `server -once` run fails review setup and can retry;
updates that pass the ordinary merge policy can still proceed.
Uninstall cleanup uses the same lock and removes retained checkouts,
verdicts and recovered cache files, including when reviews are disabled
in the server configuration.

Cache keys quote dependency names and versions so separators in either
cannot reuse another update's approval. Older keys, including those created
before github.com host validation, are not reused, so upgrading requires
one fresh review per update. Forgetting a full update
key removes only that exact update; only repository names ignore case.

## What needs attention

Under each repo the report lists, without acting on them:

- **repo-wide failures**: a check failing on every Dependabot PR that ran
  it targeting the same base branch (and that ran on at least half of
  that branch's PRs), which points at the base branch rather than the
  updates; the report names that branch. Only PRs Dependabot alone wrote,
  whose checks ran on their head commit, count towards either;
- **incompatible updates**: a PR failing its own checks, with the
  `@dependabot ignore …` command that would stop it coming back (only for
  PRs Dependabot alone wrote, whose checks ran on their head commit);
- **held majors** and **held minors**: updates that would merge, rebase or
  wait (for their checks, say) but for `max_update_type` (minors only
  under `max_update_type: patch`);
- **held updates of unknown size**: updates that would merge, rebase or
  wait but for Dependabot not recording a semver bump (a GitHub Action pinned by commit,
  a Docker digest), when no agent reviewed them;
- **review required**: branch protection wants an approving review, which
  deps-merger never gives;
- **behind**: safe PRs behind a base branch that must be up to date, left
  for a human to rebase under `dependabot_commands: false` (for runs as a
  bot, whose `@dependabot` commands Dependabot ignores);
- **stalled rebases**: a rebase request Dependabot ignored for a day, with
  `@dependabot recreate` as the fix;
- **no checks**, **read-only** and **archived** repositories (no checks
  counts only PRs Dependabot alone wrote, not drafts, archived ones or ones
  whose head commit is under 30 minutes old, on which CI may not have
  started yet, and a PR whose every check was skipped as having none);
- **truncated**: more than 100 open Dependabot PRs, so only the oldest 100
  were evaluated; newer ones wait until older ones close;
- **agent rejected** / **agent failed**: an update the agent judged unsafe,
  with its concerns, or could not review.

Truncation findings remain visible with `--open`, even when every PR on
the fetched search page has closed since GitHub indexed it. An incomplete
search is not reported as having no open PRs.
Missing search results, counts or lists, and unknown states on returned PRs,
fail the repository read instead of silently hiding PRs.
Null or unidentified search items, non-positive PR numbers and search counts
smaller than the returned list also fail the read, so `--open` cannot hide
an incomplete response as a repository with nothing to process.
Missing or null archive and draft flags also fail the read rather than
treating a repository as active or a PR as ready for merging.
Missing commit, check or comment lists or totals also fail the read, as do
totals smaller than the returned lists. Incomplete responses cannot establish
that all commits and checks were verified or that no rebase was requested.
A missing check-summary field also fails the read; only an explicit null
means the head has no checks, even when checks are optional.

Repository discovery rejects missing lists or pagination fields and missing
or repeated continuation cursors. It also rejects null or unidentified
repository entries, missing archive or fork flags, and omitted permission
or Dependabot-config lookups. Explicit null permissions and config lookups
remain valid and are filtered normally. If a later page is incomplete,
`init --discover` fails instead of writing a partial repository list.

Failure diagnosis compares checks by name and their available app, workflow
and event identities, keeping commit status contexts separate from check runs.
A same-named job from another source cannot establish or hide a shared
failure, and a shared failure does not hide an update's separate failing job.
Incomplete check lists are excluded from failure and no-checks diagnoses:
unread checks may include a passing rerun or the only check that ran.

## Scripting

A dry run never writes to GitHub, so `deps-merger run` is safe to
schedule (with `--review` it still runs the agent, and caches its
verdicts for a later `--apply`). The exit status is:

- `0`: every repository was processed and every write succeeded;
- `1`: a repository could not be processed, a merge or rebase request
  failed, an agent review could not be done, or the report could not be written;
- `2`: the command line is wrong, and nothing ran;
- `130`: interrupted (Ctrl-C, `SIGTERM` or `SIGHUP`).

A failed review still makes the run exit `1` if the PR closes or changes
before the next repository read; its failure remains in the report.
An answer returned after its review times out or the run is cancelled is
discarded and cannot become a cached approval for a later run.
`init`, `repos` and `verdicts` also exit `1` if their output cannot be
written, including confirmations after a config or cache change.

`run`, `repos` and `verdicts` take `--json`. `run --json` prints
`{"apply": bool, "results": [...]}`, one result per repository with its
policy, a decision per PR, findings, the writes made and warnings. Lists
are always arrays, never `null`. [Scripting](docs/behaviour.md#scripting)
describes every field and what each exit status covers. `run --markdown`
prints each repository's section as GitHub Markdown instead, ready for an
issue or a CI job summary.

The library's JSON report also writes an empty result set as `"results": []`,
including when the caller supplies a nil slice.

On a terminal the report is colored; `NO_COLOR=1` or `CLICOLOR=0` turns
color off and `CLICOLOR_FORCE=1` keeps it when piped.

## Config

The config lives at `~/.config/deps-merger/config.yaml`, or under
`$XDG_CONFIG_HOME` when that is set; `-c` names another file, and
`deps-merger init` writes the commented
[`config.example.yaml`](config.example.yaml) there. Defaults: squash,
patch and minor only, checks required, agent review allowed (it only runs
with `--review`), Claude as the agent. Every policy field under
`defaults` can be overridden per repo:

```yaml
defaults:
  max_update_type: minor
repos:
  - acme/Storefront
  - repo: acme/Billing
    max_update_type: patch
  - repo: acme/Mirror
    skip: true              # left out unless named with -r
```

`deps-merger run -r Billing` runs one repo and `--owner acme` every repo
of an owner. [Config](docs/behaviour.md#config) has every rule.
A `skip:` field with no value (including `null` or `~`) is an error;
use `true` or `false`, or omit the field to include the repository.

A repository can also set its own policy in `.github/deps-merger.yml` on
its default branch, and an owner for all its repositories in
`deps-merger.yml` in its `.github` repository. They take the policy
fields `defaults` takes, plus `mode: report` to have the repository only
reported on, even by `run --apply`:

```yaml
# .github/deps-merger.yml
max_update_type: patch
mode: report
```

The owner's file overrides your `defaults`, the repository's file
overrides its owner's, and your entry for the repository overrides both.
`deps-merger repos` shows your config alone; a run reads the files, and
its report names those it used.
Errors reading those files stop the repository from being processed;
only an absent file or an unambiguously missing repository inherits policy
without a file. A concurrent rate-limit error is still reported.
An incomplete policy response with missing repository or file fields also
stops processing; only explicit absence allows policy inheritance.
Policy text also requires explicit confirmation that it is neither binary
nor truncated; missing or null flags stop processing.

Recursive YAML merge aliases are rejected in both local config and
repository policy files.

`init --force` backs up the existing config before replacing it. If writing
the replacement fails, it restores the backup only when the destination is
still absent. A config created by another process is preserved, and the
error names the retained backup if restoration fails.

## As a library

The engine the CLI runs is importable from `pkg/`: `runner` drives a run
over a `github.Client`, with `policy`, `config`, `review` and `report`
behind it. Until 1.0 its API may change in any release; pin a version.
The same `Runner` can run successive passes: each `Run` reloads owner
policies and starts with a fresh review budget and rate-limit state. Calls
to `Run` must not overlap other `Run` or `RunRepo` calls on that runner.
Custom clients must normalize check states to the `github.CheckState`
constants; empty or unrecognized states block merging even when checks
are optional, an agent approved the update, or a newer check would otherwise
supersede the unrecognized one.
Custom clients must return a known `github.PRState` when reconciling a merge;
empty or unrecognized states leave the outcome uncertain and report an error.
The review cache copies verdict concerns on storage and retrieval, so callers
can edit their own copies independently. Failed cache writes leave the
in-memory verdicts unchanged. Codex and API reviews accept
absolute or relative checkout paths, including `.` for the current directory.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md), and [SECURITY.md](SECURITY.md) to
report a vulnerability.

Maintainers can tag and publish a version with `just release patch`
(or `minor` / `major`); see [Releasing](CONTRIBUTING.md#releasing).

## License

[MIT](LICENSE)
