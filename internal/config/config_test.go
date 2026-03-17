package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"symphony-go/internal/model"
	"symphony-go/internal/model/contract"
)

func TestParseWorkflowContractAndNewFromWorkflowApplyFormalSections(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "secret-key")
	t.Setenv("LINEAR_PROJECT_SLUG", "demo-from-env")
	t.Setenv("LINEAR_BRANCH_SCOPE", "Demo Scope")
	homeDir, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir() error = %v", err)
	}

	definition := &model.WorkflowDefinition{
		RootDir: "H:/code/temp/symphony-go/automation",
		Config:  validWorkflowConfigMap(),
	}
	workflowContract, err := ParseWorkflowContract(definition)
	if err != nil {
		t.Fatalf("ParseWorkflowContract() error = %v", err)
	}
	if workflowContract.Source.APIKeyRef.Kind != "env" || workflowContract.Source.APIKeyRef.Name != "LINEAR_API_KEY" {
		t.Fatalf("Source.APIKeyRef = %+v, want env LINEAR_API_KEY", workflowContract.Source.APIKeyRef)
	}
	if workflowContract.Capabilities.Static.Capabilities == nil || len(workflowContract.Capabilities.Static.Capabilities) == 0 {
		t.Fatal("Capabilities.Static.Capabilities must not be empty")
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
	if cfg.WorkspaceLinearBranchScope != "demo-scope" {
		t.Fatalf("WorkspaceLinearBranchScope = %q, want demo-scope", cfg.WorkspaceLinearBranchScope)
	}
	if cfg.PollIntervalMS != 45000 {
		t.Fatalf("PollIntervalMS = %d, want 45000", cfg.PollIntervalMS)
	}
	if cfg.WorkspaceRoot != filepath.Join(homeDir, "symphony") {
		t.Fatalf("WorkspaceRoot = %q, want %q", cfg.WorkspaceRoot, filepath.Join(homeDir, "symphony"))
	}
	if cfg.WorkspaceBranchNamespace != "Runner Alias" {
		t.Fatalf("WorkspaceBranchNamespace = %q, want Runner Alias", cfg.WorkspaceBranchNamespace)
	}
	if cfg.WorkspaceGitAuthorName != "runner-bot" {
		t.Fatalf("WorkspaceGitAuthorName = %q, want runner-bot", cfg.WorkspaceGitAuthorName)
	}
	if cfg.WorkspaceGitAuthorEmail != "runner-bot@symphony.invalid" {
		t.Fatalf("WorkspaceGitAuthorEmail = %q, want runner-bot@symphony.invalid", cfg.WorkspaceGitAuthorEmail)
	}
	if cfg.HookBeforeRun == nil || *cfg.HookBeforeRun != "echo hi" {
		t.Fatalf("HookBeforeRun = %v, want echo hi", cfg.HookBeforeRun)
	}
	if cfg.MaxTurns != 30 {
		t.Fatalf("MaxTurns = %d, want 30", cfg.MaxTurns)
	}
	if cfg.CodexTurnSandboxPolicy != `{"type":"workspaceWrite"}` {
		t.Fatalf("CodexTurnSandboxPolicy = %q", cfg.CodexTurnSandboxPolicy)
	}
	if cfg.SessionPersistence.File.Path != filepath.Join(homeDir, "session-state.json") {
		t.Fatalf("SessionPersistence.File.Path = %q, want %q", cfg.SessionPersistence.File.Path, filepath.Join(homeDir, "session-state.json"))
	}
}

func TestNewFromWorkflowResolvesNotificationSecretRefs(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "secret-key")
	t.Setenv("LINEAR_PROJECT_SLUG", "demo")
	t.Setenv("LINEAR_BRANCH_SCOPE", "demo-scope")
	t.Setenv("SLACK_WEBHOOK", "https://hooks.slack.example/services/test")
	t.Setenv("WEBHOOK_URL", "https://hooks.example.com/runtime")
	t.Setenv("WEBHOOK_TOKEN", "Bearer secret")

	definition := &model.WorkflowDefinition{
		Config: validWorkflowConfigMapWithNotifications(),
	}

	cfg, err := NewFromWorkflow(definition)
	if err != nil {
		t.Fatalf("NewFromWorkflow() error = %v", err)
	}
	if len(cfg.Notifications.Channels) != 2 {
		t.Fatalf("len(Notifications.Channels) = %d, want 2", len(cfg.Notifications.Channels))
	}
	if cfg.Notifications.Channels[0].Slack == nil || cfg.Notifications.Channels[0].Slack.IncomingWebhookURL != "https://hooks.slack.example/services/test" {
		t.Fatalf("Slack channel = %+v", cfg.Notifications.Channels[0].Slack)
	}
	if cfg.Notifications.Channels[1].Webhook == nil || cfg.Notifications.Channels[1].Webhook.URL != "https://hooks.example.com/runtime" {
		t.Fatalf("Webhook channel = %+v", cfg.Notifications.Channels[1].Webhook)
	}
	if got := cfg.Notifications.Channels[1].Webhook.Headers["Authorization"]; got != "Bearer secret" {
		t.Fatalf("Webhook.Headers[Authorization] = %q, want Bearer secret", got)
	}
}

func TestParseWorkflowContractRejectsInvalidFormalPersistenceAndSecretConfig(t *testing.T) {
	t.Setenv("LINEAR_PROJECT_SLUG", "demo")
	t.Setenv("LINEAR_BRANCH_SCOPE", "demo-scope")

	tests := []struct {
		name    string
		mutate  func(map[string]any)
		wantErr string
	}{
		{
			name: "file backend cannot claim production usage",
			mutate: func(cfg map[string]any) {
				getMap(cfg, "persistence")["backend"] = map[string]any{
					"kind":  "file",
					"usage": "production",
				}
			},
			wantErr: "file is limited to development/test/single_machine",
		},
		{
			name: "missing api key ref",
			mutate: func(cfg map[string]any) {
				getMap(getMap(cfg, "source_adapter"), "credentials")["api_key_ref"] = map[string]any{}
			},
			wantErr: "source_adapter.credentials.api_key_ref",
		},
		{
			name: "unknown job type",
			mutate: func(cfg map[string]any) {
				getMap(cfg, "job_policy")["supported_types"] = []any{"documentation"}
			},
			wantErr: "unsupported job type",
		},
		{
			name: "invalid notification header ref",
			mutate: func(cfg map[string]any) {
				cfgWithNotifications := validWorkflowConfigMapWithNotifications()
				cfg["service"] = cfgWithNotifications["service"]
				channels := getMapSlice(getMap(getMap(cfg, "service"), "notifications"), "channels")
				getMap(getMap(channels[1], "webhook"), "header_refs")["Authorization"] = map[string]any{"kind": "env"}
			},
			wantErr: "webhook.header_refs.Authorization",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			configMap := validWorkflowConfigMap()
			tc.mutate(configMap)
			_, err := ParseWorkflowContract(&model.WorkflowDefinition{Config: configMap})
			if !errors.Is(err, model.ErrWorkflowParseError) && !errors.Is(err, model.ErrMissingTrackerProjectSlug) {
				t.Fatalf("ParseWorkflowContract() error = %v, want workflow parse style error", err)
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ParseWorkflowContract() error = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestParseWorkflowContractSupportsExternalSecretProviders(t *testing.T) {
	configMap := validWorkflowConfigMap()
	getMap(configMap, "secrets")["providers"] = map[string]any{
		"env": map[string]any{"enabled": true},
		"external": []any{
			map[string]any{"name": "vault-main", "kind": "vault"},
		},
	}
	getMap(getMap(configMap, "source_adapter"), "credentials")["api_key_ref"] = map[string]any{
		"kind":      "provider",
		"provider":  "vault-main",
		"secret_id": "linear/api-key",
	}
	getMap(configMap, "source_adapter")["project_slug"] = "demo"
	getMap(configMap, "source_adapter")["branch_scope"] = "demo-scope"

	workflowContract, err := ParseWorkflowContract(&model.WorkflowDefinition{Config: configMap})
	if err != nil {
		t.Fatalf("ParseWorkflowContract() error = %v", err)
	}
	if len(workflowContract.Secrets.ExternalProviders) != 1 {
		t.Fatalf("len(Secrets.ExternalProviders) = %d, want 1", len(workflowContract.Secrets.ExternalProviders))
	}
	if workflowContract.Source.APIKeyRef.Kind != "provider" {
		t.Fatalf("Source.APIKeyRef.Kind = %q, want provider", workflowContract.Source.APIKeyRef.Kind)
	}
}

func TestValidateForDispatchUsesNewFieldPaths(t *testing.T) {
	cfg := defaultServiceConfig()
	cfg.TrackerKind = "linear"
	cfg.TrackerAPIKey = "secret"
	cfg.TrackerProjectSlug = "demo"
	cfg.WorkspaceLinearBranchScope = "demo-scope"

	tests := []struct {
		name    string
		mutate  func(*model.ServiceConfig)
		target  error
		wantErr string
	}{
		{
			name: "missing api key",
			mutate: func(cfg *model.ServiceConfig) {
				cfg.TrackerAPIKey = ""
			},
			target:  model.ErrMissingTrackerAPIKey,
			wantErr: "source_adapter.credentials.api_key_ref",
		},
		{
			name: "missing codex command",
			mutate: func(cfg *model.ServiceConfig) {
				cfg.CodexCommand = ""
			},
			target:  model.ErrInvalidCodexCommand,
			wantErr: "execution.backend.codex.command",
		},
		{
			name: "invalid persistence path",
			mutate: func(cfg *model.ServiceConfig) {
				cfg.SessionPersistence.Enabled = true
				cfg.SessionPersistence.File.Path = ""
			},
			target:  model.ErrWorkflowParseError,
			wantErr: "persistence.file.path",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			current := *cfg
			current.MaxConcurrentAgentsByState = map[string]int{}
			tc.mutate(&current)
			err := ValidateForDispatch(&current)
			if !errors.Is(err, tc.target) {
				t.Fatalf("ValidateForDispatch() error = %v, want %v", err, tc.target)
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ValidateForDispatch() error = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func validWorkflowConfigMap() map[string]any {
	return map[string]any{
		"service": map[string]any{
			"contract_version": "v1",
			"instance_name":    "symphony",
			"server": map[string]any{
				"host": "0.0.0.0",
				"port": "0",
			},
		},
		"domain": map[string]any{
			"id": "domain-main",
			"polling": map[string]any{
				"interval_ms": "45000",
			},
			"workspace": map[string]any{
				"root":             "~/symphony",
				"branch_namespace": " Runner Alias ",
				"git": map[string]any{
					"author_name":  " runner-bot ",
					"author_email": " runner-bot@symphony.invalid ",
				},
			},
		},
		"source_adapter": map[string]any{
			"name": "linear-main",
			"kind": "linear",
			"credentials": map[string]any{
				"api_key_ref": map[string]any{
					"kind": "env",
					"name": "LINEAR_API_KEY",
				},
			},
			"project_slug":  "$LINEAR_PROJECT_SLUG",
			"branch_scope":  "$LINEAR_BRANCH_SCOPE",
			"active_states": "Todo, In Progress",
			"terminal_states": []any{
				"Closed",
				"Done",
			},
			"linear": map[string]any{
				"children_block_parent": false,
			},
		},
		"execution": map[string]any{
			"backend": map[string]any{
				"kind": "codex",
				"codex": map[string]any{
					"command":             "codex app-server",
					"approval_policy":     "never",
					"thread_sandbox":      "workspace-write",
					"turn_sandbox_policy": map[string]any{"type": "workspaceWrite"},
				},
			},
			"agent": map[string]any{
				"max_turns": "30",
				"max_concurrent_agents_by_state": map[string]any{
					" Todo ": 2,
				},
			},
			"hooks": map[string]any{
				"before_run": "echo hi",
				"timeout_ms": "12000",
			},
		},
		"job_policy": map[string]any{
			"dispatch_flow": "implement",
			"supported_types": []any{
				string(contract.JobTypeCodeChange),
				string(contract.JobTypeLandChange),
				string(contract.JobTypeAnalysis),
				string(contract.JobTypeDiagnostic),
			},
		},
		"auth": map[string]any{
			"mode":                   "none",
			"leader_required":        true,
			"transparent_forwarding": false,
		},
		"persistence": map[string]any{
			"backend": map[string]any{
				"kind":  "file",
				"usage": "development",
			},
			"file": map[string]any{
				"path":              "~/session-state.json",
				"flush_interval_ms": "2500",
				"fsync_on_critical": false,
			},
			"archive": map[string]any{
				"enabled": true,
			},
			"retention": map[string]any{
				"allow_physical_delete": false,
			},
		},
		"secrets": map[string]any{
			"providers": map[string]any{
				"env": map[string]any{
					"enabled": true,
				},
			},
		},
	}
}

func validWorkflowConfigMapWithNotifications() map[string]any {
	cfg := validWorkflowConfigMap()
	getMap(cfg, "service")["notifications"] = map[string]any{
		"defaults": map[string]any{
			"timeout_ms":          8000,
			"retry_count":         3,
			"retry_delay_ms":      1500,
			"queue_size":          64,
			"critical_queue_size": 16,
		},
		"channels": []any{
			map[string]any{
				"id":   "ops-slack",
				"kind": "slack",
				"subscriptions": map[string]any{
					"types": []any{string(model.NotificationEventSystemAlert)},
				},
				"slack": map[string]any{
					"incoming_webhook_url_ref": map[string]any{
						"kind": "env",
						"name": "SLACK_WEBHOOK",
					},
				},
			},
			map[string]any{
				"id":   "ops-webhook",
				"kind": "webhook",
				"subscriptions": map[string]any{
					"types": []any{string(model.NotificationEventIssueFailed)},
				},
				"webhook": map[string]any{
					"url_ref": map[string]any{
						"kind": "env",
						"name": "WEBHOOK_URL",
					},
					"header_refs": map[string]any{
						"Authorization": map[string]any{
							"kind": "env",
							"name": "WEBHOOK_TOKEN",
						},
					},
				},
			},
		},
	}
	return cfg
}
