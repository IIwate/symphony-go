package orchestrator

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"symphony-go/internal/agent"
	"symphony-go/internal/model"
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
	if len(workspace.cleaned) != 1 || workspace.cleaned[0] != "ABC-1" {
		t.Fatalf("cleanup calls = %+v", workspace.cleaned)
	}
}

func TestHandleCodexUpdateAggregatesUsage(t *testing.T) {
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

func newTestOrchestrator(cfg *model.ServiceConfig, trackerClient *fakeTracker, workspaceManager *fakeWorkspaceManager, runner *fakeRunner, now time.Time) *Orchestrator {
	o := NewOrchestrator(trackerClient, workspaceManager, runner, func() *model.ServiceConfig {
		return cfg
	}, func() *model.WorkflowDefinition {
		return &model.WorkflowDefinition{PromptTemplate: "Issue {{ issue.identifier }}"}
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	o.now = func() time.Time { return now }
	return o
}

type fakeTracker struct {
	candidateIssues []model.Issue
	stateByID       map[string]model.Issue
	terminalIssues  []model.Issue
	candidateErr    error
	stateErr        error
	terminalErr     error
}

func (f *fakeTracker) FetchCandidateIssues(context.Context) ([]model.Issue, error) {
	if f.candidateErr != nil {
		return nil, f.candidateErr
	}
	return append([]model.Issue(nil), f.candidateIssues...), nil
}

func (f *fakeTracker) FetchIssuesByStates(_ context.Context, _ []string) ([]model.Issue, error) {
	if f.terminalErr != nil {
		return nil, f.terminalErr
	}
	return append([]model.Issue(nil), f.terminalIssues...), nil
}

func (f *fakeTracker) FetchIssueStatesByIDs(_ context.Context, ids []string) ([]model.Issue, error) {
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

func stringPtr(value string) *string {
	copyValue := value
	return &copyValue
}
