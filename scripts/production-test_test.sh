#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEST_SCRIPT="$SCRIPT_DIR/production-test.sh"
TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/kiro-production-test-regression.XXXXXX")"
trap 'rm -rf -- "$TMP_DIR"' EXIT

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

assert_fails() {
  local label="$1"
  shift
  local output status
  set +e
  output="$("$@" 2>&1)"
  status=$?
  set -e
  if ((status == 0)); then
    printf '%s\n' "$output" >&2
    fail "$label unexpectedly succeeded"
  fi
}

FAKE_BIN="$TMP_DIR/bin"
NETWORK_MARKER="$TMP_DIR/network-called"
mkdir -p "$FAKE_BIN"
printf '%s\n' '#!/usr/bin/env bash' "printf 'network-called\n' >> '$NETWORK_MARKER'" 'exit 99' >"$FAKE_BIN/curl"
chmod 700 "$FAKE_BIN/curl"

clean_env=(
  env
  -u KIRO_PROD_API_KEY
  -u KIRO_PROD_BASE_URL
  -u KIRO_PROD_MODEL
  -u KIRO_PROD_THINKING_MODEL
  -u KIRO_PROD_MODELS
  -u KIRO_PROD_WEB_SEARCH
  -u KIRO_PROD_REPORT_DIR
  -u KIRO_PROD_FAIL_ON_WARNING
  -u KIRO_PROD_ALLOW_REMOTE
  -u KIRO_PROD_CONFIRM
  -u KIRO_DEV_API_KEY
  -u KIRO_DEV_BASE_URL
  -u KIRO_DEV_ALLOW_REMOTE
  -u KIRO_DEV_MODEL
  -u KIRO_DEV_THINKING_MODEL
  -u KIRO_DEV_MODELS
  PATH="$FAKE_BIN:$PATH"
)

"${clean_env[@]}" bash "$TEST_SCRIPT" --dry-run --skip-web-search --skip-matrix \
  --skip-load --skip-client-e2e --staircase --soak --fail-on-warning >/dev/null
[[ ! -e "$NETWORK_MARKER" ]] || fail "dry-run invoked curl"

assert_fails "remote dry-run without opt-in" \
  env "PATH=$FAKE_BIN:$PATH" KIRO_PROD_API_KEY=test-key \
  bash "$TEST_SCRIPT" --base-url http://remote.invalid --dry-run
[[ ! -e "$NETWORK_MARKER" ]] || fail "remote guard invoked curl"

assert_fails "real run without confirmation" \
  env "PATH=$FAKE_BIN:$PATH" KIRO_PROD_API_KEY=test-key \
  bash "$TEST_SCRIPT" --skip-client-e2e --skip-matrix --skip-load

assert_fails "real run without API key" \
  "${clean_env[@]}" bash "$TEST_SCRIPT" --confirm-production --skip-client-e2e --skip-matrix --skip-load

assert_fails "invalid remote flag" \
  "${clean_env[@]}" KIRO_PROD_ALLOW_REMOTE=2 bash "$TEST_SCRIPT" --dry-run

assert_fails "zero phase timeout" \
  "${clean_env[@]}" bash "$TEST_SCRIPT" --phase-timeout 0s --dry-run

"${clean_env[@]}" bash "$TEST_SCRIPT" --base-url http://remote.invalid --allow-remote \
  --dry-run --skip-web-search --skip-matrix --skip-load --skip-client-e2e >/dev/null
[[ ! -e "$NETWORK_MARKER" ]] || fail "opted-in dry-run invoked curl"

CLIENT_FAKE_BIN="$TMP_DIR/client-bin"
mkdir -p "$CLIENT_FAKE_BIN"
printf '%s\n' \
  '#!/usr/bin/env bash' \
  'set -Eeuo pipefail' \
  'args=("$@")' \
  '(( ${#args[@]} >= 2 )) || exit 42' \
  'last=$((${#args[@]} - 1))' \
  'separator=$((${#args[@]} - 2))' \
  '[[ "${args[$separator]}" == "--" ]] || exit 42' \
  '[[ "${args[$last]}" == "Reply exactly CLIENT_TEXT_STREAM_OK." ]] || exit 42' \
  'printf "%s\\n" '\''{"type":"result","is_error":false,"result":"CLIENT_TEXT_STREAM_OK"}'\''' \
  >"$CLIENT_FAKE_BIN/claude"
chmod 700 "$CLIENT_FAKE_BIN/claude"
env KIRO_DEV_API_KEY=test-key KIRO_DEV_BASE_URL=http://127.0.0.1:8080 \
  PATH="$CLIENT_FAKE_BIN:$PATH" bash "$SCRIPT_DIR/client-e2e.sh" \
  --scenarios text-stream --timeout 1s >/dev/null

printf 'production-test regression checks passed\n'
