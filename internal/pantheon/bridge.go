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
	"path/filepath"
	"strings"
	"time"
)

// BridgeConfig holds the configuration for the Pantheon bridge.
type BridgeConfig struct {
	SocketPath string // path to pantheond.sock
	CliPath    string // path to pantheon CLI binary
	Enabled    bool   // whether the bridge is enabled
}

// LoadConfig reads the bridge configuration from environment variables.
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

// paneState is persisted per-pane to deduplicate hook calls.
// Keyed by tmux pane ID in pantheon-panes.json.
type paneState struct {
	RunID        string `json:"run_id"`
	AgentID      string `json:"agent_id"`
	ProjectID    string `json:"project_id"`
	RegisteredAt int64  `json:"registered_at"`
}

// RegisterAgent registers an agent in Pantheon. If the pane already has an
// active run (registered within the last 2 hours and not yet completed),
// it reuses the existing run instead of creating a new one.
func RegisterAgent(ctx context.Context, cfg BridgeConfig, info AgentInfo) (*RegisterResult, error) {
	if !cfg.Enabled {
		return nil, nil
	}

	paneID := GetCurrentPaneID()
	if paneID == "" {
		return nil, nil
	}

	// Check if this pane already has an active run.
	existing := getPaneState(paneID)
	if existing != nil && existing.AgentID != "" {
		// Reuse if registered within last 2h and agent is still running.
		if time.Since(time.Unix(existing.RegisteredAt, 0)) < 2*time.Hour {
			if isAgentRunning(ctx, cfg, existing.AgentID) {
				return &RegisterResult{RunID: existing.RunID, AgentID: existing.AgentID}, nil
			}
		}
	}

	// Check if cwd is a git repo. If not, skip registration entirely.
	if !isGitRepo(info.Cwd) {
		return nil, nil
	}

	runtime := detectRuntime(info.Runtime)

	// Find or create a project for this cwd.
	projectID := findOrCreateProject(ctx, cfg, info.Cwd)
	if projectID == "" {
		return nil, fmt.Errorf("pantheon: failed to find or create project for %s", info.Cwd)
	}

	runID := createRun(ctx, cfg, projectID, info.Prompt)
	if runID == "" {
		return nil, fmt.Errorf("pantheon: failed to create run")
	}

	agentID := startRun(ctx, cfg, runID)
	if agentID == "" {
		return nil, fmt.Errorf("pantheon: failed to start run")
	}

	_ = runtime
	st := paneState{
		RunID:        runID,
		AgentID:      agentID,
		ProjectID:    projectID,
		RegisteredAt: time.Now().Unix(),
	}
	setPaneState(paneID, st)
	return &RegisterResult{RunID: runID, AgentID: agentID}, nil
}

// CompleteAgent marks an agent as completed in Pantheon, auto-verifies the
// run (R0 auto-accept), and clears the pane state. This is the full hook-stop
// flow: agent.complete → register verifier → run.verify PASS → clear pane.
// If the agent is already exited, the CONFLICT error is silently ignored.
func CompleteAgent(ctx context.Context, cfg BridgeConfig, agentID string) error {
	if !cfg.Enabled || agentID == "" {
		return nil
	}

	// 1. Mark agent as completed.
	_, err := callRPC(ctx, cfg, "agent.complete", map[string]any{
		"agent_id":  agentID,
		"exit_code": 0,
	})
	if err != nil && !strings.Contains(err.Error(), "CONFLICT") {
		return err
	}

	// 2. Get the run ID for this agent.
	paneID := GetCurrentPaneID()
	st := getPaneState(paneID)
	if st == nil || st.RunID == "" {
		return nil
	}

	// 3. Register a verifier agent and auto-verify (R0 auto-accept).
	verifierID := registerVerifier(ctx, cfg, st.RunID)
	if verifierID != "" {
		verifyRun(ctx, cfg, st.RunID, verifierID)
	}

	// 4. Clear pane state.
	ClearPaneState(paneID)
	return nil
}

// registerVerifier registers a verifier agent for a run.
func registerVerifier(ctx context.Context, cfg BridgeConfig, runID string) string {
	result, err := callRPC(ctx, cfg, "agent.register", map[string]any{
		"run_id":  runID,
		"role":    "verifier",
		"runtime": "devin",
		"pid":     0,
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

// verifyRun auto-verifies a run with PASS verdict.
func verifyRun(ctx context.Context, cfg BridgeConfig, runID, verifierID string) {
	// Get an event ID for evidence.
	events, err := callRPC(ctx, cfg, "run.events", map[string]any{
		"run_id": runID,
	})
	if err != nil {
		return
	}
	var evResp struct {
		Result struct {
			Events []struct {
				EventID string `json:"event_id"`
			} `json:"events"`
		} `json:"result"`
	}
	if err := json.Unmarshal(events, &evResp); err != nil {
		return
	}
	evidenceRef := ""
	if len(evResp.Result.Events) > 0 {
		evidenceRef = evResp.Result.Events[0].EventID
	}

	callRPC(ctx, cfg, "run.verify", map[string]any{
		"run_id":            runID,
		"verifier_agent_id": verifierID,
		"verdict":           "PASS",
		"evidence_ref":      evidenceRef,
	})
}

// detectRuntime returns the runtime name from the environment.
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

// isGitRepo checks if path is inside a git repository.
func isGitRepo(path string) bool {
	if path == "" {
		return false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	cmd := exec.Command("git", "-C", abs, "rev-parse", "--is-inside-work-tree")
	cmd.Env = os.Environ()
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "true"
}

// isAgentRunning checks if an agent is still in running state.
func isAgentRunning(ctx context.Context, cfg BridgeConfig, agentID string) bool {
	result, err := callRPC(ctx, cfg, "run.list", map[string]any{})
	if err != nil {
		return false
	}
	var resp struct {
		Result struct {
			Runs []struct {
				AgentID string `json:"agent_id"`
				State   string `json:"state"`
			} `json:"runs"`
		} `json:"result"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return false
	}
	for _, r := range resp.Result.Runs {
		if r.AgentID == agentID {
			return r.State == "running"
		}
	}
	return false
}

// findOrCreateProject finds an existing project matching the cwd or creates one.
// It uses the basename of the cwd as the project name and the cwd as repo_path.
func findOrCreateProject(ctx context.Context, cfg BridgeConfig, cwd string) string {
	abs, _ := filepath.Abs(cwd)
	name := filepath.Base(abs)

	result, err := callRPC(ctx, cfg, "project.list", map[string]any{})
	if err != nil {
		return ""
	}
	var resp struct {
		Result struct {
			Projects []struct {
				ProjectID string `json:"project_id"`
				Name      string `json:"name"`
				RepoPath  string `json:"repo_path"`
			} `json:"projects"`
		} `json:"result"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return ""
	}
	for _, p := range resp.Result.Projects {
		if p.RepoPath == abs {
			return p.ProjectID
		}
	}

	result, err = callRPC(ctx, cfg, "project.register", map[string]any{
		"name":      name,
		"repo_path": abs,
		"base_ref":  detectBaseRef(abs),
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

// detectBaseRef returns the current branch name of the repo, or "main" as fallback.
func detectBaseRef(repoPath string) string {
	out, err := exec.Command("git", "-C", repoPath, "symbolic-ref", "--short", "HEAD").Output()
	if err == nil {
		branch := strings.TrimSpace(string(out))
		if branch != "" {
			return branch
		}
	}
	return "main"
}

// createRun creates a run in Pantheon.
func createRun(ctx context.Context, cfg BridgeConfig, projectID, objective string) string {
	params := map[string]any{
		"project_id": projectID,
		"objective":  objective,
		"risk_level": "R0",
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
func callRPC(ctx context.Context, cfg BridgeConfig, method string, params any) ([]byte, error) {
	paramJSON, _ := json.Marshal(params)
	cmd := exec.CommandContext(ctx, cfg.CliPath, method, string(paramJSON))
	cmd.Env = append(os.Environ(), "PANTHEON_SOCKET="+cfg.SocketPath)
	out, err := cmd.Output()
	if err != nil {
		// Check if stderr contains a JSON-RPC error.
		if exitErr, ok := err.(*exec.ExitError); ok {
			stderr := string(exitErr.Stderr)
			if stderr != "" {
				return out, fmt.Errorf("%s", stderr)
			}
		}
		return out, err
	}
	return out, nil
}

// --- Pane state persistence ---

func getPaneState(paneID string) *paneState {
	stateDir := defaultStateDir()
	if stateDir == "" {
		return nil
	}
	path := stateDir + "/pantheon-panes.json"
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var panes map[string]paneState
	if err := json.Unmarshal(data, &panes); err != nil {
		return nil
	}
	st, ok := panes[paneID]
	if !ok {
		return nil
	}
	return &st
}

func setPaneState(paneID string, st paneState) {
	stateDir := defaultStateDir()
	if stateDir == "" {
		return
	}
	_ = os.MkdirAll(stateDir, 0700)
	path := stateDir + "/pantheon-panes.json"
	data, _ := os.ReadFile(path)
	var panes map[string]paneState
	if len(data) > 0 {
		_ = json.Unmarshal(data, &panes)
	}
	if panes == nil {
		panes = make(map[string]paneState)
	}
	panes[paneID] = st
	newData, _ := json.Marshal(panes)
	_ = os.WriteFile(path, newData, 0600)
}

// ClearPaneState removes the state for a pane (after agent.complete).
func ClearPaneState(paneID string) {
	if paneID == "" {
		return
	}
	stateDir := defaultStateDir()
	if stateDir == "" {
		return
	}
	path := stateDir + "/pantheon-panes.json"
	data, _ := os.ReadFile(path)
	var panes map[string]paneState
	if len(data) > 0 {
		_ = json.Unmarshal(data, &panes)
	}
	if panes == nil {
		return
	}
	delete(panes, paneID)
	newData, _ := json.Marshal(panes)
	_ = os.WriteFile(path, newData, 0600)
}

// GetAgentIDForPane retrieves the agent ID for a given tmux pane.
func GetAgentIDForPane(paneID string) string {
	st := getPaneState(paneID)
	if st == nil {
		return ""
	}
	return st.AgentID
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
