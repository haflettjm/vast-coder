# Operations and verification notes

This is the durable record of the deployment, measurements, failures, and outstanding checks. Keep credentials, exact SSH endpoints, raw transcripts, and runtime state out of this document and out of Git.

## Scope and safety

- Keep the current rental. Bridge work must not stop, destroy, recycle, or recreate the Vast instance.
- Do not create another rental, increase the hourly cap, or change GPU power limits without approval.
- Preserve the exact DavidAU model and MTP GGUF file from `vast-coder.env` (Q5_K_S since 2026-10-09; Q6_K was the original baseline).
- Changes to the local SSH service are not changes to the rental.
- Never automatically replay inference requests or tool side effects after connection recovery.

## Verified deployment

- One SSH-only rental with two RTX 3090 GPUs; no public inference port or Jupyter.
- The host topology reports PCIe/PHB, not NVLink.
- The installed CUDA backend links NCCL.
- Model weights are cached in `/workspace/models`. Server-only restarts reuse them.
- Private Vast template synchronization is implemented and its definition is read back after writes.
- The template startup contains no raw model API key. Instance creation supplies the key separately; startup stores it in a root-only file.
- Local `.state/` holds private runtime state. AI instruction files, local Hermes homes, tracker databases, and credentials are Git-ignored.
- `vastcoder` is a named Hermes test profile. The default profile is not switched.

## Current inference configuration

- Context allocation: 262,144 tokens.
- Split: tensor across CUDA0 and CUDA1, equal proportions.
- MTP: `draft-mtp`, maximum two draft tokens.
- Four request slots with unified KV and continuous batching. Every slot can use the full 262,144-token pool; slots share it, they do not divide it.
- q8_0 K/V cache (`KV_CACHE_TYPE`); Flash Attention enabled; automatic context reduction disabled.
- Same Q6_K model file as the original baseline.
- Model server binds to remote loopback. Agent clients use the local loopback bridge.

Startup logs verified the MTP draft context and the 262,144-token allocation. `/props` reported the default speculative field as `none`, but actual response draft counters and log acceptance statistics proved that MTP was active. Do not use that default field alone as a readiness assertion.

## Measured speed

Same short coding prompt, 512-token output cap, sampling temperature 0.6, low reasoning effort, and deterministic request seeds. These are small-sample observations, not production capacity claims.

| Metric | Original layer / no-MTP / 64K | Tensor / MTP / 256K allocation |
| --- | ---: | ---: |
| Single-request server decode | 32.17 tok/s | 71.69 tok/s |
| Single-request end-to-end output | 26.51 tok/s | 49.18 tok/s |
| Two-request aggregate end-to-end output | 48.67 tok/s | 70.09 tok/s |
| Single-request MTP acceptance | Not applicable | 81.7% |

Generation improved by 2.23x on this short test. Server decode excludes prompt processing and network overhead; end-to-end results include both. User-observed peaks near 300 tok/s have not been independently established as sustained decode throughput.

`python3 bin/bench <label>` saves detailed local measurements under `.state/`. Those files are not published.

### Four slots and q8_0 KV cache (2026-10-09)

Same prompt, seeds and output cap as above, measured through the bridge. The model's trained context is 262,144, equal to the configured context, so extra memory cannot raise context without RoPE scaling (not attempted).

| Metric | 4 slots, FP16 KV | 4 slots, q8_0 KV |
| --- | ---: | ---: |
| Single-request server decode | 72.5 tok/s | 71.1 tok/s |
| Two-request aggregate end-to-end | 72.4 tok/s | 68.0 tok/s |
| MTP acceptance (single / dual) | 81.7% / 77.1% | 77.6% / 84.6% |
| GPU memory used (both GPUs) | 44.6 GB | 38.9 GB |

Going from 2 to 4 slots cost about 350 MB per GPU and no measurable speed. q8_0 K/V freed about 5.7 GB. Differences between the FP16 and q8_0 rows are within the noise of a single run on a lossy link. Tool-call round trip passed with q8_0. Output quality under q8_0 was not independently evaluated beyond that check. More slots do not fix the upload bottleneck described below.

## Functional checks

- Model listing returned `qwen3.8-coder`.
- A complete arithmetic tool-call/result round trip passed both before and after tuning.
- A Hermes coding workload created code, ran assertions, and repaired failures.
- An independent separated-endpoint check caught an interval-merging error that one generated solution's own tests missed. The coding model is not established as generally reliable by this workload.
- The initial workload runner omitted Hermes's explicit `--in` flag, so tools wrote in the wrong directory. The harness was corrected to pass `--in`; this was a harness error, not evidence that inference failed.

## Long-context stress test

The direct-API test used 249,021 input-text tokens (249,057 after chat formatting in the server log). It bypassed Hermes conversation autocompaction deliberately to exercise the server allocation.

- The model processed the near-full prompt without an observed startup OOM.
- Near-full prefill took roughly seven minutes and competed with live agent requests.
- A concurrent request exceeded the shared context pool and failed with `Context size has been exceeded`.
- The test was canceled. No successful final long-context response was obtained.
- Therefore, full-context response correctness and long-term stability are not validated.

A 256K unified pool is not two independent 256K allocations. Hermes autocompaction manages each conversation, not admission across separate server slots. Do not repeat a near-full-context stress test on a live server without coordinating with the user.

## Bridge investigation

The server's remote `/health` returned HTTP 200 while the local bridge on port 8000 timed out. SSH remained alive and the port stayed bound; service liveness was not inference readiness.

Earlier controlled probes: fresh temporary tunnels to the same server on ports 8001 and 8002 returned 200, option order made no difference, and a fresh manual tunnel on 8000 still timed out. That looked port-specific, but it was a confound.

### Root cause (verified 2026-10-09)

The upstream SSH TCP connection to the rental is congested and lossy, and everything forwarded over it shares one stream.

- Plain ICMP to the rental host showed 15% packet loss at about 230 ms RTT. The local Wi-Fi hop was clean (0% loss, 4 ms to the gateway), so the loss is on the long path, not the local network.
- The bridge `ssh` socket showed cwnd 2-3, roughly 700 to 1000 retransmitted segments, about 100 kbps effective throughput, and 60-95 KB of unsent data.
- Several Hermes sessions queue prompts of about 90 KB each. Clients time out and reconnect, so the local port accumulates many connections, most already in CLOSE-WAIT with unread request bytes. `ssh` forwards them in order, including requests whose client has gone.
- A `/health` request therefore waits behind the backlog and times out with zero bytes received. Fresh tunnels on other ports have an empty queue, which is why they looked healthy. Nothing local was intercepting port 8000, and no firewall rule was needed to explain the symptom.
- A fresh tunnel on 8000 fails again as soon as the Hermes clients reconnect and refill it.

Tried and not adopted (small samples, noisy shared link, 90 KB authenticated `/tokenize` upload on throwaway tunnels, two runs each): baseline 4.1 and 4.4 s, `-C` compression 4.6 and 4.6 s, `IPQoS=none` 7.1 and 7.4 s. Neither option helped.

### Consequences for the watchdog

A busy tunnel looks identical to a broken one over HTTP. Restarting a congested tunnel drops requests that are still in flight, so `bin/health --repair` now requires `HEALTH_FAILURES` (default 3) consecutive degraded checks before restarting, resets the count on any healthy or loading result, keeps the 120 s cooldown, and after a restart waits up to 30 s for SSH to reconnect before judging the result. It never touches the rental and never replays requests.

`tunnel-connected` now means the local port accepts TCP, so a stuck-but-listening tunnel reports `tunnel-connected: true, inference-ready: false, tunnel-degraded: true`.

### Remaining limitations

- The lossy path is a property of the network route to this rental, not something the bridge can fix. Lower prompt sizes, fewer concurrent Hermes sessions, or a rental with a better route are the real levers.
- Clients that time out and retry amplify the backlog. This is client behavior and is not changed here.
- Flushing a backlogged tunnel (restart) drops queued requests. Do it only when the queue is dead.

## Approved bridge improvements

1. End-to-end health probing with local/remote comparison, failure threshold, cooldown, and tunnel-only repair when the remote server is healthy. Offline tests pass; live `bin/health` correctly reported the degraded state without repair.
2. Bounded Vast/1Password discovery and a private cached SSH endpoint (24 h max age) for reconnects during temporary credential/API failures. Cache writes use unique temp files so concurrent writers cannot clobber each other. Absent or duplicate labels are authoritative and clear the cache.
3. Distinct readiness states: connected, loading, inference-ready, and unreachable, plus a degraded flag.
4. No automatic inference or tool-call replay.

The health timer is provided in `systemd/` but not yet installed or enabled.

## Loopback proxy (Go)

Why: one shared SSH stream over a lossy route let large queued prompts block everything, including `/health`, and clients that timed out left dead requests that were still forwarded. See `cmd/vast-proxy` and the README section for behaviour and flags.

Verified offline (race detector, no network): concurrency never exceeds the slot count; FIFO order; queue-full and wait-timeout return 503 with `Retry-After`; a client that disconnects while queued is never sent upstream; a failed upstream request is sent exactly once (502, no replay); server-sent events are flushed as they arrive; `/health` answers within a second while every slot is busy; reads bypass the queue; oversized bodies are refused; only loopback addresses are accepted; requests with a non-loopback `Host` header are refused with 403 and never reach the model server (DNS-rebinding protection, added after an automated review); pool balances load, survives one dead tunnel, reconnects after a drop and removes a tunnel whose keepalive fails; over a real in-process SSH server it carries HTTP, pins the host key on first use, refuses a changed key, falls back to the alternate route and refreshes discovery once when every route fails.

Mutation check: reading the request body only after taking a slot made the dead-client test hang, so that test does detect the regression. A second planned mutation (allowing connection reuse) was not completed.

Smoke run of the built binary against the current dead endpoint: `/health` answered immediately with `unreachable` and per-tunnel errors, and a POST returned 503.

Not verified: the proxy has not yet carried real traffic to a live model server, was not tested with the 1Password agent approval flow, and has not run as a service. The rental it was written for was not serving when it was finished (below). A real latency or throughput comparison against the SSH bridge is still to do.

## Engine decision (2026-10-09)

Outcome: stay on llama.cpp. vLLM works but reads prompts about 7 to 9 times slower on this GGUF.

### Current rental

- Instance 55039645 (label `vast-coder`), United States, 2x RTX 3090 (PCIe, no NVLink), about $0.527/hr. The earlier $0.351/hr cap in `vast-coder.env` is below this price, so a fresh `bin/up` against the same offer would refuse until `MAX_HOURLY_COST` is raised deliberately.
- Template `vast-coder-vllm-ssh` (id 758600) is attached to it, so the container image is `vllm/vllm-openai:v0.31.0`, not the llama.cpp image. A second template with the same name (id 758557) exists and makes `bin/template` ambiguous. It should be deleted by its owner.
- The Hebei rental from the earlier notes no longer exists, and its weights went with it.

### What was tried, in order

1. vLLM 0.31.0 with `vllm-gguf-plugin` from PyPI (0.0.5). Failed at weight loading with `RuntimeError: Unknown gguf model_type: qwen3_5`, raised from `weights_adapter/default.py`. The PyPI release has no Qwen3.5 adapter.
2. Setting `model_type` to `qwen3_8` in the local config. Wrong idea: transformers 5.17 does not know that type. (The README of the model does not ask for it.) Restored.
3. The plugin built from source (commit `e2b8ad532b8b`), which adds `weights_adapter/qwen3_5.py`. It must be installed into the system site-packages, because a check run inside the cloned repo imports the repo copy by accident while vLLM workers keep importing the old one. With it, the GGUF loads.
4. Tool calls returned HTTP 400 until the server was started with `--enable-auto-tool-choice --tool-call-parser qwen3_coder`. The reasoning text also leaked into message content until `--reasoning-parser qwen3` was added.
5. MTP did not start. vLLM builds the draft model config from the local GGUF file path and asks the Hugging Face hub for an image processor config under that path (`HFValidationError`). `--language-model-only` did not help. MTP stayed off.
6. Prefill measured at a flat 185 tok/s (5,925 tokens in 32 s, 11,844 in 64 s, 23,715 in 128 s). Not the prefill chunk size (2,048 and 16,384 gave the same), not the GDN backend (`--gdn-prefill-backend flashinfer` is unusable on sm86 and falls back to Triton/FLA), and not a missing extension (`_C_gguf` built, and Q5_K is in the plugin's CUDA GEMM list). The slow step was never isolated. The vLLM docs call GGUF "highly experimental and under-optimized", and plugin issue #142 reports that its bundled kernels come from an old llama.cpp snapshot.
7. Prefix caching works on vLLM (4,730 tokens: 25.4 s cold, 0.8 s repeated, 0.8 s with a new tail), but cold prompts stay slow.

### Measurements on the same file and hardware

`llama-bench`, Q5_K_S, q8_0 KV, flash attention, batch and micro-batch 2048, two repetitions:

| Engine and mode | pp4096 | pp16384 | tg128 |
| --- | ---: | ---: | ---: |
| llama.cpp `f39148a95`, layer split | 1,794.6 | 2,116.7 | 37.6 |
| llama.cpp, tensor split | 1,719.6 | 1,656.6 | 54.0 |
| ik_llama.cpp `5194a9e`, layer split | 1,300.0 | 1,245.9 | 38.8 |
| ik_llama.cpp, graph split | 1,659.1 | 1,632.2 | 42.1 |

The ik_llama.cpp graph run reported a 34.39 GiB model with 51.25 B parameters instead of 18.78 GiB and 26.9 B, so treat that row as unreliable. No ik_llama.cpp MTP run was done.

Live server (llama.cpp, tensor split, MTP draft 2, 4 slots, 262,144 shared context, q8_0 KV):

| Test | vLLM | llama.cpp |
| --- | ---: | ---: |
| Cold prefill, 23.7k tokens | 128 s | 18.3 s |
| One short request, decode | 50 tok/s | 88 tok/s (51 of 52 draft tokens accepted) |
| Four concurrent 29k-token prompts, 1,500 tokens each | 691 s | 163 s |
| Own marker echoed by each of the four answers | 3 of 4 | 4 of 4 |
| Per-stream decode averages during the four-job test | about 24 tok/s | 11.3, 13.7, 19.2, 30.9 tok/s |

During the four-job test, MTP acceptance was 71% to 76%. The per-stream decode figures are averages over each job's life, so jobs that finished late include time spent while the others were still reading prompts. Peak single-stream decode is the 88 tok/s above. GPU memory with four slots and 262,144 context was about 19.3 GB per GPU.

### How the live server was set up

The instance runs the vLLM image, which has no `llama-server`. Mainline llama.cpp was built inside the container, so the Docker image path in `bin/up` was not used. Reproduce it with:

```bash
apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq build-essential cmake libcurl4-openssl-dev
git clone --depth 1 https://github.com/ggml-org/llama.cpp /workspace/llama.cpp   # tested at f39148a95
cd /workspace/llama.cpp
cmake -B build -G Ninja -DGGML_CUDA=ON -DCMAKE_CUDA_ARCHITECTURES=86 -DCMAKE_BUILD_TYPE=Release -DLLAMA_CURL=ON
cmake --build build -j 32 --target llama-server llama-bench   # about 8 minutes on 64 cores
```

`/root/llama.sh` then runs `llama-server` with the same flags that `bin/onstart` prints for the llama.cpp engine, plus `-m` pointing at the local GGUF. A new rental created by `bin/up` with `ENGINE=llamacpp` uses the prebuilt image instead and needs none of this. That path was verified on the earlier Hebei rental with the Q6_K file. It has not been re-run on this host with the Q5_K_S file and the 2048 batch setting, so confirm it with `bin/check` and `bin/bench` after the next rebuild.

### Pitfalls found

- `pkill -f "vllm serve"` leaves the engine and worker processes holding GPU memory. Kill `VLLM::` workers too, or the next start fails with "Free memory on device ... is less than desired GPU memory utilization".
- vLLM's per-10-second stats credit a whole prompt to the window where it finishes, so prefill shows as `0.0 tokens/s` and then a spike. Measure prefill by wall clock.
- Wait loops that `pgrep -f` for a script name match their own command line when that name appears in the same script.
- llama.cpp logs `backend offload failed ... using CPU sampler` warnings for the speculative sampler. They are harmless.

### Still open

- Make `bin/onstart` or the template cover the in-container build, or move the instance back to the llama.cpp image template.
- Pin a verified llama.cpp image digest or commit for reproducible performance.
- The Go proxy has not carried live traffic.
- MTP draft length 2 versus 3, and the ik_llama.cpp MTP path, were not tested.
- The cold-prefill cost under four different agent system prompts at once is bounded by the heavy test above and has not been measured for real Hermes traffic.

## Further performance work

- Keep the measured tensor/MTP candidate and exact quant while testing one parameter at a time.
- Benchmark CPU thread counts rather than assuming the 48-thread default is optimal for single-token decoding.
- Measure cold prefill, cached tool turns, sustained decode, and concurrent aggregate throughput separately.
- Preserve the same prompts, sampling, model file, and output limits across comparisons.
- No GPU power-limit changes, model replacement, or paid migration is currently approved.
