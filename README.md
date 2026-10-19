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

- opened by Dependabot, and every commit is authored by Dependabot (a PR
  anyone else pushed to is left alone);
- not a draft; the repository is not archived and you have write access;
- every dependency in the commit metadata has a known semver bump, and the
  largest one is within `max_update_type` (a grouped PR is as risky as its
  riskiest bump);
- every check on the head commit passed, and at least one ran unless the
  repo sets `require_checks: false`;
- GitHub reports it `MERGEABLE` and `CLEAN` (or `HAS_HOOKS`).

A safe PR that is only `BEHIND` its base branch gets one `@dependabot rebase`
comment per head commit. Merges happen one at a time per repository, with
the head SHA pinned, re-reading state after each.

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
- **no checks** and **read-only** repositories.

## Config

See [`config.example.yaml`](config.example.yaml). Defaults: squash,
patch and minor only, checks required. Every field can be overridden per repo.
