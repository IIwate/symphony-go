package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"symphony-go/internal/model"
	"symphony-go/internal/model/contract"
	"symphony-go/internal/tracker"
)

type LedgerStorageTier string

const (
	LedgerStorageTierHot     LedgerStorageTier = "hot"
	LedgerStorageTierArchive LedgerStorageTier = "archive"
)

type LedgerLifecycleState string

const (
	LedgerLifecycleActive      LedgerLifecycleState = "active"
	LedgerLifecycleTerminated  LedgerLifecycleState = "terminated"
	LedgerLifecycleInvalidated LedgerLifecycleState = "invalidated"
	LedgerLifecycleArchived    LedgerLifecycleState = "archived"
)

type ObjectEnvelope struct {
	ObjectType    contract.ObjectType  `json:"object_type"`
	ObjectID      string               `json:"object_id"`
	StorageTier   LedgerStorageTier    `json:"storage_tier"`
	Lifecycle     LedgerLifecycleState `json:"lifecycle"`
	UpdatedAt     string               `json:"updated_at"`
	ArchivedAt    *string              `json:"archived_at,omitempty"`
	InvalidatedAt *string              `json:"invalidated_at,omitempty"`
	TerminatedAt  *string              `json:"terminated_at,omitempty"`
	Payload       json.RawMessage      `json:"payload"`
}

type ObjectLedgerSnapshot struct {
	Records []ObjectEnvelope `json:"records"`
}

func cloneObjectLedgerSnapshot(value ObjectLedgerSnapshot) ObjectLedgerSnapshot {
	records := make([]ObjectEnvelope, 0, len(value.Records))
	for _, item := range value.Records {
		cloned := item
		cloned.Payload = append([]byte(nil), item.Payload...)
		records = append(records, cloned)
	}
	return ObjectLedgerSnapshot{Records: records}
}

type ObjectLedger interface {
	UpsertJob(contract.Job) error
	UpsertRun(contract.Run) error
	UpsertIntervention(contract.Intervention) error
	UpsertOutcome(contract.Outcome) error
	UpsertArtifact(contract.Artifact) error
	UpsertAction(contract.Action) error
	UpsertInstance(contract.Instance) error
	UpsertReference(contract.Reference) error
	Archive(contract.ObjectType, string, string) error
	Invalidate(contract.ObjectType, string, string) error
	Terminate(contract.ObjectType, string, string) error
	Get(contract.ObjectType, string) (ObjectEnvelope, bool)
	Snapshot() ObjectLedgerSnapshot
}

type fileObjectLedger struct {
	path string
	mu   sync.Mutex
	data map[string]ObjectEnvelope
}

type memoryObjectLedger struct {
	mu   sync.Mutex
	data map[string]ObjectEnvelope
}

func NewFileObjectLedger(path string) (ObjectLedger, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("object ledger path is required")
	}
	ledger := &fileObjectLedger{
		path: path,
		data: map[string]ObjectEnvelope{},
	}
	if err := ledger.load(); err != nil {
		return nil, err
	}
	return ledger, nil
}

func NewMemoryObjectLedger(snapshot ObjectLedgerSnapshot) ObjectLedger {
	ledger := &memoryObjectLedger{
		data: map[string]ObjectEnvelope{},
	}
	for _, item := range snapshot.Records {
		ledger.data[ledgerKey(item.ObjectType, item.ObjectID)] = item
	}
	return ledger
}

func (l *fileObjectLedger) UpsertJob(value contract.Job) error {
	return l.upsert(value.ObjectType, value.ID, value.UpdatedAt, value)
}
func (l *memoryObjectLedger) UpsertJob(value contract.Job) error {
	return l.upsert(value.ObjectType, value.ID, value.UpdatedAt, value)
}
func (l *fileObjectLedger) UpsertRun(value contract.Run) error {
	return l.upsert(value.ObjectType, value.ID, value.UpdatedAt, value)
}
func (l *memoryObjectLedger) UpsertRun(value contract.Run) error {
	return l.upsert(value.ObjectType, value.ID, value.UpdatedAt, value)
}
func (l *fileObjectLedger) UpsertIntervention(value contract.Intervention) error {
	return l.upsert(value.ObjectType, value.ID, value.UpdatedAt, value)
}
func (l *memoryObjectLedger) UpsertIntervention(value contract.Intervention) error {
	return l.upsert(value.ObjectType, value.ID, value.UpdatedAt, value)
}
func (l *fileObjectLedger) UpsertOutcome(value contract.Outcome) error {
	return l.upsert(value.ObjectType, value.ID, value.UpdatedAt, value)
}
func (l *memoryObjectLedger) UpsertOutcome(value contract.Outcome) error {
	return l.upsert(value.ObjectType, value.ID, value.UpdatedAt, value)
}
func (l *fileObjectLedger) UpsertArtifact(value contract.Artifact) error {
	return l.upsert(value.ObjectType, value.ID, value.UpdatedAt, value)
}
func (l *memoryObjectLedger) UpsertArtifact(value contract.Artifact) error {
	return l.upsert(value.ObjectType, value.ID, value.UpdatedAt, value)
}
func (l *fileObjectLedger) UpsertAction(value contract.Action) error {
	return l.upsert(value.ObjectType, value.ID, value.UpdatedAt, value)
}
func (l *memoryObjectLedger) UpsertAction(value contract.Action) error {
	return l.upsert(value.ObjectType, value.ID, value.UpdatedAt, value)
}
func (l *fileObjectLedger) UpsertInstance(value contract.Instance) error {
	return l.upsert(value.ObjectType, value.ID, value.UpdatedAt, value)
}
func (l *memoryObjectLedger) UpsertInstance(value contract.Instance) error {
	return l.upsert(value.ObjectType, value.ID, value.UpdatedAt, value)
}
func (l *fileObjectLedger) UpsertReference(value contract.Reference) error {
	return l.upsert(value.ObjectType, value.ID, value.UpdatedAt, value)
}
func (l *memoryObjectLedger) UpsertReference(value contract.Reference) error {
	return l.upsert(value.ObjectType, value.ID, value.UpdatedAt, value)
}

func (l *fileObjectLedger) Archive(objectType contract.ObjectType, id string, at string) error {
	return l.markLifecycle(objectType, id, LedgerStorageTierArchive, LedgerLifecycleArchived, at)
}
func (l *memoryObjectLedger) Archive(objectType contract.ObjectType, id string, at string) error {
	return l.markLifecycle(objectType, id, LedgerStorageTierArchive, LedgerLifecycleArchived, at)
}

func (l *fileObjectLedger) Invalidate(objectType contract.ObjectType, id string, at string) error {
	return l.markLifecycle(objectType, id, "", LedgerLifecycleInvalidated, at)
}
func (l *memoryObjectLedger) Invalidate(objectType contract.ObjectType, id string, at string) error {
	return l.markLifecycle(objectType, id, "", LedgerLifecycleInvalidated, at)
}

func (l *fileObjectLedger) Terminate(objectType contract.ObjectType, id string, at string) error {
	return l.markLifecycle(objectType, id, "", LedgerLifecycleTerminated, at)
}
func (l *memoryObjectLedger) Terminate(objectType contract.ObjectType, id string, at string) error {
	return l.markLifecycle(objectType, id, "", LedgerLifecycleTerminated, at)
}

func (l *fileObjectLedger) Get(objectType contract.ObjectType, id string) (ObjectEnvelope, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	item, ok := l.data[ledgerKey(objectType, id)]
	return item, ok
}
func (l *memoryObjectLedger) Get(objectType contract.ObjectType, id string) (ObjectEnvelope, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	item, ok := l.data[ledgerKey(objectType, id)]
	return item, ok
}

func (l *fileObjectLedger) Snapshot() ObjectLedgerSnapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	records := make([]ObjectEnvelope, 0, len(l.data))
	for _, item := range l.data {
		records = append(records, item)
	}
	return ObjectLedgerSnapshot{Records: records}
}
func (l *memoryObjectLedger) Snapshot() ObjectLedgerSnapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	records := make([]ObjectEnvelope, 0, len(l.data))
	for _, item := range l.data {
		records = append(records, item)
	}
	return ObjectLedgerSnapshot{Records: records}
}

func (l *fileObjectLedger) upsert(objectType contract.ObjectType, id string, updatedAt string, value any) error {
	if !objectType.IsValid() {
		return fmt.Errorf("invalid object type %q", objectType)
	}
	if id == "" {
		return fmt.Errorf("object id is required")
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	existing, ok := l.data[ledgerKey(objectType, id)]
	if ok {
		existing.Payload = payload
		existing.UpdatedAt = updatedAt
		if existing.Lifecycle == "" {
			existing.Lifecycle = LedgerLifecycleActive
		}
		if existing.StorageTier == "" {
			existing.StorageTier = LedgerStorageTierHot
		}
		l.data[ledgerKey(objectType, id)] = existing
	} else {
		l.data[ledgerKey(objectType, id)] = ObjectEnvelope{
			ObjectType:  objectType,
			ObjectID:    id,
			StorageTier: LedgerStorageTierHot,
			Lifecycle:   LedgerLifecycleActive,
			UpdatedAt:   updatedAt,
			Payload:     payload,
		}
	}
	return l.flushLocked()
}
func (l *memoryObjectLedger) upsert(objectType contract.ObjectType, id string, updatedAt string, value any) error {
	if !objectType.IsValid() {
		return fmt.Errorf("invalid object type %q", objectType)
	}
	if id == "" {
		return fmt.Errorf("object id is required")
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	existing, ok := l.data[ledgerKey(objectType, id)]
	if ok {
		existing.Payload = payload
		existing.UpdatedAt = updatedAt
		if existing.Lifecycle == "" {
			existing.Lifecycle = LedgerLifecycleActive
		}
		if existing.StorageTier == "" {
			existing.StorageTier = LedgerStorageTierHot
		}
		l.data[ledgerKey(objectType, id)] = existing
	} else {
		l.data[ledgerKey(objectType, id)] = ObjectEnvelope{
			ObjectType:  objectType,
			ObjectID:    id,
			StorageTier: LedgerStorageTierHot,
			Lifecycle:   LedgerLifecycleActive,
			UpdatedAt:   updatedAt,
			Payload:     payload,
		}
	}
	return nil
}

func (l *fileObjectLedger) markLifecycle(objectType contract.ObjectType, id string, tier LedgerStorageTier, lifecycle LedgerLifecycleState, at string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	key := ledgerKey(objectType, id)
	item, ok := l.data[key]
	if !ok {
		return fmt.Errorf("object %s/%s not found", objectType, id)
	}
	if tier != "" {
		item.StorageTier = tier
	}
	item.Lifecycle = lifecycle
	item.UpdatedAt = at
	switch lifecycle {
	case LedgerLifecycleArchived:
		item.ArchivedAt = stringPtr(at)
	case LedgerLifecycleInvalidated:
		item.InvalidatedAt = stringPtr(at)
	case LedgerLifecycleTerminated:
		item.TerminatedAt = stringPtr(at)
	}
	l.data[key] = item
	return l.flushLocked()
}
func (l *memoryObjectLedger) markLifecycle(objectType contract.ObjectType, id string, tier LedgerStorageTier, lifecycle LedgerLifecycleState, at string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	key := ledgerKey(objectType, id)
	item, ok := l.data[key]
	if !ok {
		return fmt.Errorf("object %s/%s not found", objectType, id)
	}
	if tier != "" {
		item.StorageTier = tier
	}
	item.Lifecycle = lifecycle
	item.UpdatedAt = at
	switch lifecycle {
	case LedgerLifecycleArchived:
		item.ArchivedAt = stringPtr(at)
	case LedgerLifecycleInvalidated:
		item.InvalidatedAt = stringPtr(at)
	case LedgerLifecycleTerminated:
		item.TerminatedAt = stringPtr(at)
	}
	l.data[key] = item
	return nil
}

func (l *fileObjectLedger) load() error {
	if _, err := os.Stat(l.path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	raw, err := os.ReadFile(l.path)
	if err != nil {
		return err
	}
	if len(raw) == 0 {
		return nil
	}
	var snapshot ObjectLedgerSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return err
	}
	for _, item := range snapshot.Records {
		l.data[ledgerKey(item.ObjectType, item.ObjectID)] = item
	}
	return nil
}

func (l *fileObjectLedger) flushLocked() error {
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return err
	}
	snapshot := ObjectLedgerSnapshot{Records: make([]ObjectEnvelope, 0, len(l.data))}
	for _, item := range l.data {
		snapshot.Records = append(snapshot.Records, item)
	}
	raw, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(l.path, raw, 0o644)
}

func ledgerKey(objectType contract.ObjectType, id string) string {
	return string(objectType) + "/" + id
}

func stringPtr(value string) *string {
	copyValue := value
	return &copyValue
}

func objectLedgerPathForConfig(cfg *model.ServiceConfig) string {
	if cfg == nil {
		return filepath.Join(".", "local", "object-ledger.json")
	}
	if path := strings.TrimSpace(cfg.SessionPersistence.File.Path); path != "" {
		return filepath.Join(filepath.Dir(path), "object-ledger.json")
	}
	if root := strings.TrimSpace(cfg.AutomationRootDir); root != "" {
		return filepath.Join(root, "local", "object-ledger.json")
	}
	return filepath.Join(".", "local", "object-ledger.json")
}

func (o *Orchestrator) ensureObjectLedgerLocked() {
	if o.objectLedger != nil {
		return
	}
	o.objectLedger = NewMemoryObjectLedger(ObjectLedgerSnapshot{})
	o.restoreObjectLedgerStateLocked()
}

func (o *Orchestrator) restoreObjectLedgerStateLocked() {
	if o.objectLedgerRestored || o.objectLedger == nil {
		return
	}
	for _, item := range o.objectLedger.Snapshot().Records {
		if item.ObjectType != contract.ObjectTypeAction {
			continue
		}
		var action contract.Action
		if err := json.Unmarshal(item.Payload, &action); err != nil {
			continue
		}
		if action.Type != contract.ActionTypeSourceClosure || isTerminalActionState(action.State) {
			continue
		}
		if state, ok := sourceClosureActionStateFromAction(action); ok {
			o.sourceClosureActions[action.ID] = state
		}
	}
	o.objectLedgerRestored = true
}

func (o *Orchestrator) restoreObjectLedgerSnapshotLocked(snapshot ObjectLedgerSnapshot) {
	o.objectLedger = NewMemoryObjectLedger(cloneObjectLedgerSnapshot(snapshot))
	o.objectLedgerRestored = false
	clear(o.sourceClosureActions)
	o.restoreObjectLedgerStateLocked()
}

func (o *Orchestrator) GetObject(objectType contract.ObjectType, id string) (ObjectEnvelope, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.ensureObjectLedgerLocked()
	if o.objectLedger == nil {
		return ObjectEnvelope{}, false
	}
	return o.objectLedger.Get(objectType, strings.TrimSpace(id))
}

func decodeObjectEnvelope[T any](envelope ObjectEnvelope) (T, bool) {
	var payload T
	if len(envelope.Payload) == 0 {
		return payload, false
	}
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		return payload, false
	}
	return payload, true
}

func (o *Orchestrator) jobObjectByIDLocked(jobID string) (contract.Job, bool) {
	if o.objectLedger == nil || strings.TrimSpace(jobID) == "" {
		return contract.Job{}, false
	}
	envelope, ok := o.objectLedger.Get(contract.ObjectTypeJob, strings.TrimSpace(jobID))
	if !ok {
		return contract.Job{}, false
	}
	return decodeObjectEnvelope[contract.Job](envelope)
}

func (o *Orchestrator) runObjectByIDLocked(runID string) (contract.Run, bool) {
	if o.objectLedger == nil || strings.TrimSpace(runID) == "" {
		return contract.Run{}, false
	}
	envelope, ok := o.objectLedger.Get(contract.ObjectTypeRun, strings.TrimSpace(runID))
	if !ok {
		return contract.Run{}, false
	}
	return decodeObjectEnvelope[contract.Run](envelope)
}

func (o *Orchestrator) interventionObjectByIDLocked(interventionID string) (contract.Intervention, bool) {
	if o.objectLedger == nil || strings.TrimSpace(interventionID) == "" {
		return contract.Intervention{}, false
	}
	envelope, ok := o.objectLedger.Get(contract.ObjectTypeIntervention, strings.TrimSpace(interventionID))
	if !ok {
		return contract.Intervention{}, false
	}
	return decodeObjectEnvelope[contract.Intervention](envelope)
}

func (o *Orchestrator) outcomeObjectByIDLocked(outcomeID string) (contract.Outcome, bool) {
	if o.objectLedger == nil || strings.TrimSpace(outcomeID) == "" {
		return contract.Outcome{}, false
	}
	envelope, ok := o.objectLedger.Get(contract.ObjectTypeOutcome, strings.TrimSpace(outcomeID))
	if !ok {
		return contract.Outcome{}, false
	}
	return decodeObjectEnvelope[contract.Outcome](envelope)
}

func (o *Orchestrator) artifactObjectByIDLocked(artifactID string) (contract.Artifact, bool) {
	if o.objectLedger == nil || strings.TrimSpace(artifactID) == "" {
		return contract.Artifact{}, false
	}
	envelope, ok := o.objectLedger.Get(contract.ObjectTypeArtifact, strings.TrimSpace(artifactID))
	if !ok {
		return contract.Artifact{}, false
	}
	return decodeObjectEnvelope[contract.Artifact](envelope)
}

func (o *Orchestrator) ListObjects(objectType contract.ObjectType) []ObjectEnvelope {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.ensureObjectLedgerLocked()
	if o.objectLedger == nil {
		return nil
	}
	snapshot := o.objectLedger.Snapshot()
	records := make([]ObjectEnvelope, 0, len(snapshot.Records))
	for _, item := range snapshot.Records {
		if objectType != "" && item.ObjectType != objectType {
			continue
		}
		records = append(records, item)
	}
	return records
}

func (o *Orchestrator) SubscribeEvents(buffer int) (<-chan contract.EventEnvelope, func()) {
	if buffer <= 0 {
		buffer = 1
	}
	ch := make(chan contract.EventEnvelope, buffer)

	o.mu.Lock()
	id := o.nextEventSubscriberID
	o.nextEventSubscriberID++
	o.ensureObjectLedgerLocked()
	o.refreshSnapshotLocked()
	event := o.buildSnapshotEventLocked()
	o.eventSubscribers[id] = ch
	o.mu.Unlock()

	select {
	case ch <- event:
	default:
	}

	unsubscribe := func() {
		o.mu.Lock()
		if existing, ok := o.eventSubscribers[id]; ok {
			delete(o.eventSubscribers, id)
			close(existing)
		}
		o.mu.Unlock()
	}

	return ch, unsubscribe
}

func (o *Orchestrator) buildSnapshotEventLocked() contract.EventEnvelope {
	return contract.EventEnvelope{
		EventID:         o.nextEventID(),
		EventType:       contract.EventTypeSnapshot,
		Timestamp:       o.snapshot.GeneratedAt,
		ContractVersion: contract.APIVersionV1,
		DomainID:        strings.TrimSpace(o.currentConfig().DomainID),
		ServiceMode:     o.snapshot.ServiceMode,
		Reason:          firstReasonPtr(o.snapshot.Reasons),
		Objects:         o.currentEventObjectsLocked(),
	}
}

func (o *Orchestrator) currentEventObjectsLocked() []contract.EventObject {
	if o.objectLedger == nil {
		return nil
	}
	snapshot := o.objectLedger.Snapshot()
	objects := make([]contract.EventObject, 0, len(snapshot.Records))
	for _, item := range snapshot.Records {
		objects = append(objects, eventObjectFromEnvelope(item))
	}
	return objects
}

func eventObjectFromEnvelope(item ObjectEnvelope) contract.EventObject {
	payload := struct {
		State      string                   `json:"state"`
		Visibility contract.VisibilityLevel `json:"visibility"`
	}{}
	_ = json.Unmarshal(item.Payload, &payload)
	return contract.EventObject{
		ObjectType: item.ObjectType,
		ObjectID:   item.ObjectID,
		State:      payload.State,
		Visibility: payload.Visibility,
	}
}

func firstReasonPtr(reasons []contract.Reason) *contract.Reason {
	if len(reasons) == 0 {
		return nil
	}
	copyValue := reasons[0]
	return &copyValue
}

func isTerminalActionState(state contract.ActionStatus) bool {
	switch state {
	case contract.ActionStatusCompleted, contract.ActionStatusFailed, contract.ActionStatusCanceled:
		return true
	default:
		return false
	}
}

func sourceClosureActionStateFromAction(action contract.Action) (*sourceClosureActionState, bool) {
	if action.Type != contract.ActionTypeSourceClosure {
		return nil, false
	}
	state := &sourceClosureActionState{Action: action}
	if symphony, ok := action.Extensions[contract.ExtensionNamespace("symphony")]; ok {
		if jobID, ok := symphony["job_id"].(string); ok {
			state.JobID = strings.TrimSpace(jobID)
		}
	}
	for _, ref := range action.References {
		if ref.Type != contract.ReferenceTypeLinearIssue {
			continue
		}
		state.SourceIssue = model.Issue{
			ID:         ref.ExternalID,
			Identifier: ref.DisplayName,
		}
		if ref.URL != "" {
			url := ref.URL
			state.SourceIssue.URL = &url
		}
		break
	}
	if state.JobID == "" || strings.TrimSpace(state.SourceIssue.ID) == "" {
		return nil, false
	}
	return state, true
}

func (o *Orchestrator) syncFormalObjectsLocked() {
	o.ensureObjectLedgerLocked()
	if o.objectLedger == nil {
		return
	}
	o.upsertInstanceObjectLocked()
	jobs := map[string]struct{}{}
	for _, actionState := range o.sourceClosureActions {
		if actionState == nil {
			continue
		}
		o.upsertSourceClosureActionLocked(actionState)
		if actionState.JobID != "" {
			jobs[actionState.JobID] = struct{}{}
		}
	}
	for jobID := range jobs {
		o.updateJobActionSummaryLocked(jobID)
	}
}

func (o *Orchestrator) loadJobObjectLocked(record *model.JobRuntime) contract.Job {
	if record == nil {
		return contract.Job{}
	}
	o.ensureObjectLedgerLocked()
	jobID := strings.TrimSpace(record.Object.ID)
	if jobID == "" {
		jobID = jobIDForRecord(record)
	}
	if job, ok := o.jobObjectByIDLocked(jobID); ok {
		record.Object = job
		return job
	}
	job := o.jobObjectFromRecord(record, contract.JobStatusQueued)
	record.Object = job
	return job
}

func (o *Orchestrator) storeJobObjectLocked(record *model.JobRuntime, job contract.Job) {
	o.ensureObjectLedgerLocked()
	if record == nil || o.objectLedger == nil {
		return
	}
	if strings.TrimSpace(job.ID) == "" {
		return
	}
	o.upsertReferencesLocked(job.References)
	if err := o.objectLedger.UpsertJob(job); err != nil {
		o.logger.Warn("upsert job object failed", "job_id", job.ID, "error", err.Error())
	}
	record.Object = job
	o.updateJobActionSummaryLocked(job.ID)
}

func (o *Orchestrator) mutateJobObjectLocked(record *model.JobRuntime, mutate func(*contract.Job)) {
	if record == nil {
		return
	}
	job := o.loadJobObjectLocked(record)
	if mutate != nil {
		mutate(&job)
	}
	o.storeJobObjectLocked(record, job)
}

func (o *Orchestrator) persistRunObjectLocked(record *model.JobRuntime) {
	o.ensureObjectLedgerLocked()
	if record == nil || record.Run == nil || o.objectLedger == nil {
		return
	}
	run := record.Run.Object
	if strings.TrimSpace(run.ID) == "" {
		return
	}
	o.upsertReferencesLocked(run.References)
	if err := o.objectLedger.UpsertRun(run); err != nil {
		o.logger.Warn("upsert run object failed", "run_id", run.ID, "error", err.Error())
	}
}

func (o *Orchestrator) persistInterventionObjectLocked(record *model.JobRuntime) {
	o.ensureObjectLedgerLocked()
	if record == nil || record.Intervention == nil || o.objectLedger == nil {
		return
	}
	intervention := record.Intervention.Object
	if strings.TrimSpace(intervention.ID) == "" {
		return
	}
	o.upsertReferencesLocked(intervention.References)
	if err := o.objectLedger.UpsertIntervention(intervention); err != nil {
		o.logger.Warn("upsert intervention object failed", "intervention_id", intervention.ID, "error", err.Error())
	}
}

func (o *Orchestrator) persistOutcomeObjectLocked(record *model.JobRuntime) {
	o.ensureObjectLedgerLocked()
	if record == nil || record.Outcome == nil || o.objectLedger == nil {
		return
	}
	o.upsertReferencesLocked(record.Outcome.References)
	if err := o.objectLedger.UpsertOutcome(*record.Outcome); err != nil {
		o.logger.Warn("upsert outcome object failed", "outcome_id", record.Outcome.ID, "error", err.Error())
	}
}

func (o *Orchestrator) persistArtifactObjectsLocked(record *model.JobRuntime) {
	o.ensureObjectLedgerLocked()
	if record == nil || o.objectLedger == nil {
		return
	}
	for _, artifact := range record.Artifacts {
		if strings.TrimSpace(artifact.ID) == "" {
			continue
		}
		o.upsertReferencesLocked(artifact.References)
		if err := o.objectLedger.UpsertArtifact(artifact); err != nil {
			o.logger.Warn("upsert artifact failed", "artifact_id", artifact.ID, "error", err.Error())
		}
	}
}

func (o *Orchestrator) upsertInstanceObjectLocked() {
	if o.objectLedger == nil {
		return
	}
	cfg := o.currentConfig()
	if cfg == nil {
		return
	}
	now := o.now().UTC().Format(time.RFC3339Nano)
	serviceMode, _, reasons := o.publicServiceStateLocked()
	instanceID := discoveryInstanceID(o.currentRuntimeIdentity())
	instance := contract.Instance{
		BaseObject: o.baseObjectLocked(contract.ObjectTypeInstance, instanceID, contract.VisibilityLevelSummary, now),
		ObjectContext: contract.ObjectContext{
			Reasons: reasons,
		},
		State:                 serviceMode,
		Name:                  strings.TrimSpace(cfg.ServiceInstanceName),
		Version:               o.serviceVersion,
		Role:                  currentInstanceRole(cfg),
		StaticCapabilities:    cfg.CapabilityContract.Static,
		AvailableCapabilities: o.currentAvailableCapabilitiesLocked(),
	}
	if instance.Name == "" {
		instance.Name = "symphony"
	}
	if err := o.objectLedger.UpsertInstance(instance); err != nil {
		o.logger.Warn("upsert instance object failed", "instance_id", instance.ID, "error", err.Error())
	}
}

func (o *Orchestrator) currentAvailableCapabilitiesLocked() contract.AvailableCapabilitySet {
	cfg := o.currentConfig()
	if cfg == nil {
		return contract.AvailableCapabilitySet{}
	}
	available := make([]contract.AvailableCapability, 0, len(cfg.CapabilityContract.Available.Capabilities))
	role := currentInstanceRole(cfg)
	sourceClosure := tracker.SourceClosureAvailability{}
	if o.tracker != nil {
		sourceClosure = o.tracker.SourceClosureAvailability(context.Background())
	}
	for _, item := range cfg.CapabilityContract.Available.Capabilities {
		copyItem := item
		copyItem.Reasons = cloneServiceReasons(copyItem.Reasons)
		switch copyItem.Name {
		case contract.CapabilityServiceRefresh:
			if cfg.LeaderRequired && role != contract.InstanceRoleLeader {
				copyItem.Available = false
				copyItem.Reasons = []contract.Reason{
					contract.MustReason(contract.ReasonCapabilityCurrentlyUnavailable, map[string]any{
						"capability": string(copyItem.Name),
						"role":       role,
					}),
				}
			}
		case contract.CapabilitySourceClosure:
			if cfg.LeaderRequired && role != contract.InstanceRoleLeader {
				copyItem.Available = false
				copyItem.Reasons = []contract.Reason{
					contract.MustReason(contract.ReasonCapabilityCurrentlyUnavailable, map[string]any{
						"capability": string(copyItem.Name),
						"role":       role,
					}),
				}
				break
			}
			copyItem.Available = sourceClosure.Supported && sourceClosure.Available
			copyItem.Reasons = cloneServiceReasons(sourceClosure.Reasons)
		}
		available = append(available, copyItem)
	}
	return contract.AvailableCapabilitySet{Capabilities: available}
}

func currentInstanceRole(cfg *model.ServiceConfig) contract.InstanceRole {
	if cfg != nil && cfg.InstanceRole.IsValid() {
		return cfg.InstanceRole
	}
	return contract.InstanceRoleLeader
}

func objectIdentityKeyForRecord(record *model.JobRuntime) string {
	if record == nil {
		return "unknown"
	}
	if jobID := strings.TrimSpace(record.Object.ID); strings.HasPrefix(jobID, "job-") && len(jobID) > len("job-") {
		return strings.TrimPrefix(jobID, "job-")
	}
	if jobID := strings.TrimSpace(record.Object.ID); jobID != "" {
		return sanitizeObjectIDToken(jobID, "unknown")
	}
	return "unknown"
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func jobIDForRecord(record *model.JobRuntime) string {
	if record == nil {
		return "job-unknown"
	}
	if jobID := strings.TrimSpace(record.Object.ID); jobID != "" {
		return jobID
	}
	return "job-" + objectIdentityKeyForRecord(record)
}

func runIDForRecord(record *model.JobRuntime, attempt int) string {
	return fmt.Sprintf("run-%s-%d", objectIdentityKeyForRecord(record), attempt)
}

func interventionIDForRecord(record *model.JobRuntime) string {
	attempt := currentAttempt(record)
	if record.Run != nil {
		attempt = record.Run.Attempt
	}
	return fmt.Sprintf("intervention-%s-%d", objectIdentityKeyForRecord(record), attempt)
}

func outcomeIDForRecord(record *model.JobRuntime) string {
	return "outcome-" + objectIdentityKeyForRecord(record)
}

func artifactIDForRecord(record *model.JobRuntime, suffix string) string {
	return fmt.Sprintf("artifact-%s-%s", objectIdentityKeyForRecord(record), suffix)
}

func sourceClosureActionIDForRecord(record *model.JobRuntime) string {
	return "action-" + objectIdentityKeyForRecord(record) + "-source-closure"
}

func sourceReferenceIDForRecord(record *model.JobRuntime) string {
	return "ref-" + objectIdentityKeyForRecord(record) + "-source"
}

func branchReferenceIDForRecord(record *model.JobRuntime) string {
	return "ref-" + objectIdentityKeyForRecord(record) + "-branch"
}

func pullRequestReferenceIDForRecord(record *model.JobRuntime) string {
	return "ref-" + objectIdentityKeyForRecord(record) + "-pr"
}

func runtimeFromArchivedJobForLedger(record model.ArchivedJob) *model.JobRuntime {
	return &model.JobRuntime{
		Object:           record.Object,
		Lifecycle:        model.JobLifecycleCompleted,
		Reason:           cloneReason(record.Reason),
		Observation:      cloneObservation(record.Observation),
		UpdatedAt:        record.UpdatedAt,
		WorkspacePath:    record.WorkspacePath,
		SourceState:      record.SourceState,
		PullRequestState: record.PullRequestState,
		Dispatch:         model.CloneDispatchContext(record.Dispatch),
		Run:              model.CloneRunState(record.Run),
		Intervention:     model.CloneInterventionState(record.Intervention),
		Outcome:          cloneOutcome(record.Outcome),
		Artifacts:        cloneArtifacts(record.Artifacts),
	}
}

func (o *Orchestrator) baseObjectLocked(objectType contract.ObjectType, id string, visibility contract.VisibilityLevel, updatedAt string) contract.BaseObject {
	createdAt := updatedAt
	if o.objectLedger != nil {
		if envelope, ok := o.objectLedger.Get(objectType, id); ok {
			payload := contract.BaseObject{}
			if err := json.Unmarshal(envelope.Payload, &payload); err == nil && strings.TrimSpace(payload.CreatedAt) != "" {
				createdAt = payload.CreatedAt
			}
		}
	}
	cfg := o.currentConfig()
	version := contract.APIVersionV1
	domainID := ""
	if cfg != nil {
		if cfg.ServiceContractVersion != "" {
			version = cfg.ServiceContractVersion
		}
		domainID = strings.TrimSpace(cfg.DomainID)
	}
	return contract.BaseObject{
		ID:              id,
		ObjectType:      objectType,
		DomainID:        domainID,
		Visibility:      visibility,
		ContractVersion: version,
		CreatedAt:       createdAt,
		UpdatedAt:       updatedAt,
	}
}

func jobStateFromRecord(record *model.JobRuntime) contract.JobStatus {
	if record == nil {
		return contract.JobStatusQueued
	}
	switch {
	case record.Outcome != nil:
		return jobStateFromOutcomeConclusion(record.Outcome.State)
	case record.Intervention != nil && record.Intervention.Object.State == contract.InterventionStatusOpen:
		return contract.JobStatusInterventionRequired
	case record.Run != nil && record.Run.Object.State == contract.RunStatusQueued:
		return contract.JobStatusQueued
	default:
		return contract.JobStatusRunning
	}
}

func jobStateFromOutcome(outcome contract.ResultOutcome) contract.JobStatus {
	switch outcome {
	case contract.ResultOutcomeSucceeded:
		return contract.JobStatusCompleted
	case contract.ResultOutcomeFailed:
		return contract.JobStatusFailed
	case contract.ResultOutcomeAbandoned:
		return contract.JobStatusAbandoned
	default:
		return contract.JobStatusCompleted
	}
}

func jobStateFromOutcomeConclusion(outcome contract.OutcomeConclusion) contract.JobStatus {
	switch outcome {
	case contract.OutcomeConclusionSucceeded:
		return contract.JobStatusCompleted
	case contract.OutcomeConclusionFailed:
		return contract.JobStatusFailed
	case contract.OutcomeConclusionCanceled:
		return contract.JobStatusCanceled
	case contract.OutcomeConclusionRejected:
		return contract.JobStatusRejected
	case contract.OutcomeConclusionAbandoned:
		return contract.JobStatusAbandoned
	default:
		return contract.JobStatusCompleted
	}
}

func outcomeConclusionFromResult(outcome contract.ResultOutcome) contract.OutcomeConclusion {
	switch outcome {
	case contract.ResultOutcomeSucceeded:
		return contract.OutcomeConclusionSucceeded
	case contract.ResultOutcomeFailed:
		return contract.OutcomeConclusionFailed
	case contract.ResultOutcomeAbandoned:
		return contract.OutcomeConclusionAbandoned
	default:
		return contract.OutcomeConclusionSucceeded
	}
}

func (o *Orchestrator) upsertReferencesLocked(references []contract.Reference) {
	if o.objectLedger == nil {
		return
	}
	for _, reference := range references {
		if err := o.objectLedger.UpsertReference(reference); err != nil {
			o.logger.Warn("upsert reference object failed", "reference_id", reference.ID, "error", err.Error())
		}
	}
}

func (o *Orchestrator) actionObjectsForJobLocked(jobID string) []contract.Action {
	if o.objectLedger == nil || strings.TrimSpace(jobID) == "" {
		return nil
	}
	snapshot := o.objectLedger.Snapshot()
	actions := make([]contract.Action, 0, len(snapshot.Records))
	for _, item := range snapshot.Records {
		if item.ObjectType != contract.ObjectTypeAction {
			continue
		}
		var action contract.Action
		if err := json.Unmarshal(item.Payload, &action); err != nil {
			continue
		}
		state, ok := sourceClosureActionStateFromAction(action)
		if !ok || state.JobID != jobID {
			continue
		}
		actions = append(actions, action)
	}
	return actions
}

func (o *Orchestrator) actionSummaryForJobLocked(jobID string) contract.ActionSummary {
	summary := contract.ActionSummary{}
	for _, action := range o.actionObjectsForJobLocked(jobID) {
		if action.State != contract.ActionStatusExternalPending {
			continue
		}
		summary.HasPendingExternalActions = true
		summary.PendingCount++
		summary.PendingTypes = append(summary.PendingTypes, action.Type)
	}
	return summary
}

func (o *Orchestrator) jobObjectFromRecord(record *model.JobRuntime, state contract.JobStatus) contract.Job {
	updatedAt := strings.TrimSpace(record.UpdatedAt)
	if updatedAt == "" {
		updatedAt = o.now().UTC().Format(time.RFC3339Nano)
	}
	relations := []contract.ObjectRelation{}
	if record.Run != nil {
		relations = append(relations, contract.ObjectRelation{Type: contract.RelationTypeJobRun, TargetID: runIDForRecord(record, record.Run.Attempt), TargetType: contract.ObjectTypeRun})
	}
	for _, action := range o.actionObjectsForJobLocked(jobIDForRecord(record)) {
		relations = append(relations, contract.ObjectRelation{Type: contract.RelationTypeJobAction, TargetID: action.ID, TargetType: contract.ObjectTypeAction})
	}
	ctx := contract.ObjectContext{
		Relations:  relations,
		References: append([]contract.Reference(nil), record.Object.References...),
	}
	if record.Reason != nil {
		ctx.Reasons = []contract.Reason{*cloneReason(record.Reason)}
	}
	return contract.Job{
		BaseObject:    o.baseObjectLocked(contract.ObjectTypeJob, jobIDForRecord(record), contract.VisibilityLevelRestricted, updatedAt),
		ObjectContext: ctx,
		State:         state,
		JobType:       jobTypeForDispatch(record.Dispatch),
		ActionSummary: o.actionSummaryForJobLocked(jobIDForRecord(record)),
	}
}

func (o *Orchestrator) upsertSourceClosureActionLocked(state *sourceClosureActionState) {
	if state == nil || o.objectLedger == nil {
		return
	}
	action := state.Action
	o.upsertReferencesLocked(action.References)
	relations := make([]contract.ObjectRelation, 0, len(action.Relations)+len(action.References))
	for _, relation := range action.Relations {
		if relation.Type == contract.RelationTypeActionReference {
			continue
		}
		relations = append(relations, relation)
	}
	for _, reference := range action.References {
		relations = append(relations, contract.ObjectRelation{
			Type:       contract.RelationTypeActionReference,
			TargetID:   reference.ID,
			TargetType: contract.ObjectTypeReference,
		})
	}
	action.Relations = relations
	state.Action = action
	if err := o.objectLedger.UpsertAction(action); err != nil {
		o.logger.Warn("upsert action object failed", "action_id", state.Action.ID, "error", err.Error())
	}
}

func (o *Orchestrator) updateJobActionSummaryLocked(jobID string) {
	if o.objectLedger == nil || strings.TrimSpace(jobID) == "" {
		return
	}
	envelope, ok := o.objectLedger.Get(contract.ObjectTypeJob, jobID)
	if !ok {
		return
	}
	var job contract.Job
	if err := json.Unmarshal(envelope.Payload, &job); err != nil {
		return
	}
	job.ActionSummary = o.actionSummaryForJobLocked(jobID)
	job.UpdatedAt = o.now().UTC().Format(time.RFC3339Nano)
	relations := make([]contract.ObjectRelation, 0, len(job.Relations))
	for _, relation := range job.Relations {
		if relation.Type == contract.RelationTypeJobAction {
			continue
		}
		relations = append(relations, relation)
	}
	for _, action := range o.actionObjectsForJobLocked(jobID) {
		relations = append(relations, contract.ObjectRelation{Type: contract.RelationTypeJobAction, TargetID: action.ID, TargetType: contract.ObjectTypeAction})
	}
	job.Relations = relations
	if err := o.objectLedger.UpsertJob(job); err != nil {
		o.logger.Warn("update job action summary failed", "job_id", job.ID, "error", err.Error())
	}
}

func (o *Orchestrator) publishFormalEventsLocked() {
	currentObjects := map[string]ObjectEnvelope{}
	if o.objectLedger != nil {
		for _, item := range o.objectLedger.Snapshot().Records {
			currentObjects[ledgerKey(item.ObjectType, item.ObjectID)] = item
		}
	}
	if !o.eventStateInitialized {
		o.lastEventSnapshot = o.snapshot
		o.lastEventObjects = currentObjects
		o.eventStateInitialized = true
		return
	}
	if !reflect.DeepEqual(o.lastEventSnapshot, o.snapshot) {
		o.broadcastEventLocked(contract.EventEnvelope{
			EventID:         o.nextEventID(),
			EventType:       contract.EventTypeServiceStateChanged,
			Timestamp:       o.snapshot.GeneratedAt,
			ContractVersion: contract.APIVersionV1,
			DomainID:        strings.TrimSpace(o.currentConfig().DomainID),
			ServiceMode:     o.snapshot.ServiceMode,
			Reason:          firstReasonPtr(o.snapshot.Reasons),
		})
	}
	if changed := changedEventObjects(currentObjects, o.lastEventObjects); len(changed) > 0 {
		o.broadcastEventLocked(contract.EventEnvelope{
			EventID:         o.nextEventID(),
			EventType:       contract.EventTypeObjectChanged,
			Timestamp:       o.snapshot.GeneratedAt,
			ContractVersion: contract.APIVersionV1,
			DomainID:        strings.TrimSpace(o.currentConfig().DomainID),
			ServiceMode:     o.snapshot.ServiceMode,
			Objects:         changed,
		})
	}
	o.lastEventSnapshot = o.snapshot
	o.lastEventObjects = currentObjects
}

func changedEventObjects(current map[string]ObjectEnvelope, previous map[string]ObjectEnvelope) []contract.EventObject {
	keys := make([]string, 0, len(current)+len(previous))
	seen := map[string]struct{}{}
	for key := range current {
		keys = append(keys, key)
		seen[key] = struct{}{}
	}
	for key := range previous {
		if _, ok := seen[key]; ok {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	objects := make([]contract.EventObject, 0, len(keys))
	for _, key := range keys {
		currentItem, currentOK := current[key]
		previousItem, previousOK := previous[key]
		if currentOK && previousOK && reflect.DeepEqual(currentItem, previousItem) {
			continue
		}
		if currentOK {
			objects = append(objects, eventObjectFromEnvelope(currentItem))
			continue
		}
		objects = append(objects, eventObjectFromEnvelope(previousItem))
	}
	return objects
}

func (o *Orchestrator) broadcastEventLocked(event contract.EventEnvelope) {
	for id, subscriber := range o.eventSubscribers {
		select {
		case subscriber <- event:
		default:
			delete(o.eventSubscribers, id)
			close(subscriber)
		}
	}
}

func (o *Orchestrator) requiresSourceClosureAction(record *model.JobRuntime) bool {
	if record == nil {
		return false
	}
	if jobTypeForDispatch(record.Dispatch) != contract.JobTypeLandChange {
		return false
	}
	pr := recordPullRequest(record)
	if pr == nil || pr.State != PullRequestStateMerged {
		return false
	}
	return !o.isTerminalState(strings.TrimSpace(record.SourceState), o.currentConfig())
}

func (o *Orchestrator) ensureSourceClosureActionLocked(record *model.JobRuntime) {
	if record == nil {
		return
	}
	actionID := sourceClosureActionIDForRecord(record)
	if _, ok := o.sourceClosureActions[actionID]; ok {
		return
	}
	updatedAt := ""
	if record.Outcome != nil {
		updatedAt = strings.TrimSpace(record.Outcome.CompletedAt)
	}
	if updatedAt == "" {
		updatedAt = strings.TrimSpace(record.UpdatedAt)
	}
	if updatedAt == "" {
		updatedAt = o.now().UTC().Format(time.RFC3339Nano)
	}
	sourceIssue := recordSourceIssue(record)
	if sourceIssue == nil {
		sourceIssue = &model.Issue{}
	}
	action := contract.Action{
		BaseObject: o.baseObjectLocked(contract.ObjectTypeAction, actionID, contract.VisibilityLevelRestricted, updatedAt),
		ObjectContext: contract.ObjectContext{
			Reasons: []contract.Reason{
				contract.MustReason(contract.ReasonActionSourceClosurePending, map[string]any{
					"job_id":            jobIDForRecord(record),
					"source_id":         sourceIssue.ID,
					"source_identifier": sourceIssue.Identifier,
				}),
			},
			References: append([]contract.Reference(nil), referencesFromFormalObjects(record)...),
		},
		State:   contract.ActionStatusQueued,
		Type:    contract.ActionTypeSourceClosure,
		Summary: "等待 SourceClosureAction 收口外部来源。",
	}
	action.Extensions = contract.Extensions{
		contract.ExtensionNamespace("symphony"): {
			"job_id": jobIDForRecord(record),
		},
	}
	actionState := &sourceClosureActionState{
		Action:      action,
		SourceIssue: *sourceIssue,
		JobID:       jobIDForRecord(record),
	}
	o.sourceClosureActions[actionID] = actionState
	o.upsertSourceClosureActionLocked(actionState)
}

func (o *Orchestrator) reconcileSourceClosureActions(ctx context.Context) {
	o.mu.Lock()
	actionIDs := make([]string, 0, len(o.sourceClosureActions))
	for actionID, actionState := range o.sourceClosureActions {
		if actionState == nil {
			continue
		}
		if actionState.Action.State != contract.ActionStatusQueued && actionState.Action.State != contract.ActionStatusExternalPending {
			continue
		}
		actionIDs = append(actionIDs, actionID)
	}
	o.mu.Unlock()
	sort.Strings(actionIDs)
	for _, actionID := range actionIDs {
		o.mu.Lock()
		actionState := o.sourceClosureActions[actionID]
		if actionState == nil || (actionState.Action.State != contract.ActionStatusQueued && actionState.Action.State != contract.ActionStatusExternalPending) {
			o.mu.Unlock()
			continue
		}
		actionState.Action.State = contract.ActionStatusRunning
		actionState.Action.Summary = "正在执行 SourceClosureAction。"
		actionState.Action.Reasons = nil
		actionState.Action.Decision = nil
		actionState.Action.ErrorCode = ""
		actionState.Action.UpdatedAt = o.now().UTC().Format(time.RFC3339Nano)
		o.upsertSourceClosureActionLocked(actionState)
		o.updateJobActionSummaryLocked(actionState.JobID)
		o.commitStateLocked(false)
		sourceIssue := actionState.SourceIssue
		o.mu.Unlock()

		result := o.tracker.CloseSourceIssue(ctx, sourceIssue)

		o.mu.Lock()
		actionState = o.sourceClosureActions[actionID]
		if actionState == nil {
			o.mu.Unlock()
			continue
		}
		actionState.Action.UpdatedAt = o.now().UTC().Format(time.RFC3339Nano)
		switch result.Disposition {
		case tracker.SourceClosureDispositionCompleted:
			actionState.Action.State = contract.ActionStatusCompleted
			actionState.Action.Summary = "SourceClosureAction 已完成。"
			actionState.Action.Reasons = nil
			actionState.Action.Decision = nil
			actionState.Action.ErrorCode = ""
		case tracker.SourceClosureDispositionInterventionRequired:
			actionState.Action.State = contract.ActionStatusInterventionRequired
			actionState.Action.Summary = "SourceClosureAction 发生冲突，需要人工介入。"
			actionState.Action.ErrorCode = result.ErrorCode
			if result.Reason != nil {
				actionState.Action.Reasons = []contract.Reason{*result.Reason}
			}
			actionState.Action.Decision = result.Decision
		default:
			actionState.Action.State = contract.ActionStatusExternalPending
			actionState.Action.Summary = "外部来源暂不可收口，Action 进入 external_pending。"
			actionState.Action.ErrorCode = result.ErrorCode
			if result.Reason != nil {
				actionState.Action.Reasons = []contract.Reason{*result.Reason}
			}
			actionState.Action.Decision = result.Decision
		}
		o.upsertSourceClosureActionLocked(actionState)
		o.updateJobActionSummaryLocked(actionState.JobID)
		o.commitStateLocked(true)
		o.mu.Unlock()
	}
}

func (o *Orchestrator) artifactsForCompletedRecord(record *model.JobRuntime) []contract.Artifact {
	if record == nil {
		return nil
	}
	updatedAt := ""
	if record.Outcome != nil {
		updatedAt = strings.TrimSpace(record.Outcome.CompletedAt)
	}
	if updatedAt == "" {
		updatedAt = strings.TrimSpace(record.UpdatedAt)
	}
	if updatedAt == "" {
		updatedAt = o.now().UTC().Format(time.RFC3339Nano)
	}
	references := append([]contract.Reference(nil), referencesFromFormalObjects(record)...)
	pr := recordPullRequest(record)
	switch jobTypeForDispatch(record.Dispatch) {
	case contract.JobTypeCodeChange:
		if pr == nil || strings.TrimSpace(pr.URL) == "" {
			return nil
		}
		return []contract.Artifact{
			{
				BaseObject:    o.baseObjectLocked(contract.ObjectTypeArtifact, artifactIDForRecord(record, "pull-request"), contract.VisibilityLevelRestricted, updatedAt),
				ObjectContext: contract.ObjectContext{References: references},
				State:         contract.ArtifactStatusAvailable,
				Kind:          contract.ArtifactKindPullRequest,
				Role:          contract.ArtifactRolePrimary,
				Locator:       pr.URL,
			},
		}
	case contract.JobTypeLandChange:
		if pr == nil || strings.TrimSpace(pr.URL) == "" {
			return nil
		}
		return []contract.Artifact{
			{
				BaseObject:    o.baseObjectLocked(contract.ObjectTypeArtifact, artifactIDForRecord(record, "landed-change"), contract.VisibilityLevelRestricted, updatedAt),
				ObjectContext: contract.ObjectContext{References: references},
				State:         contract.ArtifactStatusAvailable,
				Kind:          contract.ArtifactKindLandedChangeRecord,
				Role:          contract.ArtifactRolePrimary,
				Locator:       pr.URL,
			},
			{
				BaseObject:    o.baseObjectLocked(contract.ObjectTypeArtifact, artifactIDForRecord(record, "merge-result"), contract.VisibilityLevelRestricted, updatedAt),
				ObjectContext: contract.ObjectContext{References: references},
				State:         contract.ArtifactStatusAvailable,
				Kind:          contract.ArtifactKindMergeResult,
				Role:          contract.ArtifactRoleSupporting,
				Locator:       pr.URL,
			},
		}
	case contract.JobTypeAnalysis:
		return []contract.Artifact{
			{
				BaseObject:    o.baseObjectLocked(contract.ObjectTypeArtifact, artifactIDForRecord(record, "analysis-report"), contract.VisibilityLevelRestricted, updatedAt),
				ObjectContext: contract.ObjectContext{References: references},
				State:         contract.ArtifactStatusAvailable,
				Kind:          contract.ArtifactKindAnalysisReport,
				Role:          contract.ArtifactRolePrimary,
			},
		}
	case contract.JobTypeDiagnostic:
		return []contract.Artifact{
			{
				BaseObject:    o.baseObjectLocked(contract.ObjectTypeArtifact, artifactIDForRecord(record, "diagnostic-report"), contract.VisibilityLevelRestricted, updatedAt),
				ObjectContext: contract.ObjectContext{References: references},
				State:         contract.ArtifactStatusAvailable,
				Kind:          contract.ArtifactKindDiagnosticReport,
				Role:          contract.ArtifactRolePrimary,
			},
		}
	default:
		return nil
	}
}
