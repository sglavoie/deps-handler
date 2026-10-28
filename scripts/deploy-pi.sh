#!/bin/sh
# Builds the server and admin for the Pi and installs them there with
# systemd (deploy/pi). Asks for the Pi's sudo password. The first time,
# /etc/deps-merger-cloud also needs prod.env, prod.pem, tunnel.json and
# cloudflared.yml: see docs/app/README.md.
#
#   scripts/deploy-pi.sh [host]    # default host: pi
set -eu
cd "$(dirname "$0")/.."
host=${1:-pi}
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT

# Build the server and admin from this repository's Go module.
for cmd in server admin; do
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o "$stage/$cmd" ./cmd/$cmd
done
cp deploy/pi/* "$stage/"
remote=/tmp/dmc-deploy
ssh "$host" "rm -rf $remote && mkdir -p $remote"
scp -q "$stage"/* "$host:$remote/"
# cloudflared: the Pi's own, so both tunnels run one version.
ssh "$host" "cp ~/.local/bin/cloudflared $remote/cloudflared"
ssh -t "$host" "sudo sh $remote/install.sh && rm -rf $remote"
