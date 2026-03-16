package orchestrator

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"symphony-go/internal/agent"
	"symphony-go/internal/model"
	"symphony-go/internal/model/contract"
	"symphony-go/internal/tracker"
	"symphony-go/internal/workspace"
)

type fakeTracker struct {
	candidates                []model.Issue
	issuesByID                map[string]model.Issue
	fetchErr                  error
	fetchCalls                int
	sourceClosureAvailability tracker.SourceClosureAvailability
	closeSourceResult         tracker.SourceClosureResult
}

type recordingNotifier struct {
	events []model.RuntimeEvent
}

func (n *recordingNotifier) Emit(event model.RuntimeEvent) {
	n.events = append(n.events, event)
}

func (n *recordingNotifier) Close(context.Context) error {
	return nil
}

func (f *fakeTracker) FetchCandidateIssues(context.Context) ([]model.Issue, error) {
	f.fetchCalls++
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

func (f *fakeTracker) SourceClosureAvailability(context.Context) tracker.SourceClosureAvailability {
	if f.sourceClosureAvailability.Supported || f.sourceClosureAvailability.Available || len(f.sourceClosureAvailability.Reasons) > 0 {
		return f.sourceClosureAvailability
	}
	return tracker.SourceClosureAvailability{Supported: true, Available: true}
}

func (f *fakeTracker) CloseSourceIssue(context.Context, model.Issue) tracker.SourceClosureResult {
	if f.closeSourceResult.Disposition != "" {
		return f.closeSourceResult
	}
	return tracker.SourceClosureResult{Disposition: tracker.SourceClosureDispositionCompleted}
}

func mustFormalObjectSnapshot(t *testing.T, objects ...any) ObjectLedgerSnapshot {
	t.Helper()
	ledger := NewMemoryObjectLedger(ObjectLedgerSnapshot{})
	for _, item := range objects {
		var err error
		switch value := item.(type) {
		case contract.Job:
			err = ledger.UpsertJob(value)
		case contract.Run:
			err = ledger.UpsertRun(value)
		case contract.Intervention:
			err = ledger.UpsertIntervention(value)
		case contract.Outcome:
			err = ledger.UpsertOutcome(value)
		case contract.Artifact:
			err = ledger.UpsertArtifact(value)
		case contract.Action:
			err = ledger.UpsertAction(value)
		case contract.Reference:
			err = ledger.UpsertReference(value)
		default:
			t.Fatalf("unsupported formal object type %T", item)
		}
		if err != nil {
			t.Fatalf("upsert formal object %T error = %v", item, err)
		}
	}
	return ledger.Snapshot()
}

func mustGetLedgerObject(t *testing.T, o *Orchestrator, objectType contract.ObjectType, id string) (ObjectEnvelope, bool) {
	t.Helper()
	o.mu.Lock()
	defer o.mu.Unlock()
	o.ensureObjectLedgerLocked()
	if o.objectLedger == nil {
		t.Fatal("objectLedger = nil, want initialized ledger")
	}
	return o.objectLedger.Get(objectType, id)
}

func testReferenceBase(id string, updatedAt string) contract.BaseObject {
	return contract.BaseObject{
		ID:              id,
		ObjectType:      contract.ObjectTypeReference,
		DomainID:        "default",
		Visibility:      contract.VisibilityLevelRestricted,
		ContractVersion: contract.APIVersionV1,
		CreatedAt:       updatedAt,
		UpdatedAt:       updatedAt,
	}
}

func testLinearIssueReference(id string, externalID string, identifier string, updatedAt string) contract.Reference {
	return contract.Reference{
		BaseObject:  testReferenceBase(id, updatedAt),
		State:       contract.ReferenceStatusActive,
		Type:        contract.ReferenceTypeLinearIssue,
		System:      string(contract.SourceKindLinear),
		Locator:     identifier,
		ExternalID:  externalID,
		DisplayName: "linear-main",
	}
}

func testBranchReference(id string, branch string, updatedAt string) contract.Reference {
	return contract.Reference{
		BaseObject:  testReferenceBase(id, updatedAt),
		State:       contract.ReferenceStatusActive,
		Type:        contract.ReferenceTypeGitBranch,
		System:      "git",
		Locator:     branch,
		DisplayName: branch,
	}
}

func testPullRequestReference(id string, number int, url string, updatedAt string) contract.Reference {
	return contract.Reference{
		BaseObject:  testReferenceBase(id, updatedAt),
		State:       contract.ReferenceStatusActive,
		Type:        contract.ReferenceTypeGitHubPullRequest,
		System:      "github",
		Locator:     url,
		URL:         url,
		ExternalID:  strconv.Itoa(number),
		DisplayName: "PR",
	}
}

func setTestWorkspacePath(record *model.JobRuntime, path string) {
	if record == nil {
		return
	}
	record.WorkspacePath = path
}

func setTestLinearSourceReference(record *model.JobRuntime, externalID string, identifier string, updatedAt string) {
	if record == nil {
		return
	}
	reference := testLinearIssueReference(sourceReferenceIDForRecord(record), externalID, identifier, updatedAt)
	record.Object.References = upsertReferenceByType(record.Object.References, &reference, contract.ReferenceTypeLinearIssue)
}

func setTestBranchReference(record *model.JobRuntime, branch string, updatedAt string) {
	if record == nil {
		return
	}
	reference := testBranchReference(branchReferenceIDForRecord(record), branch, updatedAt)
	record.Object.References = upsertReferenceByType(record.Object.References, &reference, contract.ReferenceTypeGitBranch)
}

func setTestPullRequestReference(record *model.JobRuntime, number int, url string, state string, updatedAt string) {
	if record == nil {
		return
	}
	reference := testPullRequestReference(pullRequestReferenceIDForRecord(record), number, url, updatedAt)
	record.Object.References = upsertReferenceByType(record.Object.References, &reference, contract.ReferenceTypeGitHubPullRequest)
	record.PullRequestState = state
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

type fakeRunner struct {
	runFn func(context.Context, agent.RunParams) error
}

func (f fakeRunner) Run(ctx context.Context, params agent.RunParams) error {
	if f.runFn != nil {
		return f.runFn(ctx, params)
	}
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
		TrackerKind:                "linear",
		TrackerAPIKey:              "secret-key",
		TrackerProjectSlug:         "demo",
		ActiveStates:               []string{"Todo", "In Progress"},
		TerminalStates:             []string{"Done", "Closed"},
		PollIntervalMS:             10,
		AutomationRootDir:          root,
		WorkspaceRoot:              filepath.Join(root, "workspaces"),
		WorkspaceLinearBranchScope: "demo-scope",
		MaxConcurrentAgents:        2,
		MaxTurns:                   1,
		MaxRetryBackoffMS:          100,
		RunBudgetTotalMS:           1000,
		RunExecutionBudgetMS:       1000,
		RunReviewFixBudgetMS:       0,
		CodexCommand:               "codex app-server",
		DomainID:                   "default",
		ServiceContractVersion:     contract.APIVersionV1,
		ServiceInstanceName:        "symphony",
		LeaderRequired:             true,
		InstanceRole:               contract.InstanceRoleLeader,
		CapabilityContract: contract.CapabilityContract{
			Static: contract.StaticCapabilitySet{
				Capabilities: []contract.StaticCapability{
					{Name: contract.CapabilityStreamEvents, Category: contract.CapabilityCategoryProtocol, Summary: "支持 HTTP/SSE 正式事件流。", Supported: true},
					{Name: contract.CapabilityQueryObjects, Category: contract.CapabilityCategoryQuery, Summary: "支持正式对象查询。", Supported: true},
				},
			},
			Available: contract.AvailableCapabilitySet{
				Capabilities: []contract.AvailableCapability{
					{Name: contract.CapabilityStreamEvents, Category: contract.CapabilityCategoryProtocol, Summary: "支持 HTTP/SSE 正式事件流。", Available: true},
					{Name: contract.CapabilityQueryObjects, Category: contract.CapabilityCategoryQuery, Summary: "支持正式对象查询。", Available: true},
				},
			},
		},
		SessionPersistence: model.SessionPersistenceConfig{
			Enabled: true,
			Kind:    model.SessionPersistenceKindFile,
			File: model.SessionPersistenceFileConfig{
				Path:            filepath.Join(root, "local", "runtime-state.json"),
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
	identity := RuntimeIdentity{
		Compatibility: RuntimeCompatibility{
			ActiveSource: "linear-main",
			SourceKind:   "linear",
		},
	}
	o := NewOrchestrator(trackerClient, workspaceManager, fakeRunner{}, func() *model.ServiceConfig { return cfg }, newTestWorkflow, func() RuntimeIdentity { return identity }, logger)
	o.now = func() time.Time { return time.Date(2026, 3, 14, 10, 0, 0, 0, time.UTC) }
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if o.stateStore != nil {
			_ = o.stateStore.Close(ctx)
		}
	})
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

func prepareQueuedSourceClosureAction(t *testing.T, o *Orchestrator, trackerClient *fakeTracker, issue model.Issue) *sourceClosureActionState {
	t.Helper()
	trackerClient.issuesByID[issue.ID] = issue
	o.prLookup = fakePRLookup{
		refresh: func(context.Context, string, *PullRequestInfo) (*PullRequestInfo, error) {
			return &PullRequestInfo{
				Number:     7,
				URL:        "https://example.invalid/pr/7",
				State:      PullRequestStateMerged,
				HeadBranch: "feature/abc-1",
				BaseOwner:  "acme",
				BaseRepo:   "repo",
				HeadOwner:  "acme",
			}, nil
		},
	}
	record := o.ensureRecordLocked(issue)
	record.Dispatch = &model.DispatchContext{
		JobType:         contract.JobTypeLandChange,
		ExpectedOutcome: model.CompletionModePullRequest,
		OnMissingPR:     model.CompletionActionIntervention,
		OnClosedPR:      model.CompletionActionIntervention,
	}
	record.Run = o.ensureRunState(record, o.currentConfig(), record.Dispatch, 1)
	o.moveToAwaitingMerge(issue.ID, issue.Identifier, issue.State, filepath.Join(o.currentConfig().WorkspaceRoot, issue.Identifier), "feature/abc-1", 0, 0, &PullRequestInfo{
		Number:     7,
		URL:        "https://example.invalid/pr/7",
		State:      PullRequestStateOpen,
		HeadBranch: "feature/abc-1",
	}, nil)
	o.reconcileAwaitingMerge(context.Background())
	for _, item := range o.sourceClosureActions {
		return item
	}
	t.Fatal("queued source closure action was not created")
	return nil
}

func assertUnavailableServiceSurface(
	t *testing.T,
	discovery contract.DiscoveryDocument,
	snapshot contract.ServiceStateSnapshot,
	wantComponent string,
) {
	t.Helper()
	if snapshot.ServiceMode != contract.ServiceModeUnavailable {
		t.Fatalf("snapshot service_mode = %q, want %q", snapshot.ServiceMode, contract.ServiceModeUnavailable)
	}
	if discovery.DomainID != "default" {
		t.Fatalf("discovery.domain_id = %q, want default", discovery.DomainID)
	}
	if len(snapshot.Reasons) != 1 {
		t.Fatalf("snapshot reasons = %#v, want 1 reason", snapshot.Reasons)
	}
	reason := snapshot.Reasons[0]
	if reason.ReasonCode != contract.ReasonServiceUnavailableCoreDependency {
		t.Fatalf("reason_code = %q, want %q", reason.ReasonCode, contract.ReasonServiceUnavailableCoreDependency)
	}
	if got := reason.Details["component"]; got != wantComponent {
		t.Fatalf("reason component = %v, want %q", got, wantComponent)
	}
	if got := reason.Details["source_kind"]; got != contract.SourceKindLinear {
		t.Fatalf("reason source_kind = %v, want %q", got, contract.SourceKindLinear)
	}
	if got := reason.Details["source_name"]; got != "linear-main" {
		t.Fatalf("reason source_name = %v, want linear-main", got)
	}
	detail, ok := reason.Details["detail"].(string)
	if !ok || strings.TrimSpace(detail) == "" {
		t.Fatalf("reason detail = %#v, want non-empty string", reason.Details)
	}
}

func TestEnsureRecordLockedUsesRuntimeSourceIdentity(t *testing.T) {
	o, _, _ := newTestOrchestrator(t)
	o.runtimeIdentityFn = func() RuntimeIdentity {
		return RuntimeIdentity{
			Compatibility: RuntimeCompatibility{
				ActiveSource: "github-main",
				SourceKind:   string(contract.SourceKindGitHubIssues),
			},
		}
	}

	issue := newIssue("42", "GH-42", "Todo")

	o.mu.Lock()
	record := o.ensureRecordLocked(issue)
	o.mu.Unlock()

	if record.Object.ID != "job-github_issues-github-main-42" {
		t.Fatalf("job_id = %q, want job-github_issues-github-main-42", record.Object.ID)
	}
	if strings.Contains(record.Object.ID, "rec_") {
		t.Fatalf("job_id still uses record-centric prefix: %q", record.Object.ID)
	}
}

func TestDiscoveryUsesRuntimeSourceIdentity(t *testing.T) {
	o, _, _ := newTestOrchestrator(t)
	o.runtimeIdentityFn = func() RuntimeIdentity {
		return RuntimeIdentity{
			Compatibility: RuntimeCompatibility{
				ActiveSource: "github-main",
				SourceKind:   string(contract.SourceKindGitHubIssues),
			},
		}
	}

	discovery := o.Discovery()
	if discovery.Source.Kind != contract.SourceKindGitHubIssues {
		t.Fatalf("discovery source.kind = %q, want %q", discovery.Source.Kind, contract.SourceKindGitHubIssues)
	}
	if discovery.Source.Name != "github-main" {
		t.Fatalf("discovery source.name = %q, want github-main", discovery.Source.Name)
	}
	if len(discovery.Capabilities.Capabilities) == 0 {
		t.Fatal("discovery capabilities must not be empty")
	}
}

func TestRunOnceDispatchPreflightFailureExposesUnavailableServiceSurface(t *testing.T) {
	o, trackerClient, _ := newTestOrchestrator(t)
	cfg := o.currentConfig()
	cfg.SessionPersistence.Enabled = false
	cfg.CodexCommand = ""

	o.RunOnce(context.Background(), false)

	if trackerClient.fetchCalls != 0 {
		t.Fatalf("tracker fetch calls = %d, want 0 when dispatch preflight fails", trackerClient.fetchCalls)
	}
	assertUnavailableServiceSurface(t, o.Discovery(), o.Snapshot(), "dispatch_preflight")
	refresh := o.RequestRefresh()
	if refresh.Status != contract.ControlStatusRejected {
		t.Fatalf("refresh status = %q, want %q", refresh.Status, contract.ControlStatusRejected)
	}
}

func TestRunOnceTrackerUnreachableExposesUnavailableServiceSurface(t *testing.T) {
	o, trackerClient, _ := newTestOrchestrator(t)
	cfg := o.currentConfig()
	cfg.SessionPersistence.Enabled = false
	trackerClient.fetchErr = assertErr("tracker down")

	o.RunOnce(context.Background(), false)

	if trackerClient.fetchCalls == 0 {
		t.Fatal("tracker fetch calls = 0, want candidate fetch attempt")
	}
	assertUnavailableServiceSurface(t, o.Discovery(), o.Snapshot(), "task_source")
	refresh := o.RequestRefresh()
	if refresh.Status != contract.ControlStatusRejected {
		t.Fatalf("refresh status = %q, want %q", refresh.Status, contract.ControlStatusRejected)
	}
}

func TestReconcileAwaitingMergeCompletesLandChangeAndQueuesSourceClosureAction(t *testing.T) {
	o, trackerClient, _ := newTestOrchestrator(t)
	issue := newIssue("1", "ABC-1", "Todo")
	actionState := prepareQueuedSourceClosureAction(t, o, trackerClient, issue)

	if len(o.state.ArchivedJobs) != 1 {
		t.Fatalf("archived_jobs = %#v, want 1 completed record", o.state.ArchivedJobs)
	}
	record := o.state.ArchivedJobs[0]
	if record.Outcome == nil || record.Outcome.State != contract.OutcomeConclusionSucceeded {
		t.Fatalf("outcome = %#v, want succeeded", record.Outcome)
	}
	if len(o.sourceClosureActions) != 1 {
		t.Fatalf("len(sourceClosureActions) = %d, want 1", len(o.sourceClosureActions))
	}
	if actionState == nil || actionState.Action.State != contract.ActionStatusQueued {
		t.Fatalf("action_state = %#v, want queued source closure action", actionState)
	}
	envelope, ok := o.GetObject(contract.ObjectTypeAction, actionState.Action.ID)
	if !ok {
		t.Fatal("GetObject(action) = false, want true")
	}
	var action contract.Action
	if err := json.Unmarshal(envelope.Payload, &action); err != nil {
		t.Fatalf("Unmarshal(action) error = %v", err)
	}
	if action.Type != contract.ActionTypeSourceClosure {
		t.Fatalf("action.Type = %q, want %q", action.Type, contract.ActionTypeSourceClosure)
	}
	if len(action.Relations) == 0 || action.Relations[0].Type != contract.RelationTypeActionReference {
		t.Fatalf("action.Relations = %#v, want action.reference relation", action.Relations)
	}
	if _, ok := o.GetObject(contract.ObjectTypeReference, action.Relations[0].TargetID); ok {
		t.Fatalf("GetObject(reference, %q) = true, want false for public query surface", action.Relations[0].TargetID)
	}
	if _, ok := mustGetLedgerObject(t, o, contract.ObjectTypeReference, action.Relations[0].TargetID); !ok {
		t.Fatalf("objectLedger.Get(reference, %q) = false, want true", action.Relations[0].TargetID)
	}
}

func TestReconcileSourceClosureActionsCompletesLifecycle(t *testing.T) {
	o, trackerClient, _ := newTestOrchestrator(t)
	issue := newIssue("1", "ABC-1", "Todo")
	actionState := prepareQueuedSourceClosureAction(t, o, trackerClient, issue)

	o.reconcileSourceClosureActions(context.Background())

	if got := o.sourceClosureActions[actionState.Action.ID].Action.State; got != contract.ActionStatusCompleted {
		t.Fatalf("action state = %q, want %q", got, contract.ActionStatusCompleted)
	}
	envelope, ok := o.GetObject(contract.ObjectTypeAction, actionState.Action.ID)
	if !ok {
		t.Fatal("GetObject(action) = false, want true")
	}
	var action contract.Action
	if err := json.Unmarshal(envelope.Payload, &action); err != nil {
		t.Fatalf("Unmarshal(action) error = %v", err)
	}
	if action.State != contract.ActionStatusCompleted {
		t.Fatalf("action.State = %q, want %q", action.State, contract.ActionStatusCompleted)
	}
}

func TestReconcileSourceClosureActionsTransitionsToExternalPending(t *testing.T) {
	o, trackerClient, _ := newTestOrchestrator(t)
	issue := newIssue("1", "ABC-1", "Todo")
	actionState := prepareQueuedSourceClosureAction(t, o, trackerClient, issue)
	reason := contract.MustReason(contract.ReasonActionExternalPending, map[string]any{"cause": "source_adapter_unavailable"})
	trackerClient.closeSourceResult = tracker.SourceClosureResult{
		Disposition: tracker.SourceClosureDispositionExternalPending,
		Reason:      &reason,
		ErrorCode:   contract.ErrorSourceClosureUnavailable,
	}

	o.reconcileSourceClosureActions(context.Background())

	if got := o.sourceClosureActions[actionState.Action.ID].Action.State; got != contract.ActionStatusExternalPending {
		t.Fatalf("action state = %q, want %q", got, contract.ActionStatusExternalPending)
	}
	jobEnvelope, ok := o.GetObject(contract.ObjectTypeJob, actionState.JobID)
	if !ok {
		t.Fatal("GetObject(job) = false, want true")
	}
	var job contract.Job
	if err := json.Unmarshal(jobEnvelope.Payload, &job); err != nil {
		t.Fatalf("Unmarshal(job) error = %v", err)
	}
	if !job.ActionSummary.HasPendingExternalActions || job.ActionSummary.PendingCount != 1 {
		t.Fatalf("job.ActionSummary = %#v, want one pending external action", job.ActionSummary)
	}
}

func TestReconcileSourceClosureActionsSplitsPermissionConflictsToIntervention(t *testing.T) {
	o, trackerClient, _ := newTestOrchestrator(t)
	issue := newIssue("1", "ABC-1", "Todo")
	actionState := prepareQueuedSourceClosureAction(t, o, trackerClient, issue)
	reason := contract.MustReason(contract.ReasonActionInterventionRequired, map[string]any{"cause": "permission_conflict"})
	trackerClient.closeSourceResult = tracker.SourceClosureResult{
		Disposition: tracker.SourceClosureDispositionInterventionRequired,
		Reason:      &reason,
		ErrorCode:   contract.ErrorSourceClosureConflict,
	}

	o.reconcileSourceClosureActions(context.Background())

	if got := o.sourceClosureActions[actionState.Action.ID].Action.State; got != contract.ActionStatusInterventionRequired {
		t.Fatalf("action state = %q, want %q", got, contract.ActionStatusInterventionRequired)
	}
}

func TestClassifySuccessfulRunReturnsCompletedForMergedTerminalSource(t *testing.T) {
	o, _, _ := newTestOrchestrator(t)
	o.prLookup = fakePRLookup{
		find: func(context.Context, string, string) (*PullRequestInfo, error) {
			return &PullRequestInfo{
				Number:     9,
				URL:        "https://example.invalid/pr/9",
				State:      PullRequestStateMerged,
				HeadBranch: "feature/abc-9",
			}, nil
		},
	}

	decision, err := o.classifySuccessfulRun(context.Background(), filepath.Join(o.currentConfig().WorkspaceRoot, "ABC-9"), "feature/abc-9", freshDispatchContext(normalizeCompletionContract(o.currentWorkflow().Completion)), "Done")
	if err != nil {
		t.Fatalf("classifySuccessfulRun() error = %v", err)
	}
	if decision.Disposition != DispositionCompleted {
		t.Fatalf("decision.Disposition = %q, want %q", decision.Disposition, DispositionCompleted)
	}
}

func TestClassifySuccessfulRunReturnsCompletedForOpenPRCodeChange(t *testing.T) {
	o, _, _ := newTestOrchestrator(t)
	o.prLookup = fakePRLookup{
		find: func(context.Context, string, string) (*PullRequestInfo, error) {
			return &PullRequestInfo{
				Number:     10,
				URL:        "https://example.invalid/pr/10",
				State:      PullRequestStateOpen,
				HeadBranch: "feature/abc-10",
			}, nil
		},
	}

	decision, err := o.classifySuccessfulRun(
		context.Background(),
		filepath.Join(o.currentConfig().WorkspaceRoot, "ABC-10"),
		"feature/abc-10",
		freshDispatchContext(normalizeCompletionContract(o.currentWorkflow().Completion)),
		"In Progress",
	)
	if err != nil {
		t.Fatalf("classifySuccessfulRun() error = %v", err)
	}
	if decision.Disposition != DispositionCompleted {
		t.Fatalf("decision.Disposition = %q, want %q", decision.Disposition, DispositionCompleted)
	}
}

func TestClassifySuccessfulRunReturnsCompletedForMergedActiveCodeChange(t *testing.T) {
	o, _, _ := newTestOrchestrator(t)
	o.prLookup = fakePRLookup{
		find: func(context.Context, string, string) (*PullRequestInfo, error) {
			return &PullRequestInfo{
				Number:     11,
				URL:        "https://example.invalid/pr/11",
				State:      PullRequestStateMerged,
				HeadBranch: "feature/abc-11",
			}, nil
		},
	}

	decision, err := o.classifySuccessfulRun(
		context.Background(),
		filepath.Join(o.currentConfig().WorkspaceRoot, "ABC-11"),
		"feature/abc-11",
		freshDispatchContext(normalizeCompletionContract(o.currentWorkflow().Completion)),
		"In Progress",
	)
	if err != nil {
		t.Fatalf("classifySuccessfulRun() error = %v", err)
	}
	if decision.Disposition != DispositionCompleted {
		t.Fatalf("decision.Disposition = %q, want %q", decision.Disposition, DispositionCompleted)
	}
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

func TestDurableJobStateDoesNotExposeRecordCentricCanonicalFields(t *testing.T) {
	typ := reflect.TypeOf(durableJobState{})
	disallowed := map[string]struct{}{
		"RecordID":            {},
		"SourceRef":           {},
		"Observation":         {},
		"DurableRefs":         {},
		"Result":              {},
		"LastKnownIssue":      {},
		"LastKnownIssueState": {},
	}
	for i := 0; i < typ.NumField(); i++ {
		if _, ok := disallowed[typ.Field(i).Name]; ok {
			t.Fatalf("durableJobState still exposes legacy canonical field %q", typ.Field(i).Name)
		}
	}
}

func TestJobRuntimeDoesNotExposeLegacyCanonicalFields(t *testing.T) {
	typ := reflect.TypeOf(model.JobRuntime{})
	disallowed := map[string]struct{}{
		"RecordID":            {},
		"Result":              {},
		"LastKnownIssue":      {},
		"LastKnownIssueState": {},
		"SourceRef":           {},
		"DurableRefs":         {},
	}
	for i := 0; i < typ.NumField(); i++ {
		if _, ok := disallowed[typ.Field(i).Name]; ok {
			t.Fatalf("JobRuntime still exposes legacy canonical field %q", typ.Field(i).Name)
		}
	}
}

func TestArchivedJobDoesNotExposeLegacyCanonicalFields(t *testing.T) {
	typ := reflect.TypeOf(model.ArchivedJob{})
	disallowed := map[string]struct{}{
		"RecordID":            {},
		"Result":              {},
		"LastKnownIssue":      {},
		"LastKnownIssueState": {},
		"SourceRef":           {},
		"DurableRefs":         {},
	}
	for i := 0; i < typ.NumField(); i++ {
		if _, ok := disallowed[typ.Field(i).Name]; ok {
			t.Fatalf("ArchivedJob still exposes legacy canonical field %q", typ.Field(i).Name)
		}
	}
}

func TestFormalObjectIDsDoNotUseRecordCentricPrefix(t *testing.T) {
	o, _, _ := newTestOrchestrator(t)
	record := o.ensureRecordLocked(newIssue("1", "ABC-1", "Todo"))

	for _, value := range []string{
		jobIDForRecord(record),
		runIDForRecord(record, 1),
		interventionIDForRecord(record),
		outcomeIDForRecord(record),
		artifactIDForRecord(record, "pull-request"),
		sourceClosureActionIDForRecord(record),
		sourceReferenceIDForRecord(record),
		branchReferenceIDForRecord(record),
		pullRequestReferenceIDForRecord(record),
	} {
		if strings.Contains(value, "rec_") {
			t.Fatalf("formal object id %q still uses record-centric prefix", value)
		}
	}
}

func TestBuildPersistedStateUsesLedgerRecordsAndNoLegacyDumpKeys(t *testing.T) {
	o, _, _ := newTestOrchestrator(t)
	issue := newIssue("1", "ABC-1", "In Progress")
	record := o.ensureRecordLocked(issue)
	dispatch := continuationDispatchContext(freshDispatchContext(normalizeCompletionContract(o.currentWorkflow().Completion)), normalizeCompletionContract(o.currentWorkflow().Completion), model.ContinuationReasonExecutionBudgetExhausted, "runner/demo-scope/abc-1", nil, issue.State)
	dispatch.RecoveryCheckpoint = &model.RecoveryCheckpoint{
		ArtifactID: "art-1",
		Checkpoint: contract.Checkpoint{
			Type:        contract.CheckpointTypeRecovery,
			Summary:     "saved recovery",
			CapturedAt:  o.now().UTC().Format(time.RFC3339Nano),
			Stage:       contract.RunPhaseSummaryExecuting,
			ArtifactIDs: []string{"art-1"},
		},
	}
	record.Run = o.ensureRunState(record, o.currentConfig(), dispatch, 1)
	o.scheduleContinuationLocked(issue.ID, issue.Identifier, 1, ptrString("runner failed"), 1, dispatch)
	completed := &model.JobRuntime{
		Object: contract.Job{
			BaseObject: contract.BaseObject{
				ID:         "job-done",
				ObjectType: contract.ObjectTypeJob,
			},
		},
		Lifecycle:   model.JobLifecycleCompleted,
		UpdatedAt:   o.now().UTC().Format(time.RFC3339Nano),
		SourceState: "Done",
		Outcome: &contract.Outcome{
			State:       contract.OutcomeConclusionSucceeded,
			Summary:     "done",
			CompletedAt: o.now().UTC().Format(time.RFC3339Nano),
		},
	}
	setTestLinearSourceReference(completed, "done", "DONE-1", completed.UpdatedAt)
	o.rememberCompletedLocked(completed)
	setTestWorkspacePath(record, filepath.Join(o.currentConfig().WorkspaceRoot, issue.Identifier))

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
	jobs, ok := payload["jobs"].([]any)
	if !ok || len(jobs) != 2 {
		t.Fatalf("jobs = %#v, want 2 entries", payload["jobs"])
	}
	firstJob, ok := jobs[0].(map[string]any)
	if !ok {
		t.Fatalf("jobs[0] = %#v, want object", jobs[0])
	}
	for _, forbidden := range []string{"job", "outcome", "artifacts", "record_id", "source_ref", "observation", "durable_refs", "result", "last_known_issue", "last_known_issue_state"} {
		if _, ok := firstJob[forbidden]; ok {
			t.Fatalf("jobs[0] still exposes mirrored formal field %q: %#v", forbidden, firstJob)
		}
	}
	if strings.Contains(string(raw), "\"record_id\"") {
		t.Fatalf("persisted payload still exposes legacy record_id naming: %s", string(raw))
	}
}

func TestRestorePersistedStateRebuildsRecordIndexesAndArchivedJobs(t *testing.T) {
	o, _, _ := newTestOrchestrator(t)
	retryDue := o.now().Add(10 * time.Second).Format(time.RFC3339Nano)
	updatedAt := o.now().UTC().Format(time.RFC3339Nano)
	state := durableRuntimeState{
		Version:  durableStateVersion,
		Identity: o.currentRuntimeIdentity(),
		SavedAt:  o.now().UTC(),
		Service: durableServiceMetadata{
			TokenTotal: model.TokenTotals{InputTokens: 11},
		},
		Jobs: []durableJobState{
			{
				JobID:      "job-1",
				Reason:     ptrReason(contract.MustReason(contract.ReasonRunContinuationPending, map[string]any{"attempt": 3})),
				RetryDueAt: &retryDue,
				Run: &durableRunRuntimeState{
					RunID:  "run-1",
					Budget: model.RunBudget{ExecutionMS: 100},
				},
				UpdatedAt:    updatedAt,
				RetryAttempt: 2,
				StallCount:   1,
			},
			{
				JobID:            "job-2",
				Reason:           ptrReason(contract.MustReason(contract.ReasonRecordBlockedAwaitingMerge, map[string]any{"pr_number": 42})),
				PullRequestState: "open",
				Run: &durableRunRuntimeState{
					RunID: "run-2",
				},
				UpdatedAt:    updatedAt,
				RetryAttempt: 1,
			},
			{
				JobID:     "job-done",
				OutcomeID: "outcome-done",
				UpdatedAt: updatedAt,
			},
		},
		FormalObjects: mustFormalObjectSnapshot(t,
			contract.Job{
				BaseObject:    contract.BaseObject{ID: "job-1", ObjectType: contract.ObjectTypeJob, UpdatedAt: updatedAt},
				ObjectContext: contract.ObjectContext{References: []contract.Reference{testLinearIssueReference("ref-job-1", "1", "ABC-1", updatedAt)}},
				State:         contract.JobStatusRunning,
				JobType:       contract.JobTypeCodeChange,
			},
			contract.Run{BaseObject: contract.BaseObject{ID: "run-1", ObjectType: contract.ObjectTypeRun, UpdatedAt: updatedAt}, State: contract.RunStatusContinuationPending, Phase: contract.RunPhaseSummaryBlocked, Attempt: 2},
			contract.Job{
				BaseObject: contract.BaseObject{ID: "job-2", ObjectType: contract.ObjectTypeJob, UpdatedAt: updatedAt},
				ObjectContext: contract.ObjectContext{
					References: []contract.Reference{
						testLinearIssueReference("ref-job-2", "2", "ABC-2", updatedAt),
						testBranchReference("ref-branch-2", "feature/abc-2", updatedAt),
						testPullRequestReference("ref-pr-2", 42, "https://github.example/pr/42", updatedAt),
					},
				},
				State:   contract.JobStatusRunning,
				JobType: contract.JobTypeCodeChange,
			},
			contract.Run{
				BaseObject: contract.BaseObject{ID: "run-2", ObjectType: contract.ObjectTypeRun, UpdatedAt: updatedAt},
				ObjectContext: contract.ObjectContext{
					References: []contract.Reference{
						testBranchReference("ref-run-branch-2", "feature/abc-2", updatedAt),
						testPullRequestReference("ref-run-pr-2", 42, "https://github.example/pr/42", updatedAt),
					},
				},
				State:   contract.RunStatusCompleted,
				Phase:   contract.RunPhaseSummaryPublishing,
				Attempt: 1,
			},
			contract.Job{
				BaseObject:    contract.BaseObject{ID: "job-done", ObjectType: contract.ObjectTypeJob, UpdatedAt: updatedAt},
				ObjectContext: contract.ObjectContext{References: []contract.Reference{testLinearIssueReference("ref-job-done", "done", "DONE-1", updatedAt)}},
				State:         contract.JobStatusCompleted,
				JobType:       contract.JobTypeCodeChange,
			},
			contract.Outcome{BaseObject: contract.BaseObject{ID: "outcome-done", ObjectType: contract.ObjectTypeOutcome, UpdatedAt: updatedAt}, State: contract.OutcomeConclusionSucceeded, Summary: "done", CompletedAt: updatedAt},
		),
	}

	o.restorePersistedStateLocked(&state)

	if len(o.continuationRuns) != 1 {
		t.Fatalf("continuationRuns size = %d, want 1", len(o.continuationRuns))
	}
	if len(o.candidateDeliveryJobs) != 1 {
		t.Fatalf("candidateDeliveryJobs size = %d, want 1", len(o.candidateDeliveryJobs))
	}
	if len(o.state.ArchivedJobs) != 1 {
		t.Fatalf("ArchivedJobs size = %d, want 1", len(o.state.ArchivedJobs))
	}
	if o.state.CodexTotals.InputTokens != 11 {
		t.Fatalf("CodexTotals.InputTokens = %d, want 11", o.state.CodexTotals.InputTokens)
	}
	restored := o.state.Jobs["1"]
	if restored == nil || restored.Object.ID != "job-1" || restored.Run == nil || restored.Run.Object.ID != "run-1" {
		t.Fatalf("restored runtime = %#v, want hydrated formal job/run ids", restored)
	}
}

func TestRuntimeFromDurableJobUsesFormalObjectsAndRuntimeSupplements(t *testing.T) {
	o, _, _ := newTestOrchestrator(t)
	updatedAt := o.now().UTC().Format(time.RFC3339Nano)
	o.restoreObjectLedgerSnapshotLocked(mustFormalObjectSnapshot(t,
		contract.Job{
			BaseObject: contract.BaseObject{ID: "job-1", ObjectType: contract.ObjectTypeJob, UpdatedAt: updatedAt},
			ObjectContext: contract.ObjectContext{
				References: []contract.Reference{
					testLinearIssueReference("ref-job-1-source", "1", "ABC-1", updatedAt),
					testBranchReference("ref-job-1-branch", "feature/abc-1", updatedAt),
					testPullRequestReference("ref-job-1-pr", 42, "https://github.example/pr/42", updatedAt),
				},
			},
			State:   contract.JobStatusRunning,
			JobType: contract.JobTypeCodeChange,
		},
		contract.Run{
			BaseObject: contract.BaseObject{ID: "run-1", ObjectType: contract.ObjectTypeRun, UpdatedAt: updatedAt},
			State:      contract.RunStatusContinuationPending,
			Phase:      contract.RunPhaseSummaryBlocked,
			Attempt:    1,
		},
	))

	record := o.runtimeFromDurableJob(durableJobState{
		JobID:            "job-1",
		WorkspacePath:    filepath.Join(t.TempDir(), "workspaces", "ABC-1"),
		PullRequestState: "merged",
		UpdatedAt:        updatedAt,
		Run: &durableRunRuntimeState{
			RunID: "run-1",
		},
	})

	if record == nil || record.Object.ID != "job-1" {
		t.Fatalf("runtime job = %#v, want hydrated formal job", record)
	}
	if record.Run == nil || record.Run.Object.ID != "run-1" {
		t.Fatalf("runtime run = %#v, want hydrated formal run", record.Run)
	}
	if got := recordSourceID(record); got != "1" {
		t.Fatalf("recordSourceID = %q, want 1", got)
	}
	if got := recordIdentifier(record); got != "ABC-1" {
		t.Fatalf("recordIdentifier = %q, want ABC-1", got)
	}
	if got := recordBranch(record); got != "feature/abc-1" {
		t.Fatalf("recordBranch = %q, want feature/abc-1", got)
	}
	pr := recordPullRequest(record)
	if pr == nil || pr.Number != 42 || pr.State != PullRequestStateMerged {
		t.Fatalf("recordPullRequest = %#v, want merged PR #42", pr)
	}
	if got := recordWorkspacePath(record); got == "" {
		t.Fatalf("recordWorkspacePath = %q, want restored workspace path", got)
	}
}

func TestJobObjectFromRecordUsesFormalReferencesOnly(t *testing.T) {
	o, _, _ := newTestOrchestrator(t)
	issue := newIssue("1", "ABC-1", "In Progress")
	record := o.ensureRecordLocked(issue)
	updatedAt := o.now().UTC().Format(time.RFC3339Nano)
	setTestBranchReference(record, "feature/abc-1", updatedAt)
	setTestPullRequestReference(record, 42, "https://github.example/pr/42", "open", updatedAt)

	job := o.jobObjectFromRecord(record, contract.JobStatusRunning)
	if len(job.References) != 3 {
		t.Fatalf("job references = %#v, want source + branch + pr", job.References)
	}
	if got := sourceRefFromFormalObjects(job).SourceID; got != issue.ID {
		t.Fatalf("sourceRefFromFormalObjects(job).SourceID = %q, want %q", got, issue.ID)
	}
	if got := recordBranch(&model.JobRuntime{Object: job}); got != "feature/abc-1" {
		t.Fatalf("recordBranch(job) = %q, want feature/abc-1", got)
	}
	if pr := recordPullRequest(&model.JobRuntime{Object: job, PullRequestState: "open"}); pr == nil || pr.Number != 42 {
		t.Fatalf("recordPullRequest(job) = %#v, want PR #42", pr)
	}
}

func TestConservativeRecoveryMovesUnknownActiveRecordToAwaitingIntervention(t *testing.T) {
	o, trackerClient, _ := newTestOrchestrator(t)
	issue := newIssue("1", "ABC-1", "In Progress")
	trackerClient.issuesByID[issue.ID] = issue
	record := o.ensureRecordLocked(issue)
	record.NeedsRecovery = true
	o.setRecordStatusLocked(record, model.JobLifecycleActive, nil, &contract.Observation{Running: false, Summary: "recovery pending"})

	o.reconcileRecovering(context.Background())

	current := o.interventionForIssueLocked(issue.ID)
	if current == nil {
		t.Fatal("awaitingIntervention record missing after conservative recovery")
	}
	if current.Reason == nil || current.Reason.ReasonCode != contract.ReasonRecordBlockedRecoveryUncertain {
		t.Fatalf("Reason = %#v, want %q", current.Reason, contract.ReasonRecordBlockedRecoveryUncertain)
	}
}

func TestConservativeRecoveryCompletesCodeChangeAtReviewablePR(t *testing.T) {
	o, trackerClient, _ := newTestOrchestrator(t)
	issue := newIssue("1", "ABC-1", "In Progress")
	trackerClient.issuesByID[issue.ID] = issue
	record := o.ensureRecordLocked(issue)
	record.NeedsRecovery = true
	record.Dispatch = freshDispatchContext(normalizeCompletionContract(o.currentWorkflow().Completion))
	setTestWorkspacePath(record, filepath.Join(o.currentConfig().WorkspaceRoot, issue.Identifier))
	setTestBranchReference(record, "feature/abc-1", o.now().UTC().Format(time.RFC3339Nano))
	record.Run = o.ensureRunState(record, o.currentConfig(), record.Dispatch, 1)
	record.Run.Object.ReviewGate.Status = contract.ReviewGateStatusPassed
	syncRunStateMirrorsFromObject(record.Run)
	o.prLookup = fakePRLookup{
		find: func(context.Context, string, string) (*PullRequestInfo, error) {
			return &PullRequestInfo{Number: 42, URL: "https://github.example/pr/42", State: PullRequestStateOpen, HeadBranch: "feature/abc-1"}, nil
		},
	}

	o.reconcileRecovering(context.Background())

	if current := o.candidateDeliveryForIssueLocked(issue.ID); current != nil {
		t.Fatalf("candidateDeliveryForIssueLocked() = %#v, want nil for completed code_change", current)
	}
	if len(o.state.ArchivedJobs) != 1 {
		t.Fatalf("ArchivedJobs size = %d, want 1", len(o.state.ArchivedJobs))
	}
	if got := o.state.ArchivedJobs[0].Outcome.State; got != contract.OutcomeConclusionSucceeded {
		t.Fatalf("Outcome.State = %q, want %q", got, contract.OutcomeConclusionSucceeded)
	}
}

func TestConservativeRecoveryCanPromoteLandChangeToAwaitingMerge(t *testing.T) {
	o, trackerClient, _ := newTestOrchestrator(t)
	issue := newIssue("1", "ABC-1", "In Progress")
	trackerClient.issuesByID[issue.ID] = issue
	record := o.ensureRecordLocked(issue)
	record.NeedsRecovery = true
	record.Dispatch = &model.DispatchContext{
		JobType:         contract.JobTypeLandChange,
		Kind:            model.DispatchKindFresh,
		ExpectedOutcome: model.CompletionModePullRequest,
		OnMissingPR:     model.CompletionActionIntervention,
		OnClosedPR:      model.CompletionActionIntervention,
	}
	setTestWorkspacePath(record, filepath.Join(o.currentConfig().WorkspaceRoot, issue.Identifier))
	setTestBranchReference(record, "feature/abc-1", o.now().UTC().Format(time.RFC3339Nano))
	record.Run = o.ensureRunState(record, o.currentConfig(), record.Dispatch, 1)
	record.Run.Object.ReviewGate.Status = contract.ReviewGateStatusPassed
	syncRunStateMirrorsFromObject(record.Run)
	o.prLookup = fakePRLookup{
		find: func(context.Context, string, string) (*PullRequestInfo, error) {
			return &PullRequestInfo{Number: 42, URL: "https://github.example/pr/42", State: PullRequestStateOpen, HeadBranch: "feature/abc-1"}, nil
		},
	}

	o.reconcileRecovering(context.Background())

	current := o.candidateDeliveryForIssueLocked(issue.ID)
	if current == nil {
		t.Fatal("awaitingMerge record missing after recoverable post-run classification")
	}
	if current.Reason == nil || current.Reason.ReasonCode != contract.ReasonRecordBlockedAwaitingMerge {
		t.Fatalf("Reason = %#v, want %q", current.Reason, contract.ReasonRecordBlockedAwaitingMerge)
	}
}

func TestPullRequestInfoFromAwaitingMergeUsesPersistedReasonDetails(t *testing.T) {
	record := &model.JobRuntime{
		Object: contract.Job{
			BaseObject: contract.BaseObject{
				ID:         "job-abc-1",
				ObjectType: contract.ObjectTypeJob,
			},
		},
		Lifecycle:        model.JobLifecycleAwaitingMerge,
		PullRequestState: "open",
		Reason: ptrReason(contract.MustReason(contract.ReasonRecordBlockedAwaitingMerge, map[string]any{
			"pr_base_owner": "IIwate",
			"pr_base_repo":  "symphony-go",
			"pr_head_owner": "IIwate",
		})),
	}
	setTestBranchReference(record, "feature/abc-1", time.Now().UTC().Format(time.RFC3339Nano))
	setTestPullRequestReference(record, 42, "https://github.example/pr/42", "open", time.Now().UTC().Format(time.RFC3339Nano))

	pr := pullRequestInfoFromAwaitingMerge(record)
	if pr == nil {
		t.Fatal("pullRequestInfoFromAwaitingMerge() = nil")
	}
	if pr.BaseOwner != "IIwate" || pr.BaseRepo != "symphony-go" || pr.HeadOwner != "IIwate" {
		t.Fatalf("pull request owners = %+v", pr)
	}
}

func TestReconcileAwaitingMergeKeepsPersistedPRWhenRefreshReturnsNil(t *testing.T) {
	o, trackerClient, _ := newTestOrchestrator(t)
	issue := newIssue("1", "ABC-1", "In Progress")
	trackerClient.issuesByID[issue.ID] = issue
	o.prLookup = fakePRLookup{
		refresh: func(context.Context, string, *PullRequestInfo) (*PullRequestInfo, error) {
			return nil, nil
		},
	}
	record := o.ensureRecordLocked(issue)
	record.Dispatch = &model.DispatchContext{
		JobType:         contract.JobTypeLandChange,
		Kind:            model.DispatchKindFresh,
		ExpectedOutcome: model.CompletionModePullRequest,
		OnMissingPR:     model.CompletionActionIntervention,
		OnClosedPR:      model.CompletionActionIntervention,
	}
	record.Run = o.ensureRunState(record, o.currentConfig(), record.Dispatch, 1)
	o.moveToAwaitingMerge(
		issue.ID,
		issue.Identifier,
		issue.State,
		filepath.Join(o.currentConfig().WorkspaceRoot, issue.Identifier),
		"feature/abc-1",
		1,
		0,
		&PullRequestInfo{
			Number:    42,
			URL:       "https://github.example/pr/42",
			State:     PullRequestStateOpen,
			BaseOwner: "IIwate",
			BaseRepo:  "symphony-go",
			HeadOwner: "IIwate",
		},
		nil,
	)

	o.reconcileAwaitingMerge(context.Background())

	current := o.candidateDeliveryForIssueLocked(issue.ID)
	if current == nil {
		t.Fatal("awaitingMerge record missing")
	}
	if current.Lifecycle != model.JobLifecycleAwaitingMerge {
		t.Fatalf("Lifecycle = %q, want %q", current.Lifecycle, model.JobLifecycleAwaitingMerge)
	}
}

func TestReconcileAwaitingMergeCompletesLegacyCodeChangeRecord(t *testing.T) {
	o, trackerClient, _ := newTestOrchestrator(t)
	issue := newIssue("1", "ABC-1", "In Progress")
	trackerClient.issuesByID[issue.ID] = issue
	record := o.ensureRecordLocked(issue)
	record.Dispatch = freshDispatchContext(normalizeCompletionContract(o.currentWorkflow().Completion))
	record.Run = o.ensureRunState(record, o.currentConfig(), record.Dispatch, 1)
	o.moveToAwaitingMerge(
		issue.ID,
		issue.Identifier,
		issue.State,
		filepath.Join(o.currentConfig().WorkspaceRoot, issue.Identifier),
		"feature/abc-1",
		1,
		0,
		&PullRequestInfo{
			Number:    42,
			URL:       "https://github.example/pr/42",
			State:     PullRequestStateOpen,
			BaseOwner: "IIwate",
			BaseRepo:  "symphony-go",
			HeadOwner: "IIwate",
		},
		nil,
	)

	o.reconcileAwaitingMerge(context.Background())

	if current := o.candidateDeliveryForIssueLocked(issue.ID); current != nil {
		t.Fatalf("candidateDeliveryForIssueLocked() = %#v, want nil after code_change completion", current)
	}
	if len(o.state.ArchivedJobs) != 1 || o.state.ArchivedJobs[0].Outcome == nil {
		t.Fatalf("ArchivedJobs = %#v, want completed archived code_change", o.state.ArchivedJobs)
	}
	if got := o.state.ArchivedJobs[0].Outcome.State; got != contract.OutcomeConclusionSucceeded {
		t.Fatalf("Outcome.State = %q, want %q", got, contract.OutcomeConclusionSucceeded)
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

	if len(o.state.ArchivedJobs) != 1 {
		t.Fatalf("ArchivedJobs size = %d, want 1", len(o.state.ArchivedJobs))
	}
	completed := o.state.ArchivedJobs[0]
	if completed.Outcome == nil || completed.Outcome.State != contract.OutcomeConclusionSucceeded {
		t.Fatalf("Outcome = %#v, want succeeded", completed.Outcome)
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

	if len(o.state.ArchivedJobs) != 1 {
		t.Fatalf("ArchivedJobs size = %d, want 1", len(o.state.ArchivedJobs))
	}
	if got := o.state.ArchivedJobs[0].Outcome.State; got != contract.OutcomeConclusionAbandoned {
		t.Fatalf("Outcome.State = %q, want %q", got, contract.OutcomeConclusionAbandoned)
	}
}

func TestScheduleContinuationLockedPropagatesFormalReasonIntoRuntimeAndLedger(t *testing.T) {
	o, _, _ := newTestOrchestrator(t)
	issue := newIssue("1", "ABC-1", "In Progress")
	record := o.ensureRecordLocked(issue)
	dispatch := continuationDispatchContext(freshDispatchContext(normalizeCompletionContract(o.currentWorkflow().Completion)), normalizeCompletionContract(o.currentWorkflow().Completion), model.ContinuationReasonExecutionBudgetExhausted, "runner/demo-scope/abc-1", nil, issue.State)
	dispatch.RecoveryCheckpoint = &model.RecoveryCheckpoint{
		ArtifactID: "art-cont-1",
		Checkpoint: contract.Checkpoint{
			Type:        contract.CheckpointTypeRecovery,
			Summary:     "saved recovery",
			CapturedAt:  o.now().UTC().Format(time.RFC3339Nano),
			Stage:       contract.RunPhaseSummaryExecuting,
			ArtifactIDs: []string{"art-cont-1"},
		},
	}
	record.Run = o.ensureRunState(record, o.currentConfig(), dispatch, 1)

	o.scheduleContinuationLocked(issue.ID, issue.Identifier, 1, ptrString("runner failed"), 1, dispatch)

	record = o.continuationForIssueLocked(issue.ID)
	if record == nil {
		t.Fatal("continuation record missing")
	}
	if record.Reason == nil || record.Reason.ReasonCode != contract.ReasonRunContinuationPending {
		t.Fatalf("Reason = %#v, want %q", record.Reason, contract.ReasonRunContinuationPending)
	}
	state := o.buildPersistedStateLocked()
	if len(state.Jobs) != 1 {
		t.Fatalf("persisted jobs = %d, want 1", len(state.Jobs))
	}
	if state.Jobs[0].Reason == nil || state.Jobs[0].Reason.ReasonCode != contract.ReasonRunContinuationPending {
		t.Fatalf("durable reason = %#v, want %q", state.Jobs[0].Reason, contract.ReasonRunContinuationPending)
	}
	if state.Jobs[0].RetryDueAt == nil {
		t.Fatal("durable RetryDueAt missing")
	}
}

func TestScheduleContinuationLockedRearmDoesNotDuplicateCheckpoint(t *testing.T) {
	o, _, _ := newTestOrchestrator(t)
	issue := newIssue("1", "ABC-1", "In Progress")
	record := o.ensureRecordLocked(issue)
	dispatch := continuationDispatchContext(freshDispatchContext(normalizeCompletionContract(o.currentWorkflow().Completion)), normalizeCompletionContract(o.currentWorkflow().Completion), model.ContinuationReasonExecutionBudgetExhausted, "runner/demo-scope/abc-1", nil, issue.State)
	dispatch.RecoveryCheckpoint = &model.RecoveryCheckpoint{
		ArtifactID: "art-cont-1",
		Checkpoint: contract.Checkpoint{
			Type:        contract.CheckpointTypeRecovery,
			Summary:     "saved recovery",
			CapturedAt:  o.now().UTC().Format(time.RFC3339Nano),
			Stage:       contract.RunPhaseSummaryExecuting,
			ArtifactIDs: []string{"art-cont-1"},
			BaseSHA:     "abc123",
			Branch:      "runner/demo-scope/abc-1",
		},
	}
	record.Run = o.ensureRunState(record, o.currentConfig(), dispatch, 1)

	o.scheduleContinuationLocked(issue.ID, issue.Identifier, 1, nil, 0, dispatch)
	o.scheduleContinuationLocked(issue.ID, issue.Identifier, 1, ptrString("no available orchestrator slots"), 0, dispatch)

	current := o.continuationForIssueLocked(issue.ID)
	if current == nil || current.Run == nil {
		t.Fatal("continuation record missing")
	}
	if len(current.Run.Checkpoints) != 1 {
		t.Fatalf("run checkpoints = %#v, want single checkpoint after rearm", current.Run.Checkpoints)
	}
}

func TestHandleWorkerExitTurnTimeoutSchedulesContinuationWithRecoveryCheckpoint(t *testing.T) {
	o, trackerClient, _ := newTestOrchestrator(t)
	issue := newIssue("1", "ABC-1", "In Progress")
	trackerClient.issuesByID[issue.ID] = issue

	startedAt := o.now().Add(-2 * time.Second)
	o.mu.Lock()
	record := o.ensureRecordLocked(issue)
	record.Lifecycle = model.JobLifecycleActive
	record.Observation = &contract.Observation{Running: true, Summary: "running"}
	setTestWorkspacePath(record, filepath.Join(o.currentConfig().WorkspaceRoot, issue.Identifier))
	record.Dispatch = freshDispatchContext(normalizeCompletionContract(o.currentWorkflow().Completion))
	record.StartedAt = &startedAt
	record.Run = o.ensureRunState(record, o.currentConfig(), record.Dispatch, 1)
	o.reindexRecordLocked(issue.ID, record)
	o.mu.Unlock()

	o.captureCheckpointFn = func(context.Context, *model.JobRuntime, contract.RunPhaseSummary, *contract.Reason) (*model.RecoveryCheckpoint, error) {
		return &model.RecoveryCheckpoint{
			ArtifactID:    "art-timeout-1",
			PatchPath:     filepath.Join(t.TempDir(), "timeout.patch"),
			WorkspacePath: filepath.Join(o.currentConfig().WorkspaceRoot, issue.Identifier),
			Checkpoint: contract.Checkpoint{
				Type:        contract.CheckpointTypeRecovery,
				Summary:     "已记录恢复 checkpoint。",
				CapturedAt:  o.now().UTC().Format(time.RFC3339Nano),
				Stage:       contract.RunPhaseSummaryExecuting,
				ArtifactIDs: []string{"art-timeout-1"},
				BaseSHA:     "abc123",
				Branch:      "runner/demo-scope/abc-1",
			},
		}, nil
	}

	o.handleWorkerExit(WorkerResult{
		IssueID:   issue.ID,
		StartedAt: startedAt,
		Phase:     model.PhaseTimedOut,
		Err:       model.NewAgentError(model.ErrTurnTimeout, "turn timed out", nil),
	})

	current := o.continuationForIssueLocked(issue.ID)
	if current == nil {
		t.Fatal("continuation record missing after turn timeout")
	}
	if current.Reason == nil || current.Reason.ReasonCode != contract.ReasonRunContinuationPending {
		t.Fatalf("runtime reason = %#v, want %q", current.Reason, contract.ReasonRunContinuationPending)
	}
	if current.Dispatch == nil || current.Dispatch.Kind != model.DispatchKindContinuation {
		t.Fatalf("dispatch = %#v, want continuation dispatch", current.Dispatch)
	}
	if current.Dispatch.RecoveryCheckpoint == nil || current.Dispatch.RecoveryCheckpoint.ArtifactID != "art-timeout-1" {
		t.Fatalf("recovery checkpoint = %#v, want art-timeout-1", current.Dispatch.RecoveryCheckpoint)
	}
	if current.Run == nil || current.Run.State != contract.RunStatusContinuationPending {
		t.Fatalf("run state = %#v, want continuation_pending", current.Run)
	}
	if current.Run.Recovery == nil || current.Run.Recovery.Checkpoint.Type != contract.CheckpointTypeRecovery {
		t.Fatalf("run recovery = %#v, want recovery checkpoint", current.Run.Recovery)
	}
}

func TestHandleWorkerExitHardViolationMovesToIntervention(t *testing.T) {
	o, trackerClient, _ := newTestOrchestrator(t)
	issue := newIssue("1", "ABC-1", "In Progress")
	trackerClient.issuesByID[issue.ID] = issue

	startedAt := o.now().Add(-time.Second)
	o.mu.Lock()
	record := o.ensureRecordLocked(issue)
	record.Lifecycle = model.JobLifecycleActive
	record.Observation = &contract.Observation{Running: true, Summary: "running"}
	setTestWorkspacePath(record, filepath.Join(o.currentConfig().WorkspaceRoot, issue.Identifier))
	record.Dispatch = freshDispatchContext(normalizeCompletionContract(o.currentWorkflow().Completion))
	record.StartedAt = &startedAt
	record.Run = o.ensureRunState(record, o.currentConfig(), record.Dispatch, 1)
	o.reindexRecordLocked(issue.ID, record)
	o.mu.Unlock()

	o.handleWorkerExit(WorkerResult{
		IssueID:   issue.ID,
		StartedAt: startedAt,
		Phase:     model.PhaseFailed,
		Err:       model.NewHardViolationError(model.HardViolationSubAgent, "spawn_agent", "sub-agents are forbidden"),
	})

	current := o.interventionForIssueLocked(issue.ID)
	if current == nil {
		t.Fatal("awaitingIntervention record missing after hard violation")
	}
	if current.Run == nil || current.Run.State != contract.RunStatusInterventionRequired {
		t.Fatalf("run state = %#v, want intervention_required", current.Run)
	}
	if current.Run.ErrorCode != contract.ErrorRunHardViolationDetected {
		t.Fatalf("run error_code = %q, want %q", current.Run.ErrorCode, contract.ErrorRunHardViolationDetected)
	}
	if current.Run.Reason == nil || current.Run.Reason.ReasonCode != contract.ReasonRunHardViolationDetected {
		t.Fatalf("run reason = %#v, want %q", current.Run.Reason, contract.ReasonRunHardViolationDetected)
	}
}

func TestHandleWorkerExitFailureCompletesJobWithFailedOutcome(t *testing.T) {
	o, trackerClient, _ := newTestOrchestrator(t)
	issue := newIssue("1", "ABC-1", "In Progress")
	trackerClient.issuesByID[issue.ID] = issue

	startedAt := o.now().Add(-time.Second)
	o.mu.Lock()
	record := o.ensureRecordLocked(issue)
	record.Lifecycle = model.JobLifecycleActive
	record.Observation = &contract.Observation{Running: true, Summary: "running"}
	setTestWorkspacePath(record, filepath.Join(o.currentConfig().WorkspaceRoot, issue.Identifier))
	record.Dispatch = freshDispatchContext(normalizeCompletionContract(o.currentWorkflow().Completion))
	record.StartedAt = &startedAt
	record.Run = o.ensureRunState(record, o.currentConfig(), record.Dispatch, 1)
	o.reindexRecordLocked(issue.ID, record)
	o.mu.Unlock()

	o.handleWorkerExit(WorkerResult{
		IssueID:   issue.ID,
		StartedAt: startedAt,
		Phase:     model.PhaseFailed,
		Err:       assertErr("runner failed"),
	})

	if current := o.state.Jobs[issue.ID]; current != nil {
		t.Fatalf("active job = %#v, want nil after failed outcome", current)
	}
	if len(o.state.ArchivedJobs) != 1 {
		t.Fatalf("ArchivedJobs size = %d, want 1", len(o.state.ArchivedJobs))
	}
	completed := o.state.ArchivedJobs[0]
	if completed.Outcome == nil || completed.Outcome.State != contract.OutcomeConclusionFailed {
		t.Fatalf("Outcome = %#v, want failed", completed.Outcome)
	}
	if completed.Run == nil || completed.Run.State != contract.RunStatusFailed {
		t.Fatalf("Run = %#v, want failed", completed.Run)
	}
	if completed.Object.State != contract.JobStatusFailed {
		t.Fatalf("Job state = %q, want failed", completed.Object.State)
	}
}

func TestHandleStalledRunningRecordSchedulesContinuationWithCheckpoint(t *testing.T) {
	o, trackerClient, _ := newTestOrchestrator(t)
	issue := newIssue("1", "ABC-1", "In Progress")
	trackerClient.issuesByID[issue.ID] = issue

	startedAt := o.now().Add(-time.Minute)
	o.mu.Lock()
	record := o.ensureRecordLocked(issue)
	record.Lifecycle = model.JobLifecycleActive
	record.Observation = &contract.Observation{Running: true, Summary: "running"}
	setTestWorkspacePath(record, filepath.Join(o.currentConfig().WorkspaceRoot, issue.Identifier))
	record.Dispatch = freshDispatchContext(normalizeCompletionContract(o.currentWorkflow().Completion))
	record.StartedAt = &startedAt
	record.Run = o.ensureRunState(record, o.currentConfig(), record.Dispatch, 1)
	o.reindexRecordLocked(issue.ID, record)
	o.mu.Unlock()

	o.captureCheckpointFn = func(context.Context, *model.JobRuntime, contract.RunPhaseSummary, *contract.Reason) (*model.RecoveryCheckpoint, error) {
		return &model.RecoveryCheckpoint{
			ArtifactID:    "art-stall-1",
			PatchPath:     filepath.Join(t.TempDir(), "stall.patch"),
			WorkspacePath: filepath.Join(o.currentConfig().WorkspaceRoot, issue.Identifier),
			Checkpoint: contract.Checkpoint{
				Type:        contract.CheckpointTypeRecovery,
				Summary:     "已记录恢复 checkpoint。",
				CapturedAt:  o.now().UTC().Format(time.RFC3339Nano),
				Stage:       contract.RunPhaseSummaryBlocked,
				ArtifactIDs: []string{"art-stall-1"},
				BaseSHA:     "abc123",
				Branch:      "runner/demo-scope/abc-1",
			},
		}, nil
	}

	o.handleStalledRunningRecord(context.Background(), issue.ID)

	current := o.continuationForIssueLocked(issue.ID)
	if current == nil {
		t.Fatal("continuation record missing after stalled run")
	}
	if current.Reason == nil || current.Reason.ReasonCode != contract.ReasonRunContinuationPending {
		t.Fatalf("runtime reason = %#v, want %q", current.Reason, contract.ReasonRunContinuationPending)
	}
	if current.Dispatch == nil || current.Dispatch.RecoveryCheckpoint == nil || current.Dispatch.RecoveryCheckpoint.ArtifactID != "art-stall-1" {
		t.Fatalf("recovery checkpoint = %#v, want art-stall-1", current.Dispatch)
	}
	if current.StallCount != 1 {
		t.Fatalf("stall count = %d, want 1", current.StallCount)
	}
}

func TestJobTypeDefinitionForDispatchUsesExplicitJobType(t *testing.T) {
	cases := []struct {
		name     string
		jobType  contract.JobType
		expected contract.CandidateDeliveryPointKind
	}{
		{name: "code_change", jobType: contract.JobTypeCodeChange, expected: contract.CandidateDeliveryPointReviewablePullRequest},
		{name: "land_change", jobType: contract.JobTypeLandChange, expected: contract.CandidateDeliveryPointTargetPRSnapshot},
		{name: "analysis", jobType: contract.JobTypeAnalysis, expected: contract.CandidateDeliveryPointAnalysisReportDraft},
		{name: "diagnostic", jobType: contract.JobTypeDiagnostic, expected: contract.CandidateDeliveryPointDiagnosticReportDraft},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			definition, ok := jobTypeDefinitionForDispatch(&model.DispatchContext{JobType: tc.jobType})
			if !ok {
				t.Fatalf("jobTypeDefinitionForDispatch(%q) = false, want true", tc.jobType)
			}
			if definition.CandidateDelivery.Kind != tc.expected {
				t.Fatalf("candidate delivery kind = %q, want %q", definition.CandidateDelivery.Kind, tc.expected)
			}
			if !definition.ReviewGate.Required || definition.ReviewGate.ReviewerMode != contract.ReviewerModeReadOnly || definition.ReviewGate.MaxFixRounds != 2 {
				t.Fatalf("review gate = %#v, want readonly required gate with max 2 fixes", definition.ReviewGate)
			}
		})
	}
}

func TestResumeRecoveredSuccessPathLaunchesReadOnlyReviewer(t *testing.T) {
	o, trackerClient, _ := newTestOrchestrator(t)
	issue := newIssue("1", "ABC-1", "In Progress")
	trackerClient.issuesByID[issue.ID] = issue
	o.gitBranchFn = func(context.Context, string) (string, error) {
		return "runner/demo-scope/abc-1", nil
	}
	o.prLookup = fakePRLookup{
		find: func(context.Context, string, string) (*PullRequestInfo, error) {
			return &PullRequestInfo{
				Number:     12,
				URL:        "https://example.test/pr/12",
				State:      PullRequestStateOpen,
				HeadBranch: "runner/demo-scope/abc-1",
			}, nil
		},
	}
	paramsCh := make(chan agent.RunParams, 1)
	o.runner = fakeRunner{runFn: func(_ context.Context, params agent.RunParams) error {
		paramsCh <- params
		if params.OnAssistantText != nil {
			params.OnAssistantText(`{"status":"passed","summary":"review passed","findings":[]}`)
		}
		return nil
	}}

	o.mu.Lock()
	record := o.ensureRecordLocked(issue)
	record.Lifecycle = model.JobLifecycleActive
	record.Observation = &contract.Observation{Running: false, Summary: "awaiting recovery"}
	setTestWorkspacePath(record, filepath.Join(o.currentConfig().WorkspaceRoot, issue.Identifier))
	setTestBranchReference(record, "runner/demo-scope/abc-1", o.now().UTC().Format(time.RFC3339Nano))
	record.Dispatch = &model.DispatchContext{
		JobType:         contract.JobTypeCodeChange,
		Kind:            model.DispatchKindFresh,
		ExpectedOutcome: model.CompletionModePullRequest,
		OnMissingPR:     model.CompletionActionIntervention,
		OnClosedPR:      model.CompletionActionIntervention,
	}
	record.Run = o.ensureRunState(record, o.currentConfig(), record.Dispatch, 1)
	record.SourceState = issue.State
	record.NeedsRecovery = true
	o.reindexRecordLocked(issue.ID, record)
	snapshot := cloneJobRuntime(record)
	o.mu.Unlock()

	if err := o.resumeRecoveredSuccessPath(context.Background(), issue.ID, snapshot, issue.State); err != nil {
		t.Fatalf("resumeRecoveredSuccessPath() error = %v", err)
	}

	var params agent.RunParams
	select {
	case params = <-paramsCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for reviewer launch")
	}
	if !params.ReadOnly {
		t.Fatal("reviewer run is not readonly")
	}
	if !strings.Contains(params.RawPrompt, "Read-only review only") {
		t.Fatalf("review prompt = %q, want readonly instruction", params.RawPrompt)
	}

	reviewResult := takeWorkerResult(t, o)
	if reviewResult.Kind != WorkerKindReview {
		t.Fatalf("worker result kind = %q, want review", reviewResult.Kind)
	}
	o.handleWorkerExit(reviewResult)

	if current := o.candidateDeliveryForIssueLocked(issue.ID); current != nil {
		t.Fatalf("candidateDeliveryForIssueLocked() = %#v, want nil after completed code_change review", current)
	}
	if len(o.state.ArchivedJobs) != 1 || o.state.ArchivedJobs[0].Run == nil {
		t.Fatalf("ArchivedJobs = %#v, want completed archived code_change", o.state.ArchivedJobs)
	}
	if o.state.ArchivedJobs[0].Run.ReviewGate == nil || o.state.ArchivedJobs[0].Run.ReviewGate.Status != contract.ReviewGateStatusPassed {
		t.Fatalf("review gate = %#v, want passed", o.state.ArchivedJobs[0].Run)
	}
	if o.state.ArchivedJobs[0].Outcome == nil || o.state.ArchivedJobs[0].Outcome.State != contract.OutcomeConclusionSucceeded {
		t.Fatalf("outcome = %#v, want succeeded", o.state.ArchivedJobs[0].Outcome)
	}
}

func TestParseReviewOutputExtractsValidJSONFromMixedText(t *testing.T) {
	raw := strings.Join([]string{
		"Return exactly one JSON object and nothing else.",
		`{"status":"passed|changes_requested","summary":"short summary","findings":[{"code":"short_code","summary":"what must be fixed"}]}`,
		`{"status":"changes_requested","summary":"needs one fix","findings":[{"code":"review.issue","summary":"fix the failing review item"}]}`,
	}, "\n")

	status, summary, findings, err := parseReviewOutput(raw)
	if err != nil {
		t.Fatalf("parseReviewOutput() error = %v", err)
	}
	if status != contract.ReviewGateStatusChangesRequested {
		t.Fatalf("status = %q, want %q", status, contract.ReviewGateStatusChangesRequested)
	}
	if summary != "needs one fix" {
		t.Fatalf("summary = %q, want %q", summary, "needs one fix")
	}
	if len(findings) != 1 || findings[0].Code != "review.issue" {
		t.Fatalf("findings = %#v, want review.issue", findings)
	}
}

func TestResumeRecoveredSuccessPathReviewerAcceptsFragmentedJSON(t *testing.T) {
	o, trackerClient, _ := newTestOrchestrator(t)
	issue := newIssue("1", "ABC-1", "In Progress")
	trackerClient.issuesByID[issue.ID] = issue
	o.gitBranchFn = func(context.Context, string) (string, error) {
		return "runner/demo-scope/abc-1", nil
	}
	o.prLookup = fakePRLookup{
		find: func(context.Context, string, string) (*PullRequestInfo, error) {
			return &PullRequestInfo{
				Number:     18,
				URL:        "https://example.test/pr/18",
				State:      PullRequestStateOpen,
				HeadBranch: "runner/demo-scope/abc-1",
			}, nil
		},
	}
	o.runner = fakeRunner{runFn: func(_ context.Context, params agent.RunParams) error {
		if params.OnAssistantText != nil {
			params.OnAssistantText(`{"status":"passed","sum`)
			params.OnAssistantText(`mary":"review passed","findings":[]}`)
		}
		return nil
	}}

	o.mu.Lock()
	record := o.ensureRecordLocked(issue)
	record.Lifecycle = model.JobLifecycleActive
	record.Observation = &contract.Observation{Running: false, Summary: "awaiting recovery"}
	setTestWorkspacePath(record, filepath.Join(o.currentConfig().WorkspaceRoot, issue.Identifier))
	setTestBranchReference(record, "runner/demo-scope/abc-1", o.now().UTC().Format(time.RFC3339Nano))
	record.Dispatch = &model.DispatchContext{
		JobType:         contract.JobTypeCodeChange,
		Kind:            model.DispatchKindFresh,
		ExpectedOutcome: model.CompletionModePullRequest,
		OnMissingPR:     model.CompletionActionIntervention,
		OnClosedPR:      model.CompletionActionIntervention,
	}
	record.Run = o.ensureRunState(record, o.currentConfig(), record.Dispatch, 1)
	record.SourceState = issue.State
	record.NeedsRecovery = true
	o.reindexRecordLocked(issue.ID, record)
	snapshot := cloneJobRuntime(record)
	o.mu.Unlock()

	if err := o.resumeRecoveredSuccessPath(context.Background(), issue.ID, snapshot, issue.State); err != nil {
		t.Fatalf("resumeRecoveredSuccessPath() error = %v", err)
	}

	reviewResult := takeWorkerResult(t, o)
	if reviewResult.Err != nil {
		t.Fatalf("reviewResult.Err = %v, want nil", reviewResult.Err)
	}
	if reviewResult.ReviewStatus != contract.ReviewGateStatusPassed {
		t.Fatalf("reviewResult.ReviewStatus = %q, want %q", reviewResult.ReviewStatus, contract.ReviewGateStatusPassed)
	}
}

func TestResumeRecoveredSuccessPathLaunchesReviewerForAnalysis(t *testing.T) {
	o, trackerClient, _ := newTestOrchestrator(t)
	issue := newIssue("1", "ABC-1", "In Progress")
	trackerClient.issuesByID[issue.ID] = issue
	paramsCh := make(chan agent.RunParams, 1)
	o.runner = fakeRunner{runFn: func(_ context.Context, params agent.RunParams) error {
		paramsCh <- params
		if params.OnAssistantText != nil {
			params.OnAssistantText(`{"status":"passed","summary":"analysis review passed","findings":[]}`)
		}
		return nil
	}}

	o.mu.Lock()
	record := o.ensureRecordLocked(issue)
	record.Lifecycle = model.JobLifecycleActive
	record.Observation = &contract.Observation{Running: false, Summary: "awaiting recovery"}
	setTestWorkspacePath(record, filepath.Join(o.currentConfig().WorkspaceRoot, issue.Identifier))
	record.Dispatch = &model.DispatchContext{
		JobType:         contract.JobTypeAnalysis,
		Kind:            model.DispatchKindFresh,
		ExpectedOutcome: model.CompletionModeNone,
	}
	record.Run = o.ensureRunState(record, o.currentConfig(), record.Dispatch, 1)
	record.SourceState = issue.State
	record.NeedsRecovery = true
	o.reindexRecordLocked(issue.ID, record)
	snapshot := cloneJobRuntime(record)
	o.mu.Unlock()

	if err := o.resumeRecoveredSuccessPath(context.Background(), issue.ID, snapshot, issue.State); err != nil {
		t.Fatalf("resumeRecoveredSuccessPath() error = %v", err)
	}

	select {
	case params := <-paramsCh:
		if !params.ReadOnly {
			t.Fatal("analysis reviewer is not readonly")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for analysis reviewer launch")
	}

	reviewResult := takeWorkerResult(t, o)
	o.handleWorkerExit(reviewResult)

	if current := o.state.Jobs[issue.ID]; current != nil {
		t.Fatalf("active record = %#v, want nil after completed analysis run", current)
	}
	if len(o.state.ArchivedJobs) != 1 || o.state.ArchivedJobs[0].Outcome == nil || o.state.ArchivedJobs[0].Outcome.State != contract.OutcomeConclusionSucceeded {
		t.Fatalf("archived jobs = %#v, want one completed succeeded record", o.state.ArchivedJobs)
	}
	archived := o.state.ArchivedJobs[0]
	if len(archived.Artifacts) != 1 || archived.Artifacts[0].Kind != contract.ArtifactKindAnalysisReport {
		t.Fatalf("archived artifacts = %#v, want one analysis report", archived.Artifacts)
	}
	outcomeEnvelope, ok := o.GetObject(contract.ObjectTypeOutcome, archived.Outcome.ID)
	if !ok {
		t.Fatal("GetObject(outcome) = false, want true")
	}
	var outcome contract.Outcome
	if err := json.Unmarshal(outcomeEnvelope.Payload, &outcome); err != nil {
		t.Fatalf("Unmarshal(outcome) error = %v", err)
	}
	if len(outcome.Relations) != 1 || outcome.Relations[0].Type != contract.RelationTypeOutcomeArtifact || outcome.Relations[0].TargetID != archived.Artifacts[0].ID {
		t.Fatalf("outcome.Relations = %#v, want outcome.artifact to %q", outcome.Relations, archived.Artifacts[0].ID)
	}
	if _, ok := o.GetObject(contract.ObjectTypeArtifact, archived.Artifacts[0].ID); !ok {
		t.Fatalf("GetObject(artifact, %q) = false, want true", archived.Artifacts[0].ID)
	}
	if reviewResult.ReviewStatus != contract.ReviewGateStatusPassed {
		t.Fatalf("review result = %#v, want passed", reviewResult)
	}
}

func TestMoveToAwaitingMergePublishesFormalCandidateArtifactsAndReferences(t *testing.T) {
	o, trackerClient, _ := newTestOrchestrator(t)
	issue := newIssue("1", "ABC-1", "In Progress")
	trackerClient.issuesByID[issue.ID] = issue
	workspacePath := filepath.Join(o.currentConfig().WorkspaceRoot, issue.Identifier)
	pr := &PullRequestInfo{
		Number:     12,
		URL:        "https://example.test/pr/12",
		State:      PullRequestStateOpen,
		HeadBranch: "runner/demo-scope/abc-1",
	}

	o.mu.Lock()
	record := o.ensureRecordLocked(issue)
	record.Lifecycle = model.JobLifecycleActive
	record.Observation = &contract.Observation{Running: true, Summary: "publishing pr"}
	setTestWorkspacePath(record, workspacePath)
	setTestBranchReference(record, pr.HeadBranch, o.now().UTC().Format(time.RFC3339Nano))
	record.Dispatch = &model.DispatchContext{
		JobType:         contract.JobTypeCodeChange,
		Kind:            model.DispatchKindFresh,
		ExpectedOutcome: model.CompletionModePullRequest,
		OnMissingPR:     model.CompletionActionIntervention,
		OnClosedPR:      model.CompletionActionIntervention,
	}
	record.Run = o.ensureRunState(record, o.currentConfig(), record.Dispatch, 1)
	record.SourceState = issue.State
	o.reindexRecordLocked(issue.ID, record)
	o.mu.Unlock()

	o.moveToAwaitingMerge(issue.ID, issue.Identifier, issue.State, workspacePath, pr.HeadBranch, 0, 0, pr, nil)

	current := o.candidateDeliveryForIssueLocked(issue.ID)
	if current == nil || current.Run == nil {
		t.Fatal("candidate delivery record missing")
	}
	runEnvelope, ok := o.GetObject(contract.ObjectTypeRun, runIDForRecord(current, current.Run.Attempt))
	if !ok {
		t.Fatal("GetObject(run) = false, want true")
	}
	var run contract.Run
	if err := json.Unmarshal(runEnvelope.Payload, &run); err != nil {
		t.Fatalf("Unmarshal(run) error = %v", err)
	}
	wantArtifactID := artifactIDForRecord(current, "pull-request")
	if run.CandidateDelivery == nil || len(run.CandidateDelivery.ArtifactIDs) != 1 || run.CandidateDelivery.ArtifactIDs[0] != wantArtifactID {
		t.Fatalf("run.CandidateDelivery = %#v, want artifact %q", run.CandidateDelivery, wantArtifactID)
	}
	if len(run.Checkpoints) == 0 || len(run.Checkpoints[0].ReferenceIDs) != 1 || run.Checkpoints[0].ReferenceIDs[0] != pullRequestReferenceIDForRecord(current) {
		t.Fatalf("run.Checkpoints = %#v, want PR reference id", run.Checkpoints)
	}
	artifactEnvelope, ok := o.GetObject(contract.ObjectTypeArtifact, wantArtifactID)
	if !ok {
		t.Fatalf("GetObject(artifact, %q) = false, want true", wantArtifactID)
	}
	var artifact contract.Artifact
	if err := json.Unmarshal(artifactEnvelope.Payload, &artifact); err != nil {
		t.Fatalf("Unmarshal(artifact) error = %v", err)
	}
	if artifact.Kind != contract.ArtifactKindPullRequest {
		t.Fatalf("artifact.Kind = %q, want %q", artifact.Kind, contract.ArtifactKindPullRequest)
	}
	if _, ok := o.GetObject(contract.ObjectTypeReference, pullRequestReferenceIDForRecord(current)); ok {
		t.Fatalf("GetObject(reference, %q) = true, want false for public query surface", pullRequestReferenceIDForRecord(current))
	}
	if _, ok := mustGetLedgerObject(t, o, contract.ObjectTypeReference, pullRequestReferenceIDForRecord(current)); !ok {
		t.Fatalf("objectLedger.Get(reference, %q) = false, want true", pullRequestReferenceIDForRecord(current))
	}
}

func TestFormalObjectsPublishRecoveryCheckpointArtifactsAndReferences(t *testing.T) {
	o, trackerClient, _ := newTestOrchestrator(t)
	issue := newIssue("1", "ABC-1", "In Progress")
	trackerClient.issuesByID[issue.ID] = issue
	workspacePath := filepath.Join(o.currentConfig().WorkspaceRoot, issue.Identifier)

	o.mu.Lock()
	record := o.ensureRecordLocked(issue)
	record.Lifecycle = model.JobLifecycleActive
	record.Observation = &contract.Observation{Running: false, Summary: "checkpoint captured"}
	setTestWorkspacePath(record, workspacePath)
	setTestBranchReference(record, "runner/demo-scope/abc-1", o.now().UTC().Format(time.RFC3339Nano))
	setTestPullRequestReference(record, 12, "https://example.test/pr/12", "open", o.now().UTC().Format(time.RFC3339Nano))
	record.Dispatch = &model.DispatchContext{
		JobType:         contract.JobTypeCodeChange,
		Kind:            model.DispatchKindContinuation,
		ExpectedOutcome: model.CompletionModePullRequest,
	}
	record.Run = o.ensureRunState(record, o.currentConfig(), record.Dispatch, 1)
	record.Run.State = contract.RunStatusContinuationPending
	record.Run.Recovery = &model.RecoveryCheckpoint{
		ArtifactID:    "artifact-rec-1-recovery",
		PatchPath:     filepath.Join(t.TempDir(), "recovery.patch"),
		WorkspacePath: workspacePath,
		Checkpoint: contract.Checkpoint{
			Type:         contract.CheckpointTypeRecovery,
			Summary:      "saved recovery",
			CapturedAt:   o.now().UTC().Format(time.RFC3339Nano),
			Stage:        contract.RunPhaseSummaryBlocked,
			ArtifactIDs:  []string{"artifact-rec-1-recovery"},
			ReferenceIDs: []string{sourceReferenceIDForRecord(record), branchReferenceIDForRecord(record), pullRequestReferenceIDForRecord(record)},
			BaseSHA:      "abc123",
			Branch:       "runner/demo-scope/abc-1",
		},
	}
	o.setRunContinuationPending(record, record.Run.Recovery)
	record.SourceState = issue.State
	o.reindexRecordLocked(issue.ID, record)
	o.commitStateLocked(true)
	o.mu.Unlock()

	runEnvelope, ok := o.GetObject(contract.ObjectTypeRun, runIDForRecord(record, record.Run.Attempt))
	if !ok {
		t.Fatal("GetObject(run) = false, want true")
	}
	var run contract.Run
	if err := json.Unmarshal(runEnvelope.Payload, &run); err != nil {
		t.Fatalf("Unmarshal(run) error = %v", err)
	}
	if len(run.Checkpoints) == 0 || len(run.Checkpoints[0].ReferenceIDs) != 3 {
		t.Fatalf("run.Checkpoints = %#v, want recovery checkpoint with reference ids", run.Checkpoints)
	}
	artifactEnvelope, ok := o.GetObject(contract.ObjectTypeArtifact, "artifact-rec-1-recovery")
	if !ok {
		t.Fatal("GetObject(recovery artifact) = false, want true")
	}
	var artifact contract.Artifact
	if err := json.Unmarshal(artifactEnvelope.Payload, &artifact); err != nil {
		t.Fatalf("Unmarshal(recovery artifact) error = %v", err)
	}
	if artifact.Kind != contract.ArtifactKindPatchBundle {
		t.Fatalf("artifact.Kind = %q, want %q", artifact.Kind, contract.ArtifactKindPatchBundle)
	}
	if _, ok := o.GetObject(contract.ObjectTypeReference, sourceReferenceIDForRecord(record)); ok {
		t.Fatalf("GetObject(reference, %q) = true, want false for public query surface", sourceReferenceIDForRecord(record))
	}
	if _, ok := mustGetLedgerObject(t, o, contract.ObjectTypeReference, sourceReferenceIDForRecord(record)); !ok {
		t.Fatalf("objectLedger.Get(reference, %q) = false, want true", sourceReferenceIDForRecord(record))
	}
}

func TestReviewFixRoundsLimitMovesToIntervention(t *testing.T) {
	o, trackerClient, _ := newTestOrchestrator(t)
	issue := newIssue("1", "ABC-1", "In Progress")
	trackerClient.issuesByID[issue.ID] = issue
	o.gitBranchFn = func(context.Context, string) (string, error) {
		return "runner/demo-scope/abc-1", nil
	}
	o.runner = fakeRunner{}

	o.mu.Lock()
	record := o.ensureRecordLocked(issue)
	record.Lifecycle = model.JobLifecycleActive
	record.Observation = &contract.Observation{Running: true, Summary: "review in progress"}
	setTestWorkspacePath(record, filepath.Join(o.currentConfig().WorkspaceRoot, issue.Identifier))
	setTestBranchReference(record, "runner/demo-scope/abc-1", o.now().UTC().Format(time.RFC3339Nano))
	record.Dispatch = &model.DispatchContext{
		JobType:         contract.JobTypeCodeChange,
		Kind:            model.DispatchKindFresh,
		ExpectedOutcome: model.CompletionModePullRequest,
		OnMissingPR:     model.CompletionActionIntervention,
		OnClosedPR:      model.CompletionActionIntervention,
	}
	record.Run = o.ensureRunState(record, o.currentConfig(), record.Dispatch, 1)
	record.Run.Object.ReviewGate.Status = contract.ReviewGateStatusReviewing
	syncRunStateMirrorsFromObject(record.Run)
	record.SourceState = issue.State
	startedAt := o.now().Add(-time.Second)
	record.StartedAt = &startedAt
	o.reindexRecordLocked(issue.ID, record)
	o.mu.Unlock()

	for round := 1; round <= 2; round++ {
		o.handleWorkerExit(WorkerResult{
			Kind:          WorkerKindReview,
			IssueID:       issue.ID,
			Identifier:    issue.Identifier,
			StartedAt:     startedAt,
			ReviewStatus:  contract.ReviewGateStatusChangesRequested,
			ReviewSummary: "need fixes",
			ReviewFindings: []model.ReviewFinding{
				{Code: "review.issue", Summary: "fix review feedback"},
			},
		})
		current := o.state.Jobs[issue.ID]
		if current == nil || current.Run == nil || current.Run.ReviewGate == nil {
			t.Fatalf("round %d current run missing", round)
		}
		if current.Run.ReviewGate.FixRoundsUsed != round {
			t.Fatalf("round %d fix rounds used = %d, want %d", round, current.Run.ReviewGate.FixRoundsUsed, round)
		}
		current.Lifecycle = model.JobLifecycleActive
		current.Observation = &contract.Observation{Running: true, Summary: "review in progress"}
		current.StartedAt = &startedAt
		o.reindexRecordLocked(issue.ID, current)
	}

	o.handleWorkerExit(WorkerResult{
		Kind:          WorkerKindReview,
		IssueID:       issue.ID,
		Identifier:    issue.Identifier,
		StartedAt:     startedAt,
		ReviewStatus:  contract.ReviewGateStatusChangesRequested,
		ReviewSummary: "still blocked",
		ReviewFindings: []model.ReviewFinding{
			{Code: "review.issue", Summary: "needs a third fix round"},
		},
	})

	current := o.interventionForIssueLocked(issue.ID)
	if current == nil {
		t.Fatal("awaitingIntervention record missing after fix limit")
	}
	if current.Run == nil || current.Run.ReviewGate == nil || current.Run.ReviewGate.Status != contract.ReviewGateStatusInterventionRequired {
		t.Fatalf("review gate = %#v, want intervention_required", current.Run)
	}
	if current.Run.Reason == nil || current.Run.Reason.ReasonCode != contract.ReasonRunReviewGateFixLimitReached {
		t.Fatalf("run reason = %#v, want %q", current.Run.Reason, contract.ReasonRunReviewGateFixLimitReached)
	}
}

func TestMoveToAwaitingInterventionBuildsFormalHandoff(t *testing.T) {
	o, trackerClient, _ := newTestOrchestrator(t)
	issue := newIssue("1", "ABC-1", "In Progress")
	trackerClient.issuesByID[issue.ID] = issue

	o.mu.Lock()
	record := o.ensureRecordLocked(issue)
	record.Lifecycle = model.JobLifecycleActive
	record.Observation = &contract.Observation{Running: true, Summary: "review in progress"}
	setTestWorkspacePath(record, filepath.Join(o.currentConfig().WorkspaceRoot, issue.Identifier))
	setTestBranchReference(record, "runner/demo-scope/abc-1", o.now().UTC().Format(time.RFC3339Nano))
	record.Dispatch = &model.DispatchContext{
		JobType:         contract.JobTypeCodeChange,
		Kind:            model.DispatchKindFresh,
		ExpectedOutcome: model.CompletionModePullRequest,
		OnMissingPR:     model.CompletionActionIntervention,
		OnClosedPR:      model.CompletionActionIntervention,
	}
	record.Run = o.ensureRunState(record, o.currentConfig(), record.Dispatch, 1)
	record.Run.ReviewSummary = "review blocked"
	record.Run.ReviewFindings = []model.ReviewFinding{{Code: "review.issue", Summary: "needs explicit human direction"}}
	record.Run.Object.Checkpoints = append(record.Run.Object.Checkpoints, contract.Checkpoint{
		Type:        contract.CheckpointTypeBusiness,
		Summary:     "draft PR ready",
		CapturedAt:  o.now().UTC().Format(time.RFC3339Nano),
		Stage:       contract.RunPhaseSummaryPublishing,
		ArtifactIDs: []string{"pr-12"},
		Branch:      "runner/demo-scope/abc-1",
	})
	syncRunStateMirrorsFromObject(record.Run)
	o.reindexRecordLocked(issue.ID, record)
	o.mu.Unlock()

	o.moveToAwaitingIntervention(issue.ID, issue.Identifier, filepath.Join(o.currentConfig().WorkspaceRoot, issue.Identifier), "runner/demo-scope/abc-1", 0, 0, model.CompletionModePullRequest, string(contract.ReasonRunReviewGateFixLimitReached), issue.State, &PullRequestInfo{
		Number:     12,
		URL:        "https://example.test/pr/12",
		State:      PullRequestStateOpen,
		HeadBranch: "runner/demo-scope/abc-1",
	})

	current := o.interventionForIssueLocked(issue.ID)
	if current == nil || current.Intervention == nil {
		t.Fatal("formal intervention handoff missing")
	}
	if current.Intervention.Handoff.Reason == nil || current.Intervention.Handoff.Decision == nil {
		t.Fatalf("handoff = %#v, want reason and decision", current.Intervention.Handoff)
	}
	if current.Intervention.Handoff.Phase != contract.RunPhaseSummaryBlocked {
		t.Fatalf("handoff phase = %q, want blocked", current.Intervention.Handoff.Phase)
	}
	if current.Intervention.Handoff.ReviewSummary != "review blocked" {
		t.Fatalf("handoff review summary = %q, want review blocked", current.Intervention.Handoff.ReviewSummary)
	}
	if len(current.Intervention.Handoff.ReviewFindings) != 1 {
		t.Fatalf("handoff findings = %#v, want 1 finding", current.Intervention.Handoff.ReviewFindings)
	}
	if current.Intervention.Handoff.Checkpoint == nil || current.Intervention.Handoff.Checkpoint.Type != contract.CheckpointTypeBusiness {
		t.Fatalf("handoff checkpoint = %#v, want business checkpoint", current.Intervention.Handoff.Checkpoint)
	}
	if len(current.Intervention.Handoff.RecommendedActions) == 0 || len(current.Intervention.Handoff.RequiredInputs) == 0 {
		t.Fatalf("handoff = %#v, want recommended actions and required inputs", current.Intervention.Handoff)
	}
}

func TestResumeInterventionStartsNewRunWithRecoveryContextOnly(t *testing.T) {
	o, trackerClient, _ := newTestOrchestrator(t)
	issue := newIssue("1", "ABC-1", "In Progress")
	trackerClient.issuesByID[issue.ID] = issue
	o.gitBranchFn = func(context.Context, string) (string, error) {
		return "runner/demo-scope/abc-1", nil
	}
	o.runner = fakeRunner{}

	o.mu.Lock()
	record := o.ensureRecordLocked(issue)
	record.Lifecycle = model.JobLifecycleAwaitingIntervention
	record.Observation = &contract.Observation{Running: false, Summary: "awaiting intervention"}
	setTestWorkspacePath(record, filepath.Join(o.currentConfig().WorkspaceRoot, issue.Identifier))
	setTestBranchReference(record, "runner/demo-scope/abc-1", o.now().UTC().Format(time.RFC3339Nano))
	record.Dispatch = &model.DispatchContext{
		JobType:         contract.JobTypeCodeChange,
		Kind:            model.DispatchKindFresh,
		ExpectedOutcome: model.CompletionModePullRequest,
		OnMissingPR:     model.CompletionActionIntervention,
		OnClosedPR:      model.CompletionActionIntervention,
		ReviewFeedback: &model.ReviewFeedbackContext{
			Summary:  "stale review feedback",
			Findings: []model.ReviewFinding{{Code: "stale", Summary: "should not carry over"}},
		},
	}
	record.Run = o.ensureRunState(record, o.currentConfig(), record.Dispatch, 1)
	record.Run.ReviewGate.FixRoundsUsed = 2
	record.Run.Recovery = &model.RecoveryCheckpoint{
		ArtifactID:    "art-1",
		WorkspacePath: filepath.Join(o.currentConfig().WorkspaceRoot, issue.Identifier),
		Checkpoint: contract.Checkpoint{
			Type:        contract.CheckpointTypeRecovery,
			Summary:     "saved recovery",
			CapturedAt:  o.now().UTC().Format(time.RFC3339Nano),
			Stage:       contract.RunPhaseSummaryBlocked,
			ArtifactIDs: []string{"art-1"},
			BaseSHA:     "abc123",
			Branch:      "runner/demo-scope/abc-1",
		},
	}
	record.Session = model.LiveSession{TurnCount: 7, LastCodexMessage: "stale"}
	record.Intervention = &model.InterventionState{
		Object: contract.Intervention{
			BaseObject: contract.BaseObject{
				ID: interventionIDForRecord(record),
			},
			State: contract.InterventionStatusOpen,
		},
		Handoff: model.InterventionHandoff{
			Checkpoint: &record.Run.Recovery.Checkpoint,
		},
	}
	record.SourceState = issue.State
	o.reindexRecordLocked(issue.ID, record)
	o.mu.Unlock()

	o.resumeIntervention(context.Background(), record.Intervention.Object.ID, issue, contract.InterventionResolution{
		Action:         contract.ControlActionResolveIntervention,
		ProvidedInputs: map[string]any{"resolution": "revise_scope"},
		ResolvedAt:     o.now().UTC().Format(time.RFC3339Nano),
	})

	current := o.state.Jobs[issue.ID]
	if current == nil || current.Run == nil {
		t.Fatal("run missing after intervention resume")
	}
	if current == record {
		t.Fatal("resume reused prior runtime container, want fresh runtime skeleton")
	}
	if current.Run.Attempt != 2 {
		t.Fatalf("run attempt = %d, want 2", current.Run.Attempt)
	}
	if current.Dispatch == nil || current.Dispatch.Kind != model.DispatchKindInterventionRetry {
		t.Fatalf("dispatch = %#v, want intervention_retry", current.Dispatch)
	}
	if current.Dispatch.RecoveryCheckpoint == nil || current.Dispatch.RecoveryCheckpoint.ArtifactID != "art-1" {
		t.Fatalf("recovery checkpoint = %#v, want art-1", current.Dispatch)
	}
	if current.Dispatch.ReviewFeedback != nil {
		t.Fatalf("review feedback carried over = %#v, want nil", current.Dispatch.ReviewFeedback)
	}
	if current.Run.ReviewGate == nil || current.Run.ReviewGate.FixRoundsUsed != 0 {
		t.Fatalf("review gate = %#v, want fresh run review gate", current.Run.ReviewGate)
	}
	if current.Session.TurnCount != 0 || current.Session.LastCodexMessage != "" {
		t.Fatalf("session = %#v, want cleared transient session", current.Session)
	}
	if current.Intervention == nil || current.Intervention.Object.State != contract.InterventionStatusResolved || current.Intervention.Object.Resolution == nil {
		t.Fatalf("intervention = %#v, want persisted resolved intervention object", current.Intervention)
	}
}

func TestFileLedgerStoreRoundTrip(t *testing.T) {
	cfg := model.SessionPersistenceConfig{
		Enabled: true,
		Kind:    model.SessionPersistenceKindFile,
		File: model.SessionPersistenceFileConfig{
			Path:            filepath.Join(t.TempDir(), "runtime-state.json"),
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
		Jobs: []durableJobState{
			{
				JobID:     "job-1",
				UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
			},
		},
		FormalObjects: mustFormalObjectSnapshot(t,
			contract.Job{
				BaseObject:    contract.BaseObject{ID: "job-1", ObjectType: contract.ObjectTypeJob, UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano)},
				ObjectContext: contract.ObjectContext{References: []contract.Reference{testLinearIssueReference("ref-job-1", "1", "ABC-1", time.Now().UTC().Format(time.RFC3339Nano))}},
				State:         contract.JobStatusRunning,
				JobType:       contract.JobTypeCodeChange,
			},
		),
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
	if loaded == nil || len(loaded.Jobs) != 1 {
		t.Fatalf("loaded jobs = %#v, want 1 entry", loaded)
	}
}

func TestWriteDurableRuntimeStateRoundTripJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime-state.json")
	state := durableRuntimeState{
		Version: durableStateVersion,
		SavedAt: time.Now().UTC(),
		Jobs: []durableJobState{
			{
				JobID:     "job-1",
				OutcomeID: "outcome-1",
				UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
			},
		},
		FormalObjects: mustFormalObjectSnapshot(t,
			contract.Job{
				BaseObject:    contract.BaseObject{ID: "job-1", ObjectType: contract.ObjectTypeJob, UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano)},
				ObjectContext: contract.ObjectContext{References: []contract.Reference{testLinearIssueReference("ref-job-1", "1", "ABC-1", time.Now().UTC().Format(time.RFC3339Nano))}},
				State:         contract.JobStatusCompleted,
				JobType:       contract.JobTypeCodeChange,
			},
			contract.Outcome{BaseObject: contract.BaseObject{ID: "outcome-1", ObjectType: contract.ObjectTypeOutcome, UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano)}, State: contract.OutcomeConclusionSucceeded, Summary: "done", CompletedAt: time.Now().UTC().Format(time.RFC3339Nano)},
		),
	}
	if err := writeDurableRuntimeState(path, state, true); err != nil {
		t.Fatalf("writeDurableRuntimeState() error = %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if strings.Contains(string(raw), "\"recovering\"") || strings.Contains(string(raw), "\"retrying\"") {
		t.Fatalf("runtime-state.json still contains legacy bucket keys: %s", string(raw))
	}
	for _, forbidden := range []string{"\"record_id\"", "\"source_ref\"", "\"durable_refs\"", "\"result\"", "\"observation\"", "\"last_known_issue\"", "\"last_known_issue_state\""} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("runtime-state.json still contains legacy durable field %s: %s", forbidden, string(raw))
		}
	}
}

func TestWriteDurableRuntimeStateOverwritesExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime-state.json")
	initial := durableRuntimeState{
		Version: durableStateVersion,
		SavedAt: time.Now().UTC(),
		Jobs: []durableJobState{
			{
				JobID:     "job-1",
				UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
			},
		},
		FormalObjects: mustFormalObjectSnapshot(t,
			contract.Job{
				BaseObject:    contract.BaseObject{ID: "job-1", ObjectType: contract.ObjectTypeJob, UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano)},
				ObjectContext: contract.ObjectContext{References: []contract.Reference{testLinearIssueReference("ref-job-1", "1", "ABC-1", time.Now().UTC().Format(time.RFC3339Nano))}},
				State:         contract.JobStatusInterventionRequired,
				JobType:       contract.JobTypeCodeChange,
			},
		),
	}
	updated := durableRuntimeState{
		Version: durableStateVersion,
		SavedAt: time.Now().UTC(),
		Jobs: []durableJobState{
			{
				JobID:     "job-2",
				UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
			},
		},
		FormalObjects: mustFormalObjectSnapshot(t,
			contract.Job{
				BaseObject:    contract.BaseObject{ID: "job-2", ObjectType: contract.ObjectTypeJob, UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano)},
				ObjectContext: contract.ObjectContext{References: []contract.Reference{testLinearIssueReference("ref-job-2", "2", "ABC-2", time.Now().UTC().Format(time.RFC3339Nano))}},
				State:         contract.JobStatusRunning,
				JobType:       contract.JobTypeCodeChange,
			},
		),
	}

	if err := writeDurableRuntimeState(path, initial, true); err != nil {
		t.Fatalf("writeDurableRuntimeState(initial) error = %v", err)
	}
	if err := writeDurableRuntimeState(path, updated, true); err != nil {
		t.Fatalf("writeDurableRuntimeState(updated) error = %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var loaded durableRuntimeState
	if err := json.Unmarshal(raw, &loaded); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if len(loaded.Jobs) != 1 || loaded.Jobs[0].JobID != "job-2" {
		t.Fatalf("loaded jobs = %#v, want overwritten job-2", loaded.Jobs)
	}
}

func TestBuildPersistedStateIncludesNotificationFingerprints(t *testing.T) {
	o, _, _ := newTestOrchestrator(t)

	o.mu.Lock()
	o.emittedNotificationKeys["issue_completed|issue-1"] = struct{}{}
	o.emittedNotificationKeys["issue_intervention_required|issue-1|1|missing_pr"] = struct{}{}
	state := o.buildPersistedStateLocked()
	o.mu.Unlock()

	if len(state.Service.NotificationFingerprints) != 2 {
		t.Fatalf("notification fingerprints = %#v, want 2", state.Service.NotificationFingerprints)
	}
	got := map[string]struct{}{}
	for _, item := range state.Service.NotificationFingerprints {
		got[item] = struct{}{}
	}
	for _, want := range []string{"issue_completed|issue-1", "issue_intervention_required|issue-1|1|missing_pr"} {
		if _, ok := got[want]; !ok {
			t.Fatalf("notification fingerprints = %#v, want %q", state.Service.NotificationFingerprints, want)
		}
	}
}

func TestRestorePersistedStateSkipsDuplicateNotifications(t *testing.T) {
	o, _, _ := newTestOrchestrator(t)
	recorded := &recordingNotifier{}
	event := o.newIssueInterventionRequiredEvent("issue-1", "ABC-1", "branch/demo", "missing_pr", model.CompletionModePullRequest, 0, nil)
	fingerprint := notificationFingerprint(event)
	state := durableRuntimeState{
		Version:       durableStateVersion,
		Identity:      o.currentRuntimeIdentity(),
		SavedAt:       o.now().UTC(),
		Service:       durableServiceMetadata{NotificationFingerprints: []string{fingerprint}},
		FormalObjects: mustFormalObjectSnapshot(t),
	}

	o.mu.Lock()
	o.restorePersistedStateLocked(&state)
	o.notifier = recorded
	o.mu.Unlock()

	o.emitNotification(event)
	if len(recorded.events) != 0 {
		t.Fatalf("recorded events = %#v, want no replay for persisted fingerprint", recorded.events)
	}

	freshEvent := o.newIssueInterventionRequiredEvent("issue-1", "ABC-1", "branch/demo", "missing_pr", model.CompletionModePullRequest, 1, nil)
	o.emitNotification(freshEvent)
	if len(recorded.events) != 1 {
		t.Fatalf("recorded events = %#v, want one fresh intervention notification", recorded.events)
	}
}

func takeWorkerResult(t *testing.T, o *Orchestrator) WorkerResult {
	t.Helper()
	select {
	case result := <-o.workerResultCh:
		return result
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for worker result")
		return WorkerResult{}
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
