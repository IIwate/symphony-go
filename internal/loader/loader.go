package loader

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"symphony-go/internal/model"
	"symphony-go/internal/secret"
)

const defaultDispatchFlow = "implement"

var envValuePattern = regexp.MustCompile(`^\$(\w+)$`)

func Load(dir string, profile string) (*model.AutomationDefinition, error) {
	rootDir, err := filepath.Abs(normalizeConfigDir(dir))
	if err != nil {
		return nil, model.NewWorkflowError(model.ErrWorkflowParseError, "resolve automation directory", err)
	}

	projectConfig, err := readRequiredYAMLMap(filepath.Join(rootDir, "project.yaml"))
	if err != nil {
		return nil, err
	}

	activeProfile := strings.TrimSpace(profile)
	if activeProfile == "" {
		if defaultProfile, ok := getOptionalStringValue(getMapValue(projectConfig, "defaults"), "profile"); ok {
			activeProfile = defaultProfile
		}
	}

	merged := cloneStringMap(projectConfig)
	if activeProfile != "" {
		profileConfig, err := readRequiredYAMLMap(filepath.Join(rootDir, "profiles", activeProfile+".yaml"))
		if err != nil {
			return nil, err
		}
		merged = mustStringMap(deepMerge(merged, profileConfig))
	}

	if overrides, err := readOptionalYAMLMap(filepath.Join(rootDir, "local", "overrides.yaml")); err != nil {
		return nil, err
	} else if overrides != nil {
		merged = mustStringMap(deepMerge(merged, overrides))
	}

	sources, err := readSourceRegistry(rootDir)
	if err != nil {
		return nil, err
	}
	flows, err := readFlowRegistry(rootDir)
	if err != nil {
		return nil, err
	}
	policies, err := readPolicyRegistry(rootDir)
	if err != nil {
		return nil, err
	}

	defaults := model.AutomationDefaults{}
	if profileValue, ok := getOptionalStringValue(getMapValue(projectConfig, "defaults"), "profile"); ok {
		defaults.Profile = stringPointer(profileValue)
	}

	selection := model.AutomationSelection{
		DispatchFlow:   defaultDispatchFlow,
		EnabledSources: getStringSliceValue(getMapValue(merged, "selection"), "enabled_sources"),
	}
	if dispatchFlow, ok := getOptionalStringValue(getMapValue(merged, "selection"), "dispatch_flow"); ok {
		selection.DispatchFlow = dispatchFlow
	}

	return &model.AutomationDefinition{
		RootDir:   rootDir,
		Profile:   activeProfile,
		Runtime:   cloneStringMap(getMapValue(merged, "runtime")),
		Selection: selection,
		Defaults:  defaults,
		Sources:   sources,
		Flows:     flows,
		Policies:  policies,
	}, nil
}

func ResolveActiveWorkflow(def *model.AutomationDefinition) (*model.WorkflowDefinition, error) {
	if def == nil {
		return nil, model.NewWorkflowError(model.ErrWorkflowParseError, "automation definition is nil", nil)
	}

	enabledSources := normalizedNames(def.Selection.EnabledSources)
	if len(enabledSources) != 1 {
		return nil, model.NewWorkflowError(model.ErrWorkflowParseError, "selection.enabled_sources must contain exactly one source", nil)
	}
	sourceName := enabledSources[0]
	sourceDef, ok := def.Sources[sourceName]
	if !ok || sourceDef == nil {
		return nil, model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("selected source %q not found", sourceName), nil)
	}
	resolvedSource := resolveEnvMap(sourceDef.Raw)

	flowName := strings.TrimSpace(def.Selection.DispatchFlow)
	if flowName == "" {
		flowName = defaultDispatchFlow
	}
	flowDef, ok := def.Flows[flowName]
	if !ok || flowDef == nil {
		return nil, model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("selected flow %q not found", flowName), nil)
	}

	configMap := map[string]any{}
	copyMapField(configMap, "polling", def.Runtime)
	copyMapField(configMap, "agent", def.Runtime)
	copyMapField(configMap, "codex", def.Runtime)
	copyMapField(configMap, "server", def.Runtime)
	copyMapField(configMap, "orchestrator", def.Runtime)

	workspaceMap := cloneStringMap(getMapValue(def.Runtime, "workspace"))
	if branchScope, ok := resolvedSource["branch_scope"]; ok {
		workspaceMap["linear_branch_scope"] = cloneValue(branchScope)
	}
	if len(workspaceMap) > 0 {
		configMap["workspace"] = workspaceMap
	}

	trackerMap := map[string]any{}
	for _, key := range []string{"kind", "api_key", "endpoint", "project_slug", "owner", "repo", "state_label_prefix", "active_states", "terminal_states"} {
		if value, ok := resolvedSource[key]; ok {
			trackerMap[key] = cloneValue(value)
		}
	}
	if linearConfig, ok := resolvedSource["linear"]; ok {
		trackerMap["linear"] = cloneValue(linearConfig)
	}
	configMap["tracker"] = trackerMap

	if hooksValue, ok := flowDef.Raw["hooks"]; ok {
		hooksMap, ok := asStringMap(hooksValue)
		if !ok {
			return nil, model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("flow %q hooks must be a map", flowName), nil)
		}
		resolvedHooks := make(map[string]any, len(hooksMap))
		for _, hookName := range []string{"after_create", "before_run", "after_run", "before_remove"} {
			value, exists := hooksMap[hookName]
			if !exists {
				continue
			}
			resolvedValue, err := resolveHookValue(def.RootDir, value)
			if err != nil {
				return nil, err
			}
			resolvedHooks[hookName] = resolvedValue
		}
		if len(resolvedHooks) > 0 {
			configMap["hooks"] = resolvedHooks
		}
	}

	promptPath, ok := flowDef.Raw["prompt"].(string)
	if !ok || strings.TrimSpace(promptPath) == "" {
		return nil, model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("flow %q prompt is required", flowName), nil)
	}
	promptTemplate, err := readPromptTemplate(def.RootDir, promptPath)
	if err != nil {
		return nil, err
	}

	return &model.WorkflowDefinition{
		Config:         configMap,
		PromptTemplate: promptTemplate,
		Source:         sourceBindings(resolvedSource),
	}, nil
}

func normalizeConfigDir(dir string) string {
	if strings.TrimSpace(dir) == "" {
		return "automation"
	}
	return dir
}

func readSourceRegistry(rootDir string) (map[string]*model.SourceDefinition, error) {
	fullDir := filepath.Join(rootDir, "sources")
	entries, err := os.ReadDir(fullDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]*model.SourceDefinition{}, nil
		}
		return nil, model.NewWorkflowError(model.ErrWorkflowParseError, "read sources directory", err)
	}

	registry := make(map[string]*model.SourceDefinition)
	names := make([]string, 0, len(entries))
	filesByName := make(map[string]os.DirEntry, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".yaml" {
			continue
		}
		names = append(names, entry.Name())
		filesByName[entry.Name()] = entry
	}
	sort.Strings(names)
	for _, fileName := range names {
		entry := filesByName[fileName]
		raw, err := readRequiredYAMLMap(filepath.Join(fullDir, entry.Name()))
		if err != nil {
			return nil, err
		}
		name := strings.TrimSuffix(entry.Name(), ".yaml")
		registry[name] = &model.SourceDefinition{Name: name, Raw: raw}
	}

	return registry, nil
}

func readFlowRegistry(rootDir string) (map[string]*model.FlowDefinition, error) {
	fullDir := filepath.Join(rootDir, "flows")
	entries, err := os.ReadDir(fullDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]*model.FlowDefinition{}, nil
		}
		return nil, model.NewWorkflowError(model.ErrWorkflowParseError, "read flows directory", err)
	}

	registry := make(map[string]*model.FlowDefinition)
	names := make([]string, 0, len(entries))
	filesByName := make(map[string]os.DirEntry, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".yaml" {
			continue
		}
		names = append(names, entry.Name())
		filesByName[entry.Name()] = entry
	}
	sort.Strings(names)
	for _, fileName := range names {
		entry := filesByName[fileName]
		raw, err := readRequiredYAMLMap(filepath.Join(fullDir, entry.Name()))
		if err != nil {
			return nil, err
		}
		name := strings.TrimSuffix(entry.Name(), ".yaml")
		registry[name] = &model.FlowDefinition{Name: name, Raw: raw}
	}

	return registry, nil
}

func readPolicyRegistry(rootDir string) (map[string]map[string]any, error) {
	fullDir := filepath.Join(rootDir, "policies")
	entries, err := os.ReadDir(fullDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]map[string]any{}, nil
		}
		return nil, model.NewWorkflowError(model.ErrWorkflowParseError, "read policies directory", err)
	}

	registry := make(map[string]map[string]any)
	names := make([]string, 0, len(entries))
	filesByName := make(map[string]os.DirEntry, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".yaml" {
			continue
		}
		names = append(names, entry.Name())
		filesByName[entry.Name()] = entry
	}
	sort.Strings(names)
	for _, fileName := range names {
		entry := filesByName[fileName]
		raw, err := readRequiredYAMLMap(filepath.Join(fullDir, entry.Name()))
		if err != nil {
			return nil, err
		}
		registry[strings.TrimSuffix(entry.Name(), ".yaml")] = raw
	}

	return registry, nil
}

func copyMapField(target map[string]any, key string, source map[string]any) {
	value := getMapValue(source, key)
	if len(value) == 0 {
		return
	}
	target[key] = cloneStringMap(value)
}

func resolveHookValue(rootDir string, value any) (any, error) {
	if value == nil {
		return nil, nil
	}

	text, ok := value.(string)
	if !ok {
		return nil, model.NewWorkflowError(model.ErrWorkflowParseError, "hook value must be string or null", nil)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, nil
	}
	if strings.Contains(text, "\n") {
		return text, nil
	}
	if strings.ContainsAny(text, `/\`) {
		resolvedPath, err := resolveAutomationPath(rootDir, text)
		if err != nil {
			return nil, err
		}
		content, err := os.ReadFile(resolvedPath)
		if err != nil {
			return nil, model.NewWorkflowError(model.ErrMissingWorkflowFile, fmt.Sprintf("read hook file %q", resolvedPath), err)
		}
		return strings.TrimSpace(string(content)), nil
	}
	return text, nil
}

func readPromptTemplate(rootDir string, reference string) (string, error) {
	resolvedPath, err := resolveAutomationPath(rootDir, reference)
	if err != nil {
		return "", err
	}
	content, err := os.ReadFile(resolvedPath)
	if err != nil {
		return "", model.NewWorkflowError(model.ErrMissingWorkflowFile, fmt.Sprintf("read prompt template %q", resolvedPath), err)
	}
	return strings.TrimSpace(string(content)), nil
}

func resolveAutomationPath(rootDir string, reference string) (string, error) {
	trimmed := strings.TrimSpace(reference)
	if trimmed == "" {
		return "", model.NewWorkflowError(model.ErrWorkflowParseError, "path reference is empty", nil)
	}
	normalized := strings.ReplaceAll(trimmed, "\\", "/")
	if path.IsAbs(normalized) || filepath.IsAbs(trimmed) {
		return "", model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("absolute path %q is not allowed", trimmed), nil)
	}
	cleaned := path.Clean(normalized)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("path %q escapes automation directory", trimmed), nil)
	}

	resolved := filepath.Join(rootDir, filepath.FromSlash(cleaned))
	absResolved, err := filepath.Abs(resolved)
	if err != nil {
		return "", model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("resolve path %q", trimmed), err)
	}
	if !pathWithinRoot(rootDir, absResolved) {
		return "", model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("path %q escapes automation directory", trimmed), nil)
	}
	return absResolved, nil
}

func pathWithinRoot(root string, target string) bool {
	relative, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	if relative == ".." {
		return false
	}
	return !strings.HasPrefix(relative, ".."+string(os.PathSeparator))
}

func readRequiredYAMLMap(path string) (map[string]any, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, model.NewWorkflowError(model.ErrMissingWorkflowFile, fmt.Sprintf("read %q", path), err)
		}
		return nil, model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("read %q", path), err)
	}
	return parseYAMLMap(path, content)
}

func readOptionalYAMLMap(path string) (map[string]any, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("read %q", path), err)
	}
	return parseYAMLMap(path, content)
}

func parseYAMLMap(path string, content []byte) (map[string]any, error) {
	if len(strings.TrimSpace(string(content))) == 0 {
		return map[string]any{}, nil
	}

	var decoded any
	if err := yaml.Unmarshal(content, &decoded); err != nil {
		return nil, model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("parse %q", path), err)
	}
	if decoded == nil {
		return map[string]any{}, nil
	}

	normalized := normalizeYAMLValue(decoded)
	mapping, ok := normalized.(map[string]any)
	if !ok {
		return nil, model.NewWorkflowError(model.ErrWorkflowParseError, fmt.Sprintf("%q root must be a map", path), nil)
	}
	return mapping, nil
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
		for i, item := range typed {
			normalized[i] = normalizeYAMLValue(item)
		}
		return normalized
	default:
		return value
	}
}

func getMapValue(source map[string]any, key string) map[string]any {
	if source == nil {
		return map[string]any{}
	}
	raw, ok := source[key]
	if !ok || raw == nil {
		return map[string]any{}
	}
	if mapping, ok := asStringMap(raw); ok {
		return mapping
	}
	return map[string]any{}
}

func getOptionalStringValue(source map[string]any, key string) (string, bool) {
	if source == nil {
		return "", false
	}
	raw, ok := source[key]
	if !ok || raw == nil {
		return "", false
	}
	text, ok := raw.(string)
	if !ok {
		return "", false
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", false
	}
	return text, true
}

func getStringSliceValue(source map[string]any, key string) []string {
	if source == nil {
		return nil
	}
	raw, ok := source[key]
	if !ok || raw == nil {
		return nil
	}
	switch typed := raw.(type) {
	case []string:
		return normalizedNames(typed)
	case []any:
		values := make([]string, 0, len(typed))
		for _, item := range typed {
			text, ok := item.(string)
			if !ok {
				continue
			}
			if trimmed := strings.TrimSpace(text); trimmed != "" {
				values = append(values, trimmed)
			}
		}
		return values
	case string:
		parts := strings.Split(typed, ",")
		values := make([]string, 0, len(parts))
		for _, part := range parts {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				values = append(values, trimmed)
			}
		}
		return values
	default:
		return nil
	}
}

func sourceBindings(source map[string]any) map[string]any {
	bindings := map[string]any{
		"kind":            nil,
		"project_slug":    nil,
		"owner":           nil,
		"repo":            nil,
		"branch_scope":    nil,
		"active_states":   nil,
		"terminal_states": nil,
	}
	if source == nil {
		return bindings
	}
	for _, key := range []string{"kind", "project_slug", "owner", "repo", "branch_scope", "active_states", "terminal_states"} {
		if value, ok := source[key]; ok {
			bindings[key] = cloneValue(value)
		}
	}
	return bindings
}

func resolveEnvMap(source map[string]any) map[string]any {
	if source == nil {
		return map[string]any{}
	}

	resolved := make(map[string]any, len(source))
	for key, value := range source {
		resolved[key] = resolveEnvValue(value)
	}
	return resolved
}

func resolveEnvValue(value any) any {
	switch typed := value.(type) {
	case nil:
		return nil
	case string:
		return resolveEnvString(typed)
	case map[string]any:
		return resolveEnvMap(typed)
	case []string:
		items := make([]string, len(typed))
		for i, item := range typed {
			items[i] = resolveEnvString(item)
		}
		return items
	case []any:
		items := make([]any, len(typed))
		for i, item := range typed {
			items[i] = resolveEnvValue(item)
		}
		return items
	default:
		return cloneValue(value)
	}
}

func resolveEnvString(value string) string {
	matches := envValuePattern.FindStringSubmatch(value)
	if len(matches) != 2 {
		return value
	}

	resolved, ok := secret.DefaultResolver(matches[1])
	if !ok {
		return ""
	}

	return strings.TrimSpace(resolved)
}

func mustStringMap(value any) map[string]any {
	mapping, ok := asStringMap(value)
	if !ok {
		return map[string]any{}
	}
	return mapping
}

func stringPointer(value string) *string {
	copyValue := value
	return &copyValue
}

func normalizedNames(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}
