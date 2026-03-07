package logging

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewLoggerWritesToStderrAndFile(t *testing.T) {
	var stderr bytes.Buffer
	logPath := filepath.Join(t.TempDir(), "symphony.log")

	logger, closer, err := NewLogger(Options{Level: "info", FilePath: logPath, Stderr: &stderr})
	if err != nil {
		t.Fatalf("NewLogger() error = %v", err)
	}
	defer closer.Close()

	WithSession(WithIssue(logger, "id-1", "ABC-1"), "session-1").Info("cycle2 ready")

	stderrText := stderr.String()
	if !strings.Contains(stderrText, "cycle2 ready") || !strings.Contains(stderrText, "issue_id") || !strings.Contains(stderrText, "session_id") {
		t.Fatalf("stderr output = %q", stderrText)
	}

	content, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	fileText := string(content)
	if !strings.Contains(fileText, "cycle2 ready") || !strings.Contains(fileText, "ABC-1") {
		t.Fatalf("file output = %q", fileText)
	}
}

func TestNewLoggerMasksSecrets(t *testing.T) {
	var stderr bytes.Buffer
	logger, closer, err := NewLogger(Options{Level: "info", Stderr: &stderr})
	if err != nil {
		t.Fatalf("NewLogger() error = %v", err)
	}
	defer closer.Close()

	logger.Info("secret check", "api_key", "super-secret", "authorization", "Bearer 123")

	output := stderr.String()
	if strings.Contains(output, "super-secret") || strings.Contains(output, "Bearer 123") {
		t.Fatalf("secret leaked in log output: %q", output)
	}
	if !strings.Contains(output, "***masked***") {
		t.Fatalf("masked marker missing from output: %q", output)
	}
}

func TestFanoutWriterSurvivesSinkFailure(t *testing.T) {
	var good bytes.Buffer
	logger := newLoggerWithWriters(slog.LevelInfo, []io.Writer{failingWriter{}, &good}, &good)

	logger.Info("keep going")

	if !strings.Contains(good.String(), "keep going") {
		t.Fatalf("good sink output = %q", good.String())
	}
	if !strings.Contains(good.String(), "logging sink failed") {
		t.Fatalf("warning output missing: %q", good.String())
	}
}

type failingWriter struct{}

func (failingWriter) Write(_ []byte) (int, error) {
	return 0, errors.New("boom")
}
