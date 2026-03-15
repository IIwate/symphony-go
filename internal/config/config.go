package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"symphony-go/internal/model"
	"symphony-go/internal/secret"
)

var (
	envValuePattern        = regexp.MustCompile(`^\$(\w+)$`)
	requiredHookEnvPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\:\?[^}]*\}`)
)

func NewFromWorkflow(def *model.WorkflowDefinition) (*model.ServiceConfig, error) {
	if def == nil {
		return nil, model.NewWorkflowError(model.ErrWorkflowParseError, "workflow definition is nil", nil)
	}

	contractConfig, err := ParseWorkflowContract(def)
	if err != nil {
		return nil, err
	}

	cfg := defaultServiceConfig()
	cfg.AutomationRootDir = strings.TrimSpace(def.RootDir)
	cfg.TrackerKind = contractConfig.Source.Kind
	cfg.TrackerEndpoint = contractConfig.Source.Endpoint
	cfg.TrackerProjectSlug = contractConfig.Source.ProjectSlug
	cfg.TrackerLinearChildrenBlockParent = contractConfig.Source.LinearChildrenBlockParent
	cfg.TrackerRepo = contractConfig.Source.Repo
	cfg.ActiveStates = append([]string(nil), contractConfig.Source.ActiveStates...)
	cfg.TerminalStates = append([]string(nil), contractConfig.Source.TerminalStates...)
	cfg.DomainID = contractConfig.Domain.ID
	cfg.PollIntervalMS = contractConfig.Domain.PollIntervalMS
	cfg.WorkspaceRoot = contractConfig.Domain.WorkspaceRoot
	cfg.WorkspaceLinearBranchScope = contractConfig.Source.BranchScope
	cfg.WorkspaceBranchNamespace = contractConfig.Domain.BranchNamespace
	cfg.WorkspaceGitAuthorName = contractConfig.Domain.GitAuthorName
	cfg.WorkspaceGitAuthorEmail = contractConfig.Domain.GitAuthorEmail
	cfg.HookAfterCreate = cloneStringPtr(contractConfig.Execution.HookAfterCreate)
	cfg.HookBeforeRun = cloneStringPtr(contractConfig.Execution.HookBeforeRun)
	cfg.HookBeforeRunContinuation = cloneStringPtr(contractConfig.Execution.HookBeforeRunContinuation)
	cfg.HookAfterRun = cloneStringPtr(contractConfig.Execution.HookAfterRun)
	cfg.HookBeforeRemove = cloneStringPtr(contractConfig.Execution.HookBeforeRemove)
	cfg.HookTimeoutMS = contractConfig.Execution.HookTimeoutMS
	cfg.MaxConcurrentAgents = contractConfig.Execution.MaxConcurrentAgents
	cfg.MaxTurns = contractConfig.Execution.MaxTurns
	cfg.MaxRetryBackoffMS = contractConfig.Execution.MaxRetryBackoffMS
	cfg.RunBudgetTotalMS = contractConfig.Execution.RunBudgetTotalMS
	cfg.RunExecutionBudgetMS = contractConfig.Execution.RunExecutionBudgetMS
	cfg.RunReviewFixBudgetMS = contractConfig.Execution.RunReviewFixBudgetMS
	cfg.MaxConcurrentAgentsByState = cloneIntMap(contractConfig.Execution.MaxConcurrentAgentsByState)
	cfg.CodexCommand = contractConfig.Execution.Codex.Command
	cfg.CodexApprovalPolicy = contractConfig.Execution.Codex.ApprovalPolicy
	cfg.CodexThreadSandbox = contractConfig.Execution.Codex.ThreadSandbox
	cfg.CodexTurnSandboxPolicy = contractConfig.Execution.Codex.TurnSandboxPolicy
	cfg.CodexTurnTimeoutMS = contractConfig.Execution.Codex.TurnTimeoutMS
	cfg.CodexReadTimeoutMS = contractConfig.Execution.Codex.ReadTimeoutMS
	cfg.CodexStallTimeoutMS = contractConfig.Execution.Codex.StallTimeoutMS
	cfg.ServerHost = contractConfig.Service.ServerHost
	cfg.ServerPort = cloneIntPtr(contractConfig.Service.ServerPort)
	cfg.SessionPersistence.Enabled = contractConfig.Persistence.BackendKind == PersistenceBackendKindFile
	if cfg.SessionPersistence.Enabled {
		cfg.SessionPersistence.Kind = model.SessionPersistenceKindFile
		cfg.SessionPersistence.File.Path = contractConfig.Persistence.FilePath
		cfg.SessionPersistence.File.FlushIntervalMS = contractConfig.Persistence.FlushIntervalMS
		cfg.SessionPersistence.File.FsyncOnCritical = contractConfig.Persistence.FsyncOnCritical
	}

	secretValue, ok, err := secret.DefaultRegistry.Resolve(contractConfig.Source.APIKeyRef, secret.DefaultResolver)
	if err != nil {
		return nil, err
	}
	if ok {
		cfg.TrackerAPIKey = secretValue
	}

	cfg.Notifications = resolveNotificationConfig(contractConfig)
	return cfg, nil
}

func rejectLegacySessionPersistenceKeys(cfg map[string]any) error {
	legacyMessages := map[string]string{
		"backend":           "runtime.session_persistence.backend is no longer supported; use runtime.session_persistence.kind",
		"path":              "runtime.session_persistence.path is no longer supported; use runtime.session_persistence.file.path",
		"flush_interval_ms": "runtime.session_persistence.flush_interval_ms is no longer supported; use runtime.session_persistence.file.flush_interval_ms",
		"fsync_on_critical": "runtime.session_persistence.fsync_on_critical is no longer supported; use runtime.session_persistence.file.fsync_on_critical",
	}
	for key, message := range legacyMessages {
		if _, ok := cfg[key]; ok {
			return model.NewWorkflowError(model.ErrWorkflowParseError, message, nil)
		}
	}
	return nil
}

func rejectLegacyNotificationChannelKeys(channel map[string]any, index int) error {
	legacyMessages := map[string]string{
		"name":    fmt.Sprintf("runtime.notifications.channels[%d].name is no longer supported; use id and display_name", index),
		"events":  fmt.Sprintf("runtime.notifications.channels[%d].events is no longer supported; use subscriptions.types", index),
		"url":     fmt.Sprintf("runtime.notifications.channels[%d].url is no longer supported; use webhook.url or slack.incoming_webhook_url", index),
		"headers": fmt.Sprintf("runtime.notifications.channels[%d].headers is no longer supported; use webhook.headers", index),
	}
	for key, message := range legacyMessages {
		if _, ok := channel[key]; ok {
			return model.NewWorkflowError(model.ErrWorkflowParseError, message, nil)
		}
	}
	return nil
}

func ValidateForDispatch(cfg *model.ServiceConfig) error {
	if cfg == nil {
		return model.NewWorkflowError(model.ErrWorkflowParseError, "service config is nil", nil)
	}
	if cfg.TrackerKind == "" {
		return model.NewTrackerError(model.ErrUnsupportedTrackerKind, "source_adapter.kind is required", nil)
	}
	if cfg.TrackerKind != "linear" {
		return model.NewTrackerError(model.ErrUnsupportedTrackerKind, fmt.Sprintf("unsupported source_adapter.kind %q", cfg.TrackerKind), nil)
	}
	if strings.TrimSpace(cfg.TrackerAPIKey) == "" {
		return model.NewTrackerError(model.ErrMissingTrackerAPIKey, "source_adapter.credentials.api_key_ref is required", nil)
	}
	if strings.TrimSpace(cfg.TrackerProjectSlug) == "" {
		return model.NewTrackerError(model.ErrMissingTrackerProjectSlug, "source_adapter.project_slug is required", nil)
	}
	if strings.TrimSpace(cfg.WorkspaceLinearBranchScope) == "" {
		return model.NewWorkflowError(model.ErrWorkflowParseError, "source_adapter.branch_scope is required for linear source", nil)
	}
	if strings.TrimSpace(cfg.CodexCommand) == "" {
		return model.NewWorkflowError(model.ErrInvalidCodexCommand, "execution.backend.codex.command is required", nil)
	}
	if cfg.RunExecutionBudgetMS <= 0 {
		return model.NewWorkflowError(model.ErrWorkflowParseError, "execution.agent.run_budget_ms.execution must be > 0", nil)
	}
	if cfg.RunReviewFixBudgetMS < 0 {
		return model.NewWorkflowError(model.ErrWorkflowParseError, "execution.agent.run_budget_ms.review_fix must be >= 0", nil)
	}
	if cfg.RunBudgetTotalMS <= 0 {
		return model.NewWorkflowError(model.ErrWorkflowParseError, "execution.agent.run_budget_ms.total must be > 0", nil)
	}
	if cfg.RunBudgetTotalMS < cfg.RunExecutionBudgetMS+cfg.RunReviewFixBudgetMS {
		return model.NewWorkflowError(model.ErrWorkflowParseError, "execution.agent.run_budget_ms.total must be >= execution + review_fix", nil)
	}
	if err := validateSessionPersistenceConfig(cfg.SessionPersistence); err != nil {
		return err
	}
	if err := validateNotificationsConfig(cfg.Notifications); err != nil {
		return err
	}
	if err := validateRequiredHookEnvs(cfg); err != nil {
		return err
	}

	return nil
}

func defaultServiceConfig() *model.ServiceConfig {
	return &model.ServiceConfig{
		TrackerEndpoint:                  "https://api.linear.app/graphql",
		TrackerLinearChildrenBlockParent: true,
		ActiveStates:                     []string{"Todo", "In Progress"},
		TerminalStates:                   []string{"Closed", "Cancelled", "Canceled", "Duplicate", "Done"},
		DomainID:                         "default",
		PollIntervalMS:                   30000,
		WorkspaceRoot:                    filepath.Join(os.TempDir(), "symphony_workspaces"),
		HookTimeoutMS:                    60000,
		MaxConcurrentAgents:              10,
		MaxTurns:                         20,
		MaxRetryBackoffMS:                300000,
		RunBudgetTotalMS:                 3600000,
		RunExecutionBudgetMS:             3600000,
		RunReviewFixBudgetMS:             0,
		MaxConcurrentAgentsByState:       map[string]int{},
		CodexCommand:                     "codex app-server",
		CodexApprovalPolicy:              "never",
		CodexThreadSandbox:               "workspace-write",
		CodexTurnSandboxPolicy:           `{"type":"workspaceWrite"}`,
		CodexTurnTimeoutMS:               3600000,
		CodexReadTimeoutMS:               5000,
		CodexStallTimeoutMS:              300000,
		ServerHost:                       "127.0.0.1",
		SessionPersistence: model.SessionPersistenceConfig{
			Kind: model.SessionPersistenceKindFile,
			File: model.SessionPersistenceFileConfig{
				Path:            filepath.Join(".", "local", "runtime-ledger.json"),
				FlushIntervalMS: 1000,
				FsyncOnCritical: true,
			},
		},
		Notifications: model.NotificationsConfig{
			Defaults: model.NotificationDeliveryConfig{
				TimeoutMS:         5000,
				RetryCount:        2,
				RetryDelayMS:      1000,
				QueueSize:         128,
				CriticalQueueSize: 32,
			},
		},
	}
}

func resolveNotificationConfig(contractConfig WorkflowContract) model.NotificationsConfig {
	result := model.NotificationsConfig{
		Defaults: model.NotificationDeliveryConfig{
			TimeoutMS:         contractConfig.Service.Notifications.Defaults.TimeoutMS,
			RetryCount:        contractConfig.Service.Notifications.Defaults.RetryCount,
			RetryDelayMS:      contractConfig.Service.Notifications.Defaults.RetryDelayMS,
			QueueSize:         contractConfig.Service.Notifications.Defaults.QueueSize,
			CriticalQueueSize: contractConfig.Service.Notifications.Defaults.CriticalQueueSize,
		},
	}
	if len(contractConfig.Service.Notifications.Channels) == 0 {
		return result
	}

	result.Channels = make([]model.NotificationChannelConfig, 0, len(contractConfig.Service.Notifications.Channels))
	for _, channel := range contractConfig.Service.Notifications.Channels {
		parsed := model.NotificationChannelConfig{
			ID:            channel.ID,
			DisplayName:   channel.DisplayName,
			Kind:          channel.Kind,
			Subscriptions: channel.Subscriptions,
			Delivery:      channel.Delivery,
		}
		switch channel.Kind {
		case model.NotificationChannelKindWebhook:
			headers := map[string]string{}
			if channel.Webhook != nil {
				for key, ref := range channel.Webhook.HeaderRefs {
					value, ok, err := secret.DefaultRegistry.Resolve(ref, secret.DefaultResolver)
					if err == nil && ok {
						headers[key] = value
					}
				}
				urlText, ok, err := secret.DefaultRegistry.Resolve(channel.Webhook.URLRef, secret.DefaultResolver)
				if err == nil && ok {
					parsed.Webhook = &model.WebhookNotificationConfig{
						URL:     urlText,
						Headers: headers,
					}
				} else {
					parsed.Webhook = &model.WebhookNotificationConfig{Headers: headers}
				}
			}
		case model.NotificationChannelKindSlack:
			if channel.Slack != nil {
				urlText, ok, err := secret.DefaultRegistry.Resolve(channel.Slack.IncomingWebhookURLRef, secret.DefaultResolver)
				if err == nil && ok {
					parsed.Slack = &model.SlackNotificationConfig{IncomingWebhookURL: urlText}
				} else {
					parsed.Slack = &model.SlackNotificationConfig{}
				}
			}
		}
		result.Channels = append(result.Channels, parsed)
	}
	return result
}

func cloneStringPtr(value *string) *string {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func cloneIntPtr(value *int) *int {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func cloneIntMap(source map[string]int) map[string]int {
	if len(source) == 0 {
		return map[string]int{}
	}
	result := make(map[string]int, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func getMap(source map[string]any, key string) map[string]any {
	if source == nil {
		return map[string]any{}
	}
	raw, ok := source[key]
	if !ok || raw == nil {
		return map[string]any{}
	}
	if typed, ok := raw.(map[string]any); ok {
		return typed
	}
	if typed, ok := raw.(map[any]any); ok {
		result := make(map[string]any, len(typed))
		for nestedKey, value := range typed {
			result[fmt.Sprint(nestedKey)] = value
		}
		return result
	}

	return map[string]any{}
}

func getString(source map[string]any, key string, fallback string) string {
	if source == nil {
		return fallback
	}
	raw, ok := source[key]
	if !ok || raw == nil {
		return fallback
	}
	if typed, ok := raw.(string); ok {
		return resolveEnvString(typed)
	}

	return fallback
}

func getOptionalString(source map[string]any, key string) (string, bool) {
	value := strings.TrimSpace(getString(source, key, ""))
	if value == "" {
		return "", false
	}

	return value, true
}

func getStringSlice(source map[string]any, key string) ([]string, bool) {
	if source == nil {
		return nil, false
	}
	raw, ok := source[key]
	if !ok || raw == nil {
		return nil, false
	}

	switch typed := raw.(type) {
	case string:
		items := splitAndTrim(resolveEnvString(typed))
		return items, len(items) > 0
	case []string:
		items := make([]string, 0, len(typed))
		for _, item := range typed {
			if trimmed := strings.TrimSpace(item); trimmed != "" {
				items = append(items, trimmed)
			}
		}
		return items, len(items) > 0
	case []any:
		items := make([]string, 0, len(typed))
		for _, item := range typed {
			text, ok := item.(string)
			if !ok {
				continue
			}
			if trimmed := strings.TrimSpace(text); trimmed != "" {
				items = append(items, trimmed)
			}
		}
		return items, len(items) > 0
	default:
		return nil, false
	}
}

func getMapSlice(source map[string]any, key string) []map[string]any {
	if source == nil {
		return nil
	}
	raw, ok := source[key]
	if !ok || raw == nil {
		return nil
	}

	items, ok := raw.([]any)
	if !ok {
		return nil
	}

	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		typed, ok := item.(map[string]any)
		if ok {
			result = append(result, typed)
			continue
		}
		nested, ok := item.(map[any]any)
		if !ok {
			continue
		}
		converted := make(map[string]any, len(nested))
		for nestedKey, nestedValue := range nested {
			converted[fmt.Sprint(nestedKey)] = nestedValue
		}
		result = append(result, converted)
	}
	return result
}

func getStringMap(source map[string]any) map[string]string {
	if len(source) == 0 {
		return map[string]string{}
	}
	result := make(map[string]string, len(source))
	for key, raw := range source {
		trimmedKey := strings.TrimSpace(key)
		if trimmedKey == "" {
			continue
		}
		switch typed := raw.(type) {
		case string:
			result[trimmedKey] = strings.TrimSpace(resolveEnvString(typed))
		default:
			result[trimmedKey] = strings.TrimSpace(stringifyValue(raw))
		}
	}
	return result
}

func getInt(source map[string]any, key string) (int, bool) {
	if source == nil {
		return 0, false
	}
	raw, ok := source[key]
	if !ok || raw == nil {
		return 0, false
	}

	switch typed := raw.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		return int(typed), true
	case string:
		trimmed := strings.TrimSpace(resolveEnvString(typed))
		if trimmed == "" {
			return 0, false
		}
		parsed, err := strconv.Atoi(trimmed)
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

func getBool(source map[string]any, key string) (bool, bool) {
	if source == nil {
		return false, false
	}
	raw, ok := source[key]
	if !ok || raw == nil {
		return false, false
	}

	switch typed := raw.(type) {
	case bool:
		return typed, true
	case string:
		trimmed := strings.TrimSpace(strings.ToLower(resolveEnvString(typed)))
		switch trimmed {
		case "true":
			return true, true
		case "false":
			return false, true
		default:
			return false, false
		}
	default:
		return false, false
	}
}

func normalizePositiveIntMap(source map[string]any) map[string]int {
	result := make(map[string]int)
	for key, raw := range source {
		value, ok := intFromValue(raw)
		if !ok || value <= 0 {
			continue
		}
		normalizedKey := model.NormalizeState(key)
		if normalizedKey == "" {
			continue
		}
		result[normalizedKey] = value
	}

	return result
}

func intFromValue(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		return int(typed), true
	case string:
		trimmed := strings.TrimSpace(resolveEnvString(typed))
		if trimmed == "" {
			return 0, false
		}
		parsed, err := strconv.Atoi(trimmed)
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

func resolveEnvString(value string) string {
	matches := envValuePattern.FindStringSubmatch(value)
	if len(matches) != 2 {
		return value
	}

	resolved, ok := secret.DefaultResolver(matches[1])
	if !ok {
		return ""
	}

	return strings.TrimSpace(resolved)
}

func validateSessionPersistenceConfig(cfg model.SessionPersistenceConfig) error {
	if !cfg.Enabled {
		return nil
	}
	kind := cfg.Kind
	if kind == "" {
		kind = model.SessionPersistenceKindFile
	}
	switch kind {
	case model.SessionPersistenceKindFile:
	default:
		return model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("persistence.backend.kind %q is unsupported", kind), nil)
	}
	if strings.TrimSpace(cfg.File.Path) == "" {
		return model.NewWorkflowError(model.ErrWorkflowParseError, "persistence.file.path is required", nil)
	}
	if cfg.File.FlushIntervalMS <= 0 {
		return model.NewWorkflowError(model.ErrWorkflowParseError, "persistence.file.flush_interval_ms must be > 0", nil)
	}
	return nil
}

func validateNotificationsConfig(cfg model.NotificationsConfig) error {
	if len(cfg.Channels) == 0 {
		return nil
	}
	if cfg.Defaults.TimeoutMS <= 0 {
		return model.NewWorkflowError(model.ErrWorkflowParseError, "service.notifications.defaults.timeout_ms must be > 0", nil)
	}
	if cfg.Defaults.RetryCount < 0 {
		return model.NewWorkflowError(model.ErrWorkflowParseError, "service.notifications.defaults.retry_count must be >= 0", nil)
	}
	if cfg.Defaults.RetryDelayMS < 0 {
		return model.NewWorkflowError(model.ErrWorkflowParseError, "service.notifications.defaults.retry_delay_ms must be >= 0", nil)
	}
	if cfg.Defaults.QueueSize <= 0 {
		return model.NewWorkflowError(model.ErrWorkflowParseError, "service.notifications.defaults.queue_size must be > 0", nil)
	}
	if cfg.Defaults.CriticalQueueSize <= 0 {
		return model.NewWorkflowError(model.ErrWorkflowParseError, "service.notifications.defaults.critical_queue_size must be > 0", nil)
	}

	seenIDs := make(map[string]struct{}, len(cfg.Channels))
	allowedEvents := notificationEventSet()
	allowedFamilies := runtimeEventFamilySet()
	for index, channel := range cfg.Channels {
		if strings.TrimSpace(channel.ID) == "" {
			return model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("service.notifications.channels[%d].id is required", index), nil)
		}
		if _, exists := seenIDs[channel.ID]; exists {
			return model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("service.notifications.channels[%d].id %q is duplicated", index, channel.ID), nil)
		}
		seenIDs[channel.ID] = struct{}{}

		switch channel.Kind {
		case model.NotificationChannelKindWebhook, model.NotificationChannelKindSlack:
		default:
			return model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("service.notifications.channels[%d].kind %q is unsupported", index, channel.Kind), nil)
		}

		if len(channel.Subscriptions.Families) == 0 && len(channel.Subscriptions.Types) == 0 {
			return model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("service.notifications.channels[%d].subscriptions must declare at least one family or type", index), nil)
		}
		for _, family := range channel.Subscriptions.Families {
			if _, ok := allowedFamilies[family]; !ok {
				return model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("service.notifications.channels[%d].subscriptions.families contains unsupported family %q", index, family), nil)
			}
		}
		for _, eventType := range channel.Subscriptions.Types {
			if _, ok := allowedEvents[eventType]; !ok {
				return model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("service.notifications.channels[%d].subscriptions.types contains unsupported event %q", index, eventType), nil)
			}
		}

		if channel.Delivery.TimeoutMS <= 0 {
			return model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("service.notifications.channels[%d].delivery.timeout_ms must be > 0", index), nil)
		}
		if channel.Delivery.RetryCount < 0 {
			return model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("service.notifications.channels[%d].delivery.retry_count must be >= 0", index), nil)
		}
		if channel.Delivery.RetryDelayMS < 0 {
			return model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("service.notifications.channels[%d].delivery.retry_delay_ms must be >= 0", index), nil)
		}
		if channel.Delivery.QueueSize <= 0 {
			return model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("service.notifications.channels[%d].delivery.queue_size must be > 0", index), nil)
		}
		if channel.Delivery.CriticalQueueSize <= 0 {
			return model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("service.notifications.channels[%d].delivery.critical_queue_size must be > 0", index), nil)
		}

		switch channel.Kind {
		case model.NotificationChannelKindWebhook:
			if channel.Webhook == nil || strings.TrimSpace(channel.Webhook.URL) == "" {
				return model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("service.notifications.channels[%d].webhook.url is required", index), nil)
			}
			for key, value := range channel.Webhook.Headers {
				if strings.TrimSpace(key) == "" {
					return model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("service.notifications.channels[%d].webhook.headers contains an empty key", index), nil)
				}
				if strings.TrimSpace(value) == "" {
					return model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("service.notifications.channels[%d].webhook.headers.%s is required", index, key), nil)
				}
			}
		case model.NotificationChannelKindSlack:
			if channel.Slack == nil || strings.TrimSpace(channel.Slack.IncomingWebhookURL) == "" {
				return model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("service.notifications.channels[%d].slack.incoming_webhook_url is required", index), nil)
			}
		}
	}

	return nil
}

func allNotificationEventTypes() []model.NotificationEventType {
	return []model.NotificationEventType{
		model.NotificationEventIssueDispatched,
		model.NotificationEventIssueCompleted,
		model.NotificationEventIssueFailed,
		model.NotificationEventIssueInterventionRequired,
		model.NotificationEventSystemAlert,
		model.NotificationEventSystemAlertCleared,
	}
}

func notificationEventSet() map[model.NotificationEventType]struct{} {
	result := make(map[model.NotificationEventType]struct{}, 6)
	for _, eventType := range allNotificationEventTypes() {
		result[eventType] = struct{}{}
	}
	return result
}

func runtimeEventFamilySet() map[model.RuntimeEventFamily]struct{} {
	return map[model.RuntimeEventFamily]struct{}{
		model.RuntimeEventFamilyIssue:  {},
		model.RuntimeEventFamilyHealth: {},
	}
}

func validateRequiredHookEnvs(cfg *model.ServiceConfig) error {
	for hookName, script := range requiredHookScripts(cfg) {
		matches := requiredHookEnvPattern.FindAllStringSubmatch(script, -1)
		for _, match := range matches {
			if len(match) != 2 {
				continue
			}
			envName := match[1]
			value, ok := secret.DefaultResolver(envName)
			if ok && strings.TrimSpace(value) != "" {
				continue
			}
			return model.NewWorkflowError(
				model.ErrWorkflowParseError,
				fmt.Sprintf("hooks.%s requires environment variable %s", hookName, envName),
				nil,
			)
		}
	}

	return nil
}

func requiredHookScripts(cfg *model.ServiceConfig) map[string]string {
	scripts := map[string]string{}
	if cfg == nil {
		return scripts
	}
	if cfg.HookAfterCreate != nil {
		scripts["after_create"] = *cfg.HookAfterCreate
	}
	if cfg.HookBeforeRun != nil {
		scripts["before_run"] = *cfg.HookBeforeRun
	}
	if cfg.HookBeforeRunContinuation != nil {
		scripts["before_run_continuation"] = *cfg.HookBeforeRunContinuation
	}
	if cfg.HookAfterRun != nil {
		scripts["after_run"] = *cfg.HookAfterRun
	}
	if cfg.HookBeforeRemove != nil {
		scripts["before_remove"] = *cfg.HookBeforeRemove
	}
	return scripts
}

func expandHomePath(value string) string {
	if value == "" {
		return value
	}
	if value != "~" && !strings.HasPrefix(value, "~/") && !strings.HasPrefix(value, "~\\") {
		return value
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return value
	}
	if value == "~" {
		return homeDir
	}

	relative := strings.TrimPrefix(strings.TrimPrefix(value, "~/"), "~\\")
	return filepath.Join(homeDir, filepath.FromSlash(relative))
}

func splitAndTrim(value string) []string {
	parts := strings.Split(value, ",")
	items := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			items = append(items, trimmed)
		}
	}

	return items
}

func slugifyScopeValue(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	var builder strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(trimmed) {
		isAlpha := r >= 'a' && r <= 'z'
		isDigit := r >= '0' && r <= '9'
		if isAlpha || isDigit {
			builder.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			builder.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(builder.String(), "-")
}

func stringifyValue(value any) string {
	if value == nil {
		return ""
	}
	if typed, ok := value.(string); ok {
		return strings.TrimSpace(typed)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return ""
	}

	return string(raw)
}

func stringPointer(value string) *string {
	if value == "" {
		return nil
	}

	copyValue := value
	return &copyValue
}
