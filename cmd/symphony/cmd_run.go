package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"symphony-go/internal/config"
	"symphony-go/internal/logging"
	"symphony-go/internal/model"
	"symphony-go/internal/orchestrator"
	"symphony-go/internal/runtimepolicy"
)

func newRunCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run",
		Short: "运行 Symphony 编排服务",
		Args:  cobra.NoArgs,
		RunE:  runRunCmd,
	}
	addSharedFlags(cmd)
	cmd.Flags().Int("port", -1, "HTTP server port")
	cmd.Flags().Bool("dry-run", false, "run a single validation cycle and exit")
	return cmd
}

func addSharedFlags(cmd *cobra.Command) {
	cmd.Flags().String("config-dir", "automation", "path to automation config directory")
	cmd.Flags().String("profile", "", "runtime profile name")
	cmd.Flags().String("log-level", "info", "debug/info/warn/error")
	cmd.Flags().String("log-file", "", "path to log file")
}

type sharedOptions struct {
	configDir string
	profile   string
	logLevel  string
	logFile   string
}

func runRunCmd(cmd *cobra.Command, args []string) error {
	opts, err := readSharedOptions(cmd)
	if err != nil {
		return err
	}
	port, err := cmd.Flags().GetInt("port")
	if err != nil {
		return err
	}
	dryRun, err := cmd.Flags().GetBool("dry-run")
	if err != nil {
		return err
	}
	if err := validateLogLevel(opts.logLevel); err != nil {
		return err
	}

	repoDef, definition, cfg, err := loadCommandConfig(opts.configDir, opts.profile)
	if err != nil {
		return err
	}
	if err := config.ValidateForDispatch(cfg); err != nil {
		diagnosis := config.DiagnoseConfig(cfg, repoDef)
		if !diagnosis.IsReady() {
			return diagnosis
		}
		return err
	}

	portOverride := applyPortOverride(cfg, port)
	logger, closer, err := newLoggerFactory(logging.Options{
		Level:    opts.logLevel,
		FilePath: opts.logFile,
		Stderr:   cmd.ErrOrStderr(),
	})
	if err != nil {
		return err
	}
	defer closer.Close()
	logger.Info("automation loaded", slog.String("config_dir", repoDef.RootDir))

	if dryRun {
		logger.Info("dry-run 校验通过")
		return nil
	}

	state := &runtimeState{
		repoDef:      repoDef,
		definition:   definition,
		config:       cfg,
		portOverride: portOverride,
		configDir:    repoDef.RootDir,
		profile:      repoDef.Profile,
	}
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
	orch := newOrchestratorFactory(trackerClient, workspaceManager, runner, state.CurrentConfig, state.CurrentWorkflow, state.CurrentIdentity, logger)

	ctx, cancel := notifySignalContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := watchAutomationDefinition(ctx, repoDef.RootDir, opts.profile, func(newRepoDef *model.AutomationDefinition) error {
		newDefinition, reloadErr := state.ApplyReload(newRepoDef)
		if reloadErr != nil {
			return reloadErr
		}
		orch.NotifyWorkflowReload(newDefinition)
		logger.Info("automation reloaded", slog.String("config_dir", newRepoDef.RootDir))
		return nil
	}, func(watchErr error) {
		if restartErr, ok := runtimepolicy.IsRestartRequired(watchErr); ok {
			logger.Warn("automation reload rejected", "field_path", restartErr.Decision.FieldPath, "error", watchErr.Error())
			return
		}
		logger.Warn("automation reload failed", "error", watchErr.Error())
	}); err != nil {
		return err
	}

	var httpSrv httpServer
	if cfg.ServerPort != nil {
		httpSrv, err = newHTTPServerFactory(orch, logger, cfg.ServerHost, *cfg.ServerPort)
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

func readSharedOptions(cmd *cobra.Command) (*sharedOptions, error) {
	configDir, err := cmd.Flags().GetString("config-dir")
	if err != nil {
		return nil, err
	}
	profile, err := cmd.Flags().GetString("profile")
	if err != nil {
		return nil, err
	}
	logLevel, err := cmd.Flags().GetString("log-level")
	if err != nil {
		return nil, err
	}
	logFile, err := cmd.Flags().GetString("log-file")
	if err != nil {
		return nil, err
	}

	return &sharedOptions{
		configDir: configDir,
		profile:   profile,
		logLevel:  logLevel,
		logFile:   logFile,
	}, nil
}

func loadCommandConfig(configDir string, profile string) (*model.AutomationDefinition, *model.WorkflowDefinition, *model.ServiceConfig, error) {
	if err := loadEnvFile(envLocalPath(configDir)); err != nil {
		return nil, nil, nil, err
	}

	repoDef, err := loadAutomationDefinition(configDir, profile)
	if err != nil {
		return nil, nil, nil, err
	}
	definition, err := resolveActiveWorkflow(repoDef)
	if err != nil {
		return nil, nil, nil, err
	}
	cfg, err := config.NewFromWorkflow(definition)
	if err != nil {
		return nil, nil, nil, err
	}

	return repoDef, definition, cfg, nil
}

func envLocalPath(configDir string) string {
	return filepath.Join(configDir, "local", "env.local")
}
