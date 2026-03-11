package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"symphony-go/internal/model"
	"symphony-go/internal/secret"
)

func TestNewFromWorkflowAppliesDefaultsAndCoercions(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "secret-key")
	t.Setenv("LINEAR_PROJECT_SLUG", "demo-from-env")
	t.Setenv("LINEAR_BRANCH_SCOPE", "Demo Scope")
	homeDir, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir() error = %v", err)
	}

	definition := &model.WorkflowDefinition{
		Config: map[string]any{
			"tracker": map[string]any{
				"kind":            "linear",
				"api_key":         "$LINEAR_API_KEY",
				"project_slug":    "$LINEAR_PROJECT_SLUG",
				"linear":          map[string]any{"children_block_parent": false},
				"repo":            "ignored-repo",
				"active_states":   "Todo, In Progress",
				"terminal_states": []any{"Closed", "Done"},
			},
			"polling": map[string]any{"interval_ms": "45000"},
			"workspace": map[string]any{
				"root":                "~/symphony",
				"linear_branch_scope": "$LINEAR_BRANCH_SCOPE",
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
	if cfg.TrackerProjectSlug != "demo-from-env" {
		t.Fatalf("TrackerProjectSlug = %q, want demo-from-env", cfg.TrackerProjectSlug)
	}
	if cfg.TrackerLinearChildrenBlockParent {
		t.Fatal("TrackerLinearChildrenBlockParent = true, want false")
	}
	if cfg.PollIntervalMS != 45000 {
		t.Fatalf("PollIntervalMS = %d, want 45000", cfg.PollIntervalMS)
	}
	if cfg.WorkspaceRoot != filepath.Join(homeDir, "symphony") {
		t.Fatalf("WorkspaceRoot = %q, want %q", cfg.WorkspaceRoot, filepath.Join(homeDir, "symphony"))
	}
	if cfg.WorkspaceLinearBranchScope != "demo-scope" {
		t.Fatalf("WorkspaceLinearBranchScope = %q, want demo-scope", cfg.WorkspaceLinearBranchScope)
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
	base.WorkspaceLinearBranchScope = "demo-scope"

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
			name: "missing branch scope",
			mutate: func(cfg *model.ServiceConfig) {
				cfg.WorkspaceLinearBranchScope = ""
			},
			target: model.ErrWorkflowParseError,
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

func TestValidateForDispatchFailsWhenHookRequiresMissingEnv(t *testing.T) {
	t.Setenv("SYMPHONY_GIT_REPO", "")

	cfg := defaultServiceConfig()
	cfg.TrackerKind = "linear"
	cfg.TrackerAPIKey = "secret"
	cfg.TrackerProjectSlug = "demo"
	cfg.WorkspaceLinearBranchScope = "demo-scope"
	cfg.HookBeforeRun = stringPointer(`repo_url="${SYMPHONY_GIT_REPO:?SYMPHONY_GIT_REPO is required}"`)

	err := ValidateForDispatch(cfg)
	if !errors.Is(err, model.ErrWorkflowParseError) {
		t.Fatalf("ValidateForDispatch() error = %v, want ErrWorkflowParseError", err)
	}
	if err == nil || !strings.Contains(err.Error(), "SYMPHONY_GIT_REPO") {
		t.Fatalf("ValidateForDispatch() error = %v, want missing env detail", err)
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
			if !cfg.TrackerLinearChildrenBlockParent {
				t.Fatal("TrackerLinearChildrenBlockParent = false, want default true")
			}
		})
	}
}

func TestNewFromWorkflowUsesDefaultResolver(t *testing.T) {
	originalResolver := secret.DefaultResolver
	secret.DefaultResolver = func(key string) (string, bool) {
		if key == "LINEAR_API_KEY" {
			return "resolver-secret", true
		}
		return "", false
	}
	t.Cleanup(func() { secret.DefaultResolver = originalResolver })

	cfg, err := NewFromWorkflow(&model.WorkflowDefinition{
		Config: map[string]any{
			"tracker": map[string]any{
				"kind":     "linear",
				"api_key":  "$LINEAR_API_KEY",
				"endpoint": "https://api.linear.app/graphql",
			},
		},
	})
	if err != nil {
		t.Fatalf("NewFromWorkflow() error = %v", err)
	}
	if cfg.TrackerAPIKey != "resolver-secret" {
		t.Fatalf("TrackerAPIKey = %q, want resolver-secret", cfg.TrackerAPIKey)
	}
}

func TestValidateForDispatchUsesDefaultResolverForHookEnv(t *testing.T) {
	originalResolver := secret.DefaultResolver
	secret.DefaultResolver = func(key string) (string, bool) {
		if key == "SYMPHONY_GIT_REPO" {
			return "https://example.com/repo.git", true
		}
		return "", false
	}
	t.Cleanup(func() { secret.DefaultResolver = originalResolver })

	cfg := defaultServiceConfig()
	cfg.TrackerKind = "linear"
	cfg.TrackerAPIKey = "secret"
	cfg.TrackerProjectSlug = "demo"
	cfg.WorkspaceLinearBranchScope = "demo-scope"
	cfg.HookBeforeRun = stringPointer(`repo_url="${SYMPHONY_GIT_REPO:?SYMPHONY_GIT_REPO is required}"`)

	if err := ValidateForDispatch(cfg); err != nil {
		t.Fatalf("ValidateForDispatch() error = %v, want nil", err)
	}
}
