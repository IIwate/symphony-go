package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"symphony-go/internal/config"
	"symphony-go/internal/secret"
)

func newSetupCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "setup",
		Short: "交互式配置向导",
		Args:  cobra.NoArgs,
		RunE:  runSetupCmd,
	}
}

func runSetupCmd(cmd *cobra.Command, args []string) error {
	opts, err := readSharedOptions(cmd)
	if err != nil {
		return err
	}
	if err := validateLogLevel(opts.logLevel); err != nil {
		return err
	}

	repoDef, _, cfg, err := loadCommandConfig(opts.configDir, opts.profile)
	if err != nil {
		return err
	}
	diagnosis := config.DiagnoseConfig(cfg, repoDef)
	if diagnosis.IsReady() {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "配置已完整")
		return nil
	}
	if !diagnosis.HasMissingSecrets() || len(diagnosis.OtherErrors) > 0 {
		return diagnosis
	}
	if opts.nonInteractive || !isInteractive() {
		return fmt.Errorf("setup requires an interactive terminal")
	}

	store := secret.New()
	if err := runWizardFunc(diagnosis, envLocalPath(opts.configDir), store, cmd.ErrOrStderr()); err != nil {
		return err
	}
	_, _, cfg, err = loadCommandConfig(opts.configDir, opts.profile)
	if err != nil {
		return err
	}
	if err := config.ValidateForDispatch(cfg); err != nil {
		return err
	}

	_, _ = fmt.Fprintln(cmd.OutOrStdout(), "配置已完成")
	return nil
}
