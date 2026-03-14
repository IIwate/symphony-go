package orchestrator

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"symphony-go/internal/agent"
	"symphony-go/internal/model"
	"symphony-go/internal/model/contract"
	"symphony-go/internal/workspace"
)

type fakeTracker struct {
	candidates  []model.Issue
	issuesByID  map[string]model.Issue
	fetchErr    error
	completeErr error
	completed   []string
}

func (f *fakeTracker) FetchCandidateIssues(context.Context) ([]model.Issue, error) {
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	return append([]model.Issue(nil), f.candidates...), nil
}

func (f *fakeTracker) FetchIssuesByStates(_ context.Context, states []string) ([]model.Issue, error) {
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	allowed := map[string]struct{}{}
	for _, state := range states {
		allowed[model.NormalizeState(state)] = struct{}{}
	}
	result := make([]model.Issue, 0, len(f.issuesByID))
	for _, issue := range f.issuesByID {
		if _, ok := allowed[model.NormalizeState(issue.State)]; ok {
			result = append(result, issue)
		}
	}
	return result, nil
}

func (f *fakeTracker) FetchIssueStatesByIDs(_ context.Context, ids []string) ([]model.Issue, error) {
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	result := make([]model.Issue, 0, len(ids))
	for _, id := range ids {
		if issue, ok := f.issuesByID[id]; ok {
			result = append(result, issue)
		}
	}
	return result, nil
}

func (f *fakeTracker) CompleteIssue(_ context.Context, issueID string) error {
	if f.completeErr != nil {
		return f.completeErr
	}
	f.completed = append(f.completed, issueID)
	return nil
}

type fakeWorkspace struct {
	root         string
	cleanupCalls []string
}

func (f *fakeWorkspace) CreateForIssue(_ context.Context, identifier string) (*model.Workspace, error) {
	return &model.Workspace{Path: filepath.Join(f.root, identifier)}, nil
}

func (f *fakeWorkspace) CleanupWorkspace(_ context.Context, identifier string) error {
	f.cleanupCalls = append(f.cleanupCalls, identifier)
	return nil
}

type fakeRunner struct{}

func (fakeRunner) Run(context.Context, agent.RunParams) error {
	return nil
}

type fakePRLookup struct {
	find    func(context.Context, string, string) (*PullRequestInfo, error)
	refresh func(context.Context, string, *PullRequestInfo) (*PullRequestInfo, error)
}

func (f fakePRLookup) FindByHeadBranch(ctx context.Context, workspacePath string, headBranch string) (*PullRequestInfo, error) {
	if f.find != nil {
		return f.find(ctx, workspacePath, headBranch)
	}
	return nil, nil
}

func (f fakePRLookup) Refresh(ctx context.Context, workspacePath string, pr *PullRequestInfo) (*PullRequestInfo, error) {
	if f.refresh != nil {
		return f.refresh(ctx, workspacePath, pr)
	}
	return pr, nil
}

func newTestConfig(t *testing.T) *model.ServiceConfig {
	t.Helper()
	root := t.TempDir()
	return &model.ServiceConfig{
		TrackerKind:               "linear",
		TrackerProjectSlug:        "demo",
		ActiveStates:              []string{"Todo", "In Progress"},
		TerminalStates:            []string{"Done", "Closed"},
		PollIntervalMS:            10,
		AutomationRootDir:         root,
		WorkspaceRoot:             filepath.Join(root, "workspaces"),
		MaxConcurrentAgents:       2,
		MaxTurns:                  1,
		MaxRetryBackoffMS:         100,
		OrchestratorAutoCloseOnPR: true,
		SessionPersistence: model.SessionPersistenceConfig{
			Enabled: true,
			Kind:    model.SessionPersistenceKindFile,
			File: model.SessionPersistenceFileConfig{
				Path:            filepath.Join(root, "local", "runtime-ledger.json"),
				FlushIntervalMS: 5,
				FsyncOnCritical: true,
			},
		},
	}
}

func newTestWorkflow() *model.WorkflowDefinition {
	return &model.WorkflowDefinition{
		Completion: model.CompletionContract{
			Mode:        model.CompletionModePullRequest,
			OnMissingPR: model.CompletionActionIntervention,
			OnClosedPR:  model.CompletionActionIntervention,
		},
	}
}

func newTestOrchestrator(t *testing.T) (*Orchestrator, *fakeTracker, *fakeWorkspace) {
	t.Helper()
	cfg := newTestConfig(t)
	trackerClient := &fakeTracker{issuesByID: map[string]model.Issue{}}
	workspaceManager := &fakeWorkspace{root: cfg.WorkspaceRoot}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	o := NewOrchestrator(trackerClient, workspaceManager, fakeRunner{}, func() *model.ServiceConfig { return cfg }, newTestWorkflow, nil, logger)
	o.now = func() time.Time { return time.Date(2026, 3, 14, 10, 0, 0, 0, time.UTC) }
	return o, trackerClient, workspaceManager
}

func newIssue(id string, identifier string, state string) model.Issue {
	url := "https://linear.example/" + identifier
	return model.Issue{
		ID:         id,
		Identifier: identifier,
		Title:      "Test " + identifier,
		State:      state,
		URL:        &url,
	}
}

func ptrString(value string) *string {
	return &value
}

func TestOrchestratorStateDoesNotExposeLegacyBucketFields(t *testing.T) {
	typ := reflect.TypeOf(model.OrchestratorState{})
	disallowed := map[string]struct{}{
		"Running":              {},
		"Recovering":           {},
		"AwaitingMerge":        {},
		"AwaitingIntervention": {},
		"RetryAttempts":        {},
		"Completed":            {},
	}
	for i := 0; i < typ.NumField(); i++ {
		if _, ok := disallowed[typ.Field(i).Name]; ok {
			t.Fatalf("OrchestratorState still exposes legacy bucket field %q", typ.Field(i).Name)
		}
	}
}

func TestBuildPersistedStateUsesLedgerRecordsAndNoLegacyDumpKeys(t *testing.T) {
	o, _, _ := newTestOrchestrator(t)
	issue := newIssue("1", "ABC-1", "In Progress")
	record := o.ensureRecordLocked(issue)
	o.scheduleRetryLocked(issue.ID, issue.Identifier, 2, ptrString("runner failed"), false, 1, freshDispatchContext(normalizeCompletionContract(o.currentWorkflow().Completion)))
	o.rememberCompletedLocked(contract.IssueRuntimeRecord{
		RecordID:  contract.RecordID("rec_linear_done"),
		SourceRef: contract.SourceRef{SourceKind: contract.SourceKindLinear, SourceID: "done", SourceIdentifier: "DONE-1"},
		Status:    contract.IssueStatusCompleted,
		UpdatedAt: o.now().UTC().Format(time.RFC3339Nano),
		Result:    &contract.Result{Outcome: contract.ResultOutcomeSucceeded, Summary: "done", CompletedAt: o.now().UTC().Format(time.RFC3339Nano)},
		DurableRefs: contract.DurableRefs{
			LedgerPath: o.currentConfig().SessionPersistence.File.Path,
		},
	})
	record.Runtime.DurableRefs.Workspace = &contract.WorkspaceRef{Path: filepath.Join(o.currentConfig().WorkspaceRoot, issue.Identifier)}

	state := o.buildPersistedStateLocked()
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	for _, legacy := range []string{"retrying", "recovering", "awaiting_merge", "awaiting_intervention"} {
		if _, ok := payload[legacy]; ok {
			t.Fatalf("persisted payload still exposes legacy key %q", legacy)
		}
	}
	records, ok := payload["records"].([]any)
	if !ok || len(records) != 2 {
		t.Fatalf("records = %#v, want 2 entries", payload["records"])
	}
}

func TestRestorePersistedStateRebuildsRecordIndexesAndCompletedWindow(t *testing.T) {
	o, _, _ := newTestOrchestrator(t)
	retryDue := o.now().Add(10 * time.Second).Format(time.RFC3339Nano)
	state := durableRuntimeState{
		Version:  durableStateVersion,
		Identity: o.currentRuntimeIdentity(),
		SavedAt:  o.now().UTC(),
		Service: durableServiceMetadata{
			TokenTotal: model.TokenTotals{InputTokens: 11},
			RecordMetadata: map[string]durableRecordMetadata{
				"rec_linear_1": {RetryAttempt: 2, StallCount: 1},
				"rec_linear_2": {RetryAttempt: 1, StallCount: 0},
			},
		},
		Records: []contract.IssueLedgerRecord{
			{
				RecordID:   "rec_linear_1",
				SourceRef:  contract.SourceRef{SourceKind: contract.SourceKindLinear, SourceID: "1", SourceIdentifier: "ABC-1"},
				Status:     contract.IssueStatusRetryScheduled,
				Reason:     ptrReason(contract.MustReason(contract.ReasonRecordBlockedRetryScheduled, map[string]any{"attempt": 3})),
				RetryDueAt: &retryDue,
				DurableRefs: contract.DurableRefs{
					LedgerPath: o.currentConfig().SessionPersistence.File.Path,
				},
				UpdatedAt: o.now().UTC().Format(time.RFC3339Nano),
			},
			{
				RecordID:  "rec_linear_2",
				SourceRef: contract.SourceRef{SourceKind: contract.SourceKindLinear, SourceID: "2", SourceIdentifier: "ABC-2"},
				Status:    contract.IssueStatusAwaitingMerge,
				Reason:    ptrReason(contract.MustReason(contract.ReasonRecordBlockedAwaitingMerge, map[string]any{"pr_number": 42})),
				DurableRefs: contract.DurableRefs{
					Branch:     &contract.BranchRef{Name: "feature/abc-2"},
					LedgerPath: o.currentConfig().SessionPersistence.File.Path,
				},
				UpdatedAt: o.now().UTC().Format(time.RFC3339Nano),
			},
			{
				RecordID:  "rec_linear_done",
				SourceRef: contract.SourceRef{SourceKind: contract.SourceKindLinear, SourceID: "done", SourceIdentifier: "DONE-1"},
				Status:    contract.IssueStatusCompleted,
				Result:    &contract.Result{Outcome: contract.ResultOutcomeSucceeded, Summary: "done", CompletedAt: o.now().UTC().Format(time.RFC3339Nano)},
				DurableRefs: contract.DurableRefs{
					LedgerPath: o.currentConfig().SessionPersistence.File.Path,
				},
				UpdatedAt: o.now().UTC().Format(time.RFC3339Nano),
			},
		},
	}

	o.restorePersistedStateLocked(&state)

	if len(o.retryRecords) != 1 {
		t.Fatalf("retryRecords size = %d, want 1", len(o.retryRecords))
	}
	if len(o.awaitingMergeRecords) != 1 {
		t.Fatalf("awaitingMergeRecords size = %d, want 1", len(o.awaitingMergeRecords))
	}
	if len(o.state.CompletedWindow) != 1 {
		t.Fatalf("CompletedWindow size = %d, want 1", len(o.state.CompletedWindow))
	}
	if o.state.CodexTotals.InputTokens != 11 {
		t.Fatalf("CodexTotals.InputTokens = %d, want 11", o.state.CodexTotals.InputTokens)
	}
}

func TestConservativeRecoveryMovesUnknownActiveRecordToAwaitingIntervention(t *testing.T) {
	o, trackerClient, _ := newTestOrchestrator(t)
	issue := newIssue("1", "ABC-1", "In Progress")
	trackerClient.issuesByID[issue.ID] = issue
	record := o.ensureRecordLocked(issue)
	record.NeedsRecovery = true
	o.setRecordStatusLocked(record, contract.IssueStatusActive, nil, &contract.Observation{Running: false, Summary: "recovery pending"})

	o.reconcileRecovering(context.Background())

	current := o.awaitingInterventionRecords[issue.ID]
	if current == nil {
		t.Fatal("awaitingIntervention record missing after conservative recovery")
	}
	if current.Runtime.Reason == nil || current.Runtime.Reason.ReasonCode != contract.ReasonRecordBlockedRecoveryUncertain {
		t.Fatalf("Reason = %#v, want %q", current.Runtime.Reason, contract.ReasonRecordBlockedRecoveryUncertain)
	}
}

func TestConservativeRecoveryCanPromoteToAwaitingMerge(t *testing.T) {
	o, trackerClient, _ := newTestOrchestrator(t)
	issue := newIssue("1", "ABC-1", "In Progress")
	trackerClient.issuesByID[issue.ID] = issue
	record := o.ensureRecordLocked(issue)
	record.NeedsRecovery = true
	record.Dispatch = freshDispatchContext(normalizeCompletionContract(o.currentWorkflow().Completion))
	record.Runtime.DurableRefs.Workspace = &contract.WorkspaceRef{Path: filepath.Join(o.currentConfig().WorkspaceRoot, issue.Identifier)}
	record.Runtime.DurableRefs.Branch = &contract.BranchRef{Name: "feature/abc-1"}
	o.prLookup = fakePRLookup{
		find: func(context.Context, string, string) (*PullRequestInfo, error) {
			return &PullRequestInfo{Number: 42, URL: "https://github.example/pr/42", State: PullRequestStateOpen, HeadBranch: "feature/abc-1"}, nil
		},
	}

	o.reconcileRecovering(context.Background())

	current := o.awaitingMergeRecords[issue.ID]
	if current == nil {
		t.Fatal("awaitingMerge record missing after recoverable post-run classification")
	}
	if current.Runtime.Reason == nil || current.Runtime.Reason.ReasonCode != contract.ReasonRecordBlockedAwaitingMerge {
		t.Fatalf("Reason = %#v, want %q", current.Runtime.Reason, contract.ReasonRecordBlockedAwaitingMerge)
	}
}

func TestHandleSessionPersistenceWriteFailureSetsUnavailableServiceMode(t *testing.T) {
	o, _, _ := newTestOrchestrator(t)

	o.handleSessionPersistenceWriteFailure(assertErr("disk full"))

	if got := o.serviceModeLocked(); got != model.ServiceModeUnavailable {
		t.Fatalf("serviceModeLocked() = %q, want %q", got, model.ServiceModeUnavailable)
	}
	if len(o.state.Service.Reasons) == 0 || o.state.Service.Reasons[0].ReasonCode != contract.ReasonServiceUnavailableCoreDependency {
		t.Fatalf("Service.Reasons = %#v, want %q", o.state.Service.Reasons, contract.ReasonServiceUnavailableCoreDependency)
	}
}

func TestCompleteSuccessfulIssueProducesResultWindow(t *testing.T) {
	o, _, workspaceManager := newTestOrchestrator(t)
	issue := newIssue("1", "ABC-1", "Done")
	record := o.ensureRecordLocked(issue)
	o.moveToAwaitingMerge(issue.ID, issue.Identifier, issue.State, filepath.Join(o.currentConfig().WorkspaceRoot, issue.Identifier), "feature/abc-1", 1, 0, nil, nil)
	if record == nil {
		t.Fatal("record missing")
	}

	o.completeSuccessfulIssue(context.Background(), issue.ID, issue.Identifier)

	if len(o.state.CompletedWindow) != 1 {
		t.Fatalf("CompletedWindow size = %d, want 1", len(o.state.CompletedWindow))
	}
	completed := o.state.CompletedWindow[0]
	if completed.Result == nil || completed.Result.Outcome != contract.ResultOutcomeSucceeded {
		t.Fatalf("Result = %#v, want succeeded", completed.Result)
	}
	if len(workspaceManager.cleanupCalls) != 1 || workspaceManager.cleanupCalls[0] != issue.Identifier {
		t.Fatalf("cleanupCalls = %#v, want %q", workspaceManager.cleanupCalls, issue.Identifier)
	}
}

func TestCompleteAbandonedIssueProducesAbandonedResult(t *testing.T) {
	o, _, _ := newTestOrchestrator(t)
	issue := newIssue("1", "ABC-1", "Canceled")
	o.ensureRecordLocked(issue)

	o.completeAbandonedIssue(context.Background(), issue.ID, issue.Identifier, "left active states")

	if len(o.state.CompletedWindow) != 1 {
		t.Fatalf("CompletedWindow size = %d, want 1", len(o.state.CompletedWindow))
	}
	if got := o.state.CompletedWindow[0].Result.Outcome; got != contract.ResultOutcomeAbandoned {
		t.Fatalf("Result.Outcome = %q, want %q", got, contract.ResultOutcomeAbandoned)
	}
}

func TestScheduleRetryLockedPropagatesUnifiedReasonIntoRuntimeAndLedger(t *testing.T) {
	o, _, _ := newTestOrchestrator(t)
	issue := newIssue("1", "ABC-1", "In Progress")
	o.ensureRecordLocked(issue)

	o.scheduleRetryLocked(issue.ID, issue.Identifier, 2, ptrString("runner failed"), false, 1, freshDispatchContext(normalizeCompletionContract(o.currentWorkflow().Completion)))

	record := o.retryRecords[issue.ID]
	if record == nil {
		t.Fatal("retry record missing")
	}
	if record.Runtime.Reason == nil || record.Runtime.Reason.ReasonCode != contract.ReasonRecordBlockedRetryScheduled {
		t.Fatalf("Reason = %#v, want %q", record.Runtime.Reason, contract.ReasonRecordBlockedRetryScheduled)
	}
	state := o.buildPersistedStateLocked()
	if len(state.Records) != 1 {
		t.Fatalf("persisted records = %d, want 1", len(state.Records))
	}
	if state.Records[0].Reason == nil || state.Records[0].Reason.ReasonCode != contract.ReasonRecordBlockedRetryScheduled {
		t.Fatalf("ledger reason = %#v, want %q", state.Records[0].Reason, contract.ReasonRecordBlockedRetryScheduled)
	}
	if state.Records[0].RetryDueAt == nil {
		t.Fatal("ledger RetryDueAt missing")
	}
}

func TestFileLedgerStoreRoundTrip(t *testing.T) {
	cfg := model.SessionPersistenceConfig{
		Enabled: true,
		Kind:    model.SessionPersistenceKindFile,
		File: model.SessionPersistenceFileConfig{
			Path:            filepath.Join(t.TempDir(), "runtime-ledger.json"),
			FlushIntervalMS: 5,
			FsyncOnCritical: true,
		},
	}
	identity := normalizeRuntimeIdentity(RuntimeIdentity{
		Descriptor: RuntimeDescriptor{ConfigRoot: filepath.Dir(cfg.File.Path)},
	})
	successCh := make(chan durableRuntimeState, 1)
	store := newFileStateStore(cfg, identity, slog.New(slog.NewTextHandler(io.Discard, nil)), func(_ uint64, state durableRuntimeState) {
		successCh <- state
	}, func(err error) {
		t.Fatalf("onFailure(%v)", err)
	})
	defer func() {
		if err := store.Close(context.Background()); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	state := durableRuntimeState{
		Version:  durableStateVersion,
		Identity: identity,
		SavedAt:  time.Now().UTC(),
		Service:  durableServiceMetadata{},
		Records: []contract.IssueLedgerRecord{
			{
				RecordID:  "rec_linear_1",
				SourceRef: contract.SourceRef{SourceKind: contract.SourceKindLinear, SourceID: "1", SourceIdentifier: "ABC-1"},
				Status:    contract.IssueStatusAwaitingMerge,
				UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
				DurableRefs: contract.DurableRefs{
					LedgerPath: cfg.File.Path,
				},
			},
		},
	}
	store.Schedule(state, true)
	select {
	case <-successCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ledger store flush")
	}

	loadedStore := newFileStateStore(cfg, identity, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil)
	defer loadedStore.Close(context.Background())
	loaded, err := loadedStore.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded == nil || len(loaded.Records) != 1 {
		t.Fatalf("loaded records = %#v, want 1 entry", loaded)
	}
}

func TestWriteDurableRuntimeStateRoundTripJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime-ledger.json")
	state := durableRuntimeState{
		Version: durableStateVersion,
		SavedAt: time.Now().UTC(),
		Records: []contract.IssueLedgerRecord{
			{
				RecordID:  "rec_linear_1",
				SourceRef: contract.SourceRef{SourceKind: contract.SourceKindLinear, SourceID: "1", SourceIdentifier: "ABC-1"},
				Status:    contract.IssueStatusCompleted,
				Result:    &contract.Result{Outcome: contract.ResultOutcomeSucceeded, Summary: "done", CompletedAt: time.Now().UTC().Format(time.RFC3339Nano)},
				UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
			},
		},
	}
	if err := writeDurableRuntimeState(path, state, true); err != nil {
		t.Fatalf("writeDurableRuntimeState() error = %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if strings.Contains(string(raw), "\"recovering\"") || strings.Contains(string(raw), "\"retrying\"") {
		t.Fatalf("runtime-ledger.json still contains legacy bucket keys: %s", string(raw))
	}
}

func ptrReason(value contract.Reason) *contract.Reason {
	return &value
}

func assertErr(message string) error {
	return &testError{message: message}
}

type testError struct {
	message string
}

func (e *testError) Error() string {
	return e.message
}

var _ workspace.Manager = (*fakeWorkspace)(nil)
var _ agent.Runner = fakeRunner{}
var _ LedgerStore = (*fileStateStore)(nil)
