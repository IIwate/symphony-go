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
	"testing"
	"time"

	"symphony-go/internal/agent"
	"symphony-go/internal/config"
	"symphony-go/internal/envfile"
	"symphony-go/internal/loader"
	"symphony-go/internal/logging"
	"symphony-go/internal/model"
	"symphony-go/internal/orchestrator"
	"symphony-go/internal/secret"
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

	var stderr bytes.Buffer
	if exitCode := runCLI([]string{"--dry-run"}, &stderr); exitCode != 0 {
		t.Fatalf("runCLI() exitCode = %d, stderr = %s", exitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "dry-run 仍会访问 tracker 并执行 startupCleanup") {
		t.Fatalf("stderr = %q, want dry-run side-effect warning", stderr.String())
	}
	if !strings.Contains(stderr.String(), "dry-run 校验通过") {
		t.Fatalf("stderr = %q, want dry-run success message", stderr.String())
	}
}

func TestRunCLIRejectsLegacyWorkflowArgument(t *testing.T) {
	restore := stubDependencies(t)
	defer restore()

	var stderr bytes.Buffer
	if exitCode := runCLI([]string{"./WORKFLOW.md"}, &stderr); exitCode == 0 {
		t.Fatalf("runCLI() exitCode = %d, want non-zero", exitCode)
	}
	if !strings.Contains(stderr.String(), "no longer supported") {
		t.Fatalf("stderr = %q, want legacy workflow rejection", stderr.String())
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

	var stderr bytes.Buffer
	if exitCode := runCLI(nil, &stderr); exitCode == 0 {
		t.Fatalf("runCLI() exitCode = %d, want non-zero", exitCode)
	}
	if !strings.Contains(stderr.String(), "missing_workflow_file") {
		t.Fatalf("stderr = %q, want missing_workflow_file", stderr.String())
	}
}

func TestRunCLIConfigDoctorReady(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "secret-key")
	configDir := filepath.Join(t.TempDir(), "automation")
	writeAutomationConfig(t, configDir, automationFixtureOptions{})

	var stderr bytes.Buffer
	if exitCode := runCLI([]string{"config", "doctor", "--config-dir", configDir}, &stderr); exitCode != 0 {
		t.Fatalf("runCLI() exitCode = %d, stderr = %s", exitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "配置已完整") {
		t.Fatalf("stderr = %q, want ready message", stderr.String())
	}
}

func TestRunCLIConfigDoctorReportsMissingSecret(t *testing.T) {
	configDir := filepath.Join(t.TempDir(), "automation")
	writeAutomationConfig(t, configDir, automationFixtureOptions{})
	if err := os.Unsetenv("LINEAR_API_KEY"); err != nil {
		t.Fatalf("Unsetenv() error = %v", err)
	}

	var stderr bytes.Buffer
	if exitCode := runCLI([]string{"config", "doctor", "--config-dir", configDir}, &stderr); exitCode == 0 {
		t.Fatalf("runCLI() exitCode = %d, want non-zero", exitCode)
	}
	if !strings.Contains(stderr.String(), "missing required secrets") || !strings.Contains(stderr.String(), "LINEAR_API_KEY") {
		t.Fatalf("stderr = %q, want missing secret diagnosis", stderr.String())
	}
}

func TestConfigSetWritesEnvLocalFromStdin(t *testing.T) {
	restore := stubDependencies(t)
	defer restore()

	configDir := filepath.Join(t.TempDir(), "automation")
	writeAutomationConfig(t, configDir, automationFixtureOptions{})
	stdinIsTerminal = func() bool { return false }

	var stderr bytes.Buffer
	cmd := newRootCommand(&stderr)
	cmd.SetIn(strings.NewReader("secret-key\n"))
	cmd.SetArgs([]string{"config", "set", "LINEAR_API_KEY", "--config-dir", configDir})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(configDir, "local", "env.local"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !strings.Contains(string(content), "LINEAR_API_KEY=secret-key") {
		t.Fatalf("env.local = %q, want written key", string(content))
	}
}

func TestSetupRunsWizardWhenSecretsMissing(t *testing.T) {
	restore := stubDependencies(t)
	defer restore()

	configDir := filepath.Join(t.TempDir(), "automation")
	writeAutomationConfig(t, configDir, automationFixtureOptions{})
	if err := os.Unsetenv("LINEAR_API_KEY"); err != nil {
		t.Fatalf("Unsetenv() error = %v", err)
	}

	wizardCalled := false
	runWizardFunc = func(diagnosis *config.ConfigDiagnosis, envLocalPath string, store *secret.Store) error {
		wizardCalled = true
		if err := envfile.Upsert(envLocalPath, "LINEAR_API_KEY", "wizard-secret"); err != nil {
			return err
		}
		return store.Set("LINEAR_API_KEY", "wizard-secret")
	}
	stdinIsTerminal = func() bool { return true }
	stdoutIsTerminal = func() bool { return true }

	var stderr bytes.Buffer
	if exitCode := runCLI([]string{"setup", "--config-dir", configDir}, &stderr); exitCode != 0 {
		t.Fatalf("runCLI() exitCode = %d, stderr = %s", exitCode, stderr.String())
	}
	if !wizardCalled {
		t.Fatal("wizard was not called")
	}
	if !strings.Contains(stderr.String(), "配置已完成") {
		t.Fatalf("stderr = %q, want setup completion message", stderr.String())
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

func TestExecuteStartsWatcherAndNotifiesReload(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "secret-key")
	var reloadCount int
	watchCalled := false
	restore := stubDependencies(t)
	defer restore()

	configDir := filepath.Join(t.TempDir(), "automation")
	writeAutomationConfig(t, configDir, automationFixtureOptions{})

	newOrchestratorFactory = func(_ tracker.Client, _ workspace.Manager, _ agent.Runner, _ func() *model.ServiceConfig, _ func() *model.WorkflowDefinition, _ *slog.Logger) orchestratorService {
		return &fakeOrchestrator{notifyReload: func(_ *model.WorkflowDefinition) { reloadCount++ }}
	}
	watchAutomationDefinition = func(ctx context.Context, dir string, profile string, onChange func(*model.AutomationDefinition), onError func(error)) error {
		watchCalled = true
		reloaded, err := loader.Load(dir, profile)
		if err != nil {
			return err
		}
		onChange(reloaded)
		go func() { <-ctx.Done() }()
		return nil
	}
	notifySignalContext = func(parent context.Context, _ ...os.Signal) (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithCancel(parent)
		go cancel()
		return ctx, func() {}
	}

	var stderr bytes.Buffer
	if err := execute([]string{"--config-dir", configDir}, &stderr); err != nil {
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

	newOrchestratorFactory = func(_ tracker.Client, _ workspace.Manager, _ agent.Runner, _ func() *model.ServiceConfig, _ func() *model.WorkflowDefinition, _ *slog.Logger) orchestratorService {
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
	newHTTPServerFactory = func(runtime orchestratorService, logger *slog.Logger, port int) (httpServer, error) {
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
	watchAutomationDefinition = func(context.Context, string, string, func(*model.AutomationDefinition), func(error)) error {
		return nil
	}
	notifySignalContext = func(parent context.Context, _ ...os.Signal) (context.Context, context.CancelFunc) {
		return signalCtx, func() {}
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- execute([]string{"--config-dir", configDir, "--port", "8080"}, io.Discard)
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

	newOrchestratorFactory = func(_ tracker.Client, _ workspace.Manager, _ agent.Runner, _ func() *model.ServiceConfig, _ func() *model.WorkflowDefinition, _ *slog.Logger) orchestratorService {
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
	newHTTPServerFactory = func(runtime orchestratorService, logger *slog.Logger, port int) (httpServer, error) {
		return &fakeHTTPServer{
			addr: "127.0.0.1:8081",
			shutdown: func(context.Context) error {
				close(shutdownCalled)
				return nil
			},
		}, nil
	}
	watchAutomationDefinition = func(context.Context, string, string, func(*model.AutomationDefinition), func(error)) error {
		return nil
	}
	notifySignalContext = func(parent context.Context, _ ...os.Signal) (context.Context, context.CancelFunc) {
		return signalCtx, func() {}
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- execute([]string{"--config-dir", configDir, "--port", "8081"}, io.Discard)
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

	newOrchestratorFactory = func(_ tracker.Client, _ workspace.Manager, _ agent.Runner, _ func() *model.ServiceConfig, _ func() *model.WorkflowDefinition, _ *slog.Logger) orchestratorService {
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
	newHTTPServerFactory = func(runtime orchestratorService, logger *slog.Logger, port int) (httpServer, error) {
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
	watchAutomationDefinition = func(context.Context, string, string, func(*model.AutomationDefinition), func(error)) error {
		return nil
	}
	notifySignalContext = func(parent context.Context, _ ...os.Signal) (context.Context, context.CancelFunc) {
		return signalCtx, func() {}
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- execute([]string{"--config-dir", configDir, "--port", "9090"}, io.Discard)
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
	origStdinIsTerminal := stdinIsTerminal
	origStdoutIsTerminal := stdoutIsTerminal
	origReadPassword := readPasswordInput
	origRunWizard := runWizardFunc

	loadEnvFile = envfile.Load
	loadAutomationDefinition = loader.Load
	resolveActiveWorkflow = loader.ResolveActiveWorkflow
	watchAutomationDefinition = loader.WatchWithErrors
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
	newOrchestratorFactory = func(_ tracker.Client, _ workspace.Manager, _ agent.Runner, _ func() *model.ServiceConfig, _ func() *model.WorkflowDefinition, _ *slog.Logger) orchestratorService {
		return &fakeOrchestrator{}
	}
	newHTTPServerFactory = func(runtime orchestratorService, logger *slog.Logger, port int) (httpServer, error) {
		return &fakeHTTPServer{addr: "127.0.0.1:0"}, nil
	}
	notifySignalContext = func(parent context.Context, _ ...os.Signal) (context.Context, context.CancelFunc) {
		return context.WithCancel(parent)
	}
	stdinIsTerminal = func() bool { return false }
	stdoutIsTerminal = func() bool { return false }
	readPasswordInput = func() ([]byte, error) { return []byte(""), nil }
	runWizardFunc = runWizard

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
		stdinIsTerminal = origStdinIsTerminal
		stdoutIsTerminal = origStdoutIsTerminal
		readPasswordInput = origReadPassword
		runWizardFunc = origRunWizard
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
	runOnce      func(context.Context, bool)
	notifyReload func(*model.WorkflowDefinition)
	start        func(context.Context) error
	wait         func()
	snapshot     orchestrator.Snapshot
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

func (f *fakeOrchestrator) RequestRefresh()                 {}
func (f *fakeOrchestrator) Snapshot() orchestrator.Snapshot { return f.snapshot }
func (f *fakeOrchestrator) SubscribeSnapshots(buffer int) (<-chan orchestrator.Snapshot, func()) {
	ch := make(chan orchestrator.Snapshot, max(1, buffer))
	ch <- f.snapshot
	return ch, func() { close(ch) }
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

func writeFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
}
