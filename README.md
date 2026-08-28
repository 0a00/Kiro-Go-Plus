# Kiro-Go Plus

[![Test](https://github.com/0a00/Kiro-Go-Plus/actions/workflows/test.yml/badge.svg)](https://github.com/0a00/Kiro-Go-Plus/actions/workflows/test.yml)
[![Go Version](https://img.shields.io/badge/Go-1.21%2B-00ADD8?logo=go)](https://go.dev/)
[![Docker](https://img.shields.io/badge/Docker-Compose-2496ED?logo=docker)](https://docs.docker.com/compose/)
[![License](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

A production-oriented, multi-account Kiro API gateway with OpenAI, Anthropic, and Responses API compatibility. Account pools, cache behavior, refresh scheduling, proxies, monitoring, and security controls are managed from the Web admin panel.

English | [中文](README_CN.md)

> This is an unofficial community project. It is not affiliated with, authorized by, or endorsed by Amazon, AWS, or Kiro. Ensure that your use complies with applicable terms and laws.

## Purpose

Kiro-Go Plus preserves Kiro-Go's API and deployment compatibility while adding production reliability and operations features:

- API compatibility: Anthropic `/v1/messages`, OpenAI `/v1/chat/completions`, OpenAI `/v1/responses`, and `/v1/models`
- Upstream routing: Kiro Runtime as the primary path with legacy Kiro / CodeWhisperer / Amazon Q fallback
- Multi-account scheduling: weighted, priority, and balanced modes; per-account concurrency, sticky routing, and failover
- Refresh coordination: deduplication, bounded queues, timeouts, jitter, and adaptive batches for tens or hundreds of accounts
- Failure protection: model-window-aware input truncation, first-output timeout, safe same-endpoint retry before client-visible output, actionable-output and required-tool validation, selectable safe/adaptive/balanced/live tool streams, long-tool truncation recovery, endpoint circuits, durable cooldowns, and bounded retries
- Token controls: bounded enabled/adaptive thinking, native Kiro reasoning effort, configurable default thinking/output/context budgets, and client-value precedence
- Streaming validation: AWS EventStream length and CRC validation, idle timeout, and truncated-response detection
- Authentication: Builder ID, IAM Identity Center, Kiro hosted SSO, Microsoft 365 / Entra ID, SSO Token, API key, and direct multi-file native/KAM JSON import from the authenticated Web admin; OAuth authentication regions remain separate from Profile ARN data-plane regions, while `ksk_` keys discover and validate their data-plane region before persistence
- Prompt Cache accounting: official upstream usage, legacy matched-prefix efficiency, or a total-input target range compatible with New API/Sub2API; includes 5m/1h TTLs, sharded LRU, API-key isolation, persistence, and diagnostics
- Extensions: Claude Opus 5 and Sonnet 5 metadata, GPT-5.6 aliases, dynamic model capability/effort discovery, optional safe unlisted-model pass-through, text/thinking/tool self-tests, multi-round Web Search, external token counting, and Responses history
- Operations: account inventory diagnostics with latency/error EWMAs and affinity rates, persisted request metadata, account-selection and first SSE/thinking/text/tool timing, maximum event gaps, optional complete logs with sanitized request/output, retries and stream timelines, diagnostic events, webhook alerts, `/health`, and `/ready`
- Networking: global and per-account HTTP / SOCKS5 proxies

Prompt Cache accounting does not cache model response bodies or reduce Kiro requests. `official_actual` forwards only upstream cache usage, `matched_prefix` preserves the legacy estimate, and `aggregator_target` redistributes a warm hit into the configured total-input range without changing total tokens. Existing configurations migrate to `matched_prefix`; select `aggregator_target` in Web settings for New API/Sub2API accounting. Persistence stores only versioned prompt fingerprints and metadata with `0600` permissions.

Token-budget precedence is: explicit request values, per-model registry values, global Web defaults, then automatic model detection. Supported request overrides include `max_tokens`, `max_completion_tokens`, `max_output_tokens`, `context_window`, and `max_input_tokens` where applicable. Dynamic model entries may also set `maxToolTokens` for long-tool guidance and fallback decisions.

Credential import narrowly repairs exports labeled as generic `social` when the provider is `BuilderId` or `Enterprise` and refresh token, client ID, and client secret are all present. The backend validates IDC first and retries Social once only for a clear authentication mismatch; timeouts, rate limits, upstream blocks, and server errors never trigger this fallback. AWS regions and successful token responses are validated before persistence, and Web uploads normalize camelCase and snake_case exports.

## Web Administration

Open `/admin` to manage:

- Account import, routability/inventory diagnostics, enable/disable state, weights, priority, per-account concurrency, and proxies
- Runtime/legacy endpoint preference and automatic fallback
- Load balancing, retries, pre-output same-endpoint backoff, timeouts, circuits, and upstream protection
- Token/model refresh intervals, concurrency, batch sizes, due-account refresh, and failed-account retry controls
- Prompt Cache creation/read ranges, TTL, capacity, and isolation
- Web Search enablement and per-request round limit, token counting, Responses storage, diagnostics, complete request logging, and alerts
- Claude Agent tool enforcement, thinking/output/context token defaults, response formats, long-tool protection, and safe/adaptive/balanced/live stream modes
- API keys, quotas, admin password, listener settings, and client fingerprints

Settings apply immediately unless the panel explicitly reports that a process restart is required.

Tool stream modes trade retry coverage for latency: **Adaptive** keeps ordinary tools live but buffers high-risk `Write`/`Edit`/`Bash`-style arguments so an incomplete JSON tail can be retried; **Live** forwards every tool argument delta immediately; **Balanced** buffers all tool arguments; **Safe** protects tool arguments and incomplete tool calls while forwarding validated visible text immediately. Explicit `tool_choice` requests remain strictly validated in every mode.

Pre-output stream retry defaults to one same-endpoint retry after 700 ms. It applies only when an HTTP 200 stream fails before any text, thinking, or tool output reaches the client; cancellation and timeout failures are not replayed. Every retry consumes the shared upstream-attempt and duration budgets.

Long-tool protection is enabled by default with one recovery retry and an 8192-token guidance limit. Optional preflight model fallback is disabled by default because model availability differs between accounts.

Timeouts are activity-based where labeled as idle: tool-fragment assembly and high-risk actionable-output windows reset when thinking, progress, or tool fragments arrive, so they do not cap a sustained request's total duration. The stream idle timeout still applies when no upstream bytes arrive, and the request retry-duration budget remains the overall upper bound.

Complete request logging is disabled by default. When enabled, it captures inference routes only and writes bounded details to `data/request_details.json` with mode `0600` and a 64 MiB total cap. Authorization headers and credentials are excluded; image/document Base64 and tool arguments are represented only by type, byte count, and SHA-256. Prompts and model output are still sensitive, so enable this mode only while diagnosing issues and clear it afterward.

Unlisted-model pass-through is disabled by default. Enable it only when Kiro has released a model that is not yet in the local registry; IDs are syntax-checked locally, while upstream Kiro remains the final availability validator.

## Quick Start

### 1. Clone and configure

```bash
git clone https://github.com/0a00/Kiro-Go-Plus.git
cd Kiro-Go-Plus
mkdir -p data
cp .env.example .env
```

Generate a master key:

```bash
openssl rand -base64 32
```

Edit `.env` and set at least:

```dotenv
ADMIN_PASSWORD=
KIRO_MASTER_KEY=
KIRO_PORT=8080
PUID=1000
PGID=1000
```

`KIRO_MASTER_KEY` encrypts account credentials and optional Responses history. Keep it stable and back it up securely; losing it makes existing encrypted data unrecoverable.

### 2. Start

```bash
docker compose config
docker compose up -d --build
docker compose ps
```

Admin panel: `http://127.0.0.1:8080/admin`

Health checks:

```bash
curl http://127.0.0.1:8080/health
curl http://127.0.0.1:8080/ready
```

### 3. Configure API authentication

Compose binds the process to `0.0.0.0:8080` inside the container. Compatible API routes fail closed by default on a public bind. Create and enable an API key in the Web panel, then place the service behind a TLS reverse proxy.

## API Examples

Load the API key created in the admin panel into the current shell, and replace the model name with one available to your accounts:

```bash
export KIRO_API_KEY='set-locally-do-not-commit'
```

```bash
curl http://127.0.0.1:8080/v1/messages \
  -H 'Content-Type: application/json' \
  -H 'anthropic-version: 2023-06-01' \
  -H "x-api-key: ${KIRO_API_KEY}" \
  -d '{"model":"claude-sonnet-4-6","max_tokens":1024,"messages":[{"role":"user","content":"Hello"}]}'
```

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -H "Authorization: Bearer ${KIRO_API_KEY}" \
  -d '{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"Hello"}]}'
```

Model lists and responses use official Anthropic dash-separated IDs by default. Requests accept both `claude-sonnet-4-6` and Kiro's `claude-sonnet-4.6`; the presentation can be changed under **Settings > Dynamic Model Registry**.

Read-only usage, profile, and recent request logs for the current key:

```bash
curl -H "Authorization: Bearer ${KIRO_API_KEY}" http://127.0.0.1:8080/api/stats
curl -H "Authorization: Bearer ${KIRO_API_KEY}" http://127.0.0.1:8080/api/me
curl -H "Authorization: Bearer ${KIRO_API_KEY}" 'http://127.0.0.1:8080/api/logs?limit=100'
```

These endpoints only return data owned by the current key and remain readable after the key is disabled or exhausted. `/v1/stats` is a compatibility alias for `/api/stats` and no longer exposes global account-pool statistics.

## Microsoft 365 / Entra ID SSO

Kiro hosted SSO uses the fixed callback `http://localhost:3128`. Compose publishes this port on the host loopback interface only.

When the admin panel runs on a remote server, create a tunnel from your workstation first:

```bash
ssh -L 3128:127.0.0.1:3128 user@server
```

Then open the admin panel in the local browser and start sign-in. One instance can run only one hosted SSO login at a time. Kiro profiles are discovered after login; multiple profiles can be selected and switched from the Web panel.

Configure additional discovery regions with:

```dotenv
KIRO_PROFILE_REGIONS=us-east-1,eu-central-1
```

## Updating an Existing Compose Deployment

When the production directory is a clean GitHub checkout, run the self-updater from that directory:

```bash
cd /path/to/Kiro-Go-Plus
bash scripts/update-sudo.sh
```

Git runs as the current user; only Docker/Compose commands use `sudo`. The script backs up `.env`, `data/config.json`, runtime state, and the master key, then fast-forwards the checkout, rebuilds, restarts, and checks `/health`. Build or health failures restore the previous commit and rebuild the previous container. Do not run the whole command through `sudo`, because the script deliberately rejects that mode to avoid root-owned Git files.

Optional overrides:

```bash
bash scripts/update-sudo.sh --branch main --service kiro-go --health-timeout 180
```

For an extracted archive or a separate new-version directory, use the migration updater instead:

```bash
./scripts/update-docker-compose.sh --target /path/to/old/project --yes
```

The updater:

- Preserves `data/`, `data/config.json`, runtime state, and `.env*`
- Creates a rollback copy under `.update-backups/`
- Validates Compose, rebuilds, starts, and health-checks the service
- Restores the previous version automatically if build or health checks fail

Use `--keep-compose` for a customized Compose file or `--readiness-path /ready` to include account-pool readiness.

## Running from Source

```bash
go test ./...
go build -o kiro-go .
./kiro-go
```

The display name is Kiro-Go Plus. The Go module, binary, Compose service, and data format retain the `kiro-go` identifiers for compatibility with existing deployments and update scripts.

## Development Testing

Run `bash scripts/dev-test.sh quick` for deterministic offline checks or
`bash scripts/dev-test.sh full` before deployment. Opt-in live modes provide a
three-protocol model matrix, detailed stream and heartbeat timing,
tool/MCP/Responses roundtrips, cache and multimodal accounting, WebSearch,
closed-loop, fixed-rate, ramp, and realistic mixed-protocol load tests, plus
bounded soak and post-load recovery checks. See
[Local Development Testing](docs/development-testing.md).

## Per-account IPv6 Egress

Admin Settings can bind direct account traffic to addresses from a routed IPv6 prefix:

- **Random per account** assigns one address for the lifetime of the process.
- **Fixed per account** derives a repeatable address from the account ID.
- Explicit HTTP/SOCKS proxies are unaffected because the proxy controls the exit IP.

The entire CIDR must be routed to the server. Non-local addresses usually require `net.ipv6.ip_nonlocal_bind=1` on the host and, with Docker bridge networking, in the container network namespace. Keep fallback disabled when strict account-to-IP isolation is required. Use **Test IPv6** before enabling production traffic.

Use **Detect and Recommend** to inspect public interface addresses, candidate prefixes, container state, `ip_nonlocal_bind`, prefix capacity, actual IPv6 egress, and every built-in Kiro endpoint. The recommendation remains disabled unless all checks pass; a passing system recommends fixed per-account addresses with fallback disabled. Local bind and route failures are reported as server configuration errors and do not cool down, disable, or rotate accounts.

Kiro endpoints are currently commonly IPv4-only. A large IPv6 allocation alone cannot provide distinct Kiro-visible exits; direct IPv6 mode requires native dual-stack endpoints or operator-provided DNS64/NAT64. NAT64 may still collapse traffic onto one public IPv4, so verify the upstream-visible address before relying on it for account isolation.

## Data and Security

Never commit or publish:

- `.env` or `.env.*`
- `data/` or `data/config.json`
- `kiro-accounts-*.json`, account exports, or credential exports
- Private keys, databases, logs, or backup files

These paths are excluded through `.gitignore` and `.dockerignore`. Before publishing, still review:

```bash
git status --ignored
git diff --check
```

Production recommendations:

- Use a random `ADMIN_PASSWORD` and a stable `KIRO_MASTER_KEY`
- Enable API-key authentication; do not expose `ALLOW_UNAUTHENTICATED_API=true` publicly
- Put the service behind HTTPS and restrict access to `/admin`
- Use stable outbound networking per account; control whether a failed account proxy may fall back to direct access
- Back up `data/` regularly and store the master key separately

## Health Checks

- `GET /health`: returns 200 while the process is alive; use for container liveness
- `GET /ready`: returns 503 when available account count/ratio is below its configured threshold; use for load-balancer readiness

Compose uses `/health`, so account exhaustion does not cause a restart loop. Reverse proxies and load balancers should use `/ready` when deciding whether to route new requests.

## Environment Variables

| Variable | Description | Default |
|---|---|---|
| `CONFIG_PATH` | Configuration file path | `data/config.json` |
| `ADMIN_PASSWORD` | Web admin password; overrides the config file | - |
| `LOG_LEVEL` | `debug`, `info`, `warn`, or `error` | `info` |
| `KIRO_PORT` | Host port published by Compose | `8080` |
| `KIRO_LISTEN_HOST` / `KIRO_LISTEN_PORT` | Process listener; Compose fixes the container side to `0.0.0.0:8080` | config value |
| `PUID` / `PGID` | Non-root container UID/GID; match the owner of host `data/` | `1000` |
| `KIRO_MASTER_KEY` | 32-byte Base64 or hex master key | - |
| `KIRO_MASTER_KEY_FILE` | Read the master key from a secret file; overrides the environment value | - |
| `ALLOW_INSECURE_PUBLIC_BIND` | Allow the default admin password on a public bind; emergency use only | `false` |
| `ALLOW_UNAUTHENTICATED_API` | Explicitly allow anonymous compatible API calls on a public bind | `false` |
| `KIRO_SSO_CALLBACK_BIND` | Hosted SSO callback listener | loopback only |
| `KIRO_PROFILE_REGIONS` | Comma-separated Entra Profile and `ksk_` API-key discovery regions | `us-east-1,eu-central-1` |

## Upstream and Credits

Kiro-Go Plus is based on [Quorinex/Kiro-Go](https://github.com/Quorinex/Kiro-Go) and adapts implementation ideas from:

- [zsecducna/Kiro-Go](https://github.com/zsecducna/Kiro-Go)
- [zsecducna/kiro-login-helper](https://github.com/zsecducna/kiro-login-helper)
- [Zhang161215/kiro.rs](https://github.com/Zhang161215/kiro.rs)

Thanks to the original authors and contributors. The upstream license and copyright notice remain in [LICENSE](LICENSE).

## Disclaimer

This project is intended for learning, research, and authorized integration. Do not use it to bypass access controls, quotas, billing, service restrictions, or other security mechanisms. Operators are responsible for account safety, data protection, compliance, and service availability.

## License

[MIT](LICENSE)
