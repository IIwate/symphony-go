package loader

import "testing"

func TestDeepMergeMapRecursive(t *testing.T) {
	base := map[string]any{
		"runtime": map[string]any{
			"agent": map[string]any{
				"max_turns": 20,
			},
			"codex": map[string]any{
				"command": "codex app-server",
			},
		},
	}
	override := map[string]any{
		"runtime": map[string]any{
			"agent": map[string]any{
				"max_turns": 5,
			},
		},
	}

	merged := mustStringMap(deepMerge(base, override))
	runtimeConfig := getMapValue(merged, "runtime")
	agentConfig := getMapValue(runtimeConfig, "agent")
	codexConfig := getMapValue(runtimeConfig, "codex")
	if got := agentConfig["max_turns"]; got != 5 {
		t.Fatalf("runtime.agent.max_turns = %v, want 5", got)
	}
	if got := codexConfig["command"]; got != "codex app-server" {
		t.Fatalf("runtime.codex.command = %v, want codex app-server", got)
	}
}

func TestDeepMergeArrayReplace(t *testing.T) {
	base := map[string]any{
		"selection": map[string]any{
			"enabled_sources": []any{"linear-main"},
		},
	}
	override := map[string]any{
		"selection": map[string]any{
			"enabled_sources": []any{"github-core"},
		},
	}

	merged := mustStringMap(deepMerge(base, override))
	values := getStringSliceValue(getMapValue(merged, "selection"), "enabled_sources")
	if len(values) != 1 || values[0] != "github-core" {
		t.Fatalf("enabled_sources = %v, want [github-core]", values)
	}
}

func TestDeepMergeNullExplicitClear(t *testing.T) {
	base := map[string]any{
		"runtime": map[string]any{
			"server": map[string]any{
				"port": 8080,
			},
		},
	}
	override := map[string]any{
		"runtime": map[string]any{
			"server": map[string]any{
				"port": nil,
			},
		},
	}

	merged := mustStringMap(deepMerge(base, override))
	serverConfig := getMapValue(getMapValue(merged, "runtime"), "server")
	value, ok := serverConfig["port"]
	if !ok {
		t.Fatal("runtime.server.port missing after explicit null")
	}
	if value != nil {
		t.Fatalf("runtime.server.port = %v, want nil", value)
	}
}

func TestDeepMergeMissingKeyPreserve(t *testing.T) {
	base := map[string]any{
		"runtime": map[string]any{
			"workspace": map[string]any{
				"root": "~/workspaces",
			},
		},
	}
	override := map[string]any{
		"selection": map[string]any{
			"dispatch_flow": "implement",
		},
	}

	merged := mustStringMap(deepMerge(base, override))
	workspaceConfig := getMapValue(getMapValue(merged, "runtime"), "workspace")
	if got := workspaceConfig["root"]; got != "~/workspaces" {
		t.Fatalf("runtime.workspace.root = %v, want ~/workspaces", got)
	}
}
