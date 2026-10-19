# deps-merger

Merges safe Dependabot pull requests across a list of repositories and
reports what needs fixing in each one. It uses your `gh` login.

```bash
just install                      # builds into ~/.local/bin
cp config.example.yaml ~/.config/deps-merger/config.yaml
deps-merger repos                 # effective policy per repo
deps-merger run                   # dry run: report only
deps-merger run --apply           # merge safe PRs
deps-merger run --open            # only repos with open Dependabot PRs
deps-merger run -r Storefront --json  # one repo, machine-readable
deps-merger run --review --apply  # let an AI agent judge majors too
```

## What counts as safe

A PR is merged only when all of these hold:

- opened by Dependabot, and every commit is authored by Dependabot and
  signed with GitHub's key, as Dependabot's commits are, and has no
  `Co-authored-by` trailer naming anyone else (a PR anyone else pushed to is
  left alone, even one forging Dependabot as the author);
- not a draft; the repository is not archived and you have write access;
- every dependency in the commit metadata has a known semver bump, and the
  largest one is within `max_update_type` (a grouped PR is as risky as its
  riskiest bump);
- every check on the head commit passed, and at least one ran unless the
  repo sets `require_checks: false` (a check GitHub marked stale after it
  sat unfinished for 14 days blocks the merge until it is rerun; a commit
  less than 30 minutes old with no checks yet waits for CI to start). With
  `merge_without_checks_after: 10m`, a commit that has gone 10 minutes
  without any check merges anyway, for repos whose CI only runs for some
  paths;
- GitHub reports it `MERGEABLE` and `CLEAN` (or `HAS_HOOKS`);
- nothing was cut off: a PR with more than 100 commits or 100 checks is
  skipped rather than judged on partial data.

A safe PR that is only `BEHIND` its base branch gets one `@dependabot rebase`
comment per head commit, one PR at a time: while a PR can merge, is up to
date and waiting on CI, or has a rebase pending, the others wait
(`queued behind #N`). If the base branch must be up to date, each merge puts
every other PR behind again, so rebasing them all at once would rerun CI on
each for every merge. A rebase request Dependabot has not acted on within a
day stops holding the queue: that PR is left for a human and the next one is
rebased.

Merges happen one at a time per repository, with the head SHA pinned,
re-reading state after each. A merge GitHub rejects with 405 or 409, as it
can right after another merge, is retried once. Up to four repositories are
processed at once, but merges and comments are never sent concurrently.

PRs not opened by Dependabot are never fetched: the query asks GitHub only
for `author:app/dependabot`, and the author is checked again locally.

## Agent review

Some updates are safe in every way the policy can check but their size:
a major bump over `max_update_type`, or an update whose size Dependabot
does not record as semver (a GitHub Action pinned by commit, a Docker
digest). With `--review`, deps-merger asks an AI agent about those, once
everything else about them would let them merge or rebase:

```bash
deps-merger run --review                # dry run: review, merge nothing
deps-merger run --review --apply
deps-merger run --agent codex --apply   # another agent; implies --review
```

The agent gets a shallow checkout of the PR's head commit (kept under
`~/.cache/deps-merger/checkouts` and reused), the PR description with
Dependabot's release notes and changelog, and the diff. It looks for
breaking changes upstream and how the repository uses what changed, then
answers `approve` or `reject` with its reasons. It runs read-only:

- Claude Code: `claude -p --restricted --strict-mcp-config` with only the
  Read, Grep and Glob tools, no MCP servers and no settings files;
- Codex: `codex exec --sandbox read-only --ephemeral`.

The agent never merges anything. An approved PR is evaluated again and
merged (or rebased) like any other: Dependabot's signed commits, green
checks, mergeability and the pinned head SHA are all checked again first.
A rejection is reported with the agent's concerns under "needs
attention"; a review that fails or times out leaves the PR alone and makes
the run exit 1. The PR description is treated as untrusted: the prompt
tells the agent it cannot change its instructions.

Reviews run after the run's other merges, two at a time, at most
`--max-reviews` (default 10) per run. Verdicts are kept for 14 days in
`~/.local/state/deps-merger/verdicts.json`, keyed by the PR and the
versions it moves to, so a rebase does not trigger a new review but a new
version does. Delete an entry, or the file, to have an update reviewed
again.

Configure the agent under `agent:` in the config. `claude.config_dir` is
passed as `CLAUDE_CONFIG_DIR`, picking the Claude Code account and
settings to use; `codex.home` is passed as `CODEX_HOME`. Set
`agent_review: false` on a repo to keep the agent out of it.

## What needs attention

Under each repo the report lists, without acting on them:

- **repo-wide failures**: a check failing on every Dependabot PR that ran
  it (and that ran on at least half of them), which points at the base
  branch rather than the updates;
- **incompatible updates**: a PR failing its own checks, with the
  `@dependabot ignore …` command that would stop it coming back (only for
  PRs Dependabot alone wrote, whose checks ran on their head commit);
- **held majors**: green major updates blocked only by policy;
- **review required**: branch protection wants an approving review, which
  deps-merger never gives;
- **stalled rebases**: a rebase request Dependabot ignored for a day, with
  `@dependabot recreate` as the fix;
- **no checks** and **read-only** repositories;
- **truncated**: more than 100 open Dependabot PRs, so only the oldest 100
  were evaluated;
- **agent rejected** / **agent failed**: an update the agent judged unsafe,
  with its concerns, or could not review.

## Scripting

A dry run never writes, so `deps-merger run` is safe to schedule. For
`deps-merger run --apply` from cron or CI, the exit status is:

- `0`: every repository was processed and every write succeeded;
- `1`: a repository could not be processed, a merge or rebase request
  failed, or an agent review could not be done (the report says which);
- `130`: interrupted (Ctrl-C or `SIGTERM`).

`--json` prints `{"apply": bool, "results": [...]}` with one result per
repository: `repo`, `policy`, `permission`, `decisions` (one per PR, with
`action` `merge`, `rebase`, `review`, `wait` or `skip`, a `reason`, and a
`verdict` with `approved`, `summary`, `concerns` and `error` when the agent
reviewed it), `findings` (`kind`, `summary`, `prs`, `details`,
`suggestions`), `events` (the writes made:
`merged`, `merge-failed`, `rebase-requested`, `rebase-failed`, each with the
PR's `url` and `grade`), `merge_method` (the one merges use) and `error`
when the repository could not be read (`interrupted` if the run was
stopped). Lists are always arrays, never `null`. When stderr is a terminal,
a line per repository reports progress there while the JSON waits for the
end.

## Config

See [`config.example.yaml`](config.example.yaml). Defaults: squash,
patch and minor only, checks required, agent review allowed (it only runs
with `--review`), Claude as the agent. Every field can be overridden per repo.
When a repo does not allow the configured merge method, the first one it
allows (squash, merge, rebase) is used and the report header says so.

To leave a repo out without deleting it, give its entry `skip: true`:

```yaml
repos:
  - repo: acme/Mirror
    skip: true
```

`deps-merger run -r Mirror` still runs a skipped repo when you ask for it
by name. A run that would select no repos at all fails instead of reporting
nothing.
