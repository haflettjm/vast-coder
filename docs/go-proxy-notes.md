# Notes: moving the bridge to a Go proxy

Working notes from the 2026-10-09 debugging session. Nothing here is committed policy. Keep credentials and exact endpoints out of this file.

Historical context: the measurements below describe the first rental (Hebei, CN, two slots). That rental is gone. The current rental is in the United States with four slots, and the engine decision is in `docs/operations.md`. The congestion mechanics (one shared TCP stream, queued large prompts, dead clients) are why the proxy exists and still apply to any lossy path.

## Why the SSH bridge jams (verified)

- The rental is in Hebei, CN. ICMP from the workstation showed 15% loss at about 230 ms RTT. The local Wi-Fi hop was clean (0% loss, 4 ms to the gateway).
- All forwarded requests share one SSH TCP stream. The socket showed cwnd 2-3, about 700-1000 retransmitted segments, about 100 kbps, and 60-95 KB unsent.
- Hermes sends prompts of about 90 KB. Clients time out and reconnect, so port 8000 piled up dozens of connections, mostly dead (CLOSE-WAIT with unread bytes). `ssh` forwards them in order, so `/health` waited behind the backlog.
- Fresh tunnels on 8001/8002 only looked healthy because their queues were empty. Nothing local intercepts port 8000.
- Restarting the local tunnel clears the dead queue. The jam returns if the same clients keep sending large prompts over the same path.

## Tried, no gain

On throwaway tunnels, 90 KB authenticated `/tokenize` upload, two runs each: baseline 4.1 and 4.4 s, `-C` 4.6 and 4.6 s, `IPQoS=none` 7.1 and 7.4 s. Small samples on a shared link.

## Facts about the current rental

- On-demand (`is_bid: False`), not spot. Preemption handling is not needed today.
- Stock `ghcr.io/ggml-org/llama.cpp:server-cuda` image. No Vast Instance Portal, no automatic Cloudflare tunnel.
- Only `22/tcp` is exposed. Opening another port needs a new rental (not approved).
- Engine is llama.cpp, not vLLM. The bearer token is already enforced by the server.
- `cloudflared` is installed locally but not on the rental.

## Evaluation of the pasted advice

Applies:
- A local proxy that Hermes points at, with the upstream swappable, so client config never changes.
- Clean 503 with Retry-After while the upstream is down, instead of hanging agents.
- A health loop polling the upstream, and a request concurrency cap matched to the server (`--parallel 2` here, not 8).
- Idempotent retries by clients on 502/503. The proxy itself must not replay inference or tool calls.

Does not apply: spot bidding, Portal tunnels, `-p` Docker options, vLLM flags, K3s (unless wanted).

## Options for the transport

- A: one SSH connection per request (`ssh -W 127.0.0.1:8080`, for example via a systemd `Accept=yes` socket). Removes head-of-line blocking, costs a handshake per request on a lossy link. Needs nothing on the rental.
- B: `cloudflared` on the rental. New public path into a paid instance and a change inside the container, so it needs explicit approval. Reachability from mainland China may be unreliable. Test before committing.
- A Go proxy can implement A natively with `golang.org/x/crypto/ssh` (pool of connections, per-request channel or per-request connection) and own the queueing policy.

## Go proxy requirements (draft)

- Listen on 127.0.0.1:8000, forward to the model server on the remote loopback.
- Discover the rental by label `vast-coder` (reuse the cache semantics of `bin/endpoint`: 24 h max age, authoritative on absent or duplicate label).
- Bounded dials and lookups. Never print credentials or secret-bearing arguments.
- Admission control: cap in-flight requests at the server slot count, queue with a deadline, and shed with 503 when full. Drop requests whose client has disconnected before forwarding (this was the main waste).
- Health: `/health` answered by the proxy itself, reporting tunnel connected, model loading, inference ready, degraded, without queuing behind inference traffic.
- No automatic replay of inference requests or tool side effects.
- Streaming passthrough (SSE) with flush.
- Reload upstream from env var or small file.

## Open decisions

- Repo name and location for the Go repo.
- Which transport to build first (A is the proposed start).
- Whether Hermes keeps a 262144 context setting given the 90 KB prompts over this path.

## Deferred

- Install and enable `systemd/vast-coder-health.timer`.
- Run `bin/check` and a small Hermes request through port 8000.
- CPU thread tuning, one variable at a time, after the link is stable.

## Follow-up findings (same day, after the bridge restart)

- The jam returned within 6 minutes of a clean restart. GPUs were idle (0% util) and the remote `/health` was 200. The server log shows requests that reach it finish in 3 to 10 s (prompts of 20k to 25k tokens, roughly 87 KB each). The limit is the upload, not inference.
- Clients: the Hermes Desktop app and a systemd-launched runner, about 8 concurrent `hermes -p vastcoder` sessions, with new sessions starting every few seconds.
- Hermes already retries failed API calls up to 3 times with about 2 s backoff, so each failure triples the upload. For a local endpoint with no explicit setting, Hermes disables its stale-call detector, and the API request timeout defaults to 1800 s. The 180 s value in `hermes config show` is the terminal tool, not the API.
- The vastcoder profile has `context_length` 262144 and compression threshold 50% (about 131k tokens), so compression never shrinks prompts that are already about 20k tokens. The base prompt (system prompt plus tool schemas) dominates.
- A mass `Server disconnected without sending a response` hit three sessions at 09:18:20 and is unexplained. The bridge logs nothing about it. Raise ssh log level for the bridge to capture channel errors.
- Vast docs: opening ports uses Docker options or EXPOSE (needs a new rental here). The Instance Portal creates Cloudflare quick tunnels for open ports on Vast base images. One doc page says a Tunnels page can create tunnels for ports not opened in the template, and `CF_TUNNEL_TOKEN` is added to `/etc/environment` plus a reboot for named tunnels. Not confirmed for a stock llama.cpp image, and a reboot is not allowed without approval.

## Route finding: the Vast proxy path is much faster (2026-10-09)

- The direct endpoint (public IP and mapped port) crosses a lossy international path: about 13-15% ICMP loss at 230 ms. The same box also keeps a reverse tunnel to a Vast SSH proxy host (about 43 ms from the workstation) that forwards SSH and the model port.
- Same 90 KB authenticated `/tokenize` upload, three runs each: via SSH through the proxy host, 1.0 to 1.7 s; via the direct-IP bridge, 6.4 to 11.8 s. That is roughly 5 to 8 times faster.
- The proxy's SSH forward presents the instance's own host key (identical ED25519 fingerprint to the direct endpoint), so it is end-to-end encrypted to the same machine. Prefer it over the proxy's plain-HTTP model port, which is a public, unencrypted port protected only by the API key. Do not send prompts or the key over that port.
- The proxy hostname and port come from the instance record (`ssh_host`, `ssh_port`), so discovery by label still works.

## Why the bridge connection drops (likely cause, not fully proven)

- The remote `sshd` uses `ClientAliveInterval 10` and `ClientAliveCountMax 2`, so it closes a connection after about 20 to 30 s without a keepalive reply. On a saturated, lossy stream the replies queue behind request data.
- Observed drops today: "closed by remote host" at 08:52 and 09:35, and a client-side "server not responding" at 09:23. Each kills every in-flight request, and Hermes retries refill the queue.
- The container log has no `sshd` lines, so the server-side message was not captured. The client reconnects within about 15 s via systemd.

## Status update

The proxy described above now exists as `cmd/vast-proxy` in this repo (not in `hermes-llm-gateway`, whose `task-runner` starts worker processes and has no concurrency limit in code despite what its README says). See `docs/operations.md` for what is verified. Open items: run it against a live server, decide whether to put it behind the Bifrost gateway as a custom provider (the gateway runs in Docker, so it needs host networking or `host.docker.internal` to reach a loopback proxy), and lower the worker concurrency that feeds it to the server's slot count.
