package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"symphony-go/internal/model"
)

func TestNewFromWorkflowAppliesDefaultsAndCoercions(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "secret-key")
	homeDir, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir() error = %v", err)
	}

	definition := &model.WorkflowDefinition{
		Config: map[string]any{
			"tracker": map[string]any{
				"kind":            "linear",
				"api_key":         "$LINEAR_API_KEY",
				"project_slug":    "demo",
				"repo":            "ignored-repo",
				"active_states":   "Todo, In Progress",
				"terminal_states": []any{"Closed", "Done"},
			},
			"polling": map[string]any{"interval_ms": "45000"},
			"workspace": map[string]any{
				"root":                "~/symphony",
				"linear_branch_scope": "Symphony Go",
			},
			"hooks": map[string]any{
				"before_run": "echo hi",
				"timeout_ms": "12000",
			},
			"agent": map[string]any{
				"max_turns": "30",
				"max_concurrent_agents_by_state": map[string]any{
					" Todo ": 2,
					"bad":    0,
					"oops":   "x",
				},
			},
			"orchestrator": map[string]any{
				"auto_close_on_pr": false,
			},
			"codex": map[string]any{
				"thread_sandbox":      "workspace-write",
				"turn_sandbox_policy": map[string]any{"type": "workspaceWrite"},
			},
			"server": map[string]any{"port": "0"},
		},
	}

	cfg, err := NewFromWorkflow(definition)
	if err != nil {
		t.Fatalf("NewFromWorkflow() error = %v", err)
	}

	if cfg.TrackerAPIKey != "secret-key" {
		t.Fatalf("TrackerAPIKey = %q, want secret-key", cfg.TrackerAPIKey)
	}
	if cfg.PollIntervalMS != 45000 {
		t.Fatalf("PollIntervalMS = %d, want 45000", cfg.PollIntervalMS)
	}
	if cfg.WorkspaceRoot != filepath.Join(homeDir, "symphony") {
		t.Fatalf("WorkspaceRoot = %q, want %q", cfg.WorkspaceRoot, filepath.Join(homeDir, "symphony"))
	}
	if cfg.WorkspaceLinearBranchScope != "symphony-go" {
		t.Fatalf("WorkspaceLinearBranchScope = %q, want symphony-go", cfg.WorkspaceLinearBranchScope)
	}
	if cfg.HookBeforeRun == nil || *cfg.HookBeforeRun != "echo hi" {
		t.Fatalf("HookBeforeRun = %v, want echo hi", cfg.HookBeforeRun)
	}
	if cfg.HookTimeoutMS != 12000 {
		t.Fatalf("HookTimeoutMS = %d, want 12000", cfg.HookTimeoutMS)
	}
	if cfg.MaxTurns != 30 {
		t.Fatalf("MaxTurns = %d, want 30", cfg.MaxTurns)
	}
	if got := cfg.MaxConcurrentAgentsByState["todo"]; got != 2 {
		t.Fatalf("MaxConcurrentAgentsByState[todo] = %d, want 2", got)
	}
	if len(cfg.MaxConcurrentAgentsByState) != 1 {
		t.Fatalf("MaxConcurrentAgentsByState size = %d, want 1", len(cfg.MaxConcurrentAgentsByState))
	}
	if cfg.OrchestratorAutoCloseOnPR {
		t.Fatal("OrchestratorAutoCloseOnPR = true, want false")
	}
	if cfg.ServerPort == nil || *cfg.ServerPort != 0 {
		t.Fatalf("ServerPort = %v, want 0", cfg.ServerPort)
	}
	if cfg.CodexCommand != "codex app-server" {
		t.Fatalf("CodexCommand = %q, want default", cfg.CodexCommand)
	}
	if cfg.CodexTurnSandboxPolicy != `{"type":"workspaceWrite"}` {
		t.Fatalf("CodexTurnSandboxPolicy = %q", cfg.CodexTurnSandboxPolicy)
	}
}

func TestValidateForDispatch(t *testing.T) {
	base := defaultServiceConfig()
	base.TrackerKind = "linear"
	base.TrackerAPIKey = "secret"
	base.TrackerProjectSlug = "demo"

	tests := []struct {
		name   string
		mutate func(*model.ServiceConfig)
		target error
	}{
		{
			name: "missing tracker kind",
			mutate: func(cfg *model.ServiceConfig) {
				cfg.TrackerKind = ""
			},
			target: model.ErrUnsupportedTrackerKind,
		},
		{
			name: "missing api key",
			mutate: func(cfg *model.ServiceConfig) {
				cfg.TrackerAPIKey = ""
			},
			target: model.ErrMissingTrackerAPIKey,
		},
		{
			name: "missing project slug",
			mutate: func(cfg *model.ServiceConfig) {
				cfg.TrackerProjectSlug = ""
			},
			target: model.ErrMissingTrackerProjectSlug,
		},
		{
			name: "missing codex command",
			mutate: func(cfg *model.ServiceConfig) {
				cfg.CodexCommand = ""
			},
			target: model.ErrInvalidCodexCommand,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := *base
			cfg.MaxConcurrentAgentsByState = map[string]int{}
			tc.mutate(&cfg)

			err := ValidateForDispatch(&cfg)
			if !errors.Is(err, tc.target) {
				t.Fatalf("ValidateForDispatch() error = %v, want %v", err, tc.target)
			}
		})
	}
}

func TestNewFromWorkflowFallsBackToDefaultHookTimeoutForNonPositiveValues(t *testing.T) {
	tests := []struct {
		name    string
		rawTime any
	}{
		{name: "zero", rawTime: 0},
		{name: "negative int", rawTime: -1},
		{name: "negative string", rawTime: "-10"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := NewFromWorkflow(&model.WorkflowDefinition{
				Config: map[string]any{
					"hooks": map[string]any{
						"timeout_ms": tc.rawTime,
					},
				},
			})
			if err != nil {
				t.Fatalf("NewFromWorkflow() error = %v", err)
			}
			if cfg.HookTimeoutMS != 60000 {
				t.Fatalf("HookTimeoutMS = %d, want default 60000", cfg.HookTimeoutMS)
			}
		})
	}
}
