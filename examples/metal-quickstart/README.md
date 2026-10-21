# Metal Quickstart - Local LLM Inference on Apple Silicon

This quickstart runs Kubernetes (minikube) and the LLMKube Metal Agent on the
same Mac. The agent watches the cluster for `InferenceService` resources whose
Model sets `accelerator: metal` and runs `llama-server` natively with full
Metal GPU access. Engines listen on `127.0.0.1` only: the cluster reaches them
through the agent's authenticated TLS ingress and an in-cluster relay, and
clients on the Mac use the agent's client proxy on `127.0.0.1:9999`.

For a remote cluster (the recommended production shape), memory budgets and
the full walkthrough, see the
[macOS Metal Agent guide](https://llmkube.com/docs/guides/macos-metal). The
complete agent reference is [`deployment/macos/README.md`](../../deployment/macos/README.md).
The release archive carries it, the plist template and the RBAC manifest under
`deployment/macos/`.

## Prerequisites

### Hardware
- **Apple Silicon Mac** (M1 to M5). Recommended: 16GB+ unified memory.

### Software
1. **Homebrew**, a running Docker runtime for minikube's Docker driver (Docker
   Desktop, or another such as Colima), plus **minikube**, **kubectl** and **Helm**:
   ```bash
   brew install minikube kubectl helm
   minikube start --driver=docker
   ```

2. **llama.cpp** with Metal support (installs `/opt/homebrew/bin/llama-server`):
   ```bash
   brew install llama.cpp
   ```

3. **LLMKube CLI**
   ```bash
   brew install defilantech/tap/llmkube
   ```

4. **To build the agent from source:** the Xcode Command Line Tools
   (`xcode-select --install`) and Go 1.26.10 or newer (`brew install go`).

## Step 1: Install the LLMKube operator

Install the operator with Helm, the same way the
[macOS Metal Agent guide](https://llmkube.com/docs/guides/macos-metal) does:

```bash
helm repo add llmkube https://defilantech.github.io/LLMKube
helm install llmkube llmkube/llmkube \
  -n llmkube-system --create-namespace

# Wait for the operator to be ready:
kubectl wait --for=condition=ready pod -l control-plane=controller-manager -n llmkube-system --timeout=120s
```

## Step 2: Install the Metal Agent

### From source (recommended)

The agent connects with the current context of `~/.kube/config`, which
`minikube start` sets. If your shell sets `$KUBECONFIG`, minikube writes that
file instead, and the agent under launchd does not see your shell's
environment: replace the last line below with
`make install-metal-agent METAL_KUBECONFIG=<that file>`.

```bash
git clone https://github.com/defilantech/LLMKube.git
cd LLMKube
# Build the release that matches the operator Helm installed in Step 1
git checkout v$(helm show chart llmkube/llmkube | awk '/^appVersion:/{print $2}')
make install-metal-agent
```

`helm show chart` reads the same local chart index `helm install` used, so the
checkout matches the controller's release tag instead of `main`.

`make install-metal-agent` builds the agent, copies it to
`/usr/local/bin/llmkube-metal-agent` (sudo asks for your password), renders the
launchd plist into `~/Library/LaunchAgents/com.llmkube.metal-agent.plist` and
starts the service. To run the agent with a least-privilege identity instead
of your minikube admin context, see "Dedicated kubeconfig with least-privilege
RBAC" in
[`deployment/macos/README.md`](../../deployment/macos/README.md#dedicated-kubeconfig-with-least-privilege-rbac).

### From a release archive

Each release publishes `LLMKube-metal-agent_<version>_darwin_arm64.tar.gz` on
the [releases page](https://github.com/defilantech/LLMKube/releases/latest). The
archive holds the `llmkube-metal-agent` binary, this quickstart and
`deployment/macos/` (plist template, RBAC manifest, agent README), but not the
Makefile. From the directory you extracted it into:

```bash
sudo cp llmkube-metal-agent /usr/local/bin/
mkdir -p ~/Library/LaunchAgents
mkdir -p -m 0700 ~/Library/Logs/llmkube
sed 's|__HOME__|'"$HOME"'|g' deployment/macos/com.llmkube.metal-agent.plist \
  > ~/Library/LaunchAgents/com.llmkube.metal-agent.plist
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.llmkube.metal-agent.plist
```

The release binary is not notarized. If macOS refuses to run it, install from
source instead. For an archive install, a kubeconfig other than
`~/.kube/config` goes in the plist's `ProgramArguments` as
`--kubeconfig <file>`.

Whichever way you installed it, if macOS asks whether `llmkube-metal-agent` may
accept incoming connections, allow it: the in-cluster relay connects to the
agent's TLS ingress on port 9443 (`--ingress-port`).

## Verify Setup

```bash
# 1. Check Metal support
system_profiler SPDisplaysDataType | grep "Metal"
# Should show: Metal Support: Metal 4 (or Metal 3/2)

# 2. Check llama-server is installed
which llama-server
# Should show: /opt/homebrew/bin/llama-server

# 3. Check Metal agent is running
launchctl list | grep llmkube
# Should show: com.llmkube.metal-agent

# 3b. Check Metal agent health endpoint
curl http://localhost:9090/healthz
# Should show: ok

# 4. Check minikube is running
minikube status
# Should show: host: Running, kubelet: Running

# 5. Check kubectl works
kubectl get nodes
# Should show your minikube node
```

## Step 3: Deploy a model

```bash
llmkube deploy llama-3.1-8b --accelerator metal --context 8192
```

`--accelerator metal` sends the model to the Metal Agent. `--context` caps the
context window: the catalog default for this model is 131072 tokens, and the
agent's pre-flight memory check counts the KV cache for the whole window, so a
smaller value keeps the model inside the memory budget of most Macs.

Once the model is up, check which address the agent registered for the cluster
to reach it (the `hostIP` field):

```bash
grep 'registered endpoint' ~/Library/Logs/llmkube/metal-agent.log
```

Without `--host-ip`, the agent picks a Tailscale address (100.64.0.0/10) if
the Mac has one, otherwise the first other routable IPv4 address, skipping
loopback, link-local and Docker or VM bridge ranges. Only when there is none
does it fall back to the IP address `host.minikube.internal` resolves to on the
Mac, else the one `host.docker.internal` resolves to, else 192.168.65.254. It
registers the address, never the hostname, and logs
`no routable interface for host-IP auto-detect; using co-located DNS fallback`.
If the cluster cannot reach the address it picked (see "Service not
accessible" below), set `--host-ip` as described in
[`deployment/macos/README.md`](../../deployment/macos/README.md#--host-ip-flag-remote-cluster).

A model that is not in the catalog:

```bash
llmkube deploy my-custom-model \
  --accelerator metal \
  --source https://huggingface.co/bartowski/Llama-3.2-3B-Instruct-GGUF/resolve/main/Llama-3.2-3B-Instruct-Q8_0.gguf \
  --quantization Q8_0
```

The agent runs one engine process per InferenceService on the Mac. To serve
more models, create more InferenceServices (within the memory budget); a
`replicas` value above 1 does not start extra copies.

## Monitor the Deployment

```bash
# Watch the deployment
kubectl get inferenceservices -w

# Check Metal agent health
curl http://localhost:9090/healthz   # Liveness: "ok"
curl http://localhost:9090/readyz    # Readiness: "ready" when processes are healthy

# Check Metal agent metrics
curl -s http://localhost:9090/metrics | grep llmkube_metal_agent

# Check Metal agent logs
tail -f ~/Library/Logs/llmkube/metal-agent.log

# Check the engine's own output (<model-store>/llama-server-<namespace>-<name>.log)
tail -f ~/Library/Application\ Support/llmkube/models/llama-server-default-llama-3.1-8b.log

# Monitor GPU usage
sudo powermetrics --samplers gpu_power -i 1000
```

The agent serves `/healthz`, `/readyz` and `/metrics` on `127.0.0.1:9090`
only, so run these on the Mac itself.

## Test the Endpoint

The served model name, the `model` field below, is the InferenceService's
`spec.modelRef` (`llama-3.1-8b` here). The Kubernetes Service name is
sanitized, with dots replaced by dashes: `llama-3.1-8b` becomes
`llama-3-1-8b`.

### Option 1: On the Mac, through the client proxy

The agent's client proxy on `127.0.0.1:9999` (`--client-port`) forwards to the
running engine. With several engines running on one Mac it picks one for each
request, and consecutive requests can land on different engines, so use Option
2 to reach a specific model.

```bash
curl -sS http://127.0.0.1:9999/v1/chat/completions \
  -H 'content-type: application/json' \
  -d '{"model":"llama-3.1-8b","messages":[{"role":"user","content":"What is 2+2?"}],"max_tokens":20}'
```

### Option 2: Through the Kubernetes Service

The `llama-3-1-8b` Service selects the in-cluster relay pod, so
`kubectl port-forward` works:

```bash
kubectl port-forward svc/llama-3-1-8b 8080:8080

# In a second terminal:
curl -sS http://localhost:8080/v1/chat/completions \
  -H 'content-type: application/json' \
  -d '{"model":"llama-3.1-8b","messages":[{"role":"user","content":"What is 2+2?"}]}'
```

From a pod inside the cluster, use the Service DNS name:

```bash
kubectl run curl-test --rm -it --restart=Never --image=curlimages/curl -- \
  curl -sS http://llama-3-1-8b.default.svc.cluster.local:8080/v1/chat/completions \
  -H 'content-type: application/json' \
  -d '{"model":"llama-3.1-8b","messages":[{"role":"user","content":"What is 2+2?"}]}'
```

The engine port on the Mac is not reachable from the network by design.

## How It Works

```
client pod ──▶ llama-3-1-8b Service ──▶ llama-3-1-8b-relay pod ──TLS──▶ agent ingress :9443 ──▶ llama-server on 127.0.0.1
               (in minikube)            (in minikube)                    (on the Mac)

curl on the Mac ──▶ agent client proxy 127.0.0.1:9999 ──▶ llama-server on 127.0.0.1
```

1. The agent checks that the model fits the memory budget, downloads it, and
   starts `llama-server` on `127.0.0.1` with an ephemeral port.
2. It registers its TLS ingress (port 9443, `--ingress-port`) as the Service
   and EndpointSlice `llama-3-1-8b-agent`.
3. The controller creates the relay Deployment `llama-3-1-8b-relay` and points
   the `llama-3-1-8b` Service at it. The relay pins the agent's certificate
   and authenticates with a per-namespace token.

See "Network exposure" in
[`deployment/macos/README.md`](../../deployment/macos/README.md#network-exposure)
for the details.

## Troubleshooting

### Metal agent not starting

```bash
# Check logs
cat ~/Library/Logs/llmkube/metal-agent.log

# Verify llama-server is installed
llama-server --version

# Restart the agent
launchctl kickstart -k gui/$(id -u)/com.llmkube.metal-agent
```

### Model rejected (InsufficientMemory)

The Metal Agent checks whether a model fits in memory before starting it. If your InferenceService shows `InsufficientMemory`:

```bash
# Check the status
kubectl get isvc -o wide

# See the detailed reason
kubectl get isvc <name> -o jsonpath='{.status.schedulingMessage}'
```

Fix options:
- Use a smaller quantization (Q4_K_M instead of Q8_0)
- Reduce `contextSize` in the InferenceService spec (`--context` on `llmkube deploy`)
- Increase the budget: add `--memory-fraction 0.9` to the agent's launchd
  plist (see "`--memory-fraction` flag" in
  [`deployment/macos/README.md`](../../deployment/macos/README.md#--memory-fraction-flag-memory-budget))

### Model download slow/failing

```bash
# Check disk space
df -h

# Check model store
ls -lh ~/Library/Application\ Support/llmkube/models/
```

The agent caches a downloaded model at
`<model-store>/<Model name>/<file name from the source URL>` and uses a
non-empty regular file already there instead of downloading it again. To
download it yourself, write to a temporary name and move it into place:

```bash
SLOT="$HOME/Library/Application Support/llmkube/models/llama-3.1-8b"
mkdir -p -m 0700 "$SLOT"
curl -fL -o "$SLOT/Meta-Llama-3.1-8B-Instruct-Q5_K_M.gguf.part" \
  https://huggingface.co/bartowski/Meta-Llama-3.1-8B-Instruct-GGUF/resolve/main/Meta-Llama-3.1-8B-Instruct-Q5_K_M.gguf
mv "$SLOT/Meta-Llama-3.1-8B-Instruct-Q5_K_M.gguf.part" \
  "$SLOT/Meta-Llama-3.1-8B-Instruct-Q5_K_M.gguf"
```

Do not put a symlink in that slot: the agent refuses it with
`ModelSourceNotAllowed`. To serve a GGUF that already lives elsewhere on the
Mac, set the Model's `spec.source` to its absolute path, add its directory to
the agent's `--allowed-model-roots`, and allow the same directory on the
controller with the Helm value `modelSource.allowedHostPathRoots` (see
[`deployment/macos/README.md`](../../deployment/macos/README.md#--allowed-model-roots-flag-local-model-paths)).

### Service not accessible

```bash
# The Service clients call (note the sanitized name: llama-3-1-8b)
kubectl get svc llama-3-1-8b

# The agent's registration: your Mac's address, port 9443, and the
# llmkube.ai/agent-ingress-spki annotation
kubectl get endpointslice llama-3-1-8b-agent -o yaml

# The relay pod and its logs
kubectl get pods -l inference.llmkube.dev/service=llama-3.1-8b
kubectl logs deploy/llama-3-1-8b-relay

# Events: RelayNotAdopted, InvalidAgentIngressPin, RelayReconcileFailed, RelaySecretNotManaged
kubectl describe inferenceservice llama-3.1-8b

# Check if llama-server is running
ps aux | grep '[l]lama-server'
```

### Poor performance

```bash
# Check GPU utilization
sudo powermetrics --samplers gpu_power -i 1000

# Verify all layers are offloaded to the GPU: the command line shows
# --n-gpu-layers, and llama-server logs "offloaded N/N layers to GPU"
ps aux | grep '[l]lama-server'
grep offloaded ~/Library/Application\ Support/llmkube/models/llama-server-default-llama-3.1-8b.log
```

## Cleanup

```bash
# Delete the inference service
llmkube delete llama-3.1-8b

# Stop Metal agent
launchctl bootout gui/$(id -u)/com.llmkube.metal-agent

# Or completely uninstall (from the LLMKube checkout)
make uninstall-metal-agent

# Archive install (no Makefile): after the bootout above
sudo rm /usr/local/bin/llmkube-metal-agent
rm ~/Library/LaunchAgents/com.llmkube.metal-agent.plist
```

## Next Steps

- **Remote cluster**: put the Mac on the same LAN or tailnet as the cluster
  and follow the [macOS Metal Agent guide](https://llmkube.com/docs/guides/macos-metal)
- **Integration**: connect your applications with the OpenAI SDK (below)
- **Monitoring**: scrape `127.0.0.1:9090/metrics` with a Prometheus on the
  Mac, or through an SSH tunnel; see "Health Checks & Monitoring" in
  [`deployment/macos/README.md`](../../deployment/macos/README.md#health-checks--monitoring)

## Example Applications

### Python Client

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://127.0.0.1:9999/v1",
    api_key="not-needed"
)

response = client.chat.completions.create(
    model="llama-3.1-8b",
    messages=[
        {"role": "user", "content": "Explain quantum computing"}
    ]
)

print(response.choices[0].message.content)
```

### cURL Example

```bash
curl http://127.0.0.1:9999/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "llama-3.1-8b",
    "messages": [
      {"role": "system", "content": "You are a helpful assistant."},
      {"role": "user", "content": "Write a haiku about Kubernetes"}
    ],
    "temperature": 0.7,
    "max_tokens": 100
  }'
```

Both examples use the client proxy on the Mac. From elsewhere, point them at
the Service through `kubectl port-forward` (Option 2 above).

## Support

- **Documentation**: https://github.com/defilantech/LLMKube#the-metal-agent
- **Issues**: https://github.com/defilantech/llmkube/issues
- **Discussions**: https://github.com/defilantech/llmkube/discussions
