#!/usr/bin/env bash
# Claude Code UserPromptSubmit hook. Hook failures are intentionally non-fatal.
set -uo pipefail

BEACON_ROOT="${BEACON_ROOT:-$(cd "$(dirname "$0")/.." && pwd)}"
LOG_FILE="${BEACON_LOG_FILE:-$HOME/.local/share/beacon/hook-errors.log}"
input=$(cat 2>/dev/null || true)
prompt=$(jq -r '.prompt // .user_prompt // .input // ""' <<<"$input" 2>/dev/null || true)
cwd=$(jq -r '.cwd // ""' <<<"$input" 2>/dev/null || true)
if ! "$BEACON_ROOT/bin/beacon" report working "$prompt" "$cwd" >/dev/null 2>&1; then
  mkdir -p "$(dirname "$LOG_FILE")"
  echo "$(date -Iseconds) prompt report-failed cwd=$cwd" >> "$LOG_FILE" 2>/dev/null || true
fi
exit 0
