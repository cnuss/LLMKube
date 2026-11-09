---
title: Linux Host Agent (experimental)
description: Run the metal-agent on a Linux arm64 host such as a Raspberry Pi 5, serving models through Ollama or Hailo's hailo-ollama on the AI HAT+ 2.
---

# Linux Host Agent (experimental)

The [Metal agent](/docs/guides/macos-metal) also starts on Linux. It joins the cluster
the same way a Mac does (a `MetalNode`, a Lease heartbeat, the dial-back check
and the in-cluster relay), and runs a daemon engine on the host. On Linux it
logs `Metal not available; Linux host support is experimental` at startup.

Tracking issue: [#2481](https://github.com/defilantech/LLMKube/issues/2481).

## What has been tested

| | Status |
|---|---|
| linux/arm64 agent, `runtime: ollama` with stock Ollama on CPU | Tested in kind: `Reachable=True`, chat completions through the relay |
| `runtime: ollama` against `hailo-ollama` | Tested against the [Hailo-10H simulator](#testing-without-a-hat), not yet on a real AI HAT+ 2 |
| llama.cpp (`llama-server`) on Linux | Untested. The agent's llama.cpp defaults (flash attention, mlock) are tuned for Apple Silicon |
| `llmkube metal join` | Refuses Linux. Join [by hand](#join-the-host) |
| Release artifacts | None for Linux. [Build the agent](#build-the-agent) from source |
| Raspberry Pi OS kernel (16K pages) | Untested. The tests above ran on a 4K-page kernel |

## Build the agent

```sh
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
  go build -o llmkube-metal-agent ./cmd/metal-agent
```

## Join the host

Where you have cluster access, create the host's identity as for a Mac. This
creates its ServiceAccount, RBAC, Lease and `MetalNode`, plus a join Secret:

```sh
llmkube metal token create pi5 -n default
```

`metal join` cannot redeem the join string on Linux, so delete the unused
join Secret and mint the agent's token directly:

```sh
kubectl -n default delete secret llmkube-metal-pi5-join
kubectl -n default create token llmkube-metal-pi5 --duration=24h > token
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
  context: {cluster: llmkube, user: pi5, namespace: default}
current-context: pi5
```

Copy both files to the host (0600) and start the agent under the supervisor of
your choice. `--renew-service-account-token` keeps the token file current; the
agent renews at half the token's lifetime, so a host that is off for longer
than that needs a new token.

```sh
llmkube-metal-agent \
  --kubeconfig /path/to/kubeconfig \
  --namespace default \
  --metal-node pi5 \
  --renew-service-account-token \
  --runtime ollama \
  --ollama-port 11434 \
  --model-store /var/lib/llmkube/models \
  --state-dir /var/lib/llmkube/agent
```

Then wait for the `MetalNode` to report `Reachable=True`:

```sh
kubectl get metalnode pi5 -n default
```

## Memory

On Linux the agent reads `/proc/meminfo`, capped by the tightest cgroup v2
`memory.max` from its own cgroup up to the root. A systemd `MemoryMax=` or a
container `--memory` limit therefore sizes the memory budget, the download cap
and the `MetalNode` capacity. Engines the agent starts inherit that cgroup; a
daemon it only talks to, such as Ollama or hailo-ollama, does not, and its
memory is not counted against the limit.

## Ollama on CPU

Run Ollama on the host, listening on `127.0.0.1`, and use `runtime: ollama`.
The Model still has to set `accelerator: metal` for the agent to pick it up.
The Ollama tag is the Model's name, mapped through the agent's built-in table
for a few known names, and the agent always pulls it from the registry, so a
tag that exists only locally fails.

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: tinyllama
spec:
  source: https://huggingface.co/TheBloke/TinyLlama-1.1B-Chat-v1.0-GGUF/resolve/main/tinyllama-1.1b-chat-v1.0.Q4_K_M.gguf
  format: gguf
  hardware:
    accelerator: metal
---
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: tinyllama-pi
spec:
  modelRef: tinyllama
  runtime: ollama
  replicas: 1
```

`spec.source` is only used for the memory estimate; Ollama manages its own
model storage.

## Hailo AI HAT+ 2

Hailo's [hailo-ollama](https://github.com/hailo-ai/hailo_model_zoo_genai)
serves an Ollama-compatible API on port 8000. Start it on `127.0.0.1:8000`
(`OLLAMA_HOST=127.0.0.1:8000`) and run the agent with `--ollama-port 8000`.

Every hailo-ollama tag has the form `name:tag`, which a Kubernetes name cannot
spell, and hailo-ollama has no implicit `:latest`. Name the tag with the
experimental `llmkube.ai/ollama-model` annotation:

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: qwen25-15b
  annotations:
    llmkube.ai/ollama-model: "qwen2.5:1.5b"
spec:
  source: https://huggingface.co/Qwen/Qwen2.5-1.5B-Instruct-GGUF/resolve/main/qwen2.5-1.5b-instruct-q4_k_m.gguf
  format: gguf
  hardware:
    accelerator: metal
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
- The memory budget measures the Pi's RAM and the GGUF in `spec.source`, not
  the HAT's 8 GB or the HEF the engine loads.

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
