# Vast Coder

Small, script-based infrastructure as code for a private coding-model endpoint on rented GPUs. Run a GGUF with llama.cpp on a dual RTX 3090 Vast.ai instance, then expose its OpenAI-compatible API through a reconnecting SSH bridge on your own machine.

The rental can move between hosts or countries without changing the endpoint configured in Hermes or another agent client. Instance IDs and SSH addresses are discovered by label, not baked into client configuration.

## Architecture

```text
Hermes / coding agent / batch client
                |
    http://127.0.0.1:8000/v1
                |
    local systemd user service
    SSH tunnel, rediscovered by label
                |
    rented 2x RTX 3090 instance
    llama-server on 127.0.0.1:8080
                |
       exact MTP Q6_K GGUF
```

Only SSH is exposed by the rental. The inference server and laptop bridge bind to loopback. No public inference gateway, Jupyter, custom HTTP proxy, Terraform stack, or Kubernetes cluster is needed.

## Model

- Repository: [DavidAU/Qwen3.8-27B-TURBO-Fable-Cold-Fusion-735-882-Heretic-Uncensored-NEO-CODER-MAX-MTP-GGUF](https://huggingface.co/DavidAU/Qwen3.8-27B-TURBO-Fable-Cold-Fusion-735-882-Heretic-Uncensored-NEO-CODER-MAX-MTP-GGUF)
- File: `Qwen3.8-27B-TurboFCFusion-735-882-Here-Uncen-NEO-CODER-MAX-MTP-Q6_K.gguf`
- API model alias: `qwen3.8-coder`
- Engine: `ghcr.io/ggml-org/llama.cpp:server-cuda`

The model file is explicitly selected. Scripts do not substitute a smaller model or another quant. MTP heads being present does not mean speculative decoding is enabled. Benchmark it separately before enabling it.

## Requirements

Linux with Bash, Python 3, OpenSSH, OpenSSL, `flock`, and systemd user services. Install the Vast CLI with its SDK, for example `uv tool install vastai`. Template synchronization uses the Python interpreter beside the installed Vast CLI.

Register an SSH public key with Vast.ai before starting. Unlock 1Password and make its CLI available to user services. The default API credential reference is `op://Personal/Vast.ai/credential`; change `VAST_KEY_REF` for your vault. An existing `VAST_API_KEY` environment variable takes precedence. Never put actual credentials in the checked-in config.

## Desired state and lifecycle

`vast-coder.env` is the source of truth for the label, offer filters, disk size, hourly price ceiling, countries, image, exact model, sampling, context, and ports. `bin/onstart` generates the container startup script from it. Both rental creation and template synchronization use that same script.

```bash
git clone https://github.com/haflettjm/vast-coder.git ~/git/personal/vast-coder
cd ~/git/personal/vast-coder

bin/offers                  # Read-only preview, including ingress charges
bin/template                # Create/update the private Vast template
bin/up                      # Rent only if the label is absent and no other rental exists
bin/status
bin/status --log
bin/ssh nvidia-smi
```

`bin/up` is create-if-absent, not a full reconciliation engine. Editing desired state does not silently recreate or mutate an existing rental. It refuses duplicate labels, competing local creation attempts, another unlabelled rental, and offers above `MAX_HOURLY_COST`.

The template is named `vast-coder-gguf-ssh`. It is private and contains no API key. Renting it manually requires supplying `LLAMA_API_KEY` in the instance environment; `bin/up` handles this automatically. Template updates are read back and verified. They do not alter existing instances.

## Stable bridge

```bash
systemctl --user link "$PWD/systemd/vast-coder-bridge.service"
systemctl --user daemon-reload
systemctl --user enable --now vast-coder-bridge.service
systemctl --user status vast-coder-bridge.service
journalctl --user -u vast-coder-bridge.service -f
```

The unit assumes the clone is at `~/git/personal/vast-coder`. If you clone elsewhere, use `systemctl --user edit vast-coder-bridge.service` and override `VAST_CODER_ROOT` in its `[Service]` section.

The service starts with the user service manager. It restarts failed/disconnected tunnels after 15 seconds, looks up the `vast-coder` label again, and resolves its current SSH address. Keepalives detect dead SSH connections. It never creates, stops, or destroys rentals. No-instance periods are expected during migration and will retry. If 1Password is locked or unavailable, discovery fails until credentials become available.

Use `bin/tunnel` instead for a foreground tunnel, but do not run it alongside the service on the same local port. Loopback access is still accessible to other local processes; API authentication remains required.

## Agent and inference clients

Configure an OpenAI-compatible client with:

```text
Base URL: http://127.0.0.1:8000/v1
Model:    qwen3.8-coder
API key:  contents of .state/llm_api_key (never commit or log it)
```

`bin/opencode-config` prints an OpenCode provider block that references the local key file without printing the key.

For Hermes, use the checked-in launcher:

```bash
bin/hermes --setup-only       # Configure a project-local, gitignored Hermes home
bin/hermes                   # Interactive coding agent through the bridge
bin/hermes -q "Explain this repository" --oneshot
```

It registers `custom:vast-coder` with the native `key_cmd` credential helper and Chat Completions transport. The API key is read internally from `.state/llm_api_key`, not stored in YAML or printed. The project-local `.hermes/` home is separate from the user's default provider configuration. First use may bootstrap an isolated Hermes runtime and take time; the CLI must be installed already. This launcher configures the client but does not provision GPUs or start the bridge.

See [Hermes named custom providers](https://hermes-agent.nousresearch.com/docs/integrations/providers#named-custom-providers). No Hermes plugin or custom proxy is needed.

After the model finishes downloading and loading:

```bash
python3 bin/check
```

This checks the model listing and a complete tool-call round trip: request an arithmetic tool, validate the arguments, execute it locally, return its result, and validate the final answer. A running container or successful SSH connection alone is not an inference-ready signal.

## Move the rental to North America

A region change replaces the rental and deletes its container-local files. Downloaded weights are not persistent across replacement. The local API key and agent URL remain unchanged if `.state/` is retained.

1. Set `COUNTRIES="US,CA"` in `vast-coder.env` (`US` for United States only).
2. Preview offers and confirm an affordable one before destroying anything.
3. Confirm the new hourly rate and ingress charges. A price-cap change is a deliberate user decision.
4. Stop active agent/batch jobs, then destroy and verify the old rental is gone.
5. Create the replacement, wait for loading, and run the live check.

```bash
COUNTRIES_OVERRIDE=US,CA bin/offers  # Preview without changing desired state
bin/offers --pick                  # Must succeed under the configured price cap
bin/template
bin/down                           # Interactive destruction confirmation
bin/status                         # Must report no instance before proceeding
bin/up
bin/status --log
python3 bin/check                   # After the replacement is inference-ready
```

The bridge will reconnect to the new labelled rental automatically. In-flight requests are not migrated or replayed. Retry them explicitly; never automatically replay tool side effects. Offer availability can change between preview and creation. If creation becomes unaffordable, `bin/up` fails rather than raising the cap.

Stopping a rental is different from destroying it. `bin/down` destroys it. Stopped rentals can still incur storage charges. Hourly offer estimates do not include all possible bandwidth charges, taxes, or credit fees.

## Dual-GPU and batch tuning

Start with llama.cpp's default layer split, full GPU offload, Flash Attention, and FP16 KV. Check `nvidia-smi topo -m`; two 3090s do not imply NVLink is installed. The initially provisioned host reports PCIe/PHB connectivity.

Continuous batching is enabled by default in the tested build. Concurrent client requests, server slots, and token batch size are different knobs. Bound client concurrency and benchmark two versus four concurrent requests using real coding prompts. Context memory is a shared budget, not an automatic 64K allocation for every job.

For interactive agents, measure time to first token and tool-step latency. For independent bulk jobs, measure aggregate output tokens/sec and total completion time. Save results incrementally with job IDs and bounded transient retries. Schedule bulk work separately if it interferes with interactive latency.

Do not enable experimental tensor splitting, CUDA peer-to-peer, aggressive KV quantization, or MTP merely because they exist. Verify output correctness and end-to-end speed first. Pin a verified container digest for reproducible performance; the default `server-cuda` tag moves.

References: [multi-GPU guide](https://github.com/ggml-org/llama.cpp/blob/master/docs/multi-gpu.md), [server settings](https://github.com/ggml-org/llama.cpp/blob/master/tools/server/README.md), [tool calling](https://github.com/ggml-org/llama.cpp/blob/master/docs/function-calling.md), [SPEED-Bench](https://github.com/ggml-org/llama.cpp/tree/master/tools/server/bench/speed-bench), [reported MTP multi-GPU prefill regression](https://github.com/ggml-org/llama.cpp/issues/27428).

## Checks and security

```bash
python3 test_lifecycle.py
systemd-analyze --user verify systemd/vast-coder-bridge.service
```

Offline checks use an explicitly fake Vast CLI. They do not rent GPUs. They cover the single-rental guard, price ceiling, region filtering, SSH-only startup, key permissions, and secret-free startup/template content. The GitHub workflow runs only offline checks; there are no cloud deployment credentials in CI.

`.state/` holds the local model API key and template identifiers with restrictive permissions. It is ignored by Git, as are local tracker databases and `.env`. Startup stores the remote API key in a root-only file rather than server command-line arguments. The public repository contains infrastructure code and docs, not model weights, runtime logs, or credentials.

## Verification status

The lifecycle checks, private template synchronization/readback, SSH-only rental startup, dual-GPU visibility, and local systemd tunnel have been exercised. Live model inference and tool-calling verification are still pending the initial download. No throughput claims are made without a completed benchmark.
