package contract

import "encoding/json"

type APIVersion string

const (
	APIVersionV1 APIVersion = "v1"
)

type CodeCategory string

const (
	CategoryAPI          CodeCategory = "api"
	CategoryAction       CodeCategory = "action"
	CategoryCapability   CodeCategory = "capability"
	CategoryCheckpoint   CodeCategory = "checkpoint"
	CategoryConfig       CodeCategory = "config"
	CategoryControl      CodeCategory = "control"
	CategoryIntervention CodeCategory = "intervention"
	CategoryJob          CodeCategory = "job"
	CategoryOutcome      CodeCategory = "outcome"
	CategoryRecord       CodeCategory = "record"
	CategoryReference    CodeCategory = "reference"
	CategoryRun          CodeCategory = "run"
	CategoryRuntime      CodeCategory = "runtime"
	CategorySecurity     CodeCategory = "security"
	CategoryService      CodeCategory = "service"
)

type ServiceMode string

const (
	ServiceModeServing     ServiceMode = "serving"
	ServiceModeDegraded    ServiceMode = "degraded"
	ServiceModeUnavailable ServiceMode = "unavailable"
)

func (m ServiceMode) IsValid() bool {
	switch m {
	case ServiceModeServing, ServiceModeDegraded, ServiceModeUnavailable:
		return true
	default:
		return false
	}
}

type IssueStatus string

const (
	IssueStatusActive               IssueStatus = "active"
	IssueStatusRetryScheduled       IssueStatus = "retry_scheduled"
	IssueStatusAwaitingMerge        IssueStatus = "awaiting_merge"
	IssueStatusAwaitingIntervention IssueStatus = "awaiting_intervention"
	IssueStatusCompleted            IssueStatus = "completed"
)

func (s IssueStatus) IsValid() bool {
	switch s {
	case IssueStatusActive, IssueStatusRetryScheduled, IssueStatusAwaitingMerge, IssueStatusAwaitingIntervention, IssueStatusCompleted:
		return true
	default:
		return false
	}
}

type RecordID string

type SourceKind string

const (
	SourceKindLinear       SourceKind = "linear"
	SourceKindGitHubIssues SourceKind = "github_issues"
)

func (k SourceKind) IsValid() bool {
	switch k {
	case SourceKindLinear, SourceKindGitHubIssues:
		return true
	default:
		return false
	}
}

type SourceRef struct {
	SourceKind       SourceKind `json:"source_kind"`
	SourceName       string     `json:"source_name"`
	SourceID         string     `json:"source_id"`
	SourceIdentifier string     `json:"source_identifier"`
	URL              string     `json:"url"`
}

type ReasonCode string

type Reason struct {
	ID              string          `json:"id,omitempty"`
	ObjectType      ObjectType      `json:"object_type,omitempty"`
	DomainID        string          `json:"domain_id,omitempty"`
	ContractVersion APIVersion      `json:"contract_version,omitempty"`
	CreatedAt       string          `json:"created_at,omitempty"`
	UpdatedAt       string          `json:"updated_at,omitempty"`
	Visibility      VisibilityLevel `json:"visibility,omitempty"`
	ReasonCode      ReasonCode      `json:"reason_code"`
	Category        CodeCategory    `json:"category"`
	Summary         string          `json:"summary,omitempty"`
	Details         map[string]any  `json:"details"`
	Extensions      Extensions      `json:"extensions,omitempty"`
}

type ErrorCode string

type ErrorResponse struct {
	ErrorCode ErrorCode      `json:"error_code"`
	Message   string         `json:"message"`
	Category  CodeCategory   `json:"category"`
	Retryable bool           `json:"retryable"`
	Details   map[string]any `json:"details"`
}

type ControlAction string

const (
	ControlActionRefresh             ControlAction = "refresh"
	ControlActionCancel              ControlAction = "cancel"
	ControlActionRetry               ControlAction = "retry"
	ControlActionResume              ControlAction = "resume"
	ControlActionResolveIntervention ControlAction = "resolve_intervention"
	ControlActionTerminate           ControlAction = "terminate"
)

type ControlStatus string

const (
	ControlStatusAccepted ControlStatus = "accepted"
	ControlStatusRejected ControlStatus = "rejected"
)

type ControlResult struct {
	Action              ControlAction `json:"action"`
	Status              ControlStatus `json:"status"`
	Reason              *Reason       `json:"reason"`
	RecommendedNextStep string        `json:"recommended_next_step"`
	Timestamp           string        `json:"timestamp"`
}

type Observation struct {
	Running bool           `json:"running"`
	Summary string         `json:"summary"`
	Details map[string]any `json:"details"`
}

type ResultOutcome string

const (
	ResultOutcomeSucceeded ResultOutcome = "succeeded"
	ResultOutcomeFailed    ResultOutcome = "failed"
	ResultOutcomeAbandoned ResultOutcome = "abandoned"
)

type Result struct {
	Outcome     ResultOutcome  `json:"outcome"`
	Summary     string         `json:"summary"`
	CompletedAt string         `json:"completed_at"`
	Details     map[string]any `json:"details"`
}

type WorkspaceRef struct {
	Path string `json:"path,omitempty"`
}

type BranchRef struct {
	Name string `json:"name,omitempty"`
}

type PullRequestRef struct {
	Number int    `json:"number,omitempty"`
	URL    string `json:"url,omitempty"`
	State  string `json:"state,omitempty"`
}

type DurableRefs struct {
	Workspace   *WorkspaceRef   `json:"workspace,omitempty"`
	Branch      *BranchRef      `json:"branch,omitempty"`
	PullRequest *PullRequestRef `json:"pull_request,omitempty"`
	LedgerPath  string          `json:"ledger_path"`
}

type IssueRuntimeRecord struct {
	RecordID    RecordID     `json:"record_id"`
	SourceRef   SourceRef    `json:"source_ref"`
	Status      IssueStatus  `json:"status"`
	UpdatedAt   string       `json:"updated_at"`
	Reason      *Reason      `json:"reason"`
	Observation *Observation `json:"observation"`
	DurableRefs DurableRefs  `json:"durable_refs"`
	Result      *Result      `json:"result"`
}

type IssueLedgerRecord struct {
	RecordID    RecordID    `json:"record_id"`
	SourceRef   SourceRef   `json:"source_ref"`
	Status      IssueStatus `json:"status"`
	Reason      *Reason     `json:"reason"`
	RetryDueAt  *string     `json:"retry_due_at"`
	DurableRefs DurableRefs `json:"durable_refs"`
	Result      *Result     `json:"result"`
	UpdatedAt   string      `json:"updated_at"`
}

type InstanceDocument struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version string `json:"version"`
}

type LeaderHint struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
	URL  string `json:"url,omitempty"`
}

type InstanceStateSummary struct {
	ID      string       `json:"id"`
	Name    string       `json:"name"`
	Version string       `json:"version"`
	Role    InstanceRole `json:"role"`
}

type SourceDocument struct {
	Kind SourceKind `json:"kind"`
	Name string     `json:"name"`
}

type DiscoveryDocument struct {
	APIVersion   APIVersion          `json:"api_version"`
	Instance     InstanceDocument    `json:"instance"`
	DomainID     string              `json:"domain_id"`
	Source       SourceDocument      `json:"source"`
	Capabilities StaticCapabilitySet `json:"capabilities"`
}

type ServiceStateSnapshot struct {
	GeneratedAt        string                 `json:"generated_at"`
	ServiceMode        ServiceMode            `json:"service_mode"`
	RecoveryInProgress bool                   `json:"recovery_in_progress"`
	Reasons            []Reason               `json:"reasons"`
	Instance           InstanceStateSummary   `json:"instance"`
	Leader             *LeaderHint            `json:"leader,omitempty"`
	Capabilities       AvailableCapabilitySet `json:"capabilities"`
}

type EventType string

const (
	EventTypeSnapshot            EventType = "snapshot"
	EventTypeServiceStateChanged EventType = "service_state_changed"
	EventTypeObjectChanged       EventType = "object_changed"
)

type EventObject struct {
	ObjectType ObjectType      `json:"object_type"`
	ObjectID   string          `json:"object_id"`
	State      string          `json:"state,omitempty"`
	Visibility VisibilityLevel `json:"visibility,omitempty"`
}

type EventEnvelope struct {
	EventID         string        `json:"event_id"`
	EventType       EventType     `json:"event_type"`
	Timestamp       string        `json:"timestamp"`
	ContractVersion APIVersion    `json:"contract_version"`
	DomainID        string        `json:"domain_id"`
	ServiceMode     ServiceMode   `json:"service_mode"`
	Objects         []EventObject `json:"objects,omitempty"`
	Reason          *Reason       `json:"reason"`
}

type ObjectQueryResponse struct {
	ObjectType ObjectType      `json:"object_type"`
	Item       json.RawMessage `json:"item"`
}

type ObjectListResponse struct {
	ObjectType ObjectType        `json:"object_type"`
	Items      []json.RawMessage `json:"items"`
}
