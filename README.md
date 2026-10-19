# deps-merger

Merges safe Dependabot pull requests across a list of repositories and
reports what needs fixing in each one. It uses your `gh` login.

```bash
just install                      # builds into ~/.local/bin
cp config.example.yaml ~/.config/deps-merger/config.yaml
deps-merger repos                 # effective policy per repo
deps-merger run                   # dry run: report only
deps-merger run --apply           # merge safe PRs
deps-merger run -r Storefront --json  # one repo, machine-readable
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
  repo sets `require_checks: false`;
- GitHub reports it `MERGEABLE` and `CLEAN` (or `HAS_HOOKS`);
- nothing was cut off: a PR with more than 100 commits or 100 checks is
  skipped rather than judged on partial data.

A safe PR that is only `BEHIND` its base branch gets one `@dependabot rebase`
comment per head commit. Merges happen one at a time per repository, with
the head SHA pinned, re-reading state after each. A merge GitHub rejects
with 405 or 409, as it can right after another merge, is retried once.

PRs not opened by Dependabot are never fetched: the query asks GitHub only
for `author:app/dependabot`, and the author is checked again locally.

## What needs attention

Under each repo the report lists, without acting on them:

- **repo-wide failures**: a check failing on every Dependabot PR that ran
  it (and that ran on at least half of them), which points at the base
  branch rather than the updates;
- **incompatible updates**: a PR failing its own checks, with the
  `@dependabot ignore …` command that would stop it coming back;
- **held majors**: green major updates blocked only by policy;
- **no checks** and **read-only** repositories;
- **truncated**: more than 100 open Dependabot PRs, so only the first 100
  were evaluated.

## Scripting

A dry run never writes, so `deps-merger run` is safe to schedule. For
`deps-merger run --apply` from cron or CI, the exit status is:

- `0`: every repository was processed and every write succeeded;
- `1`: a repository could not be processed, or a merge or rebase request
  failed (the report says which);
- `130`: interrupted (Ctrl-C or `SIGTERM`).

`--json` prints `{"apply": bool, "results": [...]}` with one result per
repository: `repo`, `policy`, `permission`, `decisions` (one per PR, with
`action` `merge`, `rebase`, `wait` or `skip` and a `reason`), `findings`
(`kind`, `summary`, `prs`, `suggestions`), `events` (the writes made:
`merged`, `merge-failed`, `rebase-requested`, `rebase-failed`) and `error`
when the repository could not be read. Lists are always arrays, never
`null`.

## Config

See [`config.example.yaml`](config.example.yaml). Defaults: squash,
patch and minor only, checks required. Every field can be overridden per repo.

To leave a repo out without deleting it, give its entry `skip: true`:

```yaml
repos:
  - repo: acme/Mirror
    skip: true
```

`deps-merger run -r Mirror` still runs a skipped repo when you ask for it
by name.
