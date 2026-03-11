package config

import (
	"errors"
	"strings"
	"testing"

	"symphony-go/internal/model"
	"symphony-go/internal/secret"
)

func TestExtractRequiredEnvVarsStableOrder(t *testing.T) {
	def := &model.AutomationDefinition{
		Selection: model.AutomationSelection{EnabledSources: []string{"linear-main"}},
		Sources: map[string]*model.SourceDefinition{
			"linear-main": {
				Name: "linear-main",
				Raw: map[string]any{
					"api_key":      "$LINEAR_API_KEY",
					"branch_scope": "$LINEAR_BRANCH_SCOPE",
					"labels":       []any{"$LINEAR_LABEL", map[string]any{"token": "$LABEL_TOKEN"}},
					"nested":       map[string]any{"project_slug": "$LINEAR_PROJECT_SLUG"},
				},
			},
		},
	}
	cfg := &model.ServiceConfig{
		HookBeforeRun: stringPointer(`repo_url="${SYMPHONY_GIT_REPO_URL:?required}"`),
	}

	got := ExtractRequiredEnvVars(def, cfg)
	want := []string{
		"LINEAR_API_KEY",
		"LINEAR_BRANCH_SCOPE",
		"LINEAR_LABEL",
		"LABEL_TOKEN",
		"LINEAR_PROJECT_SLUG",
		"SYMPHONY_GIT_REPO_URL",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("ExtractRequiredEnvVars() = %v, want %v", got, want)
	}
}

func TestDiagnoseConfigSeparatesMissingSecretsAndOtherErrors(t *testing.T) {
	originalResolver := secret.DefaultResolver
	secret.DefaultResolver = func(key string) (string, bool) {
		return "", false
	}
	t.Cleanup(func() { secret.DefaultResolver = originalResolver })

	def := &model.AutomationDefinition{
		Selection: model.AutomationSelection{EnabledSources: []string{"linear-main"}},
		Sources: map[string]*model.SourceDefinition{
			"linear-main": {
				Name: "linear-main",
				Raw: map[string]any{
					"api_key":      "$LINEAR_API_KEY",
					"project_slug": "$LINEAR_PROJECT_SLUG",
					"branch_scope": "$LINEAR_BRANCH_SCOPE",
				},
			},
		},
	}
	cfg := defaultServiceConfig()
	cfg.TrackerKind = "linear"
	cfg.TrackerAPIKey = ""
	cfg.TrackerProjectSlug = ""
	cfg.WorkspaceLinearBranchScope = ""
	cfg.CodexCommand = ""

	diagnosis := DiagnoseConfig(cfg, def)
	if !diagnosis.HasMissingSecrets() {
		t.Fatal("HasMissingSecrets() = false, want true")
	}
	if len(diagnosis.MissingSecrets) != 3 {
		t.Fatalf("MissingSecrets size = %d, want 3", len(diagnosis.MissingSecrets))
	}
	if len(diagnosis.OtherErrors) != 1 {
		t.Fatalf("OtherErrors size = %d, want 1", len(diagnosis.OtherErrors))
	}
	if !errors.Is(diagnosis.OtherErrors[0], model.ErrInvalidCodexCommand) {
		t.Fatalf("OtherErrors[0] = %v, want ErrInvalidCodexCommand", diagnosis.OtherErrors[0])
	}
}

func TestDiagnoseConfigDetectsHookSecrets(t *testing.T) {
	originalResolver := secret.DefaultResolver
	secret.DefaultResolver = func(key string) (string, bool) {
		if key == "LINEAR_API_KEY" || key == "LINEAR_PROJECT_SLUG" || key == "LINEAR_BRANCH_SCOPE" {
			return "present", true
		}
		return "", false
	}
	t.Cleanup(func() { secret.DefaultResolver = originalResolver })

	def := &model.AutomationDefinition{
		Selection: model.AutomationSelection{EnabledSources: []string{"linear-main"}},
		Sources: map[string]*model.SourceDefinition{
			"linear-main": {
				Name: "linear-main",
				Raw: map[string]any{
					"api_key":      "$LINEAR_API_KEY",
					"project_slug": "$LINEAR_PROJECT_SLUG",
					"branch_scope": "$LINEAR_BRANCH_SCOPE",
				},
			},
		},
	}
	cfg := defaultServiceConfig()
	cfg.TrackerKind = "linear"
	cfg.TrackerAPIKey = "key"
	cfg.TrackerProjectSlug = "demo"
	cfg.WorkspaceLinearBranchScope = "demo-scope"
	cfg.HookBeforeRun = stringPointer(`repo_url="${SYMPHONY_GIT_REPO_URL:?required}"`)

	diagnosis := DiagnoseConfig(cfg, def)
	if len(diagnosis.MissingSecrets) != 1 {
		t.Fatalf("MissingSecrets size = %d, want 1", len(diagnosis.MissingSecrets))
	}
	if diagnosis.MissingSecrets[0].EnvVar != "SYMPHONY_GIT_REPO_URL" {
		t.Fatalf("MissingSecrets[0].EnvVar = %q, want SYMPHONY_GIT_REPO_URL", diagnosis.MissingSecrets[0].EnvVar)
	}
	if diagnosis.MissingSecrets[0].Source != "hooks.before_run" {
		t.Fatalf("MissingSecrets[0].Source = %q, want hooks.before_run", diagnosis.MissingSecrets[0].Source)
	}
	if len(diagnosis.OtherErrors) != 0 {
		t.Fatalf("OtherErrors = %v, want none", diagnosis.OtherErrors)
	}
}
