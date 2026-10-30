# tradition/LogScanner#121: chore(deps-dev): bump the non-major group across 1 directory with 3 updates

PR: https://github.com/tradition/LogScanner/pull/121
Head: `8df28fdfef2044d80eb239fe16654e838611a36e`
Base: `main` (`c955118ba9db39cab86218c376981b64f3d9beff`)

Checks: not confirmed passing.

## Checks

- Worker gateway: failed (https://github.com/tradition/LogScanner/actions/runs/40187330757/job/121150794636)
- Workflow policy: passed (https://github.com/tradition/LogScanner/actions/runs/40187330759/job/121150794796)

## Assessment

PR #121 is a Dependabot bump of 3 devDependencies in deployments/cloudflare/github-webhook-gateway. Of its 2 checks, 1 failed and 1 passed. None are pending or missing. "Workflow policy" passed. "Worker gateway" failed at the "High-severity audit gate" step (`pnpm audit --audit-level=high`, .github/workflows/worker-gateway.yml:43-44). It reported 26 vulnerabilities (4 low, 14 moderate, 8 high). Install, typecheck and tests all passed (17/17 tests), so the bumped code itself works. The cause is old transitive packages. `@cloudflare/vitest-pool-workers@0.23.0` still pulls in its own `wrangler@4.124.0` and `miniflare@5.20260815.0-alpha`, which bring vulnerable `sharp@0.35.2` and `undici@7.29.0`. On top of that, `zod@4.4.3`, `vite@8.1.4`, and even the new `undici@7.29.1` from `wrangler@4.151.3` are below the patched versions. A code change is needed: pin patched versions with pnpm overrides, or move to the renamed `@cloudflare/vitest-plugin`. The PR cannot merge until this check passes (merge state UNSTABLE).

## Merge blockers

1. merge state: UNSTABLE
2. checks are not confirmed passing

## Root causes

1. Failed check 'Worker gateway' (job 121150794636): the 'High-severity audit gate' step (`pnpm audit --audit-level=high`) exited with code 1. The earlier steps passed: `pnpm install --frozen-lockfile` ('Lockfile is up to date'), `pnpm run typecheck` (no errors), and `pnpm test` (1 file, 17/17 tests). The lockfile and code are fine; only the audit gate failed.
2. Main cause, from the audit 'Paths' and deployments/cloudflare/github-webhook-gateway/pnpm-lock.yaml: `@cloudflare/vitest-pool-workers@0.23.0` still depends on `miniflare: 5.20260815.0-alpha`, `wrangler: 4.124.0`, `zod: 4.4.3` and `esbuild: 0.28.1` (snapshot block for vitest-pool-workers@0.23.0 in the diff). That old miniflare brings in `sharp@0.35.2` and `undici@7.29.0`, which trigger the sharp advisories (fixed in >=0.35.4 and >=0.35.5) and the undici advisories (fixed in >=7.29.1).
3. Not every flagged package comes through the old pool: the undici advisory GHSA-xjj6-f33q-929c affects `>=7.18.0 <7.30.1`, and the new direct path `wrangler@4.151.3 > miniflare@5.20261019.1-beta > undici@7.29.1` is also vulnerable. So bumping the pool alone would not clear the gate.
4. `vite@8.1.4` (reached via vitest@4.1.11 and @vitest/mocker) matches GHSA-587h-7wmj-h5pm (vulnerable range >=8.0.0 <=8.1.5, fixed in >=8.1.6). `zod@4.4.3` (via the pool) matches GHSA-mrhx-c7xc-m3w5 (fixed in >=4.6.6).
5. pnpm-lock.yaml has no `overrides` section, and package.json has no `pnpm.overrides` field. Nothing currently forces transitive packages to patched versions.
6. Context: the lockfile marks `@cloudflare/vitest-pool-workers@0.23.0` as `deprecated: ... renamed to @cloudflare/vitest-plugin`, and the test run prints the same warning, suggesting the codemod `npx @cloudflare/codemods vitest:pool-workers-to-vitest-plugin`. vitest.config.ts:1 imports `cloudflareTest` from `@cloudflare/vitest-pool-workers`. This warning did not fail the check.

## Repair steps

1. 1. Check out the PR branch `dependabot/npm_and_yarn/deployments/cloudflare/github-webhook-gateway/non-major-ae288b8616` (head 8df28fd) and `cd deployments/cloudflare/github-webhook-gateway`. Use pnpm 9.8.0, which package.json `packageManager` pins.
2. 2. Run `pnpm audit --audit-level=high` locally to get the complete list of high findings. The CI log shows 7 high tables but a total of '8 high', so one may be missing from the captured log.
3. 3. Preferred minimal fix: add a `pnpm` block to deployments/cloudflare/github-webhook-gateway/package.json, after `devDependencies`: `"pnpm": { "overrides": { "undici@>=7.0.0 <7.30.1": "^7.30.1", "sharp@<0.35.5": "^0.35.5", "vite@>=8.0.0 <8.1.6": "^8.1.6", "zod@<4.6.6": "^4.6.6" } }`. Add entries for any other high findings from step 2. First confirm each target version exists with `pnpm view undici@^7.30.1 version`, `pnpm view sharp@^0.35.5 version`, `pnpm view vite@^8.1.6 version` and `pnpm view zod@^4.6.6 version`.
4. 4. Run `pnpm install` (without --frozen-lockfile) to regenerate pnpm-lock.yaml. Check that the lock now has an `overrides:` section and that the old versions are gone: `grep -nE '^  (undici@7\.29|sharp@0\.35\.2|vite@8\.1\.4|zod@4\.4)' pnpm-lock.yaml` should print nothing.
5. 5. Alternative or follow-up (bigger change, needs review): migrate to the renamed package with `npx @cloudflare/codemods vitest:pool-workers-to-vitest-plugin`. This updates package.json and vitest.config.ts:1 (`cloudflareTest` import). Then check with `pnpm why miniflare` whether the duplicate wrangler@4.124.0 / miniflare@5.20260815.0-alpha tree is gone. Keep the overrides from step 3 if any high findings remain (undici <7.30.1 will remain via wrangler@4.151.3 regardless).
6. 6. Commit package.json and pnpm-lock.yaml to the PR branch, e.g. `fix(deps): override vulnerable transitive deps for worker gateway audit`. Pushing to a Dependabot branch means Dependabot will stop rebasing it, which is acceptable here.

## Validation

1. cd deployments/cloudflare/github-webhook-gateway && pnpm install --frozen-lockfile
2. pnpm run typecheck
3. pnpm test   # expect 1 file, 17 tests passed
4. pnpm audit --audit-level=high   # must exit 0
5. pnpm why undici sharp vite zod   # each resolved version should be patched (undici>=7.30.1, sharp>=0.35.5, vite>=8.1.6, zod>=4.6.6)
6. pnpm run deploy:staging --dry-run && pnpm run deploy:production --dry-run   # these CI steps never ran in the failed job
7. After pushing: confirm both 'Worker gateway' and 'Workflow policy' show passed on the new head SHA and the merge state is no longer UNSTABLE.

## Remaining work / evidence gaps

1. Check states: 'Worker gateway' failed; 'Workflow policy' passed. None are pending or missing. Inside the failed job, the 'Wrangler dry run (staging)' and 'Wrangler dry run (production)' steps were skipped because the audit step failed first, so the new wrangler@4.151.3 has not been checked with a deploy dry run.
2. Evidence gap: there is no audit run from main (base c955118). The lock context shows the base already used vitest-pool-workers@0.22.0 → wrangler@4.124.0 / miniflare@5.20260815.0-alpha with sharp 0.35.2 / undici 7.29.0, so most findings were probably already on main and not introduced by this PR (it actually upgrades the direct wrangler path to sharp 0.35.5 / undici 7.29.1). This is unconfirmed; main's Worker gateway job may fail too, and other Dependabot PRs touching this directory would hit the same gate.
3. Evidence gap: the log reports '8 high' but only 7 high advisory tables are visible. The 18 low/moderate findings are not listed (they do not block at --audit-level=high).
4. Uncertainty: I have not checked that the override targets (undici ^7.30.1, sharp ^0.35.5, vite ^8.1.6, zod ^4.6.6) exist in the registry, or that miniflare alpha/beta, vitest 4.1.11 and the pool work with them. Tests and dry runs must confirm this.
5. Uncertainty: I don't know whether `@cloudflare/vitest-plugin` has newer transitive dependencies than vitest-pool-workers@0.23.0. Migrating is advisable long-term (the package is deprecated) but is not proven to clear the audit alone.
6. Merge blockers after CI is green: the review decision is empty, so whether an approving review is required depends on branch protection, which was not in the evidence. Mergeable = MERGEABLE, so there are no conflicts.
7. This was a read-only investigation: no commands were run, no installs or audits were done, and no files were changed.

## Next step

Save this Markdown report as `repair.md` and review or edit its repair steps and validation commands. Then run:

```sh
deps-handler pr fix tradition/LogScanner#121 --plan repair.md --agent claude
```

Use the path where you saved this report in place of `repair.md`. Choose an installed repair agent (`codex` or `claude`); API models support diagnosis only. This command edits and commits locally for inspection. Add `--push` to publish the repair and verify GitHub CI. Review and merge the repaired PR yourself after its checks and remaining gates pass.

