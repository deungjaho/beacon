package pantheon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadConfig(t *testing.T) {
	tmp := t.TempDir()
	sockPath := filepath.Join(tmp, "test.sock")
	f, _ := os.Create(sockPath)
	f.Close()

	t.Setenv("PANTHEON_SOCKET", sockPath)
	cfg := LoadConfig()
	if !cfg.Enabled {
		t.Error("expected enabled when socket exists")
	}
	if cfg.SocketPath != sockPath {
		t.Errorf("socket = %s, want %s", cfg.SocketPath, sockPath)
	}

	t.Setenv("PANTHEON_DISABLED", "1")
	cfg = LoadConfig()
	if cfg.Enabled {
		t.Error("expected disabled when PANTHEON_DISABLED=1")
	}
}

func TestDetectRuntime(t *testing.T) {
	t.Setenv("CODEX_THREAD_ID", "test")
	if got := detectRuntime(""); got != "codex" {
		t.Errorf("detectRuntime() = %q, want codex", got)
	}
	os.Unsetenv("CODEX_THREAD_ID")

	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "test")
	if got := detectRuntime(""); got != "claude" {
		t.Errorf("detectRuntime() = %q, want claude", got)
	}
	os.Unsetenv("CLAUDE_CODE_ENTRYPOINT")

	if got := detectRuntime(""); got != "devin" {
		t.Errorf("detectRuntime() = %q, want devin", got)
	}
}

func TestIsGitRepo(t *testing.T) {
	tmp := t.TempDir()
	if isGitRepo(tmp) {
		t.Error("temp dir is not a git repo")
	}
	runGit(t, tmp, "init")
	runGit(t, tmp, "config", "user.email", "test@test.com")
	runGit(t, tmp, "config", "user.name", "test")
	if !isGitRepo(tmp) {
		t.Error("git repo not detected after init")
	}
}

func TestPaneStateSetGetClear(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	st := paneState{
		RunID:        "run_test",
		AgentID:      "agt_test",
		ProjectID:    "prj_test",
		RegisteredAt: time.Now().Unix(),
	}
	setPaneState("%5", st)
	got := getPaneState("%5")
	if got == nil || got.AgentID != "agt_test" {
		t.Fatalf("getPaneState = %v, want agt_test", got)
	}
	ClearPaneState("%5")
	if getPaneState("%5") != nil {
		t.Error("pane state not cleared")
	}
}

func TestRegisterAgentDisabled(t *testing.T) {
	cfg := BridgeConfig{Enabled: false}
	result, err := RegisterAgent(context.Background(), cfg, AgentInfo{Cwd: "/tmp"})
	if err != nil {
		t.Fatalf("RegisterAgent with disabled bridge should not error: %v", err)
	}
	if result != nil {
		t.Error("expected nil result when disabled")
	}
}

func TestCompleteAgentDisabled(t *testing.T) {
	cfg := BridgeConfig{Enabled: false}
	if err := CompleteAgent(context.Background(), cfg, "agt_test"); err != nil {
		t.Errorf("CompleteAgent with disabled bridge should not error: %v", err)
	}
}

func TestCompleteAgentEmptyID(t *testing.T) {
	cfg := BridgeConfig{Enabled: true}
	if err := CompleteAgent(context.Background(), cfg, ""); err != nil {
		t.Errorf("CompleteAgent with empty agentID should not error: %v", err)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v in %s: %v", args, dir, err)
	}
}
