package runtimepolicy

import (
	"errors"
	"fmt"
	"reflect"
	"strings"

	"symphony-go/internal/model"
	"symphony-go/internal/model/contract"
	"symphony-go/internal/orchestrator"
)

type ReloadOutcome string

const (
	ReloadOutcomeNoChange         ReloadOutcome = "no_change"
	ReloadOutcomeHotReloadAllowed ReloadOutcome = "hot_reload_allowed"
	ReloadOutcomeRestartRequired  ReloadOutcome = "restart_required"
)

type ReloadDecision struct {
	Outcome   ReloadOutcome
	FieldPath string
	Reason    *contract.Reason
}

func (d ReloadDecision) RequiresRestart() bool {
	return d.Outcome == ReloadOutcomeRestartRequired
}

type RestartRequiredError struct {
	Decision ReloadDecision
}

func (e *RestartRequiredError) Error() string {
	if e == nil {
		return "restart required"
	}
	fieldPath := strings.TrimSpace(e.Decision.FieldPath)
	if fieldPath == "" {
		return "restart required"
	}
	return fmt.Sprintf("%s changed: restart required", fieldPath)
}

func IsRestartRequired(err error) (*RestartRequiredError, bool) {
	var target *RestartRequiredError
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}

func NewRestartRequiredError(decision ReloadDecision) error {
	if !decision.RequiresRestart() {
		return nil
	}
	return &RestartRequiredError{Decision: decision}
}

type IdentityInput struct {
	ConfigDir            string
	Profile              string
	AutomationDefinition *model.AutomationDefinition
	ServiceConfig        *model.ServiceConfig
}

func BuildRuntimeIdentity(input IdentityInput) orchestrator.RuntimeIdentity {
	identity := orchestrator.RuntimeIdentity{
		Compatibility: orchestrator.RuntimeCompatibility{
			Profile: strings.TrimSpace(input.Profile),
		},
		Descriptor: orchestrator.RuntimeDescriptor{
			ConfigRoot: strings.TrimSpace(input.ConfigDir),
		},
	}
	if input.AutomationDefinition != nil {
		identity.Compatibility.ActiveSource = selectedSourceName(input.AutomationDefinition)
		identity.Compatibility.SourceKind = selectedSourceKind(input.AutomationDefinition)
		identity.Compatibility.FlowName = strings.TrimSpace(input.AutomationDefinition.Selection.DispatchFlow)
	}
	if input.ServiceConfig != nil {
		cfg := input.ServiceConfig
		identity.Compatibility.TrackerKind = strings.TrimSpace(cfg.TrackerKind)
		identity.Compatibility.TrackerRepo = strings.TrimSpace(cfg.TrackerRepo)
		identity.Compatibility.TrackerProjectSlug = strings.TrimSpace(cfg.TrackerProjectSlug)
		identity.Descriptor.WorkspaceRoot = strings.TrimSpace(cfg.WorkspaceRoot)
		identity.Descriptor.SessionPersistenceKind = string(cfg.SessionPersistence.Kind)
		identity.Descriptor.SessionStatePath = strings.TrimSpace(cfg.SessionPersistence.File.Path)
	}
	return identity
}

func EvaluateReload(currentRepoDef *model.AutomationDefinition, newRepoDef *model.AutomationDefinition, currentCfg *model.ServiceConfig, newCfg *model.ServiceConfig) ReloadDecision {
	if fieldPath := firstRestartRequiredDefinitionChange(currentRepoDef, newRepoDef); fieldPath != "" {
		return restartRequiredDecision(fieldPath)
	}
	if currentCfg == nil || newCfg == nil {
		return ReloadDecision{Outcome: ReloadOutcomeNoChange}
	}
	if fieldPath := firstRestartRequiredConfigChange(currentCfg, newCfg); fieldPath != "" {
		return restartRequiredDecision(fieldPath)
	}
	if fieldPath := firstHotReloadAllowedChange(currentCfg, newCfg); fieldPath != "" {
		return hotReloadAllowedDecision(fieldPath)
	}
	return ReloadDecision{Outcome: ReloadOutcomeNoChange}
}

func firstRestartRequiredDefinitionChange(currentRepoDef *model.AutomationDefinition, newRepoDef *model.AutomationDefinition) string {
	if currentRepoDef == nil || newRepoDef == nil {
		return ""
	}
	switch {
	case strings.TrimSpace(currentRepoDef.Profile) != strings.TrimSpace(newRepoDef.Profile):
		return "profile"
	case selectedSourceKind(currentRepoDef) != selectedSourceKind(newRepoDef):
		return "source.kind"
	case !enabledSourcesEqual(currentRepoDef, newRepoDef):
		return "selection.enabled_sources"
	case strings.TrimSpace(currentRepoDef.Selection.DispatchFlow) != strings.TrimSpace(newRepoDef.Selection.DispatchFlow):
		return "selection.dispatch_flow"
	default:
		return ""
	}
}

func firstRestartRequiredConfigChange(currentCfg *model.ServiceConfig, newCfg *model.ServiceConfig) string {
	switch {
	case currentCfg.ServerHost != newCfg.ServerHost:
		return "runtime.server.host"
	case !serverPortEqual(currentCfg.ServerPort, newCfg.ServerPort):
		return "runtime.server.port"
	case currentCfg.TrackerKind != newCfg.TrackerKind:
		return "runtime.tracker.kind"
	case currentCfg.TrackerEndpoint != newCfg.TrackerEndpoint:
		return "runtime.tracker.endpoint"
	case currentCfg.TrackerAPIKey != newCfg.TrackerAPIKey:
		return "runtime.tracker.api_key"
	case currentCfg.TrackerProjectSlug != newCfg.TrackerProjectSlug:
		return "runtime.tracker.project_slug"
	case currentCfg.TrackerLinearChildrenBlockParent != newCfg.TrackerLinearChildrenBlockParent:
		return "runtime.tracker.linear.children_block_parent"
	case currentCfg.TrackerRepo != newCfg.TrackerRepo:
		return "runtime.tracker.repo"
	case !reflect.DeepEqual(currentCfg.ActiveStates, newCfg.ActiveStates):
		return "runtime.tracker.active_states"
	case !reflect.DeepEqual(currentCfg.TerminalStates, newCfg.TerminalStates):
		return "runtime.tracker.terminal_states"
	case currentCfg.AutomationRootDir != newCfg.AutomationRootDir:
		return "runtime.automation_root"
	case currentCfg.WorkspaceRoot != newCfg.WorkspaceRoot:
		return "runtime.workspace.root"
	case currentCfg.WorkspaceLinearBranchScope != newCfg.WorkspaceLinearBranchScope:
		return "runtime.workspace.linear_branch_scope"
	case currentCfg.WorkspaceBranchNamespace != newCfg.WorkspaceBranchNamespace:
		return "runtime.workspace.branch_namespace"
	case currentCfg.WorkspaceGitAuthorName != newCfg.WorkspaceGitAuthorName:
		return "runtime.workspace.git.author_name"
	case currentCfg.WorkspaceGitAuthorEmail != newCfg.WorkspaceGitAuthorEmail:
		return "runtime.workspace.git.author_email"
	case !reflect.DeepEqual(currentCfg.HookAfterCreate, newCfg.HookAfterCreate):
		return "runtime.hooks.after_create"
	case !reflect.DeepEqual(currentCfg.HookBeforeRun, newCfg.HookBeforeRun):
		return "runtime.hooks.before_run"
	case !reflect.DeepEqual(currentCfg.HookBeforeRunContinuation, newCfg.HookBeforeRunContinuation):
		return "runtime.hooks.before_run_continuation"
	case !reflect.DeepEqual(currentCfg.HookAfterRun, newCfg.HookAfterRun):
		return "runtime.hooks.after_run"
	case !reflect.DeepEqual(currentCfg.HookBeforeRemove, newCfg.HookBeforeRemove):
		return "runtime.hooks.before_remove"
	case currentCfg.HookTimeoutMS != newCfg.HookTimeoutMS:
		return "runtime.hooks.timeout_ms"
	case currentCfg.MaxTurns != newCfg.MaxTurns:
		return "runtime.agent.max_turns"
	case currentCfg.OrchestratorAutoCloseOnPR != newCfg.OrchestratorAutoCloseOnPR:
		return "runtime.orchestrator.auto_close_on_pr"
	case currentCfg.CodexCommand != newCfg.CodexCommand:
		return "runtime.codex.command"
	case currentCfg.CodexApprovalPolicy != newCfg.CodexApprovalPolicy:
		return "runtime.codex.approval_policy"
	case currentCfg.CodexThreadSandbox != newCfg.CodexThreadSandbox:
		return "runtime.codex.thread_sandbox"
	case currentCfg.CodexTurnSandboxPolicy != newCfg.CodexTurnSandboxPolicy:
		return "runtime.codex.turn_sandbox_policy"
	case currentCfg.CodexTurnTimeoutMS != newCfg.CodexTurnTimeoutMS:
		return "runtime.codex.turn_timeout_ms"
	case !reflect.DeepEqual(currentCfg.SessionPersistence, newCfg.SessionPersistence):
		return "runtime.session_persistence"
	default:
		return ""
	}
}

func firstHotReloadAllowedChange(currentCfg *model.ServiceConfig, newCfg *model.ServiceConfig) string {
	switch {
	case currentCfg.PollIntervalMS != newCfg.PollIntervalMS:
		return "runtime.polling.interval_ms"
	case currentCfg.MaxConcurrentAgents != newCfg.MaxConcurrentAgents:
		return "runtime.agent.max_concurrent_agents"
	case currentCfg.MaxRetryBackoffMS != newCfg.MaxRetryBackoffMS:
		return "runtime.agent.max_retry_backoff_ms"
	case !reflect.DeepEqual(currentCfg.MaxConcurrentAgentsByState, newCfg.MaxConcurrentAgentsByState):
		return "runtime.agent.max_concurrent_agents_by_state"
	case currentCfg.CodexReadTimeoutMS != newCfg.CodexReadTimeoutMS:
		return "runtime.codex.read_timeout_ms"
	case currentCfg.CodexStallTimeoutMS != newCfg.CodexStallTimeoutMS:
		return "runtime.codex.stall_timeout_ms"
	case !reflect.DeepEqual(currentCfg.Notifications, newCfg.Notifications):
		return "runtime.notifications"
	default:
		return ""
	}
}

func restartRequiredDecision(fieldPath string) ReloadDecision {
	reason := contract.MustReason(contract.ReasonRuntimeReloadRestartRequired, map[string]any{
		"field_path": fieldPath,
	})
	return ReloadDecision{
		Outcome:   ReloadOutcomeRestartRequired,
		FieldPath: fieldPath,
		Reason:    &reason,
	}
}

func hotReloadAllowedDecision(fieldPath string) ReloadDecision {
	reason := contract.MustReason(contract.ReasonRuntimeReloadHotReloadAllowed, map[string]any{
		"field_path": fieldPath,
	})
	return ReloadDecision{
		Outcome:   ReloadOutcomeHotReloadAllowed,
		FieldPath: fieldPath,
		Reason:    &reason,
	}
}

func serverPortEqual(left *int, right *int) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func enabledSourcesEqual(left *model.AutomationDefinition, right *model.AutomationDefinition) bool {
	return reflect.DeepEqual(nilSafeSources(left), nilSafeSources(right))
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
	sourceName := selectedSourceName(def)
	if sourceName == "" || def == nil || def.Sources == nil {
		return ""
	}
	sourceDef := def.Sources[sourceName]
	if sourceDef == nil || sourceDef.Raw == nil {
		return ""
	}
	kind, _ := sourceDef.Raw["kind"].(string)
	return strings.TrimSpace(kind)
}

func selectedSourceName(def *model.AutomationDefinition) string {
	if def == nil || len(def.Selection.EnabledSources) == 0 {
		return ""
	}
	return strings.TrimSpace(def.Selection.EnabledSources[0])
}
