package main

import (
	"fmt"
	"os"
	"strings"

	"charm.land/huh/v2"
	"golang.org/x/term"

	"symphony-go/internal/config"
	"symphony-go/internal/envfile"
	"symphony-go/internal/secret"
)

var (
	stdinIsTerminal       = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }
	stdoutIsTerminal      = func() bool { return term.IsTerminal(int(os.Stdout.Fd())) }
	promptSingleValueFunc = promptSingleValue
	runWizardFunc         = runWizard
)

type wizardField struct {
	secret config.MissingSecret
	value  string
}

func isInteractive() bool {
	return stdinIsTerminal() && stdoutIsTerminal()
}

func newPromptInput(title string, description string, value *string, sensitive bool) *huh.Input {
	input := huh.NewInput().
		Title(title).
		Value(value)
	if strings.TrimSpace(description) != "" {
		input = input.Description(description)
	}
	if sensitive {
		input = input.EchoMode(huh.EchoModePassword)
	}
	return input
}

func promptSingleValue(title string, description string, sensitive bool) (string, error) {
	var value string
	if err := huh.NewForm(huh.NewGroup(newPromptInput(title, description, &value, sensitive))).Run(); err != nil {
		return "", err
	}
	return strings.TrimSpace(value), nil
}

func runWizard(diagnosis *config.ConfigDiagnosis, envLocalPath string, store *secret.Store) error {
	if diagnosis == nil || len(diagnosis.MissingSecrets) == 0 {
		return nil
	}
	if store == nil {
		store = secret.New()
	}

	fields := make([]*wizardField, 0, len(diagnosis.MissingSecrets))
	groups := make([]*huh.Group, 0, len(diagnosis.MissingSecrets))
	for _, missing := range diagnosis.MissingSecrets {
		field := &wizardField{secret: missing}
		fields = append(fields, field)
		groups = append(groups, huh.NewGroup(newPromptInput(missing.EnvVar, missing.Source, &field.value, missing.IsSensitive)))
	}

	_, _ = fmt.Fprintln(os.Stderr, "检测到以下密钥缺失，开始交互式配置")
	if err := huh.NewForm(groups...).Run(); err != nil {
		return err
	}

	pairs := make(map[string]string, len(fields))
	for _, field := range fields {
		value := strings.TrimSpace(field.value)
		if value == "" {
			return fmt.Errorf("%s is required", field.secret.EnvVar)
		}
		pairs[field.secret.EnvVar] = value
	}
	if err := envfile.UpsertMultiple(envLocalPath, pairs); err != nil {
		return err
	}
	for _, field := range fields {
		if err := store.Set(field.secret.EnvVar, pairs[field.secret.EnvVar]); err != nil {
			return err
		}
	}
	return nil
}
