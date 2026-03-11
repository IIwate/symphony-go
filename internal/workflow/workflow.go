package workflow

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/osteele/liquid"
	"gopkg.in/yaml.v3"

	"symphony-go/internal/model"
)

const (
	DefaultPrompt    = "You are working on an issue from Linear."
	defaultWatchPath = "./WORKFLOW.md"
	watchDebounce    = 250 * time.Millisecond
)

func Load(path string) (*model.WorkflowDefinition, error) {
	workflowPath := normalizeWorkflowPath(path)
	content, err := os.ReadFile(workflowPath)
	if err != nil {
		return nil, model.NewWorkflowError(model.ErrMissingWorkflowFile, fmt.Sprintf("read workflow file %q", workflowPath), err)
	}

	config, prompt, err := parseDefinition(string(content))
	if err != nil {
		return nil, err
	}

	return &model.WorkflowDefinition{
		RootDir:        filepath.Dir(workflowPath),
		Config:         config,
		PromptTemplate: prompt,
	}, nil
}

func Watch(ctx context.Context, path string, onChange func(*model.WorkflowDefinition)) error {
	return WatchWithErrors(ctx, path, onChange, nil)
}

func WatchWithErrors(ctx context.Context, path string, onChange func(*model.WorkflowDefinition), onError func(error)) error {
	workflowPath, err := filepath.Abs(normalizeWorkflowPath(path))
	if err != nil {
		return err
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}

	watchDir := filepath.Dir(workflowPath)
	if err := watcher.Add(watchDir); err != nil {
		watcher.Close()
		return err
	}

	go func() {
		defer watcher.Close()

		var timer *time.Timer
		var timerC <-chan time.Time

		resetTimer := func() {
			if timer == nil {
				timer = time.NewTimer(watchDebounce)
				timerC = timer.C
				return
			}

			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}

			timer.Reset(watchDebounce)
			timerC = timer.C
		}

		for {
			select {
			case <-ctx.Done():
				if timer != nil {
					timer.Stop()
				}
				return
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if !matchesWatchedPath(workflowPath, event.Name) {
					continue
				}
				if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) == 0 {
					continue
				}
				resetTimer()
			case <-timerC:
				timerC = nil
				definition, loadErr := Load(workflowPath)
				if loadErr != nil {
					if onError != nil {
						onError(loadErr)
					}
					continue
				}
				if onChange == nil {
					continue
				}
				onChange(definition)
			case watchErr, ok := <-watcher.Errors:
				if !ok {
					return
				}
				if onError != nil {
					onError(watchErr)
				}
			}
		}
	}()

	return nil
}

func RenderPrompt(tmpl string, issue *model.Issue, attempt *int, source map[string]any) (string, error) {
	templateSource := strings.TrimSpace(tmpl)
	if templateSource == "" {
		templateSource = DefaultPrompt
	}

	engine := liquid.NewEngine()
	engine.StrictVariables()

	template, err := engine.ParseString(templateSource)
	if err != nil {
		return "", model.NewWorkflowError(model.ErrTemplateParseError, "parse liquid template", err)
	}

	rendered, err := template.RenderString(liquid.Bindings{
		"issue":   issueBindings(issue),
		"attempt": attemptValue(attempt),
		"source":  sourceBindings(source),
	})
	if err != nil {
		return "", model.NewWorkflowError(model.ErrTemplateRenderError, "render liquid template", err)
	}

	return rendered, nil
}

func normalizeWorkflowPath(path string) string {
	if strings.TrimSpace(path) == "" {
		return defaultWatchPath
	}

	return path
}

func parseDefinition(content string) (map[string]any, string, error) {
	normalized := strings.ReplaceAll(content, "\r\n", "\n")
	lines := strings.Split(normalized, "\n")
	if len(lines) == 0 || lines[0] != "---" {
		return map[string]any{}, strings.TrimSpace(normalized), nil
	}

	endIndex := -1
	for index := 1; index < len(lines); index++ {
		if lines[index] == "---" {
			endIndex = index
			break
		}
	}
	if endIndex == -1 {
		return nil, "", model.NewWorkflowError(model.ErrWorkflowParseError, "front matter is missing closing delimiter", nil)
	}

	frontMatter := strings.Join(lines[1:endIndex], "\n")
	config, err := parseFrontMatter(frontMatter)
	if err != nil {
		return nil, "", err
	}

	promptBody := strings.TrimSpace(strings.Join(lines[endIndex+1:], "\n"))
	return config, promptBody, nil
}

func parseFrontMatter(source string) (map[string]any, error) {
	if strings.TrimSpace(source) == "" {
		return map[string]any{}, nil
	}

	var decoded any
	if err := yaml.Unmarshal([]byte(source), &decoded); err != nil {
		return nil, model.NewWorkflowError(model.ErrWorkflowParseError, "parse workflow front matter", err)
	}

	if decoded == nil {
		return map[string]any{}, nil
	}

	normalized := normalizeYAMLValue(decoded)
	config, ok := normalized.(map[string]any)
	if !ok {
		return nil, model.NewWorkflowError(model.ErrFrontMatterNotMap, "workflow front matter root must be a map", nil)
	}

	return config, nil
}

func normalizeYAMLValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		normalized := make(map[string]any, len(typed))
		for key, item := range typed {
			normalized[key] = normalizeYAMLValue(item)
		}
		return normalized
	case map[any]any:
		normalized := make(map[string]any, len(typed))
		for key, item := range typed {
			normalized[fmt.Sprint(key)] = normalizeYAMLValue(item)
		}
		return normalized
	case []any:
		normalized := make([]any, len(typed))
		for index, item := range typed {
			normalized[index] = normalizeYAMLValue(item)
		}
		return normalized
	default:
		return value
	}
}

func issueBindings(issue *model.Issue) liquid.Bindings {
	if issue == nil {
		return liquid.Bindings{}
	}

	bindings := liquid.Bindings{
		"id":          issue.ID,
		"identifier":  issue.Identifier,
		"title":       issue.Title,
		"description": nil,
		"priority":    nil,
		"state":       issue.State,
		"branch_name": nil,
		"url":         nil,
		"labels":      issue.Labels,
		"blocked_by":  blockerBindings(issue.BlockedBy),
		"created_at":  nil,
		"updated_at":  nil,
	}

	if issue.Description != nil {
		bindings["description"] = *issue.Description
	}
	if issue.Priority != nil {
		bindings["priority"] = *issue.Priority
	}
	if issue.BranchName != nil {
		bindings["branch_name"] = *issue.BranchName
	}
	if issue.URL != nil {
		bindings["url"] = *issue.URL
	}
	if issue.CreatedAt != nil {
		bindings["created_at"] = issue.CreatedAt.Format(time.RFC3339)
	}
	if issue.UpdatedAt != nil {
		bindings["updated_at"] = issue.UpdatedAt.Format(time.RFC3339)
	}

	return bindings
}

func blockerBindings(blockers []model.BlockerRef) []any {
	items := make([]any, 0, len(blockers))
	for _, blocker := range blockers {
		binding := liquid.Bindings{
			"id":         nil,
			"identifier": nil,
			"state":      nil,
		}
		if blocker.ID != nil {
			binding["id"] = *blocker.ID
		}
		if blocker.Identifier != nil {
			binding["identifier"] = *blocker.Identifier
		}
		if blocker.State != nil {
			binding["state"] = *blocker.State
		}
		items = append(items, binding)
	}

	return items
}

func attemptValue(attempt *int) any {
	if attempt == nil {
		return nil
	}

	return *attempt
}

func sourceBindings(source map[string]any) liquid.Bindings {
	bindings := liquid.Bindings{
		"kind":            nil,
		"project_slug":    nil,
		"owner":           nil,
		"repo":            nil,
		"branch_scope":    nil,
		"active_states":   nil,
		"terminal_states": nil,
	}
	for key, value := range source {
		bindings[key] = value
	}
	return bindings
}

func matchesWatchedPath(watchedPath string, eventPath string) bool {
	left := cleanComparablePath(watchedPath)
	right := cleanComparablePath(eventPath)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}

	return left == right
}

func cleanComparablePath(path string) string {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}

	return filepath.Clean(absPath)
}
