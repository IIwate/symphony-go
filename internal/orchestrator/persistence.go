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
	durableStateVersion             = 5
	sessionPersistenceWriteFailCode = "session_persistence_write_failed"
	stateStoreDrainTimeout          = 5 * time.Second
	initialStateStoreRetryDelay     = time.Second
	maxStateStoreRetryDelay         = 30 * time.Second
)

type stateStore interface {
	Load() (*durableRuntimeState, error)
	Schedule(state durableRuntimeState, critical bool) uint64
	Disable()
	Close(ctx context.Context) error
}

type durableRuntimeState struct {
	Version              int                                `json:"version"`
	Identity             RuntimeIdentity                    `json:"identity"`
	SavedAt              time.Time                          `json:"saved_at"`
	Retrying             []durableRetryEntry                `json:"retrying"`
	Recovering           []durableRecoveryEntry             `json:"recovering"`
	AwaitingMerge        []durableAwaitingMergeEntry        `json:"awaiting_merge"`
	AwaitingIntervention []durableAwaitingInterventionEntry `json:"awaiting_intervention"`
	TokenTotal           model.TokenTotals                  `json:"token_total"`
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

type durableRetryEntry struct {
	IssueID       string                  `json:"issue_id"`
	Identifier    string                  `json:"identifier"`
	WorkspacePath string                  `json:"workspace_path"`
	Attempt       int                     `json:"attempt"`
	StallCount    int                     `json:"stall_count"`
	DueAt         time.Time               `json:"due_at"`
	Error         *string                 `json:"error,omitempty"`
	Dispatch      *durableDispatchContext `json:"dispatch,omitempty"`
}

type durableRecoveryEntry struct {
	IssueID       string                  `json:"issue_id"`
	Identifier    string                  `json:"identifier"`
	WorkspacePath string                  `json:"workspace_path"`
	FinalBranch   string                  `json:"final_branch,omitempty"`
	State         string                  `json:"state,omitempty"`
	RetryAttempt  int                     `json:"retry_attempt"`
	StallCount    int                     `json:"stall_count"`
	ObservedAt    time.Time               `json:"observed_at"`
	Strategy      string                  `json:"strategy"`
	Source        string                  `json:"source,omitempty"`
	Dispatch      *durableDispatchContext `json:"dispatch,omitempty"`
}

type durableAwaitingMergeEntry struct {
	IssueID              string     `json:"issue_id"`
	Identifier           string     `json:"identifier"`
	State                string     `json:"state,omitempty"`
	WorkspacePath        string     `json:"workspace_path"`
	Branch               string     `json:"branch,omitempty"`
	PRNumber             int        `json:"pr_number,omitempty"`
	PRURL                string     `json:"pr_url,omitempty"`
	PRState              string     `json:"pr_state,omitempty"`
	PRBaseOwner          string     `json:"pr_base_owner,omitempty"`
	PRBaseRepo           string     `json:"pr_base_repo,omitempty"`
	PRHeadOwner          string     `json:"pr_head_owner,omitempty"`
	RetryAttempt         int        `json:"retry_attempt"`
	StallCount           int        `json:"stall_count"`
	AwaitingSince        time.Time  `json:"awaiting_since"`
	LastError            *string    `json:"last_error,omitempty"`
	PostMergeRetryCount  int        `json:"post_merge_retry_count,omitempty"`
	NextPostMergeRetryAt *time.Time `json:"next_post_merge_retry_at,omitempty"`
}

type durableAwaitingInterventionEntry struct {
	IssueID             string    `json:"issue_id"`
	Identifier          string    `json:"identifier"`
	WorkspacePath       string    `json:"workspace_path"`
	Branch              string    `json:"branch,omitempty"`
	PRNumber            int       `json:"pr_number,omitempty"`
	PRURL               string    `json:"pr_url,omitempty"`
	PRState             string    `json:"pr_state,omitempty"`
	PRBaseOwner         string    `json:"pr_base_owner,omitempty"`
	PRBaseRepo          string    `json:"pr_base_repo,omitempty"`
	PRHeadOwner         string    `json:"pr_head_owner,omitempty"`
	RetryAttempt        int       `json:"retry_attempt"`
	StallCount          int       `json:"stall_count"`
	ObservedAt          time.Time `json:"observed_at"`
	Reason              string    `json:"reason,omitempty"`
	ExpectedOutcome     string    `json:"expected_outcome,omitempty"`
	PreviousBranch      string    `json:"previous_branch,omitempty"`
	LastKnownIssueState string    `json:"last_known_issue_state,omitempty"`
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
			Error:         optionalError(pointerString(entry.Error)),
			Dispatch:      durableDispatchFromModel(entry.Dispatch),
		})
	}
	sort.SliceStable(state.Retrying, func(i int, j int) bool {
		if state.Retrying[i].Identifier != state.Retrying[j].Identifier {
			return state.Retrying[i].Identifier < state.Retrying[j].Identifier
		}
		return state.Retrying[i].IssueID < state.Retrying[j].IssueID
	})

	recovering := make(map[string]durableRecoveryEntry, len(o.state.Recovering)+len(o.state.Running))
	for issueID, entry := range o.state.Recovering {
		if entry == nil {
			continue
		}
		recovering[issueID] = durableRecoveryEntry{
			IssueID:       issueID,
			Identifier:    entry.Identifier,
			WorkspacePath: entry.WorkspacePath,
			FinalBranch:   entry.FinalBranch,
			State:         entry.State,
			RetryAttempt:  entry.RetryAttempt,
			StallCount:    entry.StallCount,
			ObservedAt:    entry.ObservedAt,
			Strategy:      string(entry.Strategy),
			Source:        string(entry.Source),
			Dispatch:      durableDispatchFromModel(entry.Dispatch),
		}
	}
	for issueID, entry := range o.state.Running {
		if entry == nil {
			continue
		}
		item := durableRecoveryEntry{
			IssueID:       issueID,
			Identifier:    entry.Identifier,
			WorkspacePath: entry.WorkspacePath,
			RetryAttempt:  entry.RetryAttempt,
			StallCount:    entry.StallCount,
			ObservedAt:    entry.StartedAt,
			Strategy:      string(model.RecoveryStrategyContinuationRetry),
			Source:        string(model.RecoverySourceRunning),
			Dispatch:      durableDispatchFromModel(entry.Dispatch),
		}
		if entry.Issue != nil {
			item.State = entry.Issue.State
		}
		recovering[issueID] = item
	}
	for _, item := range recovering {
		state.Recovering = append(state.Recovering, item)
	}
	sort.SliceStable(state.Recovering, func(i int, j int) bool {
		if state.Recovering[i].Identifier != state.Recovering[j].Identifier {
			return state.Recovering[i].Identifier < state.Recovering[j].Identifier
		}
		return state.Recovering[i].IssueID < state.Recovering[j].IssueID
	})

	for issueID, entry := range o.state.AwaitingMerge {
		if entry == nil {
			continue
		}
		state.AwaitingMerge = append(state.AwaitingMerge, durableAwaitingMergeEntry{
			IssueID:              issueID,
			Identifier:           entry.Identifier,
			State:                entry.State,
			WorkspacePath:        entry.WorkspacePath,
			Branch:               entry.Branch,
			PRNumber:             entry.PRNumber,
			PRURL:                entry.PRURL,
			PRState:              entry.PRState,
			PRBaseOwner:          entry.PRBaseOwner,
			PRBaseRepo:           entry.PRBaseRepo,
			PRHeadOwner:          entry.PRHeadOwner,
			RetryAttempt:         entry.RetryAttempt,
			StallCount:           entry.StallCount,
			AwaitingSince:        entry.AwaitingSince,
			LastError:            optionalError(pointerString(entry.LastError)),
			PostMergeRetryCount:  entry.PostMergeRetryCount,
			NextPostMergeRetryAt: cloneTimePtr(entry.NextPostMergeRetryAt),
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
			IssueID:             issueID,
			Identifier:          entry.Identifier,
			WorkspacePath:       entry.WorkspacePath,
			Branch:              entry.Branch,
			PRNumber:            entry.PRNumber,
			PRURL:               entry.PRURL,
			PRState:             entry.PRState,
			PRBaseOwner:         entry.PRBaseOwner,
			PRBaseRepo:          entry.PRBaseRepo,
			PRHeadOwner:         entry.PRHeadOwner,
			RetryAttempt:        entry.RetryAttempt,
			StallCount:          entry.StallCount,
			ObservedAt:          entry.ObservedAt,
			Reason:              entry.Reason,
			ExpectedOutcome:     entry.ExpectedOutcome,
			PreviousBranch:      entry.PreviousBranch,
			LastKnownIssueState: entry.LastKnownIssueState,
		})
	}
	sort.SliceStable(state.AwaitingIntervention, func(i int, j int) bool {
		if state.AwaitingIntervention[i].Identifier != state.AwaitingIntervention[j].Identifier {
			return state.AwaitingIntervention[i].Identifier < state.AwaitingIntervention[j].Identifier
		}
		return state.AwaitingIntervention[i].IssueID < state.AwaitingIntervention[j].IssueID
	})

	return state
}

func (o *Orchestrator) restorePersistedStateLocked(state *durableRuntimeState) {
	if state == nil {
		return
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
			Dispatch:      durableDispatchToModel(item.Dispatch),
		}
		retryEntry.TimerHandle = o.newRetryTimer(item.IssueID, item.DueAt)
		o.state.RetryAttempts[item.IssueID] = retryEntry
	}

	for _, item := range state.Recovering {
		o.state.Recovering[item.IssueID] = &model.RecoveryEntry{
			Identifier:    item.Identifier,
			WorkspacePath: item.WorkspacePath,
			FinalBranch:   item.FinalBranch,
			State:         item.State,
			RetryAttempt:  item.RetryAttempt,
			StallCount:    item.StallCount,
			ObservedAt:    item.ObservedAt,
			Strategy:      model.RecoveryStrategy(strings.TrimSpace(item.Strategy)),
			Source:        model.RecoverySource(strings.TrimSpace(item.Source)),
			Dispatch:      durableDispatchToModel(item.Dispatch),
		}
	}

	for _, item := range state.AwaitingMerge {
		o.state.AwaitingMerge[item.IssueID] = &model.AwaitingMergeEntry{
			Identifier:           item.Identifier,
			State:                item.State,
			WorkspacePath:        item.WorkspacePath,
			Branch:               item.Branch,
			PRNumber:             item.PRNumber,
			PRURL:                item.PRURL,
			PRState:              item.PRState,
			PRBaseOwner:          item.PRBaseOwner,
			PRBaseRepo:           item.PRBaseRepo,
			PRHeadOwner:          item.PRHeadOwner,
			RetryAttempt:         item.RetryAttempt,
			StallCount:           item.StallCount,
			AwaitingSince:        item.AwaitingSince,
			LastError:            optionalError(pointerString(item.LastError)),
			PostMergeRetryCount:  item.PostMergeRetryCount,
			NextPostMergeRetryAt: cloneTimePtr(item.NextPostMergeRetryAt),
		}
	}

	for _, item := range state.AwaitingIntervention {
		o.state.AwaitingIntervention[item.IssueID] = &model.AwaitingInterventionEntry{
			Identifier:          item.Identifier,
			WorkspacePath:       item.WorkspacePath,
			Branch:              item.Branch,
			PRNumber:            item.PRNumber,
			PRURL:               item.PRURL,
			PRState:             item.PRState,
			PRBaseOwner:         item.PRBaseOwner,
			PRBaseRepo:          item.PRBaseRepo,
			PRHeadOwner:         item.PRHeadOwner,
			RetryAttempt:        item.RetryAttempt,
			StallCount:          item.StallCount,
			ObservedAt:          item.ObservedAt,
			Reason:              item.Reason,
			ExpectedOutcome:     item.ExpectedOutcome,
			PreviousBranch:      item.PreviousBranch,
			LastKnownIssueState: item.LastKnownIssueState,
		}
	}
}

func (o *Orchestrator) reconcileRecovering(ctx context.Context) {
	o.mu.RLock()
	protected := o.isProtectedLocked()
	o.mu.RUnlock()
	if protected {
		return
	}

	o.mu.RLock()
	pending := make(map[string]model.RecoveryEntry, len(o.state.Recovering))
	for issueID, entry := range o.state.Recovering {
		if entry == nil {
			continue
		}
		if _, waitingForDurable := o.pendingResume[issueID]; waitingForDurable {
			continue
		}
		copyEntry := *entry
		copyEntry.Dispatch = model.CloneDispatchContext(entry.Dispatch)
		pending[issueID] = copyEntry
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
		o.logger.Warn("recovering state refresh failed", "error", err.Error())
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
			o.moveRecoveringIssueToAwaitingIntervention(issueID, entry, model.ContinuationReasonTrackerIssueMissing, entry.State)
			continue
		}

		switch {
		case o.isTerminalState(issue.State, cfg):
			o.completeSuccessfulIssue(ctx, issueID, entry.Identifier)
			continue
		case !o.isActiveState(issue.State, cfg):
			o.mu.Lock()
			current := o.state.Recovering[issueID]
			if current != nil {
				delete(o.pendingResume, issueID)
				delete(o.state.Recovering, issueID)
				o.commitStateLocked(true)
			}
			o.mu.Unlock()
			continue
		}

		switch entry.Strategy {
		case model.RecoveryStrategyPostRunResume:
			_ = o.resumeRecoveredSuccessPath(ctx, issueID, entry, issue.State)
		default:
			o.mu.Lock()
			current := o.state.Recovering[issueID]
			if current != nil {
				delete(o.pendingResume, issueID)
				delete(o.state.Recovering, issueID)
				o.scheduleRetryLocked(issueID, entry.Identifier, entry.RetryAttempt, nil, true, entry.StallCount, entry.Dispatch)
				o.commitStateLocked(true)
			}
			o.mu.Unlock()
		}
	}
}

func (o *Orchestrator) resumeRecoveredSuccessPath(ctx context.Context, issueID string, entry model.RecoveryEntry, issueState string) error {
	decision, err := o.classifySuccessfulRun(ctx, entry.WorkspacePath, entry.FinalBranch, entry.Dispatch, o.currentConfig().OrchestratorAutoCloseOnPR, issueState)
	if err != nil {
		o.logger.Warn("post-run completion classification failed", "issue_id", issueID, "issue_identifier", entry.Identifier, "branch", entry.FinalBranch, "error", err.Error())
		o.mu.Lock()
		current := o.state.Recovering[issueID]
		if current != nil && current.State != issueState {
			current.State = issueState
			o.commitStateLocked(false)
		}
		o.mu.Unlock()
		return err
	}

	switch decision.Disposition {
	case DispositionTryCompleteMergedPR:
		o.tryCompleteMergedPullRequest(ctx, issueID, entry.Identifier, entry.WorkspacePath, decision.FinalBranch, entry.RetryAttempt, entry.StallCount, issueState, decision.PR)
	case DispositionAwaitingMerge:
		o.moveToAwaitingMerge(issueID, entry.Identifier, issueState, entry.WorkspacePath, decision.FinalBranch, entry.RetryAttempt, entry.StallCount, decision.PR, nil)
	case DispositionAwaitingIntervention:
		reason := ""
		if decision.Reason != nil {
			reason = string(*decision.Reason)
		}
		o.moveToAwaitingIntervention(issueID, entry.Identifier, entry.WorkspacePath, decision.FinalBranch, entry.RetryAttempt, entry.StallCount, decision.ExpectedOutcome, reason, issueState, decision.PR)
	case DispositionContinuation:
		reason := model.ContinuationReasonUnfinishedIssue
		if decision.Reason != nil {
			reason = *decision.Reason
		}
		retryDispatch := continuationDispatchContext(entry.Dispatch, normalizeCompletionContract(model.CompletionContract{
			Mode:        decision.ExpectedOutcome,
			OnMissingPR: dispatchCompletionAction(entry.Dispatch, "missing"),
			OnClosedPR:  dispatchCompletionAction(entry.Dispatch, "closed"),
		}), reason, decision.FinalBranch, decision.PR, issueState)
		o.mu.Lock()
		current := o.state.Recovering[issueID]
		if current != nil {
			delete(o.pendingResume, issueID)
			delete(o.state.Recovering, issueID)
			o.scheduleRetryLocked(issueID, entry.Identifier, 1, nil, true, entry.StallCount, retryDispatch)
			o.commitStateLocked(true)
		}
		o.mu.Unlock()
	default:
		o.mu.Lock()
		current := o.state.Recovering[issueID]
		if current != nil {
			delete(o.pendingResume, issueID)
			delete(o.state.Recovering, issueID)
			o.scheduleRetryLocked(issueID, entry.Identifier, 1, nil, true, entry.StallCount, continuationDispatchContext(entry.Dispatch, normalizeCompletionContract(o.currentWorkflow().Completion), model.ContinuationReasonUnfinishedIssue, entry.FinalBranch, nil, issueState))
			o.commitStateLocked(true)
		}
		o.mu.Unlock()
	}

	return nil
}

func (o *Orchestrator) moveRecoveringIssueToAwaitingIntervention(issueID string, entry model.RecoveryEntry, reason model.ContinuationReason, issueState string) {
	expectedOutcome := model.CompletionMode("")
	var pr *PullRequestInfo
	branch := strings.TrimSpace(entry.FinalBranch)
	if entry.Dispatch != nil {
		expectedOutcome = entry.Dispatch.ExpectedOutcome
		pr = pullRequestInfoFromContext(entry.Dispatch.PreviousPR)
		if branch == "" && entry.Dispatch.PreviousBranch != nil {
			branch = strings.TrimSpace(*entry.Dispatch.PreviousBranch)
		}
	}
	o.moveToAwaitingIntervention(issueID, entry.Identifier, entry.WorkspacePath, branch, entry.RetryAttempt, entry.StallCount, expectedOutcome, string(reason), issueState, pr)
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
	o.mu.Lock()
	if o.isProtectedLocked() {
		o.mu.Unlock()
		return
	}

	now := o.now().UTC()
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
	for issueID, requiredVersion := range o.pendingResume {
		if requiredVersion <= version {
			delete(o.pendingResume, issueID)
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
	clear(o.pendingResume)
	clear(o.pendingActions)
	for issueID := range o.pendingLaunch {
		delete(o.state.Running, issueID)
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
	copyState.Retrying = append([]durableRetryEntry(nil), state.Retrying...)
	copyState.Recovering = append([]durableRecoveryEntry(nil), state.Recovering...)
	copyState.AwaitingMerge = append([]durableAwaitingMergeEntry(nil), state.AwaitingMerge...)
	copyState.AwaitingIntervention = append([]durableAwaitingInterventionEntry(nil), state.AwaitingIntervention...)
	for index := range copyState.Retrying {
		copyState.Retrying[index].Dispatch = cloneDurableDispatchContext(copyState.Retrying[index].Dispatch)
		copyState.Retrying[index].Error = optionalError(pointerString(copyState.Retrying[index].Error))
	}
	for index := range copyState.Recovering {
		copyState.Recovering[index].Dispatch = cloneDurableDispatchContext(copyState.Recovering[index].Dispatch)
	}
	for index := range copyState.AwaitingMerge {
		copyState.AwaitingMerge[index].LastError = optionalError(pointerString(copyState.AwaitingMerge[index].LastError))
		copyState.AwaitingMerge[index].NextPostMergeRetryAt = cloneTimePtr(copyState.AwaitingMerge[index].NextPostMergeRetryAt)
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
