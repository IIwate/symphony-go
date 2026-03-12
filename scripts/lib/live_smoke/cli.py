"""live smoke CLI。"""

from __future__ import annotations

import argparse
from dataclasses import dataclass, field
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import shutil
from threading import Lock, Thread
import time
from typing import Callable

from live_smoke.github import close_pull_request, ensure_gh_auth, git_env, merge_pull_request, prepare_pull_request
from live_smoke.linear import LinearClient, TeamContext
from live_smoke.paths import repo_root, temp_root
from live_smoke.shell import require_command, run
from live_smoke.symphony import (
    SmokeConfig,
    allocate_port,
    fetch_issue_state,
    fetch_json,
    start_symphony,
    symphony_binary_name,
    symphony_command,
    write_doctor_config,
    write_inline_hook_config,
    write_smoke_config,
    write_symlink_escape_config,
)

LIVE_SMOKE_PREFIX = "[live-smoke]"


@dataclass
class StepResult:
    name: str
    status: str
    detail: str


@dataclass
class Resources:
    temp_dir: Path
    issue_ids: list[str] = field(default_factory=list)
    pull_request_numbers: list[int] = field(default_factory=list)
    processes: list[object] = field(default_factory=list)
    binary_path: Path | None = None


class NotificationRecorder:
    def __init__(self) -> None:
        self._lock = Lock()
        self._events: list[dict[str, object]] = []

    def add(self, item: dict[str, object]) -> None:
        with self._lock:
            self._events.append(item)

    def count(self) -> int:
        with self._lock:
            return len(self._events)

    def find(self, *, path: str, identifier: str, event_type: str) -> list[dict[str, object]]:
        with self._lock:
            items = list(self._events)
        result: list[dict[str, object]] = []
        for item in items:
            if item.get("path") != path:
                continue
            body = item.get("body")
            if not isinstance(body, dict):
                continue
            if path == "/webhook":
                if body.get("identifier") != identifier or body.get("type") != event_type:
                    continue
            elif path == "/slack":
                text = str(body.get("text", ""))
                if identifier not in text or event_type not in text:
                    continue
            else:
                continue
            result.append(item)
        return result


class NotificationHandler(BaseHTTPRequestHandler):
    recorder: NotificationRecorder

    def do_POST(self) -> None:  # noqa: N802
        length = int(self.headers.get("Content-Length", "0"))
        raw = self.rfile.read(length)
        try:
            body = json.loads(raw.decode("utf-8"))
        except Exception:
            body = {"raw": raw.decode("utf-8", errors="replace")}
        self.recorder.add(
            {
                "path": self.path,
                "headers": dict(self.headers.items()),
                "body": body,
                "timestamp": time.time(),
            }
        )
        self.send_response(204)
        self.end_headers()

    def log_message(self, format: str, *args: object) -> None:
        return


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="live_smoke.py",
        description="Symphony live smoke validation tool",
    )
    parser.add_argument(
        "--phase",
        choices=["all", "light", "heavy"],
        default="all",
        help="Run phase: light / heavy / all",
    )
    parser.add_argument(
        "--keep-artifacts",
        action="store_true",
        help="Keep temporary artifacts for debugging",
    )
    parser.add_argument(
        "--purge-history",
        action="store_true",
        help="Archive old terminal smoke issues before running",
    )
    parser.add_argument(
        "--repo",
        default="IIwate/linear-test",
        help="GitHub repository used by live smoke",
    )
    parser.add_argument(
        "--linear-api-key",
        default="",
        help="Linear API Key. Defaults to env LINEAR_API_KEY",
    )
    parser.add_argument(
        "--linear-project-slug",
        default="",
        help="Linear project slugId. Defaults to env LINEAR_PROJECT_SLUG",
    )
    parser.add_argument(
        "--team-key",
        default="IIWATE",
        help="Linear team key. Default: IIWATE",
    )
    parser.add_argument(
        "--linear-branch-scope",
        default="integration-scope",
        help="branch_scope used by live smoke",
    )
    parser.add_argument(
        "--branch-namespace",
        default="live-smoke",
        help="Explicit workspace branch namespace used in generated smoke config",
    )
    parser.add_argument(
        "--echo-process-output",
        action="store_true",
        help="Echo symphony process output in real time",
    )
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    repo = args.repo
    repo_url = f"https://github.com/{repo}"
    linear_api_key = args.linear_api_key or _env_required("LINEAR_API_KEY")
    linear_project_slug = args.linear_project_slug or _env_required("LINEAR_PROJECT_SLUG")

    temp_dir = temp_root() / time.strftime("%Y%m%d-%H%M%S")
    temp_dir.mkdir(parents=True, exist_ok=True)
    resources = Resources(temp_dir=temp_dir)
    results: list[StepResult] = []

    linear = LinearClient(linear_api_key)
    context = linear.load_team_context(args.team_key, linear_project_slug)

    try:
        _run_step(results, "preflight", lambda: _preflight(repo))
        _run_step(results, "build_binary", lambda: _build_binary(resources))
        _run_step(results, "cleanup_active_smoke_issues", lambda: _cleanup_stale_smoke_issues(linear, context, linear_project_slug))
        if args.purge_history:
            _run_step(results, "purge_history", lambda: _purge_terminal_smoke_issues(linear, project_slug=linear_project_slug))

        if args.phase in {"all", "light"}:
            _run_step(results, "doctor_and_set", lambda: _run_doctor_and_set(resources))
            _run_step(
                results,
                "inline_hook_dry_run",
                lambda: _run_inline_hook_dry_run(resources, linear_api_key, linear_project_slug, args.linear_branch_scope),
            )
            _run_step(
                results,
                "symlink_escape",
                lambda: _run_symlink_escape_check(resources, linear_api_key, linear_project_slug, args.linear_branch_scope),
            )

        if args.phase in {"all", "heavy"}:
            _run_step(
                results,
                "missing_pr_intervention",
                lambda: _run_missing_pr_smoke(
                    resources,
                    linear,
                    context,
                    linear_project_slug,
                    repo_url,
                    args.linear_branch_scope,
                    args.branch_namespace,
                    args.echo_process_output,
                ),
            )
            _run_step(
                results,
                "runtime_extensions",
                lambda: _run_runtime_extensions_smoke(
                    resources,
                    linear,
                    context,
                    linear_project_slug,
                    repo,
                    repo_url,
                    args.linear_branch_scope,
                    args.branch_namespace,
                    args.echo_process_output,
                ),
            )
            _run_step(
                results,
                "awaiting_merge_to_done",
                lambda: _run_merge_path_smoke(
                    resources,
                    linear,
                    context,
                    linear_project_slug,
                    repo,
                    repo_url,
                    args.linear_branch_scope,
                    args.branch_namespace,
                    args.echo_process_output,
                ),
            )
    finally:
        for process in resources.processes:
            try:
                process.stop()
            except Exception:
                pass
        for pr_number in reversed(resources.pull_request_numbers):
            try:
                close_pull_request(repo, pr_number)
            except Exception:
                pass
        for issue_id in resources.issue_ids:
            try:
                linear.update_issue_state(issue_id, context.canceled_state_id)
            except Exception:
                pass
        if not args.keep_artifacts:
            shutil.rmtree(temp_dir, ignore_errors=True)

    _print_summary(results, temp_dir, args.keep_artifacts)
    return 0 if all(item.status == "PASS" for item in results) else 1


def _env_required(name: str) -> str:
    value = os.getenv(name, "").strip()
    if not value:
        raise RuntimeError(f"missing required environment variable: {name}")
    return value


def _preflight(repo: str) -> str:
    require_command("go")
    require_command("git")
    require_command("gh")
    require_command("py")
    ensure_gh_auth()
    run(["gh", "auth", "setup-git"], cwd=repo_root())
    run(["gh", "repo", "view", repo, "--json", "nameWithOwner"], cwd=repo_root())
    return f"repo={repo}"


def _build_binary(resources: Resources) -> str:
    binary_dir = resources.temp_dir / "bin"
    binary_dir.mkdir(parents=True, exist_ok=True)
    binary_path = binary_dir / symphony_binary_name()
    run(["go", "build", "-o", str(binary_path), "./cmd/symphony"], cwd=repo_root())
    resources.binary_path = binary_path
    return str(binary_path)


def _cleanup_stale_smoke_issues(linear: LinearClient, context: TeamContext, project_slug: str) -> str:
    stale = []
    for issue in linear.fetch_active_issues(project_slug):
        title = str(issue.get("title", ""))
        if title.startswith(LIVE_SMOKE_PREFIX):
            stale.append(issue)
    for issue in stale:
        linear.update_issue_state(str(issue["id"]), context.canceled_state_id)
    remaining = [
        issue
        for issue in linear.fetch_active_issues(project_slug)
        if not str(issue.get("title", "")).startswith(LIVE_SMOKE_PREFIX)
    ]
    if remaining:
        identifiers = ", ".join(str(item["identifier"]) for item in remaining)
        raise RuntimeError(f"target project still has active non-smoke issues: {identifiers}")
    return f"cleaned={len(stale)}"


def _purge_terminal_smoke_issues(linear: LinearClient, *, project_slug: str) -> str:
    purged = 0
    for issue in linear.fetch_smoke_issues(project_slug, LIVE_SMOKE_PREFIX):
        state_name = str(issue.get("state", {}).get("name", "")).strip()
        if state_name in {"Todo", "In Progress"}:
            continue
        linear.archive_issue(str(issue["id"]), trash=True)
        purged += 1
    return f"purged={purged}"


def _run_doctor_and_set(resources: Resources) -> str:
    if resources.binary_path is None:
        raise RuntimeError("symphony binary is not built")
    base_dir = resources.temp_dir / "doctor"
    write_doctor_config(base_dir)

    doctor = run(
        symphony_command(resources.binary_path, "config", "doctor", "--config-dir", str(base_dir)),
        cwd=repo_root(),
        check=False,
    )
    combined = doctor.stdout + doctor.stderr
    if doctor.returncode == 0 or "CODEX_COMMAND (runtime.codex.command)" not in combined:
        raise RuntimeError("config doctor did not report missing runtime.codex.command")

    set_result = run(
        [
            "pwsh",
            "-NoLogo",
            "-NoProfile",
            "-Command",
            f"'codex app-server' | & '{resources.binary_path}' config set CODEX_COMMAND --config-dir '{base_dir}' --non-interactive",
        ],
        cwd=repo_root(),
    )
    if "当前运行实例不会自动更新" not in set_result.stdout:
        raise RuntimeError("config set output missing runtime update warning")

    doctor_again = run(
        symphony_command(resources.binary_path, "config", "doctor", "--config-dir", str(base_dir)),
        cwd=repo_root(),
        check=False,
    )
    if "CODEX_COMMAND" in doctor_again.stdout + doctor_again.stderr:
        raise RuntimeError("config doctor still reports CODEX_COMMAND after config set")
    return "doctor/set ok"


def _run_inline_hook_dry_run(resources: Resources, linear_api_key: str, project_slug: str, branch_scope: str) -> str:
    if resources.binary_path is None:
        raise RuntimeError("symphony binary is not built")
    base_dir = resources.temp_dir / "inline-hook"
    write_inline_hook_config(base_dir, linear_api_key=linear_api_key, linear_project_slug=project_slug, linear_branch_scope=branch_scope)
    result = run(
        symphony_command(resources.binary_path, "--dry-run", "--config-dir", str(base_dir)),
        cwd=repo_root(),
    )
    combined = result.stdout + result.stderr
    if "dry-run 校验通过" not in combined:
        raise RuntimeError("inline hook dry-run did not succeed")
    return "inline hook ok"


def _run_symlink_escape_check(resources: Resources, linear_api_key: str, project_slug: str, branch_scope: str) -> str:
    if resources.binary_path is None:
        raise RuntimeError("symphony binary is not built")
    base_dir = resources.temp_dir / "symlink"
    if not write_symlink_escape_config(base_dir, linear_api_key=linear_api_key, linear_project_slug=project_slug, linear_branch_scope=branch_scope):
        return "skipped: symlink unsupported"
    result = run(
        symphony_command(resources.binary_path, "--dry-run", "--config-dir", str(base_dir)),
        cwd=repo_root(),
        check=False,
    )
    combined = result.stdout + result.stderr
    if result.returncode == 0 or "escapes automation directory" not in combined:
        raise RuntimeError("symlink escape was not rejected")
    return "symlink escape blocked"


def _run_missing_pr_smoke(
    resources: Resources,
    linear: LinearClient,
    context: TeamContext,
    project_slug: str,
    repo_url: str,
    branch_scope: str,
    branch_namespace: str,
    echo_output: bool,
) -> str:
    if resources.binary_path is None:
        raise RuntimeError("symphony binary is not built")
    issue = linear.create_issue(f"{LIVE_SMOKE_PREFIX} missing_pr {int(time.time())}", context)
    resources.issue_ids.append(str(issue["id"]))
    port = allocate_port()
    config = SmokeConfig(
        base_dir=resources.temp_dir / "missing-pr",
        port=port,
        namespace=branch_namespace,
        repo_url=repo_url,
        linear_api_key=linear.api_key,
        linear_project_slug=project_slug,
        linear_branch_scope=branch_scope,
    )
    write_smoke_config(
        config,
        prompt_text="Do not modify repository contents. Exit successfully without creating or updating a pull request.",
    )
    process = start_symphony(resources.binary_path, config.base_dir, echo=echo_output, env=git_env())
    resources.processes.append(process)
    base_url = f"http://127.0.0.1:{port}"
    _wait_for(lambda: fetch_json(f"{base_url}/api/v1/state"), process, timeout_seconds=30, description="symphony startup")
    payload = _wait_for(
        lambda: _await_issue_status(base_url, str(issue["identifier"]), "awaiting_intervention"),
        process,
        timeout_seconds=90,
        description="missing_pr intervention",
    )
    reason = payload["awaiting_intervention"]["reason"]
    if reason != "missing_pr":
        raise RuntimeError(f"unexpected intervention reason: {reason}")
    process.stop()
    resources.processes.remove(process)
    linear.update_issue_state(str(issue["id"]), context.canceled_state_id)
    resources.issue_ids.remove(str(issue["id"]))
    return f"{issue['identifier']} -> awaiting_intervention(missing_pr)"


def _run_merge_path_smoke(
    resources: Resources,
    linear: LinearClient,
    context: TeamContext,
    project_slug: str,
    repo: str,
    repo_url: str,
    branch_scope: str,
    branch_namespace: str,
    echo_output: bool,
) -> str:
    if resources.binary_path is None:
        raise RuntimeError("symphony binary is not built")
    issue = linear.create_issue(f"{LIVE_SMOKE_PREFIX} merge_path {int(time.time())}", context)
    resources.issue_ids.append(str(issue["id"]))
    branch = _linear_branch_name(branch_namespace, branch_scope, str(issue["identifier"]))
    pr = prepare_pull_request(
        repo,
        repo_url,
        branch,
        title=f"test: live smoke {issue['identifier']}",
        body="Temporary PR for symphony live smoke.",
        work_root=resources.temp_dir / "pr-prep",
    )
    resources.pull_request_numbers.append(pr.number)

    port = allocate_port()
    config = SmokeConfig(
        base_dir=resources.temp_dir / "merge-path",
        port=port,
        namespace=branch_namespace,
        repo_url=repo_url,
        linear_api_key=linear.api_key,
        linear_project_slug=project_slug,
        linear_branch_scope=branch_scope,
    )
    write_smoke_config(
        config,
        prompt_text="Do not modify repository contents. Exit successfully without creating or updating a pull request.",
    )
    process = start_symphony(resources.binary_path, config.base_dir, echo=echo_output, env=git_env())
    resources.processes.append(process)
    base_url = f"http://127.0.0.1:{port}"
    _wait_for(lambda: fetch_json(f"{base_url}/api/v1/state"), process, timeout_seconds=30, description="symphony startup")
    payload = _wait_for(
        lambda: _await_issue_status(base_url, str(issue["identifier"]), "awaiting_merge"),
        process,
        timeout_seconds=90,
        description="awaiting_merge",
    )
    if int(payload["awaiting_merge"]["pr_number"]) != pr.number:
        raise RuntimeError(f"awaiting_merge pr_number mismatch: {payload['awaiting_merge']['pr_number']} != {pr.number}")

    merge_pull_request(repo, pr.number)
    resources.pull_request_numbers.remove(pr.number)
    _wait_for(
        lambda: _await_linear_done(linear, str(issue["id"])),
        process,
        timeout_seconds=120,
        description="issue done after merge",
    )
    _wait_for(
        lambda: _await_issue_gone(base_url, str(issue["identifier"])),
        process,
        timeout_seconds=120,
        description="issue removed from runtime snapshot",
    )
    process.stop()
    resources.processes.remove(process)
    resources.issue_ids.remove(str(issue["id"]))
    return f"{issue['identifier']} -> awaiting_merge -> done"


def _run_runtime_extensions_smoke(
    resources: Resources,
    linear: LinearClient,
    context: TeamContext,
    project_slug: str,
    repo: str,
    repo_url: str,
    branch_scope: str,
    branch_namespace: str,
    echo_output: bool,
) -> str:
    if resources.binary_path is None:
        raise RuntimeError("symphony binary is not built")

    recorder = NotificationRecorder()
    notification_server = _start_notification_server(allocate_port(), recorder)
    process = None

    try:
        port = allocate_port()
        base_dir = resources.temp_dir / "runtime-extensions"
        session_state_path = base_dir / "local" / "session-state.json"
        config = SmokeConfig(
            base_dir=base_dir,
            port=port,
            namespace=f"{branch_namespace}-feature",
            repo_url=repo_url,
            linear_api_key=linear.api_key,
            linear_project_slug=project_slug,
            linear_branch_scope=branch_scope,
            session_state_path=session_state_path,
            notification_port=notification_server.server_port,
        )
        write_smoke_config(
            config,
            prompt_text="Do not modify repository contents. Exit successfully without creating or updating a pull request.",
        )

        base_url = f"http://127.0.0.1:{port}"
        process = start_symphony(resources.binary_path, config.base_dir, echo=echo_output, env=git_env())
        resources.processes.append(process)
        _wait_for(lambda: fetch_json(f"{base_url}/api/v1/state"), process, timeout_seconds=30, description="runtime_extensions startup")

        persistence_issue = linear.create_issue(f"{LIVE_SMOKE_PREFIX} runtime_extensions persistence {int(time.time())}", context)
        resources.issue_ids.append(str(persistence_issue["id"]))
        persistence_identifier = str(persistence_issue["identifier"])
        _wait_for(
            lambda: _await_issue_status(base_url, persistence_identifier, "awaiting_intervention"),
            process,
            timeout_seconds=120,
            description="runtime_extensions awaiting_intervention",
        )
        _wait_for(
            lambda: recorder.find(path="/webhook", identifier=persistence_identifier, event_type="issue_intervention_required") or None,
            process,
            timeout_seconds=30,
            description="runtime_extensions webhook intervention notification",
            interval_seconds=0.5,
        )
        _wait_for(
            lambda: recorder.find(path="/slack", identifier=persistence_identifier, event_type="issue_intervention_required") or None,
            process,
            timeout_seconds=30,
            description="runtime_extensions slack intervention notification",
            interval_seconds=0.5,
        )
        _wait_for(
            lambda: session_state_path if session_state_path.exists() else None,
            process,
            timeout_seconds=10,
            description="runtime_extensions session state file",
            interval_seconds=0.2,
        )
        persisted = _load_session_state(session_state_path)
        awaiting_intervention = _session_state_entries(persisted, "awaiting_intervention")
        if not any(item.get("Identifier") == persistence_identifier for item in awaiting_intervention):
            raise RuntimeError("session-state.json missing awaiting_intervention entry after intervention path")

        notification_count_before_restart = recorder.count()
        process.stop()
        resources.processes.remove(process)
        process = start_symphony(resources.binary_path, config.base_dir, echo=echo_output, env=git_env())
        resources.processes.append(process)
        _wait_for(lambda: fetch_json(f"{base_url}/api/v1/state"), process, timeout_seconds=30, description="runtime_extensions restart")
        _wait_for(
            lambda: _await_issue_status(base_url, persistence_identifier, "awaiting_intervention"),
            process,
            timeout_seconds=60,
            description="runtime_extensions restored awaiting_intervention",
        )
        time.sleep(5)
        notification_count_after_restart = recorder.count()
        if notification_count_after_restart != notification_count_before_restart:
            raise RuntimeError(
                f"unexpected notification replay after restart: before={notification_count_before_restart}, after={notification_count_after_restart}"
            )

        merge_issue = linear.create_issue(f"{LIVE_SMOKE_PREFIX} runtime_extensions merge {int(time.time())}", context)
        resources.issue_ids.append(str(merge_issue["id"]))
        merge_identifier = str(merge_issue["identifier"])
        branch = _linear_branch_name(f"{branch_namespace}-feature", branch_scope, merge_identifier)
        pr = prepare_pull_request(
            repo,
            repo_url,
            branch,
            title=f"test: live smoke {merge_identifier}",
            body="Temporary PR for runtime extensions smoke.",
            work_root=resources.temp_dir / "runtime-extensions-pr",
        )
        resources.pull_request_numbers.append(pr.number)
        _wait_for(
            lambda: _await_issue_status(base_url, merge_identifier, "awaiting_merge"),
            process,
            timeout_seconds=120,
            description="runtime_extensions awaiting_merge",
        )

        process.stop()
        resources.processes.remove(process)
        process = start_symphony(resources.binary_path, config.base_dir, echo=echo_output, env=git_env())
        resources.processes.append(process)
        _wait_for(lambda: fetch_json(f"{base_url}/api/v1/state"), process, timeout_seconds=30, description="runtime_extensions second restart")
        merge_payload = _wait_for(
            lambda: _await_issue_status(base_url, merge_identifier, "awaiting_merge"),
            process,
            timeout_seconds=60,
            description="runtime_extensions restored awaiting_merge",
        )
        if int(merge_payload["awaiting_merge"]["pr_number"]) != pr.number:
            raise RuntimeError(
                f"restored awaiting_merge pr_number mismatch: {merge_payload['awaiting_merge']['pr_number']} != {pr.number}"
            )
        persisted = _load_session_state(session_state_path)
        awaiting_merge = _session_state_entries(persisted, "awaiting_merge")
        if not any(item.get("Identifier") == merge_identifier for item in awaiting_merge):
            raise RuntimeError("session-state.json missing awaiting_merge entry after restart")

        merge_pull_request(repo, pr.number)
        resources.pull_request_numbers.remove(pr.number)
        _wait_for(
            lambda: _await_linear_done(linear, str(merge_issue["id"])),
            process,
            timeout_seconds=180,
            description="runtime_extensions issue done after merge",
        )
        _wait_for(
            lambda: _await_issue_gone(base_url, merge_identifier),
            process,
            timeout_seconds=180,
            description="runtime_extensions issue removed from runtime snapshot",
        )
        _wait_for(
            lambda: recorder.find(path="/webhook", identifier=merge_identifier, event_type="issue_completed") or None,
            process,
            timeout_seconds=60,
            description="runtime_extensions webhook completed notification",
            interval_seconds=0.5,
        )
        _wait_for(
            lambda: recorder.find(path="/slack", identifier=merge_identifier, event_type="issue_completed") or None,
            process,
            timeout_seconds=60,
            description="runtime_extensions slack completed notification",
            interval_seconds=0.5,
        )

        linear.update_issue_state(str(persistence_issue["id"]), context.canceled_state_id)
        resources.issue_ids.remove(str(persistence_issue["id"]))
        resources.issue_ids.remove(str(merge_issue["id"]))

        return (
            f"{persistence_identifier} restored awaiting_intervention, "
            f"{merge_identifier} restored awaiting_merge and completed, "
            f"notifications={recorder.count()} no-replay={notification_count_before_restart == notification_count_after_restart}"
        )
    finally:
        notification_server.shutdown()
        notification_server.server_close()


def _linear_branch_name(namespace: str, branch_scope: str, identifier: str) -> str:
    return f"{namespace}/linear-{_slugify(branch_scope)}-{_slugify(identifier)}"


def _slugify(value: str) -> str:
    lowered = value.strip().lower()
    builder: list[str] = []
    last_dash = False
    for char in lowered:
        if char.isalnum():
            builder.append(char)
            last_dash = False
            continue
        if not last_dash:
            builder.append("-")
            last_dash = True
    return "".join(builder).strip("-") or "issue"


def _wait_for(
    probe: Callable[[], object | None],
    process: object | None,
    *,
    timeout_seconds: float,
    description: str,
    interval_seconds: float = 1.0,
) -> object:
    deadline = time.time() + timeout_seconds
    last_error: Exception | None = None
    while time.time() < deadline:
        if process is not None:
            process.ensure_running()
        try:
            result = probe()
            if result is not None:
                return result
        except Exception as exc:
            last_error = exc
        time.sleep(interval_seconds)
    if last_error is not None:
        raise RuntimeError(f"timeout waiting for {description}: {last_error}")
    raise RuntimeError(f"timeout waiting for {description}")


def _await_issue_status(base_url: str, identifier: str, expected_status: str) -> dict[str, object] | None:
    status_code, payload = fetch_issue_state(base_url, identifier)
    if status_code != 200 or payload is None:
        return None
    if payload.get("status") != expected_status:
        return None
    return payload


def _await_issue_gone(base_url: str, identifier: str) -> dict[str, object] | None:
    status_code, payload = fetch_issue_state(base_url, identifier)
    if status_code == 404:
        return payload or {}
    return None


def _await_linear_done(linear: LinearClient, issue_id: str) -> dict[str, object] | None:
    issue = linear.fetch_issue(issue_id)
    if str(issue["state"]["name"]) == "Done":
        return issue
    return None


def _load_session_state(path: Path) -> dict[str, object]:
    return json.loads(path.read_text(encoding="utf-8"))


def _session_state_entries(payload: dict[str, object], key: str) -> list[dict[str, object]]:
    values = payload.get(key)
    if values is None:
        legacy_key = "".join(part.capitalize() for part in key.split("_"))
        values = payload.get(legacy_key)
    if not isinstance(values, list):
        return []
    return [item for item in values if isinstance(item, dict)]


def _start_notification_server(port: int, recorder: NotificationRecorder) -> ThreadingHTTPServer:
    NotificationHandler.recorder = recorder
    server = ThreadingHTTPServer(("127.0.0.1", port), NotificationHandler)
    thread = Thread(target=server.serve_forever, daemon=True)
    thread.start()
    return server


def _run_step(results: list[StepResult], name: str, func: Callable[[], str]) -> None:
    print(f"==> {name}")
    try:
        detail = func()
        results.append(StepResult(name=name, status="PASS", detail=detail))
        print(f"[PASS] {name}: {detail}")
    except Exception as exc:
        results.append(StepResult(name=name, status="FAIL", detail=str(exc)))
        print(f"[FAIL] {name}: {exc}")
        raise


def _print_summary(results: list[StepResult], temp_dir: Path, keep_artifacts: bool) -> None:
    print("\nSummary:")
    for item in results:
        print(f"- {item.status} {item.name}: {item.detail}")
    if keep_artifacts:
        print(f"artifacts kept at {temp_dir}")
    else:
        print(f"artifacts cleaned from {temp_dir}")
