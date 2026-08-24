#!/usr/bin/env bash
set -Eeuo pipefail

usage() {
  cat <<'EOF'
Usage:
  bash scripts/dev-test.sh [quick|full|bench|live|live-full|load|all] [devcheck options]

Modes:
  quick       Offline unit tests, vet, build, JavaScript, shell, and Compose checks.
  full        Quick checks plus race detection, repeated stream regressions, updater tests,
              and govulncheck when it is installed.
  bench       Run local event-stream parser benchmarks.
  live        Run the smoke suite against a local service.
  live-full   Run reasoning, Skills context, functions, MCP, Responses, and cancellation.
  load        Run bounded mixed stream/non-stream requests (default: 5 workers, 10 requests).
  all         Run full offline checks followed by the live-full suite.

Live modes require KIRO_DEV_API_KEY in the environment. Optional variables:
  KIRO_DEV_BASE_URL, KIRO_DEV_MODEL, KIRO_DEV_THINKING_MODEL

Examples:
  bash scripts/dev-test.sh quick
  bash scripts/dev-test.sh live-full --web-search
  bash scripts/dev-test.sh load --concurrency 20 --requests 100
EOF
}

info() {
  printf '[dev-test] %s\n' "$*"
}

die() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$PROJECT_DIR"

MODE="${1:-quick}"
if [[ $# -gt 0 ]]; then
  shift
fi

check_tracked_secrets() {
  local path
  while IFS= read -r path; do
    case "$path" in
      data/*|.env|.env.*|kiro-accounts-*.json|*account-export*.json|*credentials-export*.json|*.pem|*.key|*.p12|*.pfx|*.db|*.sqlite|*.sqlite3)
        [[ "$path" == ".env.example" ]] && continue
        die "tracked sensitive path detected: $path"
        ;;
    esac
  done < <(git ls-files)
}

check_compose() {
  if docker compose version >/dev/null 2>&1; then
    info "validating Docker Compose"
    docker compose config --quiet
  elif command -v docker-compose >/dev/null 2>&1; then
    info "validating Docker Compose"
    docker-compose config --quiet
  else
    info "Docker Compose unavailable; skipping Compose validation"
  fi
}

run_quick() {
  command -v go >/dev/null 2>&1 || die "go is required"
  info "running Go tests"
  go test ./...
  info "running go vet"
  go vet ./...

  local tmp_dir
  tmp_dir="$(mktemp -d)"
  trap 'rm -rf "$tmp_dir"' RETURN
  info "building service and devcheck"
  go build -o "$tmp_dir/kiro-go" .
  go build -o "$tmp_dir/kiro-devcheck" ./cmd/devcheck
  rm -rf "$tmp_dir"
  trap - RETURN

  if command -v node >/dev/null 2>&1; then
    info "checking browser JavaScript"
    node --check web/app.js
    node --check web/credential-import.js
    node scripts/credential-import.test.js
  else
    info "Node.js unavailable; skipping JavaScript checks"
  fi

  info "checking shell syntax and tracked sensitive paths"
  bash -n scripts/*.sh
  check_tracked_secrets
  check_compose
}

run_full() {
  run_quick
  info "running race detector"
  go test -race ./...
  info "repeating stream and tool regressions"
  go test ./proxy -count=20 -run 'Test(ParseEventStream|CallKiroAPIRecoversSchemaDeclaredZeroArgumentTool|ClaudeMessagesRecoversZeroArgumentTool|MeaningfulStream|ToolAssembly)'
  info "testing the sudo updater with fake Docker"
  bash scripts/update-sudo_test.sh
  if command -v govulncheck >/dev/null 2>&1; then
    info "running govulncheck"
    govulncheck ./...
  else
    info "govulncheck unavailable; install golang.org/x/vuln/cmd/govulncheck to enable dependency scanning"
  fi
}

run_live() {
  local suite="$1"
  shift
  [[ -n "${KIRO_DEV_API_KEY:-}" ]] || die "KIRO_DEV_API_KEY is required for live checks"
  info "running local API ${suite} suite against ${KIRO_DEV_BASE_URL:-http://127.0.0.1:8080}"
  go run ./cmd/devcheck --suite "$suite" "$@"
}

case "$MODE" in
  quick)
    run_quick
    ;;
  full)
    run_full
    ;;
  bench)
    info "running event-stream benchmarks"
    go test ./proxy -run '^$' -bench 'BenchmarkParseEventStream' -benchmem "$@"
    ;;
  live)
    run_live smoke "$@"
    ;;
  live-full)
    run_live full "$@"
    ;;
  load)
    run_live load "$@"
    ;;
  all)
    run_full
    run_live full "$@"
    ;;
  -h|--help|help)
    usage
    ;;
  *)
    usage
    die "unknown mode: $MODE"
    ;;
esac
