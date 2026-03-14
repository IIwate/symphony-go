package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"symphony-go/internal/model"
	"symphony-go/internal/model/contract"
)

const (
	durableStateVersion             = 5
	sessionPersistenceWriteFailCode = "session_persistence_write_failed"
	stateStoreDrainTimeout          = 5 * time.Second
	initialStateStoreRetryDelay     = time.Second
	maxStateStoreRetryDelay         = 30 * time.Second
)

type LedgerStore interface {
	Load() (*durableRuntimeState, error)
	Schedule(state durableRuntimeState, critical bool) uint64
	Disable()
	Close(ctx context.Context) error
}

type stateStore = LedgerStore

type durableRuntimeState struct {
	Version  int                          `json:"version"`
	Identity RuntimeIdentity              `json:"identity"`
	SavedAt  time.Time                    `json:"saved_at"`
	Service  durableServiceMetadata       `json:"service"`
	Records  []contract.IssueLedgerRecord `json:"records"`
}

type durablePRContext struct {
	Number     int    `json:"number,omitempty"`
	URL        string `json:"url,omitempty"`
	State      string `json:"state,omitempty"`
	Merged     bool   `json:"merged,omitempty"`
	HeadBranch string `json:"head_branch,omitempty"`
}

type durableDispatchContext struct {
	Kind               string            `json:"kind,omitempty"`
	RetryAttempt       *int              `json:"retry_attempt,omitempty"`
	ExpectedOutcome    string            `json:"expected_outcome,omitempty"`
	OnMissingPR        string            `json:"on_missing_pr,omitempty"`
	OnClosedPR         string            `json:"on_closed_pr,omitempty"`
	Reason             *string           `json:"reason,omitempty"`
	PreviousBranch     *string           `json:"previous_branch,omitempty"`
	PreviousPR         *durablePRContext `json:"previous_pr,omitempty"`
	PreviousIssueState *string           `json:"previous_issue_state,omitempty"`
}

type durableServiceMetadata struct {
	TokenTotal     model.TokenTotals                `json:"token_total"`
	RecordMetadata map[string]durableRecordMetadata `json:"record_metadata,omitempty"`
}

type durableRecordMetadata struct {
	RetryAttempt        int                     `json:"retry_attempt,omitempty"`
	StallCount          int                     `json:"stall_count,omitempty"`
	LastKnownIssueState string                  `json:"last_known_issue_state,omitempty"`
	NeedsRecovery       bool                    `json:"needs_recovery,omitempty"`
	Dispatch            *durableDispatchContext `json:"dispatch,omitempty"`
}

type scheduledDurableState struct {
	version uint64
	force   bool
	state   durableRuntimeState
}

type closeRequest struct {
	ctx    context.Context
	result chan error
}

type fileStateStore struct {
	logger    *slog.Logger
	config    model.SessionPersistenceConfig
	identity  RuntimeIdentity
	onSuccess func(uint64, durableRuntimeState)
	onFailure func(error)

	mu          sync.Mutex
	disabled    bool
	desired     *scheduledDurableState
	nextVersion uint64
	signalCh    chan struct{}
	closeCh     chan closeRequest
}

func newFileStateStore(cfg model.SessionPersistenceConfig, identity RuntimeIdentity, logger *slog.Logger, onSuccess func(uint64, durableRuntimeState), onFailure func(error)) stateStore {
	if logger == nil {
		logger = slog.Default()
	}
	resolvedCfg := cfg
	resolvedCfg.File.Path = resolveStateStorePath(cfg.File.Path, identity.Descriptor.ConfigRoot)
	store := &fileStateStore{
		logger:    logger,
		config:    resolvedCfg,
		identity:  normalizeRuntimeIdentity(identity),
		onSuccess: onSuccess,
		onFailure: onFailure,
		signalCh:  make(chan struct{}, 1),
		closeCh:   make(chan closeRequest),
	}
	go store.loop()
	return store
}

func (s *fileStateStore) Load() (*durableRuntimeState, error) {
	raw, err := os.ReadFile(s.config.File.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	var state durableRuntimeState
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, err
	}
	if state.Version != durableStateVersion {
		return nil, fmt.Errorf("unsupported session state version %d", state.Version)
	}
	if !sameRuntimeCompatibility(state.Identity.Compatibility, s.identity.Compatibility) {
		return nil, fmt.Errorf("session state identity does not match current runtime")
	}
	return &state, nil
}

func (s *fileStateStore) Schedule(state durableRuntimeState, critical bool) uint64 {
	s.mu.Lock()
	if s.disabled {
		s.mu.Unlock()
		return 0
	}
	s.nextVersion++
	version := s.nextVersion
	force := critical && s.config.File.FsyncOnCritical
	if s.desired != nil && s.desired.force {
		force = true
	}
	s.desired = &scheduledDurableState{
		version: version,
		force:   force,
		state:   cloneDurableRuntimeState(state),
	}
	s.mu.Unlock()

	select {
	case s.signalCh <- struct{}{}:
	default:
	}
	return version
}

func (s *fileStateStore) Disable() {
	s.mu.Lock()
	s.disabled = true
	s.desired = nil
	s.mu.Unlock()

	select {
	case s.signalCh <- struct{}{}:
	default:
	}
}

func (s *fileStateStore) Close(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	request := closeRequest{
		ctx:    ctx,
		result: make(chan error, 1),
	}
	select {
	case s.closeCh <- request:
	case <-ctx.Done():
		return ctx.Err()
	}
	return <-request.result
}

func (s *fileStateStore) loop() {
	var flushTimer *time.Timer
	var flushTimerCh <-chan time.Time
	var retryTimer *time.Timer
	var retryTimerCh <-chan time.Time
	retryDelay := initialStateStoreRetryDelay

	stopTimer := func(timer **time.Timer, timerCh *<-chan time.Time) {
		if *timer == nil {
			*timerCh = nil
			return
		}
		if !(*timer).Stop() {
			select {
			case <-(*timer).C:
			default:
			}
		}
		*timerCh = nil
	}

	resetTimer := func(timer **time.Timer, timerCh *<-chan time.Time, delay time.Duration) {
		if delay < 0 {
			delay = 0
		}
		if *timer == nil {
			*timer = time.NewTimer(delay)
		} else {
			if !(*timer).Stop() {
				select {
				case <-(*timer).C:
				default:
				}
			}
			(*timer).Reset(delay)
		}
		*timerCh = (*timer).C
	}

	handleFlushResult := func(pending *scheduledDurableState, force bool) {
		if pending == nil {
			return
		}
		hasMore, nextForce, err := s.flushPending(pending, force)
		if err != nil {
			if retryDelay < initialStateStoreRetryDelay {
				retryDelay = initialStateStoreRetryDelay
			}
			resetTimer(&retryTimer, &retryTimerCh, retryDelay)
			retryDelay *= 2
			if retryDelay > maxStateStoreRetryDelay {
				retryDelay = maxStateStoreRetryDelay
			}
			return
		}

		retryDelay = initialStateStoreRetryDelay
		if !hasMore {
			return
		}
		if nextForce {
			select {
			case s.signalCh <- struct{}{}:
			default:
			}
			return
		}
		delay := time.Duration(maxInt(s.config.File.FlushIntervalMS, 1)) * time.Millisecond
		resetTimer(&flushTimer, &flushTimerCh, delay)
	}

	for {
		select {
		case <-s.signalCh:
			stopTimer(&retryTimer, &retryTimerCh)
			pending := s.snapshotDesired()
			if pending == nil {
				continue
			}
			if pending.force {
				stopTimer(&flushTimer, &flushTimerCh)
				handleFlushResult(pending, true)
				continue
			}
			delay := time.Duration(maxInt(s.config.File.FlushIntervalMS, 1)) * time.Millisecond
			resetTimer(&flushTimer, &flushTimerCh, delay)
		case <-flushTimerCh:
			stopTimer(&flushTimer, &flushTimerCh)
			handleFlushResult(s.snapshotDesired(), false)
		case <-retryTimerCh:
			stopTimer(&retryTimer, &retryTimerCh)
			pending := s.snapshotDesired()
			if pending == nil {
				retryDelay = initialStateStoreRetryDelay
				continue
			}
			handleFlushResult(pending, pending.force)
		case request := <-s.closeCh:
			stopTimer(&flushTimer, &flushTimerCh)
			stopTimer(&retryTimer, &retryTimerCh)
			request.result <- s.drain(request.ctx)
			return
		}
	}
}

func (s *fileStateStore) drain(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	retryDelay := initialStateStoreRetryDelay
	for {
		pending := s.snapshotDesired()
		if pending == nil {
			return nil
		}
		_, _, err := s.flushPending(pending, true)
		if err == nil {
			retryDelay = initialStateStoreRetryDelay
			continue
		}

		select {
		case <-time.After(retryDelay):
			retryDelay *= 2
			if retryDelay > maxStateStoreRetryDelay {
				retryDelay = maxStateStoreRetryDelay
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (s *fileStateStore) snapshotDesired() *scheduledDurableState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.disabled || s.desired == nil {
		return nil
	}
	copyState := cloneDurableRuntimeState(s.desired.state)
	return &scheduledDurableState{
		version: s.desired.version,
		force:   s.desired.force,
		state:   copyState,
	}
}

func (s *fileStateStore) flushPending(pending *scheduledDurableState, force bool) (bool, bool, error) {
	if pending == nil {
		return false, false, nil
	}

	if err := writeDurableRuntimeState(s.config.File.Path, pending.state, force); err != nil {
		if s.onFailure != nil {
			s.onFailure(err)
		}
		return false, false, err
	}

	hasMore, nextForce := s.markFlushed(pending.version)
	if s.onSuccess != nil {
		s.onSuccess(pending.version, cloneDurableRuntimeState(pending.state))
	}
	return hasMore, nextForce, nil
}

func (s *fileStateStore) markFlushed(version uint64) (bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.desired == nil {
		return false, false
	}
	if s.desired.version == version {
		s.desired = nil
		return false, false
	}
	return true, s.desired.force
}

func writeDurableRuntimeState(path string, state durableRuntimeState, force bool) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')

	tempFile, err := os.CreateTemp(dir, "runtime-ledger-*.tmp")
	if err != nil {
		return err
	}
	tempPath := tempFile.Name()
	defer func() {
		_ = os.Remove(tempPath)
	}()

	if _, err := tempFile.Write(raw); err != nil {
		_ = tempFile.Close()
		return err
	}
	if force {
		if err := tempFile.Sync(); err != nil {
			_ = tempFile.Close()
			return err
		}
	}
	if err := tempFile.Close(); err != nil {
		return err
	}
	return replaceDurableStateFileAtomically(tempPath, path)
}

func normalizeRuntimeIdentity(identity RuntimeIdentity) RuntimeIdentity {
	identity.Compatibility.Profile = strings.TrimSpace(identity.Compatibility.Profile)
	identity.Compatibility.ActiveSource = strings.TrimSpace(identity.Compatibility.ActiveSource)
	identity.Compatibility.SourceKind = model.NormalizeState(identity.Compatibility.SourceKind)
	identity.Compatibility.FlowName = strings.TrimSpace(identity.Compatibility.FlowName)
	identity.Compatibility.TrackerKind = model.NormalizeState(identity.Compatibility.TrackerKind)
	identity.Compatibility.TrackerRepo = strings.TrimSpace(identity.Compatibility.TrackerRepo)
	identity.Compatibility.TrackerProjectSlug = strings.TrimSpace(identity.Compatibility.TrackerProjectSlug)
	identity.Descriptor.ConfigRoot = normalizeRuntimePath(identity.Descriptor.ConfigRoot)
	identity.Descriptor.WorkspaceRoot = normalizeRuntimePath(identity.Descriptor.WorkspaceRoot)
	identity.Descriptor.SessionPersistenceKind = model.NormalizeState(identity.Descriptor.SessionPersistenceKind)
	identity.Descriptor.SessionStatePath = normalizeRuntimePath(resolveStateStorePath(identity.Descriptor.SessionStatePath, identity.Descriptor.ConfigRoot))
	return identity
}

func sameRuntimeCompatibility(left RuntimeCompatibility, right RuntimeCompatibility) bool {
	left = normalizeRuntimeIdentity(RuntimeIdentity{Compatibility: left}).Compatibility
	right = normalizeRuntimeIdentity(RuntimeIdentity{Compatibility: right}).Compatibility
	return left == right
}

func normalizeRuntimePath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		absPath = filepath.Clean(path)
	}
	return strings.ToLower(filepath.Clean(absPath))
}

func resolveStateStorePath(path string, root string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	path = filepath.Clean(path)
	if filepath.IsAbs(path) {
		return path
	}
	root = strings.TrimSpace(root)
	if root == "" {
		return path
	}
	return filepath.Join(root, path)
}

func (o *Orchestrator) ensureRuntimeExtensions() error {
	o.mu.Lock()
	if o.extensionsReady {
		o.mu.Unlock()
		return nil
	}
	oldNotifier := o.reloadNotifierLocked()
	cfg := o.currentConfig()
	identity := o.currentRuntimeIdentity()
	o.mu.Unlock()
	o.closeNotifier(oldNotifier)

	var store stateStore
	var restoredState *durableRuntimeState
	var err error
	if cfg.SessionPersistence.Enabled {
		switch cfg.SessionPersistence.Kind {
		case "", model.SessionPersistenceKindFile:
			store = newFileStateStore(cfg.SessionPersistence, identity, o.logger, o.reportSessionPersistenceWriteSuccess, o.reportSessionPersistenceWriteFailure)
		default:
			return fmt.Errorf("unsupported session persistence kind %q", cfg.SessionPersistence.Kind)
		}
		restoredState, err = store.Load()
		if err != nil {
			o.mu.Lock()
			activeNotifier := o.notifier
			o.notifier = nil
			o.mu.Unlock()
			o.closeNotifier(activeNotifier)
			o.closeStateStore(store)
			return fmt.Errorf("session persistence state at %s is incompatible or unreadable; delete the file and restart: %w", resolveStateStorePath(cfg.SessionPersistence.File.Path, identity.Descriptor.ConfigRoot), err)
		}
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	if o.extensionsReady {
		o.closeStateStore(store)
		return nil
	}
	o.stateStore = store
	o.persistenceHealth.Enabled = cfg.SessionPersistence.Enabled
	o.persistenceHealth.Kind = string(cfg.SessionPersistence.Kind)
	if restoredState != nil {
		copyState := cloneDurableRuntimeState(*restoredState)
		o.lastPersistedState = &copyState
		o.restorePersistedStateLocked(restoredState)
	} else {
		emptyState := o.buildPersistedStateLocked()
		o.lastPersistedState = &emptyState
	}
	o.extensionsReady = true
	o.refreshSnapshotLocked()
	return nil
}

func (o *Orchestrator) closeStateStore(store stateStore) {
	if store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), stateStoreDrainTimeout)
	defer cancel()
	if err := store.Close(ctx); err != nil {
		o.logger.Warn("session persistence drain timed out", "error", err.Error())
	}
}

func (o *Orchestrator) currentRuntimeIdentity() RuntimeIdentity {
	if o.runtimeIdentityFn == nil {
		cfg := o.currentConfig()
		return normalizeRuntimeIdentity(RuntimeIdentity{
			Compatibility: RuntimeCompatibility{
				SourceKind:         cfg.TrackerKind,
				TrackerKind:        cfg.TrackerKind,
				TrackerRepo:        cfg.TrackerRepo,
				TrackerProjectSlug: cfg.TrackerProjectSlug,
			},
			Descriptor: RuntimeDescriptor{
				ConfigRoot:             cfg.AutomationRootDir,
				WorkspaceRoot:          cfg.WorkspaceRoot,
				SessionPersistenceKind: string(cfg.SessionPersistence.Kind),
				SessionStatePath:       cfg.SessionPersistence.File.Path,
			},
		})
	}
	return normalizeRuntimeIdentity(o.runtimeIdentityFn())
}

func (o *Orchestrator) scheduleStatePersistLocked(critical bool) uint64 {
	if o.stateStore == nil {
		return 0
	}
	return o.stateStore.Schedule(o.buildPersistedStateLocked(), critical)
}

func (o *Orchestrator) commitStateLocked(critical bool) uint64 {
	o.refreshSnapshotLocked()
	o.publishSnapshotLocked()
	if o.isProtectedLocked() {
		return 0
	}
	return o.scheduleStatePersistLocked(critical)
}

func (o *Orchestrator) publishViewLocked() {
	o.refreshSnapshotLocked()
	o.publishSnapshotLocked()
}

func (o *Orchestrator) buildPersistedStateLocked() durableRuntimeState {
	state := durableRuntimeState{
		Version:  durableStateVersion,
		Identity: o.currentRuntimeIdentity(),
		SavedAt:  o.now().UTC(),
		Service: durableServiceMetadata{
			TokenTotal:     o.state.CodexTotals,
			RecordMetadata: map[string]durableRecordMetadata{},
		},
	}
	for _, record := range o.state.Records {
		if record == nil {
			continue
		}
		ledger := runtimeRecordToLedger(record.Runtime, record.RetryDueAt)
		state.Records = append(state.Records, ledger)
		state.Service.RecordMetadata[string(ledger.RecordID)] = durableRecordMetadata{
			RetryAttempt:        record.RetryAttempt,
			StallCount:          record.StallCount,
			LastKnownIssueState: record.LastKnownIssueState,
			NeedsRecovery:       record.NeedsRecovery || isRecordRunning(record),
			Dispatch:            durableDispatchFromModel(record.Dispatch),
		}
	}
	for _, record := range o.state.CompletedWindow {
		state.Records = append(state.Records, runtimeRecordToLedger(record, nil))
	}
	sort.SliceStable(state.Records, func(i int, j int) bool {
		if state.Records[i].SourceRef.SourceIdentifier != state.Records[j].SourceRef.SourceIdentifier {
			return state.Records[i].SourceRef.SourceIdentifier < state.Records[j].SourceRef.SourceIdentifier
		}
		return state.Records[i].RecordID < state.Records[j].RecordID
	})
	return state
}

func runtimeRecordToLedger(record contract.IssueRuntimeRecord, retryDueAt *time.Time) contract.IssueLedgerRecord {
	ledger := contract.IssueLedgerRecord{
		RecordID:    record.RecordID,
		SourceRef:   record.SourceRef,
		Status:      record.Status,
		Reason:      cloneReason(record.Reason),
		DurableRefs: cloneDurableRefs(record.DurableRefs),
		Result:      cloneResult(record.Result),
		UpdatedAt:   record.UpdatedAt,
	}
	if retryDueAt != nil {
		value := retryDueAt.UTC().Format(time.RFC3339Nano)
		ledger.RetryDueAt = &value
	}
	return ledger
}

func ledgerRecordToRuntime(record contract.IssueLedgerRecord) contract.IssueRuntimeRecord {
	return contract.IssueRuntimeRecord{
		RecordID:    record.RecordID,
		SourceRef:   record.SourceRef,
		Status:      record.Status,
		UpdatedAt:   record.UpdatedAt,
		Reason:      cloneReason(record.Reason),
		DurableRefs: cloneDurableRefs(record.DurableRefs),
		Result:      cloneResult(record.Result),
	}
}

func (o *Orchestrator) restorePersistedStateLocked(state *durableRuntimeState) {
	if state == nil {
		return
	}

	o.state.CodexTotals = state.Service.TokenTotal
	o.state.Records = map[string]*model.IssueRecord{}
	o.runningRecords = map[string]*model.IssueRecord{}
	o.retryRecords = map[string]*model.IssueRecord{}
	o.awaitingMergeRecords = map[string]*model.IssueRecord{}
	o.awaitingInterventionRecords = map[string]*model.IssueRecord{}
	o.state.CompletedWindow = nil

	for _, item := range state.Records {
		if item.Status == contract.IssueStatusCompleted {
			o.rememberCompletedLocked(ledgerRecordToRuntime(item))
			continue
		}
		issueID := strings.TrimSpace(item.SourceRef.SourceID)
		if issueID == "" {
			continue
		}
		record := &model.IssueRecord{
			Runtime: ledgerRecordToRuntime(item),
		}
		if meta, ok := state.Service.RecordMetadata[string(item.RecordID)]; ok {
			record.RetryAttempt = meta.RetryAttempt
			record.StallCount = meta.StallCount
			record.LastKnownIssueState = meta.LastKnownIssueState
			record.NeedsRecovery = meta.NeedsRecovery
			record.Dispatch = durableDispatchToModel(meta.Dispatch)
		}
		if item.RetryDueAt != nil {
			if dueAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(*item.RetryDueAt)); err == nil {
				record.RetryDueAt = &dueAt
			}
		}
		if record.Runtime.Status == contract.IssueStatusActive {
			record.Runtime.Observation = &contract.Observation{
				Running: false,
				Summary: "restored from ledger; awaiting recovery evaluation",
			}
		}
		if record.Runtime.Status == contract.IssueStatusRetryScheduled && record.RetryDueAt != nil {
			record.RetryTimer = o.newRetryTimer(issueID, *record.RetryDueAt)
		}
		o.state.Records[issueID] = record
		o.reindexRecordLocked(issueID, record)
	}
}

func (o *Orchestrator) reconcileRecovering(ctx context.Context) {
	o.mu.RLock()
	protected := o.isProtectedLocked()
	pending := make(map[string]*model.IssueRecord)
	for issueID, record := range o.state.Records {
		if record == nil || record.Runtime.Status != contract.IssueStatusActive || !record.NeedsRecovery {
			continue
		}
		if _, waiting := o.pendingRecovery[issueID]; waiting {
			continue
		}
		pending[issueID] = cloneIssueRecord(record)
	}
	o.mu.RUnlock()
	if protected || len(pending) == 0 {
		o.mu.Lock()
		o.state.Service.RecoveryInProgress = false
		o.mu.Unlock()
		return
	}

	o.mu.Lock()
	o.state.Service.RecoveryInProgress = true
	o.mu.Unlock()

	cfg := o.currentConfig()
	ids := make([]string, 0, len(pending))
	for issueID := range pending {
		ids = append(ids, issueID)
	}
	refreshed, err := o.tracker.FetchIssueStatesByIDs(ctx, ids)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		o.logger.Warn("ledger recovery refresh failed", "error", err.Error())
		o.mu.Lock()
		if o.setHealthAlertAndNotifyLocked(AlertSnapshot{Code: "tracker_unreachable", Level: "warn", Message: err.Error()}) {
			o.publishViewLocked()
		}
		o.mu.Unlock()
		return
	}

	byID := make(map[string]model.Issue, len(refreshed))
	for _, issue := range refreshed {
		byID[issue.ID] = issue
	}

	o.mu.Lock()
	if o.clearHealthAlertAndNotifyLocked("tracker_unreachable") {
		o.publishViewLocked()
	}
	o.mu.Unlock()

	for issueID, entry := range pending {
		issue, ok := byID[issueID]
		if !ok {
			o.moveToAwaitingIntervention(issueID, recordIdentifier(entry), recordWorkspacePath(entry), recordBranch(entry), entry.RetryAttempt, entry.StallCount, o.currentWorkflow().Completion.Mode, string(model.ContinuationReasonTrackerIssueMissing), entry.LastKnownIssueState, recordPullRequest(entry))
			continue
		}
		switch {
		case o.isTerminalState(issue.State, cfg):
			o.completeSuccessfulIssue(ctx, issueID, recordIdentifier(entry))
			continue
		case !o.isActiveState(issue.State, cfg):
			o.completeAbandonedIssue(ctx, issueID, recordIdentifier(entry), "source issue left active states during recovery")
			continue
		}
		if strings.TrimSpace(recordWorkspacePath(entry)) == "" || strings.TrimSpace(recordBranch(entry)) == "" {
			o.moveToAwaitingIntervention(issueID, recordIdentifier(entry), recordWorkspacePath(entry), recordBranch(entry), entry.RetryAttempt, entry.StallCount, o.currentWorkflow().Completion.Mode, "recovery_uncertain", issue.State, recordPullRequest(entry))
			continue
		}
		_ = o.resumeRecoveredSuccessPath(ctx, issueID, entry, issue.State)
	}

	o.mu.Lock()
	remaining := false
	for _, record := range o.state.Records {
		if record != nil && record.Runtime.Status == contract.IssueStatusActive && record.NeedsRecovery {
			remaining = true
			break
		}
	}
	o.state.Service.RecoveryInProgress = remaining
	o.mu.Unlock()
}

func (o *Orchestrator) resumeRecoveredSuccessPath(ctx context.Context, issueID string, entry *model.IssueRecord, issueState string) error {
	if entry == nil {
		return nil
	}
	decision, err := o.classifySuccessfulRun(ctx, recordWorkspacePath(entry), recordBranch(entry), entry.Dispatch, issueState)
	if err != nil {
		o.logger.Warn("ledger recovery classification failed", "issue_id", issueID, "issue_identifier", recordIdentifier(entry), "branch", recordBranch(entry), "error", err.Error())
		o.moveToAwaitingIntervention(issueID, recordIdentifier(entry), recordWorkspacePath(entry), recordBranch(entry), entry.RetryAttempt, entry.StallCount, o.currentWorkflow().Completion.Mode, "recovery_uncertain", issueState, recordPullRequest(entry))
		return err
	}

	switch decision.Disposition {
	case DispositionCompleted:
		o.completeSuccessfulIssue(ctx, issueID, recordIdentifier(entry))
	case DispositionAwaitingMerge:
		o.moveToAwaitingMerge(issueID, recordIdentifier(entry), issueState, recordWorkspacePath(entry), decision.FinalBranch, entry.RetryAttempt, entry.StallCount, decision.PR, nil)
	case DispositionAwaitingIntervention:
		reason := "recovery_uncertain"
		if decision.Reason != nil {
			reason = string(*decision.Reason)
		}
		o.moveToAwaitingIntervention(issueID, recordIdentifier(entry), recordWorkspacePath(entry), decision.FinalBranch, entry.RetryAttempt, entry.StallCount, decision.ExpectedOutcome, reason, issueState, decision.PR)
	case DispositionContinuation:
		reason := model.ContinuationReasonUnfinishedIssue
		if decision.Reason != nil {
			reason = *decision.Reason
		}
		baseDispatch := entry.Dispatch
		if baseDispatch == nil {
			baseDispatch = freshDispatchContext(normalizeCompletionContract(o.currentWorkflow().Completion))
		}
		retryDispatch := continuationDispatchContext(baseDispatch, normalizeCompletionContract(model.CompletionContract{
			Mode:        decision.ExpectedOutcome,
			OnMissingPR: dispatchCompletionAction(baseDispatch, "missing"),
			OnClosedPR:  dispatchCompletionAction(baseDispatch, "closed"),
		}), reason, decision.FinalBranch, decision.PR, issueState)
		o.mu.Lock()
		current := o.state.Records[issueID]
		if current != nil {
			current.NeedsRecovery = false
			o.scheduleRetryLocked(issueID, recordIdentifier(entry), maxInt(entry.RetryAttempt, 1), nil, true, entry.StallCount, retryDispatch)
			o.commitStateLocked(true)
		}
		o.mu.Unlock()
	default:
		o.moveToAwaitingIntervention(issueID, recordIdentifier(entry), recordWorkspacePath(entry), recordBranch(entry), entry.RetryAttempt, entry.StallCount, o.currentWorkflow().Completion.Mode, "recovery_uncertain", issueState, recordPullRequest(entry))
	}

	return nil
}

func (o *Orchestrator) newRetryTimer(issueID string, dueAt time.Time) *time.Timer {
	delay := dueAt.Sub(o.now().UTC())
	if delay < 0 {
		delay = 0
	}
	return time.AfterFunc(delay, func() {
		select {
		case o.retryFireCh <- issueID:
		default:
		}
	})
}

func (o *Orchestrator) reportSessionPersistenceWriteSuccess(version uint64, state durableRuntimeState) {
	o.handleSessionPersistenceWriteSuccess(version, state)
}

func (o *Orchestrator) reportSessionPersistenceWriteFailure(err error) {
	o.handleSessionPersistenceWriteFailure(err)
}

func (o *Orchestrator) handleSessionPersistenceWriteSuccess(version uint64, state durableRuntimeState) {
	now := o.now().UTC()
	o.mu.Lock()
	if version > o.lastPersistedStateVersion {
		o.lastPersistedStateVersion = version
	}
	copyState := cloneDurableRuntimeState(state)
	o.lastPersistedState = &copyState
	o.persistenceHealth.Status = "healthy"
	o.persistenceHealth.LastAttemptAt = cloneTimePtr(&now)
	o.persistenceHealth.LastSuccessAt = cloneTimePtr(&now)
	o.persistenceHealth.LastError = nil
	o.persistenceHealth.ConsecutiveFailures = 0
	o.clearHealthAlertAndNotifyLocked(sessionPersistenceWriteFailCode)
	o.publishViewLocked()
	shouldResume := false
	actions := make([]func(), 0)
	for issueID, requiredVersion := range o.pendingRecovery {
		if requiredVersion <= version {
			delete(o.pendingRecovery, issueID)
			shouldResume = true
		}
	}
	for actionVersion, versionActions := range o.pendingActions {
		if actionVersion > version || len(versionActions) == 0 {
			continue
		}
		actions = append(actions, versionActions...)
		delete(o.pendingActions, actionVersion)
	}
	o.mu.Unlock()
	for _, action := range actions {
		action()
	}
	if shouldResume {
		o.reconcileRecovering(o.runtimeContext())
	}
}

func (o *Orchestrator) handleSessionPersistenceWriteFailure(err error) {
	o.logger.Warn("session persistence write failed", "error", err.Error())
	o.mu.Lock()
	defer o.mu.Unlock()

	now := o.now().UTC()
	errorText := err.Error()
	o.persistenceHealth.Status = "degraded"
	o.persistenceHealth.LastAttemptAt = cloneTimePtr(&now)
	o.persistenceHealth.LastError = optionalError(errorText)
	o.persistenceHealth.ConsecutiveFailures++
	clear(o.pendingRecovery)
	clear(o.pendingActions)
	for issueID := range o.pendingLaunch {
		delete(o.runningRecords, issueID)
	}
	clear(o.pendingLaunch)
	if o.stateStore != nil {
		o.stateStore.Disable()
	}
	o.enterProtectedModeLocked(errorText)
	o.publishViewLocked()
}

func cloneDurableRuntimeState(state durableRuntimeState) durableRuntimeState {
	copyState := state
	copyState.Service.RecordMetadata = map[string]durableRecordMetadata{}
	for key, meta := range state.Service.RecordMetadata {
		copyState.Service.RecordMetadata[key] = durableRecordMetadata{
			RetryAttempt:        meta.RetryAttempt,
			StallCount:          meta.StallCount,
			LastKnownIssueState: meta.LastKnownIssueState,
			NeedsRecovery:       meta.NeedsRecovery,
			Dispatch:            cloneDurableDispatchContext(meta.Dispatch),
		}
	}
	copyState.Records = append([]contract.IssueLedgerRecord(nil), state.Records...)
	for index := range copyState.Records {
		copyState.Records[index].Reason = cloneReason(copyState.Records[index].Reason)
		copyState.Records[index].DurableRefs = cloneDurableRefs(copyState.Records[index].DurableRefs)
		copyState.Records[index].Result = cloneResult(copyState.Records[index].Result)
		if copyState.Records[index].RetryDueAt != nil {
			value := strings.TrimSpace(*copyState.Records[index].RetryDueAt)
			copyState.Records[index].RetryDueAt = &value
		}
	}
	return copyState
}

func cloneDurableDispatchContext(dispatch *durableDispatchContext) *durableDispatchContext {
	if dispatch == nil {
		return nil
	}
	copyValue := *dispatch
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
	if dispatch.PreviousPR != nil {
		prCopy := *dispatch.PreviousPR
		copyValue.PreviousPR = &prCopy
	}
	return &copyValue
}

func durableDispatchFromModel(dispatch *model.DispatchContext) *durableDispatchContext {
	if dispatch == nil {
		return nil
	}
	result := &durableDispatchContext{
		Kind:            string(dispatch.Kind),
		ExpectedOutcome: string(dispatch.ExpectedOutcome),
		OnMissingPR:     string(dispatch.OnMissingPR),
		OnClosedPR:      string(dispatch.OnClosedPR),
	}
	if dispatch.RetryAttempt != nil {
		retryAttempt := *dispatch.RetryAttempt
		result.RetryAttempt = &retryAttempt
	}
	if dispatch.Reason != nil {
		reason := string(*dispatch.Reason)
		result.Reason = &reason
	}
	if dispatch.PreviousBranch != nil {
		branch := *dispatch.PreviousBranch
		result.PreviousBranch = &branch
	}
	if dispatch.PreviousIssueState != nil {
		state := *dispatch.PreviousIssueState
		result.PreviousIssueState = &state
	}
	if dispatch.PreviousPR != nil {
		result.PreviousPR = &durablePRContext{
			Number:     dispatch.PreviousPR.Number,
			URL:        dispatch.PreviousPR.URL,
			State:      dispatch.PreviousPR.State,
			Merged:     dispatch.PreviousPR.Merged,
			HeadBranch: dispatch.PreviousPR.HeadBranch,
		}
	}
	return result
}

func durableDispatchToModel(dispatch *durableDispatchContext) *model.DispatchContext {
	if dispatch == nil {
		return nil
	}
	result := &model.DispatchContext{
		Kind:            model.DispatchKind(strings.TrimSpace(dispatch.Kind)),
		ExpectedOutcome: model.CompletionMode(strings.TrimSpace(dispatch.ExpectedOutcome)),
		OnMissingPR:     model.CompletionAction(strings.TrimSpace(dispatch.OnMissingPR)),
		OnClosedPR:      model.CompletionAction(strings.TrimSpace(dispatch.OnClosedPR)),
	}
	if dispatch.RetryAttempt != nil {
		retryAttempt := *dispatch.RetryAttempt
		result.RetryAttempt = &retryAttempt
	}
	if dispatch.Reason != nil {
		reason := model.ContinuationReason(strings.TrimSpace(*dispatch.Reason))
		result.Reason = &reason
	}
	if dispatch.PreviousBranch != nil {
		branch := *dispatch.PreviousBranch
		result.PreviousBranch = &branch
	}
	if dispatch.PreviousIssueState != nil {
		state := *dispatch.PreviousIssueState
		result.PreviousIssueState = &state
	}
	if dispatch.PreviousPR != nil {
		result.PreviousPR = &model.PRContext{
			Number:     dispatch.PreviousPR.Number,
			URL:        dispatch.PreviousPR.URL,
			State:      dispatch.PreviousPR.State,
			Merged:     dispatch.PreviousPR.Merged,
			HeadBranch: dispatch.PreviousPR.HeadBranch,
		}
	}
	return result
}

func cloneTimePtr(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func pointerString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
