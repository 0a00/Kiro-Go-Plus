# Upstream Review: 2026-09-29

## Reviewed Revisions

Fetched remote refs without checking out or altering sibling worktrees. Local
changes in the original Go fork, account manager and login helper were retained.

| Project | Reviewed remote revision | Result |
| --- | --- | --- |
| Quorinex/Kiro-Go | `f8f6071` | No new remote commits; stream integrity and search already adapted |
| zsecducna/Kiro-Go | `7ee2ea4` | No new remote commits; import watcher, region recovery and effort support already adapted |
| hank9999/kiro.rs | `5ca5703`, v2 `932a89f` | Models and field-scoped refresh protections already covered |
| Zhang161215/kiro.rs | `6c2c47b` | Proxy pools, Responses and credential imports already covered |
| hj01857655/kiro-account-manager | `844aac76` | New gateway fixes adapted below |
| jwadow/kiro-gateway | `a5292ca` | No new remote commits |
| zsecducna/kiro-login-helper | `8c280d7` | No new remote commits; helper worktree left intact |
| justlovemaki/AIClient2API | `f790f4c` | Reviewed authentication and upstream-error changes below |

## Adapted Changes

- Account manager `4f0a069a`: read `x-amzn-kiro-ratelimit-retry-after` as
  milliseconds for HTTP 429. Bound the vendor hint to five minutes and preserve
  a longer standard `Retry-After`. Pass the duration directly to existing
  account/endpoint cooldowns instead of embedding and reparsing error text.
  Generation, account probes, search and control-plane errors share the parser.
- Account manager `3c9f28fc`: native forced search with mixed tools takes the
  existing server-search path. MCP accepts JSON or SSE and prefers
  `structuredContent.results`, retaining legacy text results. SSE parsing is
  bounded, matches response IDs and returns without waiting for connection EOF.
  Search queries are limited to 200 Unicode characters; dates accept epoch
  milliseconds and ISO formats. Existing structured search responses remain.
- Account manager `4c3a8d95`: prevent unsupported Anthropic server-side tools
  from becoming client-executable tools. Return an explicit local 400 for known
  unsupported server tools and disabled native search. Do not strip all typed
  tools: client-executed Bash, Computer, editors and MCP tools remain available.
- Account manager `1dfb362a`: normal generation and admin probes send the real
  payload profile ARN in `x-amzn-kiro-profile-arn`; do not invent a default ARN.

## Already Covered or Deferred

AIClient2API `b12e25e` broadens OIDC profile discovery. Plus already attempts
discovery for profileless OAuth credentials, suppresses only confirmed Builder
ID unsupported responses, preserves returned ARNs and supports profileless
fallback. Its `0324439` response-failure refactor adds no missing Kiro behavior
to the existing bounded retry and committed-output protections.

Do not adopt search-failure-to-model-answer fallback, blanket typed-tool removal,
hardcoded profile ARNs, desktop settings editors, Bedrock or unrelated providers.
Account manager `0d28c8c7` changes management usage requests to include profile
ARNs. Plus intentionally omits that query parameter for its existing endpoints
and has regression coverage for it; defer until independently verified on the
relevant endpoint/account types. No new static model inventory is inferred from
desktop changelogs; discovery and operator-managed models remain authoritative.

## Verification and Deployment

Regression coverage: vendor/standard delay precedence, malformed/overflow values,
real HTTP 429 endpoint cooldown, generation profile-header consistency, bounded
MCP responses, notifications and multiline SSE, persistent-connection completion,
response-ID mismatch, structured/text fallback, optional dates, Unicode query
limits, disabled search and client/server tool boundaries. Full offline gate:
`bash scripts/dev-test.sh full` (includes race, vet, builds, script/Compose checks,
repeated stream regressions and vulnerability scanning).

No configuration migration or new setting is required. Deploy with
`bash scripts/update-sudo.sh --health-timeout 240`; verify `/health` reports
`1.2.86`. Rollback uses the updater's previous commit backup or the previous
image while retaining `.env`, `data/` and `KIRO_MASTER_KEY`. Production quotas,
settings and accounts are not changed by this synchronization. Offline fixtures
verify protocol handling; live acceptance by each upstream region still needs
post-deployment verification.
