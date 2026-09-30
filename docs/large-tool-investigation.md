# Large Tool Investigation (1.2.92)

## What Was Reproduced

Controlled diagnostic probes used Sonnet 4.5, one Edit schema and a 32,000-token
request budget. Tools were validated but never executed. The diagnostic client
sent one request and performed no retries; production could still retry before
output. A separate loopback instance fixed the previously failing account and
CodeWhisperer endpoint, disabled all retries and background refresh, selected
live delivery and allowed 360 seconds of argument/stream inactivity. Production
settings were not changed.

| Probe | Observed result |
| --- | --- |
| 42 generated lines, production | Complete 5.5 KB tool in 22.6s |
| 420 copied lines, production | Complete 56.8 KB tool JSON in 75.8s; strict exact-copy check failed |
| 420 generated lines, production, thinking disabled | No tool delivered before the 330s probe limit; server attempted two endpoints |
| 420 generated lines, production, thinking enabled | Argument-idle error after about 191s |
| 420 generated lines, fixed account, 360s idle limit | Upstream ended at 200.9s with 54 incomplete argument bytes |
| 160 generated lines, same fixed account | Complete 22.3 KB tool in 89.2s |
| 420 generated lines, same account, long-tool guidance disabled | Upstream ended at 136.0s with 54 incomplete argument bytes |
| 420 copied lines, same account, guidance disabled | Complete 56.8 KB tool JSON in 75.0s; strict exact-copy check failed |

Each cell is one stochastic generation, not a statistical success-rate estimate.
Copied output had the expected 420 numbered lines but did not exactly equal the
input; JSON delivery success must not be reported as exact task completion.
Different production probes can select different accounts. The isolated rows
above used the same account, credential and endpoint.

## Cause and Limits of the Evidence

Successful tools also exhibited a gap after the short JSON prefix: approximately
19 seconds for a small tool, 67 seconds for copied large text, and 84 seconds for
160 generated lines. Data then arrived in many small fragments. This is consistent
with upstream generation or buffering of the long string field, not continuous
downstream token delivery. Application body-read timestamps do not reveal the
provider's internal implementation or prove where inside its stack buffering occurs.

The original 90-byte stall and the probe's 54-byte stall have different short
file paths; neither means 90 bytes is a size ceiling. A complete tool larger than
56 KB can cross the same proxy. `defaultMaxToolTokens=8192` is guidance/fallback
metadata, not a verified upstream hard limit. Converting it to `tokens * 4` as a
hard byte cap would reject tools that can succeed. That approach was rejected.

The isolated 360-second experiment ended upstream before either proxy idle
deadline. Disabling thinking or long-tool guidance did not make the generated
large tool reliable. Neither extending every deadline nor changing delivery mode
is established as a complete fix. Chunked edits remain the demonstrated practical
mitigation; never split opaque tool JSON or execute an incomplete call.

## Scoped Fixes

An account binding survived truncated/stalled tool requests. Client retries could
therefore keep selecting the same account even after the failed generation.
1.2.92 forgets only the binding acquired by that request on
`tool_assembly_timeout` or `tool_output_truncated`. It does not ban/cool the whole
account, erase other sessions, override newer concurrent bindings, add retries,
or replay a turn after client-visible output. Ordinary selection can still choose
the same account; this removes forced affinity, not guarantees of success.

Detailed tool records now retain `maxFragmentGapMs` and `bytesBeforeMaxGap`,
including the final incomplete-tool gap. These counters survive timeline
eviction and contain no raw arguments. They distinguish a stall near the JSON
prefix from a later pause after substantial output. They do not declare a
provider token limit. Existing detailed-log enablement and retention apply.

CLI reports say "recovery attempted; final status=..." instead of claiming
"recovered" when the task ultimately failed. Explicit error flags still override
a misleading successful subtype.

No configuration migration is needed. Roll back the code to restore the old
binding behavior. Production timeout values, tool budgets and stream mode remain
unchanged. Provider-side large-generation failures are still a known limitation.

## Repeatable Probe

Use a dedicated quota-limited test key via `KIRO_DEV_API_KEY`. The CLI requires
`--confirm-live`, and non-loopback URLs also require `KIRO_DEV_ALLOW_REMOTE=1`.
Set `KIRO_DEV_BASE_URL` and optionally `KIRO_DEV_MODEL`. No key goes in arguments.

```bash
KIRO_TOOL_PROBE_LINES=42 node scripts/tool-field-probe.js --confirm-live
KIRO_TOOL_PROBE_LINES=420 KIRO_TOOL_PROBE_COPY=1 \
  node scripts/tool-field-probe.js --confirm-live
KIRO_TOOL_PROBE_LINES=420 KIRO_TOOL_PROBE_THINKING=1 \
  node scripts/tool-field-probe.js --confirm-live
```

`KIRO_TOOL_PROBE_TIMEOUT_MS` defaults to 330000, maximum 900000.
`KIRO_TOOL_PROBE_REPORT` creates a new private JSON file without overwriting one.
Reports separate `toolDelivered` from `valid` (content acceptance), contain only
timings/counts/status and request IDs, and never persist arguments or raw errors.
Wire data is capped at 16 MiB and each tool at 1 MiB. The probe executes no tool,
but real model requests consume quota. Use an isolated account/server for fair
endpoint, prompt, budget and timeout comparisons.

## References and Verification

Reviewed Kiro-Go `f8f6071`, zsecducna `7ee2ea4`, kiro.rs `5ca5703` (remote
`e09625c`), Zhang `6c2c47b` (default `5aa7f56`), account-manager `844aac76`,
gateway `a5292ca`, AIClient2API `bf53f87`, and login helper `8c280d7`. Go's bounded
truncation handling and no-replay-after-flush boundary were retained. Rust's
chunking guidance already has a local counterpart. Gateway synthetic recovery
messages were not adopted; no comparison worktrees were modified or code ported.
The new AIClient2API commit changes non-Kiro model configuration only.

Regression tests cover request-scoped binding removal, concurrent newer bindings,
unrelated errors, no replay of delivered text, argument redaction/gap retention,
probe bounds, transport/content separation, and CLI recovery wording. Run
`bash scripts/dev-test.sh full`. Tests cannot promise upstream availability.
