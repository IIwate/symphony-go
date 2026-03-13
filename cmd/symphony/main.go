package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"reflect"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"symphony-go/internal/agent"
	"symphony-go/internal/config"
	"symphony-go/internal/envfile"
	"symphony-go/internal/loader"
	"symphony-go/internal/logging"
	"symphony-go/internal/model"
	"symphony-go/internal/orchestrator"
	"symphony-go/internal/server"
	"symphony-go/internal/tracker"
	"symphony-go/internal/workspace"
)

type orchestratorService interface {
	server.RuntimeSource
	Start(context.Context) error
	Wait()
	RunOnce(context.Context, bool)
	NotifyWorkflowReload(*model.WorkflowDefinition)
	RequestRefresh() orchestrator.RefreshRequestResult
}

type httpServer interface {
	Addr() string
	Shutdown(context.Context) error
}

type runtimeState struct {
	mu           sync.RWMutex
	repoDef      *model.AutomationDefinition
	definition   *model.WorkflowDefinition
	config       *model.ServiceConfig
	portOverride *int
	configDir    string
	profile      string
}

var (
	loadEnvFile               = envfile.Load
	loadAutomationDefinition  = loader.Load
	resolveActiveWorkflow     = loader.ResolveActiveWorkflow
	watchAutomationDefinition = func(ctx context.Context, dir string, profile string, onChange func(*model.AutomationDefinition) error, onError func(error)) error {
		return loader.WatchWithErrors(ctx, dir, profile, onChange, onError)
	}
	buildVersion      = "dev"
	newLoggerFactory  = logging.NewLogger
	newTrackerFactory = func(configFn func() *model.ServiceConfig) (tracker.Client, error) {
		return tracker.NewDynamicClient(configFn, nil)
	}
	newWorkspaceFactory = func(configFn func() *model.ServiceConfig, logger *slog.Logger) (workspace.Manager, error) {
		return workspace.NewDynamicManager(configFn, logger, nil)
	}
	newAgentRunnerFactory = func(configFn func() *model.ServiceConfig, logger *slog.Logger) agent.Runner {
		return agent.NewRunner(configFn, logger, nil)
	}
	newOrchestratorFactory = func(trackerClient tracker.Client, workspaceManager workspace.Manager, runner agent.Runner, configFn func() *model.ServiceConfig, workflowFn func() *model.WorkflowDefinition, identityFn func() orchestrator.RuntimeIdentity, logger *slog.Logger) orchestratorService {
		return orchestrator.NewOrchestrator(trackerClient, workspaceManager, runner, configFn, workflowFn, identityFn, logger)
	}
	newHTTPServerFactory = func(runtime orchestratorService, logger *slog.Logger, port int) (httpServer, error) {
		return server.Start(runtime, logger, port)
	}
	notifySignalContext = func(parent context.Context, signals ...os.Signal) (context.Context, context.CancelFunc) {
		return signal.NotifyContext(parent, signals...)
	}
)

func main() {
	os.Exit(runCLI(os.Args[1:], os.Stdout, os.Stderr))
}

func runCLI(args []string, stdout io.Writer, stderr io.Writer) int {
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}

	if err := execute(args, stdout, stderr); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}

	return 0
}

func execute(args []string, stdout io.Writer, stderr io.Writer) error {
	rootCmd := newRootCommand(stdout, stderr)
	if args == nil {
		rootCmd.SetArgs([]string{})
	} else {
		rootCmd.SetArgs(args)
	}
	err := rootCmd.Execute()
	if err != nil && hasLegacyWorkflowArg(args) && strings.Contains(err.Error(), "unknown command") {
		return fmt.Errorf("workflow path argument is no longer supported; use --config-dir")
	}
	return err
}

func newRootCommand(stdout io.Writer, stderr io.Writer) *cobra.Command {
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	rootCmd := &cobra.Command{
		Use:   "symphony",
		Short: "Symphony automation runner",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("workflow path argument is no longer supported; use --config-dir")
			}
			return nil
		},
		RunE:          runRunCmd,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	rootCmd.SetIn(os.Stdin)
	rootCmd.SetOut(stdout)
	rootCmd.SetErr(stderr)

	rootCmd.PersistentFlags().String("config-dir", "automation", "path to automation config directory")
	rootCmd.PersistentFlags().String("profile", "", "runtime profile name")
	rootCmd.PersistentFlags().String("log-level", "info", "debug/info/warn/error")
	rootCmd.PersistentFlags().String("log-file", "", "path to log file")
	rootCmd.PersistentFlags().Bool("non-interactive", false, "disable interactive prompts and setup wizard")

	rootCmd.Flags().Int("port", -1, "HTTP server port")
	rootCmd.Flags().Bool("dry-run", false, "run a single validation cycle and exit")

	rootCmd.AddCommand(newSetupCommand())
	rootCmd.AddCommand(newConfigCommand())
	return rootCmd
}

func hasLegacyWorkflowArg(args []string) bool {
	for _, raw := range args {
		arg := strings.TrimSpace(raw)
		if arg == "" || strings.HasPrefix(arg, "-") {
			continue
		}
		if strings.ContainsAny(arg, `/\`) {
			return true
		}
		lower := strings.ToLower(arg)
		if strings.HasSuffix(lower, ".md") || strings.HasSuffix(lower, ".markdown") {
			return true
		}
	}
	return false
}

func (s *runtimeState) CurrentConfig() *model.ServiceConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.config
}

func (s *runtimeState) CurrentWorkflow() *model.WorkflowDefinition {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.definition
}

func (s *runtimeState) CurrentIdentity() orchestrator.RuntimeIdentity {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return runtimeIdentityForConfig(s.configDir, s.profile, s.repoDef, s.config)
}

func (s *runtimeState) ApplyReload(repoDef *model.AutomationDefinition) (*model.WorkflowDefinition, error) {
	newDefinition, err := resolveActiveWorkflow(repoDef)
	if err != nil {
		return nil, err
	}
	newCfg, err := config.NewFromWorkflow(newDefinition)
	if err != nil {
		return nil, err
	}
	if s.portOverride != nil {
		port := *s.portOverride
		newCfg.ServerPort = &port
	}

	s.mu.RLock()
	currentRepoDef := s.repoDef
	currentCfg := s.config
	s.mu.RUnlock()

	if reason := reloadRestartRequiredReason(currentRepoDef, repoDef, currentCfg, newCfg); reason != "" {
		return nil, fmt.Errorf("%s: restart required", reason)
	}
	if err := config.ValidateForDispatch(newCfg); err != nil {
		return nil, err
	}

	s.mu.Lock()
	s.repoDef = repoDef
	s.definition = newDefinition
	s.config = newCfg
	s.configDir = repoDef.RootDir
	s.profile = repoDef.Profile
	s.mu.Unlock()

	return newDefinition, nil
}

func applyPortOverride(cfg *model.ServiceConfig, port int) *int {
	if port < 0 {
		return nil
	}
	cfg.ServerPort = &port
	return &port
}

func validateLogLevel(value string) error {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "debug", "info", "warn", "error":
		return nil
	default:
		return fmt.Errorf("unsupported log level %q", value)
	}
}

func serverPortEqual(left *int, right *int) bool {
	switch {
	case left == nil && right == nil:
		return true
	case left == nil || right == nil:
		return false
	default:
		return *left == *right
	}
}

func enabledSourcesEqual(left *model.AutomationDefinition, right *model.AutomationDefinition) bool {
	leftSources := nilSafeSources(left)
	rightSources := nilSafeSources(right)
	if len(leftSources) != len(rightSources) {
		return false
	}
	for i := range leftSources {
		if leftSources[i] != rightSources[i] {
			return false
		}
	}
	return true
}

func reloadRestartRequiredReason(currentRepoDef *model.AutomationDefinition, newRepoDef *model.AutomationDefinition, currentCfg *model.ServiceConfig, newCfg *model.ServiceConfig) string {
	if currentRepoDef != nil && strings.TrimSpace(currentRepoDef.Profile) != strings.TrimSpace(newRepoDef.Profile) {
		return "profile selection changed"
	}
	if currentRepoDef != nil {
		switch {
		case selectedSourceKind(currentRepoDef) != selectedSourceKind(newRepoDef):
			return "source.kind changed"
		case !enabledSourcesEqual(currentRepoDef, newRepoDef):
			return "selection.enabled_sources changed"
		case strings.TrimSpace(currentRepoDef.Selection.DispatchFlow) != strings.TrimSpace(newRepoDef.Selection.DispatchFlow):
			return "selection.dispatch_flow changed"
		}
	}
	if currentCfg == nil || newCfg == nil {
		return ""
	}
	switch {
	case !serverPortEqual(currentCfg.ServerPort, newCfg.ServerPort):
		return "runtime.server.port changed"
	case currentCfg.TrackerKind != newCfg.TrackerKind:
		return "runtime.tracker.kind changed"
	case currentCfg.TrackerEndpoint != newCfg.TrackerEndpoint:
		return "runtime.tracker.endpoint changed"
	case currentCfg.TrackerAPIKey != newCfg.TrackerAPIKey:
		return "runtime.tracker.api_key changed"
	case currentCfg.TrackerProjectSlug != newCfg.TrackerProjectSlug:
		return "runtime.tracker.project changed"
	case currentCfg.TrackerLinearChildrenBlockParent != newCfg.TrackerLinearChildrenBlockParent:
		return "runtime.tracker.linear.children_block_parent changed"
	case currentCfg.TrackerRepo != newCfg.TrackerRepo:
		return "runtime.tracker.repo changed"
	case !reflect.DeepEqual(currentCfg.ActiveStates, newCfg.ActiveStates):
		return "runtime.tracker.active_states changed"
	case !reflect.DeepEqual(currentCfg.TerminalStates, newCfg.TerminalStates):
		return "runtime.tracker.terminal_states changed"
	case currentCfg.AutomationRootDir != newCfg.AutomationRootDir:
		return "runtime.automation_root changed"
	case currentCfg.WorkspaceRoot != newCfg.WorkspaceRoot:
		return "runtime.workspace.root changed"
	case currentCfg.WorkspaceLinearBranchScope != newCfg.WorkspaceLinearBranchScope:
		return "runtime.workspace.linear_branch_scope changed"
	case currentCfg.WorkspaceBranchNamespace != newCfg.WorkspaceBranchNamespace:
		return "runtime.workspace.branch_namespace changed"
	case currentCfg.WorkspaceGitAuthorName != newCfg.WorkspaceGitAuthorName:
		return "runtime.workspace.git.author_name changed"
	case currentCfg.WorkspaceGitAuthorEmail != newCfg.WorkspaceGitAuthorEmail:
		return "runtime.workspace.git.author_email changed"
	case !reflect.DeepEqual(currentCfg.HookAfterCreate, newCfg.HookAfterCreate):
		return "runtime.hooks.after_create changed"
	case !reflect.DeepEqual(currentCfg.HookBeforeRun, newCfg.HookBeforeRun):
		return "runtime.hooks.before_run changed"
	case !reflect.DeepEqual(currentCfg.HookBeforeRunContinuation, newCfg.HookBeforeRunContinuation):
		return "runtime.hooks.before_run_continuation changed"
	case !reflect.DeepEqual(currentCfg.HookAfterRun, newCfg.HookAfterRun):
		return "runtime.hooks.after_run changed"
	case !reflect.DeepEqual(currentCfg.HookBeforeRemove, newCfg.HookBeforeRemove):
		return "runtime.hooks.before_remove changed"
	case currentCfg.HookTimeoutMS != newCfg.HookTimeoutMS:
		return "runtime.hooks.timeout_ms changed"
	case currentCfg.MaxTurns != newCfg.MaxTurns:
		return "runtime.agent.max_turns changed"
	case currentCfg.MaxRetryBackoffMS != newCfg.MaxRetryBackoffMS:
		return "runtime.agent.max_retry_backoff_ms changed"
	case currentCfg.OrchestratorAutoCloseOnPR != newCfg.OrchestratorAutoCloseOnPR:
		return "runtime.orchestrator.auto_close_on_pr changed"
	case currentCfg.CodexCommand != newCfg.CodexCommand:
		return "runtime.codex.command changed"
	case currentCfg.CodexApprovalPolicy != newCfg.CodexApprovalPolicy:
		return "runtime.codex.approval_policy changed"
	case currentCfg.CodexThreadSandbox != newCfg.CodexThreadSandbox:
		return "runtime.codex.thread_sandbox changed"
	case currentCfg.CodexTurnSandboxPolicy != newCfg.CodexTurnSandboxPolicy:
		return "runtime.codex.turn_sandbox_policy changed"
	case currentCfg.CodexTurnTimeoutMS != newCfg.CodexTurnTimeoutMS:
		return "runtime.codex.turn_timeout_ms changed"
	case currentCfg.CodexReadTimeoutMS != newCfg.CodexReadTimeoutMS:
		return "runtime.codex.read_timeout_ms changed"
	case currentCfg.CodexStallTimeoutMS != newCfg.CodexStallTimeoutMS:
		return "runtime.codex.stall_timeout_ms changed"
	case !reflect.DeepEqual(currentCfg.SessionPersistence, newCfg.SessionPersistence):
		return "runtime.session_persistence changed"
	}
	return ""
}

func runtimeIdentityForConfig(configDir string, profile string, repoDef *model.AutomationDefinition, cfg *model.ServiceConfig) orchestrator.RuntimeIdentity {
	identity := orchestrator.RuntimeIdentity{
		Compatibility: orchestrator.RuntimeCompatibility{
			Profile: strings.TrimSpace(profile),
		},
		Descriptor: orchestrator.RuntimeDescriptor{
			ConfigRoot: configDir,
		},
	}
	if repoDef != nil {
		identity.Compatibility.ActiveSource = selectedSourceName(repoDef)
		identity.Compatibility.SourceKind = selectedSourceKind(repoDef)
		identity.Compatibility.FlowName = strings.TrimSpace(repoDef.Selection.DispatchFlow)
	}
	if cfg != nil {
		identity.Compatibility.TrackerKind = strings.TrimSpace(cfg.TrackerKind)
		identity.Compatibility.TrackerRepo = strings.TrimSpace(cfg.TrackerRepo)
		identity.Compatibility.TrackerProjectSlug = strings.TrimSpace(cfg.TrackerProjectSlug)
		identity.Descriptor.WorkspaceRoot = strings.TrimSpace(cfg.WorkspaceRoot)
		identity.Descriptor.SessionPersistenceKind = string(cfg.SessionPersistence.Kind)
		identity.Descriptor.SessionStatePath = strings.TrimSpace(cfg.SessionPersistence.File.Path)
	}
	return identity
}

func nilSafeSources(def *model.AutomationDefinition) []string {
	if def == nil {
		return nil
	}
	sources := append([]string(nil), def.Selection.EnabledSources...)
	for i := range sources {
		sources[i] = strings.TrimSpace(sources[i])
	}
	return sources
}

func selectedSourceKind(def *model.AutomationDefinition) string {
	if def == nil || len(def.Selection.EnabledSources) != 1 {
		return ""
	}
	sourceName := selectedSourceName(def)
	if sourceName == "" {
		return ""
	}
	sourceDef := def.Sources[sourceName]
	if sourceDef == nil {
		return ""
	}
	value, _ := sourceDef.Raw["kind"].(string)
	return model.NormalizeState(value)
}

func selectedSourceName(def *model.AutomationDefinition) string {
	if def == nil || len(def.Selection.EnabledSources) != 1 {
		return ""
	}
	return strings.TrimSpace(def.Selection.EnabledSources[0])
}
