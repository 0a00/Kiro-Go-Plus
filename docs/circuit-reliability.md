# Circuit Reliability (1.2.89)

## Changes

- Shared endpoint circuits are partitioned by endpoint/host, model/workload and
  outbound proxy. An opaque scope ID distinguishes admin snapshot entries without
  exposing proxy credentials. The process-local registry is bounded to 4096
  entries; it preserves active/cooling routes and recycles idle closed or stale
  expired routes. Proxy-wide transport protection remains separate.
- Generic HTTP 500, empty responses, malformed tools and generation deadlines do
  not prove a shared service outage. They retain account/endpoint cooldowns and
  bounded failover, without opening a shared circuit. Transport failures and
  genuine 502/503/504 service errors can still trip scoped circuits.
- A final skipped/open endpoint no longer overwrites the last actual upstream
  failure or its account-retry flags. If all endpoints were merely skipped, the
  proxy still returns a bounded 503 with Retry-After, without scanning the pool.
- Half-open probes remain single-flight. Terminal recovery and empty-response
  budget paths release their admission state.
- Request logs and detailed/customer timing views expose first upstream headers
  and first response-body byte, relative to the whole request. The body metric
  covers generation streams, not HTTP error-body reads. They are application
  observations, not packet timing or proof of a particular upstream bottleneck.

## Safer Tests

The concurrency staircase stops escalation after any failed/warning level,
records later levels as skipped, and runs the configured recovery probe. It does
not bypass a circuit or restart the service to manufacture a passing result.
Short matrix probes warn above 30 seconds for semantic first output (streaming)
or total response time (non-streaming), even if protocol checks succeed.

Long Claude Code chains can pass after a tool error is genuinely recovered on
the same target. They still require complete pairing, successful termination,
the final marker, at least 20 calls and 12 files, and no unrecovered/protocol
errors. Client budget exhaustion remains a warning/failure, never full success.

## Limits and Deployment

No persisted settings or account credentials are migrated. Deploy 1.2.89 using
the normal Compose updater; existing circuit settings, stream mode and retry
budgets stay unchanged. Rollback restores the old behavior by deploying the
previous binary. Runtime circuit statistics reset on restart as before.

This release does not claim to fix upstream large-tool truncation or MiniMax's
long initial response. Large edits should remain chunked; already-visible output
is not replayed and incomplete JSON is not split or executed. No model mapping,
thinking budget or production timeout is silently changed to hide those issues.

## Reference and Verification

Sibling remote refs checked 2026-09-29: Go f8f6071/7ee2ea4, Rust e09625c (CI-only
after 5ca5703), Zhang 6c2c47b, account-manager 844aac76, gateway a5292ca,
AIClient2API f790f4c, login helper 8c280d7. Go pre-output failover and
AIClient2API's hasYieldedContent boundary support retaining bounded pre-output
retry, not replay after delivery. No equivalent shared-circuit fix was imported;
this is a scoped Plus correction. Comparison worktrees were not modified.

Regression fixtures cover 500 isolation, real-error preservation with skipped
backups, 503 suppression, model/proxy/workload scope, bounded registry cleanup,
concurrent half-open probes, first headers/body timing, staircase abort/recovery,
slow matrix warnings and recovered/unrecovered tool errors. Verify with
`bash scripts/dev-test.sh full` before deployment. Offline tests do not establish
the availability of production Kiro endpoints.
