package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"symphony-go/internal/orchestrator"
)

func TestStateEndpointReturnsSnapshot(t *testing.T) {
	runtime := newFakeRuntime(sampleSnapshot())
	handler := NewHandler(runtime, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/state", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if payload["generated_at"] == nil {
		t.Fatalf("generated_at missing: %+v", payload)
	}
	service := payload["service"].(map[string]any)
	if service["version"] != "test-build" {
		t.Fatalf("service.version = %v, want test-build", service["version"])
	}
	if service["started_at"] == nil {
		t.Fatalf("service.started_at missing: %+v", service)
	}
	if service["uptime_seconds"].(float64) <= 0 {
		t.Fatalf("service.uptime_seconds = %v, want positive", service["uptime_seconds"])
	}
	counts := payload["counts"].(map[string]any)
	if counts["running"].(float64) != 1 || counts["retrying"].(float64) != 1 {
		t.Fatalf("counts = %+v", counts)
	}
	alerts := payload["alerts"].([]any)
	if len(alerts) != 1 {
		t.Fatalf("alerts = %+v, want 1 alert", alerts)
	}
	rateLimits := payload["rate_limits"].(map[string]any)
	if rateLimits["remaining"] != float64(9) {
		t.Fatalf("rate_limits = %+v, want remaining=9", rateLimits)
	}
}

func TestIssueEndpointReturnsKnownIssueAnd404ForUnknown(t *testing.T) {
	runtime := newFakeRuntime(sampleSnapshot())
	handler := NewHandler(runtime, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/ABC-1", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var runningPayload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &runningPayload); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if runningPayload["workspace_path"] != "C:/work/ABC-1" {
		t.Fatalf("workspace_path = %v, want C:/work/ABC-1", runningPayload["workspace_path"])
	}
	if runningPayload["attempt_count"].(float64) != 2 {
		t.Fatalf("attempt_count = %v, want 2", runningPayload["attempt_count"])
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/ABC-2", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var retryPayload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &retryPayload); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if retryPayload["last_error"] != "workspace_hook_failed: before_run failed" {
		t.Fatalf("last_error = %v", retryPayload["last_error"])
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/MISSING", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestRefreshEndpointAndMethodNotAllowed(t *testing.T) {
	runtime := newFakeRuntime(sampleSnapshot())
	handler := NewHandler(runtime, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/refresh", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	if runtime.refreshCount != 1 {
		t.Fatalf("refreshCount = %d, want 1", runtime.refreshCount)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/refresh", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestEventsEndpointSendsSnapshotAndUpdate(t *testing.T) {
	runtime := newFakeRuntime(sampleSnapshot())
	handler := NewHandler(runtime, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(rec, req)
		close(done)
	}()

	runtime.publish(sampleSnapshot())
	time.Sleep(200 * time.Millisecond)

	body := rec.Body.String()
	if !strings.Contains(body, "event: snapshot") {
		t.Fatalf("body missing snapshot event: %s", body)
	}
	if !strings.Contains(body, "event: update") {
		t.Fatalf("body missing update event: %s", body)
	}

	cancel()
	<-done
}

func TestDashboardAndMethodNotAllowed(t *testing.T) {
	runtime := newFakeRuntime(sampleSnapshot())
	handler := NewHandler(runtime, nil)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Symphony-Go") {
		t.Fatalf("dashboard body = %q", rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestWriteJSONLogsEncodeFailure(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	writer := &failingResponseWriter{header: make(http.Header), writeErr: errors.New("boom")}

	writeJSON(writer, http.StatusOK, map[string]any{"ok": true}, logger)

	if writer.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", writer.status)
	}
	if !strings.Contains(logs.String(), "http response encode failed") {
		t.Fatalf("warn log missing: %s", logs.String())
	}
}

type fakeRuntime struct {
	mu           sync.Mutex
	snapshot     orchestrator.Snapshot
	refreshCount int
	ch           chan orchestrator.Snapshot
}

func newFakeRuntime(snapshot orchestrator.Snapshot) *fakeRuntime {
	return &fakeRuntime{
		snapshot: snapshot,
		ch:       make(chan orchestrator.Snapshot, 8),
	}
}

func (f *fakeRuntime) Snapshot() orchestrator.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snapshot
}

func (f *fakeRuntime) RequestRefresh() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshCount++
}

func (f *fakeRuntime) SubscribeSnapshots(_ int) (<-chan orchestrator.Snapshot, func()) {
	ch := make(chan orchestrator.Snapshot, 8)
	ch <- f.Snapshot()
	go func() {
		for item := range f.ch {
			ch <- item
		}
	}()
	return ch, func() { close(ch) }
}

func (f *fakeRuntime) publish(snapshot orchestrator.Snapshot) {
	f.ch <- snapshot
}

func sampleSnapshot() orchestrator.Snapshot {
	now := time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC)
	return orchestrator.Snapshot{
		GeneratedAt: now,
		Service: orchestrator.ServiceSnapshot{
			Version:   "test-build",
			StartedAt: now.Add(-5 * time.Minute),
		},
		Counts: orchestrator.SnapshotCounts{
			Running:  1,
			Retrying: 1,
		},
		Running: []orchestrator.RunningSnapshot{
			{
				IssueID:             "1",
				IssueIdentifier:     "ABC-1",
				WorkspacePath:       "C:/work/ABC-1",
				State:               "In Progress",
				SessionID:           "thread-turn",
				TurnCount:           2,
				LastEvent:           "turn_completed",
				LastMessage:         "done",
				StartedAt:           now.Add(-time.Minute),
				InputTokens:         10,
				OutputTokens:        5,
				TotalTokens:         15,
				CurrentRetryAttempt: 1,
				AttemptCount:        2,
			},
		},
		Retrying: []orchestrator.RetrySnapshot{
			{
				IssueID:         "2",
				IssueIdentifier: "ABC-2",
				WorkspacePath:   "C:/work/ABC-2",
				Attempt:         3,
				DueAt:           now.Add(time.Minute),
				Error:           stringPtr("workspace_hook_failed: before_run failed"),
			},
		},
		Alerts: []orchestrator.AlertSnapshot{
			{
				Code:    "tracker_unreachable",
				Level:   "warn",
				Message: "tracker down",
			},
		},
		CodexTotals: orchestrator.Snapshot{}.CodexTotals,
		RateLimits:  map[string]any{"remaining": 9, "resetAt": "2026-03-07T12:05:00Z"},
	}
}

func stringPtr(value string) *string {
	copyValue := value
	return &copyValue
}

type failingResponseWriter struct {
	header   http.Header
	status   int
	writeErr error
}

func (w *failingResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *failingResponseWriter) WriteHeader(status int) {
	w.status = status
}

func (w *failingResponseWriter) Write(_ []byte) (int, error) {
	return 0, w.writeErr
}
