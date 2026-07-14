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
- Failure protection: first-output timeout, actionable-output and required-tool validation, selectable safe/adaptive/balanced/live tool streams, long-tool truncation recovery, endpoint circuits, cooldowns, and bounded retries
- Token controls: bounded enabled/adaptive thinking, configurable default thinking/output/context budgets, and client-value precedence
- Streaming validation: AWS EventStream length and CRC validation, idle timeout, and truncated-response detection
- Authentication: Builder ID, IAM Identity Center, Kiro hosted SSO, Microsoft 365 / Entra ID, SSO Token, API key, and JSON import
- Prompt Cache accounting: configurable creation/read ranges, 5m/1h TTLs, sharded LRU, API-key isolation, and statistics
- Extensions: dynamic model discovery, Web Search, external token counting, and Responses history
- Operations: persisted request metadata, optional complete logs with sanitized request/output, retries and stream timing, diagnostic events, webhook alerts, `/health`, and `/ready`
- Networking: global and per-account HTTP / SOCKS5 proxies

Prompt Cache simulates and reports Anthropic cache usage. It does not cache model response bodies.

Token-budget precedence is: explicit request values, per-model registry values, global Web defaults, then automatic model detection. Supported request overrides include `max_tokens`, `max_completion_tokens`, `max_output_tokens`, `context_window`, and `max_input_tokens` where applicable. Dynamic model entries may also set `maxToolTokens` for long-tool guidance and fallback decisions.

## Web Administration

Open `/admin` to manage:

- Account import, enable/disable state, weights, priority, per-account concurrency, and proxies
- Runtime/legacy endpoint preference and automatic fallback
- Load balancing, retries, timeouts, circuits, and upstream protection
- Token/model refresh intervals, concurrency, and batch sizes
- Prompt Cache creation/read ranges, TTL, capacity, and isolation
- Web Search, token counting, Responses storage, diagnostics, complete request logging, and alerts
- Claude Agent tool enforcement, thinking/output/context token defaults, response formats, long-tool protection, and safe/adaptive/balanced/live stream modes
- API keys, quotas, admin password, listener settings, and client fingerprints

Settings apply immediately unless the panel explicitly reports that a process restart is required.

Tool stream modes trade retry coverage for latency: **Adaptive** keeps ordinary tools live but buffers high-risk `Write`/`Edit`/`Bash`-style calls so an incomplete JSON tail can be retried; **Live** forwards every tool argument delta immediately; **Balanced** buffers all tool arguments; **Safe** also defers guarded text for maximum retry coverage. Explicit `tool_choice` requests remain strictly validated in every mode.

Long-tool protection is enabled by default with one recovery retry and an 8192-token guidance limit. Optional preflight model fallback is disabled by default because model availability differs between accounts.

Complete request logging is disabled by default. When enabled, it captures inference routes only and writes bounded details to `data/request_details.json` with mode `0600` and a 64 MiB total cap. Authorization headers and credentials are excluded; image/document Base64 and tool arguments are represented only by type, byte count, and SHA-256. Prompts and model output are still sensitive, so enable this mode only while diagnosing issues and clear it afterward.

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
  -d '{"model":"claude-sonnet-4.5","max_tokens":1024,"messages":[{"role":"user","content":"Hello"}]}'
```

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -H "Authorization: Bearer ${KIRO_API_KEY}" \
  -d '{"model":"claude-sonnet-4.5","messages":[{"role":"user","content":"Hello"}]}'
```

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
| `KIRO_PROFILE_REGIONS` | Comma-separated Entra ID profile discovery regions | `us-east-1,eu-central-1` |

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
