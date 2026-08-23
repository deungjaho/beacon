#!/usr/bin/env bash
# Claude Code Notification hook. Hook failures are intentionally non-fatal.
set -uo pipefail

BEACON_ROOT="${BEACON_ROOT:-$(cd "$(dirname "$0")/.." && pwd)}"
LOG_FILE="${BEACON_LOG_FILE:-$HOME/.local/share/beacon/hook-errors.log}"
input=$(cat 2>/dev/null || true)
message=$(jq -r '.message // .notification // "Agent is waiting"' <<<"$input" 2>/dev/null || true)
cwd=$(jq -r '.cwd // ""' <<<"$input" 2>/dev/null || true)
if ! "$BEACON_ROOT/bin/beacon" report waiting "$message" "$cwd" >/dev/null 2>&1; then
  mkdir -p "$(dirname "$LOG_FILE")"
  echo "$(date -Iseconds) notification report-failed cwd=$cwd" >> "$LOG_FILE" 2>/dev/null || true
fi
"$BEACON_ROOT/lib/notify.sh" "${BEACON_AGENT_NAME:-Claude}" "⚠ $message" 2>/dev/null || true
exit 0
