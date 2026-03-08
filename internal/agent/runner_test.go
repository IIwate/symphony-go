package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"symphony-go/internal/model"
)

func TestRunnerHandshakeAndContinuationTurns(t *testing.T) {
	factory := &fakeProcessFactory{process: newFakeProcess([]string{
		jsonLine(map[string]any{"id": 1, "result": map[string]any{"ok": true}}),
		jsonLine(map[string]any{"id": 2, "result": map[string]any{"thread": map[string]any{"id": "thread-1"}}}),
		jsonLine(map[string]any{"id": 3, "result": map[string]any{"turn": map[string]any{"id": "turn-1"}}}),
		jsonLine(map[string]any{"method": "thread/tokenUsage/updated", "params": map[string]any{"tokenUsage": map[string]any{"inputTokens": 5, "outputTokens": 7, "totalTokens": 12}}}),
		jsonLine(map[string]any{"method": "turn/completed", "params": map[string]any{"message": "done"}}),
		jsonLine(map[string]any{"id": 4, "result": map[string]any{"turn": map[string]any{"id": "turn-2"}}}),
		jsonLine(map[string]any{"method": "turn/completed", "params": map[string]any{"message": "done again"}}),
	}, nil, false)}
	runner := newTestRunner(factory, 200, 200)

	var refetchCount int
	events := make([]AgentEvent, 0)
	err := runner.Run(context.Background(), RunParams{
		Issue:          &model.Issue{ID: "1", Identifier: "ABC-1", Title: "Fix bug", State: "Todo"},
		WorkspacePath:  `C:\\work\\ABC-1`,
		PromptTemplate: "Issue {{ issue.identifier }}",
		MaxTurns:       2,
		RefetchIssue: func(_ context.Context, _ string) (*model.Issue, error) {
			refetchCount++
			if refetchCount == 1 {
				return &model.Issue{ID: "1", Identifier: "ABC-1", Title: "Fix bug", State: "In Progress"}, nil
			}
			return &model.Issue{ID: "1", Identifier: "ABC-1", Title: "Fix bug", State: "Done"}, nil
		},
		IsActive: func(state string) bool { return state == "Todo" || state == "In Progress" },
		OnEvent:  func(event AgentEvent) { events = append(events, event) },
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if refetchCount != 2 {
		t.Fatalf("refetchCount = %d, want 2", refetchCount)
	}

	requests := factory.process.stdinRecorder.lines()
	if len(requests) < 5 {
		t.Fatalf("request count = %d, want >= 5", len(requests))
	}
	assertMethod(t, requests[0], "initialize")
	assertMethod(t, requests[1], "initialized")
	assertMethod(t, requests[2], "thread/start")
	assertMethod(t, requests[3], "turn/start")
	assertMethod(t, requests[4], "turn/start")

	threadStart := decodeLine(t, requests[2])
	paramsMap := threadStart["params"].(map[string]any)
	if _, ok := paramsMap["dynamicTools"]; !ok {
		t.Fatalf("thread/start missing dynamicTools: %+v", threadStart)
	}

	firstTurn := decodeLine(t, requests[3])
	secondTurn := decodeLine(t, requests[4])
	if nestedStringMust(t, firstTurn, "params", "threadId") != "thread-1" || nestedStringMust(t, secondTurn, "params", "threadId") != "thread-1" {
		t.Fatal("continuation turn did not reuse thread id")
	}
	if !containsEvent(events, "session_started") || !containsEvent(events, "turn_completed") {
		t.Fatalf("events = %+v", events)
	}

	usageFound := false
	for _, event := range events {
		if event.Usage != nil && event.Usage.TotalTokens == 12 {
			usageFound = true
		}
	}
	if !usageFound {
		t.Fatalf("usage event not found: %+v", events)
	}
}

func TestRunnerFailsOnUserInputRequest(t *testing.T) {
	factory := &fakeProcessFactory{process: newFakeProcess([]string{
		jsonLine(map[string]any{"id": 1, "result": map[string]any{"ok": true}}),
		jsonLine(map[string]any{"id": 2, "result": map[string]any{"thread": map[string]any{"id": "thread-1"}}}),
		jsonLine(map[string]any{"id": 3, "result": map[string]any{"turn": map[string]any{"id": "turn-1"}}}),
		jsonLine(map[string]any{"method": "item/tool/requestUserInput", "params": map[string]any{"prompt": "Need input"}}),
	}, nil, false)}
	runner := newTestRunner(factory, 200, 200)

	err := runner.Run(context.Background(), RunParams{
		Issue:          &model.Issue{ID: "1", Identifier: "ABC-1", Title: "Fix bug"},
		WorkspacePath:  `C:\\work\\ABC-1`,
		PromptTemplate: "Issue {{ issue.identifier }}",
	})
	if !errors.Is(err, model.ErrTurnInputRequired) {
		t.Fatalf("Run() error = %v, want ErrTurnInputRequired", err)
	}
}

func TestRunnerAutoApprovesAndRejectsUnsupportedToolCalls(t *testing.T) {
	factory := &fakeProcessFactory{process: newFakeProcess([]string{
		jsonLine(map[string]any{"id": 1, "result": map[string]any{"ok": true}}),
		jsonLine(map[string]any{"id": 2, "result": map[string]any{"thread": map[string]any{"id": "thread-1"}}}),
		jsonLine(map[string]any{"id": 3, "result": map[string]any{"turn": map[string]any{"id": "turn-1"}}}),
		jsonLine(map[string]any{"id": "approval-1", "method": "approval/request", "params": map[string]any{"kind": "shell"}}),
		jsonLine(map[string]any{"id": "tool-1", "method": "item/tool/call", "params": map[string]any{"name": "foo"}}),
		jsonLine(map[string]any{"method": "turn/completed", "params": map[string]any{}}),
	}, nil, false)}
	runner := newTestRunner(factory, 200, 200)

	err := runner.Run(context.Background(), RunParams{
		Issue:          &model.Issue{ID: "1", Identifier: "ABC-1", Title: "Fix bug"},
		WorkspacePath:  `C:\\work\\ABC-1`,
		PromptTemplate: "Issue {{ issue.identifier }}",
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	responses := factory.process.stdinRecorder.lines()
	if !containsApprovalResult(responses, "approval-1") {
		t.Fatalf("approval response missing: %v", responses)
	}
	if !containsToolFailure(responses, "tool-1") {
		t.Fatalf("tool failure response missing: %v", responses)
	}
}

func TestRunnerReadTimeout(t *testing.T) {
	factory := &fakeProcessFactory{process: newFakeProcess(nil, nil, true)}
	runner := newTestRunner(factory, 50, 200)

	err := runner.Run(context.Background(), RunParams{
		Issue:          &model.Issue{ID: "1", Identifier: "ABC-1", Title: "Fix bug"},
		WorkspacePath:  `C:\\work\\ABC-1`,
		PromptTemplate: "Issue {{ issue.identifier }}",
	})
	if !errors.Is(err, model.ErrResponseTimeout) {
		t.Fatalf("Run() error = %v, want ErrResponseTimeout", err)
	}
}

func TestRunnerLinearGraphQLToolSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "secret-key" {
			t.Fatalf("Authorization header = %q, want secret-key", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"data":{"viewer":{"id":"u1"}}}`))
	}))
	defer server.Close()

	factory := &fakeProcessFactory{process: newFakeProcess([]string{
		jsonLine(map[string]any{"id": 1, "result": map[string]any{"ok": true}}),
		jsonLine(map[string]any{"id": 2, "result": map[string]any{"thread": map[string]any{"id": "thread-1"}}}),
		jsonLine(map[string]any{"id": 3, "result": map[string]any{"turn": map[string]any{"id": "turn-1"}}}),
		jsonLine(map[string]any{"id": "tool-1", "method": "item/tool/call", "params": map[string]any{"name": "linear_graphql", "arguments": map[string]any{"query": "query Viewer { viewer { id } }"}}}),
		jsonLine(map[string]any{"method": "turn/completed", "params": map[string]any{}}),
	}, nil, false)}

	runner := newTestRunnerWithConfig(factory, &model.ServiceConfig{
		TrackerKind:            "linear",
		TrackerEndpoint:        server.URL,
		TrackerAPIKey:          "secret-key",
		CodexCommand:           "codex app-server",
		CodexApprovalPolicy:    "never",
		CodexThreadSandbox:     "workspace-write",
		CodexTurnSandboxPolicy: `{"type":"workspaceWrite"}`,
		CodexReadTimeoutMS:     200,
		CodexTurnTimeoutMS:     200,
		MaxTurns:               1,
	})

	err := runner.Run(context.Background(), RunParams{
		Issue:          &model.Issue{ID: "1", Identifier: "ABC-1", Title: "Fix bug"},
		WorkspacePath:  `C:\\work\\ABC-1`,
		PromptTemplate: "Issue {{ issue.identifier }}",
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !containsToolSuccess(factory.process.stdinRecorder.lines(), "tool-1") {
		t.Fatalf("tool success response missing: %v", factory.process.stdinRecorder.lines())
	}
}

func TestRunnerLinearGraphQLToolSuccessWithStringToolField(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"viewer":{"id":"u1"}}}`))
	}))
	defer server.Close()

	factory := &fakeProcessFactory{process: newFakeProcess([]string{
		jsonLine(map[string]any{"id": 1, "result": map[string]any{"ok": true}}),
		jsonLine(map[string]any{"id": 2, "result": map[string]any{"thread": map[string]any{"id": "thread-1"}}}),
		jsonLine(map[string]any{"id": 3, "result": map[string]any{"turn": map[string]any{"id": "turn-1"}}}),
		jsonLine(map[string]any{"id": "tool-1", "method": "item/tool/call", "params": map[string]any{"tool": "linear_graphql", "arguments": map[string]any{"query": "query Viewer { viewer { id } }"}}}),
		jsonLine(map[string]any{"method": "turn/completed", "params": map[string]any{}}),
	}, nil, false)}

	runner := newTestRunnerWithConfig(factory, &model.ServiceConfig{
		TrackerKind:            "linear",
		TrackerEndpoint:        server.URL,
		TrackerAPIKey:          "secret-key",
		CodexCommand:           "codex app-server",
		CodexApprovalPolicy:    "never",
		CodexThreadSandbox:     "workspace-write",
		CodexTurnSandboxPolicy: `{"type":"workspaceWrite"}`,
		CodexReadTimeoutMS:     200,
		CodexTurnTimeoutMS:     200,
		MaxTurns:               1,
	})

	err := runner.Run(context.Background(), RunParams{
		Issue:          &model.Issue{ID: "1", Identifier: "ABC-1", Title: "Fix bug"},
		WorkspacePath:  `C:\\work\\ABC-1`,
		PromptTemplate: "Issue {{ issue.identifier }}",
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !containsToolSuccess(factory.process.stdinRecorder.lines(), "tool-1") {
		t.Fatalf("tool success response missing: %v", factory.process.stdinRecorder.lines())
	}
}

func TestRunnerLinearGraphQLToolSuccessWithNestedMsgPayload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"viewer":{"id":"u1"}}}`))
	}))
	defer server.Close()

	factory := &fakeProcessFactory{process: newFakeProcess([]string{
		jsonLine(map[string]any{"id": 1, "result": map[string]any{"ok": true}}),
		jsonLine(map[string]any{"id": 2, "result": map[string]any{"thread": map[string]any{"id": "thread-1"}}}),
		jsonLine(map[string]any{"id": 3, "result": map[string]any{"turn": map[string]any{"id": "turn-1"}}}),
		jsonLine(map[string]any{"id": "tool-1", "method": "item/tool/call", "params": map[string]any{"msg": map[string]any{"tool": "linear_graphql", "arguments": map[string]any{"query": "query Viewer { viewer { id } }"}}}}),
		jsonLine(map[string]any{"method": "turn/completed", "params": map[string]any{}}),
	}, nil, false)}

	runner := newTestRunnerWithConfig(factory, &model.ServiceConfig{
		TrackerKind:            "linear",
		TrackerEndpoint:        server.URL,
		TrackerAPIKey:          "secret-key",
		CodexCommand:           "codex app-server",
		CodexApprovalPolicy:    "never",
		CodexThreadSandbox:     "workspace-write",
		CodexTurnSandboxPolicy: `{"type":"workspaceWrite"}`,
		CodexReadTimeoutMS:     200,
		CodexTurnTimeoutMS:     200,
		MaxTurns:               1,
	})

	err := runner.Run(context.Background(), RunParams{
		Issue:          &model.Issue{ID: "1", Identifier: "ABC-1", Title: "Fix bug"},
		WorkspacePath:  `C:\\work\\ABC-1`,
		PromptTemplate: "Issue {{ issue.identifier }}",
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !containsToolSuccess(factory.process.stdinRecorder.lines(), "tool-1") {
		t.Fatalf("tool success response missing: %v", factory.process.stdinRecorder.lines())
	}
}

func TestRunnerLinearGraphQLToolGraphQLErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"errors":[{"message":"bad query"}]}`))
	}))
	defer server.Close()

	factory := &fakeProcessFactory{process: newFakeProcess([]string{
		jsonLine(map[string]any{"id": 1, "result": map[string]any{"ok": true}}),
		jsonLine(map[string]any{"id": 2, "result": map[string]any{"thread": map[string]any{"id": "thread-1"}}}),
		jsonLine(map[string]any{"id": 3, "result": map[string]any{"turn": map[string]any{"id": "turn-1"}}}),
		jsonLine(map[string]any{"id": "tool-1", "method": "item/tool/call", "params": map[string]any{"name": "linear_graphql", "arguments": map[string]any{"query": "query Viewer { viewer { id } }"}}}),
		jsonLine(map[string]any{"method": "turn/completed", "params": map[string]any{}}),
	}, nil, false)}

	runner := newTestRunnerWithConfig(factory, &model.ServiceConfig{
		TrackerKind:            "linear",
		TrackerEndpoint:        server.URL,
		TrackerAPIKey:          "secret-key",
		CodexCommand:           "codex app-server",
		CodexApprovalPolicy:    "never",
		CodexThreadSandbox:     "workspace-write",
		CodexTurnSandboxPolicy: `{"type":"workspaceWrite"}`,
		CodexReadTimeoutMS:     200,
		CodexTurnTimeoutMS:     200,
		MaxTurns:               1,
	})

	err := runner.Run(context.Background(), RunParams{
		Issue:          &model.Issue{ID: "1", Identifier: "ABC-1", Title: "Fix bug"},
		WorkspacePath:  `C:\\work\\ABC-1`,
		PromptTemplate: "Issue {{ issue.identifier }}",
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !containsToolGraphQLError(factory.process.stdinRecorder.lines(), "tool-1") {
		t.Fatalf("tool graphql error response missing: %v", factory.process.stdinRecorder.lines())
	}
}

func TestRunnerLinearGraphQLToolInvalidArguments(t *testing.T) {
	factory := &fakeProcessFactory{process: newFakeProcess([]string{
		jsonLine(map[string]any{"id": 1, "result": map[string]any{"ok": true}}),
		jsonLine(map[string]any{"id": 2, "result": map[string]any{"thread": map[string]any{"id": "thread-1"}}}),
		jsonLine(map[string]any{"id": 3, "result": map[string]any{"turn": map[string]any{"id": "turn-1"}}}),
		jsonLine(map[string]any{"id": "tool-1", "method": "item/tool/call", "params": map[string]any{"name": "linear_graphql", "arguments": map[string]any{"query": "query A { a } query B { b }"}}}),
		jsonLine(map[string]any{"method": "turn/completed", "params": map[string]any{}}),
	}, nil, false)}
	runner := newTestRunner(factory, 200, 200)

	err := runner.Run(context.Background(), RunParams{
		Issue:          &model.Issue{ID: "1", Identifier: "ABC-1", Title: "Fix bug"},
		WorkspacePath:  `C:\\work\\ABC-1`,
		PromptTemplate: "Issue {{ issue.identifier }}",
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !containsToolInvalidArguments(factory.process.stdinRecorder.lines(), "tool-1") {
		t.Fatalf("tool invalid_arguments response missing: %v", factory.process.stdinRecorder.lines())
	}
}

func TestStreamingNoiseNotEmitted(t *testing.T) {
	factory := &fakeProcessFactory{process: newFakeProcess([]string{
		jsonLine(map[string]any{"id": 1, "result": map[string]any{"ok": true}}),
		jsonLine(map[string]any{"id": 2, "result": map[string]any{"thread": map[string]any{"id": "thread-1"}}}),
		jsonLine(map[string]any{"id": 3, "result": map[string]any{"turn": map[string]any{"id": "turn-1"}}}),
		jsonLine(map[string]any{"method": "item/agentMessage/delta", "params": map[string]any{"delta": "hi"}}),
		jsonLine(map[string]any{"method": "codex/event/agent_message_delta", "params": map[string]any{"delta": "hi"}}),
		jsonLine(map[string]any{"method": "item/started", "params": map[string]any{"id": "item-1"}}),
		jsonLine(map[string]any{"method": "turn/completed", "params": map[string]any{}}),
	}, nil, false)}
	runner := newTestRunner(factory, 200, 200)

	events := make([]AgentEvent, 0)
	err := runner.Run(context.Background(), RunParams{
		Issue:          &model.Issue{ID: "1", Identifier: "ABC-1", Title: "Noise"},
		WorkspacePath:  `C:\\work\\ABC-1`,
		PromptTemplate: "Issue {{ issue.identifier }}",
		OnEvent:        func(event AgentEvent) { events = append(events, event) },
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	for _, event := range events {
		if event.Event == "other_message" {
			t.Fatalf("unexpected other_message event: %+v", event)
		}
	}
	if !containsEvent(events, "session_started") || !containsEvent(events, "turn_completed") {
		t.Fatalf("events = %+v", events)
	}
}

func TestTelemetryEmittedOnce(t *testing.T) {
	factory := &fakeProcessFactory{process: newFakeProcess([]string{
		jsonLine(map[string]any{"id": 1, "result": map[string]any{"ok": true}}),
		jsonLine(map[string]any{"id": 2, "result": map[string]any{"thread": map[string]any{"id": "thread-1"}}}),
		jsonLine(map[string]any{"id": 3, "result": map[string]any{"turn": map[string]any{"id": "turn-1"}}}),
		jsonLine(map[string]any{"method": "thread/tokenUsage/updated", "params": map[string]any{"tokenUsage": map[string]any{"inputTokens": 5, "outputTokens": 7, "totalTokens": 12}}}),
		jsonLine(map[string]any{"method": "turn/completed", "params": map[string]any{}}),
	}, nil, false)}
	runner := newTestRunner(factory, 200, 200)

	events := make([]AgentEvent, 0)
	err := runner.Run(context.Background(), RunParams{
		Issue:          &model.Issue{ID: "1", Identifier: "ABC-1", Title: "Usage"},
		WorkspacePath:  `C:\\work\\ABC-1`,
		PromptTemplate: "Issue {{ issue.identifier }}",
		OnEvent:        func(event AgentEvent) { events = append(events, event) },
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	usageEvents := 0
	for _, event := range events {
		if event.Usage != nil && event.Usage.TotalTokens == 12 {
			usageEvents++
			if event.Event != "notification" {
				t.Fatalf("usage event = %+v, want notification only", event)
			}
		}
	}
	if usageEvents != 1 {
		t.Fatalf("usageEvents = %d, want 1; events=%+v", usageEvents, events)
	}
}

func TestTerminalEventsStillEmittedWithUsage(t *testing.T) {
	factory := &fakeProcessFactory{process: newFakeProcess([]string{
		jsonLine(map[string]any{"id": 1, "result": map[string]any{"ok": true}}),
		jsonLine(map[string]any{"id": 2, "result": map[string]any{"thread": map[string]any{"id": "thread-1"}}}),
		jsonLine(map[string]any{"id": 3, "result": map[string]any{"turn": map[string]any{"id": "turn-1"}}}),
		jsonLine(map[string]any{"method": "turn/completed", "params": map[string]any{"tokenUsage": map[string]any{"inputTokens": 5, "outputTokens": 7, "totalTokens": 12}}}),
	}, nil, false)}
	runner := newTestRunner(factory, 200, 200)

	events := make([]AgentEvent, 0)
	err := runner.Run(context.Background(), RunParams{
		Issue:          &model.Issue{ID: "1", Identifier: "ABC-1", Title: "Done"},
		WorkspacePath:  `C:\\work\\ABC-1`,
		PromptTemplate: "Issue {{ issue.identifier }}",
		OnEvent:        func(event AgentEvent) { events = append(events, event) },
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	usageEvents := 0
	for _, event := range events {
		if event.Usage != nil && event.Usage.TotalTokens == 12 {
			usageEvents++
			if event.Event != "turn_completed" {
				t.Fatalf("terminal usage event = %+v, want turn_completed", event)
			}
		}
	}
	if usageEvents != 1 {
		t.Fatalf("usageEvents = %d, want 1; events=%+v", usageEvents, events)
	}
}

func newTestRunner(factory *fakeProcessFactory, readTimeout int, turnTimeout int) Runner {
	return newTestRunnerWithConfig(factory, &model.ServiceConfig{
		TrackerKind:            "linear",
		TrackerEndpoint:        "http://127.0.0.1",
		TrackerAPIKey:          "secret-key",
		CodexCommand:           "codex app-server",
		CodexApprovalPolicy:    "never",
		CodexThreadSandbox:     "workspace-write",
		CodexTurnSandboxPolicy: `{"type":"workspaceWrite"}`,
		CodexReadTimeoutMS:     readTimeout,
		CodexTurnTimeoutMS:     turnTimeout,
		MaxTurns:               2,
	})
}

func newTestRunnerWithConfig(factory *fakeProcessFactory, cfg *model.ServiceConfig) Runner {
	return NewRunner(func() *model.ServiceConfig { return cfg }, slog.New(slog.NewTextHandler(io.Discard, nil)), factory)
}

type fakeProcessFactory struct {
	process *fakeProcess
}

func (f *fakeProcessFactory) StartProcess(_ context.Context, _ string, _ string) (Process, error) {
	f.process.start()
	return f.process, nil
}

type fakeProcess struct {
	stdoutR       *io.PipeReader
	stdoutW       *io.PipeWriter
	stderrR       *io.PipeReader
	stderrW       *io.PipeWriter
	stdinRecorder *recordingWriteCloser
	stdoutLines   []string
	stderrLines   []string
	holdOpen      bool
	done          chan struct{}
	doneOnce      sync.Once
	mu            sync.Mutex
	killed        bool
}

func newFakeProcess(stdoutLines []string, stderrLines []string, holdOpen bool) *fakeProcess {
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	return &fakeProcess{
		stdoutR:       stdoutR,
		stdoutW:       stdoutW,
		stderrR:       stderrR,
		stderrW:       stderrW,
		stdinRecorder: &recordingWriteCloser{},
		stdoutLines:   stdoutLines,
		stderrLines:   stderrLines,
		holdOpen:      holdOpen,
		done:          make(chan struct{}),
	}
}

func (p *fakeProcess) start() {
	go func() {
		for _, line := range p.stdoutLines {
			_, _ = io.WriteString(p.stdoutW, line+"\n")
		}
		for _, line := range p.stderrLines {
			_, _ = io.WriteString(p.stderrW, line+"\n")
		}
		if !p.holdOpen {
			_ = p.stdoutW.Close()
			_ = p.stderrW.Close()
			p.doneOnce.Do(func() { close(p.done) })
		}
	}()
}

func (p *fakeProcess) Stdin() io.WriteCloser { return p.stdinRecorder }
func (p *fakeProcess) Stdout() io.ReadCloser { return p.stdoutR }
func (p *fakeProcess) Stderr() io.ReadCloser { return p.stderrR }
func (p *fakeProcess) Wait() error {
	<-p.done
	return nil
}
func (p *fakeProcess) Kill() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.killed {
		return nil
	}
	p.killed = true
	_ = p.stdoutW.Close()
	_ = p.stderrW.Close()
	p.doneOnce.Do(func() { close(p.done) })
	return nil
}
func (p *fakeProcess) PID() int { return 4242 }

type recordingWriteCloser struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (r *recordingWriteCloser) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.Write(p)
}

func (r *recordingWriteCloser) Close() error { return nil }

func (r *recordingWriteCloser) lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	text := strings.TrimSpace(r.buf.String())
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func jsonLine(payload any) string {
	raw, _ := json.Marshal(payload)
	return string(raw)
}

func assertMethod(t *testing.T, line string, method string) {
	t.Helper()
	decoded := decodeLine(t, line)
	if got, _ := decoded["method"].(string); got != method {
		t.Fatalf("method = %q, want %q; line = %s", got, method, line)
	}
}

func decodeLine(t *testing.T, line string) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal([]byte(line), &decoded); err != nil {
		t.Fatalf("Unmarshal() error = %v, line = %s", err, line)
	}
	return decoded
}

func decodeLineNoTest(line string) map[string]any {
	var decoded map[string]any
	_ = json.Unmarshal([]byte(line), &decoded)
	return decoded
}

func nestedStringMust(t *testing.T, root map[string]any, path ...string) string {
	t.Helper()
	value, ok := nestedString(root, path...)
	if !ok {
		t.Fatalf("nested string missing for path %v in %+v", path, root)
	}
	return value
}

func containsEvent(events []AgentEvent, name string) bool {
	for _, event := range events {
		if event.Event == name {
			return true
		}
	}
	return false
}

func containsApprovalResult(lines []string, id string) bool {
	for _, line := range lines {
		decoded := decodeLineNoTest(line)
		if decoded["id"] != id {
			continue
		}
		result, ok := decoded["result"].(map[string]any)
		if !ok {
			continue
		}
		if result["approved"] == true {
			return true
		}
	}
	return false
}

func hasDynamicToolContentItems(result map[string]any) bool {
	items, ok := result["contentItems"].([]any)
	if !ok || len(items) == 0 {
		return false
	}
	first, ok := items[0].(map[string]any)
	if !ok {
		return false
	}
	itemType, _ := first["type"].(string)
	itemText, _ := first["text"].(string)
	return itemType == "inputText" && strings.TrimSpace(itemText) != ""
}

func containsToolFailure(lines []string, id string) bool {
	for _, line := range lines {
		decoded := decodeLineNoTest(line)
		if decoded["id"] != id {
			continue
		}
		result, ok := decoded["result"].(map[string]any)
		if !ok {
			continue
		}
		if result["success"] == false && result["error"] == "unsupported_tool_call" && hasDynamicToolContentItems(result) {
			return true
		}
	}
	return false
}

func containsToolSuccess(lines []string, id string) bool {
	for _, line := range lines {
		decoded := decodeLineNoTest(line)
		if decoded["id"] != id {
			continue
		}
		result, ok := decoded["result"].(map[string]any)
		if !ok {
			continue
		}
		if result["success"] == true && hasDynamicToolContentItems(result) {
			return true
		}
	}
	return false
}

func containsToolGraphQLError(lines []string, id string) bool {
	for _, line := range lines {
		decoded := decodeLineNoTest(line)
		if decoded["id"] != id {
			continue
		}
		result, ok := decoded["result"].(map[string]any)
		if !ok {
			continue
		}
		if result["success"] == false && result["error"] == "linear_graphql_errors" && hasDynamicToolContentItems(result) {
			return true
		}
	}
	return false
}

func containsToolInvalidArguments(lines []string, id string) bool {
	for _, line := range lines {
		decoded := decodeLineNoTest(line)
		if decoded["id"] != id {
			continue
		}
		result, ok := decoded["result"].(map[string]any)
		if !ok {
			continue
		}
		if result["success"] == false && result["error"] == "invalid_arguments" && hasDynamicToolContentItems(result) {
			return true
		}
	}
	return false
}
