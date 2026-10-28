#!/bin/sh
# Runs the server on this machine behind a Cloudflare named tunnel, until
# either stops or Ctrl-C. Settings come from DMC_ENV (default
# ~/.config/deps-merger-cloud/dev.env), the tunnel from DMC_TUNNEL_CONFIG
# (default ~/.config/deps-merger-cloud/cloudflared.yml). See
# docs/app/README.md.
set -eu
cd "$(dirname "$0")/.."
if [ "$#" -gt 1 ]; then
	echo 'usage: run-local.sh [api|claude|codex]' >&2
	exit 2
fi
if [ -z "${DMC_ENV:-}" ] && [ -f "$HOME/.config/deps-merger-cloud/prod.env" ]; then
	echo 'Choose the App explicitly with DMC_ENV: dev.env or prod.env. Both Apps can deliver to the same tunnel, but this server verifies only the selected App.' >&2
	exit 2
fi
env_file=${DMC_ENV:-$HOME/.config/deps-merger-cloud/dev.env}
tunnel_config=${DMC_TUNNEL_CONFIG:-$HOME/.config/deps-merger-cloud/cloudflared.yml}

# A variable exported for another App remains set when this profile omits it.
# Keep the server's profile settings isolated while retaining unrelated
# variables such as the API key named by DMC_REVIEW_KEY_ENV.
unset DMC_APP_ID DMC_PRIVATE_KEY DMC_PRIVATE_KEY_FILE DMC_WEBHOOK_SECRET \
	DMC_INSTALLATION_ID DMC_DB DMC_HOST DMC_SWEEP DMC_DEBOUNCE \
	DMC_ALLOW_MERGE DMC_REPOS DMC_REVIEW_AGENT DMC_REVIEW_URL \
	DMC_REVIEW_MODEL DMC_REVIEW_KEY_ENV DMC_REVIEW_TOOLS \
	DMC_REVIEWS_PER_DAY DMC_REVIEW_TIMEOUT PORT

set -a
. "$env_file"
set +a

review_agent=${1:-${DMC_REVIEW_AGENT:-api}}
case "$review_agent" in
	api|claude|codex) ;;
	*) echo 'usage: run-local.sh [api|claude|codex]' >&2; exit 2 ;;
esac
export DMC_REVIEW_AGENT=$review_agent
echo "Starting App ${DMC_APP_ID:-unknown} from $env_file with $review_agent review" >&2

state=$HOME/.local/state/deps-merger-cloud
mkdir -p "$state"

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
