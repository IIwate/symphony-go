package workflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"symphony-go/internal/model"
)

func TestLoadWithFrontMatter(t *testing.T) {
	path := writeWorkflowFile(t, `---
tracker:
  kind: linear
  project_slug: demo
---

hello {{ issue.title }}
`)

	definition, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	tracker, ok := definition.Config["tracker"].(map[string]any)
	if !ok {
		t.Fatalf("tracker config type = %T", definition.Config["tracker"])
	}
	if got := tracker["kind"]; got != "linear" {
		t.Fatalf("tracker.kind = %v, want linear", got)
	}
	if got := definition.PromptTemplate; got != "hello {{ issue.title }}" {
		t.Fatalf("PromptTemplate = %q", got)
	}
}

func TestLoadWithoutFrontMatter(t *testing.T) {
	path := writeWorkflowFile(t, "plain prompt")

	definition, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(definition.Config) != 0 {
		t.Fatalf("Config size = %d, want 0", len(definition.Config))
	}
	if definition.PromptTemplate != "plain prompt" {
		t.Fatalf("PromptTemplate = %q", definition.PromptTemplate)
	}
}

func TestLoadFrontMatterNotMap(t *testing.T) {
	path := writeWorkflowFile(t, `---
- item
---

body
`)

	_, err := Load(path)
	if !errors.Is(err, model.ErrFrontMatterNotMap) {
		t.Fatalf("Load() error = %v, want ErrFrontMatterNotMap", err)
	}
}

func TestRenderPromptUsesDefaultPrompt(t *testing.T) {
	rendered, err := RenderPrompt("", &model.Issue{Title: "Test"}, nil)
	if err != nil {
		t.Fatalf("RenderPrompt() error = %v", err)
	}
	if rendered != DefaultPrompt {
		t.Fatalf("RenderPrompt() = %q, want %q", rendered, DefaultPrompt)
	}
}

func TestRenderPromptUnknownVariable(t *testing.T) {
	_, err := RenderPrompt("{{ issue.unknown }}", &model.Issue{Title: "Test"}, nil)
	if !errors.Is(err, model.ErrTemplateRenderError) {
		t.Fatalf("RenderPrompt() error = %v, want ErrTemplateRenderError", err)
	}
}

func TestRenderPromptUnknownFilter(t *testing.T) {
	_, err := RenderPrompt("{{ issue.title | missing_filter }}", &model.Issue{Title: "Test"}, nil)
	if err == nil {
		t.Fatal("RenderPrompt() error = nil, want unknown filter error")
	}
	if !errors.Is(err, model.ErrTemplateParseError) && !errors.Is(err, model.ErrTemplateRenderError) {
		t.Fatalf("RenderPrompt() error = %v, want template parse/render error", err)
	}
}

func TestWatchReloadsOnChange(t *testing.T) {
	path := writeWorkflowFile(t, "first")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	updates := make(chan *model.WorkflowDefinition, 1)
	if err := Watch(ctx, path, func(def *model.WorkflowDefinition) {
		updates <- def
	}); err != nil {
		t.Fatalf("Watch() error = %v", err)
	}

	time.Sleep(200 * time.Millisecond)
	if err := os.WriteFile(path, []byte("second"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	select {
	case definition := <-updates:
		if definition.PromptTemplate != "second" {
			t.Fatalf("PromptTemplate = %q, want second", definition.PromptTemplate)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watch callback not triggered")
	}
}

func TestWatchSkipsInvalidReload(t *testing.T) {
	path := writeWorkflowFile(t, "first")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	updates := make(chan *model.WorkflowDefinition, 1)
	if err := Watch(ctx, path, func(def *model.WorkflowDefinition) {
		updates <- def
	}); err != nil {
		t.Fatalf("Watch() error = %v", err)
	}

	time.Sleep(200 * time.Millisecond)
	if err := os.WriteFile(path, []byte("---\nfoo: [\n---"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	select {
	case definition := <-updates:
		t.Fatalf("unexpected update received: %+v", definition)
	case <-time.After(750 * time.Millisecond):
	}
}

func writeWorkflowFile(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	return path
}
