# sglavoie/zoomflow#10: build(deps): Bump @codemirror/view from 6.43.14 to 6.43.15 in the codemirror group across 1 directory

PR: https://github.com/sglavoie/zoomflow/pull/10
Head: `c2ef60cd07cd9cb370ad397582721a34a5fac70e`
Base: `main` (`247c2b8f3111270eb1a79f0f060914b2e2b04dc2`)

Checks: not confirmed passing.

## Checks

- checks: passed (https://github.com/sglavoie/zoomflow/actions/runs/40665523490/job/122738903723)
- diagrams: passed (https://github.com/sglavoie/zoomflow/actions/runs/40665523529/job/122738901826)
- build: passed (https://github.com/sglavoie/zoomflow/actions/runs/40665523490/job/122738903150)
- update-snapshots: skipped (https://github.com/sglavoie/zoomflow/actions/runs/40665523490/job/122738905205)
- browser: passed (https://github.com/sglavoie/zoomflow/actions/runs/40665523490/job/122739095254)
- engines (chromium 1/4): passed (https://github.com/sglavoie/zoomflow/actions/runs/40665523490/job/122739095673)
- engines (chromium 2/4): passed (https://github.com/sglavoie/zoomflow/actions/runs/40665523490/job/122739095773)
- engines (chromium 3/4): passed (https://github.com/sglavoie/zoomflow/actions/runs/40665523490/job/122739095879)
- engines (chromium 4/4): passed (https://github.com/sglavoie/zoomflow/actions/runs/40665523490/job/122739095755)
- engines (firefox 1/4): failed (https://github.com/sglavoie/zoomflow/actions/runs/40665523490/job/122739095753)
- engines (firefox 2/4): passed (https://github.com/sglavoie/zoomflow/actions/runs/40665523490/job/122739095662)
- engines (firefox 3/4): passed (https://github.com/sglavoie/zoomflow/actions/runs/40665523490/job/122739095738)
- engines (firefox 4/4): passed (https://github.com/sglavoie/zoomflow/actions/runs/40665523490/job/122739095621)
- engines (webkit 1/4): passed (https://github.com/sglavoie/zoomflow/actions/runs/40665523490/job/122739095787)
- engines (webkit 2/4): failed (https://github.com/sglavoie/zoomflow/actions/runs/40665523490/job/122739095696)
- engines (webkit 3/4): passed (https://github.com/sglavoie/zoomflow/actions/runs/40665523490/job/122739095716)
- engines (webkit 4/4): passed (https://github.com/sglavoie/zoomflow/actions/runs/40665523490/job/122739095601)
- GitGuardian Security Checks: passed (https://dashboard.gitguardian.com)

## Assessment

PR #10 is a Dependabot patch bump of @codemirror/view from 6.43.14 to 6.43.15. It changes only apps/web/package.json and pnpm-lock.yaml. All checks have finished: 15 passed, 1 skipped (update-snapshots) and 2 failed (engines firefox 1/4 and engines webkit 2/4). None are pending or missing. Each failure is a single e2e test that failed on its first attempt and on its retry. Neither failure points at CodeMirror. The Firefox one is a context-menu timeout on the diagram stage in tests/e2e/atlas.spec.ts:297. The WebKit one is a download filename where WebKit returned underscores instead of spaces, in tests/e2e/documents.spec.ts:163. The same tests passed on all Chromium shards. I can't prove the bump is unrelated: I have no CI run from main at base 247c2b8 to compare against, and I couldn't open the trace artifacts. Merge state is UNSTABLE only because of these two failed jobs. The PR is MERGEABLE, and there is no review decision.

## Merge blockers

1. merge state: UNSTABLE
2. checks are not confirmed passing

## Root causes

1. FAILED engines (firefox 1/4): tests/e2e/atlas.spec.ts:291 'creates a new linked flow…' timed out after 90s at line 297. It was waiting for getByRole('menu', { name: 'Actions for Exercise' }).getByRole('menuitem', { name: 'New linked diagram…' }). The shapeMenu helper (tests/e2e/atlas.spec.ts:233-236) right-clicks `#viewer .zf-stage [data-shape-id="Exercise"]` and returns the menu without waiting for it. So either the right-click didn't open the menu in Firefox, or the menu had a different accessible name. The menu lives on the D2 SVG stage, not in the CodeMirror editor. The other 60 tests in the shard passed. Root cause is NOT established: the trace (artifact browser-report-linux-firefox-1, test-results/atlas-creates-a-new-linked-0325d-…-firefox/trace.zip) was not available to inspect.
2. FAILED engines (webkit 2/4): tests/e2e/documents.spec.ts:140, assertion at line 163. Expected suggestedFilename 'Checkout plan-root.svg', got 'Checkout_plan-root.svg'. The app's filename sanitiser (apps/web/src/text.ts:13) only replaces /\\:*?"<>| and control characters, not spaces. The download is set through link.download in apps/web/src/export-menu.ts:26. So the underscore most likely comes from WebKit/Playwright rewriting the anchor download name, not from the app. That is an inference: I did not check this against the WebKit build or the trace. CodeMirror 6.43.15 has no part in naming exports.
3. Unknown regression status: there is no evidence about whether these two tests pass on main at 247c2b8. Both failures might already exist on main, be flaky, or come from a browser/runner change (the runner logs show apt upgrades, e.g. libnss3, xvfb). I found nothing linking them to the @codemirror/view bump.
4. SKIPPED update-snapshots: this is expected to be conditional (probably only runs on demand or when visual snapshots fail). It is not a failure, and nothing suggests it blocks the merge.

## Repair steps

1. 1. Don't edit package.json or pnpm-lock.yaml. The dependency diff is consistent: the specifier, the resolution integrity and every snapshot reference were moved to 6.43.15.
2. 2. Re-run only the failed jobs of run 40665523490: `gh run rerun 40665523490 --failed -R sglavoie/zoomflow`. If both pass, the failures were flaky and no code change is needed.
3. 3. Check whether main has the same failures: `gh run list -R sglavoie/zoomflow --branch main --limit 5`, then `gh run view <id> -R sglavoie/zoomflow --log-failed` for the latest one. If main fails the same way, these are existing problems that should be fixed in a separate PR, not here.
4. 4. If the WebKit failure reproduces, don't rely on WebKit's suggestedFilename keeping spaces in tests/e2e/documents.spec.ts:163. For example, replace `.toBe('Checkout plan-root.svg')` with `.toMatch(/^Checkout[ _]plan-root\.svg$/)`. Or, if the app should keep spaces, read the anchor's download attribute instead. First confirm in the trace that link.download was 'Checkout plan-root.svg'.
5. 5. If the Firefox failure reproduces, download artifact browser-report-linux-firefox-1 into a fresh empty directory and open its trace: `pnpm exec playwright show-trace <dir>/test-results/atlas-creates-a-new-linked-0325d-and-links-on-from-its-steps-firefox/trace.zip`. Check whether the context menu opened and what its accessible name was. Only then decide on a fix, such as making shapeMenu in tests/e2e/atlas.spec.ts:233 wait for the shape to be visible or stable before right-clicking and assert the menu is visible before returning it. Don't change app code without evidence from the trace.

## Validation

1. pnpm install --frozen-lockfile
2. pnpm exec playwright test --project=webkit tests/e2e/documents.spec.ts -g "naming a diagram names its exports" --repeat-each=3
3. pnpm exec playwright test --project=firefox tests/e2e/atlas.spec.ts -g "creates a new linked flow" --repeat-each=3
4. pnpm exec playwright test --project=firefox --grep-invert @visual --shard=1/4 --workers=4
5. pnpm exec playwright test --project=webkit --grep-invert @visual --shard=2/4 --workers=4
6. (The e2e jobs download a prebuilt web-dist artifact into apps/web/dist, so build the web app locally first using the build job's command from the workflow under .github/workflows. I did not read the workflow file, so the exact command is unverified.)
7. gh pr checks 10 -R sglavoie/zoomflow  # confirm all required checks are green and merge state is no longer UNSTABLE

## Remaining work / evidence gaps

1. Merge blockers: two FAILED checks, engines (firefox 1/4) and engines (webkit 2/4), which make the merge state UNSTABLE. No checks are pending or missing. update-snapshots is SKIPPED, which I believe is expected and not blocking.
2. Missing evidence: no CI result for main at base 247c2b8, so I can't say whether this PR caused the failures. The Playwright traces and error-context.md weren't inspected. I didn't read the workflow file or branch protection, so I don't know which checks are required. The upstream changelog for @codemirror/view 6.43.15 isn't in the PR body.
3. Uncertainty: the WebKit filename explanation (the browser rewriting spaces) is inferred from apps/web/src/text.ts and export-menu.ts and hasn't been confirmed. The Firefox menu timeout has no confirmed cause.
4. Review: review_decision is empty. If branch protection requires an approval, someone with ADMIN access has to approve before merging.

## Next step

Save this Markdown report as `repair.md` and review or edit its repair steps and validation commands. Then run:

```sh
deps-handler pr fix sglavoie/zoomflow#10 --plan repair.md --agent claude
```

Use the path where you saved this report in place of `repair.md`. Choose an installed repair agent (`codex` or `claude`); API models support diagnosis only. This command edits and commits locally for inspection. Add `--push` to publish the repair and verify GitHub CI. Review and merge the repaired PR yourself after its checks and remaining gates pass.

