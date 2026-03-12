"""Symphony live smoke 配置与运行辅助。"""

from __future__ import annotations

from dataclasses import dataclass
import json
import os
import socket
from pathlib import Path
import sys
from typing import Any
from urllib import error as urlerror
from urllib import request

from live_smoke.paths import bash_single_quote, repo_root, temp_root, to_bash_path
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


def allocate_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
        sock.bind(("127.0.0.1", 0))
        return int(sock.getsockname()[1])


def build_fake_app_server_command() -> str:
    python_path = bash_single_quote(to_bash_path(Path(sys.executable)))
    server_path = bash_single_quote(to_bash_path(repo_root() / "scripts" / "lib" / "live_smoke" / "fake_app_server.py"))
    return f"{python_path} {server_path}"


def symphony_binary_name() -> str:
    return "symphony.exe" if os.name == "nt" else "symphony"


def symphony_command(binary_path: Path, *args: str) -> list[str]:
    return [str(binary_path), *args]


def write_smoke_config(config: SmokeConfig, *, prompt_text: str) -> None:
    for name in ["sources", "flows", "prompts", "hooks", "local"]:
        (config.base_dir / name).mkdir(parents=True, exist_ok=True)

    command = json.dumps(build_fake_app_server_command())
    workspace_root = str((temp_root() / f"workspaces-{config.namespace}").resolve()).replace("\\", "/")
    (config.base_dir / "project.yaml").write_text(
        f"""runtime:
  polling:
    interval_ms: 3000
  workspace:
    root: {workspace_root}
    branch_namespace: {config.namespace}
  agent:
    max_turns: 1
  codex:
    command: {command}
    turn_timeout_ms: 30000
    read_timeout_ms: 5000
    stall_timeout_ms: 30000
  server:
    port: {config.port}
selection:
  dispatch_flow: implement
  enabled_sources:
    - linear-main
defaults:
  profile: null
""",
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


def start_symphony(binary_path: Path, config_dir: Path, *, echo: bool = True) -> ManagedProcess:
    return ManagedProcess(symphony_command(binary_path, "--config-dir", str(config_dir), "--log-level", "debug"), cwd=repo_root(), echo=echo)


def fetch_json(url: str) -> dict[str, Any]:
    with request.urlopen(url, timeout=5) as resp:
        return json.loads(resp.read().decode("utf-8"))


def fetch_issue_state(base_url: str, identifier: str) -> tuple[int, dict[str, Any] | None]:
    url = f"{base_url}/api/v1/{identifier}"
    try:
        return 200, fetch_json(url)
    except urlerror.HTTPError as exc:
        if exc.code == 404:
            return 404, json.loads(exc.read().decode("utf-8"))
        raise
