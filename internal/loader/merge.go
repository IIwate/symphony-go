package loader

import "fmt"

func deepMerge(base any, override any) any {
	if override == nil {
		return nil
	}

	baseMap, baseOK := asStringMap(base)
	overrideMap, overrideOK := asStringMap(override)
	if baseOK && overrideOK {
		result := cloneStringMap(baseMap)
		for key, value := range overrideMap {
			existing, exists := result[key]
			if !exists {
				result[key] = cloneValue(value)
				continue
			}
			result[key] = deepMerge(existing, value)
		}
		return result
	}

	return cloneValue(override)
}

func cloneValue(value any) any {
	if value == nil {
		return nil
	}
	if mapping, ok := asStringMap(value); ok {
		return cloneStringMap(mapping)
	}
	switch typed := value.(type) {
	case []any:
		cloned := make([]any, len(typed))
		for i, item := range typed {
			cloned[i] = cloneValue(item)
		}
		return cloned
	case []string:
		cloned := make([]string, len(typed))
		copy(cloned, typed)
		return cloned
	default:
		return typed
	}
}

func cloneStringMap(source map[string]any) map[string]any {
	cloned := make(map[string]any, len(source))
	for key, value := range source {
		cloned[key] = cloneValue(value)
	}
	return cloned
}

func asStringMap(value any) (map[string]any, bool) {
	switch typed := value.(type) {
	case map[string]any:
		return typed, true
	case map[any]any:
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			result[fmt.Sprint(key)] = item
		}
		return result, true
	default:
		return nil, false
	}
}
