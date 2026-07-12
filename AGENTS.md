# Repository Guidelines

## Project Structure & Module Organization

- `main.go` starts the service and wires configuration, account pools, and handlers.
- `auth/` implements Builder ID, IAM Identity Center, hosted SSO, and token refresh flows.
- `config/` owns persisted settings, API keys, encryption, and validation.
- `pool/` contains account selection, concurrency limits, cooldowns, and runtime state.
- `proxy/` contains API handlers, translation, retries, streaming, caching, Web Search, and admin APIs.
- `internal/` provides shared HTTP, proxy, locking, and client-cache helpers.
- `web/` contains the admin UI, locales, styles, and vendored assets.
- `scripts/` contains deployment tools; `.github/` contains CI and issue templates.

Keep tests beside implementation as `*_test.go`. Respect existing package boundaries.

## Build, Test, and Development Commands

```bash
go run .                       # Run with data/config.json
go build -o kiro-go .          # Build the binary
go test ./...                  # Run all tests
go test -race ./...            # Detect races
go vet ./...                   # Run static checks
node --check web/app.js        # Check UI JavaScript
docker compose config --quiet  # Validate Compose
docker compose up -d --build   # Build and start
```

Run `gofmt -w` on every changed Go file before committing.

## Coding Style & Naming Conventions

Follow standard Go naming: exported identifiers use `PascalCase`, internal identifiers use `camelCase`, and package names are short and lowercase. Prefer explicit error wrapping, bounded reads, context-aware I/O, and existing helpers. JavaScript uses two-space indentation and existing DOM and escaping utilities. Add user-visible text to both locale JSON files.

## Testing Guidelines

Tests use Go's `testing`, `httptest`, and local hooks. Name tests `TestBehaviorCondition`. Cover success, rejection, timeout, cancellation, and failover where relevant. Never contact live services; use fake transports and placeholder credentials. Shared routing, refresh, security, or persistence changes require regression tests.

## Commit & Pull Request Guidelines

Use prefixes seen in history: `feat:`, `fix:`, `ci:`, `docs:`, and `test:`. Keep commits scoped and imperative. Pull requests should explain behavior, configuration impact, and verification; link issues and include screenshots for Web UI changes. State compatibility or security tradeoffs.

## Security & Configuration Tips

Never commit `.env`, `data/`, account exports, logs, databases, private keys, or real tokens. Use `.env.example` only for empty placeholders. Preserve `KIRO_MASTER_KEY` across deployments, and run a secret scanner before publishing changes involving authentication or examples.
