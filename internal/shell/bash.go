package shell

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

var (
	runtimeGOOS = runtime.GOOS
	envLookup   = os.Getenv
	fileExists  = func(path string) bool {
		info, err := os.Stat(path)
		return err == nil && !info.IsDir()
	}
	lookPath = exec.LookPath
)

func BashCommand(ctx context.Context, dir string, script string) (*exec.Cmd, error) {
	bashPath, err := resolveBashPath()
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, bashPath, "-lc", script)
	cmd.Dir = dir
	return cmd, nil
}

func resolveBashPath() (string, error) {
	if runtimeGOOS == "windows" {
		if path, ok := findGitBash(); ok {
			return path, nil
		}
	}

	path, err := lookPath("bash")
	if err != nil {
		return "", fmt.Errorf("resolve bash: %w", err)
	}
	return path, nil
}

func findGitBash() (string, bool) {
	seen := make(map[string]struct{})
	for _, candidate := range gitBashCandidates() {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		candidate = filepath.Clean(candidate)
		key := strings.ToLower(candidate)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		if fileExists(candidate) {
			return candidate, true
		}
	}
	return "", false
}

func gitBashCandidates() []string {
	var candidates []string
	for _, envName := range []string{"ProgramW6432", "ProgramFiles", "ProgramFiles(x86)"} {
		root := strings.TrimSpace(envLookup(envName))
		if root != "" {
			candidates = append(candidates, filepath.Join(root, "Git", "bin", "bash.exe"))
		}
	}
	if localAppData := strings.TrimSpace(envLookup("LocalAppData")); localAppData != "" {
		candidates = append(candidates, filepath.Join(localAppData, "Programs", "Git", "bin", "bash.exe"))
	}
	return candidates
}
