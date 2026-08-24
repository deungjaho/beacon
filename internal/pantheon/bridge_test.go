package pantheon

import (
	"os"
	"path/filepath"
	"testing"
)

// withTempHome runs fn with HOME (and thus defaultStateDir) pointing at a
// fresh temporary directory, then restores the original environment.
func withTempHome(t *testing.T, fn func(home string)) {
	t.Helper()
	origHome := os.Getenv("HOME")
	origXdg := os.Getenv("XDG_DATA_HOME")
	tmp := t.TempDir()
	os.Setenv("HOME", tmp)
	os.Setenv("XDG_DATA_HOME", "")
	t.Cleanup(func() {
		os.Setenv("HOME", origHome)
		os.Setenv("XDG_DATA_HOME", origXdg)
	})
	fn(tmp)
}

func TestLoadConfig(t *testing.T) {
	// Disabled explicitly.
	t.Setenv("PANTHEON_DISABLED", "1")
	cfg := LoadConfig()
	if cfg.Enabled {
		t.Fatalf("expected bridge disabled when PANTHEON_DISABLED=1")
	}

	// Default socket path derives from HOME.
	t.Setenv("PANTHEON_DISABLED", "")
	t.Setenv("PANTHEON_SOCKET", "")
	t.Setenv("PANTHEON_CLI", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg = LoadConfig()
	want := home + "/.local/share/pantheon/pantheond.sock"
	if cfg.SocketPath != want {
		t.Fatalf("socket path = %q, want %q", cfg.SocketPath, want)
	}
	if cfg.CliPath != "pantheon" {
		t.Fatalf("cli path = %q, want %q", cfg.CliPath, "pantheon")
	}
	// Socket does not exist -> disabled.
	if cfg.Enabled {
		t.Fatalf("expected bridge disabled when socket is absent")
	}

	// Create the socket file -> enabled.
	if err := os.MkdirAll(filepath.Dir(cfg.SocketPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.SocketPath, []byte{}, 0600); err != nil {
		t.Fatal(err)
	}
	cfg = LoadConfig()
	if !cfg.Enabled {
		t.Fatalf("expected bridge enabled when socket exists")
	}

	// Explicit CLI override.
	t.Setenv("PANTHEON_CLI", "/usr/local/bin/pantheon")
	cfg = LoadConfig()
	if cfg.CliPath != "/usr/local/bin/pantheon" {
		t.Fatalf("cli path = %q, want /usr/local/bin/pantheon", cfg.CliPath)
	}
}

func TestDetectRuntime(t *testing.T) {
	// Explicit hint wins.
	if got := detectRuntime("claude"); got != "claude" {
		t.Fatalf("detectRuntime(claude) = %q, want claude", got)
	}

	// No hint, no env -> devin.
	t.Setenv("CODEX_THREAD_ID", "")
	t.Setenv("CODEX_CI", "")
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "")
	if got := detectRuntime(""); got != "devin" {
		t.Fatalf("detectRuntime() = %q, want devin", got)
	}

	// Codex env.
	t.Setenv("CODEX_THREAD_ID", "abc")
	if got := detectRuntime(""); got != "codex" {
		t.Fatalf("detectRuntime() = %q, want codex", got)
	}
	t.Setenv("CODEX_THREAD_ID", "")

	// Claude env.
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "cli")
	if got := detectRuntime(""); got != "claude" {
		t.Fatalf("detectRuntime() = %q, want claude", got)
	}
}

func TestSetGetClearAgentID(t *testing.T) {
	withTempHome(t, func(home string) {
		pane := "%42"

		// Empty pane is a no-op.
		if err := SetAgentIDForPane("", "agent-1"); err != nil {
			t.Fatalf("SetAgentIDForPane(%q) error: %v", "", err)
		}
		if got := GetAgentIDForPane(""); got != "" {
			t.Fatalf("GetAgentIDForPane(%q) = %q, want empty", "", got)
		}

		// Round-trip set/get.
		if err := SetAgentIDForPane(pane, "agent-123"); err != nil {
			t.Fatalf("SetAgentIDForPane error: %v", err)
		}
		if got := GetAgentIDForPane(pane); got != "agent-123" {
			t.Fatalf("GetAgentIDForPane = %q, want agent-123", got)
		}

		// Clear removes it.
		ClearAgentIDForPane(pane)
		if got := GetAgentIDForPane(pane); got != "" {
			t.Fatalf("GetAgentIDForPane after clear = %q, want empty", got)
		}
	})
}

func TestRegisterAgentDisabled(t *testing.T) {
	// Disabled bridge returns nil result and nil error.
	cfg := BridgeConfig{Enabled: false}
	res, err := RegisterAgent(nil, cfg, AgentInfo{Prompt: "hi"})
	if err != nil {
		t.Fatalf("expected nil error when disabled, got %v", err)
	}
	if res != nil {
		t.Fatalf("expected nil result when disabled, got %+v", res)
	}
}

func TestCompleteAgentDisabled(t *testing.T) {
	cfg := BridgeConfig{Enabled: false}
	if err := CompleteAgent(nil, cfg, "agent-1"); err != nil {
		t.Fatalf("expected nil error when disabled, got %v", err)
	}
	// Enabled but empty agent ID is also a no-op.
	cfg.Enabled = true
	if err := CompleteAgent(nil, cfg, ""); err != nil {
		t.Fatalf("expected nil error for empty agent id, got %v", err)
	}
}
