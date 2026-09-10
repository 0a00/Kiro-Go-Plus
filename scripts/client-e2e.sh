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
  --agent-timeout DURATION Timeout for multi-turn and long-tool clients (default: KIRO_DEV_AGENT_TIMEOUT or 15m).
  --max-budget-usd N       Claude Code budget per client (default: 0.10).
  --agent-max-budget-usd N Budget for multi-turn/long-tool agent cases (default: 0.75).
  --concurrency N          Concurrent Claude Code clients (default: 2).
  --cancel-after DURATION  Cancellation probe deadline (default: 8s).
  --artifact-dir DIR       Preserve private client outputs in DIR.
  --keep-artifacts         Preserve outputs in a temporary directory and print its path.
  --fail-on-warning        Return non-zero when a scenario reports a warning.
  -h, --help               Show this help.

Scenario IDs:
  text-stream, skill-mcp, mcp-zero-arg, mcp-multi-call, file-tools,
  thinking, long-stream, cancel-recover, concurrent-clients,
  workspace-multiturn, workspace-long-tools, workspace-repo-loop,
  workspace-error-recovery, workspace-parallel-tools, permission-plan,
  structured-output, mcp-large-result, mcp-error-recovery, web-search,
  workspace-image

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
AGENT_TIMEOUT="${KIRO_DEV_AGENT_TIMEOUT:-15m}"
MAX_BUDGET="${KIRO_DEV_MAX_BUDGET_USD:-0.10}"
AGENT_MAX_BUDGET="${KIRO_DEV_AGENT_MAX_BUDGET_USD:-0.75}"
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
    --agent-timeout)
      (($# >= 2)) || die "--agent-timeout requires a value"
      AGENT_TIMEOUT="$2"
      shift 2
      ;;
    --agent-timeout=*)
      AGENT_TIMEOUT="${1#*=}"
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
    --agent-max-budget-usd)
      (($# >= 2)) || die "--agent-max-budget-usd requires a value"
      AGENT_MAX_BUDGET="$2"
      shift 2
      ;;
    --agent-max-budget-usd=*)
      AGENT_MAX_BUDGET="${1#*=}"
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
command -v jq >/dev/null 2>&1 || die "jq is required"
command -v base64 >/dev/null 2>&1 || die "base64 is required"
command -v git >/dev/null 2>&1 || die "git is required"
[[ -n "${KIRO_DEV_API_KEY:-}" ]] || die "KIRO_DEV_API_KEY is required"
[[ -n "$MODEL" ]] || die "model must not be empty"
is_nonzero_duration "$CLIENT_TIMEOUT" || die "invalid --timeout: $CLIENT_TIMEOUT"
is_nonzero_duration "$AGENT_TIMEOUT" || die "invalid --agent-timeout: $AGENT_TIMEOUT"
is_nonzero_duration "$CANCEL_AFTER" || die "invalid --cancel-after: $CANCEL_AFTER"
is_positive_integer "$CLIENT_CONCURRENCY" || die "--concurrency must be a positive integer"
((CLIENT_CONCURRENCY <= 20)) || die "--concurrency must not exceed 20 for client E2E"
is_decimal "$MAX_BUDGET" || die "--max-budget-usd must be a non-negative decimal"
is_decimal "$AGENT_MAX_BUDGET" || die "--agent-max-budget-usd must be a non-negative decimal"
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
    workspace-multiturn workspace-long-tools workspace-repo-loop
    workspace-error-recovery workspace-parallel-tools permission-plan
    structured-output mcp-large-result mcp-error-recovery web-search
    workspace-image
  )
else
  IFS=',' read -r -a requested_scenarios <<< "$SCENARIOS_RAW"
  for scenario in "${requested_scenarios[@]}"; do
    scenario="${scenario//[[:space:]]/}"
    [[ -n "$scenario" ]] || die "--scenarios contains an empty value"
    case "$scenario" in
      text-stream|skill-mcp|mcp-zero-arg|mcp-multi-call|file-tools|thinking|long-stream|cancel-recover|concurrent-clients|workspace-multiturn|workspace-long-tools|workspace-repo-loop|workspace-error-recovery|workspace-parallel-tools|permission-plan|structured-output|mcp-large-result|mcp-error-recovery|web-search|workspace-image) ;;
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
CLIENT_CONFIG_DIR="$TMP_DIR/claude-config"
mkdir -p "$WORKSPACE/.claude/skills/kiro-devcheck" "$FILE_WORKSPACE" "$CONCURRENT_ROOT" "$CLIENT_CONFIG_DIR"

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

run_cli_with_budget() {
  local budget="$1"
  local persist_session="$2"
  local deadline="$3"
  local model="$4"
  local workspace="$5"
  local output="$6"
  local prompt="$7"
  local timeout_signal="${CLI_TIMEOUT_SIGNAL:-TERM}"
  local -a persistence_args=()
  if ((persist_session == 0)); then
    persistence_args+=(--no-session-persistence)
  fi
  shift 7
  (
    cd "$workspace"
    export ANTHROPIC_BASE_URL="$BASE_URL"
    export ANTHROPIC_API_KEY="${KIRO_DEV_API_KEY}"
    export CLAUDE_CONFIG_DIR="$CLIENT_CONFIG_DIR"
    timeout --foreground --signal="$timeout_signal" --kill-after=20s "$deadline" \
      claude --bare --print --verbose --include-partial-messages \
        --setting-sources project --add-dir "$workspace" --model "$model" \
        "${persistence_args[@]}" --max-budget-usd "$budget" \
        --output-format stream-json "$@" -- "$prompt"
  ) >"$output" 2>&1
}

run_cli() {
  run_cli_with_budget "$MAX_BUDGET" 0 "$@"
}

run_cli_session() {
  run_cli_with_budget "$AGENT_MAX_BUDGET" 1 "$@"
}

run_cli_resume() {
  local deadline="$1"
  local model="$2"
  local workspace="$3"
  local output="$4"
  local session_id="$5"
  local prompt="$6"
  local timeout_signal="${CLI_TIMEOUT_SIGNAL:-TERM}"
  shift 6
  (
    cd "$workspace"
    export ANTHROPIC_BASE_URL="$BASE_URL"
    export ANTHROPIC_API_KEY="${KIRO_DEV_API_KEY}"
    export CLAUDE_CONFIG_DIR="$CLIENT_CONFIG_DIR"
    timeout --foreground --signal="$timeout_signal" --kill-after=20s "$deadline" \
      claude --bare --print --verbose --include-partial-messages \
        --resume "$session_id" --model "$model" --max-budget-usd "$AGENT_MAX_BUDGET" \
        --output-format stream-json "$@" -- "$prompt"
  ) >"$output" 2>&1
}

client_tool_use_count() {
  jq -s 'map(select(.type == "assistant") | .message.content[]? | select(.type == "tool_use")) | length' "$1"
}

client_tool_result_count() {
  jq -s 'map(select(.type == "user") | .message.content[]? | select(.type == "tool_result")) | length' "$1"
}

client_tool_error_count() {
  jq -s '[.[] | select(.type == "user") | .message.content[]? | select(.type == "tool_result" and .is_error == true)] | length' "$1"
}

client_result_subtype() {
  jq -r 'select(.type == "result") | .subtype // "unknown"' "$1" | tail -n 1
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

case_workspace_multiturn() {
  local workspace="$TMP_DIR/workspace-multiturn"
  local first="$TMP_DIR/workspace-multiturn-first.jsonl"
  local second="$TMP_DIR/workspace-multiturn-second.jsonl"
  local session_id second_status second_tools second_results second_errors subtype
  mkdir -p "$workspace"
  set +e
  run_cli_session "$AGENT_TIMEOUT" "$MODEL" "$workspace" "$first" \
    '写个shell脚本，随便写' \
    --permission-mode acceptEdits
  local first_status=$?
  set -e
  if ((first_status != 0)) || ! has_client_success_result "$first"; then
    CASE_DETAIL="initial Claude Code workspace turn failed (status ${first_status})"
    return 1
  fi
  session_id="$(jq -r 'select(.session_id != null) | .session_id' "$first" | tail -n 1)"
  if [[ -z "$session_id" ]]; then
    CASE_DETAIL="initial turn did not expose a resumable Claude Code session"
    return 1
  fi
  set +e
  run_cli_resume "$AGENT_TIMEOUT" "$MODEL" "$workspace" "$second" "$session_id" \
    '增加5倍代码量' --tools 'Read,Write,Edit,Bash' --allowedTools 'Read,Write,Edit,Bash' \
    --permission-mode acceptEdits
  second_status=$?
  set -e
  second_tools="$(client_tool_use_count "$second")"
  second_results="$(client_tool_result_count "$second")"
  second_errors="$(client_tool_error_count "$second")"
  subtype="$(client_result_subtype "$second")"
  if ((second_tools < 1 || second_results < 1)); then
    CASE_DETAIL="multi-turn edit produced no structured tool/result pair (status ${second_status}, subtype ${subtype})"
    return 1
  fi
  if ((second_status != 0 && subtype != "error_max_budget_usd")); then
    CASE_DETAIL="multi-turn edit exited unexpectedly (status ${second_status}, subtype ${subtype})"
    return 1
  fi
  if ((second_errors > 0)); then
    CASE_STATUS_HINT=WARN
    CASE_DETAIL="structured tools survived the resumed turn, but Claude Code reported ${second_errors} tool error(s)"
    return 0
  fi
  if [[ "$subtype" == "error_max_budget_usd" ]]; then
    CASE_STATUS_HINT=WARN
    CASE_DETAIL="structured tools survived the resumed turn before the Claude Code budget was exhausted (${second_tools} calls)"
    return 0
  fi
  CASE_DETAIL="resumed Claude Code turn issued ${second_tools} structured tool calls"
}

case_workspace_long_tools() {
  local workspace="$TMP_DIR/workspace-long-tools"
  local output="$TMP_DIR/workspace-long-tools.jsonl"
  local tool_uses tool_results tool_errors subtype file_count
  mkdir -p "$workspace"
  set +e
  run_cli_session "$AGENT_TIMEOUT" "$MODEL" "$workspace" "$output" \
    'Complete this task in the current workspace without asking for confirmation. Use Read, Write, Edit, Glob, Grep, and Bash only. Bash may use pwd, find, wc, grep, and sort, and must stay inside the current workspace. Create 12 small text files in three subdirectories with cross references. Inspect the tree, read files in multiple batches, use Grep to find references, edit at least 8 files to add a second version and update references, reread the edited files, and run final consistency checks. Make at least 20 separate tool calls across multiple turns. Finish with the exact marker LONG_TOOL_STRESS_OK.' \
    --tools 'Read,Write,Edit,Glob,Grep,Bash' --allowedTools 'Read,Write,Edit,Glob,Grep,Bash' --permission-mode acceptEdits
  local status=$?
  set -e
  tool_uses="$(client_tool_use_count "$output")"
  tool_results="$(client_tool_result_count "$output")"
  tool_errors="$(client_tool_error_count "$output")"
  subtype="$(client_result_subtype "$output")"
  file_count="$(find "$workspace" -type f | wc -l)"
  if ((status != 0)) || [[ "$subtype" != "success" ]]; then
    CASE_DETAIL="long tool chain did not finish (status ${status}, subtype ${subtype}, calls ${tool_uses}, files ${file_count})"
    return 1
  fi
  if ((tool_uses < 20 || tool_results < tool_uses || tool_errors > 0)) || ! rg -q 'LONG_TOOL_STRESS_OK' "$output"; then
    CASE_DETAIL="long tool chain integrity failed (calls ${tool_uses}, results ${tool_results}, errors ${tool_errors}, files ${file_count})"
    return 1
  fi
  CASE_DETAIL="${tool_uses} structured tool calls and ${tool_results} results completed across ${file_count} files"
}

case_workspace_repo_loop() {
  local workspace="$TMP_DIR/workspace-repo-loop"
  local output="$TMP_DIR/workspace-repo-loop.jsonl"
  local tool_uses tool_results subtype status
  mkdir -p "$workspace/src" "$workspace/tests"
  printf '%s\n' '# Repo loop fixture' 'TODO: replace this line' >"$workspace/README.md"
  printf '%s\n' '#!/usr/bin/env bash' 'printf "REPO_OLD\n"' >"$workspace/src/app.sh"
  printf '%s\n' '#!/usr/bin/env bash' 'bash src/app.sh' >"$workspace/tests/run.sh"
  chmod 700 "$workspace/src/app.sh" "$workspace/tests/run.sh"
  git -C "$workspace" init -q
  git -C "$workspace" config user.email claude-code-e2e@example.invalid
  git -C "$workspace" config user.name Claude-Code-E2E
  git -C "$workspace" add .
  git -C "$workspace" commit -qm 'fixture baseline'
  set +e
  run_cli_session "$AGENT_TIMEOUT" "$MODEL" "$workspace" "$output" \
    'Work as a developer in this existing git repository. Inspect the README, source, and test script with Read and Glob. Make a concrete small change: replace REPO_OLD with REPO_NEW, document the change in README.md, and add a shell syntax check. Use Edit or Write for file changes, Bash for the check, then inspect git diff and run git diff --check. Do not commit. Finish with the exact marker REPO_WORKFLOW_OK.' \
    --tools 'Read,Write,Edit,Glob,Grep,Bash' --allowedTools 'Read,Write,Edit,Glob,Grep,Bash' --permission-mode acceptEdits
  status=$?
  set -e
  tool_uses="$(client_tool_use_count "$output")"
  tool_results="$(client_tool_result_count "$output")"
  subtype="$(client_result_subtype "$output")"
  if ((status != 0)) || [[ "$subtype" != "success" ]] || ! rg -q 'REPO_WORKFLOW_OK' "$output"; then
    CASE_DETAIL="repository edit/test loop did not finish (status ${status}, subtype ${subtype}, calls ${tool_uses})"
    return 1
  fi
  if ((tool_uses < 5 || tool_results < tool_uses)) || ! rg -q 'REPO_NEW' "$workspace/src/app.sh" || ! rg -q 'REPO_NEW' "$workspace/README.md"; then
    CASE_DETAIL="repository loop integrity failed (calls ${tool_uses}, results ${tool_results})"
    return 1
  fi
  if ! git -C "$workspace" diff --check; then
    CASE_DETAIL="repository loop left whitespace errors in git diff"
    return 1
  fi
  CASE_DETAIL="repository read/edit/test/diff loop completed with ${tool_uses} tool calls"
}

case_workspace_error_recovery() {
  local workspace="$TMP_DIR/workspace-error-recovery"
  local output="$TMP_DIR/workspace-error-recovery.jsonl"
  local tool_uses tool_results tool_errors subtype status
  mkdir -p "$workspace"
  printf '%s\n' 'Known recovery input.' >"$workspace/known.txt"
  set +e
  run_cli_session "$AGENT_TIMEOUT" "$MODEL" "$workspace" "$output" \
    'Test tool error recovery. First try to read the deliberately missing file missing-file.txt. That failure is expected; do not stop. Then read known.txt, create recovered.txt containing exactly TOOL_ERROR_RECOVERY_OK, read it back, and finish with the exact marker TOOL_ERROR_RECOVERY_DONE.' \
    --tools 'Read,Write' --allowedTools 'Read,Write' --permission-mode acceptEdits
  status=$?
  set -e
  tool_uses="$(client_tool_use_count "$output")"
  tool_results="$(client_tool_result_count "$output")"
  tool_errors="$(client_tool_error_count "$output")"
  subtype="$(client_result_subtype "$output")"
  if ((status != 0)) || [[ "$subtype" != "success" ]] || ! rg -q 'TOOL_ERROR_RECOVERY_DONE' "$output"; then
    CASE_DETAIL="tool error recovery did not finish (status ${status}, subtype ${subtype}, calls ${tool_uses}, errors ${tool_errors})"
    return 1
  fi
  if ((tool_errors < 1 || tool_results < tool_uses)) || [[ ! -f "$workspace/recovered.txt" ]] || ! rg -q '^TOOL_ERROR_RECOVERY_OK$' "$workspace/recovered.txt"; then
    CASE_DETAIL="tool error was not observed and recovered (calls ${tool_uses}, results ${tool_results}, errors ${tool_errors})"
    return 1
  fi
  CASE_DETAIL="deliberate tool failure recovered with ${tool_uses} tool calls and ${tool_errors} tool error"
}

case_workspace_parallel_tools() {
  local workspace="$TMP_DIR/workspace-parallel-tools"
  local output="$TMP_DIR/workspace-parallel-tools.jsonl"
  local tool_uses tool_results subtype status
  mkdir -p "$workspace"
  printf '%s\n' 'PARALLEL_A' >"$workspace/a.txt"
  printf '%s\n' 'PARALLEL_B' >"$workspace/b.txt"
  printf '%s\n' 'PARALLEL_C' >"$workspace/c.txt"
  printf '%s\n' 'PARALLEL_D' >"$workspace/d.txt"
  set +e
  run_cli_session "$AGENT_TIMEOUT" "$MODEL" "$workspace" "$output" \
    'Inspect this workspace as a coding assistant. Use Glob to find the four text files, then issue separate Read calls for a.txt, b.txt, c.txt, and d.txt; independent reads may be parallel. Create summary.txt containing all four values, reread it, and finish with the exact marker PARALLEL_TOOLS_OK.' \
    --tools 'Read,Write,Glob' --allowedTools 'Read,Write,Glob' --permission-mode acceptEdits
  status=$?
  set -e
  tool_uses="$(client_tool_use_count "$output")"
  tool_results="$(client_tool_result_count "$output")"
  subtype="$(client_result_subtype "$output")"
  if ((status != 0)) || [[ "$subtype" != "success" ]] || ! rg -q 'PARALLEL_TOOLS_OK' "$output"; then
    CASE_DETAIL="parallel read workflow did not finish (status ${status}, subtype ${subtype}, calls ${tool_uses})"
    return 1
  fi
  if ((tool_uses < 6 || tool_results < tool_uses)) || [[ ! -f "$workspace/summary.txt" ]] || ! rg -q 'PARALLEL_[A-D]' "$workspace/summary.txt"; then
    CASE_DETAIL="parallel tool results were incomplete (calls ${tool_uses}, results ${tool_results})"
    return 1
  fi
  CASE_DETAIL="four-file parallel read and summary workflow completed with ${tool_uses} tool calls"
}

case_permission_plan() {
  local workspace="$TMP_DIR/permission-plan"
  local output="$TMP_DIR/permission-plan.jsonl"
  local before after status subtype
  mkdir -p "$workspace"
  printf '%s\n' 'PLAN_INPUT_UNCHANGED' >"$workspace/input.txt"
  before="$(sha256sum "$workspace/input.txt")"
  set +e
  run_cli "$CLIENT_TIMEOUT" "$MODEL" "$workspace" "$output" \
    'Review this workspace in plan mode. Read input.txt and explain a safe change plan, but do not write, edit, delete, execute commands, or modify any file. Finish with the exact marker PERMISSION_PLAN_OK.' \
    --tools 'Read,Write,Edit,Bash' --allowedTools 'Read,Write,Edit,Bash' --permission-mode plan
  status=$?
  set -e
  after="$(sha256sum "$workspace/input.txt")"
  subtype="$(client_result_subtype "$output")"
  if ((status != 0)) || [[ "$subtype" != "success" ]] || ! rg -q 'PERMISSION_PLAN_OK' "$output"; then
    CASE_DETAIL="plan-mode read-only workflow did not finish (status ${status}, subtype ${subtype})"
    return 1
  fi
  if [[ "$before" != "$after" ]] || find "$workspace" -maxdepth 1 -type f ! -name input.txt -print -quit | rg -q .; then
    CASE_DETAIL="plan-mode workflow modified its disposable workspace"
    return 1
  fi
  CASE_DETAIL="plan-mode inspection completed without workspace mutation"
}

case_structured_output() {
  local output="$TMP_DIR/structured-output.jsonl"
  local schema='{"type":"object","properties":{"status":{"type":"string","const":"STRUCTURED_OUTPUT_OK"}},"required":["status"],"additionalProperties":false}'
  run_cli "$CLIENT_TIMEOUT" "$MODEL" "$WORKSPACE" "$output" \
    'Return the requested structured result immediately. Set status to STRUCTURED_OUTPUT_OK.' \
    --tools '' --json-schema "$schema"
  if ! assert_client_result "$output" STRUCTURED_OUTPUT_OK; then
    CASE_DETAIL="Claude Code structured-output request did not complete"
    return 1
  fi
  if ! jq -e '.. | objects | select(has("structured_output"))' "$output" >/dev/null 2>&1; then
    CASE_STATUS_HINT=WARN
    CASE_DETAIL="response completed, but no structured_output event was exposed"
    return 0
  fi
  CASE_DETAIL="JSON-schema constrained response completed"
}

case_mcp_large_result() {
  reset_audit
  write_skill kiro-large-result \
    'Call mcp__devcheck__devcheck_large exactly once. Read the complete result, then reply exactly MCP_LARGE_RESULT_OK.'
  local output="$TMP_DIR/mcp-large-result.jsonl"
  run_cli "$CLIENT_TIMEOUT" "$MODEL" "$WORKSPACE" "$output" '/kiro-large-result' \
    --mcp-config "$MCP_CONFIG" --strict-mcp-config \
    --allowedTools 'mcp__devcheck__devcheck_large' --tools ''
  local bytes
  bytes="$(wc -c <"$output")"
  if ! assert_client_result "$output" MCP_LARGE_RESULT_OK || [[ "$(audit_count devcheck_large)" != 1 ]] || ((bytes < 8192)); then
    CASE_DETAIL="large MCP result roundtrip was incomplete (bytes ${bytes})"
    return 1
  fi
  CASE_DETAIL="bounded large MCP result roundtrip completed (${bytes} captured bytes)"
}

case_mcp_error_recovery() {
  reset_audit
  write_skill kiro-error-recovery \
    'Call mcp__devcheck__devcheck_fail exactly once and observe its deliberate error. Continue after the error by calling mcp__devcheck__devcheck_echo exactly once with value MCP_ERROR_RECOVERED, then reply exactly MCP_ERROR_RECOVERY_OK.'
  local output="$TMP_DIR/mcp-error-recovery.jsonl"
  local tool_errors
  run_cli "$CLIENT_TIMEOUT" "$MODEL" "$WORKSPACE" "$output" '/kiro-error-recovery' \
    --mcp-config "$MCP_CONFIG" --strict-mcp-config \
    --allowedTools 'mcp__devcheck__devcheck_fail,mcp__devcheck__devcheck_echo' --tools ''
  tool_errors="$(client_tool_error_count "$output")"
  if ! assert_client_result "$output" MCP_ERROR_RECOVERY_OK || [[ "$(audit_count devcheck_fail)" != 1 ]] || [[ "$(audit_count devcheck_echo)" != 1 ]] || ((tool_errors < 1)); then
    CASE_DETAIL="MCP error recovery did not execute both fixture calls"
    return 1
  fi
  CASE_DETAIL="MCP tool error was surfaced and the following call recovered"
}

case_web_search() {
  local output="$TMP_DIR/web-search.jsonl"
  local tool_uses tool_results subtype status
  set +e
  run_cli_session "$CLIENT_TIMEOUT" "$MODEL" "$WORKSPACE" "$output" \
    'Use the native WebSearch tool to search for the official Anthropic Claude Code documentation. Read the returned result, summarize one source title, and finish with the exact marker CLAUDE_WEB_SEARCH_OK.' \
    --restricted --tools 'WebSearch' --allowedTools 'WebSearch' --permission-mode acceptEdits
  status=$?
  set -e
  tool_uses="$(client_tool_use_count "$output")"
  tool_results="$(client_tool_result_count "$output")"
  subtype="$(client_result_subtype "$output")"
  if ((status != 0)) || [[ "$subtype" != "success" ]] || ! rg -q 'CLAUDE_WEB_SEARCH_OK' "$output"; then
    CASE_DETAIL="Claude Code WebSearch workflow did not finish (status ${status}, subtype ${subtype}, calls ${tool_uses})"
    return 1
  fi
  if ((tool_uses < 1 || tool_results < tool_uses)); then
    CASE_DETAIL="WebSearch completed without a structured tool/result pair (calls ${tool_uses}, results ${tool_results})"
    return 1
  fi
  CASE_DETAIL="native WebSearch completed with ${tool_uses} tool call"
}

case_workspace_image() {
  local workspace="$TMP_DIR/workspace-image"
  local output="$TMP_DIR/workspace-image.jsonl"
  local tool_uses tool_results subtype status
  mkdir -p "$workspace"
  printf '%s' 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=' \
    | base64 --decode >"$workspace/pixel.png"
  set +e
  run_cli_session "$CLIENT_TIMEOUT" "$MODEL" "$workspace" "$output" \
    'Use Read to inspect pixel.png as an image. Confirm that an image was received, then finish with the exact marker CLAUDE_IMAGE_READ_OK. Do not modify any file.' \
    --restricted --tools 'Read' --allowedTools 'Read' --permission-mode plan
  status=$?
  set -e
  tool_uses="$(client_tool_use_count "$output")"
  tool_results="$(client_tool_result_count "$output")"
  subtype="$(client_result_subtype "$output")"
  if ((status != 0)) || [[ "$subtype" != "success" ]] || ! rg -q 'CLAUDE_IMAGE_READ_OK' "$output"; then
    CASE_DETAIL="Claude Code image-read workflow did not finish (status ${status}, subtype ${subtype}, calls ${tool_uses})"
    return 1
  fi
  if ((tool_uses < 1 || tool_results < tool_uses)); then
    CASE_DETAIL="image read completed without a structured tool/result pair (calls ${tool_uses}, results ${tool_results})"
    return 1
  fi
  CASE_DETAIL="Claude Code Read image roundtrip completed"
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
    workspace-multiturn) run_case "$scenario" case_workspace_multiturn ;;
    workspace-long-tools) run_case "$scenario" case_workspace_long_tools ;;
    workspace-repo-loop) run_case "$scenario" case_workspace_repo_loop ;;
    workspace-error-recovery) run_case "$scenario" case_workspace_error_recovery ;;
    workspace-parallel-tools) run_case "$scenario" case_workspace_parallel_tools ;;
    permission-plan) run_case "$scenario" case_permission_plan ;;
    structured-output) run_case "$scenario" case_structured_output ;;
    mcp-large-result) run_case "$scenario" case_mcp_large_result ;;
    mcp-error-recovery) run_case "$scenario" case_mcp_error_recovery ;;
    web-search) run_case "$scenario" case_web_search ;;
    workspace-image) run_case "$scenario" case_workspace_image ;;
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
