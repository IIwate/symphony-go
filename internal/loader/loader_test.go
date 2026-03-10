package loader

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"symphony-go/internal/model"
)

func TestLoadProjectOnly(t *testing.T) {
	root := writeLoaderFixture(t, loaderFixtureOptions{})

	def, err := Load(root, "")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := getMapValue(def.Runtime, "codex")["command"]; got != "codex app-server" {
		t.Fatalf("runtime.codex.command = %v, want codex app-server", got)
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
		ProfileYAML: "runtime:\n  polling:\n    interval_ms: 10000\n",
	})

	def, err := Load(root, "dev")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := getMapValue(def.Runtime, "polling")["interval_ms"]; got != 10000 {
		t.Fatalf("runtime.polling.interval_ms = %v, want 10000", got)
	}
	if def.Profile != "dev" {
		t.Fatalf("profile = %q, want dev", def.Profile)
	}
}

func TestLoadWithLocalOverrides(t *testing.T) {
	root := writeLoaderFixture(t, loaderFixtureOptions{
		LocalOverridesYAML: "runtime:\n  agent:\n    max_concurrent_agents: 2\n",
	})

	def, err := Load(root, "")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := getMapValue(def.Runtime, "agent")["max_concurrent_agents"]; got != 2 {
		t.Fatalf("runtime.agent.max_concurrent_agents = %v, want 2", got)
	}
}

func TestLoadSourcesRegistry(t *testing.T) {
	root := writeLoaderFixture(t, loaderFixtureOptions{
		ExtraSources: map[string]string{
			"github-core.yaml": "kind: github\napi_key: $GITHUB_TOKEN\nowner: org\nrepo: repo\n",
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

	trackerConfig := getMapValue(workflowDef.Config, "tracker")
	if got := trackerConfig["kind"]; got != "linear" {
		t.Fatalf("tracker.kind = %v, want linear", got)
	}
	if got := workflowDef.Source["branch_scope"]; got != "symphony-go" {
		t.Fatalf("source.branch_scope = %v, want symphony-go", got)
	}
	if workflowDef.PromptTemplate != "hello {{ source.kind }} {{ issue.title }}" {
		t.Fatalf("PromptTemplate = %q", workflowDef.PromptTemplate)
	}
}

func TestResolveActiveWorkflowMultipleSourcesRejected(t *testing.T) {
	root := writeLoaderFixture(t, loaderFixtureOptions{
		ProjectYAML: `runtime:
  codex:
    command: codex app-server
selection:
  dispatch_flow: implement
  enabled_sources:
    - linear-main
    - github-core
`,
		ExtraSources: map[string]string{
			"github-core.yaml": "kind: github\napi_key: $GITHUB_TOKEN\nowner: org\nrepo: repo\n",
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
		ProjectYAML: `runtime:
  polling:
    interval_ms: 15000
  workspace:
    root: ~/workspaces
  agent:
    max_turns: 5
  codex:
    command: codex app-server
  server:
    port: 8080
  orchestrator:
    auto_close_on_pr: false
selection:
  dispatch_flow: implement
  enabled_sources:
    - linear-main
`,
		FlowYAML: `prompt: prompts/implement.md.liquid
hooks:
  before_run: hooks/before_run.sh
  after_run: null
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

	if got := getMapValue(workflowDef.Config, "polling")["interval_ms"]; got != 15000 {
		t.Fatalf("polling.interval_ms = %v, want 15000", got)
	}
	if got := getMapValue(workflowDef.Config, "workspace")["linear_branch_scope"]; got != "symphony-go" {
		t.Fatalf("workspace.linear_branch_scope = %v, want symphony-go", got)
	}
	if got := getMapValue(workflowDef.Config, "tracker")["project_slug"]; got != "demo" {
		t.Fatalf("tracker.project_slug = %v, want demo", got)
	}
	if got := getMapValue(workflowDef.Config, "hooks")["before_run"]; got != "echo before-run" {
		t.Fatalf("hooks.before_run = %v, want echo before-run", got)
	}
	value, ok := getMapValue(workflowDef.Config, "hooks")["after_run"]
	if !ok || value != nil {
		t.Fatalf("hooks.after_run = %v, want explicit nil", value)
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

func TestLoadMissingProjectYaml(t *testing.T) {
	root := t.TempDir()
	if _, err := Load(root, ""); !errors.Is(err, model.ErrMissingWorkflowFile) {
		t.Fatalf("Load() error = %v, want ErrMissingWorkflowFile", err)
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
		projectYAML = `runtime:
  codex:
    command: codex app-server
selection:
  dispatch_flow: implement
  enabled_sources:
    - linear-main
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
api_key: $LINEAR_API_KEY
project_slug: demo
branch_scope: symphony-go
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
