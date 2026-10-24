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

## Usage

```bash
mkdir -p ~/.config/deps-merger
cp config.example.yaml ~/.config/deps-merger/config.yaml   # then list your repos
deps-merger repos                 # effective policy per repo (--json too)
deps-merger run                   # dry run: report only
deps-merger run --apply           # merge safe PRs
deps-merger run --open            # only repos with open Dependabot PRs
deps-merger run -r Storefront --json  # one repo, machine-readable
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
  riskiest bump; a commit message holding more than one
  `updated-dependencies:` block, as the release notes Dependabot quotes
  could forge, or none after a `---` line, is of unknown size);
- every check on the head commit passed, and at least one ran unless the
  repo sets `require_checks: false` (a skipped or neutral check blocks
  nothing but does not count as one that ran, so a commit whose checks
  were all skipped has none; a check GitHub marked stale after it sat
  unfinished for 14 days blocks the merge until it is rerun; a commit less
  than 30 minutes old with no checks yet waits for CI to start). With
  `merge_without_checks_after: 1h`, a commit that has gone an hour without
  any check merges anyway, for repos whose CI only runs for some paths; it
  never waits less than those 30 minutes, so a shorter setting acts as
  `30m` (and the resolved policy says so). A check run counts as superseded
  only by a newer run of the same name from the same workflow, app and
  event, as when concurrency cancels it: a `test` job failing in one
  workflow is not excused by a `test` job passing in another, nor a `push`
  run failing by the `pull_request` run of the same workflow passing, and a
  commit status and a check run never stand in for each other;
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
a 502, 503, 504, a timeout or a connection closed or reset before GitHub
answered, which may have merged anyway: if the PR is no longer open when
the repository is read again, GitHub is asked whether it merged, and it is
reported merged, with the error GitHub gave, or failed if it was closed
without being merged. GitHub can show such a PR open a
moment longer, so when its retry fails too, the repository is read once
more before the merge is reported failed. A PR that is still open but not
mergeable when it would be retried, after any of these failures, is never
left unreported: as the repository's pass ends, GitHub is asked again, and
the merge is reported merged if the PR merged and failed otherwise; if
GitHub cannot say, it is reported failed, with a note that it may have
merged unless GitHub refused every try with a 405 or 409. A merge in flight
when the run is interrupted is reported failed with `interrupted; may have
merged`, since there is no asking GitHub then, unless GitHub had already
answered it with an error status other than 502, 503 or 504. One the
interruption keeps from its retry, after a failure that may have merged
it, reads `interrupted; may have merged after` that failure. A rebase
request in flight is reported requested, with a warning. Every request to
GitHub times out after a minute, so a hung merge cannot hold up the other
repositories' writes; a merge that times out is handled like a 504. A rebase request that fails
with a 502, 503, 504, a timeout or a broken connection may have been posted all the same: it is
reported requested, with a warning, rather than failed, and a later run
asks again if it was not. A merge or comment GitHub refuses for a rate limit
is made again after the pause GitHub asks for, up to two minutes; other
writes wait meanwhile, since the limit applies to the user. When GitHub
asks for a longer pause, or refuses the write again (`rate-limited again
after waiting 20s`), no further merge or comment is sent in that run, as
writing on through a rate limit is what GitHub penalises: each is reported
failed (`not sent`), and the next run tries again. The merges left in a
repository's pass are then all reported so at once, without reading the
repository again for each. Up to four repositories are
processed at once, but merges and comments are never sent concurrently.

PRs not opened by Dependabot are never fetched: the query asks GitHub only
for `author:app/dependabot`, and the author is checked again locally.

## Agent review

Some updates are safe in every way the policy can check but their size:
a bump over `max_update_type` (a major one, or a minor one under
`max_update_type: patch`), or an update whose size Dependabot does not
record as semver (a GitHub Action pinned by commit, a Docker
digest). With `--review`, deps-merger asks an AI agent about those, once
everything else about them would let them merge or rebase:

```bash
deps-merger run --review                # dry run: review, merge nothing
deps-merger run --review --apply
deps-merger run --agent codex --apply   # another agent; implies --review
```

The agent gets a shallow checkout of the PR's head commit (kept under
`~/.cache/deps-merger/checkouts`, readable by you alone, and reused), the PR description with
Dependabot's release notes and changelog, and the diff. It looks for
breaking changes upstream and how the repository uses what changed, then
answers `approve` or `reject` with its reasons. It runs read-only:

- Claude Code: `claude -p --restricted --safe-mode --strict-mcp-config`
  with only the Read, Grep and Glob tools, no MCP servers, no settings
  files and none of your customizations: no `CLAUDE.md` (yours, or one in
  the checkout or its vendored code), skills, plugins, hooks, or custom
  commands or agents. It still logs in as you do;
- Codex: `codex exec --sandbox read-only --ephemeral --ignore-user-config
  --ignore-rules`, which can run read-only commands but not write or reach
  the network. It does not load `config.toml` or execpolicy rules, which
  could add MCP servers (which run outside the sandbox) or let commands
  out of it; it still logs in from `CODEX_HOME`, but a model or provider
  set in that `config.toml` is not used: set `codex.model` instead.

Neither gets a GitHub token: `GH_TOKEN`, `GITHUB_TOKEN`,
`GH_ENTERPRISE_TOKEN` and `GITHUB_ENTERPRISE_TOKEN` are removed from the
agent's environment. Read-only is not confined to the checkout, though:
Codex's sandbox lets it read any file your user can, such as
`~/.config/gh/hosts.yml` (where `gh` keeps its token when no keyring
holds it) or SSH keys, and whatever an agent reads goes to its model
provider. On Linux that includes the environment of your other processes
under `/proc`: deps-merger's own, with `GH_TOKEN` or `GITHUB_TOKEN` if
you set one, and that of a git fetching another PR's checkout meanwhile
(reviews run two at a time), which holds the token deps-merger fetches
with. Something that looks like a GitHub token (`ghp_`, `gho_`,
`ghu_`, `ghs_`, `ghr_`, `github_pat_`) in the agent's summary or
concerns is replaced by `[redacted token]` before it is cached or
reported, and so is one in an agent's error output. Control characters
other than newlines and tabs are dropped from both, and the text report
drops them from everything it prints, along with invisible format
characters such as bidirectional overrides and zero-width spaces, so an
answer a prompt injection shaped cannot carry escape sequences to your
terminal or reorder what the report shows.

The checks the agent is shown are the ones the policy judged, without runs
that a newer run of the same check in the same workflow, for the same
event, superseded. An agent is killed along
with every process it started when its review times out or the run is
interrupted (Ctrl-C, `SIGTERM` or `SIGHUP`; on Linux and FreeBSD the
kernel also kills the agent should deps-merger die without the chance),
and whatever it left running is killed when it finishes. A fetch that
fails is tried once more; a checkout that a killed review left broken is
repaired, or made again from scratch, but not one that only failed to
fetch while GitHub cannot be reached. Git LFS files are left as their
pointers.

The checkout's git works on the checkout alone, and runs none of your
hooks: it is made without your `init.templateDir` template, and git runs
there with `core.hooksPath=/dev/null` and `core.fsmonitor=false`.
`GIT_DIR`, `GIT_WORK_TREE`, `GIT_INDEX_FILE` and the other variables that
would point git at another repository (such as `$HOME` under a dotfiles
setup) or add settings from outside your config files
(`GIT_CONFIG_PARAMETERS`, `GIT_CONFIG_COUNT`) are removed from its
environment and the agent's, and the GitHub token variables from both;
git only gets the token deps-merger fetches with. It runs without a
terminal and, unless you set `GIT_SSH_COMMAND` or `GIT_SSH`, with
`GIT_SSH_COMMAND="ssh -o BatchMode=yes"`, which takes precedence over a
`core.sshCommand`: a remote your config rewrites to SSH fails at once
rather than wait on a host key or passphrase prompt until the review
times out.

The agent never merges anything. An approved PR is evaluated again and
merged (or rebased) like any other: Dependabot's signed commits, green
checks, mergeability and the pinned head SHA are all checked again first.
The report names the agent on the merge (`squash, approved by claude`).
If Dependabot pushes to a PR as its review starts, the new head is
evaluated and reviewed instead; a head that moves again is left for the
next run rather than counted as a failed review, and is not reviewed
again in this run however many times it moves. A diff GitHub refuses to
render as too large is left out of the review; one it fails to send, or
will not for a rate limit, is asked for again like any read, and the
review fails if it still cannot be had, rather than be cached without it.
A rejection is reported with the agent's concerns under "needs
attention"; a review that fails or times out leaves the PR alone and makes
the run exit 1. A review whose time runs out while it fetches the PR or
checks it out says so, rather than blame the agent, which never ran. The PR's title, dependency names and versions, description
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
dealt with what the agent objected to. The exception is an update whose
versions Dependabot does not record (a Docker digest, an action pinned by
commit): the head commit stands in for them in the key, so a rebase has
it reviewed again even once approved. A verdict that cannot be saved is
reported as a warning (and under `warnings` in `--json`); a verdict file
that cannot be read is moved aside to `verdicts.json.corrupt-TIMESTAMP`
(with `-2`, `-3`… added should one be found twice in a second),
with a warning, and the cache starts empty, whether that is found as the
run starts (the warning then comes with the first repository's result,
which `--open` does not hide) or when it saves a verdict. `deps-merger verdicts` lists
them, optionally only for some repositories or PRs (exiting 1 when one
matches nothing), with `--json` for their concerns; its `HEAD` column
(`head_oid` in `--json`) is the commit a rejection, or an approval of an
update without versions, holds for, and `any` for other approvals; an
expired verdict is neither listed nor forgotten. `deps-merger verdicts forget acme/Storefront#42` (or a whole
`owner/name`) has those updates reviewed again on the next run. Only one
`--review` run works at a time: a second one, such as an overlapping cron
job, fails at once rather than share the checkouts. Each `--review` run
removes the checkouts of repositories not in its config that no review
used for 30 days, so runs with different configs can share them; one it
cannot remove is reported as a warning, and the run goes on.

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
schedule. With `--review` it still runs the agent, which costs what a
review costs, and caches its verdicts: a later `--apply` merges what a dry
run's review approved without asking the agent again. For
`deps-merger run --apply` from cron or CI, the exit status is:

- `0`: every repository was processed and every write succeeded;
- `1`: a repository could not be processed, a merge or rebase request
  failed, or an agent review could not be done (the report says which).
  A read GitHub fails with a 502, 503 or 504, a timeout, a connection
  closed or reset before it answered, or "Something went wrong" is tried
  once more before the repository counts as failed,
  as is one refused for a rate limit that lifts within two minutes (for
  GitHub's GraphQL limit, deps-merger asks GitHub when it resets, and does
  not wait when it cannot tell);
- `2`: the command line is wrong (an unknown command or flag, a missing
  argument, `--max-reviews` without `--review`, an empty `--repo`, or one
  the config does not list or lists under several owners, an empty
  `--owner`, one written `owner/name`, or one no configured repo has), and
  nothing runs; all but the repo and owner names are checked before the
  config is read, so
  a mistake there is reported even when the config is missing;
- `130`: interrupted (Ctrl-C, `SIGTERM` or `SIGHUP`). A second Ctrl-C
  kills deps-merger at once, should the first not stop it.

`--json` prints `{"apply": bool, "results": [...]}` with one result per
repository: `repo`, `policy`, `permission`, `decisions` (one per PR, with
`action` `merge`, `rebase`, `review`, `wait` or `skip`, a `reason`,
`dependencies` (`name`, `version`, `type`, `group` and `update_type`, empty
strings for what Dependabot did not record), `failed_checks`, and a
`verdict` with `approved`, `summary`, `concerns` and `error` when the agent
reviewed it), `findings` (`kind`, `summary`, `prs`, `details`,
`suggestions`), `events` (the writes made:
`merged`, `merge-failed`, `rebase-requested`, `rebase-failed`, each with the
PR's `url` and `grade`), `merge_method` (the one merges use; it and
`permission` are empty strings when the repository could not be read),
`warnings` (what went wrong without stopping the run), and `error`
when the repository could not be read (`interrupted` if the run was
stopped). The text report of a repository that failed part way still lists
the writes made and warnings raised before it failed. Durations such as `policy.merge_without_checks_after` are written
as in the config (`"45m"`, `"0s"`), and give the value in effect: at least
`30m`, and `"0s"` where checks are not required. Lists are always arrays, never `null`. When stderr is a terminal,
a line per repository reports progress there while the JSON waits for the
end.

`deps-merger repos --json` prints `{"repos": [...], "agent": {...}}`: each
repository's `repo`, `policy` (as in `run --json`) and `skip`, and the
agent's `use`, `timeout`, `claude` (`model`, `config_dir`,
`max_budget_usd`) and `codex` (`model`, `home`) as configured, with an
empty `model` for the agent's default.

`deps-merger verdicts --json` prints `{"verdicts": [...]}`: each verdict's
`update` (its cache key), `verdict` (`approved` or `rejected`),
`approved`, `agent`, `head_oid` (the commit a rejection, or an
approval of an update without versions, holds for, or `any`), `reviewed_at`, `expires`, `summary` and `concerns`.

On a terminal the report is colored: green for merges, cyan for rebases,
yellow for waits and what needs attention, magenta for agent reviews, red
for failures and major bumps. `NO_COLOR=1` or `CLICOLOR=0` turns color off;
`CLICOLOR_FORCE=1` keeps it when the output is piped and wins over
`CLICOLOR=0`, but `NO_COLOR` always wins, as [no-color.org](https://no-color.org)
asks.

## Config

The config lives at `~/.config/deps-merger/config.yaml`, or under
`$XDG_CONFIG_HOME` when that is set; `-c` names another file.
See [`config.example.yaml`](config.example.yaml). Defaults: squash,
patch and minor only, checks required, agent review allowed (it only runs
with `--review`), Claude as the agent. Every policy field under `defaults`
(`merge_method`, `max_update_type`, `require_checks`, `agent_review`,
`merge_without_checks_after`) can be overridden per repo; the `agent`
settings apply to every repo.
When a repo does not allow the configured merge method, the first one it
allows (squash, merge, rebase) is used and the report header says so.

To leave a repo out without deleting it, give its entry `skip: true`:

```yaml
repos:
  - repo: acme/Mirror
    skip: true
```

`deps-merger run -r Mirror` still runs a skipped repo when you ask for it
by name, and `deps-merger run --include-skipped` runs every repo, skipped
or not (with `--owner`, every repo of those owners). A bare name that more than one owner's repo shares (`-r docs` with
both `acme/docs` and `other/docs` listed) is an error; give `owner/name`.
So is an empty name (`-r ""`, `-r "Billing,"`), which a script's unset
variable would otherwise turn into a run of every repo; spaces around a
name are ignored. A run that would select no repos at all fails instead
of reporting nothing.

`deps-merger run --owner acme` runs every configured repo of `acme` (the
owner is matched regardless of case; repeat the flag or separate owners
with commas for more). It leaves out the owner's repos set to `skip: true`,
as a run without filters does; add `-r Mirror` to run one of them too,
or `--include-skipped` to run them all.
`--owner` and `--repo` add up: `--owner acme -r other/docs` runs both. An
owner no configured repo has is an error, as is an empty one or one
written `owner/name`.

Repos are written `owner/name`, as GitHub allows them: letters, digits and
hyphens in the owner; letters, digits, `-`, `_` and `.` in the name, with no
`.git` suffix. `merge_without_checks_after` set where `require_checks` is
false (in the same entry, or a repo setting it under defaults that turn
checks off) is an error, since it would do nothing; a repo that turns
checks off while the defaults set it simply does not use it, and its
resolved policy shows `0s`. The config must be a single YAML document, an
empty entry under `repos` is an error rather than skipped, and
`claude.max_budget_usd` must be a finite number. `claude.config_dir` and
`codex.home` must be absolute or start with `~/`: a relative path would
depend on the directory deps-merger runs from, which under cron is rarely
the config's. A repo entry can borrow another's settings with a YAML merge
key, and the merged fields are checked like any other:

```yaml
repos:
  - &strict {repo: acme/Billing, max_update_type: patch}
  - {<<: *strict, repo: acme/Payments}
```

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md), and [SECURITY.md](SECURITY.md) to
report a vulnerability.

## License

[MIT](LICENSE)
