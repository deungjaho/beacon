#!/usr/bin/env bash
# Fast, local-only host status provider. Emits fg|bg|text records.
set -euo pipefail

human_bytes() {
  awk -v bytes="$1" 'BEGIN {
    split("B K M G T", unit, " "); i=1;
    while (bytes >= 1024 && i < 5) { bytes /= 1024; i++ }
    if (i < 4) printf "%.0f%s", bytes, unit[i]; else printf "%.1f%s", bytes, unit[i]
  }'
}

memory_segment() {
  local total available used page_size free inactive speculative
  case "$(uname -s)" in
    Darwin)
      total=$(sysctl -n hw.memsize 2>/dev/null || true)
      page_size=$(sysctl -n hw.pagesize 2>/dev/null || printf '4096')
      [[ "$total" =~ ^[0-9]+$ ]] || return 0
      read -r free inactive speculative < <(vm_stat 2>/dev/null | awk '
        /Pages free:/ {gsub("\\.","",$3); f=$3}
        /Pages inactive:/ {gsub("\\.","",$3); i=$3}
        /Pages speculative:/ {gsub("\\.","",$3); s=$3}
        END {print f+0, i+0, s+0}')
      available=$(( (free + inactive + speculative) * page_size ))
      ;;
    Linux)
      total=$(awk '/^MemTotal:/ {print $2 * 1024}' /proc/meminfo 2>/dev/null || true)
      available=$(awk '/^MemAvailable:/ {print $2 * 1024}' /proc/meminfo 2>/dev/null || true)
      [[ "$total" =~ ^[0-9]+$ && "$available" =~ ^[0-9]+$ ]] || return 0
      ;;
    *) return 0 ;;
  esac
  (( available > total )) && available=$total
  used=$((total - available))
  printf '#1d1f21|#6CB8C7|  %s/%s \n' "$(human_bytes "$used")" "$(human_bytes "$total")"
}

host_segment() {
  [[ "${BEACON_SHOW_HOST:-0}" == "1" ]] || return 0
  local host
  host=$(hostname -s 2>/dev/null || hostname 2>/dev/null || true)
  [[ -n "$host" ]] && printf '#1d1f21|#7F9F7F| %s \n' "$host"
}

memory_segment
host_segment
