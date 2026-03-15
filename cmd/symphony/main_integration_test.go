package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"symphony-go/internal/agent"
	"symphony-go/internal/model"
	"symphony-go/internal/model/contract"
	"symphony-go/internal/orchestrator"
	"symphony-go/internal/server"
	"symphony-go/internal/tracker"
	"symphony-go/internal/workspace"
)

func TestMainIntegration_RunCommandServesFormalHTTPAPI(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "secret-key")

	restore := stubDependencies(t)
	defer restore()

	configDir := filepath.Join(t.TempDir(), "automation")
	writeAutomationConfig(t, configDir, automationFixtureOptions{})
	writeRunProjectConfig(t, configDir, "", 0)

	initial := contract.ServiceStateSnapshot{
		GeneratedAt:        "2026-03-14T00:00:00Z",
		ServiceMode:        contract.ServiceModeServing,
		RecoveryInProgress: false,
		Reasons:            []contract.Reason{},
		Counts: contract.StateCounts{
			Total:  1,
			Active: 1,
		},
		Records: []contract.IssueRuntimeRecord{
			{
				RecordID:  "rec_github_issues_github-main_1",
				SourceRef: contract.SourceRef{SourceKind: contract.SourceKindGitHubIssues, SourceName: "github-main", SourceID: "1", SourceIdentifier: "GH-1"},
				Status:    contract.IssueStatusActive,
				UpdatedAt: "2026-03-14T00:00:00Z",
				Observation: &contract.Observation{
					Running: true,
					Summary: "agent run in progress",
					Details: map[string]any{"runner": "codex"},
				},
				DurableRefs: contract.DurableRefs{
					LedgerPath: "automation/local/runtime-ledger.json",
				},
			},
		},
		CompletedWindow: contract.CompletedWindow{
			Limit:   100,
			Records: []contract.IssueRuntimeRecord{},
		},
	}
	updated := initial
	updated.GeneratedAt = "2026-03-14T00:00:01Z"
	updated.ServiceMode = contract.ServiceModeDegraded
	updated.Reasons = []contract.Reason{contract.MustReason(contract.ReasonServiceDegradedNotificationDelivery, map[string]any{
		"channel_ids": []string{"ops"},
	})}
	updated.Counts = contract.StateCounts{
		Total:         1,
		AwaitingMerge: 1,
	}
	updated.Records = []contract.IssueRuntimeRecord{
		{
			RecordID:  "rec_github_issues_github-main_1",
			SourceRef: contract.SourceRef{SourceKind: contract.SourceKindGitHubIssues, SourceName: "github-main", SourceID: "1", SourceIdentifier: "GH-1"},
			Status:    contract.IssueStatusAwaitingMerge,
			UpdatedAt: "2026-03-14T00:00:01Z",
			Reason: func() *contract.Reason {
				reason := contract.MustReason(contract.ReasonRecordBlockedAwaitingMerge, map[string]any{
					"record_id": "rec_github_issues_github-main_1",
				})
				return &reason
			}(),
			Observation: &contract.Observation{
				Running: false,
				Summary: "waiting for merge",
				Details: map[string]any{"runner": "codex"},
			},
			DurableRefs: contract.DurableRefs{
				Branch:     &contract.BranchRef{Name: "feature/abc-1"},
				LedgerPath: "automation/local/runtime-ledger.json",
			},
		},
	}

	discovery := contract.DiscoveryDocument{
		APIVersion: contract.APIVersionV1,
		Instance: contract.InstanceDocument{
			ID:      "automation",
			Name:    "symphony",
			Version: "dev",
		},
		Source: contract.SourceDocument{
			Kind: contract.SourceKindGitHubIssues,
			Name: "github-main",
		},
		ServiceMode:        contract.ServiceModeServing,
		RecoveryInProgress: false,
		Capabilities: contract.CapabilityDocument{
			EventProtocol:  "sse",
			ControlActions: []contract.ControlAction{contract.ControlActionRefresh},
			Notifications:  []string{"webhook"},
			Sources:        []contract.SourceKind{contract.SourceKindGitHubIssues},
		},
		Reasons: []contract.Reason{},
		Limits: contract.LimitDocument{
			CompletedWindowSize: 100,
		},
	}

	signalCtx, signalCancel := context.WithCancel(context.Background())
	defer signalCancel()

	fake := &fakeOrchestrator{
		discovery: discovery,
		snapshot:  initial,
		wait: func() {
			<-signalCtx.Done()
		},
	}
	fake.requestRefresh = func() orchestrator.RefreshRequestResult {
		reason := contract.MustReason(contract.ReasonControlRefreshAccepted, map[string]any{
			"service_mode": contract.ServiceModeServing,
		})
		fake.publish(updated)
		return contract.ControlResult{
			Action:              contract.ControlActionRefresh,
			Status:              contract.ControlStatusAccepted,
			Reason:              &reason,
			RecommendedNextStep: "等待 SSE 通知后回读 /api/v1/state",
			Timestamp:           "2026-03-14T00:00:01Z",
		}
	}

	serverStarted := make(chan struct{})
	serverAddr := ""
	newOrchestratorFactory = func(_ tracker.Client, _ workspace.Manager, _ agent.Runner, _ func() *model.ServiceConfig, _ func() *model.WorkflowDefinition, _ func() orchestrator.RuntimeIdentity, _ *slog.Logger) orchestratorService {
		return fake
	}
	newHTTPServerFactory = func(runtime orchestratorService, logger *slog.Logger, host string, port int) (httpServer, error) {
		httpSrv, err := server.Start(runtime, logger, host, port)
		if err == nil {
			serverAddr = httpSrv.Addr()
			close(serverStarted)
		}
		return httpSrv, err
	}
	watchAutomationDefinition = func(context.Context, string, string, func(*model.AutomationDefinition) error, func(error)) error {
		return nil
	}
	notifySignalContext = func(parent context.Context, _ ...os.Signal) (context.Context, context.CancelFunc) {
		return signalCtx, func() {}
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- execute([]string{"run", "--config-dir", configDir}, io.Discard, io.Discard)
	}()

	select {
	case <-serverStarted:
	case err := <-errCh:
		t.Fatalf("execute() returned before server start: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("http server did not start")
	}

	baseURL := "http://" + serverAddr
	discoveryResp := fetchJSON[contract.DiscoveryDocument](t, http.MethodGet, baseURL+"/api/v1/discovery", nil)
	if discoveryResp.Instance.Name != "symphony" {
		t.Fatalf("discovery instance.name = %q, want symphony", discoveryResp.Instance.Name)
	}

	stateResp := fetchJSON[contract.ServiceStateSnapshot](t, http.MethodGet, baseURL+"/api/v1/state", nil)
	if stateResp.ServiceMode != contract.ServiceModeServing || stateResp.Counts.Active != 1 {
		t.Fatalf("initial state = %#v", stateResp)
	}

	eventsResp, err := http.Get(baseURL + "/api/v1/events")
	if err != nil {
		t.Fatalf("GET /api/v1/events error = %v", err)
	}
	reader := bufio.NewReader(eventsResp.Body)
	firstEvent := readMainSSEEvent(t, reader)
	if firstEvent.Event != string(contract.EventTypeSnapshot) {
		t.Fatalf("first event = %q, want %q", firstEvent.Event, contract.EventTypeSnapshot)
	}

	controlResp := fetchJSON[contract.ControlResult](t, http.MethodPost, baseURL+"/api/v1/control/refresh", strings.NewReader("{}"))
	if controlResp.Status != contract.ControlStatusAccepted {
		t.Fatalf("refresh status = %q, want %q", controlResp.Status, contract.ControlStatusAccepted)
	}

	secondEvent := readMainSSEEvent(t, reader)
	if secondEvent.Event != string(contract.EventTypeStateChanged) {
		t.Fatalf("second event = %q, want %q", secondEvent.Event, contract.EventTypeStateChanged)
	}
	var envelope contract.EventEnvelope
	if err := json.Unmarshal([]byte(secondEvent.Data), &envelope); err != nil {
		t.Fatalf("Unmarshal(second event) error = %v", err)
	}
	if envelope.ServiceMode != contract.ServiceModeDegraded {
		t.Fatalf("event service_mode = %q, want %q", envelope.ServiceMode, contract.ServiceModeDegraded)
	}

	updatedState := fetchJSON[contract.ServiceStateSnapshot](t, http.MethodGet, baseURL+"/api/v1/state", nil)
	if updatedState.ServiceMode != contract.ServiceModeDegraded {
		t.Fatalf("updated service_mode = %q, want %q", updatedState.ServiceMode, contract.ServiceModeDegraded)
	}
	if len(updatedState.Records) != 1 || updatedState.Records[0].Status != contract.IssueStatusAwaitingMerge {
		t.Fatalf("updated records = %#v", updatedState.Records)
	}

	_ = eventsResp.Body.Close()
	signalCancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("execute() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run command did not exit")
	}
}

func TestMainIntegration_RunCommandExposesUnavailableServiceMode(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "secret-key")

	restore := stubDependencies(t)
	defer restore()

	configDir := filepath.Join(t.TempDir(), "automation")
	writeAutomationConfig(t, configDir, automationFixtureOptions{})
	writeRunProjectConfig(t, configDir, "", 0)

	serviceReason := contract.MustReason(contract.ReasonServiceUnavailableCoreDependency, map[string]any{
		"component":   "ledger_store",
		"source_kind": contract.SourceKindGitHubIssues,
		"source_name": "github-main",
		"detail":      "disk full",
	})
	discovery := contract.DiscoveryDocument{
		APIVersion: contract.APIVersionV1,
		Instance: contract.InstanceDocument{
			ID:      "automation",
			Name:    "symphony",
			Version: "dev",
		},
		Source: contract.SourceDocument{
			Kind: contract.SourceKindGitHubIssues,
			Name: "github-main",
		},
		ServiceMode:        contract.ServiceModeUnavailable,
		RecoveryInProgress: false,
		Capabilities: contract.CapabilityDocument{
			EventProtocol:  "sse",
			ControlActions: []contract.ControlAction{contract.ControlActionRefresh},
			Notifications:  []string{"webhook"},
			Sources:        []contract.SourceKind{contract.SourceKindGitHubIssues},
		},
		Reasons: []contract.Reason{serviceReason},
		Limits: contract.LimitDocument{
			CompletedWindowSize: 100,
		},
	}
	snapshot := contract.ServiceStateSnapshot{
		GeneratedAt:        "2026-03-14T00:00:00Z",
		ServiceMode:        contract.ServiceModeUnavailable,
		RecoveryInProgress: false,
		Reasons:            []contract.Reason{serviceReason},
		Counts:             contract.StateCounts{},
		Records:            []contract.IssueRuntimeRecord{},
		CompletedWindow: contract.CompletedWindow{
			Limit:   100,
			Records: []contract.IssueRuntimeRecord{},
		},
	}

	signalCtx, signalCancel := context.WithCancel(context.Background())
	defer signalCancel()

	fake := &fakeOrchestrator{
		discovery: discovery,
		snapshot:  snapshot,
		wait: func() {
			<-signalCtx.Done()
		},
	}
	fake.requestRefresh = func() orchestrator.RefreshRequestResult {
		reason := contract.MustReason(contract.ReasonControlRefreshRejectedServiceMode, map[string]any{
			"service_mode": contract.ServiceModeUnavailable,
		})
		return contract.ControlResult{
			Action:              contract.ControlActionRefresh,
			Status:              contract.ControlStatusRejected,
			Reason:              &reason,
			RecommendedNextStep: "检查核心依赖后重试",
			Timestamp:           "2026-03-14T00:00:01Z",
		}
	}

	serverStarted := make(chan struct{})
	serverAddr := ""
	newOrchestratorFactory = func(_ tracker.Client, _ workspace.Manager, _ agent.Runner, _ func() *model.ServiceConfig, _ func() *model.WorkflowDefinition, _ func() orchestrator.RuntimeIdentity, _ *slog.Logger) orchestratorService {
		return fake
	}
	newHTTPServerFactory = func(runtime orchestratorService, logger *slog.Logger, host string, port int) (httpServer, error) {
		httpSrv, err := server.Start(runtime, logger, host, port)
		if err == nil {
			serverAddr = httpSrv.Addr()
			close(serverStarted)
		}
		return httpSrv, err
	}
	watchAutomationDefinition = func(context.Context, string, string, func(*model.AutomationDefinition) error, func(error)) error {
		return nil
	}
	notifySignalContext = func(parent context.Context, _ ...os.Signal) (context.Context, context.CancelFunc) {
		return signalCtx, func() {}
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- execute([]string{"run", "--config-dir", configDir}, io.Discard, io.Discard)
	}()

	select {
	case <-serverStarted:
	case err := <-errCh:
		t.Fatalf("execute() returned before server start: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("http server did not start")
	}

	baseURL := "http://" + serverAddr
	discoveryResp := fetchJSON[contract.DiscoveryDocument](t, http.MethodGet, baseURL+"/api/v1/discovery", nil)
	if discoveryResp.ServiceMode != contract.ServiceModeUnavailable {
		t.Fatalf("discovery service_mode = %q, want %q", discoveryResp.ServiceMode, contract.ServiceModeUnavailable)
	}
	if len(discoveryResp.Reasons) != 1 || discoveryResp.Reasons[0].ReasonCode != contract.ReasonServiceUnavailableCoreDependency {
		t.Fatalf("discovery reasons = %#v, want %q", discoveryResp.Reasons, contract.ReasonServiceUnavailableCoreDependency)
	}

	stateResp := fetchJSON[contract.ServiceStateSnapshot](t, http.MethodGet, baseURL+"/api/v1/state", nil)
	if stateResp.ServiceMode != contract.ServiceModeUnavailable {
		t.Fatalf("state service_mode = %q, want %q", stateResp.ServiceMode, contract.ServiceModeUnavailable)
	}
	if len(stateResp.Reasons) != 1 || stateResp.Reasons[0].ReasonCode != contract.ReasonServiceUnavailableCoreDependency {
		t.Fatalf("state reasons = %#v, want %q", stateResp.Reasons, contract.ReasonServiceUnavailableCoreDependency)
	}

	eventsResp, err := http.Get(baseURL + "/api/v1/events")
	if err != nil {
		t.Fatalf("GET /api/v1/events error = %v", err)
	}
	reader := bufio.NewReader(eventsResp.Body)
	firstEvent := readMainSSEEvent(t, reader)
	if firstEvent.Event != string(contract.EventTypeSnapshot) {
		t.Fatalf("first event = %q, want %q", firstEvent.Event, contract.EventTypeSnapshot)
	}
	var envelope contract.EventEnvelope
	if err := json.Unmarshal([]byte(firstEvent.Data), &envelope); err != nil {
		t.Fatalf("Unmarshal(snapshot event) error = %v", err)
	}
	if envelope.ServiceMode != contract.ServiceModeUnavailable {
		t.Fatalf("snapshot service_mode = %q, want %q", envelope.ServiceMode, contract.ServiceModeUnavailable)
	}

	controlResp := fetchJSON[contract.ControlResult](t, http.MethodPost, baseURL+"/api/v1/control/refresh", strings.NewReader("{}"))
	if controlResp.Status != contract.ControlStatusRejected {
		t.Fatalf("refresh status = %q, want %q", controlResp.Status, contract.ControlStatusRejected)
	}
	if controlResp.Reason == nil || controlResp.Reason.ReasonCode != contract.ReasonControlRefreshRejectedServiceMode {
		t.Fatalf("refresh reason = %#v, want %q", controlResp.Reason, contract.ReasonControlRefreshRejectedServiceMode)
	}

	_ = eventsResp.Body.Close()
	signalCancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("execute() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run command did not exit")
	}
}

type integrationTrackerClient struct {
	fetchCandidates func(context.Context) ([]model.Issue, error)
	fetchByStates   func(context.Context, []string) ([]model.Issue, error)
	fetchByIDs      func(context.Context, []string) ([]model.Issue, error)
}

func (c integrationTrackerClient) FetchCandidateIssues(ctx context.Context) ([]model.Issue, error) {
	if c.fetchCandidates != nil {
		return c.fetchCandidates(ctx)
	}
	return nil, nil
}

func (c integrationTrackerClient) FetchIssuesByStates(ctx context.Context, states []string) ([]model.Issue, error) {
	if c.fetchByStates != nil {
		return c.fetchByStates(ctx, states)
	}
	return nil, nil
}

func (c integrationTrackerClient) FetchIssueStatesByIDs(ctx context.Context, ids []string) ([]model.Issue, error) {
	if c.fetchByIDs != nil {
		return c.fetchByIDs(ctx, ids)
	}
	return nil, nil
}

func newIntegrationServiceConfig(t *testing.T) *model.ServiceConfig {
	t.Helper()
	root := t.TempDir()
	return &model.ServiceConfig{
		TrackerKind:                "linear",
		TrackerAPIKey:              "secret-key",
		TrackerProjectSlug:         "demo",
		ActiveStates:               []string{"Todo", "In Progress"},
		TerminalStates:             []string{"Done", "Closed"},
		PollIntervalMS:             60_000,
		AutomationRootDir:          root,
		WorkspaceRoot:              filepath.Join(root, "workspaces"),
		WorkspaceLinearBranchScope: "demo-scope",
		MaxConcurrentAgents:        1,
		MaxTurns:                   1,
		MaxRetryBackoffMS:          100,
		RunBudgetTotalMS:           1000,
		RunExecutionBudgetMS:       1000,
		RunReviewFixBudgetMS:       0,
		CodexCommand:               "codex app-server",
	}
}

func newIntegrationWorkflow() *model.WorkflowDefinition {
	return &model.WorkflowDefinition{
		Completion: model.CompletionContract{
			Mode:        model.CompletionModePullRequest,
			OnMissingPR: model.CompletionActionIntervention,
			OnClosedPR:  model.CompletionActionIntervention,
		},
	}
}

func startRealRuntimeServer(t *testing.T, cfg *model.ServiceConfig, trackerClient tracker.Client) (string, func()) {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	workflow := newIntegrationWorkflow()
	runtime := orchestrator.NewOrchestrator(
		trackerClient,
		fakeWorkspaceManager{},
		fakeAgentRunner{},
		func() *model.ServiceConfig { return cfg },
		func() *model.WorkflowDefinition { return workflow },
		func() orchestrator.RuntimeIdentity {
			return orchestrator.RuntimeIdentity{
				Compatibility: orchestrator.RuntimeCompatibility{
					ActiveSource: "linear-main",
					SourceKind:   string(contract.SourceKindLinear),
				},
			}
		},
		logger,
	)
	ctx, cancel := context.WithCancel(context.Background())
	if err := runtime.Start(ctx); err != nil {
		cancel()
		t.Fatalf("runtime.Start() error = %v", err)
	}
	httpSrv, err := server.Start(runtime, logger, "127.0.0.1", 0)
	if err != nil {
		cancel()
		runtime.Wait()
		t.Fatalf("server.Start() error = %v", err)
	}
	cleanup := func() {
		cancel()
		runtime.Wait()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), time.Second)
		defer shutdownCancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}
	return "http://" + httpSrv.Addr(), cleanup
}

func waitForUnavailableServiceSurface(
	t *testing.T,
	baseURL string,
	wantComponent string,
) (contract.DiscoveryDocument, contract.ServiceStateSnapshot) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	var lastDiscovery contract.DiscoveryDocument
	var lastState contract.ServiceStateSnapshot
	for time.Now().Before(deadline) {
		lastDiscovery = fetchJSON[contract.DiscoveryDocument](t, http.MethodGet, baseURL+"/api/v1/discovery", nil)
		lastState = fetchJSON[contract.ServiceStateSnapshot](t, http.MethodGet, baseURL+"/api/v1/state", nil)
		if lastDiscovery.ServiceMode == contract.ServiceModeUnavailable &&
			lastState.ServiceMode == contract.ServiceModeUnavailable &&
			reflect.DeepEqual(lastDiscovery.Reasons, lastState.Reasons) {
			assertUnavailableReasonComponent(t, lastState.Reasons, wantComponent)
			return lastDiscovery, lastState
		}
		time.Sleep(25 * time.Millisecond)
	}
	assertServiceSurfaceConsistency(t, lastDiscovery, lastState)
	t.Fatalf("service surface did not become unavailable(component=%s): discovery=%#v state=%#v", wantComponent, lastDiscovery, lastState)
	return contract.DiscoveryDocument{}, contract.ServiceStateSnapshot{}
}

func assertServiceSurfaceConsistency(t *testing.T, discovery contract.DiscoveryDocument, state contract.ServiceStateSnapshot) {
	t.Helper()
	if discovery.ServiceMode != state.ServiceMode {
		t.Fatalf("discovery/state service_mode mismatch: discovery=%q state=%q", discovery.ServiceMode, state.ServiceMode)
	}
	if !reflect.DeepEqual(discovery.Reasons, state.Reasons) {
		t.Fatalf("discovery/state reasons mismatch: discovery=%#v state=%#v", discovery.Reasons, state.Reasons)
	}
}

func assertUnavailableReasonComponent(t *testing.T, reasons []contract.Reason, wantComponent string) {
	t.Helper()
	if len(reasons) != 1 {
		t.Fatalf("reasons = %#v, want 1 reason", reasons)
	}
	reason := reasons[0]
	if reason.ReasonCode != contract.ReasonServiceUnavailableCoreDependency {
		t.Fatalf("reason_code = %q, want %q", reason.ReasonCode, contract.ReasonServiceUnavailableCoreDependency)
	}
	if got := reason.Details["component"]; got != wantComponent {
		t.Fatalf("reason component = %v, want %q", got, wantComponent)
	}
	if got := reason.Details["source_kind"]; got != string(contract.SourceKindLinear) {
		t.Fatalf("reason source_kind = %v, want %q", got, contract.SourceKindLinear)
	}
	if got := reason.Details["source_name"]; got != "linear-main" {
		t.Fatalf("reason source_name = %v, want linear-main", got)
	}
	detail := strings.TrimSpace(reason.Details["detail"].(string))
	if detail == "" {
		t.Fatalf("reason detail = %#v, want non-empty", reason.Details)
	}
}

func TestMainIntegration_HTTPAPIExposesDispatchPreflightFailureAsUnavailable(t *testing.T) {
	cfg := newIntegrationServiceConfig(t)
	cfg.CodexCommand = ""
	baseURL, cleanup := startRealRuntimeServer(t, cfg, integrationTrackerClient{})
	defer cleanup()

	discovery, state := waitForUnavailableServiceSurface(t, baseURL, "dispatch_preflight")
	assertServiceSurfaceConsistency(t, discovery, state)
	if discovery.Source.Kind != contract.SourceKindLinear || discovery.Source.Name != "linear-main" {
		t.Fatalf("discovery source = %#v, want linear/linear-main", discovery.Source)
	}

	control := fetchJSON[contract.ControlResult](t, http.MethodPost, baseURL+"/api/v1/control/refresh", strings.NewReader("{}"))
	if control.Status != contract.ControlStatusRejected {
		t.Fatalf("refresh status = %q, want %q", control.Status, contract.ControlStatusRejected)
	}
	if control.Reason == nil || control.Reason.ReasonCode != contract.ReasonControlRefreshRejectedServiceMode {
		t.Fatalf("refresh reason = %#v, want %q", control.Reason, contract.ReasonControlRefreshRejectedServiceMode)
	}
}

func TestMainIntegration_HTTPAPIExposesTrackerUnreachableAsUnavailable(t *testing.T) {
	cfg := newIntegrationServiceConfig(t)
	trackerClient := integrationTrackerClient{
		fetchCandidates: func(context.Context) ([]model.Issue, error) {
			return nil, errors.New("tracker down")
		},
	}
	baseURL, cleanup := startRealRuntimeServer(t, cfg, trackerClient)
	defer cleanup()

	discovery, state := waitForUnavailableServiceSurface(t, baseURL, "task_source")
	assertServiceSurfaceConsistency(t, discovery, state)
	if discovery.Source.Kind != contract.SourceKindLinear || discovery.Source.Name != "linear-main" {
		t.Fatalf("discovery source = %#v, want linear/linear-main", discovery.Source)
	}

	control := fetchJSON[contract.ControlResult](t, http.MethodPost, baseURL+"/api/v1/control/refresh", strings.NewReader("{}"))
	if control.Status != contract.ControlStatusRejected {
		t.Fatalf("refresh status = %q, want %q", control.Status, contract.ControlStatusRejected)
	}
	if control.Reason == nil || control.Reason.ReasonCode != contract.ReasonControlRefreshRejectedServiceMode {
		t.Fatalf("refresh reason = %#v, want %q", control.Reason, contract.ReasonControlRefreshRejectedServiceMode)
	}
}

type mainSSEEvent struct {
	Event string
	Data  string
}

func readMainSSEEvent(t *testing.T, reader *bufio.Reader) mainSSEEvent {
	t.Helper()

	event := mainSSEEvent{}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("ReadString() error = %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if event.Event != "" || event.Data != "" {
				return event
			}
			continue
		}
		switch {
		case strings.HasPrefix(line, "event: "):
			event.Event = strings.TrimSpace(strings.TrimPrefix(line, "event: "))
		case strings.HasPrefix(line, "data: "):
			event.Data = strings.TrimSpace(strings.TrimPrefix(line, "data: "))
		}
	}
}

func fetchJSON[T any](t *testing.T, method string, url string, body io.Reader) T {
	t.Helper()

	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do(%s %s) error = %v", method, url, err)
	}
	defer resp.Body.Close()

	var payload T
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("Decode(%s %s) error = %v", method, url, err)
	}
	return payload
}
