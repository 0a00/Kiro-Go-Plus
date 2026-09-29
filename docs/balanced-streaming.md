# Balanced Streaming

Version 1.2.87 gives `toolStreamMode=balanced` its intended hybrid behavior:
text and visible thinking stream immediately; all tool calls, including MCP
tools, wait for complete JSON before client delivery. Structured history and
tool-name compatibility remain controlled by `claudeCodeTransparentMode`.

## Retry Boundary

- Before text, thinking or a complete tool is committed, normal bounded
  account/endpoint failover and truncation recovery can run.
- After output is committed, a failure ends with a protocol error. The proxy
  must not replay the turn, duplicate text, invent tool results or execute
  incomplete JSON. The client decides whether to retry.
- Heartbeats alone do not commit the turn. Tool assembly idle limits and the
  request retry budget still apply.
- Explicit tool-choice requests must return a complete tool, even if they
  have already streamed explanatory text.

## Configuration and Rollback

Upgrade the running binary first, then select **Balanced (recommended)** under
Thinking settings. Existing saved `safe`, `adaptive` and `live` settings remain
unchanged. This is not a switch that can be backported through configuration
alone to 1.2.86. Returning to `safe` restores the previous high-risk buffering.
No account, credential or data migration is required.

## Reference Review

Reviewed sibling remote refs on 2026-09-29. Quorinex/Kiro-Go `f8f6071` and
zsecducna/Kiro-Go `7ee2ea4` forward `OnText` separately from assembled
`OnToolUse`; their handlers/parser are the reference for this policy. Keep
Plus's existing bounded recovery, structured history and strict JSON checks.
Do not import the Rust `/cc` whole-response buffering or the gateways' synthetic
tool-result recovery. The Rust, account-manager, gateway, login-helper and
AIClient2API refs contain no additional policy required for this change.

## Verification

HTTP fixture tests hold an unfinished MCP tool open and assert that text and
thinking have already arrived, while no partial tool frame has. They also
verify whitespace preservation, truncation/malformed JSON after text without
replay, complete tool submission, explicit tool-choice rejection, and unchanged
safe/live modes. Use the local full quality gate and Claude Code file, resumed
edit, MCP and long-tool scenarios before deployment. Production health alone
cannot verify this behavior; compare actual SSE text/tool timing after upgrading.
