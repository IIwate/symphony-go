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

	"symphony-go/internal/loader"
	"symphony-go/internal/testutil"
)

func TestMainIntegration_DryRun(t *testing.T) {
	apiKey := testutil.RequireEnv(t, "LINEAR_API_KEY")

	configDir := filepath.Join(t.TempDir(), "automation")
	workspaceRoot := filepath.ToSlash(filepath.Join(t.TempDir(), "workspaces"))
	projectSlug := integrationProjectSlug(t)

	writeAutomationConfig(t, configDir, automationFixtureOptions{
		WorkspaceRoot:  workspaceRoot,
		PromptTemplate: "integration dry run",
	})
	writeFile(t, filepath.Join(configDir, "sources", "linear-main.yaml"), fmt.Sprintf(`kind: linear
api_key: $LINEAR_API_KEY
project_slug: %s
branch_scope: symphony-go
active_states: ["Todo", "In Progress"]
terminal_states: ["Closed", "Done"]
`, projectSlug))
	writeFile(t, filepath.Join(configDir, "local", "env.local"), "LINEAR_API_KEY="+apiKey+"\n")

	originalValue, hadValue := os.LookupEnv("LINEAR_API_KEY")
	if err := os.Unsetenv("LINEAR_API_KEY"); err != nil {
		t.Fatalf("Unsetenv() error = %v", err)
	}
	defer func() {
		if hadValue {
			_ = os.Setenv("LINEAR_API_KEY", originalValue)
			return
		}
		_ = os.Unsetenv("LINEAR_API_KEY")
	}()

	var stderr bytes.Buffer
	if exitCode := runCLI([]string{"--dry-run", "--config-dir", configDir}, &stderr); exitCode != 0 {
		t.Fatalf("runCLI() exitCode = %d, stderr = %s", exitCode, stderr.String())
	}
	if !bytes.Contains(stderr.Bytes(), []byte("dry-run 校验通过")) {
		t.Fatalf("stderr = %q, want dry-run success message", stderr.String())
	}
}

func integrationProjectSlug(t *testing.T) string {
	t.Helper()

	if projectSlug := strings.TrimSpace(os.Getenv("LINEAR_PROJECT_SLUG")); projectSlug != "" {
		return projectSlug
	}

	repoDef, err := loader.Load(repoConfigDir(t), "")
	if err != nil {
		t.Fatalf("loader.Load() error = %v", err)
	}
	for _, sourceName := range repoDef.Selection.EnabledSources {
		sourceDef := repoDef.Sources[strings.TrimSpace(sourceName)]
		if sourceDef == nil {
			continue
		}
		if projectSlug, ok := sourceDef.Raw["project_slug"].(string); ok && strings.TrimSpace(projectSlug) != "" {
			return strings.TrimSpace(projectSlug)
		}
	}
	t.Fatal("project_slug not found in repo automation config")
	return ""
}

func repoConfigDir(t *testing.T) string {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller() failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "automation"))
}
