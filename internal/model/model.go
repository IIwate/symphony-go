package model

import (
	"context"
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
	ContinuationReasonUnfinishedIssue     ContinuationReason = "unfinished_issue"
	ContinuationReasonMissingPR           ContinuationReason = "missing_pr"
	ContinuationReasonClosedUnmergedPR    ContinuationReason = "pr_closed_unmerged"
	ContinuationReasonMergedPRNotTerminal ContinuationReason = "merged_pr_source_not_terminal"
	ContinuationReasonTrackerIssueMissing ContinuationReason = "tracker_issue_missing_during_recovery"
)

type PRContext struct {
	Number     int
	URL        string
	State      string
	Merged     bool
	HeadBranch string
}

type DispatchContext struct {
	Kind               DispatchKind
	RetryAttempt       *int
	ExpectedOutcome    CompletionMode
	OnMissingPR        CompletionAction
	OnClosedPR         CompletionAction
	Reason             *ContinuationReason
	PreviousBranch     *string
	PreviousPR         *PRContext
	PreviousIssueState *string
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
	return &copyValue
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

type RetryEntry struct {
	IssueID       string
	Identifier    string
	WorkspacePath string
	Attempt       int
	StallCount    int
	DueAt         time.Time
	TimerHandle   *time.Timer
	Error         *string
	Dispatch      *DispatchContext
}

type AwaitingMergeEntry struct {
	Identifier           string
	State                string
	WorkspacePath        string
	Branch               string
	PRNumber             int
	PRURL                string
	PRState              string
	PRBaseOwner          string
	PRBaseRepo           string
	PRHeadOwner          string
	RetryAttempt         int
	StallCount           int
	AwaitingSince        time.Time
	LastError            *string
	PostMergeRetryCount  int
	NextPostMergeRetryAt *time.Time
}

type AwaitingInterventionEntry struct {
	Identifier          string
	WorkspacePath       string
	Branch              string
	PRNumber            int
	PRURL               string
	PRState             string
	PRBaseOwner         string
	PRBaseRepo          string
	PRHeadOwner         string
	RetryAttempt        int
	StallCount          int
	ObservedAt          time.Time
	Reason              string
	ExpectedOutcome     string
	PreviousBranch      string
	LastKnownIssueState string
}

type RecoveryStrategy string

const (
	RecoveryStrategyContinuationRetry RecoveryStrategy = "continuation_retry"
	RecoveryStrategyPostRunResume     RecoveryStrategy = "post_run_resume"
)

type RecoverySource string

const (
	RecoverySourceRunning   RecoverySource = "running"
	RecoverySourceRecovered RecoverySource = "recovered"
	RecoverySourceSucceeded RecoverySource = "succeeded"
)

type RecoveryEntry struct {
	Identifier    string
	WorkspacePath string
	FinalBranch   string
	State         string
	RetryAttempt  int
	StallCount    int
	ObservedAt    time.Time
	Strategy      RecoveryStrategy
	Source        RecoverySource
	Dispatch      *DispatchContext
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

type IssueRecord struct {
	Runtime             contract.IssueRuntimeRecord
	RetryDueAt          *time.Time
	RetryAttempt        int
	StallCount          int
	LastKnownIssue      *Issue
	LastKnownIssueState string
	StartedAt           *time.Time
	WorkerCancel        context.CancelFunc
	RetryTimer          *time.Timer
	Session             LiveSession
	Dispatch            *DispatchContext
	NeedsRecovery       bool
}

type OrchestratorState struct {
	PollIntervalMS      int
	MaxConcurrentAgents int
	Service             RuntimeServiceState
	Records             map[string]*IssueRecord
	ProtectedResults    map[string]*ProtectedResultEntry
	CompletedWindow     []contract.IssueRuntimeRecord
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
