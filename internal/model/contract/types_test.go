package contract

import (
	"encoding/json"
	"testing"
)

func TestServiceModeAndIssueStatusAreClosedSets(t *testing.T) {
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

	statuses := []IssueStatus{
		IssueStatusActive,
		IssueStatusRetryScheduled,
		IssueStatusAwaitingMerge,
		IssueStatusAwaitingIntervention,
		IssueStatusCompleted,
	}
	for _, status := range statuses {
		if !status.IsValid() {
			t.Fatalf("IssueStatus(%q).IsValid() = false", status)
		}
	}
	if IssueStatus("running").IsValid() {
		t.Fatal(`IssueStatus("running").IsValid() = true, want false`)
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
	record := IssueRuntimeRecord{
		RecordID: "rec_linear_linear-main_linear-123",
		SourceRef: SourceRef{
			SourceKind:       SourceKindLinear,
			SourceName:       "linear-main",
			SourceID:         "linear-123",
			SourceIdentifier: "ENG-123",
			URL:              "https://linear.app/example/issue/ENG-123",
		},
		Status:    IssueStatusAwaitingIntervention,
		UpdatedAt: "2026-03-14T00:00:00Z",
		Reason: &Reason{
			ReasonCode: ReasonRecordBlockedRecoveryUncertain,
			Category:   CategoryRecord,
			Details:    map[string]any{"step": "restore"},
		},
		Observation: &Observation{
			Running: false,
			Summary: "恢复后未确认 runner 状态",
			Details: map[string]any{"runner_seen": false},
		},
		DurableRefs: DurableRefs{
			Workspace:  &WorkspaceRef{Path: "H:/workspaces/ENG-123"},
			Branch:     &BranchRef{Name: "feature/eng-123"},
			LedgerPath: "automation/local/runtime-ledger.json",
		},
		Result: nil,
	}

	discovery := DiscoveryDocument{
		APIVersion:         APIVersionV1,
		Instance:           InstanceDocument{ID: "instance-a", Name: "symphony", Version: "dev"},
		Source:             SourceDocument{Kind: SourceKindLinear, Name: "linear-main"},
		ServiceMode:        ServiceModeDegraded,
		RecoveryInProgress: true,
		Capabilities: CapabilityDocument{
			EventProtocol:  "sse",
			ControlActions: []ControlAction{ControlActionRefresh},
			Notifications:  []string{"webhook", "slack"},
			Sources:        []SourceKind{SourceKindLinear},
		},
		Reasons: []Reason{MustReason(ReasonServiceRecoveryInProgress, map[string]any{"phase": "restore"})},
		Limits:  LimitDocument{CompletedWindowSize: 100},
	}
	state := ServiceStateSnapshot{
		GeneratedAt:        "2026-03-14T00:00:00Z",
		ServiceMode:        ServiceModeDegraded,
		RecoveryInProgress: true,
		Reasons:            []Reason{MustReason(ReasonServiceRecoveryInProgress, map[string]any{"phase": "restore"})},
		Counts: StateCounts{
			Total:                1,
			Active:               0,
			RetryScheduled:       0,
			AwaitingMerge:        0,
			AwaitingIntervention: 1,
			Completed:            0,
		},
		Records:         []IssueRuntimeRecord{record},
		CompletedWindow: CompletedWindow{Limit: 100, Records: nil},
	}
	control := ControlResult{
		Action:              ControlActionRefresh,
		Status:              ControlStatusRejected,
		Reason:              ptrReason(MustReason(ReasonControlRefreshRejectedServiceMode, map[string]any{"service_mode": "unavailable"})),
		RecommendedNextStep: "检查核心依赖后重试",
		Timestamp:           "2026-03-14T00:00:01Z",
	}

	assertJSONHasKeys(t, discovery, []string{"api_version", "instance", "source", "service_mode", "recovery_in_progress", "capabilities", "reasons", "limits"})
	assertJSONHasKeys(t, state, []string{"generated_at", "service_mode", "recovery_in_progress", "reasons", "counts", "records", "completed_window"})
	assertJSONHasKeys(t, control, []string{"action", "status", "reason", "recommended_next_step", "timestamp"})
}

func TestLedgerAndEventContractsMarshalStableFields(t *testing.T) {
	retryDueAt := "2026-03-14T00:10:00Z"
	ledger := IssueLedgerRecord{
		RecordID: "rec_linear_linear-main_linear-123",
		SourceRef: SourceRef{
			SourceKind:       SourceKindLinear,
			SourceName:       "linear-main",
			SourceID:         "linear-123",
			SourceIdentifier: "ENG-123",
			URL:              "https://linear.app/example/issue/ENG-123",
		},
		Status:      IssueStatusRetryScheduled,
		Reason:      ptrReason(MustReason(ReasonRecordBlockedRetryScheduled, map[string]any{"attempt": 2})),
		RetryDueAt:  &retryDueAt,
		DurableRefs: DurableRefs{LedgerPath: "automation/local/runtime-ledger.json"},
		Result:      nil,
		UpdatedAt:   "2026-03-14T00:00:00Z",
	}
	event := EventEnvelope{
		EventID:     "evt-1",
		EventType:   EventTypeStateChanged,
		Timestamp:   "2026-03-14T00:00:02Z",
		ServiceMode: ServiceModeServing,
		RecordIDs:   []RecordID{"rec_linear_linear-main_linear-123"},
		Reason:      ptrReason(MustReason(ReasonRecordBlockedAwaitingMerge, map[string]any{"record_id": "rec_linear_linear-main_linear-123"})),
	}

	assertJSONHasKeys(t, ledger, []string{"record_id", "source_ref", "status", "reason", "retry_due_at", "durable_refs", "result", "updated_at"})
	assertJSONHasKeys(t, event, []string{"event_id", "event_type", "timestamp", "service_mode", "record_ids", "reason"})
	assertJSONHasKeys(t, ledger.SourceRef, []string{"source_kind", "source_name", "source_id", "source_identifier", "url"})
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
