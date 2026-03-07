package orchestrator

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"symphony-go/internal/agent"
	"symphony-go/internal/config"
	"symphony-go/internal/model"
	"symphony-go/internal/tracker"
	"symphony-go/internal/workspace"
)

type WorkerResult struct {
	IssueID    string
	Identifier string
	Attempt    *int
	StartedAt  time.Time
	Phase      model.RunPhase
	Err        error
}

type CodexUpdate struct {
	IssueID string
	Event   agent.AgentEvent
}

type Snapshot struct {
	GeneratedAt time.Time
	Counts      SnapshotCounts
	Running     []RunningSnapshot
	Retrying    []RetrySnapshot
	CodexTotals model.TokenTotals
	RateLimits  any
}

type SnapshotCounts struct {
	Running  int
	Retrying int
}

type RunningSnapshot struct {
	IssueID             string
	IssueIdentifier     string
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
}

type RetrySnapshot struct {
	IssueID         string
	IssueIdentifier string
	Attempt         int
	DueAt           time.Time
	Error           *string
}

type Orchestrator struct {
	tracker    tracker.Client
	workspace  workspace.Manager
	runner     agent.Runner
	configFn   func() *model.ServiceConfig
	workflowFn func() *model.WorkflowDefinition
	logger     *slog.Logger
	now        func() time.Time
	randFloat  func() float64

	tickTimer      *time.Timer
	workerResultCh chan WorkerResult
	codexUpdateCh  chan CodexUpdate
	configReloadCh chan *model.WorkflowDefinition
	refreshCh      chan struct{}
	retryFireCh    chan string
	shutdownCh     chan struct{}
	doneCh         chan struct{}

	runCtx       context.Context
	workerWG     sync.WaitGroup
	shutdownOnce sync.Once

	state model.OrchestratorState

	mu       sync.RWMutex
	snapshot Snapshot
	started  bool
}

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
		workerResultCh: make(chan WorkerResult, 128),
		codexUpdateCh:  make(chan CodexUpdate, 256),
		configReloadCh: make(chan *model.WorkflowDefinition, 8),
		refreshCh:      make(chan struct{}, 8),
		retryFireCh:    make(chan string, 128),
		shutdownCh:     make(chan struct{}),
		doneCh:         make(chan struct{}),
		state: model.OrchestratorState{
			Running:       map[string]*model.RunningEntry{},
			Claimed:       map[string]struct{}{},
			RetryAttempts: map[string]*model.RetryEntry{},
			Completed:     map[string]struct{}{},
		},
	}
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

func (o *Orchestrator) NotifyWorkflowReload(def *model.WorkflowDefinition) {
	select {
	case o.configReloadCh <- def:
	default:
	}
}

func (o *Orchestrator) Snapshot() Snapshot {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.refreshSnapshotLocked()
	return o.snapshot
}

func (o *Orchestrator) RunOnce(ctx context.Context, allowDispatch bool) {
	o.startupCleanup(ctx)
	o.tickWithMode(ctx, allowDispatch)
}

func (o *Orchestrator) tick(ctx context.Context) {
	o.tickWithMode(ctx, true)
}

func (o *Orchestrator) tickWithMode(ctx context.Context, allowDispatch bool) {
	o.reconcileRunning(ctx)

	cfg := o.currentConfig()
	if err := config.ValidateForDispatch(cfg); err != nil {
		o.logger.Warn("dispatch preflight failed", "error", err.Error())
		return
	}

	candidates, err := o.tracker.FetchCandidateIssues(ctx)
	if err != nil {
		o.logger.Warn("fetch candidate issues failed", "error", err.Error())
		return
	}
	sort.SliceStable(candidates, func(i int, j int) bool {
		return compareIssues(candidates[i], candidates[j])
	})
	if !allowDispatch {
		o.mu.Lock()
		o.refreshSnapshotLocked()
		o.mu.Unlock()
		return
	}
	o.dispatchEligibleIssues(ctx, candidates)
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
	if existing := o.state.RetryAttempts[issue.ID]; existing != nil && existing.TimerHandle != nil {
		existing.TimerHandle.Stop()
	}
	delete(o.state.RetryAttempts, issue.ID)
	normalizedAttempt := 0
	if attempt != nil {
		normalizedAttempt = *attempt
	}
	o.state.Running[issue.ID] = &model.RunningEntry{
		Issue:        cloneIssue(&issue),
		Identifier:   issue.Identifier,
		RetryAttempt: normalizedAttempt,
		StartedAt:    o.now().UTC(),
		WorkerCancel: cancel,
	}
	o.refreshSnapshotLocked()
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
		if preparer, ok := o.workspace.(interface {
			PrepareForRun(context.Context, *model.Workspace) error
		}); ok {
			if err := preparer.PrepareForRun(workerCtx, workspaceRef); err != nil {
				result.Err = err
				o.sendWorkerResult(result)
				return
			}
		}

		result.Phase = model.PhaseStreamingTurn
		runErr := o.runner.Run(workerCtx, agent.RunParams{
			Issue:          cloneIssue(&issue),
			Attempt:        attempt,
			WorkspacePath:  workspaceRef.Path,
			PromptTemplate: o.currentWorkflow().PromptTemplate,
			MaxTurns:       o.currentConfig().MaxTurns,
			RefetchIssue: func(ctx context.Context, issueID string) (*model.Issue, error) {
				issues, err := o.tracker.FetchIssueStatesByIDs(ctx, []string{issueID})
				if err != nil {
					return nil, err
				}
				if len(issues) == 0 {
					return nil, nil
				}
				return cloneIssue(&issues[0]), nil
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
		}
		o.sendWorkerResult(result)
	}()
}

func (o *Orchestrator) handleWorkerExit(result WorkerResult) {
	o.mu.Lock()
	defer o.mu.Unlock()

	entry := o.state.Running[result.IssueID]
	if entry == nil {
		return
	}
	o.addRuntimeLocked(entry)
	delete(o.state.Running, result.IssueID)

	if result.Err == nil {
		o.state.Completed[result.IssueID] = struct{}{}
		o.scheduleRetryLocked(result.IssueID, entry.Identifier, 1, nil, true)
		o.refreshSnapshotLocked()
		return
	}

	nextAttempt := entry.RetryAttempt + 1
	if nextAttempt <= 0 {
		nextAttempt = 1
	}
	errorText := result.Err.Error()
	o.scheduleRetryLocked(result.IssueID, entry.Identifier, nextAttempt, &errorText, false)
	o.refreshSnapshotLocked()
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
	if event.TurnID != nil {
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
}

func (o *Orchestrator) reconcileRunning(ctx context.Context) {
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
		return
	}

	refreshed, err := o.tracker.FetchIssueStatesByIDs(ctx, ids)
	if err != nil {
		o.logger.Warn("reconcile state refresh failed", "error", err.Error())
		return
	}

	byID := make(map[string]model.Issue, len(refreshed))
	for _, issue := range refreshed {
		byID[issue.ID] = issue
	}

	o.mu.Lock()
	defer o.mu.Unlock()
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
			entry.Issue = cloneIssue(&issue)
			continue
		}
		o.terminateRunningLocked(ctx, issueID, false, false, "")
	}
	o.refreshSnapshotLocked()
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
		o.scheduleRetryLocked(issueID, retryEntry.Identifier, retryEntry.Attempt+1, &errorText, false)
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
		o.mu.Unlock()
		return
	}

	cfg := o.currentConfig()
	if !o.isDispatchEligible(*issue, cfg, true) {
		o.mu.Lock()
		delete(o.state.Claimed, issueID)
		o.refreshSnapshotLocked()
		o.mu.Unlock()
		return
	}
	if !o.hasAvailableSlots(*issue, cfg) {
		errorText := "no available orchestrator slots"
		o.mu.Lock()
		o.scheduleRetryLocked(issueID, issue.Identifier, retryEntry.Attempt+1, &errorText, false)
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
		return
	}
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
		identifier := entry.Identifier
		_ = o.workspace.CleanupWorkspace(ctx, identifier)
	} else if !scheduleRetry {
		delete(o.state.Claimed, issueID)
	}

	if scheduleRetry {
		nextAttempt := entry.RetryAttempt + 1
		if nextAttempt <= 0 {
			nextAttempt = 1
		}
		errorPtr := optionalError(errText)
		o.scheduleRetryLocked(issueID, entry.Identifier, nextAttempt, errorPtr, false)
	}
}

func (o *Orchestrator) scheduleRetryLocked(issueID string, identifier string, attempt int, errText *string, continuation bool) {
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
		IssueID:     issueID,
		Identifier:  identifier,
		Attempt:     attempt,
		DueAt:       dueAt,
		TimerHandle: timer,
		Error:       errText,
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
			State:               entry.Issue.State,
			SessionID:           entry.Session.SessionID,
			TurnCount:           entry.Session.TurnCount,
			LastMessage:         entry.Session.LastCodexMessage,
			StartedAt:           entry.StartedAt,
			InputTokens:         entry.Session.CodexInputTokens,
			OutputTokens:        entry.Session.CodexOutputTokens,
			TotalTokens:         entry.Session.CodexTotalTokens,
			CurrentRetryAttempt: entry.RetryAttempt,
		}
		if entry.Session.LastCodexEvent != nil {
			row.LastEvent = *entry.Session.LastCodexEvent
		}
		row.LastEventAt = entry.Session.LastCodexTimestamp
		running = append(running, row)
	}

	retrying := make([]RetrySnapshot, 0, len(o.state.RetryAttempts))
	for issueID, entry := range o.state.RetryAttempts {
		retrying = append(retrying, RetrySnapshot{
			IssueID:         issueID,
			IssueIdentifier: entry.Identifier,
			Attempt:         entry.Attempt,
			DueAt:           entry.DueAt,
			Error:           entry.Error,
		})
	}

	totals := o.state.CodexTotals
	for _, entry := range o.state.Running {
		totals.SecondsRunning += now.Sub(entry.StartedAt).Seconds()
	}

	o.snapshot = Snapshot{
		GeneratedAt: now,
		Counts: SnapshotCounts{
			Running:  len(running),
			Retrying: len(retrying),
		},
		Running:     running,
		Retrying:    retrying,
		CodexTotals: totals,
		RateLimits:  o.state.CodexRateLimits,
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

func cloneIssue(issue *model.Issue) *model.Issue {
	if issue == nil {
		return nil
	}
	copyValue := *issue
	copyValue.Labels = append([]string(nil), issue.Labels...)
	copyValue.BlockedBy = append([]model.BlockerRef(nil), issue.BlockedBy...)
	return &copyValue
}

func optionalError(value string) *string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	copyValue := value
	return &copyValue
}

func maxInt(left int, right int) int {
	if left > right {
		return left
	}
	return right
}
