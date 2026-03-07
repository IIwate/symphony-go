package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"symphony-go/internal/agent"
	"symphony-go/internal/logging"
	"symphony-go/internal/model"
	"symphony-go/internal/orchestrator"
	"symphony-go/internal/tracker"
	"symphony-go/internal/workflow"
	"symphony-go/internal/workspace"
)

func TestRunCLIUsesDefaultWorkflowPath(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "secret-key")
	workingDir := t.TempDir()
	writeWorkflow(t, filepath.Join(workingDir, "WORKFLOW.md"))

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
	if !strings.Contains(stderr.String(), "dry-run 校验通过") {
		t.Fatalf("stderr = %q, want dry-run success message", stderr.String())
	}
}

func TestRunCLIFailsForMissingExplicitWorkflow(t *testing.T) {
	restore := stubDependencies(t)
	defer restore()

	var stderr bytes.Buffer
	if exitCode := runCLI([]string{"missing.md"}, &stderr); exitCode == 0 {
		t.Fatalf("runCLI() exitCode = %d, want non-zero", exitCode)
	}
	if !strings.Contains(stderr.String(), "missing_workflow_file") {
		t.Fatalf("stderr = %q, want missing_workflow_file", stderr.String())
	}
}

func TestRunCLIFailsWhenDefaultWorkflowMissing(t *testing.T) {
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

func TestRuntimeStateApplyReloadKeepsPortOverride(t *testing.T) {
	port := 8080
	state := &runtimeState{
		definition:   &model.WorkflowDefinition{},
		config:       &model.ServiceConfig{TrackerKind: "linear", TrackerAPIKey: "secret", TrackerProjectSlug: "demo", CodexCommand: "codex app-server"},
		portOverride: &port,
	}

	definition := &model.WorkflowDefinition{Config: map[string]any{
		"tracker": map[string]any{"kind": "linear", "api_key": "secret", "project_slug": "demo"},
		"codex":   map[string]any{"command": "codex app-server"},
	}}

	cfg, err := state.ApplyReload(definition)
	if err != nil {
		t.Fatalf("ApplyReload() error = %v", err)
	}
	if cfg.ServerPort == nil || *cfg.ServerPort != 8080 {
		t.Fatalf("ServerPort = %v, want 8080", cfg.ServerPort)
	}
}

func TestExecuteStartsWatcherAndNotifiesReload(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "secret-key")
	var reloadCount int
	watchCalled := false
	restore := stubDependencies(t)
	newOrchestratorFactory = func(_ tracker.Client, _ workspace.Manager, _ agent.Runner, _ func() *model.ServiceConfig, _ func() *model.WorkflowDefinition, _ *slog.Logger) orchestratorService {
		return &fakeOrchestrator{notifyReload: func(_ *model.WorkflowDefinition) { reloadCount++ }}
	}
	watchWorkflowDefinition = func(ctx context.Context, path string, onChange func(*model.WorkflowDefinition), onError func(error)) error {
		watchCalled = true
		valid := &model.WorkflowDefinition{Config: map[string]any{
			"tracker": map[string]any{"kind": "linear", "api_key": "$LINEAR_API_KEY", "project_slug": "demo"},
			"codex":   map[string]any{"command": "codex app-server"},
		}}
		onChange(valid)
		go func() {
			<-ctx.Done()
		}()
		return nil
	}
	notifySignalContext = func(parent context.Context, _ ...os.Signal) (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithCancel(parent)
		go func() {
			cancel()
		}()
		return ctx, func() {}
	}
	defer restore()

	workflowPath := filepath.Join(t.TempDir(), "WORKFLOW.md")
	writeWorkflow(t, workflowPath)

	var stderr bytes.Buffer
	if err := execute([]string{workflowPath}, &stderr); err != nil {
		t.Fatalf("execute() error = %v", err)
	}
	if !watchCalled {
		t.Fatal("watch workflow was not called")
	}
	if reloadCount != 1 {
		t.Fatalf("reloadCount = %d, want 1", reloadCount)
	}
}

func stubDependencies(t *testing.T) func() {
	t.Helper()
	origWatch := watchWorkflowDefinition
	origLogger := newLoggerFactory
	origTracker := newTrackerFactory
	origWorkspace := newWorkspaceFactory
	origRunner := newAgentRunnerFactory
	origOrchestrator := newOrchestratorFactory
	origHTTPServer := newHTTPServerFactory
	origNotify := notifySignalContext

	watchWorkflowDefinition = workflow.WatchWithErrors
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

	return func() {
		watchWorkflowDefinition = origWatch
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

type fakeHTTPServer struct{ addr string }

func (f *fakeHTTPServer) Addr() string                   { return f.addr }
func (f *fakeHTTPServer) Shutdown(context.Context) error { return nil }

func max(left int, right int) int {
	if left > right {
		return left
	}
	return right
}

func writeWorkflow(t *testing.T, path string) {
	t.Helper()

	content := `---
tracker:
  kind: linear
  api_key: $LINEAR_API_KEY
  project_slug: demo
codex:
  command: codex app-server
---

hello {{ issue.title }}
`

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
}
