#!/usr/bin/env bash
set -Eeuo pipefail

usage() {
  cat <<'EOF'
Usage:
  bash scripts/dev-test.sh [quick|full|fault|pressure|docker-restart|fuzz|bench|live|live-full|client-e2e|production|matrix|load|staircase|soak|all] [options]

Modes:
  quick       Offline unit tests, vet, build, JavaScript, shell, and Compose checks.
  full        Quick checks plus race detection, repeated stream regressions, updater tests,
              and pinned govulncheck dependency scanning.
  fault       Run deterministic stream, retry, timeout, and fault-injection regressions.
  pressure    Repeat load, failover, endpoint-circuit, admission, cache, and persistence regressions.
  docker-restart Build an isolated image, restart a disposable container, and recheck health.
  fuzz        Run bounded EventStream, SSE, schema, and request-parser fuzzing.
  bench       Run parser, translation, cache, routing, and logging benchmarks.
  live        Run the smoke suite against a local service.
  live-full   Run reasoning, Skills, tools/MCP, cache, images, long streams, and cancellation.
  client-e2e  Use Claude Code to discover a disposable Skill and execute a local MCP fixture.
  production  Run the protected production functional, model, load, and Claude Code suite.
  matrix      Test selected models across Anthropic, Chat Completions, and Responses.
  load        Run bounded mixed-protocol stream/non-stream requests (default: 5 workers, 10 requests).
  staircase   Run concurrency levels 1,5,10,20,50,100 (explicitly quota-consuming).
  soak        Run a duration/request/token-budget bounded stability check.
  all         Run full offline checks followed by the live-full suite.

Live modes require KIRO_DEV_API_KEY in the environment. Optional variables:
  KIRO_DEV_BASE_URL, KIRO_DEV_MODEL, KIRO_DEV_THINKING_MODEL, KIRO_DEV_MODELS,
  KIRO_DEV_FUZZ_TIME. client-e2e also accepts KIRO_DEV_ALLOW_REMOTE,
  KIRO_DEV_CLIENT_TIMEOUT, KIRO_DEV_CLIENT_CANCEL_AFTER,
  KIRO_DEV_CLIENT_CONCURRENCY, and KIRO_DEV_MAX_BUDGET_USD.

Production mode requires KIRO_PROD_API_KEY and --confirm-production. Use
KIRO_PROD_BASE_URL, KIRO_PROD_ALLOW_REMOTE, KIRO_PROD_MODEL,
KIRO_PROD_THINKING_MODEL, KIRO_PROD_CLIENT_SCENARIOS, and KIRO_PROD_REPORT_DIR
to configure the target and report location. It is intentionally quota-consuming.

Load controls:
  --load-profile marker|realistic, --load-max-tokens N, --warmup-requests N,
  --load-pattern closed|fixed|ramp, --target-rps N, --ramp-duration D,
  --allow-high-load (required above 100 workers), --post-load-recovery=false,
  --staircase-hold D, --staircase-cooldown D, --server-stats=false,
  --require-server-stats,
  --max-p95-ms N, --max-ttft-p95-ms N, --max-stream-gap-p95-ms N,
  --max-client-goroutine-growth N, --max-client-heap-growth-mb N,
  --baseline FILE, --baseline-tolerance-percent N.

Examples:
  bash scripts/dev-test.sh quick
  bash scripts/dev-test.sh live-full --web-search
  bash scripts/dev-test.sh client-e2e
  KIRO_PROD_API_KEY=... bash scripts/dev-test.sh production --confirm-production
  bash scripts/dev-test.sh matrix --models claude-sonnet-5,claude-sonnet-5-thinking
  bash scripts/dev-test.sh load --concurrency 20 --requests 100
  bash scripts/dev-test.sh load --load-profile realistic --load-max-tokens 256 \
    --warmup-requests 5 --concurrency 20 --requests 140
  bash scripts/dev-test.sh load --load-pattern fixed --target-rps 10 \
    --concurrency 20 --requests 200
  bash scripts/dev-test.sh load --load-pattern ramp --target-rps 20 \
    --ramp-duration 2m --concurrency 50 --requests 600
  bash scripts/dev-test.sh load --allow-high-load --concurrency 250 --requests 500
  bash scripts/dev-test.sh staircase --requests 10
  bash scripts/dev-test.sh soak --soak-duration 10m --soak-max-requests 500 --soak-token-budget 16000
  bash scripts/dev-test.sh staircase --concurrency-levels 1,5,10,20 --staircase-hold 30s --staircase-cooldown 10s
  bash scripts/dev-test.sh load --json-report /tmp/load.json --max-p95-ms 30000
  bash scripts/dev-test.sh load --baseline /tmp/load.json --baseline-tolerance-percent 10
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

check_sensitive_paths() {
  local path
  while IFS= read -r path; do
    case "$path" in
      data/*|.env|.env.*|kiro-accounts-*.json|*account-export*.json|*credentials-export*.json|*.pem|*.key|*.p12|*.pfx|*.db|*.sqlite|*.sqlite3|*.log|*.bak|*.tar|*.tar.gz|*.zip|*.7z|.update-backups/*)
        [[ "$path" == ".env.example" ]] && continue
        die "sensitive path is tracked or staged for publication: $path"
        ;;
    esac
  done < <(git ls-files --cached --others --exclude-standard)
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

DOCKER_RESTART_NAME=""
DOCKER_RESTART_IMAGE=""
DOCKER_RESTART_DATA_DIR=""

cleanup_docker_restart() {
  if [[ -n "${DOCKER_RESTART_NAME:-}" ]]; then
    docker rm -f "$DOCKER_RESTART_NAME" >/dev/null 2>&1 || true
  fi
  if [[ -n "${DOCKER_RESTART_IMAGE:-}" ]]; then
    docker image rm "$DOCKER_RESTART_IMAGE" >/dev/null 2>&1 || true
  fi
  if [[ -n "${DOCKER_RESTART_DATA_DIR:-}" ]]; then
    rm -rf -- "$DOCKER_RESTART_DATA_DIR"
  fi
  DOCKER_RESTART_NAME=""
  DOCKER_RESTART_IMAGE=""
  DOCKER_RESTART_DATA_DIR=""
  return 0
}

run_quick() {
  command -v go >/dev/null 2>&1 || die "go is required"
  info "verifying Go module checksums"
  go mod verify
  info "running Go tests"
  go test -shuffle=on -timeout=10m ./...
  info "running go vet"
  go vet ./...

  info "checking Go formatting"
  local unformatted
  unformatted="$(gofmt -l .)"
  if [[ -n "$unformatted" ]]; then
    printf '%s\n' "$unformatted" >&2
    die "Go files are not formatted; run gofmt -w on the listed files"
  fi

  local tmp_dir
  tmp_dir="$(mktemp -d)"
  trap 'rm -rf "$tmp_dir"' RETURN
  info "building service, devcheck, and MCP fixture"
  go build -o "$tmp_dir/kiro-go" .
  go build -o "$tmp_dir/kiro-devcheck" ./cmd/devcheck
  go build -o "$tmp_dir/kiro-mcpfixture" ./cmd/mcpfixture
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
  info "checking production test safety gates"
  bash scripts/production-test_test.sh
  check_sensitive_paths
  check_compose
}

run_full() {
  run_quick
  info "running race detector"
  go test -race -shuffle=on -timeout=20m ./...
  info "repeating stream and tool regressions"
  go test ./proxy -count=20 -run 'Test(ParseEventStream|CallKiroAPIRecoversSchemaDeclaredZeroArgumentTool|ClaudeMessagesRecoversZeroArgumentTool|MeaningfulStream|ToolAssembly)'
  info "testing the sudo updater with fake Docker"
  bash scripts/update-sudo_test.sh
  info "running govulncheck v1.7.0"
  go run golang.org/x/vuln/cmd/govulncheck@v1.7.0 ./...
}

run_fault() {
  info "running deterministic downstream protocol and load fault fixtures"
  go test ./cmd/devcheck -count=1
  info "running upstream EventStream and failover fault fixtures"
  go test ./proxy -count=1 -run 'Test(ParseEventStreamRejects|ParseEventStreamMarks|AccountFailover|AccountAttemptController|MeaningfulStream|ToolAssembly)'
}

run_pressure() {
  info "repeating deterministic devcheck pressure and fault regressions"
  go test ./cmd/devcheck -shuffle=on -count=3 -timeout=15m
  info "repeating account failover, endpoint circuit, admission, cache, and persistence regressions"
  go test ./proxy -shuffle=on -count=3 -timeout=20m -run 'Test(APIKeyAdmission|AccountAttemptController|AccountEndpointRoute|UpstreamHealth|CallKiroAPIFallsBack|CallKiroAPIRetries|CallKiroAPIStops|PromptCache|ResponsesStore|RequestLogPersistence|TokenRefreshCoordinator)'
  go test ./pool -shuffle=on -count=3 -timeout=15m -run 'Test(AcquireForModel|CircuitBreaker|FailedHalfOpen|RuntimeStatePersists)'
}

run_docker_restart() {
  command -v docker >/dev/null 2>&1 || die "docker is required for docker-restart"
  command -v openssl >/dev/null 2>&1 || die "openssl is required for docker-restart"
  local name="kiro-go-plus-restart-${BASHPID}"
  local image="kiro-go-plus-restart:${BASHPID}"
  local data_dir
  local master_key
  DOCKER_RESTART_NAME="$name"
  DOCKER_RESTART_IMAGE="$image"
  data_dir="$(mktemp -d)"
  DOCKER_RESTART_DATA_DIR="$data_dir"
  trap cleanup_docker_restart EXIT
  master_key="$(openssl rand -base64 32)"
  chmod 0777 "$data_dir"

  info "building isolated restart image"
  docker build --tag "$image" .
  info "starting disposable container"
  docker run --detach --name "$name" \
    --env ADMIN_PASSWORD=devcheck-admin-password \
    --env KIRO_MASTER_KEY="$master_key" \
    --env KIRO_LISTEN_HOST=0.0.0.0 \
    --env KIRO_LISTEN_PORT=8080 \
    --volume "$data_dir:/app/data" \
    "$image" >/dev/null

  wait_for_health() {
    local attempt
    for attempt in $(seq 1 45); do
      if docker exec "$name" wget -q -O - http://127.0.0.1:8080/health | grep -q '"status":"ok"'; then
        return 0
      fi
      sleep 1
    done
    docker logs "$name" >&2 || true
    return 1
  }

  wait_for_health || die "container did not become healthy before restart"
  info "restarting disposable container"
  docker restart "$name" >/dev/null
  wait_for_health || die "container did not recover after restart"
  info "container restart health check passed"
  cleanup_docker_restart
  trap - EXIT
}

run_fuzz() {
  local fuzz_time="${KIRO_DEV_FUZZ_TIME:-10s}"
  info "fuzzing downstream SSE for ${fuzz_time}"
  go test ./cmd/devcheck -run '^$' -fuzz '^FuzzConsumeSSE$' -fuzztime "$fuzz_time"
  local target
  for target in FuzzParseEventStream FuzzClaudeToolSchemaConversion FuzzClaudeRequestTranslation FuzzOpenAIRequestTranslation FuzzResponsesInputParsing; do
    info "fuzzing ${target} for ${fuzz_time}"
    go test ./proxy -run '^$' -fuzz "^${target}$" -fuzztime "$fuzz_time"
  done
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
  fault)
    run_fault
    ;;
  pressure)
    run_pressure
    ;;
  docker-restart)
    run_docker_restart
    ;;
  fuzz)
    run_fuzz
    ;;
  bench)
    info "running parser, translation, cache, routing, and logging benchmarks"
    go test ./proxy ./pool -run '^$' -bench 'Benchmark(ParseEventStream|ClaudeRequestTranslation|ToolSchemaConversion|PromptCacheHit|RequestLogConcurrentAdd|AccountSelectionBalanced100)' -benchmem "$@"
    ;;
  live)
    run_live smoke "$@"
    ;;
  live-full)
    run_live full "$@"
    ;;
  client-e2e)
    bash scripts/client-e2e.sh "$@"
    ;;
  production)
    bash scripts/production-test.sh "$@"
    ;;
  matrix)
    run_live matrix "$@"
    ;;
  load)
    run_live load "$@"
    ;;
  staircase)
    run_live staircase "$@"
    ;;
  soak)
    run_live soak "$@"
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
