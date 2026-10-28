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
template.

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
installed repository.

The Raspberry Pi deployment in `deploy/pi/` uses systemd and a
Cloudflare tunnel. `./scripts/deploy-pi.sh [host]` builds the server and
admin for Linux arm64, copies them to the host (default `pi`), and
restarts the services. It expects `prod.env`, `prod.pem`,
`tunnel.json`, and `cloudflared.yml` already in
`/etc/deps-merger-cloud/`. It does not create those secrets.
`./scripts/serve-model.sh` is the optional local model setup used with
that deployment. Run only one tunnel connector for the App's hostname at
a time.
