package model

import (
	"context"
	"regexp"
	"strings"
	"time"
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

type BlockerRef struct {
	ID         *string
	Identifier *string
	State      *string
}

type WorkflowDefinition struct {
	Config         map[string]any
	PromptTemplate string
}

type ServiceConfig struct {
	TrackerKind                string
	TrackerEndpoint            string
	TrackerAPIKey              string
	TrackerProjectSlug         string
	TrackerRepo                string
	ActiveStates               []string
	TerminalStates             []string
	PollIntervalMS             int
	WorkspaceRoot              string
	WorkspaceLinearBranchScope string
	HookAfterCreate            *string
	HookBeforeRun              *string
	HookAfterRun               *string
	HookBeforeRemove           *string
	HookTimeoutMS              int
	MaxConcurrentAgents        int
	MaxTurns                   int
	MaxRetryBackoffMS          int
	MaxConcurrentAgentsByState map[string]int
	OrchestratorAutoCloseOnPR  bool
	CodexCommand               string
	CodexApprovalPolicy        string
	CodexThreadSandbox         string
	CodexTurnSandboxPolicy     string
	CodexTurnTimeoutMS         int
	CodexReadTimeoutMS         int
	CodexStallTimeoutMS        int
	ServerPort                 *int
}

type Workspace struct {
	Path         string
	WorkspaceKey string
	Identifier   string
	CreatedNow   bool
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
}

type OrchestratorState struct {
	PollIntervalMS      int
	MaxConcurrentAgents int
	Running             map[string]*RunningEntry
	Claimed             map[string]struct{}
	RetryAttempts       map[string]*RetryEntry
	Completed           map[string]struct{}
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
	ErrLinearStateNotFound       = &TrackerError{Code: "linear_state_not_found"}
	ErrLinearTransitionFailed    = &TrackerError{Code: "linear_transition_failed"}
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
