#!/bin/sh
# Run the real checkout test in the built server image, against local PR
# refs. No GitHub token, account, or network request is needed by the test.
set -eu
cd "$(dirname "$0")/.."
image=${1:-deps-merger:test}
arch=$(docker image inspect --format '{{.Architecture}}' "$image")
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
GOOS=linux GOARCH="$arch" CGO_ENABLED=0 go test -c -o "$stage/review.test" ./pkg/review
docker run --rm \
    --mount "type=bind,src=$stage,dst=/smoke,readonly" \
    --entrypoint /smoke/review.test "$image" \
    -test.run '^TestWorkspaceCheckout$' -test.v
