//go:build integration

package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"symphony-go/internal/config"
	"symphony-go/internal/testutil"
	"symphony-go/internal/workflow"
)

func TestMainIntegration_DryRun(t *testing.T) {
	_ = testutil.RequireEnv(t, "LINEAR_API_KEY")

	workflowPath := filepath.Join(t.TempDir(), "WORKFLOW.md")
	workspaceRoot := filepath.ToSlash(filepath.Join(t.TempDir(), "workspaces"))
	projectSlug := integrationProjectSlug(t)

	content := fmt.Sprintf(`---
tracker:
  kind: linear
  api_key: $LINEAR_API_KEY
  project_slug: %s
workspace:
  root: %s
codex:
  command: codex app-server
---

integration dry run
`, projectSlug, workspaceRoot)

	if err := os.WriteFile(workflowPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	var stderr bytes.Buffer
	if exitCode := runCLI([]string{"--dry-run", workflowPath}, &stderr); exitCode != 0 {
		t.Fatalf("runCLI() exitCode = %d, stderr = %s", exitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "dry-run 校验通过") {
		t.Fatalf("stderr = %q, want dry-run success message", stderr.String())
	}
}

func integrationProjectSlug(t *testing.T) string {
	t.Helper()

	definition, err := workflow.Load(repoWorkflowPath(t))
	if err != nil {
		t.Fatalf("workflow.Load() error = %v", err)
	}
	cfg, err := config.NewFromWorkflow(definition)
	if err != nil {
		t.Fatalf("config.NewFromWorkflow() error = %v", err)
	}
	if projectSlug := strings.TrimSpace(os.Getenv("LINEAR_PROJECT_SLUG")); projectSlug != "" {
		return projectSlug
	}
	return cfg.TrackerProjectSlug
}

func repoWorkflowPath(t *testing.T) string {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller() failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "WORKFLOW.md"))
}
