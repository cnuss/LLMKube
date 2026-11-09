#!/bin/sh
# Seeds the blob cache with a sparse HEF per installed manifest, so
# /api/pull is a cache hit and never reaches the model CDN, then starts
# hailo-ollama.
#
#   HAILO_SIM_HEF_SIZE  Apparent size of each seeded HEF in bytes
#                       (default 2 GiB). Compare HAILO_SIM_DEVICE_MEMORY.
set -eu

blob="${XDG_DATA_HOME:-$HOME/.local/share}/hailo-ollama/models/blob"
size="${HAILO_SIM_HEF_SIZE:-2147483648}"
version="$(cat /usr/local/share/hailo-sim/version)"
mkdir -p "$blob"

for manifest in /usr/local/share/hailo-ollama/models/manifests/*/*/manifest.json; do
	hef="$(sed -n 's/.*"hef_filename": *"\([^"]*\)".*/\1/p' "$manifest")"
	[ -n "$hef" ] || continue
	[ -e "$blob/$hef" ] && continue
	truncate -s "$size" "$blob/$hef"
	printf '%s' "$version" >"$blob/$hef.version"
done

exec /usr/local/bin/hailo-ollama "$@"
