package envfile

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

func Load(path string) error {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("parse %s:%d: expected KEY=VALUE", path, lineNo)
		}

		key = strings.TrimSpace(key)
		if key == "" {
			return fmt.Errorf("parse %s:%d: empty key", path, lineNo)
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}

		value, err = parseValue(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("parse %s:%d: %w", path, lineNo, err)
		}
		if err := os.Setenv(key, value); err != nil {
			return err
		}
	}

	return scanner.Err()
}

// Upsert 将 key=value 写入 env 文件。已存在则更新值，否则追加。
// 保留注释和空行。原子替换（write-to-temp + rename），不支持并发写者。
func Upsert(path string, key string, value string) error {
	return UpsertMultiple(path, map[string]string{key: value})
}

// UpsertMultiple 批量更新。同样原子替换，不支持并发写者。
func UpsertMultiple(path string, pairs map[string]string) error {
	if len(pairs) == 0 {
		return nil
	}

	lines := make([]string, 0)
	content, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	} else if len(content) > 0 {
		lines = strings.Split(strings.ReplaceAll(string(content), "\r\n", "\n"), "\n")
		if lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
	}

	keys := make([]string, 0, len(pairs))
	for key := range pairs {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	found := make(map[string]bool, len(pairs))
	output := make([]string, 0, len(lines)+len(pairs))
	for _, line := range lines {
		key, _, ok := parseAssignment(line)
		if !ok {
			output = append(output, line)
			continue
		}

		value, exists := pairs[key]
		if !exists {
			output = append(output, line)
			continue
		}

		found[key] = true
		output = append(output, formatAssignment(key, value))
	}

	for _, key := range keys {
		if found[key] {
			continue
		}
		output = append(output, formatAssignment(key, pairs[key]))
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	tmpPath := path + ".tmp"
	body := strings.Join(output, "\n")
	if len(output) > 0 {
		body += "\n"
	}
	if err := os.WriteFile(tmpPath, []byte(body), 0o600); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}

	return nil
}

func parseAssignment(line string) (string, string, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return "", "", false
	}

	key, value, ok := strings.Cut(trimmed, "=")
	if !ok {
		return "", "", false
	}

	key = strings.TrimSpace(key)
	if key == "" {
		return "", "", false
	}

	return key, strings.TrimSpace(value), true
}

func parseValue(value string) (string, error) {
	if len(value) < 2 {
		return value, nil
	}

	if value[0] == '"' && value[len(value)-1] == '"' {
		unquoted, err := strconv.Unquote(value)
		if err == nil {
			return unquoted, nil
		}
		return value[1 : len(value)-1], nil
	}
	if value[0] == '\'' && value[len(value)-1] == '\'' {
		return value[1 : len(value)-1], nil
	}

	return value, nil
}

func formatAssignment(key string, value string) string {
	return key + "=" + formatValue(value)
}

func formatValue(value string) string {
	if value == "" {
		return ""
	}
	if strings.ContainsAny(value, " \t#\"") {
		return quoteValue(value)
	}
	return value
}

func quoteValue(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + replacer.Replace(value) + `"`
}
