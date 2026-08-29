#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

usage() {
  cat <<'EOF'
Usage:
  bash scripts/production-test.sh [options]

This command sends real requests to a deployed Kiro-Go Plus service. It never
accepts an API key on the command line; set KIRO_PROD_API_KEY instead.

Required for a real run:
  --confirm-production       Acknowledge quota and production impact.
  --allow-remote             Also required for a non-loopback URL.

Options:
  --base-url URL             Target (default: KIRO_PROD_BASE_URL or 127.0.0.1:8080).
  --model MODEL              Primary Claude model (default: discover from /v1/models).
  --thinking-model MODEL     Thinking model (default: primary model plus -thinking).
  --matrix-models CSV        Explicit models for the matrix (default: all discovered Claude models).
  --web-search               Include WebSearch checks (default).
  --skip-web-search          Skip external search quota.
  --skip-matrix              Skip the all-Claude-model matrix.
  --skip-load                Skip the bounded realistic load.
  --skip-client-e2e           Skip Claude Code scenarios.
  --enable-stress             Enable staircase and soak phases.
  --staircase                 Enable the bounded concurrency staircase.
  --soak                      Enable the bounded duration soak.
  --phase-timeout DURATION    Outer timeout per phase (default: 90m).
  --request-timeout DURATION  Per-scenario timeout (default: 5m).
  --load-concurrency N        Realistic load workers (default: 5).
  --load-requests N           Realistic load requests (default: 14).
  --load-max-tokens N         Realistic load output cap (default: 256).
  --client-scenarios CSV      Claude Code cases (default: all).
  --client-concurrency N      Concurrent Claude Code clients (default: 2).
  --client-max-budget-usd N   Claude Code budget per client (default: 0.10).
  --client-cancel-after DURATION  Cancellation probe deadline (default: 8s).
  --report-dir DIR            Private report directory (default: /tmp/kiro-production-test-*).
  --keep-artifacts             Keep raw Claude Code diagnostic streams (sensitive).
  --fail-on-warning            Return non-zero when a phase reports a warning.
  --dry-run                   Validate and print the plan only.
  -h, --help                  Show this help.

Environment:
  KIRO_PROD_API_KEY, KIRO_PROD_BASE_URL, KIRO_PROD_MODEL,
  KIRO_PROD_THINKING_MODEL, KIRO_PROD_MODELS, KIRO_PROD_WEB_SEARCH,
  KIRO_PROD_REPORT_DIR, KIRO_PROD_PHASE_TIMEOUT, KIRO_PROD_REQUEST_TIMEOUT,
  KIRO_PROD_LOAD_CONCURRENCY, KIRO_PROD_LOAD_REQUESTS, KIRO_PROD_LOAD_MAX_TOKENS,
  KIRO_PROD_CLIENT_SCENARIOS, KIRO_PROD_CLIENT_CONCURRENCY,
  KIRO_PROD_CLIENT_MAX_BUDGET_USD, KIRO_PROD_CLIENT_CANCEL_AFTER,
  KIRO_PROD_FAIL_ON_WARNING.
EOF
}

die() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

is_positive_integer() {
  [[ "$1" =~ ^[1-9][0-9]*$ ]]
}

is_duration() {
  [[ "$1" =~ ^[0-9]+([.][0-9]+)?(ns|us|ms|s|m|h)$ ]]
}

is_nonzero_duration() {
  is_duration "$1" || return 1
  [[ ! "$1" =~ ^0([.]0+)?(ns|us|ms|s|m|h)$ ]]
}

is_binary_flag() {
  [[ "$1" == 0 || "$1" == 1 ]]
}

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
BASE_URL="${KIRO_PROD_BASE_URL:-http://127.0.0.1:8080}"
REPORT_DIR="${KIRO_PROD_REPORT_DIR:-}"
MODEL="${KIRO_PROD_MODEL:-}"
THINKING_MODEL="${KIRO_PROD_THINKING_MODEL:-}"
MATRIX_MODELS="${KIRO_PROD_MODELS:-}"
PHASE_TIMEOUT="${KIRO_PROD_PHASE_TIMEOUT:-90m}"
REQUEST_TIMEOUT="${KIRO_PROD_REQUEST_TIMEOUT:-5m}"
WEB_SEARCH="${KIRO_PROD_WEB_SEARCH:-1}"
ALLOW_REMOTE="${KIRO_PROD_ALLOW_REMOTE:-0}"
CONFIRM_PRODUCTION="${KIRO_PROD_CONFIRM:-0}"
DRY_RUN=0
RUN_MATRIX=1
RUN_LOAD=1
RUN_CLIENT=1
RUN_STAIRCASE=0
RUN_SOAK=0
KEEP_ARTIFACTS=0
FAIL_ON_WARNING="${KIRO_PROD_FAIL_ON_WARNING:-0}"
LOAD_CONCURRENCY="${KIRO_PROD_LOAD_CONCURRENCY:-5}"
LOAD_REQUESTS="${KIRO_PROD_LOAD_REQUESTS:-14}"
LOAD_MAX_TOKENS="${KIRO_PROD_LOAD_MAX_TOKENS:-256}"
CLIENT_SCENARIOS="${KIRO_PROD_CLIENT_SCENARIOS:-all}"
CLIENT_CONCURRENCY="${KIRO_PROD_CLIENT_CONCURRENCY:-2}"
CLIENT_MAX_BUDGET="${KIRO_PROD_CLIENT_MAX_BUDGET_USD:-${KIRO_DEV_MAX_BUDGET_USD:-0.10}}"
CLIENT_CANCEL_AFTER="${KIRO_PROD_CLIENT_CANCEL_AFTER:-${KIRO_DEV_CLIENT_CANCEL_AFTER:-8s}}"

while (($# > 0)); do
  case "$1" in
    --base-url)
      (($# >= 2)) || die "--base-url requires a value"
      BASE_URL="$2"
      shift 2
      ;;
    --base-url=*) BASE_URL="${1#*=}"; shift ;;
    --model)
      (($# >= 2)) || die "--model requires a value"
      MODEL="$2"
      shift 2
      ;;
    --model=*) MODEL="${1#*=}"; shift ;;
    --thinking-model)
      (($# >= 2)) || die "--thinking-model requires a value"
      THINKING_MODEL="$2"
      shift 2
      ;;
    --thinking-model=*) THINKING_MODEL="${1#*=}"; shift ;;
    --matrix-models)
      (($# >= 2)) || die "--matrix-models requires a value"
      MATRIX_MODELS="$2"
      shift 2
      ;;
    --matrix-models=*) MATRIX_MODELS="${1#*=}"; shift ;;
    --confirm-production) CONFIRM_PRODUCTION=1; shift ;;
    --allow-remote) ALLOW_REMOTE=1; shift ;;
    --web-search) WEB_SEARCH=1; shift ;;
    --skip-web-search) WEB_SEARCH=0; shift ;;
    --skip-matrix) RUN_MATRIX=0; shift ;;
    --skip-load) RUN_LOAD=0; shift ;;
    --skip-client-e2e) RUN_CLIENT=0; shift ;;
    --enable-stress) RUN_STAIRCASE=1; RUN_SOAK=1; shift ;;
    --staircase) RUN_STAIRCASE=1; shift ;;
    --soak) RUN_SOAK=1; shift ;;
    --phase-timeout)
      (($# >= 2)) || die "--phase-timeout requires a value"
      PHASE_TIMEOUT="$2"
      shift 2
      ;;
    --phase-timeout=*) PHASE_TIMEOUT="${1#*=}"; shift ;;
    --request-timeout)
      (($# >= 2)) || die "--request-timeout requires a value"
      REQUEST_TIMEOUT="$2"
      shift 2
      ;;
    --request-timeout=*) REQUEST_TIMEOUT="${1#*=}"; shift ;;
    --load-concurrency)
      (($# >= 2)) || die "--load-concurrency requires a value"
      LOAD_CONCURRENCY="$2"
      shift 2
      ;;
    --load-concurrency=*) LOAD_CONCURRENCY="${1#*=}"; shift ;;
    --load-requests)
      (($# >= 2)) || die "--load-requests requires a value"
      LOAD_REQUESTS="$2"
      shift 2
      ;;
    --load-requests=*) LOAD_REQUESTS="${1#*=}"; shift ;;
    --load-max-tokens)
      (($# >= 2)) || die "--load-max-tokens requires a value"
      LOAD_MAX_TOKENS="$2"
      shift 2
      ;;
    --load-max-tokens=*) LOAD_MAX_TOKENS="${1#*=}"; shift ;;
    --client-scenarios)
      (($# >= 2)) || die "--client-scenarios requires a value"
      CLIENT_SCENARIOS="$2"
      shift 2
      ;;
    --client-scenarios=*) CLIENT_SCENARIOS="${1#*=}"; shift ;;
    --client-concurrency)
      (($# >= 2)) || die "--client-concurrency requires a value"
      CLIENT_CONCURRENCY="$2"
      shift 2
      ;;
    --client-concurrency=*) CLIENT_CONCURRENCY="${1#*=}"; shift ;;
    --client-max-budget-usd)
      (($# >= 2)) || die "--client-max-budget-usd requires a value"
      CLIENT_MAX_BUDGET="$2"
      shift 2
      ;;
    --client-max-budget-usd=*) CLIENT_MAX_BUDGET="${1#*=}"; shift ;;
    --client-cancel-after)
      (($# >= 2)) || die "--client-cancel-after requires a value"
      CLIENT_CANCEL_AFTER="$2"
      shift 2
      ;;
    --client-cancel-after=*) CLIENT_CANCEL_AFTER="${1#*=}"; shift ;;
    --report-dir)
      (($# >= 2)) || die "--report-dir requires a value"
      REPORT_DIR="$2"
      shift 2
      ;;
    --report-dir=*) REPORT_DIR="${1#*=}"; shift ;;
    --keep-artifacts) KEEP_ARTIFACTS=1; shift ;;
    --fail-on-warning) FAIL_ON_WARNING=1; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown option: $1" ;;
  esac
done

[[ -n "$BASE_URL" ]] || die "base URL must not be empty"
is_binary_flag "$WEB_SEARCH" || die "KIRO_PROD_WEB_SEARCH must be 0 or 1"
is_binary_flag "$ALLOW_REMOTE" || die "KIRO_PROD_ALLOW_REMOTE must be 0 or 1"
is_binary_flag "$CONFIRM_PRODUCTION" || die "KIRO_PROD_CONFIRM must be 0 or 1"
is_binary_flag "$FAIL_ON_WARNING" || die "KIRO_PROD_FAIL_ON_WARNING must be 0 or 1"
case "$BASE_URL" in
  *'?'*|*'#'*|*'@'*) die "base URL must not contain query, fragment, or userinfo" ;;
  http://127.0.0.1:*|https://127.0.0.1:*|http://localhost:*|https://localhost:*|http://\[::1\]:*|https://\[::1\]:*) ;;
  http://*|https://*) ((ALLOW_REMOTE == 1)) || die "non-loopback URL requires --allow-remote" ;;
  *) die "base URL must use http:// or https://" ;;
esac
BASE_URL="${BASE_URL%/}"
is_nonzero_duration "$PHASE_TIMEOUT" || die "invalid --phase-timeout: $PHASE_TIMEOUT"
is_nonzero_duration "$REQUEST_TIMEOUT" || die "invalid --request-timeout: $REQUEST_TIMEOUT"
is_nonzero_duration "$CLIENT_CANCEL_AFTER" || die "invalid --client-cancel-after: $CLIENT_CANCEL_AFTER"
[[ -z "$MODEL" || "$MODEL" != *[[:space:]]* ]] || die "model must not contain whitespace"
[[ -z "$THINKING_MODEL" || "$THINKING_MODEL" != *[[:space:]]* ]] || die "thinking model must not contain whitespace"
[[ -z "$MATRIX_MODELS" || ("$MATRIX_MODELS" != *$'\n'* && "$MATRIX_MODELS" != *$'\r'*) ]] || die "matrix models must not contain newlines"
[[ "$CLIENT_MAX_BUDGET" =~ ^[0-9]+([.][0-9]+)?$ ]] || die "--client-max-budget-usd must be a non-negative decimal"
is_positive_integer "$LOAD_CONCURRENCY" || die "--load-concurrency must be a positive integer"
is_positive_integer "$LOAD_REQUESTS" || die "--load-requests must be a positive integer"
is_positive_integer "$LOAD_MAX_TOKENS" || die "--load-max-tokens must be a positive integer"
is_positive_integer "$CLIENT_CONCURRENCY" || die "--client-concurrency must be a positive integer"
((LOAD_CONCURRENCY <= 1000)) || die "--load-concurrency must not exceed 1000"
((LOAD_REQUESTS <= 10000)) || die "--load-requests must not exceed 10000"
((LOAD_MAX_TOKENS >= 128 && LOAD_MAX_TOKENS <= 128000)) || die "--load-max-tokens must be between 128 and 128000"
((CLIENT_CONCURRENCY <= 20)) || die "--client-concurrency must not exceed 20"

if ((DRY_RUN)); then
  printf 'Production test dry run\n'
  printf 'target: %s\n' "$BASE_URL"
  printf 'model: %s\n' "${MODEL:-<auto-discover>}"
  printf 'thinking_model: %s\n' "${THINKING_MODEL:-<derived>}"
  printf 'matrix_models: %s\n' "${MATRIX_MODELS:-<all discovered Claude models>}"
  printf 'web_search: %s\n' "$([[ $WEB_SEARCH == 1 ]] && printf enabled || printf skipped)"
  printf 'matrix: %s\n' "$([[ $RUN_MATRIX == 1 ]] && printf enabled || printf skipped)"
  printf 'realistic_load: %s workers=%s requests=%s max_tokens=%s\n' \
    "$([[ $RUN_LOAD == 1 ]] && printf enabled || printf skipped)" "$LOAD_CONCURRENCY" "$LOAD_REQUESTS" "$LOAD_MAX_TOKENS"
  printf 'claude_code: %s scenarios=%s concurrency=%s\n' \
    "$([[ $RUN_CLIENT == 1 ]] && printf enabled || printf skipped)" "$CLIENT_SCENARIOS" "$CLIENT_CONCURRENCY"
  printf 'staircase: %s; soak: %s\n' \
    "$([[ $RUN_STAIRCASE == 1 ]] && printf enabled || printf disabled)" \
    "$([[ $RUN_SOAK == 1 ]] && printf enabled || printf disabled)"
  printf 'fail_on_warning: %s\n' "$([[ $FAIL_ON_WARNING == 1 ]] && printf enabled || printf disabled)"
  printf 'No network request or credential access was performed.\n'
  exit 0
fi

((CONFIRM_PRODUCTION == 1)) || die "real run requires --confirm-production"
[[ -n "${KIRO_PROD_API_KEY:-}" ]] || die "KIRO_PROD_API_KEY is required"
if printf '%s' "$KIRO_PROD_API_KEY" | LC_ALL=C grep -q '[[:cntrl:]]'; then
  die "KIRO_PROD_API_KEY must not contain control characters"
fi
[[ "$KIRO_PROD_API_KEY" != *[[:space:]]* ]] || die "KIRO_PROD_API_KEY must not contain whitespace"
command -v go >/dev/null 2>&1 || die "go is required"
command -v curl >/dev/null 2>&1 || die "curl is required"
command -v timeout >/dev/null 2>&1 || die "timeout is required"
if ((RUN_CLIENT)); then
  command -v claude >/dev/null 2>&1 || die "Claude Code is required; use --skip-client-e2e to skip"
fi

if [[ -z "$REPORT_DIR" ]]; then
  REPORT_DIR="$(mktemp -d "${TMPDIR:-/tmp}/kiro-production-test.XXXXXX")"
else
  mkdir -p "$REPORT_DIR"
fi
chmod 700 "$REPORT_DIR"
BUILD_DIR="$(mktemp -d "${TMPDIR:-/tmp}/kiro-production-build.XXXXXX")"
PROBE_DIR=""
HEALTH_BODY=""
cleanup() {
  if [[ -n "${BUILD_DIR:-}" ]]; then
    rm -rf -- "$BUILD_DIR"
  fi
  if [[ -n "${PROBE_DIR:-}" ]]; then
    rm -rf -- "$PROBE_DIR"
  fi
  if [[ -n "${HEALTH_BODY:-}" ]]; then
    rm -f -- "$HEALTH_BODY"
  fi
}
trap cleanup EXIT
SUMMARY_PATH="$REPORT_DIR/production-summary.tsv"
printf 'phase\tstatus\tlog\treport\n' >"$SUMMARY_PATH"
chmod 600 "$SUMMARY_PATH"
DISCOVERED_MODEL_PATH="$REPORT_DIR/discovered-model.txt"
: >"$DISCOVERED_MODEL_PATH"
chmod 600 "$DISCOVERED_MODEL_PATH"

export KIRO_DEV_API_KEY="${KIRO_PROD_API_KEY}"
export KIRO_DEV_BASE_URL="$BASE_URL"
export KIRO_DEV_ALLOW_REMOTE="$ALLOW_REMOTE"
# Do not let inherited development model variables collide with the matrix
# suite, which intentionally passes --models or --all-models.
unset KIRO_DEV_MODEL KIRO_DEV_THINKING_MODEL KIRO_DEV_MODELS

DEV_WARNING_ARGS=()
if ((FAIL_ON_WARNING)); then
  DEV_WARNING_ARGS+=(--fail-on-warning)
fi

DEV_MODEL_ARGS=()
if [[ -n "$MODEL" ]]; then
  DEV_MODEL_ARGS+=(--model "$MODEL")
fi
if [[ -n "$THINKING_MODEL" ]]; then
  DEV_MODEL_ARGS+=(--thinking-model "$THINKING_MODEL")
fi

DEV_CHECK="$BUILD_DIR/devcheck"
if ! (cd "$PROJECT_DIR" && go build -o "$DEV_CHECK" ./cmd/devcheck) >"$REPORT_DIR/build.log" 2>&1; then
  printf 'build\tFAIL\t%s\t\n' "$REPORT_DIR/build.log" >>"$SUMMARY_PATH"
  die "failed to build production test client"
fi
chmod 700 "$DEV_CHECK"

declare -a PHASE_STATUSES=()
LAST_PHASE_STATUS=FAIL
PREFLIGHT_OK=0

record_phase() {
  local name="$1" status="$2" log="$3" report="$4"
  PHASE_STATUSES+=("$status")
  printf '%s\t%s\t%s\t%s\n' "$name" "$status" "$log" "$report" >>"$SUMMARY_PATH"
  printf '%-24s %s\n' "$name" "$status"
  LAST_PHASE_STATUS="$status"
}

record_skip() {
  local name="$1" detail="$2"
  local log="$REPORT_DIR/$name.log"
  printf '%s\n' "$detail" >"$log"
  chmod 600 "$log"
  record_phase "$name" SKIP "$log" ""
}

phase_has_warning() {
  local log="$1"
  grep -Eiq '(^|[[:space:]])(PRODUCTION_WARNING:|WARN([[:space:]]|$)|warn=[1-9][0-9]*)' "$log"
}

run_phase() {
  local name="$1" report="$2"
  shift 2
  local log="$REPORT_DIR/$name.log"
  set +e
  timeout --foreground --kill-after=30s "$PHASE_TIMEOUT" "$@" >"$log" 2>&1
  local status="$?"
  set -e
  chmod 600 "$log"
  if ((status == 0)); then
    if phase_has_warning "$log"; then
      record_phase "$name" WARN "$log" "$report"
    else
      record_phase "$name" PASS "$log" "$report"
    fi
  else
    record_phase "$name" FAIL "$log" "$report"
  fi
}

run_probe_phase() {
  local name="$1"
  shift
  local log="$REPORT_DIR/$name.log"
  set +e
  # Probe functions use bounded curl connect/response deadlines internally.
  # They must be invoked by this shell; GNU timeout cannot execute a function
  # name as an external command.
  "$@" >"$log" 2>&1
  local status="$?"
  set -e
  chmod 600 "$log"
  if ((status == 0)); then
    if phase_has_warning "$log"; then
      record_phase "$name" WARN "$log" ""
    else
      record_phase "$name" PASS "$log" ""
    fi
  else
    record_phase "$name" FAIL "$log" ""
  fi
}

json_quote() {
  local escaped="$1"
  escaped=${escaped//\\/\\\\}
  escaped=${escaped//\"/\\\"}
  escaped=${escaped//$'\n'/\\n}
  escaped=${escaped//$'\r'/\\r}
  escaped=${escaped//$'\t'/\\t}
  escaped=${escaped//$'\b'/\\b}
  escaped=${escaped//$'\f'/\\f}
  printf '"%s"' "$escaped"
}

extract_first_claude_model() {
  local file="$1"
  local model=""
  if command -v jq >/dev/null 2>&1; then
    model="$(jq -r '
      ([.data[]?.id? | select(type == "string") | select(startswith("claude-")) | select(endswith("-thinking") | not)][0]
       // [.data[]?.id? | select(type == "string") | select(startswith("claude-"))][0]
       // "")' "$file" 2>/dev/null || true)"
  fi
  if [[ -z "$model" ]]; then
    model="$(grep -oE '"id"[[:space:]]*:[[:space:]]*"claude-[^"]+"' "$file" 2>/dev/null \
      | sed -E 's/.*"id"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/' \
      | grep -v -- '-thinking$' | head -n 1 || true)"
  fi
  [[ "$model" =~ ^[A-Za-z0-9._:-]+$ ]] || return 1
  printf '%s\n' "$model"
}

curl_with_header() {
  local header_name="$1"
  local header_value="$2"
  shift 2
  local escaped="$header_value"
  escaped=${escaped//\\/\\\\}
  escaped=${escaped//\"/\\\"}
  printf 'header = "%s: %s"\n' "$header_name" "$escaped" | curl --config - --max-redirs 0 "$@"
}

curl_with_auth() {
  curl_with_header "Authorization" "Bearer $KIRO_PROD_API_KEY" "$@"
}

curl_with_api_key_header() {
  curl_with_header "X-Api-Key" "$KIRO_PROD_API_KEY" "$@"
}

health_probe() (
  local body status
  body="$(mktemp "$REPORT_DIR/.health-body.XXXXXX")"
  trap 'rm -f -- "$body"' EXIT
  if ! status="$(curl --silent --show-error --connect-timeout 5 --max-time 20 \
    --output "$body" --write-out '%{http_code}' "$BASE_URL/health")"; then
    printf 'health transport failed\n'
    return 1
  fi
  if [[ "$status" != 200 ]] || ! grep -Eq '"status"[[:space:]]*:[[:space:]]*"ok"' "$body"; then
    printf 'health endpoint returned HTTP %s or a non-ok payload\n' "$status"
    return 1
  fi
  printf 'health endpoint passed (HTTP 200)\n'
)

api_probe() (
  local probe_dir
  probe_dir="$(mktemp -d "$REPORT_DIR/.api-probe.XXXXXX")"
  chmod 700 "$probe_dir"
  PROBE_DIR="$probe_dir"
  trap 'rm -rf -- "$probe_dir"' EXIT

  api_get() {
    local name="$1" path="$2" body="$probe_dir/$1.json" status
    if ! status="$(curl_with_auth --silent --show-error --connect-timeout 5 --max-time 20 \
      --output "$body" --write-out '%{http_code}' "$BASE_URL$path")"; then
      return 1
    fi
    [[ "$status" == 200 ]] || {
      printf '%s returned HTTP %s\n' "$path" "$status"
      return 1
    }
    printf '%s HTTP 200\n' "$path"
  }

  api_post() {
    local name="$1" path="$2" payload="$3" body="$probe_dir/$1.json" status
    if ! status="$(curl_with_auth --silent --show-error --connect-timeout 5 --max-time 30 \
      --request POST --header 'Content-Type: application/json' --data-binary "@$payload" \
      --output "$body" --write-out '%{http_code}' "$BASE_URL$path")"; then
      return 1
    fi
    [[ "$status" == 200 ]] || {
      printf '%s returned HTTP %s\n' "$path" "$status"
      return 1
    }
    printf '%s HTTP 200\n' "$path"
  }

  local failed=0 body ready_body ready_status event_status root_status admin_status options_status
  local auth_header_status api_key_header_status
  local admin_body="$probe_dir/admin.html" options_headers="$probe_dir/options.headers"
  api_get me /api/me || failed=1
  body="$probe_dir/me.json"
  if ((failed == 0)) && ! grep -Eq '"status"[[:space:]]*:' "$body"; then failed=1; fi
  api_get stats /api/stats || failed=1
  body="$probe_dir/stats.json"
  if ((failed == 0)) && ! grep -Eq '"status"[[:space:]]*:[[:space:]]*"ok"' "$body"; then failed=1; fi
  api_get legacy-stats /v1/stats || failed=1
  api_get logs '/api/logs?limit=5' || failed=1
  body="$probe_dir/logs.json"
  if ((failed == 0)) && ! grep -Eq '"logs"[[:space:]]*:' "$body"; then failed=1; fi

  if ! root_status="$(curl --silent --show-error --connect-timeout 5 --max-time 20 \
    --output "$probe_dir/root.json" --write-out '%{http_code}' "$BASE_URL/")"; then
    root_status=""
    failed=1
  fi
  if [[ "$root_status" != 200 ]] || ! grep -Eq '"status"[[:space:]]*:[[:space:]]*"ok"' "$probe_dir/root.json"; then
    printf 'root health alias returned HTTP %s or a non-ok payload\n' "$root_status"
    failed=1
  else
    printf 'root health alias HTTP 200\n'
  fi

  if ! admin_status="$(curl --silent --show-error --connect-timeout 5 --max-time 20 \
    --output "$admin_body" --write-out '%{http_code}' "$BASE_URL/admin")"; then
    admin_status=""
    failed=1
  fi
  if [[ "$admin_status" != 200 ]] || ! grep -Eiq '<html|<!doctype' "$admin_body"; then
    printf 'admin page returned HTTP %s or did not contain HTML\n' "$admin_status"
    failed=1
  else
    printf 'admin page HTTP 200\n'
  fi

  if ! options_status="$(curl --silent --show-error --connect-timeout 5 --max-time 20 \
    --request OPTIONS --header 'Origin: https://production-test.invalid' \
    --dump-header "$options_headers" --output /dev/null --write-out '%{http_code}' \
    "$BASE_URL/v1/messages")"; then
    options_status=""
    failed=1
  fi
  if [[ "$options_status" != 204 ]] || ! grep -Eiq '^Access-Control-Allow-Origin:' "$options_headers"; then
    printf 'CORS preflight returned HTTP %s or lacked allow-origin\n' "$options_status"
    failed=1
  else
    printf 'CORS preflight HTTP 204\n'
  fi

  if ! auth_header_status="$(curl_with_auth --silent --show-error --connect-timeout 5 --max-time 20 \
    --output "$probe_dir/models-authorization.json" --write-out '%{http_code}' "$BASE_URL/v1/models")"; then
    auth_header_status=""
    failed=1
  fi
  if [[ "$auth_header_status" != 200 ]] || ! grep -Eq '"data"[[:space:]]*:' "$probe_dir/models-authorization.json"; then
    printf 'Authorization model probe returned HTTP %s or an invalid model list\n' "$auth_header_status"
    failed=1
  else
    printf 'Authorization model probe HTTP 200\n'
  fi

  local probe_model="${MODEL:-}"
  if [[ -z "$probe_model" ]]; then
    probe_model="$(extract_first_claude_model "$probe_dir/models-authorization.json" || true)"
  fi
  if [[ -n "$probe_model" ]]; then
    printf '%s\n' "$probe_model" >"$DISCOVERED_MODEL_PATH"
    chmod 600 "$DISCOVERED_MODEL_PATH"
    printf 'selected Claude model: %s\n' "$probe_model"
  else
    printf 'PRODUCTION_WARNING: no Claude model was found in the authenticated model list\n'
  fi

  local payload="$probe_dir/count-tokens-request.json"
  if [[ -z "$probe_model" ]]; then
    probe_model="claude-sonnet-5"
  fi
  printf '{"model":%s,"messages":[{"role":"user","content":"production count tokens probe"}]}\n' \
    "$(json_quote "$probe_model")" >"$payload"
  chmod 600 "$payload"
  api_post count-tokens /v1/messages/count_tokens "$payload" || failed=1
  body="$probe_dir/count-tokens.json"
  if ((failed == 0)) && ! grep -Eq '"input_tokens"[[:space:]]*:[[:space:]]*[1-9][0-9]*' "$body"; then failed=1; fi

  if ! api_key_header_status="$(curl_with_api_key_header --silent --show-error --connect-timeout 5 --max-time 20 \
    --output "$probe_dir/models-x-api-key.json" --write-out '%{http_code}' "$BASE_URL/v1/models")"; then
    api_key_header_status=""
    failed=1
  fi
  if [[ "$api_key_header_status" != 200 ]] || ! grep -Eq '"data"[[:space:]]*:' "$probe_dir/models-x-api-key.json"; then
    printf 'X-Api-Key model probe returned HTTP %s or an invalid model list\n' "$api_key_header_status"
    failed=1
  else
    printf 'X-Api-Key model probe HTTP 200\n'
  fi

  ready_body="$probe_dir/ready.json"
  if ! ready_status="$(curl --silent --show-error --connect-timeout 5 --max-time 20 \
    --output "$ready_body" --write-out '%{http_code}' "$BASE_URL/ready")"; then
    ready_status=""
    failed=1
  fi
  case "$ready_status" in
    200) printf '/ready HTTP 200\n' ;;
    503) printf 'PRODUCTION_WARNING: /ready is HTTP 503; account readiness threshold is not met\n' ;;
    *) printf '/ready returned HTTP %s\n' "$ready_status"; failed=1 ;;
  esac
  if ! event_status="$(curl --silent --show-error --connect-timeout 5 --max-time 20 \
    --request POST --header 'Content-Type: application/json' --data '{}' \
    --output "$probe_dir/event-logging.json" --write-out '%{http_code}' \
    "$BASE_URL/api/event_logging/batch")"; then
    event_status=""
    failed=1
  fi
  [[ "$event_status" =~ ^2[0-9][0-9] ]] || {
    printf 'event logging endpoint returned HTTP %s\n' "$event_status"
    failed=1
  }
  ((failed == 0))
)

printf 'Production test target: %s\n' "$BASE_URL"
printf 'Reports: %s\n' "$REPORT_DIR"
run_probe_phase health health_probe
if [[ "$LAST_PHASE_STATUS" != PASS ]]; then
  record_skip api-observability "health preflight failed"
  record_skip api-smoke "health preflight failed"
  record_skip functional-full "health preflight failed"
  record_skip model-matrix "health preflight failed"
  record_skip realistic-load "health preflight failed"
  record_skip claude-code-e2e "health preflight failed"
else
  run_probe_phase api-observability api_probe
  # A smoke failure is recorded, but it must not hide independent diagnostics.
  # Authentication failures are consequently visible in every affected phase,
  # while model- or protocol-specific failures still leave useful results from
  # the other surfaces.
  SMOKE_ARGS=(--suite smoke --timeout "$REQUEST_TIMEOUT" \
    --json-report "$REPORT_DIR/api-smoke.json" --allow-remote)
  SMOKE_ARGS+=("${DEV_MODEL_ARGS[@]}")
  SMOKE_ARGS+=("${DEV_WARNING_ARGS[@]}")
  run_phase api-smoke "$REPORT_DIR/api-smoke.json" "$DEV_CHECK" "${SMOKE_ARGS[@]}"
  PREFLIGHT_OK=1

  FULL_ARGS=(--suite full --timeout "$REQUEST_TIMEOUT" --cancellation=true \
    --json-report "$REPORT_DIR/functional-full.json" --allow-remote)
  FULL_ARGS+=("${DEV_MODEL_ARGS[@]}")
  if ((WEB_SEARCH)); then
    FULL_ARGS+=(--web-search)
  fi
  FULL_ARGS+=("${DEV_WARNING_ARGS[@]}")
  run_phase functional-full "$REPORT_DIR/functional-full.json" "$DEV_CHECK" "${FULL_ARGS[@]}"

  if ((RUN_MATRIX)); then
    MATRIX_ARGS=(--suite matrix --timeout "$REQUEST_TIMEOUT" \
      --json-report "$REPORT_DIR/model-matrix.json" --allow-remote)
    # Intentionally do not append DEV_MODEL_ARGS here. devcheck rejects a
    # single --model together with --models/--all-models.
    if [[ -n "$MATRIX_MODELS" ]]; then
      MATRIX_ARGS+=(--models "$MATRIX_MODELS")
    else
      MATRIX_ARGS+=(--all-models)
    fi
    MATRIX_ARGS+=("${DEV_WARNING_ARGS[@]}")
    run_phase model-matrix "$REPORT_DIR/model-matrix.json" "$DEV_CHECK" "${MATRIX_ARGS[@]}"
  else
    record_skip model-matrix "disabled by --skip-matrix"
  fi

  if ((RUN_LOAD)); then
    LOAD_ARGS=(--suite load --timeout "$REQUEST_TIMEOUT" --load-profile realistic --load-pattern closed \
      --concurrency "$LOAD_CONCURRENCY" --requests "$LOAD_REQUESTS" --load-max-tokens "$LOAD_MAX_TOKENS" \
      --warmup-requests 2 --post-load-recovery=true --server-stats=true \
      --json-report "$REPORT_DIR/realistic-load.json" --allow-remote)
    LOAD_ARGS+=("${DEV_MODEL_ARGS[@]}")
    LOAD_ARGS+=("${DEV_WARNING_ARGS[@]}")
    run_phase realistic-load "$REPORT_DIR/realistic-load.json" "$DEV_CHECK" "${LOAD_ARGS[@]}"
  else
    record_skip realistic-load "disabled by --skip-load"
  fi

  if ((RUN_CLIENT)); then
    CLIENT_MODEL="$MODEL"
    if [[ -z "$CLIENT_MODEL" && -s "$DISCOVERED_MODEL_PATH" ]]; then
      IFS= read -r CLIENT_MODEL <"$DISCOVERED_MODEL_PATH" || true
    fi
    if [[ -z "$CLIENT_MODEL" ]]; then
      CLIENT_MODEL="claude-sonnet-5"
      printf 'PRODUCTION_WARNING: model discovery did not return a base Claude model; using %s for Claude Code\n' "$CLIENT_MODEL"
    fi
    CLIENT_ARGS=(--scenarios "$CLIENT_SCENARIOS" --model "$CLIENT_MODEL" \
      --timeout "$REQUEST_TIMEOUT" --concurrency "$CLIENT_CONCURRENCY" \
      --max-budget-usd "$CLIENT_MAX_BUDGET" --cancel-after "$CLIENT_CANCEL_AFTER")
    if [[ -n "$THINKING_MODEL" ]]; then
      CLIENT_ARGS+=(--thinking-model "$THINKING_MODEL")
    fi
    if ((FAIL_ON_WARNING)); then
      CLIENT_ARGS+=(--fail-on-warning)
    fi
    if ((KEEP_ARTIFACTS)); then
      CLIENT_ARGS+=(--artifact-dir "$REPORT_DIR/claude-code")
    fi
    run_phase claude-code-e2e "" bash "$SCRIPT_DIR/client-e2e.sh" "${CLIENT_ARGS[@]}"
  else
    record_skip claude-code-e2e "disabled by --skip-client-e2e"
  fi
fi

if ((RUN_STAIRCASE)); then
  if ((PREFLIGHT_OK == 0)); then
    record_skip concurrency-staircase "preflight failed"
  else
    STAIRCASE_ARGS=(--suite staircase --timeout "$REQUEST_TIMEOUT" \
      --concurrency-levels 1,5,10,20,50,100 \
      --requests 10 --load-max-tokens 32 --json-report "$REPORT_DIR/concurrency-staircase.json" --allow-remote)
    STAIRCASE_ARGS+=("${DEV_MODEL_ARGS[@]}")
    STAIRCASE_ARGS+=("${DEV_WARNING_ARGS[@]}")
    run_phase concurrency-staircase "$REPORT_DIR/concurrency-staircase.json" "$DEV_CHECK" "${STAIRCASE_ARGS[@]}"
  fi
fi

if ((RUN_SOAK)); then
  if ((PREFLIGHT_OK == 0)); then
    record_skip bounded-soak "preflight failed"
  else
    SOAK_ARGS=(--suite soak --timeout "$REQUEST_TIMEOUT" --concurrency 10 --requests 100 \
      --load-max-tokens 32 --soak-duration 5m --soak-max-requests 100 --soak-token-budget 3200 \
      --json-report "$REPORT_DIR/bounded-soak.json" --allow-remote)
    SOAK_ARGS+=("${DEV_MODEL_ARGS[@]}")
    SOAK_ARGS+=("${DEV_WARNING_ARGS[@]}")
    run_phase bounded-soak "$REPORT_DIR/bounded-soak.json" "$DEV_CHECK" "${SOAK_ARGS[@]}"
  fi
fi

passes=0
warnings=0
failures=0
skips=0
for status in "${PHASE_STATUSES[@]}"; do
  case "$status" in
    PASS) passes=$((passes + 1)) ;;
    WARN) warnings=$((warnings + 1)) ;;
    FAIL) failures=$((failures + 1)) ;;
    SKIP) skips=$((skips + 1)) ;;
  esac
done
printf 'Production test summary: pass=%d warn=%d fail=%d skip=%d\n' "$passes" "$warnings" "$failures" "$skips"
printf 'Summary file: %s\n' "$SUMMARY_PATH"
if ((failures > 0 || (FAIL_ON_WARNING && warnings > 0))); then
  exit 1
fi
