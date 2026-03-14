package server

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"symphony-go/internal/model/contract"
	"symphony-go/internal/orchestrator"
)

func TestDiscoveryEndpointReturnsFormalContract(t *testing.T) {
	runtime := newFakeRuntime(sampleDiscovery(), sampleSnapshot())
	handler := NewHandler(runtime, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/discovery", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var payload contract.DiscoveryDocument
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if payload.APIVersion != contract.APIVersionV1 {
		t.Fatalf("api_version = %q, want %q", payload.APIVersion, contract.APIVersionV1)
	}
	if payload.Instance.Name != "symphony" {
		t.Fatalf("instance.name = %q, want symphony", payload.Instance.Name)
	}
	if payload.Source.Kind != contract.SourceKindGitHubIssues {
		t.Fatalf("source.kind = %q, want %q", payload.Source.Kind, contract.SourceKindGitHubIssues)
	}
	if payload.Capabilities.EventProtocol != "sse" {
		t.Fatalf("event_protocol = %q, want sse", payload.Capabilities.EventProtocol)
	}
	if len(payload.Capabilities.ControlActions) != 1 || payload.Capabilities.ControlActions[0] != contract.ControlActionRefresh {
		t.Fatalf("control_actions = %#v, want [refresh]", payload.Capabilities.ControlActions)
	}
	if payload.Limits.CompletedWindowSize != 100 {
		t.Fatalf("completed_window_size = %d, want 100", payload.Limits.CompletedWindowSize)
	}
}

func TestDiscoveryEndpointSerializesReasonsAsArray(t *testing.T) {
	runtime := newFakeRuntime(sampleDiscovery(), sampleSnapshot())
	handler := NewHandler(runtime, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/discovery", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	reasons, ok := payload["reasons"].([]any)
	if !ok {
		t.Fatalf("reasons json type = %T, want []any", payload["reasons"])
	}
	if len(reasons) != 0 {
		t.Fatalf("reasons = %#v, want empty array", reasons)
	}
}

func TestStateEndpointReturnsFormalSnapshot(t *testing.T) {
	runtime := newFakeRuntime(sampleDiscovery(), sampleSnapshot())
	handler := NewHandler(runtime, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/state", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var payload contract.ServiceStateSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if payload.ServiceMode != contract.ServiceModeServing {
		t.Fatalf("service_mode = %q, want %q", payload.ServiceMode, contract.ServiceModeServing)
	}
	if payload.Counts.Total != 1 || payload.Counts.Active != 1 || payload.Counts.Completed != 1 {
		t.Fatalf("counts = %#v, want total=1 active=1 completed=1", payload.Counts)
	}
	if len(payload.Records) != 1 || payload.Records[0].RecordID != "rec_github_issues_github-main_1" {
		t.Fatalf("records = %#v, want rec_github_issues_github-main_1", payload.Records)
	}
	if len(payload.CompletedWindow.Records) != 1 || payload.CompletedWindow.Records[0].RecordID != "rec_github_issues_github-main_done" {
		t.Fatalf("completed_window = %#v, want rec_github_issues_github-main_done", payload.CompletedWindow)
	}
}

func TestRefreshEndpointReturnsControlResult(t *testing.T) {
	t.Run("accepted", func(t *testing.T) {
		runtime := newFakeRuntime(sampleDiscovery(), sampleSnapshot())
		handler := NewHandler(runtime, nil)

		req := httptest.NewRequest(http.MethodPost, "/api/v1/control/refresh", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
		}
		var payload contract.ControlResult
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("Unmarshal() error = %v", err)
		}
		if payload.Status != contract.ControlStatusAccepted {
			t.Fatalf("status = %q, want %q", payload.Status, contract.ControlStatusAccepted)
		}
		if payload.Reason == nil || payload.Reason.ReasonCode != contract.ReasonControlRefreshAccepted {
			t.Fatalf("reason = %#v, want %q", payload.Reason, contract.ReasonControlRefreshAccepted)
		}
	})

	t.Run("rejected", func(t *testing.T) {
		reason := contract.MustReason(contract.ReasonControlRefreshRejectedServiceMode, map[string]any{
			"service_mode": contract.ServiceModeUnavailable,
		})
		runtime := newFakeRuntime(sampleDiscovery(), sampleSnapshot())
		runtime.refreshResult = contract.ControlResult{
			Action:              contract.ControlActionRefresh,
			Status:              contract.ControlStatusRejected,
			Reason:              &reason,
			RecommendedNextStep: "检查核心依赖后重试",
			Timestamp:           "2026-03-14T00:00:03Z",
		}
		handler := NewHandler(runtime, nil)

		req := httptest.NewRequest(http.MethodPost, "/api/v1/control/refresh", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		var payload contract.ControlResult
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("Unmarshal() error = %v", err)
		}
		if payload.Status != contract.ControlStatusRejected {
			t.Fatalf("status = %q, want %q", payload.Status, contract.ControlStatusRejected)
		}
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/control/refresh", nil)
	rec := httptest.NewRecorder()
	NewHandler(newFakeRuntime(sampleDiscovery(), sampleSnapshot()), nil).ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestEventsEndpointStreamsFormalEnvelopes(t *testing.T) {
	runtime := newFakeRuntime(sampleDiscovery(), sampleSnapshot())
	srv := httptest.NewServer(NewHandler(runtime, nil))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/events")
	if err != nil {
		t.Fatalf("GET /api/v1/events error = %v", err)
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	first := readSSEEvent(t, reader)
	if first.Event != string(contract.EventTypeSnapshot) {
		t.Fatalf("first event = %q, want %q", first.Event, contract.EventTypeSnapshot)
	}
	var firstEnvelope contract.EventEnvelope
	if err := json.Unmarshal([]byte(first.Data), &firstEnvelope); err != nil {
		t.Fatalf("Unmarshal(first) error = %v", err)
	}
	if firstEnvelope.EventType != contract.EventTypeSnapshot {
		t.Fatalf("first payload event_type = %q, want %q", firstEnvelope.EventType, contract.EventTypeSnapshot)
	}
	if len(firstEnvelope.RecordIDs) != 2 {
		t.Fatalf("first record_ids = %#v, want 2 ids", firstEnvelope.RecordIDs)
	}

	next := sampleSnapshot()
	next.ServiceMode = contract.ServiceModeDegraded
	next.Reasons = []contract.Reason{contract.MustReason(contract.ReasonServiceDegradedNotificationDelivery, map[string]any{
		"channel_ids": []string{"ops"},
	})}
	next.Records[0].Status = contract.IssueStatusAwaitingMerge
	next.Records[0].Reason = ptrReason(contract.MustReason(contract.ReasonRecordBlockedAwaitingMerge, map[string]any{
		"record_id": "rec_github_issues_github-main_1",
	}))
	next.Records[0].UpdatedAt = "2026-03-14T00:00:05Z"
	runtime.publish(next)

	second := readSSEEvent(t, reader)
	if second.Event != string(contract.EventTypeStateChanged) {
		t.Fatalf("second event = %q, want %q", second.Event, contract.EventTypeStateChanged)
	}
	var secondEnvelope contract.EventEnvelope
	if err := json.Unmarshal([]byte(second.Data), &secondEnvelope); err != nil {
		t.Fatalf("Unmarshal(second) error = %v", err)
	}
	if secondEnvelope.ServiceMode != contract.ServiceModeDegraded {
		t.Fatalf("service_mode = %q, want %q", secondEnvelope.ServiceMode, contract.ServiceModeDegraded)
	}
	if len(secondEnvelope.RecordIDs) == 0 || secondEnvelope.RecordIDs[0] != "rec_github_issues_github-main_1" {
		t.Fatalf("record_ids = %#v, want rec_github_issues_github-main_1", secondEnvelope.RecordIDs)
	}
	if secondEnvelope.Reason == nil || secondEnvelope.Reason.ReasonCode != contract.ReasonServiceDegradedNotificationDelivery {
		t.Fatalf("reason = %#v, want %q", secondEnvelope.Reason, contract.ReasonServiceDegradedNotificationDelivery)
	}
}

func TestEventsEndpointWithoutFlusherReturnsServiceUnavailable(t *testing.T) {
	runtime := newFakeRuntime(sampleDiscovery(), sampleSnapshot())
	handler := NewHandler(runtime, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
	recorder := httptest.NewRecorder()
	writer := &nonFlusherResponseWriter{ResponseWriter: recorder}
	handler.ServeHTTP(writer, req)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
}

func TestUnknownRouteReturnsStructuredNotFound(t *testing.T) {
	runtime := newFakeRuntime(sampleDiscovery(), sampleSnapshot())
	handler := NewHandler(runtime, nil)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	var payload contract.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if payload.ErrorCode != contract.ErrorAPINotFound {
		t.Fatalf("error_code = %q, want %q", payload.ErrorCode, contract.ErrorAPINotFound)
	}
}

type fakeRuntime struct {
	mu            sync.Mutex
	discovery     orchestrator.DiscoveryDocument
	snapshot      orchestrator.Snapshot
	refreshResult contract.ControlResult
	nextID        int
	subscribers   map[int]chan orchestrator.Snapshot
}

func newFakeRuntime(discovery orchestrator.DiscoveryDocument, snapshot orchestrator.Snapshot) *fakeRuntime {
	reason := contract.MustReason(contract.ReasonControlRefreshAccepted, map[string]any{
		"service_mode": contract.ServiceModeServing,
	})
	return &fakeRuntime{
		discovery: discovery,
		snapshot:  snapshot,
		refreshResult: contract.ControlResult{
			Action:              contract.ControlActionRefresh,
			Status:              contract.ControlStatusAccepted,
			Reason:              &reason,
			RecommendedNextStep: "等待 SSE 通知后回读 /api/v1/state",
			Timestamp:           "2026-03-14T00:00:02Z",
		},
		subscribers: map[int]chan orchestrator.Snapshot{},
	}
}

func (f *fakeRuntime) Discovery() orchestrator.DiscoveryDocument {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.discovery
}

func (f *fakeRuntime) Snapshot() orchestrator.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snapshot
}

func (f *fakeRuntime) RequestRefresh() orchestrator.RefreshRequestResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refreshResult
}

func (f *fakeRuntime) SubscribeSnapshots(_ int) (<-chan orchestrator.Snapshot, func()) {
	ch := make(chan orchestrator.Snapshot, 8)
	f.mu.Lock()
	id := f.nextID
	f.nextID++
	f.subscribers[id] = ch
	snapshot := f.snapshot
	f.mu.Unlock()
	ch <- snapshot
	return ch, func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if existing, ok := f.subscribers[id]; ok {
			delete(f.subscribers, id)
			close(existing)
		}
	}
}

func (f *fakeRuntime) publish(snapshot orchestrator.Snapshot) {
	f.mu.Lock()
	f.snapshot = snapshot
	subscribers := make([]chan orchestrator.Snapshot, 0, len(f.subscribers))
	for _, ch := range f.subscribers {
		subscribers = append(subscribers, ch)
	}
	f.mu.Unlock()
	for _, ch := range subscribers {
		ch <- snapshot
	}
}

type sseMessage struct {
	Event string
	Data  string
}

func readSSEEvent(t *testing.T, reader *bufio.Reader) sseMessage {
	t.Helper()

	message := sseMessage{}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("ReadString() error = %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if message.Event != "" || message.Data != "" {
				return message
			}
			continue
		}
		switch {
		case strings.HasPrefix(line, "event: "):
			message.Event = strings.TrimSpace(strings.TrimPrefix(line, "event: "))
		case strings.HasPrefix(line, "data: "):
			message.Data = strings.TrimSpace(strings.TrimPrefix(line, "data: "))
		}
	}
}

type nonFlusherResponseWriter struct {
	http.ResponseWriter
}

func sampleDiscovery() orchestrator.DiscoveryDocument {
	return contract.DiscoveryDocument{
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
			Notifications:  []string{"slack", "webhook"},
			Sources:        []contract.SourceKind{contract.SourceKindGitHubIssues},
		},
		Reasons: []contract.Reason{},
		Limits: contract.LimitDocument{
			CompletedWindowSize: 100,
		},
	}
}

func sampleSnapshot() orchestrator.Snapshot {
	return contract.ServiceStateSnapshot{
		GeneratedAt:        "2026-03-14T00:00:00Z",
		ServiceMode:        contract.ServiceModeServing,
		RecoveryInProgress: false,
		Reasons:            []contract.Reason{},
		Counts: contract.StateCounts{
			Total:     1,
			Active:    1,
			Completed: 1,
		},
		Records: []contract.IssueRuntimeRecord{
			{
				RecordID:  "rec_github_issues_github-main_1",
				SourceRef: contract.SourceRef{SourceKind: contract.SourceKindGitHubIssues, SourceName: "github-main", SourceID: "1", SourceIdentifier: "GH-1", URL: "https://github.example/issues/1"},
				Status:    contract.IssueStatusActive,
				UpdatedAt: "2026-03-14T00:00:00Z",
				Observation: &contract.Observation{
					Running: true,
					Summary: "agent run in progress",
					Details: map[string]any{"runner": "codex"},
				},
				DurableRefs: contract.DurableRefs{
					Workspace:  &contract.WorkspaceRef{Path: "/tmp/abc-1"},
					Branch:     &contract.BranchRef{Name: "feature/abc-1"},
					LedgerPath: "automation/local/runtime-ledger.json",
				},
			},
		},
		CompletedWindow: contract.CompletedWindow{
			Limit: 100,
			Records: []contract.IssueRuntimeRecord{
				{
					RecordID:  "rec_github_issues_github-main_done",
					SourceRef: contract.SourceRef{SourceKind: contract.SourceKindGitHubIssues, SourceName: "github-main", SourceID: "done", SourceIdentifier: "GH-2"},
					Status:    contract.IssueStatusCompleted,
					UpdatedAt: "2026-03-14T00:00:01Z",
					DurableRefs: contract.DurableRefs{
						LedgerPath: "automation/local/runtime-ledger.json",
					},
					Result: &contract.Result{
						Outcome:     contract.ResultOutcomeSucceeded,
						Summary:     "completed",
						CompletedAt: "2026-03-14T00:00:01Z",
						Details:     map[string]any{"record_id": "rec_github_issues_github-main_done"},
					},
				},
			},
		},
	}
}

func ptrReason(reason contract.Reason) *contract.Reason {
	return &reason
}
