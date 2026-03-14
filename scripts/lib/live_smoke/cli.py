"""live smoke CLI。"""

from __future__ import annotations

import argparse
from dataclasses import dataclass, field
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import re
import shlex
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
    fetch_json,
    open_events_stream,
    post_json,
    read_sse_event,
    start_symphony,
    symphony_binary_name,
    symphony_doctor_command,
    symphony_run_command,
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
                subject = body.get("subject")
                if not isinstance(subject, dict):
                    continue
                if subject.get("identifier") != identifier or body.get("type") != event_type:
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
        default="",
        help="Explicit workspace branch namespace used in generated smoke config; defaults to a run-scoped namespace",
    )
    parser.add_argument(
        "--echo-process-output",
        action="store_true",
        help="Echo symphony process output in real time",
    )
    parser.add_argument(
        "--codex-command",
        default=os.getenv("SYMPHONY_REAL_CODEX_COMMAND", "codex app-server"),
        help="Codex app-server command used by live smoke",
    )
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    repo = args.repo
    repo_url = f"https://github.com/{repo}"
    linear_api_key = args.linear_api_key or _env_required("LINEAR_API_KEY")
    linear_project_slug = args.linear_project_slug or _env_required("LINEAR_PROJECT_SLUG")
    branch_namespace = _resolve_branch_namespace(args.branch_namespace)
    issue_prefix = _smoke_issue_prefix(branch_namespace)
    codex_command = str(args.codex_command).strip() or "codex app-server"

    temp_dir = temp_root() / time.strftime("%Y%m%d-%H%M%S")
    temp_dir.mkdir(parents=True, exist_ok=True)
    resources = Resources(temp_dir=temp_dir)
    results: list[StepResult] = []

    linear = LinearClient(linear_api_key)
    context = linear.load_team_context(args.team_key, linear_project_slug)

    try:
        _run_step(results, "preflight", lambda: _preflight(repo, codex_command))
        _run_step(results, "build_binary", lambda: _build_binary(resources))
        _run_step(
            results,
            "cleanup_active_smoke_issues",
            lambda: _cleanup_stale_smoke_issues(linear, context, linear_project_slug, issue_prefix),
        )
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
                    branch_namespace,
                    issue_prefix,
                    codex_command,
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
                    branch_namespace,
                    issue_prefix,
                    codex_command,
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
                    branch_namespace,
                    issue_prefix,
                    codex_command,
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


def _slug_component(value: str, *, fallback: str) -> str:
    normalized = re.sub(r"[^a-z0-9]+", "-", value.strip().lower()).strip("-")
    if not normalized:
        return fallback
    return normalized[:48].rstrip("-") or fallback


def _resolve_branch_namespace(explicit: str) -> str:
    override = _slug_component(explicit, fallback="") if explicit.strip() else ""
    if override:
        return override
    run_id = os.getenv("GITHUB_RUN_ID", "").strip()
    run_attempt = _slug_component(os.getenv("GITHUB_RUN_ATTEMPT", "").strip(), fallback="1")
    if run_id:
        event_name = _slug_component(os.getenv("GITHUB_EVENT_NAME", "").strip(), fallback="ci")
        return f"live-smoke-{event_name}-{run_id}-{run_attempt}"
    local_stamp = time.strftime("%Y%m%d-%H%M%S")
    return f"live-smoke-local-{local_stamp}-{os.getpid()}"


def _smoke_issue_prefix(namespace: str) -> str:
    return f"[live-smoke:{namespace}]"


def _is_any_live_smoke_title(title: str) -> bool:
    return title.startswith(LIVE_SMOKE_PREFIX) or title.startswith("[live-smoke:")


def _preflight(repo: str, codex_command: str) -> str:
    require_command("go")
    require_command("git")
    require_command("gh")
    require_command("py")
    require_command(_command_head(codex_command))
    ensure_gh_auth()
    run(["gh", "auth", "setup-git"], cwd=repo_root())
    run(["gh", "repo", "view", repo, "--json", "nameWithOwner"], cwd=repo_root())
    return f"repo={repo} codex={_command_head(codex_command)}"


def _command_head(command: str) -> str:
    parts = shlex.split(command, posix=os.name != "nt")
    if not parts:
        raise RuntimeError(f"invalid codex command: {command!r}")
    return parts[0]


def _build_binary(resources: Resources) -> str:
    binary_dir = resources.temp_dir / "bin"
    binary_dir.mkdir(parents=True, exist_ok=True)
    binary_path = binary_dir / symphony_binary_name()
    run(["go", "build", "-o", str(binary_path), "./cmd/symphony"], cwd=repo_root())
    resources.binary_path = binary_path
    return str(binary_path)


def _cleanup_stale_smoke_issues(linear: LinearClient, context: TeamContext, project_slug: str, issue_prefix: str) -> str:
    stale = []
    foreign = []
    for issue in linear.fetch_active_issues(project_slug):
        title = str(issue.get("title", ""))
        if title.startswith(issue_prefix):
            stale.append(issue)
            continue
        foreign.append(issue)
    for issue in stale:
        linear.update_issue_state(str(issue["id"]), context.canceled_state_id)
    if foreign:
        identifiers = ", ".join(str(item["identifier"]) for item in foreign)
        raise RuntimeError(
            f"target project has active issues outside current namespace {issue_prefix}: {identifiers}"
        )
    return f"cleaned={len(stale)} namespace={issue_prefix}"


def _purge_terminal_smoke_issues(linear: LinearClient, *, project_slug: str) -> str:
    purged = 0
    for issue in linear.fetch_smoke_issues(project_slug, "[live-smoke"):
        state_name = str(issue.get("state", {}).get("name", "")).strip()
        title = str(issue.get("title", ""))
        if not _is_any_live_smoke_title(title):
            continue
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
        symphony_doctor_command(resources.binary_path, "--config-dir", str(base_dir)),
        cwd=repo_root(),
        check=False,
    )
    combined = doctor.stdout + doctor.stderr
    if doctor.returncode == 0 or "CODEX_COMMAND (runtime.codex.command)" not in combined:
        raise RuntimeError("doctor did not report missing runtime.codex.command")
    return "doctor ok"


def _run_inline_hook_dry_run(resources: Resources, linear_api_key: str, project_slug: str, branch_scope: str) -> str:
    if resources.binary_path is None:
        raise RuntimeError("symphony binary is not built")
    base_dir = resources.temp_dir / "inline-hook"
    write_inline_hook_config(base_dir, linear_api_key=linear_api_key, linear_project_slug=project_slug, linear_branch_scope=branch_scope)
    result = run(
        symphony_run_command(resources.binary_path, "--dry-run", "--config-dir", str(base_dir)),
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
        symphony_run_command(resources.binary_path, "--dry-run", "--config-dir", str(base_dir)),
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
    issue_prefix: str,
    codex_command: str,
    echo_output: bool,
) -> str:
    if resources.binary_path is None:
        raise RuntimeError("symphony binary is not built")
    issue = linear.create_issue(f"{issue_prefix} missing_pr {int(time.time())}", context)
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
        codex_command=codex_command,
    )
    write_smoke_config(
        config,
        prompt_text="Do not modify repository contents. Exit successfully without creating or updating a pull request.",
    )
    process = start_symphony(resources.binary_path, config.base_dir, echo=echo_output, env=git_env())
    resources.processes.append(process)
    base_url = f"http://127.0.0.1:{port}"
    discovery_payload, state_payload = _wait_for(
        lambda: _await_formal_startup(base_url),
        process,
        timeout_seconds=30,
        description="symphony startup",
    )
    if discovery_payload["service_mode"] != "serving" or state_payload["service_mode"] != "serving":
        raise RuntimeError(f"startup service_mode mismatch: discovery={discovery_payload} state={state_payload}")

    events = open_events_stream(f"{base_url}/api/v1/events")
    try:
        snapshot_event = _await_sse_event(
            events,
            process,
            expected_event="snapshot",
            timeout_seconds=15,
            description="missing_pr snapshot event",
        )
        if snapshot_event["service_mode"] != "serving":
            raise RuntimeError(f"snapshot service_mode mismatch: {snapshot_event}")

        control_payload = _assert_refresh_contract(base_url)
        if control_payload["status"] != "accepted":
            raise RuntimeError(f"refresh control status mismatch: {control_payload}")

        updated_state = _wait_for(
            lambda: _await_state_after_sse(
                base_url,
                events,
                expected_service_mode="serving",
                predicate=lambda payload: (
                    _find_runtime_record(payload, str(issue["identifier"]), status="awaiting_intervention") is not None
                ),
            ),
            process,
            timeout_seconds=120,
            description="missing_pr SSE -> state",
        )
        record = _require_runtime_record(updated_state, str(issue["identifier"]), status="awaiting_intervention")
        reason = record.get("reason")
        if not isinstance(reason, dict) or reason.get("reason_code") != "record.blocked.awaiting_intervention":
            raise RuntimeError(f"unexpected intervention reason: {record}")
    finally:
        events.close()
        process.stop()
        resources.processes.remove(process)
        linear.update_issue_state(str(issue["id"]), context.canceled_state_id)
        resources.issue_ids.remove(str(issue["id"]))
    return f"{issue['identifier']} -> awaiting_intervention via SSE/state; refresh=accepted"


def _run_merge_path_smoke(
    resources: Resources,
    linear: LinearClient,
    context: TeamContext,
    project_slug: str,
    repo: str,
    repo_url: str,
    branch_scope: str,
    branch_namespace: str,
    issue_prefix: str,
    codex_command: str,
    echo_output: bool,
) -> str:
    if resources.binary_path is None:
        raise RuntimeError("symphony binary is not built")
    issue = linear.create_issue(f"{issue_prefix} merge_path {int(time.time())}", context)
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
        codex_command=codex_command,
    )
    write_smoke_config(
        config,
        prompt_text="Do not modify repository contents. Exit successfully without creating or updating a pull request.",
    )
    process = start_symphony(resources.binary_path, config.base_dir, echo=echo_output, env=git_env())
    resources.processes.append(process)
    base_url = f"http://127.0.0.1:{port}"
    _wait_for(
        lambda: _await_formal_startup(base_url),
        process,
        timeout_seconds=30,
        description="symphony startup",
    )

    events = open_events_stream(f"{base_url}/api/v1/events")
    try:
        _await_sse_event(
            events,
            process,
            expected_event="snapshot",
            timeout_seconds=15,
            description="awaiting_merge snapshot event",
        )
        control_payload = _assert_refresh_contract(base_url)
        if control_payload["status"] != "accepted":
            raise RuntimeError(f"awaiting_merge refresh status mismatch: {control_payload}")
        waiting_state = _wait_for(
            lambda: _await_state_after_sse(
                base_url,
                events,
                expected_service_mode="serving",
                predicate=lambda payload: (
                    record := _find_runtime_record(payload, str(issue["identifier"]))
                )
                is not None
                and record.get("status") == "awaiting_merge",
            ),
            process,
            timeout_seconds=120,
            description="awaiting_merge SSE -> state",
        )
        record = _require_runtime_record(waiting_state, str(issue["identifier"]), status="awaiting_merge")
        pr_ref = _require_durable_ref(record, "pull_request")
        if int(pr_ref["number"]) != pr.number:
            raise RuntimeError(f"awaiting_merge pr_number mismatch: {pr_ref['number']} != {pr.number}")

        merge_pull_request(repo, pr.number)
        resources.pull_request_numbers.remove(pr.number)
        control_payload = _assert_refresh_contract(base_url)
        if control_payload["status"] != "accepted":
            raise RuntimeError(f"merged_pr_source_not_terminal refresh status mismatch: {control_payload}")
        intervention_state = _wait_for(
            lambda: _await_state_after_sse(
                base_url,
                events,
                expected_service_mode="serving",
                predicate=lambda payload: (
                    _find_runtime_record(payload, str(issue["identifier"]), status="awaiting_intervention") is not None
                ),
            ),
            process,
            timeout_seconds=120,
            description="merged_pr_source_not_terminal SSE -> state",
        )
        record = _require_runtime_record(intervention_state, str(issue["identifier"]), status="awaiting_intervention")
        reason = _require_reason(record, "record.blocked.awaiting_intervention")
        if reason.get("details", {}).get("cause") != "merged_pr_source_not_terminal":
            raise RuntimeError(f"unexpected merged intervention reason: {reason}")

        linear.update_issue_state(str(issue["id"]), context.done_state_id)
        control_payload = _assert_refresh_contract(base_url)
        if control_payload["status"] != "accepted":
            raise RuntimeError(f"completed refresh status mismatch: {control_payload}")
        _wait_for(
            lambda: _await_linear_done(linear, str(issue["id"])),
            process,
            timeout_seconds=120,
            description="issue done after external source close",
        )
        completed_state = _wait_for(
            lambda: _await_state_after_sse(
                base_url,
                events,
                expected_service_mode="serving",
                predicate=lambda payload: (
                    _find_runtime_record(payload, str(issue["identifier"])) is None
                    and _find_completed_record(payload, str(issue["identifier"])) is not None
                ),
            ),
            process,
            timeout_seconds=180,
            description="completed_window SSE -> state",
        )
        completed = _require_completed_record(completed_state, str(issue["identifier"]), outcome="succeeded")
        if completed["status"] != "completed":
            raise RuntimeError(f"completed_window status mismatch: {completed}")
    finally:
        events.close()
        process.stop()
        resources.processes.remove(process)
        resources.issue_ids.remove(str(issue["id"]))
    return f"{issue['identifier']} -> awaiting_intervention(merged_pr_source_not_terminal) -> completed_window.succeeded"


def _run_runtime_extensions_smoke(
    resources: Resources,
    linear: LinearClient,
    context: TeamContext,
    project_slug: str,
    repo: str,
    repo_url: str,
    branch_scope: str,
    branch_namespace: str,
    issue_prefix: str,
    codex_command: str,
    echo_output: bool,
) -> str:
    if resources.binary_path is None:
        raise RuntimeError("symphony binary is not built")
    recovery = _run_recovery_ledger_smoke(
        resources,
        linear,
        context,
        project_slug,
        repo,
        repo_url,
        branch_scope,
        branch_namespace,
        issue_prefix,
        codex_command,
        echo_output,
    )
    degraded = _run_notification_degraded_smoke(
        resources,
        linear,
        context,
        project_slug,
        repo_url,
        branch_scope,
        branch_namespace,
        issue_prefix,
        codex_command,
        echo_output,
    )
    unavailable = _run_unavailable_ledger_smoke(
        resources,
        linear,
        context,
        project_slug,
        repo_url,
        branch_scope,
        branch_namespace,
        issue_prefix,
        codex_command,
        echo_output,
    )
    return f"{recovery}; {degraded}; {unavailable}"


def _run_recovery_ledger_smoke(
    resources: Resources,
    linear: LinearClient,
    context: TeamContext,
    project_slug: str,
    repo: str,
    repo_url: str,
    branch_scope: str,
    branch_namespace: str,
    issue_prefix: str,
    codex_command: str,
    echo_output: bool,
) -> str:
    recorder = NotificationRecorder()
    notification_server = _start_notification_server(allocate_port(), recorder)
    process = None
    events = None

    try:
        port = allocate_port()
        base_dir = resources.temp_dir / "runtime-ledger"
        ledger_path = base_dir / "local" / "runtime-ledger.json"
        namespace = f"{branch_namespace}-feature"
        config = SmokeConfig(
            base_dir=base_dir,
            port=port,
            namespace=namespace,
            repo_url=repo_url,
            linear_api_key=linear.api_key,
            linear_project_slug=project_slug,
            linear_branch_scope=branch_scope,
            codex_command=codex_command,
            ledger_path=ledger_path,
            notification_port=notification_server.server_port,
        )
        write_smoke_config(
            config,
            prompt_text="Do not modify repository contents. Exit successfully without creating or updating a pull request.",
        )

        base_url = f"http://127.0.0.1:{port}"
        process = start_symphony(resources.binary_path, config.base_dir, echo=echo_output, env=git_env())
        resources.processes.append(process)
        _wait_for(
            lambda: _await_formal_startup(base_url),
            process,
            timeout_seconds=30,
            description="runtime_ledger startup",
        )
        _assert_discovery_source(fetch_json(f"{base_url}/api/v1/discovery"), kind="linear", name="linear-main")
        events = open_events_stream(f"{base_url}/api/v1/events")
        _await_sse_event(events, process, expected_event="snapshot", timeout_seconds=15, description="runtime_ledger snapshot")

        persistence_issue = linear.create_issue(f"{issue_prefix} runtime_ledger persistence {int(time.time())}", context)
        resources.issue_ids.append(str(persistence_issue["id"]))
        persistence_identifier = str(persistence_issue["identifier"])

        persistence_state = _wait_for(
            lambda: _await_state_after_sse(
                base_url,
                events,
                expected_service_mode="serving",
                predicate=lambda payload: (
                    _find_runtime_record(payload, persistence_identifier, status="awaiting_intervention") is not None
                ),
            ),
            process,
            timeout_seconds=120,
            description="runtime_ledger awaiting_intervention via SSE",
        )
        record = _require_runtime_record(persistence_state, persistence_identifier, status="awaiting_intervention")
        _assert_source_anchor(
            record,
            "runtime_ledger.record",
            source_kind="linear",
            source_name="linear-main",
            source_id=str(persistence_issue["id"]),
            source_identifier=persistence_identifier,
        )
        reason = _require_reason(record, "record.blocked.awaiting_intervention")
        if reason["category"] != "record":
            raise RuntimeError(f"awaiting_intervention reason category mismatch: {reason}")

        webhook_events = _wait_for(
            lambda: recorder.find(path="/webhook", identifier=persistence_identifier, event_type="issue_intervention_required") or None,
            process,
            timeout_seconds=30,
            description="runtime_ledger webhook intervention notification",
            interval_seconds=0.5,
        )
        _assert_notification_details(
            webhook_events[0],
            **{
                "dispatch.continuation_reason": "missing_pr",
                "dispatch.expected_outcome": "pull_request",
            },
        )
        _wait_for(
            lambda: recorder.find(path="/slack", identifier=persistence_identifier, event_type="issue_intervention_required") or None,
            process,
            timeout_seconds=30,
            description="runtime_ledger slack intervention notification",
            interval_seconds=0.5,
        )
        ledger_payload = _wait_for(
            lambda: _await_ledger_record(ledger_path, persistence_identifier, status="awaiting_intervention"),
            process,
            timeout_seconds=15,
            description="runtime_ledger awaiting_intervention persisted",
            interval_seconds=0.2,
        )
        _assert_ledger_identity(
            ledger_payload,
            active_source="linear-main",
            flow_name="implement",
            tracker_project_slug=project_slug,
            workspace_root=str((resources.temp_dir.parent / f"workspaces-{namespace}").resolve()).replace("\\", "/"),
            ledger_path=str(ledger_path.resolve()).replace("\\", "/"),
        )
        ledger_record = _find_ledger_record(ledger_payload, persistence_identifier, status="awaiting_intervention")
        if ledger_record is None:
            raise RuntimeError(f"runtime_ledger persisted record missing: {ledger_payload}")
        _assert_source_anchor(
            ledger_record,
            "runtime_ledger.ledger_record",
            source_kind="linear",
            source_name="linear-main",
            source_id=str(persistence_issue["id"]),
            source_identifier=persistence_identifier,
        )
        notification_count_before_restart = recorder.count()

        events.close()
        events = None
        process.stop()
        resources.processes.remove(process)
        process = start_symphony(resources.binary_path, config.base_dir, echo=echo_output, env=git_env())
        resources.processes.append(process)
        _wait_for(
            lambda: _await_formal_startup(base_url),
            process,
            timeout_seconds=30,
            description="runtime_ledger restart",
        )
        events = open_events_stream(f"{base_url}/api/v1/events")
        _await_sse_event(events, process, expected_event="snapshot", timeout_seconds=15, description="runtime_ledger restart snapshot")
        restored_state = _wait_for(
            lambda: _await_runtime_record(base_url, persistence_identifier, status="awaiting_intervention"),
            process,
            timeout_seconds=60,
            description="runtime_ledger restored awaiting_intervention",
        )
        restored_record = _require_runtime_record(restored_state, persistence_identifier, status="awaiting_intervention")
        _assert_source_anchor(
            restored_record,
            "runtime_ledger.restored_record",
            source_kind="linear",
            source_name="linear-main",
            source_id=str(persistence_issue["id"]),
            source_identifier=persistence_identifier,
        )
        time.sleep(5)
        if recorder.count() != notification_count_before_restart:
            raise RuntimeError(
                f"unexpected notification replay after restart: before={notification_count_before_restart}, after={recorder.count()}"
            )

        merge_issue = linear.create_issue(f"{issue_prefix} runtime_ledger merge {int(time.time())}", context)
        resources.issue_ids.append(str(merge_issue["id"]))
        merge_identifier = str(merge_issue["identifier"])
        branch = _linear_branch_name(namespace, branch_scope, merge_identifier)
        pr = prepare_pull_request(
            repo,
            repo_url,
            branch,
            title=f"test: live smoke {merge_identifier}",
            body="Temporary PR for runtime ledger smoke.",
            work_root=resources.temp_dir / "runtime-ledger-pr",
        )
        resources.pull_request_numbers.append(pr.number)

        merge_state = _wait_for(
            lambda: _await_state_after_sse(
                base_url,
                events,
                expected_service_mode="serving",
                predicate=lambda payload: (
                    _find_runtime_record(payload, merge_identifier, status="awaiting_merge") is not None
                ),
            ),
            process,
            timeout_seconds=120,
            description="runtime_ledger awaiting_merge via SSE",
        )
        merge_record = _require_runtime_record(merge_state, merge_identifier, status="awaiting_merge")
        pr_ref = _require_durable_ref(merge_record, "pull_request")
        if int(pr_ref["number"]) != pr.number:
            raise RuntimeError(f"runtime_ledger awaiting_merge pr_number mismatch: {pr_ref['number']} != {pr.number}")
        _wait_for(
            lambda: _await_ledger_record(ledger_path, merge_identifier, status="awaiting_merge"),
            process,
            timeout_seconds=15,
            description="runtime_ledger awaiting_merge persisted",
            interval_seconds=0.2,
        )

        events.close()
        events = None
        process.stop()
        resources.processes.remove(process)
        process = start_symphony(resources.binary_path, config.base_dir, echo=echo_output, env=git_env())
        resources.processes.append(process)
        _wait_for(
            lambda: _await_formal_startup(base_url),
            process,
            timeout_seconds=30,
            description="runtime_ledger second restart",
        )
        events = open_events_stream(f"{base_url}/api/v1/events")
        _await_sse_event(events, process, expected_event="snapshot", timeout_seconds=15, description="runtime_ledger second snapshot")
        restored_merge_state = _wait_for(
            lambda: _await_runtime_record(base_url, merge_identifier, status="awaiting_merge"),
            process,
            timeout_seconds=60,
            description="runtime_ledger restored awaiting_merge",
        )
        restored_merge = _require_runtime_record(restored_merge_state, merge_identifier, status="awaiting_merge")
        restored_pr_ref = _require_durable_ref(restored_merge, "pull_request")
        if int(restored_pr_ref["number"]) != pr.number:
            raise RuntimeError(f"runtime_ledger restored pr_number mismatch: {restored_pr_ref['number']} != {pr.number}")

        merge_pull_request(repo, pr.number)
        resources.pull_request_numbers.remove(pr.number)
        intervention_state = _wait_for(
            lambda: _await_state_after_sse(
                base_url,
                events,
                expected_service_mode="serving",
                predicate=lambda payload: (
                    _find_runtime_record(payload, merge_identifier, status="awaiting_intervention") is not None
                ),
            ),
            process,
            timeout_seconds=180,
            description="runtime_ledger merged_pr_source_not_terminal via SSE",
        )
        intervention_record = _require_runtime_record(intervention_state, merge_identifier, status="awaiting_intervention")
        intervention_reason = _require_reason(intervention_record, "record.blocked.awaiting_intervention")
        if intervention_reason.get("details", {}).get("cause") != "merged_pr_source_not_terminal":
            raise RuntimeError(f"unexpected runtime_ledger merge intervention reason: {intervention_reason}")
        _wait_for(
            lambda: _await_ledger_record(ledger_path, merge_identifier, status="awaiting_intervention"),
            process,
            timeout_seconds=15,
            description="runtime_ledger awaiting_intervention persisted after merge",
            interval_seconds=0.2,
        )
        _wait_for(
            lambda: recorder.find(path="/webhook", identifier=merge_identifier, event_type="issue_intervention_required") or None,
            process,
            timeout_seconds=60,
            description="runtime_ledger webhook intervention notification after merge",
            interval_seconds=0.5,
        )
        _wait_for(
            lambda: recorder.find(path="/slack", identifier=merge_identifier, event_type="issue_intervention_required") or None,
            process,
            timeout_seconds=60,
            description="runtime_ledger slack intervention notification after merge",
            interval_seconds=0.5,
        )

        linear.update_issue_state(str(merge_issue["id"]), context.done_state_id)
        _wait_for(
            lambda: _await_linear_done(linear, str(merge_issue["id"])),
            process,
            timeout_seconds=180,
            description="runtime_ledger issue done after external source close",
        )
        completed_state = _wait_for(
            lambda: _await_state_after_sse(
                base_url,
                events,
                expected_service_mode="serving",
                predicate=lambda payload: (
                    _find_runtime_record(payload, merge_identifier) is None
                    and _find_completed_record(payload, merge_identifier) is not None
                ),
            ),
            process,
            timeout_seconds=180,
            description="runtime_ledger completed_window via SSE",
        )
        _require_completed_record(completed_state, merge_identifier, outcome="succeeded")
        _wait_for(
            lambda: _await_ledger_record(ledger_path, merge_identifier, status="completed", outcome="succeeded"),
            process,
            timeout_seconds=15,
            description="runtime_ledger completed persisted",
            interval_seconds=0.2,
        )
        _wait_for(
            lambda: recorder.find(path="/webhook", identifier=merge_identifier, event_type="issue_completed") or None,
            process,
            timeout_seconds=60,
            description="runtime_ledger webhook completed notification",
            interval_seconds=0.5,
        )
        _wait_for(
            lambda: recorder.find(path="/slack", identifier=merge_identifier, event_type="issue_completed") or None,
            process,
            timeout_seconds=60,
            description="runtime_ledger slack completed notification",
            interval_seconds=0.5,
        )

        linear.update_issue_state(str(persistence_issue["id"]), context.canceled_state_id)
        resources.issue_ids.remove(str(persistence_issue["id"]))
        resources.issue_ids.remove(str(merge_issue["id"]))
        return f"{persistence_identifier}/awaiting_intervention + {merge_identifier}/awaiting_intervention_after_merge -> completed recovered from ledger"
    finally:
        if events is not None:
            events.close()
        if process is not None and process in resources.processes:
            process.stop()
            resources.processes.remove(process)
        notification_server.shutdown()
        notification_server.server_close()


def _run_notification_degraded_smoke(
    resources: Resources,
    linear: LinearClient,
    context: TeamContext,
    project_slug: str,
    repo_url: str,
    branch_scope: str,
    branch_namespace: str,
    issue_prefix: str,
    codex_command: str,
    echo_output: bool,
) -> str:
    recorder = NotificationRecorder()
    notification_server = _start_notification_server(allocate_port(), recorder)
    process = None
    events = None

    try:
        port = allocate_port()
        broken_port = allocate_port()
        config = SmokeConfig(
            base_dir=resources.temp_dir / "notification-degraded",
            port=port,
            namespace=f"{branch_namespace}-degraded",
            repo_url=repo_url,
            linear_api_key=linear.api_key,
            linear_project_slug=project_slug,
            linear_branch_scope=branch_scope,
            codex_command=codex_command,
            notification_port=notification_server.server_port,
            broken_notification_port=broken_port,
            broken_notification_channels=("local-slack",),
        )
        write_smoke_config(
            config,
            prompt_text="Do not modify repository contents. Exit successfully without creating or updating a pull request.",
        )

        base_url = f"http://127.0.0.1:{port}"
        process = start_symphony(resources.binary_path, config.base_dir, echo=echo_output, env=git_env())
        resources.processes.append(process)
        _wait_for(
            lambda: _await_formal_startup(base_url),
            process,
            timeout_seconds=30,
            description="notification_degraded startup",
        )
        events = open_events_stream(f"{base_url}/api/v1/events")
        _await_sse_event(events, process, expected_event="snapshot", timeout_seconds=15, description="notification_degraded snapshot")

        issue = linear.create_issue(f"{issue_prefix} notification_degraded {int(time.time())}", context)
        resources.issue_ids.append(str(issue["id"]))
        identifier = str(issue["identifier"])
        control_payload = _assert_refresh_contract(base_url)
        if control_payload["status"] != "accepted":
            raise RuntimeError(f"notification_degraded refresh status mismatch: {control_payload}")
        _wait_for(
            lambda: recorder.find(path="/webhook", identifier=identifier, event_type="issue_intervention_required") or None,
            process,
            timeout_seconds=90,
            description="notification_degraded webhook delivery",
            interval_seconds=0.5,
        )
        degraded_state = _wait_for(
            lambda: _await_state_after_sse(
                base_url,
                events,
                expected_service_mode="degraded",
                predicate=lambda payload: (
                    _find_runtime_record(payload, identifier, status="awaiting_intervention") is not None
                    and _has_reason_code(payload.get("reasons"), "service.degraded.notification_delivery")
                ),
            ),
            process,
            timeout_seconds=120,
            description="notification_degraded SSE -> state",
        )
        discovery_payload = fetch_json(f"{base_url}/api/v1/discovery")
        _assert_discovery_surface(discovery_payload)
        _assert_service_surface_consistency(discovery_payload, degraded_state)
        if discovery_payload["service_mode"] != "degraded":
            raise RuntimeError(f"notification_degraded discovery mode mismatch: {discovery_payload}")
        service_reason = _require_reason_from_list(degraded_state.get("reasons"), "service.degraded.notification_delivery")
        channel_ids = service_reason["details"].get("channel_ids")
        if channel_ids != ["local-slack"]:
            raise RuntimeError(f"notification_degraded channel_ids mismatch: {service_reason}")
        return f"{identifier} kept serving issue flow while service_mode=degraded(channel_ids={channel_ids})"
    finally:
        if events is not None:
            events.close()
        if process is not None and process in resources.processes:
            process.stop()
            resources.processes.remove(process)
        notification_server.shutdown()
        notification_server.server_close()


def _run_unavailable_ledger_smoke(
    resources: Resources,
    linear: LinearClient,
    context: TeamContext,
    project_slug: str,
    repo_url: str,
    branch_scope: str,
    branch_namespace: str,
    issue_prefix: str,
    codex_command: str,
    echo_output: bool,
) -> str:
    process = None
    events = None

    try:
        port = allocate_port()
        base_dir = resources.temp_dir / "ledger-unavailable"
        ledger_path = base_dir / "local" / "runtime-ledger.json"
        config = SmokeConfig(
            base_dir=base_dir,
            port=port,
            namespace=f"{branch_namespace}-unavailable",
            repo_url=repo_url,
            linear_api_key=linear.api_key,
            linear_project_slug=project_slug,
            linear_branch_scope=branch_scope,
            codex_command=codex_command,
            ledger_path=ledger_path,
        )
        write_smoke_config(
            config,
            prompt_text="Do not modify repository contents. Exit successfully without creating or updating a pull request.",
        )

        base_url = f"http://127.0.0.1:{port}"
        process = start_symphony(resources.binary_path, config.base_dir, echo=echo_output, env=git_env())
        resources.processes.append(process)
        _wait_for(
            lambda: _await_formal_startup(base_url),
            process,
            timeout_seconds=30,
            description="ledger_unavailable startup",
        )
        events = open_events_stream(f"{base_url}/api/v1/events")
        _await_sse_event(events, process, expected_event="snapshot", timeout_seconds=15, description="ledger_unavailable snapshot")
        _wait_for(
            lambda: _load_ledger(ledger_path) if ledger_path.exists() else None,
            process,
            timeout_seconds=10,
            description="ledger_unavailable ledger exists",
            interval_seconds=0.2,
        )

        if ledger_path.exists():
            ledger_path.unlink()
        ledger_path.mkdir(parents=True, exist_ok=True)

        issue = linear.create_issue(f"{issue_prefix} ledger_unavailable {int(time.time())}", context)
        resources.issue_ids.append(str(issue["id"]))

        unavailable_state = _wait_for(
            lambda: _await_service_mode(base_url, expected_mode="unavailable", reason_code="service.unavailable.core_dependency"),
            process,
            timeout_seconds=120,
            description="ledger_unavailable state",
            interval_seconds=0.5,
        )
        discovery_payload = fetch_json(f"{base_url}/api/v1/discovery")
        _assert_discovery_surface(discovery_payload)
        _assert_service_surface_consistency(discovery_payload, unavailable_state)
        if discovery_payload["service_mode"] != "unavailable":
            raise RuntimeError(f"ledger_unavailable discovery mode mismatch: {discovery_payload}")
        rejected_control = _post_refresh(base_url)
        _assert_control_result(rejected_control, expected_status="rejected")
        _require_reason_from_list(unavailable_state.get("reasons"), "service.unavailable.core_dependency")
        return f"{issue['identifier']} drove service_mode=unavailable via ledger write failure"
    finally:
        if events is not None:
            events.close()
        if process is not None and process in resources.processes:
            process.stop()
            resources.processes.remove(process)


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


def _await_linear_done(linear: LinearClient, issue_id: str) -> dict[str, object] | None:
    issue = linear.fetch_issue(issue_id)
    if str(issue["state"]["name"]) == "Done":
        return issue
    return None


def _await_formal_startup(base_url: str) -> tuple[dict[str, object], dict[str, object]] | None:
    discovery_payload = fetch_json(f"{base_url}/api/v1/discovery")
    state_payload = fetch_json(f"{base_url}/api/v1/state")
    _assert_discovery_surface(discovery_payload)
    _assert_state_surface(state_payload)
    _assert_service_surface_consistency(discovery_payload, state_payload)
    return discovery_payload, state_payload


def _await_sse_event(
    events,
    process: object | None,
    *,
    expected_event: str,
    timeout_seconds: float,
    description: str,
) -> dict[str, object]:
    return _wait_for(
        lambda: _read_sse_event(events, expected_event),
        process,
        timeout_seconds=timeout_seconds,
        description=description,
        interval_seconds=0.0,
    )


def _read_sse_event(events, expected_event: str) -> dict[str, object] | None:
    event = read_sse_event(events)
    if event.event != expected_event:
        return None
    try:
        payload = json.loads(event.data)
    except json.JSONDecodeError as exc:
        raise RuntimeError(f"SSE event payload is not valid json: {event.data}") from exc
    if not isinstance(payload, dict):
        raise RuntimeError(f"SSE event payload is not object: {payload!r}")
    _assert_event_envelope(payload, expected_event)
    return payload


def _await_state_after_sse(
    base_url: str,
    events,
    *,
    expected_service_mode: str,
    predicate: Callable[[dict[str, object]], bool],
) -> dict[str, object] | None:
    current = fetch_json(f"{base_url}/api/v1/state")
    _assert_state_surface(current)
    if current.get("service_mode") == expected_service_mode and predicate(current):
        return current
    if _read_sse_event(events, "state_changed") is None:
        return None
    payload = fetch_json(f"{base_url}/api/v1/state")
    _assert_state_surface(payload)
    if payload.get("service_mode") != expected_service_mode:
        return None
    if not predicate(payload):
        return None
    return payload


def _await_runtime_record(base_url: str, identifier: str, *, status: str) -> dict[str, object] | None:
    payload = fetch_json(f"{base_url}/api/v1/state")
    _assert_state_surface(payload)
    if _find_runtime_record(payload, identifier, status=status) is None:
        return None
    return payload


def _await_service_mode(base_url: str, *, expected_mode: str, reason_code: str) -> dict[str, object] | None:
    payload = fetch_json(f"{base_url}/api/v1/state")
    _assert_state_surface(payload)
    if payload.get("service_mode") != expected_mode:
        return None
    if not _has_reason_code(payload.get("reasons"), reason_code):
        return None
    return payload


def _post_refresh(base_url: str) -> dict[str, object]:
    last_error: Exception | None = None
    for attempt in range(3):
        try:
            return post_json(f"{base_url}/api/v1/control/refresh")
        except OSError as exc:
            last_error = exc
            if attempt == 2:
                break
            time.sleep(0.5)
    if last_error is not None:
        raise last_error
    raise RuntimeError("refresh request failed without error detail")


def _load_ledger(path: Path) -> dict[str, object]:
    if path.is_dir():
        raise RuntimeError(f"ledger path is directory, not file: {path}")
    return json.loads(path.read_text(encoding="utf-8"))


def _await_ledger_record(
    path: Path,
    identifier: str,
    *,
    status: str,
    outcome: str | None = None,
) -> dict[str, object] | None:
    if not path.exists() or path.is_dir():
        return None
    payload = _load_ledger(path)
    _assert_ledger_surface(payload)
    if _find_ledger_record(payload, identifier, status=status, outcome=outcome) is None:
        return None
    return payload


def _assert_discovery_surface(payload: dict[str, object]) -> None:
    _require_keys(payload, ["api_version", "instance", "source", "service_mode", "recovery_in_progress", "capabilities", "reasons", "limits"], "discovery")
    if payload["api_version"] != "v1":
        raise RuntimeError(f"discovery api_version mismatch: {payload}")
    if payload["service_mode"] not in {"serving", "degraded", "unavailable"}:
        raise RuntimeError(f"discovery service_mode mismatch: {payload}")
    _require_keys(payload["instance"], ["id", "name", "version"], "discovery.instance")
    _require_keys(payload["source"], ["kind", "name"], "discovery.source")
    capabilities = _require_mapping(payload["capabilities"], "discovery.capabilities")
    _require_keys(capabilities, ["event_protocol", "control_actions", "notifications", "sources"], "discovery.capabilities")
    if capabilities["event_protocol"] != "sse":
        raise RuntimeError(f"discovery event_protocol mismatch: {capabilities}")
    limits = _require_mapping(payload["limits"], "discovery.limits")
    if not isinstance(limits.get("completed_window_size"), int):
        raise RuntimeError(f"discovery completed_window_size missing: {limits}")
    _assert_reason_list(payload["reasons"], "discovery.reasons")


def _assert_discovery_source(payload: dict[str, object], *, kind: str, name: str) -> None:
    source = _require_mapping(payload.get("source"), "discovery.source")
    if source.get("kind") != kind or source.get("name") != name:
        raise RuntimeError(f"discovery source mismatch: {source} != kind={kind} name={name}")


def _assert_state_surface(payload: dict[str, object]) -> None:
    _require_keys(payload, ["generated_at", "service_mode", "recovery_in_progress", "reasons", "counts", "records", "completed_window"], "state")
    for legacy_key in ["recovered_pending", "recovering", "retrying", "alerts", "service", "health", "observations"]:
        if legacy_key in payload:
            raise RuntimeError(f"/api/v1/state still exposes legacy top-level field {legacy_key}")
    if payload["service_mode"] not in {"serving", "degraded", "unavailable"}:
        raise RuntimeError(f"state service_mode mismatch: {payload}")
    _assert_reason_list(payload["reasons"], "state.reasons")
    counts = _require_mapping(payload["counts"], "state.counts")
    _require_keys(
        counts,
        ["total", "active", "retry_scheduled", "awaiting_merge", "awaiting_intervention", "completed"],
        "state.counts",
    )
    for key in ["total", "active", "retry_scheduled", "awaiting_merge", "awaiting_intervention", "completed"]:
        if not isinstance(counts[key], int):
            raise RuntimeError(f"state counts.{key} is not int: {counts}")
    records = _require_list(payload["records"], "state.records")
    for index, record in enumerate(records):
        _assert_record_surface(record, f"state.records[{index}]")
    completed_window = _require_mapping(payload["completed_window"], "state.completed_window")
    _require_keys(completed_window, ["limit", "records"], "state.completed_window")
    if not isinstance(completed_window["limit"], int):
        raise RuntimeError(f"completed_window.limit is not int: {completed_window}")
    completed_records = _require_list(completed_window["records"], "state.completed_window.records")
    for index, record in enumerate(completed_records):
        _assert_record_surface(record, f"state.completed_window.records[{index}]")


def _assert_ledger_surface(payload: dict[str, object]) -> None:
    _require_keys(payload, ["version", "identity", "saved_at", "service", "records"], "ledger")
    _assert_ledger_identity_shape(payload["identity"])
    records = _require_list(payload["records"], "ledger.records")
    for index, record in enumerate(records):
        _assert_ledger_record_surface(record, f"ledger.records[{index}]")


def _assert_refresh_contract(base_url: str) -> dict[str, object]:
    payload = _post_refresh(base_url)
    _assert_control_result(payload, expected_status="accepted")
    return payload


def _assert_control_result(payload: dict[str, object], *, expected_status: str) -> None:
    _require_keys(payload, ["action", "status", "reason", "recommended_next_step", "timestamp"], "control")
    if payload["action"] != "refresh" or payload["status"] != expected_status:
        raise RuntimeError(f"control result mismatch: {payload}")
    reason = _require_mapping(payload["reason"], "control.reason")
    _assert_reason_surface(reason, "control.reason")
    if not str(payload["recommended_next_step"]).strip():
        raise RuntimeError(f"control recommended_next_step missing: {payload}")
    if not str(payload["timestamp"]).strip():
        raise RuntimeError(f"control timestamp missing: {payload}")


def _assert_event_envelope(payload: dict[str, object], expected_event: str) -> None:
    _require_keys(payload, ["event_id", "event_type", "timestamp", "service_mode", "record_ids", "reason"], "events")
    if payload["event_type"] != expected_event:
        raise RuntimeError(f"SSE event_type mismatch: {payload}")
    if payload["service_mode"] not in {"serving", "degraded", "unavailable"}:
        raise RuntimeError(f"SSE service_mode mismatch: {payload}")
    record_ids = _require_list(payload["record_ids"], "events.record_ids")
    for item in record_ids:
        if not isinstance(item, str):
            raise RuntimeError(f"events.record_ids contains non-string: {payload}")
    reason = payload["reason"]
    if reason is not None:
        _assert_reason_surface(_require_mapping(reason, "events.reason"), "events.reason")


def _assert_service_surface_consistency(discovery_payload: dict[str, object], state_payload: dict[str, object]) -> None:
    if discovery_payload.get("service_mode") != state_payload.get("service_mode"):
        raise RuntimeError(
            f"discovery/state service_mode mismatch: discovery={discovery_payload.get('service_mode')} state={state_payload.get('service_mode')}"
        )
    if discovery_payload.get("reasons") != state_payload.get("reasons"):
        raise RuntimeError(
            f"discovery/state reasons mismatch: discovery={discovery_payload.get('reasons')} state={state_payload.get('reasons')}"
        )


def _assert_record_surface(record: object, label: str) -> None:
    current = _require_mapping(record, label)
    _require_keys(current, ["record_id", "source_ref", "status", "updated_at", "reason", "observation", "durable_refs", "result"], label)
    if current["status"] not in {"active", "retry_scheduled", "awaiting_merge", "awaiting_intervention", "completed"}:
        raise RuntimeError(f"{label} status mismatch: {current}")
    _assert_source_ref_surface(current["source_ref"], f"{label}.source_ref")
    _assert_record_identity_anchor(current, label)
    reason = current["reason"]
    if reason is not None:
        _assert_reason_surface(_require_mapping(reason, f"{label}.reason"), f"{label}.reason")
    observation = current["observation"]
    if observation is not None:
        _assert_observation_surface(_require_mapping(observation, f"{label}.observation"), f"{label}.observation")
    _assert_durable_refs_surface(_require_mapping(current["durable_refs"], f"{label}.durable_refs"), f"{label}.durable_refs")
    result = current["result"]
    if result is not None:
        _assert_result_surface(_require_mapping(result, f"{label}.result"), f"{label}.result")


def _assert_ledger_record_surface(record: object, label: str) -> None:
    current = _require_mapping(record, label)
    _require_keys(current, ["record_id", "source_ref", "status", "reason", "retry_due_at", "durable_refs", "result", "updated_at"], label)
    if current["status"] not in {"active", "retry_scheduled", "awaiting_merge", "awaiting_intervention", "completed"}:
        raise RuntimeError(f"{label} status mismatch: {current}")
    _assert_source_ref_surface(current["source_ref"], f"{label}.source_ref")
    _assert_record_identity_anchor(current, label)
    reason = current["reason"]
    if reason is not None:
        _assert_reason_surface(_require_mapping(reason, f"{label}.reason"), f"{label}.reason")
    _assert_durable_refs_surface(_require_mapping(current["durable_refs"], f"{label}.durable_refs"), f"{label}.durable_refs")
    result = current["result"]
    if result is not None:
        _assert_result_surface(_require_mapping(result, f"{label}.result"), f"{label}.result")


def _assert_source_ref_surface(value: object, label: str) -> None:
    payload = _require_mapping(value, label)
    _require_keys(payload, ["source_kind", "source_name", "source_id", "source_identifier", "url"], label)


def _sanitize_record_id_token(value: object, *, fallback: str) -> str:
    token = re.sub(r"[^A-Za-z0-9._-]", "_", str(value).strip()).strip("._-")
    return token or fallback


def _expected_record_id(source_ref: object, label: str) -> str:
    payload = _require_mapping(source_ref, label)
    return "_".join(
        [
            "rec",
            _sanitize_record_id_token(payload.get("source_kind", ""), fallback="source"),
            _sanitize_record_id_token(payload.get("source_name", ""), fallback="source"),
            _sanitize_record_id_token(payload.get("source_id", ""), fallback="id"),
        ]
    )


def _assert_record_identity_anchor(record: dict[str, object], label: str) -> None:
    record_id = str(record.get("record_id", "")).strip()
    if not record_id:
        raise RuntimeError(f"{label}.record_id missing: {record}")
    expected = _expected_record_id(record.get("source_ref"), f"{label}.source_ref")
    if record_id != expected:
        raise RuntimeError(f"{label}.record_id mismatch: {record_id} != {expected}")


def _assert_source_anchor(
    record: dict[str, object],
    label: str,
    *,
    source_kind: str,
    source_name: str,
    source_id: str,
    source_identifier: str,
) -> None:
    source_ref = _require_mapping(record.get("source_ref"), f"{label}.source_ref")
    if source_ref.get("source_kind") != source_kind:
        raise RuntimeError(f"{label}.source_kind mismatch: {source_ref}")
    if source_ref.get("source_name") != source_name:
        raise RuntimeError(f"{label}.source_name mismatch: {source_ref}")
    if source_ref.get("source_id") != source_id:
        raise RuntimeError(f"{label}.source_id mismatch: {source_ref}")
    if source_ref.get("source_identifier") != source_identifier:
        raise RuntimeError(f"{label}.source_identifier mismatch: {source_ref}")
    _assert_record_identity_anchor(record, label)


def _assert_reason_surface(value: object, label: str) -> None:
    payload = _require_mapping(value, label)
    _require_keys(payload, ["reason_code", "category", "details"], label)
    if payload["category"] not in {"api", "config", "control", "record", "runtime", "service"}:
        raise RuntimeError(f"{label} category mismatch: {payload}")
    if payload["details"] is not None:
        _require_mapping(payload["details"], f"{label}.details")


def _assert_reason_list(value: object, label: str) -> None:
    reasons = _require_list(value, label)
    for index, reason in enumerate(reasons):
        _assert_reason_surface(reason, f"{label}[{index}]")


def _assert_observation_surface(value: object, label: str) -> None:
    payload = _require_mapping(value, label)
    _require_keys(payload, ["running", "summary", "details"], label)
    if not isinstance(payload["running"], bool):
        raise RuntimeError(f"{label}.running is not bool: {payload}")
    if not isinstance(payload["summary"], str):
        raise RuntimeError(f"{label}.summary is not string: {payload}")
    if payload["details"] is not None:
        _require_mapping(payload["details"], f"{label}.details")


def _assert_result_surface(value: object, label: str) -> None:
    payload = _require_mapping(value, label)
    _require_keys(payload, ["outcome", "summary", "completed_at", "details"], label)
    if payload["outcome"] not in {"succeeded", "failed", "abandoned"}:
        raise RuntimeError(f"{label}.outcome mismatch: {payload}")
    if not isinstance(payload["summary"], str) or not str(payload["completed_at"]).strip():
        raise RuntimeError(f"{label} missing summary/completed_at: {payload}")
    if payload["details"] is not None:
        _require_mapping(payload["details"], f"{label}.details")


def _assert_durable_refs_surface(value: object, label: str) -> None:
    payload = _require_mapping(value, label)
    if not str(payload.get("ledger_path", "")).strip():
        raise RuntimeError(f"{label}.ledger_path missing: {payload}")
    for key in ["workspace", "branch", "pull_request"]:
        if key in payload and payload[key] is not None:
            _require_mapping(payload[key], f"{label}.{key}")


def _assert_ledger_identity_shape(value: object) -> None:
    identity = _require_mapping(value, "ledger.identity")
    _require_keys(identity, ["Compatibility", "Descriptor"], "ledger.identity")
    compatibility = _require_mapping(identity["Compatibility"], "ledger.identity.Compatibility")
    _require_keys(
        compatibility,
        ["Profile", "ActiveSource", "SourceKind", "FlowName", "TrackerKind", "TrackerRepo", "TrackerProjectSlug"],
        "ledger.identity.Compatibility",
    )
    descriptor = _require_mapping(identity["Descriptor"], "ledger.identity.Descriptor")
    _require_keys(
        descriptor,
        ["ConfigRoot", "WorkspaceRoot", "SessionPersistenceKind", "SessionStatePath"],
        "ledger.identity.Descriptor",
    )


def _assert_ledger_identity(
    payload: dict[str, object],
    *,
    active_source: str,
    flow_name: str,
    tracker_project_slug: str,
    workspace_root: str,
    ledger_path: str,
) -> None:
    identity = _require_mapping(payload["identity"], "ledger.identity")
    compatibility = _require_mapping(identity["Compatibility"], "ledger.identity.Compatibility")
    descriptor = _require_mapping(identity["Descriptor"], "ledger.identity.Descriptor")
    if compatibility["ActiveSource"] != active_source:
        raise RuntimeError(f"ledger identity ActiveSource mismatch: {compatibility}")
    if compatibility["FlowName"] != flow_name:
        raise RuntimeError(f"ledger identity FlowName mismatch: {compatibility}")
    if compatibility["TrackerProjectSlug"] != tracker_project_slug:
        raise RuntimeError(f"ledger identity TrackerProjectSlug mismatch: {compatibility}")
    if str(descriptor["WorkspaceRoot"]).replace("\\", "/").lower() != workspace_root.replace("\\", "/").lower():
        raise RuntimeError(f"ledger identity WorkspaceRoot mismatch: {descriptor}")
    if str(descriptor["SessionStatePath"]).replace("\\", "/").lower() != ledger_path.replace("\\", "/").lower():
        raise RuntimeError(f"ledger identity SessionStatePath mismatch: {descriptor}")


def _find_runtime_record(payload: dict[str, object], identifier: str, status: str | None = None) -> dict[str, object] | None:
    for record in _require_list(payload.get("records"), "state.records"):
        if not isinstance(record, dict):
            continue
        if _record_identifier(record) != identifier:
            continue
        if status is not None and record.get("status") != status:
            continue
        return record
    return None


def _find_completed_record(payload: dict[str, object], identifier: str) -> dict[str, object] | None:
    completed_window = _require_mapping(payload.get("completed_window"), "state.completed_window")
    for record in _require_list(completed_window.get("records"), "state.completed_window.records"):
        if not isinstance(record, dict):
            continue
        if _record_identifier(record) == identifier:
            return record
    return None


def _find_ledger_record(
    payload: dict[str, object],
    identifier: str,
    *,
    status: str | None = None,
    outcome: str | None = None,
) -> dict[str, object] | None:
    for record in _require_list(payload.get("records"), "ledger.records"):
        if not isinstance(record, dict):
            continue
        if _record_identifier(record) != identifier:
            continue
        if status is not None and record.get("status") != status:
            continue
        if outcome is not None:
            result = _require_mapping(record.get("result"), "ledger.record.result")
            if result.get("outcome") != outcome:
                continue
        return record
    return None


def _record_identifier(record: dict[str, object]) -> str:
    source_ref = _require_mapping(record.get("source_ref"), "record.source_ref")
    return str(source_ref.get("source_identifier", "")).strip()


def _require_runtime_record(payload: dict[str, object], identifier: str, *, status: str) -> dict[str, object]:
    record = _find_runtime_record(payload, identifier, status=status)
    if record is None:
        raise RuntimeError(f"runtime record {identifier!r} with status {status!r} not found: {payload}")
    return record


def _require_completed_record(payload: dict[str, object], identifier: str, *, outcome: str) -> dict[str, object]:
    record = _find_completed_record(payload, identifier)
    if record is None:
        raise RuntimeError(f"completed_window record {identifier!r} not found: {payload}")
    result = _require_mapping(record.get("result"), "completed_window.record.result")
    if result.get("outcome") != outcome:
        raise RuntimeError(f"completed_window outcome mismatch: {record}")
    return record


def _require_durable_ref(record: dict[str, object], key: str) -> dict[str, object]:
    durable_refs = _require_mapping(record.get("durable_refs"), "record.durable_refs")
    value = _require_mapping(durable_refs.get(key), f"record.durable_refs.{key}")
    return value


def _require_reason(record: dict[str, object], reason_code: str) -> dict[str, object]:
    reason = _require_mapping(record.get("reason"), "record.reason")
    _assert_reason_surface(reason, "record.reason")
    if reason.get("reason_code") != reason_code:
        raise RuntimeError(f"reason_code mismatch: {reason}")
    return reason


def _require_reason_from_list(value: object, reason_code: str) -> dict[str, object]:
    reasons = _require_list(value, "reasons")
    for item in reasons:
        if isinstance(item, dict) and item.get("reason_code") == reason_code:
            _assert_reason_surface(item, "reasons[]")
            return item
    raise RuntimeError(f"reason_code {reason_code!r} not found in {value!r}")


def _has_reason_code(value: object, reason_code: str) -> bool:
    try:
        _require_reason_from_list(value, reason_code)
    except RuntimeError:
        return False
    return True


def _require_mapping(value: object, label: str) -> dict[str, object]:
    if not isinstance(value, dict):
        raise RuntimeError(f"{label} is not object: {value!r}")
    return value


def _require_list(value: object, label: str) -> list[object]:
    if not isinstance(value, list):
        raise RuntimeError(f"{label} is not list: {value!r}")
    return value


def _require_keys(payload: object, keys: list[str], label: str) -> None:
    current = _require_mapping(payload, label)
    missing = [key for key in keys if key not in current]
    if missing:
        raise RuntimeError(f"{label} missing keys {missing}: {current}")


def _assert_notification_details(event: dict[str, object], **expected_details: str) -> None:
    body = event.get("body")
    if not isinstance(body, dict):
        raise RuntimeError(f"notification body is not json object: {event}")
    for key, expected in expected_details.items():
        current: object = body
        for part in key.split("."):
            if not isinstance(current, dict):
                raise RuntimeError(f"notification field path {key!r} missing: {body}")
            current = current.get(part)
        if str(current or "").strip() != expected:
            raise RuntimeError(f"notification field {key!r} mismatch: {current!r} != {expected!r}")


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
