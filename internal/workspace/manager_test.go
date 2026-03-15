package workspace

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"symphony-go/internal/model"
	"symphony-go/internal/model/contract"
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

func TestPrepareForRunUsesContinuationHook(t *testing.T) {
	runner := &fakeRunner{}
	manager := newTestManager(t, runner)
	manager.currentConfig().HookBeforeRun = stringPtr("fresh")
	manager.currentConfig().HookBeforeRunContinuation = stringPtr("continue")

	workspace, err := manager.CreateForIssue(context.Background(), "ABC-3")
	if err != nil {
		t.Fatalf("CreateForIssue() error = %v", err)
	}
	workspace.CreatedNow = false
	workspace.Dispatch = &model.DispatchContext{
		Kind:            model.DispatchKindContinuation,
		ExpectedOutcome: model.CompletionModePullRequest,
	}

	if err := manager.PrepareForRun(context.Background(), workspace); err != nil {
		t.Fatalf("PrepareForRun() error = %v", err)
	}
	if got := runner.callCount("fresh"); got != 0 {
		t.Fatalf("fresh before_run call count = %d, want 0", got)
	}
	if got := runner.callCount("continue"); got != 1 {
		t.Fatalf("continuation hook call count = %d, want 1", got)
	}
}

func TestPrepareForRunContinuationWithoutHookSkipsFreshHook(t *testing.T) {
	runner := &fakeRunner{}
	manager := newTestManager(t, runner)
	manager.currentConfig().HookBeforeRun = stringPtr("fresh")

	workspace, err := manager.CreateForIssue(context.Background(), "ABC-3")
	if err != nil {
		t.Fatalf("CreateForIssue() error = %v", err)
	}
	workspace.CreatedNow = false
	workspace.Dispatch = &model.DispatchContext{
		Kind:            model.DispatchKindContinuation,
		ExpectedOutcome: model.CompletionModePullRequest,
	}

	if err := manager.PrepareForRun(context.Background(), workspace); err != nil {
		t.Fatalf("PrepareForRun() error = %v", err)
	}
	if got := runner.callCount("fresh"); got != 0 {
		t.Fatalf("fresh before_run call count = %d, want 0", got)
	}
}

func TestPrepareForRunContinuationNewWorkspaceFallsBackToFreshHook(t *testing.T) {
	runner := &fakeRunner{}
	manager := newTestManager(t, runner)
	manager.currentConfig().HookBeforeRun = stringPtr("fresh")

	workspace, err := manager.CreateForIssue(context.Background(), "ABC-3")
	if err != nil {
		t.Fatalf("CreateForIssue() error = %v", err)
	}
	workspace.Dispatch = &model.DispatchContext{
		Kind:            model.DispatchKindContinuation,
		ExpectedOutcome: model.CompletionModePullRequest,
	}

	if err := manager.PrepareForRun(context.Background(), workspace); err != nil {
		t.Fatalf("PrepareForRun() error = %v", err)
	}
	if got := runner.callCount("fresh"); got != 1 {
		t.Fatalf("fresh before_run call count = %d, want 1", got)
	}
}

func TestPrepareForRunUnhealthyContinuationWorkspaceAppliesRecoveryCheckpoint(t *testing.T) {
	runner := &fakeRunner{stdoutByScript: map[string]string{"git rev-parse HEAD": "def456\n"}}
	manager := newTestManager(t, runner)
	workspacePath := filepath.Join(manager.currentConfig().WorkspaceRoot, "ABC-1")
	if err := os.MkdirAll(filepath.Join(workspacePath, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.git) error = %v", err)
	}
	patchPath := filepath.Join(t.TempDir(), "checkpoint.patch")
	if err := os.WriteFile(patchPath, []byte("diff --git a/a.txt b/a.txt\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(patch) error = %v", err)
	}
	workspace := &model.Workspace{
		Path:         workspacePath,
		WorkspaceKey: "ABC-1",
		Identifier:   "ABC-1",
		CreatedNow:   false,
		Dispatch: &model.DispatchContext{
			Kind: model.DispatchKindContinuation,
			RecoveryCheckpoint: &model.RecoveryCheckpoint{
				PatchPath:     patchPath,
				WorkspacePath: workspacePath,
				Checkpoint: contract.Checkpoint{
					Type:        contract.CheckpointTypeRecovery,
					Summary:     "已记录恢复 checkpoint。",
					CapturedAt:  "2026-03-15T00:00:00Z",
					Stage:       contract.RunPhaseSummaryExecuting,
					ArtifactIDs: []string{"art-1"},
					BaseSHA:     "abc123",
				},
			},
		},
	}

	if err := manager.PrepareForRun(context.Background(), workspace); err != nil {
		t.Fatalf("PrepareForRun() error = %v", err)
	}

	applyScript := "git apply --3way --whitespace=nowarn '" + strings.ReplaceAll(patchPath, "\\", "/") + "'"
	if runner.callCount("git rev-parse HEAD") != 1 {
		t.Fatalf("health check call count = %d, want 1", runner.callCount("git rev-parse HEAD"))
	}
	if runner.callCount(applyScript) != 1 {
		t.Fatalf("apply recovery call count = %d, want 1", runner.callCount(applyScript))
	}
	if !workspace.CreatedNow {
		t.Fatal("workspace.CreatedNow = false, want true after unhealthy recovery reset")
	}
}

func TestPrepareForRunHealthyContinuationWorkspaceSkipsRecoveryCheckpointApply(t *testing.T) {
	patchBody := "diff --git a/a.txt b/a.txt\n"
	runner := &fakeRunner{stdoutByScript: map[string]string{
		"git rev-parse HEAD":  "abc123\n",
		recoveryPatchScript(): patchBody,
	}}
	manager := newTestManager(t, runner)
	workspacePath := filepath.Join(manager.currentConfig().WorkspaceRoot, "ABC-1")
	if err := os.MkdirAll(filepath.Join(workspacePath, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.git) error = %v", err)
	}
	patchPath := filepath.Join(t.TempDir(), "checkpoint.patch")
	if err := os.WriteFile(patchPath, []byte(patchBody), 0o644); err != nil {
		t.Fatalf("WriteFile(patch) error = %v", err)
	}
	workspace := &model.Workspace{
		Path:         workspacePath,
		WorkspaceKey: "ABC-1",
		Identifier:   "ABC-1",
		CreatedNow:   false,
		Dispatch: &model.DispatchContext{
			Kind: model.DispatchKindContinuation,
			RecoveryCheckpoint: &model.RecoveryCheckpoint{
				PatchPath:     patchPath,
				WorkspacePath: workspacePath,
				Checkpoint: contract.Checkpoint{
					Type:        contract.CheckpointTypeRecovery,
					Summary:     "已记录恢复 checkpoint。",
					CapturedAt:  "2026-03-15T00:00:00Z",
					Stage:       contract.RunPhaseSummaryExecuting,
					ArtifactIDs: []string{"art-1"},
					BaseSHA:     "abc123",
				},
			},
		},
	}

	if err := manager.PrepareForRun(context.Background(), workspace); err != nil {
		t.Fatalf("PrepareForRun() error = %v", err)
	}

	applyScript := "git apply --3way --whitespace=nowarn '" + strings.ReplaceAll(patchPath, "\\", "/") + "'"
	if runner.callCount(applyScript) != 0 {
		t.Fatalf("apply recovery call count = %d, want 0", runner.callCount(applyScript))
	}
	if workspace.CreatedNow {
		t.Fatal("workspace.CreatedNow = true, want false when health check passes")
	}
}

func TestPrepareForRunPassesDispatchEnvToHook(t *testing.T) {
	runner := &fakeRunner{}
	manager := newTestManager(t, runner)
	manager.currentConfig().HookBeforeRunContinuation = stringPtr("continue")
	reason := model.ContinuationReasonMissingPR
	branch := "runner/demo-1"

	workspace, err := manager.CreateForIssue(context.Background(), "ABC-3")
	if err != nil {
		t.Fatalf("CreateForIssue() error = %v", err)
	}
	workspace.CreatedNow = false
	workspace.Dispatch = &model.DispatchContext{
		Kind:            model.DispatchKindContinuation,
		ExpectedOutcome: model.CompletionModePullRequest,
		Reason:          &reason,
		PreviousBranch:  &branch,
		RetryAttempt:    intPtr(2),
	}

	if err := manager.PrepareForRun(context.Background(), workspace); err != nil {
		t.Fatalf("PrepareForRun() error = %v", err)
	}
	env := runner.lastEnv("continue")
	if env["SYMPHONY_DISPATCH_KIND"] != "continuation" {
		t.Fatalf("dispatch env = %+v", env)
	}
	if env["SYMPHONY_EXPECTED_OUTCOME"] != "pull_request" {
		t.Fatalf("dispatch env = %+v", env)
	}
	if env["SYMPHONY_CONTINUATION_REASON"] != "missing_pr" {
		t.Fatalf("dispatch env = %+v", env)
	}
	if env["SYMPHONY_PREVIOUS_BRANCH"] != "runner/demo-1" {
		t.Fatalf("dispatch env = %+v", env)
	}
	if env["SYMPHONY_RETRY_ATTEMPT"] != "2" {
		t.Fatalf("dispatch env = %+v", env)
	}
}

func TestPrepareForRunCreatesExpectedBranch(t *testing.T) {
	runner := &fakeRunner{
		stdoutByScript: map[string]string{
			"git branch --show-current":                               "main\n",
			"git for-each-ref refs/heads --format='%(refname:short)'": "main\n",
			"git ls-remote --heads origin":                            "",
		},
	}
	manager := newTestManager(t, runner)
	manager.currentConfig().WorkspaceBranchNamespace = "testuser"
	manager.currentConfig().WorkspaceGitAuthorName = "commit-bot"
	manager.currentConfig().WorkspaceGitAuthorEmail = "commit-bot@symphony.invalid"
	runner.stdoutByScript["git switch -c "+bashSingleQuote("testuser/linear-demo-scope-demo-37")] = ""

	workspace, err := manager.CreateForIssue(context.Background(), "DEMO-37")
	if err != nil {
		t.Fatalf("CreateForIssue() error = %v", err)
	}
	if err := os.MkdirAll(filepath.Join(workspace.Path, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.git) error = %v", err)
	}

	if err := manager.PrepareForRun(context.Background(), workspace); err != nil {
		t.Fatalf("PrepareForRun() error = %v", err)
	}
	if got := runner.callCount("git switch -c " + bashSingleQuote("testuser/linear-demo-scope-demo-37")); got != 1 {
		t.Fatalf("branch create call count = %d, want 1", got)
	}
	if workspace.BranchNamespace != "testuser" {
		t.Fatalf("BranchNamespace = %q, want testuser", workspace.BranchNamespace)
	}
	if workspace.GitAuthorName != "commit-bot" {
		t.Fatalf("GitAuthorName = %q, want commit-bot", workspace.GitAuthorName)
	}
	if workspace.GitAuthorEmail != "commit-bot@symphony.invalid" {
		t.Fatalf("GitAuthorEmail = %q, want commit-bot@symphony.invalid", workspace.GitAuthorEmail)
	}
}

func TestPrepareForRunReusesUniqueRemoteBranchWhenNoBindingExists(t *testing.T) {
	runner := &fakeRunner{
		stdoutByScript: map[string]string{
			"git branch --show-current":                               "main\n",
			"git for-each-ref refs/heads --format='%(refname:short)'": "main\n",
			"git ls-remote --heads origin":                            "abc\trefs/heads/testuser/linear-demo-scope-demo-37\n",
			"git fetch origin " + bashSingleQuote("+refs/heads/testuser/linear-demo-scope-demo-37:refs/remotes/origin/testuser/linear-demo-scope-demo-37"):             "",
			"git switch -c " + bashSingleQuote("testuser/linear-demo-scope-demo-37") + " " + bashSingleQuote("refs/remotes/origin/testuser/linear-demo-scope-demo-37"): "",
		},
	}
	manager := newTestManager(t, runner)
	manager.currentConfig().WorkspaceBranchNamespace = "testuser"

	workspace, err := manager.CreateForIssue(context.Background(), "DEMO-37")
	if err != nil {
		t.Fatalf("CreateForIssue() error = %v", err)
	}
	if err := os.MkdirAll(filepath.Join(workspace.Path, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.git) error = %v", err)
	}

	if err := manager.PrepareForRun(context.Background(), workspace); err != nil {
		t.Fatalf("PrepareForRun() error = %v", err)
	}
	if got := runner.callCount("git fetch origin " + bashSingleQuote("+refs/heads/testuser/linear-demo-scope-demo-37:refs/remotes/origin/testuser/linear-demo-scope-demo-37")); got != 1 {
		t.Fatalf("remote branch fetch call count = %d, want 1", got)
	}
	if got := runner.callCount("git switch -c " + bashSingleQuote("testuser/linear-demo-scope-demo-37") + " " + bashSingleQuote("refs/remotes/origin/testuser/linear-demo-scope-demo-37")); got != 1 {
		t.Fatalf("remote branch reuse call count = %d, want 1", got)
	}
	if workspace.BranchNamespace != "testuser" {
		t.Fatalf("BranchNamespace = %q, want testuser", workspace.BranchNamespace)
	}
}

func TestPrepareForRunUsesGitHubIssueNumberShortName(t *testing.T) {
	runner := &fakeRunner{
		stdoutByScript: map[string]string{
			"git branch --show-current":                                           "main\n",
			"git for-each-ref refs/heads --format='%(refname:short)'":             "main\n",
			"git ls-remote --heads origin":                                        "",
			"git switch -c " + bashSingleQuote("testuser/github-linear-test-123"): "",
		},
	}
	manager := newTestManager(t, runner)
	manager.currentConfig().TrackerKind = "github"
	manager.currentConfig().TrackerRepo = "linear-test"
	manager.currentConfig().WorkspaceBranchNamespace = "testuser"

	workspace, err := manager.CreateForIssue(context.Background(), "test-org/linear-test#123")
	if err != nil {
		t.Fatalf("CreateForIssue() error = %v", err)
	}
	if err := os.MkdirAll(filepath.Join(workspace.Path, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.git) error = %v", err)
	}

	if err := manager.PrepareForRun(context.Background(), workspace); err != nil {
		t.Fatalf("PrepareForRun() error = %v", err)
	}
	if got := runner.callCount("git switch -c " + bashSingleQuote("testuser/github-linear-test-123")); got != 1 {
		t.Fatalf("branch create call count = %d, want 1", got)
	}
}

func TestPrepareForRunUsesBoundRemoteBranch(t *testing.T) {
	runner := &fakeRunner{
		stdoutByScript: map[string]string{
			"git branch --show-current":                               "main\n",
			"git for-each-ref refs/heads --format='%(refname:short)'": "main\n",
			"git ls-remote --heads origin":                            "abc\trefs/heads/legacy/linear-demo-scope-demo-37\n",
			"git fetch origin " + bashSingleQuote("+refs/heads/legacy/linear-demo-scope-demo-37:refs/remotes/origin/legacy/linear-demo-scope-demo-37"):             "",
			"git switch -c " + bashSingleQuote("legacy/linear-demo-scope-demo-37") + " " + bashSingleQuote("refs/remotes/origin/legacy/linear-demo-scope-demo-37"): "",
		},
	}
	manager := newTestManager(t, runner)
	manager.currentConfig().WorkspaceBranchNamespace = "newns"

	workspace, err := manager.CreateForIssue(context.Background(), "DEMO-37")
	if err != nil {
		t.Fatalf("CreateForIssue() error = %v", err)
	}
	if err := os.MkdirAll(filepath.Join(workspace.Path, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.git) error = %v", err)
	}
	if err := manager.saveBranchBinding(workspace.WorkspaceKey, workspace.Identifier, "legacy/linear-demo-scope-demo-37"); err != nil {
		t.Fatalf("saveBranchBinding() error = %v", err)
	}

	if err := manager.PrepareForRun(context.Background(), workspace); err != nil {
		t.Fatalf("PrepareForRun() error = %v", err)
	}
	if got := runner.callCount("git fetch origin " + bashSingleQuote("+refs/heads/legacy/linear-demo-scope-demo-37:refs/remotes/origin/legacy/linear-demo-scope-demo-37")); got != 1 {
		t.Fatalf("remote tracking fetch call count = %d, want 1", got)
	}
	if got := runner.callCount("git switch -c " + bashSingleQuote("legacy/linear-demo-scope-demo-37") + " " + bashSingleQuote("refs/remotes/origin/legacy/linear-demo-scope-demo-37")); got != 1 {
		t.Fatalf("remote tracking switch call count = %d, want 1", got)
	}
}

func TestPrepareForRunRecreatesBoundBranchWhenMissingLocallyAndRemotely(t *testing.T) {
	runner := &fakeRunner{
		stdoutByScript: map[string]string{
			"git branch --show-current":                                            "main\n",
			"git for-each-ref refs/heads --format='%(refname:short)'":              "main\n",
			"git ls-remote --heads origin":                                         "",
			"git switch -c " + bashSingleQuote("legacy/linear-demo-scope-demo-37"): "",
		},
	}
	manager := newTestManager(t, runner)
	manager.currentConfig().WorkspaceBranchNamespace = "newns"

	workspace, err := manager.CreateForIssue(context.Background(), "DEMO-37")
	if err != nil {
		t.Fatalf("CreateForIssue() error = %v", err)
	}
	if err := os.MkdirAll(filepath.Join(workspace.Path, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.git) error = %v", err)
	}
	if err := manager.saveBranchBinding(workspace.WorkspaceKey, workspace.Identifier, "legacy/linear-demo-scope-demo-37"); err != nil {
		t.Fatalf("saveBranchBinding() error = %v", err)
	}

	if err := manager.PrepareForRun(context.Background(), workspace); err != nil {
		t.Fatalf("PrepareForRun() error = %v", err)
	}
	if got := runner.callCount("git switch -c " + bashSingleQuote("legacy/linear-demo-scope-demo-37")); got != 1 {
		t.Fatalf("recreate bound branch call count = %d, want 1", got)
	}
}

func TestPrepareForRunDiscoversUniqueLegacyBranchWithoutBinding(t *testing.T) {
	runner := &fakeRunner{
		stdoutByScript: map[string]string{
			"git branch --show-current":                               "main\n",
			"git for-each-ref refs/heads --format='%(refname:short)'": "main\n",
			"git ls-remote --heads origin":                            "abc\trefs/heads/legacy/linear-demo-scope-demo-37\n",
			"git fetch origin " + bashSingleQuote("+refs/heads/legacy/linear-demo-scope-demo-37:refs/remotes/origin/legacy/linear-demo-scope-demo-37"):             "",
			"git switch -c " + bashSingleQuote("legacy/linear-demo-scope-demo-37") + " " + bashSingleQuote("refs/remotes/origin/legacy/linear-demo-scope-demo-37"): "",
		},
	}
	manager := newTestManager(t, runner)
	manager.currentConfig().WorkspaceBranchNamespace = "newns"

	workspace, err := manager.CreateForIssue(context.Background(), "DEMO-37")
	if err != nil {
		t.Fatalf("CreateForIssue() error = %v", err)
	}
	if err := os.MkdirAll(filepath.Join(workspace.Path, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.git) error = %v", err)
	}

	if err := manager.PrepareForRun(context.Background(), workspace); err != nil {
		t.Fatalf("PrepareForRun() error = %v", err)
	}
	if got := runner.callCount("git fetch origin " + bashSingleQuote("+refs/heads/legacy/linear-demo-scope-demo-37:refs/remotes/origin/legacy/linear-demo-scope-demo-37")); got != 1 {
		t.Fatalf("legacy remote fetch call count = %d, want 1", got)
	}
	if got := runner.callCount("git switch -c " + bashSingleQuote("legacy/linear-demo-scope-demo-37") + " " + bashSingleQuote("refs/remotes/origin/legacy/linear-demo-scope-demo-37")); got != 1 {
		t.Fatalf("legacy remote switch call count = %d, want 1", got)
	}
	binding, ok, err := manager.loadBranchBinding(workspace.WorkspaceKey)
	if err != nil {
		t.Fatalf("loadBranchBinding() error = %v", err)
	}
	if !ok || binding.Branch != "legacy/linear-demo-scope-demo-37" {
		t.Fatalf("binding = %+v, ok = %t, want legacy branch binding", binding, ok)
	}
}

func TestPrepareForRunIgnoresBindingFromDifferentIdentifier(t *testing.T) {
	runner := &fakeRunner{
		stdoutByScript: map[string]string{
			"git branch --show-current":                                           "main\n",
			"git for-each-ref refs/heads --format='%(refname:short)'":             "main\n",
			"git ls-remote --heads origin":                                        "",
			"git switch -c " + bashSingleQuote("newns/linear-demo-scope-demo-37"): "",
		},
	}
	manager := newTestManager(t, runner)
	manager.currentConfig().WorkspaceBranchNamespace = "newns"

	legacyWorkspace, err := manager.CreateForIssue(context.Background(), "DEMO/37")
	if err != nil {
		t.Fatalf("CreateForIssue() legacy error = %v", err)
	}
	if err := manager.saveBranchBinding(legacyWorkspace.WorkspaceKey, legacyWorkspace.Identifier, "legacy/linear-demo-scope-demo-37"); err != nil {
		t.Fatalf("saveBranchBinding() error = %v", err)
	}

	workspace, err := manager.CreateForIssue(context.Background(), "DEMO:37")
	if err != nil {
		t.Fatalf("CreateForIssue() error = %v", err)
	}
	if err := os.MkdirAll(filepath.Join(workspace.Path, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.git) error = %v", err)
	}

	if err := manager.PrepareForRun(context.Background(), workspace); err != nil {
		t.Fatalf("PrepareForRun() error = %v", err)
	}
	if got := runner.callCount("git switch -c " + bashSingleQuote("newns/linear-demo-scope-demo-37")); got != 1 {
		t.Fatalf("new branch create call count = %d, want 1", got)
	}
	if got := runner.callCount("git switch -c " + bashSingleQuote("legacy/linear-demo-scope-demo-37")); got != 0 {
		t.Fatalf("legacy branch recreate call count = %d, want 0", got)
	}

	binding, ok, err := manager.loadBranchBinding(workspace.WorkspaceKey)
	if err != nil {
		t.Fatalf("loadBranchBinding() error = %v", err)
	}
	if !ok || binding.Identifier != "DEMO:37" || binding.Branch != "newns/linear-demo-scope-demo-37" {
		t.Fatalf("binding = %+v, ok = %t, want DEMO:37 -> newns/linear-demo-scope-demo-37", binding, ok)
	}
}

func TestPrepareForRunFailsOnMultipleLegacyBranchCandidates(t *testing.T) {
	runner := &fakeRunner{
		stdoutByScript: map[string]string{
			"git branch --show-current":                               "main\n",
			"git for-each-ref refs/heads --format='%(refname:short)'": "main\n",
			"git ls-remote --heads origin": "abc\trefs/heads/legacy-a/linear-demo-scope-demo-37\n" +
				"def\trefs/heads/legacy-b/linear-demo-scope-demo-37\n",
		},
	}
	manager := newTestManager(t, runner)
	manager.currentConfig().WorkspaceBranchNamespace = "newns"

	workspace, err := manager.CreateForIssue(context.Background(), "DEMO-37")
	if err != nil {
		t.Fatalf("CreateForIssue() error = %v", err)
	}
	if err := os.MkdirAll(filepath.Join(workspace.Path, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.git) error = %v", err)
	}

	err = manager.PrepareForRun(context.Background(), workspace)
	if !errors.Is(err, model.ErrWorkspaceHookFailed) {
		t.Fatalf("PrepareForRun() error = %v, want ErrWorkspaceHookFailed", err)
	}
	if err == nil || !strings.Contains(err.Error(), "multiple candidate branches") {
		t.Fatalf("PrepareForRun() error = %v, want multiple candidate branches detail", err)
	}
}

func TestPrepareForRunQuotesBranchSwitchCommand(t *testing.T) {
	branchName := "legacy/linear-demo-scope-demo-37&whoami"
	runner := &fakeRunner{
		stdoutByScript: map[string]string{
			"git branch --show-current":                               "main\n",
			"git for-each-ref refs/heads --format='%(refname:short)'": "main\n",
			"git ls-remote --heads origin":                            "",
			"git switch -c " + bashSingleQuote(branchName):            "",
		},
	}
	manager := newTestManager(t, runner)
	manager.currentConfig().WorkspaceBranchNamespace = "newns"

	workspace, err := manager.CreateForIssue(context.Background(), "DEMO-37")
	if err != nil {
		t.Fatalf("CreateForIssue() error = %v", err)
	}
	if err := os.MkdirAll(filepath.Join(workspace.Path, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.git) error = %v", err)
	}
	if err := manager.saveBranchBinding(workspace.WorkspaceKey, workspace.Identifier, branchName); err != nil {
		t.Fatalf("saveBranchBinding() error = %v", err)
	}

	if err := manager.PrepareForRun(context.Background(), workspace); err != nil {
		t.Fatalf("PrepareForRun() error = %v", err)
	}
	if got := runner.callCount("git switch -c " + bashSingleQuote(branchName)); got != 1 {
		t.Fatalf("quoted branch recreate call count = %d, want 1", got)
	}
}

func TestPrepareForRunUsesStableRunnerAliasFallback(t *testing.T) {
	originalGenerateRunnerAlias := generateRunnerAlias
	generateRunnerAlias = func() (string, error) { return "runner-abc123", nil }
	t.Cleanup(func() { generateRunnerAlias = originalGenerateRunnerAlias })

	runner := &fakeRunner{
		stdoutByScript: map[string]string{
			"git branch --show-current":                                                   "main\n",
			"git for-each-ref refs/heads --format='%(refname:short)'":                     "main\n",
			"git ls-remote --heads origin":                                                "",
			"git switch -c " + bashSingleQuote("runner-abc123/linear-demo-scope-demo-37"): "",
			"git switch -c " + bashSingleQuote("runner-abc123/linear-demo-scope-demo-38"): "",
		},
	}
	manager := newTestManager(t, runner)

	workspace1, err := manager.CreateForIssue(context.Background(), "DEMO-37")
	if err != nil {
		t.Fatalf("CreateForIssue() error = %v", err)
	}
	if err := os.MkdirAll(filepath.Join(workspace1.Path, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.git) error = %v", err)
	}
	if err := manager.PrepareForRun(context.Background(), workspace1); err != nil {
		t.Fatalf("PrepareForRun() error = %v", err)
	}

	generateRunnerAlias = func() (string, error) { return "runner-zzz999", nil }
	workspace2, err := manager.CreateForIssue(context.Background(), "DEMO-38")
	if err != nil {
		t.Fatalf("CreateForIssue() second error = %v", err)
	}
	if err := os.MkdirAll(filepath.Join(workspace2.Path, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.git) second error = %v", err)
	}
	if err := manager.PrepareForRun(context.Background(), workspace2); err != nil {
		t.Fatalf("PrepareForRun() second error = %v", err)
	}

	if workspace1.BranchNamespace != "runner-abc123" || workspace2.BranchNamespace != "runner-abc123" {
		t.Fatalf("branch namespaces = %q, %q, want stable runner-abc123", workspace1.BranchNamespace, workspace2.BranchNamespace)
	}
	if workspace2.GitAuthorName != "symphony-runner" {
		t.Fatalf("GitAuthorName = %q, want symphony-runner", workspace2.GitAuthorName)
	}
	if workspace2.GitAuthorEmail != "runner-abc123@symphony.invalid" {
		t.Fatalf("GitAuthorEmail = %q, want runner-abc123@symphony.invalid", workspace2.GitAuthorEmail)
	}
}

func TestPrepareForRunUsesRepoLocalGitIdentity(t *testing.T) {
	runner := &fakeRunner{
		stdoutByScript: map[string]string{
			"git branch --show-current":                                              "main\n",
			"git for-each-ref refs/heads --format='%(refname:short)'":                "main\n",
			"git ls-remote --heads origin":                                           "",
			"git switch -c " + bashSingleQuote("testuser/linear-demo-scope-demo-37"): "",
			"git config --local --get user.name 2>/dev/null || true":                 "repo-user\n",
			"git config --local --get user.email 2>/dev/null || true":                "repo-user@example.com\n",
		},
	}
	manager := newTestManager(t, runner)
	manager.currentConfig().WorkspaceBranchNamespace = "testuser"

	workspace, err := manager.CreateForIssue(context.Background(), "DEMO-37")
	if err != nil {
		t.Fatalf("CreateForIssue() error = %v", err)
	}
	if err := os.MkdirAll(filepath.Join(workspace.Path, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.git) error = %v", err)
	}
	if err := manager.PrepareForRun(context.Background(), workspace); err != nil {
		t.Fatalf("PrepareForRun() error = %v", err)
	}
	if workspace.GitAuthorName != "repo-user" {
		t.Fatalf("GitAuthorName = %q, want repo-user", workspace.GitAuthorName)
	}
	if workspace.GitAuthorEmail != "repo-user@example.com" {
		t.Fatalf("GitAuthorEmail = %q, want repo-user@example.com", workspace.GitAuthorEmail)
	}
}

func TestPrepareForRunIgnoresHalfConfiguredIdentityAndFallsBack(t *testing.T) {
	runner := &fakeRunner{
		stdoutByScript: map[string]string{
			"git branch --show-current":                                              "main\n",
			"git for-each-ref refs/heads --format='%(refname:short)'":                "main\n",
			"git ls-remote --heads origin":                                           "",
			"git switch -c " + bashSingleQuote("testuser/linear-demo-scope-demo-37"): "",
		},
	}
	manager := newTestManager(t, runner)
	manager.currentConfig().WorkspaceBranchNamespace = "testuser"
	manager.currentConfig().WorkspaceGitAuthorName = "bot-only"
	manager.currentConfig().WorkspaceGitAuthorEmail = "   "

	workspace, err := manager.CreateForIssue(context.Background(), "DEMO-37")
	if err != nil {
		t.Fatalf("CreateForIssue() error = %v", err)
	}
	if err := os.MkdirAll(filepath.Join(workspace.Path, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.git) error = %v", err)
	}
	if err := manager.PrepareForRun(context.Background(), workspace); err != nil {
		t.Fatalf("PrepareForRun() error = %v", err)
	}
	if workspace.GitAuthorName != "symphony-runner" {
		t.Fatalf("GitAuthorName = %q, want fallback symphony-runner", workspace.GitAuthorName)
	}
	if workspace.GitAuthorEmail != "testuser@symphony.invalid" {
		t.Fatalf("GitAuthorEmail = %q, want fallback testuser@symphony.invalid", workspace.GitAuthorEmail)
	}
}

func TestCleanupWorkspaceRemovesBindingWhenWorkspaceMissing(t *testing.T) {
	manager := newTestManager(t, &fakeRunner{})
	if err := manager.saveBranchBinding("DEMO-37", "DEMO-37", "legacy/linear-demo-scope-demo-37"); err != nil {
		t.Fatalf("saveBranchBinding() error = %v", err)
	}

	if err := manager.CleanupWorkspace(context.Background(), "DEMO-37"); err != nil {
		t.Fatalf("CleanupWorkspace() error = %v", err)
	}
	if _, ok, err := manager.loadBranchBinding("DEMO-37"); err != nil {
		t.Fatalf("loadBranchBinding() error = %v", err)
	} else if ok {
		t.Fatal("branch binding still exists after cleanup")
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
		AutomationRootDir:          filepath.Join(t.TempDir(), "automation"),
		WorkspaceRoot:              filepath.Join(t.TempDir(), "workspaces"),
		WorkspaceLinearBranchScope: "demo-scope",
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
	envByScript    map[string]map[string]string
}

func (f *fakeRunner) Run(ctx context.Context, _ string, script string, env map[string]string) (string, string, error) {
	f.calledScripts = append(f.calledScripts, script)
	if len(env) > 0 {
		if f.envByScript == nil {
			f.envByScript = map[string]map[string]string{}
		}
		copied := make(map[string]string, len(env))
		for key, value := range env {
			copied[key] = value
		}
		f.envByScript[script] = copied
	}
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

	return "", "", nil
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

func (f *fakeRunner) lastEnv(script string) map[string]string {
	if f.envByScript == nil {
		return nil
	}
	return f.envByScript[script]
}

func intPtr(value int) *int {
	return &value
}

func stringPtr(value string) *string {
	return &value
}
