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
	GeneratedAt          time.Time
	Service              ServiceSnapshot
	Counts               SnapshotCounts
	Running              []RunningSnapshot
	AwaitingMerge        []AwaitingMergeSnapshot
	AwaitingIntervention []AwaitingInterventionSnapshot
	Retrying             []RetrySnapshot
	Alerts               []AlertSnapshot
	CodexTotals          model.TokenTotals
	RateLimits           any
}

type ServiceSnapshot struct {
	Version   string
	StartedAt time.Time
}

type SnapshotCounts struct {
	Running              int
	AwaitingMerge        int
	AwaitingIntervention int
	Retrying             int
}

type RunningSnapshot struct {
	IssueID             string
	IssueIdentifier     string
	WorkspacePath       string
	State               string
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

type RetrySnapshot struct {
	IssueID         string
	IssueIdentifier string
	WorkspacePath   string
	Attempt         int
	DueAt           time.Time
	Error           *string
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
	IssueID         string
	IssueIdentifier string
	WorkspacePath   string
	Branch          string
	PRNumber        int
	PRURL           string
	PRState         string
	ObservedAt      time.Time
	AttemptCount    int
}

type AlertSnapshot struct {
	Code            string
	Level           string
	Message         string
	IssueID         string
	IssueIdentifier string
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
	State      PullRequestState
}

type PullRequestLookup interface {
	FindByHeadBranch(ctx context.Context, workspacePath string, headBranch string) (*PullRequestInfo, error)
}

type Orchestrator struct {
	tracker       tracker.Client
	workspace     workspace.Manager
	runner        agent.Runner
	configFn      func() *model.ServiceConfig
	workflowFn    func() *model.WorkflowDefinition
	logger        *slog.Logger
	now           func() time.Time
	randFloat     func() float64
	gitBranchFn   func(context.Context, string) (string, error)
	openPRHeadsFn func(context.Context, string) (map[string]struct{}, error)
	prLookup      PullRequestLookup

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

	mu               sync.RWMutex
	snapshot         Snapshot
	started          bool
	subscribers      map[int]chan Snapshot
	nextSubscriberID int
	systemAlerts     map[string]AlertSnapshot
	pendingCleanup   map[string]string
	completedOrder   []string
	maxCompleted     int
	startedAt        time.Time
	serviceVersion   string
}

var BuildVersion = "dev"

const defaultMaxCompletedEntries = 4096

func NewOrchestrator(trackerClient tracker.Client, workspaceManager workspace.Manager, runner agent.Runner, configFn func() *model.ServiceConfig, workflowFn func() *model.WorkflowDefinition, logger *slog.Logger) *Orchestrator {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	o := &Orchestrator{
		tracker:        trackerClient,
		workspace:      workspaceManager,
		runner:         runner,
		configFn:       configFn,
		workflowFn:     workflowFn,
		logger:         logger,
		now:            time.Now,
		randFloat:      func() float64 { return 0.5 },
		gitBranchFn:    defaultGitBranch,
		openPRHeadsFn:  defaultOpenPRHeads,
		workerResultCh: make(chan WorkerResult, 128),
		codexUpdateCh:  make(chan CodexUpdate, 1024),
		configReloadCh: make(chan struct{}, 8),
		refreshCh:      make(chan struct{}, 8),
		retryFireCh:    make(chan string, 128),
		shutdownCh:     make(chan struct{}),
		doneCh:         make(chan struct{}),
		state: model.OrchestratorState{
			Running:              map[string]*model.RunningEntry{},
			AwaitingMerge:        map[string]*model.AwaitingMergeEntry{},
			AwaitingIntervention: map[string]*model.AwaitingInterventionEntry{},
			Claimed:              map[string]struct{}{},
			RetryAttempts:        map[string]*model.RetryEntry{},
			Completed:            map[string]struct{}{},
		},
		subscribers:    map[int]chan Snapshot{},
		systemAlerts:   map[string]AlertSnapshot{},
		pendingCleanup: map[string]string{},
		maxCompleted:   defaultMaxCompletedEntries,
		startedAt:      time.Now().UTC(),
		serviceVersion: BuildVersion,
	}
	o.prLookup = ghPRLookup{}
	o.applyCurrentConfigLocked()
	o.refreshSnapshotLocked()
	return o
}

func (o *Orchestrator) Start(ctx context.Context) error {
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
				o.refreshSnapshotLocked()
				o.publishSnapshotLocked()
				o.mu.Unlock()
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

func (o *Orchestrator) RequestRefresh() {
	select {
	case o.refreshCh <- struct{}{}:
	default:
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
	o.startupCleanup(ctx)
	o.tickWithMode(ctx, allowDispatch)
}

func (o *Orchestrator) tick(ctx context.Context) {
	o.tickWithMode(ctx, true)
}

func (o *Orchestrator) tickWithMode(ctx context.Context, allowDispatch bool) {
	stateRefreshAttempted, stateRefreshSucceeded := o.reconcileRunning(ctx)
	o.reconcileAwaitingMerge(ctx)
	o.reconcileAwaitingIntervention(ctx)

	cfg := o.currentConfig()
	if err := config.ValidateForDispatch(cfg); err != nil {
		o.logger.Warn("dispatch preflight failed", "error", err.Error())
		o.mu.Lock()
		o.setSystemAlertLocked(AlertSnapshot{
			Code:    "dispatch_preflight_failed",
			Level:   "warn",
			Message: err.Error(),
		})
		o.refreshSnapshotLocked()
		o.publishSnapshotLocked()
		o.mu.Unlock()
		return
	}
	o.mu.Lock()
	o.clearSystemAlertLocked("dispatch_preflight_failed")
	o.mu.Unlock()

	candidates, err := o.tracker.FetchCandidateIssues(ctx)
	if err != nil {
		o.logger.Warn("fetch candidate issues failed", "error", err.Error())
		o.mu.Lock()
		o.setSystemAlertLocked(AlertSnapshot{
			Code:    "tracker_unreachable",
			Level:   "warn",
			Message: err.Error(),
		})
		o.refreshSnapshotLocked()
		o.publishSnapshotLocked()
		o.mu.Unlock()
		return
	}
	o.mu.Lock()
	if !stateRefreshAttempted || stateRefreshSucceeded {
		o.clearSystemAlertLocked("tracker_unreachable")
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
	workerCtx := ctx
	if o.runCtx != nil {
		workerCtx = o.runCtx
	}
	workerCtx, cancel := context.WithCancel(workerCtx)

	o.mu.Lock()
	o.state.Claimed[issue.ID] = struct{}{}
	stallCount := 0
	if existing := o.state.RetryAttempts[issue.ID]; existing != nil {
		stallCount = existing.StallCount
		if existing.TimerHandle != nil {
			existing.TimerHandle.Stop()
		}
	}
	delete(o.state.RetryAttempts, issue.ID)
	normalizedAttempt := 0
	if attempt != nil {
		normalizedAttempt = *attempt
	}
	o.state.Running[issue.ID] = &model.RunningEntry{
		Issue:         model.CloneIssue(&issue),
		Identifier:    issue.Identifier,
		WorkspacePath: "",
		RetryAttempt:  normalizedAttempt,
		StallCount:    stallCount,
		StartedAt:     o.now().UTC(),
		WorkerCancel:  cancel,
	}
	o.logger.Info(
		"dispatching issue",
		"issue_id", issue.ID,
		"issue_identifier", issue.Identifier,
		"attempt", attemptCountFromRetry(normalizedAttempt),
		"run_phase", model.PhasePreparingWorkspace.String(),
	)
	o.refreshSnapshotLocked()
	o.publishSnapshotLocked()
	o.mu.Unlock()

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
		o.mu.Lock()
		if entry := o.state.Running[issue.ID]; entry != nil {
			entry.WorkspacePath = workspaceRef.Path
			o.refreshSnapshotLocked()
			o.publishSnapshotLocked()
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

		preBranch, preOpenPRHeads, hasPRContext := o.capturePRContext(workerCtx, workspaceRef.Path)

		result.Phase = model.PhaseStreamingTurn
		runErr := o.runner.Run(workerCtx, agent.RunParams{
			Issue:          model.CloneIssue(&issue),
			Attempt:        attempt,
			WorkspacePath:  workspaceRef.Path,
			PromptTemplate: o.currentWorkflow().PromptTemplate,
			Source:         o.currentWorkflow().Source,
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
			result.Phase = model.PhaseSucceeded
			if hasPRContext {
				result.HasNewOpenPR, result.FinalBranch = o.detectNewOpenPR(workerCtx, workspaceRef.Path, preBranch, preOpenPRHeads)
			}
		}
		o.sendWorkerResult(result)
	}()
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
	o.addRuntimeLocked(entry)
	delete(o.state.Running, result.IssueID)
	identifier := entry.Identifier
	workspacePath := entry.WorkspacePath
	retryAttempt := entry.RetryAttempt
	stallCount := entry.StallCount
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

	if result.Err != nil {
		nextAttempt := retryAttempt + 1
		if nextAttempt <= 0 {
			nextAttempt = 1
		}
		errorText := result.Err.Error()
		o.scheduleRetryLocked(result.IssueID, identifier, nextAttempt, &errorText, false, stallCount)
		o.refreshSnapshotLocked()
		o.publishSnapshotLocked()
		o.mu.Unlock()
		return
	}

	o.rememberCompletedLocked(result.IssueID)
	o.refreshSnapshotLocked()
	o.publishSnapshotLocked()
	o.mu.Unlock()

	ctx := o.runtimeContext()
	cfg := o.currentConfig()
	issues, err := o.tracker.FetchIssueStatesByIDs(ctx, []string{result.IssueID})
	if err == nil && len(issues) > 0 {
		issueState = issues[0].State
		if o.isTerminalState(issues[0].State, cfg) {
			o.completeSuccessfulIssue(ctx, result.IssueID, identifier)
			return
		}
	}

	if result.HasNewOpenPR && cfg.OrchestratorAutoCloseOnPR {
		pr, lookupErr := o.lookupPullRequestByHeadBranch(ctx, workspacePath, result.FinalBranch)
		if lookupErr != nil {
			o.logger.Warn("post-run PR status lookup failed", "issue_id", result.IssueID, "issue_identifier", identifier, "branch", result.FinalBranch, "error", lookupErr.Error())
			errorText := lookupErr.Error()
			o.moveToAwaitingMerge(result.IssueID, identifier, issueState, workspacePath, result.FinalBranch, retryAttempt, stallCount, nil, &errorText)
			return
		}
		if pr == nil {
			errorText := "pull request lookup returned no match"
			o.logger.Warn("post-run PR lookup returned no match", "issue_id", result.IssueID, "issue_identifier", identifier, "branch", result.FinalBranch)
			o.moveToAwaitingMerge(result.IssueID, identifier, issueState, workspacePath, result.FinalBranch, retryAttempt, stallCount, nil, &errorText)
			return
		}
		switch pr.State {
		case PullRequestStateMerged:
			o.tryCompleteMergedPullRequest(ctx, result.IssueID, identifier, result.FinalBranch, retryAttempt, stallCount)
			return
		case PullRequestStateOpen:
			o.moveToAwaitingMerge(result.IssueID, identifier, issueState, workspacePath, result.FinalBranch, retryAttempt, stallCount, pr, nil)
			return
		case PullRequestStateClosed:
			o.moveToAwaitingIntervention(result.IssueID, identifier, workspacePath, result.FinalBranch, retryAttempt, stallCount, pr)
			return
		default:
			errorText := fmt.Sprintf("unsupported pull request state %q", pr.State)
			o.logger.Warn("post-run PR state is unsupported", "issue_id", result.IssueID, "issue_identifier", identifier, "branch", result.FinalBranch, "state", pr.State)
			o.moveToAwaitingMerge(result.IssueID, identifier, issueState, workspacePath, result.FinalBranch, retryAttempt, stallCount, pr, &errorText)
			return
		}
	}

	o.mu.Lock()
	o.scheduleRetryLocked(result.IssueID, identifier, 1, nil, true, stallCount)
	o.refreshSnapshotLocked()
	o.publishSnapshotLocked()
	o.mu.Unlock()
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
		o.applyUsageLocked(&entry.Session, event.Usage)
	}
	if event.RateLimits != nil {
		o.state.CodexRateLimits = event.RateLimits
	}
	o.refreshSnapshotLocked()
	o.publishSnapshotLocked()
}

func (o *Orchestrator) reconcileRunning(ctx context.Context) (bool, bool) {
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
		o.setSystemAlertLocked(AlertSnapshot{
			Code:    "tracker_unreachable",
			Level:   "warn",
			Message: err.Error(),
		})
		o.refreshSnapshotLocked()
		o.publishSnapshotLocked()
		o.mu.Unlock()
		return true, false
	}

	byID := make(map[string]model.Issue, len(refreshed))
	for _, issue := range refreshed {
		byID[issue.ID] = issue
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	o.clearSystemAlertLocked("tracker_unreachable")
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
	o.refreshSnapshotLocked()
	o.publishSnapshotLocked()
	return true, true
}

func (o *Orchestrator) reconcileAwaitingMerge(ctx context.Context) {
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
					delete(o.state.Claimed, issueID)
					o.refreshSnapshotLocked()
					o.publishSnapshotLocked()
				}
				o.mu.Unlock()
				continue
			default:
				o.mu.Lock()
				current := o.state.AwaitingMerge[issueID]
				if current != nil && current.State != issue.State {
					current.State = issue.State
					o.refreshSnapshotLocked()
					o.publishSnapshotLocked()
				}
				o.mu.Unlock()
			}
		}

		pr, err := o.lookupPullRequestByHeadBranch(ctx, entry.WorkspacePath, entry.Branch)
		if err != nil {
			o.logger.Warn("awaiting-merge PR lookup failed", "issue_id", issueID, "issue_identifier", entry.Identifier, "branch", entry.Branch, "error", err.Error())
			errorText := err.Error()
			o.mu.Lock()
			current := o.state.AwaitingMerge[issueID]
			if current != nil {
				current.LastError = optionalError(errorText)
				o.refreshSnapshotLocked()
				o.publishSnapshotLocked()
			}
			o.mu.Unlock()
			continue
		}
		if pr == nil {
			o.logger.Warn("awaiting-merge PR lookup returned no match", "issue_id", issueID, "issue_identifier", entry.Identifier, "branch", entry.Branch)
			o.mu.Lock()
			current := o.state.AwaitingMerge[issueID]
			if current == nil {
				o.mu.Unlock()
				continue
			}
			delete(o.state.AwaitingMerge, issueID)
			o.scheduleRetryLocked(issueID, current.Identifier, 1, nil, true, current.StallCount)
			o.refreshSnapshotLocked()
			o.publishSnapshotLocked()
			o.mu.Unlock()
			continue
		}

		switch pr.State {
		case PullRequestStateOpen:
			o.mu.Lock()
			current := o.state.AwaitingMerge[issueID]
			if current != nil {
				changed := current.PRNumber != pr.Number || current.PRURL != pr.URL || current.PRState != string(pr.State) || current.LastError != nil
				current.PRNumber = pr.Number
				current.PRURL = pr.URL
				current.PRState = string(pr.State)
				current.LastError = nil
				if changed {
					o.refreshSnapshotLocked()
					o.publishSnapshotLocked()
				}
			}
			o.mu.Unlock()
		case PullRequestStateMerged:
			o.tryCompleteMergedPullRequest(ctx, issueID, entry.Identifier, entry.Branch, entry.RetryAttempt, entry.StallCount)
		case PullRequestStateClosed:
			o.moveToAwaitingIntervention(issueID, entry.Identifier, entry.WorkspacePath, entry.Branch, entry.RetryAttempt, entry.StallCount, pr)
		default:
			errorText := fmt.Sprintf("unsupported pull request state %q", pr.State)
			o.logger.Warn("awaiting-merge PR state is unsupported", "issue_id", issueID, "issue_identifier", entry.Identifier, "branch", entry.Branch, "state", pr.State)
			o.mu.Lock()
			current := o.state.AwaitingMerge[issueID]
			if current != nil {
				current.PRNumber = pr.Number
				current.PRURL = pr.URL
				current.PRState = string(pr.State)
				current.LastError = optionalError(errorText)
				o.refreshSnapshotLocked()
				o.publishSnapshotLocked()
			}
			o.mu.Unlock()
		}
	}
}

func (o *Orchestrator) reconcileAwaitingIntervention(ctx context.Context) {
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
				delete(o.state.Claimed, issueID)
				o.refreshSnapshotLocked()
				o.publishSnapshotLocked()
			}
			o.mu.Unlock()
		}
	}
}

func (o *Orchestrator) handleRetryTimer(ctx context.Context, issueID string) {
	o.mu.Lock()
	retryEntry := o.state.RetryAttempts[issueID]
	if retryEntry == nil {
		o.mu.Unlock()
		return
	}
	delete(o.state.RetryAttempts, issueID)
	o.refreshSnapshotLocked()
	o.mu.Unlock()

	candidates, err := o.tracker.FetchCandidateIssues(ctx)
	if err != nil {
		errorText := "retry poll failed"
		o.mu.Lock()
		o.scheduleRetryLocked(issueID, retryEntry.Identifier, retryEntry.Attempt+1, &errorText, false, retryEntry.StallCount)
		o.refreshSnapshotLocked()
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
		delete(o.state.Claimed, issueID)
		o.refreshSnapshotLocked()
		o.publishSnapshotLocked()
		o.mu.Unlock()
		return
	}

	cfg := o.currentConfig()
	if !o.isDispatchEligible(*issue, cfg, true) {
		o.mu.Lock()
		delete(o.state.Claimed, issueID)
		o.refreshSnapshotLocked()
		o.publishSnapshotLocked()
		o.mu.Unlock()
		return
	}
	if !o.hasAvailableSlots(*issue, cfg) {
		errorText := "no available orchestrator slots"
		o.mu.Lock()
		o.scheduleRetryLocked(issueID, issue.Identifier, retryEntry.Attempt+1, &errorText, false, retryEntry.StallCount)
		o.refreshSnapshotLocked()
		o.mu.Unlock()
		return
	}

	attempt := retryEntry.Attempt
	o.dispatchIssue(ctx, *issue, &attempt)
}

func (o *Orchestrator) startupCleanup(ctx context.Context) {
	cfg := o.currentConfig()
	issues, err := o.tracker.FetchIssuesByStates(ctx, cfg.TerminalStates)
	if err != nil {
		o.logger.Warn("startup cleanup fetch failed", "error", err.Error())
		o.mu.Lock()
		o.setSystemAlertLocked(AlertSnapshot{
			Code:    "tracker_terminal_fetch_failed",
			Level:   "warn",
			Message: err.Error(),
		})
		o.refreshSnapshotLocked()
		o.publishSnapshotLocked()
		o.mu.Unlock()
		return
	}
	o.mu.Lock()
	o.clearSystemAlertLocked("tracker_terminal_fetch_failed")
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
		delete(o.state.Claimed, issueID)
		o.pendingCleanup[issueID] = entry.Identifier
	} else if !scheduleRetry {
		delete(o.state.Claimed, issueID)
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
		o.scheduleRetryLocked(issueID, entry.Identifier, nextAttempt, errorPtr, false, stallCount)
	}
}

func (o *Orchestrator) scheduleRetryLocked(issueID string, identifier string, attempt int, errText *string, continuation bool, stallCount int) {
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

	o.state.Claimed[issueID] = struct{}{}
	o.state.RetryAttempts[issueID] = &model.RetryEntry{
		IssueID:       issueID,
		Identifier:    identifier,
		WorkspacePath: workspacePathForIdentifier(o.currentConfig().WorkspaceRoot, identifier),
		Attempt:       attempt,
		StallCount:    stallCount,
		DueAt:         dueAt,
		TimerHandle:   timer,
		Error:         errText,
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
		row := RunningSnapshot{
			IssueID:             issueID,
			IssueIdentifier:     entry.Identifier,
			WorkspacePath:       entry.WorkspacePath,
			State:               entry.Issue.State,
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
		if entry.Session.LastCodexEvent != nil {
			row.LastEvent = *entry.Session.LastCodexEvent
		}
		row.LastEventAt = entry.Session.LastCodexTimestamp
		running = append(running, row)
	}

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
			IssueID:         issueID,
			IssueIdentifier: entry.Identifier,
			WorkspacePath:   entry.WorkspacePath,
			Branch:          entry.Branch,
			PRNumber:        entry.PRNumber,
			PRURL:           entry.PRURL,
			PRState:         entry.PRState,
			ObservedAt:      entry.ObservedAt,
			AttemptCount:    attemptCountFromRetry(entry.RetryAttempt),
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
		retrying = append(retrying, RetrySnapshot{
			IssueID:         issueID,
			IssueIdentifier: entry.Identifier,
			WorkspacePath:   entry.WorkspacePath,
			Attempt:         entry.Attempt,
			DueAt:           entry.DueAt,
			Error:           entry.Error,
		})
	}

	alerts := make([]AlertSnapshot, 0, len(o.systemAlerts)+len(o.state.RetryAttempts))
	for _, alert := range o.systemAlerts {
		alerts = append(alerts, alert)
	}
	for issueID, entry := range o.state.RetryAttempts {
		if entry.Error == nil {
			continue
		}
		errorText := *entry.Error
		lowerError := strings.ToLower(errorText)
		switch {
		case isStallErrorText(errorText) && entry.StallCount > 1:
			alerts = append(alerts, AlertSnapshot{
				Code:            "repeated_stall",
				Level:           "warn",
				Message:         errorText,
				IssueID:         issueID,
				IssueIdentifier: entry.Identifier,
			})
		case strings.Contains(lowerError, model.ErrWorkspaceHookFailed.Code), strings.Contains(lowerError, model.ErrWorkspaceHookTimeout.Code):
			alerts = append(alerts, AlertSnapshot{
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
		alerts = append(alerts, AlertSnapshot{
			Code:            "merge_status_unknown",
			Level:           "warn",
			Message:         *entry.LastError,
			IssueID:         issueID,
			IssueIdentifier: entry.Identifier,
		})
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

	totals := o.state.CodexTotals
	for _, entry := range o.state.Running {
		totals.SecondsRunning += now.Sub(entry.StartedAt).Seconds()
	}

	o.snapshot = Snapshot{
		GeneratedAt: now,
		Service: ServiceSnapshot{
			Version:   o.serviceVersion,
			StartedAt: o.startedAt,
		},
		Counts: SnapshotCounts{
			Running:              len(running),
			AwaitingMerge:        len(awaitingMerge),
			AwaitingIntervention: len(awaitingIntervention),
			Retrying:             len(retrying),
		},
		Running:              running,
		AwaitingMerge:        awaitingMerge,
		AwaitingIntervention: awaitingIntervention,
		Retrying:             retrying,
		Alerts:               alerts,
		CodexTotals:          totals,
		RateLimits:           o.state.CodexRateLimits,
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
	if _, ok := o.state.AwaitingMerge[issue.ID]; ok {
		return false
	}
	if _, ok := o.state.AwaitingIntervention[issue.ID]; ok {
		return false
	}
	if !ignoreClaim {
		if _, ok := o.state.Claimed[issue.ID]; ok {
			return false
		}
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
	}

	o.mu.Lock()
	o.state.Claimed[issueID] = struct{}{}
	o.state.AwaitingMerge[issueID] = entry
	o.refreshSnapshotLocked()
	o.publishSnapshotLocked()
	o.mu.Unlock()
}

func (o *Orchestrator) moveToAwaitingIntervention(issueID string, identifier string, workspacePath string, branch string, retryAttempt int, stallCount int, pr *PullRequestInfo) {
	entry := &model.AwaitingInterventionEntry{
		Identifier:    identifier,
		WorkspacePath: workspacePath,
		Branch:        branch,
		RetryAttempt:  retryAttempt,
		StallCount:    stallCount,
		ObservedAt:    o.now().UTC(),
	}
	if pr != nil {
		entry.PRNumber = pr.Number
		entry.PRURL = pr.URL
		entry.PRState = string(pr.State)
	}

	o.logger.Warn("issue awaiting manual intervention after PR closed", "issue_id", issueID, "issue_identifier", identifier, "branch", branch, "pr_state", entry.PRState)

	o.mu.Lock()
	delete(o.state.AwaitingMerge, issueID)
	o.state.Claimed[issueID] = struct{}{}
	o.state.AwaitingIntervention[issueID] = entry
	o.refreshSnapshotLocked()
	o.publishSnapshotLocked()
	o.mu.Unlock()
}

func (o *Orchestrator) tryCompleteMergedPullRequest(ctx context.Context, issueID string, identifier string, branch string, retryAttempt int, stallCount int) bool {
	transitioner, ok := o.tracker.(tracker.IssueTransitioner)
	errorText := ""
	if !ok {
		errorText = "tracker does not support issue transition"
		o.logger.Warn("tracker does not support issue transition", "issue_id", issueID, "issue_identifier", identifier, "branch", branch)
	} else if err := transitioner.TransitionIssue(ctx, issueID, "Done"); err != nil {
		errorText = fmt.Sprintf("post-merge transition failed: %s", err.Error())
		o.logger.Warn("post-merge transition failed", "issue_id", issueID, "issue_identifier", identifier, "branch", branch, "error", err.Error())
	} else {
		issues, err := o.tracker.FetchIssueStatesByIDs(ctx, []string{issueID})
		cfg := o.currentConfig()
		if err == nil && len(issues) > 0 && o.isTerminalState(issues[0].State, cfg) {
			o.completeSuccessfulIssue(ctx, issueID, identifier)
			return true
		}
		if err != nil {
			errorText = fmt.Sprintf("post-merge state refresh failed: %s", err.Error())
		} else {
			errorText = "post-merge transition did not reach terminal state"
		}
	}

	nextAttempt := retryAttempt + 1
	if nextAttempt <= 0 {
		nextAttempt = 1
	}
	o.mu.Lock()
	delete(o.state.AwaitingMerge, issueID)
	delete(o.state.AwaitingIntervention, issueID)
	o.scheduleRetryLocked(issueID, identifier, nextAttempt, optionalError(errorText), false, stallCount)
	o.refreshSnapshotLocked()
	o.publishSnapshotLocked()
	o.mu.Unlock()
	return false
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

func (o *Orchestrator) completeSuccessfulIssue(ctx context.Context, issueID string, identifier string) {
	if err := o.workspace.CleanupWorkspace(ctx, identifier); err != nil {
		o.logger.Warn("workspace cleanup failed", "issue_id", issueID, "identifier", identifier, "error", err.Error())
	}

	o.mu.Lock()
	delete(o.state.AwaitingMerge, issueID)
	delete(o.state.AwaitingIntervention, issueID)
	delete(o.state.Claimed, issueID)
	o.refreshSnapshotLocked()
	o.publishSnapshotLocked()
	o.mu.Unlock()
}

func (o *Orchestrator) capturePRContext(ctx context.Context, workspacePath string) (string, map[string]struct{}, bool) {
	if o.gitBranchFn == nil || o.openPRHeadsFn == nil {
		return "", nil, false
	}

	preBranch, err := o.gitBranchFn(ctx, workspacePath)
	if err != nil {
		o.logger.Warn("pre-run branch detection failed", "workspace_path", workspacePath, "error", err.Error())
		return "", nil, false
	}
	preOpenPRHeads, err := o.openPRHeadsFn(ctx, workspacePath)
	if err != nil {
		o.logger.Warn("pre-run open PR detection failed", "workspace_path", workspacePath, "error", err.Error())
		return "", nil, false
	}
	return preBranch, preOpenPRHeads, true
}

func (o *Orchestrator) detectNewOpenPR(ctx context.Context, workspacePath string, preBranch string, preOpenPRHeads map[string]struct{}) (bool, string) {
	if o.gitBranchFn == nil || o.openPRHeadsFn == nil {
		return false, ""
	}

	postBranch, err := o.gitBranchFn(ctx, workspacePath)
	if err != nil {
		o.logger.Warn("post-run branch detection failed", "workspace_path", workspacePath, "error", err.Error())
		return false, ""
	}
	postOpenPRHeads, err := o.openPRHeadsFn(ctx, workspacePath)
	if err != nil {
		o.logger.Warn("post-run open PR detection failed", "workspace_path", workspacePath, "error", err.Error())
		return false, postBranch
	}
	if strings.TrimSpace(postBranch) == "" || postBranch == preBranch {
		return false, postBranch
	}
	if _, existed := preOpenPRHeads[postBranch]; existed {
		return false, postBranch
	}
	if _, open := postOpenPRHeads[postBranch]; !open {
		return false, postBranch
	}
	return true, postBranch
}

type ghPRLookup struct{}

func (ghPRLookup) FindByHeadBranch(ctx context.Context, workspacePath string, headBranch string) (*PullRequestInfo, error) {
	stdout, stderr, err := runBashOutput(ctx, workspacePath, fmt.Sprintf("gh pr list --state all --head %s --json number,url,state,mergedAt,headRefName", bashSingleQuote(headBranch)))
	if err != nil {
		return nil, fmt.Errorf("gh pr list: %w: %s", err, strings.TrimSpace(stderr))
	}

	var payload []struct {
		Number      int     `json:"number"`
		URL         string  `json:"url"`
		State       string  `json:"state"`
		MergedAt    *string `json:"mergedAt"`
		HeadRefName string  `json:"headRefName"`
	}
	if strings.TrimSpace(stdout) == "" {
		return nil, nil
	}
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		return nil, fmt.Errorf("decode gh pr list output: %w", err)
	}

	branch := strings.TrimSpace(headBranch)
	var selected *PullRequestInfo
	for _, item := range payload {
		if strings.TrimSpace(item.HeadRefName) != branch {
			continue
		}
		state := PullRequestStateClosed
		switch {
		case item.MergedAt != nil && strings.TrimSpace(*item.MergedAt) != "":
			state = PullRequestStateMerged
		case strings.EqualFold(item.State, "open"):
			state = PullRequestStateOpen
		}
		candidate := &PullRequestInfo{
			Number:     item.Number,
			URL:        strings.TrimSpace(item.URL),
			HeadBranch: branch,
			State:      state,
		}
		if selected == nil || candidate.Number > selected.Number {
			selected = candidate
		}
	}
	return selected, nil
}

func defaultGitBranch(ctx context.Context, workspacePath string) (string, error) {
	stdout, stderr, err := runBashOutput(ctx, workspacePath, "git branch --show-current")
	if err != nil {
		return "", fmt.Errorf("git branch --show-current: %w: %s", err, strings.TrimSpace(stderr))
	}
	return strings.TrimSpace(stdout), nil
}

func defaultOpenPRHeads(ctx context.Context, workspacePath string) (map[string]struct{}, error) {
	stdout, stderr, err := runBashOutput(ctx, workspacePath, "gh pr list --state open --json headRefName")
	if err != nil {
		return nil, fmt.Errorf("gh pr list: %w: %s", err, strings.TrimSpace(stderr))
	}

	var payload []struct {
		HeadRefName string `json:"headRefName"`
	}
	if strings.TrimSpace(stdout) == "" {
		return map[string]struct{}{}, nil
	}
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		return nil, fmt.Errorf("decode gh pr list output: %w", err)
	}

	result := make(map[string]struct{}, len(payload))
	for _, item := range payload {
		branch := strings.TrimSpace(item.HeadRefName)
		if branch == "" {
			continue
		}
		result[branch] = struct{}{}
	}
	return result, nil
}

func runBashOutput(ctx context.Context, workspacePath string, script string) (string, string, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
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

func (o *Orchestrator) setSystemAlertLocked(alert AlertSnapshot) {
	if strings.TrimSpace(alert.Code) == "" {
		return
	}
	o.systemAlerts[alert.Code] = alert
}

func (o *Orchestrator) clearSystemAlertLocked(code string) {
	if strings.TrimSpace(code) == "" {
		return
	}
	delete(o.systemAlerts, code)
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
