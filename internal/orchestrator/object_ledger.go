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

type ObjectLedger interface {
	UpsertJob(contract.Job) error
	UpsertRun(contract.Run) error
	UpsertIntervention(contract.Intervention) error
	UpsertOutcome(contract.Outcome) error
	UpsertArtifact(contract.Artifact) error
	UpsertAction(contract.Action) error
	UpsertInstance(contract.Instance) error
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

func (l *fileObjectLedger) UpsertJob(value contract.Job) error {
	return l.upsert(value.ObjectType, value.ID, value.UpdatedAt, value)
}
func (l *fileObjectLedger) UpsertRun(value contract.Run) error {
	return l.upsert(value.ObjectType, value.ID, value.UpdatedAt, value)
}
func (l *fileObjectLedger) UpsertIntervention(value contract.Intervention) error {
	return l.upsert(value.ObjectType, value.ID, value.UpdatedAt, value)
}
func (l *fileObjectLedger) UpsertOutcome(value contract.Outcome) error {
	return l.upsert(value.ObjectType, value.ID, value.UpdatedAt, value)
}
func (l *fileObjectLedger) UpsertArtifact(value contract.Artifact) error {
	return l.upsert(value.ObjectType, value.ID, value.UpdatedAt, value)
}
func (l *fileObjectLedger) UpsertAction(value contract.Action) error {
	return l.upsert(value.ObjectType, value.ID, value.UpdatedAt, value)
}
func (l *fileObjectLedger) UpsertInstance(value contract.Instance) error {
	return l.upsert(value.ObjectType, value.ID, value.UpdatedAt, value)
}

func (l *fileObjectLedger) Archive(objectType contract.ObjectType, id string, at string) error {
	return l.markLifecycle(objectType, id, LedgerStorageTierArchive, LedgerLifecycleArchived, at)
}

func (l *fileObjectLedger) Invalidate(objectType contract.ObjectType, id string, at string) error {
	return l.markLifecycle(objectType, id, "", LedgerLifecycleInvalidated, at)
}

func (l *fileObjectLedger) Terminate(objectType contract.ObjectType, id string, at string) error {
	return l.markLifecycle(objectType, id, "", LedgerLifecycleTerminated, at)
}

func (l *fileObjectLedger) Get(objectType contract.ObjectType, id string) (ObjectEnvelope, bool) {
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
	ledger, err := NewFileObjectLedger(objectLedgerPathForConfig(o.currentConfig()))
	if err != nil {
		o.logger.Warn("object ledger init failed", "error", err.Error())
		return
	}
	o.objectLedger = ledger
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

func (o *Orchestrator) GetObject(objectType contract.ObjectType, id string) (ObjectEnvelope, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.ensureObjectLedgerLocked()
	if o.objectLedger == nil {
		return ObjectEnvelope{}, false
	}
	return o.objectLedger.Get(objectType, strings.TrimSpace(id))
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
	for _, record := range o.state.Records {
		if record == nil {
			continue
		}
		o.syncRecordFormalObjectsLocked(record)
	}
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

func (o *Orchestrator) syncRecordFormalObjectsLocked(record *model.IssueRecord) {
	if record == nil || o.objectLedger == nil {
		return
	}
	job := o.jobObjectFromRecord(record, jobStateFromRecord(record))
	if err := o.objectLedger.UpsertJob(job); err != nil {
		o.logger.Warn("upsert job object failed", "job_id", job.ID, "error", err.Error())
	}
	if record.Run != nil {
		run := o.runObjectFromRecord(record, nil)
		if err := o.objectLedger.UpsertRun(run); err != nil {
			o.logger.Warn("upsert run object failed", "run_id", run.ID, "error", err.Error())
		}
	}
	if record.Intervention != nil {
		intervention := o.interventionObjectFromRecord(record)
		if err := o.objectLedger.UpsertIntervention(intervention); err != nil {
			o.logger.Warn("upsert intervention object failed", "intervention_id", intervention.ID, "error", err.Error())
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

func jobIDForRecord(record *model.IssueRecord) string {
	return "job-" + strings.TrimSpace(string(record.Runtime.RecordID))
}

func runIDForRecord(record *model.IssueRecord, attempt int) string {
	return fmt.Sprintf("run-%s-%d", strings.TrimSpace(string(record.Runtime.RecordID)), attempt)
}

func interventionIDForRecord(record *model.IssueRecord) string {
	attempt := currentAttempt(record)
	if record.Run != nil {
		attempt = record.Run.Attempt
	}
	return fmt.Sprintf("intervention-%s-%d", strings.TrimSpace(string(record.Runtime.RecordID)), attempt)
}

func outcomeIDForRecord(record *model.IssueRecord) string {
	return "outcome-" + strings.TrimSpace(string(record.Runtime.RecordID))
}

func artifactIDForRecord(record *model.IssueRecord, suffix string) string {
	return fmt.Sprintf("artifact-%s-%s", strings.TrimSpace(string(record.Runtime.RecordID)), suffix)
}

func sourceClosureActionIDForRecord(record *model.IssueRecord) string {
	return "action-" + strings.TrimSpace(string(record.Runtime.RecordID)) + "-source-closure"
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

func jobStateFromRecord(record *model.IssueRecord) contract.JobStatus {
	if record == nil {
		return contract.JobStatusQueued
	}
	switch record.Runtime.Status {
	case contract.IssueStatusAwaitingIntervention:
		return contract.JobStatusInterventionRequired
	case contract.IssueStatusCompleted:
		if record.Runtime.Result != nil {
			return jobStateFromOutcome(record.Runtime.Result.Outcome)
		}
		return contract.JobStatusCompleted
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

func (o *Orchestrator) actionSummaryForJobLocked(jobID string) contract.ActionSummary {
	summary := contract.ActionSummary{}
	for _, actionState := range o.sourceClosureActions {
		if actionState == nil || actionState.JobID != jobID {
			continue
		}
		if actionState.Action.State != contract.ActionStatusExternalPending {
			continue
		}
		summary.HasPendingExternalActions = true
		summary.PendingCount++
		summary.PendingTypes = append(summary.PendingTypes, actionState.Action.Type)
	}
	return summary
}

func (o *Orchestrator) jobObjectFromRecord(record *model.IssueRecord, state contract.JobStatus) contract.Job {
	updatedAt := strings.TrimSpace(record.Runtime.UpdatedAt)
	if updatedAt == "" {
		updatedAt = o.now().UTC().Format(time.RFC3339Nano)
	}
	relations := []contract.ObjectRelation{}
	if record.Run != nil {
		relations = append(relations, contract.ObjectRelation{Type: contract.RelationTypeJobRun, TargetID: runIDForRecord(record, record.Run.Attempt), TargetType: contract.ObjectTypeRun})
	}
	for _, actionState := range o.sourceClosureActions {
		if actionState == nil || actionState.JobID != jobIDForRecord(record) {
			continue
		}
		relations = append(relations, contract.ObjectRelation{Type: contract.RelationTypeJobAction, TargetID: actionState.Action.ID, TargetType: contract.ObjectTypeAction})
	}
	ctx := contract.ObjectContext{
		Relations:  relations,
		References: referencesFromRecord(o, record, updatedAt),
	}
	if record.Runtime.Reason != nil {
		ctx.Reasons = []contract.Reason{*cloneReason(record.Runtime.Reason)}
	}
	return contract.Job{
		BaseObject:    o.baseObjectLocked(contract.ObjectTypeJob, jobIDForRecord(record), contract.VisibilityLevelRestricted, updatedAt),
		ObjectContext: ctx,
		State:         state,
		JobType:       jobTypeForDispatch(record.Dispatch),
		ActionSummary: o.actionSummaryForJobLocked(jobIDForRecord(record)),
	}
}

func (o *Orchestrator) runObjectFromRecord(record *model.IssueRecord, outcome *contract.Outcome) contract.Run {
	runState := model.CloneRunState(record.Run)
	updatedAt := strings.TrimSpace(record.Runtime.UpdatedAt)
	if updatedAt == "" {
		updatedAt = o.now().UTC().Format(time.RFC3339Nano)
	}
	ctx := contract.ObjectContext{
		References: referencesFromRecord(o, record, updatedAt),
		ErrorCode:  runState.ErrorCode,
	}
	if runState.Reason != nil {
		ctx.Reasons = []contract.Reason{*cloneReason(runState.Reason)}
	}
	if runState.Decision != nil {
		decisionCopy := *runState.Decision
		ctx.Decision = &decisionCopy
	}
	if record.Intervention != nil {
		ctx.Relations = append(ctx.Relations, contract.ObjectRelation{Type: contract.RelationTypeRunIntervention, TargetID: interventionIDForRecord(record), TargetType: contract.ObjectTypeIntervention})
	}
	if outcome != nil {
		ctx.Relations = append(ctx.Relations, contract.ObjectRelation{Type: contract.RelationTypeRunOutcome, TargetID: outcome.ID, TargetType: contract.ObjectTypeOutcome})
	}
	return contract.Run{
		BaseObject:        o.baseObjectLocked(contract.ObjectTypeRun, runIDForRecord(record, runState.Attempt), contract.VisibilityLevelRestricted, updatedAt),
		ObjectContext:     ctx,
		State:             runState.State,
		Phase:             runState.Phase,
		Attempt:           runState.Attempt,
		CandidateDelivery: runState.CandidateDelivery,
		ReviewGate:        runState.ReviewGate,
		Checkpoints:       append([]contract.Checkpoint(nil), runState.Checkpoints...),
	}
}

func (o *Orchestrator) interventionObjectFromRecord(record *model.IssueRecord) contract.Intervention {
	intervention := model.CloneInterventionState(record.Intervention).Object
	updatedAt := strings.TrimSpace(record.Runtime.UpdatedAt)
	if updatedAt == "" {
		updatedAt = o.now().UTC().Format(time.RFC3339Nano)
	}
	intervention.BaseObject = o.baseObjectLocked(contract.ObjectTypeIntervention, interventionIDForRecord(record), contract.VisibilityLevelRestricted, updatedAt)
	intervention.References = referencesFromRecord(o, record, updatedAt)
	return intervention
}

func (o *Orchestrator) upsertSourceClosureActionLocked(state *sourceClosureActionState) {
	if state == nil || o.objectLedger == nil {
		return
	}
	if err := o.objectLedger.UpsertAction(state.Action); err != nil {
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
	for _, actionState := range o.sourceClosureActions {
		if actionState == nil || actionState.JobID != jobID {
			continue
		}
		relations = append(relations, contract.ObjectRelation{Type: contract.RelationTypeJobAction, TargetID: actionState.Action.ID, TargetType: contract.ObjectTypeAction})
	}
	job.Relations = relations
	if err := o.objectLedger.UpsertJob(job); err != nil {
		o.logger.Warn("update job action summary failed", "job_id", job.ID, "error", err.Error())
	}
}

func referencesFromRecord(o *Orchestrator, record *model.IssueRecord, updatedAt string) []contract.Reference {
	if record == nil {
		return nil
	}
	references := []contract.Reference{}
	baseRef := func(id string) contract.BaseObject {
		return o.baseObjectLocked(contract.ObjectTypeReference, id, contract.VisibilityLevelRestricted, updatedAt)
	}
	sourceRef := record.Runtime.SourceRef
	switch sourceRef.SourceKind {
	case contract.SourceKindLinear:
		references = append(references, contract.Reference{
			BaseObject:  baseRef("ref-" + strings.TrimSpace(string(record.Runtime.RecordID)) + "-source"),
			State:       contract.ReferenceStatusActive,
			Type:        contract.ReferenceTypeLinearIssue,
			System:      string(sourceRef.SourceKind),
			Locator:     sourceRef.SourceIdentifier,
			URL:         sourceRef.URL,
			ExternalID:  sourceRef.SourceID,
			DisplayName: sourceRef.SourceIdentifier,
		})
	}
	if branch := record.Runtime.DurableRefs.Branch; branch != nil && strings.TrimSpace(branch.Name) != "" {
		references = append(references, contract.Reference{
			BaseObject:  baseRef("ref-" + strings.TrimSpace(string(record.Runtime.RecordID)) + "-branch"),
			State:       contract.ReferenceStatusActive,
			Type:        contract.ReferenceTypeGitBranch,
			System:      "git",
			Locator:     branch.Name,
			DisplayName: branch.Name,
		})
	}
	if pr := record.Runtime.DurableRefs.PullRequest; pr != nil && strings.TrimSpace(pr.URL) != "" {
		references = append(references, contract.Reference{
			BaseObject:  baseRef("ref-" + strings.TrimSpace(string(record.Runtime.RecordID)) + "-pr"),
			State:       contract.ReferenceStatusActive,
			Type:        contract.ReferenceTypeGitHubPullRequest,
			System:      "github",
			Locator:     pr.URL,
			URL:         pr.URL,
			ExternalID:  fmt.Sprintf("%d", pr.Number),
			DisplayName: fmt.Sprintf("PR #%d", pr.Number),
		})
	}
	return references
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

func (o *Orchestrator) persistCompletionObjectsLocked(record *model.IssueRecord) {
	if record == nil || o.objectLedger == nil || record.Runtime.Result == nil {
		return
	}
	if record.Intervention != nil {
		intervention := o.interventionObjectFromRecord(record)
		if err := o.objectLedger.UpsertIntervention(intervention); err != nil {
			o.logger.Warn("upsert final intervention object failed", "intervention_id", intervention.ID, "error", err.Error())
		}
	}
	if record.Run != nil {
		outcome := o.outcomeObjectFromRecord(record, nil)
		run := o.runObjectFromRecord(record, &outcome)
		if err := o.objectLedger.UpsertRun(run); err != nil {
			o.logger.Warn("upsert final run object failed", "run_id", run.ID, "error", err.Error())
		}
	}
	if o.requiresSourceClosureAction(record) {
		o.ensureSourceClosureActionLocked(record)
	}
	job := o.jobObjectFromRecord(record, jobStateFromOutcome(record.Runtime.Result.Outcome))
	if err := o.objectLedger.UpsertJob(job); err != nil {
		o.logger.Warn("upsert final job object failed", "job_id", job.ID, "error", err.Error())
	}
	outcome := o.outcomeObjectFromRecord(record, o.artifactsForCompletedRecord(record))
	if err := o.objectLedger.UpsertOutcome(outcome); err != nil {
		o.logger.Warn("upsert outcome object failed", "outcome_id", outcome.ID, "error", err.Error())
	}
	for _, artifact := range o.artifactsForCompletedRecord(record) {
		if err := o.objectLedger.UpsertArtifact(artifact); err != nil {
			o.logger.Warn("upsert artifact object failed", "artifact_id", artifact.ID, "error", err.Error())
		}
	}
	o.updateJobActionSummaryLocked(job.ID)
}

func (o *Orchestrator) requiresSourceClosureAction(record *model.IssueRecord) bool {
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
	sourceState := record.LastKnownIssueState
	if record.LastKnownIssue != nil {
		sourceState = record.LastKnownIssue.State
	}
	return !o.isTerminalState(sourceState, o.currentConfig())
}

func (o *Orchestrator) ensureSourceClosureActionLocked(record *model.IssueRecord) {
	if record == nil {
		return
	}
	actionID := sourceClosureActionIDForRecord(record)
	if _, ok := o.sourceClosureActions[actionID]; ok {
		return
	}
	updatedAt := strings.TrimSpace(record.Runtime.Result.CompletedAt)
	if updatedAt == "" {
		updatedAt = strings.TrimSpace(record.Runtime.UpdatedAt)
	}
	if updatedAt == "" {
		updatedAt = o.now().UTC().Format(time.RFC3339Nano)
	}
	sourceIssue := model.CloneIssue(record.LastKnownIssue)
	if sourceIssue == nil {
		sourceIssue = &model.Issue{
			ID:         record.Runtime.SourceRef.SourceID,
			Identifier: record.Runtime.SourceRef.SourceIdentifier,
			State:      record.LastKnownIssueState,
		}
		if url := strings.TrimSpace(record.Runtime.SourceRef.URL); url != "" {
			sourceIssue.URL = &url
		}
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
			References: referencesFromRecord(o, record, updatedAt),
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

func (o *Orchestrator) outcomeObjectFromRecord(record *model.IssueRecord, artifacts []contract.Artifact) contract.Outcome {
	updatedAt := strings.TrimSpace(record.Runtime.Result.CompletedAt)
	if updatedAt == "" {
		updatedAt = strings.TrimSpace(record.Runtime.UpdatedAt)
	}
	if updatedAt == "" {
		updatedAt = o.now().UTC().Format(time.RFC3339Nano)
	}
	relations := make([]contract.ObjectRelation, 0, len(artifacts))
	for _, artifact := range artifacts {
		relations = append(relations, contract.ObjectRelation{Type: contract.RelationTypeOutcomeArtifact, TargetID: artifact.ID, TargetType: contract.ObjectTypeArtifact})
	}
	return contract.Outcome{
		BaseObject: o.baseObjectLocked(contract.ObjectTypeOutcome, outcomeIDForRecord(record), contract.VisibilityLevelRestricted, updatedAt),
		ObjectContext: contract.ObjectContext{
			Relations:  relations,
			References: referencesFromRecord(o, record, updatedAt),
		},
		State:       outcomeConclusionFromResult(record.Runtime.Result.Outcome),
		Summary:     record.Runtime.Result.Summary,
		CompletedAt: record.Runtime.Result.CompletedAt,
	}
}

func (o *Orchestrator) artifactsForCompletedRecord(record *model.IssueRecord) []contract.Artifact {
	if record == nil {
		return nil
	}
	updatedAt := strings.TrimSpace(record.Runtime.Result.CompletedAt)
	if updatedAt == "" {
		updatedAt = strings.TrimSpace(record.Runtime.UpdatedAt)
	}
	if updatedAt == "" {
		updatedAt = o.now().UTC().Format(time.RFC3339Nano)
	}
	references := referencesFromRecord(o, record, updatedAt)
	pr := record.Runtime.DurableRefs.PullRequest
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
	default:
		return nil
	}
}
