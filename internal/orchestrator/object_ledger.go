package orchestrator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"symphony-go/internal/model/contract"
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
