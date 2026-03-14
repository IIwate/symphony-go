"""Symphony live smoke 配置与运行辅助。"""

from __future__ import annotations

from dataclasses import dataclass
import json
import os
import socket
from pathlib import Path
from typing import Any
from urllib import request

from live_smoke.paths import repo_root, temp_root
from live_smoke.shell import ManagedProcess


@dataclass
class SmokeConfig:
    base_dir: Path
    port: int
    namespace: str
    repo_url: str
    linear_api_key: str
    linear_project_slug: str
    linear_branch_scope: str
    codex_command: str = "codex app-server"
    ledger_path: Path | None = None
    notification_port: int | None = None
    broken_notification_port: int | None = None
    broken_notification_channels: tuple[str, ...] = ()


@dataclass
class SSEEvent:
    event: str
    data: str


def allocate_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
        sock.bind(("127.0.0.1", 0))
        return int(sock.getsockname()[1])


def symphony_binary_name() -> str:
    return "symphony.exe" if os.name == "nt" else "symphony"


def symphony_command(binary_path: Path, *args: str) -> list[str]:
    return [str(binary_path), *args]


def symphony_run_command(binary_path: Path, *args: str) -> list[str]:
    return symphony_command(binary_path, "run", *args)


def symphony_doctor_command(binary_path: Path, *args: str) -> list[str]:
    return symphony_command(binary_path, "doctor", *args)


def write_smoke_config(config: SmokeConfig, *, prompt_text: str) -> None:
    for name in ["sources", "flows", "prompts", "hooks", "local"]:
        (config.base_dir / name).mkdir(parents=True, exist_ok=True)

    command = json.dumps(config.codex_command)
    workspace_root = str((temp_root() / f"workspaces-{config.namespace}").resolve()).replace("\\", "/")
    project_lines = [
        "runtime:",
        "  polling:",
        "    interval_ms: 3000",
        "  workspace:",
        f"    root: {workspace_root}",
        f"    branch_namespace: {config.namespace}",
        "  agent:",
        "    max_turns: 1",
        "  codex:",
        f"    command: {command}",
        "    approval_policy: never",
        "    thread_sandbox: workspace-write",
        "    turn_sandbox_policy:",
        "      type: workspaceWrite",
        "    turn_timeout_ms: 120000",
        "    read_timeout_ms: 15000",
        "    stall_timeout_ms: 120000",
        "  server:",
        f"    port: {config.port}",
    ]
    if config.ledger_path is not None:
        ledger_path = str(config.ledger_path.resolve()).replace("\\", "/")
        project_lines.extend(
            [
                "  session_persistence:",
                "    enabled: true",
                "    kind: file",
                "    file:",
                f"      path: {ledger_path}",
                "      flush_interval_ms: 200",
                "      fsync_on_critical: true",
            ]
        )
    if config.notification_port is not None:
        broken_channels = set(config.broken_notification_channels)
        broken_port = config.broken_notification_port if config.broken_notification_port is not None else config.notification_port
        webhook_port = broken_port if "local-webhook" in broken_channels else config.notification_port
        slack_port = broken_port if "local-slack" in broken_channels else config.notification_port
        project_lines.extend(
            [
                "  notifications:",
                "    channels:",
                "      - id: local-webhook",
                "        display_name: Local Webhook",
                "        kind: webhook",
                "        subscriptions:",
                "          types: [issue_intervention_required, issue_completed]",
                "        webhook:",
                f"          url: http://127.0.0.1:{webhook_port}/webhook",
                "      - id: local-slack",
                "        display_name: Local Slack",
                "        kind: slack",
                "        subscriptions:",
                "          types: [issue_intervention_required, issue_completed]",
                "        slack:",
                f"          incoming_webhook_url: http://127.0.0.1:{slack_port}/slack",
                "    defaults:",
                "      timeout_ms: 3000",
                "      retry_count: 0",
                "      retry_delay_ms: 0",
                "      queue_size: 32",
                "      critical_queue_size: 8",
            ]
        )
    project_lines.extend(
        [
            "selection:",
            "  dispatch_flow: implement",
            "  enabled_sources:",
            "    - linear-main",
            "defaults:",
            "  profile: null",
        ]
    )
    (config.base_dir / "project.yaml").write_text(
        "\n".join(project_lines) + "\n",
        encoding="utf-8",
    )
    (config.base_dir / "sources" / "linear-main.yaml").write_text(
        """kind: linear
api_key: $LINEAR_API_KEY
endpoint: https://api.linear.app/graphql
project_slug: $LINEAR_PROJECT_SLUG
branch_scope: $LINEAR_BRANCH_SCOPE
active_states:
  - Todo
  - In Progress
terminal_states:
  - Closed
  - Cancelled
  - Canceled
  - Duplicate
  - Done
""",
        encoding="utf-8",
    )
    (config.base_dir / "flows" / "implement.yaml").write_text(
        """prompt: prompts/implement.md.liquid
hooks:
  before_run: hooks/before_run.sh
  before_run_continuation: hooks/before_run_continuation.sh
completion:
  mode: pull_request
  on_missing_pr: intervention
  on_closed_pr: intervention
""",
        encoding="utf-8",
    )
    (config.base_dir / "prompts" / "implement.md.liquid").write_text(prompt_text + "\n", encoding="utf-8")
    (config.base_dir / "hooks" / "before_run.sh").write_text(
        """set -euo pipefail

repo_url="${SYMPHONY_GIT_REPO_URL:?SYMPHONY_GIT_REPO_URL is required}"
find . -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +
git clone --depth 1 "$repo_url" .
""",
        encoding="utf-8",
    )
    (config.base_dir / "hooks" / "before_run_continuation.sh").write_text(
        """set -euo pipefail

if [[ ! -d .git ]]; then
  repo_url="${SYMPHONY_GIT_REPO_URL:?SYMPHONY_GIT_REPO_URL is required}"
  find . -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +
  git clone --depth 1 "$repo_url" .
  exit 0
fi

git status --short
git fetch --all --prune || true
""",
        encoding="utf-8",
    )
    (config.base_dir / "local" / "env.local").write_text(
        (
            f"LINEAR_API_KEY={config.linear_api_key}\n"
            f"LINEAR_PROJECT_SLUG={config.linear_project_slug}\n"
            f"LINEAR_BRANCH_SCOPE={config.linear_branch_scope}\n"
            f"SYMPHONY_GIT_REPO_URL={config.repo_url}\n"
        ),
        encoding="utf-8",
    )


def write_doctor_config(base_dir: Path) -> None:
    for name in ["sources", "flows", "prompts", "local"]:
        (base_dir / name).mkdir(parents=True, exist_ok=True)
    (base_dir / "project.yaml").write_text(
        """runtime:
  codex:
    command: $CODEX_COMMAND
selection:
  dispatch_flow: implement
  enabled_sources:
    - linear-main
defaults:
  profile: null
""",
        encoding="utf-8",
    )
    (base_dir / "sources" / "linear-main.yaml").write_text(
        """kind: linear
api_key: $LINEAR_API_KEY
project_slug: $LINEAR_PROJECT_SLUG
branch_scope: $LINEAR_BRANCH_SCOPE
active_states: ["Todo", "In Progress"]
terminal_states: ["Closed", "Done"]
""",
        encoding="utf-8",
    )
    (base_dir / "flows" / "implement.yaml").write_text("prompt: prompts/implement.md.liquid\n", encoding="utf-8")
    (base_dir / "prompts" / "implement.md.liquid").write_text("doctor smoke\n", encoding="utf-8")


def write_inline_hook_config(base_dir: Path, *, linear_api_key: str, linear_project_slug: str, linear_branch_scope: str) -> None:
    for name in ["sources", "flows", "prompts", "local"]:
        (base_dir / name).mkdir(parents=True, exist_ok=True)
    (base_dir / "project.yaml").write_text(
        """runtime:
  codex:
    command: codex app-server
selection:
  dispatch_flow: implement
  enabled_sources:
    - linear-main
defaults:
  profile: null
""",
        encoding="utf-8",
    )
    (base_dir / "sources" / "linear-main.yaml").write_text(
        """kind: linear
api_key: $LINEAR_API_KEY
endpoint: https://api.linear.app/graphql
project_slug: $LINEAR_PROJECT_SLUG
branch_scope: $LINEAR_BRANCH_SCOPE
active_states: ["Todo", "In Progress"]
terminal_states: ["Closed", "Done"]
""",
        encoding="utf-8",
    )
    (base_dir / "flows" / "implement.yaml").write_text(
        'prompt: prompts/implement.md.liquid\nhooks:\n  before_run: "git remote set-url origin https://example.test/repo.git"\n',
        encoding="utf-8",
    )
    (base_dir / "prompts" / "implement.md.liquid").write_text("inline hook smoke\n", encoding="utf-8")
    (base_dir / "local" / "env.local").write_text(
        f"LINEAR_API_KEY={linear_api_key}\nLINEAR_PROJECT_SLUG={linear_project_slug}\nLINEAR_BRANCH_SCOPE={linear_branch_scope}\n",
        encoding="utf-8",
    )


def write_symlink_escape_config(base_dir: Path, *, linear_api_key: str, linear_project_slug: str, linear_branch_scope: str) -> bool:
    for name in ["sources", "flows", "prompts", "hooks", "local"]:
        (base_dir / name).mkdir(parents=True, exist_ok=True)
    (base_dir / "project.yaml").write_text(
        """runtime:
  codex:
    command: codex app-server
selection:
  dispatch_flow: implement
  enabled_sources:
    - linear-main
defaults:
  profile: null
""",
        encoding="utf-8",
    )
    (base_dir / "sources" / "linear-main.yaml").write_text(
        """kind: linear
api_key: $LINEAR_API_KEY
project_slug: $LINEAR_PROJECT_SLUG
branch_scope: $LINEAR_BRANCH_SCOPE
active_states: ["Todo", "In Progress"]
terminal_states: ["Closed", "Done"]
""",
        encoding="utf-8",
    )
    (base_dir / "flows" / "implement.yaml").write_text(
        "prompt: prompts/implement.md.liquid\nhooks:\n  before_run: hooks/link.sh\n",
        encoding="utf-8",
    )
    (base_dir / "prompts" / "implement.md.liquid").write_text("symlink smoke\n", encoding="utf-8")
    (base_dir / "local" / "env.local").write_text(
        f"LINEAR_API_KEY={linear_api_key}\nLINEAR_PROJECT_SLUG={linear_project_slug}\nLINEAR_BRANCH_SCOPE={linear_branch_scope}\n",
        encoding="utf-8",
    )
    outside = base_dir.parent / "outside.sh"
    outside.write_text("echo outside\n", encoding="utf-8")
    try:
        (base_dir / "hooks" / "link.sh").symlink_to(outside)
        return True
    except OSError:
        return False


def start_symphony(binary_path: Path, config_dir: Path, *, echo: bool = True, env: dict[str, str] | None = None) -> ManagedProcess:
    return ManagedProcess(
        symphony_run_command(binary_path, "--config-dir", str(config_dir), "--log-level", "debug"),
        cwd=repo_root(),
        env=env,
        echo=echo,
    )


def fetch_json(url: str) -> dict[str, Any]:
    with request.urlopen(url, timeout=5) as resp:
        return json.loads(resp.read().decode("utf-8"))


def post_json(url: str, payload: dict[str, Any] | None = None) -> dict[str, Any]:
    raw = json.dumps(payload or {}).encode("utf-8")
    req = request.Request(
        url,
        data=raw,
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    with request.urlopen(req, timeout=5) as resp:
        return json.loads(resp.read().decode("utf-8"))

def open_events_stream(url: str, *, timeout_seconds: int = 600):
    req = request.Request(url, headers={"Accept": "text/event-stream"})
    return request.urlopen(req, timeout=timeout_seconds)


def read_sse_event(stream) -> SSEEvent:
    event_type = ""
    data_lines: list[str] = []
    while True:
        raw = stream.readline()
        if not raw:
            raise RuntimeError("SSE stream closed before event completed")
        line = raw.decode("utf-8").rstrip("\r\n")
        if not line:
            if event_type or data_lines:
                return SSEEvent(event=event_type, data="\n".join(data_lines))
            continue
        if line.startswith("event: "):
            event_type = line.removeprefix("event: ").strip()
            continue
        if line.startswith("data: "):
            data_lines.append(line.removeprefix("data: ").strip())
