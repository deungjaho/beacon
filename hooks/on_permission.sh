#!/usr/bin/env bash
# Codex PermissionRequest hook. Hook failures are intentionally non-fatal.
set -uo pipefail

BEACON_ROOT="${BEACON_ROOT:-$(cd "$(dirname "$0")/.." && pwd)}"
LOG_FILE="${BEACON_LOG_FILE:-$HOME/.local/share/beacon/hook-errors.log}"
input=$(cat 2>/dev/null || true)
tool=$(jq -r '.tool_name // .tool // "operation"' <<<"$input" 2>/dev/null || true)
cwd=$(jq -r '.cwd // ""' <<<"$input" 2>/dev/null || true)
message="Permission required: $tool"
if ! "$BEACON_ROOT/bin/beacon" report waiting "$message" "$cwd" >/dev/null 2>&1; then
  mkdir -p "$(dirname "$LOG_FILE")"
  echo "$(date -Iseconds) permission report-failed cwd=$cwd" >> "$LOG_FILE" 2>/dev/null || true
fi
"$BEACON_ROOT/lib/notify.sh" "${BEACON_AGENT_NAME:-Codex}" "$message" 2>/dev/null || true
exit 0
