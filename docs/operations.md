# Operations and verification notes

This is the durable record of the deployment, measurements, failures, and outstanding checks. Keep credentials, exact SSH endpoints, raw transcripts, and runtime state out of this document and out of Git.

## Scope and safety

- Keep the current rental. Bridge work must not stop, destroy, recycle, or recreate the Vast instance.
- Do not create another rental, increase the hourly cap, or change GPU power limits without approval.
- Preserve the exact DavidAU model and MTP Q6_K file from `vast-coder.env`.
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

## Migration to vLLM and a US rental (in progress, 2026-10-09)

- The previous rental (llama.cpp, Q6_K, Hebei) no longer exists on the account, so its cached weights are gone.
- A new instance in the United States (2x RTX 3090, about $0.351/hr) was rented from the console. It started on Vast's stock vLLM template. It was labelled `vast-coder` and switched with `vastai update instance` to the private template `vast-coder-vllm-ssh` (image `vllm/vllm-openai:v0.31.0`, Q5_K_S MTP GGUF, tensor parallel 2, fp8 KV cache, MTP with 2 speculative tokens).
- After roughly an hour the instance was still `loading` with no container logs, and SSH was refused on every route. Nothing was served, so GGUF loading, MTP and the speed of vLLM on this model are unverified.
- Decision needed: wait, reboot, or recreate the instance from the template. Creating a second instance with `bin/up --additional` is supported.
- The vLLM startup installs `vllm-gguf-plugin` from PyPI without a pinned version at boot. Pin it once a working version is known.

## Further performance work

- Keep the measured tensor/MTP candidate and exact quant while testing one parameter at a time.
- Benchmark CPU thread counts rather than assuming the 48-thread default is optimal for single-token decoding.
- Measure cold prefill, cached tool turns, sustained decode, and concurrent aggregate throughput separately.
- Preserve the same prompts, sampling, model file, and output limits across comparisons.
- No GPU power-limit changes, model replacement, or paid migration is currently approved.
