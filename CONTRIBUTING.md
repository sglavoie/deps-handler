# Contributing

Issues and pull requests are welcome. For anything beyond a small fix,
open an issue first so we can agree on the approach.

## Setup

You need Go (the version in [`go.mod`](go.mod)) and
[`just`](https://github.com/casey/just).

```bash
just test    # go test -race ./...
just ci      # Go CI checks: vet (also for Windows), staticcheck, govulncheck, tests, gofmt, go mod tidy
```

CI also builds the Docker image and runs a checkout test inside it. To
check the runtime's Git dependencies locally:

```bash
docker build -t deps-handler:test .
sh scripts/test-container.sh deps-handler:test
```

## Trying changes without touching real repositories

The `demo` build tag swaps GitHub and the AI agent for a make-believe
version with fictional `acme` repositories ([`internal/demo`](internal/demo/demo.go)).
After Go dependencies are installed, demo commands need no network or installed
agent. The demo keeps its config and verdicts
in a directory of its own, `deps-handler-demo` in your user cache directory
(`~/Library/Caches` on macOS, `$XDG_CACHE_HOME` or `~/.cache` on Linux):

```bash
just try                               # help and a terminal status pointer
just try status                        # compact overview
just try dependabot --open --review --apply
just try verdicts
just try pr list
just try pr merge                       # preview
just try pr merge --apply               # #430 merges; #431 and #432 block (exit 1)
just try pr handle --repo acme/storefront --agent claude
just try pr handle --repo acme/storefront --agent claude --apply  # exit 1: #432 needs a decision
just try pr handle acme/storefront#431 --agent claude --apply     # repair and merge (exit 0)
```

The PR scenario is separate from the Dependabot fixtures: #430 is approved,
#431 needs a simulated repair, and #432 asks whether guests may check out or
must sign in. Preview never publishes a repair or merge. Apply uses the real
orchestration with canned agent answers, an in-memory push and simulated CI;
no git or agent subprocess runs. Any retained checkout is an empty temporary
directory. `pr diagnose`, `pr fix`, `pr rerun` and agent-backed `pr eval` are
unavailable; `pr eval --list` still lists embedded cases.

Merges and repairs only change memory, so every invocation starts over.
Verdicts persist separately; the tape forgets only fictional repo verdicts to
show live review progress. `just demo`
re-records [`docs/demo.gif`](docs/demo.gif) from [`demo/demo.tape`](demo/demo.tape)
with [VHS](https://github.com/charmbracelet/vhs); re-record it when the
report output changes. The recipe creates `.demo/bin/dh` pointing to the demo
binary, so the tape cannot pick up an installed production `dh`. It shows bare
help, explicit status, reviewed Dependabot apply, verdicts and a PR repair in
under 40 seconds.

## Evaluating PR diagnosis

`go test ./...` tests the evaluation harness with deterministic agent doubles;
it never runs a live model. To measure actual diagnosis quality, use:

```bash
go run . pr eval --list
just eval --agent claude --json > evaluation.json
just eval lyzma muse --agent codex --timeout 5m
```

These opt-in runs use the configured model backend and its normal costs.
They replay embedded evidence and selected source files without contacting
GitHub. See [the evaluation guide](docs/diagnosis-evaluation.md) for the fixture
format, grading rules and review requirements.

## Releasing

From a clean `main` branch, run one of:

```bash
just release patch   # v0.3.0 -> v0.3.1
just release minor   # v0.3.0 -> v0.4.0
just release major   # v0.3.0 -> v1.0.0
```

The recipe fetches tags from `origin`, bumps the highest stable `vX.Y.Z`
tag (starting from `v0.0.0` if none exists), and creates an annotated tag.
It pushes `main` and that tag atomically, including any local commits;
if `main` is behind or has diverged from `origin`, update it first.
Prerelease tags are ignored when choosing the version.

The tag triggers the [release workflow](.github/workflows/release.yml),
which runs CI before GoReleaser publishes the GitHub release and updates
the Homebrew cask. The command returns after the push; follow the printed
Actions link to check publication. If the push fails, the local tag is
kept and the command prints the exact push to retry.

## Guidelines

- deps-handler errs on the side of not merging. A change that lets more
  PRs merge needs a test showing why each newly merged case is safe.
- Behaviour users can see is documented in the README: update it in the
  same pull request.
- Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/)
  (`feat:`, `fix:`, `docs:`...); the release notes are built from them.
