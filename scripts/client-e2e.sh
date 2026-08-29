#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

usage() {
  cat <<'EOF'
Usage:
  bash scripts/client-e2e.sh [options]

The API key is read only from KIRO_DEV_API_KEY. With no options this keeps the
low-cost compatibility check: Skill discovery plus one parameterized MCP call.

Options:
  --scenarios CSV          Cases to run, or all (default: skill-mcp).
  --model MODEL            Claude model (default: KIRO_DEV_MODEL or claude-sonnet-5).
  --thinking-model MODEL   Thinking model (default: <model>-thinking).
  --timeout DURATION       Per-client timeout (default: KIRO_DEV_CLIENT_TIMEOUT or 5m).
  --max-budget-usd N       Claude Code budget per client (default: 0.10).
  --concurrency N          Concurrent Claude Code clients (default: 2).
  --cancel-after DURATION  Cancellation probe deadline (default: 8s).
  --artifact-dir DIR       Preserve private client outputs in DIR.
  --keep-artifacts         Preserve outputs in a temporary directory and print its path.
  --fail-on-warning        Return non-zero when a scenario reports a warning.
  -h, --help               Show this help.

Scenario IDs:
  text-stream, skill-mcp, mcp-zero-arg, mcp-multi-call, file-tools,
  thinking, long-stream, cancel-recover, concurrent-clients

Set KIRO_DEV_ALLOW_REMOTE=1 before testing a non-loopback base URL. The file
tool case uses a disposable workspace and only Read/Write/Edit/Glob/Grep.
EOF
}

die() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

BASE_URL="${KIRO_DEV_BASE_URL:-http://127.0.0.1:8080}"
MODEL="${KIRO_DEV_MODEL:-claude-sonnet-5}"
THINKING_MODEL="${KIRO_DEV_THINKING_MODEL:-}"
SCENARIOS_RAW="${KIRO_DEV_CLIENT_SCENARIOS:-skill-mcp}"
CLIENT_TIMEOUT="${KIRO_DEV_CLIENT_TIMEOUT:-5m}"
MAX_BUDGET="${KIRO_DEV_MAX_BUDGET_USD:-0.10}"
CLIENT_CONCURRENCY="${KIRO_DEV_CLIENT_CONCURRENCY:-2}"
CANCEL_AFTER="${KIRO_DEV_CLIENT_CANCEL_AFTER:-8s}"
ARTIFACT_DIR="${KIRO_DEV_CLIENT_ARTIFACT_DIR:-}"
KEEP_ARTIFACTS=0
FAIL_ON_WARNING="${KIRO_DEV_CLIENT_FAIL_ON_WARNING:-0}"

while (($# > 0)); do
  case "$1" in
    --scenarios)
      (($# >= 2)) || die "--scenarios requires a value"
      SCENARIOS_RAW="$2"
      shift 2
      ;;
    --scenarios=*)
      SCENARIOS_RAW="${1#*=}"
      shift
      ;;
    --model)
      (($# >= 2)) || die "--model requires a value"
      MODEL="$2"
      shift 2
      ;;
    --model=*)
      MODEL="${1#*=}"
      shift
      ;;
    --thinking-model)
      (($# >= 2)) || die "--thinking-model requires a value"
      THINKING_MODEL="$2"
      shift 2
      ;;
    --thinking-model=*)
      THINKING_MODEL="${1#*=}"
      shift
      ;;
    --timeout)
      (($# >= 2)) || die "--timeout requires a value"
      CLIENT_TIMEOUT="$2"
      shift 2
      ;;
    --timeout=*)
      CLIENT_TIMEOUT="${1#*=}"
      shift
      ;;
    --max-budget-usd)
      (($# >= 2)) || die "--max-budget-usd requires a value"
      MAX_BUDGET="$2"
      shift 2
      ;;
    --max-budget-usd=*)
      MAX_BUDGET="${1#*=}"
      shift
      ;;
    --concurrency)
      (($# >= 2)) || die "--concurrency requires a value"
      CLIENT_CONCURRENCY="$2"
      shift 2
      ;;
    --concurrency=*)
      CLIENT_CONCURRENCY="${1#*=}"
      shift
      ;;
    --cancel-after)
      (($# >= 2)) || die "--cancel-after requires a value"
      CANCEL_AFTER="$2"
      shift 2
      ;;
    --cancel-after=*)
      CANCEL_AFTER="${1#*=}"
      shift
      ;;
    --artifact-dir)
      (($# >= 2)) || die "--artifact-dir requires a directory"
      ARTIFACT_DIR="$2"
      KEEP_ARTIFACTS=1
      shift 2
      ;;
    --artifact-dir=*)
      ARTIFACT_DIR="${1#*=}"
      KEEP_ARTIFACTS=1
      shift
      ;;
    --keep-artifacts)
      KEEP_ARTIFACTS=1
      shift
      ;;
    --fail-on-warning)
      FAIL_ON_WARNING=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      die "unknown option: $1"
      ;;
  esac
done

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

is_decimal() {
  [[ "$1" =~ ^[0-9]+([.][0-9]+)?$ ]]
}

is_binary_flag() {
  [[ "$1" == 0 || "$1" == 1 ]]
}

command -v go >/dev/null 2>&1 || die "go is required"
command -v claude >/dev/null 2>&1 || die "Claude Code is required"
command -v timeout >/dev/null 2>&1 || die "timeout is required"
command -v rg >/dev/null 2>&1 || die "ripgrep (rg) is required"
[[ -n "${KIRO_DEV_API_KEY:-}" ]] || die "KIRO_DEV_API_KEY is required"
[[ -n "$MODEL" ]] || die "model must not be empty"
is_nonzero_duration "$CLIENT_TIMEOUT" || die "invalid --timeout: $CLIENT_TIMEOUT"
is_nonzero_duration "$CANCEL_AFTER" || die "invalid --cancel-after: $CANCEL_AFTER"
is_positive_integer "$CLIENT_CONCURRENCY" || die "--concurrency must be a positive integer"
((CLIENT_CONCURRENCY <= 20)) || die "--concurrency must not exceed 20 for client E2E"
is_decimal "$MAX_BUDGET" || die "--max-budget-usd must be a non-negative decimal"
is_binary_flag "$FAIL_ON_WARNING" || die "--fail-on-warning must be 0 or 1"

case "$BASE_URL" in
  *'?'*|*'#'*|*'@'*) die "base URL must not contain query, fragment, or userinfo" ;;
  http://127.0.0.1:*|https://127.0.0.1:*|http://localhost:*|https://localhost:*|http://\[::1\]:*|https://\[::1\]:*) ;;
  http://*|https://*)
    [[ "${KIRO_DEV_ALLOW_REMOTE:-}" == "1" ]] || die "set KIRO_DEV_ALLOW_REMOTE=1 to test a non-loopback URL"
    ;;
  *) die "base URL must use http:// or https://" ;;
esac
BASE_URL="${BASE_URL%/}"

if [[ -z "$THINKING_MODEL" ]]; then
  THINKING_MODEL="${MODEL%-thinking}-thinking"
fi

declare -a SCENARIO_LIST=()
declare -A SCENARIO_SEEN=()
if [[ "$SCENARIOS_RAW" == "all" ]]; then
  SCENARIO_LIST=(
    text-stream skill-mcp mcp-zero-arg mcp-multi-call file-tools
    thinking long-stream cancel-recover concurrent-clients
  )
else
  IFS=',' read -r -a requested_scenarios <<< "$SCENARIOS_RAW"
  for scenario in "${requested_scenarios[@]}"; do
    scenario="${scenario//[[:space:]]/}"
    [[ -n "$scenario" ]] || die "--scenarios contains an empty value"
    case "$scenario" in
      text-stream|skill-mcp|mcp-zero-arg|mcp-multi-call|file-tools|thinking|long-stream|cancel-recover|concurrent-clients) ;;
      *) die "unknown client scenario: $scenario" ;;
    esac
    if [[ -z "${SCENARIO_SEEN[$scenario]:-}" ]]; then
      SCENARIO_LIST+=("$scenario")
      SCENARIO_SEEN["$scenario"]=1
    fi
  done
fi
(( ${#SCENARIO_LIST[@]} > 0 )) || die "at least one client scenario is required"

TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/kiro-client-e2e.XXXXXX")"
chmod 700 "$TMP_DIR"
declare -a ACTIVE_PIDS=()
cleanup() {
  local pid
  for pid in "${ACTIVE_PIDS[@]}"; do
    kill "$pid" >/dev/null 2>&1 || true
  done
  for pid in "${ACTIVE_PIDS[@]}"; do
    wait "$pid" >/dev/null 2>&1 || true
  done
  ACTIVE_PIDS=()
  if ((KEEP_ARTIFACTS)); then
    printf 'client E2E artifacts: %s\n' "$ARTIFACT_DIR" >&2
  fi
  rm -rf -- "$TMP_DIR"
}
trap cleanup EXIT

WORKSPACE="$TMP_DIR/workspace"
FILE_WORKSPACE="$TMP_DIR/file-workspace"
CONCURRENT_ROOT="$TMP_DIR/concurrent"
FIXTURE_BIN="$TMP_DIR/mcpfixture"
MCP_CONFIG="$TMP_DIR/mcp.json"
AUDIT_PATH="$TMP_DIR/mcp-audit.log"
SUMMARY_PATH="$TMP_DIR/client-summary.tsv"
mkdir -p "$WORKSPACE/.claude/skills/kiro-devcheck" "$FILE_WORKSPACE" "$CONCURRENT_ROOT"

if ((KEEP_ARTIFACTS)); then
  if [[ -z "$ARTIFACT_DIR" ]]; then
    ARTIFACT_DIR="$(mktemp -d "${TMPDIR:-/tmp}/kiro-client-artifacts.XXXXXX")"
  fi
  mkdir -p "$ARTIFACT_DIR"
  chmod 700 "$ARTIFACT_DIR"
  case "$ARTIFACT_DIR" in
    "$TMP_DIR"|"$TMP_DIR"/*) die "artifact directory must not be inside the temporary workspace" ;;
  esac
fi

(cd "$PROJECT_DIR" && go build -o "$FIXTURE_BIN" ./cmd/mcpfixture)
chmod 700 "$FIXTURE_BIN"

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

printf '{"mcpServers":{"devcheck":{"type":"stdio","command":%s,"env":{"KIRO_MCP_FIXTURE_AUDIT":%s}}}}\n' \
  "$(json_quote "$FIXTURE_BIN")" "$(json_quote "$AUDIT_PATH")" >"$MCP_CONFIG"
chmod 600 "$MCP_CONFIG"

reset_audit() {
  : >"$AUDIT_PATH"
  chmod 600 "$AUDIT_PATH"
}

write_skill() {
  local name="$1"
  shift
  local path="$WORKSPACE/.claude/skills/$name/SKILL.md"
  mkdir -p "$(dirname "$path")"
  printf '%s\n' '---' "name: $name" 'description: Kiro-Go Plus production client regression.' '---' "$@" >"$path"
  chmod 600 "$path"
}

run_cli() {
  local deadline="$1"
  local model="$2"
  local workspace="$3"
  local output="$4"
  local prompt="$5"
  local timeout_signal="${CLI_TIMEOUT_SIGNAL:-TERM}"
  shift 5
  (
    cd "$workspace"
    export ANTHROPIC_BASE_URL="$BASE_URL"
    export ANTHROPIC_API_KEY="${KIRO_DEV_API_KEY}"
    timeout --foreground --signal="$timeout_signal" --kill-after=20s "$deadline" \
      claude --bare --print --verbose --include-partial-messages \
        --setting-sources project --add-dir "$workspace" --model "$model" \
        --no-session-persistence --max-budget-usd "$MAX_BUDGET" \
        --output-format stream-json "$@" -- "$prompt"
  ) >"$output" 2>&1
}

assert_client_result() {
  local output="$1"
  local marker="$2"
  local result_line
  [[ -s "$output" ]] || return 1
  result_line="$(grep -E '"type"[[:space:]]*:[[:space:]]*"result"' "$output" | tail -n 1)" || return 1
  [[ -n "$result_line" && "$result_line" == *"$marker"* ]] || return 1
  [[ "$result_line" != *'"is_error":true'* ]]
}

has_client_progress() {
  local output="$1"
  [[ -s "$output" ]] || return 1
  # Claude Code emits stream_event/assistant records before a completed result.
  # Requiring one prevents an immediate auth/CLI error from being mistaken for
  # a successfully interrupted stream.
  grep -Eq '"type"[[:space:]]*:[[:space:]]*"(stream_event|assistant)"' "$output"
}

has_client_success_result() {
  local output="$1"
  local result_line
  result_line="$(grep -E '"type"[[:space:]]*:[[:space:]]*"result"' "$output" | tail -n 1)" || return 1
  [[ -n "$result_line" ]] || return 1
  [[ "$result_line" != *'"is_error":true'* && "$result_line" != *'"is_error": true'* ]]
}

is_expected_cancel_status() {
  case "$1" in
    124|130|137|143) return 0 ;;
    *) return 1 ;;
  esac
}

forget_active_pid() {
  local target="$1"
  local pid
  local -a remaining=()
  for pid in "${ACTIVE_PIDS[@]}"; do
    [[ "$pid" == "$target" ]] || remaining+=("$pid")
  done
  ACTIVE_PIDS=("${remaining[@]}")
}

audit_count() {
  local tool="$1"
  [[ -f "$AUDIT_PATH" ]] || {
    printf '0\n'
    return 0
  }
  awk -v wanted="$tool" '$0 == wanted { count++ } END { print count + 0 }' "$AUDIT_PATH"
}

record_case() {
  local name="$1"
  local status="$2"
  local detail="$3"
  CASE_NAMES+=("$name")
  CASE_STATUSES+=("$status")
  CASE_DETAILS+=("$detail")
  printf '%s\t%s\t%s\n' "$name" "$status" "$detail" >>"$SUMMARY_PATH"
  printf '%-24s %s (%s)\n' "$name" "$status" "$detail"
}

run_case() {
  local name="$1"
  shift
  CASE_STATUS_HINT=PASS
  CASE_DETAIL=""
  if "$@"; then
    record_case "$name" "$CASE_STATUS_HINT" "${CASE_DETAIL:-completed}"
  else
    record_case "$name" FAIL "${CASE_DETAIL:-check failed}"
  fi
}

case_text_stream() {
  local output="$TMP_DIR/text-stream.jsonl"
  run_cli "$CLIENT_TIMEOUT" "$MODEL" "$WORKSPACE" "$output" \
    'Reply exactly CLIENT_TEXT_STREAM_OK.' --tools ""
  assert_client_result "$output" CLIENT_TEXT_STREAM_OK || {
    CASE_DETAIL="stream result or marker missing"
    return 1
  }
  CASE_DETAIL="plain streaming response"
}

case_skill_mcp() {
  reset_audit
  write_skill kiro-devcheck \
    'Call mcp__devcheck__devcheck_echo exactly once with value MCP_CLIENT_E2E_OK.' \
    'After the tool returns, reply exactly MCP_CLIENT_E2E_OK.'
  local output="$TMP_DIR/skill-mcp.jsonl"
  run_cli "$CLIENT_TIMEOUT" "$MODEL" "$WORKSPACE" "$output" '/kiro-devcheck' \
    --mcp-config "$MCP_CONFIG" --strict-mcp-config \
    --allowedTools "mcp__devcheck__devcheck_echo" --tools ""
  if ! assert_client_result "$output" MCP_CLIENT_E2E_OK || [[ "$(audit_count devcheck_echo)" != 1 ]]; then
    CASE_DETAIL="Skill/MCP parameterized roundtrip was not completed exactly once"
    return 1
  fi
  CASE_DETAIL="Skill discovered and one MCP call completed"
}

case_mcp_zero_arg() {
  reset_audit
  write_skill kiro-zero-arg \
    'Call mcp__devcheck__devcheck_no_args exactly once. It accepts no arguments.' \
    'After the tool returns, reply exactly MCP_ZERO_ARG_OK.'
  local output="$TMP_DIR/mcp-zero-arg.jsonl"
  run_cli "$CLIENT_TIMEOUT" "$MODEL" "$WORKSPACE" "$output" '/kiro-zero-arg' \
    --mcp-config "$MCP_CONFIG" --strict-mcp-config \
    --allowedTools "mcp__devcheck__devcheck_no_args" --tools ""
  if ! assert_client_result "$output" MCP_ZERO_ARG_OK || [[ "$(audit_count devcheck_no_args)" != 1 ]]; then
    CASE_DETAIL="zero-argument MCP call was not completed exactly once"
    return 1
  fi
  CASE_DETAIL="zero-argument input survived the Claude Code roundtrip"
}

case_mcp_multi_call() {
  reset_audit
  write_skill kiro-multi-call \
    'Call mcp__devcheck__devcheck_repeat exactly twice: first with value MCP_MULTI_A, then with value MCP_MULTI_B.' \
    'Wait for both tool results, then reply exactly MCP_MULTI_OK.'
  local output="$TMP_DIR/mcp-multi-call.jsonl"
  run_cli "$CLIENT_TIMEOUT" "$MODEL" "$WORKSPACE" "$output" '/kiro-multi-call' \
    --mcp-config "$MCP_CONFIG" --strict-mcp-config \
    --allowedTools "mcp__devcheck__devcheck_repeat" --tools ""
  if ! assert_client_result "$output" MCP_MULTI_OK || [[ "$(audit_count devcheck_repeat)" != 2 ]]; then
    CASE_DETAIL="repeated MCP calls or their continuation was incomplete"
    return 1
  fi
  CASE_DETAIL="two sequential MCP calls completed without cross-talk"
}

case_file_tools() {
  local file="$FILE_WORKSPACE/claude-file-e2e.txt"
  local output="$TMP_DIR/file-tools.jsonl"
  rm -f -- "$file"
  run_cli "$CLIENT_TIMEOUT" "$MODEL" "$FILE_WORKSPACE" "$output" \
    "Use only Read, Write, and Edit. Create claude-file-e2e.txt with exactly FILE_WRITE_OK, read it, edit only FILE_WRITE_OK to FILE_EDIT_OK, read it again, then reply exactly FILE_TOOLS_OK." \
    --restricted --tools "Read,Write,Edit,Glob,Grep" \
    --allowedTools "Read,Write,Edit,Glob,Grep" --permission-mode acceptEdits
  if ! assert_client_result "$output" FILE_TOOLS_OK || [[ ! -f "$file" ]]; then
    CASE_DETAIL="Claude Code file tool sequence did not create the test file"
    return 1
  fi
  local content
  content="$(tr -d '\r\n' <"$file")"
  if [[ "$content" != FILE_EDIT_OK ]]; then
    CASE_DETAIL="file tool sequence left unexpected content"
    return 1
  fi
  CASE_DETAIL="Read/Write/Edit completed in an isolated workspace"
}

case_thinking() {
  local output="$TMP_DIR/thinking.jsonl"
  run_cli "$CLIENT_TIMEOUT" "$THINKING_MODEL" "$TMP_DIR/thinking-workspace" "$output" \
    'Think briefly, then reply exactly CLIENT_THINKING_OK.' --tools ""
  if ! assert_client_result "$output" CLIENT_THINKING_OK; then
    CASE_DETAIL="thinking-model client request did not complete"
    return 1
  fi
  if ! rg -qi 'thinking|reasoning' "$output"; then
    CASE_STATUS_HINT=WARN
    CASE_DETAIL="completed, but Claude Code output hid reasoning events"
  else
    CASE_DETAIL="thinking-model request completed"
  fi
}

case_long_stream() {
  local output="$TMP_DIR/long-stream.jsonl"
  run_cli "$CLIENT_TIMEOUT" "$MODEL" "$TMP_DIR/long-workspace" "$output" \
    'Produce 80 numbered lines, each with six short words. Begin immediately and end with LONG_CLIENT_STREAM_OK.' --tools ""
  if ! assert_client_result "$output" LONG_CLIENT_STREAM_OK; then
    CASE_DETAIL="long streaming client result or marker missing"
    return 1
  fi
  local bytes
  bytes="$(wc -c <"$output")"
  if ((bytes < 512)); then
    CASE_STATUS_HINT=WARN
    CASE_DETAIL="completed with a short captured stream (${bytes} bytes)"
  else
    CASE_DETAIL="long stream completed (${bytes} captured bytes)"
  fi
}

case_cancel_recover() {
  local cancel_workspace="$TMP_DIR/cancel-workspace"
  local canceled_output="$TMP_DIR/cancel.jsonl"
  local recovery_output="$TMP_DIR/cancel-recovery.jsonl"
  local cancel_status=0
  local previous_timeout_signal="${CLI_TIMEOUT_SIGNAL-}"
  CLI_TIMEOUT_SIGNAL=INT
  if run_cli "$CANCEL_AFTER" "$MODEL" "$cancel_workspace" "$canceled_output" \
    'Write a very long technical report with 500 numbered sections. Begin immediately.' --tools ""; then
    cancel_status=0
  else
    cancel_status=$?
  fi
  if [[ -n "$previous_timeout_signal" ]]; then
    CLI_TIMEOUT_SIGNAL="$previous_timeout_signal"
  else
    unset CLI_TIMEOUT_SIGNAL
  fi
  if [[ ! -s "$canceled_output" ]]; then
    CASE_DETAIL="cancellation probe produced no client output"
    return 1
  fi
  if ! is_expected_cancel_status "$cancel_status" && ((cancel_status != 0)); then
    CASE_DETAIL="cancellation probe exited with unexpected status ${cancel_status}"
    return 1
  fi
  if ((cancel_status == 0)) && ! has_client_success_result "$canceled_output"; then
    CASE_DETAIL="cancellation probe completed without a successful Claude Code result"
    return 1
  fi
  if ((cancel_status != 0)) && ! has_client_progress "$canceled_output"; then
    CASE_DETAIL="cancellation probe stopped before receiving a partial Claude Code stream"
    return 1
  fi
  run_cli "$CLIENT_TIMEOUT" "$MODEL" "$TMP_DIR/recovery-workspace" "$recovery_output" \
    'Reply exactly CLIENT_CANCEL_RECOVERY_OK.' --tools ""
  if ! assert_client_result "$recovery_output" CLIENT_CANCEL_RECOVERY_OK; then
    CASE_DETAIL="recovery request failed after cancellation (probe exit ${cancel_status})"
    return 1
  fi
  if ((cancel_status == 0)); then
    CASE_STATUS_HINT=WARN
    CASE_DETAIL="recovery passed; long probe completed before the cancellation deadline"
  else
    CASE_DETAIL="interrupted client stream recovered with a clean subsequent request (status ${cancel_status})"
  fi
}

case_concurrent_clients() {
  local -a pids=()
  local -a outputs=()
  local -a markers=()
  local i pid wait_status failed=0
  local -a wait_statuses=()
  for ((i = 1; i <= CLIENT_CONCURRENCY; i++)); do
    local workspace="$CONCURRENT_ROOT/client-$i"
    local output="$TMP_DIR/concurrent-$i.jsonl"
    local marker="CLIENT_CONCURRENT_${i}_OK"
    mkdir -p "$workspace"
    outputs+=("$output")
    markers+=("$marker")
    (
      run_cli "$CLIENT_TIMEOUT" "$MODEL" "$workspace" "$output" \
        "Reply exactly $marker." --tools ""
    ) &
    pids+=("$!")
    ACTIVE_PIDS+=("$!")
  done
  for pid in "${pids[@]}"; do
    if wait "$pid"; then
      wait_status=0
    else
      wait_status=$?
    fi
    wait_statuses+=("$wait_status")
    forget_active_pid "$pid"
    if ((wait_status != 0)); then
      failed=1
    fi
  done
  if ((failed)); then
    CASE_DETAIL="one or more of ${CLIENT_CONCURRENCY} concurrent clients exited abnormally (statuses: ${wait_statuses[*]})"
    return 1
  fi
  for ((i = 0; i < CLIENT_CONCURRENCY; i++)); do
    if ! assert_client_result "${outputs[$i]}" "${markers[$i]}"; then
      CASE_DETAIL="concurrent client $((i + 1)) missed its response marker"
      return 1
    fi
    local j
    for ((j = 0; j < CLIENT_CONCURRENCY; j++)); do
      if ((i != j)) && rg -Fq "${markers[$j]}" "${outputs[$i]}"; then
        CASE_DETAIL="concurrent response cross-talk detected between clients"
        return 1
      fi
    done
  done
  CASE_DETAIL="${CLIENT_CONCURRENCY} isolated Claude Code clients completed concurrently"
}

mkdir -p "$TMP_DIR/thinking-workspace" "$TMP_DIR/long-workspace" "$TMP_DIR/cancel-workspace" "$TMP_DIR/recovery-workspace"
printf 'scenario\tstatus\tdetail\n' >"$SUMMARY_PATH"
chmod 600 "$SUMMARY_PATH"
declare -a CASE_NAMES=()
declare -a CASE_STATUSES=()
declare -a CASE_DETAILS=()

for scenario in "${SCENARIO_LIST[@]}"; do
  case "$scenario" in
    text-stream) run_case "$scenario" case_text_stream ;;
    skill-mcp) run_case "$scenario" case_skill_mcp ;;
    mcp-zero-arg) run_case "$scenario" case_mcp_zero_arg ;;
    mcp-multi-call) run_case "$scenario" case_mcp_multi_call ;;
    file-tools) run_case "$scenario" case_file_tools ;;
    thinking) run_case "$scenario" case_thinking ;;
    long-stream) run_case "$scenario" case_long_stream ;;
    cancel-recover) run_case "$scenario" case_cancel_recover ;;
    concurrent-clients) run_case "$scenario" case_concurrent_clients ;;
  esac
done

if ((KEEP_ARTIFACTS)); then
  cp -- "$SUMMARY_PATH" "$ARTIFACT_DIR/client-summary.tsv"
  cp -- "$MCP_CONFIG" "$ARTIFACT_DIR/mcp.json"
  cp -- "$AUDIT_PATH" "$ARTIFACT_DIR/mcp-audit.log" 2>/dev/null || true
  find "$TMP_DIR" -maxdepth 1 -type f -name '*.jsonl' -exec cp -- {} "$ARTIFACT_DIR/" \;
  find "$ARTIFACT_DIR" -type f -exec chmod 600 {} \;
fi

failures=0
warnings=0
for status in "${CASE_STATUSES[@]}"; do
  case "$status" in
    FAIL) failures=$((failures + 1)) ;;
    WARN) warnings=$((warnings + 1)) ;;
  esac
done
printf 'Client E2E summary: pass=%d warn=%d fail=%d\n' \
  "$(( ${#CASE_STATUSES[@]} - failures - warnings ))" "$warnings" "$failures"
if ((failures > 0 || (FAIL_ON_WARNING && warnings > 0))); then
  exit 1
fi
