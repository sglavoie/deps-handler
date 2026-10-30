# sglavoie/agent-session-loop#1: chore(deps): Bump actions/setup-python from 6.3.0 to 7.1.0

PR: https://github.com/sglavoie/agent-session-loop/pull/1
Head: `884d93425e6d454ea3c929181555ed03a1d05e7e`
Base: `main` (`6ed7627b1bd696d2f49ccc72d13fb935e5335156`)

Checks: not confirmed passing.

## Checks

- Workbench browser (Playwright): failed (https://github.com/sglavoie/agent-session-loop/actions/runs/40371226856/job/121742179377)
- Linux (portable, Python 3.14): passed (https://github.com/sglavoie/agent-session-loop/actions/runs/40371226864/job/121742179900)
- codex latest: passed (https://github.com/sglavoie/agent-session-loop/actions/runs/40371226828/job/121742179683)
- Linux (portable, Python 3.15): failed (https://github.com/sglavoie/agent-session-loop/actions/runs/40371226864/job/121742180386)
- codex previous: passed (https://github.com/sglavoie/agent-session-loop/actions/runs/40371226828/job/121742179691)
- claude latest: passed (https://github.com/sglavoie/agent-session-loop/actions/runs/40371226828/job/121742179710)
- claude previous: passed (https://github.com/sglavoie/agent-session-loop/actions/runs/40371226828/job/121742179695)
- copilot latest: passed (https://github.com/sglavoie/agent-session-loop/actions/runs/40371226828/job/121742179819)
- copilot previous: passed (https://github.com/sglavoie/agent-session-loop/actions/runs/40371226828/job/121742179560)
- Linux (portable Workbench, Python 3.13): passed (https://github.com/sglavoie/agent-session-loop/actions/runs/40371226864/job/121742180349)
- upstream drift (advisory): passed (https://github.com/sglavoie/agent-session-loop/actions/runs/40371226828/job/121742179200)
- macOS (full 1/4): passed (https://github.com/sglavoie/agent-session-loop/actions/runs/40371226864/job/121742180144)
- macOS (full 2/4): passed (https://github.com/sglavoie/agent-session-loop/actions/runs/40371226864/job/121742180545)
- macOS (full 3/4): failed (https://github.com/sglavoie/agent-session-loop/actions/runs/40371226864/job/121742180285)
- macOS (full 4/4): passed (https://github.com/sglavoie/agent-session-loop/actions/runs/40371226864/job/121742180412)
- GitGuardian Security Checks: passed (https://dashboard.gitguardian.com)

## Assessment

PR #1 only changes the pinned `actions/setup-python` SHA, from v6 to 6d4fd22 (v7.1.0), in `.github/workflows/check.yml`, `harness-compatibility.yml` and `workbench-browser.yml`. It is MERGEABLE but UNSTABLE: all 16 checks completed, 13 passed and 3 failed. None are pending, missing or skipped at the job level. In all three failed jobs the new setup-python step itself succeeded (CPython 3.15.2 and 3.14.8 installed). There is no evidence that the action bump caused any of the failures.
1. 'Linux (portable, Python 3.15)' fails deterministically. mypy reports [override] errors in `agent_session_loop/cli_parser_support.py`. One code change is needed there.
2. 'macOS (full 3/4)' has one failed assertion in a timing-sensitive test. It looks like a flake.
3. 'Workbench browser (Playwright)' has 3 of 435 WebKit tests failing on the same test. That test passed in one of its WebKit runs, which suggests a flake or timing problem rather than a regression.
I could not see whether these jobs pass on main, which is the main evidence gap.

## Merge blockers

1. merge state: UNSTABLE
2. checks are not confirmed passing

## Root causes

1. Linux (portable, Python 3.15), failed, deterministic: `just lint`, then mypy, fails with 2 errors. `agent_session_loop/cli_parser_support.py:384` defines `def format_usage(self) -> str` and `:388` defines `def format_help(self) -> str`. The log shows the 3.15 supertype in mypy's typeshed is `format_usage/format_help(self, formatter: HelpFormatter | None = ...) -> str`, so these narrower overrides are flagged [override]. `pyproject.toml` `[tool.mypy]` sets no `python_version`, so mypy checks against the interpreter's version (3.15.2). The 3.14 lane passes the same lint, which is consistent with the parameter existing only in 3.15 stubs. This is a type-level incompatibility with Python 3.15, not a result of the setup-python bump. The lane runs Python 3.15 with or without the bump (`check.yml:47` matrix `["3.14", "3.15"]`).
2. macOS (full 3/4), failed, likely flaky (inferred, not proven): 1 of 170 shards failed. `tests/test_asl_harness_process_sweep_recovery.py:353` `assertEqual(1, len(suspected))` got 2 `TURN_HANG_SUSPECTED_EVENT` records. The test sets very tight timings (`ASL_HANG_RESULT_GRACE_SECONDS=0.05`, `ASL_HANG_CONFIRM_SAMPLES=1`, heartbeat_interval 0.05, lines 316-328), so a slow macOS arm64 runner can plausibly raise a second hang suspicion. The other two tests in the shard passed and Python setup succeeded (3.14.8). Nothing in the diff touches this code path. The full shard log is in artifact run-suite-logs-macos-3, which I have not read.
3. Workbench browser (Playwright), failed, likely flaky or WebKit timing (inferred): 3 failed and 432 passed. All 3 failures are `tests/workbench_browser/interactions.spec.js:239` 'an unanswered-question run link opens the live reply form', failing at line 407 `expect(form).toBeVisible({ timeout: 2500 })` with `#wb-reply-form` still `hidden`. They are desktop-webkit local, desktop-webkit remote and mobile-webkit local. The same test passed on mobile-webkit remote after 15.6s and on all desktop-chromium runs. Line 407 has a much shorter timeout than the elapsed time of that passing run, which points to a slow exact-question read rather than broken logic. setup-python here only provides `python3 fixture_server.py` (`workbench-browser.yml:73-77`), and no assertion relates to Python. The screenshots and traces (`.artifacts/results/...-webkit/trace.zip`) have not been inspected, so the root cause is unconfirmed.

## Repair steps

1. 1. Check out branch `dependabot/github_actions/actions/setup-python-7.1.0` at head 884d934, or fix on main and rebase the PR. The mypy fix is independent of the action bump.
2. 2. Edit `agent_session_loop/cli_parser_support.py:384-390` so the overrides accept the optional formatter argument that the 3.15 stubs declare. Then pass it through only when given, so the code still runs on 3.13 and 3.14, where `ArgumentParser.format_usage()/format_help()` take no argument. Suggested shape: `def format_usage(self, *args: Any, **kwargs: Any) -> str:` with `with self._with_display_groups(): return super().format_usage(*args, **kwargs)`, and the same for `format_help`. Import `Any` from typing if it is not already imported. A `*args/**kwargs` override is compatible with any supertype signature, so both the 3.14 and 3.15 stubs accept it. Do not add a required or defaulted `formatter` parameter that is always forwarded: that breaks at runtime on 3.14.
3. 3. Do not change any workflow file. The setup-python pin and its `# v7.1.0` comment are correct as Dependabot wrote them.
4. 4. For macOS (full 3/4): first re-run the failed job. If `test_post_result_leaf_recovery_advances_and_releases_lock` fails again, download artifact run-suite-logs-macos-3 and check whether both hang_suspected events belong to the same turn. Only then consider loosening the assertion at `tests/test_asl_harness_process_sweep_recovery.py:353`, or raising `ASL_HANG_CONFIRM_SAMPLES`/grace. Do not edit it before there is evidence.
5. 5. For Workbench browser (Playwright): re-run the failed job. If the test at line 239 fails again, open the WebKit trace.zip for the failing projects before changing anything. One option at that point is raising the 2500ms timeout at `tests/workbench_browser/interactions.spec.js:407`, but only if the trace shows the form simply renders late.

## Validation

1. python3.15 -m pip install --group dev && python3.15 -m mypy   # expect: no errors in agent_session_loop/cli_parser_support.py
2. python3.14 -m pip install --group dev && just lint   # 3.14 lane must stay green
3. python3.13 -c 'import agent_session_loop.cli_parser_support'   # 3.13 portable lane import check
4. python3 -m unittest tests.test_asl_harness_process_sweep_recovery   # repeat several times on macOS to judge flakiness
5. env ASL_CLI_TRACE=0 python3 -m tests.run_suite --all --slice 3/4
6. just test-browser   # or: npx playwright test tests/workbench_browser/interactions.spec.js -g 'unanswered-question run link' --project=desktop-webkit --project=mobile-webkit --repeat-each=5
7. gh pr checks 1 --repo sglavoie/agent-session-loop   # after push or re-run: all 16 checks passed, merge_state CLEAN

## Remaining work / evidence gaps

1. Evidence gap: I had no CI results for main (base 6ed7627). So I cannot confirm whether the 3.15 mypy failure, the sweep-recovery test or the WebKit reply-form test also fail without this PR. The mypy failure looks independent of the action version, but that is inferred.
2. Evidence gap: the Playwright log is truncated at the head, and I did not inspect its screenshots, traces or error-context.md. The macOS shard's full log artifact was also not read. Calling the 'flaky' failures flakes is inference from timing and from the test passing on another project, not proof.
3. Evidence gap: the only source for the 3.15 typeshed signature is the mypy note in the log. I did not check it against a local 3.15 interpreter or mypy 2.3.1's bundled stubs.
4. Merge blockers after the fixes: review_decision is empty. Whether a review or specific required checks are enforced is unknown, because I could not see branch-protection rules.
5. GitGuardian, the harness-compatibility jobs (codex/claude/copilot latest and previous), upstream drift (advisory), Linux 3.14, Linux portable Workbench 3.13 and macOS 1/4, 2/4 and 4/4 all passed. They need no action.

## Next step

Save this Markdown report as `repair.md` and review or edit its repair steps and validation commands. Then run:

```sh
deps-handler pr fix sglavoie/agent-session-loop#1 --plan repair.md --agent claude
```

Use the path where you saved this report in place of `repair.md`. Choose an installed repair agent (`codex` or `claude`); API models support diagnosis only. This command edits and commits locally for inspection. Add `--push` to publish the repair and verify GitHub CI. Review and merge the repaired PR yourself after its checks and remaining gates pass.

