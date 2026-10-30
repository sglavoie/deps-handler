# GitHub App

The free deps-merger GitHub App uses the same policy and runner as the CLI
in this repository. It opens a Dependency health issue for each installed
repository. Merging is off by default. A repository opts in with
`.github/deps-merger.yml` on its default branch:

```yaml
mode: merge
```

The deployment must also set `DMC_ALLOW_MERGE=true`. Agent review is
optional and requires a review backend plus `agent_review: true` in the
repository policy. There is no billing,
subscription, or paid tier.

## Build and test

From the repository root:

```sh
go test ./...
go build -o server ./cmd/server
go build -o admin ./cmd/admin
```

The App code is in `cmd/{server,admin,hook,register}` and `internal/`.
The CLI remains at the repository root. The Dockerfile builds the server
from this same Go module; `fly.toml` is an optional Fly.io deployment
template. The runtime image includes Git and HTTPS certificates for API
agent reviews, whose checkouts run inside the container.

## Register and configure

Register a public App with `go run ./cmd/register -public -name <name>`.
The command saves its App ID and private key under
`~/.config/deps-merger-cloud/`. Keep those files private. For a private
development App, run `go run ./cmd/register`. See
[onboarding](onboarding.md) for repository installation instructions and
[data handling](data-handling.md) for permissions and retention.

The server reads these environment variables:

| Variable | Purpose |
| --- | --- |
| `DMC_APP_ID` | GitHub App ID |
| `DMC_PRIVATE_KEY_FILE` or `DMC_PRIVATE_KEY` | App private key |
| `DMC_WEBHOOK_SECRET` | Verifies webhook deliveries |
| `DMC_DB` | SQLite file, default `dmc.db` |
| `PORT`, `DMC_HOST` | HTTP listener, default port 8080 |
| `DMC_ALLOW_MERGE` | Deployment merge switch, default false |
| `DMC_REPOS` | Optional comma-separated repository allowlist |
| `DMC_REVIEW_AGENT` | `api` (default), `claude`, or `codex` |
| `DMC_REVIEW_URL`, `DMC_REVIEW_MODEL` | Model endpoint and name when using `api` |

The remaining review settings are documented at the top of
[`cmd/server/main.go`](../../cmd/server/main.go). The server exposes
`/webhook` and `/healthz`. Put it behind HTTPS, point the GitHub App's
webhook at `https://<host>/webhook`, and set its webhook secret. The
`cmd/hook` utility can update the URL and list recent deliveries.

The App subscribes to `check_suite`, `pull_request`, `pull_request_review`,
`push`, and `status`. For an existing App, enable any missing events in
its GitHub settings and accept the subscription change on installations
when GitHub requests it. Approvals and review dismissals, draft-state
changes, and PR edits queue a fresh run, so a PR becoming ready does not
have to wait for the next sweep. Every run still fetches the current PR
state and applies the full merge policy.

## Run and operate

For the local tunnel setup, run `just app-local`. To use an installed,
logged-in CLI agent instead of the local model, run `just app-local claude`
or `just app-local codex`. Those modes run `claude -p` or `codex exec`
for reviews and do not need a model endpoint or API key. The repository
still needs `agent_review: true` to request reviews. The recipe calls
`./scripts/run-local.sh`, which loads
`~/.config/deps-merger-cloud/dev.env`, starts the server, and starts
`cloudflared` using
`~/.config/deps-merger-cloud/cloudflared.yml`. Set `DMC_ENV` and
`DMC_TUNNEL_CONFIG` to use other files. Press Ctrl-C to stop both
processes. The server sweeps installed
repositories at startup and every four hours by default.
If a sweep fails, it retries after one, two, five, then fifteen minutes
(capped by `DMC_SWEEP`); a successful sweep restores the usual schedule.
Accepted webhook and sweep jobs are saved in the SQLite database before
they are queued. Pending jobs, including a run interrupted by a restart,
resume when the server starts again. Failed runs retry after 30 seconds,
two minutes, and ten minutes; after those attempts, the next sweep is the
fallback.
Rate-limit cooldowns apply to the whole installation, across repositories
and queued retries. Repository listings (including access webhook lookups),
merge requests, review comments, health issue requests and best-effort issue
pinning share the cooldown until GitHub's requested
pause expires; other installations continue independently. The server keeps
this cooldown in memory.
Installation and repository listings are validated before reconciliation.
Missing lists, invalid entries, inconsistent repository counts, and broken
pagination fail the read and are retried; they cannot establish an uninstall
or loss of repository access. An incomplete later page discards the entire
listing, preserving the previously recorded state and review files.
Every discovered repository's access is recorded before its first run, so an
older webhook response cannot revoke it. Later sweeps can revoke access even
if the repository has not run yet.
Health issue lookups also reject missing lists, unidentified entries, unknown
states, mismatched issue numbers and broken pagination, including repeated
cursors. These failures stop issue writes and return an error for retry.
An unknown PR state during tracking preserves its open record and fails the
run, so it cannot be counted as closed without confirmation.
Installation tokens refresh for API requests and checkout fetches during
long review runs, including when the token used to start the run expires.
Review setup runs only after resolving the owner and repository policies,
and only when the effective policy enables reviews. An opted-out repository
does not open the review cache, request checkout credentials or probe the
model. Review setup failures in opted-in repositories still allow updates
that pass the ordinary merge policy to proceed, while failing the run.
Each installation's review files have a filesystem lock held through the
run, so another server or `-once` process cannot replace a checkout while
an agent reads it. A conflicting run fails review setup and can retry
after the active run finishes. Uninstall cleanup takes that same lock;
it retries on a later sweep if another process is using the files.
Cleanup also removes recovered verdict caches and works after reviews
have been disabled in the server configuration.
Installation observation times are stored in SQLite, so stale sweeps or
webhook reads cannot undo newer state recorded by another server or `-once`
process. Cleanup checks that observation time in the same transaction that
deletes records, preserving newer active or suspended installations.
The recipe clears inherited server settings before loading `DMC_ENV`, so
an allowlist or merge setting exported for another App cannot carry over.

When `prod.env` exists, set `DMC_ENV` explicitly before running the local
recipe. The default `dev.env` belongs to the private development App. A
private GitHub App can only be installed on its owner's account, so testing a
repository in another organization requires an App installed there. Use
that App's environment file. For example:

```sh
DMC_ENV="$HOME/.config/deps-merger-cloud/prod.env" just app-local codex
```

Use a separate webhook hostname and tunnel for the dev App. If the dev and
production Apps both deliver to the same URL, a server started with one
App's secret rejects the other's deliveries with HTTP 401. In particular,
the production hostname requires `prod.env` when running the production
App locally.

The CLI agent must also be logged in locally.

With `DMC_DB` set to the running server's database:

```sh
go run ./cmd/admin status
go run ./cmd/admin metrics
go run ./cmd/admin pause <installation-id>
go run ./cmd/admin resume <installation-id>
```

Pausing stops merges immediately while reports continue. To run a
repository once without serving webhooks, use
`go run ./cmd/server -once owner/repo`; `-once -all` runs every
installed repository. A named repository that the App cannot access is
reported as an error.

The Raspberry Pi deployment in `deploy/pi/` uses systemd and a
Cloudflare tunnel. `./scripts/deploy-pi.sh [host]` builds the server and
admin for Linux arm64, copies them to the host (default `pi`), and
restarts the services. It expects `prod.env`, `prod.pem`,
`tunnel.json`, and `cloudflared.yml` already in
`/etc/deps-merger-cloud/`. It does not create those secrets.
`./scripts/serve-model.sh` is the optional local model setup used with
that deployment. Run only one tunnel connector for the App's hostname at
a time.
