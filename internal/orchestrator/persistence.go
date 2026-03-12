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
)

const (
	durableStateVersion             = 2
	sessionPersistenceWriteFailCode = "session_persistence_write_failed"
	stateStoreDrainTimeout          = 5 * time.Second
	initialStateStoreRetryDelay     = time.Second
	maxStateStoreRetryDelay         = 30 * time.Second
)

type stateStore interface {
	Load() (*durableRuntimeState, error)
	Schedule(state durableRuntimeState, critical bool)
	Close(ctx context.Context) error
}

type durableRuntimeState struct {
	Version              int                                `json:"version"`
	Identity             RuntimeIdentity                    `json:"identity"`
	SavedAt              time.Time                          `json:"saved_at"`
	Retrying             []durableRetryEntry                `json:"retrying"`
	RecoveredPending     []durableRecoveredPendingEntry     `json:"recovered_pending"`
	AwaitingMerge        []durableAwaitingMergeEntry        `json:"awaiting_merge"`
	AwaitingIntervention []durableAwaitingInterventionEntry `json:"awaiting_intervention"`
	Alerts               []AlertSnapshot                    `json:"alerts"`
	TokenTotal           model.TokenTotals                  `json:"token_total"`
}

type durableRetryEntry struct {
	IssueID       string                 `json:"issue_id"`
	Identifier    string                 `json:"identifier"`
	WorkspacePath string                 `json:"workspace_path"`
	Attempt       int                    `json:"attempt"`
	StallCount    int                    `json:"stall_count"`
	DueAt         time.Time              `json:"due_at"`
	Error         *string                `json:"error,omitempty"`
	Dispatch      *model.DispatchContext `json:"dispatch,omitempty"`
}

type durableRecoveredPendingEntry struct {
	IssueID        string                 `json:"issue_id"`
	Identifier     string                 `json:"identifier"`
	WorkspacePath  string                 `json:"workspace_path"`
	State          string                 `json:"state"`
	RetryAttempt   int                    `json:"retry_attempt"`
	StallCount     int                    `json:"stall_count"`
	ObservedAt     time.Time              `json:"observed_at"`
	Dispatch       *model.DispatchContext `json:"dispatch,omitempty"`
	RecoverySource string                 `json:"recovery_source"`
}

type durableAwaitingMergeEntry struct {
	IssueID string `json:"issue_id"`
	model.AwaitingMergeEntry
}

type durableAwaitingInterventionEntry struct {
	IssueID string `json:"issue_id"`
	model.AwaitingInterventionEntry
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
	onSuccess func()
	onFailure func(error)

	mu          sync.Mutex
	desired     *scheduledDurableState
	nextVersion uint64
	signalCh    chan struct{}
	closeCh     chan closeRequest
}

func newFileStateStore(cfg model.SessionPersistenceConfig, identity RuntimeIdentity, logger *slog.Logger, onSuccess func(), onFailure func(error)) stateStore {
	if logger == nil {
		logger = slog.Default()
	}
	resolvedCfg := cfg
	resolvedCfg.Path = resolveStateStorePath(cfg.Path, identity.ConfigRoot)
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
	raw, err := os.ReadFile(s.config.Path)
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
	if normalizeRuntimeIdentity(state.Identity) != s.identity {
		return nil, fmt.Errorf("session state identity does not match current runtime")
	}
	return &state, nil
}

func (s *fileStateStore) Schedule(state durableRuntimeState, critical bool) {
	s.mu.Lock()
	s.nextVersion++
	force := critical && s.config.FsyncOnCritical
	if s.desired != nil && s.desired.force {
		force = true
	}
	s.desired = &scheduledDurableState{
		version: s.nextVersion,
		force:   force,
		state:   cloneDurableRuntimeState(state),
	}
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
		delay := time.Duration(maxInt(s.config.FlushIntervalMS, 1)) * time.Millisecond
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
			delay := time.Duration(maxInt(s.config.FlushIntervalMS, 1)) * time.Millisecond
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
	if s.desired == nil {
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

	if err := writeDurableRuntimeState(s.config.Path, pending.state, force); err != nil {
		if s.onFailure != nil {
			s.onFailure(err)
		}
		return false, false, err
	}

	hasMore, nextForce := s.markFlushed(pending.version)
	if s.onSuccess != nil {
		s.onSuccess()
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

	tempFile, err := os.CreateTemp(dir, "session-state-*.tmp")
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
	return os.Rename(tempPath, path)
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
		store = newFileStateStore(cfg.SessionPersistence, identity, o.logger, o.handleSessionPersistenceWriteSuccess, o.handleSessionPersistenceWriteFailure)
		restoredState, err = store.Load()
		if err != nil {
			o.mu.Lock()
			activeNotifier := o.notifier
			o.notifier = nil
			o.mu.Unlock()
			o.closeNotifier(activeNotifier)
			o.closeStateStore(store)
			return fmt.Errorf("session persistence state at %s is incompatible or unreadable; delete the file and restart: %w", resolveStateStorePath(cfg.SessionPersistence.Path, identity.ConfigRoot), err)
		}
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	if o.extensionsReady {
		o.closeStateStore(store)
		return nil
	}
	o.stateStore = store
	if restoredState != nil {
		o.restorePersistedStateLocked(restoredState)
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
			ConfigRoot:  cfg.AutomationRootDir,
			TrackerKind: cfg.TrackerKind,
			TrackerRepo: cfg.TrackerRepo,
		})
	}
	return normalizeRuntimeIdentity(o.runtimeIdentityFn())
}

func normalizeRuntimeIdentity(identity RuntimeIdentity) RuntimeIdentity {
	identity.ConfigRoot = normalizeRuntimePath(identity.ConfigRoot)
	identity.Profile = strings.TrimSpace(identity.Profile)
	identity.SourceName = strings.TrimSpace(identity.SourceName)
	identity.FlowName = strings.TrimSpace(identity.FlowName)
	identity.TrackerKind = model.NormalizeState(identity.TrackerKind)
	identity.TrackerRepo = strings.TrimSpace(identity.TrackerRepo)
	return identity
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

func (o *Orchestrator) scheduleStatePersistLocked(critical bool) {
	if o.stateStore == nil {
		return
	}
	o.stateStore.Schedule(o.buildPersistedStateLocked(), critical)
}

func (o *Orchestrator) commitStateLocked(critical bool) {
	o.refreshSnapshotLocked()
	o.publishSnapshotLocked()
	o.scheduleStatePersistLocked(critical)
}

func (o *Orchestrator) buildPersistedStateLocked() durableRuntimeState {
	state := durableRuntimeState{
		Version:    durableStateVersion,
		Identity:   o.currentRuntimeIdentity(),
		SavedAt:    o.now().UTC(),
		TokenTotal: o.state.CodexTotals,
	}

	for issueID, entry := range o.state.RetryAttempts {
		if entry == nil {
			continue
		}
		state.Retrying = append(state.Retrying, durableRetryEntry{
			IssueID:       issueID,
			Identifier:    entry.Identifier,
			WorkspacePath: entry.WorkspacePath,
			Attempt:       entry.Attempt,
			StallCount:    entry.StallCount,
			DueAt:         entry.DueAt,
			Error:         entry.Error,
			Dispatch:      model.CloneDispatchContext(entry.Dispatch),
		})
	}
	sort.SliceStable(state.Retrying, func(i int, j int) bool {
		if state.Retrying[i].Identifier != state.Retrying[j].Identifier {
			return state.Retrying[i].Identifier < state.Retrying[j].Identifier
		}
		return state.Retrying[i].IssueID < state.Retrying[j].IssueID
	})

	recovered := make(map[string]durableRecoveredPendingEntry, len(o.state.RecoveredPending)+len(o.state.Running)+len(o.state.Claimed))
	for issueID, entry := range o.state.RecoveredPending {
		if entry == nil {
			continue
		}
		recovered[issueID] = durableRecoveredPendingEntry{
			IssueID:        issueID,
			Identifier:     entry.Identifier,
			WorkspacePath:  entry.WorkspacePath,
			State:          entry.State,
			RetryAttempt:   entry.RetryAttempt,
			StallCount:     entry.StallCount,
			ObservedAt:     entry.ObservedAt,
			Dispatch:       model.CloneDispatchContext(entry.Dispatch),
			RecoverySource: entry.RecoverySource,
		}
	}
	for issueID, entry := range o.state.Running {
		if entry == nil {
			continue
		}
		item := durableRecoveredPendingEntry{
			IssueID:        issueID,
			Identifier:     entry.Identifier,
			WorkspacePath:  entry.WorkspacePath,
			RetryAttempt:   entry.RetryAttempt,
			StallCount:     entry.StallCount,
			ObservedAt:     entry.StartedAt,
			Dispatch:       model.CloneDispatchContext(entry.Dispatch),
			RecoverySource: "running",
		}
		if entry.Issue != nil {
			item.State = entry.Issue.State
		}
		recovered[issueID] = item
	}
	for issueID, entry := range o.state.Claimed {
		if entry == nil {
			continue
		}
		if _, exists := o.state.RetryAttempts[issueID]; exists {
			continue
		}
		if _, exists := o.state.AwaitingMerge[issueID]; exists {
			continue
		}
		if _, exists := o.state.AwaitingIntervention[issueID]; exists {
			continue
		}
		if _, exists := recovered[issueID]; exists {
			continue
		}
		recovered[issueID] = durableRecoveredPendingEntry{
			IssueID:        issueID,
			Identifier:     entry.Identifier,
			WorkspacePath:  entry.WorkspacePath,
			State:          entry.State,
			RetryAttempt:   entry.RetryAttempt,
			StallCount:     entry.StallCount,
			ObservedAt:     entry.ClaimedAt,
			Dispatch:       model.CloneDispatchContext(entry.Dispatch),
			RecoverySource: "claimed",
		}
	}
	for _, item := range recovered {
		state.RecoveredPending = append(state.RecoveredPending, item)
	}
	sort.SliceStable(state.RecoveredPending, func(i int, j int) bool {
		if state.RecoveredPending[i].Identifier != state.RecoveredPending[j].Identifier {
			return state.RecoveredPending[i].Identifier < state.RecoveredPending[j].Identifier
		}
		return state.RecoveredPending[i].IssueID < state.RecoveredPending[j].IssueID
	})

	for issueID, entry := range o.state.AwaitingMerge {
		if entry == nil {
			continue
		}
		state.AwaitingMerge = append(state.AwaitingMerge, durableAwaitingMergeEntry{
			IssueID:            issueID,
			AwaitingMergeEntry: *cloneAwaitingMergeEntry(entry),
		})
	}
	sort.SliceStable(state.AwaitingMerge, func(i int, j int) bool {
		if state.AwaitingMerge[i].Identifier != state.AwaitingMerge[j].Identifier {
			return state.AwaitingMerge[i].Identifier < state.AwaitingMerge[j].Identifier
		}
		return state.AwaitingMerge[i].IssueID < state.AwaitingMerge[j].IssueID
	})

	for issueID, entry := range o.state.AwaitingIntervention {
		if entry == nil {
			continue
		}
		state.AwaitingIntervention = append(state.AwaitingIntervention, durableAwaitingInterventionEntry{
			IssueID:                   issueID,
			AwaitingInterventionEntry: *cloneAwaitingInterventionEntry(entry),
		})
	}
	sort.SliceStable(state.AwaitingIntervention, func(i int, j int) bool {
		if state.AwaitingIntervention[i].Identifier != state.AwaitingIntervention[j].Identifier {
			return state.AwaitingIntervention[i].Identifier < state.AwaitingIntervention[j].Identifier
		}
		return state.AwaitingIntervention[i].IssueID < state.AwaitingIntervention[j].IssueID
	})

	for _, alert := range o.systemAlerts {
		state.Alerts = append(state.Alerts, alert)
	}
	sort.SliceStable(state.Alerts, func(i int, j int) bool {
		if state.Alerts[i].Code != state.Alerts[j].Code {
			return state.Alerts[i].Code < state.Alerts[j].Code
		}
		return state.Alerts[i].Message < state.Alerts[j].Message
	})

	return state
}

func (o *Orchestrator) restorePersistedStateLocked(state *durableRuntimeState) {
	if state == nil {
		return
	}

	for _, alert := range state.Alerts {
		if strings.TrimSpace(alert.Code) == "" {
			continue
		}
		o.systemAlerts[alert.Code] = alert
	}
	o.state.CodexTotals = state.TokenTotal

	for _, item := range state.Retrying {
		retryEntry := &model.RetryEntry{
			IssueID:       item.IssueID,
			Identifier:    item.Identifier,
			WorkspacePath: item.WorkspacePath,
			Attempt:       item.Attempt,
			StallCount:    item.StallCount,
			DueAt:         item.DueAt,
			Error:         optionalError(pointerString(item.Error)),
			Dispatch:      model.CloneDispatchContext(item.Dispatch),
		}
		retryEntry.TimerHandle = o.newRetryTimer(item.IssueID, item.DueAt)
		o.state.RetryAttempts[item.IssueID] = retryEntry
		o.setClaimedLocked(item.IssueID, &model.ClaimedEntry{
			Identifier:    item.Identifier,
			WorkspacePath: item.WorkspacePath,
			RetryAttempt:  item.Attempt,
			StallCount:    item.StallCount,
			ClaimedAt:     state.SavedAt,
			Dispatch:      model.CloneDispatchContext(item.Dispatch),
		})
	}

	for _, item := range state.RecoveredPending {
		o.state.RecoveredPending[item.IssueID] = &model.RecoveredPendingEntry{
			Identifier:     item.Identifier,
			WorkspacePath:  item.WorkspacePath,
			State:          item.State,
			RetryAttempt:   item.RetryAttempt,
			StallCount:     item.StallCount,
			ObservedAt:     item.ObservedAt,
			Dispatch:       model.CloneDispatchContext(item.Dispatch),
			RecoverySource: item.RecoverySource,
		}
	}

	for _, item := range state.AwaitingMerge {
		entry := cloneAwaitingMergeEntry(&item.AwaitingMergeEntry)
		o.state.AwaitingMerge[item.IssueID] = entry
		o.setClaimedLocked(item.IssueID, &model.ClaimedEntry{
			Identifier:    entry.Identifier,
			WorkspacePath: entry.WorkspacePath,
			State:         entry.State,
			RetryAttempt:  entry.RetryAttempt,
			StallCount:    entry.StallCount,
			ClaimedAt:     entry.AwaitingSince,
		})
	}

	for _, item := range state.AwaitingIntervention {
		entry := cloneAwaitingInterventionEntry(&item.AwaitingInterventionEntry)
		o.state.AwaitingIntervention[item.IssueID] = entry
		o.setClaimedLocked(item.IssueID, &model.ClaimedEntry{
			Identifier:    entry.Identifier,
			WorkspacePath: entry.WorkspacePath,
			State:         entry.LastKnownIssueState,
			RetryAttempt:  entry.RetryAttempt,
			StallCount:    entry.StallCount,
			ClaimedAt:     entry.ObservedAt,
		})
	}
}

func (o *Orchestrator) reconcileRecoveredPending(ctx context.Context) {
	o.mu.RLock()
	pending := make(map[string]model.RecoveredPendingEntry, len(o.state.RecoveredPending))
	for issueID, entry := range o.state.RecoveredPending {
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
		if ctx.Err() != nil {
			return
		}
		o.logger.Warn("recovered-pending state refresh failed", "error", err.Error())
		o.mu.Lock()
		if o.setSystemAlertLocked(AlertSnapshot{
			Code:    "tracker_unreachable",
			Level:   "warn",
			Message: err.Error(),
		}) {
			o.commitStateLocked(true)
		}
		o.mu.Unlock()
		return
	}

	byID := make(map[string]model.Issue, len(refreshed))
	for _, issue := range refreshed {
		byID[issue.ID] = issue
	}

	o.mu.Lock()
	changed := o.clearSystemAlertLocked("tracker_unreachable")
	for issueID, entry := range pending {
		current := o.state.RecoveredPending[issueID]
		if current == nil {
			continue
		}

		issue, ok := byID[issueID]
		if !ok {
			continue
		}

		switch {
		case o.isTerminalState(issue.State, cfg), !o.isActiveState(issue.State, cfg):
			delete(o.state.RecoveredPending, issueID)
			delete(o.state.Claimed, issueID)
			changed = true
		default:
			o.setClaimedLocked(issueID, &model.ClaimedEntry{
				Identifier:    entry.Identifier,
				WorkspacePath: entry.WorkspacePath,
				State:         issue.State,
				RetryAttempt:  entry.RetryAttempt,
				StallCount:    entry.StallCount,
				ClaimedAt:     entry.ObservedAt,
				Dispatch:      model.CloneDispatchContext(entry.Dispatch),
			})
			delete(o.state.RecoveredPending, issueID)
			o.scheduleRetryLocked(issueID, entry.Identifier, entry.RetryAttempt, nil, true, entry.StallCount, entry.Dispatch)
			changed = true
		}
	}
	if changed {
		o.commitStateLocked(true)
	}
	o.mu.Unlock()
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

func (o *Orchestrator) handleSessionPersistenceWriteSuccess() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.clearSystemAlertLocked(sessionPersistenceWriteFailCode) {
		o.commitStateLocked(false)
	}
}

func (o *Orchestrator) handleSessionPersistenceWriteFailure(err error) {
	o.logger.Warn("session persistence write failed", "error", err.Error())
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.setSystemAlertLocked(AlertSnapshot{
		Code:    sessionPersistenceWriteFailCode,
		Level:   "warn",
		Message: err.Error(),
	}) {
		return
	}
	o.commitStateLocked(false)
}

func (o *Orchestrator) setClaimedLocked(issueID string, entry *model.ClaimedEntry) {
	if strings.TrimSpace(issueID) == "" {
		return
	}
	o.state.Claimed[issueID] = cloneClaimedEntry(entry)
}

func (o *Orchestrator) claimedEntry(issue *model.Issue, identifier string, workspacePath string, retryAttempt int, stallCount int, dispatch *model.DispatchContext) *model.ClaimedEntry {
	state := ""
	if issue != nil {
		state = issue.State
		if strings.TrimSpace(identifier) == "" {
			identifier = issue.Identifier
		}
	}
	return &model.ClaimedEntry{
		Identifier:    identifier,
		WorkspacePath: workspacePath,
		State:         state,
		RetryAttempt:  retryAttempt,
		StallCount:    stallCount,
		ClaimedAt:     o.now().UTC(),
		Dispatch:      model.CloneDispatchContext(dispatch),
	}
}

func cloneDurableRuntimeState(state durableRuntimeState) durableRuntimeState {
	copyState := state
	copyState.Alerts = append([]AlertSnapshot(nil), state.Alerts...)
	copyState.Retrying = append([]durableRetryEntry(nil), state.Retrying...)
	copyState.RecoveredPending = append([]durableRecoveredPendingEntry(nil), state.RecoveredPending...)
	copyState.AwaitingMerge = append([]durableAwaitingMergeEntry(nil), state.AwaitingMerge...)
	copyState.AwaitingIntervention = append([]durableAwaitingInterventionEntry(nil), state.AwaitingIntervention...)
	for index := range copyState.Retrying {
		copyState.Retrying[index].Dispatch = model.CloneDispatchContext(copyState.Retrying[index].Dispatch)
	}
	for index := range copyState.RecoveredPending {
		copyState.RecoveredPending[index].Dispatch = model.CloneDispatchContext(copyState.RecoveredPending[index].Dispatch)
	}
	return copyState
}

func cloneClaimedEntry(entry *model.ClaimedEntry) *model.ClaimedEntry {
	if entry == nil {
		return nil
	}
	copyEntry := *entry
	copyEntry.Dispatch = model.CloneDispatchContext(entry.Dispatch)
	return &copyEntry
}

func cloneAwaitingMergeEntry(entry *model.AwaitingMergeEntry) *model.AwaitingMergeEntry {
	if entry == nil {
		return nil
	}
	copyEntry := *entry
	if entry.LastError != nil {
		copyEntry.LastError = optionalError(*entry.LastError)
	}
	if entry.NextPostMergeRetryAt != nil {
		nextRetryAt := *entry.NextPostMergeRetryAt
		copyEntry.NextPostMergeRetryAt = &nextRetryAt
	}
	return &copyEntry
}

func cloneAwaitingInterventionEntry(entry *model.AwaitingInterventionEntry) *model.AwaitingInterventionEntry {
	if entry == nil {
		return nil
	}
	copyEntry := *entry
	return &copyEntry
}

func pointerString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
