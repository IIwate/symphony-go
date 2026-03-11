package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"symphony-go/internal/config"
	"symphony-go/internal/envfile"
)

func newConfigCommand() *cobra.Command {
	configCmd := &cobra.Command{
		Use:   "config",
		Short: "配置管理",
	}
	configCmd.AddCommand(&cobra.Command{
		Use:   "doctor",
		Short: "诊断配置完整性",
		Args:  cobra.NoArgs,
		RunE:  runDoctorCmd,
	})
	configCmd.AddCommand(&cobra.Command{
		Use:   "set KEY",
		Short: "设置 secret",
		Args:  cobra.ExactArgs(1),
		RunE:  runSetCmd,
	})
	return configCmd
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

func runSetCmd(cmd *cobra.Command, args []string) error {
	opts, err := readSharedOptions(cmd)
	if err != nil {
		return err
	}
	if err := validateLogLevel(opts.logLevel); err != nil {
		return err
	}

	key := strings.TrimSpace(args[0])
	repoDef, _, cfg, err := loadCommandConfig(opts.configDir, opts.profile)
	if err != nil {
		return err
	}
	allowed := config.ExtractRequiredEnvVars(repoDef, cfg)
	if !containsEnvKey(allowed, key) {
		sort.Strings(allowed)
		return fmt.Errorf("config key %s is not allowed; expected one of: %s", key, strings.Join(allowed, ", "))
	}

	value, err := readConfigValue(cmd, key, isSensitiveKey(key))
	if err != nil {
		return err
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("%s is required", key)
	}

	path := envLocalPath(opts.configDir)
	if err := envfile.Upsert(path, key, value); err != nil {
		return err
	}

	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "已写入 %s 到 %s，将在下次启动生效\n", key, path)
	return nil
}

func readConfigValue(cmd *cobra.Command, key string, sensitive bool) (string, error) {
	if !stdinIsTerminal() {
		data, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(data)), nil
	}

	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "请输入 %s: ", key); err != nil {
		return "", err
	}
	if sensitive {
		value, err := readPasswordInput()
		if _, printErr := fmt.Fprintln(cmd.OutOrStdout()); printErr != nil && err == nil {
			err = printErr
		}
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(value)), nil
	}

	reader := bufio.NewReader(cmd.InOrStdin())
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func containsEnvKey(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func isSensitiveKey(value string) bool {
	upper := strings.ToUpper(value)
	for _, token := range []string{"KEY", "TOKEN", "SECRET", "PASSWORD"} {
		if strings.Contains(upper, token) {
			return true
		}
	}
	return false
}
