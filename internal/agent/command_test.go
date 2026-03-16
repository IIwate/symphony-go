package agent

import (
	"context"
	"testing"
)

func TestParseCommandStringSplitsSimpleCommand(t *testing.T) {
	got, err := parseCommandString("codex app-server --port 8080")
	if err != nil {
		t.Fatalf("parseCommandString() error = %v", err)
	}
	want := []string{"codex", "app-server", "--port", "8080"}
	assertCommandArgsEqual(t, got, want)
}

func TestParseCommandStringPreservesQuotedSegments(t *testing.T) {
	got, err := parseCommandString(`python "C:\Program Files\tool.py" --label "hello world"`)
	if err != nil {
		t.Fatalf("parseCommandString() error = %v", err)
	}
	want := []string{"python", `C:\Program Files\tool.py`, "--label", "hello world"}
	assertCommandArgsEqual(t, got, want)
}

func TestParseCommandStringRejectsUnterminatedQuote(t *testing.T) {
	if _, err := parseCommandString(`codex "app-server`); err == nil {
		t.Fatal("parseCommandString() error = nil, want unterminated quote")
	}
}

func TestCommandFromStringBuildsExecCommand(t *testing.T) {
	cmd, err := commandFromString(context.Background(), "H:/code/temp/symphony-go", `codex app-server --port "8080"`)
	if err != nil {
		t.Fatalf("commandFromString() error = %v", err)
	}
	if cmd.Dir != "H:/code/temp/symphony-go" {
		t.Fatalf("cmd.Dir = %q, want H:/code/temp/symphony-go", cmd.Dir)
	}
	want := []string{"codex", "app-server", "--port", "8080"}
	assertCommandArgsEqual(t, cmd.Args, want)
}

func assertCommandArgsEqual(t *testing.T, got []string, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("len(args) = %d, want %d; args = %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("args[%d] = %q, want %q; args = %#v", i, got[i], want[i], got)
		}
	}
}
