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
text/thinking block. The message remains open and heartbeats continue. Tool start,
arguments and completion are still held until validated JSON is available. Later
text uses a new content-block index, preserving every text character and order.

The boundary callback runs only after output has already been emitted. Silent
tool-only turns remain retryable, safe-mode pending text is not flushed, and no
tool execution, synthetic status message or whole-turn replay is introduced.

No new setting or mode is needed. Saved balanced/live/safe/adaptive values are
unchanged. Deploy 1.2.90 to activate the balanced fix; rollback needs only the
previous binary. There is no credential or persisted-data migration.

## Tests

Offline regression fixtures hold an unfinished tool open, require the preamble's
block stop to arrive before release, check native/tagged thinking, exact text,
balanced indexes, later text, and pre-output retry boundaries. Existing malformed
tool, cancellation and replay tests remain required.

For real terminal verification without production credentials or quota:

```bash
uv run --with pyte==0.8.2 python scripts/client-progress-ui.py
```

Requires Go, uv and an installed Claude Code on Linux. The script starts the real
Go handler with a fake AWS EventStream server bound to loopback, uses a private
temporary workspace/config and a fake key, and records PTY screen snapshots. It
acknowledges onboarding only for its own fixture. It does not modify user settings
or bypass managed policies. Unsupported CLI onboarding/tool changes fail visibly.
The fixture and terminal have bounded lifetimes. Raw terminal artifacts stay local.

## Reference Review

Sibling refs checked 2026-09-29: Go f8f6071/7ee2ea4, Rust e09625c (stream code
5ca5703), Zhang main 6c2c47b, account-manager 844aac76, gateway a5292ca,
AIClient2API f790f4c, login-helper 8c280d7. Rust/AIClient2API and account-manager
close text at tool-start boundaries; the Go assembled-tool path closes it later.
Adapt the boundary, not early tool delivery, whole-response buffering, whitespace
trimming or synthetic recovery. Comparison worktrees were not changed.

This fixes delayed display of an existing preamble, not continuous progress for
tool-only requests. Claude Code versions and clients can render differently.
