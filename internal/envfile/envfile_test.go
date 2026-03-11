package envfile

import (
	"os"
	"path/filepath"
	"strings"
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

func TestUpsertCreatesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env.local")
	if err := Upsert(path, "LINEAR_API_KEY", "secret"); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(content) != "LINEAR_API_KEY=secret\n" {
		t.Fatalf("file content = %q, want single key", string(content))
	}
}

func TestUpsertCreatesParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "local", "env.local")
	if err := Upsert(path, "LINEAR_API_KEY", "secret"); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
}

func TestUpsertUpdatesExistingKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env.local")
	if err := os.WriteFile(path, []byte("LINEAR_API_KEY=old\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if err := Upsert(path, "LINEAR_API_KEY", "new"); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(content) != "LINEAR_API_KEY=new\n" {
		t.Fatalf("file content = %q, want updated key", string(content))
	}
}

func TestUpsertAppendsNewKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env.local")
	if err := os.WriteFile(path, []byte("LINEAR_API_KEY=secret\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if err := Upsert(path, "LINEAR_PROJECT_SLUG", "demo"); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(content) != "LINEAR_API_KEY=secret\nLINEAR_PROJECT_SLUG=demo\n" {
		t.Fatalf("file content = %q, want appended key", string(content))
	}
}

func TestUpsertPreservesCommentsAndBlankLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env.local")
	content := "# comment\n\nLINEAR_API_KEY=secret\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if err := Upsert(path, "LINEAR_PROJECT_SLUG", "demo"); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(got) != "# comment\n\nLINEAR_API_KEY=secret\nLINEAR_PROJECT_SLUG=demo\n" {
		t.Fatalf("file content = %q, want comments preserved", string(got))
	}
}

func TestUpsertQuotesAndLoadParsesEscapedValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env.local")
	value := `value with # and "quotes"`
	if err := Upsert(path, "TOKEN", value); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !strings.Contains(string(content), `TOKEN="value with # and \"quotes\""`) {
		t.Fatalf("file content = %q, want quoted value", string(content))
	}

	if err := os.Unsetenv("TOKEN"); err != nil {
		t.Fatalf("Unsetenv() error = %v", err)
	}
	if err := Load(path); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := os.Getenv("TOKEN"); got != value {
		t.Fatalf("TOKEN = %q, want %q", got, value)
	}
}
