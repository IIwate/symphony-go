package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"symphony-go/internal/config"
)

func newDoctorCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "诊断运行配置是否可启动",
		Args:  cobra.NoArgs,
		RunE:  runDoctorCmd,
	}
	addSharedFlags(cmd)
	return cmd
}

func runDoctorCmd(cmd *cobra.Command, args []string) error {
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
	return diagnosis
}
