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
				"branch_namespace":    " Runner Alias ",
				"git": map[string]any{
					"author_name":  " runner-bot ",
					"author_email": " runner-bot@symphony.invalid ",
				},
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
	t.Setenv("SYMPHONY_GIT_REPO_URL", "")

	cfg := defaultServiceConfig()
	cfg.TrackerKind = "linear"
	cfg.TrackerAPIKey = "secret"
	cfg.TrackerProjectSlug = "demo"
	cfg.WorkspaceLinearBranchScope = "demo-scope"
	cfg.HookBeforeRun = stringPointer(`repo_url="${SYMPHONY_GIT_REPO_URL:?SYMPHONY_GIT_REPO_URL is required}"`)

	err := ValidateForDispatch(cfg)
	if !errors.Is(err, model.ErrWorkflowParseError) {
		t.Fatalf("ValidateForDispatch() error = %v, want ErrWorkflowParseError", err)
	}
	if err == nil || !strings.Contains(err.Error(), "SYMPHONY_GIT_REPO_URL") {
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

func TestNewFromWorkflowParsesSessionPersistenceAndNotifications(t *testing.T) {
	t.Setenv("SLACK_WEBHOOK_URL", "https://hooks.slack.example/services/test")
	t.Setenv("WEBHOOK_AUTH_HEADER", "Bearer secret")
	homeDir, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir() error = %v", err)
	}

	cfg, err := NewFromWorkflow(&model.WorkflowDefinition{
		Config: map[string]any{
			"session_persistence": map[string]any{
				"enabled": true,
				"kind":    "file",
				"file": map[string]any{
					"path":              "~/session-state.json",
					"flush_interval_ms": "2500",
					"fsync_on_critical": "false",
				},
			},
			"notifications": map[string]any{
				"channels": []any{
					map[string]any{
						"id":           "slack-team",
						"display_name": "Slack Team",
						"kind":         "slack",
						"slack": map[string]any{
							"incoming_webhook_url": "$SLACK_WEBHOOK_URL",
						},
						"subscriptions": map[string]any{
							"types": []any{"issue_completed", "issue_failed"},
						},
					},
					map[string]any{
						"id":           "ops-webhook",
						"display_name": "Ops Webhook",
						"kind":         "webhook",
						"webhook": map[string]any{
							"url": "https://hooks.example.com/symphony",
							"headers": map[string]any{
								"Authorization": "$WEBHOOK_AUTH_HEADER",
							},
						},
						"subscriptions": map[string]any{
							"types": []any{"system_alert"},
						},
					},
				},
				"defaults": map[string]any{
					"timeout_ms":          "8000",
					"retry_count":         "3",
					"retry_delay_ms":      "1500",
					"queue_size":          "64",
					"critical_queue_size": "16",
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("NewFromWorkflow() error = %v", err)
	}

	if !cfg.SessionPersistence.Enabled {
		t.Fatal("SessionPersistence.Enabled = false, want true")
	}
	if cfg.SessionPersistence.Kind != model.SessionPersistenceKindFile {
		t.Fatalf("SessionPersistence.Kind = %q, want file", cfg.SessionPersistence.Kind)
	}
	if cfg.SessionPersistence.File.Path != filepath.Join(homeDir, "session-state.json") {
		t.Fatalf("SessionPersistence.File.Path = %q, want %q", cfg.SessionPersistence.File.Path, filepath.Join(homeDir, "session-state.json"))
	}
	if cfg.SessionPersistence.File.FlushIntervalMS != 2500 {
		t.Fatalf("SessionPersistence.File.FlushIntervalMS = %d, want 2500", cfg.SessionPersistence.File.FlushIntervalMS)
	}
	if cfg.SessionPersistence.File.FsyncOnCritical {
		t.Fatal("SessionPersistence.File.FsyncOnCritical = true, want false")
	}
	if len(cfg.Notifications.Channels) != 2 {
		t.Fatalf("Notifications.Channels size = %d, want 2", len(cfg.Notifications.Channels))
	}
	if cfg.Notifications.Channels[0].Slack == nil || cfg.Notifications.Channels[0].Slack.IncomingWebhookURL != "https://hooks.slack.example/services/test" {
		t.Fatalf("Notifications.Channels[0].Slack = %+v", cfg.Notifications.Channels[0].Slack)
	}
	if got := cfg.Notifications.Channels[1].Webhook.Headers["Authorization"]; got != "Bearer secret" {
		t.Fatalf("Notifications.Channels[1].Headers[Authorization] = %q, want Bearer secret", got)
	}
	if cfg.Notifications.Defaults.TimeoutMS != 8000 {
		t.Fatalf("Notifications.Defaults.TimeoutMS = %d, want 8000", cfg.Notifications.Defaults.TimeoutMS)
	}
	if cfg.Notifications.Defaults.RetryCount != 3 {
		t.Fatalf("Notifications.Defaults.RetryCount = %d, want 3", cfg.Notifications.Defaults.RetryCount)
	}
	if cfg.Notifications.Defaults.RetryDelayMS != 1500 {
		t.Fatalf("Notifications.Defaults.RetryDelayMS = %d, want 1500", cfg.Notifications.Defaults.RetryDelayMS)
	}
	if cfg.Notifications.Defaults.QueueSize != 64 {
		t.Fatalf("Notifications.Defaults.QueueSize = %d, want 64", cfg.Notifications.Defaults.QueueSize)
	}
	if cfg.Notifications.Defaults.CriticalQueueSize != 16 {
		t.Fatalf("Notifications.Defaults.CriticalQueueSize = %d, want 16", cfg.Notifications.Defaults.CriticalQueueSize)
	}
}

func TestNewFromWorkflowRejectsLegacyRuntimeExtensionKeys(t *testing.T) {
	tests := []struct {
		name    string
		config  map[string]any
		wantErr string
	}{
		{
			name: "session persistence backend alias",
			config: map[string]any{
				"session_persistence": map[string]any{
					"enabled": true,
					"backend": "file",
				},
			},
			wantErr: "runtime.session_persistence.backend",
		},
		{
			name: "session persistence path alias",
			config: map[string]any{
				"session_persistence": map[string]any{
					"path": "./state.json",
				},
			},
			wantErr: "runtime.session_persistence.path",
		},
		{
			name: "session persistence flush alias",
			config: map[string]any{
				"session_persistence": map[string]any{
					"flush_interval_ms": 1000,
				},
			},
			wantErr: "runtime.session_persistence.flush_interval_ms",
		},
		{
			name: "session persistence fsync alias",
			config: map[string]any{
				"session_persistence": map[string]any{
					"fsync_on_critical": true,
				},
			},
			wantErr: "runtime.session_persistence.fsync_on_critical",
		},
		{
			name: "notification legacy channel fields",
			config: map[string]any{
				"notifications": map[string]any{
					"channels": []any{
						map[string]any{
							"id":   "ops",
							"kind": "webhook",
							"name": "Ops",
						},
					},
				},
			},
			wantErr: "runtime.notifications.channels[0].name",
		},
		{
			name: "notification events alias",
			config: map[string]any{
				"notifications": map[string]any{
					"channels": []any{
						map[string]any{
							"id":     "ops",
							"kind":   "webhook",
							"events": []any{"system_alert"},
						},
					},
				},
			},
			wantErr: "runtime.notifications.channels[0].events",
		},
		{
			name: "notification url alias",
			config: map[string]any{
				"notifications": map[string]any{
					"channels": []any{
						map[string]any{
							"id":   "ops",
							"kind": "webhook",
							"url":  "https://hooks.example.com/symphony",
						},
					},
				},
			},
			wantErr: "runtime.notifications.channels[0].url",
		},
		{
			name: "notification headers alias",
			config: map[string]any{
				"notifications": map[string]any{
					"channels": []any{
						map[string]any{
							"id":      "ops",
							"kind":    "webhook",
							"headers": map[string]any{"Authorization": "Bearer test"},
						},
					},
				},
			},
			wantErr: "runtime.notifications.channels[0].headers",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewFromWorkflow(&model.WorkflowDefinition{Config: tc.config})
			if !errors.Is(err, model.ErrWorkflowParseError) {
				t.Fatalf("NewFromWorkflow() error = %v, want ErrWorkflowParseError", err)
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("NewFromWorkflow() error = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidateForDispatchRejectsInvalidRuntimeExtensions(t *testing.T) {
	base := defaultServiceConfig()
	base.TrackerKind = "linear"
	base.TrackerAPIKey = "secret"
	base.TrackerProjectSlug = "demo"
	base.WorkspaceLinearBranchScope = "demo-scope"

	tests := []struct {
		name   string
		mutate func(*model.ServiceConfig)
	}{
		{
			name: "missing session persistence path",
			mutate: func(cfg *model.ServiceConfig) {
				cfg.SessionPersistence.Enabled = true
				cfg.SessionPersistence.Kind = model.SessionPersistenceKindFile
				cfg.SessionPersistence.File.Path = ""
			},
		},
		{
			name: "missing notification url",
			mutate: func(cfg *model.ServiceConfig) {
				cfg.Notifications.Channels = []model.NotificationChannelConfig{
					{
						ID:   "ops",
						Kind: model.NotificationChannelKindWebhook,
						Subscriptions: model.NotificationSubscriptionConfig{
							Types: []model.NotificationEventType{model.NotificationEventSystemAlert},
						},
						Delivery: cfg.Notifications.Defaults,
					},
				}
			},
		},
		{
			name: "invalid notification event",
			mutate: func(cfg *model.ServiceConfig) {
				cfg.Notifications.Channels = []model.NotificationChannelConfig{
					{
						ID:   "ops",
						Kind: model.NotificationChannelKindWebhook,
						Subscriptions: model.NotificationSubscriptionConfig{
							Types: []model.NotificationEventType{"bad_event"},
						},
						Delivery: cfg.Notifications.Defaults,
						Webhook: &model.WebhookNotificationConfig{
							URL: "https://hooks.example.com/symphony",
						},
					},
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := *base
			cfg.MaxConcurrentAgentsByState = map[string]int{}
			cfg.Notifications = base.Notifications
			tc.mutate(&cfg)

			err := ValidateForDispatch(&cfg)
			if !errors.Is(err, model.ErrWorkflowParseError) {
				t.Fatalf("ValidateForDispatch() error = %v, want ErrWorkflowParseError", err)
			}
		})
	}
}

func TestValidateForDispatchUsesDefaultResolverForHookEnv(t *testing.T) {
	originalResolver := secret.DefaultResolver
	secret.DefaultResolver = func(key string) (string, bool) {
		if key == "SYMPHONY_GIT_REPO_URL" {
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
	cfg.HookBeforeRun = stringPointer(`repo_url="${SYMPHONY_GIT_REPO_URL:?SYMPHONY_GIT_REPO_URL is required}"`)

	if err := ValidateForDispatch(cfg); err != nil {
		t.Fatalf("ValidateForDispatch() error = %v, want nil", err)
	}
}
