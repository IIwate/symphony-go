package testutil

import (
	"os"
	"strings"
	"testing"
)

func RequireEnv(t *testing.T, key string) string {
	t.Helper()

	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		t.Skipf("set %s to run integration tests", key)
	}
	return value
}
