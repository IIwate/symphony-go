package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"symphony-go/internal/model/contract"
	"symphony-go/internal/orchestrator"
)

type RuntimeSource interface {
	Discovery() orchestrator.DiscoveryDocument
	Snapshot() orchestrator.Snapshot
	RequestRefresh() orchestrator.RefreshRequestResult
	SubscribeEvents(buffer int) (<-chan contract.EventEnvelope, func())
	GetObject(objectType contract.ObjectType, id string) (orchestrator.ObjectEnvelope, bool)
	ListObjects(objectType contract.ObjectType) []orchestrator.ObjectEnvelope
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
		state := runtime.Snapshot()
		if state.Instance.Role != contract.InstanceRoleLeader {
			details := map[string]any{
				"role": state.Instance.Role,
			}
			if state.Leader != nil {
				details["leader"] = state.Leader
			}
			writeError(w, http.StatusConflict, contract.ErrorAPILeaderRequired, "write control is only available on leader", logger, details)
			return
		}
		payload := runtime.RequestRefresh()
		status := http.StatusOK
		if payload.Status == contract.ControlStatusAccepted {
			status = http.StatusAccepted
		}
		writeJSON(w, status, payload, logger)
	})
	mux.HandleFunc("/api/v1/objects/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w, logger, http.MethodGet)
			return
		}
		trimmed := strings.TrimPrefix(r.URL.Path, "/api/v1/objects/")
		parts := strings.Split(strings.Trim(trimmed, "/"), "/")
		if len(parts) == 0 || parts[0] == "" {
			writeError(w, http.StatusNotFound, contract.ErrorAPINotFound, "resource not found", logger, map[string]any{"path": r.URL.Path})
			return
		}
		objectType := contract.ObjectType(parts[0])
		if !objectType.IsValid() || !objectType.SupportsObjectQuery() {
			writeError(w, http.StatusBadRequest, contract.ErrorAPIInvalidRequest, "unsupported object query type", logger, map[string]any{"object_type": parts[0]})
			return
		}
		if len(parts) == 1 {
			items := runtime.ListObjects(objectType)
			payload := contract.ObjectListResponse{
				ObjectType: objectType,
				Items:      make([]json.RawMessage, 0, len(items)),
			}
			for _, item := range items {
				payload.Items = append(payload.Items, item.Payload)
			}
			writeJSON(w, http.StatusOK, payload, logger)
			return
		}
		if len(parts) != 2 || strings.TrimSpace(parts[1]) == "" {
			writeError(w, http.StatusBadRequest, contract.ErrorAPIInvalidRequest, "invalid object query path", logger, map[string]any{"path": r.URL.Path})
			return
		}
		item, ok := runtime.GetObject(objectType, parts[1])
		if !ok {
			writeError(w, http.StatusNotFound, contract.ErrorAPINotFound, "object not found", logger, map[string]any{"object_type": objectType, "object_id": parts[1]})
			return
		}
		writeJSON(w, http.StatusOK, contract.ObjectQueryResponse{ObjectType: objectType, Item: item.Payload}, logger)
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

		updates, unsubscribe := runtime.SubscribeEvents(8)
		defer unsubscribe()
		for {
			select {
			case <-r.Context().Done():
				return
			case event, ok := <-updates:
				if !ok {
					return
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
