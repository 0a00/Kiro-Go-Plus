#!/usr/bin/env bash
set -Eeuo pipefail

usage() {
  cat <<'EOF'
Usage:
  bash scripts/dev-test.sh [quick|full|fault|fuzz|bench|live|live-full|client-e2e|matrix|load|staircase|soak|all] [options]

Modes:
  quick       Offline unit tests, vet, build, JavaScript, shell, and Compose checks.
  full        Quick checks plus race detection, repeated stream regressions, updater tests,
              and pinned govulncheck dependency scanning.
  fault       Run deterministic stream, retry, timeout, and fault-injection regressions.
  fuzz        Run bounded EventStream, SSE, schema, and request-parser fuzzing.
  bench       Run parser, translation, cache, routing, and logging benchmarks.
  live        Run the smoke suite against a local service.
  live-full   Run reasoning, Skills context, functions, MCP, Responses, and cancellation.
  client-e2e  Use Claude Code to discover a disposable Skill and execute a local MCP fixture.
  matrix      Test selected models across Anthropic, Chat Completions, and Responses.
  load        Run bounded mixed stream/non-stream requests (default: 5 workers, 10 requests).
  staircase   Run concurrency levels 1,5,10,20,50,100 (explicitly quota-consuming).
  soak        Run a duration/request/token-budget bounded stability check.
  all         Run full offline checks followed by the live-full suite.

Live modes require KIRO_DEV_API_KEY in the environment. Optional variables:
  KIRO_DEV_BASE_URL, KIRO_DEV_MODEL, KIRO_DEV_THINKING_MODEL, KIRO_DEV_MODELS,
  KIRO_DEV_FUZZ_TIME. client-e2e also accepts KIRO_DEV_ALLOW_REMOTE,
  KIRO_DEV_CLIENT_TIMEOUT, and KIRO_DEV_MAX_BUDGET_USD.

Examples:
  bash scripts/dev-test.sh quick
  bash scripts/dev-test.sh live-full --web-search
  bash scripts/dev-test.sh client-e2e
  bash scripts/dev-test.sh matrix --models claude-sonnet-5,claude-sonnet-5-thinking
  bash scripts/dev-test.sh load --concurrency 20 --requests 100
  bash scripts/dev-test.sh staircase --requests 10
  bash scripts/dev-test.sh soak --soak-duration 10m --soak-max-requests 500 --soak-token-budget 16000
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
  info "verifying Go module checksums"
  go mod verify
  info "running Go tests"
  go test -shuffle=on -timeout=10m ./...
  info "running go vet"
  go vet ./...

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
  check_tracked_secrets
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
  info "running govulncheck v1.1.4"
  go run golang.org/x/vuln/cmd/govulncheck@v1.1.4 ./...
}

run_fault() {
  info "running deterministic downstream protocol fault fixtures"
  go test ./cmd/devcheck -count=1 -run 'Test(FaultFixtures|RunnerSeparatesResponseHeaders|AnthropicAndMCPToolResultRoundTrips|BoundedSoak|BuildLoadResult)'
  info "running upstream EventStream and failover fault fixtures"
  go test ./proxy -count=1 -run 'Test(ParseEventStreamRejects|ParseEventStreamMarks|AccountFailover|AccountAttemptController|MeaningfulStream|ToolAssembly)'
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
