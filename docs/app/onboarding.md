# GitHub App onboarding

What to send each organization invited to use the free App, and how to
follow up. Replace `<app-slug>` and the contact details with real ones.

## The invitation

> **Subject: Trying deps-merger on your Dependabot PRs**
>
> Hi <name>,
>
> Thanks for using deps-merger. It's a free GitHub App that reads
> your Dependabot pull requests and, once you let it, merges the safe ones
> for you. A PR is safe when Dependabot alone wrote it, its checks are
> green, and it is a patch or minor update. It also tells you what is
> blocking the rest: a check that's broken on your main branch, an update
> to ignore, a review your branch protection wants.
>
> 1. Install it on a few repositories that get Dependabot PRs:
>    https://github.com/apps/<app-slug>/installations/new
>    GitHub first offers your personal account: pick your **organization**
>    instead, then "Only select repositories". You can add more later.
> 2. Within a few minutes each repository gets a pinned **Dependency
>    health** issue. Until you say otherwise, that's all it does. It
>    changes nothing else.
> 3. When the report looks right to you, let it merge in a repository by
>    adding `.github/deps-merger.yml` on the default branch:
>
>    ```yaml
>    mode: merge
>    ```
>
>    The same file can tighten the policy, for example
>    `max_update_type: patch`, or `require_checks: true` (the default).
>
> What it reads, stores and writes is in the attached one-pager. By
> default it does not clone your code or send it to a model; optional
> agent review is explained there. It never comments or pushes, and
> uninstalling deletes what it kept about your repositories.
>
> If anything looks wrong, reply here and I'll pause it for your
> organization straight away.
>
> <signature>

Attach [the data-handling note](data-handling.md).

## Before sending

- The production App exists, it's public, and its homepage and
  description are set (`go run ./cmd/register -public -name <name>`).
- The server runs where GitHub's deliveries can reach it, with
  `DMC_ALLOW_MERGE=true` and no `DMC_REPOS`.
- `go run ./cmd/admin status` shows the server's database is the one in
  use.

## Follow-up

Run `go run ./cmd/admin metrics` first and bring the organization's
line. Ask:

1. Is the Dependency health issue right? Anything it got wrong, or
   missed?
2. (In report mode.) What would make you comfortable turning on
   `mode: merge`? (In merge mode.) Did it merge anything you wouldn't
   have?
3. What did you do this week about the PRs it couldn't merge? Did the
   "needs attention" items tell you what to do?
4. Did it save anyone time? Whose, roughly how much?
5. Anything that annoyed you: noise, the pinned issue, timing?

Note the answers next to the organization's metrics and act on reports
of incorrect decisions or unwanted noise.
