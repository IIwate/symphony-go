package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"symphony-go/internal/orchestrator"
)

type RuntimeSource interface {
	Snapshot() orchestrator.Snapshot
	RequestRefresh() orchestrator.RefreshRequestResult
	SubscribeSnapshots(buffer int) (<-chan orchestrator.Snapshot, func())
}

type Server struct {
	logger   *slog.Logger
	runtime  RuntimeSource
	httpSrv  *http.Server
	listener net.Listener
}

func Start(runtime RuntimeSource, logger *slog.Logger, port int) (*Server, error) {
	if logger == nil {
		logger = slog.Default()
	}
	handler := NewHandler(runtime, logger)

	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}

	httpSrv := &http.Server{Handler: handler}
	server := &Server{
		logger:   logger,
		runtime:  runtime,
		httpSrv:  httpSrv,
		listener: listener,
	}
	go func() {
		if err := httpSrv.Serve(listener); err != nil && err != http.ErrServerClosed {
			logger.Error("http server failed", "error", err.Error())
		}
	}()
	return server, nil
}

func (s *Server) Addr() string {
	if s == nil || s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

func (s *Server) Shutdown(ctx context.Context) error {
	if s == nil || s.httpSrv == nil {
		return nil
	}
	return s.httpSrv.Shutdown(ctx)
}

func NewHandler(runtime RuntimeSource, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w, logger)
			return
		}
		writeDashboard(w)
	})
	mux.HandleFunc("/api/v1/state", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w, logger)
			return
		}
		writeJSON(w, http.StatusOK, toStateResponse(runtime.Snapshot()), logger)
	})
	mux.HandleFunc("/api/v1/refresh", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeMethodNotAllowed(w, logger)
			return
		}
		result := runtime.RequestRefresh()
		if !result.Accepted {
			writeError(w, http.StatusConflict, result.RejectedCode, result.RejectedMessage, logger)
			return
		}
		payload := map[string]any{
			"accepted":     result.Accepted,
			"coalesced":    result.Coalesced,
			"requested_at": result.RequestedAt.UTC().Format(time.RFC3339),
			"operations":   result.Operations,
		}
		writeJSON(w, http.StatusAccepted, payload, logger)
	})
	mux.HandleFunc("/api/v1/events", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w, logger)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			writeError(w, http.StatusInternalServerError, "stream_not_supported", "response writer does not support flushing", logger)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		updates, unsubscribe := runtime.SubscribeSnapshots(4)
		defer unsubscribe()

		first := true
		for {
			select {
			case <-r.Context().Done():
				return
			case snapshot, ok := <-updates:
				if !ok {
					return
				}
				eventName := "update"
				if first {
					eventName = "snapshot"
					first = false
				}
				if err := writeSSEEvent(w, eventName, toStateResponse(snapshot)); err != nil {
					return
				}
				flusher.Flush()
			}
		}
	})
	mux.HandleFunc("/api/v1/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w, logger)
			return
		}
		identifier := strings.TrimPrefix(r.URL.Path, "/api/v1/")
		if identifier == "" || strings.Contains(identifier, "/") {
			http.NotFound(w, r)
			return
		}
		response, ok := findIssueResponse(runtime.Snapshot(), identifier)
		if !ok {
			writeError(w, http.StatusNotFound, "issue_not_found", "issue not found", logger)
			return
		}
		writeJSON(w, http.StatusOK, response, logger)
	})

	return mux
}

type stateResponse struct {
	GeneratedAt  string               `json:"generated_at"`
	Service      serviceResponse      `json:"service"`
	Counts       stateCounts          `json:"counts"`
	Running      []runningResponse    `json:"running"`
	Recovery     recoveryResponse     `json:"recovery"`
	Health       healthResponse       `json:"health"`
	Observations observationsResponse `json:"observations"`
	CodexTotals  totalsResponse       `json:"codex_totals"`
	RateLimits   any                  `json:"rate_limits"`
}

type serviceResponse struct {
	Version          string  `json:"version"`
	StartedAt        string  `json:"started_at"`
	UptimeSeconds    float64 `json:"uptime_seconds"`
	Mode             string  `json:"mode"`
	ProtectionReason string  `json:"protection_reason,omitempty"`
	ProtectedAt      *string `json:"protected_at"`
	RestartRequired  bool    `json:"restart_required"`
}

type stateCounts struct {
	Recovering           int `json:"recovering"`
	Running              int `json:"running"`
	AwaitingMerge        int `json:"awaiting_merge"`
	AwaitingIntervention int `json:"awaiting_intervention"`
	Retrying             int `json:"retrying"`
}

type recoveryResponse struct {
	Recovering           []recoveringResponse           `json:"recovering"`
	AwaitingMerge        []awaitingMergeResponse        `json:"awaiting_merge"`
	AwaitingIntervention []awaitingInterventionResponse `json:"awaiting_intervention"`
	Retrying             []retryResponse                `json:"retrying"`
}

type healthResponse struct {
	Alerts        []alertResponse                     `json:"alerts"`
	Notifications []notificationChannelHealthResponse `json:"notifications"`
	Persistence   persistenceHealthResponse           `json:"persistence"`
}

type observationsResponse struct {
	Derived          []observationResponse     `json:"derived"`
	ProtectedResults []protectedResultResponse `json:"protected_results"`
}

type runningResponse struct {
	IssueID             string         `json:"issue_id"`
	IssueIdentifier     string         `json:"issue_identifier"`
	State               string         `json:"state"`
	DispatchKind        string         `json:"dispatch_kind,omitempty"`
	ExpectedOutcome     string         `json:"expected_outcome,omitempty"`
	ContinuationReason  *string        `json:"continuation_reason"`
	SessionID           string         `json:"session_id"`
	TurnCount           int            `json:"turn_count"`
	LastEvent           string         `json:"last_event"`
	LastMessage         string         `json:"last_message"`
	StartedAt           string         `json:"started_at"`
	LastEventAt         *string        `json:"last_event_at"`
	Tokens              totalsResponse `json:"tokens"`
	CurrentRetryAttempt int            `json:"current_retry_attempt"`
}

type recoveringResponse struct {
	IssueID            string  `json:"issue_id"`
	IssueIdentifier    string  `json:"issue_identifier"`
	WorkspacePath      string  `json:"workspace_path"`
	State              string  `json:"state,omitempty"`
	Strategy           string  `json:"strategy"`
	Source             string  `json:"source,omitempty"`
	DispatchKind       string  `json:"dispatch_kind,omitempty"`
	ExpectedOutcome    string  `json:"expected_outcome,omitempty"`
	ContinuationReason *string `json:"continuation_reason"`
	ObservedAt         string  `json:"observed_at"`
	AttemptCount       int     `json:"attempt_count"`
}

type retryResponse struct {
	IssueID            string  `json:"issue_id"`
	IssueIdentifier    string  `json:"issue_identifier"`
	DispatchKind       string  `json:"dispatch_kind,omitempty"`
	ExpectedOutcome    string  `json:"expected_outcome,omitempty"`
	ContinuationReason *string `json:"continuation_reason"`
	Attempt            int     `json:"attempt"`
	DueAt              string  `json:"due_at"`
	Error              *string `json:"error"`
}

type awaitingMergeResponse struct {
	IssueID         string  `json:"issue_id"`
	IssueIdentifier string  `json:"issue_identifier"`
	WorkspacePath   string  `json:"workspace_path"`
	State           string  `json:"state"`
	Branch          string  `json:"branch"`
	PRNumber        int     `json:"pr_number"`
	PRURL           string  `json:"pr_url"`
	PRState         string  `json:"pr_state"`
	AwaitingSince   string  `json:"awaiting_since"`
	LastError       *string `json:"last_error"`
}

type awaitingInterventionResponse struct {
	IssueID             string `json:"issue_id"`
	IssueIdentifier     string `json:"issue_identifier"`
	WorkspacePath       string `json:"workspace_path"`
	Branch              string `json:"branch"`
	PRNumber            int    `json:"pr_number"`
	PRURL               string `json:"pr_url"`
	PRState             string `json:"pr_state"`
	Reason              string `json:"reason,omitempty"`
	ExpectedOutcome     string `json:"expected_outcome,omitempty"`
	PreviousBranch      string `json:"previous_branch,omitempty"`
	LastKnownIssueState string `json:"last_known_issue_state,omitempty"`
	ObservedAt          string `json:"observed_at"`
}

type alertResponse struct {
	Code            string `json:"code"`
	Level           string `json:"level"`
	Message         string `json:"message"`
	IssueID         string `json:"issue_id,omitempty"`
	IssueIdentifier string `json:"issue_identifier,omitempty"`
}

type observationResponse struct {
	Code            string `json:"code"`
	Level           string `json:"level"`
	Message         string `json:"message"`
	IssueID         string `json:"issue_id,omitempty"`
	IssueIdentifier string `json:"issue_identifier,omitempty"`
}

type protectedResultResponse struct {
	IssueID             string  `json:"issue_id"`
	IssueIdentifier     string  `json:"issue_identifier"`
	WorkspacePath       string  `json:"workspace_path,omitempty"`
	Outcome             string  `json:"outcome"`
	Phase               string  `json:"phase"`
	Error               *string `json:"error"`
	FinalBranch         string  `json:"final_branch,omitempty"`
	DispatchKind        string  `json:"dispatch_kind,omitempty"`
	ExpectedOutcome     string  `json:"expected_outcome,omitempty"`
	ContinuationReason  *string `json:"continuation_reason"`
	ObservedAt          string  `json:"observed_at"`
	CurrentRetryAttempt int     `json:"current_retry_attempt"`
	AttemptCount        int     `json:"attempt_count"`
}

type notificationChannelHealthResponse struct {
	ChannelID           string  `json:"channel_id"`
	DisplayName         string  `json:"display_name,omitempty"`
	Status              string  `json:"status"`
	QueueOverflow       bool    `json:"queue_overflow"`
	LastError           *string `json:"last_error"`
	LastAttemptAt       *string `json:"last_attempt_at"`
	LastSuccessAt       *string `json:"last_success_at"`
	ConsecutiveFailures int     `json:"consecutive_failures"`
}

type persistenceHealthResponse struct {
	Enabled             bool    `json:"enabled"`
	Kind                string  `json:"kind,omitempty"`
	Status              string  `json:"status,omitempty"`
	LastError           *string `json:"last_error"`
	LastAttemptAt       *string `json:"last_attempt_at"`
	LastSuccessAt       *string `json:"last_success_at"`
	ConsecutiveFailures int     `json:"consecutive_failures"`
}

type totalsResponse struct {
	InputTokens    int64   `json:"input_tokens"`
	OutputTokens   int64   `json:"output_tokens"`
	TotalTokens    int64   `json:"total_tokens"`
	SecondsRunning float64 `json:"seconds_running"`
}

type issueResponse struct {
	GeneratedAt          string                        `json:"generated_at"`
	Identifier           string                        `json:"identifier"`
	Status               string                        `json:"status"`
	WorkspacePath        string                        `json:"workspace_path,omitempty"`
	LastError            *string                       `json:"last_error"`
	AttemptCount         int                           `json:"attempt_count"`
	Running              *runningResponse              `json:"running"`
	Recovering           *recoveringResponse           `json:"recovering"`
	AwaitingMerge        *awaitingMergeResponse        `json:"awaiting_merge"`
	AwaitingIntervention *awaitingInterventionResponse `json:"awaiting_intervention"`
	Retry                *retryResponse                `json:"retry"`
	ProtectedResult      *protectedResultResponse      `json:"protected_result"`
}

func toStateResponse(snapshot orchestrator.Snapshot) stateResponse {
	serviceStartedAt := ""
	uptimeSeconds := 0.0
	if !snapshot.Service.StartedAt.IsZero() {
		serviceStartedAt = snapshot.Service.StartedAt.UTC().Format(time.RFC3339)
		uptimeSeconds = snapshot.GeneratedAt.Sub(snapshot.Service.StartedAt).Seconds()
		if uptimeSeconds < 0 {
			uptimeSeconds = 0
		}
	}
	var protectedAt *string
	if snapshot.Service.ProtectedAt != nil {
		text := snapshot.Service.ProtectedAt.UTC().Format(time.RFC3339)
		protectedAt = &text
	}

	running := make([]runningResponse, 0, len(snapshot.Running))
	for _, item := range snapshot.Running {
		var lastEventAt *string
		if item.LastEventAt != nil {
			text := item.LastEventAt.UTC().Format(time.RFC3339)
			lastEventAt = &text
		}
		running = append(running, runningResponse{
			IssueID:             item.IssueID,
			IssueIdentifier:     item.IssueIdentifier,
			State:               item.State,
			DispatchKind:        item.DispatchKind,
			ExpectedOutcome:     item.ExpectedOutcome,
			ContinuationReason:  item.ContinuationReason,
			SessionID:           item.SessionID,
			TurnCount:           item.TurnCount,
			LastEvent:           item.LastEvent,
			LastMessage:         item.LastMessage,
			StartedAt:           item.StartedAt.UTC().Format(time.RFC3339),
			LastEventAt:         lastEventAt,
			CurrentRetryAttempt: item.CurrentRetryAttempt,
			Tokens: totalsResponse{
				InputTokens:  item.InputTokens,
				OutputTokens: item.OutputTokens,
				TotalTokens:  item.TotalTokens,
			},
		})
	}

	recoveringItems := make([]recoveringResponse, 0, len(snapshot.Recovery.Recovering))
	for _, item := range snapshot.Recovery.Recovering {
		recoveringItems = append(recoveringItems, recoveringResponse{
			IssueID:            item.IssueID,
			IssueIdentifier:    item.IssueIdentifier,
			WorkspacePath:      item.WorkspacePath,
			State:              item.State,
			Strategy:           item.Strategy,
			Source:             item.Source,
			DispatchKind:       item.DispatchKind,
			ExpectedOutcome:    item.ExpectedOutcome,
			ContinuationReason: item.ContinuationReason,
			ObservedAt:         item.ObservedAt.UTC().Format(time.RFC3339),
			AttemptCount:       item.AttemptCount,
		})
	}

	awaitingMerge := make([]awaitingMergeResponse, 0, len(snapshot.Recovery.AwaitingMerge))
	for _, item := range snapshot.Recovery.AwaitingMerge {
		awaitingMerge = append(awaitingMerge, awaitingMergeResponse{
			IssueID:         item.IssueID,
			IssueIdentifier: item.IssueIdentifier,
			WorkspacePath:   item.WorkspacePath,
			State:           item.State,
			Branch:          item.Branch,
			PRNumber:        item.PRNumber,
			PRURL:           item.PRURL,
			PRState:         item.PRState,
			AwaitingSince:   item.AwaitingSince.UTC().Format(time.RFC3339),
			LastError:       item.LastError,
		})
	}

	awaitingIntervention := make([]awaitingInterventionResponse, 0, len(snapshot.Recovery.AwaitingIntervention))
	for _, item := range snapshot.Recovery.AwaitingIntervention {
		awaitingIntervention = append(awaitingIntervention, awaitingInterventionResponse{
			IssueID:             item.IssueID,
			IssueIdentifier:     item.IssueIdentifier,
			WorkspacePath:       item.WorkspacePath,
			Branch:              item.Branch,
			PRNumber:            item.PRNumber,
			PRURL:               item.PRURL,
			PRState:             item.PRState,
			Reason:              item.Reason,
			ExpectedOutcome:     item.ExpectedOutcome,
			PreviousBranch:      item.PreviousBranch,
			LastKnownIssueState: item.LastKnownIssueState,
			ObservedAt:          item.ObservedAt.UTC().Format(time.RFC3339),
		})
	}

	retrying := make([]retryResponse, 0, len(snapshot.Recovery.Retrying))
	for _, item := range snapshot.Recovery.Retrying {
		retrying = append(retrying, retryResponse{
			IssueID:            item.IssueID,
			IssueIdentifier:    item.IssueIdentifier,
			DispatchKind:       item.DispatchKind,
			ExpectedOutcome:    item.ExpectedOutcome,
			ContinuationReason: item.ContinuationReason,
			Attempt:            item.Attempt,
			DueAt:              item.DueAt.UTC().Format(time.RFC3339),
			Error:              item.Error,
		})
	}

	alerts := make([]alertResponse, 0, len(snapshot.Health.Alerts))
	for _, item := range snapshot.Health.Alerts {
		alerts = append(alerts, alertResponse{
			Code:            item.Code,
			Level:           item.Level,
			Message:         item.Message,
			IssueID:         item.IssueID,
			IssueIdentifier: item.IssueIdentifier,
		})
	}

	notifications := make([]notificationChannelHealthResponse, 0, len(snapshot.Health.Notifications))
	for _, item := range snapshot.Health.Notifications {
		var lastAttemptAt *string
		if item.LastAttemptAt != nil {
			text := item.LastAttemptAt.UTC().Format(time.RFC3339)
			lastAttemptAt = &text
		}
		var lastSuccessAt *string
		if item.LastSuccessAt != nil {
			text := item.LastSuccessAt.UTC().Format(time.RFC3339)
			lastSuccessAt = &text
		}
		notifications = append(notifications, notificationChannelHealthResponse{
			ChannelID:           item.ChannelID,
			DisplayName:         item.DisplayName,
			Status:              item.Status,
			QueueOverflow:       item.QueueOverflow,
			LastError:           item.LastError,
			LastAttemptAt:       lastAttemptAt,
			LastSuccessAt:       lastSuccessAt,
			ConsecutiveFailures: item.ConsecutiveFailures,
		})
	}

	var persistenceAttemptAt *string
	if snapshot.Health.Persistence.LastAttemptAt != nil {
		text := snapshot.Health.Persistence.LastAttemptAt.UTC().Format(time.RFC3339)
		persistenceAttemptAt = &text
	}
	var persistenceSuccessAt *string
	if snapshot.Health.Persistence.LastSuccessAt != nil {
		text := snapshot.Health.Persistence.LastSuccessAt.UTC().Format(time.RFC3339)
		persistenceSuccessAt = &text
	}
	derived := make([]observationResponse, 0, len(snapshot.Observations.Derived))
	for _, item := range snapshot.Observations.Derived {
		derived = append(derived, observationResponse{
			Code:            item.Code,
			Level:           item.Level,
			Message:         item.Message,
			IssueID:         item.IssueID,
			IssueIdentifier: item.IssueIdentifier,
		})
	}
	protectedResults := make([]protectedResultResponse, 0, len(snapshot.Observations.ProtectedResults))
	for _, item := range snapshot.Observations.ProtectedResults {
		protectedResults = append(protectedResults, protectedResultResponse{
			IssueID:             item.IssueID,
			IssueIdentifier:     item.IssueIdentifier,
			WorkspacePath:       item.WorkspacePath,
			Outcome:             item.Outcome,
			Phase:               item.Phase,
			Error:               item.Error,
			FinalBranch:         item.FinalBranch,
			DispatchKind:        item.DispatchKind,
			ExpectedOutcome:     item.ExpectedOutcome,
			ContinuationReason:  item.ContinuationReason,
			ObservedAt:          item.ObservedAt.UTC().Format(time.RFC3339),
			CurrentRetryAttempt: item.CurrentRetryAttempt,
			AttemptCount:        item.AttemptCount,
		})
	}

	return stateResponse{
		GeneratedAt: snapshot.GeneratedAt.UTC().Format(time.RFC3339),
		Service: serviceResponse{
			Version:          snapshot.Service.Version,
			StartedAt:        serviceStartedAt,
			UptimeSeconds:    uptimeSeconds,
			Mode:             string(snapshot.Service.Mode),
			ProtectionReason: snapshot.Service.ProtectionReason,
			ProtectedAt:      protectedAt,
			RestartRequired:  snapshot.Service.RestartRequired,
		},
		Counts: stateCounts{
			Recovering:           snapshot.Counts.Recovering,
			Running:              snapshot.Counts.Running,
			AwaitingMerge:        snapshot.Counts.AwaitingMerge,
			AwaitingIntervention: snapshot.Counts.AwaitingIntervention,
			Retrying:             snapshot.Counts.Retrying,
		},
		Running: running,
		Recovery: recoveryResponse{
			Recovering:           recoveringItems,
			AwaitingMerge:        awaitingMerge,
			AwaitingIntervention: awaitingIntervention,
			Retrying:             retrying,
		},
		Health: healthResponse{
			Alerts:        alerts,
			Notifications: notifications,
			Persistence: persistenceHealthResponse{
				Enabled:             snapshot.Health.Persistence.Enabled,
				Kind:                snapshot.Health.Persistence.Kind,
				Status:              snapshot.Health.Persistence.Status,
				LastError:           snapshot.Health.Persistence.LastError,
				LastAttemptAt:       persistenceAttemptAt,
				LastSuccessAt:       persistenceSuccessAt,
				ConsecutiveFailures: snapshot.Health.Persistence.ConsecutiveFailures,
			},
		},
		Observations: observationsResponse{
			Derived:          derived,
			ProtectedResults: protectedResults,
		},
		CodexTotals: totalsResponse{
			InputTokens:    snapshot.CodexTotals.InputTokens,
			OutputTokens:   snapshot.CodexTotals.OutputTokens,
			TotalTokens:    snapshot.CodexTotals.TotalTokens,
			SecondsRunning: snapshot.CodexTotals.SecondsRunning,
		},
		RateLimits: snapshot.RateLimits,
	}
}

func findIssueResponse(snapshot orchestrator.Snapshot, identifier string) (issueResponse, bool) {
	for _, item := range snapshot.Running {
		var runningLastEventAt *string
		if item.LastEventAt != nil {
			text := item.LastEventAt.UTC().Format(time.RFC3339)
			runningLastEventAt = &text
		}
		if item.IssueIdentifier == identifier {
			copyItem := runningResponse{
				IssueID:             item.IssueID,
				IssueIdentifier:     item.IssueIdentifier,
				State:               item.State,
				DispatchKind:        item.DispatchKind,
				ExpectedOutcome:     item.ExpectedOutcome,
				ContinuationReason:  item.ContinuationReason,
				SessionID:           item.SessionID,
				TurnCount:           item.TurnCount,
				LastEvent:           item.LastEvent,
				LastMessage:         item.LastMessage,
				StartedAt:           item.StartedAt.UTC().Format(time.RFC3339),
				LastEventAt:         runningLastEventAt,
				CurrentRetryAttempt: item.CurrentRetryAttempt,
				Tokens: totalsResponse{
					InputTokens:  item.InputTokens,
					OutputTokens: item.OutputTokens,
					TotalTokens:  item.TotalTokens,
				},
			}
			return issueResponse{
				GeneratedAt:   snapshot.GeneratedAt.UTC().Format(time.RFC3339),
				Identifier:    identifier,
				Status:        "running",
				WorkspacePath: item.WorkspacePath,
				AttemptCount:  item.AttemptCount,
				Running:       &copyItem,
			}, true
		}
	}
	for _, item := range snapshot.Recovery.Recovering {
		if item.IssueIdentifier == identifier {
			copyItem := recoveringResponse{
				IssueID:            item.IssueID,
				IssueIdentifier:    item.IssueIdentifier,
				WorkspacePath:      item.WorkspacePath,
				State:              item.State,
				Strategy:           item.Strategy,
				Source:             item.Source,
				DispatchKind:       item.DispatchKind,
				ExpectedOutcome:    item.ExpectedOutcome,
				ContinuationReason: item.ContinuationReason,
				ObservedAt:         item.ObservedAt.UTC().Format(time.RFC3339),
				AttemptCount:       item.AttemptCount,
			}
			return issueResponse{
				GeneratedAt:   snapshot.GeneratedAt.UTC().Format(time.RFC3339),
				Identifier:    identifier,
				Status:        "recovering",
				WorkspacePath: item.WorkspacePath,
				AttemptCount:  item.AttemptCount,
				Recovering:    &copyItem,
			}, true
		}
	}
	for _, item := range snapshot.Recovery.AwaitingMerge {
		if item.IssueIdentifier == identifier {
			copyItem := awaitingMergeResponse{
				IssueID:         item.IssueID,
				IssueIdentifier: item.IssueIdentifier,
				WorkspacePath:   item.WorkspacePath,
				State:           item.State,
				Branch:          item.Branch,
				PRNumber:        item.PRNumber,
				PRURL:           item.PRURL,
				PRState:         item.PRState,
				AwaitingSince:   item.AwaitingSince.UTC().Format(time.RFC3339),
				LastError:       item.LastError,
			}
			return issueResponse{
				GeneratedAt:   snapshot.GeneratedAt.UTC().Format(time.RFC3339),
				Identifier:    identifier,
				Status:        "awaiting_merge",
				WorkspacePath: item.WorkspacePath,
				LastError:     item.LastError,
				AttemptCount:  item.AttemptCount,
				AwaitingMerge: &copyItem,
			}, true
		}
	}
	for _, item := range snapshot.Recovery.AwaitingIntervention {
		if item.IssueIdentifier == identifier {
			copyItem := awaitingInterventionResponse{
				IssueID:             item.IssueID,
				IssueIdentifier:     item.IssueIdentifier,
				WorkspacePath:       item.WorkspacePath,
				Branch:              item.Branch,
				PRNumber:            item.PRNumber,
				PRURL:               item.PRURL,
				PRState:             item.PRState,
				Reason:              item.Reason,
				ExpectedOutcome:     item.ExpectedOutcome,
				PreviousBranch:      item.PreviousBranch,
				LastKnownIssueState: item.LastKnownIssueState,
				ObservedAt:          item.ObservedAt.UTC().Format(time.RFC3339),
			}
			return issueResponse{
				GeneratedAt:          snapshot.GeneratedAt.UTC().Format(time.RFC3339),
				Identifier:           identifier,
				Status:               "awaiting_intervention",
				WorkspacePath:        item.WorkspacePath,
				AttemptCount:         item.AttemptCount,
				AwaitingIntervention: &copyItem,
			}, true
		}
	}
	for _, item := range snapshot.Recovery.Retrying {
		if item.IssueIdentifier == identifier {
			copyItem := retryResponse{
				IssueID:            item.IssueID,
				IssueIdentifier:    item.IssueIdentifier,
				DispatchKind:       item.DispatchKind,
				ExpectedOutcome:    item.ExpectedOutcome,
				ContinuationReason: item.ContinuationReason,
				Attempt:            item.Attempt,
				DueAt:              item.DueAt.UTC().Format(time.RFC3339),
				Error:              item.Error,
			}
			return issueResponse{
				GeneratedAt:   snapshot.GeneratedAt.UTC().Format(time.RFC3339),
				Identifier:    identifier,
				Status:        "retrying",
				WorkspacePath: item.WorkspacePath,
				LastError:     item.Error,
				AttemptCount:  item.Attempt,
				Retry:         &copyItem,
			}, true
		}
	}
	for _, item := range snapshot.Observations.ProtectedResults {
		if item.IssueIdentifier == identifier {
			copyItem := protectedResultResponse{
				IssueID:             item.IssueID,
				IssueIdentifier:     item.IssueIdentifier,
				WorkspacePath:       item.WorkspacePath,
				Outcome:             item.Outcome,
				Phase:               item.Phase,
				Error:               item.Error,
				FinalBranch:         item.FinalBranch,
				DispatchKind:        item.DispatchKind,
				ExpectedOutcome:     item.ExpectedOutcome,
				ContinuationReason:  item.ContinuationReason,
				ObservedAt:          item.ObservedAt.UTC().Format(time.RFC3339),
				CurrentRetryAttempt: item.CurrentRetryAttempt,
				AttemptCount:        item.AttemptCount,
			}
			return issueResponse{
				GeneratedAt:     snapshot.GeneratedAt.UTC().Format(time.RFC3339),
				Identifier:      identifier,
				Status:          "protected_result",
				WorkspacePath:   item.WorkspacePath,
				LastError:       item.Error,
				AttemptCount:    item.AttemptCount,
				ProtectedResult: &copyItem,
			}, true
		}
	}
	return issueResponse{}, false
}

func writeDashboard(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	const page = `<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <title>Symphony-Go Dashboard</title>
  <style>
    body { font-family: system-ui, sans-serif; margin: 24px; }
    pre { background: #111; color: #eee; padding: 16px; overflow: auto; }
  </style>
</head>
<body>
  <h1>Symphony-Go</h1>
  <p>运行时状态面板</p>
  <pre id="state">loading...</pre>
  <script>
    const state = document.getElementById('state');
    const render = async () => {
      const res = await fetch('/api/v1/state');
      state.textContent = JSON.stringify(await res.json(), null, 2);
    };
    render();
    const events = new EventSource('/api/v1/events');
    events.addEventListener('snapshot', (ev) => state.textContent = JSON.stringify(JSON.parse(ev.data), null, 2));
    events.addEventListener('update', (ev) => state.textContent = JSON.stringify(JSON.parse(ev.data), null, 2));
  </script>
</body>
</html>`
	_, _ = io.WriteString(w, page)
}

func writeSSEEvent(w io.Writer, event string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, "event: "+event+"\ndata: "+string(raw)+"\n\n")
	return err
}

func writeJSON(w http.ResponseWriter, status int, payload any, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		logger.Warn("http response encode failed", "status", status, "error", err.Error())
	}
}

func writeError(w http.ResponseWriter, status int, code string, message string, logger *slog.Logger) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	}, logger)
}

func writeMethodNotAllowed(w http.ResponseWriter, logger *slog.Logger) {
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", logger)
}
