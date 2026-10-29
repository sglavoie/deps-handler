# How deps-merger behaves

The [README](../README.md) says what deps-merger does; this page holds the
details: every rule a PR must pass, how failures and rate limits are
handled, what the agent can and cannot see, the exit status and JSON
output, and every config rule.

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
  `30m` (and the resolved policy says so). This wait uses the head commit's
  timestamp, regardless of its position in the commit list. If that
  timestamp is missing, the PR waits instead of qualifying for the timeout
  exemption, and is not diagnosed as having no CI yet. A check run counts
  as superseded only by a newer run of the same name from the same workflow, app and
  event, as when concurrency cancels it: a `test` job failing in one
  workflow is not excused by a `test` job passing in another, nor a `push`
  run failing by the `pull_request` run of the same workflow passing, nor
  any run by a newer one that was skipped or neutral, and a
  commit status and a check run never stand in for each other. Both runs
  must identify their name, suite, app, workflow and event before one can
  supersede the other. Checks missing any of these, including checks from
  apps that supply no workflow identity, are all kept;
- GitHub reports it `MERGEABLE` and `CLEAN` (or `HAS_HOOKS`); an
  unrecognized mergeability value blocks both merging and rebase requests;
- nothing was cut off: a PR with more than 100 commits or 100 checks is
  skipped rather than judged on partial data. A PR with no commits to
  verify is also skipped, even if an agent approved it. If the current
  head is missing from the verified commits, the PR waits for a later
  run, even when checks already match that head.

A safe PR that is only `BEHIND` its base branch gets one `@dependabot rebase`
comment per head commit, one PR at a time per target branch: while a PR can
merge, is up to date and waiting on CI, or has a rebase pending, the others
targeting that branch wait (`queued behind #N`). If the base branch must be
up to date, each merge puts
every other PR targeting that branch behind again, so rebasing them all at
once would rerun CI on each for every merge. Independent target branches
have separate queues. A rebase request Dependabot has not acted on within a
day stops holding the queue: that PR is left for a human and the next one is
rebased. A request with the same timestamp as the head commit counts as
pending too: the timestamp cannot establish that it preceded the commit.
Only a PR's latest 50 comments are read: when even the oldest of
them is at least as recent as its head commit, a request may hide among the rest, so
the PR is not asked again but waits as if one were pending, and is left for
a human a day after that oldest comment.
A PR whose merge or rebase request failed also releases the queue for the
rest of that run, even if its checks restart or GitHub recomputes its
mergeability afterward.

Dependabot takes commands only from users with push access, not from a
GitHub App or `GITHUB_TOKEN`. With `dependabot_commands: false`,
deps-merger posts none: a safe PR that is `BEHIND` is skipped, `rebase
left to a human`, and listed under a `behind` finding suggesting
`@dependabot rebase`. Nothing queues behind it, and a major that is behind
is still reported as held for its size. A PR that conflicts needs no
command: Dependabot rebases it on its own.

Merges happen one at a time per repository, with the head SHA pinned,
re-reading state after each. A merge GitHub rejects with 405 or 409, as it
can right after another merge, is retried once. So is one that fails with
a 500, 502, 503, 504, a timeout or a connection closed or reset before GitHub
answered, which may have merged anyway: if the PR is no longer open when
the repository is read again, GitHub is asked whether it merged, and it is
reported merged, with the error GitHub gave, or failed if it was closed
without being merged. A successful HTTP response must explicitly report
`merged: true`; `merged: false` is a failed merge. A missing or malformed
confirmation is reconciled like a timeout, and an unknown or inconsistent
PR state fails the state lookup rather than counting as a closed PR.
GitHub can show such a PR open a
moment longer, so when its retry fails too, the repository is read once
more before the merge is reported failed. A PR that is still open but not
mergeable when it would be retried, after any of these failures, is never
left unreported: as the repository's pass ends, GitHub is asked again, and
the merge is reported merged if the PR merged and failed otherwise; if
GitHub cannot say, it is reported failed, with a note that it may have
merged unless GitHub refused every try with a 405 or 409. A merge in flight
when the run is interrupted is reported failed with `interrupted; may have
merged`, since there is no asking GitHub then, unless GitHub had already
answered it with an error status other than 500, 502, 503 or 504. One the
interruption keeps from its retry, after a failure that may have merged
it, reads `interrupted; may have merged after` that failure. A rebase
request in flight is reported requested, with a warning. Every request to
GitHub times out after a minute, so a hung merge cannot hold up the other
repositories' writes; a merge that times out is handled like a 504. A rebase request that fails
with a 500, 502, 503, 504, a timeout, a broken connection or unreadable JSON
in a successful HTTP response may have been posted all the same: it is
reported requested, with a warning, rather than failed, and a later run
asks again if it was not. A merge or comment GitHub refuses for a rate limit
is made again after the pause GitHub asks for, up to two minutes; other
writes wait meanwhile, since the limit applies to the user. When GitHub
asks for a longer pause, or refuses the write again (`rate-limited again
after waiting 20s`), no further merge or comment is sent in that run, as
writing on through a rate limit is what GitHub penalises: each is reported
failed (`not sent`), and the next run tries again. The merges left in a
repository's pass are then all reported so at once, without reading the
repository again for each. Up to ten repositories are
processed at once (`--parallel`), but merges and comments are never sent
concurrently.

`Retry-After` accepts either seconds or an HTTP date. A date in the past
needs no pause. Invalid values fall back to the other rate-limit signals;
numeric values too large to represent are treated as over the wait cap,
so they cannot overflow into an immediate retry.

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
  set in that `config.toml` is not used: set `codex.model` instead;
- `api`: requests to an OpenAI-compatible chat completions endpoint
  (`api.url`, such as `http://127.0.0.1:8081/v1` for a local
  `llama-server`), with no program run. The model gets the prompt and,
  unless `api.tools: false`, three tools deps-merger implements:
  `list_files`, `read_file` (at most 400 lines at a time) and `search` (a
  regular expression, at most 200 matching lines). Every path they take
  is resolved, symlinks included, and refused unless it stays inside the
  checkout and outside its `.git` (in any case, as macOS ignores it);
  searches do not follow symlinks. With `api.tools: false`, any tool call
  fails the review without reading files or sending their contents. Tool
  calls in the final answer fail the review too. After
  at most `api.max_turns` rounds of tool calls (30 by default), or a reply
  that is not the answer, the model is asked once more for the answer,
  held to its JSON schema. A model that thinks aloud (`<think>`, or only
  `</think>` when its chat template opens the thinking) or fences its
  answer is read past that leading reasoning and the surrounding fence;
  what remains must be a single JSON object. Tags inside JSON strings
  remain literal text. Arrays, surrounding prose, reasoning after the
  answer and multiple JSON values require a fresh
  answer; an invalid final answer fails the review. The key, if the
  endpoint wants one, comes from the variable `api.key_env` names, never
  from the config. A local
  server usually answers one request at a time (`llama-server -np 1`):
  run with `--review-parallel 1`, or the reviews queue behind each other
  and the wait counts against each one's `timeout`. A local 27B model
  takes a few minutes per review.

All three agents' verdicts must contain every field in the requested
schema, with the expected types and no duplicate or unknown fields.
Malformed CLI verdicts fail the review; the API agent requests a fresh
answer once, then fails if that answer is malformed too.

Neither gets a GitHub token: `GH_TOKEN`, `GITHUB_TOKEN`,
`GH_ENTERPRISE_TOKEN` and `GITHUB_ENTERPRISE_TOKEN` are removed from the
agent's environment. For the CLI agents, read-only is not confined to the
checkout, though: Codex's sandbox lets it read any file your user can, such as
`~/.config/gh/hosts.yml` (where `gh` keeps its token when no keyring
holds it) or SSH keys, and whatever an agent reads goes to its model
provider. On Linux that includes the environment of your other processes
under `/proc`: deps-merger's own, with `GH_TOKEN` or `GITHUB_TOKEN` if
you set one, and that of a git fetching another PR's checkout meanwhile
(reviews run four at a time by default), which holds the token deps-merger fetches
with. Something that looks like a GitHub token (`ghp_`, `gho_`,
`ghu_`, `ghs_`, `ghr_`, `github_pat_`) in the agent's summary or
concerns is replaced by `[redacted token]` before it is cached or
reported, and so is one in an agent's error output. This includes stateless
installation tokens whose `ghs_` value contains an App ID and a JWT with
dots, underscores and hyphens. Control characters
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
the run exit 1. If the PR closes, becomes a draft or changes so that its
latest decision no longer carries that failure, the repository's `error`
preserves it, including when a later review succeeds. A review whose time
runs out while it fetches the PR or checks it out says so, rather than
blame the agent, which never ran. The PR's title, dependency names and versions, description
and diff are treated as untrusted: the prompt tells the agent they cannot
change its instructions, and fences them with markers carrying a random
token, so they cannot fake their own end. It says the same of files in the
checkout that come from the dependency, such as vendored code and
lockfiles.

In apply mode, deps-merger posts the verdict's reviewer, summary and
concerns (including breaking changes) on the PR before acting on it.
Approvals say merging still depends on policy checks; rejections explain
why the PR is left for a human. A failed review posts its error instead.
This applies to cached verdicts too, including those produced by a dry
run. Dry runs and report-mode repositories never post these comments.

An identical explanation by the current GitHub user in the latest 50 PR
comments suppresses another post. The comment identifies the update and
target branch; rejections also identify the head, so a new review after
a rebase can post another explanation. Comments posted during the run
are remembered even if GitHub has not returned them yet. Older comments
outside the fetched window cannot suppress a new post.

Review comments use the same serialized, rate-limit-aware writes as
merges. If GitHub cannot confirm the post succeeded, the repository stops
with an error before merging, even when the response timed out and the
comment may have been posted. The next run checks the PR comments before
retrying and can reuse the cached verdict. Reviewer text has tokens and
control characters removed, mentions neutralized, and unusually long
explanations truncated to fit a GitHub comment.

Reviews run after their repository's other merges, four at a time
(`--review-parallel`), at most `--max-reviews` (default 10) per run. A
repository reviews its PRs one after another, so more reviews than
`--parallel` never run at once. Verdicts are kept for 14 days in
`~/.local/state/deps-merger/verdicts.json`, keyed by the PR (ignoring the
case of `owner/name`), the target branch (case-sensitive), and the versions
it moves to: a new version or a change of target branch is always reviewed
again. Keys carry a `v3` marker and quote each dependency name and version,
so embedded separators cannot identify a different update. Older cached
verdicts are not reused: each update needs one fresh review after upgrading.
The key leaves out the agent, so a verdict one agent gave is
reused when another runs. An approval survives a rebase; a
rejection does not, since the base branch the PR now sits on may have
dealt with what the agent objected to. The exception is an update whose
versions Dependabot does not record (a Docker digest, an action pinned by
commit), or records as whitespace only: the head commit stands in for
them in the key, so a rebase has it reviewed again even once approved.
A verdict that cannot be saved is
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
`owner/name`, or one update as `verdicts` lists it) has those updates
reviewed again on the next run; anything else is a usage error (exit 2).
An update target matches the full key exactly, including the case of its
branch, dependencies and versions; only the repository name ignores case.
Both complete the repositories and PRs that have verdicts. Only one
`--review` run works at a time: a second one, such as an overlapping cron
job, fails at once rather than share the checkouts. The checkout lock lives
beside them at `$XDG_CACHE_HOME/deps-merger/checkouts.lock` (by default
`~/.cache/deps-merger/checkouts.lock`), so separate state directories cannot
allow concurrent reviews of the same checkout. The state directory's
`review.lock` is also held for compatibility with earlier versions.
Each `--review` run
removes the checkouts of repositories not in its config that no review
used for 30 days, so runs with different configs can share them; one it
cannot remove is reported as a warning, and the run goes on.

Agent review needs Linux or macOS, for file locks and process groups:
elsewhere `--review` fails before reviewing anything. `deps-merger
verdicts` works everywhere.

Configure the agent under `agent:` in the config. `claude.config_dir` is
passed as `CLAUDE_CONFIG_DIR`, picking the Claude Code account and
settings to use; `codex.home` is passed as `CODEX_HOME`. Left unset, as
in the example, the agent gets whichever your environment sets. Set
`agent_review: false` on a repo to keep the agent out of it; its report
header then says `no agent review`. When every repo a run selects sets it,
`--review` says so and runs without the agent, which need not be
installed. `--max-reviews` or `--review-parallel` without `--review` or
`--agent` is an error.

## Scripting

A dry run never writes to GitHub, so `deps-merger run` is safe to
schedule. With `--review` it still runs the agent, which costs what a
review costs, and caches its verdicts: a later `--apply` merges what a dry
run's review approved without asking the agent again. For
`deps-merger run --apply` from cron or CI, the exit status is:

- `0`: every repository was processed and every write succeeded;
- `1`: a repository could not be processed, a merge or rebase request
  failed, or an agent review could not be done (the report says which).
  Failure to write the report also exits 1, in text, Markdown or JSON;
  writes to GitHub may still have succeeded.
  A read GitHub fails with a 500, 502, 503 or 504, a timeout, a connection
  closed or reset before it answered, or "Something went wrong" is tried
  once more before the repository counts as failed,
  as is one refused for a rate limit that lifts within two minutes (for
  GitHub's GraphQL limit, deps-merger asks GitHub when it resets, and does
  not wait when it cannot tell);
- `2`: the command line is wrong (an unknown command or flag, a missing
  argument, `--json` with `--markdown`, `--max-reviews` or `--review-parallel` without `--review`, a
  `--parallel` or `--review-parallel` below 1, an empty `--repo`, or one
  the config does not list or lists under several owners, an empty
  `--owner`, one written `owner/name`, or one no configured repo has), and
  nothing runs; all but the repo and owner names are checked before the
  config is read, so
  a mistake there is reported even when the config is missing;
- `130`: interrupted (Ctrl-C, `SIGTERM` or `SIGHUP`). A second Ctrl-C
  kills deps-merger at once, should the first not stop it.

`--json` prints `{"apply": bool, "results": [...]}` with one result per
repository: `repo`, `policy`, `config_files` (the policy files read
from GitHub, as in [Policy files in the repositories](#policy-files-in-the-repositories)), `permission`, `decisions` (one per PR, with
`action` `merge`, `rebase`, `review`, `wait` or `skip`, a `reason`,
`dependencies` (`name`, `version`, `type` and `group`, empty strings for
what Dependabot did not record, and `update_type`: `patch`, `minor`,
`major`, or `unknown` when it recorded no semver bump), `failed_checks`, and a
`verdict` with `approved`, `summary`, `concerns` and `error` when the agent
reviewed it), `findings` (`kind`, `summary`, `prs`, `details`,
`suggestions`), `events` (the writes made:
`merged`, `merge-failed`, `rebase-requested`, `rebase-failed`, each with the
PR's `url` and `grade`), `merge_method` (the one merges use; it and
`permission` are empty strings when the repository could not be read),
`warnings` (what went wrong without stopping the run), and `error`
when the repository could not be read (`interrupted` if the run was
stopped), or a failed review is no longer present in its latest decisions.
The text report of a repository that failed part way still lists
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

`run --markdown` prints each repository's section as GitHub Markdown, as
an issue body or a GitHub Actions job summary would hold it: a heading,
the policy line, the files the policy came from, a table of PRs with
their status and reason, warnings, and the findings under "Needs
attention", with Dependabot commands as code to copy. Text that comes
from elsewhere, such as PR titles and reasons quoting GitHub or an
agent, is escaped: it cannot change the layout, and an `@` in it is
followed by a zero-width space so that it mentions no one. Only
`https://github.com/` URLs are linked. No summary line follows.

On a terminal the report is colored: green for merges, cyan for rebases,
yellow for waits and what needs attention, magenta for agent reviews, red
for failures and major bumps. `NO_COLOR=1` or `CLICOLOR=0` turns color off;
`CLICOLOR_FORCE=1` keeps it when the output is piped and wins over
`CLICOLOR=0`, but `NO_COLOR` always wins, as [no-color.org](https://no-color.org)
asks.

## Config

The config lives at `~/.config/deps-merger/config.yaml`, or under
`$XDG_CONFIG_HOME` when that is set; `-c` names another file.
`deps-merger init` writes the commented example there (or to the file
`-c` names) and never overwrites one that exists, unless `--force` moves
it aside first, to a `.bak` file named after the time. Its repos are
commented out, so no command runs until you list yours: until then each
says which file lists no repos. `--discover` fills them in instead with
every repository your `gh` login can write to that holds a Dependabot
config (`.github/dependabot.yml` or `.yaml` on its default branch),
leaving out archived repositories and forks; with `--owner`, only those
of the named owners. `--stdout` prints the config rather than write it.
See [`config.example.yaml`](../config.example.yaml). Defaults: squash,
patch and minor only, checks required, agent review allowed (it only runs
with `--review`), Claude as the agent. Every policy field under `defaults`
(`merge_method`, `max_update_type`, `require_checks`, `agent_review`,
`dependabot_commands`, `merge_without_checks_after`, `mode`) can be overridden per repo; the `agent`
settings apply to every repo.
`mode` is `merge` (the default) or `report`: a repo in report mode is
only reported on, as in a dry run, even by `run --apply`, and its header
says `report only`.
When a repo does not allow the configured merge method, the first one it
allows (squash, merge, rebase) is used and the report header says so.

### Policy files in the repositories

A run reads two more files for each repo, both on the default branch:

- `deps-merger.yml` in the owner's `.github` repository (`acme/.github`),
  for every repo of that owner, read once per owner per run;
- `.github/deps-merger.yml` in the repo itself.

Each holds policy fields only, the ones `defaults` takes, and is checked
as strictly as the config. They are layered, each overriding the one
before: the built-in defaults, your `defaults`, the owner's file, the
repo's file, then your entry for the repo. A file that is missing, or in
a repository the token cannot see, is skipped. An empty file sets nothing.
A file that is invalid (an unknown field, a bad value or none, a second YAML
document, a recursive merge alias, a directory, a binary or a file too
large), or that cannot be read, fails that repo: nothing in it is judged
or written. The report
names the files used under the repo's header (`policy from ...`), and
`run --json` lists them in `config_files`. `deps-merger repos` reads
nothing from GitHub and shows the policy from your config alone.

To leave a repo out without deleting it, give its entry `skip: true`:

```yaml
repos:
  - repo: acme/Mirror
    skip: true
```

An empty `skip:` value, `null` or `~` is rejected rather than treated as
`false`. Use an explicit boolean or omit the field to include the repository.

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

`deps-merger repos` takes `-r` and `--owner` as `run` does, and lists the
repos they select, skipped ones included (its `SKIP` column says which).
Both flags complete the configured names in a shell that loads
`deps-merger completion` (bash, zsh, fish or PowerShell).

Repos are written `owner/name`, as GitHub allows them: letters, digits and
hyphens in the owner; letters, digits, `-`, `_` and `.` in the name, with no
`.git` suffix. `merge_without_checks_after` set where `require_checks` is
false (in the same entry, or a repo setting it under defaults that turn
checks off) is an error, since it would do nothing; a repo that turns
checks off while the defaults set it simply does not use it, and its
resolved policy shows `0s`. An error in a repo entry gives its line in
the config. The config must be a single YAML document, an empty entry
under `repos` is an error rather than skipped, and
`claude.max_budget_usd` must be a finite number. `claude.config_dir` and
`codex.home` must be absolute or start with `~/`: a relative path would
depend on the directory deps-merger runs from, which under cron is rarely
the config's. A repo entry can borrow another's settings with a YAML merge
key, and the merged fields are checked like any other. Recursive merge
aliases are rejected:

```yaml
repos:
  - &strict {repo: acme/Billing, max_update_type: patch}
  - {<<: *strict, repo: acme/Payments}
```
