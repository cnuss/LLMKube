#!/bin/sh
# Starts hailo-ollama 5.1.1 on the simulated Hailo-10H.
#
#   HAILO_SIM_LISTEN=host:port          Rewrites the server address in
#                                       hailo-ollama.json, the only place 5.1.1
#                                       reads it. Default: the packaged 0.0.0.0:8000.
#   HAILO_SIM_LIBRARY_LISTEN=host:port  Serves the seeded HEFs at
#                                       /blob/sha256_<digest>, as dev-public.hailo.ai
#                                       does, for HEAD requests. Default
#                                       127.0.0.1:8001; empty turns it off.
set -eu

cfg=/etc/xdg/hailo-ollama/hailo-ollama.json
if [ -n "${HAILO_SIM_LISTEN:-}" ]; then
	host=${HAILO_SIM_LISTEN%:*}
	port=${HAILO_SIM_LISTEN##*:}
	sed -i -E "/\"server\"/,/\}/ { s/\"host\": *\"[^\"]*\"/\"host\": \"$host\"/; s/\"port\": *[0-9]+/\"port\": $port/ }" "$cfg"
fi

library=${HAILO_SIM_LIBRARY_LISTEN-127.0.0.1:8001}
if [ -n "$library" ]; then
	python3 -m http.server --bind "${library%:*}" --directory /srv/hailo-library "${library##*:}" \
		>/dev/null 2>&1 &
fi

exec /usr/bin/hailo-ollama "$@"
