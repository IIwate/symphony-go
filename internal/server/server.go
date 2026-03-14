package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"reflect"
	"sort"
	"strings"

	"symphony-go/internal/model/contract"
	"symphony-go/internal/orchestrator"
)

type RuntimeSource interface {
	Discovery() orchestrator.DiscoveryDocument
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

func Start(runtime RuntimeSource, logger *slog.Logger, host string, port int) (*Server, error) {
	if logger == nil {
		logger = slog.Default()
	}
	handler := NewHandler(runtime, logger)

	listener, err := net.Listen("tcp", net.JoinHostPort(normalizeListenHost(host), fmt.Sprintf("%d", port)))
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
	mux.HandleFunc("/api/v1/discovery", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w, logger, http.MethodGet)
			return
		}
		writeJSON(w, http.StatusOK, runtime.Discovery(), logger)
	})
	mux.HandleFunc("/api/v1/state", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w, logger, http.MethodGet)
			return
		}
		writeJSON(w, http.StatusOK, runtime.Snapshot(), logger)
	})
	mux.HandleFunc("/api/v1/control/refresh", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeMethodNotAllowed(w, logger, http.MethodPost)
			return
		}
		payload := runtime.RequestRefresh()
		status := http.StatusOK
		if payload.Status == contract.ControlStatusAccepted {
			status = http.StatusAccepted
		}
		writeJSON(w, status, payload, logger)
	})
	mux.HandleFunc("/api/v1/events", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w, logger, http.MethodGet)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			writeError(w, http.StatusServiceUnavailable, contract.ErrorServiceUnavailable, "response writer does not support streaming", logger, map[string]any{
				"path": r.URL.Path,
			})
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		updates, unsubscribe := runtime.SubscribeSnapshots(8)
		defer unsubscribe()

		builder := &sseEventBuilder{}
		for {
			select {
			case <-r.Context().Done():
				return
			case snapshot, ok := <-updates:
				if !ok {
					return
				}
				event, emit := builder.Next(snapshot)
				if !emit {
					continue
				}
				if err := writeSSEEvent(w, string(event.EventType), event); err != nil {
					return
				}
				flusher.Flush()
			}
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, contract.ErrorAPINotFound, "resource not found", logger, map[string]any{
			"path": r.URL.Path,
		})
	})

	return mux
}

type sseEventBuilder struct {
	sequence uint64
	previous *orchestrator.Snapshot
}

func (b *sseEventBuilder) Next(snapshot orchestrator.Snapshot) (*contract.EventEnvelope, bool) {
	if b.previous != nil && equivalentSnapshots(*b.previous, snapshot) {
		copySnapshot := snapshot
		b.previous = &copySnapshot
		return nil, false
	}

	b.sequence++
	eventType := contract.EventTypeStateChanged
	recordIDs := changedRecordIDs(snapshot, b.previous)
	reason := eventReason(snapshot, b.previous, recordIDs)
	if b.previous == nil {
		eventType = contract.EventTypeSnapshot
	}

	event := &contract.EventEnvelope{
		EventID:     fmt.Sprintf("evt-%d", b.sequence),
		EventType:   eventType,
		Timestamp:   snapshot.GeneratedAt,
		ServiceMode: snapshot.ServiceMode,
		RecordIDs:   recordIDs,
		Reason:      reason,
	}

	copySnapshot := snapshot
	b.previous = &copySnapshot
	return event, true
}

func equivalentSnapshots(left orchestrator.Snapshot, right orchestrator.Snapshot) bool {
	return left.ServiceMode == right.ServiceMode &&
		left.RecoveryInProgress == right.RecoveryInProgress &&
		reflect.DeepEqual(left.Reasons, right.Reasons) &&
		reflect.DeepEqual(left.Counts, right.Counts) &&
		reflect.DeepEqual(left.Records, right.Records) &&
		reflect.DeepEqual(left.CompletedWindow, right.CompletedWindow)
}

func changedRecordIDs(current orchestrator.Snapshot, previous *orchestrator.Snapshot) []contract.RecordID {
	currentRecords := snapshotRecordIndex(current)
	if previous == nil {
		return sortedRecordIDs(currentRecords)
	}

	previousRecords := snapshotRecordIndex(*previous)
	changed := make([]contract.RecordID, 0, len(currentRecords)+len(previousRecords))
	seen := map[contract.RecordID]struct{}{}
	for recordID, currentRecord := range currentRecords {
		previousRecord, ok := previousRecords[recordID]
		if ok && reflect.DeepEqual(previousRecord, currentRecord) {
			continue
		}
		changed = append(changed, recordID)
		seen[recordID] = struct{}{}
	}
	for recordID := range previousRecords {
		if _, ok := currentRecords[recordID]; ok {
			continue
		}
		if _, ok := seen[recordID]; ok {
			continue
		}
		changed = append(changed, recordID)
	}
	sort.SliceStable(changed, func(i int, j int) bool {
		return changed[i] < changed[j]
	})
	return changed
}

func snapshotRecordIndex(snapshot orchestrator.Snapshot) map[contract.RecordID]contract.IssueRuntimeRecord {
	records := make(map[contract.RecordID]contract.IssueRuntimeRecord, len(snapshot.Records)+len(snapshot.CompletedWindow.Records))
	for _, record := range snapshot.Records {
		records[record.RecordID] = record
	}
	for _, record := range snapshot.CompletedWindow.Records {
		records[record.RecordID] = record
	}
	return records
}

func sortedRecordIDs(records map[contract.RecordID]contract.IssueRuntimeRecord) []contract.RecordID {
	ids := make([]contract.RecordID, 0, len(records))
	for recordID := range records {
		ids = append(ids, recordID)
	}
	sort.SliceStable(ids, func(i int, j int) bool {
		return ids[i] < ids[j]
	})
	return ids
}

func eventReason(current orchestrator.Snapshot, previous *orchestrator.Snapshot, recordIDs []contract.RecordID) *contract.Reason {
	if previous == nil {
		if len(current.Reasons) == 0 {
			return nil
		}
		return cloneReason(&current.Reasons[0])
	}
	if current.ServiceMode != previous.ServiceMode || !reflect.DeepEqual(current.Reasons, previous.Reasons) {
		if len(current.Reasons) > 0 {
			return cloneReason(&current.Reasons[0])
		}
		if len(previous.Reasons) > 0 {
			return cloneReason(&previous.Reasons[0])
		}
		return nil
	}
	if len(recordIDs) != 1 {
		return nil
	}

	recordID := recordIDs[0]
	currentRecords := snapshotRecordIndex(current)
	if record, ok := currentRecords[recordID]; ok && record.Reason != nil {
		return cloneReason(record.Reason)
	}
	previousRecords := snapshotRecordIndex(*previous)
	if record, ok := previousRecords[recordID]; ok && record.Reason != nil {
		return cloneReason(record.Reason)
	}
	return nil
}

func cloneReason(reason *contract.Reason) *contract.Reason {
	if reason == nil {
		return nil
	}
	copyReason := *reason
	if reason.Details != nil {
		copyReason.Details = map[string]any{}
		for key, value := range reason.Details {
			copyReason.Details[key] = value
		}
	}
	return &copyReason
}

func normalizeListenHost(host string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return "127.0.0.1"
	}
	return host
}

func writeSSEEvent(w io.Writer, event string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, body)
	return err
}

func writeJSON(w http.ResponseWriter, status int, payload any, logger *slog.Logger) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil && logger != nil {
		logger.Error("write json response", "error", err.Error())
	}
}

func writeError(w http.ResponseWriter, status int, code contract.ErrorCode, message string, logger *slog.Logger, details map[string]any) {
	writeJSON(w, status, contract.MustErrorResponse(code, message, details), logger)
}

func writeMethodNotAllowed(w http.ResponseWriter, logger *slog.Logger, allowed ...string) {
	for _, method := range allowed {
		if method != "" {
			w.Header().Add("Allow", method)
		}
	}
	writeError(w, http.StatusMethodNotAllowed, contract.ErrorAPIMethodNotAllowed, "method not allowed", logger, nil)
}
