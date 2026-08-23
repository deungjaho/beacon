#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")" && pwd)"
PREFIX="${BEACON_PREFIX:-$HOME/.local}"
DEST="$PREFIX/lib/beacon"
LAUNCH_AGENT_LABEL="com.cognition.beacon"
LAUNCH_AGENT_PLIST="$HOME/Library/LaunchAgents/${LAUNCH_AGENT_LABEL}.plist"
SYSTEMD_USER_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
SYSTEMD_UNIT="beacon.service"

# Build the Go binary if Go is available; otherwise fall back to the shell wrapper.
build_go_binary() {
  if command -v go >/dev/null 2>&1; then
    printf 'building Go binary...\n'
    rm -f "$DEST/bin/beacon"
    go build -o "$DEST/bin/beacon" ./cmd/beacon
    return 0
  fi
  return 1
}

# Install launchd plist (macOS).
# Uses the modern launchctl bootstrap/bootout API so the service is
# registered in the gui/UID domain and survives shell exit/logout.
# All steps are hard-fail: if bootstrap or kickstart fails, the script
# exits non-zero so the user knows the service is not managed.
install_launchd() {
  [[ "$(uname -s)" != "Darwin" ]] && return 0
  local bin="$DEST/bin/beacon"
  local state_dir="${XDG_DATA_HOME:-$HOME/.local/share}/beacon"
  local cache_dir="$HOME/Library/Caches/beacon"
  local uid
  uid="$(id -u)"
  local domain="gui/${uid}"

  mkdir -p "$HOME/Library/LaunchAgents" "$cache_dir"

  cat >"$LAUNCH_AGENT_PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>${LAUNCH_AGENT_LABEL}</string>
  <key>ProgramArguments</key>
  <array>
    <string>${bin}</string>
    <string>daemon</string>
    <string>start</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>EnvironmentVariables</key>
  <dict>
    <key>BEACON_STATE_DIR</key>
    <string>${state_dir}</string>
    <key>BEACON_CACHE_DIR</key>
    <string>${cache_dir}</string>
    <key>PATH</key>
    <string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
  </dict>
  <key>StandardOutPath</key>
  <string>${cache_dir}/daemon.log</string>
  <key>StandardErrorPath</key>
  <string>${cache_dir}/daemon.err</string>
</dict>
</plist>
EOF

  # Bootout any existing registration (ignore "not loaded" errors).
  launchctl bootout "$domain/$LAUNCH_AGENT_LABEL" 2>/dev/null || true

  # Bootstrap the plist into the gui/UID domain — hard fail on error.
  if ! launchctl bootstrap "$domain" "$LAUNCH_AGENT_PLIST" 2>&1; then
    printf 'error: launchctl bootstrap failed for %s\n' "$domain" >&2
    return 1
  fi

  # Kickstart ensures the service is running right now — hard fail on error.
  if ! launchctl kickstart -k "$domain/$LAUNCH_AGENT_LABEL" 2>&1; then
    printf 'error: launchctl kickstart failed for %s\n' "$domain" >&2
    return 1
  fi

  # Hard verification: launchctl print must succeed.
  if ! launchctl print "$domain/$LAUNCH_AGENT_LABEL" >/dev/null 2>&1; then
    printf 'error: launchctl print failed — service not registered in %s\n' "$domain" >&2
    return 1
  fi

  printf 'installed launchd agent: %s (domain=%s)\n' "$LAUNCH_AGENT_PLIST" "$domain"
}

# Install systemd user service (Linux).
install_systemd() {
  [[ "$(uname -s)" != "Linux" ]] && return 0
  local bin="$DEST/bin/beacon"
  local state_dir="${XDG_DATA_HOME:-$HOME/.local/share}/beacon"
  local cache_dir="${XDG_CACHE_HOME:-$HOME/.cache}/beacon"
  mkdir -p "$SYSTEMD_USER_DIR"
  cat >"$SYSTEMD_USER_DIR/$SYSTEMD_UNIT" <<EOF
[Unit]
Description=Beacon local agent/tmux status daemon
After=default.target

[Service]
Type=simple
ExecStart=${bin} daemon start
Environment=BEACON_STATE_DIR=${state_dir}
Environment=BEACON_CACHE_DIR=${cache_dir}
Restart=on-failure
RestartSec=3

[Install]
WantedBy=default.target
EOF
  systemctl --user daemon-reload 2>/dev/null || true
  systemctl --user enable --now "$SYSTEMD_UNIT" 2>/dev/null || true
  printf 'installed systemd user service: %s\n' "$SYSTEMD_UNIT"
}

# Stop any running daemon before re-registering the service.
# Tries the newly built binary first, then the installed symlink.
stop_daemon() {
  local candidates=("$DEST/bin/beacon" "$PREFIX/bin/beacon")
  for bin in "${candidates[@]}"; do
    if [[ -x "$bin" ]]; then
      "$bin" daemon stop >/dev/null 2>&1 || true
    fi
  done
}

# Keep the shell scripts as rollback backup.
install_shell_backup() {
  install -d "$DEST/shell-backup/bin" "$DEST/shell-backup/hooks" \
    "$DEST/shell-backup/lib" "$DEST/shell-backup/tmux"
  install -m 0755 "$ROOT/bin/beacon" "$DEST/shell-backup/bin/beacon" 2>/dev/null || true
  install -m 0755 "$ROOT/hooks/"*.sh "$DEST/shell-backup/hooks/" 2>/dev/null || true
  install -m 0755 "$ROOT/lib/"*.sh "$DEST/shell-backup/lib/" 2>/dev/null || true
  install -m 0755 "$ROOT/tmux/"*.sh "$DEST/shell-backup/tmux/" 2>/dev/null || true
}

install -d "$DEST/bin" "$DEST/hooks" "$DEST/lib" "$DEST/tmux" \
  "$DEST/skills/beacon-clear" "$PREFIX/bin"

# Keep shell scripts as rollback.
install_shell_backup

# Install hooks, lib, tmux, skills (shell scripts still used by some integrations).
install -m 0755 "$ROOT/hooks/"*.sh "$DEST/hooks/"
install -m 0755 "$ROOT/lib/"*.sh "$DEST/lib/"
install -m 0755 "$ROOT/tmux/"*.sh "$DEST/tmux/"
install -m 0644 "$ROOT/skills/beacon-clear/SKILL.md" "$DEST/skills/beacon-clear/SKILL.md"

# Build and install Go binary, or fall back to shell wrapper.
if ! build_go_binary; then
  printf 'Go not found; falling back to shell wrapper\n'
  install -m 0755 "$ROOT/bin/beacon" "$DEST/bin/beacon"
fi

ln -sfn "$DEST/bin/beacon" "$PREFIX/bin/beacon"

# Stop any running daemon before re-registering service.
stop_daemon

# Install daemon service (bootout/bootstrap handles start, no manual start).
install_launchd
install_systemd

"$PREFIX/bin/beacon" doctor
printf 'installed Beacon at %s\n' "$DEST"
