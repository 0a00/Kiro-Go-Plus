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
adds thinking, Skill instruction transport, Anthropic/Chat tool-result
roundtrips, MCP-shaped zero-argument roundtrips, Chat and Responses streaming,
cancellation recovery, and optional WebSearch. Select expensive cases with
`--scenarios id,id`; enable search with `--web-search`.

Each stream result separates response-header time, first valid SSE event, first
semantic output (TTFT), first text, first thinking, first tool output, maximum
event gap, and total duration. JSON reports use mode `0600` and never include the
API key.

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
and non-stream modes for every selected model. Staircase sends at least one
request per worker at each level. Soak scheduling stops at the first duration,
request, or requested-output-token cap and lets in-flight requests finish.
Reports include p50/p95/p99 and failure categories such as `http_429`,
`http_5xx`, `timeout`, `transport`, `empty_response`, and `stream_protocol`.

## Security and Boundaries

The runner rejects non-loopback targets unless `--allow-remote` is explicit and
never follows redirects with credentials. Verify the remote URL before opting
in.

Kiro-Go Plus transports Skills instructions and MCP/function protocol data; it
does not discover Skill files or launch local MCP servers. `devcheck` validates
schema, call ID, arguments, lifecycle, and second-turn tool results. The
optional `client-e2e` harness validates the separate Claude Code responsibilities.
