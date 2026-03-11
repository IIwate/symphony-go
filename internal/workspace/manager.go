package workspace

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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

type branchBinding struct {
	Identifier string `json:"identifier"`
	Branch     string `json:"branch"`
}

var generateRunnerAlias = func() (string, error) {
	bytes := make([]byte, 4)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return "runner-" + strings.ToLower(hex.EncodeToString(bytes)), nil
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
			if cleanupErr := os.RemoveAll(workspace.Path); cleanupErr != nil {
				m.logger.Warn("cleanup workspace after after_create failure failed", "workspace_path", workspace.Path, "error", cleanupErr.Error())
			}
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
		path := filepath.Join(workspace.Path, name)
		if err := os.RemoveAll(path); err != nil {
			m.logger.Warn("cleanup workspace artifact failed", "workspace_path", workspace.Path, "artifact_path", path, "error", err.Error())
		}
	}

	cfg := m.currentConfig()
	if cfg.HookBeforeRun != nil {
		if err := m.runFatalHook(ctx, workspace.Path, "before_run", *cfg.HookBeforeRun); err != nil {
			return err
		}
	}

	if _, err := os.Stat(filepath.Join(workspace.Path, ".git")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			m.logger.Debug("skip workspace branch preparation", "workspace_path", workspace.Path, "reason", "git metadata missing")
			return nil
		}
		return model.NewWorkspaceError(model.ErrWorkspacePathConflict, fmt.Sprintf("stat git metadata in %q", workspace.Path), err)
	}

	if err := m.ensureWorkBranch(ctx, workspace); err != nil {
		return err
	}

	workspace.GitAuthorName, workspace.GitAuthorEmail = m.resolveCommitIdentity(ctx, workspace.Path, workspace.BranchNamespace)
	return nil
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
		return m.removeBranchBinding(workspace.WorkspaceKey)
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
	if err := m.removeBranchBinding(workspace.WorkspaceKey); err != nil {
		return model.NewWorkspaceError(model.ErrWorkspacePathConflict, fmt.Sprintf("remove branch binding for %q", workspace.WorkspaceKey), err)
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

	return &model.Workspace{Path: workspacePath, WorkspaceKey: workspaceKey, Identifier: strings.TrimSpace(identifier)}, nil
}

func (m *LocalManager) ensureWorkBranch(ctx context.Context, workspace *model.Workspace) error {
	identifier := strings.TrimSpace(workspace.Identifier)
	if identifier == "" {
		identifier = strings.TrimSpace(workspace.WorkspaceKey)
	}
	if identifier == "" {
		return model.NewWorkspaceError(model.ErrWorkspacePathConflict, "workspace identifier is empty", nil)
	}

	namespace := m.resolveBranchNamespace()
	if namespace == "" {
		namespace = "runner"
	}
	workspace.BranchNamespace = namespace

	cfg := m.currentConfig()
	issueShort := shortenIssueIdentifierForTracker(cfg, identifier)
	currentBranch, stderr, err := m.runCommand(ctx, workspace.Path, "git branch --show-current")
	if err != nil {
		return model.NewWorkspaceError(model.ErrWorkspaceHookFailed, fmt.Sprintf("detect current branch: %s", strings.TrimSpace(stderr)), err)
	}

	localBranches, stderr, err := m.listLocalBranches(ctx, workspace.Path)
	if err != nil {
		return model.NewWorkspaceError(model.ErrWorkspaceHookFailed, fmt.Sprintf("list local branches: %s", strings.TrimSpace(stderr)), err)
	}
	remoteBranches, stderr, err := m.listRemoteBranches(ctx, workspace.Path)
	if err != nil {
		return model.NewWorkspaceError(model.ErrWorkspaceHookFailed, fmt.Sprintf("list remote branches: %s", strings.TrimSpace(stderr)), err)
	}

	branchName, action, err := m.resolveWorkBranch(workspace, namespace, issueShort, strings.TrimSpace(currentBranch), localBranches, remoteBranches)
	if err != nil {
		return err
	}
	switch action {
	case "current":
		if err := m.saveBranchBinding(workspace.WorkspaceKey, workspace.Identifier, branchName); err != nil {
			return model.NewWorkspaceError(model.ErrWorkspacePathConflict, fmt.Sprintf("save branch binding for %q", workspace.WorkspaceKey), err)
		}
		return nil
	case "local":
		_, stderr, err = m.runCommand(ctx, workspace.Path, "git switch "+branchName)
	case "remote":
		_, stderr, err = m.runCommand(ctx, workspace.Path, "git switch -c "+branchName+" --track origin/"+branchName)
	case "recreate":
		_, stderr, err = m.runCommand(ctx, workspace.Path, "git switch -c "+branchName)
	case "create":
		_, stderr, err = m.runCommand(ctx, workspace.Path, "git switch -c "+branchName)
	default:
		return model.NewWorkspaceError(model.ErrWorkspaceHookFailed, fmt.Sprintf("unsupported branch action %q", action), nil)
	}
	if err != nil {
		return model.NewWorkspaceError(model.ErrWorkspaceHookFailed, fmt.Sprintf("prepare workspace branch %q: %s", branchName, strings.TrimSpace(stderr)), err)
	}
	if err := m.saveBranchBinding(workspace.WorkspaceKey, workspace.Identifier, branchName); err != nil {
		return model.NewWorkspaceError(model.ErrWorkspacePathConflict, fmt.Sprintf("save branch binding for %q", workspace.WorkspaceKey), err)
	}

	m.logger.Info("workspace branch prepared", "workspace_path", workspace.Path, "identifier", identifier, "branch", branchName, "created", action == "create" || action == "recreate")
	return nil
}

func (m *LocalManager) resolveBranchNamespace() string {
	cfg := m.currentConfig()
	explicitRaw := strings.TrimSpace(cfg.WorkspaceBranchNamespace)
	if explicitRaw != "" {
		if namespace := slugifyBranchPart(explicitRaw); namespace != "" {
			return namespace
		}
		m.logger.Warn("workspace branch namespace is invalid after normalization; falling back to runner alias", "raw_value", explicitRaw)
	}

	namespace, err := m.loadOrCreateRunnerAlias()
	if err != nil {
		m.logger.Warn("workspace runner alias fallback degraded to default namespace", "error", err.Error())
	}
	if namespace == "" {
		return "runner"
	}
	return namespace
}

func (m *LocalManager) loadOrCreateRunnerAlias() (string, error) {
	aliasPath := m.runnerAliasPath()
	if strings.TrimSpace(aliasPath) == "" {
		return "", fmt.Errorf("runner alias path is empty")
	}

	if content, err := os.ReadFile(aliasPath); err == nil {
		if alias := slugifyBranchPart(strings.TrimSpace(string(content))); alias != "" {
			return alias, nil
		}
		m.logger.Warn("runner alias file is invalid; regenerating", "path", aliasPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	alias, err := generateRunnerAlias()
	if err != nil {
		return "", err
	}
	alias = slugifyBranchPart(alias)
	if alias == "" {
		return "", fmt.Errorf("generated runner alias is empty")
	}
	if err := os.MkdirAll(filepath.Dir(aliasPath), 0o755); err != nil {
		return alias, err
	}
	if err := os.WriteFile(aliasPath, []byte(alias+"\n"), 0o644); err != nil {
		return alias, err
	}
	return alias, nil
}

func (m *LocalManager) runnerAliasPath() string {
	cfg := m.currentConfig()
	rootDir := strings.TrimSpace(cfg.AutomationRootDir)
	if rootDir == "" {
		return filepath.Join(m.workspaceRoot(), ".symphony-runner-alias")
	}
	return filepath.Join(rootDir, "local", "runner-alias")
}

func (m *LocalManager) resolveCommitIdentity(ctx context.Context, workspacePath string, branchNamespace string) (string, string) {
	cfg := m.currentConfig()
	if name, email, ok := completeIdentityPair(strings.TrimSpace(cfg.WorkspaceGitAuthorName), strings.TrimSpace(cfg.WorkspaceGitAuthorEmail)); ok {
		return name, email
	} else if incompleteIdentityPair(strings.TrimSpace(cfg.WorkspaceGitAuthorName), strings.TrimSpace(cfg.WorkspaceGitAuthorEmail)) {
		m.logger.Warn("workspace git author config is incomplete; falling back", "source", "explicit_config")
	}

	if name, email, ok := m.gitConfigIdentity(ctx, workspacePath, "repo_local", "git config --local --get user.name 2>/dev/null || true", "git config --local --get user.email 2>/dev/null || true"); ok {
		return name, email
	}
	if name, email, ok := m.gitConfigIdentity(ctx, workspacePath, "global", "git config --global --get user.name 2>/dev/null || true", "git config --global --get user.email 2>/dev/null || true"); ok {
		return name, email
	}

	fallbackNamespace := slugifyBranchPart(strings.TrimSpace(branchNamespace))
	if fallbackNamespace == "" {
		fallbackNamespace = "runner"
	}
	m.logger.Warn("workspace git author identity missing; injecting fallback identity for current run", "source", "fallback_alias", "branch_namespace", fallbackNamespace)
	return "symphony-runner", fallbackNamespace + "@symphony.invalid"
}

func (m *LocalManager) gitConfigIdentity(ctx context.Context, workspacePath string, source string, nameScript string, emailScript string) (string, string, bool) {
	name, _, err := m.runCommand(ctx, workspacePath, nameScript)
	if err != nil {
		m.logger.Warn("read git author name failed", "source", source, "error", err.Error())
		return "", "", false
	}
	email, _, err := m.runCommand(ctx, workspacePath, emailScript)
	if err != nil {
		m.logger.Warn("read git author email failed", "source", source, "error", err.Error())
		return "", "", false
	}

	if completeName, completeEmail, ok := completeIdentityPair(name, email); ok {
		return completeName, completeEmail, true
	}
	if incompleteIdentityPair(name, email) {
		m.logger.Warn("workspace git author identity is incomplete; falling back", "source", source)
	}
	return "", "", false
}

func completeIdentityPair(name string, email string) (string, string, bool) {
	name = strings.TrimSpace(name)
	email = strings.TrimSpace(email)
	if name == "" || email == "" {
		return "", "", false
	}
	return name, email, true
}

func incompleteIdentityPair(name string, email string) bool {
	name = strings.TrimSpace(name)
	email = strings.TrimSpace(email)
	return (name == "") != (email == "")
}

func (m *LocalManager) resolveWorkBranch(workspace *model.Workspace, namespace string, issueShort string, currentBranch string, localBranches map[string]struct{}, remoteBranches map[string]struct{}) (string, string, error) {
	if binding, ok, err := m.loadBranchBinding(workspace.WorkspaceKey); err != nil {
		return "", "", model.NewWorkspaceError(model.ErrWorkspacePathConflict, fmt.Sprintf("read branch binding for %q", workspace.WorkspaceKey), err)
	} else if ok {
		boundBranch := strings.TrimSpace(binding.Branch)
		switch {
		case boundBranch == "":
			m.logger.Warn("branch binding is empty; falling back to discovery", "workspace_key", workspace.WorkspaceKey)
		case currentBranch == boundBranch:
			return boundBranch, "current", nil
		case hasBranch(localBranches, boundBranch):
			return boundBranch, "local", nil
		case hasBranch(remoteBranches, boundBranch):
			return boundBranch, "remote", nil
		default:
			m.logger.Warn("bound branch is missing locally and remotely; recreating locally", "workspace_key", workspace.WorkspaceKey, "branch", boundBranch)
			return boundBranch, "recreate", nil
		}
	}

	candidates := discoverIssueBranches(issueShort, currentBranch, localBranches, remoteBranches)
	switch len(candidates) {
	case 0:
		branchName, createNew := chooseWorkBranch(namespace, issueShort, currentBranch, localBranches, remoteBranches)
		if !createNew && currentBranch == branchName {
			return branchName, "current", nil
		}
		if !createNew {
			return branchName, "local", nil
		}
		return branchName, "create", nil
	case 1:
		branchName := candidates[0]
		if currentBranch == branchName {
			return branchName, "current", nil
		}
		if hasBranch(localBranches, branchName) {
			return branchName, "local", nil
		}
		return branchName, "remote", nil
	default:
		return "", "", model.NewWorkspaceError(model.ErrWorkspaceHookFailed, fmt.Sprintf("multiple candidate branches found for %q: %s", issueShort, strings.Join(candidates, ", ")), nil)
	}
}

func discoverIssueBranches(issueShort string, currentBranch string, localBranches map[string]struct{}, remoteBranches map[string]struct{}) []string {
	if strings.TrimSpace(issueShort) == "" {
		return nil
	}

	candidates := make(map[string]struct{})
	collectCandidateBranch(candidates, currentBranch, issueShort)
	for branch := range localBranches {
		collectCandidateBranch(candidates, branch, issueShort)
	}
	for branch := range remoteBranches {
		collectCandidateBranch(candidates, branch, issueShort)
	}

	result := make([]string, 0, len(candidates))
	for branch := range candidates {
		result = append(result, branch)
	}
	sort.Strings(result)
	return result
}

func collectCandidateBranch(candidates map[string]struct{}, branch string, issueShort string) {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return
	}
	_, suffix, ok := strings.Cut(branch, "/")
	if !ok {
		return
	}
	if suffix == issueShort || strings.HasPrefix(suffix, issueShort+"-") {
		candidates[branch] = struct{}{}
	}
}

func hasBranch(branches map[string]struct{}, branch string) bool {
	_, ok := branches[strings.TrimSpace(branch)]
	return ok
}

func chooseWorkBranch(namespace string, issueShort string, currentBranch string, localBranches map[string]struct{}, remoteBranches map[string]struct{}) (string, bool) {
	base := buildBranchName(namespace, issueShort, "")
	if matchesWorkBranch(currentBranch, base) {
		return currentBranch, false
	}
	if _, ok := localBranches[base]; ok {
		return base, false
	}
	for suffix := 2; suffix < 1000; suffix++ {
		candidate := buildBranchName(namespace, issueShort, strconv.Itoa(suffix))
		if matchesWorkBranch(currentBranch, candidate) {
			return currentBranch, false
		}
		if _, ok := localBranches[candidate]; ok {
			return candidate, false
		}
		if _, ok := remoteBranches[base]; !ok {
			return base, true
		}
		if _, ok := remoteBranches[candidate]; !ok {
			return candidate, true
		}
	}
	return buildBranchName(namespace, issueShort, strconv.FormatInt(time.Now().Unix(), 10)), true
}

func (m *LocalManager) branchBindingPath(workspaceKey string) string {
	return filepath.Join(m.workspaceRoot(), ".symphony-branches", workspaceKey+".json")
}

func (m *LocalManager) loadBranchBinding(workspaceKey string) (*branchBinding, bool, error) {
	path := m.branchBindingPath(workspaceKey)
	content, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	var binding branchBinding
	if err := json.Unmarshal(content, &binding); err != nil {
		return nil, false, err
	}
	return &binding, true, nil
}

func (m *LocalManager) saveBranchBinding(workspaceKey string, identifier string, branch string) error {
	path := m.branchBindingPath(workspaceKey)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(branchBinding{
		Identifier: strings.TrimSpace(identifier),
		Branch:     strings.TrimSpace(branch),
	}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(payload, '\n'), 0o644)
}

func (m *LocalManager) removeBranchBinding(workspaceKey string) error {
	path := m.branchBindingPath(workspaceKey)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func buildBranchName(namespace string, issueShort string, suffix string) string {
	branch := namespace + "/" + issueShort
	if strings.TrimSpace(suffix) != "" {
		branch += "-" + suffix
	}
	if len(branch) <= 64 {
		return branch
	}

	maxNamespaceLen := 64 - 1 - len(issueShort)
	if strings.TrimSpace(suffix) != "" {
		maxNamespaceLen -= len(suffix) + 1
	}
	if maxNamespaceLen < 1 {
		maxNamespaceLen = 1
	}
	namespace = strings.Trim(namespace, "-")
	if len(namespace) > maxNamespaceLen {
		namespace = strings.Trim(namespace[:maxNamespaceLen], "-")
	}
	if namespace == "" {
		namespace = "w"
	}
	branch = namespace + "/" + issueShort
	if strings.TrimSpace(suffix) != "" {
		branch += "-" + suffix
	}
	if len(branch) > 64 {
		branch = branch[:64]
		branch = strings.TrimRight(branch, "-/")
	}
	return branch
}

func matchesWorkBranch(currentBranch string, expectedBase string) bool {
	branch := strings.TrimSpace(currentBranch)
	if branch == "" {
		return false
	}
	if branch == expectedBase {
		return true
	}
	return strings.HasPrefix(branch, expectedBase+"-")
}

func shortenIssueIdentifierForTracker(cfg *model.ServiceConfig, identifier string) string {
	normalizedKind := ""
	if cfg != nil {
		normalizedKind = model.NormalizeState(cfg.TrackerKind)
	}
	switch normalizedKind {
	case "linear":
		normalized := slugifyBranchPart(identifier)
		scope := ""
		if cfg != nil {
			scope = slugifyBranchPart(cfg.WorkspaceLinearBranchScope)
		}
		if normalized == "" {
			normalized = "issue"
		}
		if scope == "" {
			return "linear-" + normalized
		}
		return "linear-" + scope + "-" + normalized
	case "github":
		issueNumber := extractGitHubIssueNumber(identifier)
		repo := ""
		if cfg != nil {
			repo = slugifyBranchPart(cfg.TrackerRepo)
		}
		if issueNumber == "" {
			issueNumber = "issue"
		}
		if repo == "" {
			return "github-" + issueNumber
		}
		return "github-" + repo + "-" + issueNumber
	default:
		normalized := slugifyBranchPart(identifier)
		if normalized == "" {
			return "issue"
		}
		if len(normalized) > 24 {
			normalized = strings.Trim(normalized[:24], "-")
		}
		if normalized == "" {
			return "issue"
		}
		return normalized
	}
}

func extractGitHubIssueNumber(identifier string) string {
	trimmed := strings.TrimSpace(identifier)
	if trimmed == "" {
		return ""
	}
	if index := strings.LastIndex(trimmed, "#"); index >= 0 && index < len(trimmed)-1 {
		candidate := strings.TrimSpace(trimmed[index+1:])
		if isDigits(candidate) {
			return candidate
		}
	}
	normalized := slugifyBranchPart(trimmed)
	parts := strings.Split(normalized, "-")
	if len(parts) == 0 {
		return ""
	}
	last := parts[len(parts)-1]
	if isDigits(last) {
		return last
	}
	return ""
}

func slugifyBranchPart(value string) string {
	lower := strings.ToLower(strings.TrimSpace(value))
	if lower == "" {
		return ""
	}
	var builder strings.Builder
	lastDash := false
	for _, r := range lower {
		isAlpha := r >= 'a' && r <= 'z'
		isDigit := r >= '0' && r <= '9'
		if isAlpha || isDigit {
			builder.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			builder.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(builder.String(), "-")
}

func isDigits(value string) bool {
	if strings.TrimSpace(value) == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func (m *LocalManager) listLocalBranches(ctx context.Context, dir string) (map[string]struct{}, string, error) {
	stdout, stderr, err := m.runCommand(ctx, dir, "git for-each-ref refs/heads --format='%(refname:short)'")
	if err != nil {
		return nil, stderr, err
	}
	branches := make(map[string]struct{})
	for _, line := range strings.Split(strings.ReplaceAll(stdout, "\r\n", "\n"), "\n") {
		branch := strings.TrimSpace(line)
		if branch == "" {
			continue
		}
		branches[branch] = struct{}{}
	}
	return branches, stderr, nil
}

func (m *LocalManager) listRemoteBranches(ctx context.Context, dir string) (map[string]struct{}, string, error) {
	stdout, stderr, err := m.runCommand(ctx, dir, "git ls-remote --heads origin")
	if err != nil {
		return nil, stderr, err
	}
	branches := make(map[string]struct{})
	for _, line := range strings.Split(strings.ReplaceAll(stdout, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) != 2 {
			continue
		}
		const prefix = "refs/heads/"
		if strings.HasPrefix(parts[1], prefix) {
			branches[strings.TrimPrefix(parts[1], prefix)] = struct{}{}
		}
	}
	return branches, stderr, nil
}

func (m *LocalManager) runCommand(ctx context.Context, dir string, script string) (string, string, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, time.Duration(m.currentConfig().HookTimeoutMS)*time.Millisecond)
	defer cancel()

	stdout, stderr, err := m.runner.Run(cmdCtx, dir, script)
	if cmdCtx.Err() != nil {
		return stdout, stderr, cmdCtx.Err()
	}
	return stdout, stderr, err
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
