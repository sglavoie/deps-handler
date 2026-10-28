#!/bin/sh
# Runs the server on this machine behind a Cloudflare named tunnel, until
# either stops or Ctrl-C. Settings come from DMC_ENV (default
# ~/.config/deps-merger-cloud/dev.env), the tunnel from DMC_TUNNEL_CONFIG
# (default ~/.config/deps-merger-cloud/cloudflared.yml). See
# docs/app/README.md.
set -eu
cd "$(dirname "$0")/.."
env_file=${DMC_ENV:-$HOME/.config/deps-merger-cloud/dev.env}
tunnel_config=${DMC_TUNNEL_CONFIG:-$HOME/.config/deps-merger-cloud/cloudflared.yml}
state=$HOME/.local/state/deps-merger-cloud
mkdir -p "$state"

set -a
. "$env_file"
set +a

go build -o "$state/server" ./cmd/server
"$state/server" &
server=$!
cloudflared tunnel --no-autoupdate --config "$tunnel_config" run &
tunnel=$!
trap 'kill $server $tunnel 2>/dev/null || true; wait' INT TERM EXIT

# Stop both as soon as either stops.
while kill -0 $server 2>/dev/null && kill -0 $tunnel 2>/dev/null; do
	sleep 2
done
