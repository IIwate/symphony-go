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
)

var envValuePattern = regexp.MustCompile(`^\$(\w+)$`)

func NewFromWorkflow(def *model.WorkflowDefinition) (*model.ServiceConfig, error) {
	if def == nil {
		return nil, model.NewWorkflowError(model.ErrWorkflowParseError, "workflow definition is nil", nil)
	}

	configMap := def.Config
	if configMap == nil {
		configMap = map[string]any{}
	}

	cfg := defaultServiceConfig()

	tracker := getMap(configMap, "tracker")
	cfg.TrackerKind = model.NormalizeState(getString(tracker, "kind", ""))
	if endpoint := strings.TrimSpace(getString(tracker, "endpoint", "")); endpoint != "" {
		cfg.TrackerEndpoint = endpoint
	}
	cfg.TrackerAPIKey = resolveEnvString(strings.TrimSpace(getString(tracker, "api_key", "")))
	cfg.TrackerProjectSlug = strings.TrimSpace(getString(tracker, "project_slug", ""))
	cfg.TrackerRepo = strings.TrimSpace(getString(tracker, "repo", ""))
	if states, ok := getStringSlice(tracker, "active_states"); ok && len(states) > 0 {
		cfg.ActiveStates = states
	}
	if states, ok := getStringSlice(tracker, "terminal_states"); ok && len(states) > 0 {
		cfg.TerminalStates = states
	}

	polling := getMap(configMap, "polling")
	if interval, ok := getInt(polling, "interval_ms"); ok && interval > 0 {
		cfg.PollIntervalMS = interval
	}

	workspace := getMap(configMap, "workspace")
	if root := expandHomePath(strings.TrimSpace(getString(workspace, "root", ""))); root != "" {
		cfg.WorkspaceRoot = root
	}
	cfg.WorkspaceLinearBranchScope = slugifyScopeValue(getString(workspace, "linear_branch_scope", ""))

	hooks := getMap(configMap, "hooks")
	if value, ok := getOptionalString(hooks, "after_create"); ok {
		cfg.HookAfterCreate = stringPointer(value)
	}
	if value, ok := getOptionalString(hooks, "before_run"); ok {
		cfg.HookBeforeRun = stringPointer(value)
	}
	if value, ok := getOptionalString(hooks, "after_run"); ok {
		cfg.HookAfterRun = stringPointer(value)
	}
	if value, ok := getOptionalString(hooks, "before_remove"); ok {
		cfg.HookBeforeRemove = stringPointer(value)
	}
	if timeout, ok := getInt(hooks, "timeout_ms"); ok && timeout > 0 {
		cfg.HookTimeoutMS = timeout
	}

	agent := getMap(configMap, "agent")
	if maxConcurrent, ok := getInt(agent, "max_concurrent_agents"); ok && maxConcurrent > 0 {
		cfg.MaxConcurrentAgents = maxConcurrent
	}
	if maxTurns, ok := getInt(agent, "max_turns"); ok && maxTurns > 0 {
		cfg.MaxTurns = maxTurns
	}
	if maxBackoff, ok := getInt(agent, "max_retry_backoff_ms"); ok && maxBackoff > 0 {
		cfg.MaxRetryBackoffMS = maxBackoff
	}
	if byState := getMap(agent, "max_concurrent_agents_by_state"); len(byState) > 0 {
		cfg.MaxConcurrentAgentsByState = normalizePositiveIntMap(byState)
	}

	orchestrator := getMap(configMap, "orchestrator")
	if autoClose, ok := getBool(orchestrator, "auto_close_on_pr"); ok {
		cfg.OrchestratorAutoCloseOnPR = autoClose
	}

	codex := getMap(configMap, "codex")
	if command := strings.TrimSpace(getString(codex, "command", "")); command != "" {
		cfg.CodexCommand = command
	}
	if approvalPolicy := strings.TrimSpace(getString(codex, "approval_policy", "")); approvalPolicy != "" {
		cfg.CodexApprovalPolicy = approvalPolicy
	}
	if threadSandbox := strings.TrimSpace(getString(codex, "thread_sandbox", "")); threadSandbox != "" {
		cfg.CodexThreadSandbox = threadSandbox
	}
	if sandboxPolicy, ok := codex["turn_sandbox_policy"]; ok {
		cfg.CodexTurnSandboxPolicy = stringifyValue(sandboxPolicy)
	}
	if turnTimeout, ok := getInt(codex, "turn_timeout_ms"); ok && turnTimeout > 0 {
		cfg.CodexTurnTimeoutMS = turnTimeout
	}
	if readTimeout, ok := getInt(codex, "read_timeout_ms"); ok && readTimeout > 0 {
		cfg.CodexReadTimeoutMS = readTimeout
	}
	if stallTimeout, ok := getInt(codex, "stall_timeout_ms"); ok {
		cfg.CodexStallTimeoutMS = stallTimeout
	}

	server := getMap(configMap, "server")
	if port, ok := getInt(server, "port"); ok && port >= 0 {
		cfg.ServerPort = &port
	}

	return cfg, nil
}

func ValidateForDispatch(cfg *model.ServiceConfig) error {
	if cfg == nil {
		return model.NewWorkflowError(model.ErrWorkflowParseError, "service config is nil", nil)
	}
	if cfg.TrackerKind == "" {
		return model.NewTrackerError(model.ErrUnsupportedTrackerKind, "tracker.kind is required", nil)
	}
	if cfg.TrackerKind != "linear" {
		return model.NewTrackerError(model.ErrUnsupportedTrackerKind, fmt.Sprintf("unsupported tracker.kind %q", cfg.TrackerKind), nil)
	}
	if strings.TrimSpace(cfg.TrackerAPIKey) == "" {
		return model.NewTrackerError(model.ErrMissingTrackerAPIKey, "tracker.api_key is required", nil)
	}
	if strings.TrimSpace(cfg.TrackerProjectSlug) == "" {
		return model.NewTrackerError(model.ErrMissingTrackerProjectSlug, "tracker.project_slug is required", nil)
	}
	if strings.TrimSpace(cfg.CodexCommand) == "" {
		return model.NewWorkflowError(model.ErrInvalidCodexCommand, "codex.command is required", nil)
	}

	return nil
}

func defaultServiceConfig() *model.ServiceConfig {
	return &model.ServiceConfig{
		TrackerEndpoint:            "https://api.linear.app/graphql",
		ActiveStates:               []string{"Todo", "In Progress"},
		TerminalStates:             []string{"Closed", "Cancelled", "Canceled", "Duplicate", "Done"},
		PollIntervalMS:             30000,
		WorkspaceRoot:              filepath.Join(os.TempDir(), "symphony_workspaces"),
		HookTimeoutMS:              60000,
		MaxConcurrentAgents:        10,
		MaxTurns:                   20,
		MaxRetryBackoffMS:          300000,
		MaxConcurrentAgentsByState: map[string]int{},
		OrchestratorAutoCloseOnPR:  true,
		CodexCommand:               "codex app-server",
		CodexApprovalPolicy:        "never",
		CodexThreadSandbox:         "workspace-write",
		CodexTurnSandboxPolicy:     `{"type":"workspaceWrite"}`,
		CodexTurnTimeoutMS:         3600000,
		CodexReadTimeoutMS:         5000,
		CodexStallTimeoutMS:        300000,
	}
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
		return typed
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
		items := splitAndTrim(typed)
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
		trimmed := strings.TrimSpace(typed)
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
		trimmed := strings.TrimSpace(strings.ToLower(typed))
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
		trimmed := strings.TrimSpace(typed)
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

	resolved, ok := os.LookupEnv(matches[1])
	if !ok {
		return ""
	}

	return strings.TrimSpace(resolved)
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
