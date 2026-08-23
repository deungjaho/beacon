#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
export BEACON_ROOT="$ROOT"
export BEACON_STATE_DIR="$TMP/state"
export BEACON_NOTIFY=0
export BEACON_TMUX_BIN="$TMP/bin/tmux"
mkdir -p "$TMP/bin"

cat >"$TMP/bin/tmux" <<'FAKE'
#!/usr/bin/env bash
case "${1:-}" in
  display-message)
    target=''; format=''
    while (($#)); do
      case "$1" in
        -t) target="$2"; shift 2 ;;
        '#{'*) format="$1"; shift ;;
        *) shift ;;
      esac
    done
    case "$format" in
      '#{session_name}') printf 'test-session\n' ;;
      '#{window_id}') printf '@1\n' ;;
      '#{pane_id}') printf '%s\n' "$target" ;;
    esac
    ;;
  list-panes) for i in $(seq 1 20); do printf '%%%s\n' "$i"; done ;;
  list-windows) printf '@1\n@2\n' ;;
  list-sessions) printf 'test-session\n' ;;
  set-option) : ;; # no-op for bell sync
  switch-client|select-pane) printf '%s\n' "$*" >>"${BEACON_TEST_TMUX_LOG:-/dev/null}" ;;
esac
FAKE
chmod +x "$TMP/bin/tmux"
chmod +x "$ROOT/bin/beacon" "$ROOT/hooks/"*.sh "$ROOT/lib/"*.sh "$ROOT/tmux/"*.sh

fail() { printf 'not ok - %s\n' "$1" >&2; exit 1; }
PASS_COUNT=0
pass() { printf 'ok - %s\n' "$1"; PASS_COUNT=$((PASS_COUNT + 1)); }
assert_eq() { [[ "$1" == "$2" ]] || fail "$3 (want=$2 got=$1)"; }
assert_contains() { [[ "$1" == *"$2"* ]] || fail "$3 (missing=$2)"; }
assert_not_contains() { [[ "$1" != *"$2"* ]] || fail "$3 (found=$2)"; }

"$ROOT/bin/beacon" reset
jq -e '.panes == {} and .last_completed == null' "$BEACON_STATE_DIR/panes.json" >/dev/null
pass 'reset creates valid state'

TMUX_PANE='%1' BEACON_NOW=100 "$ROOT/bin/beacon" report working $'build\nproject' '/tmp/project'
assert_eq "$(jq -r '.panes["%1"].status' "$BEACON_STATE_DIR/panes.json")" working 'working status'
assert_eq "$(jq -r '.panes["%1"].summary' "$BEACON_STATE_DIR/panes.json")" 'build project' 'summary sanitization'
assert_eq "$(jq -r '.panes["%1"].window' "$BEACON_STATE_DIR/panes.json")" '@1' 'window identity'
pass 'report records pane context'

"$ROOT/bin/beacon" reset
for i in $(seq 1 20); do
  TMUX_PANE="%$i" BEACON_NOW="$i" "$ROOT/bin/beacon" report working "job-$i" &
done
wait
assert_eq "$(jq '.panes | length' "$BEACON_STATE_DIR/panes.json")" 20 'concurrent update count'
pass 'concurrent reports do not lose updates'

# status-right must NOT show agent status — only resource metrics.
"$ROOT/bin/beacon" reset
TMUX_PANE='%1' BEACON_NOW=100 "$ROOT/bin/beacon" report completed 'all tests passed'
rendered=$(BEACON_NOW=100 BEACON_SHOW_SYSTEM=0 "$ROOT/bin/beacon" status-tmux 160 black test-session 1 '%1' '@1')
assert_not_contains "$rendered" 'all tests passed' 'no agent summary in status-right'
assert_not_contains "$rendered" 'completed' 'no agent status in status-right'
pass 'status-right has no agent status'

before_hash=$(cksum "$BEACON_STATE_DIR/panes.json")
BEACON_NOW=100 BEACON_SHOW_SYSTEM=0 "$ROOT/bin/beacon" status-tmux 160 black test-session 1 '%1' '@1' >/dev/null
after_hash=$(cksum "$BEACON_STATE_DIR/panes.json")
assert_eq "$after_hash" "$before_hash" 'statusline state hash'
pass 'tmux renderer is read-only'

"$ROOT/bin/beacon" reset
rendered=$(BEACON_SHOW_SYSTEM=0 "$ROOT/bin/beacon" status-tmux 160 black test-session 1 '%9' '@9')
assert_not_contains "$rendered" 'codex working' 'no inferred agent working status'
assert_not_contains "$rendered" 'claude working' 'no inferred agent working status'
[[ "$(jq '.panes | length' "$BEACON_STATE_DIR/panes.json")" == 0 ]] || fail 'no record should persist'
pass 'no inferred agent working without explicit record'

# Explicit report must NOT show agent text in status-right.
TMUX_PANE='%9' BEACON_NOW=100 "$ROOT/bin/beacon" report waiting 'needs input'
rendered=$(BEACON_NOW=100 BEACON_SHOW_SYSTEM=0 "$ROOT/bin/beacon" status-tmux 160 black test-session 1 '%9' '@1')
assert_not_contains "$rendered" 'needs input' 'no agent text in status-right'
assert_not_contains "$rendered" 'waiting' 'no agent status in status-right'
pass 'explicit report does not render agent in status-right'

# Acknowledge/jump/notification tests are Go-only (shell is rollback path).
# Shell tests verify resource-only rendering and no agent status.

TMUX_PANE='%1' BEACON_NOW=100 "$ROOT/bin/beacon" report completed old
TMUX_PANE='%2' BEACON_NOW=100 "$ROOT/bin/beacon" report working active
BEACON_NOW=1000 BEACON_COMPLETED_TTL_SECONDS=300 "$ROOT/bin/beacon" cleanup
assert_eq "$(jq -r '.panes["%1"] // "missing"' "$BEACON_STATE_DIR/panes.json")" missing 'completed expiry'
assert_eq "$(jq -r '.panes["%2"].status' "$BEACON_STATE_DIR/panes.json")" working 'active retention'
pass 'cleanup expires completion but retains live work'

printf '{broken' | "$ROOT/bin/beacon" hook prompt
pass 'malformed hook input is non-fatal'

TMUX_PANE='%1' BEACON_NOW=100 printf '{"tool_name":"shell"}' | TMUX_PANE='%1' BEACON_NOW=100 "$ROOT/bin/beacon" hook permission
assert_eq "$(jq -r '.panes["%1"].status' "$BEACON_STATE_DIR/panes.json")" waiting 'permission waiting status'
pass 'permission hook marks waiting'

"$ROOT/bin/beacon" doctor >/dev/null
pass 'doctor validates dependencies and state'

mkdir -p "$TMP/prefix/bin"
ln -s "$ROOT/bin/beacon" "$TMP/prefix/bin/beacon"
"$TMP/prefix/bin/beacon" status >/dev/null
pass 'CLI resolves installation symlink'

# Verify status render scripts do not reference panes.json or Beacon status parsing.
for script in "$ROOT/tmux/status.sh"; do
  assert_not_contains "$(cat "$script")" 'panes.json' "no panes.json in $script"
  assert_not_contains "$(cat "$script")" 'acknowledged' "no acknowledged in $script"
done
pass 'status render scripts have no Beacon state parsing'

# Verify Powerline separator U+E0B2 is present in status.sh output format.
sep_bytes=$(printf '\xee\x82\xb2')
assert_contains "$(cat "$ROOT/tmux/status.sh")" "$sep_bytes" 'Powerline separator in status.sh'
pass 'Powerline separator present in status.sh'

PLAN=14
if [[ "$PASS_COUNT" -ne "$PLAN" ]]; then
  fail "plan mismatch: expected $PLAN passes, got $PASS_COUNT"
fi
printf '1..%d\n' "$PLAN"
