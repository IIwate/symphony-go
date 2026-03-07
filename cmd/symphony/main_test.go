package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunCLIUsesDefaultWorkflowPath(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "secret-key")
	workingDir := t.TempDir()
	writeWorkflow(t, filepath.Join(workingDir, "WORKFLOW.md"))

	originalDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	defer func() {
		_ = os.Chdir(originalDir)
	}()
	if err := os.Chdir(workingDir); err != nil {
		t.Fatalf("Chdir() error = %v", err)
	}

	var stderr bytes.Buffer
	if exitCode := runCLI([]string{"--dry-run"}, &stderr); exitCode != 0 {
		t.Fatalf("runCLI() exitCode = %d, stderr = %s", exitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "dry-run 校验通过") {
		t.Fatalf("stderr = %q, want dry-run success message", stderr.String())
	}
}

func TestRunCLIFailsForMissingExplicitWorkflow(t *testing.T) {
	var stderr bytes.Buffer
	if exitCode := runCLI([]string{"missing.md"}, &stderr); exitCode == 0 {
		t.Fatalf("runCLI() exitCode = %d, want non-zero", exitCode)
	}
	if !strings.Contains(stderr.String(), "missing_workflow_file") {
		t.Fatalf("stderr = %q, want missing_workflow_file", stderr.String())
	}
}

func TestRunCLIFailsWhenDefaultWorkflowMissing(t *testing.T) {
	workingDir := t.TempDir()
	originalDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	defer func() {
		_ = os.Chdir(originalDir)
	}()
	if err := os.Chdir(workingDir); err != nil {
		t.Fatalf("Chdir() error = %v", err)
	}

	var stderr bytes.Buffer
	if exitCode := runCLI(nil, &stderr); exitCode == 0 {
		t.Fatalf("runCLI() exitCode = %d, want non-zero", exitCode)
	}
	if !strings.Contains(stderr.String(), "missing_workflow_file") {
		t.Fatalf("stderr = %q, want missing_workflow_file", stderr.String())
	}
}

func writeWorkflow(t *testing.T, path string) {
	t.Helper()

	content := `---
tracker:
  kind: linear
  api_key: $LINEAR_API_KEY
  project_slug: demo
codex:
  command: codex app-server
---

hello {{ issue.title }}
`

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
}
