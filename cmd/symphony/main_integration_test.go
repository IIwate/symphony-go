package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
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
				RecordID:  "rec_linear_1",
				SourceRef: contract.SourceRef{SourceKind: contract.SourceKindLinear, SourceID: "1", SourceIdentifier: "ABC-1"},
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
			RecordID:  "rec_linear_1",
			SourceRef: contract.SourceRef{SourceKind: contract.SourceKindLinear, SourceID: "1", SourceIdentifier: "ABC-1"},
			Status:    contract.IssueStatusAwaitingMerge,
			UpdatedAt: "2026-03-14T00:00:01Z",
			Reason: func() *contract.Reason {
				reason := contract.MustReason(contract.ReasonRecordBlockedAwaitingMerge, map[string]any{
					"record_id": "rec_linear_1",
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
			Kind: contract.SourceKindLinear,
			Name: "linear-main",
		},
		ServiceMode:        contract.ServiceModeServing,
		RecoveryInProgress: false,
		Capabilities: contract.CapabilityDocument{
			EventProtocol:  "sse",
			ControlActions: []contract.ControlAction{contract.ControlActionRefresh},
			Notifications:  []string{"webhook"},
			Sources:        []contract.SourceKind{contract.SourceKindLinear},
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
