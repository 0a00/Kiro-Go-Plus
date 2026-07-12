#!/usr/bin/env bash
set -Eeuo pipefail

usage() {
  cat <<'EOF'
Usage:
  scripts/update-docker-compose.sh --target /path/to/old/Kiro-Go [options]

Options:
  --target DIR        Existing production Kiro-Go directory to update.
  --archive FILE     Optional Kiro-Go-update.tar.gz archive. If omitted, the
                     script uses the current project directory as the source.
  --no-build         Copy files and backups only; skip docker compose build/up.
  --no-prune         Do not delete stale files in the target.
  --keep-compose     Preserve target docker-compose.yml / compose.yaml.
  --service NAME     Compose service name (default: kiro-go).
  --health-timeout N Seconds to wait for the updated service (default: 120).
  --readiness-path P HTTP path checked inside the container (default: /health).
  --yes              Non-interactive mode; do not ask for confirmation.
  -h, --help         Show this help.

What is preserved:
  - target data/ directory
  - target data/config.json
  - target .env* deployment files
  - backups are written to target/.update-backups/<timestamp>/
EOF
}

die() {
  echo "ERROR: $*" >&2
  exit 1
}

info() {
  echo "[update] $*"
}

TARGET_DIR=""
ARCHIVE=""
DO_BUILD=1
DO_PRUNE=1
KEEP_COMPOSE=0
ASSUME_YES=0
SERVICE_NAME="kiro-go"
HEALTH_TIMEOUT=120
READINESS_PATH="/health"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --target)
      TARGET_DIR="${2:-}"
      shift 2
      ;;
    --archive)
      ARCHIVE="${2:-}"
      shift 2
      ;;
    --no-build)
      DO_BUILD=0
      shift
      ;;
    --no-prune)
      DO_PRUNE=0
      shift
      ;;
    --keep-compose)
      KEEP_COMPOSE=1
      shift
      ;;
    --yes)
      ASSUME_YES=1
      shift
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
    -h|--help)
      usage
      exit 0
      ;;
    *)
      die "unknown argument: $1"
      ;;
  esac
done

[[ -n "$TARGET_DIR" ]] || { usage; die "--target is required"; }
[[ -d "$TARGET_DIR" ]] || die "target directory does not exist: $TARGET_DIR"
[[ -n "$SERVICE_NAME" ]] || die "--service must not be empty"
[[ "$HEALTH_TIMEOUT" =~ ^[0-9]+$ && "$HEALTH_TIMEOUT" -ge 10 ]] || die "--health-timeout must be an integer >= 10"
[[ "$READINESS_PATH" == /* ]] || die "--readiness-path must start with /"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CURRENT_PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
TARGET_DIR="$(cd "$TARGET_DIR" && pwd)"

command -v rsync >/dev/null 2>&1 || die "rsync is required"
command -v tar >/dev/null 2>&1 || die "tar is required"

TMP_DIR=""
cleanup() {
  if [[ -n "$TMP_DIR" && -d "$TMP_DIR" ]]; then
    rm -rf "$TMP_DIR"
  fi
}
trap cleanup EXIT

SOURCE_DIR="$CURRENT_PROJECT_DIR"
if [[ -n "$ARCHIVE" ]]; then
  [[ -f "$ARCHIVE" ]] || die "archive does not exist: $ARCHIVE"
  TMP_DIR="$(mktemp -d)"
  info "extracting archive: $ARCHIVE"
  tar -xzf "$ARCHIVE" -C "$TMP_DIR"
  if [[ -f "$TMP_DIR/go.mod" ]]; then
    SOURCE_DIR="$TMP_DIR"
  elif [[ -d "$TMP_DIR/Kiro-Go" ]]; then
    SOURCE_DIR="$TMP_DIR/Kiro-Go"
  else
    first_dir="$(find "$TMP_DIR" -mindepth 1 -maxdepth 2 -type f -name go.mod -printf '%h\n' | head -n 1)"
    [[ -n "$first_dir" ]] || die "archive does not contain a project directory"
    SOURCE_DIR="$first_dir"
  fi
fi

[[ -f "$SOURCE_DIR/go.mod" ]] || die "source does not look like Kiro-Go: missing go.mod"
[[ -f "$SOURCE_DIR/Dockerfile" ]] || die "source does not look like Kiro-Go: missing Dockerfile"
[[ -d "$SOURCE_DIR/web" ]] || die "source does not look like Kiro-Go: missing web/"
[[ "$SOURCE_DIR" != "$TARGET_DIR" ]] || die "source and target are the same directory"

COMPOSE_CMD=()
if docker compose version >/dev/null 2>&1; then
  COMPOSE_CMD=(docker compose)
elif command -v docker-compose >/dev/null 2>&1; then
  COMPOSE_CMD=(docker-compose)
else
  [[ "$DO_BUILD" -eq 0 ]] || die "docker compose or docker-compose is required"
fi

if [[ "$DO_BUILD" -eq 1 ]]; then
  command -v docker >/dev/null 2>&1 || die "docker is required"
fi

timestamp="$(date +%Y%m%d-%H%M%S)"
BACKUP_DIR="$TARGET_DIR/.update-backups/$timestamp"
UPDATE_STARTED=0
UPDATE_COMPLETE=0

backup_runtime_data() {
  mkdir -p "$BACKUP_DIR/data"
  local name
  for name in config.json runtime_state.json; do
    if [[ -f "$TARGET_DIR/data/$name" ]]; then
      cp -a "$TARGET_DIR/data/$name" "$BACKUP_DIR/data/$name"
    fi
  done
}

wait_for_service() {
  local deadline=$((SECONDS + HEALTH_TIMEOUT))
  local container_id=""
  local state=""
  info "waiting up to ${HEALTH_TIMEOUT}s for ${SERVICE_NAME}${READINESS_PATH}"
  while (( SECONDS < deadline )); do
    container_id="$(cd "$TARGET_DIR" && "${COMPOSE_CMD[@]}" ps -q "$SERVICE_NAME" 2>/dev/null || true)"
    if [[ -n "$container_id" ]]; then
      state="$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$container_id" 2>/dev/null || true)"
      if [[ "$state" == "healthy" || "$state" == "running" ]]; then
        if (cd "$TARGET_DIR" && "${COMPOSE_CMD[@]}" exec -T "$SERVICE_NAME" \
          sh -c 'wget -q -O /dev/null "http://127.0.0.1:${HEALTHCHECK_PORT:-8080}$1"' sh "$READINESS_PATH") >/dev/null 2>&1; then
          info "service check passed (${state})"
          return 0
        fi
      fi
    fi
    sleep 2
  done
  info "service did not become ready; recent logs follow"
  (cd "$TARGET_DIR" && "${COMPOSE_CMD[@]}" logs --tail 100 "$SERVICE_NAME") >&2 || true
  return 1
}

rollback_update() {
  local failed=0
  info "update failed; restoring backup"
  if [[ "$DO_BUILD" -eq 1 ]]; then
    (cd "$TARGET_DIR" && "${COMPOSE_CMD[@]}" stop -t 35 "$SERVICE_NAME") >/dev/null 2>&1 || true
  fi
  if ! rsync -a --delete \
    --exclude '/data/' \
    --exclude '/.update-backups/' \
    "$BACKUP_DIR/project/" "$TARGET_DIR/"; then
    failed=1
  fi
  local name
  for name in config.json runtime_state.json; do
    if [[ -f "$BACKUP_DIR/data/$name" ]]; then
      mkdir -p "$TARGET_DIR/data"
      if ! cp -a "$BACKUP_DIR/data/$name" "$TARGET_DIR/data/$name"; then
        failed=1
      fi
    fi
  done
  if [[ "$DO_BUILD" -eq 1 ]]; then
    info "rebuilding and restarting previous version"
    if ! (cd "$TARGET_DIR" && "${COMPOSE_CMD[@]}" build); then
      failed=1
    elif ! (cd "$TARGET_DIR" && "${COMPOSE_CMD[@]}" up -d); then
      failed=1
    elif ! wait_for_service; then
      failed=1
    fi
  fi
  if [[ "$failed" -ne 0 ]]; then
    return 1
  fi
  info "rollback completed"
}

on_error() {
  local code=$?
  trap - ERR
  if [[ "$UPDATE_STARTED" -eq 1 && "$UPDATE_COMPLETE" -eq 0 ]]; then
    set +e
    rollback_update
    local rollback_code=$?
    set -e
    if [[ "$rollback_code" -ne 0 ]]; then
      echo "ERROR: automatic rollback failed; backup is at $BACKUP_DIR" >&2
    fi
  fi
  exit "$code"
}

trap on_error ERR

info "source: $SOURCE_DIR"
info "target: $TARGET_DIR"
info "backup: $BACKUP_DIR"

if [[ "$ASSUME_YES" -ne 1 ]]; then
  echo
  echo "This will update target code while preserving target data/."
  read -r -p "Continue? [y/N] " answer
  case "$answer" in
    y|Y|yes|YES) ;;
    *) die "cancelled" ;;
  esac
fi

mkdir -p "$BACKUP_DIR/project"
info "backing up current project files (persistent data remains in place)"
rsync -a \
  --exclude '/data/' \
  --exclude '/.update-backups/' \
  "$TARGET_DIR/" "$BACKUP_DIR/project/"
backup_runtime_data

RSYNC_ARGS=(-a)
if [[ "$DO_PRUNE" -eq 1 ]]; then
  RSYNC_ARGS+=(--delete)
fi
RSYNC_ARGS+=(
  --exclude '/data/'
  --exclude '/.git/'
  --exclude '/.update-backups/'
  --include '/.env.example'
  --exclude '/.env*'
  --exclude '*.log'
)
if [[ "$KEEP_COMPOSE" -eq 1 ]]; then
  RSYNC_ARGS+=(--exclude docker-compose.yml --exclude docker-compose.yaml --exclude compose.yml --exclude compose.yaml)
fi

info "copying source files"
UPDATE_STARTED=1
rsync "${RSYNC_ARGS[@]}" "$SOURCE_DIR/" "$TARGET_DIR/"

if [[ -d "$BACKUP_DIR/project/data" ]]; then
  mkdir -p "$TARGET_DIR/data"
fi

if [[ -f "$TARGET_DIR/data/config.json" ]]; then
  chmod 600 "$TARGET_DIR/data/config.json" || true
fi

if [[ "$DO_BUILD" -eq 1 ]]; then
  # Match the non-root container user to the deployment account that owns data/.
  host_uid="$(id -u)"
  host_gid="$(id -g)"
  if [[ "$host_uid" -eq 0 ]]; then
    host_uid=1000
    host_gid=1000
  fi
  export PUID="${PUID:-$host_uid}"
  export PGID="${PGID:-$host_gid}"
  [[ "$PUID" =~ ^[0-9]+$ && "$PGID" =~ ^[0-9]+$ ]] || die "PUID and PGID must be numeric"
  [[ "$PUID" -ne 0 && "$PGID" -ne 0 ]] || die "PUID and PGID must be non-zero"
  info "container user: $PUID:$PGID"

  info "validating docker compose configuration"
  (cd "$TARGET_DIR" && "${COMPOSE_CMD[@]}" config --quiet)

  info "building docker image"
  (cd "$TARGET_DIR" && "${COMPOSE_CMD[@]}" build --no-cache)

  # Capture the latest persisted credentials immediately before replacing the
  # running container so rollback does not discard refreshes performed during build.
  backup_runtime_data

  info "starting service"
  (cd "$TARGET_DIR" && "${COMPOSE_CMD[@]}" up -d)

  wait_for_service

  info "service status"
  (cd "$TARGET_DIR" && "${COMPOSE_CMD[@]}" ps)
else
  info "skipped docker compose build/up"
fi

UPDATE_COMPLETE=1
trap - ERR
info "done"
info "rollback backup is at: $BACKUP_DIR"
