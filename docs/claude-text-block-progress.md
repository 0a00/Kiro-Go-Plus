# Claude Code Text-Block Progress

## Reproduction

Claude Code 2.1.284 was exercised in a real PTY with a fixed local SSE fixture.
It received thinking, a short text preamble, then a 12-second tool wait. At
4/8/12 seconds, a still-open text block rendered as a blank bullet even though
all text deltas had already arrived. Removing leading whitespace did not help.
Sending `content_block_stop` for that same text before the wait made the preamble
visible at all three observations. The tool executed successfully in every case.

The reported production request also had a preamble and buffered Edit arguments.
An earlier test asserted first text-delta arrival, not terminal rendering, so it
missed this distinction. This is separate from upstream read gaps: closing a text
block cannot generate missing upstream tokens or speed up tool assembly.

## Fix

In balanced mode only, an upstream tool-start boundary now finishes the existing
text/thinking block. The message remains open and heartbeats continue. Arguments
and completion are held until validated JSON is available. From 1.2.94, a
recognized Claude Code request with committed output can receive the real tool
name and ID early. Silent attempts and other clients still hold the start. Later
text uses a new content-block index, preserving every text character and order.

The boundary callback runs only after output has already been emitted. Silent
tool-only turns remain retryable, safe-mode pending text is not flushed, and no
tool execution, synthetic status message or whole-turn replay is introduced.
An announced tool that fails remains unfinished: no empty arguments or successful
tool stop are fabricated. Name restoration applies to early metadata as well as
completed tools, including namespaced MCP names.

No new setting or mode is needed. Saved balanced/live/safe/adaptive values are
unchanged. 1.2.90 introduced the text boundary; 1.2.94 adds the restricted early
metadata. Rollback needs only the previous binary. There is no credential or
persisted-data migration.

## Tests

Offline regression fixtures hold an unfinished tool open, require the preamble's
block stop to arrive before release, check native/tagged thinking, exact text,
balanced indexes, later text, and pre-output retry boundaries. Existing malformed
tool, cancellation and replay tests remain required.

For real terminal verification without production credentials or quota:

```bash
uv run --with pyte==0.8.2 python scripts/client-progress-ui.py
uv run --with pyte==0.8.2 python scripts/client-progress-ui.py --outcome truncated
uv run --with pyte==0.8.2 python scripts/client-progress-ui.py --tool-stream-mode live
```

Requires Go, uv and an installed Claude Code on Linux. The script starts the real
Go handler with a fake AWS EventStream server bound to loopback, uses a private
temporary workspace/config and a fake key, and records PTY screen snapshots. It
acknowledges onboarding only for its own fixture. It does not modify user settings
or bypass managed policies. Unsupported CLI onboarding/tool changes fail visibly.
The fixture and terminal have bounded lifetimes. Raw terminal artifacts stay local.
The fixture also captures fake-upstream SSE privately. `toolProgressStatus` is
separate from file/lifecycle acceptance: WARN means no early tool row was seen.
Use `--require-tool-progress` to make this an explicit failure. Truncation must
display an error, leave the file absent, and never complete an announced tool;
content-block indexes are validated separately for each response.

## Reference Review

Sibling refs checked 2026-09-29: Go f8f6071/7ee2ea4, Rust e09625c (stream code
5ca5703), Zhang main 6c2c47b, account-manager 844aac76, gateway a5292ca,
AIClient2API f790f4c, login-helper 8c280d7. Rust/AIClient2API and account-manager
close text at tool-start boundaries; the Go assembled-tool path closes it later.
Adapt the boundary, not early tool delivery, whole-response buffering, whitespace
trimming or synthetic recovery. Comparison worktrees were not changed.

This fixes delayed display of an existing preamble, not continuous progress for
tool-only requests. Claude Code versions and clients can render differently.

## Follow Up in 1.2.94

The 2026-10-03 review found the same sibling remote heads: Go f8f6071/7ee2ea4,
Rust e09625c (CI only, stream implementation 5ca5703), Zhang 5aa7f56 (README),
account-manager 844aac76, gateway a5292ca, AIClient2API 9a29d60 (reasoning-effort
forwarding), login-helper 8c280d7. No comparison worktree was changed.

Reference: account-manager `src-tauri/src/gateway/proxy/streaming.rs` sends tool
metadata before assembling arguments. Adapted only for recognized Claude Code
streams after content is committed; unlike unconditional early starts, this
preserves silent pre-output recovery. Rust's unconditional argument deltas and
gateway synthetic recovery were rejected for balanced mode. Local regressions
cover delayed tails, restored names, truncation, idle timeout, cancellation,
interleaved IDs, complete-then-broken tools, and no replay or premature stop.

Real Claude Code 2.1.286 received early tool metadata before a 12-second delayed
tail and completed the file correctly. However, its terminal showed the existing
preamble and spinner, not a tool row, at 4/8/12 seconds. Live mode with a partial
path behaved similarly. A truncated balanced call displayed an API error and
did not create a file. This is protocol-level progress, not a demonstrated
terminal-rendering fix. Do not claim the upstream large-generation stall is fixed.
