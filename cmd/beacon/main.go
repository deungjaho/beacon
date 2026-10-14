// Beacon: local-first Agent/tmux status tool. Single binary, no dependencies.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/deungjaho/beacon/internal/agents"
	"github.com/deungjaho/beacon/internal/collector"
	"github.com/deungjaho/beacon/internal/daemon"
	"github.com/deungjaho/beacon/internal/dashboard"
	"github.com/deungjaho/beacon/internal/pantheon"
	"github.com/deungjaho/beacon/internal/render"
	"github.com/deungjaho/beacon/internal/state"
)

const (
	envStateDir   = "BEACON_STATE_DIR"
	envCacheDir   = "BEACON_CACHE_DIR"
	envTmuxBin    = "BEACON_TMUX_BIN"
	envNow        = "BEACON_NOW"
	envNotify     = "BEACON_NOTIFY"
	envShowSystem = "BEACON_SHOW_SYSTEM"
)

// paneIDPattern validates tmux pane IDs: %<digits> only.
var paneIDPattern = regexp.MustCompile(`^%[0-9]+$`)

func defaultStateDir() string {
	if v := os.Getenv(envStateDir); v != "" {
		return v
	}
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return filepath.Join(v, "beacon")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "beacon")
}

func defaultCacheDir() string {
	if v := os.Getenv(envCacheDir); v != "" {
		return v
	}
	if v := os.Getenv("XDG_CACHE_HOME"); v != "" {
		return filepath.Join(v, "beacon")
	}
	home, _ := os.UserHomeDir()
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Caches", "beacon")
	}
	return filepath.Join(home, ".cache", "beacon")
}

func defaultSocketPath() string {
	// Socket lives in the state dir so that overriding BEACON_STATE_DIR
	// also redirects the socket, preventing mismatched daemon/CLI state.
	return filepath.Join(defaultStateDir(), "beacon.sock")
}

func tmuxBin() string {
	if v := os.Getenv(envTmuxBin); v != "" {
		return v
	}
	return "tmux"
}

func nowSeconds() int64 {
	if v := os.Getenv(envNow); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return time.Now().Unix()
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: beacon <command> [args]

commands:
  report <working|waiting|blocked|completed> [summary] [cwd]
  notify <title> <message>
  status                  print JSON state
  status-tmux [args...]   render tmux status-right (resource metrics only)
  jump [pane_id]          jump to oldest pending pane, or specific pane (e.g. %3)
  acknowledge <pane_id>   clear notification bell for a pane
  acknowledge-visible     clear bells for all panes visible to attached clients
  sync-bells             sync tmux user options from Beacon state
  cleanup                 remove expired or dead-pane records
  register <type> <id> [--pane %N] [--data JSON] [--producer name]
  unregister <type> <id>
  resources [--type t] [--pane %N] [--stale] [--json]
  tabs [--json] [--count] [--stale]   list agent browser tabs
  worktrees [--json] [--pick]         list worktrees + pane occupancy + cleanup candidates
  worktree close <path> [--force]     graceful teardown: unregister, remove, drop merged branch
  wt new <name> [--from ref]          create worktree workspace + run wt.d hooks + register route
  wt list / wt env / wt close <name> [--destroy] [--force]
  reset                   clear local Beacon state
  hook <prompt|stop|notification|permission>
  daemon [start|stop|status]  manage background sampler
  agents [--json]         list discovered agent sessions across panes
  dashboard [--once]      interactive system status monitor
  doctor                  validate local dependencies and state
`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	switch cmd {
	case "report":
		os.Exit(cmdReport(args))
	case "notify":
		os.Exit(cmdNotify(args))
	case "status":
		os.Exit(cmdStatus(args))
	case "status-tmux":
		os.Exit(cmdStatusTmux(args))
	case "dashboard":
		os.Exit(cmdDashboard(args))
	case "jump":
		os.Exit(cmdJump(args))
	case "acknowledge":
		os.Exit(cmdAcknowledge(args))
	case "acknowledge-visible":
		os.Exit(cmdAcknowledgeVisible(args))
	case "sync-bells":
		os.Exit(cmdSyncBells(args))
	case "cleanup":
		os.Exit(cmdCleanup(args))
	case "reset":
		os.Exit(cmdReset(args))
	case "hook":
		os.Exit(cmdHook(args))
	case "daemon":
		os.Exit(cmdDaemon(args))
	case "agents":
		os.Exit(cmdAgents(args))
	case "register":
		os.Exit(cmdRegister(args))
	case "unregister":
		os.Exit(cmdUnregister(args))
	case "resources":
		os.Exit(cmdResources(args))
	case "tabs":
		os.Exit(cmdTabs(args))
	case "worktrees":
		os.Exit(cmdWorktrees(args))
	case "worktree":
		os.Exit(cmdWorktree(args))
	case "wt":
		os.Exit(cmdWt(args))
	case "doctor":
		os.Exit(cmdDoctor(args))
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "beacon: unknown command: %s\n", cmd)
		usage()
		os.Exit(2)
	}
}

func cmdReport(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "beacon: report requires a status")
		return 2
	}
	status := args[0]
	summary := ""
	if len(args) > 1 {
		summary = args[1]
	}
	cwd := ""
	if len(args) > 2 {
		cwd = args[2]
	}
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	pane := os.Getenv("TMUX_PANE")
	if pane == "" {
		return 0
	}
	session, window := tmuxContext(pane)
	now := nowSeconds()
	summary = state.SanitizeSummary(summary)

	// Try daemon socket first (fast path, no fork).
	sock := defaultSocketPath()
	if daemon.IsRunning(sock) {
		req := daemon.SocketRequest{
			Action:  "report",
			Pane:    pane,
			Status:  status,
			Summary: summary,
			Window:  window,
			Session: session,
			Cwd:     cwd,
			Time:    now,
		}
		if err := daemon.SendReport(sock, req); err == nil {
			if status == "completed" {
				_ = daemon.SendReport(sock, daemon.SocketRequest{
					Action:  "set-last",
					Pane:    pane,
					Session: session,
					Window:  window,
					Summary: summary,
					Time:    now,
				})
			}
			// Best-effort cleanup.
			_ = daemon.SendReport(sock, daemon.SocketRequest{Action: "cleanup"})
			// Sync tmux bell options (event-driven, not per-render).
			if store, err := state.NewStore(defaultStateDir()); err == nil {
				syncBells(store)
			}
			return 0
		}
	}

	// Fallback: direct file write.
	store, err := state.NewStore(defaultStateDir())
	if err != nil {
		return 0 // non-fatal
	}
	rec := state.PaneRecord{
		Status:  status,
		Summary: summary,
		Window:  window,
		Session: session,
		Time:    now,
		Cwd:     cwd,
	}
	if err := store.SetPane(pane, rec); err != nil {
		return 0
	}
	if status == "completed" {
		_ = store.SetLast(state.LastCompleted{
			Pane:    pane,
			Session: session,
			Window:  window,
			Summary: summary,
			Time:    now,
		})
	}
	// Best-effort cleanup.
	livePanes := listLivePanes()
	store.Cleanup(now, state.CompletedTTLSeconds, livePanes)
	if reg, err := state.NewRegistryStore(defaultStateDir()); err == nil {
		reg.MarkStaleForDeadPanes(livePanes)
	}
	syncBells(store)
	return 0
}

func tmuxContext(pane string) (session, window string) {
	tb := tmuxBin()
	out, err := exec.Command(tb, "display-message", "-p", "-t", pane, "#{session_name}").Output()
	if err == nil {
		session = strings.TrimSpace(string(out))
	}
	out, err = exec.Command(tb, "display-message", "-p", "-t", pane, "#{window_id}").Output()
	if err == nil {
		window = strings.TrimSpace(string(out))
	}
	return
}

func listLivePanes() []string {
	out, err := exec.Command(tmuxBin(), "list-panes", "-a", "-F", "#{pane_id}").Output()
	if err != nil {
		return nil
	}
	var panes []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			panes = append(panes, line)
		}
	}
	return panes
}

func cmdNotify(args []string) int {
	title := "Agent"
	if len(args) > 0 {
		title = args[0]
	}
	message := ""
	if len(args) > 1 {
		message = args[1]
	}
	if message == "" || os.Getenv(envNotify) == "0" {
		return 0
	}
	runNotify(title, message)
	return 0
}

func cmdStatus(args []string) int {
	store, err := state.NewStore(defaultStateDir())
	if err != nil {
		fmt.Fprintf(os.Stderr, "beacon: %v\n", err)
		return 1
	}
	st, err := store.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "beacon: %v\n", err)
		return 1
	}
	data, _ := json.MarshalIndent(st, "", "  ")
	fmt.Println(string(data))
	return 0
}

func cmdStatusTmux(args []string) int {
	// Args: [flags...] width status_bg session_name window_index pane_id window_id
	rargs := render.Args{
		Width:       100,
		StatusBG:    "black",
		SessionName: "",
		WindowIndex: "",
		PaneID:      "",
		WindowID:    "",
		Compact:     os.Getenv("BEACON_COMPACT_STATUS") == "1",
	}

	var posArgs []string
	for _, a := range args {
		switch a {
		case "--full", "-f":
			rargs.Compact = false
		case "--compact", "-c":
			rargs.Compact = true
		case "--with-tmux-mem", "-m":
			rargs.WithTmuxMem = true
		default:
			posArgs = append(posArgs, a)
		}
	}
	if os.Getenv("BEACON_STATUS_ALL") == "1" || os.Getenv("BEACON_COMPACT_STATUS") == "0" {
		rargs.Compact = false
	}

	if len(posArgs) > 0 {
		rargs.Width, _ = strconv.Atoi(strings.TrimSpace(posArgs[0]))
	}
	if len(posArgs) > 1 {
		rargs.StatusBG = strings.TrimSpace(posArgs[1])
	}
	if len(posArgs) > 2 {
		rargs.SessionName = strings.TrimSpace(posArgs[2])
	}
	if len(posArgs) > 3 {
		rargs.WindowIndex = strings.TrimSpace(posArgs[3])
	}
	if len(posArgs) > 4 {
		rargs.PaneID = strings.TrimSpace(posArgs[4])
	}
	if len(posArgs) > 5 {
		rargs.WindowID = strings.TrimSpace(posArgs[5])
	}

	// Read metrics snapshot (fast file read, no subprocess).
	var m collector.Metrics
	snapshotPath := filepath.Join(defaultCacheDir(), "snapshot.json")
	if data, err := os.ReadFile(snapshotPath); err == nil {
		_ = json.Unmarshal(data, &m)
	}

	output := render.Render(rargs, m)
	if output != "" {
		fmt.Print(output)
	}
	return 0
}

// termCols returns the terminal column count, or 0 when not a TTY.
func termCols() int {
	cmd := exec.Command("stty", "size")
	cmd.Stdin = os.Stdin
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	var rows, cols int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(out)), "%d %d", &rows, &cols); err != nil {
		return 0
	}
	return cols
}

func isTerminal(f *os.File) bool {
	stat, err := f.Stat()
	if err != nil {
		return false
	}
	return (stat.Mode() & os.ModeCharDevice) != 0
}

func setupRawTerminal() (func(), error) {
	cmdGet := exec.Command("stty", "-g")
	cmdGet.Stdin = os.Stdin
	out, err := cmdGet.Output()
	if err != nil {
		return nil, err
	}
	saved := strings.TrimSpace(string(out))
	cmdSet := exec.Command("stty", "cbreak", "-echo")
	cmdSet.Stdin = os.Stdin
	if err := cmdSet.Run(); err != nil {
		return nil, err
	}
	restore := func() {
		cmdRestore := exec.Command("stty", saved)
		cmdRestore.Stdin = os.Stdin
		_ = cmdRestore.Run()
	}
	return restore, nil
}

func cmdDashboard(args []string) int {
	once := false
	for _, a := range args {
		if a == "--once" || a == "-1" {
			once = true
		}
	}

	loadData := func() dashboard.Data {
		var m collector.Metrics
		snapshotPath := filepath.Join(defaultCacheDir(), "snapshot.json")
		if data, err := os.ReadFile(snapshotPath); err == nil {
			_ = json.Unmarshal(data, &m)
		}
		var st *state.State
		if store, err := state.NewStore(defaultStateDir()); err == nil {
			st, _ = store.Load()
		}
		sock := defaultSocketPath()
		daemonRunning := daemon.IsRunning(sock)
		host, _ := os.Hostname()

		return dashboard.Data{
			Metrics:       m,
			State:         st,
			DaemonRunning: daemonRunning,
			Hostname:      host,
			Width:         termCols(),
		}
	}

	if once || !isTerminal(os.Stdin) || !isTerminal(os.Stdout) {
		data := loadData()
		fmt.Print(dashboard.Render(data))
		return 0
	}

	restore, _ := setupRawTerminal()
	if restore != nil {
		defer restore()
	}

	// Hide cursor
	fmt.Print("\033[?25l")
	defer fmt.Print("\033[?25h\033[0m\n")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	keyCh := make(chan byte, 32)
	go func() {
		buf := make([]byte, 1)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil || n == 0 {
				keyCh <- 'q'
				return
			}
			keyCh <- buf[0]
		}
	}()

	redraw := func() {
		data := loadData()
		content := dashboard.Render(data)
		fmt.Print("\033[H\033[2J" + content)
	}

	redraw()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-sigCh:
			return 0
		case b := <-keyCh:
			switch b {
			case 'q', 'Q', 3: // 3 = Ctrl-C
				return 0
			case 27: // Esc
				time.Sleep(30 * time.Millisecond)
				isSeq := false
				for {
					select {
					case <-keyCh:
						isSeq = true
					default:
						goto escChecked
					}
				}
			escChecked:
				if !isSeq {
					return 0
				}
			case 'r', 'R':
				redraw()
			}
		case <-ticker.C:
			redraw()
		}
	}
}

// cmdJump jumps to a notification pane. With no argument it jumps to the
// oldest unacknowledged pending pane. With a pane ID argument (validated
// as ^%[0-9]+$) it queries the live pane's session and jumps directly,
// then acknowledges it.
// Fail-closed: the bell is only acknowledged after a successful jump.
// If any tmux step fails, the bell is preserved and an error is printed.
func cmdJump(args []string) int {
	store, err := state.NewStore(defaultStateDir())
	if err != nil {
		return 0
	}
	// `beacon jump --tab <id>`: activate the browser tab via its producer
	// (argus owns CDP); fall back to landing on the owning agent pane.
	if len(args) >= 2 && args[0] == "--tab" {
		return jumpToTab(store, args[1])
	}
	// `beacon jump --worktree <path>`: land on the worktree's owner pane,
	// falling back to a pane currently inside it.
	if len(args) >= 2 && args[0] == "--worktree" {
		reg, err := state.NewRegistryStore(defaultStateDir())
		if err != nil {
			return 1
		}
		views, _ := assembleWorktrees(reg)
		for _, v := range views {
			if v.Path != canonPath(args[1]) {
				continue
			}
			target := v.OwnerPane
			if !paneIDPattern.MatchString(target) && len(v.Panes) > 0 {
				target = v.Panes[0]
			}
			if paneIDPattern.MatchString(target) {
				return jumpToPane(store, target)
			}
			fmt.Fprintf(os.Stderr, "beacon: worktree %s has no pane to jump to\n", args[1])
			return 1
		}
		fmt.Fprintf(os.Stderr, "beacon: no worktree %q\n", args[1])
		return 1
	}
	// If a pane ID argument is given, jump directly to that pane.
	if len(args) > 0 {
		pane := args[0]
		if !paneIDPattern.MatchString(pane) {
			fmt.Fprintf(os.Stderr, "beacon: invalid pane ID %q (must match ^%%[0-9]+$)\n", pane)
			return 2
		}
		return jumpToPane(store, pane)
	}
	// No argument: jump to oldest unacknowledged notification pane.
	pending := store.PendingNotifications()
	for _, target := range pending {
		// Verify pane is still live.
		if _, err := exec.Command(tmuxBin(), "display-message", "-p", "-t", target.Pane, "#{pane_id}").Output(); err != nil {
			// Pane is gone; acknowledge it (clears stale bell) and try next.
			_ = ackPane(store, target.Pane)
			continue
		}
		if !switchToTarget(target.Session, target.Pane) {
			fmt.Fprintf(os.Stderr, "beacon: jump to %s failed (no client or tmux error); bell preserved\n", target.Pane)
			return 1
		}
		_ = ackPane(store, target.Pane)
		return 0
	}
	return 0
}

// cmdTabs lists browser tab resources owned by panes:
// beacon tabs [--json] [--count] [--stale] [--pick]
// --pick opens an interactive selector (fzf when present, else a numbered
// prompt on /dev/tty) and jumps to the chosen tab — the tmux popup entry point.
func cmdTabs(args []string) int {
	asJSON, countOnly, staleOnly, pick := false, false, false, false
	for _, a := range args {
		switch a {
		case "--json":
			asJSON = true
		case "--count":
			countOnly = true
		case "--stale":
			staleOnly = true
		case "--pick":
			pick = true
		}
	}
	reg, err := state.NewRegistryStore(defaultStateDir())
	if err != nil {
		return 1
	}
	recs := reg.List("tab", "", staleOnly)
	recs = reconcileLiveTabs(reg, recs)
	if pick {
		store, err := state.NewStore(defaultStateDir())
		if err != nil {
			return 1
		}
		return pickTab(store, recs)
	}
	if countOnly {
		fmt.Println(len(recs))
		return 0
	}
	if asJSON {
		out, _ := json.Marshal(recs)
		fmt.Println(string(out))
		return 0
	}
	if len(recs) == 0 {
		fmt.Println("no agent tabs")
		return 0
	}
	now := nowSeconds()
	for _, r := range recs {
		title, _ := r.Metadata["title"].(string)
		u, _ := r.Metadata["url"].(string)
		label := title
		if label == "" {
			label = u
		} else if u != "" {
			label += "  " + u
		}
		staleMark := ""
		if r.Stale {
			staleMark = " [stale]"
		}
		fmt.Printf("%-40s pane=%-4s age=%-5ds %s%s\n",
			r.ID, r.OwnerPane, now-r.CreatedAt, label, staleMark)
	}
	return 0
}

// worktreeView is the read-time assembly of one worktree's full state:
// git worktree list (existence/branch/lock) ∪ registry (port/slug/owner)
// ∪ live pane occupancy. Nothing here is persisted beyond the registry row.
type worktreeView struct {
	Path       string   `json:"path"`
	Repo       string   `json:"repo"`
	Branch     string   `json:"branch"`
	IsMain     bool     `json:"is_main"`
	Registered bool     `json:"registered"`
	Port       int      `json:"port,omitempty"`
	Slug       string   `json:"slug,omitempty"`
	OwnerPane  string   `json:"owner_pane,omitempty"`
	Panes      []string `json:"panes,omitempty"`
	Dirty      bool     `json:"dirty"`
	DirtyFiles []string `json:"dirty_files,omitempty"`
	Merged     bool     `json:"merged"`
	Locked     bool     `json:"locked"`
	Prunable   bool     `json:"prunable"`
	State      string   `json:"state"` // occupied | idle | exposed
	regID      string   // exact registry ID for removal
	repoDir    string   // main checkout dir, for git worktree remove/branch ops
}

// cmdWorktrees lists every visible worktree: registered ones plus all
// worktrees enumerated via `git worktree list` over the active repo set
// (pane toplevels ∪ registered worktree repos). Unregistered ones surface
// as "exposed" so nothing accumulates invisibly.
// beacon worktrees [--json] [--pick]  — pick jumps to the owner pane.
func cmdWorktrees(args []string) int {
	asJSON, pick := false, false
	for _, a := range args {
		switch a {
		case "--json":
			asJSON = true
		case "--pick":
			pick = true
		}
	}
	reg, err := state.NewRegistryStore(defaultStateDir())
	if err != nil {
		return 1
	}
	views, livePanes := assembleWorktrees(reg)
	if pick {
		store, err := state.NewStore(defaultStateDir())
		if err != nil {
			return 1
		}
		return pickWorktree(store, views)
	}
	if asJSON {
		out, _ := json.Marshal(views)
		fmt.Println(string(out))
		return 0
	}
	if len(views) == 0 {
		fmt.Println("no worktrees visible (devproxy register <path> or a pane inside a repo)")
		return 0
	}
	for _, v := range views {
		var b strings.Builder
		fmt.Fprintf(&b, "%-48s %-8s", v.Path, v.State)
		if v.IsMain {
			b.WriteString(" main")
		}
		if v.Repo != "" {
			fmt.Fprintf(&b, " repo=%s", v.Repo)
		}
		if v.Branch != "" {
			fmt.Fprintf(&b, " branch=%s", v.Branch)
		}
		if v.Registered {
			fmt.Fprintf(&b, " port=%d slug=%s", v.Port, v.Slug)
		}
		if v.OwnerPane != "" {
			fmt.Fprintf(&b, " owner=%s", v.OwnerPane)
		}
		if len(v.Panes) > 0 {
			fmt.Fprintf(&b, " in=%s", strings.Join(v.Panes, ","))
		}
		if v.Dirty {
			fmt.Fprintf(&b, " dirty(%d)", len(v.DirtyFiles))
		}
		if v.Merged {
			b.WriteString(" merged")
		}
		if v.Locked {
			b.WriteString(" locked")
		}
		if v.Prunable {
			b.WriteString(" prunable")
		}
		fmt.Println(b.String())
	}
	var cands []*worktreeView
	for _, v := range views {
		if v.IsMain || len(v.Panes) > 0 || v.Locked {
			continue
		}
		if v.OwnerPane != "" && livePanes[v.OwnerPane] {
			continue // owner pane still live — task line not over
		}
		cands = append(cands, v)
	}
	if len(cands) > 0 {
		fmt.Println("\ncleanup candidates (beacon worktree close <path>):")
		for _, v := range cands {
			hint := "clean"
			if v.Prunable {
				hint = "prunable"
			} else if v.Dirty {
				hint = fmt.Sprintf("dirty (%d files)", len(v.DirtyFiles))
			} else if v.Merged {
				hint = "merged, clean"
			}
			fmt.Printf("  %s  %s\n", v.Path, hint)
		}
	}
	return 0
}

// cmdWorktree: beacon worktree close <path> [--force]
// Graceful teardown in fixed order: refuse occupied/dirty (unless --force),
// drop devproxy route + registry record, git worktree remove, delete the
// merged branch.
func cmdWorktree(args []string) int {
	force := false
	var path string
	rest := args
	if len(rest) > 0 && rest[0] == "close" {
		rest = rest[1:]
	} else {
		fmt.Fprintln(os.Stderr, "usage: beacon worktree close <path> [--force]")
		return 2
	}
	for _, a := range rest {
		if a == "--force" {
			force = true
		} else {
			path = a
		}
	}
	if path == "" {
		fmt.Fprintln(os.Stderr, "usage: beacon worktree close <path> [--force]")
		return 2
	}
	path = canonPath(path)
	reg, err := state.NewRegistryStore(defaultStateDir())
	if err != nil {
		return 1
	}
	views, livePanes := assembleWorktrees(reg)
	var v *worktreeView
	for _, w := range views {
		if w.Path == path {
			v = w
			break
		}
	}
	if v == nil {
		// Cold path: repo not in the active repo set — still a real
		// worktree, build a minimal view from the filesystem.
		v = inspectWorktree(path)
	}
	if v == nil {
		fmt.Fprintf(os.Stderr, "beacon: no worktree at %s\n", path)
		return 1
	}
	if code := wtClosePreflight(v, force, livePanes); code != 0 {
		return code
	}
	if v.repoDir != "" && !v.Prunable {
		argv := []string{"-C", v.repoDir, "worktree", "remove"}
		if force {
			// 单个 -f 只覆盖已跟踪改动；未跟踪内容（node_modules 等）需要 -f -f
			argv = append(argv, "--force", "--force")
		}
		argv = append(argv, v.Path)
		if out, err := exec.Command("git", argv...).CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "beacon: git worktree remove: %s\n", strings.TrimSpace(string(out)))
			return 1
		}
	}
	// 先删 worktree 再摘路由/注册：反过来一旦 remove 失败，目录残留却已从
	// 发现源里消失，变成谁都看不见的孤儿。路由残留会由 devproxy 陈旧回收兜住。
	if v.Registered {
		if dp, err := exec.LookPath("devproxy"); err == nil {
			_ = exec.Command(dp, "unregister", v.Path).Run()
		}
		if v.regID != "" {
			_ = reg.Remove("worktree", v.regID)
		}
	}
	if v.Merged && v.Branch != "" && v.Branch != "(detached)" && v.repoDir != "" {
		_ = exec.Command("git", "-C", v.repoDir, "branch", "-d", v.Branch).Run()
	}
	fmt.Printf("closed %s\n", path)
	return 0
}

// assembleWorktrees merges three sources into one view per canonical path:
// registry records, `git worktree list` over the derived repo set, and live
// pane cwd occupancy. Returns views sorted (main → occupied → idle →
// exposed) and the set of live pane IDs.
func assembleWorktrees(reg *state.RegistryStore) ([]*worktreeView, map[string]bool) {
	recs := reconcileWorktrees(reg, reg.List("worktree", "", false))
	livePanes := map[string]bool{}
	occ := map[string][]string{}
	repoDirs := map[string]string{} // commonDir → main checkout dir
	addRepo := func(dir string) {
		if c := gitCommonDir(dir); c != "" {
			repoDirs[c] = filepath.Dir(c)
		}
	}
	for _, p := range listPanes() {
		livePanes[p.id] = true
		if top := gitToplevel(p.cwd); top != "" {
			top = canonPath(top)
			occ[top] = append(occ[top], p.id)
			addRepo(top)
		}
	}
	for _, r := range recs {
		addRepo(r.ID)
	}

	views := map[string]*worktreeView{}
	var order []string
	get := func(path string) *worktreeView {
		if v := views[path]; v != nil {
			return v
		}
		v := &worktreeView{Path: path}
		views[path] = v
		order = append(order, path)
		return v
	}
	for _, mainDir := range repoDirs {
		out, err := gitOut(mainDir, "worktree", "list", "--porcelain")
		if err != nil {
			continue
		}
		repo := filepath.Base(mainDir)
		def := gitDefaultBranch(mainDir)
		for i, e := range parseWorktreeList(out) {
			v := get(e.path)
			v.repoDir = mainDir
			v.Repo = repo
			v.IsMain = i == 0 || e.bare
			v.Locked = e.locked
			v.Prunable = e.prunable
			if e.detached {
				v.Branch = "(detached)"
			} else {
				v.Branch = e.branch
			}
			if st, err := gitOut(e.path, "status", "--porcelain"); err == nil && st != "" {
				v.Dirty = true
				v.DirtyFiles = strings.Split(st, "\n")
			}
			if !v.IsMain && !e.detached && e.branch != "" && def != "" {
				if err := gitRun(mainDir, "merge-base", "--is-ancestor", e.branch, def); err == nil {
					v.Merged = true
				}
			}
		}
	}
	for _, r := range recs {
		v := get(canonPath(r.ID))
		v.Registered = true
		v.regID = r.ID
		v.OwnerPane = r.OwnerPane
		if port, ok := r.Metadata["port"].(float64); ok {
			v.Port = int(port)
		}
		if slug, ok := r.Metadata["slug"].(string); ok {
			v.Slug = slug
		}
		if v.Repo == "" {
			if repo, ok := r.Metadata["repo"].(string); ok {
				v.Repo = repo
			}
		}
		if v.Branch == "" {
			if b, ok := r.Metadata["branch"].(string); ok {
				v.Branch = b
			}
		}
	}
	var out []*worktreeView
	for _, p := range order {
		v := views[p]
		v.Panes = occ[p]
		switch {
		case len(v.Panes) > 0:
			v.State = "occupied"
		case v.Registered:
			v.State = "idle"
		default:
			v.State = "exposed"
		}
		out = append(out, v)
	}
	sort.SliceStable(out, func(i, j int) bool {
		rank := func(v *worktreeView) int {
			if v.IsMain {
				return 0
			}
			switch v.State {
			case "occupied":
				return 1
			case "idle":
				return 2
			}
			return 3
		}
		ri, rj := rank(out[i]), rank(out[j])
		if ri != rj {
			return ri < rj
		}
		return out[i].Path < out[j].Path
	})
	return out, livePanes
}

// reconcileWorktrees drops registry worktree records whose path is gone or
// no longer a git worktree (directory residue without a .git pointer).
func reconcileWorktrees(reg *state.RegistryStore, recs []state.ResourceRecord) []state.ResourceRecord {
	out := recs[:0]
	for _, r := range recs {
		if _, err := os.Stat(r.ID); err != nil || gitToplevel(r.ID) == "" {
			_ = reg.Remove(r.Type, r.ID)
			continue
		}
		out = append(out, r)
	}
	return out
}

type paneInfo struct{ id, cwd string }

func listPanes() []paneInfo {
	out, err := exec.Command(tmuxBin(), "list-panes", "-a", "-F", "#{pane_id}\t#{pane_current_path}").Output()
	if err != nil {
		return nil
	}
	var panes []paneInfo
	for _, line := range strings.Split(string(out), "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), "\t", 2)
		if len(parts) < 2 || parts[1] == "" {
			continue
		}
		panes = append(panes, paneInfo{id: parts[0], cwd: parts[1]})
	}
	return panes
}

func canonPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

func gitOut(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	argv := append([]string{"-C", dir}, args...)
	out, err := exec.CommandContext(ctx, "git", argv...).Output()
	return strings.TrimSpace(string(out)), err
}

func gitRun(dir string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	argv := append([]string{"-C", dir}, args...)
	return exec.CommandContext(ctx, "git", argv...).Run()
}

// gitRunSlow for mutating ops (worktree add/remove) that can take minutes on
// large repos — the 1.5s probe timeout would kill them mid-write.
func gitRunSlow(dir string, args ...string) error {
	argv := append([]string{"-C", dir}, args...)
	return exec.Command("git", argv...).Run()
}

func gitToplevel(dir string) string {
	if dir == "" {
		return ""
	}
	out, err := gitOut(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return ""
	}
	return out
}

// gitCommonDir resolves the repo's shared .git dir (absolute, symlinks
// evaluated) — the join key that maps any worktree back to its main repo.
// --git-common-dir returns a path relative to the worktree (e.g. ".git" in
// the main checkout), so it must be anchored at dir before canonicalizing.
func gitCommonDir(dir string) string {
	out, err := gitOut(dir, "rev-parse", "--git-common-dir")
	if err != nil || out == "" {
		return ""
	}
	if !filepath.IsAbs(out) {
		out = filepath.Join(dir, out)
	}
	return canonPath(out)
}

// gitDefaultBranch: origin/HEAD short name, else main, else master.
func gitDefaultBranch(mainDir string) string {
	if out, err := gitOut(mainDir, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil && out != "" {
		return strings.TrimPrefix(out, "origin/")
	}
	for _, b := range []string{"main", "master"} {
		if err := gitRun(mainDir, "show-ref", "--verify", "--quiet", "refs/heads/"+b); err == nil {
			return b
		}
	}
	return ""
}

// inspectWorktree builds a worktreeView for a path outside the active repo
// set — used by `worktree close` on cold worktrees no pane/registration
// currently tracks.
func inspectWorktree(path string) *worktreeView {
	if gitToplevel(path) == "" || canonPath(gitToplevel(path)) != path {
		return nil
	}
	common := gitCommonDir(path)
	if common == "" {
		return nil
	}
	v := &worktreeView{Path: path, repoDir: filepath.Dir(common), Repo: filepath.Base(filepath.Dir(common))}
	if b, err := gitOut(path, "branch", "--show-current"); err == nil && b != "" {
		v.Branch = b
	} else {
		v.Branch = "(detached)"
	}
	if st, err := gitOut(path, "status", "--porcelain"); err == nil && st != "" {
		v.Dirty = true
		v.DirtyFiles = strings.Split(st, "\n")
	}
	if out, err := gitOut(v.repoDir, "worktree", "list", "--porcelain"); err == nil {
		for _, e := range parseWorktreeList(out) {
			if e.path == path {
				v.Locked = e.locked
				v.Prunable = e.prunable
			}
		}
	}
	if def := gitDefaultBranch(v.repoDir); def != "" && v.Branch != "(detached)" {
		if err := gitRun(v.repoDir, "merge-base", "--is-ancestor", v.Branch, def); err == nil {
			v.Merged = true
		}
	}
	return v
}

type wtEntry struct {
	path     string
	branch   string
	detached bool
	bare     bool
	locked   bool
	prunable bool
}

func parseWorktreeList(out string) []wtEntry {
	var ents []wtEntry
	var cur *wtEntry
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			ents = append(ents, wtEntry{path: canonPath(strings.TrimPrefix(line, "worktree "))})
			cur = &ents[len(ents)-1]
		case cur == nil:
		case strings.HasPrefix(line, "branch refs/heads/"):
			cur.branch = strings.TrimPrefix(line, "branch refs/heads/")
		case line == "detached":
			cur.detached = true
		case line == "bare":
			cur.bare = true
		case strings.HasPrefix(line, "locked"):
			cur.locked = true
		case strings.HasPrefix(line, "prunable"):
			cur.prunable = true
		}
	}
	return ents
}

// pickWorktree is the tmux-popup selector; selection jumps to the owner
// pane, falling back to a pane currently inside the worktree.
func pickWorktree(store *state.Store, views []*worktreeView) int {
	if len(views) == 0 {
		fmt.Println("no worktrees")
		return 0
	}
	labels := make([]string, len(views))
	for i, v := range views {
		labels[i] = fmt.Sprintf("%s\t%s %s %s %s", v.Path, v.State, v.Slug, v.Branch, v.OwnerPane)
	}
	var picked string
	if fzf, err := exec.LookPath("fzf"); err == nil {
		cmd := exec.Command(fzf, "--with-nth=2..", "--header=worktrees — enter to jump to pane")
		cmd.Stdin = strings.NewReader(strings.Join(labels, "\n") + "\n")
		out, err := cmd.Output()
		if err != nil {
			return 0
		}
		picked = strings.Fields(string(out))[0]
	} else {
		for i, l := range labels {
			fmt.Printf("%2d) %s\n", i+1, l)
		}
		fmt.Print("jump to # ")
		var n int
		if _, err := fmt.Scan(&n); err != nil || n < 1 || n > len(views) {
			return 0
		}
		picked = views[n-1].Path
	}
	for _, v := range views {
		if v.Path == canonPath(picked) {
			target := v.OwnerPane
			if !paneIDPattern.MatchString(target) && len(v.Panes) > 0 {
				target = v.Panes[0]
			}
			if paneIDPattern.MatchString(target) {
				return jumpToPane(store, target)
			}
			fmt.Fprintf(os.Stderr, "beacon: worktree %s has no pane to jump to\n", picked)
			return 1
		}
	}
	return 1
}

// ── wt: generic worktree workspace lifecycle ────────────────────────────
//
// A "workspace" = git worktree + deterministic ports + project hooks.
// beacon knows nothing about project internals; the repo declares hooks as
// executables under <root>/wt.d/ (or <root>/local/wt.d/):
//
//	ports    plain text, one service name per line → WT_PORT_<NAME>
//	seed     runs once at `wt new` (write config, create db, install deps)
//	start    bring the stack up (local, remote, anywhere — script's choice)
//	stop     graceful shutdown before teardown
//	destroy  deep cleanup on `wt close --destroy` (drop db, remote purge)
//
// All hooks run with cwd=worktree and env: WT_PATH WT_ROOT WT_SLUG WT_PANE
// WT_PORT (entry port) WT_PORT_<NAME> for each ports file entry.
// Port/slug derivation MUST match devproxy (shared fnv contract).

const (
	wtBasePort = 3000
	wtPortSpan = 4000
)

var wtNonDNS = regexp.MustCompile(`[^a-z0-9-]+`)

func wtSlug(absPath string) string {
	slug := wtNonDNS.ReplaceAllString(strings.ToLower(filepath.Base(absPath)), "-")
	if slug = strings.Trim(slug, "-"); slug == "" {
		return "root"
	}
	return slug
}

func wtPort(absPath, name string) int {
	key := absPath
	if name != "" {
		key += "\x00" + name
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return wtBasePort + int(h.Sum32()%uint32(wtPortSpan))
}

// wtHookDir locates the project's hook dir: wt.d/ first, local/wt.d/ for
// repos whose personal files live under local/.
func wtHookDir(root string) string {
	for _, d := range []string{filepath.Join(root, "wt.d"), filepath.Join(root, "local", "wt.d")} {
		if info, err := os.Stat(d); err == nil && info.IsDir() {
			return d
		}
	}
	return ""
}

func wtPortNames(root string) []string {
	hd := wtHookDir(root)
	if hd == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(hd, "ports"))
	if err != nil {
		return nil
	}
	var names []string
	for _, line := range strings.Split(string(data), "\n") {
		if n := strings.TrimSpace(line); n != "" && !strings.HasPrefix(n, "#") {
			names = append(names, n)
		}
	}
	return names
}

// wtMainRoot resolves the main checkout of whatever dir sits inside —
// toplevel of a worktree is the worktree, so anchor via git-common-dir.
func wtMainRoot(dir string) string {
	top := gitToplevel(dir)
	if top == "" {
		return ""
	}
	if c := gitCommonDir(top); c != "" {
		return canonPath(filepath.Dir(c))
	}
	return canonPath(top)
}

func wtEnv(root, path string) []string {
	env := append(os.Environ(),
		"WT_PATH="+path,
		"WT_ROOT="+root,
		"WT_SLUG="+wtSlug(path),
		"WT_PORT="+strconv.Itoa(wtPort(path, "")),
	)
	for _, name := range wtPortNames(root) {
		key := "WT_PORT_" + strings.ToUpper(strings.NewReplacer("-", "_", " ", "_").Replace(name))
		env = append(env, key+"="+strconv.Itoa(wtPort(path, name)))
	}
	if p := wtOwnerPane(); p != "" {
		env = append(env, "WT_PANE="+p)
	}
	return env
}

// wtOwnerPane 解析调用者所在 tmux pane。交互 shell 有 TMUX_PANE；agent exec
// 环境（devin/agy 的工具进程）不继承它，沿 ppid 链上溯到 pane shell pid 反推。
func wtOwnerPane() string {
	if p := os.Getenv("TMUX_PANE"); p != "" {
		return p
	}
	out, err := exec.Command(tmuxBin(), "list-panes", "-a", "-F", "#{pane_id} #{pane_pid}").Output()
	if err != nil {
		return ""
	}
	paneByPID := map[int]string{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		if pid, err := strconv.Atoi(f[1]); err == nil {
			paneByPID[pid] = f[0]
		}
	}
	if len(paneByPID) == 0 {
		return ""
	}
	ppid := map[int]int{}
	if ps, err := exec.Command("ps", "-axo", "pid=,ppid=").Output(); err == nil {
		for _, line := range strings.Split(string(ps), "\n") {
			f := strings.Fields(line)
			if len(f) != 2 {
				continue
			}
			pid, _ := strconv.Atoi(f[0])
			p, _ := strconv.Atoi(f[1])
			ppid[pid] = p
		}
	}
	for pid, ok := ppid[os.Getpid()]; ok && pid > 1; pid, ok = ppid[pid] {
		if pane, hit := paneByPID[pid]; hit {
			return pane
		}
	}
	return ""
}

func runWtHook(hookDir, name, worktree string, env []string) error {
	hook := filepath.Join(hookDir, name)
	info, err := os.Stat(hook)
	if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		return nil // absent or non-executable = no hook
	}
	fmt.Fprintf(os.Stderr, "wt: hook %s\n", name)
	cmd := exec.Command(hook)
	cmd.Dir = worktree
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func cmdWt(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: beacon wt <new|list|close|env>")
		return 2
	}
	switch args[0] {
	case "new":
		return cmdWtNew(args[1:])
	case "list":
		return cmdWorktrees(args[1:])
	case "env":
		return cmdWtEnv()
	case "close":
		return cmdWtClose(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "beacon: unknown wt subcommand %q\n", args[0])
		return 2
	}
}

func cmdWtNew(args []string) int {
	name, from := "", "HEAD"
	for i := 0; i < len(args); i++ {
		if args[i] == "--from" && i+1 < len(args) {
			from = args[i+1]
			i++
		} else if name == "" {
			name = args[i]
		}
	}
	if name == "" {
		fmt.Fprintln(os.Stderr, "usage: beacon wt new <name> [--from ref]")
		return 2
	}
	cwd, _ := os.Getwd()
	root := wtMainRoot(cwd)
	if root == "" {
		fmt.Fprintln(os.Stderr, "beacon: not inside a git repo")
		return 1
	}
	dirName := strings.NewReplacer("/", "-", " ", "-").Replace(name)
	path := canonPath(filepath.Join(filepath.Dir(root), filepath.Base(root)+"-"+dirName))
	if _, err := os.Stat(path); err == nil {
		// 区分活工作区与残渣目录：前者直接报身份，后者提示手动清
		live := false
		if out, err := gitOut(root, "worktree", "list", "--porcelain"); err == nil {
			for _, e := range parseWorktreeList(out) {
				if e.path == path {
					live = true
				}
			}
		}
		if live {
			fmt.Fprintf(os.Stderr, "beacon: %s already exists as a live worktree\n", path)
		} else {
			fmt.Fprintf(os.Stderr, "beacon: %s already exists and is not a worktree (stale dir — remove it or pick another name)\n", path)
		}
		return 1
	}
	if err := gitRunSlow(root, "worktree", "add", path, "-b", name, from); err != nil {
		// Branch may already exist — attach to it instead of creating.
		if err := gitRunSlow(root, "worktree", "add", path, name); err != nil {
			fmt.Fprintf(os.Stderr, "beacon: git worktree add failed (branch %q)\n", name)
			return 1
		}
	}
	// 早注册：provisioning 要跑几分钟，期间 owner/路由记录就得在位，
	// 否则并发 close 看不到 owner 防护和占用信息
	wtRegisterRoute(path)
	env := wtEnv(root, path)
	hookDir := wtHookDir(root)
	if hookDir == "" {
		fmt.Fprintln(os.Stderr, "wt: no wt.d hooks found — route registered but nothing is serving on the entry port yet")
	} else {
		if err := runWtHook(hookDir, "seed", path, env); err != nil {
			fmt.Fprintf(os.Stderr, "beacon: seed hook failed — removing worktree\n")
			_ = gitRunSlow(root, "worktree", "remove", "--force", path)
			wtUnregisterRoute(path)
			return 1
		}
		if err := runWtHook(hookDir, "start", path, env); err != nil {
			fmt.Fprintf(os.Stderr, "beacon: start hook failed — worktree kept, stack may be partial\n")
		}
	}
	// 并发 close 可能在 hook 跑完后把目录端掉：注册/打 URL 前先确认还活着
	if _, err := os.Stat(path); err != nil {
		fmt.Fprintf(os.Stderr, "beacon: %s vanished during provisioning (concurrent close?) — skipping route registration\n", path)
		wtUnregisterRoute(path)
		return 1
	}
	fmt.Printf("%s  →  http://%s.dev.localhost:7999 (entry port %d)\n",
		path, wtSlug(path), wtPort(path, ""))
	return 0
}

// wtRegisterRoute/wtUnregisterRoute 经 devproxy 注册路由；register 顺带把
// owner pane 推给 beacon registry，通过 TMUX_PANE 把解析出的 pane 传下去。
func wtRegisterRoute(path string) {
	dp, err := exec.LookPath("devproxy")
	if err != nil {
		return
	}
	cmd := exec.Command(dp, "register", path)
	if p := wtOwnerPane(); p != "" {
		cmd.Env = append(os.Environ(), "TMUX_PANE="+p)
	}
	_ = cmd.Run()
}

func wtUnregisterRoute(path string) {
	if dp, err := exec.LookPath("devproxy"); err == nil {
		_ = exec.Command(dp, "unregister", path).Run()
	}
}

// wtClosePreflight 是 wt close / worktree close 共用的拒绝检查：
// 必须先于任何 stop 钩子运行——否则拒绝发生时服务已停、目录还在，是半拆状态。
// 返回退出码，0 = 放行。
func wtClosePreflight(v *worktreeView, force bool, livePanes map[string]bool) int {
	if v == nil || force {
		return 0
	}
	if len(v.Panes) > 0 {
		fmt.Fprintf(os.Stderr, "beacon: refused: %s occupied by panes %s (--force to override)\n",
			v.Path, strings.Join(v.Panes, ","))
		return 1
	}
	if v.Locked {
		fmt.Fprintf(os.Stderr, "beacon: refused: %s is locked (git worktree unlock %s)\n", v.Path, v.Path)
		return 1
	}
	if v.Dirty {
		fmt.Fprintf(os.Stderr, "beacon: refused: %s has uncommitted changes (--force to override):\n", v.Path)
		for _, f := range v.DirtyFiles {
			fmt.Fprintf(os.Stderr, "  %s\n", f)
		}
		return 1
	}
	// owner 防护：owner pane 活着且不是调用者 → 无提示拆除是事故路径
	if v.OwnerPane != "" && livePanes[v.OwnerPane] {
		if caller := wtOwnerPane(); caller != v.OwnerPane {
			fmt.Fprintf(os.Stderr, "beacon: refused: %s is owned by live pane %s (you are pane %q; --force to override)\n",
				v.Path, v.OwnerPane, caller)
			return 1
		}
	}
	return 0
}

// cmdWtClose runs project stop/destroy hooks, then the standard worktree
// teardown. Hooks run after close succeeds — they key off WT_SLUG/WT_PORT,
// not the (now removed) directory.
func cmdWtClose(args []string) int {
	var extra []string
	var target string
	destroy, force := false, false
	for _, a := range args {
		switch a {
		case "--destroy":
			destroy = true
		case "--force":
			force = true
			extra = append(extra, a)
		default:
			target = a
		}
	}
	if target == "" {
		fmt.Fprintln(os.Stderr, "usage: beacon wt close <name|path> [--destroy] [--force]")
		return 2
	}
	root := wtMainRoot(mustGetwd())
	if root == "" {
		fmt.Fprintln(os.Stderr, "beacon: not inside a git repo")
		return 1
	}
	path := canonPath(target)
	if !filepath.IsAbs(target) {
		path = canonPath(filepath.Join(filepath.Dir(root), filepath.Base(root)+"-"+target))
	}
	// 预检必须先于 stop 钩子：占用/锁/dirty/owner 拒绝时服务不该已停
	if reg, err := state.NewRegistryStore(defaultStateDir()); err == nil {
		views, livePanes := assembleWorktrees(reg)
		for _, v := range views {
			if v.Path == path {
				if code := wtClosePreflight(v, force, livePanes); code != 0 {
					return code
				}
				break
			}
		}
	}
	env := wtEnv(root, path)
	hookDir := wtHookDir(root)
	if hookDir != "" {
		if err := runWtHook(hookDir, "stop", filepath.Dir(path), env); err != nil {
			fmt.Fprintf(os.Stderr, "beacon: wt stop hook failed: %v\n", err)
		}
	}
	code := cmdWorktree(append([]string{"close", path}, extra...))
	if code != 0 {
		return code
	}
	if hookDir != "" && destroy {
		if err := runWtHook(hookDir, "destroy", filepath.Dir(path), env); err != nil {
			fmt.Fprintf(os.Stderr, "beacon: wt destroy hook failed: %v\n", err)
		}
	}
	return 0
}

func cmdWtEnv() int {
	root := wtMainRoot(mustGetwd())
	if root == "" {
		fmt.Fprintln(os.Stderr, "beacon: not inside a git repo")
		return 1
	}
	for _, e := range wtEnv(root, canonPath(mustGetwd())) {
		if strings.HasPrefix(e, "WT_") {
			fmt.Println(e)
		}
	}
	return 0
}

func mustGetwd() string {
	d, _ := os.Getwd()
	return d
}

// reconcileLiveTabs drops registry tab records whose CDP target is gone.
// Best-effort: when the argus endpoint is unreachable every record is kept —
// absence of the endpoint must not nuke the registry.
func reconcileLiveTabs(reg *state.RegistryStore, recs []state.ResourceRecord) []state.ResourceRecord {
	port := os.Getenv("ARGUS_CDP_PORT")
	if port == "" {
		port = "9224"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET",
		fmt.Sprintf("http://127.0.0.1:%s/json/list", port), nil)
	if err != nil {
		return recs
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return recs
	}
	defer resp.Body.Close()
	var targets []struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&targets); err != nil {
		return recs
	}
	live := map[string]bool{}
	for _, t := range targets {
		live[t.ID] = true
	}
	out := recs[:0]
	for _, r := range recs {
		if r.Producer == "argus" && !live[r.ID] {
			_ = reg.Remove(r.Type, r.ID)
			continue
		}
		out = append(out, r)
	}
	return out
}

// pickTab is the tmux-popup selector: fzf when available, else a numbered
// prompt. Selection jumps through the producer's activation path.
func pickTab(store *state.Store, recs []state.ResourceRecord) int {
	if len(recs) == 0 {
		fmt.Println("no agent tabs")
		return 0
	}
	now := nowSeconds()
	labels := make([]string, len(recs))
	for i, r := range recs {
		title, _ := r.Metadata["title"].(string)
		u, _ := r.Metadata["url"].(string)
		label := title
		if label == "" {
			label = u
		} else if u != "" {
			label += "  " + u
		}
		labels[i] = fmt.Sprintf("%s\tpane=%s age=%ds %s", r.ID, r.OwnerPane, now-r.CreatedAt, label)
	}

	var pickedID string
	if fzf, err := exec.LookPath("fzf"); err == nil {
		cmd := exec.Command(fzf, "--with-nth=2..", "--header=agent tabs — enter to jump")
		cmd.Stdin = strings.NewReader(strings.Join(labels, "\n") + "\n")
		out, err := cmd.Output()
		if err != nil {
			return 0 // cancelled
		}
		pickedID = strings.Fields(string(out))[0]
	} else {
		for i, l := range labels {
			fmt.Printf("%2d) %s\n", i+1, l)
		}
		fmt.Print("jump to # ")
		var n int
		if _, err := fmt.Scan(&n); err != nil || n < 1 || n > len(recs) {
			return 0
		}
		pickedID = recs[n-1].ID
	}
	return jumpToTab(store, pickedID)
}

// jumpToTab activates a browser tab through its producer's activation CLI
// (argus owns CDP). If the producer is unavailable or activation fails,
// fall back to jumping to the owning pane so the user still lands somewhere
// relevant.
func jumpToTab(store *state.Store, tabID string) int {
	reg, err := state.NewRegistryStore(defaultStateDir())
	if err != nil {
		return 1
	}
	var rec *state.ResourceRecord
	for _, r := range reg.List("tab", "", false) {
		if r.ID == tabID {
			c := r
			rec = &c
			break
		}
	}
	if rec == nil {
		fmt.Fprintf(os.Stderr, "beacon: no registered tab %q\n", tabID)
		return 1
	}
	if argus, err := exec.LookPath("argus"); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Require the success marker — an argus binary that predates the
		// activate subcommand exits 0 without doing anything.
		out, err := exec.CommandContext(ctx, argus, "activate", tabID).Output()
		if err == nil && strings.Contains(string(out), "activated "+tabID) {
			return 0
		}
	}
	if rec.OwnerPane != "" && paneIDPattern.MatchString(rec.OwnerPane) {
		return jumpToPane(store, rec.OwnerPane)
	}
	fmt.Fprintf(os.Stderr, "beacon: tab %s activation failed and owner pane %q is gone\n", tabID, rec.OwnerPane)
	return 1
}

// jumpToPane queries the live pane's session and window from live tmux,
// then jumps to it using switch-client -c with an appropriate client.
// The pane ID must already be validated.
// Fail-closed: only acknowledges after a successful jump.
func jumpToPane(store *state.Store, pane string) int {
	tb := tmuxBin()
	// Query session, window, and pane ID from live tmux.
	// Use | separator (consistent with list-clients); tmux identifiers
	// never contain |.
	out, err := exec.Command(tb, "display-message", "-p", "-t", pane, "#{session_name}|#{window_id}|#{pane_id}").Output()
	if err != nil {
		fmt.Fprintf(os.Stderr, "beacon: pane %s not live: %v\n", pane, err)
		return 1
	}
	parts := strings.SplitN(strings.TrimSpace(string(out)), "|", 3)
	if len(parts) < 3 {
		fmt.Fprintf(os.Stderr, "beacon: unexpected display-message output for %s: %q\n", pane, string(out))
		return 1
	}
	session, _, paneID := parts[0], parts[1], parts[2]
	if !switchToTarget(session, paneID) {
		fmt.Fprintf(os.Stderr, "beacon: jump to %s failed (no client or tmux error); bell preserved\n", pane)
		return 1
	}
	_ = ackPane(store, pane)
	return 0
}

// switchToTarget finds the best attached client and switches it to the
// target session/pane. Returns false if no client is attached or any
// tmux command fails. Fail-closed: every step must succeed.
// Client selection priority:
//  1. A client already in the target session.
//  2. The most recently active client (by client_activity).
func switchToTarget(session, paneID string) bool {
	tb := tmuxBin()
	// List clients: tty|session_name|activity_timestamp
	out, err := exec.Command(tb, "list-clients", "-F", "#{client_tty}|#{session_name}|#{client_activity}").Output()
	if err != nil {
		return false
	}
	type clientInfo struct {
		tty      string
		session  string
		activity int64
	}
	var clients []clientInfo
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "|", 3)
		if len(fields) < 3 {
			continue
		}
		act, _ := strconv.ParseInt(strings.TrimSpace(fields[2]), 10, 64)
		clients = append(clients, clientInfo{
			tty:      strings.TrimSpace(fields[0]),
			session:  strings.TrimSpace(fields[1]),
			activity: act,
		})
	}
	if len(clients) == 0 {
		return false
	}
	// Prefer a client already in the target session.
	// Among those, pick the most recently active.
	// If none in target session, pick the most recently active overall.
	var best *clientInfo
	for i := range clients {
		c := &clients[i]
		if c.session == session {
			if best == nil || best.session != session || c.activity > best.activity {
				best = c
			}
		} else if best == nil || (best.session != session && c.activity > best.activity) {
			best = c
		}
	}
	if best == nil {
		return false
	}
	// switch-client -c client_tty -t session — must succeed.
	if err := exec.Command(tb, "switch-client", "-c", best.tty, "-t", session).Run(); err != nil {
		fmt.Fprintf(os.Stderr, "beacon: switch-client -c %s -t %s failed: %v\n", best.tty, session, err)
		return false
	}
	// Query the window ID for the pane.
	winOut, err := exec.Command(tb, "display-message", "-p", "-t", paneID, "#{window_id}").Output()
	if err != nil {
		fmt.Fprintf(os.Stderr, "beacon: display-message for window of %s failed: %v\n", paneID, err)
		return false
	}
	windowID := strings.TrimSpace(string(winOut))
	if windowID != "" {
		if err := exec.Command(tb, "select-window", "-t", windowID).Run(); err != nil {
			fmt.Fprintf(os.Stderr, "beacon: select-window -t %s failed: %v\n", windowID, err)
			return false
		}
	}
	// select-pane — must succeed.
	if err := exec.Command(tb, "select-pane", "-t", paneID).Run(); err != nil {
		fmt.Fprintf(os.Stderr, "beacon: select-pane -t %s failed: %v\n", paneID, err)
		return false
	}
	return true
}

func cmdAcknowledge(args []string) int {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "beacon: acknowledge requires a pane ID\n")
		return 2
	}
	pane := args[0]
	store, err := state.NewStore(defaultStateDir())
	if err != nil {
		return 1
	}
	if err := ackPane(store, pane); err != nil {
		fmt.Fprintf(os.Stderr, "beacon: acknowledge: %v\n", err)
		return 1
	}
	return 0
}

// cmdAcknowledgeVisible acknowledges all pending notification panes that are
// currently visible to any attached tmux client. This is the unified hook
// handler: after-select-pane, after-select-window, and client-session-changed
// all call this. It runs one tmux list-clients, deduplicates visible pane IDs,
// acknowledges only those that have pending (unacknowledged) notifications,
// then syncs bells once. Detached sessions keep their bells.
func cmdAcknowledgeVisible(args []string) int {
	store, err := state.NewStore(defaultStateDir())
	if err != nil {
		return 1
	}

	// Get all pending notification panes (unacknowledged waiting/blocked/completed).
	pending := store.PendingNotifications()
	if len(pending) == 0 {
		return 0
	}
	pendingSet := make(map[string]bool, len(pending))
	for _, p := range pending {
		pendingSet[p.Pane] = true
	}

	// One tmux call: list all attached clients and their visible pane.
	// Use | as separator — tmux format \t produces literal backslash-t, not a real tab.
	tb := tmuxBin()
	out, err := exec.Command(tb, "list-clients", "-F", "#{client_tty}|#{pane_id}").Output()
	if err != nil {
		return 0 // non-fatal: tmux not available
	}

	// Collect unique visible pane IDs that have pending notifications.
	visiblePending := make(map[string]bool)
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "|", 2)
		if len(fields) < 2 {
			continue
		}
		paneID := strings.TrimSpace(fields[1])
		if pendingSet[paneID] {
			visiblePending[paneID] = true
		}
	}

	if len(visiblePending) == 0 {
		return 0
	}

	// Acknowledge each visible pending pane.
	for paneID := range visiblePending {
		_ = store.Acknowledge(paneID)
	}

	// Sync bells once after all acknowledgements.
	syncBells(store)
	return 0
}

// ackPane tries the daemon socket first (fast path), falls back to direct file write.
// After acknowledging, syncs tmux bell options.
func ackPane(store *state.Store, pane string) error {
	socketPath := filepath.Join(defaultStateDir(), "beacon.sock")
	if err := daemon.SendAcknowledge(socketPath, pane); err == nil {
		syncBells(store)
		return nil
	}
	if err := store.Acknowledge(pane); err != nil {
		return err
	}
	syncBells(store)
	return nil
}

// cmdSyncBells synchronizes tmux user options @beacon_pane_bell (pane scope),
// @beacon_window_bell (window scope), and @beacon_session_bell (session scope)
// from the current Beacon state. Called after report/acknowledge/cleanup/reset.
// Uses three distinct option names to avoid tmux scope inheritance confusion.
func cmdSyncBells(args []string) int {
	store, err := state.NewStore(defaultStateDir())
	if err != nil {
		return 1
	}
	syncBells(store)
	return 0
}

// syncBells reads state and sets/unsets tmux user options at the correct scope.
// Pane options are set per-pane with -t pane. Window options with -t window.
// Session options with -t session. This is event-driven (called only on state
// change), not per-render.
func syncBells(store *state.Store) {
	tb := tmuxBin()
	st, err := store.Load()
	if err != nil {
		return
	}

	// Build sets of panes/windows/sessions with unacknowledged notifications.
	paneBell := map[string]bool{}    // pane_id -> has bell
	windowBell := map[string]bool{}  // window_id -> has bell
	sessionBell := map[string]bool{} // session_name -> has bell

	for paneID, rec := range st.Panes {
		if state.IsNotificationStatus(rec.Status) && !rec.Acknowledged {
			paneBell[paneID] = true
			if rec.Window != "" {
				windowBell[rec.Window] = true
			}
			if rec.Session != "" {
				sessionBell[rec.Session] = true
			}
		}
	}

	// Sync pane-scope options: set bell on notified panes, unset on others.
	// We iterate all live panes to clear stale bells on panes that no longer
	// have a notification.
	livePanes := listLivePanesWithTargets()
	for _, lp := range livePanes {
		if paneBell[lp.paneID] {
			_ = exec.Command(tb, "set-option", "-p", "-t", lp.paneID, "@beacon_pane_bell", "1").Run()
		} else {
			_ = exec.Command(tb, "set-option", "-p", "-t", lp.paneID, "-u", "@beacon_pane_bell").Run()
		}
	}

	// Sync window-scope options.
	liveWindows := listLiveWindows()
	for _, wid := range liveWindows {
		if windowBell[wid] {
			_ = exec.Command(tb, "set-option", "-w", "-t", wid, "@beacon_window_bell", "1").Run()
		} else {
			_ = exec.Command(tb, "set-option", "-w", "-t", wid, "-u", "@beacon_window_bell").Run()
		}
	}

	// Sync session-scope options (no -g; -t targets the specific session).
	liveSessions := listLiveSessions()
	for _, sname := range liveSessions {
		if sessionBell[sname] {
			_ = exec.Command(tb, "set-option", "-t", sname, "@beacon_session_bell", "1").Run()
		} else {
			_ = exec.Command(tb, "set-option", "-t", sname, "-u", "@beacon_session_bell").Run()
		}
	}
}

type livePaneTarget struct {
	paneID   string
	windowID string
	session  string
}

func listLivePanesWithTargets() []livePaneTarget {
	out, err := exec.Command(tmuxBin(), "list-panes", "-a", "-F", "#{pane_id}\t#{window_id}\t#{session_name}").Output()
	if err != nil {
		return nil
	}
	var panes []livePaneTarget
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 3 {
			continue
		}
		panes = append(panes, livePaneTarget{paneID: parts[0], windowID: parts[1], session: parts[2]})
	}
	return panes
}

func listLiveWindows() []string {
	out, err := exec.Command(tmuxBin(), "list-windows", "-a", "-F", "#{window_id}").Output()
	if err != nil {
		return nil
	}
	var windows []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			windows = append(windows, line)
		}
	}
	return windows
}

func listLiveSessions() []string {
	out, err := exec.Command(tmuxBin(), "list-sessions", "-F", "#{session_name}").Output()
	if err != nil {
		return nil
	}
	var sessions []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			sessions = append(sessions, line)
		}
	}
	return sessions
}

func cmdCleanup(args []string) int {
	store, err := state.NewStore(defaultStateDir())
	if err != nil {
		return 0
	}
	livePanes := listLivePanes()
	store.Cleanup(nowSeconds(), state.CompletedTTLSeconds, livePanes)
	if reg, err := state.NewRegistryStore(defaultStateDir()); err == nil {
		reg.MarkStaleForDeadPanes(livePanes)
	}
	syncBells(store)
	return 0
}

// cmdRegister records a pane-owned resource reported by an external
// producer: beacon register <type> <id> [--pane %N] [--data JSON] [--producer name]
func cmdRegister(args []string) int {
	var positional []string
	pane, dataJSON, producer := "", "", ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--pane":
			if i+1 < len(args) {
				i++
				pane = args[i]
			}
		case "--data":
			if i+1 < len(args) {
				i++
				dataJSON = args[i]
			}
		case "--producer":
			if i+1 < len(args) {
				i++
				producer = args[i]
			}
		default:
			positional = append(positional, args[i])
		}
	}
	if len(positional) < 2 {
		fmt.Fprintln(os.Stderr, "beacon: register requires <type> <id>")
		return 2
	}
	if pane == "" {
		pane = os.Getenv("TMUX_PANE")
	}
	var meta map[string]any
	if dataJSON != "" {
		if err := json.Unmarshal([]byte(dataJSON), &meta); err != nil {
			fmt.Fprintf(os.Stderr, "beacon: register: invalid --data JSON: %v\n", err)
			return 2
		}
	}
	reg, err := state.NewRegistryStore(defaultStateDir())
	if err != nil {
		return 0 // non-fatal for producers
	}
	rec := state.ResourceRecord{
		Type:      positional[0],
		ID:        positional[1],
		OwnerPane: pane,
		Producer:  producer,
		Metadata:  meta,
	}
	if err := reg.Upsert(rec); err != nil {
		fmt.Fprintln(os.Stderr, "beacon: register:", err)
		return 1
	}
	return 0
}

func cmdUnregister(args []string) int {
	var positional []string
	for _, a := range args {
		if a != "--quiet" && a != "-q" {
			positional = append(positional, a)
		}
	}
	if len(positional) < 2 {
		fmt.Fprintln(os.Stderr, "beacon: unregister requires <type> <id>")
		return 2
	}
	reg, err := state.NewRegistryStore(defaultStateDir())
	if err != nil {
		return 0
	}
	if err := reg.Remove(positional[0], positional[1]); err != nil {
		return 1
	}
	return 0
}

// cmdResources lists pane-owned resources:
// beacon resources [--type tab] [--pane %5] [--stale] [--json]
func cmdResources(args []string) int {
	typ, pane := "", ""
	asJSON, staleOnly := false, false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--type":
			if i+1 < len(args) {
				i++
				typ = args[i]
			}
		case "--pane":
			if i+1 < len(args) {
				i++
				pane = args[i]
			}
		case "--stale":
			staleOnly = true
		case "--json":
			asJSON = true
		}
	}
	reg, err := state.NewRegistryStore(defaultStateDir())
	if err != nil {
		return 1
	}
	recs := reg.List(typ, pane, staleOnly)
	if asJSON {
		out, _ := json.Marshal(recs)
		fmt.Println(string(out))
		return 0
	}
	if len(recs) == 0 {
		fmt.Println("no resources")
		return 0
	}
	now := nowSeconds()
	for _, r := range recs {
		age := now - r.CreatedAt
		label := ""
		if t, ok := r.Metadata["title"].(string); ok && t != "" {
			label = t
		} else if u, ok := r.Metadata["url"].(string); ok {
			label = u
		}
		staleMark := ""
		if r.Stale {
			staleMark = " [stale]"
		}
		fmt.Printf("%-10s %-42s pane=%-4s by=%-10s age=%-4ds %s%s\n",
			r.Type, r.ID, r.OwnerPane, r.Producer, age, label, staleMark)
	}
	return 0
}

func cmdReset(args []string) int {
	store, err := state.NewStore(defaultStateDir())
	if err != nil {
		return 1
	}
	if err := store.Reset(); err != nil {
		return 1
	}
	syncBells(store)
	return 0
}

func cmdHook(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "beacon: hook requires an event name")
		return 2
	}
	event := args[0]
	switch event {
	case "prompt", "stop", "notification", "permission":
	default:
		fmt.Fprintf(os.Stderr, "beacon: unsupported hook: %s\n", event)
		return 2
	}
	// Read stdin (hook payload). Non-fatal on parse errors.
	input, _ := io.ReadAll(os.Stdin)
	var payload map[string]interface{}
	_ = json.Unmarshal(input, &payload)

	strVal := func(key string) string {
		if payload == nil {
			return ""
		}
		if v, ok := payload[key]; ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
		return ""
	}

	cwd := strVal("cwd")
	if cwd == "" {
		cwd, _ = os.Getwd()
	}

	// Pantheon bridge is best-effort: load config once, use it per-event.
	pantheonCfg := pantheon.LoadConfig()

	switch event {
	case "prompt":
		prompt := strVal("prompt")
		if prompt == "" {
			prompt = strVal("user_prompt")
		}
		if prompt == "" {
			prompt = strVal("input")
		}
		runReport("working", prompt, cwd)
		if pantheonCfg.Enabled {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, err := pantheon.RegisterAgent(ctx, pantheonCfg, pantheon.AgentInfo{
				Runtime: detectRuntimeName(),
				Cwd:     cwd,
				Prompt:  prompt,
			})
			if err != nil {
				logHookError(fmt.Sprintf("pantheon register: %v", err))
			}
		}
	case "stop":
		msg := strVal("last_assistant_message")
		if msg == "" {
			msg = strVal("last-assistant-message")
		}
		if msg == "" {
			msg = strVal("message")
		}
		if msg == "" {
			msg = "completed"
		}
		cleanMsg := state.StripMarkdown(msg)
		cleanMsg = state.SanitizeSummary(cleanMsg)
		runReport("completed", cleanMsg, cwd)
		runNotify(detectAgentName(), "✓ "+cleanMsg)
		if pantheonCfg.Enabled {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			paneID := pantheon.GetCurrentPaneID()
			agentID := pantheon.GetAgentIDForPane(paneID)
			if err := pantheon.CompleteAgent(ctx, pantheonCfg, agentID); err != nil {
				logHookError(fmt.Sprintf("pantheon complete: %v", err))
			}
		}
	case "notification":
		msg := strVal("message")
		if msg == "" {
			msg = strVal("notification")
		}
		if msg == "" {
			msg = "Agent is waiting"
		}
		cleanMsg := state.StripMarkdown(msg)
		cleanMsg = state.SanitizeSummary(cleanMsg)
		runReport("waiting", cleanMsg, cwd)
		runNotify(detectAgentName(), "⚠ "+cleanMsg)
	case "permission":
		tool := strVal("tool_name")
		if tool == "" {
			tool = strVal("tool")
		}
		if tool == "" {
			tool = "operation"
		}
		msg := "Permission required: " + tool
		cleanMsg := state.StripMarkdown(msg)
		cleanMsg = state.SanitizeSummary(cleanMsg)
		runReport("waiting", cleanMsg, cwd)
		runNotify(detectAgentName(), cleanMsg)
	}
	return 0
}

// runReport is a thin wrapper around cmdReport logic for hooks.
func runReport(status, summary, cwd string) {
	pane := os.Getenv("TMUX_PANE")
	if pane == "" {
		return
	}
	session, window := tmuxContext(pane)
	now := nowSeconds()
	summary = state.SanitizeSummary(summary)

	sock := defaultSocketPath()
	if daemon.IsRunning(sock) {
		req := daemon.SocketRequest{
			Action:  "report",
			Pane:    pane,
			Status:  status,
			Summary: summary,
			Window:  window,
			Session: session,
			Cwd:     cwd,
			Time:    now,
		}
		if err := daemon.SendReport(sock, req); err == nil {
			if status == "completed" {
				_ = daemon.SendReport(sock, daemon.SocketRequest{
					Action:  "set-last",
					Pane:    pane,
					Session: session,
					Window:  window,
					Summary: summary,
					Time:    now,
				})
			}
			if st, err := state.NewStore(defaultStateDir()); err == nil {
				syncBells(st)
			}
			return
		}
	}

	store, err := state.NewStore(defaultStateDir())
	if err != nil {
		return
	}
	rec := state.PaneRecord{
		Status:  status,
		Summary: summary,
		Window:  window,
		Session: session,
		Time:    now,
		Cwd:     cwd,
	}
	_ = store.SetPane(pane, rec)
	if status == "completed" {
		_ = store.SetLast(state.LastCompleted{
			Pane:    pane,
			Session: session,
			Window:  window,
			Summary: summary,
			Time:    now,
		})
	}
	syncBells(store)
}

// runNotify sends a desktop notification. On macOS it prefers
// terminal-notifier (if installed) with a -execute action that jumps to
// the originating tmux pane and activates the terminal app. If
// terminal-notifier is not available, it falls back to osascript.
// The pane ID is read from TMUX_PANE; if absent, a plain notification
// without a click action is sent.
func runNotify(title, message string) {
	if os.Getenv(envNotify) == "0" {
		return
	}
	pane := os.Getenv("TMUX_PANE")
	switch runtime.GOOS {
	case "darwin":
		if path, err := exec.LookPath("terminal-notifier"); err == nil {
			args := []string{
				"-title", title,
				"-message", message,
				"-sound", "default",
			}
			if paneIDPattern.MatchString(pane) {
				args = append(args, "-group", pane)
				args = append(args, "-execute", buildExecuteAction(pane))
			}
			_ = exec.Command(path, args...).Run()
		} else {
			_ = exec.Command("osascript", "-e", "on run argv", "-e", "display notification (item 2 of argv) with title (item 1 of argv)", "-e", "end run", "--", title, message).Run()
		}
	case "linux":
		_ = exec.Command("notify-send", "--", title, message).Run()
	}
}

// shellQuote wraps a string in POSIX single quotes, escaping any
// embedded single quotes. This makes any string safe for shell embedding.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// buildExecuteAction constructs a shell-safe command string for
// terminal-notifier's -execute flag. The action activates the terminal
// app (if recognized) and jumps to the given tmux pane.
// Uses the absolute path of the current beacon binary (via os.Executable)
// so it works even when the notification shell's PATH doesn't include
// ~/.local/bin. All arguments are POSIX shell-quoted.
func buildExecuteAction(pane string) string {
	var actions []string
	app := detectTerminalApp()
	switch app {
	case "ghostty":
		actions = append(actions, shellQuote("/usr/bin/open")+" -a "+shellQuote("Ghostty"))
	}
	// Use absolute path to beacon binary, not PATH lookup.
	beaconPath, err := os.Executable()
	if err != nil {
		beaconPath = "beacon" // fallback, but absolute is preferred
	}
	// All arguments are shell-quoted.
	actions = append(actions, shellQuote(beaconPath)+" jump "+shellQuote(pane))
	return strings.Join(actions, " && ")
}

// detectTerminalApp determines the terminal application name.
// It first checks TERM_PROGRAM (case-insensitive). If the value is
// "tmux" or empty and TMUX is set (we're inside tmux), it queries
// tmux's global environment for TERM_PROGRAM, which holds the real
// terminal app (e.g. ghostty) that launched tmux.
func detectTerminalApp() string {
	tp := strings.ToLower(strings.TrimSpace(os.Getenv("TERM_PROGRAM")))
	if tp != "" && tp != "tmux" {
		return tp
	}
	// Inside tmux: query global environment for the real TERM_PROGRAM.
	if os.Getenv("TMUX") == "" {
		return tp
	}
	out, err := exec.Command(tmuxBin(), "show-environment", "-g", "TERM_PROGRAM").Output()
	if err != nil {
		return tp
	}
	// tmux show-environment output: "-TERM_PROGRAM=ghostty" or "TERM_PROGRAM=ghostty"
	line := strings.TrimSpace(string(out))
	// Strip leading "-" (unset marker shouldn't appear, but be safe).
	line = strings.TrimPrefix(line, "-")
	if idx := strings.Index(line, "="); idx >= 0 {
		val := strings.TrimSpace(line[idx+1:])
		if val != "" && strings.ToLower(val) != "tmux" {
			return strings.ToLower(val)
		}
	}
	return tp
}

func cmdDaemon(args []string) int {
	action := "start"
	if len(args) > 0 {
		action = args[0]
	}
	sock := defaultSocketPath()
	switch action {
	case "start":
		if daemon.IsRunning(sock) {
			fmt.Println("beacon: daemon already running")
			return 0
		}
		cfg := daemon.Config{
			StateDir:   defaultStateDir(),
			CacheDir:   defaultCacheDir(),
			SocketPath: sock,
			Interval:   4 * time.Second,
			OS:         runtime.GOOS,
			TmuxBin:    tmuxBin(),
		}
		d, err := daemon.New(cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "beacon: %v\n", err)
			return 1
		}
		// Write pid file.
		_ = os.WriteFile(sock+".pid", []byte(fmt.Sprintf("%d", os.Getpid())), 0o600)
		defer os.Remove(sock + ".pid")
		if err := d.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "beacon: %v\n", err)
			return 1
		}
		return 0
	case "stop":
		if !daemon.IsRunning(sock) {
			// Try pid file fallback.
			if err := daemon.KillDaemon(sock); err != nil {
				fmt.Fprintln(os.Stderr, "beacon: daemon not running")
				return 1
			}
			fmt.Println("beacon: daemon stopped")
			return 0
		}
		// Send stop via socket? We don't have a stop action. Use pid file.
		if err := daemon.KillDaemon(sock); err != nil {
			fmt.Fprintf(os.Stderr, "beacon: %v\n", err)
			return 1
		}
		fmt.Println("beacon: daemon stopped")
		return 0
	case "status":
		if daemon.IsRunning(sock) {
			fmt.Println("running")
			return 0
		}
		fmt.Println("stopped")
		return 1
	default:
		fmt.Fprintf(os.Stderr, "beacon: unknown daemon action: %s\n", action)
		return 2
	}
}

func cmdAgents(args []string) int {
	jsonOut := false
	for _, a := range args {
		if a == "--json" || a == "-j" {
			jsonOut = true
		}
	}
	sessions := agents.DiscoverAll(tmuxBin())
	if jsonOut {
		agents.PrintJSON(sessions)
	} else {
		agents.PrintTable(sessions)
	}
	return 0
}

func cmdDoctor(args []string) int {
	failed := 0
	// Binary self-check.
	fmt.Println("ok      beacon")
	// tmux.
	if _, err := exec.LookPath(tmuxBin()); err == nil {
		fmt.Println("ok      tmux")
	} else {
		fmt.Println("missing tmux")
		failed = 1
	}
	// State.
	store, err := state.NewStore(defaultStateDir())
	if err != nil {
		fmt.Println("failed  state")
		failed = 1
	} else if _, err := store.Load(); err != nil {
		fmt.Println("failed  state")
		failed = 1
	} else {
		fmt.Println("ok      state")
	}
	// Daemon + socket.
	sock := defaultSocketPath()
	if daemon.IsRunning(sock) {
		fmt.Println("ok      daemon")
		fmt.Println("ok      socket")
	} else {
		fmt.Println("info    daemon-not-running")
	}
	// Cache freshness.
	snapshotPath := filepath.Join(defaultCacheDir(), "snapshot.json")
	if info, err := os.Stat(snapshotPath); err == nil {
		age := time.Since(info.ModTime())
		if age < 30*time.Second {
			fmt.Printf("ok      cache (%.0fs old)\n", age.Seconds())
		} else {
			fmt.Printf("stale   cache (%.0fs old)\n", age.Seconds())
		}
	} else {
		fmt.Println("missing cache")
	}
	// tmux environment.
	if os.Getenv("TMUX") != "" {
		fmt.Println("ok      tmux-environment")
	} else {
		fmt.Println("info    outside-tmux")
	}
	// Hook errors (last 24h).
	hookLog := filepath.Join(defaultStateDir(), "hook-errors.log")
	if info, err := os.Stat(hookLog); err == nil && time.Since(info.ModTime()) < 24*time.Hour {
		count := countHookErrors(hookLog, 24*time.Hour)
		if count > 0 {
			fmt.Printf("warn    hook-errors: %d in 24h\n", count)
		} else {
			fmt.Println("ok      hook-errors")
		}
	} else {
		fmt.Println("ok      hook-errors")
	}
	// Pantheon socket (optional integration).
	pantheonSock := os.Getenv("PANTHEON_SOCKET")
	if pantheonSock == "" {
		home, _ := os.UserHomeDir()
		pantheonSock = home + "/.local/share/pantheon/pantheond.sock"
	}
	if _, err := os.Stat(pantheonSock); err == nil {
		fmt.Printf("ok      pantheon (%s)\n", pantheonSock)
	} else {
		fmt.Println("info    pantheon-not-running")
	}
	return failed
}

// countHookErrors counts lines in the hook error log within the given duration.
func countHookErrors(path string, window time.Duration) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	cutoff := time.Now().Add(-window)
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		if len(line) == 0 {
			continue
		}
		// Lines start with ISO timestamp: 2026-08-21T12:00:00+08:00
		if ts, err := time.Parse(time.RFC3339, strings.SplitN(line, " ", 2)[0]); err == nil {
			if ts.After(cutoff) {
				count++
			}
		}
	}
	return count
}

// detectAgentName determines the agent name from environment variables.
func detectAgentName() string {
	if name := os.Getenv("BEACON_AGENT_NAME"); name != "" {
		return name
	}
	if os.Getenv("CODEX_THREAD_ID") != "" || os.Getenv("CODEX_CI") != "" {
		return "Codex"
	}
	if os.Getenv("CLAUDE_CODE_ENTRYPOINT") != "" {
		return "Claude"
	}
	return "Agent"
}

// detectRuntimeName returns the Pantheon runtime identifier for the current
// environment. It mirrors detectAgentName but yields the lowercase runtime
// name expected by Pantheon (devin/claude/codex).
func detectRuntimeName() string {
	if os.Getenv("CODEX_THREAD_ID") != "" || os.Getenv("CODEX_CI") != "" {
		return "codex"
	}
	if os.Getenv("CLAUDE_CODE_ENTRYPOINT") != "" {
		return "claude"
	}
	return "devin"
}

// logHookError appends a timestamped message to the hook-errors log in the
// Beacon state directory. It is best-effort and never returns an error.
func logHookError(msg string) {
	stateDir := defaultStateDir()
	if stateDir == "" {
		return
	}
	path := filepath.Join(stateDir, "hook-errors.log")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", time.Now().Format(time.RFC3339), msg)
}
