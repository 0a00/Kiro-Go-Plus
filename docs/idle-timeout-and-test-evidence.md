# Idle Timeouts and Recovery Evidence

## Runtime Change in 1.2.91

Claude Code previously received an implicit extension: an argument-idle timeout
of 180 seconds with a legacy assembly value of 600 seconds became 600 seconds.
The upstream stream-idle timer could also be extended to that value. A tool
could therefore stop producing bytes much longer than the operator intended.

The explicit `toolArgumentIdleTimeoutSeconds` now wins for every client.
Zero uses `toolAssemblyTimeoutSeconds` as the legacy fallback; setting both to
zero disables that monitor. `streamIdleTimeoutSeconds` remains independent.
Neither limit is silently increased based on User-Agent or transparent mode.
These are idle limits, not total tool durations. Actual argument fragments,
including buffered ones, renew the tool timer; metadata and downstream pings
do not. A confirmed tool-idle timeout retains its specific diagnostic and bounded
recovery policy if another watchdog expires during cancellation.
Stream-idle errors are now labeled `stream_idle_timeout`, not first-output
timeouts, while retaining timeout status mapping and request-local retry policy.

There is no data migration. Existing configured limits take effect on deploy.
Operators intentionally relying on the old grace period can set the explicit
argument-idle and stream-idle values themselves. Code rollback restores the old
resolution behavior without changing stored settings. First-output deadlines,
tool budgets, JSON validation, and replay boundaries are unchanged.

This fixes unnecessary waiting, not the underlying provider's generation stalls.
Do not execute incomplete JSON or replay an already delivered turn. A shorter
first-output deadline (for example 60-90 seconds) should be evaluated separately
with thinking workloads before changing production.

## More Honest Test Results

- Claude Code's known synthetic "cut off mid-stream" continuation is counted
  only in standalone user records, not tool results or assistant prose. A
  successful case with it is WARN, with `automatic_stream_continuations=N`.
  `observed_stream_errors=N` records explicit client error events. These are
  observable client counts, not a complete count of internal server retries.
  Unknown future client message formats still require server-log correlation.
- Load reports include up to 32 `failureDetails` per result plus an omitted
  count. Each includes stage, fixed cause code, request ID when available,
  HTTP status, timing and stream completion evidence. Raw error strings, URLs,
  response bodies, prompts and tool arguments are deliberately excluded.
- The default acceptance model prefers advertised Sonnet 4.5 aliases. API,
  load and CLI phases use the same discovered model; explicit `--model` wins.
  The independent matrix still covers every advertised Claude name, including
  Haiku. Content mismatches and output limits remain failures, not successes.
- `--soak-interval` paces request starts while retaining request, token and time
  caps. Reports expose `soakDurationReached` (true when reached) and
  `soakStopReason`. Reaching the quota first yields WARN even if every response
  passed. The production soak uses a 3-second interval, five minutes, and at most
  101 requests / 3232 requested output tokens. This tests duration at low rate,
  not continuous ten-way saturation; use staircase for bounded bursts.

## References and Rejected Alternatives

Reviewed Kiro-Go `f8f6071` (stream integrity and no replay after flush), zsecducna
`7ee2ea4`, kiro.rs `5ca5703`/remote `e09625c`, Zhang `6c2c47b`/default HEAD
`5aa7f56`, account-manager `844aac76`, gateway `a5292ca`, AIClient2API `f790f4c`,
and login helper `8c280d7`. No directly reusable timeout-resolution or acceptance
diagnostics implementation was found. No comparison worktree was modified.

The gateway's `kiro/truncation_recovery.py` injects synthetic recovery messages.
That approach is not adopted: this change preserves transparent forwarding and
leaves recovery to the client once output has been delivered. Existing local
watchdogs, failure categories and jq evidence checks are extended instead.

## Verification

Regression coverage includes explicit/fallback/disabled timeouts, stalled and
continuously growing tools, incomplete-tool recovery without leaking arguments,
client recovery warnings and strict warning exits, bounded private diagnostics,
transport versus content classification, paced soak quota/duration boundaries,
baseline compatibility and default-model selection. Run
`bash scripts/dev-test.sh full` before deploying. Offline fixtures prove these
behaviors; they cannot guarantee live provider availability or resolve an
unreproduced 50-concurrency network failure by themselves.
