package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"symphony-go/internal/model"
	"symphony-go/internal/shell"
)

type Manager interface {
	CreateForIssue(ctx context.Context, identifier string) (*model.Workspace, error)
	CleanupWorkspace(ctx context.Context, identifier string) error
}

type HookRunner interface {
	Run(ctx context.Context, dir string, script string) (string, string, error)
}

type LocalManager struct {
	configProvider func() *model.ServiceConfig
	logger         *slog.Logger
	runner         HookRunner
}

func NewManager(cfg *model.ServiceConfig, logger *slog.Logger, runner HookRunner) (*LocalManager, error) {
	return NewDynamicManager(func() *model.ServiceConfig { return cfg }, logger, runner)
}

func NewDynamicManager(configProvider func() *model.ServiceConfig, logger *slog.Logger, runner HookRunner) (*LocalManager, error) {
	if configProvider == nil {
		return nil, model.NewWorkspaceError(nil, "service config provider is nil", nil)
	}
	cfg := configProvider()
	if cfg == nil {
		return nil, model.NewWorkspaceError(nil, "service config is nil", nil)
	}

	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if runner == nil {
		runner = ShellRunner{}
	}

	return &LocalManager{configProvider: configProvider, logger: logger, runner: runner}, nil
}

func (m *LocalManager) CreateForIssue(ctx context.Context, identifier string) (*model.Workspace, error) {
	workspace, err := m.newWorkspace(identifier)
	if err != nil {
		return nil, err
	}

	info, statErr := os.Stat(workspace.Path)
	switch {
	case statErr == nil && !info.IsDir():
		return nil, model.NewWorkspaceError(model.ErrWorkspacePathConflict, fmt.Sprintf("workspace path %q is not a directory", workspace.Path), nil)
	case statErr == nil:
		workspace.CreatedNow = false
	case errors.Is(statErr, os.ErrNotExist):
		workspace.CreatedNow = true
		if err := os.MkdirAll(workspace.Path, 0o755); err != nil {
			return nil, model.NewWorkspaceError(model.ErrWorkspacePathConflict, fmt.Sprintf("create workspace %q", workspace.Path), err)
		}
	default:
		return nil, model.NewWorkspaceError(model.ErrWorkspacePathConflict, fmt.Sprintf("stat workspace %q", workspace.Path), statErr)
	}

	cfg := m.currentConfig()
	if workspace.CreatedNow && cfg.HookAfterCreate != nil {
		if err := m.runFatalHook(ctx, workspace.Path, "after_create", *cfg.HookAfterCreate); err != nil {
			_ = os.RemoveAll(workspace.Path)
			return nil, err
		}
	}

	return workspace, nil
}

func (m *LocalManager) PrepareForRun(ctx context.Context, workspace *model.Workspace) error {
	if workspace == nil {
		return model.NewWorkspaceError(model.ErrWorkspacePathConflict, "workspace is nil", nil)
	}

	for _, name := range []string{"tmp", ".elixir_ls"} {
		_ = os.RemoveAll(filepath.Join(workspace.Path, name))
	}

	cfg := m.currentConfig()
	if cfg.HookBeforeRun == nil {
		return nil
	}

	return m.runFatalHook(ctx, workspace.Path, "before_run", *cfg.HookBeforeRun)
}

func (m *LocalManager) FinalizeRun(ctx context.Context, workspace *model.Workspace) {
	cfg := m.currentConfig()
	if workspace == nil || cfg.HookAfterRun == nil {
		return
	}

	_ = m.runBestEffortHook(ctx, workspace.Path, "after_run", *cfg.HookAfterRun)
}

func (m *LocalManager) CleanupWorkspace(ctx context.Context, identifier string) error {
	workspace, err := m.newWorkspace(identifier)
	if err != nil {
		return err
	}

	info, statErr := os.Stat(workspace.Path)
	if errors.Is(statErr, os.ErrNotExist) {
		return nil
	}
	if statErr != nil {
		return model.NewWorkspaceError(model.ErrWorkspacePathConflict, fmt.Sprintf("stat workspace %q", workspace.Path), statErr)
	}
	if !info.IsDir() {
		return model.NewWorkspaceError(model.ErrWorkspacePathConflict, fmt.Sprintf("workspace path %q is not a directory", workspace.Path), nil)
	}

	cfg := m.currentConfig()
	if cfg.HookBeforeRemove != nil {
		_ = m.runBestEffortHook(ctx, workspace.Path, "before_remove", *cfg.HookBeforeRemove)
	}

	if err := os.RemoveAll(workspace.Path); err != nil {
		return model.NewWorkspaceError(model.ErrWorkspacePathConflict, fmt.Sprintf("remove workspace %q", workspace.Path), err)
	}

	return nil
}

func (m *LocalManager) newWorkspace(identifier string) (*model.Workspace, error) {
	workspaceKey := model.SanitizeWorkspaceKey(identifier)
	root := m.workspaceRoot()
	workspacePath := filepath.Join(root, workspaceKey)
	workspacePath, err := filepath.Abs(workspacePath)
	if err != nil {
		return nil, model.NewWorkspaceError(model.ErrWorkspacePathEscape, "resolve workspace path", err)
	}
	if err := ensureWithinRoot(root, workspacePath); err != nil {
		return nil, err
	}

	return &model.Workspace{Path: workspacePath, WorkspaceKey: workspaceKey}, nil
}

func (m *LocalManager) runFatalHook(ctx context.Context, dir string, hookName string, script string) error {
	stdout, stderr, err := m.runHook(ctx, dir, hookName, script)
	m.logHook(hookName, dir, stdout, stderr, err)
	if err == nil {
		return nil
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return model.NewWorkspaceError(model.ErrWorkspaceHookTimeout, fmt.Sprintf("hook %s timed out", hookName), err)
	}

	return model.NewWorkspaceError(model.ErrWorkspaceHookFailed, fmt.Sprintf("hook %s failed", hookName), err)
}

func (m *LocalManager) runBestEffortHook(ctx context.Context, dir string, hookName string, script string) error {
	stdout, stderr, err := m.runHook(ctx, dir, hookName, script)
	m.logHook(hookName, dir, stdout, stderr, err)
	return nil
}

func (m *LocalManager) runHook(ctx context.Context, dir string, hookName string, script string) (string, string, error) {
	m.logger.Debug("workspace hook start", "hook", hookName, "workspace_path", dir)
	hookCtx, cancel := context.WithTimeout(ctx, time.Duration(m.currentConfig().HookTimeoutMS)*time.Millisecond)
	defer cancel()

	stdout, stderr, err := m.runner.Run(hookCtx, dir, script)
	if hookCtx.Err() != nil {
		return stdout, stderr, hookCtx.Err()
	}

	return stdout, stderr, err
}

func (m *LocalManager) logHook(hookName string, dir string, stdout string, stderr string, err error) {
	attrs := []any{
		"hook", hookName,
		"workspace_path", dir,
		"stdout", truncateOutput(stdout),
		"stderr", truncateOutput(stderr),
	}
	if err != nil {
		m.logger.Warn("workspace hook completed", append(attrs, "error", err.Error())...)
		return
	}

	m.logger.Debug("workspace hook completed", attrs...)
}

func ensureWithinRoot(root string, path string) error {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return model.NewWorkspaceError(model.ErrWorkspacePathEscape, "compute workspace relative path", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return model.NewWorkspaceError(model.ErrWorkspacePathEscape, fmt.Sprintf("workspace path %q escapes root %q", path, root), nil)
	}

	return nil
}

func truncateOutput(value string) string {
	trimmed := strings.TrimSpace(value)
	if len(trimmed) <= 256 {
		return trimmed
	}

	return trimmed[:256] + "...(truncated)"
}

func (m *LocalManager) currentConfig() *model.ServiceConfig {
	if m.configProvider == nil {
		return &model.ServiceConfig{}
	}
	cfg := m.configProvider()
	if cfg == nil {
		return &model.ServiceConfig{}
	}
	return cfg
}

func (m *LocalManager) workspaceRoot() string {
	root, err := filepath.Abs(m.currentConfig().WorkspaceRoot)
	if err != nil {
		return filepath.Clean(m.currentConfig().WorkspaceRoot)
	}
	return filepath.Clean(root)
}

type ShellRunner struct{}

func (ShellRunner) Run(ctx context.Context, dir string, script string) (string, string, error) {
	cmd, err := shell.BashCommand(ctx, dir, script)
	if err != nil {
		return "", "", err
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err = cmd.Run()
	return stdout.String(), stderr.String(), err
}
