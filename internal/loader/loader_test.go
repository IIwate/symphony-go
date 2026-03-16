package loader

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"symphony-go/internal/model"
	"symphony-go/internal/secret"
)

func TestLoadProjectOnly(t *testing.T) {
	root := writeLoaderFixture(t, loaderFixtureOptions{})

	def, err := Load(root, "")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := getMapValue(getMapValue(getMapValue(def.Runtime, "execution"), "backend"), "codex")["command"]; got != "codex app-server" {
		t.Fatalf("execution.backend.codex.command = %v, want codex app-server", got)
	}
	if got := def.Selection.DispatchFlow; got != "implement" {
		t.Fatalf("dispatch flow = %q, want implement", got)
	}
	if len(def.Selection.EnabledSources) != 1 || def.Selection.EnabledSources[0] != "linear-main" {
		t.Fatalf("enabled sources = %v, want [linear-main]", def.Selection.EnabledSources)
	}
}

func TestLoadWithProfile(t *testing.T) {
	root := writeLoaderFixture(t, loaderFixtureOptions{
		ProfileName: "dev",
		ProfileYAML: "domain:\n  polling:\n    interval_ms: 10000\n",
	})

	def, err := Load(root, "dev")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := getMapValue(getMapValue(def.Runtime, "domain"), "polling")["interval_ms"]; got != 10000 {
		t.Fatalf("domain.polling.interval_ms = %v, want 10000", got)
	}
	if def.Profile != "dev" {
		t.Fatalf("profile = %q, want dev", def.Profile)
	}
}

func TestLoadWithLocalOverrides(t *testing.T) {
	root := writeLoaderFixture(t, loaderFixtureOptions{
		LocalOverridesYAML: "execution:\n  agent:\n    max_concurrent_agents: 2\n",
	})

	def, err := Load(root, "")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := getMapValue(getMapValue(def.Runtime, "execution"), "agent")["max_concurrent_agents"]; got != 2 {
		t.Fatalf("execution.agent.max_concurrent_agents = %v, want 2", got)
	}
}

func TestLoadSourcesRegistry(t *testing.T) {
	root := writeLoaderFixture(t, loaderFixtureOptions{
		ExtraSources: map[string]string{
			"github-core.yaml": "kind: github\ncredentials:\n  api_key_ref:\n    kind: env\n    name: GITHUB_TOKEN\nowner: org\nrepo: repo\n",
		},
	})

	def, err := Load(root, "")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(def.Sources) != 2 {
		t.Fatalf("sources size = %d, want 2", len(def.Sources))
	}
}

func TestLoadFlowsRegistry(t *testing.T) {
	root := writeLoaderFixture(t, loaderFixtureOptions{
		ExtraFlows: map[string]string{
			"review-pr.yaml": "prompt: prompts/review-pr.md.liquid\n",
		},
		ExtraPrompts: map[string]string{
			"review-pr.md.liquid": "review {{ issue.title }}\n",
		},
	})

	def, err := Load(root, "")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(def.Flows) != 2 {
		t.Fatalf("flows size = %d, want 2", len(def.Flows))
	}
}

func TestResolveActiveWorkflowSingleSource(t *testing.T) {
	root := writeLoaderFixture(t, loaderFixtureOptions{})

	def, err := Load(root, "")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	workflowDef, err := ResolveActiveWorkflow(def)
	if err != nil {
		t.Fatalf("ResolveActiveWorkflow() error = %v", err)
	}

	sourceConfig := getMapValue(workflowDef.Config, "source_adapter")
	if got := sourceConfig["kind"]; got != "linear" {
		t.Fatalf("source_adapter.kind = %v, want linear", got)
	}
	if got := workflowDef.Source["branch_scope"]; got != "demo-scope" {
		t.Fatalf("source.branch_scope = %v, want demo-scope", got)
	}
	if workflowDef.PromptTemplate != "hello {{ source.kind }} {{ issue.title }}" {
		t.Fatalf("PromptTemplate = %q", workflowDef.PromptTemplate)
	}
}

func TestResolveActiveWorkflowMultipleSourcesRejected(t *testing.T) {
	root := writeLoaderFixture(t, loaderFixtureOptions{
		ProjectYAML: `service:
  contract_version: v1
  instance_name: symphony
domain:
  workspace:
    root: ~/workspaces
sources:
  enabled:
    - linear-main
    - github-core
execution:
  backend:
    kind: codex
    codex:
      command: codex app-server
job_policy:
  dispatch_flow: implement
`,
		ExtraSources: map[string]string{
			"github-core.yaml": "kind: github\ncredentials:\n  api_key_ref:\n    kind: env\n    name: GITHUB_TOKEN\nowner: org\nrepo: repo\n",
		},
	})

	def, err := Load(root, "")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if _, err := ResolveActiveWorkflow(def); err == nil {
		t.Fatal("ResolveActiveWorkflow() error = nil, want multi-source rejection")
	}
}

func TestResolveActiveWorkflowMappingTable(t *testing.T) {
	root := writeLoaderFixture(t, loaderFixtureOptions{
		ProjectYAML: `service:
  contract_version: v1
  instance_name: symphony
  server:
    port: 8080
domain:
  polling:
    interval_ms: 15000
  workspace:
    root: ~/workspaces
sources:
  enabled:
    - linear-main
execution:
  backend:
    kind: codex
    codex:
      command: codex app-server
  hooks:
    timeout_ms: 12345
  agent:
    max_turns: 5
job_policy:
  dispatch_flow: implement
`,
		FlowYAML: `prompt: prompts/implement.md.liquid
hooks:
  before_run: hooks/before_run.sh
  before_run_continuation: hooks/before_run.sh
  after_run: null
completion:
  mode: pull_request
  on_missing_pr: intervention
  on_closed_pr: continue
`,
		Hooks: map[string]string{
			"before_run.sh": "echo before-run\n",
		},
	})

	def, err := Load(root, "")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	workflowDef, err := ResolveActiveWorkflow(def)
	if err != nil {
		t.Fatalf("ResolveActiveWorkflow() error = %v", err)
	}

	if got := getMapValue(getMapValue(workflowDef.Config, "domain"), "polling")["interval_ms"]; got != 15000 {
		t.Fatalf("domain.polling.interval_ms = %v, want 15000", got)
	}
	if got := getMapValue(workflowDef.Config, "source_adapter")["branch_scope"]; got != "demo-scope" {
		t.Fatalf("source_adapter.branch_scope = %v, want demo-scope", got)
	}
	if got := getMapValue(workflowDef.Config, "source_adapter")["project_slug"]; got != "demo" {
		t.Fatalf("source_adapter.project_slug = %v, want demo", got)
	}
	if got := getMapValue(getMapValue(workflowDef.Config, "execution"), "hooks")["before_run"]; got != "echo before-run" {
		t.Fatalf("execution.hooks.before_run = %v, want echo before-run", got)
	}
	if got := getMapValue(getMapValue(workflowDef.Config, "execution"), "hooks")["before_run_continuation"]; got != "echo before-run" {
		t.Fatalf("execution.hooks.before_run_continuation = %v, want echo before-run", got)
	}
	if got := getMapValue(getMapValue(workflowDef.Config, "execution"), "hooks")["timeout_ms"]; got != 12345 {
		t.Fatalf("execution.hooks.timeout_ms = %v, want 12345", got)
	}
	value, ok := getMapValue(getMapValue(workflowDef.Config, "execution"), "hooks")["after_run"]
	if !ok || value != nil {
		t.Fatalf("execution.hooks.after_run = %v, want explicit nil", value)
	}
	if workflowDef.Completion.Mode != model.CompletionModePullRequest {
		t.Fatalf("completion mode = %q, want pull_request", workflowDef.Completion.Mode)
	}
	if workflowDef.Completion.OnMissingPR != model.CompletionActionIntervention {
		t.Fatalf("completion on_missing_pr = %q, want intervention", workflowDef.Completion.OnMissingPR)
	}
	if workflowDef.Completion.OnClosedPR != model.CompletionActionContinue {
		t.Fatalf("completion on_closed_pr = %q, want continue", workflowDef.Completion.OnClosedPR)
	}
}

func TestResolveActiveWorkflowCompletionDefaultsToNone(t *testing.T) {
	root := writeLoaderFixture(t, loaderFixtureOptions{})

	def, err := Load(root, "")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	workflowDef, err := ResolveActiveWorkflow(def)
	if err != nil {
		t.Fatalf("ResolveActiveWorkflow() error = %v", err)
	}
	if workflowDef.Completion.Mode != model.CompletionModeNone {
		t.Fatalf("completion mode = %q, want none", workflowDef.Completion.Mode)
	}
	if workflowDef.Completion.OnMissingPR != model.CompletionActionContinue {
		t.Fatalf("completion on_missing_pr = %q, want continue", workflowDef.Completion.OnMissingPR)
	}
	if workflowDef.Completion.OnClosedPR != model.CompletionActionContinue {
		t.Fatalf("completion on_closed_pr = %q, want continue", workflowDef.Completion.OnClosedPR)
	}
}

func TestResolveActiveWorkflowRejectsInvalidCompletionValues(t *testing.T) {
	root := writeLoaderFixture(t, loaderFixtureOptions{
		FlowYAML: `prompt: prompts/implement.md.liquid
completion:
  mode: unsupported
`,
	})

	def, err := Load(root, "")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if _, err := ResolveActiveWorkflow(def); err == nil {
		t.Fatal("ResolveActiveWorkflow() error = nil, want invalid completion error")
	}
}

func TestResolveActiveWorkflowResolvesSourceEnvStrings(t *testing.T) {
	t.Setenv("LINEAR_PROJECT_SLUG", "demo-from-env")
	t.Setenv("LINEAR_BRANCH_SCOPE", "Demo Scope")

	root := writeLoaderFixture(t, loaderFixtureOptions{})
	writeLoaderFile(t, filepath.Join(root, "sources", "linear-main.yaml"), `kind: linear
credentials:
  api_key_ref:
    kind: env
    name: LINEAR_API_KEY
project_slug: $LINEAR_PROJECT_SLUG
branch_scope: $LINEAR_BRANCH_SCOPE
active_states: ["Todo", "In Progress"]
terminal_states: ["Closed", "Done"]
`)

	def, err := Load(root, "")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	workflowDef, err := ResolveActiveWorkflow(def)
	if err != nil {
		t.Fatalf("ResolveActiveWorkflow() error = %v", err)
	}

	if got := getMapValue(workflowDef.Config, "source_adapter")["project_slug"]; got != "demo-from-env" {
		t.Fatalf("source_adapter.project_slug = %v, want demo-from-env", got)
	}
	if got := getMapValue(workflowDef.Config, "source_adapter")["branch_scope"]; got != "Demo Scope" {
		t.Fatalf("source_adapter.branch_scope = %v, want Demo Scope", got)
	}
	if got := workflowDef.Source["project_slug"]; got != "demo-from-env" {
		t.Fatalf("source.project_slug = %v, want demo-from-env", got)
	}
	if got := workflowDef.Source["branch_scope"]; got != "Demo Scope" {
		t.Fatalf("source.branch_scope = %v, want Demo Scope", got)
	}
}

func TestResolveActiveWorkflowSourceBindingsExcludeSensitiveFields(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "secret-key")

	root := writeLoaderFixture(t, loaderFixtureOptions{})
	def, err := Load(root, "")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	workflowDef, err := ResolveActiveWorkflow(def)
	if err != nil {
		t.Fatalf("ResolveActiveWorkflow() error = %v", err)
	}
	if _, exists := workflowDef.Source["api_key"]; exists {
		t.Fatalf("workflowDef.Source = %+v, want api_key to stay hidden", workflowDef.Source)
	}
}

func TestResolveActiveWorkflowHookPathEscape(t *testing.T) {
	root := writeLoaderFixture(t, loaderFixtureOptions{
		FlowYAML: `prompt: prompts/implement.md.liquid
hooks:
  before_run: ../outside.sh
`,
	})

	def, err := Load(root, "")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if _, err := ResolveActiveWorkflow(def); err == nil {
		t.Fatal("ResolveActiveWorkflow() error = nil, want path escape error")
	}
}

func TestResolveActiveWorkflowHookPathEscapeViaSymlink(t *testing.T) {
	root := writeLoaderFixture(t, loaderFixtureOptions{
		FlowYAML: `prompt: prompts/implement.md.liquid
hooks:
  before_run: hooks/link.sh
`,
	})
	outsidePath := filepath.Join(t.TempDir(), "outside.sh")
	writeLoaderFile(t, outsidePath, "echo outside\n")
	linkPath := filepath.Join(root, "hooks", "link.sh")
	if err := os.Remove(linkPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Remove(%q) error = %v", linkPath, err)
	}
	if err := os.Symlink(outsidePath, linkPath); err != nil {
		t.Skipf("Symlink() unavailable: %v", err)
	}

	def, err := Load(root, "")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if _, err := ResolveActiveWorkflow(def); err == nil {
		t.Fatal("ResolveActiveWorkflow() error = nil, want symlink path escape error")
	}
}

func TestResolveActiveWorkflowTreatsSingleLineHookWithSlashAsInlineScript(t *testing.T) {
	script := "git remote set-url origin https://example.test/repo.git"
	root := writeLoaderFixture(t, loaderFixtureOptions{
		FlowYAML: "prompt: prompts/implement.md.liquid\nhooks:\n  before_run: " + script + "\n",
	})

	def, err := Load(root, "")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	workflowDef, err := ResolveActiveWorkflow(def)
	if err != nil {
		t.Fatalf("ResolveActiveWorkflow() error = %v", err)
	}
	if got := getMapValue(getMapValue(workflowDef.Config, "execution"), "hooks")["before_run"]; got != script {
		t.Fatalf("execution.hooks.before_run = %v, want inline script", got)
	}
}

func TestResolveActiveWorkflowRejectsHookSymlinkEscape(t *testing.T) {
	root := writeLoaderFixture(t, loaderFixtureOptions{
		FlowYAML: `prompt: prompts/implement.md.liquid
hooks:
  before_run: hooks/before_run.sh
`,
	})
	outsidePath := filepath.Join(t.TempDir(), "outside.sh")
	writeLoaderFile(t, outsidePath, "echo outside\n")
	linkPath := filepath.Join(root, "hooks", "before_run.sh")
	if err := os.Symlink(outsidePath, linkPath); err != nil {
		t.Skipf("Symlink() unsupported on this host: %v", err)
	}

	def, err := Load(root, "")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if _, err := ResolveActiveWorkflow(def); err == nil {
		t.Fatal("ResolveActiveWorkflow() error = nil, want symlink escape error")
	}
}

func TestResolveActiveWorkflowTreatsInlineHookWithSlashAsScript(t *testing.T) {
	root := writeLoaderFixture(t, loaderFixtureOptions{
		FlowYAML: "prompt: prompts/implement.md.liquid\nhooks:\n  before_run: \"git remote set-url origin https://example.test/repo.git\"\n",
	})

	def, err := Load(root, "")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	workflowDef, err := ResolveActiveWorkflow(def)
	if err != nil {
		t.Fatalf("ResolveActiveWorkflow() error = %v", err)
	}
	if got := getMapValue(getMapValue(workflowDef.Config, "execution"), "hooks")["before_run"]; got != "git remote set-url origin https://example.test/repo.git" {
		t.Fatalf("execution.hooks.before_run = %v, want inline hook script", got)
	}
}

func TestLoadMissingProjectYaml(t *testing.T) {
	root := t.TempDir()
	if _, err := Load(root, ""); !errors.Is(err, model.ErrMissingWorkflowFile) {
		t.Fatalf("Load() error = %v, want ErrMissingWorkflowFile", err)
	}
}

func TestResolveActiveWorkflowUsesDefaultResolver(t *testing.T) {
	originalResolver := secret.DefaultResolver
	secret.DefaultResolver = func(key string) (string, bool) {
		switch key {
		case "LINEAR_PROJECT_SLUG":
			return "resolver-project", true
		case "LINEAR_BRANCH_SCOPE":
			return "Resolver Scope", true
		default:
			return "", false
		}
	}
	t.Cleanup(func() { secret.DefaultResolver = originalResolver })

	root := writeLoaderFixture(t, loaderFixtureOptions{})
	writeLoaderFile(t, filepath.Join(root, "sources", "linear-main.yaml"), `kind: linear
credentials:
  api_key_ref:
    kind: env
    name: LINEAR_API_KEY
project_slug: $LINEAR_PROJECT_SLUG
branch_scope: $LINEAR_BRANCH_SCOPE
active_states: ["Todo", "In Progress"]
terminal_states: ["Closed", "Done"]
`)

	def, err := Load(root, "")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	workflowDef, err := ResolveActiveWorkflow(def)
	if err != nil {
		t.Fatalf("ResolveActiveWorkflow() error = %v", err)
	}
	if got := getMapValue(workflowDef.Config, "source_adapter")["project_slug"]; got != "resolver-project" {
		t.Fatalf("source_adapter.project_slug = %v, want resolver-project", got)
	}
	if got := getMapValue(workflowDef.Config, "source_adapter")["branch_scope"]; got != "Resolver Scope" {
		t.Fatalf("source_adapter.branch_scope = %v, want Resolver Scope", got)
	}
}

func TestWatchReloadsOnPromptChange(t *testing.T) {
	root := writeLoaderFixture(t, loaderFixtureOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	updates := make(chan *model.AutomationDefinition, 1)
	if err := Watch(ctx, root, "", func(def *model.AutomationDefinition) {
		updates <- def
	}); err != nil {
		t.Fatalf("Watch() error = %v", err)
	}

	time.Sleep(200 * time.Millisecond)
	writeLoaderFile(t, filepath.Join(root, "prompts", "implement.md.liquid"), "updated {{ issue.title }}\n")

	select {
	case updated := <-updates:
		workflowDef, err := ResolveActiveWorkflow(updated)
		if err != nil {
			t.Fatalf("ResolveActiveWorkflow() error = %v", err)
		}
		if workflowDef.PromptTemplate != "updated {{ issue.title }}" {
			t.Fatalf("PromptTemplate = %q, want updated template", workflowDef.PromptTemplate)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watch callback not triggered")
	}
}

func TestWatchReloadsOnNewPromptFileChanges(t *testing.T) {
	root := writeLoaderFixture(t, loaderFixtureOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	updates := make(chan *model.AutomationDefinition, 2)
	if err := Watch(ctx, root, "", func(def *model.AutomationDefinition) {
		updates <- def
	}); err != nil {
		t.Fatalf("Watch() error = %v", err)
	}

	time.Sleep(200 * time.Millisecond)
	promptPath := filepath.Join(root, "prompts", "review-pr.md.liquid")
	writeLoaderFile(t, promptPath, "review {{ issue.title }}\n")
	awaitWatchUpdate(t, updates)

	writeLoaderFile(t, promptPath, "review updated {{ issue.title }}\n")
	awaitWatchUpdate(t, updates)
}

func TestWatchReloadsWhenPoliciesDirectoryCreatedLater(t *testing.T) {
	root := writeLoaderFixture(t, loaderFixtureOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	updates := make(chan *model.AutomationDefinition, 1)
	if err := Watch(ctx, root, "", func(def *model.AutomationDefinition) {
		updates <- def
	}); err != nil {
		t.Fatalf("Watch() error = %v", err)
	}

	time.Sleep(200 * time.Millisecond)
	policyDir := filepath.Join(root, "policies")
	if err := os.MkdirAll(policyDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	writeLoaderFile(t, filepath.Join(policyDir, "pr-gate.yaml"), "mode: strict\n")

	awaitWatchUpdate(t, updates)
}

func TestWatchSkipsInvalidReload(t *testing.T) {
	root := writeLoaderFixture(t, loaderFixtureOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	updates := make(chan *model.AutomationDefinition, 1)
	if err := Watch(ctx, root, "", func(def *model.AutomationDefinition) {
		updates <- def
	}); err != nil {
		t.Fatalf("Watch() error = %v", err)
	}

	time.Sleep(200 * time.Millisecond)
	writeLoaderFile(t, filepath.Join(root, "project.yaml"), "runtime: [\n")

	select {
	case definition := <-updates:
		t.Fatalf("unexpected update received: %+v", definition)
	case <-time.After(750 * time.Millisecond):
	}
}

func TestWatchIgnoresEnvLocalChange(t *testing.T) {
	root := writeLoaderFixture(t, loaderFixtureOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	updates := make(chan *model.AutomationDefinition, 1)
	if err := Watch(ctx, root, "", func(def *model.AutomationDefinition) {
		updates <- def
	}); err != nil {
		t.Fatalf("Watch() error = %v", err)
	}

	time.Sleep(200 * time.Millisecond)
	writeLoaderFile(t, filepath.Join(root, "local", "env.local"), "LINEAR_API_KEY=changed\n")

	select {
	case definition := <-updates:
		t.Fatalf("unexpected update received for env.local change: %+v", definition)
	case <-time.After(750 * time.Millisecond):
	}
}

func TestWatchIgnoresRuntimeLedgerChange(t *testing.T) {
	root := writeLoaderFixture(t, loaderFixtureOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	updates := make(chan *model.AutomationDefinition, 1)
	if err := Watch(ctx, root, "", func(def *model.AutomationDefinition) {
		updates <- def
	}); err != nil {
		t.Fatalf("Watch() error = %v", err)
	}

	time.Sleep(200 * time.Millisecond)
	writeLoaderFile(t, filepath.Join(root, "local", "runtime-state.json"), "{\"version\":1}\n")

	select {
	case definition := <-updates:
		t.Fatalf("unexpected update received for runtime-state.json change: %+v", definition)
	case <-time.After(750 * time.Millisecond):
	}
}

func TestWatchIgnoresLocalRuntimeScratchFileChange(t *testing.T) {
	root := writeLoaderFixture(t, loaderFixtureOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	updates := make(chan *model.AutomationDefinition, 1)
	if err := Watch(ctx, root, "", func(def *model.AutomationDefinition) {
		updates <- def
	}); err != nil {
		t.Fatalf("Watch() error = %v", err)
	}

	time.Sleep(200 * time.Millisecond)
	writeLoaderFile(t, filepath.Join(root, "local", "worker-runtime.json"), "{\"running\":true}\n")

	select {
	case definition := <-updates:
		t.Fatalf("unexpected update received for local runtime scratch file: %+v", definition)
	case <-time.After(750 * time.Millisecond):
	}
}

func TestWatchReloadsActiveDefaultProfileFile(t *testing.T) {
	root := writeLoaderFixture(t, loaderFixtureOptions{
		ProfileName: "dev",
		ProfileYAML: "domain:\n  polling:\n    interval_ms: 10000\n",
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	updates := make(chan *model.AutomationDefinition, 2)
	if err := Watch(ctx, root, "", func(def *model.AutomationDefinition) {
		updates <- def
	}); err != nil {
		t.Fatalf("Watch() error = %v", err)
	}

	time.Sleep(200 * time.Millisecond)
	writeLoaderFile(t, filepath.Join(root, "project.yaml"), `service:
  contract_version: v1
  instance_name: symphony
domain:
  workspace:
    root: ~/workspaces
sources:
  enabled:
    - linear-main
execution:
  backend:
    kind: codex
    codex:
      command: codex app-server
job_policy:
  dispatch_flow: implement
defaults:
  profile: dev
`)

	updated := awaitWatchUpdate(t, updates)
	if updated.Profile != "dev" {
		t.Fatalf("Profile = %q, want dev", updated.Profile)
	}

	writeLoaderFile(t, filepath.Join(root, "profiles", "dev.yaml"), "domain:\n  polling:\n    interval_ms: 15000\n")
	updated = awaitWatchUpdate(t, updates)
	if updated.Profile != "dev" {
		t.Fatalf("Profile = %q after profile update, want dev", updated.Profile)
	}
	if got := getMapValue(getMapValue(updated.Runtime, "domain"), "polling")["interval_ms"]; got != 15000 {
		t.Fatalf("domain.polling.interval_ms = %v, want 15000", got)
	}
}

func TestWatchTracksActiveProfileAfterDefaultProfileSwitch(t *testing.T) {
	root := writeLoaderFixture(t, loaderFixtureOptions{
		ProjectYAML: `service:
  contract_version: v1
  instance_name: symphony
domain:
  workspace:
    root: ~/workspaces
sources:
  enabled:
    - linear-main
execution:
  backend:
    kind: codex
    codex:
      command: codex app-server
job_policy:
  dispatch_flow: implement
defaults:
  profile: dev
`,
		ProfileName: "dev",
		ProfileYAML: "execution:\n  agent:\n    max_turns: 3\n",
	})
	writeLoaderFile(t, filepath.Join(root, "profiles", "prod.yaml"), "execution:\n  agent:\n    max_turns: 7\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	updates := make(chan *model.AutomationDefinition, 2)
	if err := Watch(ctx, root, "", func(def *model.AutomationDefinition) {
		updates <- def
	}); err != nil {
		t.Fatalf("Watch() error = %v", err)
	}

	time.Sleep(200 * time.Millisecond)
	writeLoaderFile(t, filepath.Join(root, "project.yaml"), `service:
  contract_version: v1
  instance_name: symphony
domain:
  workspace:
    root: ~/workspaces
sources:
  enabled:
    - linear-main
execution:
  backend:
    kind: codex
    codex:
      command: codex app-server
job_policy:
  dispatch_flow: implement
defaults:
  profile: prod
`)
	updated := awaitWatchUpdate(t, updates)
	if updated.Profile != "prod" {
		t.Fatalf("updated profile = %q, want prod", updated.Profile)
	}

	writeLoaderFile(t, filepath.Join(root, "profiles", "prod.yaml"), "execution:\n  agent:\n    max_turns: 9\n")
	updated = awaitWatchUpdate(t, updates)
	if updated.Profile != "prod" {
		t.Fatalf("updated profile = %q, want prod", updated.Profile)
	}
	if got := getMapValue(getMapValue(updated.Runtime, "execution"), "agent")["max_turns"]; got != 9 {
		t.Fatalf("execution.agent.max_turns = %v, want 9", got)
	}
}

func TestWatchKeepsAcceptedProfileWhenProfileSwitchRejected(t *testing.T) {
	root := writeLoaderFixture(t, loaderFixtureOptions{
		ProjectYAML: `service:
  contract_version: v1
  instance_name: symphony
domain:
  workspace:
    root: ~/workspaces
sources:
  enabled:
    - linear-main
execution:
  backend:
    kind: codex
    codex:
      command: codex app-server
job_policy:
  dispatch_flow: implement
defaults:
  profile: dev
`,
		ProfileName: "dev",
		ProfileYAML: "domain:\n  polling:\n    interval_ms: 10000\n",
	})
	writeLoaderFile(t, filepath.Join(root, "profiles", "prod.yaml"), "domain:\n  polling:\n    interval_ms: 20000\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	updates := make(chan *model.AutomationDefinition, 4)
	watchErrs := make(chan error, 4)
	if err := WatchWithErrors(ctx, root, "", func(def *model.AutomationDefinition) error {
		if def.Profile == "prod" {
			return errors.New("profile selection changed: restart required")
		}
		updates <- def
		return nil
	}, func(err error) {
		watchErrs <- err
	}); err != nil {
		t.Fatalf("WatchWithErrors() error = %v", err)
	}

	time.Sleep(200 * time.Millisecond)
	writeLoaderFile(t, filepath.Join(root, "project.yaml"), `service:
  contract_version: v1
  instance_name: symphony
domain:
  workspace:
    root: ~/workspaces
sources:
  enabled:
    - linear-main
execution:
  backend:
    kind: codex
    codex:
      command: codex app-server
job_policy:
  dispatch_flow: implement
defaults:
  profile: prod
`)
	if err := awaitWatchError(t, watchErrs); err == nil {
		t.Fatal("expected rejected profile-switch error")
	}
	time.Sleep(300 * time.Millisecond)
	drainWatchUpdates(updates)

	writeLoaderFile(t, filepath.Join(root, "profiles", "dev.yaml"), "domain:\n  polling:\n    interval_ms: 15000\n")
	updated := awaitWatchUpdate(t, updates)
	if updated.Profile != "dev" {
		t.Fatalf("Profile = %q, want dev", updated.Profile)
	}
	if got := getMapValue(getMapValue(updated.Runtime, "domain"), "polling")["interval_ms"]; got != 15000 {
		t.Fatalf("domain.polling.interval_ms = %v, want 15000", got)
	}
}

func TestWatchKeepsAcceptedProfileWhenSelectedProfileIsInvalid(t *testing.T) {
	root := writeLoaderFixture(t, loaderFixtureOptions{
		ProjectYAML: `service:
  contract_version: v1
  instance_name: symphony
domain:
  workspace:
    root: ~/workspaces
sources:
  enabled:
    - linear-main
execution:
  backend:
    kind: codex
    codex:
      command: codex app-server
job_policy:
  dispatch_flow: implement
defaults:
  profile: dev
`,
		ProfileName: "dev",
		ProfileYAML: "domain:\n  polling:\n    interval_ms: 10000\n",
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	updates := make(chan *model.AutomationDefinition, 4)
	watchErrs := make(chan error, 4)
	if err := WatchWithErrors(ctx, root, "", func(def *model.AutomationDefinition) error {
		updates <- def
		return nil
	}, func(err error) {
		watchErrs <- err
	}); err != nil {
		t.Fatalf("WatchWithErrors() error = %v", err)
	}

	time.Sleep(200 * time.Millisecond)
	writeLoaderFile(t, filepath.Join(root, "project.yaml"), `service:
  contract_version: v1
  instance_name: symphony
domain:
  workspace:
    root: ~/workspaces
sources:
  enabled:
    - linear-main
execution:
  backend:
    kind: codex
    codex:
      command: codex app-server
job_policy:
  dispatch_flow: implement
defaults:
  profile: prod
`)
	if err := awaitWatchError(t, watchErrs); err == nil {
		t.Fatal("expected invalid selected-profile error")
	}
	time.Sleep(300 * time.Millisecond)
	drainWatchUpdates(updates)

	writeLoaderFile(t, filepath.Join(root, "profiles", "dev.yaml"), "domain:\n  polling:\n    interval_ms: 16000\n")
	updated := awaitWatchUpdate(t, updates)
	if updated.Profile != "dev" {
		t.Fatalf("Profile = %q, want dev", updated.Profile)
	}
	if got := getMapValue(getMapValue(updated.Runtime, "domain"), "polling")["interval_ms"]; got != 16000 {
		t.Fatalf("domain.polling.interval_ms = %v, want 16000", got)
	}
}

func awaitWatchUpdate(t *testing.T, updates <-chan *model.AutomationDefinition) *model.AutomationDefinition {
	t.Helper()

	select {
	case updated := <-updates:
		return updated
	case <-time.After(5 * time.Second):
		t.Fatal("watch callback not triggered")
		return nil
	}
}

func awaitWatchError(t *testing.T, watchErrs <-chan error) error {
	t.Helper()

	select {
	case err := <-watchErrs:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("watch error callback not triggered")
		return nil
	}
}

func drainWatchUpdates(updates <-chan *model.AutomationDefinition) {
	for {
		select {
		case <-updates:
		default:
			return
		}
	}
}

type loaderFixtureOptions struct {
	ProjectYAML        string
	FlowYAML           string
	PromptTemplate     string
	ProfileName        string
	ProfileYAML        string
	LocalOverridesYAML string
	ExtraSources       map[string]string
	ExtraFlows         map[string]string
	ExtraPrompts       map[string]string
	Hooks              map[string]string
}

func writeLoaderFixture(t *testing.T, opts loaderFixtureOptions) string {
	t.Helper()

	root := filepath.Join(t.TempDir(), "automation")
	for _, dir := range []string{
		root,
		filepath.Join(root, "sources"),
		filepath.Join(root, "flows"),
		filepath.Join(root, "prompts"),
		filepath.Join(root, "hooks"),
		filepath.Join(root, "local"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%q) error = %v", dir, err)
		}
	}
	if opts.ProfileName != "" {
		if err := os.MkdirAll(filepath.Join(root, "profiles"), 0o755); err != nil {
			t.Fatalf("MkdirAll(profiles) error = %v", err)
		}
	}

	projectYAML := opts.ProjectYAML
	if projectYAML == "" {
		projectYAML = `service:
  contract_version: v1
  instance_name: symphony
domain:
  id: default
  workspace:
    root: ~/workspaces
sources:
  enabled:
    - linear-main
execution:
  backend:
    kind: codex
    codex:
      command: codex app-server
job_policy:
  dispatch_flow: implement
defaults:
  profile: null
`
	}
	flowYAML := opts.FlowYAML
	if flowYAML == "" {
		flowYAML = "prompt: prompts/implement.md.liquid\n"
	}
	promptTemplate := opts.PromptTemplate
	if promptTemplate == "" {
		promptTemplate = "hello {{ source.kind }} {{ issue.title }}\n"
	}

	writeLoaderFile(t, filepath.Join(root, "project.yaml"), projectYAML)
	writeLoaderFile(t, filepath.Join(root, "sources", "linear-main.yaml"), `kind: linear
credentials:
  api_key_ref:
    kind: env
    name: LINEAR_API_KEY
project_slug: demo
branch_scope: demo-scope
active_states: ["Todo", "In Progress"]
terminal_states: ["Closed", "Done"]
`)
	writeLoaderFile(t, filepath.Join(root, "flows", "implement.yaml"), flowYAML)
	writeLoaderFile(t, filepath.Join(root, "prompts", "implement.md.liquid"), promptTemplate)

	if opts.ProfileName != "" {
		writeLoaderFile(t, filepath.Join(root, "profiles", opts.ProfileName+".yaml"), opts.ProfileYAML)
	}
	if opts.LocalOverridesYAML != "" {
		writeLoaderFile(t, filepath.Join(root, "local", "overrides.yaml"), opts.LocalOverridesYAML)
	}
	for name, content := range opts.ExtraSources {
		writeLoaderFile(t, filepath.Join(root, "sources", name), content)
	}
	for name, content := range opts.ExtraFlows {
		writeLoaderFile(t, filepath.Join(root, "flows", name), content)
	}
	for name, content := range opts.ExtraPrompts {
		writeLoaderFile(t, filepath.Join(root, "prompts", name), content)
	}
	for name, content := range opts.Hooks {
		writeLoaderFile(t, filepath.Join(root, "hooks", name), content)
	}

	return root
}

func writeLoaderFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
}
