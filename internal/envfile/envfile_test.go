package envfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadKeyValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env.local")
	if err := os.WriteFile(path, []byte("LINEAR_API_KEY=secret\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	t.Setenv("LINEAR_API_KEY", "")
	if err := os.Unsetenv("LINEAR_API_KEY"); err != nil {
		t.Fatalf("Unsetenv() error = %v", err)
	}

	if err := Load(path); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := os.Getenv("LINEAR_API_KEY"); got != "secret" {
		t.Fatalf("LINEAR_API_KEY = %q, want secret", got)
	}
}

func TestLoadCommentsAndQuotes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env.local")
	content := "# comment\n\nTOKEN=\"quoted value\"\nPLAIN='plain value'\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if err := os.Unsetenv("TOKEN"); err != nil {
		t.Fatalf("Unsetenv(TOKEN) error = %v", err)
	}
	if err := os.Unsetenv("PLAIN"); err != nil {
		t.Fatalf("Unsetenv(PLAIN) error = %v", err)
	}

	if err := Load(path); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := os.Getenv("TOKEN"); got != "quoted value" {
		t.Fatalf("TOKEN = %q, want quoted value", got)
	}
	if got := os.Getenv("PLAIN"); got != "plain value" {
		t.Fatalf("PLAIN = %q, want plain value", got)
	}
}

func TestLoadPreservesExistingEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env.local")
	if err := os.WriteFile(path, []byte("LINEAR_API_KEY=secret\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	t.Setenv("LINEAR_API_KEY", "existing")
	if err := Load(path); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := os.Getenv("LINEAR_API_KEY"); got != "existing" {
		t.Fatalf("LINEAR_API_KEY = %q, want existing", got)
	}
}

func TestLoadMissingFileIsIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.env")
	if err := Load(path); err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
}
