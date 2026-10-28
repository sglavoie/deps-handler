# deps-merger App: what it reads, stores and writes

This note describes what the free deps-merger GitHub App does today. If
that changes, this note changes first and installed organizations are told.

## What it reads

Only the repositories you install it on, and only:

- each repository's metadata: name, default branch, which merge methods it
  allows, and whether it is archived;
- its open pull requests opened by Dependabot:
  - their title, state and merge state;
  - their commits' authors and signatures;
  - their check results;
  - their latest 50 comments;
- its policy files: `.github/deps-merger.yml` on the default branch, and
  `deps-merger.yml` in your organization's `.github` repository if the App
  is installed there.

Unless you turn on agent review (below), it does **not** clone your
repositories, read your code, or fetch pull request diffs, and nothing it
reads is sent to an AI model or any third party service.

## Agent review, if you turn it on

Off unless a repository's `.github/deps-merger.yml` says
`agent_review: true`. Then, for an update the policy alone would not
merge (a major, say, or one Dependabot records no size for), an AI model
judges whether it is safe:

- the App checks out the pull request's head commit, shallowly, with its
  installation token;
- the model gets the pull request's description (Dependabot's release
  notes and changelog) and its diff. The production API model has three
  tools to list, read and search that checkout; they cannot reach
  anything outside it, and the model runs no commands. Local CLI agents
  can inspect the checkout in read-only mode;
- it answers approve or reject with its reasons. An approved update still
  goes through every other check before it merges, and only in `mode:
  merge`; a rejected one is listed in the issue with the reasons.

The production model currently runs on a machine of the operator's (an
open-weights model served there), reached from the App's machine over an
encrypted private network, so your code goes to no AI provider. Local
development can instead use Claude Code or Codex; those CLI modes send
review data to their respective providers. Before using either mode for
an organization's repositories, the operator must tell that organization.
If the production deployment changes to a hosted model, this note names
the provider first, and agent review stays off until you turn it on again.

The checkout and the verdict are kept, on the App's machine, to avoid
reviewing the same update twice: verdicts for 14 days, checkouts until
you uninstall. Each review is recorded with the pull request number, the
verdict and the tokens it used. An installation gets at most a fixed
number of reviews a day.

## Permissions it asks for

| Permission | Access | Why |
| --- | --- | --- |
| Metadata | read | required by GitHub for every App |
| Contents | read and write | read the policy file; merge pull requests |
| Pull requests | read and write | read Dependabot's pull requests; merge them |
| Issues | read and write | keep the "Dependency health" issue |
| Checks, Commit statuses | read | tell whether a pull request's CI passed |
| Actions | read | tell reruns of a check apart from the original |
| Workflows | read and write | merge Dependabot updates to `.github/workflows` |

It subscribes to pull request, check suite and push events, so it can
re-check a repository when one of its Dependabot pull requests changes.

## What it writes to GitHub

- **One "Dependency health" issue per repository**, opened by the App the
  first time it has something to report. The issue is pinned when the
  repository has room, edited only when its content changes, and closed
  when there is nothing left to report.
- **Merges, only where you turn them on** with `mode: merge` in
  `.github/deps-merger.yml`. Without that line the App only reports. A
  merge happens only when Dependabot alone wrote the pull request, its
  checks are green and the update is within your policy (patch and minor
  by default). The App merges one pull request at a time, pinned to the
  commit it checked.

It never comments, pushes commits, approves pull requests, or changes
settings.

## What it stores, and where

The service currently runs on a single machine operated by the
maintainer of deps-merger, with its database on that machine; only agent
review, when you turn it on, also uses the model's machine (above). GitHub's
webhook deliveries reach it through a Cloudflare tunnel, which means they
pass through Cloudflare's network on the way.

For each installation, it stores:
- its ID and account name, and when it was added and removed;
- per repository:
  - its name;
  - whether it is in report or merge mode;
  - the number of its "Dependency health" issue;
- per Dependabot pull request:
  - its number and title;
  - when it was opened, first and last seen open, and closed;
  - whether the App merged it, someone else did, or it closed unmerged;
- per run:
  - when it ran, and what triggered it;
  - how many pull requests it found;
  - any error message from GitHub;
- every write it made: issue updates and merges, the latter with the pull
  request number and the commit it merged.

Without agent review, it does not store your code, diffs or comments;
with it, it keeps the checkouts described above. It never stores
tokens. The App's access
tokens are short-lived (one hour) and kept in memory only. The server's
logs hold repository names, pull request numbers and error messages; they
stay on the same machine.

## When you uninstall

When you uninstall the App, everything stored about your repositories
(runs, writes, pull requests, modes, issue numbers, reviews, checkouts
and verdicts) is deleted. That
happens as GitHub's uninstall notice arrives, or within four hours if the
notice is missed. What is kept is the installation's ID, account name, and
when it was added and removed, for installation activity metrics.

The "Dependency health" issues stay in your repositories, as GitHub
content you own. Delete them if you like.

Suspending the App instead stops it from running, and keeps its data
until you resume or uninstall it.

## Stopping it

At any time, you can:
- remove `mode: merge` to go back to reports only;
- remove repositories from the installation;
- uninstall the App;
- ask the maintainer to pause it for your organization. Merges stop
  immediately, a run in progress included.
