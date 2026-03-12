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
	RequestRefresh()
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
	newOrchestratorFactory = func(trackerClient tracker.Client, workspaceManager workspace.Manager, runner agent.Runner, configFn func() *model.ServiceConfig, workflowFn func() *model.WorkflowDefinition, logger *slog.Logger) orchestratorService {
		return orchestrator.NewOrchestrator(trackerClient, workspaceManager, runner, configFn, workflowFn, logger)
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

	if currentRepoDef != nil {
		if strings.TrimSpace(currentRepoDef.Profile) != strings.TrimSpace(repoDef.Profile) {
			return nil, fmt.Errorf("profile selection changed: restart required")
		}
		if selectedSourceKind(currentRepoDef) != selectedSourceKind(repoDef) {
			return nil, fmt.Errorf("source.kind changed: restart required")
		}
		if !enabledSourcesEqual(currentRepoDef, repoDef) {
			return nil, fmt.Errorf("selection.enabled_sources changed: restart required")
		}
	}
	if currentCfg != nil {
		if currentCfg.WorkspaceRoot != newCfg.WorkspaceRoot {
			return nil, fmt.Errorf("runtime.workspace.root changed: restart required")
		}
		if !serverPortEqual(currentCfg.ServerPort, newCfg.ServerPort) {
			return nil, fmt.Errorf("runtime.server.port changed: restart required")
		}
		if !reflect.DeepEqual(currentCfg.SessionPersistence, newCfg.SessionPersistence) {
			return nil, fmt.Errorf("runtime.session_persistence changed: restart required")
		}
		if !reflect.DeepEqual(currentCfg.Notifications, newCfg.Notifications) {
			return nil, fmt.Errorf("runtime.notifications changed: restart required")
		}
	}
	if err := config.ValidateForDispatch(newCfg); err != nil {
		return nil, err
	}

	s.mu.Lock()
	s.repoDef = repoDef
	s.definition = newDefinition
	s.config = newCfg
	s.configDir = repoDef.RootDir
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
	sourceName := strings.TrimSpace(def.Selection.EnabledSources[0])
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
