package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"symphony-go/internal/agent"
	"symphony-go/internal/config"
	"symphony-go/internal/logging"
	"symphony-go/internal/model"
	"symphony-go/internal/orchestrator"
	"symphony-go/internal/tracker"
	"symphony-go/internal/workflow"
	"symphony-go/internal/workspace"
)

type orchestratorService interface {
	Start(context.Context) error
	Wait()
	RunOnce(context.Context, bool)
	NotifyWorkflowReload(*model.WorkflowDefinition)
	RequestRefresh()
}

type runtimeState struct {
	mu           sync.RWMutex
	definition   *model.WorkflowDefinition
	config       *model.ServiceConfig
	portOverride *int
}

var (
	loadWorkflowDefinition  = workflow.Load
	watchWorkflowDefinition = workflow.WatchWithErrors
	newLoggerFactory        = logging.NewLogger
	newTrackerFactory       = func(configFn func() *model.ServiceConfig) (tracker.Client, error) {
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
	notifySignalContext = func(parent context.Context, signals ...os.Signal) (context.Context, context.CancelFunc) {
		return signal.NotifyContext(parent, signals...)
	}
)

func main() {
	os.Exit(runCLI(os.Args[1:], os.Stderr))
}

func runCLI(args []string, stderr io.Writer) int {
	if stderr == nil {
		stderr = os.Stderr
	}

	if err := execute(args, stderr); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}

	return 0
}

func execute(args []string, stderr io.Writer) error {
	flags := flag.NewFlagSet("symphony", flag.ContinueOnError)
	flags.SetOutput(stderr)

	port := -1
	dryRun := false
	logFile := ""
	logLevel := "info"

	flags.IntVar(&port, "port", -1, "HTTP server port")
	flags.BoolVar(&dryRun, "dry-run", false, "run a single validation cycle and exit")
	flags.StringVar(&logFile, "log-file", "", "path to log file")
	flags.StringVar(&logLevel, "log-level", "info", "debug/info/warn/error")

	if err := flags.Parse(args); err != nil {
		return err
	}
	if err := validateLogLevel(logLevel); err != nil {
		return err
	}

	remaining := flags.Args()
	if len(remaining) > 1 {
		return fmt.Errorf("expected at most one workflow path argument, got %d", len(remaining))
	}

	workflowPath := "./WORKFLOW.md"
	if len(remaining) == 1 {
		workflowPath = remaining[0]
	}

	definition, err := loadWorkflowDefinition(workflowPath)
	if err != nil {
		return err
	}

	cfg, err := config.NewFromWorkflow(definition)
	if err != nil {
		return err
	}
	portOverride := applyPortOverride(cfg, port)

	logger, closer, err := newLoggerFactory(logging.Options{Level: logLevel, FilePath: logFile, Stderr: stderr})
	if err != nil {
		return err
	}
	defer closer.Close()
	logger.Info("workflow loaded", slog.String("workflow_path", workflowPath))

	if err := config.ValidateForDispatch(cfg); err != nil {
		return err
	}

	state := &runtimeState{definition: definition, config: cfg, portOverride: portOverride}
	trackerClient, err := newTrackerFactory(state.CurrentConfig)
	if err != nil {
		return err
	}
	workspaceManager, err := newWorkspaceFactory(state.CurrentConfig, logger)
	if err != nil {
		return err
	}
	runner := newAgentRunnerFactory(state.CurrentConfig, logger)
	orch := newOrchestratorFactory(trackerClient, workspaceManager, runner, state.CurrentConfig, state.CurrentWorkflow, logger)

	if dryRun {
		orch.RunOnce(context.Background(), false)
		logger.Info("dry-run 校验通过")
		return nil
	}

	ctx, cancel := notifySignalContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := watchWorkflowDefinition(ctx, workflowPath, func(def *model.WorkflowDefinition) {
		if _, reloadErr := state.ApplyReload(def); reloadErr != nil {
			logger.Warn("workflow reload rejected", "error", reloadErr.Error())
			return
		}
		orch.NotifyWorkflowReload(def)
		logger.Info("workflow reloaded", slog.String("workflow_path", workflowPath))
	}, func(watchErr error) {
		logger.Warn("workflow reload failed", "error", watchErr.Error())
	}); err != nil {
		return err
	}

	if cfg.ServerPort != nil {
		logger.Warn("http server not implemented yet", "port", *cfg.ServerPort)
	}

	if err := orch.Start(ctx); err != nil {
		return err
	}
	logger.Info("symphony started", slog.String("workflow_path", workflowPath))
	orch.Wait()
	logger.Info("symphony stopped")
	return nil
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

func (s *runtimeState) ApplyReload(def *model.WorkflowDefinition) (*model.ServiceConfig, error) {
	newCfg, err := config.NewFromWorkflow(def)
	if err != nil {
		return nil, err
	}
	if s.portOverride != nil {
		port := *s.portOverride
		newCfg.ServerPort = &port
	}
	if err := config.ValidateForDispatch(newCfg); err != nil {
		return nil, err
	}

	s.mu.Lock()
	s.definition = def
	s.config = newCfg
	s.mu.Unlock()

	return newCfg, nil
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
