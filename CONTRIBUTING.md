# Contributing

Issues and pull requests are welcome. For anything beyond a small fix,
open an issue first so we can agree on the approach.

## Setup

You need Go (the version in [`go.mod`](go.mod)) and
[`just`](https://github.com/casey/just).

```bash
just test    # go test -race ./...
just ci      # everything CI checks: vet, staticcheck, govulncheck, tests, gofmt, go mod tidy
```

## Trying changes without touching real repositories

The `demo` build tag swaps GitHub and the AI agent for a make-believe
version with fictional `acme` repositories ([`internal/demo`](internal/demo/demo.go)).
It never reaches the network, and keeps its config, verdicts and checkouts
in a directory of its own under the system temp dir:

```bash
just try run                    # dry run
just try run --review --apply   # "merge", "rebase", have the "agent" review
just try verdicts
```

Merges only change the demo's memory, so every run starts over. `just demo`
re-records [`docs/demo.gif`](docs/demo.gif) from [`demo/demo.tape`](demo/demo.tape)
with [VHS](https://github.com/charmbracelet/vhs); re-record it when the
report output changes.

## Guidelines

- deps-merger errs on the side of not merging. A change that lets more
  PRs merge needs a test showing why each newly merged case is safe.
- Behaviour users can see is documented in the README: update it in the
  same pull request.
- Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/)
  (`feat:`, `fix:`, `docs:`...); the release notes are built from them.
