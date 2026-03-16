package contract

import (
	"fmt"
	"regexp"
	"strings"
)

type ObjectType string

const (
	ObjectTypeJob          ObjectType = "job"
	ObjectTypeRun          ObjectType = "run"
	ObjectTypeIntervention ObjectType = "intervention"
	ObjectTypeOutcome      ObjectType = "outcome"
	ObjectTypeArtifact     ObjectType = "artifact"
	ObjectTypeAction       ObjectType = "action"
	ObjectTypeInstance     ObjectType = "instance"
	ObjectTypeReference    ObjectType = "reference"
	ObjectTypeReason       ObjectType = "reason"
	ObjectTypeDecision     ObjectType = "decision"
)

func (t ObjectType) IsValid() bool {
	switch t {
	case ObjectTypeJob, ObjectTypeRun, ObjectTypeIntervention, ObjectTypeOutcome, ObjectTypeArtifact, ObjectTypeAction, ObjectTypeInstance, ObjectTypeReference, ObjectTypeReason, ObjectTypeDecision:
		return true
	default:
		return false
	}
}

type VisibilityLevel string

const (
	VisibilityLevelSummary    VisibilityLevel = "summary"
	VisibilityLevelRestricted VisibilityLevel = "restricted"
	VisibilityLevelSensitive  VisibilityLevel = "sensitive"
)

func (v VisibilityLevel) IsValid() bool {
	switch v {
	case VisibilityLevelSummary, VisibilityLevelRestricted, VisibilityLevelSensitive:
		return true
	default:
		return false
	}
}

type SensitiveFieldClass string

const (
	SensitiveFieldClassOperationalMetadata SensitiveFieldClass = "operational_metadata"
	SensitiveFieldClassExternalReference   SensitiveFieldClass = "external_reference"
	SensitiveFieldClassHumanInput          SensitiveFieldClass = "human_input"
	SensitiveFieldClassArtifactLocator     SensitiveFieldClass = "artifact_locator"
	SensitiveFieldClassSecretReference     SensitiveFieldClass = "secret_reference"
)

func (c SensitiveFieldClass) IsValid() bool {
	switch c {
	case SensitiveFieldClassOperationalMetadata, SensitiveFieldClassExternalReference, SensitiveFieldClassHumanInput, SensitiveFieldClassArtifactLocator, SensitiveFieldClassSecretReference:
		return true
	default:
		return false
	}
}

type SensitiveFieldRule struct {
	FieldPath         string              `json:"field_path"`
	Class             SensitiveFieldClass `json:"class"`
	MinimumVisibility VisibilityLevel     `json:"minimum_visibility"`
	Redaction         string              `json:"redaction"`
}

func (r SensitiveFieldRule) Validate() error {
	if r.FieldPath == "" {
		return fmt.Errorf("field_path is required")
	}
	if !r.Class.IsValid() {
		return fmt.Errorf("invalid sensitive field class %q", r.Class)
	}
	if !r.MinimumVisibility.IsValid() {
		return fmt.Errorf("invalid minimum visibility %q", r.MinimumVisibility)
	}
	if r.Redaction == "" {
		return fmt.Errorf("redaction is required")
	}
	return nil
}

type ExtensionNamespace string

type Extensions map[ExtensionNamespace]map[string]any

var extensionNamespacePattern = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:\.[a-z0-9]+(?:[._-][a-z0-9]+)*)+$`)

func (n ExtensionNamespace) IsValid() bool {
	return extensionNamespacePattern.MatchString(string(n))
}

func (e Extensions) Validate() error {
	for namespace, fields := range e {
		if !namespace.IsValid() {
			return fmt.Errorf("invalid extension namespace %q", namespace)
		}
		if len(fields) == 0 {
			return fmt.Errorf("extension namespace %q must contain at least one field", namespace)
		}
		for key := range fields {
			if key == "" {
				return fmt.Errorf("extension namespace %q contains an empty key", namespace)
			}
		}
	}
	return nil
}

func cloneExtensions(extensions Extensions) Extensions {
	if len(extensions) == 0 {
		return nil
	}
	clone := make(Extensions, len(extensions))
	for namespace, fields := range extensions {
		clone[namespace] = cloneDetails(fields)
	}
	return clone
}

type RelationType string

const (
	RelationTypeJobRun          RelationType = "job.run"
	RelationTypeJobAction       RelationType = "job.action"
	RelationTypeRunIntervention RelationType = "run.intervention"
	RelationTypeRunOutcome      RelationType = "run.outcome"
	RelationTypeOutcomeArtifact RelationType = "outcome.artifact"
	RelationTypeActionReference RelationType = "action.reference"
)

func (t RelationType) IsValid() bool {
	switch t {
	case RelationTypeJobRun, RelationTypeJobAction, RelationTypeRunIntervention, RelationTypeRunOutcome, RelationTypeOutcomeArtifact, RelationTypeActionReference:
		return true
	default:
		return false
	}
}

type ObjectRelation struct {
	Type       RelationType `json:"type"`
	TargetID   string       `json:"target_id"`
	TargetType ObjectType   `json:"target_type"`
}

var coreRelationTargets = map[ObjectType]map[RelationType]ObjectType{
	ObjectTypeJob: {
		RelationTypeJobRun:    ObjectTypeRun,
		RelationTypeJobAction: ObjectTypeAction,
	},
	ObjectTypeRun: {
		RelationTypeRunIntervention: ObjectTypeIntervention,
		RelationTypeRunOutcome:      ObjectTypeOutcome,
	},
	ObjectTypeOutcome: {
		RelationTypeOutcomeArtifact: ObjectTypeArtifact,
	},
	ObjectTypeAction: {
		RelationTypeActionReference: ObjectTypeReference,
	},
}

func (r ObjectRelation) ValidateForSource(source ObjectType) error {
	if !source.IsValid() {
		return fmt.Errorf("invalid source object type %q", source)
	}
	if !r.Type.IsValid() {
		return fmt.Errorf("invalid relation type %q", r.Type)
	}
	if strings.TrimSpace(r.TargetID) == "" {
		return fmt.Errorf("relation %q target_id is required", r.Type)
	}
	if !r.TargetType.IsValid() {
		return fmt.Errorf("relation %q has invalid target type %q", r.Type, r.TargetType)
	}
	allowedTargets, ok := coreRelationTargets[source]
	if !ok {
		return fmt.Errorf("object type %q does not define formal outbound relations", source)
	}
	expectedTarget, ok := allowedTargets[r.Type]
	if !ok {
		return fmt.Errorf("relation %q is not allowed for source object type %q", r.Type, source)
	}
	if r.TargetType != expectedTarget {
		return fmt.Errorf("relation %q from %q must target %q, got %q", r.Type, source, expectedTarget, r.TargetType)
	}
	return nil
}

type CapabilityCategory string

const (
	CapabilityCategoryProtocol CapabilityCategory = "protocol"
	CapabilityCategorySource   CapabilityCategory = "source"
	CapabilityCategoryExecutor CapabilityCategory = "executor"
	CapabilityCategoryStorage  CapabilityCategory = "storage"
	CapabilityCategoryControl  CapabilityCategory = "control"
	CapabilityCategoryQuery    CapabilityCategory = "query"
	CapabilityCategorySecurity CapabilityCategory = "security"
)

func (c CapabilityCategory) IsValid() bool {
	switch c {
	case CapabilityCategoryProtocol, CapabilityCategorySource, CapabilityCategoryExecutor, CapabilityCategoryStorage, CapabilityCategoryControl, CapabilityCategoryQuery, CapabilityCategorySecurity:
		return true
	default:
		return false
	}
}

type CapabilityName string

const (
	CapabilitySubmitJob           CapabilityName = "submit_job"
	CapabilityStreamEvents        CapabilityName = "stream_events"
	CapabilityQueryObjects        CapabilityName = "query_objects"
	CapabilityServiceRefresh      CapabilityName = "service_refresh"
	CapabilitySourceClosure       CapabilityName = "source_closure"
	CapabilityDirectJobSource     CapabilityName = "direct_job_source"
	CapabilityLinearSource        CapabilityName = "linear_source"
	CapabilityCodexExecutor       CapabilityName = "codex_executor"
	CapabilityRelationalLedger    CapabilityName = "relational_ledger"
	CapabilityFileLedger          CapabilityName = "file_ledger"
	CapabilityIdentityAuth        CapabilityName = "identity_auth"
	CapabilityDomainAccessControl CapabilityName = "domain_access_control"
	CapabilityActionAuthorization CapabilityName = "action_authorization"
)

func (c CapabilityName) IsValid() bool {
	switch c {
	case CapabilitySubmitJob, CapabilityStreamEvents, CapabilityQueryObjects, CapabilityServiceRefresh, CapabilitySourceClosure, CapabilityDirectJobSource, CapabilityLinearSource, CapabilityCodexExecutor, CapabilityRelationalLedger, CapabilityFileLedger, CapabilityIdentityAuth, CapabilityDomainAccessControl, CapabilityActionAuthorization:
		return true
	default:
		return false
	}
}

type StaticCapability struct {
	Name      CapabilityName     `json:"name"`
	Category  CapabilityCategory `json:"category"`
	Summary   string             `json:"summary"`
	Supported bool               `json:"supported"`
}

type AvailableCapability struct {
	Name      CapabilityName     `json:"name"`
	Category  CapabilityCategory `json:"category"`
	Summary   string             `json:"summary"`
	Available bool               `json:"available"`
	Reasons   []Reason           `json:"reasons,omitempty"`
}

type StaticCapabilitySet struct {
	Capabilities []StaticCapability `json:"capabilities"`
}

type AvailableCapabilitySet struct {
	Capabilities []AvailableCapability `json:"capabilities"`
}

type CapabilityContract struct {
	Static    StaticCapabilitySet    `json:"static"`
	Available AvailableCapabilitySet `json:"available"`
}

type JobType string

const (
	JobTypeCodeChange JobType = "code_change"
	JobTypeLandChange JobType = "land_change"
	JobTypeAnalysis   JobType = "analysis"
	JobTypeDiagnostic JobType = "diagnostic"
)

func (t JobType) IsValid() bool {
	switch t {
	case JobTypeCodeChange, JobTypeLandChange, JobTypeAnalysis, JobTypeDiagnostic:
		return true
	default:
		return false
	}
}

type JobStatus string

const (
	JobStatusQueued               JobStatus = "queued"
	JobStatusRunning              JobStatus = "running"
	JobStatusInterventionRequired JobStatus = "intervention_required"
	JobStatusCompleted            JobStatus = "completed"
	JobStatusFailed               JobStatus = "failed"
	JobStatusCanceled             JobStatus = "canceled"
	JobStatusRejected             JobStatus = "rejected"
	JobStatusAbandoned            JobStatus = "abandoned"
)

func (s JobStatus) IsValid() bool {
	switch s {
	case JobStatusQueued, JobStatusRunning, JobStatusInterventionRequired, JobStatusCompleted, JobStatusFailed, JobStatusCanceled, JobStatusRejected, JobStatusAbandoned:
		return true
	default:
		return false
	}
}

func (s JobStatus) IsTerminal() bool {
	switch s {
	case JobStatusCompleted, JobStatusFailed, JobStatusCanceled, JobStatusRejected, JobStatusAbandoned:
		return true
	default:
		return false
	}
}

type RunStatus string

const (
	RunStatusQueued               RunStatus = "queued"
	RunStatusRunning              RunStatus = "running"
	RunStatusInterventionRequired RunStatus = "intervention_required"
	RunStatusContinuationPending  RunStatus = "continuation_pending"
	RunStatusCompleted            RunStatus = "completed"
	RunStatusFailed               RunStatus = "failed"
	RunStatusCanceled             RunStatus = "canceled"
	RunStatusAbandoned            RunStatus = "abandoned"
)

var runStatusTransitions = map[RunStatus][]RunStatus{
	RunStatusQueued:               {RunStatusRunning, RunStatusCanceled, RunStatusAbandoned},
	RunStatusRunning:              {RunStatusInterventionRequired, RunStatusContinuationPending, RunStatusCompleted, RunStatusFailed, RunStatusCanceled, RunStatusAbandoned},
	RunStatusInterventionRequired: {RunStatusCanceled, RunStatusAbandoned},
	RunStatusContinuationPending:  {RunStatusCanceled, RunStatusAbandoned},
	RunStatusCompleted:            {},
	RunStatusFailed:               {},
	RunStatusCanceled:             {},
	RunStatusAbandoned:            {},
}

func (s RunStatus) IsValid() bool {
	_, ok := runStatusTransitions[s]
	return ok
}

func (s RunStatus) CanTransitionTo(next RunStatus) bool {
	for _, candidate := range runStatusTransitions[s] {
		if candidate == next {
			return true
		}
	}
	return false
}

func AllowedRunTransitions(status RunStatus) []RunStatus {
	transitions := runStatusTransitions[status]
	if len(transitions) == 0 {
		return []RunStatus{}
	}
	clone := make([]RunStatus, len(transitions))
	copy(clone, transitions)
	return clone
}

type RunPhaseSummary string

const (
	RunPhaseSummaryPreparing  RunPhaseSummary = "preparing"
	RunPhaseSummaryExecuting  RunPhaseSummary = "executing"
	RunPhaseSummaryVerifying  RunPhaseSummary = "verifying"
	RunPhaseSummaryPublishing RunPhaseSummary = "publishing"
	RunPhaseSummaryBlocked    RunPhaseSummary = "blocked"
)

func (s RunPhaseSummary) IsValid() bool {
	switch s {
	case RunPhaseSummaryPreparing, RunPhaseSummaryExecuting, RunPhaseSummaryVerifying, RunPhaseSummaryPublishing, RunPhaseSummaryBlocked:
		return true
	default:
		return false
	}
}

type InterventionStatus string

const (
	InterventionStatusOpen     InterventionStatus = "open"
	InterventionStatusResolved InterventionStatus = "resolved"
	InterventionStatusCanceled InterventionStatus = "canceled"
	InterventionStatusExpired  InterventionStatus = "expired"
)

var interventionStatusTransitions = map[InterventionStatus][]InterventionStatus{
	InterventionStatusOpen:     {InterventionStatusResolved, InterventionStatusCanceled, InterventionStatusExpired},
	InterventionStatusResolved: {},
	InterventionStatusCanceled: {},
	InterventionStatusExpired:  {},
}

func (s InterventionStatus) IsValid() bool {
	_, ok := interventionStatusTransitions[s]
	return ok
}

func (s InterventionStatus) CanTransitionTo(next InterventionStatus) bool {
	for _, candidate := range interventionStatusTransitions[s] {
		if candidate == next {
			return true
		}
	}
	return false
}

func AllowedInterventionTransitions(status InterventionStatus) []InterventionStatus {
	transitions := interventionStatusTransitions[status]
	if len(transitions) == 0 {
		return []InterventionStatus{}
	}
	clone := make([]InterventionStatus, len(transitions))
	copy(clone, transitions)
	return clone
}

type OutcomeConclusion string

const (
	OutcomeConclusionSucceeded       OutcomeConclusion = "succeeded"
	OutcomeConclusionFailed          OutcomeConclusion = "failed"
	OutcomeConclusionCanceled        OutcomeConclusion = "canceled"
	OutcomeConclusionRejected        OutcomeConclusion = "rejected"
	OutcomeConclusionAbandoned       OutcomeConclusion = "abandoned"
	OutcomeConclusionHumanTerminated OutcomeConclusion = "human_terminated"
)

func (c OutcomeConclusion) IsValid() bool {
	switch c {
	case OutcomeConclusionSucceeded, OutcomeConclusionFailed, OutcomeConclusionCanceled, OutcomeConclusionRejected, OutcomeConclusionAbandoned, OutcomeConclusionHumanTerminated:
		return true
	default:
		return false
	}
}

type ActionType string

const (
	ActionTypeSourceClosure ActionType = "source_closure"
)

func (t ActionType) IsValid() bool {
	switch t {
	case ActionTypeSourceClosure:
		return true
	default:
		return false
	}
}

type ActionStatus string

const (
	ActionStatusQueued               ActionStatus = "queued"
	ActionStatusRunning              ActionStatus = "running"
	ActionStatusExternalPending      ActionStatus = "external_pending"
	ActionStatusInterventionRequired ActionStatus = "intervention_required"
	ActionStatusCompleted            ActionStatus = "completed"
	ActionStatusFailed               ActionStatus = "failed"
	ActionStatusCanceled             ActionStatus = "canceled"
)

var actionStatusTransitions = map[ActionStatus][]ActionStatus{
	ActionStatusQueued:               {ActionStatusRunning, ActionStatusCanceled},
	ActionStatusRunning:              {ActionStatusExternalPending, ActionStatusInterventionRequired, ActionStatusCompleted, ActionStatusFailed, ActionStatusCanceled},
	ActionStatusExternalPending:      {ActionStatusRunning, ActionStatusInterventionRequired, ActionStatusCompleted, ActionStatusFailed, ActionStatusCanceled},
	ActionStatusInterventionRequired: {ActionStatusRunning, ActionStatusCanceled},
	ActionStatusCompleted:            {},
	ActionStatusFailed:               {},
	ActionStatusCanceled:             {},
}

func (s ActionStatus) IsValid() bool {
	_, ok := actionStatusTransitions[s]
	return ok
}

func (s ActionStatus) CanTransitionTo(next ActionStatus) bool {
	for _, candidate := range actionStatusTransitions[s] {
		if candidate == next {
			return true
		}
	}
	return false
}

type ArtifactKind string

const (
	ArtifactKindPullRequest        ArtifactKind = "github_pull_request"
	ArtifactKindGitBranch          ArtifactKind = "git_branch"
	ArtifactKindPatchBundle        ArtifactKind = "patch_bundle"
	ArtifactKindLandedChangeRecord ArtifactKind = "landed_change_record"
	ArtifactKindMergeResult        ArtifactKind = "merge_result_reference"
	ArtifactKindAnalysisReport     ArtifactKind = "analysis_report"
	ArtifactKindDiagnosticReport   ArtifactKind = "diagnostic_report"
	ArtifactKindEvidenceBundle     ArtifactKind = "evidence_bundle"
)

func (k ArtifactKind) IsValid() bool {
	switch k {
	case ArtifactKindPullRequest, ArtifactKindGitBranch, ArtifactKindPatchBundle, ArtifactKindLandedChangeRecord, ArtifactKindMergeResult, ArtifactKindAnalysisReport, ArtifactKindDiagnosticReport, ArtifactKindEvidenceBundle:
		return true
	default:
		return false
	}
}

type ArtifactRole string

const (
	ArtifactRolePrimary    ArtifactRole = "primary"
	ArtifactRoleSupporting ArtifactRole = "supporting"
)

func (r ArtifactRole) IsValid() bool {
	switch r {
	case ArtifactRolePrimary, ArtifactRoleSupporting:
		return true
	default:
		return false
	}
}

type ArtifactStatus string

const (
	ArtifactStatusAvailable ArtifactStatus = "available"
	ArtifactStatusRedacted  ArtifactStatus = "redacted"
	ArtifactStatusArchived  ArtifactStatus = "archived"
)

func (s ArtifactStatus) IsValid() bool {
	switch s {
	case ArtifactStatusAvailable, ArtifactStatusRedacted, ArtifactStatusArchived:
		return true
	default:
		return false
	}
}

type ReferenceType string

const (
	ReferenceTypeLinearIssue       ReferenceType = "linear_issue"
	ReferenceTypeGitHubRepo        ReferenceType = "github_repo"
	ReferenceTypeGitBranch         ReferenceType = "git_branch"
	ReferenceTypeGitHubPullRequest ReferenceType = "github_pull_request"
	ReferenceTypeReportURL         ReferenceType = "report_url"
)

func (t ReferenceType) IsValid() bool {
	switch t {
	case ReferenceTypeLinearIssue, ReferenceTypeGitHubRepo, ReferenceTypeGitBranch, ReferenceTypeGitHubPullRequest, ReferenceTypeReportURL:
		return true
	default:
		return false
	}
}

type ReferenceStatus string

const (
	ReferenceStatusActive      ReferenceStatus = "active"
	ReferenceStatusStale       ReferenceStatus = "stale"
	ReferenceStatusUnavailable ReferenceStatus = "unavailable"
)

func (s ReferenceStatus) IsValid() bool {
	switch s {
	case ReferenceStatusActive, ReferenceStatusStale, ReferenceStatusUnavailable:
		return true
	default:
		return false
	}
}

type InstanceRole string

const (
	InstanceRoleLeader  InstanceRole = "leader"
	InstanceRoleStandby InstanceRole = "standby"
)

func (r InstanceRole) IsValid() bool {
	switch r {
	case InstanceRoleLeader, InstanceRoleStandby:
		return true
	default:
		return false
	}
}

func (a ControlAction) IsServiceAction() bool {
	return a == ControlActionRefresh
}

// IsValid only accepts object-level control actions.
func (a ControlAction) IsValid() bool {
	switch a {
	case ControlActionCancel, ControlActionRetry, ControlActionResume, ControlActionResolveIntervention, ControlActionTerminate:
		return true
	default:
		return false
	}
}

type CandidateDeliveryPointKind string

const (
	CandidateDeliveryPointReviewablePullRequest CandidateDeliveryPointKind = "reviewable_pull_request"
	CandidateDeliveryPointTargetPRSnapshot      CandidateDeliveryPointKind = "target_pr_snapshot"
	CandidateDeliveryPointAnalysisReportDraft   CandidateDeliveryPointKind = "analysis_report_draft"
	CandidateDeliveryPointDiagnosticReportDraft CandidateDeliveryPointKind = "diagnostic_report_draft"
)

func (k CandidateDeliveryPointKind) IsValid() bool {
	switch k {
	case CandidateDeliveryPointReviewablePullRequest, CandidateDeliveryPointTargetPRSnapshot, CandidateDeliveryPointAnalysisReportDraft, CandidateDeliveryPointDiagnosticReportDraft:
		return true
	default:
		return false
	}
}

type CandidateDeliveryPoint struct {
	Kind                   CandidateDeliveryPointKind `json:"kind"`
	Summary                string                     `json:"summary"`
	RequiredArtifactKinds  []ArtifactKind             `json:"required_artifact_kinds,omitempty"`
	RequiredReferenceTypes []ReferenceType            `json:"required_reference_types,omitempty"`
}

func (p CandidateDeliveryPoint) Validate() error {
	if !p.Kind.IsValid() {
		return fmt.Errorf("invalid candidate delivery point kind %q", p.Kind)
	}
	if p.Summary == "" {
		return fmt.Errorf("candidate delivery point %q summary is required", p.Kind)
	}
	for _, kind := range p.RequiredArtifactKinds {
		if !kind.IsValid() {
			return fmt.Errorf("candidate delivery point %q has invalid artifact kind %q", p.Kind, kind)
		}
	}
	for _, referenceType := range p.RequiredReferenceTypes {
		if !referenceType.IsValid() {
			return fmt.Errorf("candidate delivery point %q has invalid reference type %q", p.Kind, referenceType)
		}
	}
	if len(p.RequiredArtifactKinds) == 0 && len(p.RequiredReferenceTypes) == 0 {
		return fmt.Errorf("candidate delivery point %q requires at least one artifact or reference constraint", p.Kind)
	}
	return nil
}

type CandidateDelivery struct {
	Kind        CandidateDeliveryPointKind `json:"kind"`
	Reached     bool                       `json:"reached"`
	ReachedAt   string                     `json:"reached_at,omitempty"`
	Summary     string                     `json:"summary,omitempty"`
	ArtifactIDs []string                   `json:"artifact_ids,omitempty"`
}

func (d CandidateDelivery) Validate() error {
	if !d.Kind.IsValid() {
		return fmt.Errorf("invalid candidate delivery kind %q", d.Kind)
	}
	if d.Reached && d.ReachedAt == "" {
		return fmt.Errorf("candidate delivery %q reached_at is required when reached", d.Kind)
	}
	return nil
}

type CheckpointType string

const (
	CheckpointTypeBusiness CheckpointType = "business"
	CheckpointTypeRecovery CheckpointType = "recovery"
)

func (t CheckpointType) IsValid() bool {
	switch t {
	case CheckpointTypeBusiness, CheckpointTypeRecovery:
		return true
	default:
		return false
	}
}

type BusinessCheckpointRule struct {
	Type                      CheckpointType             `json:"type"`
	CandidateDeliveryKind     CandidateDeliveryPointKind `json:"candidate_delivery_kind"`
	Summary                   string                     `json:"summary"`
	RequiredArtifactKinds     []ArtifactKind             `json:"required_artifact_kinds,omitempty"`
	RequiredReferenceTypes    []ReferenceType            `json:"required_reference_types,omitempty"`
	RequiresRemotePublication bool                       `json:"requires_remote_publication"`
}

func (r BusinessCheckpointRule) Validate() error {
	if r.Type != CheckpointTypeBusiness {
		return fmt.Errorf("business checkpoint rule must use type %q", CheckpointTypeBusiness)
	}
	if !r.CandidateDeliveryKind.IsValid() {
		return fmt.Errorf("business checkpoint rule has invalid candidate delivery kind %q", r.CandidateDeliveryKind)
	}
	if r.Summary == "" {
		return fmt.Errorf("business checkpoint rule summary is required")
	}
	for _, kind := range r.RequiredArtifactKinds {
		if !kind.IsValid() {
			return fmt.Errorf("business checkpoint rule has invalid artifact kind %q", kind)
		}
	}
	for _, referenceType := range r.RequiredReferenceTypes {
		if !referenceType.IsValid() {
			return fmt.Errorf("business checkpoint rule has invalid reference type %q", referenceType)
		}
	}
	if len(r.RequiredArtifactKinds) == 0 && len(r.RequiredReferenceTypes) == 0 {
		return fmt.Errorf("business checkpoint rule requires at least one artifact or reference constraint")
	}
	return nil
}

type Checkpoint struct {
	Type         CheckpointType  `json:"type"`
	Summary      string          `json:"summary"`
	CapturedAt   string          `json:"captured_at"`
	Stage        RunPhaseSummary `json:"stage"`
	ArtifactIDs  []string        `json:"artifact_ids,omitempty"`
	ReferenceIDs []string        `json:"reference_ids,omitempty"`
	BaseSHA      string          `json:"base_sha,omitempty"`
	Branch       string          `json:"branch,omitempty"`
	Reason       *Reason         `json:"reason,omitempty"`
}

func (c Checkpoint) Validate() error {
	if !c.Type.IsValid() {
		return fmt.Errorf("invalid checkpoint type %q", c.Type)
	}
	if c.Summary == "" {
		return fmt.Errorf("checkpoint summary is required")
	}
	if c.CapturedAt == "" {
		return fmt.Errorf("checkpoint captured_at is required")
	}
	if !c.Stage.IsValid() {
		return fmt.Errorf("invalid checkpoint stage %q", c.Stage)
	}
	if len(c.ArtifactIDs) == 0 && len(c.ReferenceIDs) == 0 {
		return fmt.Errorf("checkpoint requires at least one artifact or reference id")
	}
	return nil
}

type ReviewerMode string

const (
	ReviewerModeReadOnly ReviewerMode = "read_only"
)

func (m ReviewerMode) IsValid() bool {
	switch m {
	case ReviewerModeReadOnly:
		return true
	default:
		return false
	}
}

type ReviewGateStatus string

const (
	ReviewGateStatusPending              ReviewGateStatus = "pending"
	ReviewGateStatusReviewing            ReviewGateStatus = "reviewing"
	ReviewGateStatusPassed               ReviewGateStatus = "passed"
	ReviewGateStatusChangesRequested     ReviewGateStatus = "changes_requested"
	ReviewGateStatusInterventionRequired ReviewGateStatus = "intervention_required"
)

func (s ReviewGateStatus) IsValid() bool {
	switch s {
	case ReviewGateStatusPending, ReviewGateStatusReviewing, ReviewGateStatusPassed, ReviewGateStatusChangesRequested, ReviewGateStatusInterventionRequired:
		return true
	default:
		return false
	}
}

type ReviewGatePolicy struct {
	Required     bool         `json:"required"`
	ReviewerMode ReviewerMode `json:"reviewer_mode"`
	MaxFixRounds int          `json:"max_fix_rounds"`
}

func (p ReviewGatePolicy) Validate() error {
	if !p.Required {
		return nil
	}
	if !p.ReviewerMode.IsValid() {
		return fmt.Errorf("invalid reviewer mode %q", p.ReviewerMode)
	}
	if p.MaxFixRounds <= 0 {
		return fmt.Errorf("max_fix_rounds must be greater than zero")
	}
	return nil
}

type ReviewGate struct {
	ReviewGatePolicy
	Status        ReviewGateStatus `json:"status"`
	FixRoundsUsed int              `json:"fix_rounds_used"`
}

func (g ReviewGate) Validate() error {
	if err := g.ReviewGatePolicy.Validate(); err != nil {
		return err
	}
	if !g.Status.IsValid() {
		return fmt.Errorf("invalid review gate status %q", g.Status)
	}
	if g.FixRoundsUsed < 0 {
		return fmt.Errorf("fix_rounds_used must be non-negative")
	}
	if g.MaxFixRounds > 0 && g.FixRoundsUsed > g.MaxFixRounds {
		return fmt.Errorf("fix_rounds_used must not exceed max_fix_rounds")
	}
	return nil
}

type InterventionInputKind string

const (
	InterventionInputKindString     InterventionInputKind = "string"
	InterventionInputKindStringList InterventionInputKind = "string_list"
	InterventionInputKindEnum       InterventionInputKind = "enum"
	InterventionInputKindBoolean    InterventionInputKind = "boolean"
	InterventionInputKindReference  InterventionInputKind = "reference"
)

func (k InterventionInputKind) IsValid() bool {
	switch k {
	case InterventionInputKindString, InterventionInputKindStringList, InterventionInputKindEnum, InterventionInputKindBoolean, InterventionInputKindReference:
		return true
	default:
		return false
	}
}

type InterventionInputField struct {
	Field         string                `json:"field"`
	Label         string                `json:"label"`
	Kind          InterventionInputKind `json:"kind"`
	Required      bool                  `json:"required"`
	AllowedValues []string              `json:"allowed_values,omitempty"`
	Description   string                `json:"description"`
	Visibility    VisibilityLevel       `json:"visibility"`
	Sensitivity   SensitiveFieldClass   `json:"sensitivity"`
}

func (f InterventionInputField) Validate() error {
	if f.Field == "" {
		return fmt.Errorf("field is required")
	}
	if f.Label == "" {
		return fmt.Errorf("label is required")
	}
	if !f.Kind.IsValid() {
		return fmt.Errorf("invalid intervention input kind %q", f.Kind)
	}
	if !f.Visibility.IsValid() {
		return fmt.Errorf("invalid intervention input visibility %q", f.Visibility)
	}
	if !f.Sensitivity.IsValid() {
		return fmt.Errorf("invalid intervention input sensitivity %q", f.Sensitivity)
	}
	if f.Kind == InterventionInputKindEnum && len(f.AllowedValues) == 0 {
		return fmt.Errorf("enum intervention input %q requires allowed values", f.Field)
	}
	if f.Description == "" {
		return fmt.Errorf("description is required for intervention input %q", f.Field)
	}
	return nil
}

type CompletionCriterion struct {
	Code                   string              `json:"code"`
	Summary                string              `json:"summary"`
	AcceptableOutcomes     []OutcomeConclusion `json:"acceptable_outcomes"`
	RequiredArtifactKinds  []ArtifactKind      `json:"required_artifact_kinds,omitempty"`
	RequiredReferenceTypes []ReferenceType     `json:"required_reference_types,omitempty"`
	RequiresMergedTarget   bool                `json:"requires_merged_target"`
}

func (c CompletionCriterion) Validate() error {
	if c.Code == "" {
		return fmt.Errorf("completion criterion code is required")
	}
	if c.Summary == "" {
		return fmt.Errorf("completion criterion %q summary is required", c.Code)
	}
	if len(c.AcceptableOutcomes) == 0 {
		return fmt.Errorf("completion criterion %q requires at least one acceptable outcome", c.Code)
	}
	for _, outcome := range c.AcceptableOutcomes {
		if !outcome.IsValid() {
			return fmt.Errorf("completion criterion %q has invalid outcome %q", c.Code, outcome)
		}
	}
	for _, kind := range c.RequiredArtifactKinds {
		if !kind.IsValid() {
			return fmt.Errorf("completion criterion %q has invalid artifact kind %q", c.Code, kind)
		}
	}
	for _, referenceType := range c.RequiredReferenceTypes {
		if !referenceType.IsValid() {
			return fmt.Errorf("completion criterion %q has invalid reference type %q", c.Code, referenceType)
		}
	}
	if len(c.RequiredArtifactKinds) == 0 && len(c.RequiredReferenceTypes) == 0 {
		return fmt.Errorf("completion criterion %q requires at least one artifact or reference kind", c.Code)
	}
	if c.RequiresMergedTarget && len(c.RequiredReferenceTypes) == 0 {
		return fmt.Errorf("completion criterion %q requires merged target evidence via reference types", c.Code)
	}
	return nil
}

type ArtifactExpectation struct {
	Role     ArtifactRole `json:"role"`
	Kind     ArtifactKind `json:"kind"`
	Required bool         `json:"required"`
	Summary  string       `json:"summary"`
}

func (e ArtifactExpectation) Validate() error {
	if !e.Role.IsValid() {
		return fmt.Errorf("invalid artifact role %q", e.Role)
	}
	if !e.Kind.IsValid() {
		return fmt.Errorf("invalid artifact kind %q", e.Kind)
	}
	if e.Summary == "" {
		return fmt.Errorf("artifact expectation summary is required")
	}
	return nil
}

type TargetReferencePolicy struct {
	Required              bool            `json:"required"`
	RequiresCodeSpace     bool            `json:"requires_code_space"`
	AllowedReferenceTypes []ReferenceType `json:"allowed_reference_types"`
}

func (p TargetReferencePolicy) Validate() error {
	for _, referenceType := range p.AllowedReferenceTypes {
		if !referenceType.IsValid() {
			return fmt.Errorf("invalid target reference type %q", referenceType)
		}
	}
	return nil
}

type InterventionTemplate struct {
	TemplateID             string                   `json:"template_id"`
	Summary                string                   `json:"summary"`
	AllowedActions         []ControlAction          `json:"allowed_actions"`
	RequiredInputs         []InterventionInputField `json:"required_inputs"`
	AllowsSupplementalText bool                     `json:"allows_supplemental_text"`
}

func (t InterventionTemplate) Validate() error {
	if t.TemplateID == "" {
		return fmt.Errorf("intervention template id is required")
	}
	if t.Summary == "" {
		return fmt.Errorf("intervention template %q summary is required", t.TemplateID)
	}
	if len(t.AllowedActions) == 0 {
		return fmt.Errorf("intervention template %q requires allowed actions", t.TemplateID)
	}
	for _, action := range t.AllowedActions {
		if !action.IsValid() {
			return fmt.Errorf("intervention template %q has invalid action %q", t.TemplateID, action)
		}
	}
	if len(t.RequiredInputs) == 0 {
		return fmt.Errorf("intervention template %q requires structured inputs", t.TemplateID)
	}
	for _, field := range t.RequiredInputs {
		if err := field.Validate(); err != nil {
			return fmt.Errorf("intervention template %q: %w", t.TemplateID, err)
		}
	}
	return nil
}

type JobTypeDefinition struct {
	Type                  JobType                `json:"type"`
	Summary               string                 `json:"summary"`
	Target                TargetReferencePolicy  `json:"target"`
	CandidateDelivery     CandidateDeliveryPoint `json:"candidate_delivery"`
	BusinessCheckpoint    BusinessCheckpointRule `json:"business_checkpoint"`
	ReviewGate            ReviewGatePolicy       `json:"review_gate"`
	CompletionCriteria    []CompletionCriterion  `json:"completion_criteria"`
	DefaultArtifacts      []ArtifactExpectation  `json:"default_artifacts"`
	InterventionTemplates []InterventionTemplate `json:"intervention_templates"`
}

func (d JobTypeDefinition) Validate() error {
	if !d.Type.IsValid() {
		return fmt.Errorf("invalid job type %q", d.Type)
	}
	if d.Summary == "" {
		return fmt.Errorf("job type %q summary is required", d.Type)
	}
	if err := d.Target.Validate(); err != nil {
		return fmt.Errorf("job type %q target: %w", d.Type, err)
	}
	if err := d.CandidateDelivery.Validate(); err != nil {
		return fmt.Errorf("job type %q candidate delivery: %w", d.Type, err)
	}
	if err := d.BusinessCheckpoint.Validate(); err != nil {
		return fmt.Errorf("job type %q business checkpoint: %w", d.Type, err)
	}
	if d.BusinessCheckpoint.CandidateDeliveryKind != d.CandidateDelivery.Kind {
		return fmt.Errorf("job type %q business checkpoint must align with candidate delivery kind %q", d.Type, d.CandidateDelivery.Kind)
	}
	if err := d.ReviewGate.Validate(); err != nil {
		return fmt.Errorf("job type %q review gate: %w", d.Type, err)
	}
	if len(d.CompletionCriteria) == 0 {
		return fmt.Errorf("job type %q requires completion criteria", d.Type)
	}
	for _, criterion := range d.CompletionCriteria {
		if err := criterion.Validate(); err != nil {
			return fmt.Errorf("job type %q: %w", d.Type, err)
		}
	}
	if len(d.DefaultArtifacts) == 0 {
		return fmt.Errorf("job type %q requires default artifacts", d.Type)
	}
	for _, artifact := range d.DefaultArtifacts {
		if err := artifact.Validate(); err != nil {
			return fmt.Errorf("job type %q: %w", d.Type, err)
		}
	}
	if len(d.InterventionTemplates) == 0 {
		return fmt.Errorf("job type %q requires intervention templates", d.Type)
	}
	for _, template := range d.InterventionTemplates {
		if err := template.Validate(); err != nil {
			return fmt.Errorf("job type %q: %w", d.Type, err)
		}
	}
	return nil
}

type DecisionCode string

type DecisionActionKind string

const (
	DecisionActionKindControl           DecisionActionKind = "control"
	DecisionActionKindInspectArtifact   DecisionActionKind = "inspect_artifact"
	DecisionActionKindHandoffJobType    DecisionActionKind = "handoff_job_type"
	DecisionActionKindWaitForCapability DecisionActionKind = "wait_for_capability"
)

func (k DecisionActionKind) IsValid() bool {
	switch k {
	case DecisionActionKindControl, DecisionActionKindInspectArtifact, DecisionActionKindHandoffJobType, DecisionActionKindWaitForCapability:
		return true
	default:
		return false
	}
}

type DecisionAction struct {
	Kind           DecisionActionKind `json:"kind"`
	Control        ControlAction      `json:"control,omitempty"`
	RelatedJobType JobType            `json:"related_job_type,omitempty"`
	Summary        string             `json:"summary,omitempty"`
}

type Decision struct {
	ID                 string           `json:"id,omitempty"`
	ObjectType         ObjectType       `json:"object_type,omitempty"`
	DomainID           string           `json:"domain_id,omitempty"`
	ContractVersion    APIVersion       `json:"contract_version,omitempty"`
	CreatedAt          string           `json:"created_at,omitempty"`
	UpdatedAt          string           `json:"updated_at,omitempty"`
	Visibility         VisibilityLevel  `json:"visibility,omitempty"`
	DecisionCode       DecisionCode     `json:"decision_code"`
	Category           CodeCategory     `json:"category"`
	Summary            string           `json:"summary"`
	RecommendedActions []DecisionAction `json:"recommended_actions,omitempty"`
	Details            map[string]any   `json:"details"`
	Extensions         Extensions       `json:"extensions,omitempty"`
}

type BaseObject struct {
	ID              string          `json:"id,omitempty"`
	ObjectType      ObjectType      `json:"object_type,omitempty"`
	DomainID        string          `json:"domain_id,omitempty"`
	Visibility      VisibilityLevel `json:"visibility,omitempty"`
	ContractVersion APIVersion      `json:"contract_version,omitempty"`
	CreatedAt       string          `json:"created_at,omitempty"`
	UpdatedAt       string          `json:"updated_at,omitempty"`
	Extensions      Extensions      `json:"extensions,omitempty"`
}

type ActionSummary struct {
	HasPendingExternalActions bool         `json:"has_pending_external_actions"`
	PendingCount              int          `json:"pending_count"`
	PendingTypes              []ActionType `json:"pending_types,omitempty"`
}

func (s ActionSummary) Validate() error {
	if s.PendingCount < 0 {
		return fmt.Errorf("pending_count must be non-negative")
	}
	for _, actionType := range s.PendingTypes {
		if !actionType.IsValid() {
			return fmt.Errorf("invalid pending action type %q", actionType)
		}
	}
	if !s.HasPendingExternalActions && s.PendingCount != 0 {
		return fmt.Errorf("pending_count must be zero when no pending external actions exist")
	}
	return nil
}

type ObjectContext struct {
	Relations  []ObjectRelation `json:"relations,omitempty"`
	References []Reference      `json:"references,omitempty"`
	Reasons    []Reason         `json:"reasons,omitempty"`
	Decision   *Decision        `json:"decision,omitempty"`
	ErrorCode  ErrorCode        `json:"error_code,omitempty"`
}

func (c ObjectContext) ValidateForSource(source ObjectType) error {
	for _, relation := range c.Relations {
		if err := relation.ValidateForSource(source); err != nil {
			return err
		}
	}
	return nil
}

type Job struct {
	BaseObject
	ObjectContext
	State         JobStatus     `json:"state"`
	JobType       JobType       `json:"job_type"`
	ActionSummary ActionSummary `json:"action_summary"`
}

type Run struct {
	BaseObject
	ObjectContext
	State             RunStatus          `json:"state"`
	Phase             RunPhaseSummary    `json:"phase"`
	Attempt           int                `json:"attempt"`
	CandidateDelivery *CandidateDelivery `json:"candidate_delivery,omitempty"`
	ReviewGate        *ReviewGate        `json:"review_gate,omitempty"`
	Checkpoints       []Checkpoint       `json:"checkpoints,omitempty"`
}

type InterventionResolution struct {
	Action           ControlAction  `json:"action"`
	ProvidedInputs   map[string]any `json:"provided_inputs"`
	SupplementalText string         `json:"supplemental_text,omitempty"`
	ResolvedAt       string         `json:"resolved_at"`
	Decision         *Decision      `json:"decision,omitempty"`
}

type Intervention struct {
	BaseObject
	ObjectContext
	State          InterventionStatus       `json:"state"`
	TemplateID     string                   `json:"template_id"`
	Summary        string                   `json:"summary"`
	RequiredInputs []InterventionInputField `json:"required_inputs"`
	AllowedActions []ControlAction          `json:"allowed_actions"`
	Resolution     *InterventionResolution  `json:"resolution,omitempty"`
}

type Outcome struct {
	BaseObject
	ObjectContext
	State       OutcomeConclusion `json:"state"`
	Summary     string            `json:"summary"`
	CompletedAt string            `json:"completed_at"`
}

type Artifact struct {
	BaseObject
	ObjectContext
	State   ArtifactStatus `json:"state"`
	Kind    ArtifactKind   `json:"kind"`
	Role    ArtifactRole   `json:"role"`
	Locator string         `json:"locator,omitempty"`
}

type Action struct {
	BaseObject
	ObjectContext
	State   ActionStatus `json:"state"`
	Type    ActionType   `json:"type"`
	Summary string       `json:"summary"`
}

type Instance struct {
	BaseObject
	ObjectContext
	State                 ServiceMode            `json:"state"`
	Name                  string                 `json:"name"`
	Version               string                 `json:"version"`
	Role                  InstanceRole           `json:"role"`
	StaticCapabilities    StaticCapabilitySet    `json:"static_capabilities"`
	AvailableCapabilities AvailableCapabilitySet `json:"available_capabilities"`
}

type Reference struct {
	BaseObject
	State       ReferenceStatus `json:"state"`
	Type        ReferenceType   `json:"type"`
	System      string          `json:"system"`
	Locator     string          `json:"locator"`
	URL         string          `json:"url,omitempty"`
	ExternalID  string          `json:"external_id,omitempty"`
	DisplayName string          `json:"display_name,omitempty"`
}
