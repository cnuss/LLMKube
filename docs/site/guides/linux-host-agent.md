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
| `runtime: ollama` against `hailo-ollama` | Tested against the [Hailo-10H simulator](#testing-without-a-hat), not yet on a real AI HAT+ 2 |
| llama.cpp (`llama-server`) on Linux | Untested. The agent's llama.cpp defaults (flash attention, mlock) are tuned for Apple Silicon |
| `llmkube metal join` | Refuses Linux. Join [by hand](#join-the-host) |
| Release artifacts | None for Linux. [Build the agent](#build-the-agent) from source |
| Raspberry Pi OS kernel (16K pages) | Untested. The tests above ran on a 4K-page kernel |
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
  --ollama-port 8000 \
  --hailo-version 5.5.0
```

`--hailo-version` is the version of the installed hailo-ollama (`5.5.0` for
hailo-ollama 5.5.0); the agent needs it to size `hailo://` models, see below.

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
serves an Ollama-compatible API on port 8000. Start it on `127.0.0.1:8000`
(`OLLAMA_HOST=127.0.0.1:8000`) and run the agent with `--ollama-port 8000`.

Name the model with a `hailo://` source: a tag in Hailo's GenAI model zoo,
such as `hailo://qwen2.5:1.5b`. hailo-ollama maps the tag to a HEF compiled for
the Hailo-10H through the manifests it installs, and pulls it from Hailo's
model host (`dev-public.hailo.ai`) into its own cache, so neither the
controller nor the agent downloads anything. The source needs
`accelerator: metal` and `runtime: ollama`; the controller marks the Model
Failed otherwise. Only `name` or `name:tag` is accepted: a `/`, which could
name another registry, is refused.

Before it pulls, the agent checks that the daemon on `--ollama-port` is
hailo-ollama (it serves `GET /hailo/v1/list`) and that it lists the tag, so a
host running stock Ollama refuses the service instead of pulling a different
model under the same name.

Memory admission sizes a `hailo://` model the way it sizes an `https://` one,
with a HEAD request and nothing downloaded: the agent reads the HEF filename
from hailo-ollama's manifests (the directories hailo-ollama reads, or
`--hailo-manifests-dir`), and HEADs that HEF on Hailo's model host
(`--hailo-library-url`) at the blob version hailo-ollama would pull for
`--hailo-version`, including its fallback to an earlier minor version. The
HEAD goes through `--allowed-download-hosts` like any other download, so a
host that sets that flag must list `dev-public.hailo.ai`, and it verifies TLS
against the system's CA certificates, which Raspberry Pi OS ships (a minimal
container image may not).

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: qwen25-15b
spec:
  source: hailo://qwen2.5:1.5b
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

`GET /hailo/v1/list` on the host lists the tags the installed hailo-ollama
knows.

Known limits, from hailo-ollama's server code:

- Clients must send the hailo-ollama tag (`qwen2.5:1.5b`) as `model` in the
  request body. Nothing rewrites the service's model name.
- `GET /v1/models` is not served.
- Requests need `Content-Type: application/json`.
- hailo-ollama keeps one model loaded and reloads on every switch, so run one
  `InferenceService` per HAT.
- The memory estimate is the HEF's size (2.3 GiB for `qwen2.5:1.5b`), but
  the budget it is checked against is the Pi's RAM, not the HAT's 8 GB. To
  test the HAT's limit, use the simulator's `HAILO_SIM_DEVICE_MEMORY`.
- Sizing reproduces how hailo-ollama resolves a model to a file (its manifest
  layout and blob versions), so a hailo-ollama release that changes either
  breaks it until the agent follows.

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
