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
	if counts["running"].(float64) != 1 || counts["awaiting_merge"].(float64) != 1 || counts["awaiting_intervention"].(float64) != 1 || counts["retrying"].(float64) != 1 {
		t.Fatalf("counts = %+v", counts)
	}
	awaitingMerge := payload["awaiting_merge"].([]any)
	if len(awaitingMerge) != 1 {
		t.Fatalf("awaiting_merge = %+v, want 1 entry", awaitingMerge)
	}
	awaitingIntervention := payload["awaiting_intervention"].([]any)
	if len(awaitingIntervention) != 1 {
		t.Fatalf("awaiting_intervention = %+v, want 1 entry", awaitingIntervention)
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

	req = httptest.NewRequest(http.MethodGet, "/api/v1/ABC-3", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var awaitingPayload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &awaitingPayload); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if awaitingPayload["status"] != "awaiting_merge" {
		t.Fatalf("status = %v, want awaiting_merge", awaitingPayload["status"])
	}
	if awaitingPayload["attempt_count"].(float64) != 1 {
		t.Fatalf("attempt_count = %v, want 1", awaitingPayload["attempt_count"])
	}
	awaitingEntry := awaitingPayload["awaiting_merge"].(map[string]any)
	if awaitingEntry["pr_state"] != "open" {
		t.Fatalf("awaiting_merge.pr_state = %v, want open", awaitingEntry["pr_state"])
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/ABC-4", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var interventionPayload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &interventionPayload); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if interventionPayload["status"] != "awaiting_intervention" {
		t.Fatalf("status = %v, want awaiting_intervention", interventionPayload["status"])
	}
	if interventionPayload["attempt_count"].(float64) != 2 {
		t.Fatalf("attempt_count = %v, want 2", interventionPayload["attempt_count"])
	}
	interventionEntry := interventionPayload["awaiting_intervention"].(map[string]any)
	if interventionEntry["pr_state"] != "closed" {
		t.Fatalf("awaiting_intervention.pr_state = %v, want closed", interventionEntry["pr_state"])
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
	writer := newStreamingResponseWriter()

	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(writer, req)
		close(done)
	}()

	writer.waitForBody(t, "event: snapshot")
	runtime.publish(sampleSnapshot())
	writer.waitForBody(t, "event: update")

	body := writer.bodyString()
	if !strings.Contains(body, "event: snapshot") {
		t.Fatalf("body missing snapshot event: %s", body)
	}
	if !strings.Contains(body, "event: update") {
		t.Fatalf("body missing update event: %s", body)
	}

	cancel()
	<-done
}

func TestEventsEndpointClientDisconnect(t *testing.T) {
	runtime := newFakeRuntime(sampleSnapshot())
	handler := NewHandler(runtime, nil)

	ctx, cancel := context.WithCancel(context.Background())
	writer := newStreamingResponseWriter()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil).WithContext(ctx)

	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(writer, req)
		close(done)
	}()

	writer.waitForBody(t, "event: snapshot")
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("SSE handler did not exit after client disconnect")
	}
}

func TestEventsEndpointNoFlusherReturns500(t *testing.T) {
	runtime := newFakeRuntime(sampleSnapshot())
	handler := NewHandler(runtime, nil)
	writer := &failingResponseWriter{header: make(http.Header)}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
	handler.ServeHTTP(writer, req)

	if writer.status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", writer.status)
	}

	var payload map[string]any
	if err := json.Unmarshal(writer.buf.Bytes(), &payload); err != nil {
		t.Fatalf("Unmarshal() error = %v, body = %q", err, writer.buf.String())
	}
	errPayload := payload["error"].(map[string]any)
	if errPayload["code"] != "stream_not_supported" {
		t.Fatalf("error.code = %v, want stream_not_supported", errPayload["code"])
	}
}

func TestEventsEndpointConcurrentClients(t *testing.T) {
	runtime := newFakeRuntime(sampleSnapshot())
	handler := NewHandler(runtime, nil)

	type client struct {
		cancel func()
		writer *streamingResponseWriter
		done   chan struct{}
	}

	clients := make([]client, 0, 2)
	for range 2 {
		ctx, cancel := context.WithCancel(context.Background())
		writer := newStreamingResponseWriter()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil).WithContext(ctx)
		done := make(chan struct{})
		go func() {
			handler.ServeHTTP(writer, req)
			close(done)
		}()
		clients = append(clients, client{cancel: cancel, writer: writer, done: done})
	}

	for _, client := range clients {
		client.writer.waitForBody(t, "event: snapshot")
	}

	runtime.publish(sampleSnapshot())

	for _, client := range clients {
		client.writer.waitForBody(t, "event: update")
		client.cancel()
	}

	for _, client := range clients {
		select {
		case <-client.done:
		case <-time.After(time.Second):
			t.Fatal("concurrent SSE client did not exit")
		}
	}
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
	nextID       int
	subscribers  map[int]chan orchestrator.Snapshot
}

func newFakeRuntime(snapshot orchestrator.Snapshot) *fakeRuntime {
	return &fakeRuntime{
		snapshot:    snapshot,
		subscribers: make(map[int]chan orchestrator.Snapshot),
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
		subscriber, ok := f.subscribers[id]
		if !ok {
			return
		}
		delete(f.subscribers, id)
		close(subscriber)
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

func sampleSnapshot() orchestrator.Snapshot {
	now := time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC)
	return orchestrator.Snapshot{
		GeneratedAt: now,
		Service: orchestrator.ServiceSnapshot{
			Version:   "test-build",
			StartedAt: now.Add(-5 * time.Minute),
		},
		Counts: orchestrator.SnapshotCounts{
			Running:              1,
			AwaitingMerge:        1,
			AwaitingIntervention: 1,
			Retrying:             1,
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
		AwaitingMerge: []orchestrator.AwaitingMergeSnapshot{
			{
				IssueID:         "3",
				IssueIdentifier: "ABC-3",
				WorkspacePath:   "C:/work/ABC-3",
				State:           "In Progress",
				Branch:          "testuser/linear-demo-scope-abc-3",
				PRNumber:        99,
				PRURL:           "https://example.test/pr/99",
				PRState:         "open",
				AwaitingSince:   now.Add(-2 * time.Minute),
				AttemptCount:    1,
			},
		},
		AwaitingIntervention: []orchestrator.AwaitingInterventionSnapshot{
			{
				IssueID:         "4",
				IssueIdentifier: "ABC-4",
				WorkspacePath:   "C:/work/ABC-4",
				Branch:          "testuser/linear-demo-scope-abc-4",
				PRNumber:        100,
				PRURL:           "https://example.test/pr/100",
				PRState:         "closed",
				ObservedAt:      now.Add(-3 * time.Minute),
				AttemptCount:    2,
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
	buf      bytes.Buffer
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

func (w *failingResponseWriter) Write(p []byte) (int, error) {
	if w.writeErr == nil {
		return w.buf.Write(p)
	}
	return 0, w.writeErr
}

type streamingResponseWriter struct {
	header  http.Header
	status  int
	buf     bytes.Buffer
	mu      sync.Mutex
	flushCh chan struct{}
}

func newStreamingResponseWriter() *streamingResponseWriter {
	return &streamingResponseWriter{
		header:  make(http.Header),
		flushCh: make(chan struct{}, 16),
	}
}

func (w *streamingResponseWriter) Header() http.Header { return w.header }

func (w *streamingResponseWriter) WriteHeader(status int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.status = status
}

func (w *streamingResponseWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *streamingResponseWriter) Flush() {
	select {
	case w.flushCh <- struct{}{}:
	default:
	}
}

func (w *streamingResponseWriter) bodyString() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func (w *streamingResponseWriter) waitForBody(t *testing.T, needle string) {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		if strings.Contains(w.bodyString(), needle) {
			return
		}
		select {
		case <-w.flushCh:
		case <-time.After(10 * time.Millisecond):
		case <-deadline:
			t.Fatalf("body %q missing %q", w.bodyString(), needle)
		}
	}
}
