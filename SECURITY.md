# Security

deps-merger merges pull requests with your GitHub credentials and can hand
untrusted PR content to an AI agent, so a flaw in it can matter. Examples
worth reporting:

- a PR that anyone but Dependabot wrote to, or one whose size or checks
  are misjudged, getting merged;
- PR content (title, release notes, diff, files in the checkout) steering
  the agent's verdict past the fences around it, or escaping its
  read-only sandbox;
- a GitHub token reaching the agent, its output, the verdict cache or
  the report.

## Reporting

Please report vulnerabilities privately through
[GitHub's private vulnerability reporting](https://github.com/sglavoie/deps-merger/security/advisories/new),
not in a public issue. Include the version (`deps-merger --version`), what
you ran, and what happened. You can expect an answer within a week.

Only the latest release is supported with fixes.
