package model

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"symphony-go/internal/model/contract"
)

type Issue struct {
	ID          string
	Identifier  string
	Title       string
	Description *string
	Priority    *int
	State       string
	BranchName  *string
	URL         *string
	Labels      []string
	BlockedBy   []BlockerRef
	CreatedAt   *time.Time
	UpdatedAt   *time.Time
}

func CloneIssue(issue *Issue) *Issue {
	if issue == nil {
		return nil
	}
	copyValue := *issue
	copyValue.Labels = append([]string(nil), issue.Labels...)
	copyValue.BlockedBy = append([]BlockerRef(nil), issue.BlockedBy...)
	return &copyValue
}

type BlockerRef struct {
	ID         *string
	Identifier *string
	State      *string
}

type CompletionMode string

const (
	CompletionModeNone        CompletionMode = "none"
	CompletionModePullRequest CompletionMode = "pull_request"
)

type CompletionAction string

const (
	CompletionActionContinue     CompletionAction = "continue"
	CompletionActionIntervention CompletionAction = "intervention"
)

type CompletionContract struct {
	Mode        CompletionMode
	OnMissingPR CompletionAction
	OnClosedPR  CompletionAction
}

type ServiceMode = contract.ServiceMode

const (
	ServiceModeServing     ServiceMode = contract.ServiceModeServing
	ServiceModeDegraded    ServiceMode = contract.ServiceModeDegraded
	ServiceModeUnavailable ServiceMode = contract.ServiceModeUnavailable
)

type DispatchKind string

const (
	DispatchKindFresh             DispatchKind = "fresh"
	DispatchKindContinuation      DispatchKind = "continuation"
	DispatchKindInterventionRetry DispatchKind = "intervention_retry"
)

type ContinuationReason string

const (
	ContinuationReasonUnfinishedIssue          ContinuationReason = "unfinished_issue"
	ContinuationReasonMissingPR                ContinuationReason = "missing_pr"
	ContinuationReasonClosedUnmergedPR         ContinuationReason = "pr_closed_unmerged"
	ContinuationReasonMergedPRNotTerminal      ContinuationReason = "merged_pr_source_not_terminal"
	ContinuationReasonExecutionBudgetExhausted ContinuationReason = "execution_budget_exhausted"
	ContinuationReasonRecoverableRuntimeError  ContinuationReason = "recoverable_runtime_error"
	ContinuationReasonTrackerIssueMissing      ContinuationReason = "tracker_issue_missing_during_recovery"
)

type PRContext struct {
	Number     int
	URL        string
	State      string
	Merged     bool
	HeadBranch string
}

type ReviewFinding struct {
	Code    string `json:"code"`
	Summary string `json:"summary"`
}

type ReviewFeedbackContext struct {
	Summary  string          `json:"summary,omitempty"`
	Findings []ReviewFinding `json:"findings,omitempty"`
}

type DispatchContext struct {
	JobType            contract.JobType
	Kind               DispatchKind
	RetryAttempt       *int
	ExpectedOutcome    CompletionMode
	OnMissingPR        CompletionAction
	OnClosedPR         CompletionAction
	Reason             *ContinuationReason
	PreviousBranch     *string
	PreviousPR         *PRContext
	PreviousIssueState *string
	RecoveryCheckpoint *RecoveryCheckpoint
	ReviewFeedback     *ReviewFeedbackContext
}

func CloneDispatchContext(dispatch *DispatchContext) *DispatchContext {
	if dispatch == nil {
		return nil
	}
	copyValue := *dispatch
	if dispatch.PreviousPR != nil {
		prCopy := *dispatch.PreviousPR
		copyValue.PreviousPR = &prCopy
	}
	if dispatch.RetryAttempt != nil {
		retryAttempt := *dispatch.RetryAttempt
		copyValue.RetryAttempt = &retryAttempt
	}
	if dispatch.Reason != nil {
		reason := *dispatch.Reason
		copyValue.Reason = &reason
	}
	if dispatch.PreviousBranch != nil {
		branch := *dispatch.PreviousBranch
		copyValue.PreviousBranch = &branch
	}
	if dispatch.PreviousIssueState != nil {
		state := *dispatch.PreviousIssueState
		copyValue.PreviousIssueState = &state
	}
	if dispatch.RecoveryCheckpoint != nil {
		copyValue.RecoveryCheckpoint = CloneRecoveryCheckpoint(dispatch.RecoveryCheckpoint)
	}
	if dispatch.ReviewFeedback != nil {
		copyValue.ReviewFeedback = cloneReviewFeedback(dispatch.ReviewFeedback)
	}
	return &copyValue
}

type RunBudget struct {
	TotalMS     int `json:"total_ms,omitempty"`
	ExecutionMS int `json:"execution_ms,omitempty"`
	ReviewFixMS int `json:"review_fix_ms,omitempty"`
}

type RunBudgetUsage struct {
	TotalMS     int `json:"total_ms,omitempty"`
	ExecutionMS int `json:"execution_ms,omitempty"`
	ReviewFixMS int `json:"review_fix_ms,omitempty"`
}

type RecoveryCheckpoint struct {
	ArtifactID    string              `json:"artifact_id,omitempty"`
	PatchPath     string              `json:"patch_path,omitempty"`
	WorkspacePath string              `json:"workspace_path,omitempty"`
	HiddenRef     string              `json:"hidden_ref,omitempty"`
	HiddenCommit  string              `json:"hidden_commit,omitempty"`
	Checkpoint    contract.Checkpoint `json:"checkpoint"`
}

type RunState struct {
	Object            contract.Run                `json:"object"`
	Attempt           int                         `json:"attempt"`
	State             contract.RunStatus          `json:"state"`
	Phase             contract.RunPhaseSummary    `json:"phase"`
	CandidateDelivery *contract.CandidateDelivery `json:"candidate_delivery,omitempty"`
	ReviewGate        *contract.ReviewGate        `json:"review_gate,omitempty"`
	ReviewSummary     string                      `json:"review_summary,omitempty"`
	ReviewFindings    []ReviewFinding             `json:"review_findings,omitempty"`
	Checkpoints       []contract.Checkpoint       `json:"checkpoints,omitempty"`
	Budget            RunBudget                   `json:"budget,omitempty"`
	Usage             RunBudgetUsage              `json:"usage,omitempty"`
	Reason            *contract.Reason            `json:"reason,omitempty"`
	Decision          *contract.Decision          `json:"decision,omitempty"`
	ErrorCode         contract.ErrorCode          `json:"error_code,omitempty"`
	Recovery          *RecoveryCheckpoint         `json:"recovery,omitempty"`
}

func CloneRecoveryCheckpoint(value *RecoveryCheckpoint) *RecoveryCheckpoint {
	if value == nil {
		return nil
	}
	copyValue := *value
	copyValue.Checkpoint = cloneCheckpoint(value.Checkpoint)
	return &copyValue
}

func CloneRunState(value *RunState) *RunState {
	if value == nil {
		return nil
	}
	copyValue := *value
	copyValue.CandidateDelivery = cloneCandidateDelivery(value.CandidateDelivery)
	copyValue.ReviewGate = cloneReviewGate(value.ReviewGate)
	copyValue.ReviewFindings = cloneReviewFindings(value.ReviewFindings)
	copyValue.Checkpoints = cloneCheckpoints(value.Checkpoints)
	copyValue.Reason = cloneContractReason(value.Reason)
	copyValue.Decision = cloneContractDecision(value.Decision)
	copyValue.Recovery = CloneRecoveryCheckpoint(value.Recovery)
	copyValue.Object = value.Object
	copyValue.Object.Relations = append([]contract.ObjectRelation(nil), value.Object.Relations...)
	copyValue.Object.References = append([]contract.Reference(nil), value.Object.References...)
	if len(value.Object.Reasons) > 0 {
		copyValue.Object.Reasons = make([]contract.Reason, 0, len(value.Object.Reasons))
		for _, reason := range value.Object.Reasons {
			cloned := reason
			cloned.Details = cloneDetails(reason.Details)
			copyValue.Object.Reasons = append(copyValue.Object.Reasons, cloned)
		}
	}
	copyValue.Object.Decision = cloneContractDecision(value.Object.Decision)
	return &copyValue
}

func cloneReviewFeedback(value *ReviewFeedbackContext) *ReviewFeedbackContext {
	if value == nil {
		return nil
	}
	copyValue := *value
	copyValue.Findings = cloneReviewFindings(value.Findings)
	return &copyValue
}

func cloneReviewGate(value *contract.ReviewGate) *contract.ReviewGate {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func cloneReviewFindings(value []ReviewFinding) []ReviewFinding {
	if len(value) == 0 {
		return nil
	}
	result := make([]ReviewFinding, len(value))
	copy(result, value)
	return result
}

func cloneCandidateDelivery(value *contract.CandidateDelivery) *contract.CandidateDelivery {
	if value == nil {
		return nil
	}
	copyValue := *value
	copyValue.ArtifactIDs = append([]string(nil), value.ArtifactIDs...)
	return &copyValue
}

func cloneCheckpoints(value []contract.Checkpoint) []contract.Checkpoint {
	if len(value) == 0 {
		return nil
	}
	result := make([]contract.Checkpoint, len(value))
	for i, item := range value {
		result[i] = cloneCheckpoint(item)
	}
	return result
}

func cloneCheckpoint(value contract.Checkpoint) contract.Checkpoint {
	copyValue := value
	copyValue.ArtifactIDs = append([]string(nil), value.ArtifactIDs...)
	copyValue.ReferenceIDs = append([]string(nil), value.ReferenceIDs...)
	copyValue.Reason = cloneContractReason(value.Reason)
	return copyValue
}

func cloneContractReason(value *contract.Reason) *contract.Reason {
	if value == nil {
		return nil
	}
	copyValue := *value
	if value.Details != nil {
		copyValue.Details = cloneDetails(value.Details)
	}
	return &copyValue
}

func cloneContractDecision(value *contract.Decision) *contract.Decision {
	if value == nil {
		return nil
	}
	copyValue := *value
	copyValue.RecommendedActions = append([]contract.DecisionAction(nil), value.RecommendedActions...)
	if value.Details != nil {
		copyValue.Details = cloneDetails(value.Details)
	}
	return &copyValue
}

func cloneDetails(details map[string]any) map[string]any {
	if len(details) == 0 {
		return nil
	}
	result := make(map[string]any, len(details))
	for key, value := range details {
		result[key] = value
	}
	return result
}

type InterventionHandoff struct {
	Phase              contract.RunPhaseSummary          `json:"phase"`
	Reason             *contract.Reason                  `json:"reason,omitempty"`
	Decision           *contract.Decision                `json:"decision,omitempty"`
	ReviewSummary      string                            `json:"review_summary,omitempty"`
	ReviewFindings     []ReviewFinding                   `json:"review_findings,omitempty"`
	Checkpoint         *contract.Checkpoint              `json:"checkpoint,omitempty"`
	RecommendedActions []contract.DecisionAction         `json:"recommended_actions,omitempty"`
	RequiredInputs     []contract.InterventionInputField `json:"required_inputs,omitempty"`
}

type InterventionState struct {
	Object  contract.Intervention `json:"object"`
	Handoff InterventionHandoff   `json:"handoff"`
}

func CloneInterventionState(value *InterventionState) *InterventionState {
	if value == nil {
		return nil
	}
	copyValue := *value
	copyValue.Object = cloneInterventionObject(value.Object)
	copyValue.Handoff = cloneInterventionHandoff(value.Handoff)
	return &copyValue
}

func cloneInterventionObject(value contract.Intervention) contract.Intervention {
	copyValue := value
	copyValue.Relations = append([]contract.ObjectRelation(nil), value.Relations...)
	copyValue.References = append([]contract.Reference(nil), value.References...)
	if len(value.Reasons) > 0 {
		copyValue.Reasons = make([]contract.Reason, 0, len(value.Reasons))
		for _, reason := range value.Reasons {
			cloned := reason
			cloned.Details = cloneDetails(reason.Details)
			copyValue.Reasons = append(copyValue.Reasons, cloned)
		}
	}
	copyValue.Decision = cloneContractDecision(value.Decision)
	copyValue.RequiredInputs = cloneInterventionInputs(value.RequiredInputs)
	copyValue.AllowedActions = append([]contract.ControlAction(nil), value.AllowedActions...)
	if value.Resolution != nil {
		resolution := *value.Resolution
		resolution.ProvidedInputs = cloneDetails(value.Resolution.ProvidedInputs)
		resolution.Decision = cloneContractDecision(value.Resolution.Decision)
		copyValue.Resolution = &resolution
	}
	return copyValue
}

func cloneInterventionHandoff(value InterventionHandoff) InterventionHandoff {
	copyValue := value
	copyValue.Reason = cloneContractReason(value.Reason)
	copyValue.Decision = cloneContractDecision(value.Decision)
	if value.Checkpoint != nil {
		checkpoint := cloneCheckpoint(*value.Checkpoint)
		copyValue.Checkpoint = &checkpoint
	}
	copyValue.ReviewFindings = cloneReviewFindings(value.ReviewFindings)
	copyValue.RecommendedActions = append([]contract.DecisionAction(nil), value.RecommendedActions...)
	copyValue.RequiredInputs = cloneInterventionInputs(value.RequiredInputs)
	return copyValue
}

func cloneInterventionInputs(value []contract.InterventionInputField) []contract.InterventionInputField {
	if len(value) == 0 {
		return nil
	}
	result := make([]contract.InterventionInputField, len(value))
	for i, item := range value {
		cloned := item
		cloned.AllowedValues = append([]string(nil), item.AllowedValues...)
		result[i] = cloned
	}
	return result
}

type WorkflowDefinition struct {
	RootDir        string
	Config         map[string]any
	PromptTemplate string
	Source         map[string]any
	Completion     CompletionContract
}

type AutomationDefinition struct {
	RootDir   string
	Profile   string
	Runtime   map[string]any
	Selection AutomationSelection
	Defaults  AutomationDefaults
	Sources   map[string]*SourceDefinition
	Flows     map[string]*FlowDefinition
	Policies  map[string]map[string]any
}

type AutomationSelection struct {
	DispatchFlow   string
	EnabledSources []string
}

type AutomationDefaults struct {
	Profile *string
}

type SourceDefinition struct {
	Name string
	Raw  map[string]any
}

type FlowDefinition struct {
	Name string
	Raw  map[string]any
}

type ServiceConfig struct {
	TrackerKind                      string
	TrackerEndpoint                  string
	TrackerAPIKey                    string
	TrackerProjectSlug               string
	TrackerLinearChildrenBlockParent bool
	TrackerRepo                      string
	ActiveStates                     []string
	TerminalStates                   []string
	DomainID                         string
	PollIntervalMS                   int
	AutomationRootDir                string
	WorkspaceRoot                    string
	WorkspaceLinearBranchScope       string
	WorkspaceBranchNamespace         string
	WorkspaceGitAuthorName           string
	WorkspaceGitAuthorEmail          string
	HookAfterCreate                  *string
	HookBeforeRun                    *string
	HookBeforeRunContinuation        *string
	HookAfterRun                     *string
	HookBeforeRemove                 *string
	HookTimeoutMS                    int
	MaxConcurrentAgents              int
	MaxTurns                         int
	MaxRetryBackoffMS                int
	RunBudgetTotalMS                 int
	RunExecutionBudgetMS             int
	RunReviewFixBudgetMS             int
	MaxConcurrentAgentsByState       map[string]int
	CodexCommand                     string
	CodexApprovalPolicy              string
	CodexThreadSandbox               string
	CodexTurnSandboxPolicy           string
	CodexTurnTimeoutMS               int
	CodexReadTimeoutMS               int
	CodexStallTimeoutMS              int
	ServerHost                       string
	ServerPort                       *int
	ServiceContractVersion           contract.APIVersion
	ServiceInstanceName              string
	CapabilityContract               contract.CapabilityContract
	LeaderRequired                   bool
	TransparentForwarding            bool
	InstanceRole                     contract.InstanceRole
	LeaderHint                       *contract.LeaderHint
	SessionPersistence               SessionPersistenceConfig
	Notifications                    NotificationsConfig
}

type SessionPersistenceKind string

const (
	SessionPersistenceKindFile SessionPersistenceKind = "file"
)

type SessionPersistenceFileConfig struct {
	Path            string
	FlushIntervalMS int
	FsyncOnCritical bool
}

type SessionPersistenceConfig struct {
	Enabled bool
	Kind    SessionPersistenceKind
	File    SessionPersistenceFileConfig
}

type NotificationChannelKind string

const (
	NotificationChannelKindWebhook NotificationChannelKind = "webhook"
	NotificationChannelKindSlack   NotificationChannelKind = "slack"
)

type NotificationEventType string

const (
	NotificationEventIssueDispatched           NotificationEventType = "issue_dispatched"
	NotificationEventIssueCompleted            NotificationEventType = "issue_completed"
	NotificationEventIssueFailed               NotificationEventType = "issue_failed"
	NotificationEventIssueInterventionRequired NotificationEventType = "issue_intervention_required"
	NotificationEventSystemAlert               NotificationEventType = "system_alert"
	NotificationEventSystemAlertCleared        NotificationEventType = "system_alert_cleared"
)

type RuntimeEventFamily string

const (
	RuntimeEventFamilyIssue  RuntimeEventFamily = "issue"
	RuntimeEventFamilyHealth RuntimeEventFamily = "health"
)

type RuntimeEventPriority string

const (
	RuntimeEventPriorityNormal   RuntimeEventPriority = "normal"
	RuntimeEventPriorityCritical RuntimeEventPriority = "critical"
)

type NotificationSubscriptionConfig struct {
	Families []RuntimeEventFamily
	Types    []NotificationEventType
}

type NotificationDeliveryConfig struct {
	TimeoutMS         int
	RetryCount        int
	RetryDelayMS      int
	QueueSize         int
	CriticalQueueSize int
}

type WebhookNotificationConfig struct {
	URL     string
	Headers map[string]string
}

type SlackNotificationConfig struct {
	IncomingWebhookURL string
}

type NotificationChannelConfig struct {
	ID            string
	DisplayName   string
	Kind          NotificationChannelKind
	Subscriptions NotificationSubscriptionConfig
	Delivery      NotificationDeliveryConfig
	Webhook       *WebhookNotificationConfig
	Slack         *SlackNotificationConfig
}

type NotificationsConfig struct {
	Channels []NotificationChannelConfig
	Defaults NotificationDeliveryConfig
}

type RuntimeEventSubject struct {
	IssueID       string `json:"issue_id,omitempty"`
	Identifier    string `json:"identifier,omitempty"`
	WorkspacePath string `json:"workspace_path,omitempty"`
}

type RuntimeEventDispatch struct {
	Kind               string  `json:"kind,omitempty"`
	ExpectedOutcome    string  `json:"expected_outcome,omitempty"`
	ContinuationReason *string `json:"continuation_reason,omitempty"`
	AttemptCount       int     `json:"attempt_count,omitempty"`
}

type RuntimeEventPullRequest struct {
	Number int    `json:"number,omitempty"`
	URL    string `json:"url,omitempty"`
	State  string `json:"state,omitempty"`
	Branch string `json:"branch,omitempty"`
}

type RuntimeEventFailure struct {
	Phase string `json:"phase,omitempty"`
	Error string `json:"error,omitempty"`
}

type RuntimeEventAlert struct {
	Code    string `json:"code,omitempty"`
	Status  string `json:"status,omitempty"`
	Level   string `json:"level,omitempty"`
	Message string `json:"message,omitempty"`
}

type RuntimeEvent struct {
	Version     int                      `json:"version"`
	EventID     string                   `json:"event_id"`
	Type        NotificationEventType    `json:"type"`
	Family      RuntimeEventFamily       `json:"family"`
	Priority    RuntimeEventPriority     `json:"priority"`
	Level       string                   `json:"level"`
	OccurredAt  time.Time                `json:"occurred_at"`
	Summary     string                   `json:"summary"`
	Subject     *RuntimeEventSubject     `json:"subject,omitempty"`
	Dispatch    *RuntimeEventDispatch    `json:"dispatch,omitempty"`
	PullRequest *RuntimeEventPullRequest `json:"pull_request,omitempty"`
	Failure     *RuntimeEventFailure     `json:"failure,omitempty"`
	Alert       *RuntimeEventAlert       `json:"alert,omitempty"`
}

type Workspace struct {
	Path            string
	WorkspaceKey    string
	Identifier      string
	CreatedNow      bool
	BranchNamespace string
	GitAuthorName   string
	GitAuthorEmail  string
	Dispatch        *DispatchContext
}

type RunAttempt struct {
	IssueID         string
	IssueIdentifier string
	Attempt         *int
	WorkspacePath   string
	StartedAt       time.Time
	Status          RunPhase
	Error           *string
}

type LiveSession struct {
	SessionID                string
	ThreadID                 string
	TurnID                   string
	CodexAppServerPID        *string
	LastCodexEvent           *string
	LastCodexTimestamp       *time.Time
	LastCodexMessage         string
	CodexInputTokens         int64
	CodexOutputTokens        int64
	CodexTotalTokens         int64
	LastReportedInputTokens  int64
	LastReportedOutputTokens int64
	LastReportedTotalTokens  int64
	TurnCount                int
}

type ProtectedResultOutcome string

const (
	ProtectedResultOutcomeSucceeded ProtectedResultOutcome = "succeeded"
	ProtectedResultOutcomeFailed    ProtectedResultOutcome = "failed"
)

type ProtectedState struct {
	Reason      string
	EnteredAt   time.Time
	MustRestart bool
}

type ProtectedResultEntry struct {
	Identifier    string
	WorkspacePath string
	Outcome       ProtectedResultOutcome
	Phase         RunPhase
	Error         *string
	FinalBranch   string
	ObservedAt    time.Time
	RetryAttempt  int
	Dispatch      *DispatchContext
}

type RuntimeServiceState struct {
	Mode               ServiceMode
	RecoveryInProgress bool
	Reasons            []contract.Reason
}

type JobLifecycleState string

const (
	JobLifecycleActive               JobLifecycleState = "active"
	JobLifecycleRetryScheduled       JobLifecycleState = "retry_scheduled"
	JobLifecycleAwaitingMerge        JobLifecycleState = "awaiting_merge"
	JobLifecycleAwaitingIntervention JobLifecycleState = "awaiting_intervention"
	JobLifecycleCompleted            JobLifecycleState = "completed"
)

type JobRuntime struct {
	Object           contract.Job
	Lifecycle        JobLifecycleState
	Reason           *contract.Reason
	Observation      *contract.Observation
	UpdatedAt        string
	WorkspacePath    string
	SourceState      string
	PullRequestState string
	RetryDueAt       *time.Time
	RetryAttempt     int
	StallCount       int
	StartedAt        *time.Time
	WorkerCancel     context.CancelFunc
	RetryTimer       *time.Timer
	Session          LiveSession
	Dispatch         *DispatchContext
	NeedsRecovery    bool
	Run              *RunState
	Intervention     *InterventionState
	Outcome          *contract.Outcome
	Artifacts        []contract.Artifact
}

type ArchivedJob struct {
	Object           contract.Job
	Reason           *contract.Reason
	Observation      *contract.Observation
	UpdatedAt        string
	WorkspacePath    string
	SourceState      string
	PullRequestState string
	Dispatch         *DispatchContext
	Run              *RunState
	Intervention     *InterventionState
	Outcome          *contract.Outcome
	Artifacts        []contract.Artifact
}

type OrchestratorState struct {
	PollIntervalMS      int
	MaxConcurrentAgents int
	Service             RuntimeServiceState
	Jobs                map[string]*JobRuntime
	ProtectedResults    map[string]*ProtectedResultEntry
	ArchivedJobs        []ArchivedJob
	CodexTotals         TokenTotals
	CodexRateLimits     any
}

type RunningEntry struct {
	Issue         *Issue
	Identifier    string
	WorkspacePath string
	Session       LiveSession
	RetryAttempt  int
	StallCount    int
	StartedAt     time.Time
	WorkerCancel  context.CancelFunc
	Dispatch      *DispatchContext
}

type TokenTotals struct {
	InputTokens    int64
	OutputTokens   int64
	TotalTokens    int64
	SecondsRunning float64
}

type OrchState int

const (
	OrchUnclaimed OrchState = iota
	OrchClaimed
	OrchRunning
	OrchRetryQueued
	OrchReleased
)

type RunPhase int

const (
	PhasePreparingWorkspace RunPhase = iota
	PhaseBuildingPrompt
	PhaseLaunchingAgent
	PhaseInitializingSession
	PhaseStreamingTurn
	PhaseFinishing
	PhaseSucceeded
	PhaseFailed
	PhaseTimedOut
	PhaseStalled
	PhaseCanceledByReconciliation
)

func (p RunPhase) String() string {
	switch p {
	case PhasePreparingWorkspace:
		return "preparing_workspace"
	case PhaseBuildingPrompt:
		return "building_prompt"
	case PhaseLaunchingAgent:
		return "launching_agent"
	case PhaseInitializingSession:
		return "initializing_session"
	case PhaseStreamingTurn:
		return "streaming_turn"
	case PhaseFinishing:
		return "finishing"
	case PhaseSucceeded:
		return "succeeded"
	case PhaseFailed:
		return "failed"
	case PhaseTimedOut:
		return "timed_out"
	case PhaseStalled:
		return "stalled"
	case PhaseCanceledByReconciliation:
		return "canceled_by_reconciliation"
	default:
		return "unknown"
	}
}

type HardViolationCode string

const (
	HardViolationSubAgent         HardViolationCode = "subagent"
	HardViolationParallelWork     HardViolationCode = "parallel_work"
	HardViolationBannedTool       HardViolationCode = "banned_tool"
	HardViolationOutOfScopeAccess HardViolationCode = "out_of_scope_access"
	HardViolationBypassResultPath HardViolationCode = "bypass_result_path"
	HardViolationPolicyOverride   HardViolationCode = "policy_override"
)

type HardViolationError struct {
	Code    HardViolationCode
	Tool    string
	Message string
}

type WorkflowError struct {
	Code    string
	Message string
	Err     error
}

type WorkspaceError struct {
	Code    string
	Message string
	Err     error
}

type AgentError struct {
	Code    string
	Message string
	Err     error
}

type TrackerError struct {
	Code    string
	Message string
	Err     error
}

var (
	ErrMissingWorkflowFile       = &WorkflowError{Code: "missing_workflow_file"}
	ErrWorkflowParseError        = &WorkflowError{Code: "workflow_parse_error"}
	ErrFrontMatterNotMap         = &WorkflowError{Code: "workflow_front_matter_not_a_map"}
	ErrTemplateParseError        = &WorkflowError{Code: "template_parse_error"}
	ErrTemplateRenderError       = &WorkflowError{Code: "template_render_error"}
	ErrInvalidCodexCommand       = &WorkflowError{Code: "invalid_codex_command"}
	ErrWorkspacePathEscape       = &WorkspaceError{Code: "workspace_path_escape"}
	ErrWorkspacePathConflict     = &WorkspaceError{Code: "workspace_path_conflict"}
	ErrWorkspaceHookFailed       = &WorkspaceError{Code: "workspace_hook_failed"}
	ErrWorkspaceHookTimeout      = &WorkspaceError{Code: "workspace_hook_timeout"}
	ErrCodexNotFound             = &AgentError{Code: "codex_not_found"}
	ErrInvalidWorkspaceCWD       = &AgentError{Code: "invalid_workspace_cwd"}
	ErrResponseTimeout           = &AgentError{Code: "response_timeout"}
	ErrTurnTimeout               = &AgentError{Code: "turn_timeout"}
	ErrHardViolation             = &AgentError{Code: "hard_violation"}
	ErrPortExit                  = &AgentError{Code: "port_exit"}
	ErrResponseError             = &AgentError{Code: "response_error"}
	ErrTurnFailed                = &AgentError{Code: "turn_failed"}
	ErrTurnCancelled             = &AgentError{Code: "turn_cancelled"}
	ErrTurnInputRequired         = &AgentError{Code: "turn_input_required"}
	ErrUnsupportedTrackerKind    = &TrackerError{Code: "unsupported_tracker_kind"}
	ErrMissingTrackerAPIKey      = &TrackerError{Code: "missing_tracker_api_key"}
	ErrMissingTrackerProjectSlug = &TrackerError{Code: "missing_tracker_project_slug"}
	ErrLinearAPIRequest          = &TrackerError{Code: "linear_api_request"}
	ErrLinearAPIStatus           = &TrackerError{Code: "linear_api_status"}
	ErrLinearGraphQLErrors       = &TrackerError{Code: "linear_graphql_errors"}
	ErrLinearUnknownPayload      = &TrackerError{Code: "linear_unknown_payload"}
	ErrLinearMissingEndCursor    = &TrackerError{Code: "linear_missing_end_cursor"}
)

var workspaceKeyPattern = regexp.MustCompile(`[^A-Za-z0-9._-]`)

func NormalizeState(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func SanitizeWorkspaceKey(value string) string {
	return workspaceKeyPattern.ReplaceAllString(strings.TrimSpace(value), "_")
}

func NewWorkflowError(kind *WorkflowError, message string, err error) error {
	code := ""
	if kind != nil {
		code = kind.Code
	}

	return &WorkflowError{Code: code, Message: message, Err: err}
}

func NewWorkspaceError(kind *WorkspaceError, message string, err error) error {
	code := ""
	if kind != nil {
		code = kind.Code
	}

	return &WorkspaceError{Code: code, Message: message, Err: err}
}

func NewAgentError(kind *AgentError, message string, err error) error {
	code := ""
	if kind != nil {
		code = kind.Code
	}

	return &AgentError{Code: code, Message: message, Err: err}
}

func NewHardViolationError(code HardViolationCode, tool string, message string) error {
	if strings.TrimSpace(message) == "" {
		message = "hard violation detected"
	}
	return &HardViolationError{
		Code:    code,
		Tool:    strings.TrimSpace(tool),
		Message: strings.TrimSpace(message),
	}
}

func AsHardViolation(err error) (*HardViolationError, bool) {
	var target *HardViolationError
	if err == nil || !errors.As(err, &target) || target == nil {
		return nil, false
	}
	copyValue := *target
	return &copyValue, true
}

func NewTrackerError(kind *TrackerError, message string, err error) error {
	code := ""
	if kind != nil {
		code = kind.Code
	}

	return &TrackerError{Code: code, Message: message, Err: err}
}

func (e *WorkflowError) Error() string {
	return formatTypedError(e.Code, e.Message, e.Err)
}

func (e *WorkflowError) Unwrap() error {
	if e == nil {
		return nil
	}

	return e.Err
}

func (e *WorkflowError) Is(target error) bool {
	typed, ok := target.(*WorkflowError)
	if !ok || e == nil || typed == nil {
		return false
	}

	return e.Code != "" && e.Code == typed.Code
}

func (e *WorkspaceError) Error() string {
	return formatTypedError(e.Code, e.Message, e.Err)
}

func (e *WorkspaceError) Unwrap() error {
	if e == nil {
		return nil
	}

	return e.Err
}

func (e *WorkspaceError) Is(target error) bool {
	typed, ok := target.(*WorkspaceError)
	if !ok || e == nil || typed == nil {
		return false
	}

	return e.Code != "" && e.Code == typed.Code
}

func (e *AgentError) Error() string {
	return formatTypedError(e.Code, e.Message, e.Err)
}

func (e *AgentError) Unwrap() error {
	if e == nil {
		return nil
	}

	return e.Err
}

func (e *AgentError) Is(target error) bool {
	typed, ok := target.(*AgentError)
	if !ok || e == nil || typed == nil {
		return false
	}

	return e.Code != "" && e.Code == typed.Code
}

func (e *TrackerError) Error() string {
	return formatTypedError(e.Code, e.Message, e.Err)
}

func (e *HardViolationError) Error() string {
	code := strings.TrimSpace(string(e.Code))
	if code == "" {
		code = ErrHardViolation.Code
	}
	parts := []string{code}
	if strings.TrimSpace(e.Tool) != "" {
		parts = append(parts, "tool="+strings.TrimSpace(e.Tool))
	}
	if strings.TrimSpace(e.Message) != "" {
		parts = append(parts, strings.TrimSpace(e.Message))
	}
	return strings.Join(parts, ": ")
}

func (e *HardViolationError) Is(target error) bool {
	switch typed := target.(type) {
	case *AgentError:
		return typed != nil && typed.Code == ErrHardViolation.Code
	case *HardViolationError:
		if typed == nil {
			return false
		}
		if typed.Code == "" {
			return true
		}
		return typed.Code == e.Code
	default:
		return false
	}
}

func (e *TrackerError) Unwrap() error {
	if e == nil {
		return nil
	}

	return e.Err
}

func (e *TrackerError) Is(target error) bool {
	typed, ok := target.(*TrackerError)
	if !ok || e == nil || typed == nil {
		return false
	}

	return e.Code != "" && e.Code == typed.Code
}

func formatTypedError(code string, message string, err error) string {
	parts := make([]string, 0, 3)
	if code != "" {
		parts = append(parts, code)
	}
	if message != "" {
		parts = append(parts, message)
	}
	if err != nil {
		parts = append(parts, err.Error())
	}

	if len(parts) == 0 {
		return "unknown error"
	}

	return strings.Join(parts, ": ")
}
