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
docker build -t deps-merger:test .
sh scripts/test-container.sh deps-merger:test
```

## Trying changes without touching real repositories

The `demo` build tag swaps GitHub and the AI agent for a make-believe
version with fictional `acme` repositories ([`internal/demo`](internal/demo/demo.go)).
It never reaches the network, and keeps its config, verdicts and checkouts
in a directory of its own, `deps-merger-demo` in your user cache directory
(`~/Library/Caches` on macOS, `$XDG_CACHE_HOME` or `~/.cache` on Linux):

```bash
just try run                    # dry run
just try run --review --apply   # "merge", "rebase", have the "agent" review
just try verdicts
```

Merges only change the demo's memory, so every run starts over. `just demo`
re-records [`docs/demo.gif`](docs/demo.gif) from [`demo/demo.tape`](demo/demo.tape)
with [VHS](https://github.com/charmbracelet/vhs); re-record it when the
report output changes.

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

- deps-merger errs on the side of not merging. A change that lets more
  PRs merge needs a test showing why each newly merged case is safe.
- Behaviour users can see is documented in the README: update it in the
  same pull request.
- Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/)
  (`feat:`, `fix:`, `docs:`...); the release notes are built from them.
