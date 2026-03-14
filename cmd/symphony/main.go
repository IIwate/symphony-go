package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
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
	"symphony-go/internal/runtimepolicy"
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
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	rootCmd.SetIn(os.Stdin)
	rootCmd.SetOut(stdout)
	rootCmd.SetErr(stderr)

	rootCmd.AddCommand(newRunCommand())
	rootCmd.AddCommand(newDoctorCommand())
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

	return runtimepolicy.BuildRuntimeIdentity(runtimepolicy.IdentityInput{
		ConfigDir:            s.configDir,
		Profile:              s.profile,
		AutomationDefinition: s.repoDef,
		ServiceConfig:        s.config,
	})
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

	decision := runtimepolicy.EvaluateReload(currentRepoDef, repoDef, currentCfg, newCfg)
	if decision.RequiresRestart() {
		return nil, runtimepolicy.NewRestartRequiredError(decision)
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
