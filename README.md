# deps-merger

[![CI](https://github.com/sglavoie/deps-merger/actions/workflows/ci.yml/badge.svg)](https://github.com/sglavoie/deps-merger/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/sglavoie/deps-merger)](https://github.com/sglavoie/deps-merger/releases)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Merges safe Dependabot pull requests across a list of repositories and
reports what needs fixing in each one. It uses your `gh` login.

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
deps-merger verdicts              # the agent's cached verdicts
deps-merger verdicts acme/Storefront --json  # one repo's, with concerns
```

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
  anyone else. A PR anyone else pushed to is left alone.
- It is not a draft, and the repository is not archived and you can write
  to it.
- Every dependency it moves has a known semver bump, and the largest is
  within `max_update_type` (a grouped PR is as risky as its riskiest bump).
- Every check on its head commit passed, and at least one ran, unless the
  repo sets `require_checks: false` or `merge_without_checks_after`.
- GitHub reports it mergeable and clean, and nothing about it was cut off
  (over 100 commits or checks).

A safe PR that is only behind its base branch gets one `@dependabot rebase`
comment, one PR at a time per repository, so that merges do not rerun CI
on every other PR. Merges are made one at a time with the head SHA pinned,
re-reading the repository after each; a merge GitHub fails in a way that
may hide a success is checked before it is reported. Writes are never sent
concurrently, and a rate limit stops them rather than push through it.

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
  read and search), which cannot reach outside it.

Read-only is not confined to the checkout for Claude Code and Codex: they
can read any file your user can, and what an agent reads goes to its
model provider (for `api`, wherever `api.url` points).
[Agent review](docs/behaviour.md#agent-review) says exactly what each
agent can reach.

The agent never merges anything: an approved PR goes through every check
again before it merges. Reviews run after their repository's other
merges, four at a time (`--review-parallel`), at most 10 per run
(`--max-reviews`). Verdicts
are cached for 14 days in `~/.local/state/deps-merger/verdicts.json`, so a
scheduled run does not pay twice for the same update; a new version is
always reviewed again, and a rejection lasts only until Dependabot
rebases the PR. `deps-merger verdicts` lists them and
`deps-merger verdicts forget acme/Storefront#42` drops them. Set
`agent_review: false` on a repo to keep the agent out of it.

## What needs attention

Under each repo the report lists, without acting on them:

- **repo-wide failures**: a check failing on every Dependabot PR that ran
  it (and that ran on at least half of them), which points at the base
  branch rather than the updates; only PRs Dependabot alone wrote, whose
  checks ran on their head commit, count towards either;
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

## Scripting

A dry run never writes to GitHub, so `deps-merger run` is safe to
schedule (with `--review` it still runs the agent, and caches its
verdicts for a later `--apply`). The exit status is:

- `0`: every repository was processed and every write succeeded;
- `1`: a repository could not be processed, a merge or rebase request
  failed, or an agent review could not be done;
- `2`: the command line is wrong, and nothing ran;
- `130`: interrupted (Ctrl-C, `SIGTERM` or `SIGHUP`).

`run`, `repos` and `verdicts` take `--json`. `run --json` prints
`{"apply": bool, "results": [...]}`, one result per repository with its
policy, a decision per PR, findings, the writes made and warnings. Lists
are always arrays, never `null`. [Scripting](docs/behaviour.md#scripting)
describes every field and what each exit status covers. `run --markdown`
prints each repository's section as GitHub Markdown instead, ready for an
issue or a CI job summary.

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

## As a library

The engine the CLI runs is importable from `pkg/`: `runner` drives a run
over a `github.Client`, with `policy`, `config`, `review` and `report`
behind it. Until 1.0 its API may change in any release; pin a version.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md), and [SECURITY.md](SECURITY.md) to
report a vulnerability.

## License

[MIT](LICENSE)
