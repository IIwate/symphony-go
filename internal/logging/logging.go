package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type Options struct {
	Level    string
	FilePath string
	Stderr   io.Writer
}

func NewLogger(opts Options) (*slog.Logger, io.Closer, error) {
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	level, err := parseLevel(opts.Level)
	if err != nil {
		return nil, nil, err
	}

	writers := []io.Writer{stderr}
	closers := make([]io.Closer, 0, 1)
	if path := strings.TrimSpace(opts.FilePath); path != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, nil, err
		}
		file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, nil, err
		}
		writers = append(writers, file)
		closers = append(closers, file)
	}

	logger := newLoggerWithWriters(level, writers, stderr)
	return logger, multiCloser{closers: closers}, nil
}

func WithIssue(logger *slog.Logger, issueID string, issueIdentifier string) *slog.Logger {
	if logger == nil {
		return nil
	}

	return logger.With("issue_id", issueID, "issue_identifier", issueIdentifier)
}

func WithSession(logger *slog.Logger, sessionID string) *slog.Logger {
	if logger == nil {
		return nil
	}

	return logger.With("session_id", sessionID)
}

type multiCloser struct {
	closers []io.Closer
}

func (m multiCloser) Close() error {
	var firstErr error
	for _, closer := range m.closers {
		if closer == nil {
			continue
		}
		if err := closer.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}

	return firstErr
}

type fanoutWriter struct {
	mu      sync.Mutex
	writers []io.Writer
	warning io.Writer
}

func (w *fanoutWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	var firstErr error
	var successCount int

	for _, writer := range w.writers {
		if writer == nil {
			continue
		}
		if _, err := writer.Write(p); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			w.warn(writer, err)
			continue
		}
		successCount++
	}

	if successCount > 0 {
		return len(p), nil
	}
	if firstErr != nil {
		return 0, firstErr
	}

	return len(p), nil
}

func (w *fanoutWriter) warn(failed io.Writer, err error) {
	if w.warning == nil || w.warning == failed {
		return
	}
	_, _ = fmt.Fprintf(w.warning, "logging sink failed: %v\n", err)
}

func newLoggerWithWriters(level slog.Level, writers []io.Writer, warning io.Writer) *slog.Logger {
	handler := slog.NewJSONHandler(&fanoutWriter{writers: writers, warning: warning}, &slog.HandlerOptions{
		Level:       level,
		ReplaceAttr: redactSecrets,
	})

	return slog.New(handler)
}

func redactSecrets(_ []string, attr slog.Attr) slog.Attr {
	key := strings.ToLower(attr.Key)
	if strings.Contains(key, "token") || strings.Contains(key, "secret") || strings.Contains(key, "password") || strings.Contains(key, "authorization") || strings.Contains(key, "api_key") {
		return slog.String(attr.Key, "***masked***")
	}

	return attr
}

func parseLevel(value string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unsupported log level %q", value)
	}
}
