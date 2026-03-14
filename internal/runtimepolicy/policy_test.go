package runtimepolicy

import (
	"errors"
	"testing"

	"symphony-go/internal/model"
	"symphony-go/internal/model/contract"
)

func TestBuildRuntimeIdentityUsesStableCompatibilityFields(t *testing.T) {
	input := IdentityInput{
		ConfigDir: "automation",
		Profile:   "prod",
		AutomationDefinition: &model.AutomationDefinition{
			Selection: model.AutomationSelection{
				DispatchFlow:   "implement",
				EnabledSources: []string{"linear-main"},
			},
			Sources: map[string]*model.SourceDefinition{
				"linear-main": {
					Name: "linear-main",
					Raw:  map[string]any{"kind": "linear"},
				},
			},
		},
		ServiceConfig: &model.ServiceConfig{
			TrackerKind:        "linear",
			TrackerRepo:        "repo",
			TrackerProjectSlug: "proj",
			WorkspaceRoot:      "H:/workspaces",
			SessionPersistence: model.SessionPersistenceConfig{
				Kind: model.SessionPersistenceKindFile,
				File: model.SessionPersistenceFileConfig{
					Path: "automation/local/runtime-ledger.json",
				},
			},
		},
	}

	identity := BuildRuntimeIdentity(input)
	if identity.Compatibility.Profile != "prod" {
		t.Fatalf("identity.Compatibility.Profile = %q, want prod", identity.Compatibility.Profile)
	}
	if identity.Compatibility.ActiveSource != "linear-main" {
		t.Fatalf("identity.Compatibility.ActiveSource = %q, want linear-main", identity.Compatibility.ActiveSource)
	}
	if identity.Compatibility.SourceKind != "linear" {
		t.Fatalf("identity.Compatibility.SourceKind = %q, want linear", identity.Compatibility.SourceKind)
	}
	if identity.Compatibility.FlowName != "implement" {
		t.Fatalf("identity.Compatibility.FlowName = %q, want implement", identity.Compatibility.FlowName)
	}
	if identity.Descriptor.ConfigRoot != "automation" {
		t.Fatalf("identity.Descriptor.ConfigRoot = %q, want automation", identity.Descriptor.ConfigRoot)
	}
	if identity.Descriptor.WorkspaceRoot != "H:/workspaces" {
		t.Fatalf("identity.Descriptor.WorkspaceRoot = %q, want H:/workspaces", identity.Descriptor.WorkspaceRoot)
	}
	if identity.Descriptor.SessionPersistenceKind != "file" {
		t.Fatalf("identity.Descriptor.SessionPersistenceKind = %q, want file", identity.Descriptor.SessionPersistenceKind)
	}
	if identity.Descriptor.SessionStatePath != "automation/local/runtime-ledger.json" {
		t.Fatalf("identity.Descriptor.SessionStatePath = %q, want automation/local/runtime-ledger.json", identity.Descriptor.SessionStatePath)
	}
}

func TestEvaluateReloadReturnsHotReloadAllowedForWhitelistedChanges(t *testing.T) {
	currentRepoDef, currentCfg := fixtureConfig()
	nextRepoDef, nextCfg := fixtureConfig()
	nextCfg.PollIntervalMS = 45000

	decision := EvaluateReload(currentRepoDef, nextRepoDef, currentCfg, nextCfg)
	if decision.Outcome != ReloadOutcomeHotReloadAllowed {
		t.Fatalf("decision.Outcome = %q, want %q", decision.Outcome, ReloadOutcomeHotReloadAllowed)
	}
	if decision.FieldPath != "runtime.polling.interval_ms" {
		t.Fatalf("decision.FieldPath = %q, want runtime.polling.interval_ms", decision.FieldPath)
	}
	if decision.Reason == nil || decision.Reason.ReasonCode != contract.ReasonRuntimeReloadHotReloadAllowed {
		t.Fatalf("decision.Reason = %#v, want %q", decision.Reason, contract.ReasonRuntimeReloadHotReloadAllowed)
	}
}

func TestEvaluateReloadReturnsRestartRequiredForProtectedChanges(t *testing.T) {
	currentRepoDef, currentCfg := fixtureConfig()
	nextRepoDef, nextCfg := fixtureConfig()
	nextCfg.WorkspaceRoot = "H:/other-workspaces"

	decision := EvaluateReload(currentRepoDef, nextRepoDef, currentCfg, nextCfg)
	if decision.Outcome != ReloadOutcomeRestartRequired {
		t.Fatalf("decision.Outcome = %q, want %q", decision.Outcome, ReloadOutcomeRestartRequired)
	}
	if decision.FieldPath != "runtime.workspace.root" {
		t.Fatalf("decision.FieldPath = %q, want runtime.workspace.root", decision.FieldPath)
	}
	if decision.Reason == nil || decision.Reason.ReasonCode != contract.ReasonRuntimeReloadRestartRequired {
		t.Fatalf("decision.Reason = %#v, want %q", decision.Reason, contract.ReasonRuntimeReloadRestartRequired)
	}
	if decision.Reason.Details["field_path"] != "runtime.workspace.root" {
		t.Fatalf("decision.Reason.Details[field_path] = %v, want runtime.workspace.root", decision.Reason.Details["field_path"])
	}
}

func TestEvaluateReloadTreatsListenHostAsRestartBoundary(t *testing.T) {
	currentRepoDef, currentCfg := fixtureConfig()
	nextRepoDef, nextCfg := fixtureConfig()
	nextCfg.ServerHost = "0.0.0.0"

	decision := EvaluateReload(currentRepoDef, nextRepoDef, currentCfg, nextCfg)
	if decision.Outcome != ReloadOutcomeRestartRequired {
		t.Fatalf("decision.Outcome = %q, want %q", decision.Outcome, ReloadOutcomeRestartRequired)
	}
	if decision.FieldPath != "runtime.server.host" {
		t.Fatalf("decision.FieldPath = %q, want runtime.server.host", decision.FieldPath)
	}
}

func TestEvaluateReloadUsesDefinitionBoundaries(t *testing.T) {
	currentRepoDef, currentCfg := fixtureConfig()
	nextRepoDef, nextCfg := fixtureConfig()
	nextRepoDef.Selection.DispatchFlow = "other-flow"

	decision := EvaluateReload(currentRepoDef, nextRepoDef, currentCfg, nextCfg)
	if decision.Outcome != ReloadOutcomeRestartRequired {
		t.Fatalf("decision.Outcome = %q, want %q", decision.Outcome, ReloadOutcomeRestartRequired)
	}
	if decision.FieldPath != "selection.dispatch_flow" {
		t.Fatalf("decision.FieldPath = %q, want selection.dispatch_flow", decision.FieldPath)
	}
}

func TestRestartRequiredErrorWrapsStructuredDecision(t *testing.T) {
	decision := restartRequiredDecision("runtime.session_persistence")
	err := NewRestartRequiredError(decision)
	if err == nil {
		t.Fatal("NewRestartRequiredError() = nil, want error")
	}
	if err.Error() != "runtime.session_persistence changed: restart required" {
		t.Fatalf("err.Error() = %q, want runtime.session_persistence changed: restart required", err.Error())
	}

	wrapped := errors.New("outer: " + err.Error())
	if _, ok := IsRestartRequired(wrapped); ok {
		t.Fatal("IsRestartRequired() = true for unrelated error, want false")
	}

	restartErr, ok := IsRestartRequired(err)
	if !ok {
		t.Fatal("IsRestartRequired() = false, want true")
	}
	if restartErr.Decision.FieldPath != "runtime.session_persistence" {
		t.Fatalf("restartErr.Decision.FieldPath = %q, want runtime.session_persistence", restartErr.Decision.FieldPath)
	}
}

func fixtureConfig() (*model.AutomationDefinition, *model.ServiceConfig) {
	return &model.AutomationDefinition{
			Profile: "prod",
			Selection: model.AutomationSelection{
				DispatchFlow:   "implement",
				EnabledSources: []string{"linear-main"},
			},
			Sources: map[string]*model.SourceDefinition{
				"linear-main": {
					Name: "linear-main",
					Raw:  map[string]any{"kind": "linear"},
				},
			},
		}, &model.ServiceConfig{
			TrackerKind:                "linear",
			TrackerProjectSlug:         "proj",
			TrackerRepo:                "repo",
			PollIntervalMS:             30000,
			AutomationRootDir:          "automation",
			WorkspaceRoot:              "H:/workspaces",
			WorkspaceLinearBranchScope: "scope",
			WorkspaceBranchNamespace:   "runner-a",
			ServerHost:                 "127.0.0.1",
			MaxConcurrentAgents:        4,
			MaxRetryBackoffMS:          60000,
			CodexCommand:               "codex app-server",
			CodexReadTimeoutMS:         5000,
			CodexStallTimeoutMS:        30000,
			SessionPersistence: model.SessionPersistenceConfig{
				Kind: model.SessionPersistenceKindFile,
				File: model.SessionPersistenceFileConfig{
					Path: "automation/local/runtime-ledger.json",
				},
			},
			Notifications: model.NotificationsConfig{
				Channels: []model.NotificationChannelConfig{
					{
						ID:   "ops",
						Kind: "webhook",
						Webhook: &model.WebhookNotificationConfig{
							URL: "https://hooks.example.com/a",
						},
					},
				},
			},
		}
}
