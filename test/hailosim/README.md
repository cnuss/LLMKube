# hailo-sim

The real [hailo-ollama](https://github.com/hailo-ai/hailo_model_zoo_genai)
server, compiled against HailoRT's public headers and linked to
`fake_hailort.cpp`, a stand-in `libhailort` that emulates a Hailo-10H
(Raspberry Pi AI HAT+ 2). The HTTP layer, model store, pull cache and
model swapping are hailo-ollama's own code; only the device is fake.

```sh
make docker-build-hailo-sim
docker run --rm -p 8000:8000 localhost/llmkube-hailo-sim:dev
hack/hailo-probe.sh http://127.0.0.1:8000
```

| Variable | Default | Effect |
|---|---|---|
| `HAILO_SIM_DEVICE` | `1` | `0`: VDevice creation fails, as with no HAT |
| `HAILO_SIM_DEVICE_MEMORY` | 8 GiB | A HEF larger than this fails to load |
| `HAILO_SIM_TOKEN_DELAY_MS` | `0` | Delay per generated token |
| `HAILO_SIM_HEF_SIZE` | 2 GiB | Apparent size of each seeded (sparse) HEF |
| `OLLAMA_HOST` | `0.0.0.0:8000` | hailo-ollama's listen address |

The entrypoint seeds a sparse HEF and version marker per installed
manifest, so `/api/pull` is a cache hit and never downloads. Generation is
deterministic: `hailo-sim:` followed by the last user message.

Not emulated: real token output, device memory used by the KV cache,
HailoRT's exact error codes for a HEF that does not fit, and the Pi 5's
16K kernel pages.
