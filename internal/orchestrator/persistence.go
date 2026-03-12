package orchestrator

import (
	"encoding/json"
	"errors"
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
	sessionStateVersion             = 1
	sessionPersistenceRestoreCode   = "session_persistence_restore_failed"
	sessionPersistenceWriteFailCode = "session_persistence_write_failed"
)

type persistedSessionState struct {
	Version              int                                  `json:"Version"`
	SavedAt              time.Time                            `json:"SavedAt"`
	Retrying             []persistedRetryEntry                `json:"Retrying"`
	Claimed              []persistedClaimedEntry              `json:"Claimed"`
	Running              []persistedRunningEntry              `json:"Running"`
	AwaitingMerge        []persistedAwaitingMergeEntry        `json:"AwaitingMerge"`
	AwaitingIntervention []persistedAwaitingInterventionEntry `json:"AwaitingIntervention"`
	Alerts               []AlertSnapshot                      `json:"Alerts"`
	TokenTotal           model.TokenTotals                    `json:"TokenTotal"`
}

type persistedClaimedEntry struct {
	IssueID string `json:"IssueID"`
	model.ClaimedEntry
}

type persistedRetryEntry struct {
	IssueID       string                 `json:"IssueID"`
	Identifier    string                 `json:"Identifier"`
	WorkspacePath string                 `json:"WorkspacePath"`
	Attempt       int                    `json:"Attempt"`
	StallCount    int                    `json:"StallCount"`
	DueAt         time.Time              `json:"DueAt"`
	Error         *string                `json:"Error,omitempty"`
	Dispatch      *model.DispatchContext `json:"Dispatch,omitempty"`
}

type persistedRunningEntry struct {
	IssueID       string                 `json:"IssueID"`
	Identifier    string                 `json:"Identifier"`
	State         string                 `json:"State"`
	WorkspacePath string                 `json:"WorkspacePath"`
	Session       model.LiveSession      `json:"Session"`
	RetryAttempt  int                    `json:"RetryAttempt"`
	StallCount    int                    `json:"StallCount"`
	StartedAt     time.Time              `json:"StartedAt"`
	Dispatch      *model.DispatchContext `json:"Dispatch,omitempty"`
}

type persistedAwaitingMergeEntry struct {
	IssueID string `json:"IssueID"`
	model.AwaitingMergeEntry
}

type persistedAwaitingInterventionEntry struct {
	IssueID string `json:"IssueID"`
	model.AwaitingInterventionEntry
}

type sessionStateWriter struct {
	logger    *slog.Logger
	config    model.SessionPersistenceConfig
	onSuccess func()
	onFailure func(error)

	mu           sync.Mutex
	latest       *persistedSessionState
	forcePending bool
	signalCh     chan struct{}
	closeCh      chan struct{}
}

func newSessionStateWriter(cfg model.SessionPersistenceConfig, logger *slog.Logger, onSuccess func(), onFailure func(error)) *sessionStateWriter {
	if logger == nil {
		logger = slog.Default()
	}
	writer := &sessionStateWriter{
		logger:    logger,
		config:    cfg,
		onSuccess: onSuccess,
		onFailure: onFailure,
		signalCh:  make(chan struct{}, 1),
		closeCh:   make(chan struct{}),
	}
	go writer.loop()
	return writer
}

func (w *sessionStateWriter) Load() (*persistedSessionState, error) {
	raw, err := os.ReadFile(w.config.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var state persistedSessionState
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, err
	}
	if state.Version != sessionStateVersion {
		return nil, errors.New("session persistence version is incompatible")
	}
	return &state, nil
}

func (w *sessionStateWriter) Schedule(state persistedSessionState, critical bool) {
	w.mu.Lock()
	copyState := clonePersistedSessionState(state)
	w.latest = &copyState
	if critical && w.config.FsyncOnCritical {
		w.forcePending = true
	}
	w.mu.Unlock()

	select {
	case w.signalCh <- struct{}{}:
	default:
	}
}

func (w *sessionStateWriter) Close() {
	close(w.closeCh)
}

func (w *sessionStateWriter) loop() {
	var timer *time.Timer
	var timerCh <-chan time.Time

	stopTimer := func() {
		if timer == nil {
			timerCh = nil
			return
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timerCh = nil
	}

	resetTimer := func() {
		delay := time.Duration(maxInt(w.config.FlushIntervalMS, 1)) * time.Millisecond
		if timer == nil {
			timer = time.NewTimer(delay)
		} else {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(delay)
		}
		timerCh = timer.C
	}

	for {
		select {
		case <-w.signalCh:
			w.mu.Lock()
			force := w.forcePending
			hasLatest := w.latest != nil
			w.mu.Unlock()
			if !hasLatest {
				continue
			}
			if force {
				stopTimer()
				w.flush(true)
				continue
			}
			resetTimer()
		case <-timerCh:
			stopTimer()
			w.flush(false)
		case <-w.closeCh:
			stopTimer()
			return
		}
	}
}

func (w *sessionStateWriter) flush(force bool) {
	w.mu.Lock()
	if w.latest == nil {
		if force {
			w.forcePending = false
		}
		w.mu.Unlock()
		return
	}
	state := clonePersistedSessionState(*w.latest)
	w.latest = nil
	if force {
		w.forcePending = false
	}
	w.mu.Unlock()

	err := writePersistedSessionState(w.config.Path, state, force)
	if err != nil {
		if w.onFailure != nil {
			w.onFailure(err)
		}
		return
	}
	if w.onSuccess != nil {
		w.onSuccess()
	}
}

func writePersistedSessionState(path string, state persistedSessionState, force bool) error {
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

func (o *Orchestrator) initRuntimeExtensions() {
	cfg := o.currentConfig()
	if len(cfg.Notifications.Channels) > 0 {
		o.notifier = newAsyncNotifier(cfg.Notifications, o.logger, o.handleNotificationDeliveryResult)
	}
	if !cfg.SessionPersistence.Enabled {
		return
	}

	o.persistence = newSessionStateWriter(cfg.SessionPersistence, o.logger, o.handleSessionPersistenceWriteSuccess, o.handleSessionPersistenceWriteFailure)
	state, err := o.persistence.Load()
	if err != nil {
		o.logger.Warn("session persistence restore failed", "path", cfg.SessionPersistence.Path, "error", err.Error())
		o.setSystemAlertLocked(AlertSnapshot{
			Code:    sessionPersistenceRestoreCode,
			Level:   "warn",
			Message: err.Error(),
		})
		return
	}
	if state == nil {
		return
	}

	o.restorePersistedStateLocked(state)
}

func (o *Orchestrator) scheduleStatePersistLocked(critical bool) {
	if o.persistence == nil {
		return
	}
	o.persistence.Schedule(o.buildPersistedStateLocked(), critical)
}

func (o *Orchestrator) commitStateLocked(critical bool) {
	o.refreshSnapshotLocked()
	o.publishSnapshotLocked()
	o.scheduleStatePersistLocked(critical)
}

func (o *Orchestrator) buildPersistedStateLocked() persistedSessionState {
	state := persistedSessionState{
		Version:    sessionStateVersion,
		SavedAt:    o.now().UTC(),
		TokenTotal: o.state.CodexTotals,
	}

	for issueID, entry := range o.state.Claimed {
		if entry == nil {
			continue
		}
		state.Claimed = append(state.Claimed, persistedClaimedEntry{
			IssueID:      issueID,
			ClaimedEntry: *cloneClaimedEntry(entry),
		})
	}
	sort.SliceStable(state.Claimed, func(i int, j int) bool {
		if state.Claimed[i].Identifier != state.Claimed[j].Identifier {
			return state.Claimed[i].Identifier < state.Claimed[j].Identifier
		}
		return state.Claimed[i].IssueID < state.Claimed[j].IssueID
	})

	for issueID, entry := range o.state.RetryAttempts {
		if entry == nil {
			continue
		}
		state.Retrying = append(state.Retrying, persistedRetryEntry{
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

	for issueID, entry := range o.state.Running {
		if entry == nil {
			continue
		}
		item := persistedRunningEntry{
			IssueID:       issueID,
			Identifier:    entry.Identifier,
			WorkspacePath: entry.WorkspacePath,
			Session:       entry.Session,
			RetryAttempt:  entry.RetryAttempt,
			StallCount:    entry.StallCount,
			StartedAt:     entry.StartedAt,
			Dispatch:      model.CloneDispatchContext(entry.Dispatch),
		}
		if entry.Issue != nil {
			item.State = entry.Issue.State
		}
		state.Running = append(state.Running, item)
	}
	sort.SliceStable(state.Running, func(i int, j int) bool {
		if state.Running[i].Identifier != state.Running[j].Identifier {
			return state.Running[i].Identifier < state.Running[j].Identifier
		}
		return state.Running[i].IssueID < state.Running[j].IssueID
	})

	for issueID, entry := range o.state.AwaitingMerge {
		if entry == nil {
			continue
		}
		state.AwaitingMerge = append(state.AwaitingMerge, persistedAwaitingMergeEntry{
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
		state.AwaitingIntervention = append(state.AwaitingIntervention, persistedAwaitingInterventionEntry{
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

func (o *Orchestrator) restorePersistedStateLocked(state *persistedSessionState) {
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

	claimed := make(map[string]*model.ClaimedEntry, len(state.Claimed))
	for _, item := range state.Claimed {
		claimed[item.IssueID] = cloneClaimedEntry(&item.ClaimedEntry)
	}

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
		if _, exists := claimed[item.IssueID]; !exists {
			claimed[item.IssueID] = &model.ClaimedEntry{
				Identifier:    item.Identifier,
				WorkspacePath: item.WorkspacePath,
				RetryAttempt:  item.Attempt,
				StallCount:    item.StallCount,
				ClaimedAt:     state.SavedAt,
				Dispatch:      model.CloneDispatchContext(item.Dispatch),
			}
		}
	}

	for _, item := range state.Running {
		o.state.Running[item.IssueID] = &model.RunningEntry{
			Issue: &model.Issue{
				ID:         item.IssueID,
				Identifier: item.Identifier,
				Title:      item.Identifier,
				State:      item.State,
			},
			Identifier:    item.Identifier,
			WorkspacePath: item.WorkspacePath,
			Session:       item.Session,
			RetryAttempt:  item.RetryAttempt,
			StallCount:    item.StallCount,
			StartedAt:     item.StartedAt,
			Dispatch:      model.CloneDispatchContext(item.Dispatch),
		}
		if _, exists := claimed[item.IssueID]; !exists {
			claimed[item.IssueID] = &model.ClaimedEntry{
				Identifier:    item.Identifier,
				WorkspacePath: item.WorkspacePath,
				State:         item.State,
				RetryAttempt:  item.RetryAttempt,
				StallCount:    item.StallCount,
				ClaimedAt:     item.StartedAt,
				Dispatch:      model.CloneDispatchContext(item.Dispatch),
			}
		}
	}

	for _, item := range state.AwaitingMerge {
		entry := cloneAwaitingMergeEntry(&item.AwaitingMergeEntry)
		o.state.AwaitingMerge[item.IssueID] = entry
		if _, exists := claimed[item.IssueID]; !exists {
			claimed[item.IssueID] = &model.ClaimedEntry{
				Identifier:    entry.Identifier,
				WorkspacePath: entry.WorkspacePath,
				State:         entry.State,
				RetryAttempt:  entry.RetryAttempt,
				StallCount:    entry.StallCount,
				ClaimedAt:     entry.AwaitingSince,
				Dispatch:      nil,
			}
		}
	}

	for _, item := range state.AwaitingIntervention {
		entry := cloneAwaitingInterventionEntry(&item.AwaitingInterventionEntry)
		o.state.AwaitingIntervention[item.IssueID] = entry
		if _, exists := claimed[item.IssueID]; !exists {
			claimed[item.IssueID] = &model.ClaimedEntry{
				Identifier:    entry.Identifier,
				WorkspacePath: entry.WorkspacePath,
				State:         entry.LastKnownIssueState,
				RetryAttempt:  entry.RetryAttempt,
				StallCount:    entry.StallCount,
				ClaimedAt:     entry.ObservedAt,
				Dispatch:      nil,
			}
		}
	}

	for issueID, entry := range claimed {
		o.state.Claimed[issueID] = cloneClaimedEntry(entry)
		if _, ok := o.state.Running[issueID]; ok {
			continue
		}
		if _, ok := o.state.RetryAttempts[issueID]; ok {
			continue
		}
		if _, ok := o.state.AwaitingMerge[issueID]; ok {
			continue
		}
		if _, ok := o.state.AwaitingIntervention[issueID]; ok {
			continue
		}
		retryEntry := &model.RetryEntry{
			IssueID:       issueID,
			Identifier:    entry.Identifier,
			WorkspacePath: entry.WorkspacePath,
			Attempt:       entry.RetryAttempt,
			StallCount:    entry.StallCount,
			DueAt:         o.now().UTC(),
			Dispatch:      model.CloneDispatchContext(entry.Dispatch),
		}
		retryEntry.TimerHandle = o.newRetryTimer(issueID, retryEntry.DueAt)
		o.state.RetryAttempts[issueID] = retryEntry
	}
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
	if _, exists := o.systemAlerts[sessionPersistenceWriteFailCode]; !exists {
		o.mu.Unlock()
		return
	}
	delete(o.systemAlerts, sessionPersistenceWriteFailCode)
	o.refreshSnapshotLocked()
	o.publishSnapshotLocked()
	o.mu.Unlock()
}

func (o *Orchestrator) handleSessionPersistenceWriteFailure(err error) {
	o.logger.Warn("session persistence write failed", "error", err.Error())
	o.mu.Lock()
	o.systemAlerts[sessionPersistenceWriteFailCode] = AlertSnapshot{
		Code:    sessionPersistenceWriteFailCode,
		Level:   "warn",
		Message: err.Error(),
	}
	o.refreshSnapshotLocked()
	o.publishSnapshotLocked()
	o.mu.Unlock()
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

func clonePersistedSessionState(state persistedSessionState) persistedSessionState {
	copyState := state
	copyState.Alerts = append([]AlertSnapshot(nil), state.Alerts...)
	copyState.Claimed = append([]persistedClaimedEntry(nil), state.Claimed...)
	copyState.Retrying = append([]persistedRetryEntry(nil), state.Retrying...)
	copyState.Running = append([]persistedRunningEntry(nil), state.Running...)
	copyState.AwaitingMerge = append([]persistedAwaitingMergeEntry(nil), state.AwaitingMerge...)
	copyState.AwaitingIntervention = append([]persistedAwaitingInterventionEntry(nil), state.AwaitingIntervention...)
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
