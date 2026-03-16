package server

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"symphony-go/internal/model/contract"
	"symphony-go/internal/orchestrator"
)

func TestDiscoveryEndpointReturnsStaticContract(t *testing.T) {
	runtime := newFakeRuntime(sampleDiscovery(), sampleSnapshot(), sampleObjects())
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
	if payload.DomainID != "default" {
		t.Fatalf("domain_id = %q, want default", payload.DomainID)
	}
	if payload.Instance.Name != "symphony" {
		t.Fatalf("instance.name = %q, want symphony", payload.Instance.Name)
	}
	if payload.Source.Kind != contract.SourceKindGitHubIssues {
		t.Fatalf("source.kind = %q, want %q", payload.Source.Kind, contract.SourceKindGitHubIssues)
	}
	if len(payload.Capabilities.Capabilities) != 2 {
		t.Fatalf("len(capabilities) = %d, want 2", len(payload.Capabilities.Capabilities))
	}
}

func TestStateEndpointReturnsCurrentCapabilitiesAndRole(t *testing.T) {
	runtime := newFakeRuntime(sampleDiscovery(), sampleSnapshot(), sampleObjects())
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
	if payload.Instance.Role != contract.InstanceRoleLeader {
		t.Fatalf("instance.role = %q, want %q", payload.Instance.Role, contract.InstanceRoleLeader)
	}
	if len(payload.Capabilities.Capabilities) != 2 {
		t.Fatalf("len(capabilities) = %d, want 2", len(payload.Capabilities.Capabilities))
	}
}

func TestFormalEndpointsDoNotExposeLegacyIssueFields(t *testing.T) {
	runtime := newFakeRuntime(sampleDiscovery(), sampleSnapshot(), sampleObjects())
	handler := NewHandler(runtime, nil)

	t.Run("discovery", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/discovery", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		var payload map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("Unmarshal() error = %v", err)
		}
		for _, forbidden := range []string{"service_mode", "recovery_in_progress", "reasons", "limits", "records"} {
			if _, ok := payload[forbidden]; ok {
				t.Fatalf("discovery still exposes legacy field %q: %#v", forbidden, payload)
			}
		}
	})

	t.Run("state", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/state", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		var payload map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("Unmarshal() error = %v", err)
		}
		for _, forbidden := range []string{"counts", "records", "completed_window", "source", "limits"} {
			if _, ok := payload[forbidden]; ok {
				t.Fatalf("state still exposes legacy field %q: %#v", forbidden, payload)
			}
		}
	})

	t.Run("object_query", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/objects/action/act-1", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		var payload map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("Unmarshal() error = %v", err)
		}
		item, ok := payload["item"].(map[string]any)
		if !ok {
			t.Fatalf("item = %#v, want object", payload["item"])
		}
		for _, forbidden := range []string{"record_id", "source_ref", "durable_refs", "result", "observation"} {
			if _, ok := item[forbidden]; ok {
				t.Fatalf("object query still exposes legacy field %q: %#v", forbidden, item)
			}
		}
	})
}

func TestRefreshEndpointReturnsControlResultForLeader(t *testing.T) {
	t.Run("accepted", func(t *testing.T) {
		runtime := newFakeRuntime(sampleDiscovery(), sampleSnapshot(), sampleObjects())
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
		runtime := newFakeRuntime(sampleDiscovery(), sampleSnapshot(), sampleObjects())
		reason := contract.MustReason(contract.ReasonControlRefreshRejectedServiceMode, map[string]any{
			"service_mode": contract.ServiceModeUnavailable,
		})
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
}

func TestRefreshEndpointRejectsStandbyWrites(t *testing.T) {
	state := sampleSnapshot()
	state.Instance.Role = contract.InstanceRoleStandby
	state.Leader = &contract.LeaderHint{ID: "leader-a", Name: "symphony-leader", URL: "http://127.0.0.1:9090"}

	runtime := newFakeRuntime(sampleDiscovery(), state, sampleObjects())
	handler := NewHandler(runtime, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/control/refresh", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusConflict)
	}
	var payload contract.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if payload.ErrorCode != contract.ErrorAPILeaderRequired {
		t.Fatalf("error_code = %q, want %q", payload.ErrorCode, contract.ErrorAPILeaderRequired)
	}
}

func TestObjectQueryEndpointsSupportFormalObjects(t *testing.T) {
	runtime := newFakeRuntime(sampleDiscovery(), sampleSnapshot(), sampleObjects())
	handler := NewHandler(runtime, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/objects/action/act-1", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET action status = %d, want %d", rec.Code, http.StatusOK)
	}
	var item contract.ObjectQueryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &item); err != nil {
		t.Fatalf("Unmarshal(action) error = %v", err)
	}
	var action contract.Action
	if err := json.Unmarshal(item.Item, &action); err != nil {
		t.Fatalf("Unmarshal(action item) error = %v", err)
	}
	if action.Type != contract.ActionTypeSourceClosure {
		t.Fatalf("action.type = %q, want %q", action.Type, contract.ActionTypeSourceClosure)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/objects/action", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("LIST action status = %d, want %d", rec.Code, http.StatusOK)
	}
	var list contract.ObjectListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("Unmarshal(list) error = %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(list.Items))
	}
}

func TestEventsEndpointStreamsFormalEnvelopes(t *testing.T) {
	runtime := newFakeRuntime(sampleDiscovery(), sampleSnapshot(), sampleObjects())
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
	if firstEnvelope.ContractVersion != contract.APIVersionV1 {
		t.Fatalf("contract_version = %q, want %q", firstEnvelope.ContractVersion, contract.APIVersionV1)
	}
	if len(firstEnvelope.Objects) != 2 {
		t.Fatalf("len(objects) = %d, want 2", len(firstEnvelope.Objects))
	}
	var firstRaw map[string]any
	if err := json.Unmarshal([]byte(first.Data), &firstRaw); err != nil {
		t.Fatalf("Unmarshal(first raw) error = %v", err)
	}
	if _, ok := firstRaw["record_ids"]; ok {
		t.Fatalf("snapshot event still exposes legacy record_ids: %#v", firstRaw)
	}

	runtime.publishEvent(contract.EventEnvelope{
		EventID:         "evt-2",
		EventType:       contract.EventTypeObjectChanged,
		Timestamp:       "2026-03-14T00:00:05Z",
		ContractVersion: contract.APIVersionV1,
		DomainID:        "default",
		ServiceMode:     contract.ServiceModeServing,
		Objects:         []contract.EventObject{{ObjectType: contract.ObjectTypeAction, ObjectID: "act-1", State: string(contract.ActionStatusCompleted), Visibility: contract.VisibilityLevelRestricted}},
	})

	second := readSSEEvent(t, reader)
	if second.Event != string(contract.EventTypeObjectChanged) {
		t.Fatalf("second event = %q, want %q", second.Event, contract.EventTypeObjectChanged)
	}
	var secondEnvelope contract.EventEnvelope
	if err := json.Unmarshal([]byte(second.Data), &secondEnvelope); err != nil {
		t.Fatalf("Unmarshal(second) error = %v", err)
	}
	if len(secondEnvelope.Objects) != 1 || secondEnvelope.Objects[0].ObjectID != "act-1" {
		t.Fatalf("objects = %#v, want act-1", secondEnvelope.Objects)
	}
	var secondRaw map[string]any
	if err := json.Unmarshal([]byte(second.Data), &secondRaw); err != nil {
		t.Fatalf("Unmarshal(second raw) error = %v", err)
	}
	if _, ok := secondRaw["record_ids"]; ok {
		t.Fatalf("object_changed event still exposes legacy record_ids: %#v", secondRaw)
	}
}

func TestEventsEndpointWithoutFlusherReturnsServiceUnavailable(t *testing.T) {
	runtime := newFakeRuntime(sampleDiscovery(), sampleSnapshot(), sampleObjects())
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
	runtime := newFakeRuntime(sampleDiscovery(), sampleSnapshot(), sampleObjects())
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
	subscribers   map[int]chan contract.EventEnvelope
	objects       map[string]orchestrator.ObjectEnvelope
}

func newFakeRuntime(discovery orchestrator.DiscoveryDocument, snapshot orchestrator.Snapshot, objects []orchestrator.ObjectEnvelope) *fakeRuntime {
	reason := contract.MustReason(contract.ReasonControlRefreshAccepted, map[string]any{
		"service_mode": contract.ServiceModeServing,
	})
	store := make(map[string]orchestrator.ObjectEnvelope, len(objects))
	for _, item := range objects {
		store[string(item.ObjectType)+"/"+item.ObjectID] = item
	}
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
		subscribers: map[int]chan contract.EventEnvelope{},
		objects:     store,
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

func (f *fakeRuntime) SubscribeEvents(_ int) (<-chan contract.EventEnvelope, func()) {
	ch := make(chan contract.EventEnvelope, 8)
	f.mu.Lock()
	id := f.nextID
	f.nextID++
	f.subscribers[id] = ch
	event := sampleSnapshotEvent(f.snapshot, f.sortedObjectsLocked())
	f.mu.Unlock()
	ch <- event
	return ch, func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if existing, ok := f.subscribers[id]; ok {
			delete(f.subscribers, id)
			close(existing)
		}
	}
}

func (f *fakeRuntime) GetObject(objectType contract.ObjectType, id string) (orchestrator.ObjectEnvelope, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	item, ok := f.objects[string(objectType)+"/"+id]
	return item, ok
}

func (f *fakeRuntime) ListObjects(objectType contract.ObjectType) []orchestrator.ObjectEnvelope {
	f.mu.Lock()
	defer f.mu.Unlock()
	items := make([]orchestrator.ObjectEnvelope, 0, len(f.objects))
	for _, item := range f.objects {
		if item.ObjectType == objectType {
			items = append(items, item)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ObjectID < items[j].ObjectID })
	return items
}

func (f *fakeRuntime) publishEvent(event contract.EventEnvelope) {
	f.mu.Lock()
	subscribers := make([]chan contract.EventEnvelope, 0, len(f.subscribers))
	for _, ch := range f.subscribers {
		subscribers = append(subscribers, ch)
	}
	f.mu.Unlock()
	for _, ch := range subscribers {
		ch <- event
	}
}

func (f *fakeRuntime) sortedObjectsLocked() []orchestrator.ObjectEnvelope {
	items := make([]orchestrator.ObjectEnvelope, 0, len(f.objects))
	for _, item := range f.objects {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].ObjectType != items[j].ObjectType {
			return items[i].ObjectType < items[j].ObjectType
		}
		return items[i].ObjectID < items[j].ObjectID
	})
	return items
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
		DomainID: "default",
		Source: contract.SourceDocument{
			Kind: contract.SourceKindGitHubIssues,
			Name: "github-main",
		},
		Capabilities: contract.StaticCapabilitySet{
			Capabilities: []contract.StaticCapability{
				{Name: contract.CapabilityStreamEvents, Category: contract.CapabilityCategoryProtocol, Summary: "支持 HTTP/SSE 正式事件流。", Supported: true},
				{Name: contract.CapabilityQueryObjects, Category: contract.CapabilityCategoryQuery, Summary: "支持正式对象查询。", Supported: true},
			},
		},
	}
}

func sampleSnapshot() orchestrator.Snapshot {
	return contract.ServiceStateSnapshot{
		GeneratedAt:        "2026-03-14T00:00:00Z",
		ServiceMode:        contract.ServiceModeServing,
		RecoveryInProgress: false,
		Reasons:            []contract.Reason{},
		Instance: contract.InstanceStateSummary{
			ID:      "automation",
			Name:    "symphony",
			Version: "dev",
			Role:    contract.InstanceRoleLeader,
		},
		Capabilities: contract.AvailableCapabilitySet{
			Capabilities: []contract.AvailableCapability{
				{Name: contract.CapabilityQueryObjects, Category: contract.CapabilityCategoryQuery, Summary: "支持正式对象查询。", Available: true},
				{Name: contract.CapabilityServiceRefresh, Category: contract.CapabilityCategoryControl, Summary: "支持服务级 refresh 控制。", Available: true},
			},
		},
	}
}

func sampleObjects() []orchestrator.ObjectEnvelope {
	action := contract.Action{
		BaseObject: contract.BaseObject{
			ID:              "act-1",
			ObjectType:      contract.ObjectTypeAction,
			DomainID:        "default",
			Visibility:      contract.VisibilityLevelRestricted,
			ContractVersion: contract.APIVersionV1,
			CreatedAt:       "2026-03-14T00:00:00Z",
			UpdatedAt:       "2026-03-14T00:00:01Z",
		},
		State:   contract.ActionStatusExternalPending,
		Type:    contract.ActionTypeSourceClosure,
		Summary: "等待外部来源关闭。",
	}
	instance := contract.Instance{
		BaseObject: contract.BaseObject{
			ID:              "automation",
			ObjectType:      contract.ObjectTypeInstance,
			DomainID:        "default",
			Visibility:      contract.VisibilityLevelSummary,
			ContractVersion: contract.APIVersionV1,
			CreatedAt:       "2026-03-14T00:00:00Z",
			UpdatedAt:       "2026-03-14T00:00:01Z",
		},
		State:   contract.ServiceModeServing,
		Name:    "symphony",
		Version: "dev",
		Role:    contract.InstanceRoleLeader,
	}
	return []orchestrator.ObjectEnvelope{
		mustEnvelope(action),
		mustEnvelope(instance),
	}
}

func sampleSnapshotEvent(snapshot orchestrator.Snapshot, objects []orchestrator.ObjectEnvelope) contract.EventEnvelope {
	items := make([]contract.EventObject, 0, len(objects))
	for _, item := range objects {
		items = append(items, contract.EventObject{
			ObjectType: item.ObjectType,
			ObjectID:   item.ObjectID,
			Visibility: contract.VisibilityLevelRestricted,
		})
	}
	return contract.EventEnvelope{
		EventID:         "evt-1",
		EventType:       contract.EventTypeSnapshot,
		Timestamp:       snapshot.GeneratedAt,
		ContractVersion: contract.APIVersionV1,
		DomainID:        "default",
		ServiceMode:     snapshot.ServiceMode,
		Objects:         items,
	}
}

func mustEnvelope(value any) orchestrator.ObjectEnvelope {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	switch object := value.(type) {
	case contract.Action:
		return orchestrator.ObjectEnvelope{ObjectType: object.ObjectType, ObjectID: object.ID, Payload: raw}
	case contract.Instance:
		return orchestrator.ObjectEnvelope{ObjectType: object.ObjectType, ObjectID: object.ID, Payload: raw}
	default:
		panic("unsupported object type")
	}
}
