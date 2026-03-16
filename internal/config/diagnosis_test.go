package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"symphony-go/internal/model"
	"symphony-go/internal/secret"
)

func TestExtractRequiredEnvVarsStableOrderOnFormalSections(t *testing.T) {
	def := &model.AutomationDefinition{
		Runtime: map[string]any{
			"execution": map[string]any{
				"backend": map[string]any{
					"codex": map[string]any{
						"command": "$CODEX_COMMAND",
					},
				},
			},
			"domain": map[string]any{
				"workspace": map[string]any{
					"git": map[string]any{
						"author_email": "$BOT_EMAIL",
					},
				},
			},
		},
		Selection: model.AutomationSelection{EnabledSources: []string{"linear-main"}},
		Sources: map[string]*model.SourceDefinition{
			"linear-main": {
				Name: "linear-main",
				Raw: map[string]any{
					"credentials": map[string]any{
						"api_key_ref": map[string]any{
							"kind": "env",
							"name": "LINEAR_API_KEY",
						},
					},
					"branch_scope": "$LINEAR_BRANCH_SCOPE",
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
		"BOT_EMAIL",
		"CODEX_COMMAND",
		"LINEAR_BRANCH_SCOPE",
		"LINEAR_PROJECT_SLUG",
		"SYMPHONY_GIT_REPO_URL",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("ExtractRequiredEnvVars() = %v, want %v", got, want)
	}
}

func TestDiagnoseConfigReportsMissingEnvAndSecretRefs(t *testing.T) {
	t.Setenv("LINEAR_PROJECT_SLUG", "demo")
	t.Setenv("LINEAR_BRANCH_SCOPE", "demo-scope")
	originalResolver := secret.DefaultResolver
	secret.DefaultResolver = func(key string) (string, bool) {
		switch key {
		case "LINEAR_PROJECT_SLUG":
			return "demo", true
		case "LINEAR_BRANCH_SCOPE":
			return "demo-scope", true
		default:
			return "", false
		}
	}
	t.Cleanup(func() { secret.DefaultResolver = originalResolver })

	workflow := &model.WorkflowDefinition{Config: validWorkflowConfigMap()}
	def := &model.AutomationDefinition{
		Runtime: map[string]any{
			"execution": map[string]any{
				"backend": map[string]any{
					"codex": map[string]any{
						"command": "$CODEX_COMMAND",
					},
				},
			},
		},
		Selection: model.AutomationSelection{EnabledSources: []string{"linear-main"}},
		Sources: map[string]*model.SourceDefinition{
			"linear-main": {
				Name: "linear-main",
				Raw: map[string]any{
					"credentials": map[string]any{
						"api_key_ref": map[string]any{
							"kind": "env",
							"name": "LINEAR_API_KEY",
						},
					},
					"project_slug": "$LINEAR_PROJECT_SLUG",
					"branch_scope": "$LINEAR_BRANCH_SCOPE",
				},
			},
		},
	}
	cfg := defaultServiceConfig()
	cfg.TrackerKind = "linear"
	cfg.TrackerAPIKey = ""
	cfg.TrackerProjectSlug = "demo"
	cfg.WorkspaceLinearBranchScope = "demo-scope"
	cfg.CodexCommand = ""

	diagnosis := DiagnoseConfig(cfg, workflow, def)
	if !diagnosis.HasMissingSecrets() {
		t.Fatal("HasMissingSecrets() = false, want true")
	}
	if len(diagnosis.MissingSecrets) != 2 {
		t.Fatalf("len(MissingSecrets) = %d, want 2", len(diagnosis.MissingSecrets))
	}
	if len(diagnosis.OtherErrors) != 0 {
		t.Fatalf("OtherErrors = %v, want none", diagnosis.OtherErrors)
	}
}

func TestDiagnoseConfigDetectsMissingHookEnvOnFormalConfig(t *testing.T) {
	originalResolver := secret.DefaultResolver
	secret.DefaultResolver = func(key string) (string, bool) {
		if key == "LINEAR_API_KEY" {
			return "present", true
		}
		return "", false
	}
	t.Cleanup(func() { secret.DefaultResolver = originalResolver })

	workflow := &model.WorkflowDefinition{Config: validWorkflowConfigMap()}
	def := &model.AutomationDefinition{
		Selection: model.AutomationSelection{EnabledSources: []string{"linear-main"}},
		Sources: map[string]*model.SourceDefinition{
			"linear-main": {
				Name: "linear-main",
				Raw: map[string]any{
					"credentials": map[string]any{
						"api_key_ref": map[string]any{
							"kind": "env",
							"name": "LINEAR_API_KEY",
						},
					},
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

	diagnosis := DiagnoseConfig(cfg, workflow, def)
	if len(diagnosis.MissingSecrets) != 1 {
		t.Fatalf("len(MissingSecrets) = %d, want 1", len(diagnosis.MissingSecrets))
	}
	if diagnosis.MissingSecrets[0].EnvVar != "SYMPHONY_GIT_REPO_URL" {
		t.Fatalf("MissingSecrets[0].EnvVar = %q, want SYMPHONY_GIT_REPO_URL", diagnosis.MissingSecrets[0].EnvVar)
	}
	if diagnosis.MissingSecrets[0].Source != "hooks.before_run" {
		t.Fatalf("MissingSecrets[0].Source = %q, want hooks.before_run", diagnosis.MissingSecrets[0].Source)
	}
}

func TestDiagnoseConfigDetectsMissingEnvFromPythonHookFile(t *testing.T) {
	originalResolver := secret.DefaultResolver
	secret.DefaultResolver = func(key string) (string, bool) {
		if key == "LINEAR_API_KEY" {
			return "present", true
		}
		return "", false
	}
	t.Cleanup(func() { secret.DefaultResolver = originalResolver })

	hookPath := filepath.Join(t.TempDir(), "before_run.py")
	if err := os.WriteFile(hookPath, []byte("import os\nrepo_url = os.environ[\"SYMPHONY_GIT_REPO_URL\"]\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	workflow := &model.WorkflowDefinition{Config: validWorkflowConfigMap()}
	def := &model.AutomationDefinition{
		Selection: model.AutomationSelection{EnabledSources: []string{"linear-main"}},
		Sources: map[string]*model.SourceDefinition{
			"linear-main": {
				Name: "linear-main",
				Raw: map[string]any{
					"credentials": map[string]any{
						"api_key_ref": map[string]any{
							"kind": "env",
							"name": "LINEAR_API_KEY",
						},
					},
				},
			},
		},
	}
	cfg := defaultServiceConfig()
	cfg.TrackerKind = "linear"
	cfg.TrackerAPIKey = "key"
	cfg.TrackerProjectSlug = "demo"
	cfg.WorkspaceLinearBranchScope = "demo-scope"
	cfg.HookBeforeRun = stringPointer(hookPath)

	diagnosis := DiagnoseConfig(cfg, workflow, def)
	if len(diagnosis.MissingSecrets) != 1 {
		t.Fatalf("len(MissingSecrets) = %d, want 1", len(diagnosis.MissingSecrets))
	}
	if diagnosis.MissingSecrets[0].EnvVar != "SYMPHONY_GIT_REPO_URL" {
		t.Fatalf("MissingSecrets[0].EnvVar = %q, want SYMPHONY_GIT_REPO_URL", diagnosis.MissingSecrets[0].EnvVar)
	}
	if diagnosis.MissingSecrets[0].Source != "hooks.before_run" {
		t.Fatalf("MissingSecrets[0].Source = %q, want hooks.before_run", diagnosis.MissingSecrets[0].Source)
	}
}
