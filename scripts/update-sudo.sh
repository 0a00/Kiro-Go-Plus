#!/usr/bin/env bash
set -Eeuo pipefail

usage() {
  cat <<'EOF'
Usage:
  bash scripts/update-sudo.sh [options]

Options:
  --remote NAME       Git remote to fetch (default: origin).
  --branch NAME       Branch to update (default: current branch).
  --service NAME      Compose service name (default: kiro-go).
  --health-timeout N  Seconds to wait for health (default: 180).
  --readiness-path P  In-container HTTP path (default: /health).
  --no-pull           Do not pull newer Docker base images while building.
  -h, --help          Show this help.

The script updates the current Git checkout with a fast-forward merge, runs
Docker Compose through sudo, preserves data/ and .env files, and automatically
rolls back the code and container if build, startup, or health checks fail.
EOF
}

info() {
  printf '[update] %s\n' "$*"
}

warn() {
  printf 'WARNING: %s\n' "$*" >&2
}

die() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

REMOTE="${UPDATE_REMOTE:-origin}"
BRANCH="${UPDATE_BRANCH:-}"
SERVICE_NAME="${UPDATE_SERVICE:-kiro-go}"
HEALTH_TIMEOUT="${UPDATE_HEALTH_TIMEOUT:-180}"
READINESS_PATH="${UPDATE_READINESS_PATH:-/health}"
PULL_BASE_IMAGES=1

while [[ $# -gt 0 ]]; do
  case "$1" in
    --remote)
      REMOTE="${2:-}"
      shift 2
      ;;
    --branch)
      BRANCH="${2:-}"
      shift 2
      ;;
    --service)
      SERVICE_NAME="${2:-}"
      shift 2
      ;;
    --health-timeout)
      HEALTH_TIMEOUT="${2:-}"
      shift 2
      ;;
    --readiness-path)
      READINESS_PATH="${2:-}"
      shift 2
      ;;
    --no-pull)
      PULL_BASE_IMAGES=0
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      die "unknown argument: $1"
      ;;
  esac
done

[[ -n "$REMOTE" ]] || die "--remote must not be empty"
[[ -n "$SERVICE_NAME" ]] || die "--service must not be empty"
[[ "$HEALTH_TIMEOUT" =~ ^[0-9]+$ && "$HEALTH_TIMEOUT" -ge 10 ]] || die "--health-timeout must be an integer >= 10"
[[ "$READINESS_PATH" == /* ]] || die "--readiness-path must start with /"

command -v git >/dev/null 2>&1 || die "git is required"

if [[ "${EUID:-$(id -u)}" -eq 0 && -n "${SUDO_USER:-}" && "${SUDO_USER}" != "root" ]]; then
  die "do not run the whole script with sudo; run: bash scripts/update-sudo.sh"
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(git -C "$SCRIPT_DIR/.." rev-parse --show-toplevel 2>/dev/null)" || die "script must run from a Git checkout"
cd "$PROJECT_DIR"

[[ -w "$PROJECT_DIR/.git" ]] || die "the current user cannot update .git; fix repository ownership instead of running git through sudo"
current_branch="$(git symbolic-ref --quiet --short HEAD 2>/dev/null)" || die "detached HEAD is not supported"
if [[ -z "$BRANCH" ]]; then
  BRANCH="$current_branch"
fi
[[ "$current_branch" == "$BRANCH" ]] || die "current branch is $current_branch, expected $BRANCH"
git remote get-url "$REMOTE" >/dev/null 2>&1 || die "Git remote not found: $REMOTE"

if ! git diff --quiet || ! git diff --cached --quiet; then
  die "tracked files have local changes; commit, stash, or restore them before updating"
fi

SUDO=()
SUDO_KEEPALIVE_PID=""
if [[ "${EUID:-$(id -u)}" -ne 0 ]]; then
  command -v sudo >/dev/null 2>&1 || die "sudo is required"
  info "requesting sudo access for Docker Compose"
  sudo -v
  SUDO=(sudo)
  (
    while true; do
      sudo -n true >/dev/null 2>&1 || exit
      sleep 50
    done
  ) &
  SUDO_KEEPALIVE_PID=$!
fi

cleanup() {
  if [[ -n "$SUDO_KEEPALIVE_PID" ]]; then
    kill "$SUDO_KEEPALIVE_PID" >/dev/null 2>&1 || true
    wait "$SUDO_KEEPALIVE_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

DOCKER=("${SUDO[@]}" docker)
COMPOSE=()
if "${DOCKER[@]}" compose version >/dev/null 2>&1; then
  COMPOSE=("${DOCKER[@]}" compose)
elif command -v docker-compose >/dev/null 2>&1; then
  COMPOSE=("${SUDO[@]}" docker-compose)
else
  die "docker compose or docker-compose is required"
fi

wait_for_service() {
  local deadline=$((SECONDS + HEALTH_TIMEOUT))
  local container_id=""
  local state=""
  info "waiting up to ${HEALTH_TIMEOUT}s for ${SERVICE_NAME}${READINESS_PATH}"
  while (( SECONDS < deadline )); do
    container_id="$("${COMPOSE[@]}" ps -q "$SERVICE_NAME" 2>/dev/null || true)"
    if [[ -n "$container_id" ]]; then
      state="$("${DOCKER[@]}" inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$container_id" 2>/dev/null || true)"
      if [[ "$state" == "healthy" || "$state" == "running" ]]; then
        if "${COMPOSE[@]}" exec -T "$SERVICE_NAME" \
          sh -c 'wget -q -O /dev/null "http://127.0.0.1:${HEALTHCHECK_PORT:-8080}$1"' sh "$READINESS_PATH" >/dev/null 2>&1; then
          info "service check passed (${state})"
          return 0
        fi
      fi
    fi
    sleep 2
  done
  warn "service did not become ready; recent logs follow"
  "${COMPOSE[@]}" logs --tail 100 "$SERVICE_NAME" >&2 || true
  return 1
}

timestamp="$(date +%Y%m%d-%H%M%S)"
BACKUP_DIR="$PROJECT_DIR/.update-backups/self-$timestamp"
OLD_COMMIT="$(git rev-parse HEAD)"
TARGET_COMMIT=""
UPDATE_APPLIED=0
DEPLOY_STARTED=0

backup_runtime_files() {
  mkdir -p "$BACKUP_DIR/data"
  chmod 700 "$BACKUP_DIR" "$BACKUP_DIR/data"
  printf '%s\n' "$OLD_COMMIT" >"$BACKUP_DIR/previous-commit.txt"
  local path=""
  for path in data/config.json data/runtime_state.json data/master.key; do
    if [[ -f "$PROJECT_DIR/$path" ]]; then
      mkdir -p "$BACKUP_DIR/$(dirname "$path")"
      cp -a "$PROJECT_DIR/$path" "$BACKUP_DIR/$path"
    fi
  done
  local env_file=""
  shopt -s nullglob
  for env_file in "$PROJECT_DIR"/.env "$PROJECT_DIR"/.env.*; do
    if [[ "$(basename "$env_file")" == ".env.example" ]]; then
      continue
    fi
    cp -a "$env_file" "$BACKUP_DIR/$(basename "$env_file")"
  done
  shopt -u nullglob
}

restore_previous_code() {
  if [[ "$UPDATE_APPLIED" -ne 1 ]]; then
    return 0
  fi
  local current_commit=""
  current_commit="$(git rev-parse HEAD 2>/dev/null || true)"
  if [[ "$current_commit" != "$TARGET_COMMIT" ]]; then
    warn "HEAD changed unexpectedly; automatic Git rollback was skipped"
    return 1
  fi
  info "restoring Git checkout to $OLD_COMMIT"
  git update-ref "refs/heads/$BRANCH" "$OLD_COMMIT" "$TARGET_COMMIT"
  git restore --source="$OLD_COMMIT" --staged --worktree -- .
  UPDATE_APPLIED=0
}

rollback_update() {
  local failed=0
  warn "update failed; rolling back to the previous version"
  if ! restore_previous_code; then
    failed=1
  fi
  if [[ "$failed" -eq 0 ]]; then
    info "rebuilding previous Docker image"
    if ! "${COMPOSE[@]}" config --quiet; then
      failed=1
    elif ! "${COMPOSE[@]}" build; then
      failed=1
    elif ! "${COMPOSE[@]}" up -d --remove-orphans; then
      failed=1
    elif ! wait_for_service; then
      failed=1
    fi
  fi
  if [[ "$failed" -ne 0 ]]; then
    warn "automatic rollback did not complete; manual backup: $BACKUP_DIR"
    return 1
  fi
  info "rollback completed"
}

on_error() {
  local code=$?
  trap - ERR
  set +e
  local rollback_code=0
  if [[ "$UPDATE_APPLIED" -eq 1 || "$DEPLOY_STARTED" -eq 1 ]]; then
    rollback_update
    rollback_code=$?
  fi
  set -e
  if [[ "$rollback_code" -ne 0 ]]; then
    warn "manual recovery may be required"
  fi
  exit "$code"
}
trap on_error ERR

info "project: $PROJECT_DIR"
info "remote branch: $REMOTE/$BRANCH"
info "current commit: $OLD_COMMIT"
backup_runtime_files
info "backup: $BACKUP_DIR"

info "fetching latest source"
git fetch --prune "$REMOTE" "$BRANCH"
TARGET_COMMIT="$(git rev-parse FETCH_HEAD)"
git merge-base --is-ancestor "$OLD_COMMIT" "$TARGET_COMMIT" || die "$REMOTE/$BRANCH is not a fast-forward update"

if [[ "$TARGET_COMMIT" != "$OLD_COMMIT" ]]; then
  info "updating checkout to $TARGET_COMMIT"
  git merge --ff-only "$TARGET_COMMIT"
  UPDATE_APPLIED=1
else
  info "source is already up to date; rebuilding the current version"
fi

info "validating Docker Compose configuration"
"${COMPOSE[@]}" config --quiet

info "building Docker image"
BUILD_ARGS=()
if [[ "$PULL_BASE_IMAGES" -eq 1 ]]; then
  BUILD_ARGS+=(--pull)
fi
"${COMPOSE[@]}" build "${BUILD_ARGS[@]}"

info "starting updated service"
DEPLOY_STARTED=1
"${COMPOSE[@]}" up -d --remove-orphans
wait_for_service

info "service status"
"${COMPOSE[@]}" ps

trap - ERR
info "update completed: $(git rev-parse --short HEAD)"
info "rollback backup: $BACKUP_DIR"
