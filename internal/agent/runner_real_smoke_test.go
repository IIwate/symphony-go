//go:build integration

package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"symphony-go/internal/model"
	"symphony-go/internal/testutil"
)

func TestRealCodexSmoke(t *testing.T) {
	testutil.RequireEnv(t, "SYMPHONY_REAL_CODEX_SMOKE")
	if testing.Short() {
		t.Skip("short 模式下跳过 real codex smoke")
	}

	workspacePath := initRealCodexSmokeRepo(t)
	command := strings.TrimSpace(os.Getenv("SYMPHONY_REAL_CODEX_COMMAND"))
	if command == "" {
		command = "codex app-server"
	}

	cfg := &model.ServiceConfig{
		CodexCommand:           command,
		CodexApprovalPolicy:    "never",
		CodexThreadSandbox:     "workspace-write",
		CodexTurnSandboxPolicy: `{"type":"workspaceWrite"}`,
		CodexReadTimeoutMS:     15000,
		CodexTurnTimeoutMS:     120000,
		MaxTurns:               1,
	}
	runner := NewRunner(func() *model.ServiceConfig { return cfg }, nil, nil)

	issue := &model.Issue{
		ID:         "real-codex-smoke",
		Identifier: "SMOKE-1",
		Title:      "real codex smoke",
		State:      "Todo",
	}
	var (
		mu     sync.Mutex
		events []AgentEvent
	)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	err := runner.Run(ctx, RunParams{
		Issue:         issue,
		WorkspacePath: workspacePath,
		PromptTemplate: `不要调用任何工具，不要修改任何文件。
只回复 OK，然后结束。`,
		MaxTurns: 1,
		OnEvent: func(event AgentEvent) {
			mu.Lock()
			events = append(events, event)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	mu.Lock()
	captured := append([]AgentEvent(nil), events...)
	mu.Unlock()

	var sawSessionStarted bool
	var sawTurnCompleted bool
	for _, event := range captured {
		switch event.Event {
		case "session_started":
			sawSessionStarted = true
			if strings.TrimSpace(optionalString(event.SessionID)) == "" {
				t.Fatalf("session_started event missing session id: %+v", event)
			}
		case "turn_completed":
			sawTurnCompleted = true
		}
	}
	if !sawSessionStarted {
		t.Fatalf("events = %+v, want session_started", captured)
	}
	if !sawTurnCompleted {
		t.Fatalf("events = %+v, want turn_completed", captured)
	}
}

func initRealCodexSmokeRepo(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	runGitCommand(t, dir, "init")
	runGitCommand(t, dir, "branch", "-M", "main")
	runGitCommand(t, dir, "config", "user.name", "Symphony Smoke")
	runGitCommand(t, dir, "config", "user.email", "symphony-smoke@example.invalid")

	readmePath := filepath.Join(dir, "README.md")
	if err := os.WriteFile(readmePath, []byte("# real codex smoke\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", readmePath, err)
	}
	runGitCommand(t, dir, "add", "README.md")
	runGitCommand(t, dir, "commit", "-m", "test: init real codex smoke")

	absDir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("filepath.Abs(%q) error = %v", dir, err)
	}
	return absDir
}

func runGitCommand(t *testing.T, dir string, args ...string) {
	t.Helper()

	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, string(output))
	}
}
