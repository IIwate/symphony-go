package loader

import "testing"

func TestDeepMergeMapRecursive(t *testing.T) {
	base := map[string]any{
		"execution": map[string]any{
			"agent": map[string]any{
				"max_turns": 20,
			},
			"backend": map[string]any{
				"codex": map[string]any{
					"command": "codex app-server",
				},
			},
		},
	}
	override := map[string]any{
		"execution": map[string]any{
			"agent": map[string]any{
				"max_turns": 5,
			},
		},
	}

	merged := mustStringMap(deepMerge(base, override))
	executionConfig := getMapValue(merged, "execution")
	agentConfig := getMapValue(executionConfig, "agent")
	codexConfig := getMapValue(getMapValue(executionConfig, "backend"), "codex")
	if got := agentConfig["max_turns"]; got != 5 {
		t.Fatalf("execution.agent.max_turns = %v, want 5", got)
	}
	if got := codexConfig["command"]; got != "codex app-server" {
		t.Fatalf("execution.backend.codex.command = %v, want codex app-server", got)
	}
}

func TestDeepMergeArrayReplace(t *testing.T) {
	base := map[string]any{
		"sources": map[string]any{
			"enabled": []any{"linear-main"},
		},
	}
	override := map[string]any{
		"sources": map[string]any{
			"enabled": []any{"github-core"},
		},
	}

	merged := mustStringMap(deepMerge(base, override))
	values := getStringSliceValue(getMapValue(merged, "sources"), "enabled")
	if len(values) != 1 || values[0] != "github-core" {
		t.Fatalf("sources.enabled = %v, want [github-core]", values)
	}
}

func TestDeepMergeNullExplicitClear(t *testing.T) {
	base := map[string]any{
		"service": map[string]any{
			"server": map[string]any{
				"port": 8080,
			},
		},
	}
	override := map[string]any{
		"service": map[string]any{
			"server": map[string]any{
				"port": nil,
			},
		},
	}

	merged := mustStringMap(deepMerge(base, override))
	serverConfig := getMapValue(getMapValue(merged, "service"), "server")
	value, ok := serverConfig["port"]
	if !ok {
		t.Fatal("service.server.port missing after explicit null")
	}
	if value != nil {
		t.Fatalf("service.server.port = %v, want nil", value)
	}
}

func TestDeepMergeMissingKeyPreserve(t *testing.T) {
	base := map[string]any{
		"domain": map[string]any{
			"workspace": map[string]any{
				"root": "~/workspaces",
			},
		},
	}
	override := map[string]any{
		"job_policy": map[string]any{
			"dispatch_flow": "implement",
		},
	}

	merged := mustStringMap(deepMerge(base, override))
	workspaceConfig := getMapValue(getMapValue(merged, "domain"), "workspace")
	if got := workspaceConfig["root"]; got != "~/workspaces" {
		t.Fatalf("domain.workspace.root = %v, want ~/workspaces", got)
	}
}
