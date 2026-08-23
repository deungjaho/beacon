#!/usr/bin/env bash
# Concurrent-safe Beacon state management.
set -euo pipefail

STATE_DIR="${BEACON_STATE_DIR:-${XDG_DATA_HOME:-$HOME/.local/share}/beacon}"
STATE_FILE="$STATE_DIR/panes.json"
LOCK_DIR="$STATE_DIR/.state.lock"
DEFAULT_STATE='{"panes":{},"last_completed":null}'
LOCK_TIMEOUT_MS="${BEACON_LOCK_TIMEOUT_MS:-1500}"
COMPLETED_TTL_SECONDS="${BEACON_COMPLETED_TTL_SECONDS:-300}"
TMUX_BIN="${BEACON_TMUX_BIN:-tmux}"

now_seconds() {
  if [[ -n "${BEACON_NOW:-}" ]]; then
    printf '%s' "$BEACON_NOW"
  else
    date +%s
  fi
}

mtime_seconds() {
  stat -f %m "$1" 2>/dev/null || stat -c %Y "$1" 2>/dev/null || printf '0'
}

acquire_lock() {
  mkdir -p "$STATE_DIR"
  local waited=0 now mtime
  while ! mkdir "$LOCK_DIR" 2>/dev/null; do
    now=$(now_seconds)
    mtime=$(mtime_seconds "$LOCK_DIR")
    if [[ "$mtime" =~ ^[0-9]+$ ]] && (( mtime > 0 && now - mtime > 30 )); then
      rmdir "$LOCK_DIR" 2>/dev/null || true
      continue
    fi
    if (( waited >= LOCK_TIMEOUT_MS )); then
      echo "beacon: state lock timeout" >&2
      return 1
    fi
    sleep 0.01
    waited=$((waited + 10))
  done
}

release_lock() {
  rmdir "$LOCK_DIR" 2>/dev/null || true
}

valid_state_or_default() {
  if [[ -f "$STATE_FILE" ]] && jq -e '
      type == "object" and
      (.panes | type == "object") and
      (has("last_completed"))
    ' "$STATE_FILE" >/dev/null 2>&1; then
    cat "$STATE_FILE"
  else
    printf '%s\n' "$DEFAULT_STATE"
  fi
}

write_state_unlocked() {
  local content="$1" tmp
  jq -e 'type == "object" and (.panes | type == "object")' <<<"$content" >/dev/null
  tmp="$STATE_DIR/.panes.json.tmp.$$"
  umask 077
  printf '%s\n' "$content" >"$tmp"
  chmod 600 "$tmp"
  mv "$tmp" "$STATE_FILE"
}

mutate() {
  acquire_lock
  trap release_lock EXIT
  local cur next
  cur=$(valid_state_or_default)
  next=$(jq "$@" <<<"$cur")
  write_state_unlocked "$next"
  trap - EXIT
  release_lock
}

init_state() {
  acquire_lock
  trap release_lock EXIT
  if [[ ! -f "$STATE_FILE" ]] || ! jq -e 'type == "object" and (.panes | type == "object")' "$STATE_FILE" >/dev/null 2>&1; then
    write_state_unlocked "$DEFAULT_STATE"
  fi
  trap - EXIT
  release_lock
}

set_pane() {
  local pane="$1" record="$2"
  [[ -n "$pane" ]] || { echo "beacon: pane is required" >&2; return 2; }
  jq -e '
    type == "object" and
    (.status | type == "string") and
    (.summary | type == "string") and
    (.time | type == "number")
  ' <<<"$record" >/dev/null
  mutate --arg pane "$pane" --argjson rec "$record" '.panes[$pane] = $rec'
}

del_pane() {
  local pane="$1"
  mutate --arg pane "$pane" 'del(.panes[$pane])'
}

set_last() {
  local record="$1"
  jq -e 'type == "object" and (.pane | type == "string")' <<<"$record" >/dev/null
  mutate --argjson rec "$record" '.last_completed = $rec'
}

cleanup() {
  local cutoff live_json='null'
  cutoff=$(( $(now_seconds) - COMPLETED_TTL_SECONDS ))
  if command -v "$TMUX_BIN" >/dev/null 2>&1; then
    live_json=$("$TMUX_BIN" list-panes -a -F '#{pane_id}' 2>/dev/null \
      | jq -Rsc 'split("\n") | map(select(length > 0))' 2>/dev/null || printf 'null')
  fi
  mutate --argjson cutoff "$cutoff" --argjson live "$live_json" '
    .panes |= with_entries(
      .key as $key
      | select(
          ((.value.status != "completed") or (.value.time > $cutoff)) and
          (($live == null) or ($live | index($key)) != null)
        )
    )
  '
}

reset_state() {
  acquire_lock
  trap release_lock EXIT
  write_state_unlocked "$DEFAULT_STATE"
  trap - EXIT
  release_lock
}

case "${1:-}" in
  init) init_state ;;
  set-pane) set_pane "${2:-}" "${3:-}" ;;
  del-pane) del_pane "${2:-}" ;;
  set-last) set_last "${2:-}" ;;
  cleanup) cleanup ;;
  reset) reset_state ;;
  get) init_state; valid_state_or_default ;;
  *) echo "usage: state.sh {init|set-pane|del-pane|set-last|cleanup|reset|get}" >&2; exit 2 ;;
esac
