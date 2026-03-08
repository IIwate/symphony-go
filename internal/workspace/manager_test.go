package workspace

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"symphony-go/internal/model"
)

func TestCreateForIssueCreatesAndReusesWorkspace(t *testing.T) {
	runner := &fakeRunner{}
	manager := newTestManager(t, runner)
	manager.currentConfig().HookAfterCreate = stringPtr("setup")

	workspace1, err := manager.CreateForIssue(context.Background(), "ABC/1")
	if err != nil {
		t.Fatalf("CreateForIssue() error = %v", err)
	}
	if !workspace1.CreatedNow {
		t.Fatal("CreatedNow = false, want true")
	}
	if workspace1.WorkspaceKey != "ABC_1" {
		t.Fatalf("WorkspaceKey = %q, want ABC_1", workspace1.WorkspaceKey)
	}

	workspace2, err := manager.CreateForIssue(context.Background(), "ABC/1")
	if err != nil {
		t.Fatalf("CreateForIssue() second error = %v", err)
	}
	if workspace2.CreatedNow {
		t.Fatal("CreatedNow = true, want false")
	}
	if got := runner.callCount("setup"); got != 1 {
		t.Fatalf("after_create call count = %d, want 1", got)
	}
}

func TestCreateForIssueRejectsEscapeAndFileConflict(t *testing.T) {
	manager := newTestManager(t, &fakeRunner{})

	if _, err := manager.CreateForIssue(context.Background(), ".."); !errors.Is(err, model.ErrWorkspacePathEscape) {
		t.Fatalf("CreateForIssue(..) error = %v, want ErrWorkspacePathEscape", err)
	}

	if err := os.MkdirAll(manager.workspaceRoot(), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	conflictPath := filepath.Join(manager.workspaceRoot(), "conflict")
	if err := os.WriteFile(conflictPath, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, err := manager.CreateForIssue(context.Background(), "conflict"); !errors.Is(err, model.ErrWorkspacePathConflict) {
		t.Fatalf("CreateForIssue(conflict) error = %v, want ErrWorkspacePathConflict", err)
	}
}

func TestCreateForIssueCleansUpOnAfterCreateFailure(t *testing.T) {
	runner := &fakeRunner{errorsByScript: map[string]error{"setup": errors.New("boom")}}
	manager := newTestManager(t, runner)
	manager.currentConfig().HookAfterCreate = stringPtr("setup")

	_, err := manager.CreateForIssue(context.Background(), "ABC-1")
	if !errors.Is(err, model.ErrWorkspaceHookFailed) {
		t.Fatalf("CreateForIssue() error = %v, want ErrWorkspaceHookFailed", err)
	}

	if _, statErr := os.Stat(filepath.Join(manager.workspaceRoot(), "ABC-1")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("workspace still exists, stat error = %v", statErr)
	}
}

func TestPrepareForRunCleansArtifactsAndFailsOnBeforeRun(t *testing.T) {
	runner := &fakeRunner{errorsByScript: map[string]error{"before": errors.New("bad hook")}}
	manager := newTestManager(t, runner)
	manager.currentConfig().HookBeforeRun = stringPtr("before")

	workspace, err := manager.CreateForIssue(context.Background(), "ABC-2")
	if err != nil {
		t.Fatalf("CreateForIssue() error = %v", err)
	}
	for _, name := range []string{"tmp", ".elixir_ls"} {
		if err := os.MkdirAll(filepath.Join(workspace.Path, name), 0o755); err != nil {
			t.Fatalf("MkdirAll() error = %v", err)
		}
	}

	err = manager.PrepareForRun(context.Background(), workspace)
	if !errors.Is(err, model.ErrWorkspaceHookFailed) {
		t.Fatalf("PrepareForRun() error = %v, want ErrWorkspaceHookFailed", err)
	}
	for _, name := range []string{"tmp", ".elixir_ls"} {
		if _, statErr := os.Stat(filepath.Join(workspace.Path, name)); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("artifact %q still exists, stat error = %v", name, statErr)
		}
	}
}

func TestPrepareForRunTimeout(t *testing.T) {
	runner := &fakeRunner{blockScript: "before"}
	manager := newTestManager(t, runner)
	manager.currentConfig().HookBeforeRun = stringPtr("before")
	manager.currentConfig().HookTimeoutMS = 50

	workspace, err := manager.CreateForIssue(context.Background(), "ABC-3")
	if err != nil {
		t.Fatalf("CreateForIssue() error = %v", err)
	}

	err = manager.PrepareForRun(context.Background(), workspace)
	if !errors.Is(err, model.ErrWorkspaceHookTimeout) {
		t.Fatalf("PrepareForRun() error = %v, want ErrWorkspaceHookTimeout", err)
	}
}

func TestPrepareForRunCreatesExpectedBranch(t *testing.T) {
	runner := &fakeRunner{
		stdoutByScript: map[string]string{
			"git config user.name":                                     "IIwate4268\n",
			"git branch --show-current":                                "main\n",
			"git for-each-ref refs/heads --format='%(refname:short)'":  "main\n",
			"git ls-remote --heads origin":                             "",
			"git switch -c iiwate4268/linear-symphony-smoke-iiwate-37": "",
		},
	}
	manager := newTestManager(t, runner)

	workspace, err := manager.CreateForIssue(context.Background(), "IIWATE-37")
	if err != nil {
		t.Fatalf("CreateForIssue() error = %v", err)
	}
	if err := os.MkdirAll(filepath.Join(workspace.Path, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.git) error = %v", err)
	}

	if err := manager.PrepareForRun(context.Background(), workspace); err != nil {
		t.Fatalf("PrepareForRun() error = %v", err)
	}
	if got := runner.callCount("git switch -c iiwate4268/linear-symphony-smoke-iiwate-37"); got != 1 {
		t.Fatalf("branch create call count = %d, want 1", got)
	}
}

func TestPrepareForRunAddsSuffixWhenRemoteBranchExists(t *testing.T) {
	runner := &fakeRunner{
		stdoutByScript: map[string]string{
			"git config user.name":                                       "IIwate4268\n",
			"git branch --show-current":                                  "main\n",
			"git for-each-ref refs/heads --format='%(refname:short)'":    "main\n",
			"git ls-remote --heads origin":                               "abc\trefs/heads/iiwate4268/linear-symphony-smoke-iiwate-37\n",
			"git switch -c iiwate4268/linear-symphony-smoke-iiwate-37-2": "",
		},
	}
	manager := newTestManager(t, runner)

	workspace, err := manager.CreateForIssue(context.Background(), "IIWATE-37")
	if err != nil {
		t.Fatalf("CreateForIssue() error = %v", err)
	}
	if err := os.MkdirAll(filepath.Join(workspace.Path, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.git) error = %v", err)
	}

	if err := manager.PrepareForRun(context.Background(), workspace); err != nil {
		t.Fatalf("PrepareForRun() error = %v", err)
	}
	if got := runner.callCount("git switch -c iiwate4268/linear-symphony-smoke-iiwate-37-2"); got != 1 {
		t.Fatalf("branch create call count = %d, want 1", got)
	}
}

func TestPrepareForRunUsesGitHubIssueNumberShortName(t *testing.T) {
	runner := &fakeRunner{
		stdoutByScript: map[string]string{
			"git config user.name":                                    "IIwate4268\n",
			"git branch --show-current":                               "main\n",
			"git for-each-ref refs/heads --format='%(refname:short)'": "main\n",
			"git ls-remote --heads origin":                            "",
			"git switch -c iiwate4268/github-linear-test-123":         "",
		},
	}
	manager := newTestManager(t, runner)
	manager.currentConfig().TrackerKind = "github"
	manager.currentConfig().TrackerRepo = "linear-test"

	workspace, err := manager.CreateForIssue(context.Background(), "IIwate/linear-test#123")
	if err != nil {
		t.Fatalf("CreateForIssue() error = %v", err)
	}
	if err := os.MkdirAll(filepath.Join(workspace.Path, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.git) error = %v", err)
	}

	if err := manager.PrepareForRun(context.Background(), workspace); err != nil {
		t.Fatalf("PrepareForRun() error = %v", err)
	}
	if got := runner.callCount("git switch -c iiwate4268/github-linear-test-123"); got != 1 {
		t.Fatalf("branch create call count = %d, want 1", got)
	}
}

func TestFinalizeRunAndCleanupIgnoreBestEffortHooks(t *testing.T) {
	runner := &fakeRunner{errorsByScript: map[string]error{
		"after":  errors.New("after failed"),
		"remove": errors.New("remove failed"),
	}}
	manager := newTestManager(t, runner)
	manager.currentConfig().HookAfterRun = stringPtr("after")
	manager.currentConfig().HookBeforeRemove = stringPtr("remove")

	workspace, err := manager.CreateForIssue(context.Background(), "ABC-4")
	if err != nil {
		t.Fatalf("CreateForIssue() error = %v", err)
	}

	manager.FinalizeRun(context.Background(), workspace)
	if err := manager.CleanupWorkspace(context.Background(), "ABC-4"); err != nil {
		t.Fatalf("CleanupWorkspace() error = %v", err)
	}
	if got := runner.callCount("after"); got != 1 {
		t.Fatalf("after_run call count = %d, want 1", got)
	}
	if got := runner.callCount("remove"); got != 1 {
		t.Fatalf("before_remove call count = %d, want 1", got)
	}
	if _, statErr := os.Stat(workspace.Path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("workspace still exists, stat error = %v", statErr)
	}
}

func newTestManager(t *testing.T, runner HookRunner) *LocalManager {
	t.Helper()

	manager, err := NewManager(&model.ServiceConfig{
		TrackerKind:                "linear",
		TrackerRepo:                "linear-test",
		WorkspaceRoot:              filepath.Join(t.TempDir(), "workspaces"),
		WorkspaceLinearBranchScope: "symphony-smoke",
		HookTimeoutMS:              200,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), runner)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}

	return manager
}

type fakeRunner struct {
	blockScript    string
	calledScripts  []string
	errorsByScript map[string]error
	stdoutByScript map[string]string
	stderrByScript map[string]string
}

func (f *fakeRunner) Run(ctx context.Context, _ string, script string) (string, string, error) {
	f.calledScripts = append(f.calledScripts, script)
	if script == f.blockScript {
		<-ctx.Done()
		return "", "", ctx.Err()
	}
	if err := f.errorsByScript[script]; err != nil {
		return f.stdoutByScript[script], coalesceString(f.stderrByScript[script], "stderr"), err
	}
	if stdout, ok := f.stdoutByScript[script]; ok {
		return stdout, f.stderrByScript[script], nil
	}

	return "stdout", "", nil
}

func (f *fakeRunner) callCount(script string) int {
	count := 0
	for _, item := range f.calledScripts {
		if item == script {
			count++
		}
	}
	return count
}

func coalesceString(value string, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func stringPtr(value string) *string {
	return &value
}
