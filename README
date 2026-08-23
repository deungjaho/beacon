# Beacon

Beacon is a local-first attention and status tool for agent-driven terminal
work. It observes the current machine only: agent lifecycle hooks write a
small bounded JSON snapshot, tmux renders it, notifications surface events,
and `beacon jump` returns to the most recently completed pane.

Beacon does not schedule tasks, own tmux sessions, or require Pantheon. A Mac
and an Omarchy host run independent Beacon instances. Pantheon may become an
optional provider later, but local operation must never depend on a network.

## Architecture

Beacon is a single Go binary with a background daemon:

- `beacon daemon`: local daemon, samples host metrics on a three-tier
  cadence, maintains an atomic snapshot cache, and listens on a Unix
  socket for report/hook updates.
- `beacon report/status/status-tmux/jump/notify/doctor/reset/hook`: CLI
  commands that read the snapshot cache and agent state. `status-tmux` is
  read-only and never invokes subprocesses; it reads pre-sampled metrics
  from the daemon's cache and renders in-process (p95 < 5ms).
- When the daemon is unreachable, `report` falls back to direct atomic file
  writes and `status-tmux` reads the last cached snapshot.

### Sampling cadence

The daemon samples metrics in three independent tiers, each with its own
interval, `sampled_at` timestamp, and `stale` flag. On startup, all three
tiers fire immediately. If a tier fails, last-good values are retained
and marked stale.

| Tier | Interval | Metrics | macOS source | Linux source |
|------|----------|---------|--------------|--------------|
| Fast | 4s | CPU usage, memory pressure, process count | `iostat -c 2 -w 1`, `memory_pressure`, `ps aux` | `/proc/stat`, `/proc/meminfo`, `ps aux` |
| Footprint | 10s | per-pane/window/session/total tmux memory | `top -l 1 -n 999` (physical footprint) | `ps -eo pid,ppid,rss` (RSS) |
| Slow | 60s | root disk used/total | `df -k /System/Volumes/Data` (consumed = total − available) | `df -k /` (Used) |

The fast tier (4s) matches the tmux 5s status refresh and never scans
the full process list. The footprint tier (10s) runs `top -n 999` which
takes ~0.7s on a typical machine — well within the 10s budget. The slow
tier (60s) uses lightweight system calls.

### Memory metrics

Per-process memory (`MemoryKB`) uses different semantics by platform:

- **macOS**: physical footprint from `top`'s MEM column. This is the
  actual physical memory consumed by the process, **not** MEM+CMPRS
  (which would double-count compressed pages). The `MemoryKind` is
  `"footprint"`.
- **Linux**: RSS (resident set size) from `ps`. This is the standard
  per-process memory metric on Linux. The `MemoryKind` is `"rss"`.

## Capabilities

- Claude Code prompt/stop/notification hooks
- Codex permission hook
- explicit status reporting for any agent via `beacon report`
- concurrent-safe, atomic local state under
  `${XDG_DATA_HOME:-~/.local/share}/beacon/panes.json`
- bounded tmux status-right output with:
  - CPU usage (macOS `top`, Linux `/proc/stat`)
  - memory pressure (macOS `memory_pressure`, Linux `/proc/meminfo`)
  - process count
  - per-pane, per-window, per-session, and total tmux memory (RSS aggregation)
  - Agent working/waiting/blocked/completed status
- jump to the last completed live pane
- macOS and Linux desktop notifications
- launchd (macOS) and systemd (Linux) user service management
- no network, database, or model calls

## Install

Requirements: Go 1.26+ and tmux.

```bash
./install.sh
beacon doctor
```

The install script builds the Go binary, installs it to `~/.local/lib/beacon/`,
symlinks it to `~/.local/bin/beacon`, and sets up the daemon service
(launchd on macOS, systemd on Linux). The previous shell-based implementation
is preserved under `~/.local/lib/beacon/shell-backup/` for rollback within
the current release.

Claude Code hooks:

```json
{
  "hooks": {
    "UserPromptSubmit": [{"hooks": [{"type": "command", "command": "beacon hook prompt"}]}],
    "Stop": [{"hooks": [{"type": "command", "command": "beacon hook stop"}]}],
    "Notification": [{"hooks": [{"type": "command", "command": "beacon hook notification"}]}]
  }
}
```

Codex can use the same prompt/stop hooks and the permission hook in
`~/.codex/hooks.json`; each Beacon entry may coexist with other integrations:

```json
{
  "hooks": {
    "UserPromptSubmit": [{"hooks": [{"type": "command", "command": "beacon hook prompt"}]}],
    "Stop": [{"hooks": [{"type": "command", "command": "beacon hook stop"}]}],
    "PermissionRequest": [{"hooks": [{"type": "command", "command": "beacon hook permission"}]}]
  }
}
```

tmux:

```tmux
set -g status-right '#(beacon status-tmux "#{client_width}" "#{status-bg}" "#{session_name}" "#{window_index}" "#{pane_id}" "#{window_id}")'
bind-key P run-shell 'beacon jump'
```

Any agent or wrapper can report a state without knowing about Pantheon:

```bash
beacon report working 'running tests'
beacon report waiting 'needs user input'
beacon report blocked 'dependency unavailable'
beacon report completed 'tests passed'
```

## Daemon management

```bash
beacon daemon start    # start the daemon (foreground)
beacon daemon stop     # stop the running daemon
beacon daemon status   # check if the daemon is running
```

Under launchd/systemd, the daemon starts automatically and restarts on
failure. `beacon doctor` reports daemon, socket, and cache freshness.

## Status bar segments

Strict display order: CPU, memory pressure, process count, pane
memory, window memory, session memory, total tmux memory, root disk.
All available metrics are always rendered in fixed order;
tmux handles truncation natively when the terminal is narrow.

Agent notifications (waiting/blocked/completed) are not shown in
status-right. They display as a bell icon in the session/window/pane
name via event-driven tmux user options.

### Notifications

On macOS, Beacon prefers `terminal-notifier` (if installed) for desktop
notifications. When a notification is triggered from a tmux pane, the
notification includes a `-execute` action that activates the terminal
app and runs `beacon jump <pane_id>` to switch the attached tmux client
to the originating pane. Clicking the notification jumps directly to
the pane that produced it.

If `terminal-notifier` is not installed, Beacon falls back to `osascript`
for basic notifications without click-to-jump.

Install terminal-notifier via Homebrew:

```
brew install terminal-notifier
```

The `-execute` action uses the absolute path of the running beacon
binary (via `os.Executable`), so it works even when the notification
shell's PATH does not include `~/.local/bin`. All arguments are
POSIX shell-quoted to prevent injection.

| Segment | Icon | Color (bg) | Cadence |
|---|---|---|---|
| CPU | `` | green/yellow/red by usage | 4s |
| Memory pressure | `` | green/yellow/red by pressure | 4s |
| Process count | `` | `#B48EAD` | 4s |
| Pane memory | `` | `#7CB8BB` | 10s |
| Window memory | `󰖲` | `#5A8A8A` | 10s |
| Session memory | `` | `#4A7A7A` | 10s |
| Total tmux memory | `󰍛` | `#3A6A6A` | 10s |
| Root disk | `` | `#DFAF8F` (red `#CC9393` when available < 5 GiB) | 60s |

## Boundaries

- Runtime state is local and disposable; Git stores configuration, not state.
- tmux is a presentation/execution surface, not a task-state authority.
- Beacon does not replace Pantheon orchestration or Mnemos memory.
- Host telemetry stays deliberately small and cheap. Use a monitoring system
  for historical or fleet-level metrics.

## Development

```bash
go test ./...           # run all tests
go test -race ./...     # run with race detector
go build ./cmd/beacon   # build the binary
```
