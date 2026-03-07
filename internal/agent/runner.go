package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"symphony-go/internal/model"
	"symphony-go/internal/workflow"
)

const maxProtocolLineSize = 10 * 1024 * 1024

type Runner interface {
	Run(ctx context.Context, params RunParams) error
}

type RunParams struct {
	Issue          *model.Issue
	Attempt        *int
	WorkspacePath  string
	PromptTemplate string
	MaxTurns       int
	RefetchIssue   func(context.Context, string) (*model.Issue, error)
	IsActive       func(string) bool
	OnEvent        func(AgentEvent)
}

type AgentEvent struct {
	Event             string
	Timestamp         time.Time
	CodexAppServerPID *string
	SessionID         *string
	ThreadID          *string
	TurnID            *string
	Usage             *TokenUsage
	RateLimits        any
	Payload           any
	Message           string
}

type TokenUsage struct {
	InputTokens  int64
	OutputTokens int64
	TotalTokens  int64
}

type ProcessFactory interface {
	StartProcess(ctx context.Context, cwd string, command string) (Process, error)
}

type Process interface {
	Stdin() io.WriteCloser
	Stdout() io.ReadCloser
	Stderr() io.ReadCloser
	Wait() error
	Kill() error
	PID() int
}

type AppServerRunner struct {
	configProvider func() *model.ServiceConfig
	logger         *slog.Logger
	processFactory ProcessFactory
	httpClient     *http.Client
	now            func() time.Time
}

func NewRunner(configProvider func() *model.ServiceConfig, logger *slog.Logger, processFactory ProcessFactory) Runner {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if processFactory == nil {
		processFactory = execProcessFactory{}
	}

	return &AppServerRunner{
		configProvider: configProvider,
		logger:         logger,
		processFactory: processFactory,
		httpClient:     &http.Client{Timeout: 30 * time.Second},
		now:            time.Now,
	}
}

func (r *AppServerRunner) Run(ctx context.Context, params RunParams) error {
	if params.Issue == nil {
		return model.NewAgentError(model.ErrResponseError, "issue is nil", nil)
	}
	if !filepath.IsAbs(params.WorkspacePath) {
		return model.NewAgentError(model.ErrInvalidWorkspaceCWD, fmt.Sprintf("workspace path %q is not absolute", params.WorkspacePath), nil)
	}

	cfg := r.config()
	process, err := r.processFactory.StartProcess(ctx, params.WorkspacePath, cfg.CodexCommand)
	if err != nil {
		return model.NewAgentError(model.ErrCodexNotFound, "start codex app-server", err)
	}
	defer r.stopProcess(process)

	pidValue := strconv.Itoa(process.PID())
	pid := &pidValue

	stdoutCh, stdoutErrCh := scanLines(process.Stdout())
	stderrCh, stderrErrCh := scanLines(process.Stderr())
	waitCh := make(chan error, 1)
	go func() {
		waitCh <- process.Wait()
	}()

	writer := process.Stdin()
	defer writer.Close()

	requestID := 1
	if err := writeJSONLine(writer, map[string]any{
		"id":     requestID,
		"method": "initialize",
		"params": map[string]any{
			"clientInfo":   map[string]any{"name": "symphony-orchestrator", "version": "1.0"},
			"capabilities": map[string]any{"experimentalApi": true},
		},
	}); err != nil {
		return model.NewAgentError(model.ErrResponseError, "write initialize request", err)
	}
	if _, err := r.waitForResponse(ctx, cfg.CodexReadTimeoutMS, requestID, writer, stdoutCh, stdoutErrCh, stderrCh, stderrErrCh, waitCh, params, pid); err != nil {
		return err
	}

	if err := writeJSONLine(writer, map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		return model.NewAgentError(model.ErrResponseError, "write initialized notification", err)
	}

	requestID++
	threadResponse, err := r.waitThreadStart(ctx, cfg, requestID, writer, stdoutCh, stdoutErrCh, stderrCh, stderrErrCh, waitCh, params, pid)
	if err != nil {
		return err
	}
	threadID, _ := nestedString(threadResponse, "result", "thread", "id")
	if strings.TrimSpace(threadID) == "" {
		return model.NewAgentError(model.ErrResponseError, "thread/start response missing thread id", nil)
	}

	issue := cloneIssue(params.Issue)
	maxTurns := params.MaxTurns
	if maxTurns <= 0 {
		maxTurns = cfg.MaxTurns
	}
	for turnNumber := 1; turnNumber <= maxTurns; turnNumber++ {
		prompt, err := buildTurnPrompt(params, issue, turnNumber, maxTurns)
		if err != nil {
			return model.NewAgentError(model.ErrResponseError, "build turn prompt", err)
		}

		requestID++
		turnResponse, err := r.waitTurnStart(ctx, cfg, requestID, writer, stdoutCh, stdoutErrCh, stderrCh, stderrErrCh, waitCh, params, pid, threadID, issue, prompt)
		if err != nil {
			return err
		}
		turnID, _ := nestedString(turnResponse, "result", "turn", "id")
		if strings.TrimSpace(turnID) == "" {
			return model.NewAgentError(model.ErrResponseError, "turn/start response missing turn id", nil)
		}
		sessionID := threadID + "-" + turnID
		r.emit(params, AgentEvent{
			Event:             "session_started",
			Timestamp:         r.now().UTC(),
			CodexAppServerPID: pid,
			ThreadID:          stringPtr(threadID),
			TurnID:            stringPtr(turnID),
			SessionID:         stringPtr(sessionID),
			Message:           sessionID,
		})

		if err := r.waitForTurnEnd(ctx, cfg.CodexTurnTimeoutMS, writer, stdoutCh, stdoutErrCh, stderrCh, stderrErrCh, waitCh, params, pid, threadID, turnID, sessionID); err != nil {
			return err
		}

		if params.RefetchIssue == nil || params.IsActive == nil {
			break
		}
		refreshed, err := params.RefetchIssue(ctx, issue.ID)
		if err != nil {
			return model.NewAgentError(model.ErrResponseError, "refresh issue state after turn", err)
		}
		if refreshed == nil {
			break
		}
		issue = cloneIssue(refreshed)
		if !params.IsActive(issue.State) {
			break
		}
		if turnNumber >= maxTurns {
			break
		}
	}

	return nil
}

func (r *AppServerRunner) waitThreadStart(ctx context.Context, cfg *model.ServiceConfig, requestID int, writer io.Writer, stdoutCh <-chan string, stdoutErrCh <-chan error, stderrCh <-chan string, stderrErrCh <-chan error, waitCh <-chan error, params RunParams, pid *string) (map[string]any, error) {
	request := map[string]any{
		"id":     requestID,
		"method": "thread/start",
		"params": map[string]any{
			"approvalPolicy": cfg.CodexApprovalPolicy,
			"sandbox":        cfg.CodexThreadSandbox,
			"cwd":            params.WorkspacePath,
		},
	}
	if tools := r.dynamicTools(cfg); len(tools) > 0 {
		requestParams := request["params"].(map[string]any)
		requestParams["dynamicTools"] = tools
	}
	if err := writeJSONLine(writer, request); err != nil {
		return nil, model.NewAgentError(model.ErrResponseError, "write thread/start request", err)
	}

	return r.waitForResponse(ctx, cfg.CodexReadTimeoutMS, requestID, writer, stdoutCh, stdoutErrCh, stderrCh, stderrErrCh, waitCh, params, pid)
}

func (r *AppServerRunner) waitTurnStart(ctx context.Context, cfg *model.ServiceConfig, requestID int, writer io.Writer, stdoutCh <-chan string, stdoutErrCh <-chan error, stderrCh <-chan string, stderrErrCh <-chan error, waitCh <-chan error, params RunParams, pid *string, threadID string, issue *model.Issue, prompt string) (map[string]any, error) {
	request := map[string]any{
		"id":     requestID,
		"method": "turn/start",
		"params": map[string]any{
			"threadId":       threadID,
			"input":          []any{map[string]any{"type": "text", "text": prompt}},
			"cwd":            params.WorkspacePath,
			"title":          issue.Identifier + ": " + issue.Title,
			"approvalPolicy": cfg.CodexApprovalPolicy,
			"sandboxPolicy":  json.RawMessage(cfg.CodexTurnSandboxPolicy),
		},
	}
	if err := writeJSONLine(writer, request); err != nil {
		return nil, model.NewAgentError(model.ErrResponseError, "write turn/start request", err)
	}

	return r.waitForResponse(ctx, cfg.CodexReadTimeoutMS, requestID, writer, stdoutCh, stdoutErrCh, stderrCh, stderrErrCh, waitCh, params, pid)
}

func (r *AppServerRunner) waitForResponse(ctx context.Context, timeoutMS int, expectedID int, writer io.Writer, stdoutCh <-chan string, stdoutErrCh <-chan error, stderrCh <-chan string, stderrErrCh <-chan error, waitCh <-chan error, params RunParams, pid *string) (map[string]any, error) {
	timer := time.NewTimer(time.Duration(timeoutMS) * time.Millisecond)
	defer timer.Stop()
	var waitErr error
	processExited := false

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			return nil, model.NewAgentError(model.ErrResponseTimeout, fmt.Sprintf("response timeout waiting for request id %d", expectedID), nil)
		case err, ok := <-stdoutErrCh:
			if ok && err != nil {
				return nil, model.NewAgentError(model.ErrResponseError, "read stdout", err)
			}
		case err, ok := <-stderrErrCh:
			if ok && err != nil {
				return nil, model.NewAgentError(model.ErrResponseError, "read stderr", err)
			}
		case line, ok := <-stderrCh:
			if ok {
				r.emit(params, AgentEvent{Event: "notification", Timestamp: r.now().UTC(), CodexAppServerPID: pid, Message: strings.TrimSpace(line)})
			}
		case waitErr = <-waitCh:
			processExited = true
		case line, ok := <-stdoutCh:
			if !ok {
				if processExited {
					return nil, model.NewAgentError(model.ErrPortExit, "stdout closed before response", waitErr)
				}
				return nil, model.NewAgentError(model.ErrPortExit, "stdout closed before response", nil)
			}
			message, err := parseJSONLine(line)
			if err != nil {
				r.emit(params, AgentEvent{Event: "malformed", Timestamp: r.now().UTC(), CodexAppServerPID: pid, Message: line, Payload: err.Error()})
				continue
			}
			if responseID, ok := intID(message["id"]); ok && responseID == expectedID {
				if _, hasError := message["error"]; hasError {
					return nil, model.NewAgentError(model.ErrResponseError, fmt.Sprintf("request id %d returned error", expectedID), nil)
				}
				return message, nil
			}
			if eventErr, terminal := r.handleStreamMessage(writer, message, params, pid, "", "", ""); eventErr != nil {
				return nil, eventErr
			} else if terminal {
				return nil, model.NewAgentError(model.ErrResponseError, "turn terminated before response completed", nil)
			}
		}
	}
}

func (r *AppServerRunner) waitForTurnEnd(ctx context.Context, timeoutMS int, writer io.Writer, stdoutCh <-chan string, stdoutErrCh <-chan error, stderrCh <-chan string, stderrErrCh <-chan error, waitCh <-chan error, params RunParams, pid *string, threadID string, turnID string, sessionID string) error {
	timer := time.NewTimer(time.Duration(timeoutMS) * time.Millisecond)
	defer timer.Stop()
	var waitErr error
	processExited := false

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return model.NewAgentError(model.ErrTurnTimeout, fmt.Sprintf("turn %s timed out", turnID), nil)
		case err, ok := <-stdoutErrCh:
			if ok && err != nil {
				return model.NewAgentError(model.ErrResponseError, "read stdout", err)
			}
		case err, ok := <-stderrErrCh:
			if ok && err != nil {
				return model.NewAgentError(model.ErrResponseError, "read stderr", err)
			}
		case line, ok := <-stderrCh:
			if ok {
				r.emit(params, AgentEvent{Event: "notification", Timestamp: r.now().UTC(), CodexAppServerPID: pid, SessionID: stringPtr(sessionID), ThreadID: stringPtr(threadID), TurnID: stringPtr(turnID), Message: strings.TrimSpace(line)})
			}
		case waitErr = <-waitCh:
			processExited = true
		case line, ok := <-stdoutCh:
			if !ok {
				if processExited {
					return model.NewAgentError(model.ErrPortExit, "stdout closed during turn", waitErr)
				}
				return model.NewAgentError(model.ErrPortExit, "stdout closed during turn", nil)
			}
			message, err := parseJSONLine(line)
			if err != nil {
				r.emit(params, AgentEvent{Event: "malformed", Timestamp: r.now().UTC(), CodexAppServerPID: pid, SessionID: stringPtr(sessionID), ThreadID: stringPtr(threadID), TurnID: stringPtr(turnID), Message: line, Payload: err.Error()})
				continue
			}
			if eventErr, terminal := r.handleStreamMessage(writer, message, params, pid, threadID, turnID, sessionID); eventErr != nil {
				return eventErr
			} else if terminal {
				return nil
			}
		}
	}
}

func (r *AppServerRunner) handleStreamMessage(writer io.Writer, message map[string]any, params RunParams, pid *string, threadID string, turnID string, sessionID string) (error, bool) {
	timestamp := r.now().UTC()
	method, _ := message["method"].(string)
	usage := extractUsage(message)
	rateLimits := extractRateLimits(message)

	if usage != nil || rateLimits != nil {
		r.emit(params, AgentEvent{
			Event:             "notification",
			Timestamp:         timestamp,
			CodexAppServerPID: pid,
			SessionID:         optionalPtr(sessionID),
			ThreadID:          optionalPtr(threadID),
			TurnID:            optionalPtr(turnID),
			Usage:             usage,
			RateLimits:        rateLimits,
			Message:           method,
			Payload:           message,
		})
	}

	lowerMethod := strings.ToLower(method)
	if strings.Contains(lowerMethod, "requestuserinput") {
		r.emit(params, AgentEvent{Event: "turn_input_required", Timestamp: timestamp, CodexAppServerPID: pid, SessionID: optionalPtr(sessionID), ThreadID: optionalPtr(threadID), TurnID: optionalPtr(turnID), Message: method, Payload: message})
		return model.NewAgentError(model.ErrTurnInputRequired, "turn requested user input", nil), false
	}
	if strings.Contains(lowerMethod, "approval") && message["id"] != nil {
		_ = writeJSONLine(writer, map[string]any{"id": message["id"], "result": map[string]any{"approved": true}})
		r.emit(params, AgentEvent{Event: "approval_auto_approved", Timestamp: timestamp, CodexAppServerPID: pid, SessionID: optionalPtr(sessionID), ThreadID: optionalPtr(threadID), TurnID: optionalPtr(turnID), Message: method, Payload: message})
		return nil, false
	}
	if strings.Contains(lowerMethod, "tool/call") && message["id"] != nil {
		toolResult, eventName := r.handleToolCall(message)
		_ = writeJSONLine(writer, map[string]any{"id": message["id"], "result": toolResult})
		r.emit(params, AgentEvent{Event: eventName, Timestamp: timestamp, CodexAppServerPID: pid, SessionID: optionalPtr(sessionID), ThreadID: optionalPtr(threadID), TurnID: optionalPtr(turnID), Message: method, Payload: toolResult})
		return nil, false
	}

	switch lowerMethod {
	case "turn/completed":
		r.emit(params, AgentEvent{Event: "turn_completed", Timestamp: timestamp, CodexAppServerPID: pid, SessionID: optionalPtr(sessionID), ThreadID: optionalPtr(threadID), TurnID: optionalPtr(turnID), Usage: usage, Message: method, Payload: message})
		return nil, true
	case "turn/failed":
		r.emit(params, AgentEvent{Event: "turn_failed", Timestamp: timestamp, CodexAppServerPID: pid, SessionID: optionalPtr(sessionID), ThreadID: optionalPtr(threadID), TurnID: optionalPtr(turnID), Usage: usage, Message: method, Payload: message})
		return model.NewAgentError(model.ErrTurnFailed, "turn failed", nil), false
	case "turn/cancelled":
		r.emit(params, AgentEvent{Event: "turn_cancelled", Timestamp: timestamp, CodexAppServerPID: pid, SessionID: optionalPtr(sessionID), ThreadID: optionalPtr(threadID), TurnID: optionalPtr(turnID), Usage: usage, Message: method, Payload: message})
		return model.NewAgentError(model.ErrTurnCancelled, "turn cancelled", nil), false
	}

	if method != "" {
		r.emit(params, AgentEvent{Event: "other_message", Timestamp: timestamp, CodexAppServerPID: pid, SessionID: optionalPtr(sessionID), ThreadID: optionalPtr(threadID), TurnID: optionalPtr(turnID), Usage: usage, Message: method, Payload: message})
	}

	return nil, false
}

func (r *AppServerRunner) stopProcess(process Process) {
	if process == nil {
		return
	}
	_ = process.Stdin().Close()
	_ = process.Kill()
}

func (r *AppServerRunner) config() *model.ServiceConfig {
	if r.configProvider == nil {
		return &model.ServiceConfig{}
	}
	cfg := r.configProvider()
	if cfg == nil {
		return &model.ServiceConfig{}
	}
	return cfg
}

func (r *AppServerRunner) dynamicTools(cfg *model.ServiceConfig) []any {
	if cfg == nil || model.NormalizeState(cfg.TrackerKind) != "linear" || strings.TrimSpace(cfg.TrackerAPIKey) == "" {
		return nil
	}
	return []any{
		map[string]any{
			"name":        "linear_graphql",
			"description": "Execute a single Linear GraphQL operation using Symphony runtime auth.",
			"inputSchema": map[string]any{
				"type":     "object",
				"required": []string{"query"},
				"properties": map[string]any{
					"query":     map[string]any{"type": "string"},
					"variables": map[string]any{"type": "object"},
				},
			},
		},
	}
}

func (r *AppServerRunner) handleToolCall(message map[string]any) (map[string]any, string) {
	toolName, arguments, ok := extractToolCall(message)
	if !ok {
		return map[string]any{"success": false, "error": "unsupported_tool_call"}, "unsupported_tool_call"
	}
	if toolName != "linear_graphql" {
		return map[string]any{"success": false, "error": "unsupported_tool_call"}, "unsupported_tool_call"
	}
	result := r.executeLinearGraphQL(arguments)
	return result, "notification"
}

func (r *AppServerRunner) executeLinearGraphQL(arguments any) map[string]any {
	query, variables, err := parseLinearGraphQLInput(arguments)
	if err != nil {
		return map[string]any{"success": false, "error": "invalid_arguments", "message": err.Error()}
	}
	cfg := r.config()
	if model.NormalizeState(cfg.TrackerKind) != "linear" || strings.TrimSpace(cfg.TrackerAPIKey) == "" {
		return map[string]any{"success": false, "error": "missing_auth", "message": "linear auth is not configured"}
	}

	body, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return map[string]any{"success": false, "error": "invalid_arguments", "message": err.Error()}
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, cfg.TrackerEndpoint, bytes.NewReader(body))
	if err != nil {
		return map[string]any{"success": false, "error": "transport_failure", "message": err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", cfg.TrackerAPIKey)

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return map[string]any{"success": false, "error": "transport_failure", "message": err.Error()}
	}
	defer resp.Body.Close()
	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return map[string]any{"success": false, "error": "transport_failure", "message": err.Error()}
	}
	var decoded any
	if err := json.Unmarshal(rawBody, &decoded); err != nil {
		return map[string]any{"success": false, "error": "invalid_response", "message": err.Error()}
	}
	if resp.StatusCode != http.StatusOK {
		return map[string]any{"success": false, "error": "http_status", "status": resp.StatusCode, "body": decoded}
	}
	if envelope, ok := decoded.(map[string]any); ok {
		if errorsValue, exists := envelope["errors"]; exists && errorsValue != nil {
			if list, ok := errorsValue.([]any); ok && len(list) > 0 {
				return map[string]any{"success": false, "error": "linear_graphql_errors", "body": decoded}
			}
		}
	}
	return map[string]any{"success": true, "body": decoded}
}

func extractToolCall(message map[string]any) (string, any, bool) {
	params, ok := message["params"].(map[string]any)
	if !ok {
		return "", nil, false
	}
	toolName, _ := params["name"].(string)
	if toolName == "" {
		if rawTool, ok := params["tool"].(map[string]any); ok {
			toolName, _ = rawTool["name"].(string)
		}
	}
	arguments := firstNonNil(params["arguments"], params["input"], params["payload"])
	if arguments == nil {
		arguments = params
	}
	return toolName, arguments, toolName != ""
}

func parseLinearGraphQLInput(arguments any) (string, map[string]any, error) {
	if text, ok := arguments.(string); ok {
		query := strings.TrimSpace(text)
		if query == "" {
			return "", nil, fmt.Errorf("query must be a non-empty string")
		}
		if !hasSingleGraphQLOperation(query) {
			return "", nil, fmt.Errorf("query must contain exactly one GraphQL operation")
		}
		return query, nil, nil
	}
	argsMap, ok := arguments.(map[string]any)
	if !ok {
		return "", nil, fmt.Errorf("arguments must be an object")
	}
	query, _ := argsMap["query"].(string)
	query = strings.TrimSpace(query)
	if query == "" {
		return "", nil, fmt.Errorf("query must be a non-empty string")
	}
	if !hasSingleGraphQLOperation(query) {
		return "", nil, fmt.Errorf("query must contain exactly one GraphQL operation")
	}
	if variablesValue, ok := argsMap["variables"]; ok && variablesValue != nil {
		variables, ok := variablesValue.(map[string]any)
		if !ok {
			return "", nil, fmt.Errorf("variables must be an object")
		}
		return query, variables, nil
	}
	return query, nil, nil
}

func hasSingleGraphQLOperation(query string) bool {
	lower := strings.ToLower(query)
	count := 0
	for _, keyword := range []string{"query", "mutation", "subscription"} {
		count += strings.Count(lower, keyword+" ")
		count += strings.Count(lower, keyword+"\n")
		count += strings.Count(lower, keyword+"{")
	}
	if count == 0 {
		return true
	}
	return count == 1
}

func (r *AppServerRunner) emit(params RunParams, event AgentEvent) {
	if params.OnEvent != nil {
		params.OnEvent(event)
	}
	if r.logger != nil {
		r.logger.Debug("agent event", "event", event.Event, "message", event.Message)
	}
}

func buildTurnPrompt(params RunParams, issue *model.Issue, turnNumber int, maxTurns int) (string, error) {
	if turnNumber == 1 {
		return workflow.RenderPrompt(params.PromptTemplate, issue, params.Attempt)
	}

	return fmt.Sprintf("Continue working on issue %s (%d/%d turns). Re-check progress and finish only if the issue no longer needs active work.", issue.Identifier, turnNumber, maxTurns), nil
}

func cloneIssue(issue *model.Issue) *model.Issue {
	if issue == nil {
		return nil
	}
	copyValue := *issue
	copyValue.Labels = append([]string(nil), issue.Labels...)
	copyValue.BlockedBy = append([]model.BlockerRef(nil), issue.BlockedBy...)
	return &copyValue
}

func optionalPtr(value string) *string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return stringPtr(value)
}

func stringPtr(value string) *string {
	copyValue := value
	return &copyValue
}

func writeJSONLine(writer io.Writer, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = writer.Write(append(raw, '\n'))
	return err
}

func parseJSONLine(line string) (map[string]any, error) {
	var message map[string]any
	decoder := json.NewDecoder(strings.NewReader(line))
	decoder.UseNumber()
	if err := decoder.Decode(&message); err != nil {
		return nil, err
	}
	return message, nil
}

func scanLines(reader io.ReadCloser) (<-chan string, <-chan error) {
	lines := make(chan string, 16)
	errs := make(chan error, 1)
	go func() {
		defer close(lines)
		defer close(errs)
		defer reader.Close()

		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 1024), maxProtocolLineSize)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		if err := scanner.Err(); err != nil {
			errs <- err
		}
	}()
	return lines, errs
}

func nestedString(root map[string]any, path ...string) (string, bool) {
	var current any = root
	for _, part := range path {
		mapping, ok := current.(map[string]any)
		if !ok {
			return "", false
		}
		current, ok = mapping[part]
		if !ok {
			return "", false
		}
	}
	text, ok := current.(string)
	return text, ok
}

func intID(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		return int(typed), true
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil {
			return 0, false
		}
		return int(parsed), true
	default:
		return 0, false
	}
}

func extractUsage(payload any) *TokenUsage {
	result, ok := findUsage(payload)
	if !ok {
		return nil
	}
	return &result
}

func findUsage(payload any) (TokenUsage, bool) {
	switch typed := payload.(type) {
	case map[string]any:
		if usage, ok := usageFromMap(typed); ok {
			return usage, true
		}
		for _, value := range typed {
			if usage, ok := findUsage(value); ok {
				return usage, true
			}
		}
	case []any:
		for _, value := range typed {
			if usage, ok := findUsage(value); ok {
				return usage, true
			}
		}
	}
	return TokenUsage{}, false
}

func usageFromMap(mapping map[string]any) (TokenUsage, bool) {
	input, okInput := int64FromAny(firstNonNil(mapping["inputTokens"], mapping["input_tokens"]))
	output, okOutput := int64FromAny(firstNonNil(mapping["outputTokens"], mapping["output_tokens"]))
	total, okTotal := int64FromAny(firstNonNil(mapping["totalTokens"], mapping["total_tokens"]))
	if okInput || okOutput || okTotal {
		return TokenUsage{InputTokens: input, OutputTokens: output, TotalTokens: total}, true
	}
	return TokenUsage{}, false
}

func extractRateLimits(payload any) any {
	switch typed := payload.(type) {
	case map[string]any:
		for key, value := range typed {
			lower := strings.ToLower(key)
			if strings.Contains(lower, "ratelimit") || strings.Contains(lower, "rate_limit") {
				return value
			}
			if nested := extractRateLimits(value); nested != nil {
				return nested
			}
		}
	case []any:
		for _, value := range typed {
			if nested := extractRateLimits(value); nested != nil {
				return nested
			}
		}
	}
	return nil
}

func firstNonNil(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func int64FromAny(value any) (int64, bool) {
	switch typed := value.(type) {
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	case float64:
		return int64(typed), true
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

type execProcessFactory struct{}

func (execProcessFactory) StartProcess(ctx context.Context, cwd string, command string) (Process, error) {
	cmd := exec.CommandContext(ctx, "bash", "-lc", command)
	cmd.Dir = cwd
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &execProcess{cmd: cmd, stdin: stdin, stdout: stdout, stderr: stderr}, nil
}

type execProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser
}

func (p *execProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *execProcess) Stdout() io.ReadCloser { return p.stdout }
func (p *execProcess) Stderr() io.ReadCloser { return p.stderr }
func (p *execProcess) Wait() error           { return p.cmd.Wait() }
func (p *execProcess) PID() int              { return p.cmd.Process.Pid }

func (p *execProcess) Kill() error {
	if p.cmd.Process == nil {
		return nil
	}
	err := p.cmd.Process.Kill()
	if err != nil && errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}
