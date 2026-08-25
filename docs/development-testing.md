# Local Development Testing

The test tooling separates deterministic offline checks from opt-in live checks.
Offline modes never contact Kiro. Live modes use configured accounts and consume
quota, so use a dedicated API key and start with small limits.

## Offline Quality Gates

```bash
bash scripts/dev-test.sh quick   # module verification, shuffled tests, vet, builds, UI/shell/Compose checks
bash scripts/dev-test.sh fault   # delay, empty/truncated/corrupt stream, 429/5xx, retry, and cancellation fixtures
bash scripts/dev-test.sh full    # quick + race + repeated regressions + updater + pinned govulncheck
bash scripts/dev-test.sh bench   # parser, translation, tool schema, cache, routing, and logging benchmarks
KIRO_DEV_FUZZ_TIME=30s bash scripts/dev-test.sh fuzz
```

Fuzz targets cover AWS EventStream, downstream SSE, Claude tool schemas, Claude
request translation, and Responses input parsing. Inputs are bounded to prevent
the test harness from creating unbounded allocations.

## Live Functional Checks

Start Kiro-Go Plus, then provide credentials only through the environment:

```bash
read -rsp 'Test API key: ' KIRO_DEV_API_KEY; printf '\n'; export KIRO_DEV_API_KEY
bash scripts/dev-test.sh live
bash scripts/dev-test.sh live-full --json-report /tmp/kiro-devcheck.json
go run ./cmd/devcheck --list-scenarios
```

`smoke` checks health, models, authentication, Anthropic JSON, and SSE. `full`
adds thinking on all three protocols, Skill instruction transport,
Anthropic/Chat/Responses tool-result roundtrips, Responses custom tools,
MCP-shaped zero-argument roundtrips, prompt-cache cold/create/read behavior,
same-dimension image accounting, output-limit semantics, a bounded long stream,
cancellation recovery, and optional WebSearch. Select expensive cases with
`--scenarios id,id`; enable search with `--web-search`. Cache and reasoning
checks warn instead of failing when the selected upstream accounting mode or
model intentionally hides those fields.

Each stream result separates response-header time, first valid SSE event, first
semantic output (TTFT), first text, first thinking, first tool output, maximum
protocol-event gap, maximum wire-activity gap, SSE heartbeat count, and total
duration. Results also expose the downstream request ID, stop reason, and
available input/output/reasoning/cache token fields. JSON reports use mode
`0600`, include the service version and a credential-free test-settings
fingerprint, and never include the API key, request body, image, or tool arguments.

To validate actual client-side Skill discovery and MCP process execution, not
just proxy protocol transport, run the isolated Claude Code harness:

```bash
bash scripts/dev-test.sh client-e2e
```

It builds `cmd/mcpfixture`, creates a disposable Skill and workspace, disables
built-in tools, permits only the fixture tool, applies a small client budget,
and deletes all temporary files. Set `KIRO_DEV_ALLOW_REMOTE=1` explicitly for a
non-loopback `KIRO_DEV_BASE_URL`.

## Model, Load, and Soak Checks

```bash
bash scripts/dev-test.sh matrix --models claude-sonnet-5,claude-sonnet-5-thinking
bash scripts/dev-test.sh matrix --all-models
bash scripts/dev-test.sh load --concurrency 20 --requests 100 --timeout 5m
bash scripts/dev-test.sh staircase --concurrency-levels 1,5,10,20,50,100 --requests 10
bash scripts/dev-test.sh soak --concurrency 10 --soak-duration 10m \
  --soak-max-requests 500 --soak-token-budget 16000
```

The matrix runs Anthropic Messages, Chat Completions, and Responses in stream
and non-stream modes for every selected model. Load, staircase, and soak probes
rotate across all three protocols and both transfer modes. Staircase sends at
least one request per worker at each level. Soak scheduling stops at the first
duration, request, or requested-output-token cap and lets in-flight requests
finish. Reports include distinct request-ID counts, p50/p95/p99, per-protocol
successes, and categorized failures such as `anthropic_http_429`,
`openai_timeout`, `responses_empty_response`, and `responses_stream_protocol`.
Responses or individual SSE lines beyond the diagnostic memory budget are
reported as `*_response_too_large`, rather than being misclassified as a
generic transport failure. SSE parsing accepts both LF and CRLF framing.

The default load profile is a low-cost exact-marker probe. It validates the
complete response against a request-unique `LOAD_OK_n` marker, so a duplicated
or cross-talk response is a failure rather than a false success. The optional
`realistic` profile requires at least 128 requested output tokens and cycles
through protocol modes, thinking, long output, function tools, MCP-shaped
tools, image input, prompt cache, and Skill-style system context. Add
`--web-search` to include a bounded native WebSearch case; this consumes
external search quota.

Arrival and concurrency controls are explicit:

```bash
# Closed-loop workers, with an unmeasured warmup.
bash scripts/dev-test.sh load --warmup-requests 5 --concurrency 20 --requests 100

# Open-loop fixed arrivals; client queue overflow is reported as client_overload.
bash scripts/dev-test.sh load --load-pattern fixed --target-rps 10 \
  --concurrency 20 --requests 200

# Ramp from 10% to the target rate over the selected duration.
bash scripts/dev-test.sh load --load-pattern ramp --target-rps 20 \
  --ramp-duration 2m --concurrency 50 --requests 600

# Realistic workload mix. Use a small request count first.
bash scripts/dev-test.sh load --load-profile realistic --load-max-tokens 256 \
  --concurrency 10 --requests 56
```

Concurrency above 100 requires `--allow-high-load` and is capped at 1000.
`--load-max-tokens`, request count, warmup count, and WebSearch are quota
controls; the harness does not silently raise them. `--post-load-recovery`
performs one health check and one deterministic request after measured load and
is enabled by default.

Load `p50/p95/p99` fields describe individual request latency. `wallMillis`
describes the complete scheduler plus drain interval; `achievedRps` and
`arrivalRps` is the scheduled arrival rate, while `achievedRps` excludes
dropped arrivals and `successRps` counts successful completions; all use that
wall interval. Separate success/failure latencies, response-header p95, TTFT
percentiles, queue delay, stream/wire gap, client goroutine delta, and heap
delta make buffering and client saturation visible. Fixed/ramp modes use a
bounded queue, so dropped arrivals are counted as `client_overload` instead of
being hidden.

After a load run, the harness best-effort correlates up to 500 in-memory
customer request-log entries by request ID. Only normalized endpoint, account
attempt count, selection latency, affinity, cache status, and tool count are
used; account IDs, emails, tool names/arguments, request bodies, and API keys
are never included in the report.

## Continuous Integration

Pull requests run shuffled tests, race detection, Go 386 compatibility,
`govulncheck`, a Docker health smoke test, and upload a 14-day coverage artifact.
A weekly workflow runs every bounded fuzz target for 20 seconds. CI never runs
live Kiro checks because they consume quota and depend on external availability.

## Security and Boundaries

The runner rejects non-loopback targets unless `--allow-remote` is explicit and
never follows redirects with credentials. Verify the remote URL before opting
in.

Kiro-Go Plus transports Skills instructions and MCP/function protocol data; it
does not discover Skill files or launch local MCP servers. `devcheck` validates
schema, call ID, arguments, lifecycle, and second-turn tool results. The
optional `client-e2e` harness validates the separate Claude Code responsibilities.
