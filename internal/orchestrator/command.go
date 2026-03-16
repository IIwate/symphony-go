package orchestrator

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

func commandOutputWithTimeout(ctx context.Context, dir string, timeout time.Duration, argv []string) (string, string, error) {
	stdout, stderr, _, err := commandResultWithTimeout(ctx, dir, timeout, argv)
	return stdout, stderr, err
}

func commandResultWithTimeout(ctx context.Context, dir string, timeout time.Duration, argv []string) (string, string, int, error) {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if len(argv) == 0 {
		return "", "", -1, fmt.Errorf("command argv is empty")
	}
	cmd := exec.CommandContext(commandCtx, argv[0], argv[1:]...)
	cmd.Dir = dir
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}
	return stdout.String(), stderr.String(), exitCode, err
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, string, error) {
	return commandOutputWithTimeout(ctx, dir, 10*time.Second, append([]string{"git"}, args...))
}

func gitOutputWithTimeout(ctx context.Context, dir string, timeout time.Duration, args ...string) (string, string, error) {
	return commandOutputWithTimeout(ctx, dir, timeout, append([]string{"git"}, args...))
}

func gitResultWithTimeout(ctx context.Context, dir string, timeout time.Duration, args ...string) (string, string, int, error) {
	return commandResultWithTimeout(ctx, dir, timeout, append([]string{"git"}, args...))
}

func buildGitPatch(ctx context.Context, dir string, timeout time.Duration) (string, error) {
	trackedPatch, trackedErrOutput, err := gitOutputWithTimeout(ctx, dir, timeout, "diff", "--binary", "--no-color", "--no-ext-diff", "HEAD", "--", ".")
	if err != nil {
		return "", fmt.Errorf("git diff tracked files: %w: %s", err, strings.TrimSpace(trackedErrOutput))
	}
	untrackedOutput, untrackedErrOutput, err := gitOutputWithTimeout(ctx, dir, timeout, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return "", fmt.Errorf("git ls-files --others --exclude-standard: %w: %s", err, strings.TrimSpace(untrackedErrOutput))
	}
	var patch strings.Builder
	patch.WriteString(trackedPatch)
	for _, line := range strings.Split(strings.ReplaceAll(untrackedOutput, "\r\n", "\n"), "\n") {
		file := strings.TrimSpace(line)
		if file == "" {
			continue
		}
		diffOutput, diffErrOutput, exitCode, diffErr := gitResultWithTimeout(ctx, dir, timeout, "diff", "--binary", "--no-color", "--no-ext-diff", "--no-index", "--", "/dev/null", file)
		if diffErr != nil && exitCode != 1 {
			return "", fmt.Errorf("git diff untracked file %q: %w: %s", file, diffErr, strings.TrimSpace(diffErrOutput))
		}
		patch.WriteString(diffOutput)
	}
	return patch.String(), nil
}
