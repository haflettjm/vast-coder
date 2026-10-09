# vast-coder technical report

Date of the state described: 2026-10-09. Repository: `vast-coder`, branch tip `6dc6636` ("Record the engine decision: llama.cpp over vLLM for the Q5_K_S GGUF"). One working-tree change was uncommitted when this was written (`bin/_common.sh`, see section 6).

Conventions in this report: a number appears exactly as it was recorded in `docs/operations.md`, `docs/go-proxy-notes.md`, or the README. Numbers I derived myself are marked "derived". Anything nobody measured is marked "unmeasured" or "unverified".

## 1. Summary and current state

vast-coder is a small, script-based setup that gives Hermes coding agents a private OpenAI-compatible endpoint, `http://127.0.0.1:8000/v1`, with the model alias `qwen3.8-coder`. The model runs on a rented Vast.ai machine with two RTX 3090 GPUs. The rental exposes only SSH. A process on the workstation turns that SSH access into a loopback HTTP port.

What exists:

- Shell and Python tooling in `bin/` that searches offers, creates one labelled rental, generates a secret-free startup script, syncs a private Vast template, and checks health.
- A systemd user service (`vast-coder-bridge`) that keeps an `ssh -L` tunnel alive and rediscovers the rental by label.
- A Go program, `cmd/vast-proxy`, intended as a better replacement for that tunnel. It is built and tested offline only.
- Engine measurements comparing llama.cpp, vLLM and ik_llama.cpp on the same GGUF file and hardware.

Current state, as recorded:

| Item | State |
| --- | --- |
| Live rental | Instance 55039645, label `vast-coder`, United States, 2x RTX 3090 (PCIe, no NVLink), about $0.527/hr |
| Model file | `...NEO-CODER-MAX-MTP-Q5_K_S.gguf` (Q6_K was the first baseline) |
| Engine on the live box | Mainline llama.cpp commit `f39148a95`, built from source inside a container that was created from the vLLM image |
| Live server settings | tensor split, MTP draft 2, 4 slots, shared 262,144-token context, q8_0 KV cache, batch 2048 |
| Decision | Stay on llama.cpp. vLLM works on this file but reads cold prompts about 7 to 9 times slower |
| Go proxy | 13 offline Python tests and the Go test suite pass; it has carried no live traffic |
| Not done | Near-full-context test never completed; in-container build is not reproducible from `bin/up`; the watchdog timer is not installed |

The single most important open item is that the live server is not reproducible from the repository, and the proxy that was written to fix the connection problem has never been run against a live model server.

## 2. Architecture

```text
Hermes / coding agent / batch client
        |  HTTP, Bearer key from .state/llm_api_key
        v
http://127.0.0.1:8000/v1        (loopback only)
        |
   +----+-----------------------------+
   | Option A: ssh -L bridge          |  one SSH TCP stream, systemd user unit
   | Option B: Go vast-proxy          |  2 SSH connections, FIFO queue, own /health
   +----+-----------------------------+
        |  SSH, preferred route: Vast SSH proxy host (ssh_host/ssh_port)
        |                        alternate: instance public IP and mapped port
        v
Rented instance, 2x RTX 3090, only 22/tcp exposed
        llama-server on 127.0.0.1:8080  (API key from a root-only file)
        |
        v
Q5_K_S GGUF in /workspace/models, tensor split over CUDA0 and CUDA1
```

Components:

- **Client.** Hermes is launched by `bin/hermes`, which writes a project-local, gitignored Hermes home and registers `custom:vast-coder` with a `key_cmd` credential helper (`cat .state/llm_api_key`) and the Chat Completions transport.
- **Bridge or proxy.** Both bind `127.0.0.1:8000` and the proxy unit declares `Conflicts=` with the bridge unit, so only one runs. The bridge is `bin/tunnel` (an `ssh -N -L` with `ExitOnForwardFailure`, `ServerAliveInterval=30`, `ServerAliveCountMax=3`). The proxy is the Go binary.
- **Discovery.** `bin/endpoint` looks up the single instance with the configured label through the Vast CLI, resolves its SSH address, and writes `.state/ssh_endpoint.json` (mode restricted). It adds a second, alternate route when the instance record has one.
- **Rented instance.** SSH only. The model server binds to the instance loopback.
- **llama-server.** One process with the flags that `bin/onstart` prints: `--split-mode tensor --tensor-split 1,1`, `--parallel 4 --kv-unified --cont-batching`, `-b 2048 -ub 2048`, `--spec-type draft-mtp --spec-draft-n-max 2`, `--cache-type-k/v q8_0`, `--flash-attn on`, `--fit off`, `--jinja`, `--api-key-file /root/llm_api_key`, sampling `--temp 0.6 --top-p 0.95 --top-k 20 --min-p 0`.

Desired state lives in `vast-coder.env`. `bin/up` is create-if-absent: it refuses a duplicate label, a competing local creation (a `flock`), any other unlabelled rental, and an offer above `MAX_HOURLY_COST`.

## 3. The bridge reliability problem

### Symptoms

- The server's own `/health` returned HTTP 200 while the same request through local port 8000 timed out. SSH stayed up and the port stayed bound, so process liveness did not mean inference readiness.
- Fresh throwaway tunnels to the same server on ports 8001 and 8002 returned 200 immediately. This made the problem look port-specific. It was a confound: those tunnels simply had empty queues.
- The jam returned within 6 minutes of a clean bridge restart, with the GPUs idle (0% utilization) and remote `/health` at 200.
- Occasional connection loss: "closed by remote host" at 08:52 and 09:35, and a client-side "server not responding" at 09:23. A mass `Server disconnected without sending a response` hit three sessions at 09:18:20 and is unexplained.

### Root cause (verified, with one part still a hypothesis)

Verified:

1. The route to the first rental (a host in Hebei, CN according to `go-proxy-notes.md`) was lossy. ICMP showed 15% loss at about 230 ms RTT. The local Wi-Fi hop was clean (0% loss, 4 ms to the gateway).
2. Everything forwarded by `ssh -L` shares one TCP stream. On the bridge socket: cwnd 2 to 3, roughly 700 to 1000 retransmitted segments, about 100 kbps effective throughput, 60 to 95 KB of unsent data.
3. Hermes prompts are about 90 KB each (20k to 25k tokens, roughly 87 KB). Several sessions queued them. Clients timed out and reconnected, so the local port accumulated many connections, mostly in CLOSE-WAIT with unread request bytes. `ssh` forwards them in order, including requests whose client had gone. A `/health` request waited behind that backlog and returned zero bytes.
4. The model was not the bottleneck. Requests that reached the server finished in 3 to 10 s. The limit was the upload.
5. Hermes retries a failed API call up to 3 times with about 2 s backoff, so each failure triples the upload. About 8 concurrent `hermes -p vastcoder` sessions were running, with new ones starting every few seconds.

Not fully proven: why the SSH connection drops. The remote `sshd` uses `ClientAliveInterval 10` and `ClientAliveCountMax 2`, so it closes a silent connection after roughly 20 to 30 s. On a saturated stream the keepalive replies queue behind request data. The container log has no `sshd` lines, so the server-side message was never captured. The checked-in bridge uses `ServerAliveInterval=30` with `ServerAliveCountMax=3`, which is slower than the remote limit. The Go proxy sends keepalives every 5 s for this reason.

The route finding changed the picture. The same box also holds a reverse tunnel to a Vast SSH proxy host, about 43 ms from the workstation. Same 90 KB authenticated `/tokenize` upload, three runs each:

| Route | Upload time |
| --- | --- |
| Via the Vast SSH proxy host | 1.0 to 1.7 s |
| Via the direct-IP bridge | 6.4 to 11.8 s |

That is roughly 5 to 8 times faster through the proxy host. The proxy host's SSH forward presents the instance's own host key (identical ED25519 fingerprint to the direct endpoint), so the path is still end-to-end encrypted to the same machine. The notes recommend against using the proxy host's plain-HTTP model port, which is public and protected only by the API key.

### Tried and not adopted

On throwaway tunnels, 90 KB upload, two runs each: baseline 4.1 and 4.4 s, `-C` compression 4.6 and 4.6 s, `IPQoS=none` 7.1 and 7.4 s. Neither option helped. These are small samples on a shared link. The baseline of 4.1 and 4.4 s on throwaway tunnels also differs from the 6.4 to 11.8 s direct-IP figure above, which shows how much the link varied between measurements.

### Fixes

| Fix | Where | Status |
| --- | --- | --- |
| Prefer the Vast SSH proxy route, keep the direct IP as the alternate | `bin/endpoint` (`proxy_route`), `bin/ssh --alt`, `bin/tunnel` falls back once if the preferred route fails within 30 s | Offline test covers record shape; live effect measured only by the upload timing above |
| Discovery by label with a cached endpoint | `bin/endpoint`: bounded Vast and 1Password lookups, private cache with a 24 h maximum age, unique temp files for concurrent writers. A successful lookup is authoritative: an absent or duplicate label deletes the cache | `test_endpoint.py` passes |
| Distinct readiness states | `bin/health` reports `inference-ready`, `model-loading`, `unreachable`, plus `tunnel-connected` (local port accepts TCP) and `tunnel-degraded` (remote healthy, local not) | `test_health.py`, 13 tests, pass |
| Never replay requests | Stated policy in every layer; the proxy enforces it in code (section 4) | Tested offline for the proxy |
| Watchdog | `bin/health --repair` restarts only the local bridge after `HEALTH_FAILURES` (default 3) consecutive degraded checks, keeps a 120 s cooldown, waits up to 30 s for SSH to reconnect, and never touches the rental. A busy tunnel looks the same as a broken one over HTTP, which is why it needs consecutive failures | Offline tests pass. The timer unit exists but is not installed or enabled |

Remaining limits, as recorded: the lossy path is a property of the route to that rental; client retry behavior amplifies the backlog and was not changed; restarting a backlogged tunnel drops queued requests. Hermes `context_length` is 262144 with a 50% compression threshold (about 131k tokens), so compression never shrinks prompts that are already about 20k tokens. The base prompt (system prompt plus tool schemas) dominates.

The rental moved to a US host (California) later the same day. The recorded notes do not contain a bridge measurement on the new host, so whether the congestion problem persists there is unmeasured.

## 4. The Go loopback proxy

`cmd/vast-proxy` serves `http://127.0.0.1:8000/v1` like the bridge but manages the transport itself. Source files: `main.go`, `proxy.go`, `pool.go`, `limiter.go`, `sshtunnel.go`, each with a `_test.go`.

### Design

| Concern | Behavior |
| --- | --- |
| SSH pool | `-conns` (default 2) independent SSH connections, each its own TCP stream, 5 s keepalives, reconnect with backoff. Tries the preferred route, then the alternate from `.state/ssh_endpoint.json`. When every route fails it runs `bin/endpoint` to rediscover, at most every two minutes |
| Admission | POST requests take one of `-max-inflight` slots (default 4, set to the server slot count) in FIFO order. Others wait up to `-queue-wait` (default 30 s) and then get 503 with `Retry-After`. A full queue (`-max-queue`, default 64) is refused at once. GETs such as `/v1/models` skip the queue |
| Dead-client dropping | The request body is read locally first (limit `-max-body`, default 32 MiB, larger gets 413). After the body is consumed, Go notices a departed client and cancels the request context, so a queued request from a dead client never uses a slot or the slow link |
| No replay | Fresh channel per request, `DisableKeepAlives`, no `GetBody`. A failed upstream request is answered 502 with the text "it was not retried" and never resent |
| Independent `/health` | Answered by the proxy from tunnel state plus a probe of the server's own `/health` every 10 s. Never queues and never touches a tunnel. Returns `inference-ready` (200), or `model-loading`, `degraded`, `unreachable` (503), with tunnel and queue counters. A probe older than 45 s counts as stale |
| Host validation | Any request whose `Host` is not `localhost` or a loopback IP gets 403 (DNS-rebinding defense, added after an automated review). This applies before routing, including `/health` |
| Bind | Refuses to start unless the listen address is a loopback IP. The proxy adds no authentication of its own; the model server enforces the Bearer key |
| Host keys | Checks the user's `~/.ssh/known_hosts` first, then its own `.state/proxy_known_hosts`, pinned on first use. A changed key is always refused |
| Auth | ssh-agent (`SSH_AUTH_SOCK`, or the 1Password agent socket if present) and unencrypted key files |
| Streaming | `FlushInterval: -1`, so server-sent events are forwarded as they arrive |

The systemd unit runs it with `-conns 2 -max-inflight 4 -max-queue 64 -queue-wait 30s`, `NoNewPrivileges=yes`, `UMask=0077`, and `Conflicts=vast-coder-bridge.service`. Do not enable the watchdog timer next to it, because `bin/health --repair` restarts the bridge unit, which would stop the proxy.

### Verified offline

Per `docs/operations.md`, with the race detector and no network: concurrency never exceeds the slot count; FIFO order; queue-full and wait-timeout return 503 with `Retry-After`; a client that disconnects while queued is never sent upstream; a failed upstream request is sent exactly once; SSE is flushed as it arrives; `/health` answers within a second while every slot is busy; reads bypass the queue; oversized bodies are refused; only loopback listen addresses are accepted; non-loopback `Host` gets 403 and never reaches the model server; the pool balances load, survives one dead tunnel, reconnects after a drop, and removes a tunnel whose keepalive fails; over a real in-process SSH server it carries HTTP, pins the host key on first use, refuses a changed key, falls back to the alternate route, and refreshes discovery once when every route fails.

Also done: a mutation check (reading the body only after taking a slot made the dead-client test hang, so that test detects the regression) and a smoke run of the built binary against the then-dead endpoint (`/health` answered immediately with `unreachable` and per-tunnel errors; a POST returned 503).

While writing this report I re-ran `go test -race -count=1 ./...` and the three Python suites. The Python suites passed. The Go suite failed on the first run and then passed on six consecutive reruns. I did not capture the failing test name. Files `cmd/vast-proxy/sshtunnel.go` and `sshtunnel_test.go` were modified in the working tree at 13:52, shortly after that first run, by something other than this report's author, so the first failure may have come from a half-edited tree rather than a flaky test. Treat it as unexplained, and note that uncommitted proxy edits exist beyond what this report describes.

### Not verified

- The proxy has never carried real traffic to a live model server.
- It was not tested with the 1Password agent approval flow.
- It has never run as a systemd service.
- No latency or throughput comparison against the SSH bridge exists.
- The second planned mutation check (allowing connection reuse) was not completed.
- Whether per-request SSH channels over a pool of 2 actually avoid head-of-line blocking on a lossy path is a design expectation, not a measurement. Two TCP streams still share the same lossy route, and the preferred route is now the faster Vast proxy host.

## 5. Inference engine evaluation

### Setup and methodology

- Hardware: 2x RTX 3090, PCIe (PHB), no NVLink, on the US rental.
- Model: the same Q5_K_S GGUF for every engine.
- `llama-bench`: Q5_K_S, q8_0 KV, flash attention, batch and micro-batch 2048, two repetitions. `pp4096` and `pp16384` are prompt processing at those lengths; `tg128` is generation of 128 tokens.
- Cold prefill on live servers used unique prompts, so no prefix cache could apply. The vLLM prefix-cache check was run separately on purpose.
- Prefill on vLLM was timed by wall clock.
- Live llama.cpp configuration under test: tensor split, MTP draft 2, 4 slots, 262,144 shared context, q8_0 KV.
- Sample sizes are small. `llama-bench` has two repetitions; the live tests are single runs.

### Earlier measurements on Q6_K (before the engine comparison)

Same short coding prompt, 512-token output cap, temperature 0.6, low reasoning effort, deterministic request seeds, measured through the lossy bridge:

| Metric | Original layer split, no MTP, 64K | Tensor split, MTP, 256K allocation |
| --- | ---: | ---: |
| Single-request server decode | 32.17 tok/s | 71.69 tok/s |
| Single-request end-to-end output | 26.51 tok/s | 49.18 tok/s |
| Two-request aggregate end-to-end output | 48.67 tok/s | 70.09 tok/s |
| Single-request MTP acceptance | not applicable | 81.7% |

Server decode excludes prompt processing and network overhead; end-to-end includes both. The docs state a 2.23x generation improvement on this short test. User-observed peaks near 300 tok/s were not independently established as sustained decode.

Four slots and q8_0 KV cache (2026-10-09), same prompt, seeds and cap:

| Metric | 4 slots, FP16 KV | 4 slots, q8_0 KV |
| --- | ---: | ---: |
| Single-request server decode | 72.5 tok/s | 71.1 tok/s |
| Two-request aggregate end-to-end | 72.4 tok/s | 68.0 tok/s |
| MTP acceptance (single / dual) | 81.7% / 77.1% | 77.6% / 84.6% |
| GPU memory used (both GPUs) | 44.6 GB | 38.9 GB |

Going from 2 to 4 slots cost about 350 MB per GPU and no measurable speed. q8_0 freed about 5.7 GB. The FP16 versus q8_0 differences are within the noise of one run on a lossy link. A tool-call round trip passed with q8_0, but output quality under q8_0 was not otherwise evaluated. The model's trained context (262,144) equals the configured context, so extra memory cannot raise context without RoPE scaling, which was not attempted.

### Engine comparison on Q5_K_S: `llama-bench`

| Engine and mode | pp4096 (tok/s) | pp16384 (tok/s) | tg128 (tok/s) |
| --- | ---: | ---: | ---: |
| llama.cpp `f39148a95`, layer split | 1,794.6 | 2,116.7 | 37.6 |
| llama.cpp, tensor split | 1,719.6 | 1,656.6 | 54.0 |
| ik_llama.cpp `5194a9e`, layer split | 1,300.0 | 1,245.9 | 38.8 |
| ik_llama.cpp, graph split | 1,659.1 | 1,632.2 | 42.1 |

The ik_llama.cpp graph row reported a 34.39 GiB model with 51.25 B parameters instead of 18.78 GiB and 26.9 B. The docs treat that row as unreliable, and so should a reader. No ik_llama.cpp MTP run was done.

Layer split has better prefill than tensor split (notably at 16k) while tensor split has better single-stream decode, and only the tensor path was combined with MTP in the live tests. No layer-split live run with MTP is recorded.

### Engine comparison on the live servers

| Test | vLLM 0.31.0 | llama.cpp |
| --- | ---: | ---: |
| Cold prefill, 23.7k tokens | 128 s | 18.3 s |
| One short request, decode | 50 tok/s | 88 tok/s (51 of 52 draft tokens accepted) |
| Four concurrent 29k-token prompts, 1,500 tokens each, total time | 691 s | 163 s |
| Own marker echoed by each of the four answers | 3 of 4 | 4 of 4 |
| Per-stream decode averages in the four-job test | about 24 tok/s | 11.3, 13.7, 19.2, 30.9 tok/s |

During the llama.cpp four-job test, MTP acceptance was 71% to 76% (README says 71% to 98% across tests). Per-stream averages cover each job's whole life, so late finishers include time spent while the other jobs were still reading prompts. Peak single-stream decode is the 88 tok/s above. GPU memory with four slots and 262,144 context was about 19.3 GB per GPU.

Derived (not in the docs): 23.7k tokens in 18.3 s is about 1,300 tok/s on the live server, below the `llama-bench` figure of 1,656.6 to 1,719.6 for tensor split. The live number includes request overhead and the MTP draft path, so the two should not be compared as like for like.

### vLLM failure chain, in order

1. vLLM 0.31.0 with `vllm-gguf-plugin` 0.0.5 from PyPI failed at weight loading: `RuntimeError: Unknown gguf model_type: qwen3_5`, from `weights_adapter/default.py`. The PyPI release has no Qwen3.5 adapter.
2. Setting `model_type` to `qwen3_8` in the local config was the wrong idea: transformers 5.17 does not know that type. Restored.
3. Building the plugin from source (commit `e2b8ad532b8b`, which adds `weights_adapter/qwen3_5.py`) fixed loading. It has to go into the system site-packages. A check run inside the cloned repo imported the repo copy by accident while vLLM workers kept importing the old one.
4. Tool calls returned HTTP 400 until the server was started with `--enable-auto-tool-choice --tool-call-parser qwen3_coder`. Reasoning text leaked into message content until `--reasoning-parser qwen3` was added.
5. MTP did not start. vLLM builds the draft model config from the local GGUF path and asks the Hugging Face hub for an image processor config under that path (`HFValidationError`). `--language-model-only` did not help. MTP stayed off.
6. Prefill was a flat 185 tok/s: 5,925 tokens in 32 s, 11,844 in 64 s, 23,715 in 128 s. It was not the chunk size (2,048 and 16,384 gave the same), not the GDN backend (`--gdn-prefill-backend flashinfer` is unusable on sm86 and falls back to Triton/FLA), and not a missing extension (`_C_gguf` built, and Q5_K is in the plugin's CUDA GEMM list). The slow step was never isolated. The vLLM docs call GGUF "highly experimental and under-optimized", and plugin issue #142 reports that its bundled kernels come from an old llama.cpp snapshot.
7. Prefix caching works: 4,730 tokens took 25.4 s cold, 0.8 s repeated, and 0.8 s with a new tail. Cold prompts stay slow.

### Why llama.cpp

- Cold prefill is the dominant cost for agents with large system prompts and tool schemas. vLLM was about 7 times slower on the live comparison (128 s vs 18.3 s, derived ratio about 7.0) and about 9 times slower against the `llama-bench` tensor-split figures (derived from 185 vs 1,656.6 to 1,719.6).
- Only llama.cpp ran MTP on this file, which gave 88 tok/s on a short request against 50 tok/s for vLLM.
- Four concurrent 29k-token jobs took 163 s instead of 691 s, and all four answers carried their own marker.
- The cause of the vLLM prefill gap was not isolated, so the decision rests on measured behavior of this GGUF with this plugin version, and a different model format (a safetensors quant) was not tested.

### Measurement pitfalls found

- **vLLM statistics credit a whole prompt to the window where it finishes.** Its per-10-second stats show `0.0 tokens/s` during prefill and then a spike. Prefill must be measured by wall clock around the request.
- **Single runs on a lossy link are noisy.** The FP16 and q8_0 rows differ by amounts inside run-to-run variation.
- **Server decode versus end-to-end.** The two differ by prefill and network time. Per-stream averages in multi-job tests include waiting.
- **`/props` can report the default speculative setting as `none` while MTP is active.** Check draft counters and startup logs.
- **Streamed chunks and UI peaks are not sustained throughput.**
- **A short request with a 256K allocation does not prove long-context performance.**
- **Process hygiene.** `pkill -f "vllm serve"` leaves engine and worker processes holding GPU memory; kill the `VLLM::` workers too. A wait loop that `pgrep -f`s a script name can match its own command line. llama.cpp logs `backend offload failed ... using CPU sampler` warnings for the speculative sampler, which are harmless.
- **A harness error looked like an inference failure.** An early workload runner omitted Hermes's `--in` flag, so tools wrote to the wrong directory. This was a harness bug.

## 6. Operations

### How the live server was built

The instance runs the vLLM image (template `vast-coder-vllm-ssh`, id 758600), which has no `llama-server`. Mainline llama.cpp was built inside the container:

```bash
apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq build-essential cmake libcurl4-openssl-dev
git clone --depth 1 https://github.com/ggml-org/llama.cpp /workspace/llama.cpp   # tested at f39148a95
cd /workspace/llama.cpp
cmake -B build -G Ninja -DGGML_CUDA=ON -DCMAKE_CUDA_ARCHITECTURES=86 -DCMAKE_BUILD_TYPE=Release -DLLAMA_CURL=ON
cmake --build build -j 32 --target llama-server llama-bench   # about 8 minutes on 64 cores
```

`/root/llama.sh` runs `llama-server` with the flags from `bin/onstart` plus `-m` pointing at the local GGUF. A new rental from `bin/up` with `ENGINE=llamacpp` would use the prebuilt `ghcr.io/ggml-org/llama.cpp:server-cuda` image and need none of the build. That path was verified on the earlier Hebei rental with the Q6_K file. It has not been re-run with the Q5_K_S file and the 2048 batch setting.

Weights live in `/workspace/models`, so a server-only restart does not re-download. `bin/apply` pushes server-only changes without replacing the rental and keeps a one-level rollback script (`bin/apply --rollback`). A region change replaces the rental and deletes container-local files including the weights.

A second template with the same name exists (id 758557), which makes `bin/template` ambiguous. The docs say its owner should delete it.

### Security model

- Loopback only on both ends: the proxy refuses a non-loopback bind; the bridge forwards to `127.0.0.1`; `llama-server` binds the instance loopback. The rental exposes only 22/tcp.
- Other local processes can still reach loopback, so the API key stays required. The key is a random hex string generated once into `.state/llm_api_key` (mode 0600), passed to the instance as an environment value at creation, and written by the startup script into a root-only file read through `--api-key-file`, so it is not on a process command line.
- The generated startup script and the Vast template contain no key (checked by `test_lifecycle.py`, which uses a fake Vast CLI and rents nothing).
- The Vast API key comes from 1Password (`VAST_KEY_REF`) or `VAST_API_KEY`.
- Nothing stateful is committed: `.state/`, `.hermes/`, `.beads/`, `.dolt/`, `*.db`, `.env`, assistant instruction files, and the built `bin/vast-proxy` are gitignored. CI has no cloud credentials and runs only offline checks (Go format, vet and race tests, then the three Python suites, then shell and Python syntax checks).
- Host key handling differs by transport. The bridge uses `StrictHostKeyChecking=accept-new`; the proxy pins on first use and refuses changes.
- Uncommitted change at the time of writing: `bin/_common.sh` adds a cache of the Vast API key in `$XDG_RUNTIME_DIR/vast-coder-api-key` (RAM-backed, created with umask 077, default lifetime `KEY_CACHE_TTL` of 28800 s) so that each short-lived script does not trigger a fresh 1Password prompt. This is a deliberate trade of a short-lived plaintext copy in a per-user runtime directory against repeated prompts. It is not described in the README, and I did not evaluate it further.

### Cost notes

- Live rate: about $0.527/hr. The checked-in cap, `MAX_HOURLY_COST=0.35138499542366775`, is below that, so `bin/up` against the same offer would refuse until the cap is raised deliberately.
- `bin/offers` previews offers including ingress charges. The README warns that hourly estimates exclude some bandwidth charges, taxes and credit fees. Actual cumulative spend, including the engine tests and the earlier rental, is unmeasured here.
- Stopped rentals can still incur storage charges, and `bin/down` destroys the rental. The rental is on-demand (`is_bid: False`), not spot, so no preemption handling exists or is needed.
- Disk is 56 GB (`DISK_GB`).

## 7. Limitations and caveats

- **Small samples.** Throughput tables come from single runs or two repetitions on a noisy shared link. Differences of a few percent are not meaningful. These are not production capacity figures.
- **Single host.** All measurements are from one US rental (and the earlier Hebei host for the Q6_K and bridge numbers). Other hosts, drivers or PCIe layouts may behave differently.
- **Mixed quants.** The speed tables in section 5 up to the 4-slot comparison used Q6_K; the engine comparison used Q5_K_S. They are not interchangeable.
- **Long context is untested.** The 249,021-token direct-API test processed the prompt without a startup OOM, and prefill took roughly seven minutes while competing with live agent requests. A concurrent request exceeded the shared pool and failed with `Context size has been exceeded`. The test was cancelled and no successful final long-context response was obtained. Correctness and stability at full context are not validated. The 256K pool is shared by the four slots, not four times 256K.
- **Quality is barely tested.** A tool-call round trip passed before and after tuning, and a Hermes coding workload created code, ran assertions and repaired failures. An independent check caught an interval-merging error that a generated solution's own tests missed, so the model is not established as reliable. q8_0 KV quality was not evaluated beyond the tool-call check.
- **Stability.** The README notes open reports of lockups with tensor splitting plus MTP. A smoke test is not evidence of long-term stability.
- **Reproducibility.** The live server depends on a manual in-container build of an unpinned `master` checkout at `f39148a95`. The default `server-cuda` image tag also moves.
- **Proxy.** Claimed in design, tested offline, not run live (section 4).
- **Root cause residue.** The remote `sshd` keepalive explanation for connection drops is a likely cause, not proven.
- **Not tested at all:** MTP draft length 3 versus 2; the ik_llama.cpp MTP path; CPU thread counts (the 48-thread default is assumed, not measured); cold-prefill cost under four different real Hermes system prompts at once (bounded only by the synthetic 29k-token test); the vLLM safetensors path; `--cache-reuse` (the server logs that it is unsupported on this hybrid model).
- **Process note.** Several figures come from the automated assistant session's own runs. The commands and raw logs are not archived in the repository (`bin/bench` writes to `.state/`, which is not published).

## 8. Open work and recommended next steps, ranked

1. **Make the live server reproducible.** Either teach `bin/onstart` and the template to cover the in-container build, or move the instance back to the llama.cpp image template. Pin a llama.cpp commit or image digest. Delete the duplicate template (758557). Then re-run `bin/check` and `bin/bench` on a rebuild with the Q5_K_S file and 2048 batch. This is first because the working server cannot currently be recreated from the repo.
2. **Run the Go proxy against the live server.** Start it as the systemd unit with the real agent socket (including the 1Password approval flow), run `bin/check` through it, then compare upload time and `/health` behavior against the bridge on the current US host. Find out whether the congestion problem exists on the new host at all before investing further. Re-run the Go suite several times on a clean tree to rule out a flaky test (one unexplained failure was seen, see section 4), and commit or discard the uncommitted `sshtunnel.go` edits first.
3. **Fix the stated inconsistencies in the config and docs.** Decide the price cap (`MAX_HOURLY_COST` below the live rate), update the stale Q6_K line in "Current inference configuration", and remove or qualify the README sentence that no custom proxy is needed. Document the `_common.sh` key cache or drop it.
4. **Lower the load that causes the backlog.** Limit concurrent Hermes workers to the slot count (4), and consider reducing the base prompt or tool schema size. Client retries triple uploads, so retry behavior matters more than transport tuning.
5. **Finish the long-context test in a controlled window.** Coordinate with the user, run it with no competing requests, and verify answer correctness. Add admission control for the shared pool if concurrent long requests are expected.
6. **Measure real agent traffic.** Cold prefill with four distinct Hermes system prompts in parallel, time to first token, and tool-step latency, since the heavy test is synthetic.
7. **Install or retire the watchdog.** If the bridge stays in use, install `vast-coder-health.timer`. If the proxy replaces it, remove the timer from the plan, because it conflicts with the proxy unit.
8. **Tune one variable at a time.** MTP draft 2 versus 3, CPU threads, and the ik_llama.cpp MTP path. Compare layer split versus tensor split for prefill-heavy workloads, since layer split prefilled faster in `llama-bench`.
9. **Quality checks for q8_0 KV** against FP16 on a coding task set, if the cache precision is to be trusted.
