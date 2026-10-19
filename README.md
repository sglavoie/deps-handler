# deps-merger

Merges safe Dependabot pull requests across a list of repositories and
reports what needs fixing in each one. It uses your `gh` login.

```bash
just install                      # builds into ~/.local/bin
mkdir -p ~/.config/deps-merger
cp config.example.yaml ~/.config/deps-merger/config.yaml
deps-merger repos                 # effective policy per repo
deps-merger run                   # dry run: report only
deps-merger run --apply           # merge safe PRs
deps-merger run --open            # only repos with open Dependabot PRs
deps-merger run -r Storefront --json  # one repo, machine-readable
deps-merger run --review --apply  # let an AI agent judge majors too
deps-merger verdicts              # the agent's cached verdicts
deps-merger verdicts acme/Storefront --json  # one repo's, with concerns
```

## What counts as safe

A PR is merged only when all of these hold:

- opened by Dependabot, and every commit is authored by Dependabot's own
  GitHub account (a commit whose author only bears Dependabot's name or
  email, or has no author, does not count), committed by GitHub itself
  and signed with GitHub's key, as Dependabot's commits are, and has no
  `Co-authored-by` trailer naming
  anyone else (a PR anyone else pushed to is left alone, even one forging
  Dependabot as the author);
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
  paths. A check run counts as superseded only by a newer run of the same
  name from the same workflow and app, as when concurrency cancels it: a
  `test` job failing in one workflow is not excused by a `test` job passing
  in another, and a commit status and a check run never stand in for each
  other;
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
rebased. Only a PR's latest 50 comments are read: when even the oldest of
them is newer than its head commit, a request may hide among the rest, so
the PR is not asked again but waits as if one were pending, and is left for
a human a day after that oldest comment.

Merges happen one at a time per repository, with the head SHA pinned,
re-reading state after each. A merge GitHub rejects with 405 or 409, as it
can right after another merge, is retried once. So is one that fails with
a 502, 503, 504 or a timeout, which may have merged anyway: if the PR is no
longer open when the repository is read again, it is reported merged, with
the error GitHub gave. GitHub can show such a PR open a moment longer, so
when its retry fails too, the repository is read once more before the
merge is reported failed; if it cannot be read, the merge is reported
failed with a note that it may have merged. A rebase request that fails
with a 502, 503, 504 or a timeout may have been posted all the same: it is
reported requested, with a warning, rather than failed, and a later run
asks again if it was not. A merge or comment GitHub refuses for a rate limit
is made again after the pause GitHub asks for, up to two minutes; other
writes wait meanwhile, since the limit applies to the user. Up to four repositories are
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
- Codex: `codex exec --sandbox read-only --ephemeral`, which can run
  read-only commands but not write or reach the network.

Neither gets a GitHub token: `GH_TOKEN`, `GITHUB_TOKEN`,
`GH_ENTERPRISE_TOKEN` and `GITHUB_ENTERPRISE_TOKEN` are removed from the
agent's environment.

The checks the agent is shown are the ones the policy judged, without runs
that a newer run of the same check in the same workflow superseded. An agent is killed along
with every process it started when its review times out or the run is
interrupted, and whatever it left running is killed when it finishes.
A checkout that a killed review left broken is repaired, or made again
from scratch.

The agent never merges anything. An approved PR is evaluated again and
merged (or rebased) like any other: Dependabot's signed commits, green
checks, mergeability and the pinned head SHA are all checked again first.
The report names the agent on the merge (`squash, approved by claude`).
If Dependabot pushes to a PR as its review starts, the new head is
evaluated and reviewed instead; a head that moves again is left for the
next run rather than counted as a failed review.
A rejection is reported with the agent's concerns under "needs
attention"; a review that fails or times out leaves the PR alone and makes
the run exit 1. The PR's title, dependency names and versions, description
and diff are treated as untrusted: the prompt tells the agent they cannot
change its instructions, and fences them with markers carrying a random
token, so they cannot fake their own end. It says the same of files in the
checkout that come from the dependency, such as vendored code and
lockfiles.

Reviews run after the run's other merges, two at a time, at most
`--max-reviews` (default 10) per run. Verdicts are kept for 14 days in
`~/.local/state/deps-merger/verdicts.json`, keyed by the PR (ignoring the
case of `owner/name`) and the versions it moves to: a new version is
always reviewed again. The key leaves out the agent, so a verdict one
agent gave is reused when another runs. An approval survives a rebase; a
rejection does not, since the base branch the PR now sits on may have
dealt with what the agent objected to. A verdict that cannot be saved is
reported as a warning (and under `warnings` in `--json`); a verdict file
that cannot be read is moved aside to `verdicts.json.corrupt-TIMESTAMP`,
with a warning, and the cache starts empty, whether that is found as the
run starts or when it saves a verdict. `deps-merger verdicts` lists
them, optionally only for some repositories or PRs (exiting 1 when one
matches nothing), with `--json` for their concerns; its `HEAD` column
(`head_oid` in `--json`) is the commit a rejection holds for, `any` for an
approval. `deps-merger verdicts forget acme/Storefront#42` (or a whole
`owner/name`) has those updates reviewed again on the next run. Only one
`--review` run works at a time: a second one, such as an overlapping cron
job, fails at once rather than share the checkouts. Each `--review` run
removes the checkouts of repositories not in its config that no review
used for 30 days, so runs with different configs can share them.

Agent review needs Linux or macOS, for file locks and process groups:
elsewhere `--review` fails before reviewing anything. `deps-merger
verdicts` works everywhere.

Configure the agent under `agent:` in the config. `claude.config_dir` is
passed as `CLAUDE_CONFIG_DIR`, picking the Claude Code account and
settings to use; `codex.home` is passed as `CODEX_HOME`. Set
`agent_review: false` on a repo to keep the agent out of it; its report
header then says `no agent review`. When every repo a run selects sets it,
`--review` says so and runs without the agent, which need not be
installed. `--max-reviews` without `--review` or `--agent` is an error.

## What needs attention

Under each repo the report lists, without acting on them:

- **repo-wide failures**: a check failing on every Dependabot PR that ran
  it (and that ran on at least half of them), which points at the base
  branch rather than the updates;
- **incompatible updates**: a PR failing its own checks, with the
  `@dependabot ignore …` command that would stop it coming back (only for
  PRs Dependabot alone wrote, whose checks ran on their head commit);
- **held majors**: major updates that would merge or rebase but for
  `max_update_type`;
- **held updates of unknown size**: updates that would merge but for
  Dependabot not recording a semver bump (a GitHub Action pinned by commit,
  a Docker digest), when no agent reviewed them;
- **review required**: branch protection wants an approving review, which
  deps-merger never gives;
- **stalled rebases**: a rebase request Dependabot ignored for a day, with
  `@dependabot recreate` as the fix;
- **no checks**, **read-only** and **archived** repositories (no checks
  counts only PRs Dependabot alone wrote, not drafts or archived ones);
- **truncated**: more than 100 open Dependabot PRs, so only the oldest 100
  were evaluated;
- **agent rejected** / **agent failed**: an update the agent judged unsafe,
  with its concerns, or could not review.

## Scripting

A dry run never writes to GitHub, so `deps-merger run` is safe to
schedule. With `--review` it still runs the agent, which costs what a
review costs, and caches its verdicts: a later `--apply` merges what a dry
run's review approved without asking the agent again. For
`deps-merger run --apply` from cron or CI, the exit status is:

- `0`: every repository was processed and every write succeeded;
- `1`: a repository could not be processed, a merge or rebase request
  failed, or an agent review could not be done (the report says which).
  A read GitHub fails with a 502, 503 or 504, a timeout or "Something
  went wrong" is tried once more before the repository counts as failed,
  as is one refused for a rate limit that lifts within two minutes (for
  GitHub's GraphQL limit, deps-merger asks GitHub when it resets, and does
  not wait when it cannot tell);
- `130`: interrupted (Ctrl-C or `SIGTERM`).

`--json` prints `{"apply": bool, "results": [...]}` with one result per
repository: `repo`, `policy`, `permission`, `decisions` (one per PR, with
`action` `merge`, `rebase`, `review`, `wait` or `skip`, a `reason`, and a
`verdict` with `approved`, `summary`, `concerns` and `error` when the agent
reviewed it), `findings` (`kind`, `summary`, `prs`, `details`,
`suggestions`), `events` (the writes made:
`merged`, `merge-failed`, `rebase-requested`, `rebase-failed`, each with the
PR's `url` and `grade`), `merge_method` (the one merges use), `warnings`
when something went wrong without stopping the run, and `error`
when the repository could not be read (`interrupted` if the run was
stopped). Durations such as `policy.merge_without_checks_after` are written
as in the config (`"10m"`, `"0s"`). Lists are always arrays, never `null`. When stderr is a terminal,
a line per repository reports progress there while the JSON waits for the
end.

On a terminal the report is colored: green for merges, cyan for rebases,
yellow for waits and what needs attention, magenta for agent reviews, red
for failures and major bumps. `NO_COLOR=1` or `CLICOLOR=0` turns color off;
`CLICOLOR_FORCE=1` keeps it when the output is piped.

## Config

The config lives at `~/.config/deps-merger/config.yaml`, or under
`$XDG_CONFIG_HOME` when that is set; `-c` names another file.
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
by name. A bare name that more than one owner's repo shares (`-r docs` with
both `acme/docs` and `other/docs` listed) is an error; give `owner/name`.
A run that would select no repos at all fails instead of reporting
nothing.

Repos are written `owner/name`, as GitHub allows them: letters, digits and
hyphens in the owner; letters, digits, `-`, `_` and `.` in the name, with no
`.git` suffix. `merge_without_checks_after` set where `require_checks` is
false (in the same entry, or a repo setting it under defaults that turn
checks off) is an error, since it would do nothing; a repo that turns
checks off while the defaults set it simply does not use it.
