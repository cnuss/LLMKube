# hailo-sim

The real [hailo-ollama](https://github.com/hailo-ai/hailo_model_zoo_genai)
5.1.1 server, the version Raspberry Pi ships for the AI HAT+ 2, compiled
against HailoRT 5.1.1's public headers and linked to `fake_hailort.cpp`, a
stand-in `libhailort` that emulates a Hailo-10H. The HTTP layer, chat
templates, model store, pull cache and model swapping are hailo-ollama's own
code; only the device is fake. Files are laid out as Hailo's
`hailo_gen_ai_model_zoo_5.1.1_arm64.deb` lays them out.

```sh
make docker-build-hailo-sim
docker run --rm -p 8000:8000 localhost/llmkube-hailo-sim:dev
hack/hailo-probe.sh http://127.0.0.1:8000
```

| Variable | Default | Effect |
|---|---|---|
| `HAILO_SIM_LISTEN` | `0.0.0.0:8000` (the packaged config) | hailo-ollama's listen address, written into `hailo-ollama.json` |
| `HAILO_SIM_LIBRARY_LISTEN` | `127.0.0.1:8001` | Serves the seeded HEFs at `/blob/sha256_<digest>` for HEAD requests, as `dev-public.hailo.ai` does; empty turns it off |
| `HAILO_SIM_DEVICE` | `1` | `0`: VDevice creation fails, as with no HAT |
| `HAILO_SIM_DEVICE_MEMORY` | 8 GiB | A HEF larger than this fails to load |
| `HAILO_SIM_TOKEN_DELAY_MS` | `0` | Delay per generated token |

Build argument `HAILO_SIM_HEF_SIZE` (default 64 MiB) sets the size of each
fake HEF. The build writes one per installed manifest, points the manifest's
`hef_h10h` at its digest and seeds it in the blob cache, so `/api/pull` is a
cache hit and never downloads. Generation is deterministic: `hailo-sim:`
followed by the last user message.

Not emulated: real token output, device memory used by the KV cache,
HailoRT's exact error codes for a HEF that does not fit, and the Pi 5's
16K kernel pages.
