package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"symphony-go/internal/agent"
	"symphony-go/internal/config"
	"symphony-go/internal/model"
	"symphony-go/internal/model/contract"
	"symphony-go/internal/shell"
	"symphony-go/internal/tracker"
	"symphony-go/internal/workflow"
	"symphony-go/internal/workspace"
)

type WorkerKind string

const (
	WorkerKindExecution WorkerKind = "execution"
	WorkerKindReview    WorkerKind = "review"
)

type WorkerResult struct {
	Kind           WorkerKind
	IssueID        string
	Identifier     string
	Attempt        *int
	StartedAt      time.Time
	Phase          model.RunPhase
	Err            error
	HasNewOpenPR   bool
	FinalBranch    string
	ReviewStatus   contract.ReviewGateStatus
	ReviewSummary  string
	ReviewFindings []model.ReviewFinding
}

type CodexUpdate struct {
	IssueID string
	Event   agent.AgentEvent
}

type Snapshot = contract.ServiceStateSnapshot
type DiscoveryDocument = contract.DiscoveryDocument
type RefreshRequestResult = contract.ControlResult

type AlertSnapshot struct {
	Code            string
	Level           string
	Message         string
	IssueID         string
	IssueIdentifier string
}

type NotificationChannelHealthSnapshot struct {
	ChannelID           string
	DisplayName         string
	Status              string
	QueueOverflow       bool
	LastError           *string
	LastAttemptAt       *time.Time
	LastSuccessAt       *time.Time
	ConsecutiveFailures int
}

type PersistenceHealthSnapshot struct {
	Enabled             bool
	Kind                string
	Status              string
	LastError           *string
	LastAttemptAt       *time.Time
	LastSuccessAt       *time.Time
	ConsecutiveFailures int
}

type PullRequestState string

const (
	PullRequestStateOpen   PullRequestState = "open"
	PullRequestStateMerged PullRequestState = "merged"
	PullRequestStateClosed PullRequestState = "closed"
)

type PullRequestInfo struct {
	Number     int
	URL        string
	HeadBranch string
	HeadOwner  string
	BaseOwner  string
	BaseRepo   string
	State      PullRequestState
}

type SuccessfulRunDisposition string

const (
	DispositionCompleted            SuccessfulRunDisposition = "completed"
	DispositionAwaitingMerge        SuccessfulRunDisposition = "awaiting_merge"
	DispositionAwaitingIntervention SuccessfulRunDisposition = "awaiting_intervention"
	DispositionContinuation         SuccessfulRunDisposition = "continuation"
)

type SuccessfulRunDecision struct {
	Disposition     SuccessfulRunDisposition
	Reason          *model.ContinuationReason
	ExpectedOutcome model.CompletionMode
	PR              *PullRequestInfo
	FinalBranch     string
}

type PullRequestLookup interface {
	FindByHeadBranch(ctx context.Context, workspacePath string, headBranch string) (*PullRequestInfo, error)
	Refresh(ctx context.Context, workspacePath string, pr *PullRequestInfo) (*PullRequestInfo, error)
}

type RuntimeCompatibility struct {
	Profile            string
	ActiveSource       string
	SourceKind         string
	FlowName           string
	TrackerKind        string
	TrackerRepo        string
	TrackerProjectSlug string
}

type RuntimeDescriptor struct {
	ConfigRoot             string
	WorkspaceRoot          string
	SessionPersistenceKind string
	SessionStatePath       string
}

type RuntimeIdentity struct {
	Compatibility RuntimeCompatibility
	Descriptor    RuntimeDescriptor
}

type sourceClosureActionState struct {
	Action      contract.Action
	SourceIssue model.Issue
	JobID       string
}

type Orchestrator struct {
	tracker             tracker.Client
	workspace           workspace.Manager
	runner              agent.Runner
	configFn            func() *model.ServiceConfig
	workflowFn          func() *model.WorkflowDefinition
	runtimeIdentityFn   func() RuntimeIdentity
	logger              *slog.Logger
	now                 func() time.Time
	randFloat           func() float64
	gitBranchFn         func(context.Context, string) (string, error)
	captureCheckpointFn func(context.Context, *model.JobRuntime, contract.RunPhaseSummary, *contract.Reason) (*model.RecoveryCheckpoint, error)
	prLookup            PullRequestLookup
	stateStore          stateStore
	notifier            notifier

	tickTimer      *time.Timer
	workerResultCh chan WorkerResult
	codexUpdateCh  chan CodexUpdate
	configReloadCh chan struct{}
	refreshCh      chan struct{}
	retryFireCh    chan string
	shutdownCh     chan struct{}
	doneCh         chan struct{}

	runCtx       context.Context
	workerWG     sync.WaitGroup
	shutdownOnce sync.Once

	state model.OrchestratorState

	mu                          sync.RWMutex
	snapshot                    Snapshot
	started                     bool
	subscribers                 map[int]chan Snapshot
	nextSubscriberID            int
	eventSubscribers            map[int]chan contract.EventEnvelope
	nextEventSubscriberID       int
	healthAlerts                map[string]AlertSnapshot
	notificationHealth          map[string]*NotificationChannelHealthSnapshot
	persistenceHealth           PersistenceHealthSnapshot
	runningRecords              map[string]*model.JobRuntime
	retryRecords                map[string]*model.JobRuntime
	awaitingMergeRecords        map[string]*model.JobRuntime
	awaitingInterventionRecords map[string]*model.JobRuntime
	pendingCleanup              map[string]string
	pendingRecovery             map[string]uint64
	pendingLaunch               map[string]uint64
	pendingActions              map[uint64][]func()
	maxCompleted                int
	startedAt                   time.Time
	serviceVersion              string
	extensionsReady             bool
	eventSeq                    uint64
	notifierGeneration          uint64
	lastPersistedStateVersion   uint64
	lastPersistedState          *durableRuntimeState
	lastEventSnapshot           Snapshot
	lastEventObjects            map[string]ObjectEnvelope
	eventStateInitialized       bool
	objectLedger                ObjectLedger
	objectLedgerRestored        bool
	sourceClosureActions        map[string]*sourceClosureActionState
}

var BuildVersion = "dev"

const defaultMaxCompletedEntries = 100
const serviceProtectedModeCode = "service_protected_mode"

func NewOrchestrator(trackerClient tracker.Client, workspaceManager workspace.Manager, runner agent.Runner, configFn func() *model.ServiceConfig, workflowFn func() *model.WorkflowDefinition, runtimeIdentityFn func() RuntimeIdentity, logger *slog.Logger) *Orchestrator {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	healthAlerts := map[string]AlertSnapshot{}

	o := &Orchestrator{
		tracker:             trackerClient,
		workspace:           workspaceManager,
		runner:              runner,
		configFn:            configFn,
		workflowFn:          workflowFn,
		runtimeIdentityFn:   runtimeIdentityFn,
		logger:              logger,
		now:                 time.Now,
		randFloat:           func() float64 { return 0.5 },
		gitBranchFn:         defaultGitBranch,
		captureCheckpointFn: nil,
		workerResultCh:      make(chan WorkerResult, 128),
		codexUpdateCh:       make(chan CodexUpdate, 1024),
		configReloadCh:      make(chan struct{}, 8),
		refreshCh:           make(chan struct{}, 8),
		retryFireCh:         make(chan string, 128),
		shutdownCh:          make(chan struct{}),
		doneCh:              make(chan struct{}),
		state: model.OrchestratorState{
			Service: model.RuntimeServiceState{
				Mode: model.ServiceModeServing,
			},
			Jobs:             map[string]*model.JobRuntime{},
			ProtectedResults: map[string]*model.ProtectedResultEntry{},
		},
		subscribers:                 map[int]chan Snapshot{},
		eventSubscribers:            map[int]chan contract.EventEnvelope{},
		healthAlerts:                healthAlerts,
		notificationHealth:          map[string]*NotificationChannelHealthSnapshot{},
		runningRecords:              map[string]*model.JobRuntime{},
		retryRecords:                map[string]*model.JobRuntime{},
		awaitingMergeRecords:        map[string]*model.JobRuntime{},
		awaitingInterventionRecords: map[string]*model.JobRuntime{},
		pendingCleanup:              map[string]string{},
		pendingRecovery:             map[string]uint64{},
		pendingLaunch:               map[string]uint64{},
		pendingActions:              map[uint64][]func(){},
		maxCompleted:                defaultMaxCompletedEntries,
		startedAt:                   time.Now().UTC(),
		serviceVersion:              BuildVersion,
		lastEventObjects:            map[string]ObjectEnvelope{},
		sourceClosureActions:        map[string]*sourceClosureActionState{},
	}
	o.prLookup = newGitHubPRLookup()
	o.captureCheckpointFn = o.captureRecoveryCheckpoint
	o.applyCurrentConfigLocked()
	o.refreshSnapshotLocked()
	return o
}

func cloneReason(value *contract.Reason) *contract.Reason {
	if value == nil {
		return nil
	}
	copyValue := *value
	if value.Details != nil {
		copyValue.Details = map[string]any{}
		for key, item := range value.Details {
			copyValue.Details[key] = item
		}
	}
	return &copyValue
}

func cloneObservation(value *contract.Observation) *contract.Observation {
	if value == nil {
		return nil
	}
	copyValue := *value
	if value.Details != nil {
		copyValue.Details = map[string]any{}
		for key, item := range value.Details {
			copyValue.Details[key] = item
		}
	}
	return &copyValue
}

func cloneResult(value *contract.Result) *contract.Result {
	if value == nil {
		return nil
	}
	copyValue := *value
	if value.Details != nil {
		copyValue.Details = map[string]any{}
		for key, item := range value.Details {
			copyValue.Details[key] = item
		}
	}
	return &copyValue
}

func cloneDurableRefs(value contract.DurableRefs) contract.DurableRefs {
	copyValue := value
	if value.Workspace != nil {
		workspaceCopy := *value.Workspace
		copyValue.Workspace = &workspaceCopy
	}
	if value.Branch != nil {
		branchCopy := *value.Branch
		copyValue.Branch = &branchCopy
	}
	if value.PullRequest != nil {
		prCopy := *value.PullRequest
		copyValue.PullRequest = &prCopy
	}
	return copyValue
}

func cloneArchivedJob(value model.ArchivedJob) model.ArchivedJob {
	copyValue := value
	copyValue.Reason = cloneReason(value.Reason)
	copyValue.Observation = cloneObservation(value.Observation)
	copyValue.DurableRefs = cloneDurableRefs(value.DurableRefs)
	copyValue.Result = cloneResult(value.Result)
	copyValue.LastKnownIssue = model.CloneIssue(value.LastKnownIssue)
	copyValue.Dispatch = model.CloneDispatchContext(value.Dispatch)
	copyValue.Run = model.CloneRunState(value.Run)
	copyValue.Intervention = model.CloneInterventionState(value.Intervention)
	copyValue.Artifacts = append([]contract.Artifact(nil), value.Artifacts...)
	if value.Outcome != nil {
		outcomeCopy := *value.Outcome
		copyValue.Outcome = &outcomeCopy
	}
	return copyValue
}

func cloneArchivedJobs(records []model.ArchivedJob) []model.ArchivedJob {
	if len(records) == 0 {
		return []model.ArchivedJob{}
	}
	cloned := make([]model.ArchivedJob, 0, len(records))
	for _, record := range records {
		cloned = append(cloned, cloneArchivedJob(record))
	}
	return cloned
}

func cloneServiceReasons(reasons []contract.Reason) []contract.Reason {
	if len(reasons) == 0 {
		return []contract.Reason{}
	}
	cloned := make([]contract.Reason, 0, len(reasons))
	for _, reason := range reasons {
		cloned = append(cloned, *cloneReason(&reason))
	}
	return cloned
}

func sanitizeRecordIDToken(value string, fallback string) string {
	token := model.SanitizeWorkspaceKey(strings.TrimSpace(value))
	token = strings.Trim(token, "._-")
	if token == "" {
		return fallback
	}
	return token
}

func recordIDForSource(sourceKind contract.SourceKind, sourceName string, sourceID string) contract.RecordID {
	return contract.RecordID(fmt.Sprintf(
		"rec_%s_%s_%s",
		sanitizeRecordIDToken(string(sourceKind), "source"),
		sanitizeRecordIDToken(sourceName, "source"),
		sanitizeRecordIDToken(sourceID, "id"),
	))
}

func recordIDForIssue(sourceKind contract.SourceKind, sourceName string, issue *model.Issue) contract.RecordID {
	if issue == nil {
		return ""
	}
	return recordIDForSource(sourceKind, sourceName, issue.ID)
}

func sourceRefForIssue(sourceKind contract.SourceKind, sourceName string, issue *model.Issue) contract.SourceRef {
	if issue == nil {
		return contract.SourceRef{SourceKind: sourceKind, SourceName: strings.TrimSpace(sourceName)}
	}
	url := ""
	if issue.URL != nil {
		url = strings.TrimSpace(*issue.URL)
	}
	return contract.SourceRef{
		SourceKind:       sourceKind,
		SourceName:       strings.TrimSpace(sourceName),
		SourceID:         strings.TrimSpace(issue.ID),
		SourceIdentifier: strings.TrimSpace(issue.Identifier),
		URL:              url,
	}
}

func cloneJobRuntime(record *model.JobRuntime) *model.JobRuntime {
	if record == nil {
		return nil
	}
	copyValue := *record
	copyValue.Reason = cloneReason(record.Reason)
	copyValue.Observation = cloneObservation(record.Observation)
	copyValue.DurableRefs = cloneDurableRefs(record.DurableRefs)
	copyValue.Result = cloneResult(record.Result)
	copyValue.RetryDueAt = cloneTimePtr(record.RetryDueAt)
	copyValue.LastKnownIssue = model.CloneIssue(record.LastKnownIssue)
	copyValue.StartedAt = cloneTimePtr(record.StartedAt)
	copyValue.Dispatch = model.CloneDispatchContext(record.Dispatch)
	copyValue.Run = model.CloneRunState(record.Run)
	copyValue.Intervention = model.CloneInterventionState(record.Intervention)
	copyValue.Artifacts = append([]contract.Artifact(nil), record.Artifacts...)
	if record.Outcome != nil {
		outcomeCopy := *record.Outcome
		copyValue.Outcome = &outcomeCopy
	}
	copyValue.RetryTimer = nil
	copyValue.WorkerCancel = nil
	return &copyValue
}

func jobTypeForDispatch(dispatch *model.DispatchContext) contract.JobType {
	if dispatch != nil && dispatch.JobType.IsValid() {
		return dispatch.JobType
	}
	if dispatch != nil && dispatch.ExpectedOutcome == model.CompletionModePullRequest {
		return contract.JobTypeCodeChange
	}
	return contract.JobTypeAnalysis
}

func jobTypeDefinitionForDispatch(dispatch *model.DispatchContext) (contract.JobTypeDefinition, bool) {
	return contract.DescribeJobType(jobTypeForDispatch(dispatch))
}

func runPhaseSummaryForModelPhase(phase model.RunPhase) contract.RunPhaseSummary {
	switch phase {
	case model.PhasePreparingWorkspace, model.PhaseBuildingPrompt, model.PhaseLaunchingAgent, model.PhaseInitializingSession:
		return contract.RunPhaseSummaryPreparing
	case model.PhaseStreamingTurn:
		return contract.RunPhaseSummaryExecuting
	case model.PhaseFinishing:
		return contract.RunPhaseSummaryPublishing
	case model.PhaseTimedOut, model.PhaseStalled, model.PhaseCanceledByReconciliation, model.PhaseFailed:
		return contract.RunPhaseSummaryBlocked
	case model.PhaseSucceeded:
		return contract.RunPhaseSummaryPublishing
	default:
		return contract.RunPhaseSummaryExecuting
	}
}

func runBudgetForConfig(cfg *model.ServiceConfig) model.RunBudget {
	if cfg == nil {
		return model.RunBudget{}
	}
	return model.RunBudget{
		TotalMS:     cfg.RunBudgetTotalMS,
		ExecutionMS: cfg.RunExecutionBudgetMS,
		ReviewFixMS: cfg.RunReviewFixBudgetMS,
	}
}

func continuationCheckpointID(record *model.JobRuntime, attempt int) string {
	recordID := "record"
	if record != nil && strings.TrimSpace(string(record.RecordID)) != "" {
		recordID = strings.TrimSpace(string(record.RecordID))
	}
	if attempt <= 0 {
		attempt = 1
	}
	return fmt.Sprintf("chk-%s-%d", recordID, attempt)
}

func continuationArtifactID(record *model.JobRuntime, attempt int) string {
	recordID := "record"
	if record != nil && strings.TrimSpace(string(record.RecordID)) != "" {
		recordID = strings.TrimSpace(string(record.RecordID))
	}
	if attempt <= 0 {
		attempt = 1
	}
	return fmt.Sprintf("art-%s-recovery-%d", recordID, attempt)
}

func ensureRunState(record *model.JobRuntime, cfg *model.ServiceConfig, dispatch *model.DispatchContext, attempt int) *model.RunState {
	if record == nil {
		return nil
	}
	if record.Run != nil && record.Run.Attempt == attempt && dispatch != nil && dispatch.ReviewFeedback != nil {
		record.Run.State = contract.RunStatusRunning
		record.Run.Phase = contract.RunPhaseSummaryPreparing
		record.Run.Reason = nil
		record.Run.Decision = nil
		record.Run.ErrorCode = ""
		record.Run.ReviewSummary = dispatch.ReviewFeedback.Summary
		record.Run.ReviewFindings = append([]model.ReviewFinding(nil), dispatch.ReviewFeedback.Findings...)
		return record.Run
	}
	state := &model.RunState{
		Attempt:     attempt,
		State:       contract.RunStatusRunning,
		Phase:       contract.RunPhaseSummaryPreparing,
		Budget:      runBudgetForConfig(cfg),
		Recovery:    model.CloneRecoveryCheckpoint(nil),
		Checkpoints: nil,
	}
	if definition, ok := jobTypeDefinitionForDispatch(dispatch); ok && definition.ReviewGate.Required {
		state.ReviewGate = &contract.ReviewGate{
			ReviewGatePolicy: definition.ReviewGate,
			Status:           contract.ReviewGateStatusPending,
		}
	}
	if dispatch != nil && dispatch.RecoveryCheckpoint != nil {
		state.Recovery = model.CloneRecoveryCheckpoint(dispatch.RecoveryCheckpoint)
	}
	record.Run = state
	return state
}

func updateRunPhase(record *model.JobRuntime, phase contract.RunPhaseSummary) {
	if record == nil || record.Run == nil {
		return
	}
	record.Run.Phase = phase
}

func updateRunUsage(record *model.JobRuntime, executionMS int) {
	if record == nil || record.Run == nil || executionMS <= 0 {
		return
	}
	record.Run.Usage.ExecutionMS += executionMS
	record.Run.Usage.TotalMS += executionMS
}

func updateRunReviewUsage(record *model.JobRuntime, reviewMS int) {
	if record == nil || record.Run == nil || reviewMS <= 0 {
		return
	}
	record.Run.Usage.ReviewFixMS += reviewMS
	record.Run.Usage.TotalMS += reviewMS
}

func setRunContinuationPending(record *model.JobRuntime, checkpoint *model.RecoveryCheckpoint) {
	if record == nil || record.Run == nil {
		return
	}
	checkpointID := ""
	if checkpoint != nil {
		if checkpoint.ArtifactID != "" {
			checkpointID = checkpoint.ArtifactID
		} else if len(checkpoint.Checkpoint.ArtifactIDs) > 0 {
			checkpointID = checkpoint.Checkpoint.ArtifactIDs[0]
		}
	}
	record.Run.State = contract.RunStatusContinuationPending
	record.Run.Phase = contract.RunPhaseSummaryBlocked
	record.Run.Recovery = model.CloneRecoveryCheckpoint(checkpoint)
	record.Run.Decision = contractDecisionPtr(contract.MustDecision(contract.DecisionStartContinuationRun, map[string]any{
		"checkpoint_id": checkpointID,
	}))
	record.Run.ErrorCode = ""
	if checkpoint != nil {
		record.Run.Checkpoints = append(record.Run.Checkpoints, checkpoint.Checkpoint)
		record.Run.Reason = contractReasonPtr(contract.MustReason(contract.ReasonRunContinuationPending, map[string]any{
			"checkpoint_id": checkpointID,
		}))
	}
}

func setRunIntervention(record *model.JobRuntime, reason contract.Reason, decision contract.Decision, errorCode contract.ErrorCode) {
	if record == nil || record.Run == nil {
		return
	}
	record.Run.State = contract.RunStatusInterventionRequired
	record.Run.Phase = contract.RunPhaseSummaryBlocked
	record.Run.Reason = contractReasonPtr(reason)
	record.Run.Decision = contractDecisionPtr(decision)
	record.Run.ErrorCode = errorCode
}

func markRunCandidateDelivery(record *model.JobRuntime, pr *PullRequestInfo) {
	if record == nil || record.Run == nil {
		return
	}
	definition, ok := jobTypeDefinitionForDispatch(record.Dispatch)
	if !ok {
		return
	}
	artifactIDs := []string{}
	switch definition.Type {
	case contract.JobTypeCodeChange, contract.JobTypeLandChange:
		if pr == nil {
			return
		}
		artifactIDs = append(artifactIDs, fmt.Sprintf("pr-%d", pr.Number))
	case contract.JobTypeAnalysis:
		artifactIDs = append(artifactIDs, fmt.Sprintf("art-%s-analysis-%d", record.RecordID, currentAttempt(record)))
	case contract.JobTypeDiagnostic:
		artifactIDs = append(artifactIDs, fmt.Sprintf("art-%s-diagnostic-%d", record.RecordID, currentAttempt(record)))
	}
	record.Run.CandidateDelivery = &contract.CandidateDelivery{
		Kind:        definition.CandidateDelivery.Kind,
		Reached:     true,
		ReachedAt:   time.Now().UTC().Format(time.RFC3339Nano),
		Summary:     definition.CandidateDelivery.Summary,
		ArtifactIDs: artifactIDs,
	}
	checkpointReasonDetails := map[string]any{
		"record_id": record.RecordID,
	}
	checkpoint := contract.Checkpoint{
		Type:        contract.CheckpointTypeBusiness,
		Summary:     definition.BusinessCheckpoint.Summary,
		CapturedAt:  time.Now().UTC().Format(time.RFC3339Nano),
		Stage:       contract.RunPhaseSummaryPublishing,
		ArtifactIDs: artifactIDs,
	}
	if pr != nil {
		checkpoint.Branch = pr.HeadBranch
		checkpointReasonDetails["pr_number"] = pr.Number
	}
	checkpoint.Reason = contractReasonPtr(contract.MustReason(contract.ReasonCheckpointBusinessCaptured, checkpointReasonDetails))
	record.Run.Checkpoints = append(record.Run.Checkpoints, checkpoint)
}

func openRunReviewGate(record *model.JobRuntime) {
	if record == nil || record.Run == nil {
		return
	}
	if record.Run.ReviewGate == nil {
		if definition, ok := jobTypeDefinitionForDispatch(record.Dispatch); ok && definition.ReviewGate.Required {
			record.Run.ReviewGate = &contract.ReviewGate{
				ReviewGatePolicy: definition.ReviewGate,
				Status:           contract.ReviewGateStatusPending,
			}
		}
	}
	if record.Run.ReviewGate == nil {
		return
	}
	record.Run.ReviewGate.Status = contract.ReviewGateStatusReviewing
	record.Run.Reason = contractReasonPtr(contract.MustReason(contract.ReasonRunReviewGateCandidateReady, map[string]any{
		"record_id": record.RecordID,
	}))
	record.Run.Decision = contractDecisionPtr(contract.MustDecision(contract.DecisionOpenReviewGate, map[string]any{
		"record_id": record.RecordID,
	}))
	record.Run.ErrorCode = ""
}

func markRunReviewPassed(record *model.JobRuntime, summary string) {
	if record == nil || record.Run == nil {
		return
	}
	if record.Run.ReviewGate != nil {
		record.Run.ReviewGate.Status = contract.ReviewGateStatusPassed
	}
	record.Run.ReviewSummary = strings.TrimSpace(summary)
	record.Run.ReviewFindings = nil
}

func markRunReviewChangesRequested(record *model.JobRuntime, summary string, findings []model.ReviewFinding) {
	if record == nil || record.Run == nil {
		return
	}
	if record.Run.ReviewGate != nil {
		record.Run.ReviewGate.Status = contract.ReviewGateStatusChangesRequested
	}
	record.Run.ReviewSummary = strings.TrimSpace(summary)
	record.Run.ReviewFindings = append([]model.ReviewFinding(nil), findings...)
}

func latestRunCheckpoint(record *model.JobRuntime) *contract.Checkpoint {
	if record == nil || record.Run == nil || len(record.Run.Checkpoints) == 0 {
		return nil
	}
	checkpoint := record.Run.Checkpoints[len(record.Run.Checkpoints)-1]
	return &checkpoint
}

func contractReasonPtr(value contract.Reason) *contract.Reason {
	copyValue := value
	return &copyValue
}

func contractDecisionPtr(value contract.Decision) *contract.Decision {
	copyValue := value
	return &copyValue
}

func (o *Orchestrator) currentSourceKind() contract.SourceKind {
	return discoverySourceKind(o.currentRuntimeIdentity())
}

func (o *Orchestrator) currentSourceName() string {
	return discoverySourceName(o.currentRuntimeIdentity())
}

func (o *Orchestrator) ensureRecordIdentityLocked(record *model.JobRuntime, issueID string, identifier string) {
	if record == nil {
		return
	}
	sourceKind := o.currentSourceKind()
	sourceName := o.currentSourceName()
	record.RecordID = recordIDForSource(sourceKind, sourceName, issueID)
	record.SourceRef.SourceKind = sourceKind
	record.SourceRef.SourceName = strings.TrimSpace(sourceName)
	record.SourceRef.SourceID = strings.TrimSpace(issueID)
	record.SourceRef.SourceIdentifier = strings.TrimSpace(identifier)
}

func currentAttempt(record *model.JobRuntime) int {
	if record == nil {
		return 0
	}
	return record.RetryAttempt
}

func (o *Orchestrator) reconcileActiveRecovery(ctx context.Context) {
	o.reconcileRecovering(ctx)
}

func ledgerPathForConfig(cfg *model.ServiceConfig) string {
	if cfg == nil {
		return ""
	}
	return strings.TrimSpace(cfg.SessionPersistence.File.Path)
}

func recordIdentifier(record *model.JobRuntime) string {
	if record == nil {
		return ""
	}
	return strings.TrimSpace(record.SourceRef.SourceIdentifier)
}

func recordWorkspacePath(record *model.JobRuntime) string {
	if record == nil || record.DurableRefs.Workspace == nil {
		return ""
	}
	return strings.TrimSpace(record.DurableRefs.Workspace.Path)
}

func recordBranch(record *model.JobRuntime) string {
	if record == nil || record.DurableRefs.Branch == nil {
		return ""
	}
	return strings.TrimSpace(record.DurableRefs.Branch.Name)
}

func recordPullRequest(record *model.JobRuntime) *PullRequestInfo {
	if record == nil || record.DurableRefs.PullRequest == nil {
		return nil
	}
	pr := record.DurableRefs.PullRequest
	return &PullRequestInfo{
		Number:     pr.Number,
		URL:        pr.URL,
		State:      PullRequestState(pr.State),
		HeadBranch: recordBranch(record),
	}
}

func recordReasonDetailString(record *model.JobRuntime, key string) string {
	if record == nil || record.Reason == nil || record.Reason.Details == nil {
		return ""
	}
	value, _ := record.Reason.Details[key].(string)
	return strings.TrimSpace(value)
}

func isRecordRunning(record *model.JobRuntime) bool {
	return record != nil && record.Lifecycle == model.JobLifecycleActive && record.Observation != nil && record.Observation.Running
}

func isRetryScheduled(record *model.JobRuntime) bool {
	return record != nil && record.Lifecycle == model.JobLifecycleRetryScheduled
}

func isContinuationPending(record *model.JobRuntime) bool {
	return isRetryScheduled(record) &&
		record.Reason != nil &&
		record.Reason.ReasonCode == contract.ReasonRunContinuationPending
}

func isAwaitingMerge(record *model.JobRuntime) bool {
	return record != nil && record.Lifecycle == model.JobLifecycleAwaitingMerge
}

func isAwaitingIntervention(record *model.JobRuntime) bool {
	return record != nil && record.Lifecycle == model.JobLifecycleAwaitingIntervention
}

func (o *Orchestrator) jobRuntimeFromIssue(issue model.Issue, ledgerPath string) *model.JobRuntime {
	sourceKind := o.currentSourceKind()
	sourceName := o.currentSourceName()
	record := &model.JobRuntime{
		Lifecycle:           model.JobLifecycleActive,
		RecordID:            recordIDForIssue(sourceKind, sourceName, &issue),
		SourceRef:           sourceRefForIssue(sourceKind, sourceName, &issue),
		UpdatedAt:           time.Now().UTC().Format(time.RFC3339Nano),
		DurableRefs:         contract.DurableRefs{LedgerPath: ledgerPath},
		LastKnownIssue:      model.CloneIssue(&issue),
		LastKnownIssueState: strings.TrimSpace(issue.State),
	}
	return record
}

func (o *Orchestrator) ensureRecordLocked(issue model.Issue) *model.JobRuntime {
	record := o.state.Jobs[issue.ID]
	if record == nil {
		record = o.jobRuntimeFromIssue(issue, ledgerPathForConfig(o.currentConfig()))
		o.state.Jobs[issue.ID] = record
	}
	record.LastKnownIssue = model.CloneIssue(&issue)
	record.LastKnownIssueState = strings.TrimSpace(issue.State)
	sourceKind := o.currentSourceKind()
	sourceName := o.currentSourceName()
	record.SourceRef = sourceRefForIssue(sourceKind, sourceName, &issue)
	record.RecordID = recordIDForIssue(sourceKind, sourceName, &issue)
	record.DurableRefs.LedgerPath = ledgerPathForConfig(o.currentConfig())
	return record
}

func (o *Orchestrator) setRecordObservationLocked(record *model.JobRuntime, observation *contract.Observation) {
	if record == nil {
		return
	}
	record.Observation = cloneObservation(observation)
	record.UpdatedAt = o.now().UTC().Format(time.RFC3339Nano)
}

func (o *Orchestrator) setRecordReasonLocked(record *model.JobRuntime, reason *contract.Reason) {
	if record == nil {
		return
	}
	record.Reason = cloneReason(reason)
	record.UpdatedAt = o.now().UTC().Format(time.RFC3339Nano)
}

func (o *Orchestrator) setRecordStatusLocked(record *model.JobRuntime, status model.JobLifecycleState, reason *contract.Reason, observation *contract.Observation) {
	if record == nil {
		return
	}
	record.Lifecycle = status
	record.Reason = cloneReason(reason)
	record.Observation = cloneObservation(observation)
	record.UpdatedAt = o.now().UTC().Format(time.RFC3339Nano)
	if status != model.JobLifecycleRetryScheduled {
		record.RetryDueAt = nil
	}
}

func (o *Orchestrator) removeRecordLocked(issueID string) *model.JobRuntime {
	record := o.state.Jobs[issueID]
	delete(o.state.Jobs, issueID)
	delete(o.runningRecords, issueID)
	delete(o.retryRecords, issueID)
	delete(o.awaitingMergeRecords, issueID)
	delete(o.awaitingInterventionRecords, issueID)
	return record
}

func (o *Orchestrator) reindexRecordLocked(issueID string, record *model.JobRuntime) {
	delete(o.runningRecords, issueID)
	delete(o.retryRecords, issueID)
	delete(o.awaitingMergeRecords, issueID)
	delete(o.awaitingInterventionRecords, issueID)
	if record == nil {
		return
	}
	switch {
	case isRecordRunning(record):
		o.runningRecords[issueID] = record
	case isRetryScheduled(record):
		o.retryRecords[issueID] = record
	case isAwaitingMerge(record):
		o.awaitingMergeRecords[issueID] = record
	case isAwaitingIntervention(record):
		o.awaitingInterventionRecords[issueID] = record
	}
}

func recordUpdatedAt(record *model.JobRuntime, fallback time.Time) time.Time {
	if record == nil {
		return fallback
	}
	if ts, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(record.UpdatedAt)); err == nil {
		return ts
	}
	return fallback
}

func (o *Orchestrator) Start(ctx context.Context) error {
	if err := o.ensureRuntimeExtensions(); err != nil {
		return err
	}

	o.mu.Lock()
	if o.started {
		o.mu.Unlock()
		return nil
	}
	o.started = true
	o.runCtx = ctx
	interval := time.Duration(maxInt(o.state.PollIntervalMS, 1)) * time.Millisecond
	o.tickTimer = time.NewTimer(0)
	o.mu.Unlock()

	o.startupCleanup(ctx)
	o.mu.Lock()
	o.syncFormalObjectsLocked()
	o.refreshSnapshotLocked()
	o.publishSnapshotLocked()
	o.publishFormalEventsLocked()
	o.mu.Unlock()

	go func() {
		defer close(o.doneCh)
		for {
			select {
			case <-ctx.Done():
				o.gracefulShutdown()
				return
			case <-o.shutdownCh:
				o.gracefulShutdown()
				return
			case <-o.tickTimer.C:
				o.tick(ctx)
				o.mu.RLock()
				nextInterval := time.Duration(maxInt(o.state.PollIntervalMS, 1)) * time.Millisecond
				o.mu.RUnlock()
				o.tickTimer.Reset(nextInterval)
			case result := <-o.workerResultCh:
				o.handleWorkerExit(result)
			case update := <-o.codexUpdateCh:
				o.handleCodexUpdate(update)
			case <-o.refreshCh:
				o.tick(ctx)
			case issueID := <-o.retryFireCh:
				o.handleRetryTimer(ctx, issueID)
			case <-o.configReloadCh:
				o.mu.Lock()
				o.applyCurrentConfigLocked()
				oldNotifier := o.reloadNotifierLocked()
				o.reconcileNotificationHealthLocked()
				o.publishViewLocked()
				o.mu.Unlock()
				o.closeNotifier(oldNotifier)
			}
		}
	}()

	_ = interval
	return nil
}

func (o *Orchestrator) Stop() {
	o.shutdownOnce.Do(func() {
		close(o.shutdownCh)
	})
}

func (o *Orchestrator) Wait() {
	<-o.doneCh
}

func (o *Orchestrator) schedulePersistedActionLocked(version uint64, action func()) {
	if version == 0 || action == nil {
		return
	}
	o.pendingActions[version] = append(o.pendingActions[version], action)
}

func (o *Orchestrator) emitNotification(event model.RuntimeEvent) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.emitNotificationLocked(event)
}

func (o *Orchestrator) serviceModeLocked() model.ServiceMode {
	if o.state.Service.Mode == "" {
		return model.ServiceModeServing
	}
	return o.state.Service.Mode
}

func (o *Orchestrator) isProtectedLocked() bool {
	return o.serviceModeLocked() == model.ServiceModeUnavailable
}

func (o *Orchestrator) enterProtectedModeLocked(reason string) bool {
	if o.isProtectedLocked() {
		return false
	}
	o.state.Service.Mode = model.ServiceModeUnavailable
	o.state.Service.Reasons = []contract.Reason{
		serviceUnavailableReason("ledger_store", reason, o.currentSourceKind(), o.currentSourceName()),
	}
	o.setHealthAlertAndNotifyLocked(AlertSnapshot{
		Code:    serviceProtectedModeCode,
		Level:   "warn",
		Message: fmt.Sprintf("service became unavailable: %s", reason),
	})
	return true
}

func serviceUnavailableReason(component string, detail string, sourceKind contract.SourceKind, sourceName string) contract.Reason {
	return contract.MustReason(contract.ReasonServiceUnavailableCoreDependency, map[string]any{
		"component":   strings.TrimSpace(component),
		"source_kind": sourceKind,
		"source_name": strings.TrimSpace(sourceName),
		"detail":      strings.TrimSpace(detail),
	})
}

func (o *Orchestrator) coreDependencyReasonsLocked() []contract.Reason {
	identity := o.currentRuntimeIdentity()
	sourceKind := discoverySourceKind(identity)
	sourceName := discoverySourceName(identity)
	mappings := []struct {
		alertCode string
		component string
	}{
		{alertCode: "dispatch_preflight_failed", component: "dispatch_preflight"},
		{alertCode: "tracker_unreachable", component: "task_source"},
	}
	reasons := make([]contract.Reason, 0, len(mappings))
	for _, mapping := range mappings {
		alert, ok := o.healthAlerts[mapping.alertCode]
		if !ok {
			continue
		}
		reasons = append(reasons, serviceUnavailableReason(mapping.component, alert.Message, sourceKind, sourceName))
	}
	return reasons
}

func (o *Orchestrator) rememberProtectedResultLocked(issueID string, entry *model.JobRuntime, result WorkerResult) {
	if strings.TrimSpace(issueID) == "" || entry == nil {
		return
	}
	outcome := model.ProtectedResultOutcomeSucceeded
	var errText *string
	if result.Err != nil {
		outcome = model.ProtectedResultOutcomeFailed
		text := result.Err.Error()
		errText = &text
	}
	resultEntry := &model.ProtectedResultEntry{
		Identifier:    entry.SourceRef.SourceIdentifier,
		WorkspacePath: entry.DurableRefs.Workspace.Path,
		Outcome:       outcome,
		Phase:         result.Phase,
		Error:         errText,
		FinalBranch:   strings.TrimSpace(result.FinalBranch),
		ObservedAt:    o.now().UTC(),
		RetryAttempt:  entry.RetryAttempt,
		Dispatch:      model.CloneDispatchContext(entry.Dispatch),
	}
	o.state.ProtectedResults[issueID] = resultEntry
}

func (o *Orchestrator) RequestRefresh() RefreshRequestResult {
	o.mu.RLock()
	serviceMode, _, _ := o.publicServiceStateLocked()
	o.mu.RUnlock()

	timestamp := o.now().UTC().Format(time.RFC3339Nano)
	if serviceMode == contract.ServiceModeUnavailable {
		reason := contract.MustReason(contract.ReasonControlRefreshRejectedServiceMode, map[string]any{
			"service_mode": serviceMode,
		})
		return contract.ControlResult{
			Action:              contract.ControlActionRefresh,
			Status:              contract.ControlStatusRejected,
			Reason:              &reason,
			RecommendedNextStep: "检查核心依赖后重试",
			Timestamp:           timestamp,
		}
	}

	reasonDetails := map[string]any{
		"service_mode": serviceMode,
	}
	select {
	case o.refreshCh <- struct{}{}:
	default:
		reasonDetails["coalesced"] = true
	}
	reason := contract.MustReason(contract.ReasonControlRefreshAccepted, reasonDetails)
	return contract.ControlResult{
		Action:              contract.ControlActionRefresh,
		Status:              contract.ControlStatusAccepted,
		Reason:              &reason,
		RecommendedNextStep: "等待 SSE 通知后回读 /api/v1/state",
		Timestamp:           timestamp,
	}
}

func (o *Orchestrator) NotifyWorkflowReload(_ *model.WorkflowDefinition) {
	select {
	case o.configReloadCh <- struct{}{}:
	default:
	}
}

func (o *Orchestrator) Snapshot() Snapshot {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.refreshSnapshotLocked()
	return o.snapshot
}

func (o *Orchestrator) Discovery() DiscoveryDocument {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.refreshSnapshotLocked()
	identity := o.currentRuntimeIdentity()
	sourceKind := discoverySourceKind(identity)
	sourceName := discoverySourceName(identity)
	cfg := o.currentConfig()
	instanceName := "symphony"
	domainID := ""
	capabilities := contract.StaticCapabilitySet{}
	apiVersion := contract.APIVersionV1
	if cfg != nil {
		if strings.TrimSpace(cfg.ServiceInstanceName) != "" {
			instanceName = strings.TrimSpace(cfg.ServiceInstanceName)
		}
		domainID = strings.TrimSpace(cfg.DomainID)
		capabilities = cfg.CapabilityContract.Static
		if cfg.ServiceContractVersion != "" {
			apiVersion = cfg.ServiceContractVersion
		}
	}

	return contract.DiscoveryDocument{
		APIVersion: apiVersion,
		Instance: contract.InstanceDocument{
			ID:      discoveryInstanceID(identity),
			Name:    instanceName,
			Version: o.serviceVersion,
		},
		DomainID: domainID,
		Source: contract.SourceDocument{
			Kind: sourceKind,
			Name: sourceName,
		},
		Capabilities: capabilities,
	}
}

func (o *Orchestrator) SubscribeSnapshots(buffer int) (<-chan Snapshot, func()) {
	if buffer <= 0 {
		buffer = 1
	}
	ch := make(chan Snapshot, buffer)

	o.mu.Lock()
	id := o.nextSubscriberID
	o.nextSubscriberID++
	o.refreshSnapshotLocked()
	snapshot := o.snapshot
	o.subscribers[id] = ch
	o.mu.Unlock()

	select {
	case ch <- snapshot:
	default:
	}

	unsubscribe := func() {
		o.mu.Lock()
		defer o.mu.Unlock()
		if existing, ok := o.subscribers[id]; ok {
			delete(o.subscribers, id)
			close(existing)
		}
	}

	return ch, unsubscribe
}

func (o *Orchestrator) RunOnce(ctx context.Context, allowDispatch bool) {
	if err := o.ensureRuntimeExtensions(); err != nil {
		o.logger.Error("runtime extensions initialization failed", "error", err.Error())
		return
	}
	o.startupCleanup(ctx)
	o.tickWithMode(ctx, allowDispatch)
}

func (o *Orchestrator) tick(ctx context.Context) {
	o.tickWithMode(ctx, true)
}

func (o *Orchestrator) tickWithMode(ctx context.Context, allowDispatch bool) {
	cfg := o.currentConfig()
	if cfg.LeaderRequired && currentInstanceRole(cfg) != contract.InstanceRoleLeader {
		o.mu.Lock()
		if o.isProtectedLocked() {
			o.refreshSnapshotLocked()
			o.publishSnapshotLocked()
			o.publishFormalEventsLocked()
			o.mu.Unlock()
			return
		}
		o.refreshSnapshotLocked()
		o.publishSnapshotLocked()
		o.publishFormalEventsLocked()
		o.mu.Unlock()
		return
	}

	o.mu.Lock()
	if o.isProtectedLocked() {
		o.refreshSnapshotLocked()
		o.publishSnapshotLocked()
		o.mu.Unlock()
		return
	}
	o.mu.Unlock()

	stateRefreshAttempted, stateRefreshSucceeded := o.reconcileRunning(ctx)
	o.reconcileRecovering(ctx)
	o.reconcileAwaitingMerge(ctx)
	o.reconcileAwaitingIntervention(ctx)
	o.reconcileSourceClosureActions(ctx)
	if err := config.ValidateForDispatch(cfg); err != nil {
		o.logger.Warn("dispatch preflight failed", "error", err.Error())
		o.mu.Lock()
		if o.setHealthAlertAndNotifyLocked(AlertSnapshot{
			Code:    "dispatch_preflight_failed",
			Level:   "warn",
			Message: err.Error(),
		}) {
			o.publishViewLocked()
		}
		o.mu.Unlock()
		return
	}
	o.mu.Lock()
	if o.clearHealthAlertAndNotifyLocked("dispatch_preflight_failed") {
		o.publishViewLocked()
	}
	o.mu.Unlock()

	o.processDueRetries(ctx)

	candidates, err := o.tracker.FetchCandidateIssues(ctx)
	if err != nil {
		o.logger.Warn("fetch candidate issues failed", "error", err.Error())
		o.mu.Lock()
		if o.setHealthAlertAndNotifyLocked(AlertSnapshot{
			Code:    "tracker_unreachable",
			Level:   "warn",
			Message: err.Error(),
		}) {
			o.publishViewLocked()
		}
		o.mu.Unlock()
		return
	}
	o.mu.Lock()
	if !stateRefreshAttempted || stateRefreshSucceeded {
		if o.clearHealthAlertAndNotifyLocked("tracker_unreachable") {
			o.publishViewLocked()
		}
	}
	o.mu.Unlock()
	sort.SliceStable(candidates, func(i int, j int) bool {
		return compareIssues(candidates[i], candidates[j])
	})
	if !allowDispatch {
		o.mu.Lock()
		o.refreshSnapshotLocked()
		o.publishSnapshotLocked()
		o.mu.Unlock()
		return
	}
	o.dispatchEligibleIssues(ctx, candidates)
	o.mu.Lock()
	o.refreshSnapshotLocked()
	o.publishSnapshotLocked()
	o.mu.Unlock()
}

func (o *Orchestrator) dispatchEligibleIssues(ctx context.Context, candidates []model.Issue) {
	o.mu.RLock()
	protected := o.isProtectedLocked()
	o.mu.RUnlock()
	if protected {
		return
	}
	cfg := o.currentConfig()
	sorted := append([]model.Issue(nil), candidates...)
	sort.SliceStable(sorted, func(i int, j int) bool {
		return compareIssues(sorted[i], sorted[j])
	})
	for _, issue := range sorted {
		if !o.isDispatchEligible(issue, cfg, false) {
			continue
		}
		if !o.hasAvailableSlots(issue, cfg) {
			continue
		}
		o.dispatchIssue(ctx, issue, nil)
	}
}

func (o *Orchestrator) dispatchIssue(ctx context.Context, issue model.Issue, attempt *int) {
	o.mu.RLock()
	protected := o.isProtectedLocked()
	o.mu.RUnlock()
	if protected {
		return
	}
	workerCtx := ctx
	if o.runCtx != nil {
		workerCtx = o.runCtx
	}
	workerCtx, cancel := context.WithCancel(workerCtx)
	completion := normalizeCompletionContract(o.currentWorkflow().Completion)

	o.mu.Lock()
	if o.isProtectedLocked() {
		o.mu.Unlock()
		cancel()
		return
	}
	stallCount := 0
	var dispatch *model.DispatchContext
	record := o.ensureRecordLocked(issue)
	if isRetryScheduled(record) {
		stallCount = record.StallCount
		if record.RetryTimer != nil {
			record.RetryTimer.Stop()
			record.RetryTimer = nil
		}
		dispatch = model.CloneDispatchContext(record.Dispatch)
	} else if record.Dispatch != nil && record.Dispatch.Kind == model.DispatchKindInterventionRetry {
		dispatch = model.CloneDispatchContext(record.Dispatch)
	} else if record.Dispatch != nil && record.Dispatch.ReviewFeedback != nil {
		dispatch = model.CloneDispatchContext(record.Dispatch)
	}
	normalizedAttempt := 0
	if attempt != nil {
		normalizedAttempt = *attempt
	}
	if dispatch == nil {
		dispatch = freshDispatchContext(completion)
	}
	if dispatch.RetryAttempt == nil && normalizedAttempt > 0 {
		retryAttempt := normalizedAttempt
		dispatch.RetryAttempt = &retryAttempt
	}
	startedAt := o.now().UTC()
	record.RetryAttempt = normalizedAttempt
	record.StallCount = stallCount
	record.StartedAt = &startedAt
	record.WorkerCancel = cancel
	record.Dispatch = model.CloneDispatchContext(dispatch)
	record.Session = model.LiveSession{}
	record.NeedsRecovery = false
	record.Run = ensureRunState(record, o.currentConfig(), dispatch, attemptCountFromRetry(normalizedAttempt))
	record.Result = nil
	record.LastKnownIssue = model.CloneIssue(&issue)
	record.LastKnownIssueState = strings.TrimSpace(issue.State)
	o.setRecordStatusLocked(record, model.JobLifecycleActive, nil, &contract.Observation{
		Running: true,
		Summary: "agent run in progress",
		Details: map[string]any{
			"attempt":   attemptCountFromRetry(normalizedAttempt),
			"issue_id":  issue.ID,
			"issue_key": issue.Identifier,
		},
	})
	o.reindexRecordLocked(issue.ID, record)
	o.logger.Info(
		"dispatching issue",
		"issue_id", issue.ID,
		"issue_identifier", issue.Identifier,
		"attempt", attemptCountFromRetry(normalizedAttempt),
		"run_phase", model.PhasePreparingWorkspace.String(),
	)
	version := o.commitStateLocked(true)
	issueCopy := issue
	dispatchCopy := model.CloneDispatchContext(dispatch)
	action := func() {
		o.mu.Lock()
		delete(o.pendingLaunch, issueCopy.ID)
		o.mu.Unlock()
		o.emitNotification(o.newIssueDispatchedEvent(issueCopy, normalizedAttempt, dispatchCopy))
		o.launchWorker(workerCtx, issueCopy, attempt, dispatchCopy)
	}
	if version > 0 {
		o.pendingLaunch[issue.ID] = version
		o.schedulePersistedActionLocked(version, action)
	}
	o.mu.Unlock()
	if version == 0 {
		action()
	}
}

func (o *Orchestrator) launchWorker(workerCtx context.Context, issue model.Issue, attempt *int, dispatch *model.DispatchContext) {
	o.workerWG.Add(1)
	go func() {
		defer o.workerWG.Done()

		result := WorkerResult{
			Kind:       WorkerKindExecution,
			IssueID:    issue.ID,
			Identifier: issue.Identifier,
			Attempt:    attempt,
			StartedAt:  o.now().UTC(),
			Phase:      model.PhasePreparingWorkspace,
		}

		workspaceRef, err := o.workspace.CreateForIssue(workerCtx, issue.Identifier)
		if err != nil {
			result.Err = err
			o.sendWorkerResult(result)
			return
		}
		workspaceRef.Dispatch = model.CloneDispatchContext(dispatch)
		o.mu.Lock()
		if entry := o.state.Jobs[issue.ID]; isRecordRunning(entry) {
			entry.DurableRefs.Workspace = &contract.WorkspaceRef{Path: workspaceRef.Path}
			entry.Dispatch = model.CloneDispatchContext(dispatch)
			o.commitStateLocked(false)
		}
		o.mu.Unlock()
		if preparer, ok := o.workspace.(interface {
			PrepareForRun(context.Context, *model.Workspace) error
		}); ok {
			if err := preparer.PrepareForRun(workerCtx, workspaceRef); err != nil {
				result.Err = err
				o.sendWorkerResult(result)
				return
			}
		}

		workflowDef := o.currentWorkflow()
		rawPrompt, promptErr := buildExecutionPrompt(workflowDef, issue, attempt, dispatch)
		if promptErr != nil {
			result.Err = model.NewAgentError(model.ErrResponseError, "render worker prompt", promptErr)
			o.sendWorkerResult(result)
			return
		}

		result.Phase = model.PhaseStreamingTurn
		runErr := o.runner.Run(workerCtx, agent.RunParams{
			Issue:             model.CloneIssue(&issue),
			Attempt:           attempt,
			WorkspacePath:     workspaceRef.Path,
			PromptTemplate:    workflowDef.PromptTemplate,
			RawPrompt:         rawPrompt,
			Source:            workflowDef.Source,
			Dispatch:          model.CloneDispatchContext(dispatch),
			ProcessEnv:        workspaceProcessEnv(workspaceRef),
			MaxTurns:          o.currentConfig().MaxTurns,
			ExecutionBudgetMS: o.currentConfig().RunExecutionBudgetMS,
			RefetchIssue: func(ctx context.Context, issueID string) (*model.Issue, error) {
				issues, err := o.tracker.FetchIssueStatesByIDs(ctx, []string{issueID})
				if err != nil {
					return nil, err
				}
				if len(issues) == 0 {
					return nil, nil
				}
				return model.CloneIssue(&issues[0]), nil
			},
			IsActive: func(state string) bool { return o.isActiveState(state, o.currentConfig()) },
			OnEvent: func(event agent.AgentEvent) {
				o.sendCodexUpdate(CodexUpdate{IssueID: issue.ID, Event: event})
			},
		})
		if finalizer, ok := o.workspace.(interface {
			FinalizeRun(context.Context, *model.Workspace)
		}); ok {
			finalizer.FinalizeRun(workerCtx, workspaceRef)
		}

		if runErr != nil {
			result.Err = runErr
			result.Phase = phaseFromError(runErr)
		} else {
			result.Phase = model.PhaseFinishing
			finalBranch, branchErr := o.currentBranch(workerCtx, workspaceRef.Path)
			if branchErr != nil {
				result.Err = branchErr
				result.Phase = phaseFromError(branchErr)
			} else {
				result.Phase = model.PhaseSucceeded
				result.FinalBranch = finalBranch
			}
		}
		o.sendWorkerResult(result)
	}()
}

func buildExecutionPrompt(workflowDef *model.WorkflowDefinition, issue model.Issue, attempt *int, dispatch *model.DispatchContext) (string, error) {
	var promptTemplate string
	var source map[string]any
	if workflowDef != nil {
		promptTemplate = workflowDef.PromptTemplate
		source = workflowDef.Source
	}
	prompt, err := workflow.RenderPrompt(promptTemplate, &issue, attempt, source, dispatch)
	if err != nil {
		return "", err
	}
	if dispatch == nil || dispatch.ReviewFeedback == nil {
		return prompt, nil
	}
	return prompt + formatReviewFeedbackPrompt(dispatch.ReviewFeedback), nil
}

func formatReviewFeedbackPrompt(feedback *model.ReviewFeedbackContext) string {
	if feedback == nil {
		return ""
	}
	lines := []string{
		"",
		"Platform review feedback for this same Run:",
	}
	if strings.TrimSpace(feedback.Summary) != "" {
		lines = append(lines, "Summary: "+strings.TrimSpace(feedback.Summary))
	}
	if len(feedback.Findings) > 0 {
		lines = append(lines, "Address all requested changes before presenting the next candidate delivery:")
		for _, finding := range feedback.Findings {
			line := strings.TrimSpace(finding.Summary)
			if strings.TrimSpace(finding.Code) != "" {
				line = fmt.Sprintf("%s: %s", strings.TrimSpace(finding.Code), line)
			}
			if line != "" {
				lines = append(lines, "- "+line)
			}
		}
	}
	return "\n" + strings.Join(lines, "\n")
}

func reviewBudgetForConfig(cfg *model.ServiceConfig) int {
	if cfg == nil {
		return 0
	}
	if cfg.RunReviewFixBudgetMS > 0 {
		return cfg.RunReviewFixBudgetMS
	}
	return cfg.RunExecutionBudgetMS
}

func buildReviewPrompt(issue model.Issue, record *model.JobRuntime) string {
	definition, _ := jobTypeDefinitionForDispatch(record.Dispatch)
	candidateSummary := ""
	if record != nil && record.Run != nil && record.Run.CandidateDelivery != nil {
		candidateSummary = strings.TrimSpace(record.Run.CandidateDelivery.Summary)
	}
	artifactList := ""
	if record != nil && record.Run != nil && record.Run.CandidateDelivery != nil && len(record.Run.CandidateDelivery.ArtifactIDs) > 0 {
		artifactList = strings.Join(record.Run.CandidateDelivery.ArtifactIDs, ", ")
	}
	pr := recordPullRequest(record)
	lines := []string{
		"You are Symphony platform reviewer.",
		"Read-only review only. Do not edit code. Do not edit documents. Do not invoke tools. Do not delegate or spawn sub-agents.",
		fmt.Sprintf("Issue: %s", issue.Identifier),
		fmt.Sprintf("Job type: %s", definition.Type),
		fmt.Sprintf("Candidate rule: %s", definition.CandidateDelivery.Summary),
	}
	if candidateSummary != "" {
		lines = append(lines, "Current candidate: "+candidateSummary)
	}
	if artifactList != "" {
		lines = append(lines, "Candidate artifacts: "+artifactList)
	}
	if pr != nil {
		lines = append(lines, fmt.Sprintf("Pull request: #%d %s (%s)", pr.Number, pr.URL, pr.State))
	}
	lines = append(lines,
		"Return exactly one JSON object and nothing else.",
		`{"status":"passed|changes_requested","summary":"short summary","findings":[{"code":"short_code","summary":"what must be fixed"}]}`,
	)
	return strings.Join(lines, "\n")
}

type reviewResponsePayload struct {
	Status   string                `json:"status"`
	Summary  string                `json:"summary"`
	Findings []model.ReviewFinding `json:"findings"`
}

func parseReviewOutput(raw string) (contract.ReviewGateStatus, string, []model.ReviewFinding, error) {
	body := strings.TrimSpace(raw)
	if strings.HasPrefix(body, "```") {
		body = strings.TrimPrefix(body, "```json")
		body = strings.TrimPrefix(body, "```")
		body = strings.TrimSuffix(strings.TrimSpace(body), "```")
		body = strings.TrimSpace(body)
	}
	start := strings.Index(body, "{")
	end := strings.LastIndex(body, "}")
	if start < 0 || end < start {
		return "", "", nil, fmt.Errorf("reviewer output does not contain a JSON object")
	}
	body = body[start : end+1]
	var payload reviewResponsePayload
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		return "", "", nil, err
	}
	status := contract.ReviewGateStatus(strings.TrimSpace(payload.Status))
	switch status {
	case contract.ReviewGateStatusPassed, contract.ReviewGateStatusChangesRequested:
	default:
		return "", "", nil, fmt.Errorf("unsupported review status %q", payload.Status)
	}
	return status, strings.TrimSpace(payload.Summary), append([]model.ReviewFinding(nil), payload.Findings...), nil
}

func (o *Orchestrator) launchReviewWorker(workerCtx context.Context, issue model.Issue, record *model.JobRuntime) {
	if record == nil {
		return
	}
	workspacePath := recordWorkspacePath(record)
	attempt := record.RetryAttempt
	dispatch := model.CloneDispatchContext(record.Dispatch)
	o.workerWG.Add(1)
	go func() {
		defer o.workerWG.Done()
		result := WorkerResult{
			Kind:       WorkerKindReview,
			IssueID:    issue.ID,
			Identifier: issue.Identifier,
			Attempt:    &attempt,
			StartedAt:  o.now().UTC(),
			Phase:      model.PhaseStreamingTurn,
		}
		if strings.TrimSpace(workspacePath) == "" {
			result.Err = model.NewAgentError(model.ErrInvalidWorkspaceCWD, "review workspace path is empty", nil)
			result.Phase = phaseFromError(result.Err)
			o.sendWorkerResult(result)
			return
		}
		reviewWorkspace := &model.Workspace{
			Path:       workspacePath,
			Identifier: issue.Identifier,
			Dispatch:   model.CloneDispatchContext(dispatch),
		}
		fragments := make([]string, 0, 4)
		runErr := o.runner.Run(workerCtx, agent.RunParams{
			Issue:             model.CloneIssue(&issue),
			WorkspacePath:     workspacePath,
			RawPrompt:         buildReviewPrompt(issue, record),
			Dispatch:          model.CloneDispatchContext(dispatch),
			ProcessEnv:        workspaceProcessEnv(reviewWorkspace),
			MaxTurns:          1,
			ExecutionBudgetMS: reviewBudgetForConfig(o.currentConfig()),
			ReadOnly:          true,
			OnAssistantText: func(text string) {
				fragments = append(fragments, text)
			},
			OnEvent: func(event agent.AgentEvent) {
				o.sendCodexUpdate(CodexUpdate{IssueID: issue.ID, Event: event})
			},
		})
		if runErr != nil {
			result.Err = runErr
			result.Phase = phaseFromError(runErr)
			o.sendWorkerResult(result)
			return
		}
		status, summary, findings, err := parseReviewOutput(strings.Join(fragments, "\n"))
		if err != nil {
			result.Err = model.NewAgentError(model.ErrResponseError, "parse reviewer output", err)
			result.Phase = phaseFromError(result.Err)
			o.sendWorkerResult(result)
			return
		}
		result.Phase = model.PhaseSucceeded
		result.ReviewStatus = status
		result.ReviewSummary = summary
		result.ReviewFindings = findings
		o.sendWorkerResult(result)
	}()
}

func workspaceProcessEnv(workspace *model.Workspace) map[string]string {
	if workspace == nil {
		return nil
	}
	name := strings.TrimSpace(workspace.GitAuthorName)
	email := strings.TrimSpace(workspace.GitAuthorEmail)
	if name == "" || email == "" {
		return nil
	}
	return map[string]string{
		"GIT_AUTHOR_NAME":     name,
		"GIT_AUTHOR_EMAIL":    email,
		"GIT_COMMITTER_NAME":  name,
		"GIT_COMMITTER_EMAIL": email,
	}
}

func normalizeCompletionContract(contract model.CompletionContract) model.CompletionContract {
	if contract.Mode == "" {
		contract.Mode = model.CompletionModeNone
	}
	if contract.Mode == model.CompletionModePullRequest {
		if contract.OnMissingPR == "" {
			contract.OnMissingPR = model.CompletionActionIntervention
		}
		if contract.OnClosedPR == "" {
			contract.OnClosedPR = model.CompletionActionIntervention
		}
		return contract
	}
	if contract.OnMissingPR == "" {
		contract.OnMissingPR = model.CompletionActionContinue
	}
	if contract.OnClosedPR == "" {
		contract.OnClosedPR = model.CompletionActionContinue
	}
	return contract
}

func freshDispatchContext(contract model.CompletionContract) *model.DispatchContext {
	contract = normalizeCompletionContract(contract)
	return &model.DispatchContext{
		JobType:         jobTypeForCompletion(contract),
		Kind:            model.DispatchKindFresh,
		ExpectedOutcome: contract.Mode,
		OnMissingPR:     contract.OnMissingPR,
		OnClosedPR:      contract.OnClosedPR,
	}
}

func jobTypeForCompletion(completion model.CompletionContract) contract.JobType {
	if completion.Mode == model.CompletionModePullRequest {
		return contract.JobTypeCodeChange
	}
	return contract.JobTypeAnalysis
}

func dispatchCompletionAction(dispatch *model.DispatchContext, key string) model.CompletionAction {
	if dispatch == nil {
		return model.CompletionActionContinue
	}
	switch key {
	case "missing":
		if dispatch.OnMissingPR != "" {
			return dispatch.OnMissingPR
		}
	case "closed":
		if dispatch.OnClosedPR != "" {
			return dispatch.OnClosedPR
		}
	}
	return model.CompletionActionContinue
}

func continuationDispatchContext(base *model.DispatchContext, fallback model.CompletionContract, reason model.ContinuationReason, branch string, pr *PullRequestInfo, issueState string) *model.DispatchContext {
	fallback = normalizeCompletionContract(fallback)
	dispatch := model.CloneDispatchContext(base)
	if dispatch == nil {
		dispatch = freshDispatchContext(fallback)
	}
	dispatch.Kind = model.DispatchKindContinuation
	if dispatch.ExpectedOutcome == "" {
		dispatch.ExpectedOutcome = fallback.Mode
	}
	if !dispatch.JobType.IsValid() {
		dispatch.JobType = jobTypeForCompletion(fallback)
	}
	if dispatch.OnMissingPR == "" {
		dispatch.OnMissingPR = fallback.OnMissingPR
	}
	if dispatch.OnClosedPR == "" {
		dispatch.OnClosedPR = fallback.OnClosedPR
	}
	dispatch.Reason = reasonPtr(reason)
	if strings.TrimSpace(branch) != "" {
		dispatch.PreviousBranch = dispatchStringPtr(strings.TrimSpace(branch))
	}
	dispatch.PreviousPR = pullRequestContext(pr)
	if strings.TrimSpace(issueState) != "" {
		dispatch.PreviousIssueState = dispatchStringPtr(strings.TrimSpace(issueState))
	}
	dispatch.ReviewFeedback = nil
	return dispatch
}

func reasonPtr(value model.ContinuationReason) *model.ContinuationReason {
	copyValue := value
	return &copyValue
}

func dispatchStringPtr(value string) *string {
	copyValue := value
	return &copyValue
}

func pullRequestContext(pr *PullRequestInfo) *model.PRContext {
	if pr == nil {
		return nil
	}
	return &model.PRContext{
		Number:     pr.Number,
		URL:        pr.URL,
		State:      string(pr.State),
		Merged:     pr.State == PullRequestStateMerged,
		HeadBranch: pr.HeadBranch,
	}
}

func clonePullRequestInfo(pr *PullRequestInfo) *PullRequestInfo {
	if pr == nil {
		return nil
	}
	copyPR := *pr
	return &copyPR
}

func pullRequestInfoFromContext(pr *model.PRContext) *PullRequestInfo {
	if pr == nil {
		return nil
	}
	return &PullRequestInfo{
		Number:     pr.Number,
		URL:        pr.URL,
		HeadBranch: pr.HeadBranch,
		State:      PullRequestState(pr.State),
	}
}

func (o *Orchestrator) currentBranch(ctx context.Context, workspacePath string) (string, error) {
	if o.gitBranchFn == nil {
		return "", model.NewAgentError(model.ErrResponseError, "detect final branch", errors.New("branch detection is not configured"))
	}
	branch, err := o.gitBranchFn(ctx, workspacePath)
	if err != nil {
		o.logger.Warn("post-run branch detection failed", "workspace_path", workspacePath, "error", err.Error())
		return "", model.NewAgentError(model.ErrResponseError, "detect final branch", err)
	}
	trimmed := strings.TrimSpace(branch)
	if trimmed == "" {
		return "", model.NewAgentError(model.ErrResponseError, "detect final branch: branch is empty", nil)
	}
	return trimmed, nil
}

func (o *Orchestrator) captureRecoveryCheckpoint(ctx context.Context, record *model.JobRuntime, stage contract.RunPhaseSummary, reason *contract.Reason) (*model.RecoveryCheckpoint, error) {
	if record == nil {
		return nil, fmt.Errorf("record is nil")
	}
	workspacePath := recordWorkspacePath(record)
	if strings.TrimSpace(workspacePath) == "" {
		return nil, fmt.Errorf("workspace path is empty")
	}

	attempt := 1
	if record.Run != nil && record.Run.Attempt > 0 {
		attempt = record.Run.Attempt
	}
	artifactID := continuationArtifactID(record, attempt)
	checkpointID := continuationCheckpointID(record, attempt)
	ledgerPath := ledgerPathForConfig(o.currentConfig())
	checkpointRoot := filepath.Join(filepath.Dir(ledgerPath), "checkpoints")
	if strings.TrimSpace(ledgerPath) == "" {
		checkpointRoot = filepath.Join(workspacePath, ".symphony", "checkpoints")
	}
	if err := os.MkdirAll(checkpointRoot, 0o755); err != nil {
		return nil, err
	}
	fileBase := model.SanitizeWorkspaceKey(recordIdentifier(record))
	if fileBase == "" {
		fileBase = "record"
	}
	patchPath := filepath.Join(checkpointRoot, fmt.Sprintf("%s-%d.patch", fileBase, attempt))

	baseSHA := ""
	if stdout, _, err := runBashOutputWithTimeout(ctx, workspacePath, "git rev-parse HEAD", 15*time.Second); err == nil {
		baseSHA = strings.TrimSpace(stdout)
	}
	branch := recordBranch(record)
	if strings.TrimSpace(branch) == "" {
		if currentBranch, err := o.currentBranch(ctx, workspacePath); err == nil {
			branch = currentBranch
		}
	}

	patchScript := `git diff --binary --no-color --no-ext-diff HEAD -- .
while IFS= read -r file; do
  git diff --binary --no-color --no-ext-diff --no-index -- /dev/null "$file" || true
done < <(git ls-files --others --exclude-standard)`
	patchBody, _, err := runBashOutputWithTimeout(ctx, workspacePath, patchScript, 30*time.Second)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(patchPath, []byte(patchBody), 0o644); err != nil {
		return nil, err
	}

	hiddenRef := ""
	hiddenCommit := ""
	if stashSHA, _, err := runBashOutputWithTimeout(ctx, workspacePath, "git stash create 'symphony recovery checkpoint'", 15*time.Second); err == nil {
		hiddenCommit = strings.TrimSpace(stashSHA)
		if hiddenCommit != "" {
			hiddenRef = fmt.Sprintf("refs/symphony/recovery/%s/%d", fileBase, attempt)
			if _, _, updateErr := runBashOutputWithTimeout(ctx, workspacePath, fmt.Sprintf("git update-ref %s %s", bashSingleQuote(hiddenRef), bashSingleQuote(hiddenCommit)), 15*time.Second); updateErr != nil {
				hiddenRef = ""
				hiddenCommit = ""
			}
		}
	}

	checkpointReason := contract.MustReason(contract.ReasonCheckpointRecoveryCaptured, map[string]any{
		"checkpoint_id": checkpointID,
		"artifact_id":   artifactID,
		"patch_path":    patchPath,
		"workspace":     workspacePath,
	})
	if reason != nil && reason.ReasonCode != "" {
		if checkpointReason.Details == nil {
			checkpointReason.Details = map[string]any{}
		}
		checkpointReason.Details["cause"] = reason.ReasonCode
	}
	checkpoint := contract.Checkpoint{
		Type:        contract.CheckpointTypeRecovery,
		Summary:     "已记录可续跑的恢复 checkpoint。",
		CapturedAt:  o.now().UTC().Format(time.RFC3339Nano),
		Stage:       stage,
		ArtifactIDs: []string{artifactID},
		BaseSHA:     baseSHA,
		Branch:      strings.TrimSpace(branch),
		Reason:      contractReasonPtr(checkpointReason),
	}
	return &model.RecoveryCheckpoint{
		ArtifactID:    artifactID,
		PatchPath:     patchPath,
		WorkspacePath: workspacePath,
		HiddenRef:     hiddenRef,
		HiddenCommit:  hiddenCommit,
		Checkpoint:    checkpoint,
	}, nil
}

func (o *Orchestrator) classifySuccessfulRun(ctx context.Context, workspacePath string, finalBranch string, dispatch *model.DispatchContext, issueState string) (*SuccessfulRunDecision, error) {
	completion := normalizeCompletionContract(model.CompletionContract{
		Mode:        model.CompletionModeNone,
		OnMissingPR: dispatchCompletionAction(dispatch, "missing"),
		OnClosedPR:  dispatchCompletionAction(dispatch, "closed"),
	})
	jobType := jobTypeForDispatch(dispatch)
	if dispatch != nil {
		if dispatch.ExpectedOutcome != "" {
			completion.Mode = dispatch.ExpectedOutcome
		}
		if dispatch.OnMissingPR != "" {
			completion.OnMissingPR = dispatch.OnMissingPR
		}
		if dispatch.OnClosedPR != "" {
			completion.OnClosedPR = dispatch.OnClosedPR
		}
	}
	branch := strings.TrimSpace(finalBranch)
	if jobType == contract.JobTypeAnalysis || jobType == contract.JobTypeDiagnostic {
		return &SuccessfulRunDecision{
			Disposition:     DispositionCompleted,
			ExpectedOutcome: completion.Mode,
			FinalBranch:     branch,
		}, nil
	}
	if completion.Mode != model.CompletionModePullRequest {
		reason := model.ContinuationReasonUnfinishedIssue
		return &SuccessfulRunDecision{
			Disposition:     DispositionContinuation,
			Reason:          &reason,
			ExpectedOutcome: completion.Mode,
			FinalBranch:     branch,
		}, nil
	}
	if branch == "" {
		return decisionForMissingPullRequest(completion, branch), nil
	}
	pr, err := o.lookupPullRequestByHeadBranch(ctx, workspacePath, branch)
	if err != nil {
		return nil, err
	}
	if pr == nil {
		return decisionForMissingPullRequest(completion, branch), nil
	}
	switch pr.State {
	case PullRequestStateOpen:
		return &SuccessfulRunDecision{
			Disposition:     DispositionAwaitingMerge,
			ExpectedOutcome: completion.Mode,
			PR:              clonePullRequestInfo(pr),
			FinalBranch:     branch,
		}, nil
	case PullRequestStateMerged:
		if jobType == contract.JobTypeLandChange {
			return &SuccessfulRunDecision{
				Disposition:     DispositionCompleted,
				ExpectedOutcome: completion.Mode,
				PR:              clonePullRequestInfo(pr),
				FinalBranch:     branch,
			}, nil
		}
		if o.isTerminalState(issueState, o.currentConfig()) {
			return &SuccessfulRunDecision{
				Disposition:     DispositionCompleted,
				ExpectedOutcome: completion.Mode,
				PR:              clonePullRequestInfo(pr),
				FinalBranch:     branch,
			}, nil
		}
		reason := model.ContinuationReasonMergedPRNotTerminal
		return &SuccessfulRunDecision{
			Disposition:     DispositionAwaitingIntervention,
			Reason:          &reason,
			ExpectedOutcome: completion.Mode,
			PR:              clonePullRequestInfo(pr),
			FinalBranch:     branch,
		}, nil
	case PullRequestStateClosed:
		reason := model.ContinuationReasonClosedUnmergedPR
		if completion.OnClosedPR == model.CompletionActionContinue {
			return &SuccessfulRunDecision{
				Disposition:     DispositionContinuation,
				Reason:          &reason,
				ExpectedOutcome: completion.Mode,
				PR:              clonePullRequestInfo(pr),
				FinalBranch:     branch,
			}, nil
		}
		return &SuccessfulRunDecision{
			Disposition:     DispositionAwaitingIntervention,
			Reason:          &reason,
			ExpectedOutcome: completion.Mode,
			PR:              clonePullRequestInfo(pr),
			FinalBranch:     branch,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported pull request state %q", pr.State)
	}
}

func decisionForMissingPullRequest(contract model.CompletionContract, branch string) *SuccessfulRunDecision {
	reason := model.ContinuationReasonMissingPR
	if contract.OnMissingPR == model.CompletionActionContinue {
		return &SuccessfulRunDecision{
			Disposition:     DispositionContinuation,
			Reason:          &reason,
			ExpectedOutcome: contract.Mode,
			FinalBranch:     branch,
		}
	}
	return &SuccessfulRunDecision{
		Disposition:     DispositionAwaitingIntervention,
		Reason:          &reason,
		ExpectedOutcome: contract.Mode,
		FinalBranch:     branch,
	}
}

func (o *Orchestrator) handleReviewWorkerExit(result WorkerResult, entry *model.JobRuntime, identifier string, workspacePath string, dispatch *model.DispatchContext, issueState string) {
	updateRunReviewUsage(entry, int(o.now().UTC().Sub(result.StartedAt).Milliseconds()))
	updateRunPhase(entry, contract.RunPhaseSummaryVerifying)
	expectedOutcome := normalizeCompletionContract(o.currentWorkflow().Completion).Mode
	if dispatch != nil && dispatch.ExpectedOutcome != "" {
		expectedOutcome = dispatch.ExpectedOutcome
	}
	if result.Err != nil {
		o.mu.Unlock()
		o.moveToAwaitingIntervention(result.IssueID, identifier, workspacePath, recordBranch(entry), entry.RetryAttempt, entry.StallCount, expectedOutcome, "reviewer_unavailable", issueState, recordPullRequest(entry))
		return
	}

	switch result.ReviewStatus {
	case contract.ReviewGateStatusPassed:
		markRunReviewPassed(entry, result.ReviewSummary)
		if entry.Dispatch != nil {
			entry.Dispatch.ReviewFeedback = nil
		}
		entry.NeedsRecovery = false
		o.setRecordObservationLocked(entry, &contract.Observation{
			Running: false,
			Summary: "review passed; finalizing candidate delivery",
		})
		snapshot := cloneJobRuntime(entry)
		o.commitStateLocked(true)
		o.mu.Unlock()
		_ = o.resumeRecoveredSuccessPath(o.runtimeContext(), result.IssueID, snapshot, issueState)
		return
	case contract.ReviewGateStatusChangesRequested:
		if entry.Run != nil && entry.Run.ReviewGate != nil && entry.Run.ReviewGate.FixRoundsUsed >= entry.Run.ReviewGate.MaxFixRounds {
			if entry.Run.ReviewGate != nil {
				entry.Run.ReviewGate.Status = contract.ReviewGateStatusInterventionRequired
			}
			o.mu.Unlock()
			o.moveToAwaitingIntervention(result.IssueID, identifier, workspacePath, recordBranch(entry), entry.RetryAttempt, entry.StallCount, expectedOutcome, string(contract.ReasonRunReviewGateFixLimitReached), issueState, recordPullRequest(entry))
			return
		}
		markRunReviewChangesRequested(entry, result.ReviewSummary, result.ReviewFindings)
		if entry.Run != nil && entry.Run.ReviewGate != nil {
			entry.Run.ReviewGate.FixRoundsUsed++
		}
		nextDispatch := model.CloneDispatchContext(dispatch)
		if nextDispatch == nil {
			nextDispatch = freshDispatchContext(normalizeCompletionContract(o.currentWorkflow().Completion))
		}
		nextDispatch.ReviewFeedback = &model.ReviewFeedbackContext{
			Summary:  result.ReviewSummary,
			Findings: append([]model.ReviewFinding(nil), result.ReviewFindings...),
		}
		entry.Dispatch = model.CloneDispatchContext(nextDispatch)
		issue := model.CloneIssue(entry.LastKnownIssue)
		if issue == nil {
			issue = &model.Issue{
				ID:         result.IssueID,
				Identifier: identifier,
				Title:      identifier,
				State:      issueState,
			}
		}
		attempt := entry.RetryAttempt
		o.mu.Unlock()
		o.dispatchIssue(o.runtimeContext(), *issue, &attempt)
		return
	default:
		o.mu.Unlock()
		o.moveToAwaitingIntervention(result.IssueID, identifier, workspacePath, recordBranch(entry), entry.RetryAttempt, entry.StallCount, expectedOutcome, "reviewer_unavailable", issueState, recordPullRequest(entry))
		return
	}
}

func (o *Orchestrator) handleWorkerExit(result WorkerResult) {
	o.mu.Lock()
	entry := o.state.Jobs[result.IssueID]
	if !isRecordRunning(entry) {
		identifier, pending := o.pendingCleanup[result.IssueID]
		if pending {
			delete(o.pendingCleanup, result.IssueID)
			o.mu.Unlock()
			ctx := o.runtimeContext()
			if err := o.workspace.CleanupWorkspace(ctx, identifier); err != nil {
				o.logger.Warn("deferred workspace cleanup failed", "issue_id", result.IssueID, "issue_identifier", identifier, "error", err.Error())
			}
			return
		}
		o.mu.Unlock()
		return
	}
	identifier := recordIdentifier(entry)
	workspacePath := recordWorkspacePath(entry)
	retryAttempt := entry.RetryAttempt
	stallCount := entry.StallCount
	dispatch := model.CloneDispatchContext(entry.Dispatch)
	issueState := entry.LastKnownIssueState
	o.logger.Info(
		"worker finished",
		"issue_id", result.IssueID,
		"issue_identifier", identifier,
		"attempt", attemptCountFromRetry(retryAttempt),
		"run_phase", result.Phase.String(),
		"success", result.Err == nil,
		"error", errorString(result.Err),
	)

	if o.isProtectedLocked() {
		entry.WorkerCancel = nil
		entry.StartedAt = nil
		o.setRecordObservationLocked(entry, &contract.Observation{
			Running: false,
			Summary: "service unavailable; run result captured for operator review",
		})
		o.reindexRecordLocked(result.IssueID, entry)
		o.rememberProtectedResultLocked(result.IssueID, entry, result)
		o.publishViewLocked()
		o.mu.Unlock()
		return
	}

	o.addRuntimeLocked(entry)
	entry.WorkerCancel = nil
	entry.StartedAt = nil
	if result.Kind == WorkerKindReview {
		o.handleReviewWorkerExit(result, entry, identifier, workspacePath, dispatch, issueState)
		return
	}
	updateRunUsage(entry, int(o.now().UTC().Sub(result.StartedAt).Milliseconds()))
	updateRunPhase(entry, runPhaseSummaryForModelPhase(result.Phase))

	if result.Err != nil {
		if hardViolation, ok := model.AsHardViolation(result.Err); ok {
			expectedOutcome := normalizeCompletionContract(o.currentWorkflow().Completion).Mode
			if dispatch != nil && dispatch.ExpectedOutcome != "" {
				expectedOutcome = dispatch.ExpectedOutcome
			}
			o.mu.Unlock()
			o.moveToAwaitingIntervention(result.IssueID, identifier, workspacePath, recordBranch(entry), retryAttempt, stallCount, expectedOutcome, string(contract.ReasonRunHardViolationDetected), issueState, recordPullRequest(entry))
			o.logger.Warn("hard violation detected", "issue_id", result.IssueID, "issue_identifier", identifier, "violation_code", hardViolation.Code, "tool", hardViolation.Tool)
			return
		}
		if errors.Is(result.Err, model.ErrTurnTimeout) {
			captureRecord := cloneJobRuntime(entry)
			captureReason := contract.MustReason(contract.ReasonRunContinuationPending, map[string]any{
				"record_id": entry.RecordID,
				"cause":     model.ContinuationReasonExecutionBudgetExhausted,
			})
			o.setRecordObservationLocked(entry, &contract.Observation{
				Running: false,
				Summary: "capturing recovery checkpoint",
			})
			o.reindexRecordLocked(result.IssueID, entry)
			o.mu.Unlock()

			checkpoint, checkpointErr := o.captureCheckpointFn(o.runtimeContext(), captureRecord, contract.RunPhaseSummaryExecuting, &captureReason)
			if checkpointErr != nil {
				o.moveToAwaitingIntervention(result.IssueID, identifier, workspacePath, recordBranch(captureRecord), retryAttempt, stallCount, model.CompletionModePullRequest, "recovery_uncertain", issueState, recordPullRequest(captureRecord))
				return
			}

			continuationDispatch := continuationDispatchContext(dispatch, normalizeCompletionContract(o.currentWorkflow().Completion), model.ContinuationReasonExecutionBudgetExhausted, checkpoint.Checkpoint.Branch, recordPullRequest(captureRecord), issueState)
			continuationDispatch.RecoveryCheckpoint = model.CloneRecoveryCheckpoint(checkpoint)

			o.mu.Lock()
			current := o.state.Jobs[result.IssueID]
			if current == nil {
				o.mu.Unlock()
				return
			}
			nextAttempt := retryAttempt + 1
			if nextAttempt <= 0 {
				nextAttempt = 1
			}
			current.NeedsRecovery = false
			o.scheduleRetryLocked(result.IssueID, identifier, nextAttempt, optionalError(result.Err.Error()), true, stallCount, continuationDispatch)
			o.commitStateLocked(true)
			o.mu.Unlock()
			return
		}
		nextAttempt := retryAttempt + 1
		if nextAttempt <= 0 {
			nextAttempt = 1
		}
		errorText := result.Err.Error()
		o.scheduleRetryLocked(result.IssueID, identifier, nextAttempt, &errorText, false, stallCount, dispatch)
		version := o.commitStateLocked(true)
		event := o.newIssueFailedEvent(result.IssueID, identifier, workspacePath, result.Phase, retryAttempt, result.Err, dispatch)
		if version > 0 {
			o.schedulePersistedActionLocked(version, func() {
				o.emitNotification(event)
			})
		}
		o.mu.Unlock()
		if version == 0 {
			o.emitNotification(event)
		}
		return
	}

	if workspacePath != "" {
		entry.DurableRefs.Workspace = &contract.WorkspaceRef{Path: workspacePath}
	}
	finalBranch := strings.TrimSpace(result.FinalBranch)
	if finalBranch != "" {
		entry.DurableRefs.Branch = &contract.BranchRef{Name: finalBranch}
	}
	entry.NeedsRecovery = true
	entry.Dispatch = model.CloneDispatchContext(dispatch)
	entry.LastKnownIssueState = issueState
	o.setRecordStatusLocked(entry, model.JobLifecycleActive, nil, &contract.Observation{
		Running: false,
		Summary: "awaiting conservative recovery evaluation",
		Details: map[string]any{
			"final_branch": finalBranch,
			"issue_state":  issueState,
		},
	})
	o.reindexRecordLocked(result.IssueID, entry)
	version := o.commitStateLocked(true)
	if version > 0 {
		o.pendingRecovery[result.IssueID] = version
	}
	o.mu.Unlock()

	if version == 0 {
		ctx := o.runtimeContext()
		o.reconcileActiveRecovery(ctx)
	}
}

func (o *Orchestrator) handleCodexUpdate(update CodexUpdate) {
	o.mu.Lock()
	defer o.mu.Unlock()

	entry := o.state.Jobs[update.IssueID]
	if !isRecordRunning(entry) {
		return
	}
	event := update.Event
	protected := o.isProtectedLocked()
	entry.Session.LastCodexMessage = event.Message
	lastEvent := event.Event
	entry.Session.LastCodexEvent = &lastEvent
	timestamp := event.Timestamp.UTC()
	entry.Session.LastCodexTimestamp = &timestamp
	if event.CodexAppServerPID != nil {
		entry.Session.CodexAppServerPID = event.CodexAppServerPID
	}
	if event.SessionID != nil {
		entry.Session.SessionID = *event.SessionID
	}
	if event.ThreadID != nil {
		entry.Session.ThreadID = *event.ThreadID
	}
	if event.TurnID != nil && entry.Session.TurnID != *event.TurnID {
		entry.Session.TurnID = *event.TurnID
		entry.Session.TurnCount++
	}
	if event.Usage != nil {
		if protected {
			entry.Session.CodexInputTokens = event.Usage.InputTokens
			entry.Session.CodexOutputTokens = event.Usage.OutputTokens
			entry.Session.CodexTotalTokens = event.Usage.TotalTokens
			entry.Session.LastReportedInputTokens = event.Usage.InputTokens
			entry.Session.LastReportedOutputTokens = event.Usage.OutputTokens
			entry.Session.LastReportedTotalTokens = event.Usage.TotalTokens
		} else {
			o.applyUsageLocked(&entry.Session, event.Usage)
		}
	}
	if event.RateLimits != nil && !protected {
		o.state.CodexRateLimits = event.RateLimits
	}
	observation := &contract.Observation{
		Running: true,
		Summary: "agent run in progress",
		Details: map[string]any{
			"session_id": entry.Session.SessionID,
			"turn_count": entry.Session.TurnCount,
		},
	}
	updateRunPhase(entry, contract.RunPhaseSummaryExecuting)
	o.setRecordObservationLocked(entry, observation)
	o.reindexRecordLocked(update.IssueID, entry)
	o.commitStateLocked(false)
}

func (o *Orchestrator) reconcileRunning(ctx context.Context) (bool, bool) {
	o.mu.RLock()
	protected := o.isProtectedLocked()
	o.mu.RUnlock()
	if protected {
		return false, false
	}

	cfg := o.currentConfig()

	o.mu.Lock()
	stalledIDs := make([]string, 0)
	for issueID, entry := range o.state.Jobs {
		if !isRecordRunning(entry) {
			continue
		}
		stallTimeout := cfg.CodexStallTimeoutMS
		if stallTimeout <= 0 {
			continue
		}
		lastSeen := o.now().UTC()
		if entry.StartedAt != nil {
			lastSeen = *entry.StartedAt
		}
		if entry.Session.LastCodexTimestamp != nil {
			lastSeen = *entry.Session.LastCodexTimestamp
		}
		if o.now().Sub(lastSeen) > time.Duration(stallTimeout)*time.Millisecond {
			stalledIDs = append(stalledIDs, issueID)
		}
	}
	ids := make([]string, 0, len(o.state.Jobs))
	for issueID, entry := range o.state.Jobs {
		if isRecordRunning(entry) {
			ids = append(ids, issueID)
		}
	}
	o.mu.Unlock()

	for _, issueID := range stalledIDs {
		o.handleStalledRunningRecord(ctx, issueID)
	}

	if len(ids) == 0 {
		return false, false
	}

	refreshed, err := o.tracker.FetchIssueStatesByIDs(ctx, ids)
	if err != nil {
		o.logger.Warn("reconcile state refresh failed", "error", err.Error())
		o.mu.Lock()
		if o.setHealthAlertAndNotifyLocked(AlertSnapshot{
			Code:    "tracker_unreachable",
			Level:   "warn",
			Message: err.Error(),
		}) {
			o.publishViewLocked()
		}
		o.mu.Unlock()
		return true, false
	}

	byID := make(map[string]model.Issue, len(refreshed))
	for _, issue := range refreshed {
		byID[issue.ID] = issue
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	if o.clearHealthAlertAndNotifyLocked("tracker_unreachable") {
		o.publishViewLocked()
	}
	for issueID, entry := range o.state.Jobs {
		if !isRecordRunning(entry) {
			continue
		}
		issue, ok := byID[issueID]
		if !ok {
			continue
		}
		if o.isTerminalState(issue.State, cfg) {
			o.terminateRunningLocked(ctx, issueID, true, false, "")
			continue
		}
		if o.isActiveState(issue.State, cfg) {
			entry.LastKnownIssue = model.CloneIssue(&issue)
			entry.LastKnownIssueState = strings.TrimSpace(issue.State)
			sourceKind := o.currentSourceKind()
			sourceName := o.currentSourceName()
			entry.SourceRef = sourceRefForIssue(sourceKind, sourceName, &issue)
			entry.RecordID = recordIDForIssue(sourceKind, sourceName, &issue)
			continue
		}
		o.terminateRunningLocked(ctx, issueID, false, false, "")
	}
	o.commitStateLocked(true)
	return true, true
}

func (o *Orchestrator) handleStalledRunningRecord(ctx context.Context, issueID string) {
	o.mu.Lock()
	if o.isProtectedLocked() {
		o.mu.Unlock()
		return
	}
	entry := o.runningRecords[issueID]
	if entry == nil {
		o.mu.Unlock()
		return
	}
	identifier := recordIdentifier(entry)
	workspacePath := recordWorkspacePath(entry)
	retryAttempt := entry.RetryAttempt
	stallCount := entry.StallCount + 1
	dispatch := model.CloneDispatchContext(entry.Dispatch)
	issueState := entry.LastKnownIssueState
	if entry.WorkerCancel != nil {
		entry.WorkerCancel()
	}
	entry.WorkerCancel = nil
	entry.StartedAt = nil
	updateRunPhase(entry, contract.RunPhaseSummaryBlocked)
	o.setRecordObservationLocked(entry, &contract.Observation{
		Running: false,
		Summary: "capturing recovery checkpoint after stall",
	})
	o.reindexRecordLocked(issueID, entry)
	snapshot := cloneJobRuntime(entry)
	o.mu.Unlock()

	checkpointReason := contract.MustReason(contract.ReasonRunContinuationPending, map[string]any{
		"record_id": snapshot.RecordID,
		"cause":     "stalled_session",
	})
	checkpoint, checkpointErr := o.captureCheckpointFn(ctx, snapshot, contract.RunPhaseSummaryBlocked, &checkpointReason)
	if checkpointErr != nil {
		expectedOutcome := normalizeCompletionContract(o.currentWorkflow().Completion).Mode
		if dispatch != nil && dispatch.ExpectedOutcome != "" {
			expectedOutcome = dispatch.ExpectedOutcome
		}
		o.moveToAwaitingIntervention(issueID, identifier, workspacePath, recordBranch(snapshot), retryAttempt, stallCount, expectedOutcome, "recovery_uncertain", issueState, recordPullRequest(snapshot))
		return
	}

	continuationDispatch := continuationDispatchContext(dispatch, normalizeCompletionContract(o.currentWorkflow().Completion), model.ContinuationReasonRecoverableRuntimeError, checkpoint.Checkpoint.Branch, recordPullRequest(snapshot), issueState)
	continuationDispatch.RecoveryCheckpoint = model.CloneRecoveryCheckpoint(checkpoint)

	o.mu.Lock()
	current := o.state.Jobs[issueID]
	if current != nil {
		nextAttempt := retryAttempt + 1
		if nextAttempt <= 0 {
			nextAttempt = 1
		}
		current.NeedsRecovery = false
		o.scheduleRetryLocked(issueID, identifier, nextAttempt, optionalError("stalled session"), true, stallCount, continuationDispatch)
		o.commitStateLocked(true)
	}
	o.mu.Unlock()
}

func (o *Orchestrator) reconcileAwaitingMerge(ctx context.Context) {
	o.mu.RLock()
	protected := o.isProtectedLocked()
	o.mu.RUnlock()
	if protected {
		return
	}

	o.mu.RLock()
	pending := make(map[string]*model.JobRuntime, len(o.awaitingMergeRecords))
	for issueID, entry := range o.awaitingMergeRecords {
		if entry == nil {
			continue
		}
		pending[issueID] = cloneJobRuntime(entry)
	}
	o.mu.RUnlock()
	if len(pending) == 0 {
		return
	}

	cfg := o.currentConfig()
	ids := make([]string, 0, len(pending))
	for issueID := range pending {
		ids = append(ids, issueID)
	}
	refreshed, err := o.tracker.FetchIssueStatesByIDs(ctx, ids)
	byID := make(map[string]model.Issue, len(refreshed))
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		o.logger.Warn("awaiting-merge state refresh failed", "error", err.Error())
	} else {
		for _, issue := range refreshed {
			byID[issue.ID] = issue
		}
	}

	for issueID, entry := range pending {
		if issue, ok := byID[issueID]; ok {
			switch {
			case o.isTerminalState(issue.State, cfg):
				o.completeSuccessfulIssue(ctx, issueID, recordIdentifier(entry))
				continue
			case !o.isActiveState(issue.State, cfg):
				o.completeAbandonedIssue(ctx, issueID, recordIdentifier(entry), "source issue left active states while awaiting merge")
				continue
			default:
				o.mu.Lock()
				current := o.awaitingMergeRecords[issueID]
				if current != nil && current.LastKnownIssueState != issue.State {
					current.LastKnownIssue = model.CloneIssue(&issue)
					current.LastKnownIssueState = issue.State
					sourceKind := o.currentSourceKind()
					sourceName := o.currentSourceName()
					current.SourceRef = sourceRefForIssue(sourceKind, sourceName, &issue)
					current.RecordID = recordIDForIssue(sourceKind, sourceName, &issue)
					o.commitStateLocked(true)
				}
				o.mu.Unlock()
			}
		}

		pr, err := o.lookupAwaitingMergePullRequest(ctx, recordWorkspacePath(entry), entry)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			o.logger.Warn("awaiting-merge PR lookup failed", "issue_id", issueID, "issue_identifier", recordIdentifier(entry), "branch", recordBranch(entry), "error", err.Error())
			errorText := err.Error()
			o.mu.Lock()
			current := o.awaitingMergeRecords[issueID]
			if current != nil {
				o.setRecordReasonLocked(current, &contract.Reason{
					ReasonCode: contract.ReasonRecordBlockedAwaitingMerge,
					Category:   contract.CategoryRecord,
					Details: map[string]any{
						"record_id":  current.RecordID,
						"last_error": errorText,
					},
				})
				o.commitStateLocked(true)
			}
			o.mu.Unlock()
			continue
		}
		if pr == nil {
			o.logger.Warn("awaiting-merge PR lookup returned no match", "issue_id", issueID, "issue_identifier", recordIdentifier(entry), "branch", recordBranch(entry))
			if entry.DurableRefs.PullRequest != nil {
				o.mu.Lock()
				current := o.awaitingMergeRecords[issueID]
				if current != nil {
					o.setRecordReasonLocked(current, &contract.Reason{
						ReasonCode: contract.ReasonRecordBlockedAwaitingMerge,
						Category:   contract.CategoryRecord,
						Details: map[string]any{
							"record_id":     current.RecordID,
							"last_error":    "pull request refresh returned no match",
							"pr_number":     current.DurableRefs.PullRequest.Number,
							"pr_state":      current.DurableRefs.PullRequest.State,
							"pr_base_owner": recordReasonDetailString(current, "pr_base_owner"),
							"pr_base_repo":  recordReasonDetailString(current, "pr_base_repo"),
							"pr_head_owner": recordReasonDetailString(current, "pr_head_owner"),
						},
					})
					o.commitStateLocked(true)
				}
				o.mu.Unlock()
				continue
			}
			o.moveToAwaitingIntervention(issueID, recordIdentifier(entry), recordWorkspacePath(entry), recordBranch(entry), entry.RetryAttempt, entry.StallCount, model.CompletionModePullRequest, string(model.ContinuationReasonMissingPR), entry.LastKnownIssueState, nil)
			continue
		}

		switch pr.State {
		case PullRequestStateOpen:
			o.mu.Lock()
			current := o.awaitingMergeRecords[issueID]
			if current != nil {
				current.DurableRefs.PullRequest = &contract.PullRequestRef{
					Number: pr.Number,
					URL:    pr.URL,
					State:  string(pr.State),
				}
				current.RetryDueAt = nil
				o.setRecordReasonLocked(current, &contract.Reason{
					ReasonCode: contract.ReasonRecordBlockedAwaitingMerge,
					Category:   contract.CategoryRecord,
					Details: map[string]any{
						"record_id":     current.RecordID,
						"pr_number":     pr.Number,
						"pr_state":      pr.State,
						"pr_base_owner": pr.BaseOwner,
						"pr_base_repo":  pr.BaseRepo,
						"pr_head_owner": pr.HeadOwner,
					},
				})
				o.commitStateLocked(true)
			}
			o.mu.Unlock()
		case PullRequestStateMerged:
			issue, ok := byID[issueID]
			if !ok {
				continue
			}
			if jobTypeForDispatch(entry.Dispatch) == contract.JobTypeLandChange {
				o.mu.Lock()
				current := o.awaitingMergeRecords[issueID]
				if current != nil {
					current.DurableRefs.PullRequest = &contract.PullRequestRef{
						Number: pr.Number,
						URL:    pr.URL,
						State:  string(pr.State),
					}
					current.LastKnownIssueState = issue.State
				}
				o.mu.Unlock()
				o.completeSuccessfulIssue(ctx, issueID, recordIdentifier(entry))
				continue
			}
			o.moveToAwaitingIntervention(issueID, recordIdentifier(entry), recordWorkspacePath(entry), recordBranch(entry), entry.RetryAttempt, entry.StallCount, model.CompletionModePullRequest, string(model.ContinuationReasonMergedPRNotTerminal), issue.State, pr)
		case PullRequestStateClosed:
			o.moveToAwaitingIntervention(issueID, recordIdentifier(entry), recordWorkspacePath(entry), recordBranch(entry), entry.RetryAttempt, entry.StallCount, model.CompletionModePullRequest, string(model.ContinuationReasonClosedUnmergedPR), entry.LastKnownIssueState, pr)
		default:
			errorText := fmt.Sprintf("unsupported pull request state %q", pr.State)
			o.logger.Warn("awaiting-merge PR state is unsupported", "issue_id", issueID, "issue_identifier", recordIdentifier(entry), "branch", recordBranch(entry), "state", pr.State)
			o.mu.Lock()
			current := o.awaitingMergeRecords[issueID]
			if current != nil {
				current.DurableRefs.PullRequest = &contract.PullRequestRef{
					Number: pr.Number,
					URL:    pr.URL,
					State:  string(pr.State),
				}
				o.setRecordReasonLocked(current, &contract.Reason{
					ReasonCode: contract.ReasonRecordBlockedAwaitingMerge,
					Category:   contract.CategoryRecord,
					Details: map[string]any{
						"record_id":     current.RecordID,
						"last_error":    errorText,
						"pr_state":      pr.State,
						"pr_base_owner": pr.BaseOwner,
						"pr_base_repo":  pr.BaseRepo,
						"pr_head_owner": pr.HeadOwner,
					},
				})
				o.commitStateLocked(true)
			}
			o.mu.Unlock()
		}
	}
}

func (o *Orchestrator) reconcileAwaitingIntervention(ctx context.Context) {
	o.mu.RLock()
	protected := o.isProtectedLocked()
	o.mu.RUnlock()
	if protected {
		return
	}

	o.mu.RLock()
	pending := make(map[string]*model.JobRuntime, len(o.awaitingInterventionRecords))
	for issueID, entry := range o.awaitingInterventionRecords {
		if entry == nil {
			continue
		}
		pending[issueID] = cloneJobRuntime(entry)
	}
	o.mu.RUnlock()
	if len(pending) == 0 {
		return
	}

	cfg := o.currentConfig()
	ids := make([]string, 0, len(pending))
	for issueID := range pending {
		ids = append(ids, issueID)
	}
	refreshed, err := o.tracker.FetchIssueStatesByIDs(ctx, ids)
	if err != nil {
		o.logger.Warn("awaiting-intervention state refresh failed", "error", err.Error())
		return
	}

	byID := make(map[string]model.Issue, len(refreshed))
	for _, issue := range refreshed {
		byID[issue.ID] = issue
	}

	for issueID, entry := range pending {
		issue, ok := byID[issueID]
		if !ok {
			o.mu.Lock()
			current := o.awaitingInterventionRecords[issueID]
			if current != nil {
				o.setRecordReasonLocked(current, &contract.Reason{
					ReasonCode: contract.ReasonRecordBlockedRecoveryUncertain,
					Category:   contract.CategoryRecord,
					Details: map[string]any{
						"record_id": current.RecordID,
						"cause":     string(model.ContinuationReasonTrackerIssueMissing),
					},
				})
				o.commitStateLocked(true)
			}
			o.mu.Unlock()
			continue
		}
		switch {
		case o.isTerminalState(issue.State, cfg):
			o.completeSuccessfulIssue(ctx, issueID, recordIdentifier(entry))
		case !o.isActiveState(issue.State, cfg):
			o.completeAbandonedIssue(ctx, issueID, recordIdentifier(entry), "source issue left active states while awaiting intervention")
		}
	}
}

func (o *Orchestrator) handleRetryTimer(ctx context.Context, issueID string) {
	o.mu.RLock()
	protected := o.isProtectedLocked()
	o.mu.RUnlock()
	if protected {
		return
	}
	o.processRetryIssue(ctx, issueID)
}

func (o *Orchestrator) processDueRetries(ctx context.Context) {
	type dueRetry struct {
		IssueID    string
		Identifier string
		DueAt      time.Time
	}

	now := o.now().UTC()
	o.mu.Lock()
	due := make([]dueRetry, 0, len(o.retryRecords))
	for issueID, entry := range o.retryRecords {
		if entry == nil || entry.RetryDueAt == nil || entry.RetryDueAt.After(now) {
			continue
		}
		if entry.RetryTimer != nil {
			entry.RetryTimer.Stop()
			entry.RetryTimer = nil
		}
		due = append(due, dueRetry{
			IssueID:    issueID,
			Identifier: recordIdentifier(entry),
			DueAt:      *entry.RetryDueAt,
		})
	}
	o.mu.Unlock()

	sort.SliceStable(due, func(i int, j int) bool {
		if !due[i].DueAt.Equal(due[j].DueAt) {
			return due[i].DueAt.Before(due[j].DueAt)
		}
		if due[i].Identifier != due[j].Identifier {
			return due[i].Identifier < due[j].Identifier
		}
		return due[i].IssueID < due[j].IssueID
	})

	for _, item := range due {
		o.processRetryIssue(ctx, item.IssueID)
	}
}

func (o *Orchestrator) processRetryIssue(ctx context.Context, issueID string) {
	o.mu.RLock()
	protected := o.isProtectedLocked()
	o.mu.RUnlock()
	if protected {
		return
	}
	now := o.now().UTC()

	o.mu.Lock()
	if o.isProtectedLocked() {
		o.mu.Unlock()
		return
	}
	retryEntry := o.retryRecords[issueID]
	if retryEntry == nil || retryEntry.RetryDueAt == nil || retryEntry.RetryDueAt.After(now) {
		o.mu.Unlock()
		return
	}
	if retryEntry.RetryTimer != nil {
		retryEntry.RetryTimer.Stop()
		retryEntry.RetryTimer = nil
	}
	snapshot := cloneJobRuntime(retryEntry)
	o.mu.Unlock()

	candidates, err := o.tracker.FetchCandidateIssues(ctx)
	if err != nil {
		errorText := "retry poll failed"
		o.mu.Lock()
		if o.isProtectedLocked() {
			o.mu.Unlock()
			return
		}
		current := o.retryRecords[issueID]
		if current != nil && current.RetryDueAt != nil && !current.RetryDueAt.After(o.now().UTC()) {
			nextAttempt := current.RetryAttempt + 1
			if isContinuationPending(current) {
				nextAttempt = current.RetryAttempt
			}
			o.scheduleRetryLocked(issueID, recordIdentifier(current), nextAttempt, &errorText, isContinuationPending(current), current.StallCount, current.Dispatch)
			o.commitStateLocked(true)
		}
		o.mu.Unlock()
		return
	}

	var issue *model.Issue
	for _, candidate := range candidates {
		if candidate.ID == issueID {
			copied := candidate
			issue = &copied
			break
		}
	}
	if issue == nil {
		o.mu.Lock()
		if o.isProtectedLocked() {
			o.mu.Unlock()
			return
		}
		current := o.retryRecords[issueID]
		o.mu.Unlock()
		if current != nil {
			o.completeAbandonedIssue(ctx, issueID, recordIdentifier(current), "record disappeared from candidate issue set")
		}
		return
	}

	cfg := o.currentConfig()
	if !o.isDispatchEligible(*issue, cfg, true) {
		o.mu.Lock()
		if o.isProtectedLocked() {
			o.mu.Unlock()
			return
		}
		current := o.retryRecords[issueID]
		o.mu.Unlock()
		if current != nil {
			o.completeAbandonedIssue(ctx, issueID, recordIdentifier(current), "record is no longer dispatch eligible")
		}
		return
	}
	if !o.hasAvailableSlots(*issue, cfg) {
		errorText := "no available orchestrator slots"
		o.mu.Lock()
		if o.isProtectedLocked() {
			o.mu.Unlock()
			return
		}
		current := o.retryRecords[issueID]
		if current != nil && current.RetryDueAt != nil && !current.RetryDueAt.After(o.now().UTC()) {
			nextAttempt := current.RetryAttempt + 1
			if isContinuationPending(current) {
				nextAttempt = current.RetryAttempt
			}
			o.scheduleRetryLocked(issueID, issue.Identifier, nextAttempt, &errorText, isContinuationPending(current), current.StallCount, current.Dispatch)
			o.commitStateLocked(true)
		}
		o.mu.Unlock()
		return
	}

	attempt := snapshot.RetryAttempt
	o.dispatchIssue(ctx, *issue, &attempt)
}

func (o *Orchestrator) startupCleanup(ctx context.Context) {
	cfg := o.currentConfig()
	issues, err := o.tracker.FetchIssuesByStates(ctx, cfg.TerminalStates)
	if err != nil {
		o.logger.Warn("startup cleanup fetch failed", "error", err.Error())
		o.mu.Lock()
		if o.setHealthAlertAndNotifyLocked(AlertSnapshot{
			Code:    "tracker_terminal_fetch_failed",
			Level:   "warn",
			Message: err.Error(),
		}) {
			o.publishViewLocked()
		}
		o.mu.Unlock()
		return
	}
	o.mu.Lock()
	if o.clearHealthAlertAndNotifyLocked("tracker_terminal_fetch_failed") {
		o.publishViewLocked()
	}
	o.mu.Unlock()
	for _, issue := range issues {
		if err := o.workspace.CleanupWorkspace(ctx, issue.Identifier); err != nil {
			o.logger.Warn("cleanup workspace failed", "issue_identifier", issue.Identifier, "error", err.Error())
		}
	}
}

func (o *Orchestrator) gracefulShutdown() {
	o.mu.Lock()
	if o.tickTimer != nil {
		o.tickTimer.Stop()
	}
	for _, retryEntry := range o.retryRecords {
		if retryEntry.RetryTimer != nil {
			retryEntry.RetryTimer.Stop()
		}
	}
	for _, entry := range o.runningRecords {
		if entry.WorkerCancel != nil {
			entry.WorkerCancel()
		}
	}
	o.mu.Unlock()

	done := make(chan struct{})
	go func() {
		o.workerWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
	o.closeStateStore(o.stateStore)
	o.closeNotifier(o.notifier)
}

func (o *Orchestrator) terminateRunningLocked(ctx context.Context, issueID string, cleanup bool, scheduleRetry bool, errText string) {
	entry := o.runningRecords[issueID]
	if entry == nil {
		return
	}
	o.addRuntimeLocked(entry)
	if entry.WorkerCancel != nil {
		entry.WorkerCancel()
	}
	entry.WorkerCancel = nil
	entry.StartedAt = nil

	if cleanup {
		o.pendingCleanup[issueID] = recordIdentifier(entry)
	}

	if scheduleRetry {
		nextAttempt := entry.RetryAttempt + 1
		if nextAttempt <= 0 {
			nextAttempt = 1
		}
		stallCount := entry.StallCount
		if isStallErrorText(errText) {
			stallCount++
		}
		errorPtr := optionalError(errText)
		o.scheduleRetryLocked(issueID, recordIdentifier(entry), nextAttempt, errorPtr, false, stallCount, entry.Dispatch)
		return
	}
	o.setRecordStatusLocked(entry, model.JobLifecycleActive, nil, &contract.Observation{
		Running: false,
		Summary: "run terminated by orchestrator",
	})
	o.reindexRecordLocked(issueID, entry)
}

func (o *Orchestrator) scheduleRetryLocked(issueID string, identifier string, attempt int, errText *string, continuation bool, stallCount int, dispatch *model.DispatchContext) {
	record := o.state.Jobs[issueID]
	if record == nil {
		record = &model.JobRuntime{
			Lifecycle:   model.JobLifecycleActive,
			DurableRefs: contract.DurableRefs{LedgerPath: ledgerPathForConfig(o.currentConfig())},
		}
		o.ensureRecordIdentityLocked(record, issueID, identifier)
		o.state.Jobs[issueID] = record
	}
	if existing := o.retryRecords[issueID]; existing != nil && existing.RetryTimer != nil {
		existing.RetryTimer.Stop()
	}

	delay := time.Second
	if !continuation {
		maxBackoff := maxInt(o.currentConfig().MaxRetryBackoffMS, 10000)
		base := math.Min(float64(10000)*math.Pow(2, float64(maxInt(attempt, 1)-1)), float64(maxBackoff))
		delay = time.Duration(base*(0.5+o.randFloat()*0.5)) * time.Millisecond
	}
	dueAt := o.now().UTC().Add(delay)
	timer := time.AfterFunc(delay, func() {
		select {
		case o.retryFireCh <- issueID:
		default:
		}
	})

	retryDispatch := model.CloneDispatchContext(dispatch)
	if retryDispatch != nil {
		retryAttempt := attempt
		retryDispatch.RetryAttempt = &retryAttempt
	}
	if record.DurableRefs.Workspace == nil {
		record.DurableRefs.Workspace = &contract.WorkspaceRef{Path: workspacePathForIdentifier(o.currentConfig().WorkspaceRoot, identifier)}
	}
	record.RetryAttempt = attempt
	record.StallCount = stallCount
	record.RetryDueAt = &dueAt
	record.RetryTimer = timer
	record.Dispatch = retryDispatch
	o.ensureRecordIdentityLocked(record, issueID, identifier)
	reasonCode := contract.ReasonRecordBlockedRetryScheduled
	reasonCategory := contract.CategoryRecord
	reasonDetails := map[string]any{
		"record_id": record.RecordID,
		"attempt":   attemptCountFromRetry(attempt),
	}
	observationSummary := "retry scheduled"
	if continuation {
		reasonCode = contract.ReasonRunContinuationPending
		reasonCategory = contract.CategoryRun
		observationSummary = "continuation pending"
		if retryDispatch != nil && retryDispatch.RecoveryCheckpoint != nil {
			if retryDispatch.RecoveryCheckpoint.ArtifactID != "" {
				reasonDetails["checkpoint_id"] = retryDispatch.RecoveryCheckpoint.ArtifactID
			}
			if retryDispatch.RecoveryCheckpoint.PatchPath != "" {
				reasonDetails["patch_path"] = retryDispatch.RecoveryCheckpoint.PatchPath
			}
			setRunContinuationPending(record, retryDispatch.RecoveryCheckpoint)
		}
	}
	o.setRecordStatusLocked(record, model.JobLifecycleRetryScheduled, &contract.Reason{
		ReasonCode: reasonCode,
		Category:   reasonCategory,
		Details:    reasonDetails,
	}, &contract.Observation{
		Running: false,
		Summary: observationSummary,
	})
	if errText != nil {
		record.Reason.Details["last_error"] = *errText
	}
	record.SourceRef.SourceIdentifier = strings.TrimSpace(identifier)
	record.SourceRef.SourceID = strings.TrimSpace(issueID)
	record.DurableRefs.LedgerPath = ledgerPathForConfig(o.currentConfig())
	o.reindexRecordLocked(issueID, record)
}

func (o *Orchestrator) applyUsageLocked(session *model.LiveSession, usage *agent.TokenUsage) {
	deltaInput := usage.InputTokens - session.LastReportedInputTokens
	deltaOutput := usage.OutputTokens - session.LastReportedOutputTokens
	deltaTotal := usage.TotalTokens - session.LastReportedTotalTokens

	if deltaInput > 0 {
		o.state.CodexTotals.InputTokens += deltaInput
	}
	if deltaOutput > 0 {
		o.state.CodexTotals.OutputTokens += deltaOutput
	}
	if deltaTotal > 0 {
		o.state.CodexTotals.TotalTokens += deltaTotal
	}

	session.CodexInputTokens = usage.InputTokens
	session.CodexOutputTokens = usage.OutputTokens
	session.CodexTotalTokens = usage.TotalTokens
	session.LastReportedInputTokens = usage.InputTokens
	session.LastReportedOutputTokens = usage.OutputTokens
	session.LastReportedTotalTokens = usage.TotalTokens
}

func (o *Orchestrator) addRuntimeLocked(entry *model.JobRuntime) {
	if entry == nil {
		return
	}
	if entry.StartedAt != nil {
		o.state.CodexTotals.SecondsRunning += o.now().Sub(*entry.StartedAt).Seconds()
	}
}

func (o *Orchestrator) applyCurrentConfigLocked() {
	cfg := o.currentConfig()
	o.state.PollIntervalMS = cfg.PollIntervalMS
	o.state.MaxConcurrentAgents = cfg.MaxConcurrentAgents
}

func (o *Orchestrator) refreshSnapshotLocked() {
	now := o.now().UTC()
	serviceMode, recoveryInProgress, reasons := o.publicServiceStateLocked()
	if reasons == nil {
		reasons = []contract.Reason{}
	}
	cfg := o.currentConfig()
	instanceName := "symphony"
	leaderHint := (*contract.LeaderHint)(nil)
	role := contract.InstanceRoleLeader
	if cfg != nil {
		if strings.TrimSpace(cfg.ServiceInstanceName) != "" {
			instanceName = strings.TrimSpace(cfg.ServiceInstanceName)
		}
		role = currentInstanceRole(cfg)
		if role == contract.InstanceRoleStandby && cfg.LeaderHint != nil {
			copyHint := *cfg.LeaderHint
			leaderHint = &copyHint
		}
	}

	o.snapshot = contract.ServiceStateSnapshot{
		GeneratedAt:        now.Format(time.RFC3339Nano),
		ServiceMode:        serviceMode,
		RecoveryInProgress: recoveryInProgress,
		Reasons:            reasons,
		Instance: contract.InstanceStateSummary{
			ID:      discoveryInstanceID(o.currentRuntimeIdentity()),
			Name:    instanceName,
			Version: o.serviceVersion,
			Role:    role,
		},
		Leader:       leaderHint,
		Capabilities: o.currentAvailableCapabilitiesLocked(),
	}
}

func (o *Orchestrator) publicServiceStateLocked() (contract.ServiceMode, bool, []contract.Reason) {
	recoveryInProgress := o.state.Service.RecoveryInProgress
	reasons := cloneServiceReasons(o.state.Service.Reasons)
	if recoveryInProgress {
		reasons = append(reasons, contract.MustReason(contract.ReasonServiceRecoveryInProgress, map[string]any{
			"phase": "restore",
		}))
	}

	if o.serviceModeLocked() == contract.ServiceModeUnavailable {
		return contract.ServiceModeUnavailable, recoveryInProgress, reasons
	}
	if coreReasons := o.coreDependencyReasonsLocked(); len(coreReasons) > 0 {
		reasons = append(reasons, coreReasons...)
		return contract.ServiceModeUnavailable, recoveryInProgress, reasons
	}
	if o.serviceModeLocked() == contract.ServiceModeDegraded {
		return contract.ServiceModeDegraded, recoveryInProgress, reasons
	}

	channelIDs := make([]string, 0, len(o.notificationHealth))
	for channelID, status := range o.notificationHealth {
		if status == nil {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(status.Status), "degraded") {
			channelIDs = append(channelIDs, channelID)
		}
	}
	if len(channelIDs) > 0 {
		sort.Strings(channelIDs)
		reasons = append(reasons, contract.MustReason(contract.ReasonServiceDegradedNotificationDelivery, map[string]any{
			"channel_ids": channelIDs,
		}))
		return contract.ServiceModeDegraded, recoveryInProgress, reasons
	}
	return contract.ServiceModeServing, recoveryInProgress, reasons
}

func discoverySourceKind(identity RuntimeIdentity) contract.SourceKind {
	if kind := contract.SourceKind(model.NormalizeState(identity.Compatibility.SourceKind)); kind.IsValid() {
		return kind
	}
	if kind := contract.SourceKind(model.NormalizeState(identity.Compatibility.TrackerKind)); kind.IsValid() {
		return kind
	}
	return ""
}

func discoverySourceName(identity RuntimeIdentity) string {
	return strings.TrimSpace(identity.Compatibility.ActiveSource)
}

func discoveryInstanceID(identity RuntimeIdentity) string {
	if value := strings.TrimSpace(identity.Descriptor.ConfigRoot); value != "" {
		return value
	}
	if value := strings.TrimSpace(identity.Compatibility.ActiveSource); value != "" {
		return value
	}
	return "symphony"
}

func notificationCapabilityKinds(cfg *model.ServiceConfig) []string {
	if cfg == nil || len(cfg.Notifications.Channels) == 0 {
		return []string{}
	}
	values := map[string]struct{}{}
	for _, channel := range cfg.Notifications.Channels {
		kind := strings.TrimSpace(string(channel.Kind))
		if kind == "" {
			continue
		}
		values[kind] = struct{}{}
	}
	result := make([]string, 0, len(values))
	for kind := range values {
		result = append(result, kind)
	}
	sort.Strings(result)
	return result
}

func discoveryCapabilitySources(sourceKind contract.SourceKind) []contract.SourceKind {
	if !sourceKind.IsValid() {
		return []contract.SourceKind{}
	}
	return []contract.SourceKind{sourceKind}
}

func (o *Orchestrator) publishSnapshotLocked() {
	if len(o.subscribers) == 0 {
		return
	}
	snapshot := o.snapshot
	for _, ch := range o.subscribers {
		select {
		case ch <- snapshot:
		default:
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- snapshot:
			default:
			}
		}
	}
}

func (o *Orchestrator) currentConfig() *model.ServiceConfig {
	if o.configFn == nil {
		return &model.ServiceConfig{}
	}
	cfg := o.configFn()
	if cfg == nil {
		return &model.ServiceConfig{}
	}
	return cfg
}

func (o *Orchestrator) currentWorkflow() *model.WorkflowDefinition {
	if o.workflowFn == nil {
		return &model.WorkflowDefinition{}
	}
	def := o.workflowFn()
	if def == nil {
		return &model.WorkflowDefinition{}
	}
	return def
}

func (o *Orchestrator) isDispatchEligible(issue model.Issue, cfg *model.ServiceConfig, ignoreClaim bool) bool {
	if strings.TrimSpace(issue.ID) == "" || strings.TrimSpace(issue.Identifier) == "" || strings.TrimSpace(issue.Title) == "" || strings.TrimSpace(issue.State) == "" {
		return false
	}
	if !o.isActiveState(issue.State, cfg) || o.isTerminalState(issue.State, cfg) {
		return false
	}

	o.mu.RLock()
	defer o.mu.RUnlock()
	if _, ok := o.runningRecords[issue.ID]; ok {
		return false
	}
	if record := o.state.Jobs[issue.ID]; record != nil && record.NeedsRecovery {
		return false
	}
	if _, ok := o.awaitingMergeRecords[issue.ID]; ok {
		return false
	}
	if _, ok := o.awaitingInterventionRecords[issue.ID]; ok {
		return false
	}
	if _, ok := o.retryRecords[issue.ID]; ok && !ignoreClaim {
		return false
	}

	if model.NormalizeState(issue.State) == "todo" {
		for _, blocker := range issue.BlockedBy {
			if blocker.State == nil || !o.isTerminalState(*blocker.State, cfg) {
				return false
			}
		}
	}
	return true
}

func (o *Orchestrator) hasAvailableSlots(issue model.Issue, cfg *model.ServiceConfig) bool {
	o.mu.RLock()
	defer o.mu.RUnlock()

	if cfg.MaxConcurrentAgents <= len(o.runningRecords) {
		return false
	}
	normalized := model.NormalizeState(issue.State)
	limit, ok := cfg.MaxConcurrentAgentsByState[normalized]
	if !ok {
		return true
	}
	count := 0
	for _, entry := range o.runningRecords {
		if model.NormalizeState(entry.LastKnownIssueState) == normalized {
			count++
		}
	}
	return count < limit
}

func (o *Orchestrator) isActiveState(state string, cfg *model.ServiceConfig) bool {
	normalized := model.NormalizeState(state)
	for _, item := range cfg.ActiveStates {
		if model.NormalizeState(item) == normalized {
			return true
		}
	}
	return false
}

func (o *Orchestrator) isTerminalState(state string, cfg *model.ServiceConfig) bool {
	normalized := model.NormalizeState(state)
	for _, item := range cfg.TerminalStates {
		if model.NormalizeState(item) == normalized {
			return true
		}
	}
	return false
}

func (o *Orchestrator) sendWorkerResult(result WorkerResult) {
	select {
	case o.workerResultCh <- result:
	default:
		o.logger.Warn("worker result channel is full", "issue_id", result.IssueID)
	}
}

func (o *Orchestrator) sendCodexUpdate(update CodexUpdate) {
	select {
	case o.codexUpdateCh <- update:
	default:
		o.logger.Warn("codex update channel is full", "issue_id", update.IssueID)
	}
}

func (o *Orchestrator) runtimeContext() context.Context {
	if o.runCtx != nil {
		return o.runCtx
	}
	return context.Background()
}

func (o *Orchestrator) moveToAwaitingMerge(issueID string, identifier string, issueState string, workspacePath string, branch string, retryAttempt int, stallCount int, pr *PullRequestInfo, lastError *string) {
	o.mu.Lock()
	if o.isProtectedLocked() {
		o.mu.Unlock()
		return
	}
	record := o.state.Jobs[issueID]
	if record == nil {
		record = &model.JobRuntime{
			Lifecycle:   model.JobLifecycleActive,
			DurableRefs: contract.DurableRefs{LedgerPath: ledgerPathForConfig(o.currentConfig())},
		}
		o.ensureRecordIdentityLocked(record, issueID, identifier)
		o.state.Jobs[issueID] = record
	}
	record.RetryAttempt = retryAttempt
	record.StallCount = stallCount
	record.LastKnownIssueState = issueState
	record.NeedsRecovery = false
	record.RetryDueAt = nil
	o.ensureRecordIdentityLocked(record, issueID, identifier)
	record.DurableRefs.LedgerPath = ledgerPathForConfig(o.currentConfig())
	record.DurableRefs.Workspace = &contract.WorkspaceRef{Path: workspacePath}
	record.DurableRefs.Branch = &contract.BranchRef{Name: branch}
	if pr != nil {
		record.DurableRefs.PullRequest = &contract.PullRequestRef{
			Number: pr.Number,
			URL:    pr.URL,
			State:  string(pr.State),
		}
	}
	reasonDetails := map[string]any{
		"record_id": record.RecordID,
	}
	if lastError != nil {
		reasonDetails["last_error"] = *lastError
	}
	if pr != nil {
		reasonDetails["pr_number"] = pr.Number
		reasonDetails["pr_state"] = pr.State
		reasonDetails["pr_base_owner"] = pr.BaseOwner
		reasonDetails["pr_base_repo"] = pr.BaseRepo
		reasonDetails["pr_head_owner"] = pr.HeadOwner
	}
	o.setRecordStatusLocked(record, model.JobLifecycleAwaitingMerge, &contract.Reason{
		ReasonCode: contract.ReasonRecordBlockedAwaitingMerge,
		Category:   contract.CategoryRecord,
		Details:    reasonDetails,
	}, &contract.Observation{
		Running: false,
		Summary: "awaiting pull request merge",
	})
	if record.Run != nil {
		markRunCandidateDelivery(record, pr)
		record.Run.State = contract.RunStatusCompleted
		record.Run.Phase = contract.RunPhaseSummaryPublishing
	}
	o.reindexRecordLocked(issueID, record)
	delete(o.pendingRecovery, issueID)
	o.commitStateLocked(true)
	o.mu.Unlock()
}

func cloneInterventionTemplateInputs(value []contract.InterventionInputField) []contract.InterventionInputField {
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

func interventionTemplateForRecord(record *model.JobRuntime) (contract.InterventionTemplate, bool) {
	if record == nil {
		return contract.InterventionTemplate{}, false
	}
	definition, ok := jobTypeDefinitionForDispatch(record.Dispatch)
	if !ok || len(definition.InterventionTemplates) == 0 {
		return contract.InterventionTemplate{}, false
	}
	return definition.InterventionTemplates[0], true
}

func buildInterventionState(cfg *model.ServiceConfig, now time.Time, record *model.JobRuntime, reason contract.Reason, decision contract.Decision, errorCode contract.ErrorCode) *model.InterventionState {
	if record == nil {
		return nil
	}
	template, ok := interventionTemplateForRecord(record)
	if !ok {
		return nil
	}
	domainID := "default"
	if cfg != nil && strings.TrimSpace(cfg.DomainID) != "" {
		domainID = strings.TrimSpace(cfg.DomainID)
	}
	id := fmt.Sprintf("int-%s-%d", record.RecordID, currentAttempt(record))
	state := &model.InterventionState{
		Object: contract.Intervention{
			BaseObject: contract.BaseObject{
				ID:              id,
				ObjectType:      contract.ObjectTypeIntervention,
				DomainID:        domainID,
				Visibility:      contract.VisibilityLevelRestricted,
				ContractVersion: contract.APIVersionV1,
				CreatedAt:       now.UTC().Format(time.RFC3339Nano),
				UpdatedAt:       now.UTC().Format(time.RFC3339Nano),
			},
			ObjectContext: contract.ObjectContext{
				Reasons:   []contract.Reason{reason},
				Decision:  contractDecisionPtr(decision),
				ErrorCode: errorCode,
			},
			State:          contract.InterventionStatusOpen,
			TemplateID:     template.TemplateID,
			Summary:        template.Summary,
			RequiredInputs: cloneInterventionTemplateInputs(template.RequiredInputs),
			AllowedActions: append([]contract.ControlAction(nil), template.AllowedActions...),
		},
		Handoff: model.InterventionHandoff{
			Phase:              contract.RunPhaseSummaryBlocked,
			Reason:             contractReasonPtr(reason),
			Decision:           contractDecisionPtr(decision),
			RecommendedActions: append([]contract.DecisionAction(nil), decision.RecommendedActions...),
			RequiredInputs:     cloneInterventionTemplateInputs(template.RequiredInputs),
		},
	}
	if record.Run != nil {
		state.Handoff.Phase = record.Run.Phase
		state.Handoff.ReviewSummary = strings.TrimSpace(record.Run.ReviewSummary)
		state.Handoff.ReviewFindings = append([]model.ReviewFinding(nil), record.Run.ReviewFindings...)
		if checkpoint := latestRunCheckpoint(record); checkpoint != nil {
			cloned := *checkpoint
			cloned.ArtifactIDs = append([]string(nil), checkpoint.ArtifactIDs...)
			cloned.ReferenceIDs = append([]string(nil), checkpoint.ReferenceIDs...)
			if checkpoint.Reason != nil {
				clonedReason := *checkpoint.Reason
				if len(checkpoint.Reason.Details) > 0 {
					clonedReason.Details = map[string]any{}
					for key, value := range checkpoint.Reason.Details {
						clonedReason.Details[key] = value
					}
				}
				cloned.Reason = &clonedReason
			}
			state.Handoff.Checkpoint = &cloned
		}
	}
	return state
}

func (o *Orchestrator) moveToAwaitingIntervention(issueID string, identifier string, workspacePath string, branch string, retryAttempt int, stallCount int, expectedOutcome model.CompletionMode, reason string, issueState string, pr *PullRequestInfo) {
	o.logger.Warn("issue awaiting manual intervention", "issue_id", issueID, "issue_identifier", identifier, "branch", branch, "reason", reason)

	o.mu.Lock()
	if o.isProtectedLocked() {
		o.mu.Unlock()
		return
	}
	record := o.state.Jobs[issueID]
	if record == nil {
		record = &model.JobRuntime{
			Lifecycle:   model.JobLifecycleActive,
			DurableRefs: contract.DurableRefs{LedgerPath: ledgerPathForConfig(o.currentConfig())},
		}
		o.ensureRecordIdentityLocked(record, issueID, identifier)
		o.state.Jobs[issueID] = record
	}
	reasonCode := contract.ReasonRecordBlockedAwaitingIntervention
	if reason == string(model.ContinuationReasonTrackerIssueMissing) || reason == "recovery_uncertain" {
		reasonCode = contract.ReasonRecordBlockedRecoveryUncertain
	}
	record.RetryAttempt = retryAttempt
	record.StallCount = stallCount
	record.LastKnownIssueState = issueState
	record.NeedsRecovery = false
	o.ensureRecordIdentityLocked(record, issueID, identifier)
	record.DurableRefs.LedgerPath = ledgerPathForConfig(o.currentConfig())
	record.DurableRefs.Workspace = &contract.WorkspaceRef{Path: workspacePath}
	record.DurableRefs.Branch = &contract.BranchRef{Name: branch}
	if pr != nil {
		record.DurableRefs.PullRequest = &contract.PullRequestRef{
			Number: pr.Number,
			URL:    pr.URL,
			State:  string(pr.State),
		}
	}
	o.setRecordStatusLocked(record, model.JobLifecycleAwaitingIntervention, &contract.Reason{
		ReasonCode: reasonCode,
		Category:   contract.CategoryRecord,
		Details: map[string]any{
			"record_id":        record.RecordID,
			"cause":            reason,
			"expected_outcome": string(expectedOutcome),
			"previous_branch":  branch,
			"source_state":     issueState,
		},
	}, &contract.Observation{
		Running: false,
		Summary: "awaiting external intervention",
	})
	if record.Run != nil {
		runReasonCode := contract.ReasonRunBlockedInterventionRequired
		runDecisionCode := contract.DecisionResumeAfterIntervention
		runErrorCode := contract.ErrorAPIInterventionConflict
		if reason == string(contract.ReasonRunHardViolationDetected) {
			runReasonCode = contract.ReasonRunHardViolationDetected
			runDecisionCode = contract.DecisionEscalateHardViolation
			runErrorCode = contract.ErrorRunHardViolationDetected
		} else if reason == string(contract.ReasonRunReviewGateFixLimitReached) {
			runReasonCode = contract.ReasonRunReviewGateFixLimitReached
			runDecisionCode = contract.DecisionResumeAfterIntervention
			runErrorCode = contract.ErrorReviewGateBlocked
			if record.Run.ReviewGate != nil {
				record.Run.ReviewGate.Status = contract.ReviewGateStatusInterventionRequired
			}
		}
		runReason := contract.MustReason(runReasonCode, map[string]any{
			"record_id": record.RecordID,
			"cause":     reason,
		})
		runDecision := contract.MustDecision(runDecisionCode, map[string]any{
			"record_id": record.RecordID,
			"cause":     reason,
		})
		setRunIntervention(record, runReason, runDecision, runErrorCode)
		record.Intervention = buildInterventionState(o.currentConfig(), o.now().UTC(), record, runReason, runDecision, runErrorCode)
	} else {
		record.Intervention = buildInterventionState(o.currentConfig(), o.now().UTC(), record, contract.MustReason(contract.ReasonRunBlockedInterventionRequired, map[string]any{
			"record_id": record.RecordID,
			"cause":     reason,
		}), contract.MustDecision(contract.DecisionResumeAfterIntervention, map[string]any{
			"record_id": record.RecordID,
			"cause":     reason,
		}), contract.ErrorAPIInterventionConflict)
	}
	o.reindexRecordLocked(issueID, record)
	delete(o.pendingRecovery, issueID)
	version := o.commitStateLocked(true)
	event := o.newIssueInterventionRequiredEvent(issueID, identifier, branch, reason, expectedOutcome, pr)
	if version > 0 {
		o.schedulePersistedActionLocked(version, func() {
			o.emitNotification(event)
		})
	}
	o.mu.Unlock()
	if version == 0 {
		o.emitNotification(event)
	}
}

func (o *Orchestrator) resumeIntervention(ctx context.Context, issue model.Issue, resolution contract.InterventionResolution) {
	o.mu.Lock()
	record := o.awaitingInterventionRecords[issue.ID]
	if record == nil {
		o.mu.Unlock()
		return
	}
	baseDispatch := freshDispatchContext(normalizeCompletionContract(o.currentWorkflow().Completion))
	if record.Dispatch != nil {
		baseDispatch.JobType = record.Dispatch.JobType
		baseDispatch.ExpectedOutcome = record.Dispatch.ExpectedOutcome
		baseDispatch.OnMissingPR = record.Dispatch.OnMissingPR
		baseDispatch.OnClosedPR = record.Dispatch.OnClosedPR
	}
	baseDispatch.Kind = model.DispatchKindInterventionRetry
	if strings.TrimSpace(recordBranch(record)) != "" {
		baseDispatch.PreviousBranch = dispatchStringPtr(recordBranch(record))
	}
	baseDispatch.PreviousPR = pullRequestContext(recordPullRequest(record))
	if strings.TrimSpace(record.LastKnownIssueState) != "" {
		baseDispatch.PreviousIssueState = dispatchStringPtr(record.LastKnownIssueState)
	}
	if record.Run != nil && record.Run.Recovery != nil {
		baseDispatch.RecoveryCheckpoint = model.CloneRecoveryCheckpoint(record.Run.Recovery)
	}
	if record.Intervention != nil {
		record.Intervention.Object.State = contract.InterventionStatusResolved
		resolutionCopy := resolution
		if len(resolution.ProvidedInputs) > 0 {
			resolutionCopy.ProvidedInputs = map[string]any{}
			for key, value := range resolution.ProvidedInputs {
				resolutionCopy.ProvidedInputs[key] = value
			}
		}
		if resolution.Decision != nil {
			decisionCopy := *resolution.Decision
			decisionCopy.RecommendedActions = append([]contract.DecisionAction(nil), resolution.Decision.RecommendedActions...)
			if len(resolution.Decision.Details) > 0 {
				decisionCopy.Details = map[string]any{}
				for key, value := range resolution.Decision.Details {
					decisionCopy.Details[key] = value
				}
			}
			resolutionCopy.Decision = &decisionCopy
		}
		record.Intervention.Object.Resolution = &resolutionCopy
	}
	record.Dispatch = model.CloneDispatchContext(baseDispatch)
	nextAttempt := record.RetryAttempt + 1
	o.mu.Unlock()
	o.dispatchIssue(ctx, issue, &nextAttempt)
}

func (o *Orchestrator) lookupPullRequestByHeadBranch(ctx context.Context, workspacePath string, headBranch string) (*PullRequestInfo, error) {
	branch := strings.TrimSpace(headBranch)
	if branch == "" {
		return nil, nil
	}
	if o.prLookup == nil {
		return nil, errors.New("pull request lookup is not configured")
	}
	return o.prLookup.FindByHeadBranch(ctx, workspacePath, branch)
}

func pullRequestInfoFromAwaitingMerge(record *model.JobRuntime) *PullRequestInfo {
	if record == nil {
		return nil
	}
	pr := record.DurableRefs.PullRequest
	branch := recordBranch(record)
	if pr == nil && branch == "" {
		return nil
	}
	return &PullRequestInfo{
		HeadBranch: branch,
		HeadOwner:  recordReasonDetailString(record, "pr_head_owner"),
		BaseOwner:  recordReasonDetailString(record, "pr_base_owner"),
		BaseRepo:   recordReasonDetailString(record, "pr_base_repo"),
		State:      PullRequestState(""),
	}
}

func (o *Orchestrator) lookupAwaitingMergePullRequest(ctx context.Context, workspacePath string, entry *model.JobRuntime) (*PullRequestInfo, error) {
	if o.prLookup == nil {
		return nil, errors.New("pull request lookup is not configured")
	}
	pr := pullRequestInfoFromAwaitingMerge(entry)
	if pr != nil && entry != nil && entry.DurableRefs.PullRequest != nil {
		ref := entry.DurableRefs.PullRequest
		pr.Number = ref.Number
		pr.URL = ref.URL
		pr.State = PullRequestState(ref.State)
	}
	return o.prLookup.Refresh(ctx, workspacePath, pr)
}

func (o *Orchestrator) completeSuccessfulIssue(ctx context.Context, issueID string, identifier string) {
	summary := "issue reached a terminal state"
	o.mu.RLock()
	record := o.state.Jobs[issueID]
	if record != nil && jobTypeForDispatch(record.Dispatch) == contract.JobTypeLandChange {
		summary = "target pull request merged"
	}
	o.mu.RUnlock()
	o.completeIssueWithOutcome(ctx, issueID, identifier, contract.ResultOutcomeSucceeded, summary)
}

func (o *Orchestrator) completeAbandonedIssue(ctx context.Context, issueID string, identifier string, summary string) {
	o.completeIssueWithOutcome(ctx, issueID, identifier, contract.ResultOutcomeAbandoned, summary)
}

func (o *Orchestrator) completeIssueWithOutcome(ctx context.Context, issueID string, identifier string, outcome contract.ResultOutcome, summary string) {
	o.mu.Lock()
	if o.isProtectedLocked() {
		o.mu.Unlock()
		return
	}
	delete(o.pendingRecovery, issueID)
	record := o.removeRecordLocked(issueID)
	if record == nil {
		o.mu.Unlock()
		return
	}
	record.WorkerCancel = nil
	record.RetryTimer = nil
	record.StartedAt = nil
	record.NeedsRecovery = false
	record.Lifecycle = model.JobLifecycleCompleted
	record.Reason = nil
	record.Observation = &contract.Observation{
		Running: false,
		Summary: summary,
	}
	record.Result = &contract.Result{
		Outcome:     outcome,
		Summary:     summary,
		CompletedAt: o.now().UTC().Format(time.RFC3339Nano),
		Details: map[string]any{
			"record_id": record.RecordID,
		},
	}
	record.UpdatedAt = o.now().UTC().Format(time.RFC3339Nano)
	o.persistCompletionObjectsLocked(record)
	o.rememberCompletedLocked(record)
	version := o.commitStateLocked(true)
	event := o.newIssueCompletedEvent(issueID, identifier)
	if version > 0 {
		o.schedulePersistedActionLocked(version, func() {
			o.emitNotification(event)
			if err := o.workspace.CleanupWorkspace(ctx, identifier); err != nil {
				o.logger.Warn("workspace cleanup failed", "issue_id", issueID, "identifier", identifier, "error", err.Error())
			}
		})
	}
	o.mu.Unlock()
	if version == 0 {
		o.emitNotification(event)
		if err := o.workspace.CleanupWorkspace(ctx, identifier); err != nil {
			o.logger.Warn("workspace cleanup failed", "issue_id", issueID, "identifier", identifier, "error", err.Error())
		}
	}
}

func defaultGitBranch(ctx context.Context, workspacePath string) (string, error) {
	stdout, stderr, err := runBashOutput(ctx, workspacePath, "git branch --show-current")
	if err != nil {
		return "", fmt.Errorf("git branch --show-current: %w: %s", err, strings.TrimSpace(stderr))
	}
	return strings.TrimSpace(stdout), nil
}

func runBashOutput(ctx context.Context, workspacePath string, script string) (string, string, error) {
	return runBashOutputWithTimeout(ctx, workspacePath, script, 10*time.Second)
}

func runBashOutputWithTimeout(ctx context.Context, workspacePath string, script string, timeout time.Duration) (string, string, error) {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd, err := shell.BashCommand(probeCtx, workspacePath, script)
	if err != nil {
		return "", "", err
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	return stdout.String(), stderr.String(), err
}

func compareIssues(left model.Issue, right model.Issue) bool {
	leftPriority := maxInt(priorityValue(left.Priority), 999)
	rightPriority := maxInt(priorityValue(right.Priority), 999)
	if leftPriority != rightPriority {
		return leftPriority < rightPriority
	}

	leftTime := time.Time{}
	if left.CreatedAt != nil {
		leftTime = *left.CreatedAt
	}
	rightTime := time.Time{}
	if right.CreatedAt != nil {
		rightTime = *right.CreatedAt
	}
	if !leftTime.Equal(rightTime) {
		if leftTime.IsZero() {
			return false
		}
		if rightTime.IsZero() {
			return true
		}
		return leftTime.Before(rightTime)
	}

	return left.Identifier < right.Identifier
}

func priorityValue(value *int) int {
	if value == nil {
		return 999
	}
	return *value
}

func phaseFromError(err error) model.RunPhase {
	switch {
	case errors.Is(err, model.ErrTurnTimeout):
		return model.PhaseTimedOut
	case errors.Is(err, model.ErrTurnInputRequired), errors.Is(err, model.ErrTurnFailed), errors.Is(err, model.ErrTurnCancelled), errors.Is(err, model.ErrResponseError), errors.Is(err, model.ErrCodexNotFound), errors.Is(err, model.ErrInvalidWorkspaceCWD), errors.Is(err, model.ErrPortExit):
		return model.PhaseFailed
	default:
		return model.PhaseFailed
	}
}

func optionalError(value string) *string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	copyValue := value
	return &copyValue
}

func isStallErrorText(errText string) bool {
	return strings.Contains(strings.ToLower(errText), "stalled session")
}

func (o *Orchestrator) setHealthAlertLocked(alert AlertSnapshot) bool {
	if strings.TrimSpace(alert.Code) == "" {
		return false
	}
	if existing, ok := o.healthAlerts[alert.Code]; ok {
		if existing.Level == alert.Level &&
			existing.Message == alert.Message &&
			existing.IssueID == alert.IssueID &&
			existing.IssueIdentifier == alert.IssueIdentifier {
			return false
		}
	}
	o.healthAlerts[alert.Code] = alert
	return true
}

func (o *Orchestrator) setHealthAlertAndNotifyLocked(alert AlertSnapshot) bool {
	if !o.setHealthAlertLocked(alert) {
		return false
	}
	o.emitNotificationLocked(o.newSystemAlertEvent(model.NotificationEventSystemAlert, alert))
	return true
}

func (o *Orchestrator) clearHealthAlertLocked(code string) bool {
	_, cleared := o.clearHealthAlertStateLocked(code)
	return cleared
}

func (o *Orchestrator) clearHealthAlertStateLocked(code string) (*AlertSnapshot, bool) {
	if strings.TrimSpace(code) == "" {
		return nil, false
	}
	existing, ok := o.healthAlerts[code]
	if !ok {
		return nil, false
	}
	delete(o.healthAlerts, code)
	copyAlert := existing
	return &copyAlert, true
}

func (o *Orchestrator) clearHealthAlertAndNotifyLocked(code string) bool {
	alert, cleared := o.clearHealthAlertStateLocked(code)
	if !cleared || alert == nil {
		return false
	}
	o.emitNotificationLocked(o.newSystemAlertEvent(model.NotificationEventSystemAlertCleared, *alert))
	return true
}

func attemptCountFromRetry(retryAttempt int) int {
	if retryAttempt <= 0 {
		return 1
	}
	return retryAttempt + 1
}

func workspacePathForIdentifier(root string, identifier string) string {
	cleanRoot := strings.TrimSpace(root)
	if cleanRoot == "" {
		return ""
	}
	absRoot, err := filepath.Abs(cleanRoot)
	if err != nil {
		absRoot = filepath.Clean(cleanRoot)
	}
	return filepath.Join(absRoot, model.SanitizeWorkspaceKey(identifier))
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func bashSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func archivedJobFromRuntime(record *model.JobRuntime) model.ArchivedJob {
	if record == nil {
		return model.ArchivedJob{}
	}
	return model.ArchivedJob{
		Object:              record.Object,
		RecordID:            record.RecordID,
		SourceRef:           record.SourceRef,
		Reason:              cloneReason(record.Reason),
		Observation:         cloneObservation(record.Observation),
		DurableRefs:         cloneDurableRefs(record.DurableRefs),
		Result:              cloneResult(record.Result),
		UpdatedAt:           record.UpdatedAt,
		LastKnownIssue:      model.CloneIssue(record.LastKnownIssue),
		LastKnownIssueState: record.LastKnownIssueState,
		Dispatch:            model.CloneDispatchContext(record.Dispatch),
		Run:                 model.CloneRunState(record.Run),
		Intervention:        model.CloneInterventionState(record.Intervention),
		Outcome:             record.Outcome,
		Artifacts:           append([]contract.Artifact(nil), record.Artifacts...),
	}
}

func (o *Orchestrator) rememberCompletedLocked(record *model.JobRuntime) {
	if record == nil || strings.TrimSpace(string(record.RecordID)) == "" {
		return
	}
	next := make([]model.ArchivedJob, 0, len(o.state.ArchivedJobs)+1)
	next = append(next, archivedJobFromRuntime(record))
	for _, item := range o.state.ArchivedJobs {
		if item.RecordID == record.RecordID {
			continue
		}
		next = append(next, cloneArchivedJob(item))
		if o.maxCompleted > 0 && len(next) >= o.maxCompleted {
			break
		}
	}
	o.state.ArchivedJobs = next
}

func maxInt(left int, right int) int {
	if left > right {
		return left
	}
	return right
}
