package contract

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestServiceModeIsClosedSetAndLegacyIssueContractsDoNotReturn(t *testing.T) {
	serviceModes := []ServiceMode{
		ServiceModeServing,
		ServiceModeDegraded,
		ServiceModeUnavailable,
	}
	for _, mode := range serviceModes {
		if !mode.IsValid() {
			t.Fatalf("ServiceMode(%q).IsValid() = false", mode)
		}
	}
	if ServiceMode("normal").IsValid() {
		t.Fatal(`ServiceMode("normal").IsValid() = true, want false`)
	}
	raw, err := os.ReadFile("types.go")
	if err != nil {
		t.Fatalf("ReadFile(types.go) error = %v", err)
	}
	for _, legacy := range []string{"IssueStatus", "IssueRuntimeRecord", "IssueLedgerRecord"} {
		if strings.Contains(string(raw), legacy) {
			t.Fatalf("legacy issue-centric contract %q returned to types.go", legacy)
		}
	}
}

func TestReasonAndErrorDescriptorsAreStructured(t *testing.T) {
	reason := MustReason(ReasonRuntimeReloadRestartRequired, map[string]any{
		"field_path": "runtime.workspace.root",
	})
	if reason.Category != CategoryRuntime {
		t.Fatalf("reason.Category = %q, want %q", reason.Category, CategoryRuntime)
	}
	if reason.Details["field_path"] != "runtime.workspace.root" {
		t.Fatalf("reason.Details[field_path] = %v, want runtime.workspace.root", reason.Details["field_path"])
	}

	errResp := MustErrorResponse(ErrorServiceUnavailable, "service is unavailable", map[string]any{
		"dependency": "tracker",
	})
	if errResp.Category != CategoryService {
		t.Fatalf("errResp.Category = %q, want %q", errResp.Category, CategoryService)
	}
	if !errResp.Retryable {
		t.Fatal("errResp.Retryable = false, want true")
	}
	if errResp.Details["dependency"] != "tracker" {
		t.Fatalf("errResp.Details[dependency] = %v, want tracker", errResp.Details["dependency"])
	}
}

func TestDiscoveryStateAndControlContractsMarshalStableFields(t *testing.T) {
	discovery := DiscoveryDocument{
		APIVersion: APIVersionV1,
		Instance:   InstanceDocument{ID: "instance-a", Name: "symphony", Version: "dev"},
		DomainID:   "default",
		Source:     SourceDocument{Kind: SourceKindLinear, Name: "linear-main"},
		Capabilities: StaticCapabilitySet{
			Capabilities: []StaticCapability{
				{Name: CapabilityStreamEvents, Category: CapabilityCategoryProtocol, Summary: "支持 HTTP/SSE 正式事件流。", Supported: true},
				{Name: CapabilityQueryObjects, Category: CapabilityCategoryQuery, Summary: "支持正式对象查询。", Supported: true},
			},
		},
	}
	state := ServiceStateSnapshot{
		GeneratedAt:        "2026-03-14T00:00:00Z",
		ServiceMode:        ServiceModeDegraded,
		RecoveryInProgress: true,
		Reasons:            []Reason{MustReason(ReasonServiceRecoveryInProgress, map[string]any{"phase": "restore"})},
		Instance:           InstanceStateSummary{ID: "instance-a", Name: "symphony", Version: "dev", Role: InstanceRoleLeader},
		Leader:             &LeaderHint{ID: "instance-a", Name: "symphony", URL: "http://127.0.0.1:8080"},
		Capabilities: AvailableCapabilitySet{
			Capabilities: []AvailableCapability{
				{Name: CapabilityStreamEvents, Category: CapabilityCategoryProtocol, Summary: "支持 HTTP/SSE 正式事件流。", Available: true},
				{Name: CapabilitySourceClosure, Category: CapabilityCategorySource, Summary: "支持 SourceClosureAction 收口外部来源。", Available: false, Reasons: []Reason{MustReason(ReasonCapabilityCurrentlyUnavailable, map[string]any{"capability": string(CapabilitySourceClosure)})}},
			},
		},
	}
	control := ControlResult{
		Action:              ControlActionRefresh,
		Status:              ControlStatusRejected,
		Reason:              ptrReason(MustReason(ReasonControlRefreshRejectedServiceMode, map[string]any{"service_mode": "unavailable"})),
		RecommendedNextStep: "检查核心依赖后重试",
		Timestamp:           "2026-03-14T00:00:01Z",
	}

	assertJSONHasKeys(t, discovery, []string{"api_version", "instance", "domain_id", "source", "capabilities"})
	assertJSONHasKeys(t, state, []string{"generated_at", "service_mode", "recovery_in_progress", "reasons", "instance", "leader", "capabilities"})
	assertJSONHasKeys(t, control, []string{"action", "status", "reason", "recommended_next_step", "timestamp"})
}

func TestEventContractsMarshalStableFields(t *testing.T) {
	event := EventEnvelope{
		EventID:         "evt-1",
		EventType:       EventTypeServiceStateChanged,
		Timestamp:       "2026-03-14T00:00:02Z",
		ContractVersion: APIVersionV1,
		DomainID:        "default",
		ServiceMode:     ServiceModeServing,
		Objects:         []EventObject{{ObjectType: ObjectTypeAction, ObjectID: "act-1", State: string(ActionStatusExternalPending), Visibility: VisibilityLevelRestricted}},
		Reason:          ptrReason(MustReason(ReasonActionExternalPending, map[string]any{"action_type": string(ActionTypeSourceClosure)})),
	}

	assertJSONHasKeys(t, event, []string{"event_id", "event_type", "timestamp", "contract_version", "domain_id", "service_mode", "objects", "reason"})
}

func assertJSONHasKeys(t *testing.T, value any, keys []string) {
	t.Helper()

	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	for _, key := range keys {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("json payload missing key %q: %s", key, string(raw))
		}
	}
}

func ptrReason(reason Reason) *Reason {
	return &reason
}
