package main

import (
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"symphony-go/internal/config"
	"symphony-go/internal/logging"
	"symphony-go/internal/workflow"
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

	definition, err := workflow.Load(workflowPath)
	if err != nil {
		return err
	}

	cfg, err := config.NewFromWorkflow(definition)
	if err != nil {
		return err
	}
	if port >= 0 {
		cfg.ServerPort = &port
	}
	logger, closer, err := logging.NewLogger(logging.Options{Level: logLevel, FilePath: logFile, Stderr: stderr})
	if err != nil {
		return err
	}
	defer closer.Close()
	logger.Info("workflow loaded", slog.String("workflow_path", workflowPath))
	if err := config.ValidateForDispatch(cfg); err != nil {
		return err
	}

	if dryRun {
		logger.Info("dry-run 校验通过")
		return nil
	}

	logger.Info("Cycle 2 基础设施层已开始，运行时编排将在后续周期接入。")
	return nil
}

func validateLogLevel(value string) error {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "debug", "info", "warn", "error":
		return nil
	default:
		return fmt.Errorf("unsupported log level %q", value)
	}
}
