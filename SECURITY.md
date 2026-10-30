# Security

deps-handler merges pull requests with your GitHub credentials and can hand
untrusted PR content to an AI agent, so a flaw in it can matter. Examples
worth reporting:

- a PR that anyone but Dependabot wrote to, or one whose size or checks
  are misjudged, getting merged;
- PR content (title, release notes, diff, files in the checkout) steering
  the agent's verdict past the fences around it, or escaping its
  read-only sandbox;
- a GitHub token reaching the agent, its output, the verdict cache or
  the report.

`pr diagnose` uses the same read-only agents as reviews. `pr fix` is an
explicit writable operation: its agent edits a checkout and can run repository
commands. Repairs commit locally and retain the checkout by default; `--push`
authorizes publication and GitHub CI verification. Codex uses its workspace-write sandbox; Claude
permits Bash and editing. This is a broader execution boundary than review.
The parent process supplies GitHub authentication only to Git network commands,
checks the selected PR again before publishing, and uses a head-pinned lease.
With `--working-directory`, the agent runs in the selected existing checkout
under a repair lock; its HEAD and clean state are validated before editing,
and the directory is never reset, cleaned or deleted. Repair never authorizes
merging or bypassing branch protection.

## Reporting

Please report vulnerabilities privately through
[GitHub's private vulnerability reporting](https://github.com/sglavoie/deps-handler/security/advisories/new),
not in a public issue. Include the version (`deps-handler --version`), what
you ran, and what happened. You can expect an answer within a week.

Only the latest release is supported with fixes.
