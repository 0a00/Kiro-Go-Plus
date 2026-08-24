# Local Development Testing

The test tooling has two layers. Offline checks are deterministic and never
contact Kiro or consume account quota. Live checks are opt-in and exercise a
running local Kiro-Go Plus instance with real configured accounts.

## Offline Quality Gates

Run the normal development gate:

```bash
bash scripts/dev-test.sh quick
```

Run the release-level gate before deployment:

```bash
bash scripts/dev-test.sh full
bash scripts/dev-test.sh bench
```

`full` adds the race detector, repeated stream/tool regressions, updater tests,
and `govulncheck` when installed. The script also validates JavaScript, shell
syntax, Compose configuration, builds both binaries, and rejects tracked
credential-like files.

## Live Scenario Checks

Start the service first, then provide a dedicated, low-quota test API key
without placing it on the command line:

```bash
read -rsp 'Test API key: ' KIRO_DEV_API_KEY
printf '\n'
export KIRO_DEV_API_KEY
bash scripts/dev-test.sh live
bash scripts/dev-test.sh live-full --json-report /tmp/kiro-devcheck.json
```

Use `KIRO_DEV_BASE_URL` when the local service is not on
`http://127.0.0.1:8080`. Use `--model` and `--thinking-model` to override
automatic model selection. The runner refuses non-loopback hosts by default;
add `--allow-remote` only after verifying the destination. HTTP redirects are
never followed with credentials.

The smoke suite checks health, model discovery, authentication rejection,
non-stream output, SSE termination, first semantic output, TTFT, and total
latency. The full suite additionally checks:

- Thinking deltas; the first thinking delta counts as TTFT.
- System instruction transport used by client-side Skills.
- Anthropic and OpenAI forced function calls with complete JSON arguments.
- MCP-shaped zero-argument tool calls, exact `{}` recovery, and complete block
  lifecycle validation.
- Responses API output, cancellation after the first valid SSE event, and
  post-cancellation health.
- Native WebSearch when explicitly enabled with `--web-search`.

Run bounded concurrency separately:

```bash
bash scripts/dev-test.sh load --concurrency 20 --requests 100 --timeout 5m
```

The load suite alternates streaming and non-streaming requests. Each request
gets its own timeout while the suite retains a bounded overall deadline. The
report includes per-mode success counts and p50/p95 total latency. Start small:
live checks consume quota and may trigger upstream rate limits.

## Interpretation and Boundaries

`PASS` validates the observable proxy contract. `WARN` usually means a valid
response was buffered into one burst, thinking was not exposed, or a
probabilistic instruction marker was not followed. `FAIL` means a protocol,
auth, malformed SSE/JSON, terminal-event, timeout, or tool-integrity assertion
failed.

Kiro-Go Plus transports tool definitions and calls; it does not launch client
MCP servers or discover Skill files. Therefore the suite validates MCP/function
protocol integrity and Skill instruction preservation. Test actual MCP server
execution and Skill discovery separately in Claude Code or the relevant client.
