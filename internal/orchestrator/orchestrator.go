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

type Snapshot struct {
	GeneratedAt  time.Time
	Service      ServiceSnapshot
	Counts       SnapshotCounts
	Running      []RunningSnapshot
	Recovery     RecoverySnapshot
	Health       RuntimeHealthSnapshot
	Observations RuntimeObservationsSnapshot
	CodexTotals  model.TokenTotals
	RateLimits   any
}

type ServiceSnapshot struct {
	Version          string
	StartedAt        time.Time
	Mode             model.ServiceMode
	ProtectionReason string
	ProtectedAt      *time.Time
	RestartRequired  bool
}

type SnapshotCounts struct {
	Recovering           int
	Running              int
	AwaitingMerge        int
	AwaitingIntervention int
	Retrying             int
}

type RecoverySnapshot struct {
	Recovering           []RecoveringSnapshot
	AwaitingMerge        []AwaitingMergeSnapshot
	AwaitingIntervention []AwaitingInterventionSnapshot
	Retrying             []RetrySnapshot
}

type RuntimeHealthSnapshot struct {
	Alerts        []AlertSnapshot
	Notifications []NotificationChannelHealthSnapshot
	Persistence   PersistenceHealthSnapshot
}

type RuntimeObservationsSnapshot struct {
	Derived          []ObservationSnapshot
	ProtectedResults []ProtectedResultSnapshot
}

type RunningSnapshot struct {
	IssueID             string
	IssueIdentifier     string
	WorkspacePath       string
	State               string
	DispatchKind        string
	ExpectedOutcome     string
	ContinuationReason  *string
	SessionID           string
	TurnCount           int
	LastEvent           string
	LastMessage         string
	StartedAt           time.Time
	LastEventAt         *time.Time
	InputTokens         int64
	OutputTokens        int64
	TotalTokens         int64
	CurrentRetryAttempt int
	AttemptCount        int
}

type RecoveringSnapshot struct {
	IssueID            string
	IssueIdentifier    string
	WorkspacePath      string
	State              string
	Strategy           string
	Source             string
	DispatchKind       string
	ExpectedOutcome    string
	ContinuationReason *string
	ObservedAt         time.Time
	AttemptCount       int
}

type RetrySnapshot struct {
	IssueID            string
	IssueIdentifier    string
	WorkspacePath      string
	DispatchKind       string
	ExpectedOutcome    string
	ContinuationReason *string
	Attempt            int
	DueAt              time.Time
	Error              *string
}

type AwaitingMergeSnapshot struct {
	IssueID         string
	IssueIdentifier string
	WorkspacePath   string
	State           string
	Branch          string
	PRNumber        int
	PRURL           string
	PRState         string
	AwaitingSince   time.Time
	LastError       *string
	AttemptCount    int
}

type AwaitingInterventionSnapshot struct {
	IssueID             string
	IssueIdentifier     string
	WorkspacePath       string
	Branch              string
	PRNumber            int
	PRURL               string
	PRState             string
	Reason              string
	ExpectedOutcome     string
	PreviousBranch      string
	LastKnownIssueState string
	ObservedAt          time.Time
	AttemptCount        int
}

type AlertSnapshot struct {
	Code            string
	Level           string
	Message         string
	IssueID         string
	IssueIdentifier string
}

type ObservationSnapshot struct {
	Code            string
	Level           string
	Message         string
	IssueID         string
	IssueIdentifier string
}

type ProtectedResultSnapshot struct {
	IssueID             string
	IssueIdentifier     string
	WorkspacePath       string
	Outcome             string
	Phase               string
	Error               *string
	FinalBranch         string
	ObservedAt          time.Time
	DispatchKind        string
	ExpectedOutcome     string
	ContinuationReason  *string
	CurrentRetryAttempt int
	AttemptCount        int
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

type RefreshRequestResult struct {
	Accepted        bool
	Coalesced       bool
	RequestedAt     time.Time
	Operations      []string
	RejectedCode    string
	RejectedMessage string
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

	mu                        sync.RWMutex
	snapshot                  Snapshot
	started                   bool
	subscribers               map[int]chan Snapshot
	nextSubscriberID          int
	healthAlerts              map[string]AlertSnapshot
	notificationHealth        map[string]*NotificationChannelHealthSnapshot
	persistenceHealth         PersistenceHealthSnapshot
	pendingCleanup            map[string]string
	recoveredPending          map[string]*model.RecoveryEntry
	pendingResume             map[string]uint64
	pendingLaunch             map[string]uint64
	pendingActions            map[uint64][]func()
	completedOrder            []string
	maxCompleted              int
	startedAt                 time.Time
	serviceVersion            string
	extensionsReady           bool
	eventSeq                  uint64
	notifierGeneration        uint64
	lastPersistedStateVersion uint64
	lastPersistedState        *durableRuntimeState
}

var BuildVersion = "dev"

const defaultMaxCompletedEntries = 4096
const serviceProtectedModeCode = "service_protected_mode"

func NewOrchestrator(trackerClient tracker.Client, workspaceManager workspace.Manager, runner agent.Runner, configFn func() *model.ServiceConfig, workflowFn func() *model.WorkflowDefinition, runtimeIdentityFn func() RuntimeIdentity, logger *slog.Logger) *Orchestrator {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	recovering := map[string]*model.RecoveryEntry{}
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
			Mode:                 model.ServiceModeNormal,
			Running:              map[string]*model.RunningEntry{},
			Recovering:           recovering,
			AwaitingMerge:        map[string]*model.AwaitingMergeEntry{},
			AwaitingIntervention: map[string]*model.AwaitingInterventionEntry{},
			RetryAttempts:        map[string]*model.RetryEntry{},
			ProtectedResults:     map[string]*model.ProtectedResultEntry{},
			Completed:            map[string]struct{}{},
		},
		subscribers:        map[int]chan Snapshot{},
		healthAlerts:       healthAlerts,
		notificationHealth: map[string]*NotificationChannelHealthSnapshot{},
		pendingCleanup:     map[string]string{},
		recoveredPending:   recovering,
		pendingResume:      map[string]uint64{},
		pendingLaunch:      map[string]uint64{},
		pendingActions:     map[uint64][]func(){},
		maxCompleted:       defaultMaxCompletedEntries,
		startedAt:          time.Now().UTC(),
		serviceVersion:     BuildVersion,
	}
	o.prLookup = newGitHubPRLookup()
	o.applyCurrentConfigLocked()
	o.refreshSnapshotLocked()
	return o
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
	if o.state.Mode == "" {
		return model.ServiceModeNormal
	}
	return o.state.Mode
}

func (o *Orchestrator) isProtectedLocked() bool {
	return o.serviceModeLocked() == model.ServiceModeProtected
}

func (o *Orchestrator) enterProtectedModeLocked(reason string) bool {
	if o.isProtectedLocked() {
		return false
	}
	now := o.now().UTC()
	o.state.Mode = model.ServiceModeProtected
	o.state.Protection = &model.ProtectedState{
		Reason:      reason,
		EnteredAt:   now,
		MustRestart: true,
	}
	o.setHealthAlertAndNotifyLocked(AlertSnapshot{
		Code:    serviceProtectedModeCode,
		Level:   "warn",
		Message: fmt.Sprintf("service entered protected mode: %s", reason),
	})
	return true
}

func (o *Orchestrator) rememberProtectedResultLocked(issueID string, entry *model.RunningEntry, result WorkerResult) {
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
		Identifier:    entry.Identifier,
		WorkspacePath: entry.WorkspacePath,
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
	protected := o.isProtectedLocked()
	protection := o.state.Protection
	o.mu.RUnlock()

	result := RefreshRequestResult{RequestedAt: o.now().UTC()}
	if protected {
		result.RejectedCode = serviceProtectedModeCode
		result.RejectedMessage = "service is in protected mode; restart is required after fixing session persistence"
		if protection != nil && strings.TrimSpace(protection.Reason) != "" {
			result.RejectedMessage = fmt.Sprintf("%s: %s", result.RejectedMessage, protection.Reason)
		}
		return result
	}

	result.Accepted = true
	result.Operations = []string{"poll", "reconcile"}
	select {
	case o.refreshCh <- struct{}{}:
		return result
	default:
		result.Coalesced = true
		return result
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
	if existing := o.state.RetryAttempts[issue.ID]; existing != nil {
		stallCount = existing.StallCount
		if existing.TimerHandle != nil {
			existing.TimerHandle.Stop()
		}
		dispatch = model.CloneDispatchContext(existing.Dispatch)
	}
	delete(o.state.RetryAttempts, issue.ID)
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
	o.state.Running[issue.ID] = &model.RunningEntry{
		Issue:         model.CloneIssue(&issue),
		Identifier:    issue.Identifier,
		WorkspacePath: "",
		RetryAttempt:  normalizedAttempt,
		StallCount:    stallCount,
		StartedAt:     o.now().UTC(),
		WorkerCancel:  cancel,
		Dispatch:      model.CloneDispatchContext(dispatch),
	}
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
		if entry := o.state.Running[issue.ID]; entry != nil {
			entry.WorkspacePath = workspaceRef.Path
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
	entry := o.state.Running[result.IssueID]
	if entry == nil {
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
	identifier := entry.Identifier
	workspacePath := entry.WorkspacePath
	retryAttempt := entry.RetryAttempt
	stallCount := entry.StallCount
	dispatch := model.CloneDispatchContext(entry.Dispatch)
	issueState := ""
	if entry.Issue != nil {
		issueState = entry.Issue.State
	}
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
		delete(o.state.Running, result.IssueID)
		o.rememberProtectedResultLocked(result.IssueID, entry, result)
		o.publishViewLocked()
		o.mu.Unlock()
		return
	}

	o.addRuntimeLocked(entry)
	delete(o.state.Running, result.IssueID)

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

	o.state.Recovering[result.IssueID] = &model.RecoveryEntry{
		Identifier:    identifier,
		WorkspacePath: workspacePath,
		FinalBranch:   strings.TrimSpace(result.FinalBranch),
		State:         issueState,
		RetryAttempt:  retryAttempt,
		StallCount:    stallCount,
		ObservedAt:    o.now().UTC(),
		Strategy:      model.RecoveryStrategyPostRunResume,
		Source:        model.RecoverySourceSucceeded,
		Dispatch:      model.CloneDispatchContext(dispatch),
	}
	version := o.commitStateLocked(true)
	if version > 0 {
		o.pendingResume[result.IssueID] = version
	}
	o.mu.Unlock()

	if version == 0 {
		ctx := o.runtimeContext()
		o.reconcileRecovering(ctx)
	}
}

func (o *Orchestrator) handleCodexUpdate(update CodexUpdate) {
	o.mu.Lock()
	defer o.mu.Unlock()

	entry := o.state.Running[update.IssueID]
	if entry == nil {
		return
	}
	event := update.Event
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
		if o.isProtectedLocked() {
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
	if event.RateLimits != nil {
		o.state.CodexRateLimits = event.RateLimits
	}
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
	for issueID, entry := range o.state.Running {
		stallTimeout := cfg.CodexStallTimeoutMS
		if stallTimeout <= 0 {
			continue
		}
		lastSeen := entry.StartedAt
		if entry.Session.LastCodexTimestamp != nil {
			lastSeen = *entry.Session.LastCodexTimestamp
		}
		if o.now().Sub(lastSeen) > time.Duration(stallTimeout)*time.Millisecond {
			o.terminateRunningLocked(ctx, issueID, false, true, "stalled session")
		}
	}
	ids := make([]string, 0, len(o.state.Running))
	for issueID := range o.state.Running {
		ids = append(ids, issueID)
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
	for issueID, entry := range o.state.Running {
		issue, ok := byID[issueID]
		if !ok {
			continue
		}
		if o.isTerminalState(issue.State, cfg) {
			o.terminateRunningLocked(ctx, issueID, true, false, "")
			continue
		}
		if o.isActiveState(issue.State, cfg) {
			entry.Issue = model.CloneIssue(&issue)
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
	pending := make(map[string]model.AwaitingMergeEntry, len(o.state.AwaitingMerge))
	for issueID, entry := range o.state.AwaitingMerge {
		if entry == nil {
			continue
		}
		pending[issueID] = *entry
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
				o.completeSuccessfulIssue(ctx, issueID, entry.Identifier)
				continue
			case !o.isActiveState(issue.State, cfg):
				o.mu.Lock()
				current := o.state.AwaitingMerge[issueID]
				if current != nil {
					delete(o.state.AwaitingMerge, issueID)
					o.commitStateLocked(true)
				}
				o.mu.Unlock()
				continue
			default:
				o.mu.Lock()
				current := o.state.AwaitingMerge[issueID]
				if current != nil && current.State != issue.State {
					current.State = issue.State
					o.commitStateLocked(true)
				}
				o.mu.Unlock()
			}
		}

		pr, err := o.lookupAwaitingMergePullRequest(ctx, entry.WorkspacePath, entry)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			o.logger.Warn("awaiting-merge PR lookup failed", "issue_id", issueID, "issue_identifier", entry.Identifier, "branch", entry.Branch, "error", err.Error())
			errorText := err.Error()
			o.mu.Lock()
			current := o.state.AwaitingMerge[issueID]
			if current != nil {
				current.LastError = optionalError(errorText)
				o.commitStateLocked(true)
			}
			o.mu.Unlock()
			continue
		}
		if pr == nil {
			o.logger.Warn("awaiting-merge PR lookup returned no match", "issue_id", issueID, "issue_identifier", entry.Identifier, "branch", entry.Branch)
			o.moveToAwaitingIntervention(issueID, entry.Identifier, entry.WorkspacePath, entry.Branch, entry.RetryAttempt, entry.StallCount, model.CompletionModePullRequest, string(model.ContinuationReasonMissingPR), entry.State, nil)
			continue
		}

		switch pr.State {
		case PullRequestStateOpen:
			o.mu.Lock()
			current := o.state.AwaitingMerge[issueID]
			if current != nil {
				changed := current.PRNumber != pr.Number ||
					current.PRURL != pr.URL ||
					current.PRState != string(pr.State) ||
					current.PRBaseOwner != pr.BaseOwner ||
					current.PRBaseRepo != pr.BaseRepo ||
					current.PRHeadOwner != pr.HeadOwner ||
					current.LastError != nil ||
					current.PostMergeRetryCount != 0 ||
					current.NextPostMergeRetryAt != nil
				current.PRNumber = pr.Number
				current.PRURL = pr.URL
				current.PRState = string(pr.State)
				current.PRBaseOwner = pr.BaseOwner
				current.PRBaseRepo = pr.BaseRepo
				current.PRHeadOwner = pr.HeadOwner
				current.LastError = nil
				current.PostMergeRetryCount = 0
				current.NextPostMergeRetryAt = nil
				if changed {
					o.commitStateLocked(true)
				}
			}
			o.mu.Unlock()
		case PullRequestStateMerged:
			if entry.NextPostMergeRetryAt != nil && entry.NextPostMergeRetryAt.After(o.now().UTC()) {
				continue
			}
			o.tryCompleteMergedPullRequest(ctx, issueID, entry.Identifier, entry.WorkspacePath, entry.Branch, entry.RetryAttempt, entry.StallCount, entry.State, pr)
		case PullRequestStateClosed:
			o.moveToAwaitingIntervention(issueID, entry.Identifier, entry.WorkspacePath, entry.Branch, entry.RetryAttempt, entry.StallCount, model.CompletionModePullRequest, string(model.ContinuationReasonClosedUnmergedPR), entry.State, pr)
		default:
			errorText := fmt.Sprintf("unsupported pull request state %q", pr.State)
			o.logger.Warn("awaiting-merge PR state is unsupported", "issue_id", issueID, "issue_identifier", entry.Identifier, "branch", entry.Branch, "state", pr.State)
			o.mu.Lock()
			current := o.state.AwaitingMerge[issueID]
			if current != nil {
				current.PRNumber = pr.Number
				current.PRURL = pr.URL
				current.PRState = string(pr.State)
				current.PRBaseOwner = pr.BaseOwner
				current.PRBaseRepo = pr.BaseRepo
				current.PRHeadOwner = pr.HeadOwner
				current.LastError = optionalError(errorText)
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
	pending := make(map[string]model.AwaitingInterventionEntry, len(o.state.AwaitingIntervention))
	for issueID, entry := range o.state.AwaitingIntervention {
		if entry == nil {
			continue
		}
		pending[issueID] = *entry
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
			current := o.state.AwaitingIntervention[issueID]
			if current != nil && current.Reason != string(model.ContinuationReasonTrackerIssueMissing) {
				current.Reason = string(model.ContinuationReasonTrackerIssueMissing)
				o.commitStateLocked(true)
			}
			o.mu.Unlock()
			continue
		}
		switch {
		case o.isTerminalState(issue.State, cfg):
			o.completeSuccessfulIssue(ctx, issueID, entry.Identifier)
		case !o.isActiveState(issue.State, cfg):
			o.mu.Lock()
			current := o.state.AwaitingIntervention[issueID]
			if current != nil {
				delete(o.state.AwaitingIntervention, issueID)
				o.commitStateLocked(true)
			}
			o.mu.Unlock()
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
	due := make([]dueRetry, 0, len(o.state.RetryAttempts))
	for issueID, entry := range o.state.RetryAttempts {
		if entry == nil || entry.DueAt.After(now) {
			continue
		}
		if entry.TimerHandle != nil {
			entry.TimerHandle.Stop()
			entry.TimerHandle = nil
		}
		due = append(due, dueRetry{
			IssueID:    issueID,
			Identifier: entry.Identifier,
			DueAt:      entry.DueAt,
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
	retryEntry := o.state.RetryAttempts[issueID]
	if retryEntry == nil || retryEntry.DueAt.After(now) {
		o.mu.Unlock()
		return
	}
	if retryEntry.TimerHandle != nil {
		retryEntry.TimerHandle.Stop()
		retryEntry.TimerHandle = nil
	}
	snapshot := *retryEntry
	snapshot.Error = optionalError(pointerString(retryEntry.Error))
	snapshot.Dispatch = model.CloneDispatchContext(retryEntry.Dispatch)
	o.mu.Unlock()

	candidates, err := o.tracker.FetchCandidateIssues(ctx)
	if err != nil {
		errorText := "retry poll failed"
		o.mu.Lock()
		if o.isProtectedLocked() {
			o.mu.Unlock()
			return
		}
		current := o.state.RetryAttempts[issueID]
		if current != nil && !current.DueAt.After(o.now().UTC()) {
			o.scheduleRetryLocked(issueID, current.Identifier, current.Attempt+1, &errorText, false, current.StallCount, current.Dispatch)
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
		current := o.state.RetryAttempts[issueID]
		if current != nil && !current.DueAt.After(o.now().UTC()) {
			delete(o.state.RetryAttempts, issueID)
			o.commitStateLocked(true)
		}
		o.mu.Unlock()
		return
	}

	cfg := o.currentConfig()
	if !o.isDispatchEligible(*issue, cfg, true) {
		o.mu.Lock()
		if o.isProtectedLocked() {
			o.mu.Unlock()
			return
		}
		current := o.state.RetryAttempts[issueID]
		if current != nil && !current.DueAt.After(o.now().UTC()) {
			delete(o.state.RetryAttempts, issueID)
			o.commitStateLocked(true)
		}
		o.mu.Unlock()
		return
	}
	if !o.hasAvailableSlots(*issue, cfg) {
		errorText := "no available orchestrator slots"
		o.mu.Lock()
		if o.isProtectedLocked() {
			o.mu.Unlock()
			return
		}
		current := o.state.RetryAttempts[issueID]
		if current != nil && !current.DueAt.After(o.now().UTC()) {
			o.scheduleRetryLocked(issueID, issue.Identifier, current.Attempt+1, &errorText, false, current.StallCount, current.Dispatch)
			o.commitStateLocked(true)
		}
		o.mu.Unlock()
		return
	}

	attempt := snapshot.Attempt
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
	for _, retryEntry := range o.state.RetryAttempts {
		if retryEntry.TimerHandle != nil {
			retryEntry.TimerHandle.Stop()
		}
	}
	for _, entry := range o.state.Running {
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
	entry := o.state.Running[issueID]
	if entry == nil {
		return
	}
	o.addRuntimeLocked(entry)
	if entry.WorkerCancel != nil {
		entry.WorkerCancel()
	}
	delete(o.state.Running, issueID)

	if cleanup {
		o.pendingCleanup[issueID] = entry.Identifier
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
		o.scheduleRetryLocked(issueID, entry.Identifier, nextAttempt, errorPtr, false, stallCount, entry.Dispatch)
	}
}

func (o *Orchestrator) scheduleRetryLocked(issueID string, identifier string, attempt int, errText *string, continuation bool, stallCount int, dispatch *model.DispatchContext) {
	if existing := o.state.RetryAttempts[issueID]; existing != nil && existing.TimerHandle != nil {
		existing.TimerHandle.Stop()
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
	o.state.RetryAttempts[issueID] = &model.RetryEntry{
		IssueID:       issueID,
		Identifier:    identifier,
		WorkspacePath: workspacePathForIdentifier(o.currentConfig().WorkspaceRoot, identifier),
		Attempt:       attempt,
		StallCount:    stallCount,
		DueAt:         dueAt,
		TimerHandle:   timer,
		Error:         errText,
		Dispatch:      retryDispatch,
	}
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

func (o *Orchestrator) addRuntimeLocked(entry *model.RunningEntry) {
	if entry == nil {
		return
	}
	o.state.CodexTotals.SecondsRunning += o.now().Sub(entry.StartedAt).Seconds()
}

func (o *Orchestrator) applyCurrentConfigLocked() {
	cfg := o.currentConfig()
	o.state.PollIntervalMS = cfg.PollIntervalMS
	o.state.MaxConcurrentAgents = cfg.MaxConcurrentAgents
}

func (o *Orchestrator) refreshSnapshotLocked() {
	now := o.now().UTC()
	running := make([]RunningSnapshot, 0, len(o.state.Running))
	for issueID, entry := range o.state.Running {
		if _, pendingLaunch := o.pendingLaunch[issueID]; pendingLaunch {
			continue
		}
		row := RunningSnapshot{
			IssueID:             issueID,
			IssueIdentifier:     entry.Identifier,
			WorkspacePath:       entry.WorkspacePath,
			SessionID:           entry.Session.SessionID,
			TurnCount:           entry.Session.TurnCount,
			LastMessage:         entry.Session.LastCodexMessage,
			StartedAt:           entry.StartedAt,
			InputTokens:         entry.Session.CodexInputTokens,
			OutputTokens:        entry.Session.CodexOutputTokens,
			TotalTokens:         entry.Session.CodexTotalTokens,
			CurrentRetryAttempt: entry.RetryAttempt,
			AttemptCount:        attemptCountFromRetry(entry.RetryAttempt),
		}
		if entry.Issue != nil {
			row.State = entry.Issue.State
		}
		if entry.Dispatch != nil {
			row.DispatchKind = string(entry.Dispatch.Kind)
			row.ExpectedOutcome = string(entry.Dispatch.ExpectedOutcome)
			if entry.Dispatch.Reason != nil {
				reason := string(*entry.Dispatch.Reason)
				row.ContinuationReason = &reason
			}
		}
		if entry.Session.LastCodexEvent != nil {
			row.LastEvent = *entry.Session.LastCodexEvent
		}
		row.LastEventAt = entry.Session.LastCodexTimestamp
		running = append(running, row)
	}

	recovering := make([]RecoveringSnapshot, 0, len(o.state.Recovering))
	for issueID, entry := range o.state.Recovering {
		row := RecoveringSnapshot{
			IssueID:         issueID,
			IssueIdentifier: entry.Identifier,
			WorkspacePath:   entry.WorkspacePath,
			State:           entry.State,
			Strategy:        string(entry.Strategy),
			Source:          string(entry.Source),
			ObservedAt:      entry.ObservedAt,
			AttemptCount:    attemptCountFromRetry(entry.RetryAttempt),
		}
		if entry.Dispatch != nil {
			row.DispatchKind = string(entry.Dispatch.Kind)
			row.ExpectedOutcome = string(entry.Dispatch.ExpectedOutcome)
			if entry.Dispatch.Reason != nil {
				reason := string(*entry.Dispatch.Reason)
				row.ContinuationReason = &reason
			}
		}
		recovering = append(recovering, row)
	}
	sort.SliceStable(recovering, func(i int, j int) bool {
		if recovering[i].IssueIdentifier != recovering[j].IssueIdentifier {
			return recovering[i].IssueIdentifier < recovering[j].IssueIdentifier
		}
		return recovering[i].IssueID < recovering[j].IssueID
	})

	awaitingMerge := make([]AwaitingMergeSnapshot, 0, len(o.state.AwaitingMerge))
	for issueID, entry := range o.state.AwaitingMerge {
		awaitingMerge = append(awaitingMerge, AwaitingMergeSnapshot{
			IssueID:         issueID,
			IssueIdentifier: entry.Identifier,
			WorkspacePath:   entry.WorkspacePath,
			State:           entry.State,
			Branch:          entry.Branch,
			PRNumber:        entry.PRNumber,
			PRURL:           entry.PRURL,
			PRState:         entry.PRState,
			AwaitingSince:   entry.AwaitingSince,
			LastError:       entry.LastError,
			AttemptCount:    attemptCountFromRetry(entry.RetryAttempt),
		})
	}
	sort.SliceStable(awaitingMerge, func(i int, j int) bool {
		if awaitingMerge[i].IssueIdentifier != awaitingMerge[j].IssueIdentifier {
			return awaitingMerge[i].IssueIdentifier < awaitingMerge[j].IssueIdentifier
		}
		return awaitingMerge[i].IssueID < awaitingMerge[j].IssueID
	})

	awaitingIntervention := make([]AwaitingInterventionSnapshot, 0, len(o.state.AwaitingIntervention))
	for issueID, entry := range o.state.AwaitingIntervention {
		awaitingIntervention = append(awaitingIntervention, AwaitingInterventionSnapshot{
			IssueID:             issueID,
			IssueIdentifier:     entry.Identifier,
			WorkspacePath:       entry.WorkspacePath,
			Branch:              entry.Branch,
			PRNumber:            entry.PRNumber,
			PRURL:               entry.PRURL,
			PRState:             entry.PRState,
			Reason:              entry.Reason,
			ExpectedOutcome:     entry.ExpectedOutcome,
			PreviousBranch:      entry.PreviousBranch,
			LastKnownIssueState: entry.LastKnownIssueState,
			ObservedAt:          entry.ObservedAt,
			AttemptCount:        attemptCountFromRetry(entry.RetryAttempt),
		})
	}
	sort.SliceStable(awaitingIntervention, func(i int, j int) bool {
		if awaitingIntervention[i].IssueIdentifier != awaitingIntervention[j].IssueIdentifier {
			return awaitingIntervention[i].IssueIdentifier < awaitingIntervention[j].IssueIdentifier
		}
		return awaitingIntervention[i].IssueID < awaitingIntervention[j].IssueID
	})

	retrying := make([]RetrySnapshot, 0, len(o.state.RetryAttempts))
	for issueID, entry := range o.state.RetryAttempts {
		row := RetrySnapshot{
			IssueID:         issueID,
			IssueIdentifier: entry.Identifier,
			WorkspacePath:   entry.WorkspacePath,
			Attempt:         entry.Attempt,
			DueAt:           entry.DueAt,
			Error:           entry.Error,
		}
		if entry.Dispatch != nil {
			row.DispatchKind = string(entry.Dispatch.Kind)
			row.ExpectedOutcome = string(entry.Dispatch.ExpectedOutcome)
			if entry.Dispatch.Reason != nil {
				reason := string(*entry.Dispatch.Reason)
				row.ContinuationReason = &reason
			}
		}
		retrying = append(retrying, row)
	}
	sort.SliceStable(retrying, func(i int, j int) bool {
		if retrying[i].IssueIdentifier != retrying[j].IssueIdentifier {
			return retrying[i].IssueIdentifier < retrying[j].IssueIdentifier
		}
		return retrying[i].IssueID < retrying[j].IssueID
	})

	var protectedPersistedState *durableRuntimeState
	if o.isProtectedLocked() && o.lastPersistedState != nil {
		persisted := cloneDurableRuntimeState(*o.lastPersistedState)
		protectedPersistedState = &persisted
		recovering = make([]RecoveringSnapshot, 0, len(persisted.Recovering))
		for _, item := range persisted.Recovering {
			row := RecoveringSnapshot{
				IssueID:         item.IssueID,
				IssueIdentifier: item.Identifier,
				WorkspacePath:   item.WorkspacePath,
				State:           item.State,
				Strategy:        item.Strategy,
				Source:          item.Source,
				ObservedAt:      item.ObservedAt,
				AttemptCount:    attemptCountFromRetry(item.RetryAttempt),
			}
			dispatch := durableDispatchToModel(item.Dispatch)
			if dispatch != nil {
				row.DispatchKind = string(dispatch.Kind)
				row.ExpectedOutcome = string(dispatch.ExpectedOutcome)
				if dispatch.Reason != nil {
					reason := string(*dispatch.Reason)
					row.ContinuationReason = &reason
				}
			}
			recovering = append(recovering, row)
		}
		awaitingMerge = make([]AwaitingMergeSnapshot, 0, len(persisted.AwaitingMerge))
		for _, item := range persisted.AwaitingMerge {
			awaitingMerge = append(awaitingMerge, AwaitingMergeSnapshot{
				IssueID:         item.IssueID,
				IssueIdentifier: item.Identifier,
				WorkspacePath:   item.WorkspacePath,
				State:           item.State,
				Branch:          item.Branch,
				PRNumber:        item.PRNumber,
				PRURL:           item.PRURL,
				PRState:         item.PRState,
				AwaitingSince:   item.AwaitingSince,
				LastError:       optionalError(pointerString(item.LastError)),
				AttemptCount:    attemptCountFromRetry(item.RetryAttempt),
			})
		}
		awaitingIntervention = make([]AwaitingInterventionSnapshot, 0, len(persisted.AwaitingIntervention))
		for _, item := range persisted.AwaitingIntervention {
			awaitingIntervention = append(awaitingIntervention, AwaitingInterventionSnapshot{
				IssueID:             item.IssueID,
				IssueIdentifier:     item.Identifier,
				WorkspacePath:       item.WorkspacePath,
				Branch:              item.Branch,
				PRNumber:            item.PRNumber,
				PRURL:               item.PRURL,
				PRState:             item.PRState,
				Reason:              item.Reason,
				ExpectedOutcome:     item.ExpectedOutcome,
				PreviousBranch:      item.PreviousBranch,
				LastKnownIssueState: item.LastKnownIssueState,
				ObservedAt:          item.ObservedAt,
				AttemptCount:        attemptCountFromRetry(item.RetryAttempt),
			})
		}
		retrying = make([]RetrySnapshot, 0, len(persisted.Retrying))
		for _, item := range persisted.Retrying {
			row := RetrySnapshot{
				IssueID:         item.IssueID,
				IssueIdentifier: item.Identifier,
				WorkspacePath:   item.WorkspacePath,
				Attempt:         item.Attempt,
				DueAt:           item.DueAt,
				Error:           optionalError(pointerString(item.Error)),
			}
			dispatch := durableDispatchToModel(item.Dispatch)
			if dispatch != nil {
				row.DispatchKind = string(dispatch.Kind)
				row.ExpectedOutcome = string(dispatch.ExpectedOutcome)
				if dispatch.Reason != nil {
					reason := string(*dispatch.Reason)
					row.ContinuationReason = &reason
				}
			}
			retrying = append(retrying, row)
		}
	}

	alerts := make([]AlertSnapshot, 0, len(o.healthAlerts))
	for _, alert := range o.healthAlerts {
		alerts = append(alerts, alert)
	}
	sort.SliceStable(alerts, func(i int, j int) bool {
		if alerts[i].Code != alerts[j].Code {
			return alerts[i].Code < alerts[j].Code
		}
		if alerts[i].IssueIdentifier != alerts[j].IssueIdentifier {
			return alerts[i].IssueIdentifier < alerts[j].IssueIdentifier
		}
		return alerts[i].Message < alerts[j].Message
	})

	derived := make([]ObservationSnapshot, 0, len(o.state.RetryAttempts)+len(o.state.AwaitingMerge))
	if protectedPersistedState != nil {
		for _, entry := range protectedPersistedState.Retrying {
			if entry.Error == nil {
				continue
			}
			errorText := pointerString(entry.Error)
			lowerError := strings.ToLower(errorText)
			switch {
			case isStallErrorText(errorText) && entry.StallCount > 1:
				derived = append(derived, ObservationSnapshot{
					Code:            "repeated_stall",
					Level:           "warn",
					Message:         errorText,
					IssueID:         entry.IssueID,
					IssueIdentifier: entry.Identifier,
				})
			case strings.Contains(lowerError, model.ErrWorkspaceHookFailed.Code), strings.Contains(lowerError, model.ErrWorkspaceHookTimeout.Code):
				derived = append(derived, ObservationSnapshot{
					Code:            "workspace_hook_failure",
					Level:           "warn",
					Message:         errorText,
					IssueID:         entry.IssueID,
					IssueIdentifier: entry.Identifier,
				})
			}
		}
		for _, entry := range protectedPersistedState.AwaitingMerge {
			if entry.LastError == nil {
				continue
			}
			derived = append(derived, ObservationSnapshot{
				Code:            "merge_status_unknown",
				Level:           "warn",
				Message:         pointerString(entry.LastError),
				IssueID:         entry.IssueID,
				IssueIdentifier: entry.Identifier,
			})
		}
	} else {
		for issueID, entry := range o.state.RetryAttempts {
			if entry.Error == nil {
				continue
			}
			errorText := *entry.Error
			lowerError := strings.ToLower(errorText)
			switch {
			case isStallErrorText(errorText) && entry.StallCount > 1:
				derived = append(derived, ObservationSnapshot{
					Code:            "repeated_stall",
					Level:           "warn",
					Message:         errorText,
					IssueID:         issueID,
					IssueIdentifier: entry.Identifier,
				})
			case strings.Contains(lowerError, model.ErrWorkspaceHookFailed.Code), strings.Contains(lowerError, model.ErrWorkspaceHookTimeout.Code):
				derived = append(derived, ObservationSnapshot{
					Code:            "workspace_hook_failure",
					Level:           "warn",
					Message:         errorText,
					IssueID:         issueID,
					IssueIdentifier: entry.Identifier,
				})
			}
		}
		for issueID, entry := range o.state.AwaitingMerge {
			if entry.LastError == nil {
				continue
			}
			derived = append(derived, ObservationSnapshot{
				Code:            "merge_status_unknown",
				Level:           "warn",
				Message:         *entry.LastError,
				IssueID:         issueID,
				IssueIdentifier: entry.Identifier,
			})
		}
	}
	sort.SliceStable(derived, func(i int, j int) bool {
		if derived[i].Code != derived[j].Code {
			return derived[i].Code < derived[j].Code
		}
		if derived[i].IssueIdentifier != derived[j].IssueIdentifier {
			return derived[i].IssueIdentifier < derived[j].IssueIdentifier
		}
		return derived[i].Message < derived[j].Message
	})

	protectedResults := make([]ProtectedResultSnapshot, 0, len(o.state.ProtectedResults))
	for issueID, entry := range o.state.ProtectedResults {
		if entry == nil {
			continue
		}
		row := ProtectedResultSnapshot{
			IssueID:             issueID,
			IssueIdentifier:     entry.Identifier,
			WorkspacePath:       entry.WorkspacePath,
			Outcome:             string(entry.Outcome),
			Phase:               entry.Phase.String(),
			Error:               optionalError(pointerString(entry.Error)),
			FinalBranch:         entry.FinalBranch,
			ObservedAt:          entry.ObservedAt,
			CurrentRetryAttempt: entry.RetryAttempt,
			AttemptCount:        attemptCountFromRetry(entry.RetryAttempt),
		}
		if entry.Dispatch != nil {
			row.DispatchKind = string(entry.Dispatch.Kind)
			row.ExpectedOutcome = string(entry.Dispatch.ExpectedOutcome)
			if entry.Dispatch.Reason != nil {
				reason := string(*entry.Dispatch.Reason)
				row.ContinuationReason = &reason
			}
		}
		protectedResults = append(protectedResults, row)
	}
	sort.SliceStable(protectedResults, func(i int, j int) bool {
		if protectedResults[i].IssueIdentifier != protectedResults[j].IssueIdentifier {
			return protectedResults[i].IssueIdentifier < protectedResults[j].IssueIdentifier
		}
		return protectedResults[i].IssueID < protectedResults[j].IssueID
	})

	notificationHealth := make([]NotificationChannelHealthSnapshot, 0, len(o.notificationHealth))
	for _, status := range o.notificationHealth {
		if status == nil {
			continue
		}
		copyStatus := *status
		copyStatus.LastAttemptAt = cloneTimePtr(status.LastAttemptAt)
		copyStatus.LastSuccessAt = cloneTimePtr(status.LastSuccessAt)
		copyStatus.LastError = optionalError(pointerString(status.LastError))
		notificationHealth = append(notificationHealth, copyStatus)
	}
	sort.SliceStable(notificationHealth, func(i int, j int) bool {
		return notificationHealth[i].ChannelID < notificationHealth[j].ChannelID
	})

	persistenceHealth := o.persistenceHealth
	persistenceHealth.LastAttemptAt = cloneTimePtr(o.persistenceHealth.LastAttemptAt)
	persistenceHealth.LastSuccessAt = cloneTimePtr(o.persistenceHealth.LastSuccessAt)
	persistenceHealth.LastError = optionalError(pointerString(o.persistenceHealth.LastError))

	mode := o.serviceModeLocked()
	protectionReason := ""
	protectedAt := (*time.Time)(nil)
	restartRequired := false
	if o.state.Protection != nil {
		protectionReason = o.state.Protection.Reason
		protectedAt = cloneTimePtr(&o.state.Protection.EnteredAt)
		restartRequired = o.state.Protection.MustRestart
	}

	totals := o.state.CodexTotals
	if protectedPersistedState != nil {
		totals = protectedPersistedState.TokenTotal
	} else {
		for _, entry := range o.state.Running {
			totals.SecondsRunning += now.Sub(entry.StartedAt).Seconds()
		}
	}

	o.snapshot = Snapshot{
		GeneratedAt: now,
		Service: ServiceSnapshot{
			Version:          o.serviceVersion,
			StartedAt:        o.startedAt,
			Mode:             mode,
			ProtectionReason: protectionReason,
			ProtectedAt:      protectedAt,
			RestartRequired:  restartRequired,
		},
		Counts: SnapshotCounts{
			Recovering:           len(recovering),
			Running:              len(running),
			AwaitingMerge:        len(awaitingMerge),
			AwaitingIntervention: len(awaitingIntervention),
			Retrying:             len(retrying),
		},
		Running: running,
		Recovery: RecoverySnapshot{
			Recovering:           recovering,
			AwaitingMerge:        awaitingMerge,
			AwaitingIntervention: awaitingIntervention,
			Retrying:             retrying,
		},
		Health: RuntimeHealthSnapshot{
			Alerts:        alerts,
			Notifications: notificationHealth,
			Persistence:   persistenceHealth,
		},
		Observations: RuntimeObservationsSnapshot{
			Derived:          derived,
			ProtectedResults: protectedResults,
		},
		CodexTotals: totals,
		RateLimits:  o.state.CodexRateLimits,
	}
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
	if _, ok := o.state.Running[issue.ID]; ok {
		return false
	}
	if _, ok := o.state.Recovering[issue.ID]; ok {
		return false
	}
	if _, ok := o.state.AwaitingMerge[issue.ID]; ok {
		return false
	}
	if _, ok := o.state.AwaitingIntervention[issue.ID]; ok {
		return false
	}
	if _, ok := o.state.RetryAttempts[issue.ID]; ok && !ignoreClaim {
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

	if cfg.MaxConcurrentAgents <= len(o.state.Running) {
		return false
	}
	normalized := model.NormalizeState(issue.State)
	limit, ok := cfg.MaxConcurrentAgentsByState[normalized]
	if !ok {
		return true
	}
	count := 0
	for _, entry := range o.state.Running {
		if model.NormalizeState(entry.Issue.State) == normalized {
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
	var errorCopy *string
	if lastError != nil {
		errorCopy = optionalError(*lastError)
	}
	entry := &model.AwaitingMergeEntry{
		Identifier:    identifier,
		State:         issueState,
		WorkspacePath: workspacePath,
		Branch:        branch,
		RetryAttempt:  retryAttempt,
		StallCount:    stallCount,
		AwaitingSince: o.now().UTC(),
		LastError:     errorCopy,
	}
	if pr != nil {
		entry.PRNumber = pr.Number
		entry.PRURL = pr.URL
		entry.PRState = string(pr.State)
		entry.PRBaseOwner = pr.BaseOwner
		entry.PRBaseRepo = pr.BaseRepo
		entry.PRHeadOwner = pr.HeadOwner
	}

	o.mu.Lock()
	if o.isProtectedLocked() {
		o.mu.Unlock()
		return
	}
	delete(o.pendingResume, issueID)
	delete(o.state.Recovering, issueID)
	o.state.AwaitingMerge[issueID] = entry
	o.commitStateLocked(true)
	o.mu.Unlock()
}

func (o *Orchestrator) moveToAwaitingIntervention(issueID string, identifier string, workspacePath string, branch string, retryAttempt int, stallCount int, expectedOutcome model.CompletionMode, reason string, issueState string, pr *PullRequestInfo) {
	entry := &model.AwaitingInterventionEntry{
		Identifier:          identifier,
		WorkspacePath:       workspacePath,
		Branch:              branch,
		RetryAttempt:        retryAttempt,
		StallCount:          stallCount,
		ObservedAt:          o.now().UTC(),
		Reason:              reason,
		ExpectedOutcome:     string(expectedOutcome),
		PreviousBranch:      branch,
		LastKnownIssueState: issueState,
	}
	if pr != nil {
		entry.PRNumber = pr.Number
		entry.PRURL = pr.URL
		entry.PRState = string(pr.State)
		entry.PRBaseOwner = pr.BaseOwner
		entry.PRBaseRepo = pr.BaseRepo
		entry.PRHeadOwner = pr.HeadOwner
	}

	o.logger.Warn("issue awaiting manual intervention", "issue_id", issueID, "issue_identifier", identifier, "branch", branch, "pr_state", entry.PRState, "reason", reason)

	o.mu.Lock()
	if o.isProtectedLocked() {
		o.mu.Unlock()
		return
	}
	delete(o.pendingResume, issueID)
	delete(o.state.Recovering, issueID)
	delete(o.state.AwaitingMerge, issueID)
	o.state.AwaitingIntervention[issueID] = entry
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
	current := o.state.AwaitingMerge[issueID]
	awaitingSince := o.now().UTC()
	postMergeRetryCount := 0
	if current == nil {
		current = &model.AwaitingMergeEntry{}
		o.state.AwaitingMerge[issueID] = current
	} else {
		if !current.AwaitingSince.IsZero() {
			awaitingSince = current.AwaitingSince
		}
		postMergeRetryCount = current.PostMergeRetryCount
	}
	postMergeRetryCount++
	if postMergeRetryCount > maxPostMergeCloseRetries {
		o.mu.Unlock()
		o.moveToAwaitingIntervention(issueID, identifier, workspacePath, branch, retryAttempt, stallCount, model.CompletionModePullRequest, "post_merge_transition_failed", issueState, pr)
		return false
	}

	nextRetryAt := o.now().UTC().Add(postMergeRetryDelay(postMergeRetryCount, o.currentConfig().MaxRetryBackoffMS))
	current.Identifier = identifier
	current.State = issueState
	current.WorkspacePath = workspacePath
	current.Branch = branch
	current.RetryAttempt = retryAttempt
	current.StallCount = stallCount
	current.AwaitingSince = awaitingSince
	current.LastError = optionalError(errorText)
	current.PostMergeRetryCount = postMergeRetryCount
	current.NextPostMergeRetryAt = &nextRetryAt
	if pr != nil {
		current.PRNumber = pr.Number
		current.PRURL = pr.URL
		current.PRState = string(pr.State)
		current.PRBaseOwner = pr.BaseOwner
		current.PRBaseRepo = pr.BaseRepo
		current.PRHeadOwner = pr.HeadOwner
	}
	delete(o.pendingResume, issueID)
	delete(o.state.Recovering, issueID)
	delete(o.state.AwaitingIntervention, issueID)
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

func (o *Orchestrator) tryCompleteMergedPullRequest(ctx context.Context, issueID string, identifier string, workspacePath string, branch string, retryAttempt int, stallCount int, issueState string, pr *PullRequestInfo) bool {
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

func pullRequestInfoFromAwaitingMerge(entry model.AwaitingMergeEntry) *PullRequestInfo {
	if entry.PRNumber <= 0 && strings.TrimSpace(entry.Branch) == "" {
		return nil
	}
	return &PullRequestInfo{
		Number:     entry.PRNumber,
		URL:        entry.PRURL,
		HeadBranch: entry.Branch,
		HeadOwner:  entry.PRHeadOwner,
		BaseOwner:  entry.PRBaseOwner,
		BaseRepo:   entry.PRBaseRepo,
		State:      PullRequestState(entry.PRState),
	}
}

func (o *Orchestrator) lookupAwaitingMergePullRequest(ctx context.Context, workspacePath string, entry model.AwaitingMergeEntry) (*PullRequestInfo, error) {
	if o.prLookup == nil {
		return nil, errors.New("pull request lookup is not configured")
	}
	return o.prLookup.Refresh(ctx, workspacePath, pullRequestInfoFromAwaitingMerge(entry))
}

func (o *Orchestrator) completeSuccessfulIssue(ctx context.Context, issueID string, identifier string) {
	o.mu.Lock()
	if o.isProtectedLocked() {
		o.mu.Unlock()
		return
	}
	delete(o.pendingResume, issueID)
	delete(o.state.Recovering, issueID)
	delete(o.state.AwaitingMerge, issueID)
	delete(o.state.AwaitingIntervention, issueID)
	o.rememberCompletedLocked(issueID)
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

func (o *Orchestrator) rememberCompletedLocked(issueID string) {
	if strings.TrimSpace(issueID) == "" {
		return
	}
	if _, exists := o.state.Completed[issueID]; exists {
		return
	}
	o.state.Completed[issueID] = struct{}{}
	if o.maxCompleted <= 0 {
		return
	}
	o.completedOrder = append(o.completedOrder, issueID)
	for len(o.completedOrder) > o.maxCompleted {
		evicted := o.completedOrder[0]
		o.completedOrder = o.completedOrder[1:]
		delete(o.state.Completed, evicted)
	}
}

func maxInt(left int, right int) int {
	if left > right {
		return left
	}
	return right
}
