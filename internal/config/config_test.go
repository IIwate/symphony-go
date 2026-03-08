package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
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
				"active_states":   "Todo, In Progress",
				"terminal_states": []any{"Closed", "Done"},
			},
			"polling":   map[string]any{"interval_ms": "45000"},
			"workspace": map[string]any{"root": "~/symphony"},
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

func TestNewFromWorkflowAppliesGitHubDefaults(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "gh-secret")

	definition := &model.WorkflowDefinition{
		Config: map[string]any{
			"tracker": map[string]any{
				"kind":    "github",
				"api_key": "$GITHUB_TOKEN",
				"owner":   "octocat",
				"repo":    "demo",
			},
		},
	}

	cfg, err := NewFromWorkflow(definition)
	if err != nil {
		t.Fatalf("NewFromWorkflow() error = %v", err)
	}

	if cfg.TrackerAPIKey != "gh-secret" {
		t.Fatalf("TrackerAPIKey = %q, want gh-secret", cfg.TrackerAPIKey)
	}
	if cfg.TrackerEndpoint != "https://api.github.com" {
		t.Fatalf("TrackerEndpoint = %q, want GitHub default", cfg.TrackerEndpoint)
	}
	if cfg.TrackerOwner != "octocat" {
		t.Fatalf("TrackerOwner = %q, want octocat", cfg.TrackerOwner)
	}
	if cfg.TrackerRepo != "demo" {
		t.Fatalf("TrackerRepo = %q, want demo", cfg.TrackerRepo)
	}
	if cfg.TrackerStateLabelPrefix != "symphony:" {
		t.Fatalf("TrackerStateLabelPrefix = %q, want symphony:", cfg.TrackerStateLabelPrefix)
	}
	if !reflect.DeepEqual(cfg.ActiveStates, []string{"todo", "in-progress"}) {
		t.Fatalf("ActiveStates = %#v, want GitHub defaults", cfg.ActiveStates)
	}
	if !reflect.DeepEqual(cfg.TerminalStates, []string{"closed", "cancelled"}) {
		t.Fatalf("TerminalStates = %#v, want GitHub defaults", cfg.TerminalStates)
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

func TestValidateForDispatchGitHub(t *testing.T) {
	base := defaultServiceConfigForTracker("github")
	base.TrackerKind = "github"
	base.TrackerAPIKey = "secret"
	base.TrackerOwner = "octocat"
	base.TrackerRepo = "demo"

	valid := *base
	valid.MaxConcurrentAgentsByState = map[string]int{}
	valid.TrackerProjectSlug = ""
	if err := ValidateForDispatch(&valid); err != nil {
		t.Fatalf("ValidateForDispatch(valid github) error = %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*model.ServiceConfig)
		target error
	}{
		{
			name: "missing api key",
			mutate: func(cfg *model.ServiceConfig) {
				cfg.TrackerAPIKey = ""
			},
			target: model.ErrMissingTrackerAPIKey,
		},
		{
			name: "missing owner",
			mutate: func(cfg *model.ServiceConfig) {
				cfg.TrackerOwner = ""
			},
			target: model.ErrMissingTrackerOwner,
		},
		{
			name: "missing repo",
			mutate: func(cfg *model.ServiceConfig) {
				cfg.TrackerRepo = ""
			},
			target: model.ErrMissingTrackerRepo,
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
