#!/usr/bin/env bash
# tmux status-right: resource metrics only (no agent status).
# Agent notifications display as 🔔 in session/window/pane names instead.
set -euo pipefail

BEACON_ROOT="${BEACON_ROOT:-$(cd "$(dirname "$0")/.." && pwd)}"

width="${1:-100}"
status_bg="${2:-black}"
session_name="${3:-}"
window_index="${4:-}"
pane_id="${5:-}"
window_id="${6:-}"
[[ -z "$status_bg" || "$status_bg" == "default" ]] && status_bg="black"
if [[ "$width" =~ ^[0-9]+$ ]] && (( width < ${BEACON_MIN_WIDTH:-80} )); then
  exit 0
fi

# Resource metrics only — no agent status segments.
segments=""
if [[ "${BEACON_SHOW_SYSTEM:-1}" == "1" ]]; then
  system_segments=$("$BEACON_ROOT/lib/system.sh" 2>/dev/null || true)
  if [[ -n "$system_segments" ]]; then
    segments="${system_segments}"
  fi
fi

[[ -n "$segments" ]] || exit 0
prev_bg="$status_bg"
output=""
while IFS='|' read -r fg bg text; do
  [[ -n "$fg" && -n "$bg" && -n "$text" ]] || continue
  output+="#[fg=${bg},bg=${prev_bg}]#[fg=${fg},bg=${bg}]${text}"
  prev_bg="$bg"
done <<<"$segments"

[[ -n "$output" ]] || exit 0
printf '%s#[fg=%s,bg=%s]█' "$output" "$prev_bg" "$status_bg"
