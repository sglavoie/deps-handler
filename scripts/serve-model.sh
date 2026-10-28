#!/bin/sh
# Serves the review model on this Mac to the App on the Pi, over Tailscale
# only, with the key in ~/.config/deps-merger-cloud/model.key (the Pi's
# DMC_REVIEW_KEY). One slot: one review at a time.
set -eu
key=$HOME/.config/deps-merger-cloud/model.key
[ -s "$key" ] || { echo "serve-model: no $key" >&2; exit 1; }
exec llama-server -hf "${DMC_MODEL:-unsloth/Qwen3.8-27B-GGUF:Q8_0}" \
	--host "$(tailscale ip -4)" --port 8081 --api-key-file "$key" \
	--jinja -c 65536 -np 1
