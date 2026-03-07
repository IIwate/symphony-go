package shell

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestResolveBashPathWindowsPrefersGitBash(t *testing.T) {
	cleanup := stubShellGlobals()
	defer cleanup()

	runtimeGOOS = "windows"
	envLookup = func(name string) string {
		switch name {
		case "ProgramFiles":
			return "C:/Program Files"
		default:
			return ""
		}
	}
	fileExists = func(path string) bool {
		return path == filepath.Join("C:/Program Files", "Git", "bin", "bash.exe")
	}
	lookPath = func(file string) (string, error) {
		return "C:/Windows/System32/bash.exe", nil
	}

	got, err := resolveBashPath()
	if err != nil {
		t.Fatalf("resolveBashPath() error = %v", err)
	}
	want := filepath.Join("C:/Program Files", "Git", "bin", "bash.exe")
	if got != want {
		t.Fatalf("resolveBashPath() = %q, want %q", got, want)
	}
}

func TestResolveBashPathWindowsFallsBackToPathLookup(t *testing.T) {
	cleanup := stubShellGlobals()
	defer cleanup()

	runtimeGOOS = "windows"
	envLookup = func(string) string { return "" }
	fileExists = func(string) bool { return false }
	lookPath = func(file string) (string, error) {
		return "C:/custom/bash.exe", nil
	}

	got, err := resolveBashPath()
	if err != nil {
		t.Fatalf("resolveBashPath() error = %v", err)
	}
	if got != "C:/custom/bash.exe" {
		t.Fatalf("resolveBashPath() = %q, want %q", got, "C:/custom/bash.exe")
	}
}

func TestResolveBashPathNonWindowsUsesPathLookup(t *testing.T) {
	cleanup := stubShellGlobals()
	defer cleanup()

	runtimeGOOS = "linux"
	lookPath = func(file string) (string, error) {
		return "/usr/bin/bash", nil
	}

	got, err := resolveBashPath()
	if err != nil {
		t.Fatalf("resolveBashPath() error = %v", err)
	}
	if got != "/usr/bin/bash" {
		t.Fatalf("resolveBashPath() = %q, want %q", got, "/usr/bin/bash")
	}
}

func TestBashCommandBuildsCommand(t *testing.T) {
	cleanup := stubShellGlobals()
	defer cleanup()

	runtimeGOOS = "windows"
	envLookup = func(name string) string {
		if name == "ProgramFiles" {
			return "C:/Program Files"
		}
		return ""
	}
	fileExists = func(path string) bool {
		return path == filepath.Join("C:/Program Files", "Git", "bin", "bash.exe")
	}
	lookPath = func(file string) (string, error) {
		return "", exec.ErrNotFound
	}

	cmd, err := BashCommand(context.Background(), "H:/code/temp/symphony-go", "echo ok")
	if err != nil {
		t.Fatalf("BashCommand() error = %v", err)
	}
	wantPath := filepath.Join("C:/Program Files", "Git", "bin", "bash.exe")
	if cmd.Path != wantPath {
		t.Fatalf("cmd.Path = %q, want %q", cmd.Path, wantPath)
	}
	if cmd.Dir != "H:/code/temp/symphony-go" {
		t.Fatalf("cmd.Dir = %q, want %q", cmd.Dir, "H:/code/temp/symphony-go")
	}
	if len(cmd.Args) != 3 || cmd.Args[1] != "-lc" || cmd.Args[2] != "echo ok" {
		t.Fatalf("cmd.Args = %#v, want [bash -lc echo ok]", cmd.Args)
	}
}

func stubShellGlobals() func() {
	oldRuntimeGOOS := runtimeGOOS
	oldEnvLookup := envLookup
	oldFileExists := fileExists
	oldLookPath := lookPath
	return func() {
		runtimeGOOS = oldRuntimeGOOS
		envLookup = oldEnvLookup
		fileExists = oldFileExists
		lookPath = oldLookPath
	}
}
