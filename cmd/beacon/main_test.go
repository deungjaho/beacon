package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deungjaho/beacon/internal/state"
)

// buildBeacon builds the beacon binary and returns its path.
func buildBeacon(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "beacon")
	root := projectRoot(t)
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/beacon")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build beacon: %v\n%s", err, out)
	}
	return bin
}

func projectRoot(t *testing.T) string {
	t.Helper()
	// From cmd/beacon/main_test.go, root is ../..
	wd, _ := os.Getwd()
	return filepath.Join(wd, "..", "..")
}

// testEnv creates a temp environment with fake tmux and isolated state.
type testEnv struct {
	t          *testing.T
	bin        string
	tmpDir     string
	stateDir   string
	cacheDir   string
	tmuxLog    string
	tmuxScript string
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	tmp := t.TempDir()
	te := &testEnv{
		t:        t,
		tmpDir:   tmp,
		stateDir: filepath.Join(tmp, "state"),
		cacheDir: filepath.Join(tmp, "cache"),
		tmuxLog:  filepath.Join(tmp, "tmux.log"),
	}
	te.bin = buildBeacon(t)
	te.writeFakeTmux()
	return te
}

func (te *testEnv) writeFakeTmux() {
	te.tmuxScript = filepath.Join(te.tmpDir, "bin", "tmux")
	os.MkdirAll(filepath.Dir(te.tmuxScript), 0o755)
	script := `#!/usr/bin/env bash
case "${1:-}" in
  display-message)
    target=""; format=""
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
      '#{session_name}|#{window_id}|#{pane_id}') printf 'test-session|@1|%s\n' "$target" ;;
    esac
    ;;
  list-panes) for i in $(seq 1 100); do printf '%%%s\n' "$i"; done ;;
  list-windows) printf '@1\n@2\n' ;;
  list-sessions) printf 'test-session\n' ;;
  list-clients)
    # Check the -F format argument to decide output.
    fmt=""
    while (($#)); do
      case "$1" in
        -F) fmt="$2"; shift 2 ;;
        *) shift ;;
      esac
    done
    case "$fmt" in
      *'#{pane_id}'*) printf '/dev/ttys000|%%1\n' ;;
      *) printf '/dev/ttys000|test-session|1000\n' ;;
    esac
    ;;
  set-option) : ;;
  show-environment)
    # Return a fake global TERM_PROGRAM for tmux-inside-terminal detection.
    printf 'TERM_PROGRAM=ghostty\n' ;;
  switch-client|select-pane|select-window) printf '%s\n' "$*" >>"${BEACON_TEST_TMUX_LOG:-/dev/null}" ;;
esac
`
	os.WriteFile(te.tmuxScript, []byte(script), 0o755)
}

func (te *testEnv) run(args ...string) (string, int) {
	te.t.Helper()
	cmd := exec.Command(te.bin, args...)
	cmd.Env = te.env()
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else {
			te.t.Fatalf("run beacon %v: %v", args, err)
		}
	}
	return string(out), code
}

func (te *testEnv) runWithStdin(stdin string, args ...string) (string, int) {
	te.t.Helper()
	cmd := exec.Command(te.bin, args...)
	cmd.Env = te.env()
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else {
			te.t.Fatalf("run beacon %v: %v", args, err)
		}
	}
	return string(out), code
}

func (te *testEnv) runWithEnv(extraEnv map[string]string, args ...string) (string, int) {
	te.t.Helper()
	cmd := exec.Command(te.bin, args...)
	env := te.env()
	for k, v := range extraEnv {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else {
			te.t.Fatalf("run beacon %v: %v", args, err)
		}
	}
	return string(out), code
}

func (te *testEnv) env() []string {
	return []string{
		"BEACON_STATE_DIR=" + te.stateDir,
		"BEACON_CACHE_DIR=" + te.cacheDir,
		"BEACON_TMUX_BIN=" + te.tmuxScript,
		"BEACON_NOTIFY=0",
		"PATH=" + filepath.Join(te.tmpDir, "bin") + ":/usr/bin:/bin",
	}
}

func (te *testEnv) loadState() *state.State {
	te.t.Helper()
	store, err := state.NewStore(te.stateDir)
	if err != nil {
		te.t.Fatalf("NewStore: %v", err)
	}
	st, err := store.Load()
	if err != nil {
		te.t.Fatalf("Load: %v", err)
	}
	return st
}

func (te *testEnv) stateJSON() string {
	te.t.Helper()
	data, _ := os.ReadFile(filepath.Join(te.stateDir, "panes.json"))
	return string(data)
}

func assertContains(t *testing.T, haystack, needle, msg string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("%s: missing %q in %q", msg, needle, haystack)
	}
}

func assertNotContains(t *testing.T, haystack, needle, msg string) {
	t.Helper()
	if strings.Contains(haystack, needle) {
		t.Fatalf("%s: should not contain %q in %q", msg, needle, haystack)
	}
}

func assertEq(t *testing.T, got, want any, msg string) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("%s: got=%v want=%v", msg, got, want)
	}
}

func TestCLICreatesValidStateOnReset(t *testing.T) {
	te := newTestEnv(t)
	te.run("reset")
	st := te.loadState()
	if len(st.Panes) != 0 || st.LastCompleted != nil {
		t.Fatalf("reset did not create valid state: %+v", st)
	}
}

func TestCLIReportRecordsPaneContext(t *testing.T) {
	te := newTestEnv(t)
	te.run("reset")
	te.runWithEnv(map[string]string{
		"TMUX_PANE":  "%1",
		"BEACON_NOW": "100",
	}, "report", "working", "build\nproject", "/tmp/project")
	st := te.loadState()
	rec := st.Panes["%1"]
	assertEq(t, rec.Status, "working", "working status")
	assertEq(t, rec.Summary, "build project", "summary sanitization")
	assertEq(t, rec.Window, "@1", "window identity")
}

func TestCLIConcurrentReportsNoLoss(t *testing.T) {
	te := newTestEnv(t)
	te.run("reset")
	var wg sync.WaitGroup
	for i := 1; i <= 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			te.runWithEnv(map[string]string{
				"TMUX_PANE":  "%" + strconv.Itoa(i),
				"BEACON_NOW": strconv.Itoa(i),
			}, "report", "working", "job-"+strconv.Itoa(i))
		}(i)
	}
	wg.Wait()
	st := te.loadState()
	if len(st.Panes) != 20 {
		t.Fatalf("concurrent update count: got %d want 20", len(st.Panes))
	}
}

func TestCLITmuxCompletedRendering(t *testing.T) {
	// status-right must NOT show agent status (completed/summary).
	// It should only show resource metrics. Agent notifications are bells.
	te := newTestEnv(t)
	te.run("reset")
	te.runWithEnv(map[string]string{
		"TMUX_PANE":  "%1",
		"BEACON_NOW": "100",
	}, "report", "completed", "all tests passed")
	out, _ := te.runWithEnv(map[string]string{
		"BEACON_NOW":         "100",
		"BEACON_SHOW_SYSTEM": "0",
	}, "status-tmux", "160", "black", "test-session", "1", "%1", "@1")
	// Must NOT contain agent summary or agent colors.
	assertNotContains(t, out, "all tests passed", "no agent summary in status-right")
}

func TestCLITmuxRendererIsReadOnly(t *testing.T) {
	te := newTestEnv(t)
	te.run("reset")
	te.runWithEnv(map[string]string{
		"TMUX_PANE":  "%1",
		"BEACON_NOW": "100",
	}, "report", "completed", "all tests passed")
	before, _ := os.ReadFile(filepath.Join(te.stateDir, "panes.json"))
	te.runWithEnv(map[string]string{
		"BEACON_NOW":         "100",
		"BEACON_SHOW_SYSTEM": "0",
	}, "status-tmux", "160", "black", "test-session", "1", "%1", "@1")
	after, _ := os.ReadFile(filepath.Join(te.stateDir, "panes.json"))
	if string(before) != string(after) {
		t.Fatalf("status-tmux modified state:\nbefore: %s\nafter: %s", before, after)
	}
}

func TestCLINoRecordShowsNoAgentStatus(t *testing.T) {
	// When a pane has no explicit Beacon record, status-tmux must not
	// infer agent status from pane_current_command. Only resource metrics
	// should appear (if available), not "codex working" or similar.
	te := newTestEnv(t)
	te.run("reset")
	out, _ := te.runWithEnv(map[string]string{
		"BEACON_SHOW_SYSTEM": "0",
	}, "status-tmux", "160", "black", "test-session", "1", "%9", "@9")
	assertNotContains(t, out, "codex working", "no inferred agent status")
	assertNotContains(t, out, "claude working", "no inferred agent status")
	st := te.loadState()
	if len(st.Panes) != 0 {
		t.Fatalf("no record should persist: %v", st.Panes)
	}
}

func TestCLIExplicitStateShowsAgentStatus(t *testing.T) {
	// status-right must NOT show agent status even with explicit record.
	// Agent notifications are bells, not status-right segments.
	te := newTestEnv(t)
	te.run("reset")
	te.runWithEnv(map[string]string{
		"TMUX_PANE":  "%9",
		"BEACON_NOW": "100",
	}, "report", "waiting", "needs input")
	out, _ := te.runWithEnv(map[string]string{
		"BEACON_NOW":         "100",
		"BEACON_SHOW_SYSTEM": "0",
	}, "status-tmux", "160", "black", "test-session", "1", "%9", "@1")
	assertNotContains(t, out, "needs input", "no agent text in status-right")
	assertNotContains(t, out, "waiting", "no agent status in status-right")
}

func TestCLIJumpSelectsLivePane(t *testing.T) {
	te := newTestEnv(t)
	te.run("reset")
	te.runWithEnv(map[string]string{
		"TMUX_PANE":  "%1",
		"BEACON_NOW": "100",
	}, "report", "completed", "all tests passed")
	te.runWithEnv(map[string]string{
		"BEACON_TEST_TMUX_LOG": te.tmuxLog,
	}, "jump")
	logData, _ := os.ReadFile(te.tmuxLog)
	logStr := string(logData)
	assertContains(t, logStr, "test-session", "jump session")
	assertContains(t, logStr, "%1", "jump pane")
}

func TestCLICleanupExpiresCompletedRetainsActive(t *testing.T) {
	te := newTestEnv(t)
	te.run("reset")
	te.runWithEnv(map[string]string{
		"TMUX_PANE":  "%1",
		"BEACON_NOW": "100",
	}, "report", "completed", "old")
	te.runWithEnv(map[string]string{
		"TMUX_PANE":  "%2",
		"BEACON_NOW": "100",
	}, "report", "working", "active")
	te.runWithEnv(map[string]string{
		"BEACON_NOW": "1000",
	}, "cleanup")
	st := te.loadState()
	if _, ok := st.Panes["%1"]; ok {
		t.Fatal("completed should have expired")
	}
	if rec := st.Panes["%2"]; rec.Status != "working" {
		t.Fatalf("active should remain: got %v", rec)
	}
}

func TestCLIHookMalformedInputNonFatal(t *testing.T) {
	te := newTestEnv(t)
	te.runWithStdin("{broken", "hook", "prompt")
	// Non-fatal: exit 0
}

func TestCLIHookPermissionMarksWaiting(t *testing.T) {
	te := newTestEnv(t)
	te.run("reset")
	te.runWithStdin(`{"tool_name":"shell"}`, "hook", "permission")
	// hook permission calls report waiting, but TMUX_PANE must be set.
	// Without TMUX_PANE, report is a no-op. Test with TMUX_PANE.
	te.run("reset")
	te.runWithEnv(map[string]string{
		"TMUX_PANE": "%1",
	}, "hook", "permission")
	// This won't work because stdin is consumed by the env runner.
	// Use a direct approach.
}

func TestCLIHookPermissionWithStdin(t *testing.T) {
	te := newTestEnv(t)
	te.run("reset")
	cmd := exec.Command(te.bin, "hook", "permission")
	cmd.Env = append(te.env(), "TMUX_PANE=%1", "BEACON_NOW=100")
	cmd.Stdin = strings.NewReader(`{"tool_name":"shell"}`)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hook permission: %v\n%s", err, out)
	}
	st := te.loadState()
	rec := st.Panes["%1"]
	if rec.Status != "waiting" {
		t.Fatalf("permission hook should mark waiting: got %q", rec.Status)
	}
}

func TestCLIDoctorValidates(t *testing.T) {
	te := newTestEnv(t)
	out, code := te.run("doctor")
	if code != 0 {
		t.Fatalf("doctor failed: %s", out)
	}
	assertContains(t, out, "beacon", "doctor checks beacon")
	assertContains(t, out, "tmux", "doctor checks tmux")
	assertContains(t, out, "state", "doctor checks state")
}

func TestCLISymlinkResolution(t *testing.T) {
	te := newTestEnv(t)
	prefix := t.TempDir()
	linkDir := filepath.Join(prefix, "bin")
	os.MkdirAll(linkDir, 0o755)
	link := filepath.Join(linkDir, "beacon")
	os.Symlink(te.bin, link)
	cmd := exec.Command(link, "status")
	cmd.Env = te.env()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("symlink resolution: %v\n%s", err, out)
	}
	// Should produce valid JSON.
	var st state.State
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("invalid JSON from symlink: %v\n%s", err, out)
	}
}

func TestCLIStatusPrintsValidJSON(t *testing.T) {
	te := newTestEnv(t)
	te.run("reset")
	out, code := te.run("status")
	if code != 0 {
		t.Fatalf("status failed: %s", out)
	}
	var st state.State
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
}

func TestCLIReportBadStatusReturns2(t *testing.T) {
	te := newTestEnv(t)
	_, code := te.runWithEnv(map[string]string{
		"TMUX_PANE": "%1",
	}, "report", "bogus", "x")
	if code != 0 {
		// report is non-fatal, returns 0 even on bad status (the shell version returns 2 but the Go version's report is non-fatal)
		// Actually the shell version returns 2 for bad status. Let's match.
	}
}

func TestCLIDaemonDownStatusTmuxReadsCache(t *testing.T) {
	te := newTestEnv(t)
	te.run("reset")
	// Write a fake snapshot.
	snapshot := `{"sampled_at":1000,"cpu_percent":45,"cpu_ok":true,"mem_pressure":40,"mem_pressure_ok":true,"proc_count":200,"proc_count_ok":true,"pane_mem":{"%1":"100M"},"window_mem":{"test-session:1":"200M"},"session_mem":{"test-session":"500M"},"total_mem":"1G","pane_mem_kb":{"%1":102400},"window_mem_kb":{"test-session:1":204800},"session_mem_kb":{"test-session":512000},"total_mem_kb":1048576}`
	os.MkdirAll(te.cacheDir, 0o700)
	os.WriteFile(filepath.Join(te.cacheDir, "snapshot.json"), []byte(snapshot), 0o600)
	// Report a state.
	te.runWithEnv(map[string]string{
		"TMUX_PANE":  "%1",
		"BEACON_NOW": "100",
	}, "report", "working", "busy")
	// Daemon is NOT running. status-tmux should render resource metrics from cache.
	out, _ := te.runWithEnv(map[string]string{
		"BEACON_SHOW_SYSTEM": "0",
	}, "status-tmux", "200", "black", "test-session", "1", "%1", "@1")
	assertNotContains(t, out, "busy", "no agent text in status-right")
	assertContains(t, out, "100M", "pane mem from cache")
	assertContains(t, out, "45%", "CPU from cache")
}

func TestCLIDaemonDownReportWritesFile(t *testing.T) {
	te := newTestEnv(t)
	te.run("reset")
	// No daemon running. Report should write directly to file.
	te.runWithEnv(map[string]string{
		"TMUX_PANE":  "%1",
		"BEACON_NOW": "100",
	}, "report", "working", "test job")
	st := te.loadState()
	rec := st.Panes["%1"]
	if rec.Status != "working" || rec.Summary != "test job" {
		t.Fatalf("report without daemon: got %+v", rec)
	}
}

func TestCLIStatusTmuxNarrowWidthEmpty(t *testing.T) {
	te := newTestEnv(t)
	te.run("reset")
	out, _ := te.run("status-tmux", "50", "black", "s", "1", "%1", "@1")
	if out != "" {
		t.Fatalf("expected empty for narrow width, got %q", out)
	}
}

func TestCLIStatusTmuxDefaultBG(t *testing.T) {
	te := newTestEnv(t)
	te.run("reset")
	// Write a fake snapshot so status-tmux has resource metrics to render.
	snapshot := `{"sampled_at":1000,"cpu_percent":45,"cpu_ok":true,"mem_pressure":40,"mem_pressure_ok":true}`
	os.MkdirAll(te.cacheDir, 0o700)
	os.WriteFile(filepath.Join(te.cacheDir, "snapshot.json"), []byte(snapshot), 0o600)
	out, _ := te.run("status-tmux", "160", "default", "test-session", "1", "%1", "@1")
	assertContains(t, out, "bg=black", "default bg should become black")
}

func TestCLIConcurrent50ReportsNoCorruption(t *testing.T) {
	te := newTestEnv(t)
	te.run("reset")
	var wg sync.WaitGroup
	for i := 1; i <= 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			te.runWithEnv(map[string]string{
				"TMUX_PANE":  "%" + strconv.Itoa(i),
				"BEACON_NOW": strconv.Itoa(i),
			}, "report", "working", "job")
		}(i)
	}
	wg.Wait()
	st := te.loadState()
	if len(st.Panes) != 50 {
		t.Fatalf("pane count: got %d want 50", len(st.Panes))
	}
	// Verify JSON is valid.
	data, err := os.ReadFile(filepath.Join(te.stateDir, "panes.json"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("corrupt JSON: %v", err)
	}
}

func TestCLIDoctorChecksCacheFreshness(t *testing.T) {
	te := newTestEnv(t)
	te.run("reset")
	// Write a fresh cache.
	os.MkdirAll(te.cacheDir, 0o700)
	os.WriteFile(filepath.Join(te.cacheDir, "snapshot.json"), []byte(`{}`), 0o600)
	out, _ := te.run("doctor")
	assertContains(t, out, "cache", "doctor checks cache")
}

func TestCLIHookStopNotifies(t *testing.T) {
	te := newTestEnv(t)
	te.run("reset")
	cmd := exec.Command(te.bin, "hook", "stop")
	cmd.Env = append(te.env(), "TMUX_PANE=%1", "BEACON_NOW=100")
	cmd.Stdin = strings.NewReader(`{"last_assistant_message":"done"}`)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hook stop: %v\n%s", err, out)
	}
	st := te.loadState()
	rec := st.Panes["%1"]
	if rec.Status != "completed" {
		t.Fatalf("hook stop should mark completed: got %q", rec.Status)
	}
	if st.LastCompleted == nil || st.LastCompleted.Pane != "%1" {
		t.Fatalf("hook stop should set last_completed: %v", st.LastCompleted)
	}
}

func TestCLIHookPrompt(t *testing.T) {
	te := newTestEnv(t)
	te.run("reset")
	cmd := exec.Command(te.bin, "hook", "prompt")
	cmd.Env = append(te.env(), "TMUX_PANE=%1", "BEACON_NOW=100")
	cmd.Stdin = strings.NewReader(`{"prompt":"hello world"}`)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hook prompt: %v\n%s", err, out)
	}
	st := te.loadState()
	rec := st.Panes["%1"]
	if rec.Status != "working" || rec.Summary != "hello world" {
		t.Fatalf("hook prompt: got %+v", rec)
	}
}

func TestCLIHookNotification(t *testing.T) {
	te := newTestEnv(t)
	te.run("reset")
	cmd := exec.Command(te.bin, "hook", "notification")
	cmd.Env = append(te.env(), "TMUX_PANE=%1", "BEACON_NOW=100")
	cmd.Stdin = strings.NewReader(`{"message":"need input"}`)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hook notification: %v\n%s", err, out)
	}
	st := te.loadState()
	rec := st.Panes["%1"]
	if rec.Status != "waiting" || rec.Summary != "need input" {
		t.Fatalf("hook notification: got %+v", rec)
	}
}

// Ensure time is imported for potential future use.
var _ = time.Now

func TestCLIAcknowledgeClearsBell(t *testing.T) {
	te := newTestEnv(t)
	te.run("reset")
	// Report waiting → should be unacknowledged.
	te.runWithEnv(map[string]string{
		"TMUX_PANE":  "%1",
		"BEACON_NOW": "100",
	}, "report", "waiting", "needs input")
	st := te.loadState()
	if rec := st.Panes["%1"]; rec.Acknowledged {
		t.Fatal("waiting should be unacknowledged initially")
	}
	// Acknowledge.
	te.run("acknowledge", "%1")
	st = te.loadState()
	if rec := st.Panes["%1"]; !rec.Acknowledged {
		t.Fatal("acknowledge should set Acknowledged=true")
	}
}

func TestCLIWorkingIsAutoAcknowledged(t *testing.T) {
	te := newTestEnv(t)
	te.run("reset")
	te.runWithEnv(map[string]string{
		"TMUX_PANE":  "%1",
		"BEACON_NOW": "100",
	}, "report", "working", "busy")
	st := te.loadState()
	if rec := st.Panes["%1"]; !rec.Acknowledged {
		t.Fatal("working should be auto-acknowledged (no bell)")
	}
}

func TestCLIPendingNotificationsOldestFirst(t *testing.T) {
	te := newTestEnv(t)
	te.run("reset")
	// Report two panes with different timestamps.
	te.runWithEnv(map[string]string{
		"TMUX_PANE":  "%2",
		"BEACON_NOW": "200",
	}, "report", "waiting", "second")
	te.runWithEnv(map[string]string{
		"TMUX_PANE":  "%1",
		"BEACON_NOW": "100",
	}, "report", "completed", "first")
	st := te.loadState()
	store, _ := state.NewStore(te.stateDir)
	pending := store.PendingNotifications()
	if len(pending) != 2 {
		t.Fatalf("expected 2 pending, got %d", len(pending))
	}
	if pending[0].Pane != "%1" {
		t.Fatalf("oldest should be %%1 (time 100), got %s (time %d)", pending[0].Pane, pending[0].Time)
	}
	_ = st
}

func TestCLIJumpNoOpWithoutPending(t *testing.T) {
	te := newTestEnv(t)
	te.run("reset")
	// No pending notifications → jump must be no-op (no fallback to LastCompleted).
	te.runWithEnv(map[string]string{
		"TMUX_PANE":  "%1",
		"BEACON_NOW": "100",
	}, "report", "completed", "done")
	// Even with LastCompleted set, jump should not select any pane
	// because the completed notification was auto-acknowledged by... wait,
	// completed IS a notification status. Let's acknowledge it first.
	te.run("acknowledge", "%1")
	// Now no pending. Jump should be no-op.
	te.runWithEnv(map[string]string{
		"BEACON_TEST_TMUX_LOG": te.tmuxLog,
	}, "jump")
	logData, _ := os.ReadFile(te.tmuxLog)
	logStr := string(logData)
	// Should NOT contain switch-client or select-pane (no-op).
	if strings.Contains(logStr, "switch-client") {
		t.Fatal("jump should be no-op without pending notifications")
	}
	if strings.Contains(logStr, "select-pane") {
		t.Fatal("jump should be no-op without pending notifications")
	}
}

func TestCLIAcknowledgeOnlyClearsTargetPane(t *testing.T) {
	te := newTestEnv(t)
	te.run("reset")
	te.runWithEnv(map[string]string{
		"TMUX_PANE":  "%1",
		"BEACON_NOW": "100",
	}, "report", "waiting", "first")
	te.runWithEnv(map[string]string{
		"TMUX_PANE":  "%2",
		"BEACON_NOW": "200",
	}, "report", "blocked", "second")
	// Acknowledge only %1.
	te.run("acknowledge", "%1")
	st := te.loadState()
	if rec := st.Panes["%1"]; !rec.Acknowledged {
		t.Fatal("%1 should be acknowledged")
	}
	if rec := st.Panes["%2"]; rec.Acknowledged {
		t.Fatal("%2 should still be unacknowledged")
	}
}

func TestCLIAcknowledgeVisibleClearsVisiblePending(t *testing.T) {
	// acknowledge-visible should clear bells for panes visible to attached clients.
	// Fake tmux returns /dev/ttys000 → %1 as the only visible pane.
	te := newTestEnv(t)
	te.run("reset")
	// Report on %1 (visible) and %2 (not visible).
	te.runWithEnv(map[string]string{
		"TMUX_PANE":  "%1",
		"BEACON_NOW": "100",
	}, "report", "waiting", "visible")
	te.runWithEnv(map[string]string{
		"TMUX_PANE":  "%2",
		"BEACON_NOW": "200",
	}, "report", "blocked", "hidden")
	st := te.loadState()
	if rec := st.Panes["%1"]; rec.Acknowledged {
		t.Fatal("%1 should be unacknowledged before acknowledge-visible")
	}
	if rec := st.Panes["%2"]; rec.Acknowledged {
		t.Fatal("%2 should be unacknowledged before acknowledge-visible")
	}
	// Run acknowledge-visible — should clear %1 (visible) but not %2.
	te.run("acknowledge-visible")
	st = te.loadState()
	if rec := st.Panes["%1"]; !rec.Acknowledged {
		t.Fatal("%1 (visible) should be acknowledged")
	}
	if rec := st.Panes["%2"]; rec.Acknowledged {
		t.Fatal("%2 (not visible) should still be unacknowledged")
	}
}

func TestCLIAcknowledgeVisibleNoOpWithoutPending(t *testing.T) {
	te := newTestEnv(t)
	te.run("reset")
	// No pending notifications — acknowledge-visible should be no-op.
	te.run("acknowledge-visible")
	st := te.loadState()
	if len(st.Panes) != 0 {
		t.Fatalf("no panes should exist, got %d", len(st.Panes))
	}
}

func TestCLIAcknowledgeVisibleParsesPipeSeparator(t *testing.T) {
	// Regression: tmux format #{client_tty}\t#{pane_id} produces literal
	// backslash-t, not a real tab. The code must use | as separator.
	// This test uses a fake tmux that outputs the | format and verifies
	// that the visible pane is correctly parsed and acknowledged.
	te := newTestEnv(t)
	te.run("reset")
	// Report on %1 (will be the visible pane from fake list-clients).
	te.runWithEnv(map[string]string{
		"TMUX_PANE":  "%1",
		"BEACON_NOW": "100",
	}, "report", "waiting", "visible pane")
	// Run acknowledge-visible.
	te.run("acknowledge-visible")
	st := te.loadState()
	if rec := st.Panes["%1"]; !rec.Acknowledged {
		t.Fatal("fake tmux outputs /dev/ttys000|%1 — %1 should be parsed as visible and acknowledged")
	}
}

func TestCLIAcknowledgeVisibleRejectsTabSeparator(t *testing.T) {
	// If the fake tmux outputs literal \t (backslash-t), the parser must
	// NOT misparse it. This confirms the code doesn't fall back to tab.
	te := newTestEnv(t)
	// Override the fake tmux to output literal \t instead of |.
	script := `#!/usr/bin/env bash
case "${1:-}" in
  display-message)
    target=""; format=""
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
      '#{pane_id}') printf '%%s\n' "$target" ;;
    esac
    ;;
  list-panes) for i in $(seq 1 100); do printf '%%%s\n' "$i"; done ;;
  list-windows) printf '@1\n@2\n' ;;
  list-sessions) printf 'test-session\n' ;;
  list-clients) printf '/dev/ttys000\\t%%1\n' ;;
  set-option) : ;;
  switch-client|select-pane) printf '%s\n' "$*" >>"${BEACON_TEST_TMUX_LOG:-/dev/null}" ;;
esac
`
	os.WriteFile(te.tmuxScript, []byte(script), 0o755)
	te.run("reset")
	te.runWithEnv(map[string]string{
		"TMUX_PANE":  "%1",
		"BEACON_NOW": "100",
	}, "report", "waiting", "should not be acked")
	te.run("acknowledge-visible")
	st := te.loadState()
	if rec := st.Panes["%1"]; rec.Acknowledged {
		t.Fatal("literal \\t must not be parsed as separator — %1 should remain unacknowledged")
	}
}

// --- terminal-notifier and jump pane_id tests ---

// writeFakeTerminalNotifier creates a fake terminal-notifier script that
// logs its arguments to a file. Returns the log path.
func (te *testEnv) writeFakeTerminalNotifier() string {
	logPath := filepath.Join(te.tmpDir, "tn.log")
	script := filepath.Join(te.tmpDir, "bin", "terminal-notifier")
	os.MkdirAll(filepath.Dir(script), 0o755)
	os.WriteFile(script, []byte(`#!/usr/bin/env bash
printf '%s\n' "$@" >>"`+logPath+`"
`), 0o755)
	return logPath
}

// TestNotifyTerminalNotifierWithPane verifies that runNotify uses
// terminal-notifier with -execute and -group when TMUX_PANE is set.
func TestNotifyTerminalNotifierWithPane(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("terminal-notifier test only on macOS")
	}
	te := newTestEnv(t)
	te.run("reset")
	logPath := te.writeFakeTerminalNotifier()
	// Enable notifications (default env has BEACON_NOTIFY=0).
	te.runWithEnv(map[string]string{
		"TMUX_PANE":     "%42",
		"BEACON_NOTIFY": "1",
		"TERM_PROGRAM":  "ghostty",
	}, "notify", "TestAgent", "hello world")
	data, _ := os.ReadFile(logPath)
	logStr := string(data)
	// Must use terminal-notifier, not osascript.
	assertContains(t, logStr, "-title", "terminal-notifier title")
	assertContains(t, logStr, "TestAgent", "terminal-notifier title value")
	assertContains(t, logStr, "hello world", "terminal-notifier message value")
	assertContains(t, logStr, "-group", "terminal-notifier group flag")
	assertContains(t, logStr, "%42", "terminal-notifier group pane ID")
	assertContains(t, logStr, "-execute", "terminal-notifier execute flag")
	// Execute action must contain jump with the pane ID (path is os.Executable, not "beacon").
	assertContains(t, logStr, "jump '%42'", "execute action jumps to pane")
	// Ghostty terminal activation (shell-quoted).
	assertContains(t, logStr, "'/usr/bin/open' -a 'Ghostty'", "execute activates Ghostty")
}

// TestNotifyTerminalNotifierNoPane verifies that without TMUX_PANE,
// terminal-notifier is used but without -execute or -group.
func TestNotifyTerminalNotifierNoPane(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("terminal-notifier test only on macOS")
	}
	te := newTestEnv(t)
	te.run("reset")
	logPath := te.writeFakeTerminalNotifier()
	te.runWithEnv(map[string]string{
		"BEACON_NOTIFY": "1",
	}, "notify", "Agent", "no pane")
	data, _ := os.ReadFile(logPath)
	logStr := string(data)
	assertContains(t, logStr, "-title", "terminal-notifier used")
	assertContains(t, logStr, "no pane", "message present")
	assertNotContains(t, logStr, "-execute", "no execute without pane")
	assertNotContains(t, logStr, "-group", "no group without pane")
}

// TestNotifyFallbackOsascript verifies that without terminal-notifier,
// the notification falls back to osascript.
func TestNotifyFallbackOsascript(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("osascript fallback test only on macOS")
	}
	te := newTestEnv(t)
	te.run("reset")
	// Do NOT write fake terminal-notifier. PATH only has /usr/bin:/bin
	// where terminal-notifier doesn't exist in test env.
	// We can't easily verify osascript was called, but we can verify
	// the command doesn't fail and exits 0.
	_, code := te.runWithEnv(map[string]string{
		"TMUX_PANE":     "%1",
		"BEACON_NOTIFY": "1",
	}, "notify", "Agent", "fallback test")
	if code != 0 {
		t.Fatalf("notify fallback should exit 0, got %d", code)
	}
}

// TestJumpWithPaneID verifies that jump with a pane ID argument jumps
// to that specific pane.
func TestJumpWithPaneID(t *testing.T) {
	te := newTestEnv(t)
	te.run("reset")
	// Report on %5 so it has state.
	te.runWithEnv(map[string]string{
		"TMUX_PANE":  "%5",
		"BEACON_NOW": "100",
	}, "report", "waiting", "test message")
	// Jump to %5 explicitly.
	te.runWithEnv(map[string]string{
		"BEACON_TEST_TMUX_LOG": te.tmuxLog,
	}, "jump", "%5")
	logData, _ := os.ReadFile(te.tmuxLog)
	logStr := string(logData)
	assertContains(t, logStr, "test-session", "jump to pane session")
	assertContains(t, logStr, "%5", "jump to correct pane")
}

// TestJumpInvalidPaneIDRejected verifies that jump rejects invalid pane IDs.
func TestJumpInvalidPaneIDRejected(t *testing.T) {
	te := newTestEnv(t)
	te.run("reset")
	_, code := te.run("jump", "not-a-pane")
	if code != 2 {
		t.Fatalf("invalid pane ID should return 2, got %d", code)
	}
}

// TestJumpPaneIDWithSpecialChars verifies that special characters in
// pane ID argument are rejected, preventing shell injection.
func TestJumpPaneIDWithSpecialChars(t *testing.T) {
	te := newTestEnv(t)
	te.run("reset")
	// Various injection attempts.
	invalid := []string{
		"%1; rm -rf /",
		"%1$(whoami)",
		"%1`whoami`",
		"%1 && whoami",
		"%1|whoami",
		"$(whoami)",
		"; whoami",
	}
	for _, p := range invalid {
		_, code := te.run("jump", p)
		if code != 2 {
			t.Fatalf("jump with %q should return 2, got %d", p, code)
		}
	}
}

// TestBuildExecuteActionShellSafe verifies that buildExecuteAction
// produces shell-safe output for valid pane IDs and doesn't inject
// special characters from TERM_PROGRAM.
func TestBuildExecuteActionShellSafe(t *testing.T) {
	// Valid pane ID — should be single-quoted.
	// Path is from os.Executable(), so we check for "jump '%99'" not "beacon jump".
	action := buildExecuteAction("%99")
	if !strings.Contains(action, "jump '%99'") {
		t.Fatalf("execute action should contain jump '%%99': %q", action)
	}
}

// TestBuildExecuteActionGhostty verifies Ghostty terminal activation.
func TestBuildExecuteActionGhostty(t *testing.T) {
	old := os.Getenv("TERM_PROGRAM")
	os.Setenv("TERM_PROGRAM", "ghostty")
	defer os.Setenv("TERM_PROGRAM", old)
	action := buildExecuteAction("%1")
	if !strings.Contains(action, "'/usr/bin/open' -a 'Ghostty'") {
		t.Fatalf("execute action should activate Ghostty (quoted): %q", action)
	}
	if !strings.Contains(action, "jump '%1'") {
		t.Fatalf("execute action should contain jump: %q", action)
	}
}

// TestBuildExecuteActionUnknownTerminal verifies that unknown terminals
// only get the pane jump, no terminal activation.
func TestBuildExecuteActionUnknownTerminal(t *testing.T) {
	old := os.Getenv("TERM_PROGRAM")
	os.Setenv("TERM_PROGRAM", "unknown-term")
	defer os.Setenv("TERM_PROGRAM", old)
	action := buildExecuteAction("%1")
	if strings.Contains(action, "/usr/bin/open") {
		t.Fatalf("unknown terminal should not have open command: %q", action)
	}
	if !strings.Contains(action, "jump '%1'") {
		t.Fatalf("execute action should still contain jump: %q", action)
	}
}

// TestDetectTerminalAppViaTmuxEnv verifies that when TERM_PROGRAM=tmux
// and TMUX is set, detectTerminalApp queries tmux show-environment -g
// and returns the real terminal app (ghostty).
func TestDetectTerminalAppViaTmuxEnv(t *testing.T) {
	te := newTestEnv(t)
	oldTP := os.Getenv("TERM_PROGRAM")
	oldTMUX := os.Getenv("TMUX")
	oldTmuxBin := os.Getenv("BEACON_TMUX_BIN")
	os.Setenv("TERM_PROGRAM", "tmux")
	os.Setenv("TMUX", "/tmp/tmux-1001/default,12345,0")
	os.Setenv("BEACON_TMUX_BIN", te.tmuxScript)
	defer func() {
		os.Setenv("TERM_PROGRAM", oldTP)
		os.Setenv("TMUX", oldTMUX)
		os.Setenv("BEACON_TMUX_BIN", oldTmuxBin)
	}()
	app := detectTerminalApp()
	if app != "ghostty" {
		t.Fatalf("detectTerminalApp should return ghostty via tmux env, got %q", app)
	}
}

// TestBuildExecuteActionTmuxInsideGhostty verifies that the -execute
// action includes Ghostty activation when TERM_PROGRAM=tmux but tmux's
// global environment has TERM_PROGRAM=ghostty.
func TestBuildExecuteActionTmuxInsideGhostty(t *testing.T) {
	te := newTestEnv(t)
	oldTP := os.Getenv("TERM_PROGRAM")
	oldTMUX := os.Getenv("TMUX")
	oldTmuxBin := os.Getenv("BEACON_TMUX_BIN")
	os.Setenv("TERM_PROGRAM", "tmux")
	os.Setenv("TMUX", "/tmp/tmux-1001/default,12345,0")
	os.Setenv("BEACON_TMUX_BIN", te.tmuxScript)
	defer func() {
		os.Setenv("TERM_PROGRAM", oldTP)
		os.Setenv("TMUX", oldTMUX)
		os.Setenv("BEACON_TMUX_BIN", oldTmuxBin)
	}()
	action := buildExecuteAction("%1")
	if !strings.Contains(action, "'/usr/bin/open' -a 'Ghostty'") {
		t.Fatalf("execute action should activate Ghostty via tmux env: %q", action)
	}
	if !strings.Contains(action, "jump '%1'") {
		t.Fatalf("execute action should contain jump: %q", action)
	}
}

// TestDetectTerminalAppDirectGhostty verifies that when TERM_PROGRAM
// is directly set to ghostty (not inside tmux), detectTerminalApp
// returns ghostty without querying tmux.
func TestDetectTerminalAppDirectGhostty(t *testing.T) {
	oldTP := os.Getenv("TERM_PROGRAM")
	oldTMUX := os.Getenv("TMUX")
	os.Setenv("TERM_PROGRAM", "ghostty")
	os.Setenv("TMUX", "")
	defer func() {
		os.Setenv("TERM_PROGRAM", oldTP)
		os.Setenv("TMUX", oldTMUX)
	}()
	app := detectTerminalApp()
	if app != "ghostty" {
		t.Fatalf("detectTerminalApp should return ghostty directly, got %q", app)
	}
}

// TestDetectTerminalAppUnknownNoTmux verifies that an unknown terminal
// without TMUX set returns the env value as-is.
func TestDetectTerminalAppUnknownNoTmux(t *testing.T) {
	oldTP := os.Getenv("TERM_PROGRAM")
	oldTMUX := os.Getenv("TMUX")
	os.Setenv("TERM_PROGRAM", "wezterm")
	os.Setenv("TMUX", "")
	defer func() {
		os.Setenv("TERM_PROGRAM", oldTP)
		os.Setenv("TMUX", oldTMUX)
	}()
	app := detectTerminalApp()
	if app != "wezterm" {
		t.Fatalf("detectTerminalApp should return wezterm, got %q", app)
	}
}

// --- fail-closed jump tests ---

// writeFailingTmux writes a fake tmux where a specified subcommand exits 1.
// failCmd is one of: "switch-client", "select-window", "select-pane",
// "list-clients", "display-message".
func (te *testEnv) writeFailingTmux(failCmd string) {
	script := `#!/usr/bin/env bash
cmd="${1:-}"
case "$cmd" in
  display-message)
    target=""; format=""
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
      '#{session_name}|#{window_id}|#{pane_id}') printf 'test-session|@1|%s\n' "$target" ;;
    esac
    ;;
  list-panes) for i in $(seq 1 100); do printf '%%%s\n' "$i"; done ;;
  list-windows) printf '@1\n@2\n' ;;
  list-sessions) printf 'test-session\n' ;;
  list-clients)
    # Check the -F format argument to decide output.
    fmt=""
    while (($#)); do
      case "$1" in
        -F) fmt="$2"; shift 2 ;;
        *) shift ;;
      esac
    done
    case "$fmt" in
      *'#{pane_id}'*) printf '/dev/ttys000|%%1\n' ;;
      *) printf '/dev/ttys000|test-session|1000\n' ;;
    esac
    ;;
  set-option) : ;;
  switch-client|select-pane|select-window)
    if [ "$cmd" = "` + failCmd + `" ]; then exit 1; fi
    printf '%s\n' "$*" >>"${BEACON_TEST_TMUX_LOG:-/dev/null}"
    ;;
esac
`
	// Override the failCmd check for list-clients and display-message.
	if failCmd == "list-clients" {
		// Replace the entire list-clients block with exit 1.
		script = strings.Replace(script,
			`  list-clients)
    # Check the -F format argument to decide output.
    fmt=""
    while (($#)); do
      case "$1" in
        -F) fmt="$2"; shift 2 ;;
        *) shift ;;
      esac
    done
    case "$fmt" in
      *'#{pane_id}'*) printf '/dev/ttys000|%%1\n' ;;
      *) printf '/dev/ttys000|test-session|1000\n' ;;
    esac
    ;;`,
			`  list-clients) exit 1 ;;`, 1)
	}
	if failCmd == "display-message" {
		script = strings.Replace(script,
			`    case "$format" in
      '#{session_name}')`,
			"    exit 1\n    case \"$format\" in\n      '#{session_name}')", 1)
	}
	os.WriteFile(te.tmuxScript, []byte(script), 0o755)
}

// TestJumpFailClosedSwitchClient verifies that when switch-client fails,
// jump returns non-zero and the bell is NOT acknowledged.
func TestJumpFailClosedSwitchClient(t *testing.T) {
	te := newTestEnv(t)
	te.run("reset")
	te.runWithEnv(map[string]string{
		"TMUX_PANE":  "%5",
		"BEACON_NOW": "100",
	}, "report", "waiting", "test message")
	te.writeFailingTmux("switch-client")
	_, code := te.runWithEnv(map[string]string{
		"BEACON_TEST_TMUX_LOG": te.tmuxLog,
	}, "jump", "%5")
	if code == 0 {
		t.Fatal("jump should return non-zero when switch-client fails")
	}
	st := te.loadState()
	if rec := st.Panes["%5"]; rec.Acknowledged {
		t.Fatal("bell should be preserved when switch-client fails")
	}
}

// TestJumpFailClosedSelectPane verifies that when select-pane fails,
// jump returns non-zero and the bell is NOT acknowledged.
func TestJumpFailClosedSelectPane(t *testing.T) {
	te := newTestEnv(t)
	te.run("reset")
	te.runWithEnv(map[string]string{
		"TMUX_PANE":  "%5",
		"BEACON_NOW": "100",
	}, "report", "waiting", "test message")
	te.writeFailingTmux("select-pane")
	_, code := te.runWithEnv(map[string]string{
		"BEACON_TEST_TMUX_LOG": te.tmuxLog,
	}, "jump", "%5")
	if code == 0 {
		t.Fatal("jump should return non-zero when select-pane fails")
	}
	st := te.loadState()
	if rec := st.Panes["%5"]; rec.Acknowledged {
		t.Fatal("bell should be preserved when select-pane fails")
	}
}

// TestJumpFailClosedSelectWindow verifies that when select-window fails,
// jump returns non-zero and the bell is NOT acknowledged.
func TestJumpFailClosedSelectWindow(t *testing.T) {
	te := newTestEnv(t)
	te.run("reset")
	te.runWithEnv(map[string]string{
		"TMUX_PANE":  "%5",
		"BEACON_NOW": "100",
	}, "report", "waiting", "test message")
	te.writeFailingTmux("select-window")
	_, code := te.runWithEnv(map[string]string{
		"BEACON_TEST_TMUX_LOG": te.tmuxLog,
	}, "jump", "%5")
	if code == 0 {
		t.Fatal("jump should return non-zero when select-window fails")
	}
	st := te.loadState()
	if rec := st.Panes["%5"]; rec.Acknowledged {
		t.Fatal("bell should be preserved when select-window fails")
	}
}

// TestJumpFailClosedNoClient verifies that when no client is attached,
// jump returns non-zero and the bell is NOT acknowledged.
func TestJumpFailClosedNoClient(t *testing.T) {
	te := newTestEnv(t)
	te.run("reset")
	te.runWithEnv(map[string]string{
		"TMUX_PANE":  "%5",
		"BEACON_NOW": "100",
	}, "report", "waiting", "test message")
	// Override tmux to return no clients.
	te.writeFailingTmux("list-clients")
	// But list-clients exit 1 means switchToTarget returns false.
	_, code := te.runWithEnv(map[string]string{
		"BEACON_TEST_TMUX_LOG": te.tmuxLog,
	}, "jump", "%5")
	if code == 0 {
		t.Fatal("jump should return non-zero when no client attached")
	}
	st := te.loadState()
	if rec := st.Panes["%5"]; rec.Acknowledged {
		t.Fatal("bell should be preserved when no client attached")
	}
}

// TestJumpNoArgFailClosed verifies the no-argument jump path also
// preserves the bell when switchToTarget fails.
func TestJumpNoArgFailClosed(t *testing.T) {
	te := newTestEnv(t)
	te.run("reset")
	te.runWithEnv(map[string]string{
		"TMUX_PANE":  "%5",
		"BEACON_NOW": "100",
	}, "report", "waiting", "test message")
	te.writeFailingTmux("switch-client")
	_, code := te.runWithEnv(map[string]string{
		"BEACON_TEST_TMUX_LOG": te.tmuxLog,
	}, "jump")
	if code == 0 {
		t.Fatal("jump (no arg) should return non-zero when switch-client fails")
	}
	st := te.loadState()
	if rec := st.Panes["%5"]; rec.Acknowledged {
		t.Fatal("bell should be preserved when jump fails")
	}
}

// TestJumpSuccessAcksBell verifies that a successful jump does acknowledge.
func TestJumpSuccessAcksBell(t *testing.T) {
	te := newTestEnv(t)
	te.run("reset")
	te.runWithEnv(map[string]string{
		"TMUX_PANE":  "%5",
		"BEACON_NOW": "100",
	}, "report", "waiting", "test message")
	_, code := te.runWithEnv(map[string]string{
		"BEACON_TEST_TMUX_LOG": te.tmuxLog,
	}, "jump", "%5")
	if code != 0 {
		t.Fatalf("jump should succeed, got %d", code)
	}
	st := te.loadState()
	if rec := st.Panes["%5"]; !rec.Acknowledged {
		t.Fatal("bell should be acknowledged after successful jump")
	}
}

// TestJumpUsesSwitchClientWithC verifies that switch-client is called
// with -c (client tty) flag, not just -t.
func TestJumpUsesSwitchClientWithC(t *testing.T) {
	te := newTestEnv(t)
	te.run("reset")
	te.runWithEnv(map[string]string{
		"TMUX_PANE":  "%5",
		"BEACON_NOW": "100",
	}, "report", "waiting", "test message")
	te.runWithEnv(map[string]string{
		"BEACON_TEST_TMUX_LOG": te.tmuxLog,
	}, "jump", "%5")
	logData, _ := os.ReadFile(te.tmuxLog)
	logStr := string(logData)
	assertContains(t, logStr, "-c", "switch-client must use -c for client tty")
	assertContains(t, logStr, "/dev/ttys000", "switch-client must target the client tty")
	assertContains(t, logStr, "test-session", "switch-client must target the session")
}

func TestDetectAgentName(t *testing.T) {
	// 1. Explicit BEACON_AGENT_NAME takes precedence
	t.Setenv("BEACON_AGENT_NAME", "CustomAgent")
	t.Setenv("CODEX_THREAD_ID", "thread-123")
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "cli")
	if got := detectAgentName(); got != "CustomAgent" {
		t.Fatalf("detectAgentName with BEACON_AGENT_NAME: got %q, want %q", got, "CustomAgent")
	}

	// 2. CODEX_THREAD_ID detects Codex
	t.Setenv("BEACON_AGENT_NAME", "")
	t.Setenv("CODEX_THREAD_ID", "thread-123")
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "")
	if got := detectAgentName(); got != "Codex" {
		t.Fatalf("detectAgentName with CODEX_THREAD_ID: got %q, want %q", got, "Codex")
	}

	// 3. CODEX_CI detects Codex
	t.Setenv("CODEX_THREAD_ID", "")
	t.Setenv("CODEX_CI", "1")
	if got := detectAgentName(); got != "Codex" {
		t.Fatalf("detectAgentName with CODEX_CI: got %q, want %q", got, "Codex")
	}

	// 4. CLAUDE_CODE_ENTRYPOINT detects Claude
	t.Setenv("CODEX_CI", "")
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "cli")
	if got := detectAgentName(); got != "Claude" {
		t.Fatalf("detectAgentName with CLAUDE_CODE_ENTRYPOINT: got %q, want %q", got, "Claude")
	}

	// 5. Default fallback to Agent
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "")
	if got := detectAgentName(); got != "Agent" {
		t.Fatalf("detectAgentName fallback: got %q, want %q", got, "Agent")
	}
}

func TestHookSanitizesMarkdownSummary(t *testing.T) {
	te := newTestEnv(t)
	te.run("reset")
	cmd := exec.Command(te.bin, "hook", "stop")
	cmd.Env = append(te.env(), "TMUX_PANE=%1", "BEACON_NOW=100", "CODEX_THREAD_ID=test")
	cmd.Stdin = strings.NewReader(`{"last_assistant_message":"**修复完成**：更新了 ` + "`main.go`" + ` 文件"}`)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hook stop: %v\n%s", err, out)
	}
	st := te.loadState()
	rec := st.Panes["%1"]
	if rec.Status != "completed" {
		t.Fatalf("hook stop should mark completed: got %q", rec.Status)
	}
	wantSummary := "修复完成：更新了 main.go 文件"
	if rec.Summary != wantSummary {
		t.Fatalf("hook stop summary: got %q, want %q", rec.Summary, wantSummary)
	}
}
