// Package agents discovers live agent sessions across tmux panes.
package agents

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// AgentSession is a single discovered agent session bound to a tmux pane.
type AgentSession struct {
	Pane      string `json:"pane"`
	Session   string `json:"session"`    // tmux session name
	Window    string `json:"window"`     // tmux window
	Agent     string `json:"agent"`      // devin, claude, codex, agy
	SessionID string `json:"session_id"` // agent's internal session ID
	Title     string `json:"title"`      // session title/description
	Cwd       string `json:"cwd"`        // working directory
	PID       int    `json:"pid"`        // agent main process PID
}

// PaneInfo is a tmux pane's basic identity.
type PaneInfo struct {
	ID      string
	PID     int
	Session string
	Window  string
	Cmd     string
}

// ListPanes returns all tmux panes with their PID and current command.
func ListPanes(tmuxBin string) ([]PaneInfo, error) {
	if tmuxBin == "" {
		tmuxBin = "tmux"
	}
	out, err := exec.Command(tmuxBin, "list-panes", "-a", "-F",
		"#{pane_id}|#{pane_pid}|#{session_name}|#{window_index}|#{pane_current_command}").Output()
	if err != nil {
		return nil, fmt.Errorf("agents: tmux list-panes: %w", err)
	}
	var panes []PaneInfo
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) < 5 {
			continue
		}
		pid, _ := strconv.Atoi(parts[1])
		panes = append(panes, PaneInfo{
			ID:      parts[0],
			PID:     pid,
			Session: parts[2],
			Window:  parts[3],
			Cmd:     parts[4],
		})
	}
	return panes, nil
}

// procTable 是一次 `ps` 快照：pid→{ppid,cmd} 与 ppid→children 的内存索引。
// 逐 pid 起 pgrep/ps 既慢又在 macOS 上实测间歇性漏报子进程，必须一次取全表。
type procTable struct {
	procs    map[int]string
	children map[int][]int
}

func loadProcTable() *procTable {
	out, err := exec.Command("ps", "-axo", "pid=,ppid=,command=").Output()
	t := &procTable{procs: map[int]string{}, children: map[int][]int{}}
	if err != nil {
		return t
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			continue
		}
		t.procs[pid] = strings.Join(f[2:], " ")
		t.children[ppid] = append(t.children[ppid], pid)
	}
	return t
}

func (t *procTable) cmd(pid int) string      { return t.procs[pid] }
func (t *procTable) childPIDs(pid int) []int { return t.children[pid] }

// firstAgentDescendant 在 pane 进程树里找第一个已知 agent 进程名，
// 覆盖 agent 跑在 zsh 后台/子 shell 等前台 cmd 非 agent 的情形。
func (t *procTable) firstAgentDescendant(pid int) string {
	seen := map[int]bool{}
	var walk func(int) string
	walk = func(p int) string {
		for _, c := range t.children[p] {
			if seen[c] {
				continue
			}
			seen[c] = true
			f := strings.Fields(t.procs[c])
			if len(f) == 0 {
				continue
			}
			if b := filepath.Base(f[0]); agentCmds[b] {
				return b
			}
			if r := walk(c); r != "" {
				return r
			}
		}
		return ""
	}
	return walk(pid)
}

var agentCmds = map[string]bool{"devin": true, "claude": true, "codex": true, "agy": true}

// ── Devin discovery ───────────────────────────────────────────────────────

func discoverDevin(pane PaneInfo, pt *procTable) *AgentSession {
	// Chain: pane_pid → devin main → devin acp → session_locks/<id>.lock (contains acp pid)
	// Walk down the process tree to find "devin acp"
	var acpPID int
	var mainCmd string
	for _, child := range pt.childPIDs(pane.PID) {
		cmd := pt.cmd(child)
		if strings.Contains(cmd, "devin") {
			mainCmd = cmd
			// This is the devin main process; find its acp child
			for _, grandchild := range pt.childPIDs(child) {
				gcCmd := pt.cmd(grandchild)
				if strings.Contains(gcCmd, "devin") && strings.Contains(gcCmd, "acp") {
					acpPID = grandchild
					break
				}
			}
			if acpPID == 0 {
				// Some devin versions don't have a separate acp process
				acpPID = child
			}
			break
		}
	}
	if acpPID == 0 {
		return nil
	}

	// Scan session_locks for a file containing this PID; sweep stale locks
	// (dead owner pid) opportunistically — the dir grows unbounded otherwise.
	var sessionID string
	lockDir := filepath.Join(os.Getenv("HOME"), ".local", "share", "devin", "cli", "session_locks")
	if entries, err := os.ReadDir(lockDir); err == nil {
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".lock") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(lockDir, entry.Name()))
			if err != nil {
				continue
			}
			filePID, _ := strconv.Atoi(strings.TrimSpace(string(data)))
			if filePID == acpPID {
				sessionID = strings.TrimSuffix(entry.Name(), ".lock")
				continue
			}
			// 死锁清理：pid 死亡或已被非 devin 进程复用 → 删除
			if owner, alive := pt.procs[filePID]; !alive || !strings.Contains(owner, "devin") {
				_ = os.Remove(filepath.Join(lockDir, entry.Name()))
			}
		}
	}
	// 锁表查不到（pgrep 抖动、锁未写）时从主进程 cmdline 的 -r/--resume 兜底；
	// 仍拿不到也返回空 id 的条目——pane 里有 agent 这个事实不该丢。
	if sessionID == "" {
		sessionID = devinResumeID(mainCmd)
	}
	title, cwd := "", ""
	if sessionID != "" {
		title, cwd = devinSessionInfo(sessionID)
	}
	return &AgentSession{
		Pane:      pane.ID,
		Session:   pane.Session,
		Window:    pane.Window,
		Agent:     "devin",
		SessionID: sessionID,
		Title:     title,
		Cwd:       cwd,
		PID:       pane.PID,
	}
}

// devinResumeID extracts the session id from a `devin -r/--resume <id>` cmdline.
func devinResumeID(cmd string) string {
	fields := strings.Fields(cmd)
	for i, f := range fields {
		if (f == "-r" || f == "--resume") && i+1 < len(fields) && !strings.HasPrefix(fields[i+1], "-") {
			return fields[i+1]
		}
	}
	return ""
}

// devinSessionInfo queries the Devin sessions.db for title and cwd.
func devinSessionInfo(sessionID string) (title, cwd string) {
	// Validate sessionID to prevent SQL injection via the sqlite3 CLI.
	// Devin session IDs are UUID-like: hex digits and hyphens only.
	if !isSafeSessionID(sessionID) {
		return "", ""
	}
	dbPath := filepath.Join(os.Getenv("HOME"), ".local", "share", "devin", "cli", "sessions.db")
	// Use sqlite3 CLI to avoid CGO dependency.
	// sessionID is validated above to contain only [0-9a-f-].
	out, err := exec.Command("sqlite3", dbPath,
		"SELECT title, working_directory FROM sessions WHERE id='"+sessionID+"' LIMIT 1").Output()
	if err != nil {
		return "", ""
	}
	parts := strings.SplitN(strings.TrimSpace(string(out)), "|", 2)
	if len(parts) >= 1 {
		title = parts[0]
	}
	if len(parts) >= 2 {
		cwd = parts[1]
	}
	return
}

// isSafeSessionID returns true if s contains only characters safe for
// embedding in a SQL string literal: alphanumeric, hyphen, and underscore.
// Devin session IDs are human-readable names (e.g. "almond-chef"), not UUIDs.
func isSafeSessionID(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// ── Claude discovery ──────────────────────────────────────────────────────

func discoverClaude(pane PaneInfo) *AgentSession {
	// Claude stores sessions as .jsonl in ~/.claude/projects/<path-hash>/
	// The path-hash is derived from cwd but the exact encoding is an
	// internal implementation detail that may change between versions.
	// Instead of guessing the directory name, we scan all project dirs,
	// find the most recently modified .jsonl in each, read its cwd field,
	// and match against the pane's cwd.
	cwd := tmuxPaneCwd(pane.ID)
	if cwd == "" {
		return nil
	}
	projectsDir := filepath.Join(os.Getenv("HOME"), ".claude", "projects")
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return nil
	}
	var bestSession string
	var bestMod int64
	for _, dir := range entries {
		if !dir.IsDir() {
			continue
		}
		jsonlPath, modTime, ok := newestClaudeSession(filepath.Join(projectsDir, dir.Name()))
		if !ok {
			continue
		}
		if modTime <= bestMod {
			continue
		}
		// Read the cwd from the jsonl file
		fileCwd := claudeSessionCwd(jsonlPath)
		if fileCwd == "" {
			continue
		}
		if fileCwd == cwd {
			bestMod = modTime
			bestSession = filepath.Base(jsonlPath)
			bestSession = strings.TrimSuffix(bestSession, ".jsonl")
		}
	}
	if bestSession == "" {
		return nil
	}
	return &AgentSession{
		Pane:      pane.ID,
		Session:   pane.Session,
		Window:    pane.Window,
		Agent:     "claude",
		SessionID: bestSession,
		Title:     "",
		Cwd:       cwd,
		PID:       pane.PID,
	}
}

// newestClaudeSession returns the path and mod time of the most recently
// modified .jsonl file in the given directory.
func newestClaudeSession(dir string) (path string, modTime int64, ok bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", 0, false
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Unix() > modTime {
			modTime = info.ModTime().Unix()
			path = filepath.Join(dir, entry.Name())
			ok = true
		}
	}
	return
}

// claudeSessionCwd reads the first few lines of a .jsonl file and extracts
// the cwd field. Returns empty string if not found.
func claudeSessionCwd(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 64*1024)
	for i := 0; i < 50 && scanner.Scan(); i++ {
		var msg map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			continue
		}
		if cwd, ok := msg["cwd"].(string); ok && cwd != "" {
			return cwd
		}
	}
	return ""
}

// ── Codex discovery ───────────────────────────────────────────────────────

func discoverCodex(pane PaneInfo) *AgentSession {
	// Codex stores sessions in ~/.codex/sessions/YYYY/MM/DD/ as rollout-*.jsonl
	// The first line of each file is a session_meta JSON object containing
	// cwd and session_id. We match by cwd to find the pane's session.
	cwd := tmuxPaneCwd(pane.ID)
	if cwd == "" {
		return nil
	}
	sessionsDir := filepath.Join(os.Getenv("HOME"), ".codex", "sessions")
	var bestSession string
	var bestMod int64
	filepath.Walk(sessionsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !strings.HasPrefix(filepath.Base(path), "rollout-") {
			return nil
		}
		modTime := info.ModTime().Unix()
		if modTime <= bestMod {
			return nil
		}
		fileCwd, sid := codexSessionMeta(path)
		if fileCwd != cwd || sid == "" {
			return nil
		}
		bestMod = modTime
		bestSession = sid
		return nil
	})
	if bestSession == "" {
		return nil
	}
	return &AgentSession{
		Pane:      pane.ID,
		Session:   pane.Session,
		Window:    pane.Window,
		Agent:     "codex",
		SessionID: bestSession,
		Title:     "",
		Cwd:       cwd,
		PID:       pane.PID,
	}
}

// codexSessionMeta reads the first line of a Codex rollout file and extracts
// the cwd and session_id from the session_meta payload.
func codexSessionMeta(path string) (cwd, sessionID string) {
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 64*1024)
	if scanner.Scan() {
		var msg map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			return "", ""
		}
		if payload, ok := msg["payload"].(map[string]any); ok {
			if c, ok := payload["cwd"].(string); ok {
				cwd = c
			}
			if sid, ok := payload["session_id"].(string); ok {
				sessionID = sid
			}
		}
	}
	return
}

// ── AGy discovery ─────────────────────────────────────────────────────────

func discoverAGy(pane PaneInfo) *AgentSession {
	// AGy (another coding agent) does not expose queryable session storage.
	// We report the pane as an active AGy session with no internal session ID.
	// If AGy adds session storage in the future, this can be extended.
	return &AgentSession{
		Pane:      pane.ID,
		Session:   pane.Session,
		Window:    pane.Window,
		Agent:     "agy",
		SessionID: "",
		Title:     "",
		Cwd:       tmuxPaneCwd(pane.ID),
		PID:       pane.PID,
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────

func tmuxPaneCwd(paneID string) string {
	out, err := exec.Command("tmux", "display-message", "-p", "-t", paneID, "#{pane_current_path}").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// ── Public API ────────────────────────────────────────────────────────────

// DiscoverAll finds all agent sessions across all tmux panes.
func DiscoverAll(tmuxBin string) []AgentSession {
	panes, err := ListPanes(tmuxBin)
	if err != nil {
		return nil
	}
	pt := loadProcTable()

	var mu sync.Mutex
	var results []AgentSession
	var wg sync.WaitGroup

	for _, pane := range panes {
		pane := pane
		agent := pane.Cmd
		if !agentCmds[agent] {
			// 前台不是 agent：看进程树里有没有（agent 跑在后台/子 shell 的情形）
			agent = pt.firstAgentDescendant(pane.PID)
			if agent == "" {
				continue
			}
		}
		wg.Add(1)
		go func(agent string) {
			defer wg.Done()
			var s *AgentSession
			switch agent {
			case "devin":
				s = discoverDevin(pane, pt)
			case "claude":
				s = discoverClaude(pane)
			case "codex":
				s = discoverCodex(pane)
			case "agy":
				s = discoverAGy(pane)
			}
			if s != nil {
				mu.Lock()
				results = append(results, *s)
				mu.Unlock()
			}
		}(agent)
	}
	wg.Wait()
	return results
}

// PrintJSON writes agent sessions as JSON to stdout.
func PrintJSON(sessions []AgentSession) {
	data, _ := json.MarshalIndent(sessions, "", "  ")
	fmt.Println(string(data))
}

// PrintTable writes agent sessions as a human-readable table to stdout.
func PrintTable(sessions []AgentSession) {
	if len(sessions) == 0 {
		fmt.Fprintln(os.Stderr, "no agent sessions found")
		return
	}
	fmt.Printf("%-6s  %-8s  %-20s  %-40s  %s\n", "PANE", "AGENT", "SESSION_ID", "TITLE", "CWD")
	fmt.Printf("%-6s  %-8s  %-20s  %-40s  %s\n", "----", "-----", "----------", "-----", "---")
	for _, s := range sessions {
		title := s.Title
		if len(title) > 38 {
			title = title[:38] + ".."
		}
		fmt.Printf("%-6s  %-8s  %-20s  %-40s  %s\n", s.Pane, s.Agent, s.SessionID, title, s.Cwd)
	}
}
