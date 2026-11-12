---
title: Linux Host Agent (experimental)
description: Run the metal-agent on a Linux arm64 host such as a Raspberry Pi 5, serving models through Ollama or Hailo's hailo-ollama on the AI HAT+ 2.
---

# Linux Host Agent (experimental)

The [Metal agent](/docs/guides/macos-metal) also starts on Linux. It joins the cluster
the same way a Mac does (a `MetalNode`, a Lease heartbeat, the dial-back check
and the in-cluster relay), and runs a daemon engine on the host.

Models served by a Linux host use `hardware.accelerator: metal`, like a Mac's:
`metal` is what sends a service to a host agent instead of in-cluster pods. On
Linux the agent logs `Metal not available; Linux host support is
experimental` at startup, and whether it found a Hailo device node
(`/dev/hailo*`).

Every agent watches one namespace (`--namespace`) and considers every `metal`
service in it, so keep a Linux host's services apart from the Macs':

- **Use a namespace of its own (recommended).** Create the host's identity in
  that namespace (`llmkube metal token create pi5 -n pi`) and put its Models
  and InferenceServices there. No Mac agent sees them.
- **Or allowlist every agent in a shared namespace**, the Macs included, with
  `--inference-service-allowlist` (comma-separated InferenceService names).
  An allowlist only limits the agent it is set on. A Mac without one claims
  the Linux host's services too: every agent registers the Ollama runtime, so
  a Mac running Ollama can quietly serve the registry's copy of the same tag,
  and one without it refuses the service and writes status over the Linux
  host's.

Making placement on a `MetalNode` explicit is proposed in
[#2481](https://github.com/defilantech/LLMKube/issues/2481).

Tracking issue: [#2481](https://github.com/defilantech/LLMKube/issues/2481).

## What has been tested

| | Status |
|---|---|
| linux/arm64 agent, `runtime: ollama` with stock Ollama on CPU | Tested in kind: `Reachable=True`, chat completions through the relay. Run it like hailo-ollama below, with `--ollama-port 11434` |
| `runtime: ollama` against `hailo-ollama` 5.1.1 | The endpoints the agent uses, checked on a Raspberry Pi 5 with an AI HAT+ 2 (`hack/hailo-probe.sh`). The agent end to end, against the [Hailo-10H simulator](#testing-without-a-hat) |
| llama.cpp (`llama-server`) on Linux | Untested. The agent's llama.cpp defaults (flash attention, mlock) are tuned for Apple Silicon |
| `llmkube metal join` | Refuses Linux. Join [by hand](#join-the-host) |
| Release artifacts | None for Linux. [Build the agent](#build-the-agent) from source |
| Raspberry Pi OS kernel (16K pages) | `hailo-h10-all` and hailo-ollama run on `6.18.50+rpt-rpi-2712` (16K pages). The agent itself has not run on a Pi yet |
| `llmkube deploy` for a Linux host | Untested. Write the Model and InferenceService by hand |

## Build the agent

```sh
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
  go build -o llmkube-metal-agent ./cmd/metal-agent
```

## Join the host

Where you have cluster access, create the host's identity as for a Mac. This
creates its ServiceAccount, RBAC, Lease and `MetalNode`, plus a join Secret:

```sh
kubectl create namespace pi
llmkube metal token create pi5 -n pi
```

`metal join` cannot redeem the join string on Linux, so delete the unused
join Secret and mint the agent's token directly:

```sh
kubectl -n pi delete secret llmkube-metal-pi5-join
kubectl -n pi create token llmkube-metal-pi5 --duration=24h > token
chmod 600 token
```

Write a kubeconfig that reads that token file, with the API server address
the host can reach and the cluster's CA:

```yaml
apiVersion: v1
kind: Config
clusters:
- name: llmkube
  cluster:
    server: https://<api-server>:6443
    certificate-authority-data: <base64 CA>
users:
- name: pi5
  user:
    tokenFile: /path/to/token
contexts:
- name: pi5
  context: {cluster: llmkube, user: pi5, namespace: pi}
current-context: pi5
```

Copy both files to the host (0600) and start the agent under the supervisor of
your choice. `--renew-service-account-token` keeps the token file current; the
agent renews at half the token's lifetime, so a host that is off for longer
than that needs a new token.

```sh
llmkube-metal-agent \
  --kubeconfig /path/to/kubeconfig \
  --namespace pi \
  --metal-node pi5 \
  --renew-service-account-token \
  --runtime ollama \
  --ollama-port 8000
```

The model store and state directory default to
`$XDG_DATA_HOME/llmkube/models` and `$XDG_DATA_HOME/llmkube/metal-agent`
(`~/.local/share/llmkube/...` when `XDG_DATA_HOME` is unset); `--model-store`
and `--state-dir` override them.

Then wait for the `MetalNode` to report `Reachable=True`:

```sh
kubectl get metalnode pi5 -n pi
```

## Memory

On Linux the agent reads `/proc/meminfo`, capped by the cgroup v2 limits from
its own cgroup up to the root: total memory is the smallest `memory.max` on
that path, and available memory the least room left under any of them. A
systemd `MemoryMax=` or a container `--memory` limit therefore sizes the
download cap, the `MetalNode` capacity and the budget for engines the agent
starts, which inherit its cgroup.

Ollama and hailo-ollama are daemons the agent only talks to and run outside
its cgroup, so `runtime: ollama` services are admitted against the host's
`MemTotal` and `MemAvailable`, whatever the agent's own limit.

For a fit decision that does not depend on the machine, as in tests, set
`spec.hardware.memoryBudget` on the Model. It may not exceed the host budget.

## Hailo AI HAT+ 2

Hailo's [hailo-ollama](https://github.com/hailo-ai/hailo_model_zoo_genai)
serves an Ollama-compatible API on the HAT's Hailo-10H. Raspberry Pi ships
HailoRT 5.1.1 for it, and hailo-ollama 5.1.1 is the matching server.

Install the driver and runtime from Raspberry Pi's repository, and reboot:

```sh
sudo apt install dkms hailo-h10-all
sudo reboot
```

`hailortcli fw-control identify` should then report a `HAILO10H` device.
hailo-ollama 5.1.1 is Hailo's `hailo_gen_ai_model_zoo` package:

```sh
curl -fLO https://dev-public.hailo.ai/2025_12/Hailo10/hailo_gen_ai_model_zoo_5.1.1_arm64.deb
sudo dpkg -i hailo_gen_ai_model_zoo_5.1.1_arm64.deb
```

hailo-ollama 5.1.1 takes its listen address only from `hailo-ollama.json`,
and the packaged one (`/etc/xdg/hailo-ollama/hailo-ollama.json`) listens on
`0.0.0.0:8000`, an unauthenticated API on your network. Give the user that
runs it a copy listening on `127.0.0.1`, which hailo-ollama reads first:

```sh
mkdir -p ~/.config/hailo-ollama
sed 's/"0.0.0.0"/"127.0.0.1"/' /etc/xdg/hailo-ollama/hailo-ollama.json \
  > ~/.config/hailo-ollama/hailo-ollama.json
hailo-ollama
```

The package's install script makes `/usr/share/hailo-ollama/models`, where
hailo-ollama 5.1.1 caches the HEFs it pulls, writable by every user.

Run the agent with `--runtime ollama --ollama-port 8000`, and name the model
with a `hailo://` source: a tag in the model zoo of the installed
hailo-ollama, which `GET /hailo/v1/list` lists. hailo-ollama 5.1.1 has
`qwen2.5-instruct:1.5b`, `qwen2.5-coder:1.5b`, `qwen2:1.5b`, `llama3.2:3b`
and `deepseek_r1_distill_qwen:1.5b`. hailo-ollama maps the tag to a HEF
compiled for the Hailo-10H through the manifests it installs, and pulls it
from Hailo's model host (`dev-public.hailo.ai`) into its own cache, so neither
the controller nor the agent downloads anything. The source needs
`accelerator: metal` and `runtime: ollama`; the controller marks the Model
Failed otherwise. Only `name` or `name:tag` is accepted: a `/`, which could
name another registry, is refused.

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: qwen25-15b
spec:
  source: hailo://qwen2.5-instruct:1.5b
  hardware:
    accelerator: metal
---
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: qwen-hailo
spec:
  modelRef: qwen25-15b
  runtime: ollama
  replicas: 1
```

Like any Model change, editing the source does not restart a running
service; change the InferenceService (or restart the agent) to pick it up.

Before it pulls, the agent checks that the daemon on `--ollama-port` is
hailo-ollama (it serves `GET /hailo/v1/list`) and that it lists the tag, so a
host running stock Ollama refuses the service instead of pulling a different
model under the same name.

Memory admission sizes a `hailo://` model the way it sizes an `https://` one,
with a HEAD request and nothing downloaded. The agent reads the HEF's SHA-256
(`hef_h10h`) from hailo-ollama's manifest for the tag, in the directory
hailo-ollama reads them from (`/usr/share/hailo-ollama/models/manifests` for
the package) or `--hailo-manifests-dir`, and HEADs
`<--hailo-library-url>/blob/sha256_<digest>`. The HEAD goes through
`--allowed-download-hosts` like any other download, so a host that sets that
flag must list `dev-public.hailo.ai`, and it verifies TLS against the system's
CA certificates.

Known limits, checked on a Raspberry Pi 5 with an AI HAT+ 2:

- Clients must send the hailo-ollama tag (`qwen2.5-instruct:1.5b`) as
  `model` in the request body. Nothing rewrites the service's model name.
- `GET /v1/models` is not served, and `created` in `/v1/chat/completions` is
  in nanoseconds.
- Requests need `Content-Type: application/json`.
- hailo-ollama keeps one model loaded and reloads on every switch, so run one
  `InferenceService` per HAT.
- The memory estimate is the HEF's size (2.2 GiB for `qwen2.5-instruct:1.5b`),
  but the budget it is checked against is the Pi's RAM, not the HAT's 8 GB. To
  test the HAT's limit, use the simulator's `HAILO_SIM_DEVICE_MEMORY`.

## Testing without a HAT

`test/hailosim` builds the real hailo-ollama server against HailoRT's headers
and a fake `libhailort` that emulates a Hailo-10H. See its
[README](https://github.com/defilantech/LLMKube/tree/main/test/hailosim) for
the settings it takes.

```sh
make docker-build-hailo-sim
docker run --rm -p 8000:8000 localhost/llmkube-hailo-sim:dev
hack/hailo-probe.sh http://127.0.0.1:8000
```

`hack/hailo-probe.sh` checks every endpoint the agent's Ollama executor and
ingress use. Run it on a real host to compare against the simulator.

`make test-e2e-host-agent` runs the whole path in kind: the operator, and two
Linux agents in their own namespaces, each next to a simulator. It checks that
both pass the dial-back, that each serves its own `hailo://` service through
the relay and only its own, and that a `hailo://` Model on another accelerator
is refused. CI runs it on every pull request.
