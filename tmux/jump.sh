#!/usr/bin/env bash
# Jump to the most recently completed live pane.
set -euo pipefail

STATE_FILE="${BEACON_STATE_DIR:-${XDG_DATA_HOME:-$HOME/.local/share}/beacon}/panes.json"
TMUX_BIN="${BEACON_TMUX_BIN:-tmux}"
[[ -f "$STATE_FILE" ]] || exit 0

pane=$(jq -r '.last_completed.pane // empty' "$STATE_FILE" 2>/dev/null || true)
session=$(jq -r '.last_completed.session // empty' "$STATE_FILE" 2>/dev/null || true)
[[ -n "$pane" && -n "$session" ]] || exit 0
"$TMUX_BIN" display-message -p -t "$pane" '#{pane_id}' >/dev/null 2>&1 || exit 0
"$TMUX_BIN" switch-client -t "$session" 2>/dev/null || true
"$TMUX_BIN" select-pane -t "$pane" 2>/dev/null || true
