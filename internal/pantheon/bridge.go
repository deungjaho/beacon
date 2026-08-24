// Package pantheon implements a best-effort bridge from Beacon hooks to
// Pantheon (pantheond). All calls are fire-and-forget: a failure here must
// never cause a Beacon hook to fail.
package pantheon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// BridgeConfig holds the configuration for the Pantheon bridge.
type BridgeConfig struct {
	SocketPath string // path to pantheond.sock
	CliPath    string // path to pantheon CLI binary
	Enabled    bool   // whether the bridge is enabled
}

// LoadConfig reads the bridge configuration from environment variables.
// PANTHEON_SOCKET: path to the Pantheon Unix socket (default: $HOME/.local/share/pantheon/pantheond.sock)
// PANTHEON_CLI: path to the pantheon CLI binary (default: pantheon on PATH)
// PANTHEON_DISABLED: if set to "1" or "true", the bridge is disabled
func LoadConfig() BridgeConfig {
	cfg := BridgeConfig{
		SocketPath: os.Getenv("PANTHEON_SOCKET"),
		CliPath:    "pantheon",
	}
	if cfg.SocketPath == "" {
		home, _ := os.UserHomeDir()
		cfg.SocketPath = home + "/.local/share/pantheon/pantheond.sock"
	}
	if cli := os.Getenv("PANTHEON_CLI"); cli != "" {
		cfg.CliPath = cli
	}
	if d := os.Getenv("PANTHEON_DISABLED"); d == "1" || d == "true" {
		cfg.Enabled = false
	} else {
		// Enable if the socket exists
		if _, err := os.Stat(cfg.SocketPath); err == nil {
			cfg.Enabled = true
		}
	}
	return cfg
}

// AgentInfo holds the agent information for registration.
type AgentInfo struct {
	Runtime string // devin, claude, codex
	PID     int    // process ID
	Cwd     string // current working directory
	Prompt  string // the prompt that triggered this agent
}

// RegisterResult holds the result of agent registration.
type RegisterResult struct {
	RunID   string
	AgentID string
}

// RegisterAgent registers an agent in Pantheon. It creates a run if needed
// and registers the agent. The run is created with a "manual" project if
// no project exists.
func RegisterAgent(ctx context.Context, cfg BridgeConfig, info AgentInfo) (*RegisterResult, error) {
	if !cfg.Enabled {
		return nil, nil
	}

	// Detect runtime from environment
	runtime := detectRuntime(info.Runtime)

	// Try to find an existing "manual" project
	projectID := findOrCreateProject(ctx, cfg)
	if projectID == "" {
		return nil, fmt.Errorf("pantheon: failed to find or create project")
	}

	// Create a run
	runID := createRun(ctx, cfg, projectID, info.Prompt)
	if runID == "" {
		return nil, fmt.Errorf("pantheon: failed to create run")
	}

	// Start the run (this auto-registers a worker agent via the runtime adapter)
	agentID := startRun(ctx, cfg, runID)
	if agentID == "" {
		return nil, fmt.Errorf("pantheon: failed to start run")
	}

	_ = runtime // reserved for future use / logging
	return &RegisterResult{RunID: runID, AgentID: agentID}, nil
}

// CompleteAgent marks an agent as completed in Pantheon.
func CompleteAgent(ctx context.Context, cfg BridgeConfig, agentID string) error {
	if !cfg.Enabled || agentID == "" {
		return nil
	}
	_, err := callRPC(ctx, cfg, "agent.complete", map[string]any{
		"agent_id":  agentID,
		"exit_code": 0,
	})
	return err
}

// detectRuntime returns the runtime name from the environment or the provided hint.
func detectRuntime(hint string) string {
	if hint != "" {
		return hint
	}
	if os.Getenv("CODEX_THREAD_ID") != "" || os.Getenv("CODEX_CI") != "" {
		return "codex"
	}
	if os.Getenv("CLAUDE_CODE_ENTRYPOINT") != "" {
		return "claude"
	}
	return "devin"
}

// findOrCreateProject finds an existing "manual" project or creates one.
func findOrCreateProject(ctx context.Context, cfg BridgeConfig) string {
	// List projects and look for "manual"
	result, err := callRPC(ctx, cfg, "project.list", map[string]any{})
	if err != nil {
		return ""
	}
	var resp struct {
		Result struct {
			Projects []struct {
				ProjectID string `json:"project_id"`
				Name      string `json:"name"`
			} `json:"projects"`
		} `json:"result"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return ""
	}
	for _, p := range resp.Result.Projects {
		if p.Name == "manual" {
			return p.ProjectID
		}
	}

	// Create a "manual" project using /tmp as the repo path
	cwd, _ := os.Getwd()
	result, err = callRPC(ctx, cfg, "project.register", map[string]any{
		"name":      "manual",
		"repo_path": cwd,
		"base_ref":  "main",
	})
	if err != nil {
		return ""
	}
	var regResp struct {
		Result struct {
			ProjectID string `json:"project_id"`
		} `json:"result"`
	}
	if err := json.Unmarshal(result, &regResp); err != nil {
		return ""
	}
	return regResp.Result.ProjectID
}

// createRun creates a run in Pantheon.
func createRun(ctx context.Context, cfg BridgeConfig, projectID, objective string) string {
	params := map[string]any{
		"project_id": projectID,
		"objective":  objective,
		"risk_level": "R1",
	}
	result, err := callRPC(ctx, cfg, "run.create", params)
	if err != nil {
		return ""
	}
	var resp struct {
		Result struct {
			RunID string `json:"run_id"`
		} `json:"result"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return ""
	}
	return resp.Result.RunID
}

// startRun starts a run and returns the agent ID.
func startRun(ctx context.Context, cfg BridgeConfig, runID string) string {
	result, err := callRPC(ctx, cfg, "run.start", map[string]any{
		"run_id": runID,
	})
	if err != nil {
		return ""
	}
	var resp struct {
		Result struct {
			AgentID string `json:"agent_id"`
		} `json:"result"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return ""
	}
	return resp.Result.AgentID
}

// callRPC sends a JSON-RPC request to Pantheon via the CLI.
// Uses the CLI binary to avoid managing the socket protocol directly.
func callRPC(ctx context.Context, cfg BridgeConfig, method string, params any) ([]byte, error) {
	paramJSON, _ := json.Marshal(params)
	cmd := exec.CommandContext(ctx, cfg.CliPath, method, string(paramJSON))
	cmd.Env = append(os.Environ(), "PANTHEON_SOCKET="+cfg.SocketPath)
	return cmd.Output()
}

// GetAgentIDFromEnv returns the agent ID stored in environment by a previous
// hook prompt call. This allows hook stop to know which agent to complete.
func GetAgentIDFromEnv() string {
	return os.Getenv("PANTHEON_AGENT_ID")
}

// SetAgentIDInEnv stores the agent ID in an env file for later retrieval.
// Since hook processes are short-lived, we store the agent ID in a file
// keyed by the tmux pane ID.
func SetAgentIDForPane(paneID, agentID string) error {
	if paneID == "" {
		return nil
	}
	stateDir := defaultStateDir()
	if stateDir == "" {
		return nil
	}
	path := stateDir + "/pantheon-agents.json"
	data, _ := os.ReadFile(path)
	var agents map[string]string
	if len(data) > 0 {
		_ = json.Unmarshal(data, &agents)
	}
	if agents == nil {
		agents = make(map[string]string)
	}
	agents[paneID] = agentID
	newData, _ := json.Marshal(agents)
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return err
	}
	return os.WriteFile(path, newData, 0600)
}

// GetAgentIDForPane retrieves the agent ID for a given tmux pane.
func GetAgentIDForPane(paneID string) string {
	if paneID == "" {
		return ""
	}
	stateDir := defaultStateDir()
	if stateDir == "" {
		return ""
	}
	path := stateDir + "/pantheon-agents.json"
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var agents map[string]string
	if err := json.Unmarshal(data, &agents); err != nil {
		return ""
	}
	return agents[paneID]
}

// ClearAgentIDForPane removes the agent ID for a pane.
func ClearAgentIDForPane(paneID string) {
	if paneID == "" {
		return
	}
	stateDir := defaultStateDir()
	if stateDir == "" {
		return
	}
	path := stateDir + "/pantheon-agents.json"
	data, _ := os.ReadFile(path)
	var agents map[string]string
	if len(data) > 0 {
		_ = json.Unmarshal(data, &agents)
	}
	if agents == nil {
		return
	}
	delete(agents, paneID)
	newData, _ := json.Marshal(agents)
	_ = os.MkdirAll(stateDir, 0700)
	_ = os.WriteFile(path, newData, 0600)
}

func defaultStateDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home + "/.local/share/beacon"
}

// GetCurrentPaneID returns the current tmux pane ID.
func GetCurrentPaneID() string {
	out, err := exec.Command("tmux", "display", "-p", "#{pane_id}").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// GetCurrentPID returns the current process's PID.
func GetCurrentPID() int {
	return os.Getpid()
}

// FormatPID returns the PID as a string.
func FormatPID(pid int) string {
	return strconv.Itoa(pid)
}
