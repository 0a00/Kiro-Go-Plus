#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
UPDATE_SCRIPT="$SCRIPT_DIR/update-sudo.sh"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

FAKE_BIN="$TMP_DIR/bin"
mkdir -p "$FAKE_BIN"

cat >"$FAKE_BIN/sudo" <<'EOF'
#!/usr/bin/env bash
set -e
case "${1:-}" in
  -v)
    exit 0
    ;;
  -n)
    shift
    exec "$@"
    ;;
  *)
    exec "$@"
    ;;
esac
EOF

cat >"$FAKE_BIN/docker" <<'EOF'
#!/usr/bin/env bash
set -e
printf '%s\n' "$*" >>"$FAKE_DOCKER_LOG"
if [[ "${1:-}" == "inspect" ]]; then
  printf 'healthy\n'
  exit 0
fi
[[ "${1:-}" == "compose" ]] || exit 1
shift
command="${1:-}"
shift || true
case "$command" in
  version|config|up|exec|logs)
    exit 0
    ;;
  build)
    if [[ -n "${FAKE_DOCKER_FAIL_ONCE:-}" && -f "$FAKE_DOCKER_FAIL_ONCE" ]]; then
      rm -f "$FAKE_DOCKER_FAIL_ONCE"
      exit 1
    fi
    exit 0
    ;;
  ps)
    if [[ "${1:-}" == "-q" ]]; then
      printf 'test-container\n'
    else
      printf 'kiro-go running\n'
    fi
    exit 0
    ;;
esac
exit 1
EOF

chmod +x "$FAKE_BIN/sudo" "$FAKE_BIN/docker"

SOURCE_REPO="$TMP_DIR/source"
REMOTE_REPO="$TMP_DIR/remote.git"
SUCCESS_REPO="$TMP_DIR/deploy-success"
FAILURE_REPO="$TMP_DIR/deploy-failure"

git init -q -b main "$SOURCE_REPO"
git -C "$SOURCE_REPO" config user.name "Update Test"
git -C "$SOURCE_REPO" config user.email "update-test@example.invalid"
mkdir -p "$SOURCE_REPO/scripts"
cp "$UPDATE_SCRIPT" "$SOURCE_REPO/scripts/update-sudo.sh"
chmod +x "$SOURCE_REPO/scripts/update-sudo.sh"
printf 'old\n' >"$SOURCE_REPO/app.txt"
printf 'services:\n  kiro-go:\n    image: test\n' >"$SOURCE_REPO/docker-compose.yml"
git -C "$SOURCE_REPO" add .
git -C "$SOURCE_REPO" commit -q -m "base"
BASE_COMMIT="$(git -C "$SOURCE_REPO" rev-parse HEAD)"

git clone -q --bare "$SOURCE_REPO" "$REMOTE_REPO"
git clone -q "$REMOTE_REPO" "$SUCCESS_REPO"
git clone -q "$REMOTE_REPO" "$FAILURE_REPO"

for repo in "$SUCCESS_REPO" "$FAILURE_REPO"; do
  mkdir -p "$repo/data"
  printf 'ADMIN_PASSWORD=test-only\n' >"$repo/.env"
  printf '{"preserved":true}\n' >"$repo/data/config.json"
done

printf 'new\n' >"$SOURCE_REPO/app.txt"
git -C "$SOURCE_REPO" add app.txt
git -C "$SOURCE_REPO" commit -q -m "update"
git -C "$SOURCE_REPO" remote add origin "$REMOTE_REPO"
git -C "$SOURCE_REPO" push -q origin main
TARGET_COMMIT="$(git -C "$SOURCE_REPO" rev-parse HEAD)"

SUCCESS_LOG="$TMP_DIR/docker-success.log"
if ! PATH="$FAKE_BIN:$PATH" FAKE_DOCKER_LOG="$SUCCESS_LOG" \
  bash "$SUCCESS_REPO/scripts/update-sudo.sh" --health-timeout 10 --no-pull >"$TMP_DIR/success.log" 2>&1; then
  cat "$TMP_DIR/success.log" >&2
  fail "successful update returned an error"
fi
[[ "$(git -C "$SUCCESS_REPO" rev-parse HEAD)" == "$TARGET_COMMIT" ]] || fail "successful update did not advance HEAD"
[[ "$(cat "$SUCCESS_REPO/app.txt")" == "new" ]] || fail "successful update did not replace source"
[[ "$(cat "$SUCCESS_REPO/.env")" == "ADMIN_PASSWORD=test-only" ]] || fail ".env was not preserved"
[[ "$(cat "$SUCCESS_REPO/data/config.json")" == '{"preserved":true}' ]] || fail "data/config.json was not preserved"
grep -q '^compose build' "$SUCCESS_LOG" || fail "Docker build was not called"
grep -q '^compose up -d --remove-orphans' "$SUCCESS_LOG" || fail "Docker Compose up was not called"

FAILURE_LOG="$TMP_DIR/docker-failure.log"
FAIL_ONCE="$TMP_DIR/fail-build-once"
touch "$FAIL_ONCE"
if PATH="$FAKE_BIN:$PATH" FAKE_DOCKER_LOG="$FAILURE_LOG" FAKE_DOCKER_FAIL_ONCE="$FAIL_ONCE" \
  bash "$FAILURE_REPO/scripts/update-sudo.sh" --health-timeout 10 --no-pull >"$TMP_DIR/failure.log" 2>&1; then
  cat "$TMP_DIR/failure.log" >&2
  fail "failed build unexpectedly returned success"
fi
[[ "$(git -C "$FAILURE_REPO" rev-parse HEAD)" == "$BASE_COMMIT" ]] || fail "rollback did not restore HEAD"
[[ "$(cat "$FAILURE_REPO/app.txt")" == "old" ]] || fail "rollback did not restore source"
[[ "$(cat "$FAILURE_REPO/.env")" == "ADMIN_PASSWORD=test-only" ]] || fail "rollback changed .env"
[[ "$(cat "$FAILURE_REPO/data/config.json")" == '{"preserved":true}' ]] || fail "rollback changed data/config.json"
[[ "$(grep -c '^compose build' "$FAILURE_LOG")" -eq 2 ]] || fail "rollback did not rebuild the previous image"

printf 'update-sudo tests passed\n'
