#!/usr/bin/env bash
set -Eeuo pipefail

die() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

command -v go >/dev/null 2>&1 || die "go is required"
command -v claude >/dev/null 2>&1 || die "Claude Code is required"
command -v timeout >/dev/null 2>&1 || die "timeout is required"
command -v rg >/dev/null 2>&1 || die "ripgrep (rg) is required"
[[ -n "${KIRO_DEV_API_KEY:-}" ]] || die "KIRO_DEV_API_KEY is required"
[[ $# -eq 0 ]] || die "client-e2e accepts configuration through KIRO_DEV_* environment variables"

BASE_URL="${KIRO_DEV_BASE_URL:-http://127.0.0.1:8080}"
case "$BASE_URL" in
  http://127.0.0.1:*|https://127.0.0.1:*|http://localhost:*|https://localhost:*) ;;
  *) [[ "${KIRO_DEV_ALLOW_REMOTE:-}" == "1" ]] || die "set KIRO_DEV_ALLOW_REMOTE=1 to test a non-loopback URL" ;;
esac

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

FIXTURE_BIN="$TMP_DIR/mcpfixture"
MCP_CONFIG="$TMP_DIR/mcp.json"
AUDIT_PATH="$TMP_DIR/mcp-audit.log"
OUTPUT_PATH="$TMP_DIR/client-output.jsonl"
SKILL_DIR="$TMP_DIR/workspace/.claude/skills/kiro-devcheck"
mkdir -p "$SKILL_DIR"

(cd "$PROJECT_DIR" && go build -o "$FIXTURE_BIN" ./cmd/mcpfixture)
printf '{"mcpServers":{"devcheck":{"type":"stdio","command":"%s","env":{"KIRO_MCP_FIXTURE_AUDIT":"%s"}}}}\n' \
  "$FIXTURE_BIN" "$AUDIT_PATH" >"$MCP_CONFIG"
printf '%s\n' \
  '---' \
  'name: kiro-devcheck' \
  'description: Verify Kiro-Go Plus Skill and MCP transport.' \
  '---' \
  'Call mcp__devcheck__devcheck_echo exactly once with value MCP_CLIENT_E2E_OK.' \
  'After the tool returns, reply exactly MCP_CLIENT_E2E_OK.' >"$SKILL_DIR/SKILL.md"

(
  cd "$TMP_DIR/workspace"
  ANTHROPIC_BASE_URL="$BASE_URL" ANTHROPIC_API_KEY="$KIRO_DEV_API_KEY" \
    timeout "${KIRO_DEV_CLIENT_TIMEOUT:-5m}" claude --bare --print --verbose \
      --setting-sources project --add-dir "$TMP_DIR/workspace" \
      --model "${KIRO_DEV_MODEL:-claude-sonnet-5}" \
      --mcp-config "$MCP_CONFIG" --strict-mcp-config \
      --allowedTools "mcp__devcheck__devcheck_echo" --tools "" \
      --no-session-persistence --max-budget-usd "${KIRO_DEV_MAX_BUDGET_USD:-0.10}" \
      --output-format stream-json '/kiro-devcheck' >"$OUTPUT_PATH"
)

[[ -s "$AUDIT_PATH" ]] || die "Claude Code did not execute the MCP fixture tool"
rg -q '"result":"MCP_CLIENT_E2E_OK"' "$OUTPUT_PATH" || die "client did not complete the tool-result roundtrip"
printf 'client E2E passed: Skill discovered and MCP tool executed through %s\n' "$BASE_URL"
