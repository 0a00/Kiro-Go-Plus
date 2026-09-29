# Live Tool Progress

In 1.2.88, `live` streams the real tool name, ID and argument deltas as they
arrive. Completion is sent only after the arguments parse as a JSON object.
Malformed or truncated tools terminate with an error, without a successful tool
stop or a replay of already-visible content. Empty arguments remain supported
at the existing explicit/schema-authorized completion boundaries.

`balanced` still buffers complete tools. Both modes stream text; neither can
manufacture progress when upstream stops producing events. SSE pings keep the
connection alive but are not visible tool progress in Claude Code. Partial tool
delivery does not guarantee a particular terminal rendering or reduce generation
time. No synthetic progress text or tool results are injected.

## Upgrade and Rollback

Saved modes are unchanged. Upgrade before trying `live` for interactive coding;
keep Claude Code transparent compatibility enabled. Switch back to `balanced`
for clients that need complete tool input. No credential/data migration is needed.
After tool start/deltas reach the client, automatic whole-turn replay is unsafe.

## Diagnosis

Request logs, details and customer timing views add:

- `maxUpstreamReadGapMs`: maximum application-level body-read interval, including
  initial and trailing waits within each attempt, excluding retry backoff.
- `maxUpstreamFrameGapMs`: maximum complete EventStream-frame interval, including
  a terminal partial-frame wait. Heartbeats sent downstream do not reset it.
- `firstToolDispatchDelayMs`: time from the emitted tool's upstream start to its
  first downstream callback, not the sum of discarded attempts.

Read/frame intervals can include local callback/backpressure time. They are not
packet timestamps and cannot alone distinguish upstream batching from network
buffering. Timeline retention is bounded and favors startup, long gaps, tool
boundaries, errors and the recent tail. Sequence gaps indicate omitted events.

## Verification and References

Offline fixtures exercise 40/80 KiB tool bodies with a held-open tail, malformed
JSON, truncation, event/read gaps and bounded timelines. Run `dev-test.sh full`.
The optional `client-e2e.sh --scenarios workspace-large-write-progress` checks
file contents, paired tools and arrival timing of actual Claude Code partial
messages. Buffered/bursty progress is a warning, not a false pass. Use
`--fail-on-warning` when testing live mode; terminal rendering needs a separate
interactive check. Allow a sufficient per-client budget and agent timeout.

Release verification passed the full offline gate, additional repeated race
regressions, three real Claude Code file/multiturn/MCP cases, and eight protocol
checks on an isolated local instance against real upstream. The real 40+ KiB
single-write probe did **not** pass: early tool fragments arrived, followed by
an approximately 180-second upstream read gap and another truncated attempt.
The run was stopped rather than allowed to retry indefinitely. Interactive
Claude Code also continued to show its spinner during an upstream wait. Do not
interpret this release as eliminating upstream stalls or guaranteeing continuous
terminal progress. Production mode is not changed automatically.

Reviewed sibling refs on 2026-09-29: Rust `5ca5703` (latest `e09625c` changes CI
only), Zhang `6c2c47b`, and AIClient2API `f790f4c` provide incremental tool events.
The Go references `f8f6071`/`7ee2ea4` and gateway `a5292ca` retain assembled-tool
paths. Keep Plus's live callbacks but add strict completion validation; do not
copy permissive invalid-JSON recovery or synthetic tool results. Account-manager
`844aac76` and login-helper `8c280d7` add no required progress fix.
