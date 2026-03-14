package config

import (
	"fmt"
	"sort"
	"strings"

	"symphony-go/internal/model"
	"symphony-go/internal/secret"
)

type MissingSecret struct {
	EnvVar      string
	Source      string
	IsSensitive bool
}

type ConfigDiagnosis struct {
	MissingSecrets []MissingSecret
	OtherErrors    []error
}

func (d *ConfigDiagnosis) IsReady() bool {
	return d != nil && len(d.MissingSecrets) == 0 && len(d.OtherErrors) == 0
}

func (d *ConfigDiagnosis) HasMissingSecrets() bool {
	return d != nil && len(d.MissingSecrets) > 0
}

func (d *ConfigDiagnosis) Error() string {
	if d == nil || d.IsReady() {
		return ""
	}

	lines := make([]string, 0, len(d.MissingSecrets)+len(d.OtherErrors)+2)
	if len(d.MissingSecrets) > 0 {
		lines = append(lines, "missing required secrets:")
		for _, item := range d.MissingSecrets {
			lines = append(lines, fmt.Sprintf("- %s (%s)", item.EnvVar, item.Source))
		}
	}
	if len(d.OtherErrors) > 0 {
		lines = append(lines, "other configuration errors:")
		for _, err := range d.OtherErrors {
			lines = append(lines, "- "+err.Error())
		}
	}

	return strings.Join(lines, "\n")
}

func ExtractRequiredEnvVars(def *model.AutomationDefinition, cfg *model.ServiceConfig) []string {
	refs, _, _ := collectRequiredEnvRefs(def, cfg)
	result := make([]string, 0, len(refs))
	for _, ref := range refs {
		result = append(result, ref.EnvVar)
	}
	return result
}

func DiagnoseConfig(cfg *model.ServiceConfig, def *model.AutomationDefinition) *ConfigDiagnosis {
	diagnosis := &ConfigDiagnosis{}
	refs, fieldRefs, collectionErrs := collectRequiredEnvRefs(def, cfg)
	diagnosis.OtherErrors = append(diagnosis.OtherErrors, collectionErrs...)

	missingEnvVars := map[string]bool{}
	for _, ref := range refs {
		value, ok := secret.DefaultResolver(ref.EnvVar)
		if ok && strings.TrimSpace(value) != "" {
			continue
		}
		if missingEnvVars[ref.EnvVar] {
			continue
		}

		missingEnvVars[ref.EnvVar] = true
		diagnosis.MissingSecrets = append(diagnosis.MissingSecrets, MissingSecret{
			EnvVar:      ref.EnvVar,
			Source:      ref.Source,
			IsSensitive: isSensitiveEnvVar(ref.EnvVar),
		})
	}

	diagnosis.OtherErrors = append(diagnosis.OtherErrors, diagnoseStructuralErrors(cfg, fieldRefs, missingEnvVars)...)
	return diagnosis
}

type requiredEnvRef struct {
	EnvVar string
	Source string
}

func collectRequiredEnvRefs(def *model.AutomationDefinition, cfg *model.ServiceConfig) ([]requiredEnvRef, map[string]string, []error) {
	refs := make([]requiredEnvRef, 0)
	fieldRefs := map[string]string{}
	errorsList := make([]error, 0)

	if def != nil {
		collectRuntimeEnvRefs(def.Runtime, "runtime", "", &refs, fieldRefs)
	}
	sourceName, sourceRaw, err := activeSourceRaw(def)
	if err != nil {
		errorsList = append(errorsList, err)
	} else {
		collectSourceEnvRefs(sourceRaw, "source."+sourceName, "", &refs, fieldRefs)
	}

	refs = append(refs, collectHookEnvRefs(cfg)...)
	return uniqueRequiredEnvRefs(refs), fieldRefs, errorsList
}

func activeSourceRaw(def *model.AutomationDefinition) (string, map[string]any, error) {
	if def == nil {
		return "", nil, model.NewWorkflowError(model.ErrWorkflowParseError, "automation definition is nil", nil)
	}

	enabledSources := make([]string, 0, len(def.Selection.EnabledSources))
	for _, source := range def.Selection.EnabledSources {
		if trimmed := strings.TrimSpace(source); trimmed != "" {
			enabledSources = append(enabledSources, trimmed)
		}
	}
	if len(enabledSources) != 1 {
		return "", nil, model.NewWorkflowError(model.ErrWorkflowParseError, "selection.enabled_sources must contain exactly one source", nil)
	}

	sourceName := enabledSources[0]
	sourceDef, ok := def.Sources[sourceName]
	if !ok || sourceDef == nil {
		return "", nil, model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("selected source %q not found", sourceName), nil)
	}

	return sourceName, sourceDef.Raw, nil
}

func collectSourceEnvRefs(value any, sourceBase string, path string, refs *[]requiredEnvRef, fieldRefs map[string]string) {
	switch typed := value.(type) {
	case string:
		matches := envValuePattern.FindStringSubmatch(strings.TrimSpace(typed))
		if len(matches) != 2 {
			return
		}

		recordEnvRef(refs, fieldRefs, sourceBase, path, matches[1])
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			collectSourceEnvRefs(typed[key], sourceBase, joinNestedPath(path, key), refs, fieldRefs)
		}
	case map[any]any:
		keys := make([]string, 0, len(typed))
		values := make(map[string]any, len(typed))
		for key, nested := range typed {
			stringKey := fmt.Sprint(key)
			keys = append(keys, stringKey)
			values[stringKey] = nested
		}
		sort.Strings(keys)
		for _, key := range keys {
			collectSourceEnvRefs(values[key], sourceBase, joinNestedPath(path, key), refs, fieldRefs)
		}
	case []any:
		for index, nested := range typed {
			collectSourceEnvRefs(nested, sourceBase, joinSlicePath(path, index), refs, fieldRefs)
		}
	}
}

func collectRuntimeEnvRefs(value any, sourceBase string, path string, refs *[]requiredEnvRef, fieldRefs map[string]string) {
	switch typed := value.(type) {
	case string:
		matches := envValuePattern.FindStringSubmatch(strings.TrimSpace(typed))
		if len(matches) != 2 {
			return
		}

		recordEnvRef(refs, fieldRefs, sourceBase, path, matches[1])
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			collectRuntimeEnvRefs(typed[key], sourceBase, joinNestedPath(path, key), refs, fieldRefs)
		}
	case map[any]any:
		keys := make([]string, 0, len(typed))
		values := make(map[string]any, len(typed))
		for key, nested := range typed {
			stringKey := fmt.Sprint(key)
			keys = append(keys, stringKey)
			values[stringKey] = nested
		}
		sort.Strings(keys)
		for _, key := range keys {
			collectRuntimeEnvRefs(values[key], sourceBase, joinNestedPath(path, key), refs, fieldRefs)
		}
	case []any:
		for index, nested := range typed {
			collectRuntimeEnvRefs(nested, sourceBase, joinSlicePath(path, index), refs, fieldRefs)
		}
	}
}

func recordEnvRef(refs *[]requiredEnvRef, fieldRefs map[string]string, sourceBase string, path string, envVar string) {
	joinedPath := joinDiagnosisPath(sourceBase, path)
	*refs = append(*refs, requiredEnvRef{
		EnvVar: envVar,
		Source: joinedPath,
	})
	if path == "" {
		return
	}
	if _, exists := fieldRefs[joinedPath]; !exists {
		fieldRefs[joinedPath] = envVar
	}
	if logicalKey := logicalFieldRefKey(sourceBase, path); logicalKey != "" {
		if _, exists := fieldRefs[logicalKey]; !exists {
			fieldRefs[logicalKey] = envVar
		}
	}
}

func logicalFieldRefKey(sourceBase string, path string) string {
	switch {
	case sourceBase == "runtime":
		return joinDiagnosisPath(sourceBase, path)
	case strings.HasPrefix(sourceBase, "source.") && path != "" && !strings.Contains(path, ".") && !strings.Contains(path, "["):
		return "source." + path
	default:
		return ""
	}
}

func collectHookEnvRefs(cfg *model.ServiceConfig) []requiredEnvRef {
	if cfg == nil {
		return nil
	}

	refs := make([]requiredEnvRef, 0)
	for _, hook := range []struct {
		name   string
		script *string
	}{
		{name: "after_create", script: cfg.HookAfterCreate},
		{name: "before_run", script: cfg.HookBeforeRun},
		{name: "before_run_continuation", script: cfg.HookBeforeRunContinuation},
		{name: "after_run", script: cfg.HookAfterRun},
		{name: "before_remove", script: cfg.HookBeforeRemove},
	} {
		if hook.script == nil {
			continue
		}

		matches := requiredHookEnvPattern.FindAllStringSubmatch(*hook.script, -1)
		for _, match := range matches {
			if len(match) != 2 {
				continue
			}
			refs = append(refs, requiredEnvRef{
				EnvVar: match[1],
				Source: "hooks." + hook.name,
			})
		}
	}

	return refs
}

func uniqueRequiredEnvRefs(refs []requiredEnvRef) []requiredEnvRef {
	seen := map[string]bool{}
	result := make([]requiredEnvRef, 0, len(refs))
	for _, ref := range refs {
		if seen[ref.EnvVar] {
			continue
		}
		seen[ref.EnvVar] = true
		result = append(result, ref)
	}
	return result
}

func diagnoseStructuralErrors(cfg *model.ServiceConfig, fieldRefs map[string]string, missingEnvVars map[string]bool) []error {
	if cfg == nil {
		return []error{model.NewWorkflowError(model.ErrWorkflowParseError, "service config is nil", nil)}
	}

	errorsList := make([]error, 0)
	if strings.TrimSpace(cfg.TrackerKind) == "" {
		errorsList = append(errorsList, model.NewTrackerError(model.ErrUnsupportedTrackerKind, "tracker.kind is required", nil))
		return errorsList
	}
	if cfg.TrackerKind != "linear" {
		errorsList = append(errorsList, model.NewTrackerError(model.ErrUnsupportedTrackerKind, fmt.Sprintf("unsupported tracker.kind %q", cfg.TrackerKind), nil))
		return errorsList
	}
	if strings.TrimSpace(cfg.TrackerAPIKey) == "" && !fieldMissingBecauseSecret(fieldRefs, "source.api_key", missingEnvVars) {
		errorsList = append(errorsList, model.NewTrackerError(model.ErrMissingTrackerAPIKey, "tracker.api_key is required", nil))
	}
	if strings.TrimSpace(cfg.TrackerProjectSlug) == "" && !fieldMissingBecauseSecret(fieldRefs, "source.project_slug", missingEnvVars) {
		errorsList = append(errorsList, model.NewTrackerError(model.ErrMissingTrackerProjectSlug, "tracker.project_slug is required", nil))
	}
	if strings.TrimSpace(cfg.WorkspaceLinearBranchScope) == "" && !fieldMissingBecauseSecret(fieldRefs, "source.branch_scope", missingEnvVars) {
		errorsList = append(errorsList, model.NewWorkflowError(model.ErrWorkflowParseError, "source.branch_scope is required for linear tracker", nil))
	}
	if strings.TrimSpace(cfg.CodexCommand) == "" && !fieldMissingBecauseSecret(fieldRefs, "runtime.codex.command", missingEnvVars) {
		errorsList = append(errorsList, model.NewWorkflowError(model.ErrInvalidCodexCommand, "codex.command is required", nil))
	}
	if err := validateSessionPersistenceConfig(cfg.SessionPersistence); err != nil {
		errorsList = append(errorsList, err)
	}
	errorsList = append(errorsList, diagnoseNotificationsStructuralErrors(cfg.Notifications, fieldRefs, missingEnvVars)...)
	return errorsList
}

func fieldMissingBecauseSecret(sourceFields map[string]string, field string, missingEnvVars map[string]bool) bool {
	envVar, ok := sourceFields[field]
	return ok && missingEnvVars[envVar]
}

func isSensitiveEnvVar(value string) bool {
	upper := strings.ToUpper(value)
	for _, token := range []string{"KEY", "TOKEN", "SECRET", "PASSWORD"} {
		if strings.Contains(upper, token) {
			return true
		}
	}
	return false
}

func joinNestedPath(base string, key string) string {
	if base == "" {
		return key
	}
	return base + "." + key
}

func joinSlicePath(base string, index int) string {
	if base == "" {
		return fmt.Sprintf("[%d]", index)
	}
	return fmt.Sprintf("%s[%d]", base, index)
}

func joinDiagnosisPath(base string, path string) string {
	if path == "" {
		return base
	}
	if strings.HasPrefix(path, "[") {
		return base + path
	}
	return base + "." + path
}

func diagnoseNotificationsStructuralErrors(cfg model.NotificationsConfig, fieldRefs map[string]string, missingEnvVars map[string]bool) []error {
	errorsList := make([]error, 0)
	if len(cfg.Channels) == 0 {
		return errorsList
	}
	if cfg.Defaults.TimeoutMS <= 0 {
		errorsList = append(errorsList, model.NewWorkflowError(model.ErrWorkflowParseError, "runtime.notifications.defaults.timeout_ms must be > 0", nil))
	}
	if cfg.Defaults.RetryCount < 0 {
		errorsList = append(errorsList, model.NewWorkflowError(model.ErrWorkflowParseError, "runtime.notifications.defaults.retry_count must be >= 0", nil))
	}
	if cfg.Defaults.RetryDelayMS < 0 {
		errorsList = append(errorsList, model.NewWorkflowError(model.ErrWorkflowParseError, "runtime.notifications.defaults.retry_delay_ms must be >= 0", nil))
	}
	if cfg.Defaults.QueueSize <= 0 {
		errorsList = append(errorsList, model.NewWorkflowError(model.ErrWorkflowParseError, "runtime.notifications.defaults.queue_size must be > 0", nil))
	}
	if cfg.Defaults.CriticalQueueSize <= 0 {
		errorsList = append(errorsList, model.NewWorkflowError(model.ErrWorkflowParseError, "runtime.notifications.defaults.critical_queue_size must be > 0", nil))
	}

	seenIDs := make(map[string]struct{}, len(cfg.Channels))
	allowedEvents := notificationEventSet()
	allowedFamilies := runtimeEventFamilySet()
	for index, channel := range cfg.Channels {
		if strings.TrimSpace(channel.ID) == "" {
			errorsList = append(errorsList, model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("runtime.notifications.channels[%d].id is required", index), nil))
			continue
		}
		if _, exists := seenIDs[channel.ID]; exists {
			errorsList = append(errorsList, model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("runtime.notifications.channels[%d].id %q is duplicated", index, channel.ID), nil))
		}
		seenIDs[channel.ID] = struct{}{}

		switch channel.Kind {
		case model.NotificationChannelKindWebhook, model.NotificationChannelKindSlack:
		default:
			errorsList = append(errorsList, model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("runtime.notifications.channels[%d].kind %q is unsupported", index, channel.Kind), nil))
		}

		if len(channel.Subscriptions.Families) == 0 && len(channel.Subscriptions.Types) == 0 {
			errorsList = append(errorsList, model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("runtime.notifications.channels[%d].subscriptions must declare at least one family or type", index), nil))
		}
		for _, family := range channel.Subscriptions.Families {
			if _, ok := allowedFamilies[family]; !ok {
				errorsList = append(errorsList, model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("runtime.notifications.channels[%d].subscriptions.families contains unsupported family %q", index, family), nil))
			}
		}
		for _, eventType := range channel.Subscriptions.Types {
			if _, ok := allowedEvents[eventType]; !ok {
				errorsList = append(errorsList, model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("runtime.notifications.channels[%d].subscriptions.types contains unsupported event %q", index, eventType), nil))
			}
		}

		if channel.Delivery.TimeoutMS <= 0 {
			errorsList = append(errorsList, model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("runtime.notifications.channels[%d].delivery.timeout_ms must be > 0", index), nil))
		}
		if channel.Delivery.RetryCount < 0 {
			errorsList = append(errorsList, model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("runtime.notifications.channels[%d].delivery.retry_count must be >= 0", index), nil))
		}
		if channel.Delivery.RetryDelayMS < 0 {
			errorsList = append(errorsList, model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("runtime.notifications.channels[%d].delivery.retry_delay_ms must be >= 0", index), nil))
		}
		if channel.Delivery.QueueSize <= 0 {
			errorsList = append(errorsList, model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("runtime.notifications.channels[%d].delivery.queue_size must be > 0", index), nil))
		}
		if channel.Delivery.CriticalQueueSize <= 0 {
			errorsList = append(errorsList, model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("runtime.notifications.channels[%d].delivery.critical_queue_size must be > 0", index), nil))
		}

		switch channel.Kind {
		case model.NotificationChannelKindWebhook:
			urlField := fmt.Sprintf("runtime.notifications.channels[%d].webhook.url", index)
			if (channel.Webhook == nil || strings.TrimSpace(channel.Webhook.URL) == "") && !fieldMissingBecauseSecret(fieldRefs, urlField, missingEnvVars) {
				errorsList = append(errorsList, model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("runtime.notifications.channels[%d].webhook.url is required", index), nil))
			}
			if channel.Webhook != nil {
				for key, value := range channel.Webhook.Headers {
					if strings.TrimSpace(key) == "" {
						errorsList = append(errorsList, model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("runtime.notifications.channels[%d].webhook.headers contains an empty key", index), nil))
						continue
					}
					field := fmt.Sprintf("runtime.notifications.channels[%d].webhook.headers.%s", index, key)
					if strings.TrimSpace(value) == "" && !fieldMissingBecauseSecret(fieldRefs, field, missingEnvVars) {
						errorsList = append(errorsList, model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("runtime.notifications.channels[%d].webhook.headers.%s is required", index, key), nil))
					}
				}
			}
		case model.NotificationChannelKindSlack:
			urlField := fmt.Sprintf("runtime.notifications.channels[%d].slack.incoming_webhook_url", index)
			if (channel.Slack == nil || strings.TrimSpace(channel.Slack.IncomingWebhookURL) == "") && !fieldMissingBecauseSecret(fieldRefs, urlField, missingEnvVars) {
				errorsList = append(errorsList, model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("runtime.notifications.channels[%d].slack.incoming_webhook_url is required", index), nil))
			}
		}
	}
	return errorsList
}
