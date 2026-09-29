# Tool Delivery Experiments

These are development tools, not a new production stream mode. No account,
model routing or production settings are changed by the scripts.

## Client Capability Checks

`client-e2e.sh` probes the actual Claude Code init tool list before generating a
large file. It selects `Write` when available, otherwise `Edit` with an empty
`old_string`. It does not override client policy. Missing capabilities are a
skip; failed authentication, a missing init record or a capability change during
the workload is a failure, not a skip.

Two quota-consuming cases compare approximately the same total output:

- `workspace-large-write-progress`: one mutation with at least 40 KiB of content.
- `workspace-chunked-edit-progress`: ten bounded edits replacing independent
  placeholders, then a final read and disk check.

Both verify complete tool/result pairing, client errors and on-disk output.
Timing artifacts contain names, byte counts and arrival times, not arguments.
Raw client artifacts remain private and may contain generated file contents.
Some client error paths synthesize block stops. A stop alone is not proof of
completion: the timing collector validates the assembled object and the scenario
also requires successful tool results and on-disk content.

## Isolated Relay

Start an isolated Kiro-Go instance on `127.0.0.1:18089`, with one account,
`live` mode, fixed endpoint/model and background refresh disabled. Do not point
this experiment at a production service or disable its normal retry policies.

```bash
node scripts/dev-stream-relay.js
KIRO_DEV_BASE_URL=http://127.0.0.1:18090/early-start \
  bash scripts/client-e2e.sh \
  --scenarios workspace-large-write-progress,workspace-chunked-edit-progress \
  --model claude-sonnet-4-5 --agent-timeout 6m \
  --agent-max-budget-usd 3 --artifact-dir /tmp/kiro-progress-results
```

Supply `KIRO_DEV_API_KEY` through the environment. The relay binds only loopback
and only accepts explicit loopback upstream origins. It forwards authentication,
Claude beta queries and cancellations, bounds buffers, and never retries a turn.

Paths select delivery only:

- `/live`: forward tool start and argument deltas immediately.
- `/balanced`: hold tool start and arguments until validated completion.
- `/early-start`: forward the real tool start immediately, hold arguments until
  validated completion.

All modes reject incomplete or non-object tool JSON at completion. Errors do not
produce successful tool stops. Text, thinking and pings pass through. The
buffered path approximates *delivery* only: since the real backend is in live
mode, it does not reproduce production balanced mode's pre-output retry window.

Run cases serially with identical account, endpoint, model, budget and client
version. A single stochastic generation per case is diagnostic evidence, not a
statistical performance comparison. A start event is not proof of visible
terminal progress or of a safely executable tool; verify the real UI separately.

Reference: account-manager `844aac76` sends tool starts early and accumulates
arguments in `src-tauri/src/gateway/proxy/streaming.rs`. Rust `5ca5703`/Zhang
`6c2c47b` and AIClient2API `f790f4c` illustrate incremental delivery. This relay
adapts only delivery timing, not their permissive completion or recovery behavior.

Run `node --test scripts/client-e2e.test.js scripts/dev-stream-relay.test.js`
or `bash scripts/dev-test.sh quick` for offline validation.

## Observed Results (2026-09-29)

Single diagnostic run per cell, Claude Code 2.1.281, Sonnet 4.5, one fixed account
and CodeWhisperer endpoint. The client exposed `Read/Edit`, not `Write`.

| Delivery | Single 40+ KiB Edit | Ten smaller Edits |
| --- | --- | --- |
| Early start | Upstream truncation after ~178s; start at 10.1s | Completed, 68,381 bytes, ~330s |
| Live | Upstream truncation after ~225s; start at 8.0s, first delta 8.5s | Completed, 57,023 bytes, ~298s |
| Balanced delivery | Upstream truncation after ~195s; no tool exposed | Six edits completed, then HTTP 402 MONTHLY_REQUEST_COUNT; incomplete |

Failed large requests were stopped after their first recorded upstream failure
when the client retried; wall-clock abort times are not upstream failure times.
The account was not replaced after quota exhaustion, so the final cell is not a
fair success-rate comparison. Output sizes varied despite identical target sizes.

First successful small edit: early-start exposed the tool at 10.9s and arguments
at 34.9s; live exposed it at 11.5s and arguments at 11.9s (completion 33.1s);
balanced exposed both at 32.9s. Live still had ~20-23s argument gaps.

A separate fixed 13-second local SSE fixture was consumed by the real interactive
client for all three modes. Files were created correctly; tool details remained
completion-oriented while live deltas changed reception counters earlier. An
early start alone did not establish continuously visible tool progress. This was
a local simulated upstream/UI observation, not another production measurement.

Do not add a new production mode based on this sample. The useful result is to
test client capabilities and prefer bounded edits for large coding tasks; changing
delivery timing alone did not cure upstream truncation. Production was unchanged.
