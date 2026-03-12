package orchestrator

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"symphony-go/internal/agent"
	"symphony-go/internal/model"
	"symphony-go/internal/tracker"
)

func TestDispatchEligibleIssuesSortsAndBlocksTodo(t *testing.T) {
	runCh := make(chan string, 2)
	runner := &fakeRunner{runCh: runCh, block: make(chan struct{})}
	now := time.Date(2026, 3, 7, 10, 0, 0, 0, time.UTC)
	cfg := &model.ServiceConfig{
		ActiveStates:        []string{"Todo", "In Progress"},
		TerminalStates:      []string{"Done"},
		MaxConcurrentAgents: 1,
		MaxTurns:            2,
	}
	o := newTestOrchestrator(cfg, &fakeTracker{}, &fakeWorkspaceManager{}, runner, now)
	older := now.Add(-2 * time.Hour)
	newer := now.Add(-1 * time.Hour)
	priority1 := 1
	priority2 := 2
	blockerState := "In Progress"

	candidates := []model.Issue{
		{ID: "3", Identifier: "ABC-3", Title: "Blocked", State: "Todo", Priority: &priority1, CreatedAt: &older, BlockedBy: []model.BlockerRef{{State: &blockerState}}},
		{ID: "2", Identifier: "ABC-2", Title: "Second", State: "Todo", Priority: &priority2, CreatedAt: &newer},
		{ID: "1", Identifier: "ABC-1", Title: "First", State: "Todo", Priority: &priority1, CreatedAt: &older},
	}

	o.dispatchEligibleIssues(context.Background(), candidates)

	select {
	case identifier := <-runCh:
		if identifier != "ABC-1" {
			t.Fatalf("dispatched identifier = %q, want ABC-1", identifier)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not receive dispatch")
	}
	close(runner.block)
}

func TestHandleWorkerExitSchedulesContinuationAndBackoffRetry(t *testing.T) {
	now := time.Date(2026, 3, 7, 10, 0, 0, 0, time.UTC)
	cfg := &model.ServiceConfig{
		ActiveStates:        []string{"Todo", "In Progress"},
		TerminalStates:      []string{"Done"},
		MaxConcurrentAgents: 2,
		MaxRetryBackoffMS:   300000,
	}
	o := newTestOrchestrator(cfg, &fakeTracker{}, &fakeWorkspaceManager{}, &fakeRunner{}, now)
	o.randFloat = func() float64 { return 0 }

	o.state.Running["1"] = &model.RunningEntry{
		Issue:        &model.Issue{ID: "1", Identifier: "ABC-1", State: "Todo"},
		Identifier:   "ABC-1",
		RetryAttempt: 0,
		StartedAt:    now.Add(-5 * time.Second),
		WorkerCancel: func() {},
	}
	o.handleWorkerExit(WorkerResult{IssueID: "1", Identifier: "ABC-1", StartedAt: now, Phase: model.PhaseSucceeded})
	retry := o.state.RetryAttempts["1"]
	if retry == nil || retry.Attempt != 1 {
		t.Fatalf("continuation retry = %+v, want attempt 1", retry)
	}
	if retry.DueAt.Sub(now) != time.Second {
		t.Fatalf("continuation retry delay = %v, want 1s", retry.DueAt.Sub(now))
	}

	o.state.Running["2"] = &model.RunningEntry{
		Issue:        &model.Issue{ID: "2", Identifier: "ABC-2", State: "Todo"},
		Identifier:   "ABC-2",
		RetryAttempt: 2,
		StartedAt:    now.Add(-3 * time.Second),
		WorkerCancel: func() {},
	}
	o.handleWorkerExit(WorkerResult{IssueID: "2", Identifier: "ABC-2", StartedAt: now, Phase: model.PhaseFailed, Err: model.NewAgentError(model.ErrTurnFailed, "failed", nil)})
	retry = o.state.RetryAttempts["2"]
	if retry == nil || retry.Attempt != 3 {
		t.Fatalf("failure retry = %+v, want attempt 3", retry)
	}
	if retry.DueAt.Sub(now) != 20*time.Second {
		t.Fatalf("failure retry delay = %v, want 20s", retry.DueAt.Sub(now))
	}
}

func TestScheduleRetryLockedCapsBackoffAtConfiguredMaximum(t *testing.T) {
	now := time.Date(2026, 3, 7, 10, 0, 0, 0, time.UTC)
	cfg := &model.ServiceConfig{
		ActiveStates:        []string{"Todo", "In Progress"},
		TerminalStates:      []string{"Done"},
		MaxConcurrentAgents: 1,
		MaxRetryBackoffMS:   25000,
		WorkspaceRoot:       "/tmp/workspaces",
	}
	o := newTestOrchestrator(cfg, &fakeTracker{}, &fakeWorkspaceManager{}, &fakeRunner{}, now)
	o.randFloat = func() float64 { return 1 }

	o.mu.Lock()
	o.scheduleRetryLocked("1", "ABC-1", 10, stringPtr("boom"), false, 0, nil)
	retry := o.state.RetryAttempts["1"]
	o.mu.Unlock()

	if retry == nil {
		t.Fatal("retry entry missing")
	}
	if retry.DueAt.Sub(now) != 25*time.Second {
		t.Fatalf("retry delay = %v, want 25s", retry.DueAt.Sub(now))
	}
}

func TestReconcileRunningStopsTerminalAndInactiveIssues(t *testing.T) {
	now := time.Date(2026, 3, 7, 10, 0, 0, 0, time.UTC)
	cfg := &model.ServiceConfig{
		ActiveStates:        []string{"Todo", "In Progress"},
		TerminalStates:      []string{"Done"},
		MaxConcurrentAgents: 2,
		CodexStallTimeoutMS: 300000,
	}
	workspace := &fakeWorkspaceManager{}
	tracker := &fakeTracker{
		stateByID: map[string]model.Issue{
			"1": {ID: "1", Identifier: "ABC-1", State: "Done"},
			"2": {ID: "2", Identifier: "ABC-2", State: "Paused"},
		},
	}
	o := newTestOrchestrator(cfg, tracker, workspace, &fakeRunner{}, now)

	cancelled := make(map[string]int)
	o.state.Running["1"] = &model.RunningEntry{
		Issue:      &model.Issue{ID: "1", Identifier: "ABC-1", State: "Todo"},
		Identifier: "ABC-1",
		StartedAt:  now.Add(-10 * time.Second),
		WorkerCancel: func() {
			cancelled["1"]++
		},
	}
	o.state.Running["2"] = &model.RunningEntry{
		Issue:      &model.Issue{ID: "2", Identifier: "ABC-2", State: "In Progress"},
		Identifier: "ABC-2",
		StartedAt:  now.Add(-10 * time.Second),
		WorkerCancel: func() {
			cancelled["2"]++
		},
	}
	o.state.Claimed["1"] = struct{}{}
	o.state.Claimed["2"] = struct{}{}

	o.reconcileRunning(context.Background())

	if len(o.state.Running) != 0 {
		t.Fatalf("running entries still exist: %+v", o.state.Running)
	}
	if cancelled["1"] != 1 || cancelled["2"] != 1 {
		t.Fatalf("cancel counts = %+v", cancelled)
	}
	if got := o.pendingCleanup["1"]; got != "ABC-1" {
		t.Fatalf("pending cleanup = %q, want ABC-1", got)
	}
	if len(workspace.cleaned) != 0 {
		t.Fatalf("cleanup should be deferred, got %+v", workspace.cleaned)
	}

	o.handleWorkerExit(WorkerResult{IssueID: "1", Identifier: "ABC-1", StartedAt: now, Phase: model.PhaseCanceledByReconciliation, Err: context.Canceled})
	if len(workspace.cleaned) != 1 || workspace.cleaned[0] != "ABC-1" {
		t.Fatalf("cleanup calls after worker exit = %+v", workspace.cleaned)
	}
}

func TestReconcileRunningSchedulesRetryForStalledSession(t *testing.T) {
	now := time.Date(2026, 3, 7, 10, 0, 0, 0, time.UTC)
	cfg := &model.ServiceConfig{
		ActiveStates:        []string{"Todo", "In Progress"},
		TerminalStates:      []string{"Done"},
		MaxConcurrentAgents: 1,
		MaxRetryBackoffMS:   300000,
		CodexStallTimeoutMS: 1000,
	}
	tracker := &fakeTracker{}
	o := newTestOrchestrator(cfg, tracker, &fakeWorkspaceManager{}, &fakeRunner{}, now)
	o.randFloat = func() float64 { return 0 }

	cancelCount := 0
	o.state.Running["1"] = &model.RunningEntry{
		Issue:        &model.Issue{ID: "1", Identifier: "ABC-1", State: "In Progress"},
		Identifier:   "ABC-1",
		RetryAttempt: 0,
		StartedAt:    now.Add(-2 * time.Second),
		WorkerCancel: func() { cancelCount++ },
	}
	o.state.Claimed["1"] = struct{}{}

	o.reconcileRunning(context.Background())

	if cancelCount != 1 {
		t.Fatalf("cancelCount = %d, want 1", cancelCount)
	}
	if len(o.state.Running) != 0 {
		t.Fatalf("running entries still exist: %+v", o.state.Running)
	}
	retry := o.state.RetryAttempts["1"]
	if retry == nil {
		t.Fatal("retry entry missing for stalled session")
	}
	if retry.Attempt != 1 {
		t.Fatalf("retry attempt = %d, want 1", retry.Attempt)
	}
	if retry.Error == nil || *retry.Error != "stalled session" {
		t.Fatalf("retry error = %v, want stalled session", retry.Error)
	}
	if retry.DueAt.Sub(now) != 5*time.Second {
		t.Fatalf("retry delay = %v, want 5s", retry.DueAt.Sub(now))
	}
	if tracker.stateFetchCalls != 0 {
		t.Fatalf("state fetch calls = %d, want 0 after stall removal", tracker.stateFetchCalls)
	}
	if retry.StallCount != 1 {
		t.Fatalf("stall count = %d, want 1", retry.StallCount)
	}
}

func TestReconcileRunningFirstStallAfterContinuationDoesNotTriggerRepeatedStall(t *testing.T) {
	now := time.Date(2026, 3, 7, 10, 0, 0, 0, time.UTC)
	cfg := &model.ServiceConfig{
		ActiveStates:        []string{"Todo", "In Progress"},
		TerminalStates:      []string{"Done"},
		MaxConcurrentAgents: 1,
		MaxRetryBackoffMS:   300000,
		CodexStallTimeoutMS: 1000,
	}
	tracker := &fakeTracker{}
	o := newTestOrchestrator(cfg, tracker, &fakeWorkspaceManager{}, &fakeRunner{}, now)
	o.randFloat = func() float64 { return 0 }

	o.state.Running["1"] = &model.RunningEntry{
		Issue:        &model.Issue{ID: "1", Identifier: "ABC-1", State: "In Progress"},
		Identifier:   "ABC-1",
		RetryAttempt: 1,
		StallCount:   0,
		StartedAt:    now.Add(-2 * time.Second),
		WorkerCancel: func() {},
	}
	o.state.Claimed["1"] = struct{}{}

	o.reconcileRunning(context.Background())

	retry := o.state.RetryAttempts["1"]
	if retry == nil {
		t.Fatal("retry entry missing for stalled session")
	}
	if retry.Attempt != 2 {
		t.Fatalf("retry attempt = %d, want 2", retry.Attempt)
	}
	if retry.StallCount != 1 {
		t.Fatalf("stall count = %d, want 1", retry.StallCount)
	}
	if hasAlertCode(o.Snapshot().Alerts, "repeated_stall") {
		t.Fatalf("alerts = %+v, want no repeated_stall", o.Snapshot().Alerts)
	}
}

func TestHandleRetryTimerRequeuesWhenNoSlotsAvailable(t *testing.T) {
	now := time.Date(2026, 3, 7, 10, 0, 0, 0, time.UTC)
	cfg := &model.ServiceConfig{
		ActiveStates:        []string{"Todo", "In Progress"},
		TerminalStates:      []string{"Done"},
		MaxConcurrentAgents: 1,
		MaxRetryBackoffMS:   300000,
		WorkspaceRoot:       "/tmp/workspaces",
	}
	tracker := &fakeTracker{
		candidateIssues: []model.Issue{
			{ID: "1", Identifier: "ABC-1", Title: "Retry me", State: "Todo"},
		},
	}
	o := newTestOrchestrator(cfg, tracker, &fakeWorkspaceManager{}, &fakeRunner{}, now)
	o.randFloat = func() float64 { return 0 }
	o.state.Running["busy"] = &model.RunningEntry{
		Issue:      &model.Issue{ID: "busy", Identifier: "ABC-9", State: "Todo"},
		Identifier: "ABC-9",
		StartedAt:  now.Add(-time.Second),
	}
	o.state.RetryAttempts["1"] = &model.RetryEntry{
		IssueID:       "1",
		Identifier:    "ABC-1",
		WorkspacePath: "/tmp/workspaces/ABC-1",
		Attempt:       1,
		DueAt:         now,
	}
	o.state.Claimed["1"] = struct{}{}

	o.handleRetryTimer(context.Background(), "1")

	retry := o.state.RetryAttempts["1"]
	if retry == nil {
		t.Fatal("retry entry missing after slot exhaustion")
	}
	if retry.Attempt != 2 {
		t.Fatalf("retry attempt = %d, want 2", retry.Attempt)
	}
	if retry.Error == nil || *retry.Error != "no available orchestrator slots" {
		t.Fatalf("retry error = %v, want no available orchestrator slots", retry.Error)
	}
	if retry.DueAt.Sub(now) != 10*time.Second {
		t.Fatalf("retry delay = %v, want 10s", retry.DueAt.Sub(now))
	}
}

func TestSnapshotIncludesAlertsAndWorkspaceContext(t *testing.T) {
	now := time.Date(2026, 3, 7, 10, 0, 0, 0, time.UTC)
	cfg := &model.ServiceConfig{
		ActiveStates:        []string{"Todo", "In Progress"},
		TerminalStates:      []string{"Done"},
		MaxConcurrentAgents: 2,
	}
	o := newTestOrchestrator(cfg, &fakeTracker{}, &fakeWorkspaceManager{}, &fakeRunner{}, now)
	o.systemAlerts["tracker_unreachable"] = AlertSnapshot{
		Code:    "tracker_unreachable",
		Level:   "warn",
		Message: "tracker down",
	}
	stallError := "stalled session"
	hookError := model.NewWorkspaceError(model.ErrWorkspaceHookFailed, "before_run failed", nil).Error()
	o.state.Running["1"] = &model.RunningEntry{
		Issue:         &model.Issue{ID: "1", Identifier: "ABC-1", State: "In Progress"},
		Identifier:    "ABC-1",
		WorkspacePath: "/tmp/ABC-1",
		RetryAttempt:  1,
		StartedAt:     now.Add(-time.Second),
		WorkerCancel:  func() {},
	}
	o.state.RetryAttempts["2"] = &model.RetryEntry{
		IssueID:       "2",
		Identifier:    "ABC-2",
		WorkspacePath: "/tmp/ABC-2",
		Attempt:       2,
		StallCount:    2,
		DueAt:         now.Add(time.Second),
		Error:         &stallError,
	}
	o.state.RetryAttempts["3"] = &model.RetryEntry{
		IssueID:       "3",
		Identifier:    "ABC-3",
		WorkspacePath: "/tmp/ABC-3",
		Attempt:       1,
		DueAt:         now.Add(time.Second),
		Error:         &hookError,
	}

	snapshot := o.Snapshot()
	if len(snapshot.Running) != 1 {
		t.Fatalf("running snapshot = %+v", snapshot.Running)
	}
	if snapshot.Running[0].WorkspacePath != "/tmp/ABC-1" {
		t.Fatalf("running workspace path = %q, want /tmp/ABC-1", snapshot.Running[0].WorkspacePath)
	}
	if snapshot.Running[0].AttemptCount != 2 {
		t.Fatalf("running attempt count = %d, want 2", snapshot.Running[0].AttemptCount)
	}
	codes := make(map[string]struct{}, len(snapshot.Alerts))
	for _, alert := range snapshot.Alerts {
		codes[alert.Code] = struct{}{}
	}
	for _, code := range []string{"tracker_unreachable", "repeated_stall", "workspace_hook_failure"} {
		if _, ok := codes[code]; !ok {
			t.Fatalf("missing alert code %q in %+v", code, snapshot.Alerts)
		}
	}
}

func TestRunOnceSetsAndClearsTrackerAlert(t *testing.T) {
	now := time.Date(2026, 3, 7, 10, 0, 0, 0, time.UTC)
	cfg := &model.ServiceConfig{
		TrackerKind:                "linear",
		TrackerAPIKey:              "secret",
		TrackerProjectSlug:         "demo",
		WorkspaceLinearBranchScope: "demo-scope",
		ActiveStates:               []string{"Todo", "In Progress"},
		TerminalStates:             []string{"Done"},
		MaxConcurrentAgents:        1,
		CodexCommand:               "codex app-server",
	}
	tracker := &fakeTracker{candidateErr: errors.New("tracker down")}
	o := newTestOrchestrator(cfg, tracker, &fakeWorkspaceManager{}, &fakeRunner{}, now)

	o.RunOnce(context.Background(), false)
	snapshot := o.Snapshot()
	if !hasAlertCode(snapshot.Alerts, "tracker_unreachable") {
		t.Fatalf("alerts = %+v, want tracker_unreachable", snapshot.Alerts)
	}

	tracker.candidateErr = nil
	tracker.candidateIssues = []model.Issue{}
	o.RunOnce(context.Background(), false)
	snapshot = o.Snapshot()
	if hasAlertCode(snapshot.Alerts, "tracker_unreachable") {
		t.Fatalf("alerts = %+v, want tracker_unreachable cleared", snapshot.Alerts)
	}
}

func TestRunOnceKeepsTrackerAlertWhenReconcileFails(t *testing.T) {
	now := time.Date(2026, 3, 7, 10, 0, 0, 0, time.UTC)
	cfg := &model.ServiceConfig{
		TrackerKind:                "linear",
		TrackerAPIKey:              "secret",
		TrackerProjectSlug:         "demo",
		WorkspaceLinearBranchScope: "demo-scope",
		ActiveStates:               []string{"Todo", "In Progress"},
		TerminalStates:             []string{"Done"},
		MaxConcurrentAgents:        1,
		CodexCommand:               "codex app-server",
	}
	tracker := &fakeTracker{
		candidateIssues: []model.Issue{},
		stateErr:        errors.New("state refresh down"),
	}
	o := newTestOrchestrator(cfg, tracker, &fakeWorkspaceManager{}, &fakeRunner{}, now)
	o.state.Running["1"] = &model.RunningEntry{
		Issue:        &model.Issue{ID: "1", Identifier: "ABC-1", State: "In Progress"},
		Identifier:   "ABC-1",
		StartedAt:    now.Add(-time.Second),
		WorkerCancel: func() {},
	}
	o.state.Claimed["1"] = struct{}{}

	o.RunOnce(context.Background(), false)
	snapshot := o.Snapshot()
	if !hasAlertCode(snapshot.Alerts, "tracker_unreachable") {
		t.Fatalf("alerts = %+v, want tracker_unreachable", snapshot.Alerts)
	}
	if tracker.candidateFetchCalls != 1 {
		t.Fatalf("candidate fetch calls = %d, want 1", tracker.candidateFetchCalls)
	}
	if tracker.stateFetchCalls != 1 {
		t.Fatalf("state fetch calls = %d, want 1", tracker.stateFetchCalls)
	}

	tracker.stateErr = nil
	tracker.stateByID = map[string]model.Issue{
		"1": {ID: "1", Identifier: "ABC-1", State: "In Progress"},
	}
	o.RunOnce(context.Background(), false)
	snapshot = o.Snapshot()
	if hasAlertCode(snapshot.Alerts, "tracker_unreachable") {
		t.Fatalf("alerts = %+v, want tracker_unreachable cleared", snapshot.Alerts)
	}
}

func TestHandleWorkerExitPerformsDeferredCleanupAfterTerminalReconcile(t *testing.T) {
	now := time.Date(2026, 3, 7, 10, 0, 0, 0, time.UTC)
	cfg := &model.ServiceConfig{
		ActiveStates:        []string{"Todo", "In Progress"},
		TerminalStates:      []string{"Done"},
		MaxConcurrentAgents: 1,
	}
	workspace := &fakeWorkspaceManager{}
	o := newTestOrchestrator(cfg, &fakeTracker{}, workspace, &fakeRunner{}, now)
	cancelCount := 0
	o.state.Running["1"] = &model.RunningEntry{
		Issue:         &model.Issue{ID: "1", Identifier: "ABC-1", State: "In Progress"},
		Identifier:    "ABC-1",
		WorkspacePath: "/tmp/ABC-1",
		StartedAt:     now.Add(-time.Second),
		WorkerCancel:  func() { cancelCount++ },
	}
	o.state.Claimed["1"] = struct{}{}

	o.mu.Lock()
	o.terminateRunningLocked(context.Background(), "1", true, false, "")
	o.mu.Unlock()

	if cancelCount != 1 {
		t.Fatalf("cancelCount = %d, want 1", cancelCount)
	}
	if len(workspace.cleaned) != 0 {
		t.Fatalf("cleanup should be deferred, got %+v", workspace.cleaned)
	}
	if got := o.pendingCleanup["1"]; got != "ABC-1" {
		t.Fatalf("pending cleanup = %q, want ABC-1", got)
	}

	o.handleWorkerExit(WorkerResult{
		IssueID:    "1",
		Identifier: "ABC-1",
		StartedAt:  now,
		Phase:      model.PhaseCanceledByReconciliation,
		Err:        context.Canceled,
	})

	if len(workspace.cleaned) != 1 || workspace.cleaned[0] != "ABC-1" {
		t.Fatalf("cleanup calls = %+v, want [ABC-1]", workspace.cleaned)
	}
	if _, ok := o.pendingCleanup["1"]; ok {
		t.Fatal("pending cleanup still exists")
	}
}

func TestRunOncePreflightFailureStillReconcilesRunningIssues(t *testing.T) {
	now := time.Date(2026, 3, 7, 10, 0, 0, 0, time.UTC)
	cfg := &model.ServiceConfig{
		ActiveStates:        []string{"Todo", "In Progress"},
		TerminalStates:      []string{"Done"},
		MaxConcurrentAgents: 1,
		CodexStallTimeoutMS: 300000,
	}
	runCh := make(chan string, 1)
	runner := &fakeRunner{runCh: runCh}
	workspace := &fakeWorkspaceManager{}
	tracker := &fakeTracker{
		candidateIssues: []model.Issue{
			{ID: "candidate-1", Identifier: "ABC-NEW", Title: "new work", State: "Todo"},
		},
		stateByID: map[string]model.Issue{
			"1": {ID: "1", Identifier: "ABC-1", State: "Done"},
		},
	}
	o := newTestOrchestrator(cfg, tracker, workspace, runner, now)
	o.state.Running["1"] = &model.RunningEntry{
		Issue:        &model.Issue{ID: "1", Identifier: "ABC-1", State: "Todo"},
		Identifier:   "ABC-1",
		StartedAt:    now.Add(-10 * time.Second),
		WorkerCancel: func() {},
	}
	o.state.Claimed["1"] = struct{}{}

	o.RunOnce(context.Background(), true)

	if tracker.stateFetchCalls != 1 {
		t.Fatalf("state fetch calls = %d, want 1", tracker.stateFetchCalls)
	}
	if tracker.candidateFetchCalls != 0 {
		t.Fatalf("candidate fetch calls = %d, want 0", tracker.candidateFetchCalls)
	}
	if len(o.state.Running) != 0 {
		t.Fatalf("running entries still exist: %+v", o.state.Running)
	}
	if got := o.pendingCleanup["1"]; got != "ABC-1" {
		t.Fatalf("pending cleanup = %q, want ABC-1", got)
	}
	o.handleWorkerExit(WorkerResult{IssueID: "1", Identifier: "ABC-1", StartedAt: now, Phase: model.PhaseCanceledByReconciliation, Err: context.Canceled})
	if len(workspace.cleaned) != 1 || workspace.cleaned[0] != "ABC-1" {
		t.Fatalf("cleanup calls = %+v, want [ABC-1]", workspace.cleaned)
	}
	select {
	case identifier := <-runCh:
		t.Fatalf("unexpected dispatch for %q during preflight failure", identifier)
	default:
	}
}

func TestHandleCodexUpdateAggregatesUsage(t *testing.T) {
	now := time.Date(2026, 3, 7, 10, 0, 0, 0, time.UTC)
	cfg := &model.ServiceConfig{
		ActiveStates:              []string{"Todo", "In Progress"},
		TerminalStates:            []string{"Done"},
		MaxConcurrentAgents:       1,
		OrchestratorAutoCloseOnPR: true,
	}
	o := newTestOrchestrator(cfg, &fakeTracker{}, &fakeWorkspaceManager{}, &fakeRunner{}, now)
	o.state.Running["1"] = &model.RunningEntry{
		Issue:      &model.Issue{ID: "1", Identifier: "ABC-1", State: "Todo"},
		Identifier: "ABC-1",
		StartedAt:  now,
	}

	o.handleCodexUpdate(CodexUpdate{
		IssueID: "1",
		Event: agent.AgentEvent{
			Event:     "notification",
			Timestamp: now,
			SessionID: stringPtr("thread-1-turn-1"),
			ThreadID:  stringPtr("thread-1"),
			TurnID:    stringPtr("turn-1"),
			Usage:     &agent.TokenUsage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
			Message:   "tokenUsage",
		},
	})
	o.handleCodexUpdate(CodexUpdate{
		IssueID: "1",
		Event: agent.AgentEvent{
			Event:     "notification",
			Timestamp: now.Add(time.Second),
			Usage:     &agent.TokenUsage{InputTokens: 12, OutputTokens: 8, TotalTokens: 20},
			Message:   "tokenUsage",
		},
	})

	if o.state.CodexTotals.InputTokens != 12 || o.state.CodexTotals.OutputTokens != 8 || o.state.CodexTotals.TotalTokens != 20 {
		t.Fatalf("codex totals = %+v", o.state.CodexTotals)
	}
	entry := o.state.Running["1"]
	if entry.Session.SessionID != "thread-1-turn-1" || entry.Session.TurnCount == 0 {
		t.Fatalf("session = %+v", entry.Session)
	}
}

func TestHandleCodexUpdateStoresRateLimitsInSnapshot(t *testing.T) {
	now := time.Date(2026, 3, 7, 10, 0, 0, 0, time.UTC)
	cfg := &model.ServiceConfig{
		ActiveStates:        []string{"Todo", "In Progress"},
		TerminalStates:      []string{"Done"},
		MaxConcurrentAgents: 1,
	}
	o := newTestOrchestrator(cfg, &fakeTracker{}, &fakeWorkspaceManager{}, &fakeRunner{}, now)
	o.state.Running["1"] = &model.RunningEntry{
		Issue:      &model.Issue{ID: "1", Identifier: "ABC-1", State: "Todo"},
		Identifier: "ABC-1",
		StartedAt:  now,
	}
	rateLimits := map[string]any{"remaining": 12, "resetAt": "2026-03-07T10:05:00Z"}

	o.handleCodexUpdate(CodexUpdate{
		IssueID: "1",
		Event: agent.AgentEvent{
			Event:      "notification",
			Timestamp:  now,
			RateLimits: rateLimits,
		},
	})

	snapshot := o.Snapshot()
	got, ok := snapshot.RateLimits.(map[string]any)
	if !ok {
		t.Fatalf("snapshot rate limits type = %T, want map[string]any", snapshot.RateLimits)
	}
	if got["remaining"] != 12 {
		t.Fatalf("snapshot rate limits = %+v, want remaining=12", got)
	}
}

func TestHandleWorkerExitAlreadyTerminal(t *testing.T) {
	now := time.Date(2026, 3, 7, 10, 0, 0, 0, time.UTC)
	cfg := &model.ServiceConfig{
		ActiveStates:        []string{"Todo", "In Progress"},
		TerminalStates:      []string{"Done"},
		MaxConcurrentAgents: 1,
	}
	tracker := &fakeTracker{
		stateByID: map[string]model.Issue{
			"1": {ID: "1", Identifier: "ABC-1", State: "Done"},
		},
	}
	workspace := &fakeWorkspaceManager{}
	o := newTestOrchestrator(cfg, tracker, workspace, &fakeRunner{}, now)
	o.state.Running["1"] = &model.RunningEntry{
		Issue:        &model.Issue{ID: "1", Identifier: "ABC-1", State: "Todo"},
		Identifier:   "ABC-1",
		RetryAttempt: 0,
		StartedAt:    now.Add(-time.Second),
		WorkerCancel: func() {},
	}
	o.state.Claimed["1"] = struct{}{}

	o.handleWorkerExit(WorkerResult{IssueID: "1", Identifier: "ABC-1", StartedAt: now, Phase: model.PhaseSucceeded})

	if len(o.state.RetryAttempts) != 0 {
		t.Fatalf("retry attempts = %+v, want none", o.state.RetryAttempts)
	}
	if len(workspace.cleaned) != 1 || workspace.cleaned[0] != "ABC-1" {
		t.Fatalf("cleanup calls = %+v, want [ABC-1]", workspace.cleaned)
	}
	if _, ok := o.state.Claimed["1"]; ok {
		t.Fatal("claimed entry still exists")
	}
}

func TestHandleWorkerExitHasNewOpenPRMergedTransitionsToDone(t *testing.T) {
	now := time.Date(2026, 3, 7, 10, 0, 0, 0, time.UTC)
	cfg := &model.ServiceConfig{
		ActiveStates:              []string{"Todo", "In Progress"},
		TerminalStates:            []string{"Done"},
		MaxConcurrentAgents:       1,
		OrchestratorAutoCloseOnPR: true,
	}
	tracker := &fakeTracker{
		stateByID: map[string]model.Issue{
			"1": {ID: "1", Identifier: "ABC-1", State: "In Progress"},
		},
	}
	tracker.onTransition = func(issueID string, targetState string) {
		trackerIssue := tracker.stateByID[issueID]
		trackerIssue.State = "Done"
		tracker.stateByID[issueID] = trackerIssue
	}
	workspace := &fakeWorkspaceManager{}
	o := newTestOrchestrator(cfg, tracker, workspace, &fakeRunner{}, now)
	branch := "iiwate4268/iiwate-33-test"
	o.prLookup = &fakePRLookup{
		byBranch: map[string]*PullRequestInfo{
			branch: {Number: 42, URL: "https://example.test/pr/42", HeadBranch: branch, State: PullRequestStateMerged},
		},
	}
	o.state.Running["1"] = &model.RunningEntry{
		Issue:         &model.Issue{ID: "1", Identifier: "ABC-1", State: "Todo"},
		Identifier:    "ABC-1",
		WorkspacePath: "C:/work/ABC-1",
		RetryAttempt:  0,
		StartedAt:     now.Add(-time.Second),
		WorkerCancel:  func() {},
		Dispatch:      pullRequestDispatch(),
	}
	o.state.Claimed["1"] = struct{}{}

	o.handleWorkerExit(WorkerResult{
		IssueID:      "1",
		Identifier:   "ABC-1",
		StartedAt:    now,
		Phase:        model.PhaseSucceeded,
		HasNewOpenPR: true,
		FinalBranch:  branch,
	})

	if tracker.transitionCalls != 1 || tracker.transitionTarget != "Done" {
		t.Fatalf("transition calls = %d target = %q", tracker.transitionCalls, tracker.transitionTarget)
	}
	if len(o.state.RetryAttempts) != 0 {
		t.Fatalf("retry attempts = %+v, want none", o.state.RetryAttempts)
	}
	if len(workspace.cleaned) != 1 || workspace.cleaned[0] != "ABC-1" {
		t.Fatalf("cleanup calls = %+v, want [ABC-1]", workspace.cleaned)
	}
}

func TestHandleWorkerExitHasNewOpenPRMovesToAwaitingMerge(t *testing.T) {
	now := time.Date(2026, 3, 7, 10, 0, 0, 0, time.UTC)
	cfg := &model.ServiceConfig{
		ActiveStates:              []string{"Todo", "In Progress"},
		TerminalStates:            []string{"Done"},
		MaxConcurrentAgents:       1,
		OrchestratorAutoCloseOnPR: true,
	}
	tracker := &fakeTracker{
		stateByID: map[string]model.Issue{
			"1": {ID: "1", Identifier: "ABC-1", State: "In Progress"},
		},
	}
	workspace := &fakeWorkspaceManager{}
	o := newTestOrchestrator(cfg, tracker, workspace, &fakeRunner{}, now)
	branch := "iiwate4268/iiwate-48-await"
	o.prLookup = &fakePRLookup{
		byBranch: map[string]*PullRequestInfo{
			branch: {Number: 48, URL: "https://example.test/pr/48", HeadBranch: branch, State: PullRequestStateOpen},
		},
	}
	o.state.Running["1"] = &model.RunningEntry{
		Issue:         &model.Issue{ID: "1", Identifier: "ABC-1", State: "Todo"},
		Identifier:    "ABC-1",
		WorkspacePath: "C:/work/ABC-1",
		RetryAttempt:  0,
		StartedAt:     now.Add(-time.Second),
		WorkerCancel:  func() {},
		Dispatch:      pullRequestDispatch(),
	}
	o.state.Claimed["1"] = struct{}{}

	o.handleWorkerExit(WorkerResult{
		IssueID:      "1",
		Identifier:   "ABC-1",
		StartedAt:    now,
		Phase:        model.PhaseSucceeded,
		HasNewOpenPR: true,
		FinalBranch:  branch,
	})

	if tracker.transitionCalls != 0 {
		t.Fatalf("transition calls = %d, want 0", tracker.transitionCalls)
	}
	if len(o.state.RetryAttempts) != 0 {
		t.Fatalf("retry attempts = %+v, want none", o.state.RetryAttempts)
	}
	if len(workspace.cleaned) != 0 {
		t.Fatalf("cleanup calls = %+v, want none", workspace.cleaned)
	}
	awaiting := o.state.AwaitingMerge["1"]
	if awaiting == nil {
		t.Fatal("awaiting merge entry missing")
	}
	if awaiting.Branch != branch || awaiting.PRState != string(PullRequestStateOpen) {
		t.Fatalf("awaiting merge entry = %+v", awaiting)
	}
}

func TestHandleWorkerExitClosedPRMovesToAwaitingIntervention(t *testing.T) {
	now := time.Date(2026, 3, 7, 10, 0, 0, 0, time.UTC)
	cfg := &model.ServiceConfig{
		ActiveStates:              []string{"Todo", "In Progress"},
		TerminalStates:            []string{"Done"},
		MaxConcurrentAgents:       1,
		OrchestratorAutoCloseOnPR: true,
	}
	tracker := &fakeTracker{
		stateByID: map[string]model.Issue{
			"1": {ID: "1", Identifier: "ABC-1", State: "In Progress"},
		},
	}
	workspace := &fakeWorkspaceManager{}
	o := newTestOrchestrator(cfg, tracker, workspace, &fakeRunner{}, now)
	branch := "iiwate4268/iiwate-48-closed"
	o.prLookup = &fakePRLookup{
		byBranch: map[string]*PullRequestInfo{
			branch: {Number: 48, URL: "https://example.test/pr/48", HeadBranch: branch, State: PullRequestStateClosed},
		},
	}
	o.state.Running["1"] = &model.RunningEntry{
		Issue:         &model.Issue{ID: "1", Identifier: "ABC-1", State: "Todo"},
		Identifier:    "ABC-1",
		WorkspacePath: "C:/work/ABC-1",
		RetryAttempt:  0,
		StartedAt:     now.Add(-time.Second),
		WorkerCancel:  func() {},
		Dispatch:      pullRequestDispatch(),
	}
	o.state.Claimed["1"] = struct{}{}

	o.handleWorkerExit(WorkerResult{
		IssueID:      "1",
		Identifier:   "ABC-1",
		StartedAt:    now,
		Phase:        model.PhaseSucceeded,
		HasNewOpenPR: true,
		FinalBranch:  branch,
	})

	if tracker.transitionCalls != 0 {
		t.Fatalf("transition calls = %d, want 0", tracker.transitionCalls)
	}
	if _, ok := o.state.AwaitingMerge["1"]; ok {
		t.Fatal("awaiting merge entry should not exist for closed PR")
	}
	awaiting := o.state.AwaitingIntervention["1"]
	if awaiting == nil {
		t.Fatal("awaiting intervention entry missing")
	}
	if awaiting.Branch != branch || awaiting.PRState != string(PullRequestStateClosed) {
		t.Fatalf("awaiting intervention entry = %+v", awaiting)
	}
	if len(o.state.RetryAttempts) != 0 {
		t.Fatalf("retry attempts = %+v, want none", o.state.RetryAttempts)
	}
	if _, ok := o.state.Claimed["1"]; !ok {
		t.Fatal("claimed entry should be retained while awaiting intervention")
	}
	if len(workspace.cleaned) != 0 {
		t.Fatalf("cleanup calls = %+v, want none", workspace.cleaned)
	}
}

func TestHandleWorkerExitNoNewPRSchedulesContinuation(t *testing.T) {
	now := time.Date(2026, 3, 7, 10, 0, 0, 0, time.UTC)
	cfg := &model.ServiceConfig{
		ActiveStates:        []string{"Todo", "In Progress"},
		TerminalStates:      []string{"Done"},
		MaxConcurrentAgents: 1,
	}
	tracker := &fakeTracker{
		stateByID: map[string]model.Issue{
			"1": {ID: "1", Identifier: "ABC-1", State: "In Progress"},
		},
	}
	o := newTestOrchestrator(cfg, tracker, &fakeWorkspaceManager{}, &fakeRunner{}, now)
	o.state.Running["1"] = &model.RunningEntry{
		Issue:        &model.Issue{ID: "1", Identifier: "ABC-1", State: "Todo"},
		Identifier:   "ABC-1",
		RetryAttempt: 0,
		StartedAt:    now.Add(-time.Second),
		WorkerCancel: func() {},
	}

	o.handleWorkerExit(WorkerResult{IssueID: "1", Identifier: "ABC-1", StartedAt: now, Phase: model.PhaseSucceeded})

	retry := o.state.RetryAttempts["1"]
	if retry == nil {
		t.Fatal("continuation retry missing")
	}
	if retry.DueAt.Sub(now) != time.Second {
		t.Fatalf("continuation retry delay = %v, want 1s", retry.DueAt.Sub(now))
	}
}

func TestDispatchIssueBranchDetectionFailureSchedulesRetry(t *testing.T) {
	now := time.Date(2026, 3, 7, 10, 0, 0, 0, time.UTC)
	cfg := &model.ServiceConfig{
		ActiveStates:        []string{"Todo", "In Progress"},
		TerminalStates:      []string{"Done"},
		MaxConcurrentAgents: 1,
	}
	o := newTestOrchestrator(cfg, &fakeTracker{}, &fakeWorkspaceManager{}, &fakeRunner{}, now)
	o.gitBranchFn = func(context.Context, string) (string, error) {
		return "", errors.New("git branch failed")
	}

	o.dispatchIssue(context.Background(), model.Issue{
		ID:         "1",
		Identifier: "ABC-1",
		Title:      "test",
		State:      "Todo",
	}, nil)

	var result WorkerResult
	select {
	case result = <-o.workerResultCh:
	case <-time.After(2 * time.Second):
		t.Fatal("worker result not received")
	}
	if result.Err == nil || !strings.Contains(result.Err.Error(), "detect final branch") {
		t.Fatalf("worker result error = %v, want detect final branch failure", result.Err)
	}
	if result.Phase != model.PhaseFailed {
		t.Fatalf("worker result phase = %s, want failed", result.Phase.String())
	}

	o.handleWorkerExit(result)

	if _, ok := o.state.AwaitingIntervention["1"]; ok {
		t.Fatal("awaiting intervention should not be created for branch detection failure")
	}
	retry := o.state.RetryAttempts["1"]
	if retry == nil {
		t.Fatal("retry entry missing")
	}
	if retry.Error == nil || !strings.Contains(*retry.Error, "detect final branch") {
		t.Fatalf("retry entry = %+v, want detect final branch error", retry)
	}
}

func TestHandleWorkerExitMissingPRMovesToAwaitingInterventionForPullRequestMode(t *testing.T) {
	now := time.Date(2026, 3, 7, 10, 0, 0, 0, time.UTC)
	cfg := &model.ServiceConfig{
		ActiveStates:        []string{"Todo", "In Progress"},
		TerminalStates:      []string{"Done"},
		MaxConcurrentAgents: 1,
	}
	tracker := &fakeTracker{
		stateByID: map[string]model.Issue{
			"1": {ID: "1", Identifier: "ABC-1", State: "In Progress"},
		},
	}
	o := newTestOrchestrator(cfg, tracker, &fakeWorkspaceManager{}, &fakeRunner{}, now)
	o.prLookup = &fakePRLookup{byBranch: map[string]*PullRequestInfo{}}
	o.state.Running["1"] = &model.RunningEntry{
		Issue:         &model.Issue{ID: "1", Identifier: "ABC-1", State: "Todo"},
		Identifier:    "ABC-1",
		WorkspacePath: "C:/work/ABC-1",
		RetryAttempt:  0,
		StartedAt:     now.Add(-time.Second),
		WorkerCancel:  func() {},
		Dispatch:      pullRequestDispatch(),
	}
	o.state.Claimed["1"] = struct{}{}

	o.handleWorkerExit(WorkerResult{
		IssueID:     "1",
		Identifier:  "ABC-1",
		StartedAt:   now,
		Phase:       model.PhaseSucceeded,
		FinalBranch: "iiwate4268/iiwate-48-missing",
	})

	awaiting := o.state.AwaitingIntervention["1"]
	if awaiting == nil {
		t.Fatal("awaiting intervention entry missing")
	}
	if awaiting.Reason != "missing_pr" {
		t.Fatalf("awaiting intervention entry = %+v", awaiting)
	}
	if len(o.state.RetryAttempts) != 0 {
		t.Fatalf("retry attempts = %+v, want none", o.state.RetryAttempts)
	}
}

func TestHandleWorkerExitMergedPRTransitionFailureMovesToAwaitingIntervention(t *testing.T) {
	now := time.Date(2026, 3, 7, 10, 0, 0, 0, time.UTC)
	cfg := &model.ServiceConfig{
		ActiveStates:              []string{"Todo", "In Progress"},
		TerminalStates:            []string{"Done"},
		MaxConcurrentAgents:       1,
		MaxRetryBackoffMS:         300000,
		OrchestratorAutoCloseOnPR: true,
	}
	tracker := &fakeTracker{
		stateByID: map[string]model.Issue{
			"1": {ID: "1", Identifier: "ABC-1", State: "In Progress"},
		},
		transitionErr: errors.New("boom"),
	}
	o := newTestOrchestrator(cfg, tracker, &fakeWorkspaceManager{}, &fakeRunner{}, now)
	o.randFloat = func() float64 { return 0 }
	branch := "iiwate4268/iiwate-33-test"
	o.prLookup = &fakePRLookup{
		byBranch: map[string]*PullRequestInfo{
			branch: {Number: 42, URL: "https://example.test/pr/42", HeadBranch: branch, State: PullRequestStateMerged},
		},
	}
	o.state.Running["1"] = &model.RunningEntry{
		Issue:         &model.Issue{ID: "1", Identifier: "ABC-1", State: "Todo"},
		Identifier:    "ABC-1",
		WorkspacePath: "C:/work/ABC-1",
		RetryAttempt:  0,
		StartedAt:     now.Add(-time.Second),
		WorkerCancel:  func() {},
		Dispatch:      pullRequestDispatch(),
	}

	o.handleWorkerExit(WorkerResult{
		IssueID:      "1",
		Identifier:   "ABC-1",
		StartedAt:    now,
		Phase:        model.PhaseSucceeded,
		HasNewOpenPR: true,
		FinalBranch:  branch,
	})

	if len(o.state.RetryAttempts) != 0 {
		t.Fatalf("retry attempts = %+v, want none", o.state.RetryAttempts)
	}
	if _, ok := o.state.AwaitingMerge["1"]; ok {
		t.Fatal("awaiting merge entry should be cleared for non-retryable transition failure")
	}
	awaiting := o.state.AwaitingIntervention["1"]
	if awaiting == nil {
		t.Fatal("awaiting intervention entry missing")
	}
	if awaiting.Reason != "post_merge_transition_failed" {
		t.Fatalf("awaiting intervention entry = %+v, want post_merge_transition_failed", awaiting)
	}
	if awaiting.PRState != string(PullRequestStateMerged) {
		t.Fatalf("awaiting intervention entry = %+v, want merged PR state", awaiting)
	}
	if _, ok := o.state.Claimed["1"]; !ok {
		t.Fatal("claimed entry should be retained while awaiting intervention")
	}
}

func TestHandleWorkerExitMergedPRRetryableTransitionFailureSchedulesBackoff(t *testing.T) {
	now := time.Date(2026, 3, 7, 10, 0, 0, 0, time.UTC)
	cfg := &model.ServiceConfig{
		ActiveStates:              []string{"Todo", "In Progress"},
		TerminalStates:            []string{"Done"},
		MaxConcurrentAgents:       1,
		MaxRetryBackoffMS:         300000,
		OrchestratorAutoCloseOnPR: true,
	}
	tracker := &fakeTracker{
		stateByID: map[string]model.Issue{
			"1": {ID: "1", Identifier: "ABC-1", State: "In Progress"},
		},
		transitionErr: model.NewTrackerError(model.ErrLinearAPIRequest, "temporary", nil),
	}
	o := newTestOrchestrator(cfg, tracker, &fakeWorkspaceManager{}, &fakeRunner{}, now)
	branch := "iiwate4268/iiwate-33-test"
	o.prLookup = &fakePRLookup{
		byBranch: map[string]*PullRequestInfo{
			branch: {Number: 42, URL: "https://example.test/pr/42", HeadBranch: branch, State: PullRequestStateMerged, BaseOwner: "IIwate", BaseRepo: "linear-test"},
		},
	}
	o.state.Running["1"] = &model.RunningEntry{
		Issue:         &model.Issue{ID: "1", Identifier: "ABC-1", State: "Todo"},
		Identifier:    "ABC-1",
		WorkspacePath: "C:/work/ABC-1",
		RetryAttempt:  0,
		StartedAt:     now.Add(-time.Second),
		WorkerCancel:  func() {},
		Dispatch:      pullRequestDispatch(),
	}

	o.handleWorkerExit(WorkerResult{
		IssueID:      "1",
		Identifier:   "ABC-1",
		StartedAt:    now,
		Phase:        model.PhaseSucceeded,
		HasNewOpenPR: true,
		FinalBranch:  branch,
	})

	awaiting := o.state.AwaitingMerge["1"]
	if awaiting == nil {
		t.Fatal("awaiting merge entry missing")
	}
	if awaiting.PostMergeRetryCount != 1 {
		t.Fatalf("awaiting merge entry = %+v, want PostMergeRetryCount=1", awaiting)
	}
	if awaiting.NextPostMergeRetryAt == nil || !awaiting.NextPostMergeRetryAt.Equal(now.Add(10*time.Second)) {
		t.Fatalf("awaiting merge entry = %+v, want next retry at %s", awaiting, now.Add(10*time.Second))
	}
	if awaiting.LastError == nil || !strings.Contains(*awaiting.LastError, "post-merge transition failed") {
		t.Fatalf("awaiting merge entry = %+v, want post-merge transition error", awaiting)
	}
	if _, ok := o.state.AwaitingIntervention["1"]; ok {
		t.Fatal("awaiting intervention entry should not be created for retryable transition failure")
	}
}

func TestHandleWorkerExitMergedPRWithoutTransitionSupportMovesToAwaitingIntervention(t *testing.T) {
	now := time.Date(2026, 3, 7, 10, 0, 0, 0, time.UTC)
	cfg := &model.ServiceConfig{
		ActiveStates:              []string{"Todo", "In Progress"},
		TerminalStates:            []string{"Done"},
		MaxConcurrentAgents:       1,
		OrchestratorAutoCloseOnPR: true,
	}
	trackerClient := &fakeTrackerClientOnly{
		stateByID: map[string]model.Issue{
			"1": {ID: "1", Identifier: "ABC-1", State: "In Progress"},
		},
	}
	o := newTestOrchestrator(cfg, trackerClient, &fakeWorkspaceManager{}, &fakeRunner{}, now)
	branch := "iiwate4268/iiwate-33-test"
	o.prLookup = &fakePRLookup{
		byBranch: map[string]*PullRequestInfo{
			branch: {Number: 42, URL: "https://example.test/pr/42", HeadBranch: branch, State: PullRequestStateMerged},
		},
	}
	o.state.Running["1"] = &model.RunningEntry{
		Issue:         &model.Issue{ID: "1", Identifier: "ABC-1", State: "Todo"},
		Identifier:    "ABC-1",
		WorkspacePath: "C:/work/ABC-1",
		RetryAttempt:  0,
		StartedAt:     now.Add(-time.Second),
		WorkerCancel:  func() {},
		Dispatch:      pullRequestDispatch(),
	}

	o.handleWorkerExit(WorkerResult{
		IssueID:      "1",
		Identifier:   "ABC-1",
		StartedAt:    now,
		Phase:        model.PhaseSucceeded,
		HasNewOpenPR: true,
		FinalBranch:  branch,
	})

	if _, ok := o.state.AwaitingMerge["1"]; ok {
		t.Fatal("awaiting merge entry should be cleared for unsupported transition")
	}
	awaiting := o.state.AwaitingIntervention["1"]
	if awaiting == nil {
		t.Fatal("awaiting intervention entry missing")
	}
	if awaiting.Reason != "post_merge_transition_unsupported" {
		t.Fatalf("awaiting intervention entry = %+v, want post_merge_transition_unsupported", awaiting)
	}
	if len(o.state.RetryAttempts) != 0 {
		t.Fatalf("retry attempts = %+v, want none", o.state.RetryAttempts)
	}
}

func TestHandleWorkerExitMergedPRAutoCloseDisabledMovesToAwaitingIntervention(t *testing.T) {
	now := time.Date(2026, 3, 7, 10, 0, 0, 0, time.UTC)
	cfg := &model.ServiceConfig{
		ActiveStates:              []string{"Todo", "In Progress"},
		TerminalStates:            []string{"Done"},
		MaxConcurrentAgents:       1,
		OrchestratorAutoCloseOnPR: false,
	}
	tracker := &fakeTracker{
		stateByID: map[string]model.Issue{
			"1": {ID: "1", Identifier: "ABC-1", State: "In Progress"},
		},
	}
	o := newTestOrchestrator(cfg, tracker, &fakeWorkspaceManager{}, &fakeRunner{}, now)
	o.prLookup = &fakePRLookup{
		byBranch: map[string]*PullRequestInfo{
			"iiwate4268/iiwate-33-test": {Number: 42, URL: "https://example.test/pr/42", HeadBranch: "iiwate4268/iiwate-33-test", State: PullRequestStateMerged},
		},
	}
	o.state.Running["1"] = &model.RunningEntry{
		Issue:         &model.Issue{ID: "1", Identifier: "ABC-1", State: "Todo"},
		Identifier:    "ABC-1",
		WorkspacePath: "C:/work/ABC-1",
		RetryAttempt:  0,
		StartedAt:     now.Add(-time.Second),
		WorkerCancel:  func() {},
		Dispatch:      pullRequestDispatch(),
	}

	o.handleWorkerExit(WorkerResult{
		IssueID:      "1",
		Identifier:   "ABC-1",
		StartedAt:    now,
		Phase:        model.PhaseSucceeded,
		HasNewOpenPR: true,
		FinalBranch:  "iiwate4268/iiwate-33-test",
	})

	if tracker.transitionCalls != 0 {
		t.Fatalf("transition calls = %d, want 0", tracker.transitionCalls)
	}
	awaiting := o.state.AwaitingIntervention["1"]
	if awaiting == nil {
		t.Fatal("awaiting intervention entry missing")
	}
	if awaiting.Reason != string(model.ContinuationReasonMergedPRAutoCloseOff) {
		t.Fatalf("awaiting intervention entry = %+v", awaiting)
	}
}

func TestReconcileAwaitingMergeMergedClosesIssue(t *testing.T) {
	now := time.Date(2026, 3, 7, 10, 0, 0, 0, time.UTC)
	cfg := &model.ServiceConfig{
		ActiveStates:              []string{"Todo", "In Progress"},
		TerminalStates:            []string{"Done"},
		MaxConcurrentAgents:       1,
		OrchestratorAutoCloseOnPR: true,
	}
	tracker := &fakeTracker{
		stateByID: map[string]model.Issue{
			"1": {ID: "1", Identifier: "ABC-1", State: "In Progress"},
		},
	}
	tracker.onTransition = func(issueID string, targetState string) {
		trackerIssue := tracker.stateByID[issueID]
		trackerIssue.State = "Done"
		tracker.stateByID[issueID] = trackerIssue
	}
	workspace := &fakeWorkspaceManager{}
	o := newTestOrchestrator(cfg, tracker, workspace, &fakeRunner{}, now)
	branch := "iiwate4268/iiwate-48-await"
	o.prLookup = &fakePRLookup{
		byBranch: map[string]*PullRequestInfo{
			branch: {Number: 48, URL: "https://example.test/pr/48", HeadBranch: branch, State: PullRequestStateMerged},
		},
	}
	o.state.AwaitingMerge["1"] = &model.AwaitingMergeEntry{
		Identifier:    "ABC-1",
		State:         "In Progress",
		WorkspacePath: "C:/work/ABC-1",
		Branch:        branch,
		RetryAttempt:  0,
		AwaitingSince: now.Add(-time.Minute),
	}
	o.state.Claimed["1"] = struct{}{}

	o.reconcileAwaitingMerge(context.Background())

	if tracker.transitionCalls != 1 || tracker.transitionTarget != "Done" {
		t.Fatalf("transition calls = %d target = %q", tracker.transitionCalls, tracker.transitionTarget)
	}
	if _, ok := o.state.AwaitingMerge["1"]; ok {
		t.Fatal("awaiting merge entry still exists")
	}
	if _, ok := o.state.Claimed["1"]; ok {
		t.Fatal("claimed entry still exists")
	}
	if len(workspace.cleaned) != 1 || workspace.cleaned[0] != "ABC-1" {
		t.Fatalf("cleanup calls = %+v, want [ABC-1]", workspace.cleaned)
	}
}

func TestReconcileAwaitingMergeTerminalIssueCleansUpAndReleasesClaim(t *testing.T) {
	now := time.Date(2026, 3, 7, 10, 0, 0, 0, time.UTC)
	cfg := &model.ServiceConfig{
		ActiveStates:        []string{"Todo", "In Progress"},
		TerminalStates:      []string{"Done", "Cancelled"},
		MaxConcurrentAgents: 1,
	}
	tracker := &fakeTracker{
		stateByID: map[string]model.Issue{
			"1": {ID: "1", Identifier: "ABC-1", State: "Done"},
		},
	}
	workspace := &fakeWorkspaceManager{}
	o := newTestOrchestrator(cfg, tracker, workspace, &fakeRunner{}, now)
	branch := "iiwate4268/iiwate-48-await"
	o.prLookup = &fakePRLookup{
		byBranch: map[string]*PullRequestInfo{
			branch: {Number: 48, URL: "https://example.test/pr/48", HeadBranch: branch, State: PullRequestStateOpen},
		},
	}
	o.state.AwaitingMerge["1"] = &model.AwaitingMergeEntry{
		Identifier:    "ABC-1",
		State:         "In Progress",
		WorkspacePath: "C:/work/ABC-1",
		Branch:        branch,
		RetryAttempt:  0,
		AwaitingSince: now.Add(-time.Minute),
	}
	o.state.Claimed["1"] = struct{}{}

	o.reconcileAwaitingMerge(context.Background())

	if tracker.stateFetchCalls == 0 {
		t.Fatal("tracker state refresh was not attempted")
	}
	if _, ok := o.state.AwaitingMerge["1"]; ok {
		t.Fatal("awaiting merge entry still exists")
	}
	if _, ok := o.state.Claimed["1"]; ok {
		t.Fatal("claimed entry still exists")
	}
	if len(workspace.cleaned) != 1 || workspace.cleaned[0] != "ABC-1" {
		t.Fatalf("cleanup calls = %+v, want [ABC-1]", workspace.cleaned)
	}
	if len(o.state.RetryAttempts) != 0 {
		t.Fatalf("retry attempts = %+v, want none", o.state.RetryAttempts)
	}
}

func TestReconcileAwaitingMergeNonActiveIssueReleasesClaimWithoutCleanup(t *testing.T) {
	now := time.Date(2026, 3, 7, 10, 0, 0, 0, time.UTC)
	cfg := &model.ServiceConfig{
		ActiveStates:        []string{"Todo", "In Progress"},
		TerminalStates:      []string{"Done"},
		MaxConcurrentAgents: 1,
	}
	tracker := &fakeTracker{
		stateByID: map[string]model.Issue{
			"1": {ID: "1", Identifier: "ABC-1", State: "Backlog"},
		},
	}
	workspace := &fakeWorkspaceManager{}
	o := newTestOrchestrator(cfg, tracker, workspace, &fakeRunner{}, now)
	branch := "iiwate4268/iiwate-48-await"
	o.prLookup = &fakePRLookup{
		byBranch: map[string]*PullRequestInfo{
			branch: {Number: 48, URL: "https://example.test/pr/48", HeadBranch: branch, State: PullRequestStateOpen},
		},
	}
	o.state.AwaitingMerge["1"] = &model.AwaitingMergeEntry{
		Identifier:    "ABC-1",
		State:         "In Progress",
		WorkspacePath: "C:/work/ABC-1",
		Branch:        branch,
		RetryAttempt:  0,
		AwaitingSince: now.Add(-time.Minute),
	}
	o.state.Claimed["1"] = struct{}{}

	o.reconcileAwaitingMerge(context.Background())

	if tracker.stateFetchCalls == 0 {
		t.Fatal("tracker state refresh was not attempted")
	}
	if _, ok := o.state.AwaitingMerge["1"]; ok {
		t.Fatal("awaiting merge entry still exists")
	}
	if _, ok := o.state.Claimed["1"]; ok {
		t.Fatal("claimed entry still exists")
	}
	if len(workspace.cleaned) != 0 {
		t.Fatalf("cleanup calls = %+v, want none", workspace.cleaned)
	}
	if len(o.state.RetryAttempts) != 0 {
		t.Fatalf("retry attempts = %+v, want none", o.state.RetryAttempts)
	}
}

func TestReconcileAwaitingMergeClosedMovesToAwaitingIntervention(t *testing.T) {
	now := time.Date(2026, 3, 7, 10, 0, 0, 0, time.UTC)
	cfg := &model.ServiceConfig{
		ActiveStates:        []string{"Todo", "In Progress"},
		TerminalStates:      []string{"Done"},
		MaxConcurrentAgents: 1,
	}
	o := newTestOrchestrator(cfg, &fakeTracker{}, &fakeWorkspaceManager{}, &fakeRunner{}, now)
	branch := "iiwate4268/iiwate-48-await"
	o.prLookup = &fakePRLookup{
		byBranch: map[string]*PullRequestInfo{
			branch: {Number: 48, URL: "https://example.test/pr/48", HeadBranch: branch, State: PullRequestStateClosed},
		},
	}
	o.state.AwaitingMerge["1"] = &model.AwaitingMergeEntry{
		Identifier:    "ABC-1",
		State:         "In Progress",
		WorkspacePath: "C:/work/ABC-1",
		Branch:        branch,
		RetryAttempt:  0,
		StallCount:    2,
		AwaitingSince: now.Add(-time.Minute),
	}
	o.state.Claimed["1"] = struct{}{}

	o.reconcileAwaitingMerge(context.Background())

	if _, ok := o.state.AwaitingMerge["1"]; ok {
		t.Fatal("awaiting merge entry still exists")
	}
	awaiting := o.state.AwaitingIntervention["1"]
	if awaiting == nil {
		t.Fatal("awaiting intervention entry missing")
	}
	if awaiting.PRState != string(PullRequestStateClosed) {
		t.Fatalf("awaiting intervention entry = %+v", awaiting)
	}
	if len(o.state.RetryAttempts) != 0 {
		t.Fatalf("retry attempts = %+v, want none", o.state.RetryAttempts)
	}
	if _, ok := o.state.Claimed["1"]; !ok {
		t.Fatal("claimed entry should be retained while awaiting intervention")
	}
}

func TestReconcileAwaitingMergeLookupFailureKeepsAwaitingAndAlert(t *testing.T) {
	now := time.Date(2026, 3, 7, 10, 0, 0, 0, time.UTC)
	cfg := &model.ServiceConfig{
		ActiveStates:        []string{"Todo", "In Progress"},
		TerminalStates:      []string{"Done"},
		MaxConcurrentAgents: 1,
	}
	o := newTestOrchestrator(cfg, &fakeTracker{}, &fakeWorkspaceManager{}, &fakeRunner{}, now)
	branch := "iiwate4268/iiwate-48-await"
	o.prLookup = &fakePRLookup{
		errByBranch: map[string]error{
			branch: errors.New("gh unavailable"),
		},
	}
	o.state.AwaitingMerge["1"] = &model.AwaitingMergeEntry{
		Identifier:    "ABC-1",
		State:         "In Progress",
		WorkspacePath: "C:/work/ABC-1",
		Branch:        branch,
		RetryAttempt:  0,
		AwaitingSince: now.Add(-time.Minute),
	}
	o.state.Claimed["1"] = struct{}{}

	o.reconcileAwaitingMerge(context.Background())

	awaiting := o.state.AwaitingMerge["1"]
	if awaiting == nil {
		t.Fatal("awaiting merge entry missing")
	}
	if awaiting.LastError == nil || *awaiting.LastError != "gh unavailable" {
		t.Fatalf("awaiting merge error = %v, want gh unavailable", awaiting.LastError)
	}
	if len(o.state.RetryAttempts) != 0 {
		t.Fatalf("retry attempts = %+v, want none", o.state.RetryAttempts)
	}
	if !hasAlertCode(o.Snapshot().Alerts, "merge_status_unknown") {
		t.Fatalf("snapshot alerts = %+v, want merge_status_unknown", o.Snapshot().Alerts)
	}
}

func TestReconcileAwaitingMergeLookupFailureDuringContextCancelIsIgnored(t *testing.T) {
	now := time.Date(2026, 3, 7, 10, 0, 0, 0, time.UTC)
	cfg := &model.ServiceConfig{
		ActiveStates:        []string{"Todo", "In Progress"},
		TerminalStates:      []string{"Done"},
		MaxConcurrentAgents: 1,
	}
	o := newTestOrchestrator(cfg, &fakeTracker{}, &fakeWorkspaceManager{}, &fakeRunner{}, now)
	branch := "iiwate4268/iiwate-48-await"
	o.prLookup = &fakePRLookup{
		errByBranch: map[string]error{
			branch: errors.New("gh unavailable"),
		},
	}
	o.state.AwaitingMerge["1"] = &model.AwaitingMergeEntry{
		Identifier:    "ABC-1",
		State:         "In Progress",
		WorkspacePath: "C:/work/ABC-1",
		Branch:        branch,
		RetryAttempt:  0,
		AwaitingSince: now.Add(-time.Minute),
	}
	o.state.Claimed["1"] = struct{}{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	o.reconcileAwaitingMerge(ctx)

	awaiting := o.state.AwaitingMerge["1"]
	if awaiting == nil {
		t.Fatal("awaiting merge entry missing")
	}
	if awaiting.LastError != nil {
		t.Fatalf("awaiting merge error = %v, want nil during shutdown", awaiting.LastError)
	}
	if hasAlertCode(o.Snapshot().Alerts, "merge_status_unknown") {
		t.Fatalf("snapshot alerts = %+v, want no merge_status_unknown during shutdown", o.Snapshot().Alerts)
	}
}

func TestReconcileAwaitingMergeMissingPRMovesToAwaitingIntervention(t *testing.T) {
	now := time.Date(2026, 3, 7, 10, 0, 0, 0, time.UTC)
	cfg := &model.ServiceConfig{
		ActiveStates:        []string{"Todo", "In Progress"},
		TerminalStates:      []string{"Done"},
		MaxConcurrentAgents: 1,
	}
	o := newTestOrchestrator(cfg, &fakeTracker{}, &fakeWorkspaceManager{}, &fakeRunner{}, now)
	branch := "iiwate4268/iiwate-48-await"
	o.prLookup = &fakePRLookup{byBranch: map[string]*PullRequestInfo{}}
	o.state.AwaitingMerge["1"] = &model.AwaitingMergeEntry{
		Identifier:    "ABC-1",
		State:         "In Progress",
		WorkspacePath: "C:/work/ABC-1",
		Branch:        branch,
		RetryAttempt:  0,
		StallCount:    2,
		AwaitingSince: now.Add(-time.Minute),
	}
	o.state.Claimed["1"] = struct{}{}

	o.reconcileAwaitingMerge(context.Background())

	if _, ok := o.state.AwaitingMerge["1"]; ok {
		t.Fatal("awaiting merge entry still exists")
	}
	awaiting := o.state.AwaitingIntervention["1"]
	if awaiting == nil {
		t.Fatal("awaiting intervention entry missing")
	}
	if awaiting.Reason != string(model.ContinuationReasonMissingPR) {
		t.Fatalf("awaiting intervention entry = %+v, want missing_pr reason", awaiting)
	}
	if len(o.state.RetryAttempts) != 0 {
		t.Fatalf("retry attempts = %+v, want none", o.state.RetryAttempts)
	}
}

func TestIsDispatchEligibleRejectsAwaitingMerge(t *testing.T) {
	now := time.Date(2026, 3, 7, 10, 0, 0, 0, time.UTC)
	cfg := &model.ServiceConfig{
		ActiveStates:        []string{"Todo", "In Progress"},
		TerminalStates:      []string{"Done"},
		MaxConcurrentAgents: 1,
	}
	o := newTestOrchestrator(cfg, &fakeTracker{}, &fakeWorkspaceManager{}, &fakeRunner{}, now)
	o.state.AwaitingMerge["1"] = &model.AwaitingMergeEntry{
		Identifier:    "ABC-1",
		State:         "In Progress",
		WorkspacePath: "C:/work/ABC-1",
		Branch:        "iiwate4268/iiwate-48-await",
		AwaitingSince: now.Add(-time.Minute),
	}

	eligible := o.isDispatchEligible(model.Issue{
		ID:         "1",
		Identifier: "ABC-1",
		Title:      "Awaiting merge",
		State:      "In Progress",
	}, cfg, false)
	if eligible {
		t.Fatal("awaiting-merge issue should not be dispatch eligible")
	}
}

func TestIsDispatchEligibleRejectsAwaitingIntervention(t *testing.T) {
	now := time.Date(2026, 3, 7, 10, 0, 0, 0, time.UTC)
	cfg := &model.ServiceConfig{
		ActiveStates:        []string{"Todo", "In Progress"},
		TerminalStates:      []string{"Done"},
		MaxConcurrentAgents: 1,
	}
	o := newTestOrchestrator(cfg, &fakeTracker{}, &fakeWorkspaceManager{}, &fakeRunner{}, now)
	o.state.AwaitingIntervention["1"] = &model.AwaitingInterventionEntry{
		Identifier:    "ABC-1",
		WorkspacePath: "C:/work/ABC-1",
		Branch:        "iiwate4268/iiwate-48-await",
		PRNumber:      48,
		PRURL:         "https://example.test/pr/48",
		PRState:       string(PullRequestStateClosed),
		ObservedAt:    now.Add(-time.Minute),
	}

	eligible := o.isDispatchEligible(model.Issue{
		ID:         "1",
		Identifier: "ABC-1",
		Title:      "Awaiting intervention",
		State:      "In Progress",
	}, cfg, false)
	if eligible {
		t.Fatal("awaiting-intervention issue should not be dispatch eligible")
	}
}

func TestReconcileAwaitingInterventionReleasesInactiveIssue(t *testing.T) {
	now := time.Date(2026, 3, 7, 10, 0, 0, 0, time.UTC)
	cfg := &model.ServiceConfig{
		ActiveStates:        []string{"Todo", "In Progress"},
		TerminalStates:      []string{"Done"},
		MaxConcurrentAgents: 1,
	}
	tracker := &fakeTracker{
		stateByID: map[string]model.Issue{
			"1": {ID: "1", Identifier: "ABC-1", State: "Backlog"},
		},
	}
	workspace := &fakeWorkspaceManager{}
	o := newTestOrchestrator(cfg, tracker, workspace, &fakeRunner{}, now)
	o.state.AwaitingIntervention["1"] = &model.AwaitingInterventionEntry{
		Identifier:    "ABC-1",
		WorkspacePath: "C:/work/ABC-1",
		Branch:        "iiwate4268/iiwate-48-await",
		PRNumber:      48,
		PRURL:         "https://example.test/pr/48",
		PRState:       string(PullRequestStateClosed),
		RetryAttempt:  1,
		ObservedAt:    now.Add(-time.Minute),
	}
	o.state.Claimed["1"] = struct{}{}

	o.reconcileAwaitingIntervention(context.Background())

	if tracker.stateFetchCalls == 0 {
		t.Fatal("tracker state refresh was not attempted")
	}
	if _, ok := o.state.AwaitingIntervention["1"]; ok {
		t.Fatal("awaiting intervention entry still exists")
	}
	if _, ok := o.state.Claimed["1"]; ok {
		t.Fatal("claimed entry still exists")
	}
	if len(workspace.cleaned) != 0 {
		t.Fatalf("cleanup calls = %+v, want none", workspace.cleaned)
	}
	if len(o.state.RetryAttempts) != 0 {
		t.Fatalf("retry attempts = %+v, want none", o.state.RetryAttempts)
	}
}

func TestRememberCompletedLockedCapsCompletedEntries(t *testing.T) {
	now := time.Date(2026, 3, 7, 10, 0, 0, 0, time.UTC)
	cfg := &model.ServiceConfig{
		ActiveStates:        []string{"Todo", "In Progress"},
		TerminalStates:      []string{"Done"},
		MaxConcurrentAgents: 1,
	}
	o := newTestOrchestrator(cfg, &fakeTracker{}, &fakeWorkspaceManager{}, &fakeRunner{}, now)
	o.maxCompleted = 2

	o.mu.Lock()
	o.rememberCompletedLocked("1")
	o.rememberCompletedLocked("2")
	o.rememberCompletedLocked("3")
	o.mu.Unlock()

	if _, ok := o.state.Completed["1"]; ok {
		t.Fatalf("completed entries = %+v, want oldest evicted", o.state.Completed)
	}
	if _, ok := o.state.Completed["2"]; !ok {
		t.Fatalf("completed entries = %+v, want issue 2 retained", o.state.Completed)
	}
	if _, ok := o.state.Completed["3"]; !ok {
		t.Fatalf("completed entries = %+v, want issue 3 retained", o.state.Completed)
	}
}

func TestHandleCodexUpdateTurnCountIncrementsOnTurnChangeOnly(t *testing.T) {
	now := time.Date(2026, 3, 7, 10, 0, 0, 0, time.UTC)
	cfg := &model.ServiceConfig{
		ActiveStates:        []string{"Todo", "In Progress"},
		TerminalStates:      []string{"Done"},
		MaxConcurrentAgents: 1,
	}
	o := newTestOrchestrator(cfg, &fakeTracker{}, &fakeWorkspaceManager{}, &fakeRunner{}, now)
	o.state.Running["1"] = &model.RunningEntry{
		Issue:      &model.Issue{ID: "1", Identifier: "ABC-1", State: "Todo"},
		Identifier: "ABC-1",
		StartedAt:  now,
	}

	o.handleCodexUpdate(CodexUpdate{IssueID: "1", Event: agent.AgentEvent{Event: "notification", Timestamp: now, TurnID: stringPtr("turn-1")}})
	o.handleCodexUpdate(CodexUpdate{IssueID: "1", Event: agent.AgentEvent{Event: "notification", Timestamp: now.Add(time.Second), TurnID: stringPtr("turn-1")}})
	o.handleCodexUpdate(CodexUpdate{IssueID: "1", Event: agent.AgentEvent{Event: "notification", Timestamp: now.Add(2 * time.Second), TurnID: stringPtr("turn-2")}})

	entry := o.state.Running["1"]
	if entry.Session.TurnCount != 2 {
		t.Fatalf("turn count = %d, want 2", entry.Session.TurnCount)
	}
}

func newTestOrchestrator(cfg *model.ServiceConfig, trackerClient tracker.Client, workspaceManager *fakeWorkspaceManager, runner *fakeRunner, now time.Time) *Orchestrator {
	o := NewOrchestrator(trackerClient, workspaceManager, runner, func() *model.ServiceConfig {
		return cfg
	}, func() *model.WorkflowDefinition {
		return &model.WorkflowDefinition{PromptTemplate: "Issue {{ issue.identifier }}"}
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	o.now = func() time.Time { return now }
	o.startedAt = now
	o.serviceVersion = "test"
	o.gitBranchFn = func(context.Context, string) (string, error) { return "test/branch", nil }
	o.prLookup = &fakePRLookup{byBranch: map[string]*PullRequestInfo{}}
	return o
}

type fakeTracker struct {
	candidateIssues     []model.Issue
	stateByID           map[string]model.Issue
	terminalIssues      []model.Issue
	candidateErr        error
	stateErr            error
	terminalErr         error
	transitionErr       error
	onTransition        func(issueID string, targetState string)
	candidateFetchCalls int
	stateFetchCalls     int
	transitionCalls     int
	transitionIssueID   string
	transitionTarget    string
}

type fakeTrackerClientOnly struct {
	stateByID map[string]model.Issue
}

func (f *fakeTracker) FetchCandidateIssues(context.Context) ([]model.Issue, error) {
	f.candidateFetchCalls++
	if f.candidateErr != nil {
		return nil, f.candidateErr
	}
	return append([]model.Issue(nil), f.candidateIssues...), nil
}

func (f *fakeTrackerClientOnly) FetchCandidateIssues(context.Context) ([]model.Issue, error) {
	return nil, nil
}

func (f *fakeTrackerClientOnly) FetchIssuesByStates(context.Context, []string) ([]model.Issue, error) {
	return nil, nil
}

func (f *fakeTrackerClientOnly) FetchIssueStatesByIDs(_ context.Context, ids []string) ([]model.Issue, error) {
	items := make([]model.Issue, 0, len(ids))
	for _, id := range ids {
		if issue, ok := f.stateByID[id]; ok {
			items = append(items, issue)
		}
	}
	return items, nil
}

func (f *fakeTracker) FetchIssuesByStates(_ context.Context, _ []string) ([]model.Issue, error) {
	if f.terminalErr != nil {
		return nil, f.terminalErr
	}
	return append([]model.Issue(nil), f.terminalIssues...), nil
}

func (f *fakeTracker) FetchIssueStatesByIDs(_ context.Context, ids []string) ([]model.Issue, error) {
	f.stateFetchCalls++
	if f.stateErr != nil {
		return nil, f.stateErr
	}
	result := make([]model.Issue, 0, len(ids))
	for _, id := range ids {
		if issue, ok := f.stateByID[id]; ok {
			result = append(result, issue)
		}
	}
	return result, nil
}

func (f *fakeTracker) TransitionIssue(_ context.Context, issueID string, targetState string) error {
	f.transitionCalls++
	f.transitionIssueID = issueID
	f.transitionTarget = targetState
	if f.transitionErr != nil {
		return f.transitionErr
	}
	if f.onTransition != nil {
		f.onTransition(issueID, targetState)
	}
	return nil
}

type fakeWorkspaceManager struct {
	cleaned []string
}

func (f *fakeWorkspaceManager) CreateForIssue(_ context.Context, identifier string) (*model.Workspace, error) {
	return &model.Workspace{Path: "/tmp/" + identifier, WorkspaceKey: identifier, CreatedNow: true}, nil
}

func (f *fakeWorkspaceManager) CleanupWorkspace(_ context.Context, identifier string) error {
	f.cleaned = append(f.cleaned, identifier)
	return nil
}

func (f *fakeWorkspaceManager) PrepareForRun(context.Context, *model.Workspace) error { return nil }
func (f *fakeWorkspaceManager) FinalizeRun(context.Context, *model.Workspace)         {}

type fakeRunner struct {
	runCh chan string
	block chan struct{}
	err   error
}

type fakePRLookup struct {
	byBranch    map[string]*PullRequestInfo
	errByBranch map[string]error
	byNumber    map[int]*PullRequestInfo
	errByNumber map[int]error
}

func (f *fakeRunner) Run(ctx context.Context, params agent.RunParams) error {
	if f.runCh != nil {
		f.runCh <- params.Issue.Identifier
	}
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return f.err
}

func (f *fakePRLookup) FindByHeadBranch(_ context.Context, _ string, headBranch string) (*PullRequestInfo, error) {
	if f.errByBranch != nil {
		if err := f.errByBranch[headBranch]; err != nil {
			return nil, err
		}
	}
	if f.byBranch == nil {
		return nil, nil
	}
	pr := f.byBranch[headBranch]
	if pr == nil {
		return nil, nil
	}
	copyPR := *pr
	return &copyPR, nil
}

func (f *fakePRLookup) Refresh(_ context.Context, _ string, pr *PullRequestInfo) (*PullRequestInfo, error) {
	if pr == nil {
		return nil, nil
	}
	if f.errByNumber != nil {
		if err := f.errByNumber[pr.Number]; err != nil {
			return nil, err
		}
	}
	if f.byNumber != nil {
		if refreshed := f.byNumber[pr.Number]; refreshed != nil {
			copyPR := *refreshed
			return &copyPR, nil
		}
		return nil, nil
	}
	return f.FindByHeadBranch(context.Background(), "", pr.HeadBranch)
}

func stringPtr(value string) *string {
	copyValue := value
	return &copyValue
}

func pullRequestDispatch() *model.DispatchContext {
	return &model.DispatchContext{
		Kind:            model.DispatchKindFresh,
		ExpectedOutcome: model.CompletionModePullRequest,
		OnMissingPR:     model.CompletionActionIntervention,
		OnClosedPR:      model.CompletionActionIntervention,
	}
}

func hasAlertCode(alerts []AlertSnapshot, code string) bool {
	for _, alert := range alerts {
		if alert.Code == code {
			return true
		}
	}
	return false
}
