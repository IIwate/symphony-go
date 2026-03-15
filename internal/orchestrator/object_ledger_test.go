package orchestrator

import (
	"path/filepath"
	"testing"

	"symphony-go/internal/model/contract"
)

func TestFileObjectLedgerPersistsFormalObjectsAndArchiveBoundaries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "object-ledger.json")
	ledger, err := NewFileObjectLedger(path)
	if err != nil {
		t.Fatalf("NewFileObjectLedger() error = %v", err)
	}

	job := contract.Job{
		BaseObject: contract.BaseObject{
			ID:         "job-1",
			ObjectType: contract.ObjectTypeJob,
			UpdatedAt:  "2026-03-15T00:00:00Z",
		},
		State:   contract.JobStatusRunning,
		JobType: contract.JobTypeCodeChange,
	}
	action := contract.Action{
		BaseObject: contract.BaseObject{
			ID:         "act-1",
			ObjectType: contract.ObjectTypeAction,
			UpdatedAt:  "2026-03-15T00:00:01Z",
		},
		State:   contract.ActionStatusExternalPending,
		Type:    contract.ActionTypeSourceClosure,
		Summary: "waiting on external source closure",
	}

	if err := ledger.UpsertJob(job); err != nil {
		t.Fatalf("UpsertJob() error = %v", err)
	}
	if err := ledger.UpsertAction(action); err != nil {
		t.Fatalf("UpsertAction() error = %v", err)
	}
	if err := ledger.Archive(contract.ObjectTypeJob, "job-1", "2026-03-15T00:00:02Z"); err != nil {
		t.Fatalf("Archive() error = %v", err)
	}
	if err := ledger.Terminate(contract.ObjectTypeAction, "act-1", "2026-03-15T00:00:03Z"); err != nil {
		t.Fatalf("Terminate() error = %v", err)
	}

	archivedJob, ok := ledger.Get(contract.ObjectTypeJob, "job-1")
	if !ok {
		t.Fatal("Get(job-1) = false, want true")
	}
	if archivedJob.StorageTier != LedgerStorageTierArchive {
		t.Fatalf("job storage tier = %q, want %q", archivedJob.StorageTier, LedgerStorageTierArchive)
	}
	if archivedJob.Lifecycle != LedgerLifecycleArchived {
		t.Fatalf("job lifecycle = %q, want %q", archivedJob.Lifecycle, LedgerLifecycleArchived)
	}

	terminatedAction, ok := ledger.Get(contract.ObjectTypeAction, "act-1")
	if !ok {
		t.Fatal("Get(act-1) = false, want true")
	}
	if terminatedAction.StorageTier != LedgerStorageTierHot {
		t.Fatalf("action storage tier = %q, want %q", terminatedAction.StorageTier, LedgerStorageTierHot)
	}
	if terminatedAction.Lifecycle != LedgerLifecycleTerminated {
		t.Fatalf("action lifecycle = %q, want %q", terminatedAction.Lifecycle, LedgerLifecycleTerminated)
	}

	reloaded, err := NewFileObjectLedger(path)
	if err != nil {
		t.Fatalf("NewFileObjectLedger(reload) error = %v", err)
	}
	snapshot := reloaded.Snapshot()
	if len(snapshot.Records) != 2 {
		t.Fatalf("len(snapshot.Records) = %d, want 2", len(snapshot.Records))
	}
}

func TestFileObjectLedgerDoesNotSupportPhysicalDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "object-ledger.json")
	ledger, err := NewFileObjectLedger(path)
	if err != nil {
		t.Fatalf("NewFileObjectLedger() error = %v", err)
	}

	instance := contract.Instance{
		BaseObject: contract.BaseObject{
			ID:         "inst-1",
			ObjectType: contract.ObjectTypeInstance,
			UpdatedAt:  "2026-03-15T00:00:00Z",
		},
		State: contract.ServiceModeServing,
		Name:  "symphony",
	}
	if err := ledger.UpsertInstance(instance); err != nil {
		t.Fatalf("UpsertInstance() error = %v", err)
	}
	if err := ledger.Invalidate(contract.ObjectTypeInstance, "inst-1", "2026-03-15T00:00:01Z"); err != nil {
		t.Fatalf("Invalidate() error = %v", err)
	}
	item, ok := ledger.Get(contract.ObjectTypeInstance, "inst-1")
	if !ok {
		t.Fatal("Get(inst-1) = false, want true")
	}
	if item.Lifecycle != LedgerLifecycleInvalidated {
		t.Fatalf("instance lifecycle = %q, want %q", item.Lifecycle, LedgerLifecycleInvalidated)
	}
}
