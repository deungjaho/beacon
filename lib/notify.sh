#!/usr/bin/env bash
# Cross-platform best-effort desktop notification. Args: title, message.
set -uo pipefail

title="${1:-Agent}"
message="${2:-}"
[[ -n "$message" && "${BEACON_NOTIFY:-1}" != "0" ]] || exit 0

case "$(uname -s)" in
  Darwin)
    if command -v osascript >/dev/null 2>&1; then
      osascript \
        -e 'on run argv' \
        -e 'display notification (item 2 of argv) with title (item 1 of argv)' \
        -e 'end run' \
        -- "$title" "$message" >/dev/null 2>&1 || true
    fi
    ;;
  Linux)
    if command -v notify-send >/dev/null 2>&1; then
      notify-send -- "$title" "$message" >/dev/null 2>&1 || true
    fi
    ;;
esac

exit 0
