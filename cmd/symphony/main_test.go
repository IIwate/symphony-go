package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"symphony-go/internal/agent"
	"symphony-go/internal/config"
	"symphony-go/internal/envfile"
	"symphony-go/internal/loader"
	"symphony-go/internal/logging"
	"symphony-go/internal/model"
	"symphony-go/internal/model/contract"
	"symphony-go/internal/orchestrator"
	"symphony-go/internal/tracker"
	"symphony-go/internal/workspace"
)

func TestRunCLIUsesDefaultAutomationDir(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "secret-key")
	workingDir := t.TempDir()
	writeAutomationConfig(t, filepath.Join(workingDir, "automation"), automationFixtureOptions{})

	originalDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	defer func() { _ = os.Chdir(originalDir) }()
	if err := os.Chdir(workingDir); err != nil {
		t.Fatalf("Chdir() error = %v", err)
	}

	restore := stubDependencies(t)
	defer restore()

	var stdout, stderr bytes.Buffer
	if exitCode := runCLI([]string{"run", "--dry-run"}, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("runCLI() exitCode = %d, stderr = %s", exitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "dry-run 校验通过") {
		t.Fatalf("stderr = %q, want dry-run success message", stderr.String())
	}
}

func TestRunCLIDryRunSkipsRuntimeDependencies(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "secret-key")
	restore := stubDependencies(t)
	defer restore()

	configDir := filepath.Join(t.TempDir(), "automation")
	writeAutomationConfig(t, configDir, automationFixtureOptions{})
	statePath := filepath.Join(configDir, "local", "session-state.json")
	writeFile(t, statePath, "not-json\n")
	projectYAML := fmt.Sprintf(`runtime:
  workspace:
    root: %s
  codex:
    command: codex app-server
  session_persistence:
    enabled: true
    kind: file
    file:
      path: ./local/session-state.json
      flush_interval_ms: 1000
      fsync_on_critical: true
selection:
  dispatch_flow: implement
  enabled_sources:
    - linear-main
defaults:
  profile: null
`, filepath.ToSlash(filepath.Join(filepath.Dir(configDir), "workspaces")))
	writeFile(t, filepath.Join(configDir, "project.yaml"), projectYAML)

	trackerCalls := 0
	workspaceCalls := 0
	runnerCalls := 0
	orchestratorCalls := 0
	serverCalls := 0
	watchCalls := 0

	newTrackerFactory = func(func() *model.ServiceConfig) (tracker.Client, error) {
		trackerCalls++
		return &fakeTrackerClient{}, nil
	}
	newWorkspaceFactory = func(func() *model.ServiceConfig, *slog.Logger) (workspace.Manager, error) {
		workspaceCalls++
		return &fakeWorkspaceManager{}, nil
	}
	newAgentRunnerFactory = func(func() *model.ServiceConfig, *slog.Logger) agent.Runner {
		runnerCalls++
		return fakeAgentRunner{}
	}
	newOrchestratorFactory = func(_ tracker.Client, _ workspace.Manager, _ agent.Runner, _ func() *model.ServiceConfig, _ func() *model.WorkflowDefinition, _ func() orchestrator.RuntimeIdentity, _ *slog.Logger) orchestratorService {
		orchestratorCalls++
		return &fakeOrchestrator{}
	}
	newHTTPServerFactory = func(runtime orchestratorService, logger *slog.Logger, host string, port int) (httpServer, error) {
		serverCalls++
		return &fakeHTTPServer{addr: "127.0.0.1:0"}, nil
	}
	watchAutomationDefinition = func(ctx context.Context, dir string, profile string, onChange func(*model.AutomationDefinition) error, onError func(error)) error {
		watchCalls++
		return nil
	}

	var stdout, stderr bytes.Buffer
	if exitCode := runCLI([]string{"run", "--dry-run", "--config-dir", configDir}, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("runCLI() exitCode = %d, stderr = %s", exitCode, stderr.String())
	}
	if trackerCalls != 0 || workspaceCalls != 0 || runnerCalls != 0 || orchestratorCalls != 0 || serverCalls != 0 || watchCalls != 0 {
		t.Fatalf(
			"dry-run initialized runtime deps: tracker=%d workspace=%d runner=%d orchestrator=%d server=%d watch=%d",
			trackerCalls,
			workspaceCalls,
			runnerCalls,
			orchestratorCalls,
			serverCalls,
			watchCalls,
		)
	}
	content, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", statePath, err)
	}
	if string(content) != "not-json\n" {
		t.Fatalf("session state content changed during dry-run: %q", string(content))
	}
}

func TestRunCLIRejectsLegacyWorkflowArgument(t *testing.T) {
	restore := stubDependencies(t)
	defer restore()

	var stdout, stderr bytes.Buffer
	if exitCode := runCLI([]string{"./WORKFLOW.md"}, &stdout, &stderr); exitCode == 0 {
		t.Fatalf("runCLI() exitCode = %d, want non-zero", exitCode)
	}
	if !strings.Contains(stderr.String(), "no longer supported") {
		t.Fatalf("stderr = %q, want legacy workflow rejection", stderr.String())
	}
}

func TestRunCLIHelpCommand(t *testing.T) {
	restore := stubDependencies(t)
	defer restore()

	var stdout, stderr bytes.Buffer
	if exitCode := runCLI([]string{"help"}, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("runCLI() exitCode = %d, stderr = %s", exitCode, stderr.String())
	}
	if strings.Contains(stderr.String(), "no longer supported") {
		t.Fatalf("stderr = %q, want Cobra help output instead of legacy workflow rejection", stderr.String())
	}
	if !strings.Contains(stdout.String(), "Available Commands:") {
		t.Fatalf("stdout = %q, want Cobra help output", stdout.String())
	}
}

func TestRunCLIFailsWhenDefaultAutomationMissing(t *testing.T) {
	restore := stubDependencies(t)
	defer restore()

	workingDir := t.TempDir()
	originalDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	defer func() { _ = os.Chdir(originalDir) }()
	if err := os.Chdir(workingDir); err != nil {
		t.Fatalf("Chdir() error = %v", err)
	}

	var stdout, stderr bytes.Buffer
	if exitCode := runCLI([]string{"run"}, &stdout, &stderr); exitCode == 0 {
		t.Fatalf("runCLI() exitCode = %d, want non-zero", exitCode)
	}
	if !strings.Contains(stderr.String(), "missing_workflow_file") {
		t.Fatalf("stderr = %q, want missing_workflow_file", stderr.String())
	}
}

func TestRunCLIRequiresExplicitSubcommand(t *testing.T) {
	restore := stubDependencies(t)
	defer restore()

	var stdout, stderr bytes.Buffer
	if exitCode := runCLI(nil, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("runCLI() exitCode = %d, stderr = %s", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "run") || !strings.Contains(stdout.String(), "doctor") {
		t.Fatalf("stdout = %q, want run/doctor help output", stdout.String())
	}
}

func TestRunCLIDoctorReady(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "secret-key")
	configDir := filepath.Join(t.TempDir(), "automation")
	writeAutomationConfig(t, configDir, automationFixtureOptions{})

	var stdout, stderr bytes.Buffer
	if exitCode := runCLI([]string{"doctor", "--config-dir", configDir}, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("runCLI() exitCode = %d, stderr = %s", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "配置已完整") {
		t.Fatalf("stdout = %q, want ready message", stdout.String())
	}
}

func TestRunCLIDoctorReportsMissingSecret(t *testing.T) {
	configDir := filepath.Join(t.TempDir(), "automation")
	writeAutomationConfig(t, configDir, automationFixtureOptions{})
	if err := os.Unsetenv("LINEAR_API_KEY"); err != nil {
		t.Fatalf("Unsetenv() error = %v", err)
	}

	var stdout, stderr bytes.Buffer
	if exitCode := runCLI([]string{"doctor", "--config-dir", configDir}, &stdout, &stderr); exitCode == 0 {
		t.Fatalf("runCLI() exitCode = %d, want non-zero", exitCode)
	}
	if !strings.Contains(stderr.String(), "missing required secrets") || !strings.Contains(stderr.String(), "LINEAR_API_KEY") {
		t.Fatalf("stderr = %q, want missing secret diagnosis", stderr.String())
	}
}

func TestRunCLIRejectsRemovedCommands(t *testing.T) {
	restore := stubDependencies(t)
	defer restore()

	cases := [][]string{
		{"setup"},
		{"config"},
		{"config", "set"},
		{"wizard"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if exitCode := runCLI(args, &stdout, &stderr); exitCode == 0 {
				t.Fatalf("runCLI(%v) exitCode = %d, want non-zero", args, exitCode)
			}
			if !strings.Contains(stderr.String(), "unknown command") {
				t.Fatalf("stderr = %q, want unknown command", stderr.String())
			}
		})
	}
}

func TestRunCLIRunDoesNotWriteSecretsInteractively(t *testing.T) {
	restore := stubDependencies(t)
	defer restore()

	configDir := filepath.Join(t.TempDir(), "automation")
	writeAutomationConfig(t, configDir, automationFixtureOptions{})
	if err := os.Unsetenv("LINEAR_API_KEY"); err != nil {
		t.Fatalf("Unsetenv() error = %v", err)
	}

	var stdout, stderr bytes.Buffer
	if exitCode := runCLI([]string{"run", "--config-dir", configDir}, &stdout, &stderr); exitCode == 0 {
		t.Fatalf("runCLI() exitCode = %d, want non-zero", exitCode)
	}
	if !strings.Contains(stderr.String(), "missing required secrets") {
		t.Fatalf("stderr = %q, want diagnosis output", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(configDir, "local", "env.local")); err == nil {
		t.Fatal("env.local was created, want no secret write side effect")
	} else if !os.IsNotExist(err) {
		t.Fatalf("Stat(env.local) error = %v", err)
	}
}

func TestRuntimeStateApplyReloadKeepsPortOverride(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "secret-key")
	configDir := filepath.Join(t.TempDir(), "automation")
	writeAutomationConfig(t, configDir, automationFixtureOptions{})

	repoDef, err := loader.Load(configDir, "")
	if err != nil {
		t.Fatalf("loader.Load() error = %v", err)
	}
	definition, err := loader.ResolveActiveWorkflow(repoDef)
	if err != nil {
		t.Fatalf("loader.ResolveActiveWorkflow() error = %v", err)
	}
	cfg, err := config.NewFromWorkflow(definition)
	if err != nil {
		t.Fatalf("config.NewFromWorkflow() error = %v", err)
	}

	port := 8080
	cfg.ServerPort = &port
	state := &runtimeState{
		repoDef:      repoDef,
		definition:   definition,
		config:       cfg,
		portOverride: &port,
		configDir:    repoDef.RootDir,
	}

	if _, err := state.ApplyReload(repoDef); err != nil {
		t.Fatalf("ApplyReload() error = %v", err)
	}
	if state.CurrentConfig().ServerPort == nil || *state.CurrentConfig().ServerPort != 8080 {
		t.Fatalf("ServerPort = %v, want 8080", state.CurrentConfig().ServerPort)
	}
}

func TestRuntimeStateApplyReloadRejectsRuntimeExtensionChanges(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "secret-key")
	tmpDir := t.TempDir()
	configDir := filepath.Join(tmpDir, "automation")
	workspaceRoot := filepath.ToSlash(filepath.Join(tmpDir, "workspaces"))
	writeAutomationConfig(t, configDir, automationFixtureOptions{WorkspaceRoot: workspaceRoot})
	writeFile(t, filepath.Join(configDir, "flows", "other-flow.yaml"), "prompt: prompts/other.md.liquid\n")
	writeFile(t, filepath.Join(configDir, "prompts", "other.md.liquid"), "other flow\n")

	writeProject := func(workspaceValue string, branchNamespace string, sessionPath string, notificationURL string, pollInterval int, dispatchFlow string, codexCommand string, maxConcurrentAgents int, agentExtras string) {
		agentBlock := fmt.Sprintf("    max_concurrent_agents: %d\n    max_turns: 20\n", maxConcurrentAgents)
		if agentExtras != "" {
			agentBlock += agentExtras
		}
		projectYAML := fmt.Sprintf(`runtime:
  workspace:
    root: %s
    branch_namespace: %s
  polling:
    interval_ms: %d
  agent:
%s  codex:
    command: %s
  session_persistence:
    enabled: true
    kind: file
    file:
      path: %s
      flush_interval_ms: 1000
      fsync_on_critical: true
  notifications:
    channels:
      - id: ops
        display_name: Ops
        kind: webhook
        subscriptions:
          types: [system_alert]
        webhook:
          url: %s
    defaults:
      timeout_ms: 5000
      retry_count: 2
      retry_delay_ms: 1000
      queue_size: 64
      critical_queue_size: 16
selection:
  dispatch_flow: %s
  enabled_sources:
    - linear-main
defaults:
  profile: null
`, workspaceValue, branchNamespace, pollInterval, agentBlock, codexCommand, sessionPath, notificationURL, dispatchFlow)
		writeFile(t, filepath.Join(configDir, "project.yaml"), projectYAML)
	}

	writeProject(workspaceRoot, "runner-a", "./automation/local/session-state.json", "https://hooks.example.com/a", 30000, "implement", "codex app-server", 10, "")

	repoDef, err := loader.Load(configDir, "")
	if err != nil {
		t.Fatalf("loader.Load() error = %v", err)
	}
	definition, err := loader.ResolveActiveWorkflow(repoDef)
	if err != nil {
		t.Fatalf("loader.ResolveActiveWorkflow() error = %v", err)
	}
	cfg, err := config.NewFromWorkflow(definition)
	if err != nil {
		t.Fatalf("config.NewFromWorkflow() error = %v", err)
	}
	state := &runtimeState{
		repoDef:    repoDef,
		definition: definition,
		config:     cfg,
		configDir:  repoDef.RootDir,
		profile:    repoDef.Profile,
	}

	tests := []struct {
		name              string
		workspace         string
		branchNamespace   string
		sessionPath       string
		url               string
		pollInterval      int
		dispatchFlow      string
		codexCommand      string
		maxConcurrent     int
		agentExtras       string
		wantErr           string
		wantURL           string
		wantPoll          int
		wantMaxConcurrent int
		wantStateLimit    int
	}{
		{
			name:            "session persistence",
			workspace:       workspaceRoot,
			branchNamespace: "runner-a",
			sessionPath:     "./automation/local/other-session-state.json",
			url:             "https://hooks.example.com/a",
			pollInterval:    30000,
			dispatchFlow:    "implement",
			codexCommand:    "codex app-server",
			maxConcurrent:   10,
			wantErr:         "runtime.session_persistence changed: restart required",
		},
		{
			name:            "workspace root",
			workspace:       filepath.ToSlash(filepath.Join(tmpDir, "other-workspaces")),
			branchNamespace: "runner-a",
			sessionPath:     "./automation/local/session-state.json",
			url:             "https://hooks.example.com/a",
			pollInterval:    30000,
			dispatchFlow:    "implement",
			codexCommand:    "codex app-server",
			maxConcurrent:   10,
			wantErr:         "runtime.workspace.root changed: restart required",
		},
		{
			name:            "workspace branch namespace",
			workspace:       workspaceRoot,
			branchNamespace: "runner-b",
			sessionPath:     "./automation/local/session-state.json",
			url:             "https://hooks.example.com/a",
			pollInterval:    30000,
			dispatchFlow:    "implement",
			codexCommand:    "codex app-server",
			maxConcurrent:   10,
			wantErr:         "runtime.workspace.branch_namespace changed: restart required",
		},
		{
			name:            "dispatch flow",
			workspace:       workspaceRoot,
			branchNamespace: "runner-a",
			sessionPath:     "./automation/local/session-state.json",
			url:             "https://hooks.example.com/a",
			pollInterval:    30000,
			dispatchFlow:    "other-flow",
			codexCommand:    "codex app-server",
			maxConcurrent:   10,
			wantErr:         "selection.dispatch_flow changed: restart required",
		},
		{
			name:            "codex command",
			workspace:       workspaceRoot,
			branchNamespace: "runner-a",
			sessionPath:     "./automation/local/session-state.json",
			url:             "https://hooks.example.com/a",
			pollInterval:    30000,
			dispatchFlow:    "implement",
			codexCommand:    "codex next-server",
			maxConcurrent:   10,
			wantErr:         "runtime.codex.command changed: restart required",
		},
		{
			name:            "notifications",
			workspace:       workspaceRoot,
			branchNamespace: "runner-a",
			sessionPath:     "./automation/local/session-state.json",
			url:             "https://hooks.example.com/b",
			pollInterval:    30000,
			dispatchFlow:    "implement",
			codexCommand:    "codex app-server",
			maxConcurrent:   10,
			wantURL:         "https://hooks.example.com/b",
		},
		{
			name:            "poll interval",
			workspace:       workspaceRoot,
			branchNamespace: "runner-a",
			sessionPath:     "./automation/local/session-state.json",
			url:             "https://hooks.example.com/a",
			pollInterval:    45000,
			dispatchFlow:    "implement",
			codexCommand:    "codex app-server",
			maxConcurrent:   10,
			wantPoll:        45000,
		},
		{
			name:              "max concurrent agents",
			workspace:         workspaceRoot,
			branchNamespace:   "runner-a",
			sessionPath:       "./automation/local/session-state.json",
			url:               "https://hooks.example.com/a",
			pollInterval:      30000,
			dispatchFlow:      "implement",
			codexCommand:      "codex app-server",
			maxConcurrent:     3,
			wantMaxConcurrent: 3,
		},
		{
			name:            "max concurrent agents by state",
			workspace:       workspaceRoot,
			branchNamespace: "runner-a",
			sessionPath:     "./automation/local/session-state.json",
			url:             "https://hooks.example.com/a",
			pollInterval:    30000,
			dispatchFlow:    "implement",
			codexCommand:    "codex app-server",
			maxConcurrent:   10,
			agentExtras: "    max_concurrent_agents_by_state:\n" +
				"      todo: 2\n",
			wantStateLimit: 2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			writeProject(tc.workspace, tc.branchNamespace, tc.sessionPath, tc.url, tc.pollInterval, tc.dispatchFlow, tc.codexCommand, tc.maxConcurrent, tc.agentExtras)
			reloaded, err := loader.Load(configDir, "")
			if err != nil {
				t.Fatalf("loader.Load() reload error = %v", err)
			}
			_, err = state.ApplyReload(reloaded)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("ApplyReload() error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ApplyReload() error = %v", err)
			}
			if tc.wantURL != "" {
				if got := state.CurrentConfig().Notifications.Channels[0].Webhook.URL; got != tc.wantURL {
					t.Fatalf("Notifications.Channels[0].Webhook.URL = %q, want %q", got, tc.wantURL)
				}
			}
			if tc.wantPoll != 0 && state.CurrentConfig().PollIntervalMS != tc.wantPoll {
				t.Fatalf("PollIntervalMS = %d, want %d", state.CurrentConfig().PollIntervalMS, tc.wantPoll)
			}
			if tc.wantMaxConcurrent != 0 && state.CurrentConfig().MaxConcurrentAgents != tc.wantMaxConcurrent {
				t.Fatalf("MaxConcurrentAgents = %d, want %d", state.CurrentConfig().MaxConcurrentAgents, tc.wantMaxConcurrent)
			}
			if tc.wantStateLimit != 0 && state.CurrentConfig().MaxConcurrentAgentsByState["todo"] != tc.wantStateLimit {
				t.Fatalf("MaxConcurrentAgentsByState[todo] = %d, want %d", state.CurrentConfig().MaxConcurrentAgentsByState["todo"], tc.wantStateLimit)
			}
		})
	}
}

func TestExecuteFailsWhenSessionStateIdentityMismatch(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "secret-key")
	restore := stubDependencies(t)
	defer restore()

	tmpDir := t.TempDir()
	configDir := filepath.Join(tmpDir, "automation")
	writeAutomationConfig(t, configDir, automationFixtureOptions{})
	statePath := filepath.Join(configDir, "local", "session-state.json")
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", filepath.Dir(statePath), err)
	}
	projectYAML := fmt.Sprintf(`runtime:
  workspace:
    root: %s
  codex:
    command: codex app-server
  session_persistence:
    enabled: true
    kind: file
    file:
      path: ./local/session-state.json
      flush_interval_ms: 1000
      fsync_on_critical: true
selection:
  dispatch_flow: implement
  enabled_sources:
    - linear-main
defaults:
  profile: null
`, filepath.ToSlash(filepath.Join(tmpDir, "workspaces")))
	writeFile(t, filepath.Join(configDir, "project.yaml"), projectYAML)
	writeFile(t, statePath, fmt.Sprintf(`{
  "version": 5,
  "identity": {
    "compatibility": {
      "profile": "",
      "active_source": "linear-main",
      "source_kind": "",
      "flow_name": "implement",
      "tracker_kind": "different",
      "tracker_repo": "",
      "tracker_project_slug": "demo"
    },
    "descriptor": {
      "config_root": "C:/different/root",
      "workspace_root": "",
      "session_persistence_kind": "file",
      "session_state_path": "%s"
    }
  },
  "saved_at": "2026-03-12T00:00:00Z"
}
`, filepath.ToSlash(statePath)))

	newTrackerFactory = func(func() *model.ServiceConfig) (tracker.Client, error) {
		return &fakeTrackerClient{}, nil
	}
	newWorkspaceFactory = func(func() *model.ServiceConfig, *slog.Logger) (workspace.Manager, error) {
		return &fakeWorkspaceManager{}, nil
	}
	newAgentRunnerFactory = func(func() *model.ServiceConfig, *slog.Logger) agent.Runner {
		return fakeAgentRunner{}
	}
	newOrchestratorFactory = func(tc tracker.Client, wm workspace.Manager, runner agent.Runner, configFn func() *model.ServiceConfig, workflowFn func() *model.WorkflowDefinition, identityFn func() orchestrator.RuntimeIdentity, logger *slog.Logger) orchestratorService {
		return orchestrator.NewOrchestrator(tc, wm, runner, configFn, workflowFn, identityFn, logger)
	}
	newHTTPServerFactory = func(runtime orchestratorService, logger *slog.Logger, host string, port int) (httpServer, error) {
		return &fakeHTTPServer{addr: "127.0.0.1:0"}, nil
	}
	notifySignalContext = func(parent context.Context, _ ...os.Signal) (context.Context, context.CancelFunc) {
		return context.WithCancel(parent)
	}

	var stdout, stderr bytes.Buffer
	err := execute([]string{"run", "--config-dir", configDir}, &stdout, &stderr)
	if err == nil {
		t.Fatal("execute() error = nil, want session state identity mismatch failure")
	}
	if !strings.Contains(err.Error(), "delete the file and restart") {
		t.Fatalf("execute() error = %v, want delete/restart guidance", err)
	}
	if !strings.Contains(err.Error(), "identity does not match current runtime") {
		t.Fatalf("execute() error = %v, want identity mismatch detail", err)
	}
}

func TestExecuteStartsWatcherAndNotifiesReload(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "secret-key")
	var reloadCount int
	watchCalled := false
	restore := stubDependencies(t)
	defer restore()

	configDir := filepath.Join(t.TempDir(), "automation")
	writeAutomationConfig(t, configDir, automationFixtureOptions{})

	newOrchestratorFactory = func(_ tracker.Client, _ workspace.Manager, _ agent.Runner, _ func() *model.ServiceConfig, _ func() *model.WorkflowDefinition, _ func() orchestrator.RuntimeIdentity, _ *slog.Logger) orchestratorService {
		return &fakeOrchestrator{notifyReload: func(_ *model.WorkflowDefinition) { reloadCount++ }}
	}
	watchAutomationDefinition = func(ctx context.Context, dir string, profile string, onChange func(*model.AutomationDefinition) error, onError func(error)) error {
		watchCalled = true
		reloaded, err := loader.Load(dir, profile)
		if err != nil {
			return err
		}
		if err := onChange(reloaded); err != nil {
			if onError != nil {
				onError(err)
			}
		}
		go func() { <-ctx.Done() }()
		return nil
	}
	notifySignalContext = func(parent context.Context, _ ...os.Signal) (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithCancel(parent)
		go cancel()
		return ctx, func() {}
	}

	var stdout, stderr bytes.Buffer
	if err := execute([]string{"run", "--config-dir", configDir}, &stdout, &stderr); err != nil {
		t.Fatalf("execute() error = %v", err)
	}
	if !watchCalled {
		t.Fatal("watch automation was not called")
	}
	if reloadCount != 1 {
		t.Fatalf("reloadCount = %d, want 1", reloadCount)
	}
}

func TestExecuteGracefulShutdownOnContextCancel(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "secret-key")
	restore := stubDependencies(t)
	defer restore()

	configDir := filepath.Join(t.TempDir(), "automation")
	writeAutomationConfig(t, configDir, automationFixtureOptions{})

	signalCtx, signalCancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	waitReturned := make(chan struct{})
	shutdownCalled := make(chan struct{})

	newOrchestratorFactory = func(_ tracker.Client, _ workspace.Manager, _ agent.Runner, _ func() *model.ServiceConfig, _ func() *model.WorkflowDefinition, _ func() orchestrator.RuntimeIdentity, _ *slog.Logger) orchestratorService {
		return &fakeOrchestrator{
			start: func(context.Context) error {
				close(started)
				return nil
			},
			wait: func() {
				<-signalCtx.Done()
				close(waitReturned)
			},
		}
	}
	newHTTPServerFactory = func(runtime orchestratorService, logger *slog.Logger, host string, port int) (httpServer, error) {
		return &fakeHTTPServer{
			addr: "127.0.0.1:8080",
			shutdown: func(context.Context) error {
				select {
				case <-waitReturned:
				default:
					t.Fatal("http shutdown happened before orchestrator.Wait returned")
				}
				close(shutdownCalled)
				return nil
			},
		}, nil
	}
	watchAutomationDefinition = func(context.Context, string, string, func(*model.AutomationDefinition) error, func(error)) error {
		return nil
	}
	notifySignalContext = func(parent context.Context, _ ...os.Signal) (context.Context, context.CancelFunc) {
		return signalCtx, func() {}
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- execute([]string{"run", "--config-dir", configDir, "--port", "8080"}, io.Discard, io.Discard)
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("orchestrator did not start")
	}

	signalCancel()

	select {
	case <-shutdownCalled:
	case <-time.After(time.Second):
		t.Fatal("http shutdown was not called")
	}

	if err := <-errCh; err != nil {
		t.Fatalf("execute() error = %v", err)
	}
}

func TestExecuteShutdownWaitsForWorkers(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "secret-key")
	restore := stubDependencies(t)
	defer restore()

	configDir := filepath.Join(t.TempDir(), "automation")
	writeAutomationConfig(t, configDir, automationFixtureOptions{})

	signalCtx, signalCancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	waitEntered := make(chan struct{})
	releaseWait := make(chan struct{})
	shutdownCalled := make(chan struct{})

	newOrchestratorFactory = func(_ tracker.Client, _ workspace.Manager, _ agent.Runner, _ func() *model.ServiceConfig, _ func() *model.WorkflowDefinition, _ func() orchestrator.RuntimeIdentity, _ *slog.Logger) orchestratorService {
		return &fakeOrchestrator{
			start: func(context.Context) error {
				close(started)
				return nil
			},
			wait: func() {
				close(waitEntered)
				<-releaseWait
			},
		}
	}
	newHTTPServerFactory = func(runtime orchestratorService, logger *slog.Logger, host string, port int) (httpServer, error) {
		return &fakeHTTPServer{
			addr: "127.0.0.1:8081",
			shutdown: func(context.Context) error {
				close(shutdownCalled)
				return nil
			},
		}, nil
	}
	watchAutomationDefinition = func(context.Context, string, string, func(*model.AutomationDefinition) error, func(error)) error {
		return nil
	}
	notifySignalContext = func(parent context.Context, _ ...os.Signal) (context.Context, context.CancelFunc) {
		return signalCtx, func() {}
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- execute([]string{"run", "--config-dir", configDir, "--port", "8081"}, io.Discard, io.Discard)
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("orchestrator did not start")
	}

	signalCancel()

	select {
	case <-waitEntered:
	case <-time.After(time.Second):
		t.Fatal("orchestrator.Wait was not entered")
	}

	select {
	case <-shutdownCalled:
		t.Fatal("http shutdown happened before workers finished")
	case <-time.After(100 * time.Millisecond):
	}

	close(releaseWait)

	select {
	case <-shutdownCalled:
	case <-time.After(time.Second):
		t.Fatal("http shutdown was not called after workers finished")
	}

	if err := <-errCh; err != nil {
		t.Fatalf("execute() error = %v", err)
	}
}

func TestExecuteShutdownWithHTTPServer(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "secret-key")
	restore := stubDependencies(t)
	defer restore()

	configDir := filepath.Join(t.TempDir(), "automation")
	writeAutomationConfig(t, configDir, automationFixtureOptions{})

	signalCtx, signalCancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	shutdownCalled := make(chan struct{})
	shutdownCount := 0
	gotPort := -1

	newOrchestratorFactory = func(_ tracker.Client, _ workspace.Manager, _ agent.Runner, _ func() *model.ServiceConfig, _ func() *model.WorkflowDefinition, _ func() orchestrator.RuntimeIdentity, _ *slog.Logger) orchestratorService {
		return &fakeOrchestrator{
			start: func(context.Context) error {
				close(started)
				return nil
			},
			wait: func() {
				<-signalCtx.Done()
			},
		}
	}
	newHTTPServerFactory = func(runtime orchestratorService, logger *slog.Logger, host string, port int) (httpServer, error) {
		gotPort = port
		return &fakeHTTPServer{
			addr: "127.0.0.1:9090",
			shutdown: func(context.Context) error {
				shutdownCount++
				close(shutdownCalled)
				return nil
			},
		}, nil
	}
	watchAutomationDefinition = func(context.Context, string, string, func(*model.AutomationDefinition) error, func(error)) error {
		return nil
	}
	notifySignalContext = func(parent context.Context, _ ...os.Signal) (context.Context, context.CancelFunc) {
		return signalCtx, func() {}
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- execute([]string{"run", "--config-dir", configDir, "--port", "9090"}, io.Discard, io.Discard)
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("orchestrator did not start")
	}

	signalCancel()

	select {
	case <-shutdownCalled:
	case <-time.After(time.Second):
		t.Fatal("http shutdown was not called")
	}

	if err := <-errCh; err != nil {
		t.Fatalf("execute() error = %v", err)
	}
	if gotPort != 9090 {
		t.Fatalf("http server port = %d, want 9090", gotPort)
	}
	if shutdownCount != 1 {
		t.Fatalf("http shutdown count = %d, want 1", shutdownCount)
	}
}

func TestExecutePassesListenHostConfigurationToHTTPServer(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "secret-key")

	tests := []struct {
		name     string
		host     string
		expected string
	}{
		{name: "default localhost", expected: "127.0.0.1"},
		{name: "explicit remote host", host: "0.0.0.0", expected: "0.0.0.0"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			restore := stubDependencies(t)
			defer restore()

			configDir := filepath.Join(t.TempDir(), "automation")
			writeAutomationConfig(t, configDir, automationFixtureOptions{})
			writeRunProjectConfig(t, configDir, tc.host, 0)

			signalCtx, signalCancel := context.WithCancel(context.Background())
			started := make(chan struct{})
			gotHost := ""
			gotPort := -1

			newOrchestratorFactory = func(_ tracker.Client, _ workspace.Manager, _ agent.Runner, _ func() *model.ServiceConfig, _ func() *model.WorkflowDefinition, _ func() orchestrator.RuntimeIdentity, _ *slog.Logger) orchestratorService {
				return &fakeOrchestrator{
					start: func(context.Context) error {
						close(started)
						return nil
					},
					wait: func() {
						<-signalCtx.Done()
					},
				}
			}
			newHTTPServerFactory = func(runtime orchestratorService, logger *slog.Logger, host string, port int) (httpServer, error) {
				gotHost = host
				gotPort = port
				return &fakeHTTPServer{addr: "127.0.0.1:0"}, nil
			}
			watchAutomationDefinition = func(context.Context, string, string, func(*model.AutomationDefinition) error, func(error)) error {
				return nil
			}
			notifySignalContext = func(parent context.Context, _ ...os.Signal) (context.Context, context.CancelFunc) {
				return signalCtx, func() {}
			}

			errCh := make(chan error, 1)
			go func() {
				errCh <- execute([]string{"run", "--config-dir", configDir}, io.Discard, io.Discard)
			}()

			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("orchestrator did not start")
			}

			signalCancel()

			if err := <-errCh; err != nil {
				t.Fatalf("execute() error = %v", err)
			}
			if gotHost != tc.expected {
				t.Fatalf("http host = %q, want %q", gotHost, tc.expected)
			}
			if gotPort != 0 {
				t.Fatalf("http port = %d, want 0", gotPort)
			}
		})
	}
}

func stubDependencies(t *testing.T) func() {
	t.Helper()
	origLoadEnv := loadEnvFile
	origLoadAutomation := loadAutomationDefinition
	origResolve := resolveActiveWorkflow
	origWatch := watchAutomationDefinition
	origLogger := newLoggerFactory
	origTracker := newTrackerFactory
	origWorkspace := newWorkspaceFactory
	origRunner := newAgentRunnerFactory
	origOrchestrator := newOrchestratorFactory
	origHTTPServer := newHTTPServerFactory
	origNotify := notifySignalContext

	loadEnvFile = envfile.Load
	loadAutomationDefinition = loader.Load
	resolveActiveWorkflow = loader.ResolveActiveWorkflow
	watchAutomationDefinition = func(ctx context.Context, dir string, profile string, onChange func(*model.AutomationDefinition) error, onError func(error)) error {
		return loader.WatchWithErrors(ctx, dir, profile, onChange, onError)
	}
	newLoggerFactory = func(opts logging.Options) (*slog.Logger, io.Closer, error) {
		return logging.NewLogger(opts)
	}
	newTrackerFactory = func(func() *model.ServiceConfig) (tracker.Client, error) {
		return &fakeTrackerClient{}, nil
	}
	newWorkspaceFactory = func(func() *model.ServiceConfig, *slog.Logger) (workspace.Manager, error) {
		return &fakeWorkspaceManager{}, nil
	}
	newAgentRunnerFactory = func(func() *model.ServiceConfig, *slog.Logger) agent.Runner {
		return fakeAgentRunner{}
	}
	newOrchestratorFactory = func(_ tracker.Client, _ workspace.Manager, _ agent.Runner, _ func() *model.ServiceConfig, _ func() *model.WorkflowDefinition, _ func() orchestrator.RuntimeIdentity, _ *slog.Logger) orchestratorService {
		return &fakeOrchestrator{}
	}
	newHTTPServerFactory = func(runtime orchestratorService, logger *slog.Logger, host string, port int) (httpServer, error) {
		return &fakeHTTPServer{addr: "127.0.0.1:0"}, nil
	}
	notifySignalContext = func(parent context.Context, _ ...os.Signal) (context.Context, context.CancelFunc) {
		return context.WithCancel(parent)
	}

	return func() {
		loadEnvFile = origLoadEnv
		loadAutomationDefinition = origLoadAutomation
		resolveActiveWorkflow = origResolve
		watchAutomationDefinition = origWatch
		newLoggerFactory = origLogger
		newTrackerFactory = origTracker
		newWorkspaceFactory = origWorkspace
		newAgentRunnerFactory = origRunner
		newOrchestratorFactory = origOrchestrator
		newHTTPServerFactory = origHTTPServer
		notifySignalContext = origNotify
	}
}

type fakeTrackerClient struct{}

func (fakeTrackerClient) FetchCandidateIssues(context.Context) ([]model.Issue, error) {
	return nil, nil
}
func (fakeTrackerClient) FetchIssuesByStates(context.Context, []string) ([]model.Issue, error) {
	return nil, nil
}
func (fakeTrackerClient) FetchIssueStatesByIDs(context.Context, []string) ([]model.Issue, error) {
	return nil, nil
}

type fakeWorkspaceManager struct{}

func (fakeWorkspaceManager) CreateForIssue(context.Context, string) (*model.Workspace, error) {
	return &model.Workspace{}, nil
}
func (fakeWorkspaceManager) CleanupWorkspace(context.Context, string) error { return nil }

type fakeAgentRunner struct{}

func (fakeAgentRunner) Run(context.Context, agent.RunParams) error { return nil }

type fakeOrchestrator struct {
	mu             sync.Mutex
	runOnce        func(context.Context, bool)
	notifyReload   func(*model.WorkflowDefinition)
	requestRefresh func() orchestrator.RefreshRequestResult
	start          func(context.Context) error
	wait           func()
	discovery      orchestrator.DiscoveryDocument
	snapshot       orchestrator.Snapshot
	nextID         int
	subscribers    map[int]chan orchestrator.Snapshot
}

func (f *fakeOrchestrator) Start(ctx context.Context) error {
	if f.start != nil {
		return f.start(ctx)
	}
	return nil
}

func (f *fakeOrchestrator) Wait() {
	if f.wait != nil {
		f.wait()
	}
}

func (f *fakeOrchestrator) RunOnce(ctx context.Context, allowDispatch bool) {
	if f.runOnce != nil {
		f.runOnce(ctx, allowDispatch)
	}
}

func (f *fakeOrchestrator) NotifyWorkflowReload(def *model.WorkflowDefinition) {
	if f.notifyReload != nil {
		f.notifyReload(def)
	}
}

func (f *fakeOrchestrator) Discovery() orchestrator.DiscoveryDocument {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.discovery
}

func (f *fakeOrchestrator) RequestRefresh() orchestrator.RefreshRequestResult {
	if f.requestRefresh != nil {
		return f.requestRefresh()
	}
	reason := contract.MustReason(contract.ReasonControlRefreshAccepted, map[string]any{
		"service_mode": contract.ServiceModeServing,
	})
	return contract.ControlResult{
		Action:              contract.ControlActionRefresh,
		Status:              contract.ControlStatusAccepted,
		Reason:              &reason,
		RecommendedNextStep: "等待 SSE 通知后回读 /api/v1/state",
		Timestamp:           "2026-03-14T00:00:00Z",
	}
}
func (f *fakeOrchestrator) Snapshot() orchestrator.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snapshot
}
func (f *fakeOrchestrator) SubscribeSnapshots(buffer int) (<-chan orchestrator.Snapshot, func()) {
	f.mu.Lock()
	if f.subscribers == nil {
		f.subscribers = map[int]chan orchestrator.Snapshot{}
	}
	ch := make(chan orchestrator.Snapshot, max(1, buffer))
	id := f.nextID
	f.nextID++
	f.subscribers[id] = ch
	snapshot := f.snapshot
	f.mu.Unlock()
	ch <- snapshot
	return ch, func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if existing, ok := f.subscribers[id]; ok {
			delete(f.subscribers, id)
			close(existing)
		}
	}
}

func (f *fakeOrchestrator) publish(snapshot orchestrator.Snapshot) {
	f.mu.Lock()
	f.snapshot = snapshot
	subscribers := make([]chan orchestrator.Snapshot, 0, len(f.subscribers))
	for _, ch := range f.subscribers {
		subscribers = append(subscribers, ch)
	}
	f.mu.Unlock()
	for _, ch := range subscribers {
		ch <- snapshot
	}
}

type fakeHTTPServer struct {
	addr     string
	shutdown func(context.Context) error
}

func (f *fakeHTTPServer) Addr() string { return f.addr }
func (f *fakeHTTPServer) Shutdown(ctx context.Context) error {
	if f.shutdown != nil {
		return f.shutdown(ctx)
	}
	return nil
}

func max(left int, right int) int {
	if left > right {
		return left
	}
	return right
}

type automationFixtureOptions struct {
	PromptTemplate string
	WorkspaceRoot  string
}

func writeAutomationConfig(t *testing.T, root string, opts automationFixtureOptions) {
	t.Helper()

	if opts.PromptTemplate == "" {
		opts.PromptTemplate = "hello {{ source.kind }} {{ issue.title }}"
	}
	if opts.WorkspaceRoot == "" {
		opts.WorkspaceRoot = filepath.ToSlash(filepath.Join(filepath.Dir(root), "workspaces"))
	}

	for _, dir := range []string{
		root,
		filepath.Join(root, "sources"),
		filepath.Join(root, "flows"),
		filepath.Join(root, "prompts"),
		filepath.Join(root, "local"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%q) error = %v", dir, err)
		}
	}

	projectYAML := fmt.Sprintf(`runtime:
  workspace:
    root: %s
  codex:
    command: codex app-server
selection:
  dispatch_flow: implement
  enabled_sources:
    - linear-main
defaults:
  profile: null
`, filepath.ToSlash(opts.WorkspaceRoot))
	writeFile(t, filepath.Join(root, "project.yaml"), projectYAML)
	writeFile(t, filepath.Join(root, "sources", "linear-main.yaml"), `kind: linear
api_key: $LINEAR_API_KEY
project_slug: demo
branch_scope: demo-scope
active_states: ["Todo", "In Progress"]
terminal_states: ["Closed", "Done"]
`)
	writeFile(t, filepath.Join(root, "flows", "implement.yaml"), `prompt: prompts/implement.md.liquid
`)
	writeFile(t, filepath.Join(root, "prompts", "implement.md.liquid"), opts.PromptTemplate+"\n")
}

func writeRunProjectConfig(t *testing.T, root string, host string, port int) {
	t.Helper()

	workspaceRoot := filepath.ToSlash(filepath.Join(filepath.Dir(root), "workspaces"))
	hostBlock := ""
	if strings.TrimSpace(host) != "" {
		hostBlock = fmt.Sprintf("    host: %s\n", host)
	}
	projectYAML := fmt.Sprintf(`runtime:
  workspace:
    root: %s
  codex:
    command: codex app-server
  server:
%s    port: %d
selection:
  dispatch_flow: implement
  enabled_sources:
    - linear-main
defaults:
  profile: null
`, workspaceRoot, hostBlock, port)
	writeFile(t, filepath.Join(root, "project.yaml"), projectYAML)
}

func writeFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
}
