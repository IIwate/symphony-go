package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
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
	"symphony-go/internal/workspace"
)

type WorkerResult struct {
	IssueID      string
	Identifier   string
	Attempt      *int
	StartedAt    time.Time
	Phase        model.RunPhase
	Err          error
	HasNewOpenPR bool
	FinalBranch  string
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
	DispositionTryCompleteMergedPR  SuccessfulRunDisposition = "try_complete_merged_pr"
	DispositionAwaitingMerge        SuccessfulRunDisposition = "awaiting_merge"
	DispositionAwaitingIntervention SuccessfulRunDisposition = "awaiting_intervention"
	DispositionContinuation         SuccessfulRunDisposition = "continuation"
)

const maxPostMergeCloseRetries = 3

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

type Orchestrator struct {
	tracker           tracker.Client
	workspace         workspace.Manager
	runner            agent.Runner
	configFn          func() *model.ServiceConfig
	workflowFn        func() *model.WorkflowDefinition
	runtimeIdentityFn func() RuntimeIdentity
	logger            *slog.Logger
	now               func() time.Time
	randFloat         func() float64
	gitBranchFn       func(context.Context, string) (string, error)
	prLookup          PullRequestLookup
	stateStore        stateStore
	notifier          notifier

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
	healthAlerts                map[string]AlertSnapshot
	notificationHealth          map[string]*NotificationChannelHealthSnapshot
	persistenceHealth           PersistenceHealthSnapshot
	runningRecords              map[string]*model.IssueRecord
	retryRecords                map[string]*model.IssueRecord
	awaitingMergeRecords        map[string]*model.IssueRecord
	awaitingInterventionRecords map[string]*model.IssueRecord
	pendingCleanup              map[string]string
	pendingRecovery             map[string]uint64
	pendingLaunch               map[string]uint64
	pendingPostMergeTransitions map[string]uint64
	pendingActions              map[uint64][]func()
	maxCompleted                int
	startedAt                   time.Time
	serviceVersion              string
	extensionsReady             bool
	eventSeq                    uint64
	notifierGeneration          uint64
	lastPersistedStateVersion   uint64
	lastPersistedState          *durableRuntimeState
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
		tracker:           trackerClient,
		workspace:         workspaceManager,
		runner:            runner,
		configFn:          configFn,
		workflowFn:        workflowFn,
		runtimeIdentityFn: runtimeIdentityFn,
		logger:            logger,
		now:               time.Now,
		randFloat:         func() float64 { return 0.5 },
		gitBranchFn:       defaultGitBranch,
		workerResultCh:    make(chan WorkerResult, 128),
		codexUpdateCh:     make(chan CodexUpdate, 1024),
		configReloadCh:    make(chan struct{}, 8),
		refreshCh:         make(chan struct{}, 8),
		retryFireCh:       make(chan string, 128),
		shutdownCh:        make(chan struct{}),
		doneCh:            make(chan struct{}),
		state: model.OrchestratorState{
			Service: model.RuntimeServiceState{
				Mode: model.ServiceModeServing,
			},
			Records:          map[string]*model.IssueRecord{},
			ProtectedResults: map[string]*model.ProtectedResultEntry{},
		},
		subscribers:                 map[int]chan Snapshot{},
		healthAlerts:                healthAlerts,
		notificationHealth:          map[string]*NotificationChannelHealthSnapshot{},
		runningRecords:              map[string]*model.IssueRecord{},
		retryRecords:                map[string]*model.IssueRecord{},
		awaitingMergeRecords:        map[string]*model.IssueRecord{},
		awaitingInterventionRecords: map[string]*model.IssueRecord{},
		pendingCleanup:              map[string]string{},
		pendingRecovery:             map[string]uint64{},
		pendingLaunch:               map[string]uint64{},
		pendingPostMergeTransitions: map[string]uint64{},
		pendingActions:              map[uint64][]func(){},
		maxCompleted:                defaultMaxCompletedEntries,
		startedAt:                   time.Now().UTC(),
		serviceVersion:              BuildVersion,
	}
	o.prLookup = newGitHubPRLookup()
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

func cloneRuntimeRecord(value contract.IssueRuntimeRecord) contract.IssueRuntimeRecord {
	value.Reason = cloneReason(value.Reason)
	value.Observation = cloneObservation(value.Observation)
	value.DurableRefs = cloneDurableRefs(value.DurableRefs)
	value.Result = cloneResult(value.Result)
	return value
}

func cloneCompletedWindow(records []contract.IssueRuntimeRecord) []contract.IssueRuntimeRecord {
	if len(records) == 0 {
		return []contract.IssueRuntimeRecord{}
	}
	cloned := make([]contract.IssueRuntimeRecord, 0, len(records))
	for _, record := range records {
		cloned = append(cloned, cloneRuntimeRecord(record))
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

func recordIDForIssue(issue *model.Issue) contract.RecordID {
	if issue == nil {
		return ""
	}
	return contract.RecordID(fmt.Sprintf("rec_%s_%s", contract.SourceKindLinear, strings.TrimSpace(issue.ID)))
}

func sourceRefForIssue(issue *model.Issue) contract.SourceRef {
	if issue == nil {
		return contract.SourceRef{SourceKind: contract.SourceKindLinear}
	}
	url := ""
	if issue.URL != nil {
		url = strings.TrimSpace(*issue.URL)
	}
	return contract.SourceRef{
		SourceKind:       contract.SourceKindLinear,
		SourceID:         strings.TrimSpace(issue.ID),
		SourceIdentifier: strings.TrimSpace(issue.Identifier),
		URL:              url,
	}
}

func cloneIssueRecord(record *model.IssueRecord) *model.IssueRecord {
	if record == nil {
		return nil
	}
	copyValue := *record
	copyValue.Runtime = cloneRuntimeRecord(record.Runtime)
	copyValue.RetryDueAt = cloneTimePtr(record.RetryDueAt)
	copyValue.LastKnownIssue = model.CloneIssue(record.LastKnownIssue)
	copyValue.StartedAt = cloneTimePtr(record.StartedAt)
	copyValue.Dispatch = model.CloneDispatchContext(record.Dispatch)
	copyValue.RetryTimer = nil
	copyValue.WorkerCancel = nil
	return &copyValue
}

func currentAttempt(record *model.IssueRecord) int {
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

func recordIdentifier(record *model.IssueRecord) string {
	if record == nil {
		return ""
	}
	return strings.TrimSpace(record.Runtime.SourceRef.SourceIdentifier)
}

func recordWorkspacePath(record *model.IssueRecord) string {
	if record == nil || record.Runtime.DurableRefs.Workspace == nil {
		return ""
	}
	return strings.TrimSpace(record.Runtime.DurableRefs.Workspace.Path)
}

func recordBranch(record *model.IssueRecord) string {
	if record == nil || record.Runtime.DurableRefs.Branch == nil {
		return ""
	}
	return strings.TrimSpace(record.Runtime.DurableRefs.Branch.Name)
}

func recordPullRequest(record *model.IssueRecord) *PullRequestInfo {
	if record == nil || record.Runtime.DurableRefs.PullRequest == nil {
		return nil
	}
	pr := record.Runtime.DurableRefs.PullRequest
	return &PullRequestInfo{
		Number:     pr.Number,
		URL:        pr.URL,
		State:      PullRequestState(pr.State),
		HeadBranch: recordBranch(record),
	}
}

func recordReasonDetailString(record *model.IssueRecord, key string) string {
	if record == nil || record.Runtime.Reason == nil || record.Runtime.Reason.Details == nil {
		return ""
	}
	value, _ := record.Runtime.Reason.Details[key].(string)
	return strings.TrimSpace(value)
}

func isRecordRunning(record *model.IssueRecord) bool {
	return record != nil && record.Runtime.Status == contract.IssueStatusActive && record.Runtime.Observation != nil && record.Runtime.Observation.Running
}

func isRetryScheduled(record *model.IssueRecord) bool {
	return record != nil && record.Runtime.Status == contract.IssueStatusRetryScheduled
}

func isAwaitingMerge(record *model.IssueRecord) bool {
	return record != nil && record.Runtime.Status == contract.IssueStatusAwaitingMerge
}

func isAwaitingIntervention(record *model.IssueRecord) bool {
	return record != nil && record.Runtime.Status == contract.IssueStatusAwaitingIntervention
}

func issueRecordFromIssue(issue model.Issue, ledgerPath string) *model.IssueRecord {
	record := &model.IssueRecord{
		Runtime: contract.IssueRuntimeRecord{
			RecordID:  recordIDForIssue(&issue),
			SourceRef: sourceRefForIssue(&issue),
			Status:    contract.IssueStatusActive,
			UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
			DurableRefs: contract.DurableRefs{
				LedgerPath: ledgerPath,
			},
		},
		LastKnownIssue:      model.CloneIssue(&issue),
		LastKnownIssueState: strings.TrimSpace(issue.State),
	}
	return record
}

func (o *Orchestrator) ensureRecordLocked(issue model.Issue) *model.IssueRecord {
	record := o.state.Records[issue.ID]
	if record == nil {
		record = issueRecordFromIssue(issue, ledgerPathForConfig(o.currentConfig()))
		o.state.Records[issue.ID] = record
	}
	record.LastKnownIssue = model.CloneIssue(&issue)
	record.LastKnownIssueState = strings.TrimSpace(issue.State)
	record.Runtime.SourceRef = sourceRefForIssue(&issue)
	record.Runtime.RecordID = recordIDForIssue(&issue)
	record.Runtime.DurableRefs.LedgerPath = ledgerPathForConfig(o.currentConfig())
	return record
}

func (o *Orchestrator) setRecordObservationLocked(record *model.IssueRecord, observation *contract.Observation) {
	if record == nil {
		return
	}
	record.Runtime.Observation = cloneObservation(observation)
	record.Runtime.UpdatedAt = o.now().UTC().Format(time.RFC3339Nano)
}

func (o *Orchestrator) setRecordReasonLocked(record *model.IssueRecord, reason *contract.Reason) {
	if record == nil {
		return
	}
	record.Runtime.Reason = cloneReason(reason)
	record.Runtime.UpdatedAt = o.now().UTC().Format(time.RFC3339Nano)
}

func (o *Orchestrator) setRecordStatusLocked(record *model.IssueRecord, status contract.IssueStatus, reason *contract.Reason, observation *contract.Observation) {
	if record == nil {
		return
	}
	record.Runtime.Status = status
	record.Runtime.Reason = cloneReason(reason)
	record.Runtime.Observation = cloneObservation(observation)
	record.Runtime.UpdatedAt = o.now().UTC().Format(time.RFC3339Nano)
	if status != contract.IssueStatusRetryScheduled {
		record.RetryDueAt = nil
	}
}

func (o *Orchestrator) removeRecordLocked(issueID string) *model.IssueRecord {
	record := o.state.Records[issueID]
	delete(o.state.Records, issueID)
	delete(o.runningRecords, issueID)
	delete(o.retryRecords, issueID)
	delete(o.awaitingMergeRecords, issueID)
	delete(o.awaitingInterventionRecords, issueID)
	return record
}

func (o *Orchestrator) reindexRecordLocked(issueID string, record *model.IssueRecord) {
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

func recordUpdatedAt(record *model.IssueRecord, fallback time.Time) time.Time {
	if record == nil {
		return fallback
	}
	if ts, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(record.Runtime.UpdatedAt)); err == nil {
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
		contract.MustReason(contract.ReasonServiceUnavailableCoreDependency, map[string]any{
			"component": "ledger_store",
			"detail":    strings.TrimSpace(reason),
		}),
	}
	o.setHealthAlertAndNotifyLocked(AlertSnapshot{
		Code:    serviceProtectedModeCode,
		Level:   "warn",
		Message: fmt.Sprintf("service became unavailable: %s", reason),
	})
	return true
}

func (o *Orchestrator) rememberProtectedResultLocked(issueID string, entry *model.IssueRecord, result WorkerResult) {
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
		Identifier:    entry.Runtime.SourceRef.SourceIdentifier,
		WorkspacePath: entry.Runtime.DurableRefs.Workspace.Path,
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
	snapshot := o.snapshot
	identity := o.currentRuntimeIdentity()
	sourceKind := discoverySourceKind(identity)
	sourceName := strings.TrimSpace(identity.Compatibility.ActiveSource)
	if sourceName == "" {
		sourceName = "linear-main"
	}

	return contract.DiscoveryDocument{
		APIVersion: contract.APIVersionV1,
		Instance: contract.InstanceDocument{
			ID:      discoveryInstanceID(identity),
			Name:    "symphony",
			Version: o.serviceVersion,
		},
		Source: contract.SourceDocument{
			Kind: sourceKind,
			Name: sourceName,
		},
		ServiceMode:        snapshot.ServiceMode,
		RecoveryInProgress: snapshot.RecoveryInProgress,
		Capabilities: contract.CapabilityDocument{
			EventProtocol:  "sse",
			ControlActions: []contract.ControlAction{contract.ControlActionRefresh},
			Notifications:  notificationCapabilityKinds(o.currentConfig()),
			Sources:        []contract.SourceKind{sourceKind},
		},
		Reasons: cloneServiceReasons(snapshot.Reasons),
		Limits: contract.LimitDocument{
			CompletedWindowSize: o.maxCompleted,
		},
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

	cfg := o.currentConfig()
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
	record.Runtime.Result = nil
	record.LastKnownIssue = model.CloneIssue(&issue)
	record.LastKnownIssueState = strings.TrimSpace(issue.State)
	o.setRecordStatusLocked(record, contract.IssueStatusActive, nil, &contract.Observation{
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
		if entry := o.state.Records[issue.ID]; isRecordRunning(entry) {
			entry.Runtime.DurableRefs.Workspace = &contract.WorkspaceRef{Path: workspaceRef.Path}
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

		result.Phase = model.PhaseStreamingTurn
		runErr := o.runner.Run(workerCtx, agent.RunParams{
			Issue:          model.CloneIssue(&issue),
			Attempt:        attempt,
			WorkspacePath:  workspaceRef.Path,
			PromptTemplate: workflowDef.PromptTemplate,
			Source:         workflowDef.Source,
			Dispatch:       model.CloneDispatchContext(dispatch),
			ProcessEnv:     workspaceProcessEnv(workspaceRef),
			MaxTurns:       o.currentConfig().MaxTurns,
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
		Kind:            model.DispatchKindFresh,
		ExpectedOutcome: contract.Mode,
		OnMissingPR:     contract.OnMissingPR,
		OnClosedPR:      contract.OnClosedPR,
	}
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

func (o *Orchestrator) classifySuccessfulRun(ctx context.Context, workspacePath string, finalBranch string, dispatch *model.DispatchContext, autoCloseOnPR bool, issueState string) (*SuccessfulRunDecision, error) {
	contract := normalizeCompletionContract(model.CompletionContract{
		Mode:        model.CompletionModeNone,
		OnMissingPR: dispatchCompletionAction(dispatch, "missing"),
		OnClosedPR:  dispatchCompletionAction(dispatch, "closed"),
	})
	if dispatch != nil {
		if dispatch.ExpectedOutcome != "" {
			contract.Mode = dispatch.ExpectedOutcome
		}
		if dispatch.OnMissingPR != "" {
			contract.OnMissingPR = dispatch.OnMissingPR
		}
		if dispatch.OnClosedPR != "" {
			contract.OnClosedPR = dispatch.OnClosedPR
		}
	}
	branch := strings.TrimSpace(finalBranch)
	if contract.Mode != model.CompletionModePullRequest {
		reason := model.ContinuationReasonUnfinishedIssue
		return &SuccessfulRunDecision{
			Disposition:     DispositionContinuation,
			Reason:          &reason,
			ExpectedOutcome: contract.Mode,
			FinalBranch:     branch,
		}, nil
	}
	if branch == "" {
		return decisionForMissingPullRequest(contract, branch), nil
	}
	pr, err := o.lookupPullRequestByHeadBranch(ctx, workspacePath, branch)
	if err != nil {
		return nil, err
	}
	if pr == nil {
		return decisionForMissingPullRequest(contract, branch), nil
	}
	switch pr.State {
	case PullRequestStateOpen:
		return &SuccessfulRunDecision{
			Disposition:     DispositionAwaitingMerge,
			ExpectedOutcome: contract.Mode,
			PR:              clonePullRequestInfo(pr),
			FinalBranch:     branch,
		}, nil
	case PullRequestStateMerged:
		if autoCloseOnPR {
			return &SuccessfulRunDecision{
				Disposition:     DispositionTryCompleteMergedPR,
				ExpectedOutcome: contract.Mode,
				PR:              clonePullRequestInfo(pr),
				FinalBranch:     branch,
			}, nil
		}
		reason := model.ContinuationReasonMergedPRAutoCloseOff
		return &SuccessfulRunDecision{
			Disposition:     DispositionAwaitingIntervention,
			Reason:          &reason,
			ExpectedOutcome: contract.Mode,
			PR:              clonePullRequestInfo(pr),
			FinalBranch:     branch,
		}, nil
	case PullRequestStateClosed:
		reason := model.ContinuationReasonClosedUnmergedPR
		if contract.OnClosedPR == model.CompletionActionContinue {
			return &SuccessfulRunDecision{
				Disposition:     DispositionContinuation,
				Reason:          &reason,
				ExpectedOutcome: contract.Mode,
				PR:              clonePullRequestInfo(pr),
				FinalBranch:     branch,
			}, nil
		}
		return &SuccessfulRunDecision{
			Disposition:     DispositionAwaitingIntervention,
			Reason:          &reason,
			ExpectedOutcome: contract.Mode,
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

func (o *Orchestrator) handleWorkerExit(result WorkerResult) {
	o.mu.Lock()
	entry := o.state.Records[result.IssueID]
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

	if result.Err != nil {
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
		entry.Runtime.DurableRefs.Workspace = &contract.WorkspaceRef{Path: workspacePath}
	}
	finalBranch := strings.TrimSpace(result.FinalBranch)
	if finalBranch != "" {
		entry.Runtime.DurableRefs.Branch = &contract.BranchRef{Name: finalBranch}
	}
	entry.NeedsRecovery = true
	entry.Dispatch = model.CloneDispatchContext(dispatch)
	entry.LastKnownIssueState = issueState
	o.setRecordStatusLocked(entry, contract.IssueStatusActive, nil, &contract.Observation{
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

	entry := o.state.Records[update.IssueID]
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
	for issueID, entry := range o.state.Records {
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
			o.terminateRunningLocked(ctx, issueID, false, true, "stalled session")
		}
	}
	ids := make([]string, 0, len(o.state.Records))
	for issueID, entry := range o.state.Records {
		if isRecordRunning(entry) {
			ids = append(ids, issueID)
		}
	}
	o.mu.Unlock()

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
	for issueID, entry := range o.state.Records {
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
			entry.Runtime.SourceRef = sourceRefForIssue(&issue)
			continue
		}
		o.terminateRunningLocked(ctx, issueID, false, false, "")
	}
	o.commitStateLocked(true)
	return true, true
}

func (o *Orchestrator) reconcileAwaitingMerge(ctx context.Context) {
	o.mu.RLock()
	protected := o.isProtectedLocked()
	o.mu.RUnlock()
	if protected {
		return
	}

	o.mu.RLock()
	pending := make(map[string]*model.IssueRecord, len(o.awaitingMergeRecords))
	for issueID, entry := range o.awaitingMergeRecords {
		if entry == nil {
			continue
		}
		if _, waitingForAck := o.pendingPostMergeTransitions[issueID]; waitingForAck {
			continue
		}
		pending[issueID] = cloneIssueRecord(entry)
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
					current.Runtime.SourceRef = sourceRefForIssue(&issue)
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
						"record_id":  current.Runtime.RecordID,
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
			if entry.Runtime.DurableRefs.PullRequest != nil {
				o.mu.Lock()
				current := o.awaitingMergeRecords[issueID]
				if current != nil {
					o.setRecordReasonLocked(current, &contract.Reason{
						ReasonCode: contract.ReasonRecordBlockedAwaitingMerge,
						Category:   contract.CategoryRecord,
						Details: map[string]any{
							"record_id":     current.Runtime.RecordID,
							"last_error":    "pull request refresh returned no match",
							"pr_number":     current.Runtime.DurableRefs.PullRequest.Number,
							"pr_state":      current.Runtime.DurableRefs.PullRequest.State,
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
				current.Runtime.DurableRefs.PullRequest = &contract.PullRequestRef{
					Number: pr.Number,
					URL:    pr.URL,
					State:  string(pr.State),
				}
				current.RetryDueAt = nil
				o.setRecordReasonLocked(current, &contract.Reason{
					ReasonCode: contract.ReasonRecordBlockedAwaitingMerge,
					Category:   contract.CategoryRecord,
					Details: map[string]any{
						"record_id":     current.Runtime.RecordID,
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
			if entry.RetryDueAt != nil && entry.RetryDueAt.After(o.now().UTC()) {
				continue
			}
			o.tryCompleteMergedPullRequest(ctx, issueID, recordIdentifier(entry), recordWorkspacePath(entry), recordBranch(entry), entry.RetryAttempt, entry.StallCount, entry.LastKnownIssueState, pr)
		case PullRequestStateClosed:
			o.moveToAwaitingIntervention(issueID, recordIdentifier(entry), recordWorkspacePath(entry), recordBranch(entry), entry.RetryAttempt, entry.StallCount, model.CompletionModePullRequest, string(model.ContinuationReasonClosedUnmergedPR), entry.LastKnownIssueState, pr)
		default:
			errorText := fmt.Sprintf("unsupported pull request state %q", pr.State)
			o.logger.Warn("awaiting-merge PR state is unsupported", "issue_id", issueID, "issue_identifier", recordIdentifier(entry), "branch", recordBranch(entry), "state", pr.State)
			o.mu.Lock()
			current := o.awaitingMergeRecords[issueID]
			if current != nil {
				current.Runtime.DurableRefs.PullRequest = &contract.PullRequestRef{
					Number: pr.Number,
					URL:    pr.URL,
					State:  string(pr.State),
				}
				o.setRecordReasonLocked(current, &contract.Reason{
					ReasonCode: contract.ReasonRecordBlockedAwaitingMerge,
					Category:   contract.CategoryRecord,
					Details: map[string]any{
						"record_id":     current.Runtime.RecordID,
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
	pending := make(map[string]*model.IssueRecord, len(o.awaitingInterventionRecords))
	for issueID, entry := range o.awaitingInterventionRecords {
		if entry == nil {
			continue
		}
		pending[issueID] = cloneIssueRecord(entry)
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
						"record_id": current.Runtime.RecordID,
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
	snapshot := cloneIssueRecord(retryEntry)
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
			o.scheduleRetryLocked(issueID, recordIdentifier(current), current.RetryAttempt+1, &errorText, false, current.StallCount, current.Dispatch)
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
			o.scheduleRetryLocked(issueID, issue.Identifier, current.RetryAttempt+1, &errorText, false, current.StallCount, current.Dispatch)
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
	o.setRecordStatusLocked(entry, contract.IssueStatusActive, nil, &contract.Observation{
		Running: false,
		Summary: "run terminated by orchestrator",
	})
	o.reindexRecordLocked(issueID, entry)
}

func (o *Orchestrator) scheduleRetryLocked(issueID string, identifier string, attempt int, errText *string, continuation bool, stallCount int, dispatch *model.DispatchContext) {
	record := o.state.Records[issueID]
	if record == nil {
		record = &model.IssueRecord{
			Runtime: contract.IssueRuntimeRecord{
				RecordID: contract.RecordID(fmt.Sprintf("rec_%s_%s", contract.SourceKindLinear, strings.TrimSpace(issueID))),
				SourceRef: contract.SourceRef{
					SourceKind:       contract.SourceKindLinear,
					SourceID:         strings.TrimSpace(issueID),
					SourceIdentifier: strings.TrimSpace(identifier),
				},
				DurableRefs: contract.DurableRefs{
					LedgerPath: ledgerPathForConfig(o.currentConfig()),
				},
			},
		}
		o.state.Records[issueID] = record
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
	if record.Runtime.DurableRefs.Workspace == nil {
		record.Runtime.DurableRefs.Workspace = &contract.WorkspaceRef{Path: workspacePathForIdentifier(o.currentConfig().WorkspaceRoot, identifier)}
	}
	record.RetryAttempt = attempt
	record.StallCount = stallCount
	record.RetryDueAt = &dueAt
	record.RetryTimer = timer
	record.Dispatch = retryDispatch
	o.setRecordStatusLocked(record, contract.IssueStatusRetryScheduled, &contract.Reason{
		ReasonCode: contract.ReasonRecordBlockedRetryScheduled,
		Category:   contract.CategoryRecord,
		Details: map[string]any{
			"record_id": record.Runtime.RecordID,
			"attempt":   attemptCountFromRetry(attempt),
		},
	}, &contract.Observation{
		Running: false,
		Summary: "retry scheduled",
	})
	if errText != nil {
		record.Runtime.Reason.Details["last_error"] = *errText
	}
	record.Runtime.SourceRef.SourceIdentifier = strings.TrimSpace(identifier)
	record.Runtime.SourceRef.SourceID = strings.TrimSpace(issueID)
	record.Runtime.DurableRefs.LedgerPath = ledgerPathForConfig(o.currentConfig())
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

func (o *Orchestrator) addRuntimeLocked(entry *model.IssueRecord) {
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
	records := make([]contract.IssueRuntimeRecord, 0, len(o.state.Records))
	for _, entry := range o.state.Records {
		if entry == nil {
			continue
		}
		records = append(records, cloneRuntimeRecord(entry.Runtime))
	}
	sort.SliceStable(records, func(i int, j int) bool {
		if records[i].SourceRef.SourceIdentifier != records[j].SourceRef.SourceIdentifier {
			return records[i].SourceRef.SourceIdentifier < records[j].SourceRef.SourceIdentifier
		}
		return records[i].RecordID < records[j].RecordID
	})

	completed := cloneCompletedWindow(o.state.CompletedWindow)
	if completed == nil {
		completed = []contract.IssueRuntimeRecord{}
	}

	counts := contract.StateCounts{
		Total:     len(records),
		Completed: len(completed),
	}
	for _, record := range records {
		switch record.Status {
		case contract.IssueStatusActive:
			counts.Active++
		case contract.IssueStatusRetryScheduled:
			counts.RetryScheduled++
		case contract.IssueStatusAwaitingMerge:
			counts.AwaitingMerge++
		case contract.IssueStatusAwaitingIntervention:
			counts.AwaitingIntervention++
		case contract.IssueStatusCompleted:
			counts.Completed++
		}
	}

	serviceMode, recoveryInProgress, reasons := o.publicServiceStateLocked()
	if reasons == nil {
		reasons = []contract.Reason{}
	}

	o.snapshot = contract.ServiceStateSnapshot{
		GeneratedAt:        now.Format(time.RFC3339Nano),
		ServiceMode:        serviceMode,
		RecoveryInProgress: recoveryInProgress,
		Reasons:            reasons,
		Counts:             counts,
		Records:            records,
		CompletedWindow: contract.CompletedWindow{
			Limit:   o.maxCompleted,
			Records: completed,
		},
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

	switch o.serviceModeLocked() {
	case contract.ServiceModeUnavailable:
		return contract.ServiceModeUnavailable, recoveryInProgress, reasons
	case contract.ServiceModeDegraded:
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
	kind := contract.SourceKind(model.NormalizeState(identity.Compatibility.SourceKind))
	if kind.IsValid() {
		return kind
	}
	return contract.SourceKindLinear
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
	if record := o.state.Records[issue.ID]; record != nil && record.NeedsRecovery {
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
	record := o.state.Records[issueID]
	if record == nil {
		record = &model.IssueRecord{
			Runtime: contract.IssueRuntimeRecord{
				RecordID: contract.RecordID(fmt.Sprintf("rec_%s_%s", contract.SourceKindLinear, strings.TrimSpace(issueID))),
				SourceRef: contract.SourceRef{
					SourceKind:       contract.SourceKindLinear,
					SourceID:         strings.TrimSpace(issueID),
					SourceIdentifier: strings.TrimSpace(identifier),
				},
				DurableRefs: contract.DurableRefs{
					LedgerPath: ledgerPathForConfig(o.currentConfig()),
				},
			},
		}
		o.state.Records[issueID] = record
	}
	record.RetryAttempt = retryAttempt
	record.StallCount = stallCount
	record.LastKnownIssueState = issueState
	record.NeedsRecovery = false
	record.RetryDueAt = nil
	record.Runtime.SourceRef.SourceIdentifier = identifier
	record.Runtime.DurableRefs.LedgerPath = ledgerPathForConfig(o.currentConfig())
	record.Runtime.DurableRefs.Workspace = &contract.WorkspaceRef{Path: workspacePath}
	record.Runtime.DurableRefs.Branch = &contract.BranchRef{Name: branch}
	if pr != nil {
		record.Runtime.DurableRefs.PullRequest = &contract.PullRequestRef{
			Number: pr.Number,
			URL:    pr.URL,
			State:  string(pr.State),
		}
	}
	reasonDetails := map[string]any{
		"record_id": record.Runtime.RecordID,
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
	o.setRecordStatusLocked(record, contract.IssueStatusAwaitingMerge, &contract.Reason{
		ReasonCode: contract.ReasonRecordBlockedAwaitingMerge,
		Category:   contract.CategoryRecord,
		Details:    reasonDetails,
	}, &contract.Observation{
		Running: false,
		Summary: "awaiting pull request merge",
	})
	o.reindexRecordLocked(issueID, record)
	delete(o.pendingRecovery, issueID)
	o.commitStateLocked(true)
	o.mu.Unlock()
}

func (o *Orchestrator) moveToAwaitingIntervention(issueID string, identifier string, workspacePath string, branch string, retryAttempt int, stallCount int, expectedOutcome model.CompletionMode, reason string, issueState string, pr *PullRequestInfo) {
	o.logger.Warn("issue awaiting manual intervention", "issue_id", issueID, "issue_identifier", identifier, "branch", branch, "reason", reason)

	o.mu.Lock()
	if o.isProtectedLocked() {
		o.mu.Unlock()
		return
	}
	record := o.state.Records[issueID]
	if record == nil {
		record = &model.IssueRecord{
			Runtime: contract.IssueRuntimeRecord{
				RecordID: contract.RecordID(fmt.Sprintf("rec_%s_%s", contract.SourceKindLinear, strings.TrimSpace(issueID))),
				SourceRef: contract.SourceRef{
					SourceKind:       contract.SourceKindLinear,
					SourceID:         strings.TrimSpace(issueID),
					SourceIdentifier: strings.TrimSpace(identifier),
				},
				DurableRefs: contract.DurableRefs{
					LedgerPath: ledgerPathForConfig(o.currentConfig()),
				},
			},
		}
		o.state.Records[issueID] = record
	}
	reasonCode := contract.ReasonRecordBlockedAwaitingIntervention
	if reason == string(model.ContinuationReasonTrackerIssueMissing) || reason == "recovery_uncertain" {
		reasonCode = contract.ReasonRecordBlockedRecoveryUncertain
	}
	record.RetryAttempt = retryAttempt
	record.StallCount = stallCount
	record.LastKnownIssueState = issueState
	record.NeedsRecovery = false
	record.Runtime.SourceRef.SourceIdentifier = identifier
	record.Runtime.DurableRefs.LedgerPath = ledgerPathForConfig(o.currentConfig())
	record.Runtime.DurableRefs.Workspace = &contract.WorkspaceRef{Path: workspacePath}
	record.Runtime.DurableRefs.Branch = &contract.BranchRef{Name: branch}
	if pr != nil {
		record.Runtime.DurableRefs.PullRequest = &contract.PullRequestRef{
			Number: pr.Number,
			URL:    pr.URL,
			State:  string(pr.State),
		}
	}
	o.setRecordStatusLocked(record, contract.IssueStatusAwaitingIntervention, &contract.Reason{
		ReasonCode: reasonCode,
		Category:   contract.CategoryRecord,
		Details: map[string]any{
			"record_id":        record.Runtime.RecordID,
			"cause":            reason,
			"expected_outcome": string(expectedOutcome),
			"previous_branch":  branch,
		},
	}, &contract.Observation{
		Running: false,
		Summary: "operator intervention required",
	})
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

func (o *Orchestrator) handleFailedPostMergeTransition(issueID string, identifier string, workspacePath string, branch string, retryAttempt int, stallCount int, issueState string, pr *PullRequestInfo, errorText string, retryable bool) bool {
	if !retryable {
		o.moveToAwaitingIntervention(issueID, identifier, workspacePath, branch, retryAttempt, stallCount, model.CompletionModePullRequest, "post_merge_transition_failed", issueState, pr)
		return false
	}

	o.mu.Lock()
	if o.isProtectedLocked() {
		o.mu.Unlock()
		return false
	}
	current := o.state.Records[issueID]
	if current == nil {
		current = &model.IssueRecord{
			Runtime: contract.IssueRuntimeRecord{
				RecordID: contract.RecordID(fmt.Sprintf("rec_%s_%s", contract.SourceKindLinear, strings.TrimSpace(issueID))),
				SourceRef: contract.SourceRef{
					SourceKind:       contract.SourceKindLinear,
					SourceID:         strings.TrimSpace(issueID),
					SourceIdentifier: strings.TrimSpace(identifier),
				},
				DurableRefs: contract.DurableRefs{
					LedgerPath: ledgerPathForConfig(o.currentConfig()),
				},
			},
		}
		o.state.Records[issueID] = current
	}
	postMergeRetryCount := 1
	if current.Runtime.Reason != nil {
		if value, ok := current.Runtime.Reason.Details["post_merge_retry_count"].(int); ok {
			postMergeRetryCount = value + 1
		}
		if value, ok := current.Runtime.Reason.Details["post_merge_retry_count"].(float64); ok {
			postMergeRetryCount = int(value) + 1
		}
	}
	if postMergeRetryCount > maxPostMergeCloseRetries {
		o.mu.Unlock()
		o.moveToAwaitingIntervention(issueID, identifier, workspacePath, branch, retryAttempt, stallCount, model.CompletionModePullRequest, "post_merge_transition_failed", issueState, pr)
		return false
	}

	nextRetryAt := o.now().UTC().Add(postMergeRetryDelay(postMergeRetryCount, o.currentConfig().MaxRetryBackoffMS))
	current.RetryAttempt = retryAttempt
	current.StallCount = stallCount
	current.LastKnownIssueState = issueState
	current.Runtime.SourceRef.SourceIdentifier = identifier
	current.Runtime.DurableRefs.LedgerPath = ledgerPathForConfig(o.currentConfig())
	current.Runtime.DurableRefs.Workspace = &contract.WorkspaceRef{Path: workspacePath}
	current.Runtime.DurableRefs.Branch = &contract.BranchRef{Name: branch}
	current.RetryDueAt = &nextRetryAt
	if pr != nil {
		current.Runtime.DurableRefs.PullRequest = &contract.PullRequestRef{
			Number: pr.Number,
			URL:    pr.URL,
			State:  string(pr.State),
		}
	}
	o.setRecordStatusLocked(current, contract.IssueStatusAwaitingMerge, &contract.Reason{
		ReasonCode: contract.ReasonRecordBlockedAwaitingMerge,
		Category:   contract.CategoryRecord,
		Details: map[string]any{
			"record_id":                current.Runtime.RecordID,
			"last_error":               errorText,
			"post_merge_retry_count":   postMergeRetryCount,
			"next_post_merge_retry_at": nextRetryAt.Format(time.RFC3339Nano),
		},
	}, &contract.Observation{
		Running: false,
		Summary: "awaiting post-merge retry",
	})
	if pr != nil && current.Runtime.Reason != nil {
		current.Runtime.Reason.Details["pr_number"] = pr.Number
		current.Runtime.Reason.Details["pr_state"] = pr.State
		current.Runtime.Reason.Details["pr_base_owner"] = pr.BaseOwner
		current.Runtime.Reason.Details["pr_base_repo"] = pr.BaseRepo
		current.Runtime.Reason.Details["pr_head_owner"] = pr.HeadOwner
	}
	o.reindexRecordLocked(issueID, current)
	delete(o.pendingRecovery, issueID)
	o.commitStateLocked(true)
	o.mu.Unlock()
	return false
}

func postMergeRetryDelay(attempt int, maxBackoffMS int) time.Duration {
	maxBackoff := maxInt(maxBackoffMS, 10000)
	base := math.Min(float64(10000)*math.Pow(2, float64(maxInt(attempt, 1)-1)), float64(maxBackoff))
	return time.Duration(base) * time.Millisecond
}

func isRetryablePostMergeError(err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, context.Canceled):
		return false
	case errors.Is(err, context.DeadlineExceeded):
		return true
	case errors.Is(err, model.ErrLinearAPIRequest):
		return true
	case errors.Is(err, model.ErrLinearAPIStatus):
		return true
	default:
		return false
	}
}

func (o *Orchestrator) stageMergedPullRequestCompletion(issueID string, identifier string, workspacePath string, branch string, retryAttempt int, stallCount int, issueState string, pr *PullRequestInfo, action func()) (uint64, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.isProtectedLocked() {
		return 0, false
	}
	if _, waitingForAck := o.pendingPostMergeTransitions[issueID]; waitingForAck {
		return 0, true
	}

	current := o.state.Records[issueID]
	if current == nil {
		current = &model.IssueRecord{
			Runtime: contract.IssueRuntimeRecord{
				RecordID: contract.RecordID(fmt.Sprintf("rec_%s_%s", contract.SourceKindLinear, strings.TrimSpace(issueID))),
				SourceRef: contract.SourceRef{
					SourceKind:       contract.SourceKindLinear,
					SourceID:         strings.TrimSpace(issueID),
					SourceIdentifier: strings.TrimSpace(identifier),
				},
				DurableRefs: contract.DurableRefs{
					LedgerPath: ledgerPathForConfig(o.currentConfig()),
				},
			},
		}
		o.state.Records[issueID] = current
	}
	current.RetryAttempt = retryAttempt
	current.StallCount = stallCount
	current.LastKnownIssueState = issueState
	current.NeedsRecovery = false
	current.RetryDueAt = nil
	current.Runtime.SourceRef.SourceIdentifier = identifier
	current.Runtime.DurableRefs.LedgerPath = ledgerPathForConfig(o.currentConfig())
	current.Runtime.DurableRefs.Workspace = &contract.WorkspaceRef{Path: workspacePath}
	current.Runtime.DurableRefs.Branch = &contract.BranchRef{Name: branch}
	if pr != nil {
		current.Runtime.DurableRefs.PullRequest = &contract.PullRequestRef{
			Number: pr.Number,
			URL:    pr.URL,
			State:  string(pr.State),
		}
	}
	o.setRecordStatusLocked(current, contract.IssueStatusAwaitingMerge, &contract.Reason{
		ReasonCode: contract.ReasonRecordBlockedAwaitingMerge,
		Category:   contract.CategoryRecord,
		Details: map[string]any{
			"record_id": current.Runtime.RecordID,
		},
	}, &contract.Observation{
		Running: false,
		Summary: "awaiting pull request merge",
	})
	if pr != nil && current.Runtime.Reason != nil {
		current.Runtime.Reason.Details["pr_number"] = pr.Number
		current.Runtime.Reason.Details["pr_state"] = pr.State
		current.Runtime.Reason.Details["pr_base_owner"] = pr.BaseOwner
		current.Runtime.Reason.Details["pr_base_repo"] = pr.BaseRepo
		current.Runtime.Reason.Details["pr_head_owner"] = pr.HeadOwner
	}
	o.reindexRecordLocked(issueID, current)
	delete(o.pendingRecovery, issueID)
	version := o.commitStateLocked(true)
	if version > 0 && action != nil {
		o.pendingPostMergeTransitions[issueID] = version
		o.schedulePersistedActionLocked(version, action)
	}
	return version, false
}

func (o *Orchestrator) finishMergedPullRequestCompletion(ctx context.Context, issueID string, identifier string, workspacePath string, branch string, retryAttempt int, stallCount int, issueState string, pr *PullRequestInfo) bool {
	completer, ok := o.tracker.(tracker.IssueCompleter)
	if !ok {
		o.logger.Warn("tracker does not support issue completion", "issue_id", issueID, "issue_identifier", identifier, "branch", branch)
		o.moveToAwaitingIntervention(issueID, identifier, workspacePath, branch, retryAttempt, stallCount, model.CompletionModePullRequest, "post_merge_transition_unsupported", issueState, pr)
		return false
	}
	if err := completer.CompleteIssue(ctx, issueID); err != nil {
		errorText := fmt.Sprintf("post-merge transition failed: %s", err.Error())
		o.logger.Warn("post-merge transition failed", "issue_id", issueID, "issue_identifier", identifier, "branch", branch, "error", err.Error())
		return o.handleFailedPostMergeTransition(issueID, identifier, workspacePath, branch, retryAttempt, stallCount, issueState, pr, errorText, isRetryablePostMergeError(err))
	}

	issues, err := o.tracker.FetchIssueStatesByIDs(ctx, []string{issueID})
	cfg := o.currentConfig()
	if err == nil && len(issues) > 0 {
		issueState = issues[0].State
		if o.isTerminalState(issues[0].State, cfg) {
			o.completeSuccessfulIssue(ctx, issueID, identifier)
			return true
		}
	}
	if err != nil {
		errorText := fmt.Sprintf("post-merge state refresh failed: %s", err.Error())
		return o.handleFailedPostMergeTransition(issueID, identifier, workspacePath, branch, retryAttempt, stallCount, issueState, pr, errorText, isRetryablePostMergeError(err))
	}
	return o.handleFailedPostMergeTransition(issueID, identifier, workspacePath, branch, retryAttempt, stallCount, issueState, pr, "post-merge transition did not reach terminal state", true)
}

func (o *Orchestrator) tryCompleteMergedPullRequest(ctx context.Context, issueID string, identifier string, workspacePath string, branch string, retryAttempt int, stallCount int, issueState string, pr *PullRequestInfo) bool {
	var version uint64
	copyPR := clonePullRequestInfo(pr)
	action := func() {
		defer func() {
			o.mu.Lock()
			if pendingVersion, ok := o.pendingPostMergeTransitions[issueID]; ok && pendingVersion == version {
				delete(o.pendingPostMergeTransitions, issueID)
			}
			o.mu.Unlock()
		}()
		o.finishMergedPullRequestCompletion(o.runtimeContext(), issueID, identifier, workspacePath, branch, retryAttempt, stallCount, issueState, copyPR)
	}
	var waitingForAck bool
	version, waitingForAck = o.stageMergedPullRequestCompletion(issueID, identifier, workspacePath, branch, retryAttempt, stallCount, issueState, copyPR, action)
	if waitingForAck {
		return true
	}
	if version > 0 {
		return true
	}
	return o.finishMergedPullRequestCompletion(ctx, issueID, identifier, workspacePath, branch, retryAttempt, stallCount, issueState, copyPR)
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

func pullRequestInfoFromAwaitingMerge(record *model.IssueRecord) *PullRequestInfo {
	if record == nil {
		return nil
	}
	pr := record.Runtime.DurableRefs.PullRequest
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

func (o *Orchestrator) lookupAwaitingMergePullRequest(ctx context.Context, workspacePath string, entry *model.IssueRecord) (*PullRequestInfo, error) {
	if o.prLookup == nil {
		return nil, errors.New("pull request lookup is not configured")
	}
	pr := pullRequestInfoFromAwaitingMerge(entry)
	if pr != nil && entry != nil && entry.Runtime.DurableRefs.PullRequest != nil {
		ref := entry.Runtime.DurableRefs.PullRequest
		pr.Number = ref.Number
		pr.URL = ref.URL
		pr.State = PullRequestState(ref.State)
	}
	return o.prLookup.Refresh(ctx, workspacePath, pr)
}

func (o *Orchestrator) completeSuccessfulIssue(ctx context.Context, issueID string, identifier string) {
	o.completeIssueWithOutcome(ctx, issueID, identifier, contract.ResultOutcomeSucceeded, "issue reached a terminal state")
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
	record.Runtime.Status = contract.IssueStatusCompleted
	record.Runtime.Reason = nil
	record.Runtime.Observation = &contract.Observation{
		Running: false,
		Summary: summary,
	}
	record.Runtime.Result = &contract.Result{
		Outcome:     outcome,
		Summary:     summary,
		CompletedAt: o.now().UTC().Format(time.RFC3339Nano),
		Details: map[string]any{
			"record_id": record.Runtime.RecordID,
		},
	}
	record.Runtime.UpdatedAt = o.now().UTC().Format(time.RFC3339Nano)
	o.rememberCompletedLocked(cloneRuntimeRecord(record.Runtime))
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

func (o *Orchestrator) rememberCompletedLocked(record contract.IssueRuntimeRecord) {
	if strings.TrimSpace(string(record.RecordID)) == "" {
		return
	}
	next := make([]contract.IssueRuntimeRecord, 0, len(o.state.CompletedWindow)+1)
	next = append(next, cloneRuntimeRecord(record))
	for _, item := range o.state.CompletedWindow {
		if item.RecordID == record.RecordID {
			continue
		}
		next = append(next, cloneRuntimeRecord(item))
		if o.maxCompleted > 0 && len(next) >= o.maxCompleted {
			break
		}
	}
	o.state.CompletedWindow = next
}

func maxInt(left int, right int) int {
	if left > right {
		return left
	}
	return right
}
