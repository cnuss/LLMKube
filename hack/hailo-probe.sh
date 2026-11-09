#!/usr/bin/env bash
# Probe a hailo-ollama server for the endpoints the metal-agent's Ollama
# executor and ingress allowlist use. Run on the Pi with the AI HAT+ 2.
#
# Usage: hack/hailo-probe.sh [base-url] [model]
#   base-url  default http://127.0.0.1:8000
#   model     default qwen2.5:1.5b
set -u

BASE="${1:-http://127.0.0.1:8000}"
MODEL="${2:-qwen2.5:1.5b}"

section() { printf '\n== %s\n' "$1"; }

section "host"
printf 'model:     %s\n' "$(tr -d '\0' </proc/device-tree/model 2>/dev/null || echo unknown)"
printf 'kernel:    %s\n' "$(uname -r)"
printf 'pagesize:  %s\n' "$(getconf PAGESIZE)"
printf 'memtotal:  %s\n' "$(awk '/^MemTotal/{print $2" kB"}' /proc/meminfo)"
printf 'hailo dev: %s\n' "$(find /dev -maxdepth 1 -name 'hailo*' 2>/dev/null | tr '\n' ' ')"
command -v lspci >/dev/null && lspci | grep -i hailo
command -v hailortcli >/dev/null && hailortcli fw-control identify 2>&1 | head -8

# probe METHOD PATH [JSON-BODY] [TIMEOUT]
probe() {
	local method="$1" path="$2" body="${3:-}" timeout="${4:-15}" out code
	out="$(mktemp)"
	if [ -n "$body" ]; then
		code=$(curl -sS -o "$out" -w '%{http_code}' -m "$timeout" -X "$method" \
			-H 'Content-Type: application/json' -d "$body" "$BASE$path" 2>&1)
	else
		code=$(curl -sS -o "$out" -w '%{http_code}' -m "$timeout" -X "$method" "$BASE$path" 2>&1)
	fi
	printf '%-5s %-24s %s  %s\n' "$method" "$path" "$code" \
		"$(head -c 160 "$out" | tr '\n' ' ')"
	rm -f "$out"
}

section "used by OllamaExecutor (pkg/agent/executor_ollama.go)"
probe GET / ""
probe POST /api/pull "{\"model\":\"$MODEL\",\"stream\":false}" 900
probe POST /api/generate "{\"model\":\"$MODEL\",\"prompt\":\"\"}" 120
probe GET /api/ps ""
probe POST /api/generate "{\"model\":\"$MODEL\",\"prompt\":\"\",\"keep_alive\":0}" 30

section "ollama routes the ingress allows (pkg/agent/ingress/paths.go)"
probe GET /api/version ""
probe GET /api/tags ""
probe POST /api/show "{\"model\":\"$MODEL\"}"
probe POST /api/chat "{\"model\":\"$MODEL\",\"stream\":false,\"messages\":[{\"role\":\"user\",\"content\":\"Say hi\"}]}" 120

section "OpenAI-compatible common routes"
probe GET /v1/models ""
probe POST /v1/chat/completions "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"Say hi\"}]}" 120

section "hailo-specific"
probe GET /hailo/v1/list ""
