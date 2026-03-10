package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

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
	watchAutomationDefinition = loader.WatchWithErrors
	buildVersion              = "dev"
	newLoggerFactory          = logging.NewLogger
	newTrackerFactory         = func(configFn func() *model.ServiceConfig) (tracker.Client, error) {
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
	configDir := "automation"
	profile := ""

	flags.IntVar(&port, "port", -1, "HTTP server port")
	flags.BoolVar(&dryRun, "dry-run", false, "run a single validation cycle and exit")
	flags.StringVar(&logFile, "log-file", "", "path to log file")
	flags.StringVar(&logLevel, "log-level", "info", "debug/info/warn/error")
	flags.StringVar(&configDir, "config-dir", "automation", "path to automation config directory")
	flags.StringVar(&profile, "profile", "", "runtime profile name")

	if err := flags.Parse(args); err != nil {
		return err
	}
	if err := validateLogLevel(logLevel); err != nil {
		return err
	}

	remaining := flags.Args()
	if len(remaining) > 0 {
		return fmt.Errorf("workflow path argument is no longer supported; use --config-dir")
	}

	if err := loadEnvFile(filepath.Join(configDir, "local", "env.local")); err != nil {
		return err
	}

	repoDef, err := loadAutomationDefinition(configDir, profile)
	if err != nil {
		return err
	}
	definition, err := resolveActiveWorkflow(repoDef)
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
	logger.Info("automation loaded", slog.String("config_dir", repoDef.RootDir))

	if err := config.ValidateForDispatch(cfg); err != nil {
		return err
	}

	state := &runtimeState{repoDef: repoDef, definition: definition, config: cfg, portOverride: portOverride, configDir: repoDef.RootDir}
	orchestrator.BuildVersion = buildVersion
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
		logger.Warn("dry-run 仍会访问 tracker 并执行 startupCleanup，可能产生副作用", slog.String("config_dir", repoDef.RootDir))
		orch.RunOnce(context.Background(), false)
		logger.Info("dry-run 校验通过")
		return nil
	}

	ctx, cancel := notifySignalContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := watchAutomationDefinition(ctx, repoDef.RootDir, profile, func(newRepoDef *model.AutomationDefinition) {
		newDefinition, reloadErr := state.ApplyReload(newRepoDef)
		if reloadErr != nil {
			logger.Warn("automation reload rejected", "error", reloadErr.Error())
			return
		}
		orch.NotifyWorkflowReload(newDefinition)
		logger.Info("automation reloaded", slog.String("config_dir", newRepoDef.RootDir))
	}, func(watchErr error) {
		logger.Warn("automation reload failed", "error", watchErr.Error())
	}); err != nil {
		return err
	}

	var httpSrv httpServer
	if cfg.ServerPort != nil {
		httpSrv, err = newHTTPServerFactory(orch, logger, *cfg.ServerPort)
		if err != nil {
			return err
		}
		logger.Info("http server started", "addr", httpSrv.Addr())
	}

	if err := orch.Start(ctx); err != nil {
		if httpSrv != nil {
			shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancelShutdown()
			_ = httpSrv.Shutdown(shutdownCtx)
		}
		return err
	}
	logger.Info("symphony started", slog.String("config_dir", repoDef.RootDir))
	orch.Wait()
	if httpSrv != nil {
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancelShutdown()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			logger.Warn("http server shutdown failed", "error", err.Error())
		}
	}
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
