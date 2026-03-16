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
	"strconv"
	"strings"
	"sync"
	"time"

	"symphony-go/internal/model"
	"symphony-go/internal/model/contract"
)

const (
	durableStateVersion             = 7
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
	Version       int                    `json:"version"`
	Identity      RuntimeIdentity        `json:"identity"`
	SavedAt       time.Time              `json:"saved_at"`
	Service       durableServiceMetadata `json:"service"`
	Jobs          []durableJobState      `json:"jobs"`
	FormalObjects ObjectLedgerSnapshot   `json:"formal_objects"`
}

type durablePRContext struct {
	Number     int    `json:"number,omitempty"`
	URL        string `json:"url,omitempty"`
	State      string `json:"state,omitempty"`
	Merged     bool   `json:"merged,omitempty"`
	HeadBranch string `json:"head_branch,omitempty"`
}

type durableDispatchContext struct {
	JobType            string                       `json:"job_type,omitempty"`
	Kind               string                       `json:"kind,omitempty"`
	RetryAttempt       *int                         `json:"retry_attempt,omitempty"`
	ExpectedOutcome    string                       `json:"expected_outcome,omitempty"`
	OnMissingPR        string                       `json:"on_missing_pr,omitempty"`
	OnClosedPR         string                       `json:"on_closed_pr,omitempty"`
	Reason             *string                      `json:"reason,omitempty"`
	PreviousBranch     *string                      `json:"previous_branch,omitempty"`
	PreviousPR         *durablePRContext            `json:"previous_pr,omitempty"`
	PreviousIssueState *string                      `json:"previous_issue_state,omitempty"`
	RecoveryCheckpoint *model.RecoveryCheckpoint    `json:"recovery_checkpoint,omitempty"`
	ReviewFeedback     *model.ReviewFeedbackContext `json:"review_feedback,omitempty"`
}

type durableServiceMetadata struct {
	TokenTotal               model.TokenTotals `json:"token_total"`
	NotificationFingerprints []string          `json:"notification_fingerprints,omitempty"`
}

type durableRunRuntimeState struct {
	RunID          string                    `json:"run_id,omitempty"`
	ReviewSummary  string                    `json:"review_summary,omitempty"`
	ReviewFindings []model.ReviewFinding     `json:"review_findings,omitempty"`
	Budget         model.RunBudget           `json:"budget,omitempty"`
	Usage          model.RunBudgetUsage      `json:"usage,omitempty"`
	Recovery       *model.RecoveryCheckpoint `json:"recovery,omitempty"`
}

type durableInterventionRuntimeState struct {
	InterventionID string                    `json:"intervention_id,omitempty"`
	Handoff        model.InterventionHandoff `json:"handoff,omitempty"`
}

type durableJobState struct {
	JobID            string                           `json:"job_id"`
	Reason           *contract.Reason                 `json:"reason,omitempty"`
	UpdatedAt        string                           `json:"updated_at,omitempty"`
	WorkspacePath    string                           `json:"workspace_path,omitempty"`
	PullRequestState string                           `json:"pull_request_state,omitempty"`
	RetryDueAt       *string                          `json:"retry_due_at,omitempty"`
	RetryAttempt     int                              `json:"retry_attempt,omitempty"`
	StallCount       int                              `json:"stall_count,omitempty"`
	NeedsRecovery    bool                             `json:"needs_recovery,omitempty"`
	Dispatch         *durableDispatchContext          `json:"dispatch,omitempty"`
	Run              *durableRunRuntimeState          `json:"run,omitempty"`
	Intervention     *durableInterventionRuntimeState `json:"intervention,omitempty"`
	OutcomeID        string                           `json:"outcome_id,omitempty"`
	ArtifactIDs      []string                         `json:"artifact_ids,omitempty"`
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

	tempFile, err := os.CreateTemp(dir, "runtime-state-*.tmp")
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
	o.syncFormalObjectsLocked()
	o.refreshSnapshotLocked()
	o.publishSnapshotLocked()
	o.publishFormalEventsLocked()
	if o.isProtectedLocked() {
		return 0
	}
	return o.scheduleStatePersistLocked(critical)
}

func (o *Orchestrator) publishViewLocked() {
	o.syncFormalObjectsLocked()
	o.refreshSnapshotLocked()
	o.publishSnapshotLocked()
	o.publishFormalEventsLocked()
}

func (o *Orchestrator) buildPersistedStateLocked() durableRuntimeState {
	state := durableRuntimeState{
		Version:  durableStateVersion,
		Identity: o.currentRuntimeIdentity(),
		SavedAt:  o.now().UTC(),
		Service: durableServiceMetadata{
			TokenTotal:               o.state.CodexTotals,
			NotificationFingerprints: durableNotificationFingerprints(o.emittedNotificationKeys),
		},
	}
	for _, record := range o.state.Jobs {
		if record == nil {
			continue
		}
		state.Jobs = append(state.Jobs, o.durableJobStateFromRuntime(record))
	}
	for _, record := range o.state.ArchivedJobs {
		state.Jobs = append(state.Jobs, o.durableJobStateFromArchived(record))
	}
	o.ensureObjectLedgerLocked()
	if o.objectLedger != nil {
		state.FormalObjects = o.objectLedger.Snapshot()
	}
	sort.SliceStable(state.Jobs, func(i int, j int) bool {
		left := strings.TrimSpace(state.Jobs[i].JobID)
		right := strings.TrimSpace(state.Jobs[j].JobID)
		if left != right {
			return left < right
		}
		return strings.TrimSpace(state.Jobs[i].UpdatedAt) < strings.TrimSpace(state.Jobs[j].UpdatedAt)
	})
	return state
}

func (o *Orchestrator) durableJobStateFromRuntime(record *model.JobRuntime) durableJobState {
	item := durableJobState{
		JobID:         strings.TrimSpace(record.Object.ID),
		Reason:        cloneReason(record.Reason),
		UpdatedAt:     record.UpdatedAt,
		WorkspacePath: recordWorkspacePath(record),
		RetryAttempt:  record.RetryAttempt,
		StallCount:    record.StallCount,
		NeedsRecovery: record.NeedsRecovery || isRecordRunning(record),
		Dispatch:      durableDispatchFromModel(record.Dispatch),
		Run:           durableRunRuntimeFromModel(record.Run),
		Intervention:  durableInterventionRuntimeFromModel(record.Intervention),
	}
	if record.Outcome != nil {
		item.OutcomeID = strings.TrimSpace(record.Outcome.ID)
	}
	if pr := recordPullRequest(record); pr != nil {
		item.PullRequestState = string(pr.State)
	}
	if len(record.Artifacts) > 0 {
		item.ArtifactIDs = make([]string, 0, len(record.Artifacts))
		for _, artifact := range record.Artifacts {
			if id := strings.TrimSpace(artifact.ID); id != "" {
				item.ArtifactIDs = append(item.ArtifactIDs, id)
			}
		}
	}
	if record.RetryDueAt != nil {
		value := record.RetryDueAt.UTC().Format(time.RFC3339Nano)
		item.RetryDueAt = &value
	}
	return item
}

func (o *Orchestrator) durableJobStateFromArchived(record model.ArchivedJob) durableJobState {
	runtime := &model.JobRuntime{
		Object:           record.Object,
		Lifecycle:        model.JobLifecycleCompleted,
		SourceState:      record.SourceState,
		Reason:           cloneReason(record.Reason),
		UpdatedAt:        record.UpdatedAt,
		WorkspacePath:    record.WorkspacePath,
		PullRequestState: record.PullRequestState,
		Dispatch:         model.CloneDispatchContext(record.Dispatch),
		Run:              model.CloneRunState(record.Run),
		Intervention:     model.CloneInterventionState(record.Intervention),
		Outcome:          cloneOutcome(record.Outcome),
		Artifacts:        cloneArtifacts(record.Artifacts),
	}
	return o.durableJobStateFromRuntime(runtime)
}

func durableRunRuntimeFromModel(value *model.RunState) *durableRunRuntimeState {
	if value == nil {
		return nil
	}
	return &durableRunRuntimeState{
		RunID:          strings.TrimSpace(value.Object.ID),
		ReviewSummary:  value.ReviewSummary,
		ReviewFindings: cloneRuntimeReviewFindings(value.ReviewFindings),
		Budget:         value.Budget,
		Usage:          value.Usage,
		Recovery:       model.CloneRecoveryCheckpoint(value.Recovery),
	}
}

func durableInterventionRuntimeFromModel(value *model.InterventionState) *durableInterventionRuntimeState {
	if value == nil {
		return nil
	}
	return &durableInterventionRuntimeState{
		InterventionID: strings.TrimSpace(value.Object.ID),
		Handoff:        cloneRuntimeInterventionHandoff(value.Handoff),
	}
}

func sourceRefFromFormalObjects(job contract.Job) contract.SourceRef {
	for _, reference := range job.References {
		switch reference.Type {
		case contract.ReferenceTypeLinearIssue:
			return contract.SourceRef{
				SourceKind:       contract.SourceKindLinear,
				SourceName:       strings.TrimSpace(reference.DisplayName),
				SourceID:         strings.TrimSpace(reference.ExternalID),
				SourceIdentifier: strings.TrimSpace(reference.Locator),
				URL:              strings.TrimSpace(reference.URL),
			}
		}
	}
	return contract.SourceRef{}
}

func issueFromSourceRef(ref contract.SourceRef) *model.Issue {
	sourceID := strings.TrimSpace(ref.SourceID)
	identifier := strings.TrimSpace(ref.SourceIdentifier)
	if sourceID == "" && identifier == "" {
		return nil
	}
	issue := &model.Issue{
		ID:         sourceID,
		Identifier: identifier,
	}
	if url := strings.TrimSpace(ref.URL); url != "" {
		issue.URL = &url
	}
	return issue
}

func branchNameFromFormalObjects(record *model.JobRuntime) string {
	for _, reference := range referencesFromFormalObjects(record) {
		if reference.Type == contract.ReferenceTypeGitBranch {
			return strings.TrimSpace(reference.Locator)
		}
	}
	return ""
}

func pullRequestRefFromFormalObjects(record *model.JobRuntime) *contract.PullRequestRef {
	for _, reference := range referencesFromFormalObjects(record) {
		if reference.Type != contract.ReferenceTypeGitHubPullRequest {
			continue
		}
		ref := &contract.PullRequestRef{
			URL: strings.TrimSpace(reference.URL),
		}
		if number, err := strconv.Atoi(strings.TrimSpace(reference.ExternalID)); err == nil {
			ref.Number = number
		}
		return ref
	}
	return nil
}

func referencesFromFormalObjects(record *model.JobRuntime) []contract.Reference {
	if record == nil {
		return nil
	}
	merged := make([]contract.Reference, 0, len(record.Object.References)+8)
	seen := map[string]struct{}{}
	appendReferences := func(items []contract.Reference) {
		for _, reference := range items {
			key := strings.TrimSpace(reference.ID)
			if key == "" {
				key = fmt.Sprintf("%s:%s:%s", reference.Type, reference.System, reference.Locator)
			}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			merged = append(merged, reference)
		}
	}
	appendReferences(record.Object.References)
	if record.Run != nil {
		appendReferences(record.Run.Object.References)
	}
	if record.Intervention != nil {
		appendReferences(record.Intervention.Object.References)
	}
	if record.Outcome != nil {
		appendReferences(record.Outcome.References)
	}
	for _, artifact := range record.Artifacts {
		appendReferences(artifact.References)
	}
	return merged
}

func (o *Orchestrator) runtimeFromDurableJob(item durableJobState) *model.JobRuntime {
	record := &model.JobRuntime{
		Reason:           cloneReason(item.Reason),
		UpdatedAt:        item.UpdatedAt,
		WorkspacePath:    strings.TrimSpace(item.WorkspacePath),
		PullRequestState: strings.TrimSpace(item.PullRequestState),
		RetryAttempt:     item.RetryAttempt,
		StallCount:       item.StallCount,
		NeedsRecovery:    item.NeedsRecovery,
		Dispatch:         durableDispatchToModel(item.Dispatch),
	}
	if job, ok := o.jobObjectByIDLocked(item.JobID); ok {
		record.Object = job
	}
	if item.Run != nil {
		runState := &model.RunState{
			ReviewSummary:  item.Run.ReviewSummary,
			ReviewFindings: cloneRuntimeReviewFindings(item.Run.ReviewFindings),
			Budget:         item.Run.Budget,
			Usage:          item.Run.Usage,
			Recovery:       model.CloneRecoveryCheckpoint(item.Run.Recovery),
		}
		if run, ok := o.runObjectByIDLocked(item.Run.RunID); ok {
			runState.Object = run
		}
		syncRunStateMirrorsFromObject(runState)
		record.Run = runState
	}
	if item.Intervention != nil {
		interventionState := &model.InterventionState{
			Handoff: cloneRuntimeInterventionHandoff(item.Intervention.Handoff),
		}
		if intervention, ok := o.interventionObjectByIDLocked(item.Intervention.InterventionID); ok {
			interventionState.Object = intervention
		}
		record.Intervention = interventionState
	}
	if item.OutcomeID != "" {
		if outcome, ok := o.outcomeObjectByIDLocked(item.OutcomeID); ok {
			record.Outcome = &outcome
		}
	}
	if len(item.ArtifactIDs) > 0 {
		record.Artifacts = make([]contract.Artifact, 0, len(item.ArtifactIDs))
		for _, artifactID := range item.ArtifactIDs {
			if artifact, ok := o.artifactObjectByIDLocked(artifactID); ok {
				record.Artifacts = append(record.Artifacts, artifact)
			}
		}
	}
	record.SourceState = ""
	if item.RetryDueAt != nil {
		if dueAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(*item.RetryDueAt)); err == nil {
			record.RetryDueAt = &dueAt
		}
	}
	record.Lifecycle = deriveLifecycleFromRuntime(record)
	return record
}

func deriveLifecycleFromRuntime(record *model.JobRuntime) model.JobLifecycleState {
	switch {
	case record == nil:
		return model.JobLifecycleActive
	case record.Outcome != nil || record.Object.State.IsTerminal():
		return model.JobLifecycleCompleted
	case record.Intervention != nil && record.Intervention.Object.State == contract.InterventionStatusOpen:
		return model.JobLifecycleAwaitingIntervention
	case record.Run != nil && record.Run.State == contract.RunStatusContinuationPending:
		return model.JobLifecycleRetryScheduled
	case record.Run != nil && record.Run.State == contract.RunStatusCompleted && recordPullRequest(record) != nil:
		return model.JobLifecycleAwaitingMerge
	case record.Reason != nil && (record.Reason.ReasonCode == contract.ReasonRecordBlockedAwaitingIntervention || record.Reason.ReasonCode == contract.ReasonRecordBlockedRecoveryUncertain):
		return model.JobLifecycleAwaitingIntervention
	default:
		return model.JobLifecycleActive
	}
}

func (o *Orchestrator) restorePersistedStateLocked(state *durableRuntimeState) {
	if state == nil {
		return
	}

	o.state.CodexTotals = state.Service.TokenTotal
	o.emittedNotificationKeys = notificationFingerprintSet(state.Service.NotificationFingerprints)
	o.state.Jobs = map[string]*model.JobRuntime{}
	o.activeRuns = map[string]*model.JobRuntime{}
	o.continuationRuns = map[string]*model.JobRuntime{}
	o.candidateDeliveryJobs = map[string]*model.JobRuntime{}
	o.interventionJobs = map[string]*model.JobRuntime{}
	o.state.ArchivedJobs = nil
	o.restoreObjectLedgerSnapshotLocked(state.FormalObjects)

	for _, item := range state.Jobs {
		record := o.runtimeFromDurableJob(item)
		issueID := recordSourceID(record)
		if issueID == "" {
			continue
		}
		if record.Outcome != nil || record.Object.State.IsTerminal() {
			o.rememberCompletedLocked(record)
			continue
		}
		if record.Run != nil && record.Run.State == contract.RunStatusRunning {
			record.Observation = &contract.Observation{
				Running: false,
				Summary: "restored from ledger; awaiting recovery evaluation",
			}
		}
		if record.Run != nil && record.Run.State == contract.RunStatusContinuationPending && record.RetryDueAt != nil {
			record.RetryTimer = o.newContinuationTimer(issueID, *record.RetryDueAt)
		}
		o.state.Jobs[issueID] = record
	}
	o.rebuildRuntimeIndexesLocked()
}

func (o *Orchestrator) reconcileRecovering(ctx context.Context) {
	o.mu.RLock()
	protected := o.isProtectedLocked()
	pending := make(map[string]*model.JobRuntime)
	for issueID, record := range o.state.Jobs {
		if record == nil || jobStateFromRecord(record) != contract.JobStatusRunning || !record.NeedsRecovery {
			continue
		}
		if _, waiting := o.pendingRecovery[issueID]; waiting {
			continue
		}
		pending[issueID] = cloneJobRuntime(record)
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
			o.moveToAwaitingIntervention(issueID, recordIdentifier(entry), recordWorkspacePath(entry), recordBranch(entry), entry.RetryAttempt, entry.StallCount, o.currentWorkflow().Completion.Mode, string(model.ContinuationReasonTrackerIssueMissing), entry.SourceState, recordPullRequest(entry))
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
	for _, record := range o.state.Jobs {
		if record != nil && jobStateFromRecord(record) == contract.JobStatusRunning && record.NeedsRecovery {
			remaining = true
			break
		}
	}
	o.state.Service.RecoveryInProgress = remaining
	o.mu.Unlock()
}

func (o *Orchestrator) resumeRecoveredSuccessPath(ctx context.Context, issueID string, entry *model.JobRuntime, issueState string) error {
	if entry == nil {
		return nil
	}
	decision, err := o.classifySuccessfulRun(ctx, recordWorkspacePath(entry), recordBranch(entry), entry.Dispatch, issueState)
	if err != nil {
		o.logger.Warn("ledger recovery classification failed", "issue_id", issueID, "issue_identifier", recordIdentifier(entry), "branch", recordBranch(entry), "error", err.Error())
		o.moveToAwaitingIntervention(issueID, recordIdentifier(entry), recordWorkspacePath(entry), recordBranch(entry), entry.RetryAttempt, entry.StallCount, o.currentWorkflow().Completion.Mode, "recovery_uncertain", issueState, recordPullRequest(entry))
		return err
	}

	if entry.Run != nil {
		if decision.Disposition == DispositionAwaitingMerge {
			o.markRunCandidateDelivery(entry, decision.PR)
		} else if decision.Disposition == DispositionCompleted {
			switch jobTypeForDispatch(entry.Dispatch) {
			case contract.JobTypeCodeChange, contract.JobTypeLandChange:
				o.markRunCandidateDelivery(entry, decision.PR)
			case contract.JobTypeAnalysis, contract.JobTypeDiagnostic:
				o.markRunCandidateDelivery(entry, nil)
			}
		}
		if entry.Run.CandidateDelivery != nil && entry.Run.CandidateDelivery.Reached && entry.Run.ReviewGate != nil &&
			entry.Run.ReviewGate.Status != contract.ReviewGateStatusPassed &&
			entry.Run.ReviewGate.Status != contract.ReviewGateStatusInterventionRequired {
			reviewIssue := recordSourceIssue(entry)
			if reviewIssue == nil {
				reviewIssue = &model.Issue{
					ID:         issueID,
					Identifier: recordIdentifier(entry),
					Title:      recordIdentifier(entry),
					State:      issueState,
				}
			}
			workerCtx, cancel := context.WithCancel(o.runtimeContext())
			o.mu.Lock()
			current := o.state.Jobs[issueID]
			if current == nil {
				o.mu.Unlock()
				cancel()
				return nil
			}
			startedAt := o.now().UTC()
			current.StartedAt = &startedAt
			current.WorkerCancel = cancel
			current.NeedsRecovery = false
			current.Dispatch = model.CloneDispatchContext(entry.Dispatch)
			current.Run = model.CloneRunState(entry.Run)
			current.SourceState = issueState
			o.syncRecordFormalReferencesLocked(current, &model.Issue{ID: issueID, Identifier: recordIdentifier(entry), State: issueState}, recordBranch(current), recordPullRequest(current))
			o.openRunReviewGate(current)
			o.setRecordStatusLocked(current, model.JobLifecycleActive, nil, &contract.Observation{
				Running: true,
				Summary: "review in progress",
			})
			snapshot := cloneJobRuntime(current)
			version := o.commitStateLocked(true)
			action := func() {
				o.launchReviewWorker(workerCtx, *reviewIssue, snapshot)
			}
			if version > 0 {
				o.schedulePersistedActionLocked(version, action)
			}
			o.mu.Unlock()
			if version == 0 {
				action()
			}
			return nil
		}
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
		checkpointDetails := jobIdentityDetails(entry)
		checkpointDetails["cause"] = reason
		checkpointReason := contract.MustReason(contract.ReasonRunContinuationPending, checkpointDetails)
		checkpoint, checkpointErr := o.captureCheckpointFn(ctx, entry, contract.RunPhaseSummaryPublishing, &checkpointReason)
		if checkpointErr != nil {
			o.moveToAwaitingIntervention(issueID, recordIdentifier(entry), recordWorkspacePath(entry), recordBranch(entry), entry.RetryAttempt, entry.StallCount, decision.ExpectedOutcome, "recovery_uncertain", issueState, decision.PR)
			return checkpointErr
		}
		retryDispatch := continuationDispatchContext(baseDispatch, normalizeCompletionContract(model.CompletionContract{
			Mode:        decision.ExpectedOutcome,
			OnMissingPR: dispatchCompletionAction(baseDispatch, "missing"),
			OnClosedPR:  dispatchCompletionAction(baseDispatch, "closed"),
		}), reason, decision.FinalBranch, decision.PR, issueState)
		retryDispatch.RecoveryCheckpoint = model.CloneRecoveryCheckpoint(checkpoint)
		o.mu.Lock()
		current := o.state.Jobs[issueID]
		if current != nil {
			current.NeedsRecovery = false
			nextAttempt := entry.RetryAttempt + 1
			if nextAttempt <= 0 {
				nextAttempt = 1
			}
			o.scheduleContinuationLocked(issueID, recordIdentifier(entry), nextAttempt, nil, entry.StallCount, retryDispatch)
			o.commitStateLocked(true)
		}
		o.mu.Unlock()
	default:
		o.moveToAwaitingIntervention(issueID, recordIdentifier(entry), recordWorkspacePath(entry), recordBranch(entry), entry.RetryAttempt, entry.StallCount, o.currentWorkflow().Completion.Mode, "recovery_uncertain", issueState, recordPullRequest(entry))
	}

	return nil
}

func (o *Orchestrator) newContinuationTimer(issueID string, dueAt time.Time) *time.Timer {
	delay := dueAt.Sub(o.now().UTC())
	if delay < 0 {
		delay = 0
	}
	return time.AfterFunc(delay, func() {
		select {
		case o.continuationCh <- issueID:
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
	clear(o.pendingLaunch)
	o.rebuildRuntimeIndexesLocked()
	if o.stateStore != nil {
		o.stateStore.Disable()
	}
	o.enterProtectedModeLocked(errorText)
	o.publishViewLocked()
}

func cloneDurableRuntimeState(state durableRuntimeState) durableRuntimeState {
	copyState := state
	copyState.Service.NotificationFingerprints = append([]string(nil), state.Service.NotificationFingerprints...)
	copyState.Jobs = make([]durableJobState, 0, len(state.Jobs))
	for _, item := range state.Jobs {
		cloned := item
		cloned.Reason = cloneReason(item.Reason)
		cloned.Dispatch = cloneDurableDispatchContext(item.Dispatch)
		cloned.Run = cloneDurableRunRuntimeState(item.Run)
		cloned.Intervention = cloneDurableInterventionRuntimeState(item.Intervention)
		cloned.ArtifactIDs = append([]string(nil), item.ArtifactIDs...)
		if item.RetryDueAt != nil {
			value := strings.TrimSpace(*item.RetryDueAt)
			cloned.RetryDueAt = &value
		}
		copyState.Jobs = append(copyState.Jobs, cloned)
	}
	copyState.FormalObjects = cloneObjectLedgerSnapshot(state.FormalObjects)
	return copyState
}

func durableNotificationFingerprints(values map[string]struct{}) []string {
	if len(values) == 0 {
		return nil
	}
	result := make([]string, 0, len(values))
	for value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func notificationFingerprintSet(values []string) map[string]struct{} {
	result := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		result[value] = struct{}{}
	}
	return result
}

func cloneDurableRunRuntimeState(value *durableRunRuntimeState) *durableRunRuntimeState {
	if value == nil {
		return nil
	}
	copyValue := *value
	copyValue.ReviewFindings = cloneRuntimeReviewFindings(value.ReviewFindings)
	copyValue.Recovery = model.CloneRecoveryCheckpoint(value.Recovery)
	return &copyValue
}

func cloneDurableInterventionRuntimeState(value *durableInterventionRuntimeState) *durableInterventionRuntimeState {
	if value == nil {
		return nil
	}
	copyValue := *value
	copyValue.Handoff = cloneRuntimeInterventionHandoff(value.Handoff)
	return &copyValue
}

func cloneRuntimeReviewFindings(value []model.ReviewFinding) []model.ReviewFinding {
	if len(value) == 0 {
		return nil
	}
	result := make([]model.ReviewFinding, len(value))
	copy(result, value)
	return result
}

func cloneRuntimeInterventionHandoff(value model.InterventionHandoff) model.InterventionHandoff {
	copyValue := value
	copyValue.Reason = cloneReason(value.Reason)
	copyValue.Decision = cloneDecision(value.Decision)
	if value.Checkpoint != nil {
		checkpoint := *value.Checkpoint
		checkpoint.ArtifactIDs = append([]string(nil), value.Checkpoint.ArtifactIDs...)
		checkpoint.ReferenceIDs = append([]string(nil), value.Checkpoint.ReferenceIDs...)
		checkpoint.Reason = cloneReason(value.Checkpoint.Reason)
		copyValue.Checkpoint = &checkpoint
	}
	copyValue.ReviewFindings = cloneRuntimeReviewFindings(value.ReviewFindings)
	copyValue.RecommendedActions = append([]contract.DecisionAction(nil), value.RecommendedActions...)
	copyValue.RequiredInputs = cloneInterventionTemplateInputs(value.RequiredInputs)
	return copyValue
}

func cloneOutcome(value *contract.Outcome) *contract.Outcome {
	if value == nil {
		return nil
	}
	copyValue := *value
	copyValue.Relations = append([]contract.ObjectRelation(nil), value.Relations...)
	copyValue.References = append([]contract.Reference(nil), value.References...)
	if len(value.Reasons) > 0 {
		copyValue.Reasons = make([]contract.Reason, 0, len(value.Reasons))
		for _, reason := range value.Reasons {
			cloned := reason
			cloned.Details = cloneAnyMap(reason.Details)
			copyValue.Reasons = append(copyValue.Reasons, cloned)
		}
	}
	copyValue.Decision = cloneDecision(value.Decision)
	return &copyValue
}

func cloneArtifacts(value []contract.Artifact) []contract.Artifact {
	if len(value) == 0 {
		return nil
	}
	result := make([]contract.Artifact, 0, len(value))
	for _, item := range value {
		cloned := item
		cloned.Relations = append([]contract.ObjectRelation(nil), item.Relations...)
		cloned.References = append([]contract.Reference(nil), item.References...)
		if len(item.Reasons) > 0 {
			cloned.Reasons = make([]contract.Reason, 0, len(item.Reasons))
			for _, reason := range item.Reasons {
				copyReason := reason
				copyReason.Details = cloneAnyMap(reason.Details)
				cloned.Reasons = append(cloned.Reasons, copyReason)
			}
		}
		cloned.Decision = cloneDecision(item.Decision)
		result = append(result, cloned)
	}
	return result
}

func cloneDecision(value *contract.Decision) *contract.Decision {
	if value == nil {
		return nil
	}
	copyValue := *value
	copyValue.Details = cloneAnyMap(value.Details)
	copyValue.RecommendedActions = append([]contract.DecisionAction(nil), value.RecommendedActions...)
	return &copyValue
}

func cloneAnyMap(value map[string]any) map[string]any {
	if len(value) == 0 {
		return nil
	}
	result := make(map[string]any, len(value))
	for key, item := range value {
		result[key] = item
	}
	return result
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
	copyValue.RecoveryCheckpoint = model.CloneRecoveryCheckpoint(dispatch.RecoveryCheckpoint)
	copyValue.ReviewFeedback = cloneReviewFeedbackContext(dispatch.ReviewFeedback)
	return &copyValue
}

func durableDispatchFromModel(dispatch *model.DispatchContext) *durableDispatchContext {
	if dispatch == nil {
		return nil
	}
	result := &durableDispatchContext{
		JobType:         string(dispatch.JobType),
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
	result.RecoveryCheckpoint = model.CloneRecoveryCheckpoint(dispatch.RecoveryCheckpoint)
	result.ReviewFeedback = cloneReviewFeedbackContext(dispatch.ReviewFeedback)
	return result
}

func durableDispatchToModel(dispatch *durableDispatchContext) *model.DispatchContext {
	if dispatch == nil {
		return nil
	}
	result := &model.DispatchContext{
		JobType:         contract.JobType(strings.TrimSpace(dispatch.JobType)),
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
	result.RecoveryCheckpoint = model.CloneRecoveryCheckpoint(dispatch.RecoveryCheckpoint)
	result.ReviewFeedback = cloneReviewFeedbackContext(dispatch.ReviewFeedback)
	return result
}

func cloneReviewFeedbackContext(value *model.ReviewFeedbackContext) *model.ReviewFeedbackContext {
	if value == nil {
		return nil
	}
	copyValue := *value
	copyValue.Findings = append([]model.ReviewFinding(nil), value.Findings...)
	return &copyValue
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
