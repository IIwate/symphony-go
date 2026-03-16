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
    if state_payload["service_mode"] != "serving":
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

        _wait_for(
            lambda: _read_sse_event(events, "object_changed"),
            process,
            timeout_seconds=60,
            description="missing_pr object_changed",
        )
        identifier = str(issue["identifier"])
        job = _wait_for(
            lambda: _find_object_by_source_identifier(base_url, "job", identifier, state="intervention_required"),
            process,
            timeout_seconds=120,
            description="missing_pr intervention_required job",
            interval_seconds=0.5,
        )
        run = _wait_for(
            lambda: _find_object_by_source_identifier(base_url, "run", identifier, state="intervention_required"),
            process,
            timeout_seconds=120,
            description="missing_pr intervention_required run",
            interval_seconds=0.5,
        )
        intervention = _wait_for(
            lambda: (
                related
                if (related := _find_related_object(base_url, run, relation_type="run.intervention", target_type="intervention")) is not None
                and related.get("state") == "open"
                else None
            ),
            process,
            timeout_seconds=120,
            description="missing_pr open intervention",
            interval_seconds=0.5,
        )
        reason = _require_reason_from_object(intervention, "run.blocked.intervention_required")
        if reason.get("details", {}).get("cause") != "missing_pr":
            raise RuntimeError(f"unexpected intervention reason: {intervention}")
        if job.get("state") != "intervention_required":
            raise RuntimeError(f"job state mismatch: {job}")
    finally:
        events.close()
        process.stop()
        resources.processes.remove(process)
        linear.update_issue_state(str(issue["id"]), context.canceled_state_id)
        resources.issue_ids.remove(str(issue["id"]))
    return f"{issue['identifier']} -> intervention_required via object query; refresh=accepted"


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
        _wait_for(
            lambda: _read_sse_event(events, "object_changed"),
            process,
            timeout_seconds=60,
            description="awaiting_merge object_changed",
        )
        identifier = str(issue["identifier"])
        run = _wait_for(
            lambda: (
                current
                if (current := _find_object_by_source_identifier(base_url, "run", identifier, state="completed")) is not None
                and current.get("phase") == "publishing"
                and isinstance(current.get("candidate_delivery"), dict)
                and current["candidate_delivery"].get("reached") is True
                else None
            ),
            process,
            timeout_seconds=120,
            description="awaiting_merge candidate delivery run",
            interval_seconds=0.5,
        )
        pr_ref = _require_reference(run, "github_pull_request")
        if int(str(pr_ref["external_id"])) != pr.number:
            raise RuntimeError(f"awaiting_merge pr_number mismatch: {pr_ref['external_id']} != {pr.number}")

        merge_pull_request(repo, pr.number)
        resources.pull_request_numbers.remove(pr.number)
        control_payload = _assert_refresh_contract(base_url)
        if control_payload["status"] != "accepted":
            raise RuntimeError(f"merged_pr_source_not_terminal refresh status mismatch: {control_payload}")
        _wait_for(
            lambda: _read_sse_event(events, "object_changed"),
            process,
            timeout_seconds=60,
            description="merged_pr_source_not_terminal object_changed",
        )
        job = _wait_for(
            lambda: _find_object_by_source_identifier(base_url, "job", identifier, state="intervention_required"),
            process,
            timeout_seconds=120,
            description="merged_pr_source_not_terminal intervention_required job",
            interval_seconds=0.5,
        )
        run = _wait_for(
            lambda: _find_object_by_source_identifier(base_url, "run", identifier, state="intervention_required"),
            process,
            timeout_seconds=120,
            description="merged_pr_source_not_terminal intervention_required run",
            interval_seconds=0.5,
        )
        intervention = _wait_for(
            lambda: (
                related
                if (related := _find_related_object(base_url, run, relation_type="run.intervention", target_type="intervention")) is not None
                and related.get("state") == "open"
                else None
            ),
            process,
            timeout_seconds=120,
            description="merged_pr_source_not_terminal open intervention",
            interval_seconds=0.5,
        )
        reason = _require_reason_from_object(intervention, "run.blocked.intervention_required")
        if reason.get("details", {}).get("cause") != "merged_pr_source_not_terminal":
            raise RuntimeError(f"unexpected merged intervention reason: {reason}")
        if job.get("state") != "intervention_required":
            raise RuntimeError(f"job state mismatch: {job}")

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
        _wait_for(
            lambda: _read_sse_event(events, "object_changed"),
            process,
            timeout_seconds=60,
            description="completed object_changed",
        )
        completed_job = _wait_for(
            lambda: _find_object_by_source_identifier(base_url, "job", identifier, state="completed"),
            process,
            timeout_seconds=180,
            description="completed job",
            interval_seconds=0.5,
        )
        outcome = _wait_for(
            lambda: _find_object_by_source_identifier(base_url, "outcome", identifier, state="succeeded"),
            process,
            timeout_seconds=180,
            description="completed outcome",
            interval_seconds=0.5,
        )
        if completed_job.get("state") != "completed":
            raise RuntimeError(f"completed job state mismatch: {completed_job}")
        if outcome.get("state") != "succeeded":
            raise RuntimeError(f"outcome state mismatch: {outcome}")
    finally:
        events.close()
        process.stop()
        resources.processes.remove(process)
        resources.issue_ids.remove(str(issue["id"]))
    return f"{issue['identifier']} -> candidate_delivery -> intervention_required(merged_pr_source_not_terminal) -> outcome.succeeded"


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
        base_dir = resources.temp_dir / "formal-objects"
        session_state_path = base_dir / "local" / "runtime-state.json"
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
        _wait_for(
            lambda: _await_formal_startup(base_url),
            process,
            timeout_seconds=30,
            description="formal_objects startup",
        )
        _assert_discovery_source(fetch_json(f"{base_url}/api/v1/discovery"), kind="linear", name="linear-main")
        events = open_events_stream(f"{base_url}/api/v1/events")
        _await_sse_event(events, process, expected_event="snapshot", timeout_seconds=15, description="formal_objects snapshot")

        persistence_issue = linear.create_issue(f"{issue_prefix} formal_objects persistence {int(time.time())}", context)
        resources.issue_ids.append(str(persistence_issue["id"]))
        persistence_identifier = str(persistence_issue["identifier"])

        _wait_for(
            lambda: _read_sse_event(events, "object_changed"),
            process,
            timeout_seconds=60,
            description="formal_objects intervention object_changed",
        )
        job = _wait_for(
            lambda: _find_object_by_source_identifier(base_url, "job", persistence_identifier, state="intervention_required"),
            process,
            timeout_seconds=120,
            description="formal_objects intervention_required job",
            interval_seconds=0.5,
        )
        run = _wait_for(
            lambda: _find_object_by_source_identifier(base_url, "run", persistence_identifier, state="intervention_required"),
            process,
            timeout_seconds=120,
            description="formal_objects intervention_required run",
            interval_seconds=0.5,
        )
        intervention = _wait_for(
            lambda: (
                related
                if (related := _find_related_object(base_url, run, relation_type="run.intervention", target_type="intervention")) is not None
                and related.get("state") == "open"
                else None
            ),
            process,
            timeout_seconds=120,
            description="formal_objects open intervention",
            interval_seconds=0.5,
        )
        reason = _require_reason_from_object(intervention, "run.blocked.intervention_required")
        if reason.get("details", {}).get("cause") != "missing_pr":
            raise RuntimeError(f"awaiting_intervention reason mismatch: {intervention}")
        source_reference = _require_reference(job, "linear_issue")
        if source_reference.get("external_id") != str(persistence_issue["id"]):
            raise RuntimeError(f"job linear reference mismatch: {source_reference}")

        webhook_events = _wait_for(
            lambda: recorder.find(path="/webhook", identifier=persistence_identifier, event_type="issue_intervention_required") or None,
            process,
            timeout_seconds=30,
            description="formal_objects webhook intervention notification",
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
            description="formal_objects slack intervention notification",
            interval_seconds=0.5,
        )
        session_state_payload = _wait_for(
            lambda: _await_session_state_object(session_state_path, "job", persistence_identifier, state="intervention_required"),
            process,
            timeout_seconds=15,
            description="formal_objects session_state persisted",
            interval_seconds=0.2,
        )
        _assert_ledger_identity(
            session_state_payload,
            active_source="linear-main",
            flow_name="implement",
            tracker_project_slug=project_slug,
            workspace_root=str((resources.temp_dir.parent / f"workspaces-{namespace}").resolve()).replace("\\", "/"),
            ledger_path=str(session_state_path.resolve()).replace("\\", "/"),
        )
        if _find_session_state_object(session_state_payload, "run", persistence_identifier, state="intervention_required") is None:
            raise RuntimeError(f"session_state missing run snapshot: {session_state_payload}")
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
            description="formal_objects restart",
        )
        events = open_events_stream(f"{base_url}/api/v1/events")
        _await_sse_event(events, process, expected_event="snapshot", timeout_seconds=15, description="formal_objects restart snapshot")
        _wait_for(
            lambda: _find_object_by_source_identifier(base_url, "job", persistence_identifier, state="intervention_required"),
            process,
            timeout_seconds=60,
            description="formal_objects restored intervention_required job",
            interval_seconds=0.5,
        )
        time.sleep(5)
        if recorder.count() != notification_count_before_restart:
            raise RuntimeError(
                f"unexpected notification replay after restart: before={notification_count_before_restart}, after={recorder.count()}"
            )

        merge_issue = linear.create_issue(f"{issue_prefix} formal_objects merge {int(time.time())}", context)
        resources.issue_ids.append(str(merge_issue["id"]))
        merge_identifier = str(merge_issue["identifier"])
        branch = _linear_branch_name(namespace, branch_scope, merge_identifier)
        pr = prepare_pull_request(
            repo,
            repo_url,
            branch,
            title=f"test: live smoke {merge_identifier}",
            body="Temporary PR for formal object smoke.",
            work_root=resources.temp_dir / "formal-objects-pr",
        )
        resources.pull_request_numbers.append(pr.number)

        _wait_for(
            lambda: _read_sse_event(events, "object_changed"),
            process,
            timeout_seconds=60,
            description="formal_objects candidate delivery object_changed",
        )
        merge_run = _wait_for(
            lambda: (
                current
                if (current := _find_object_by_source_identifier(base_url, "run", merge_identifier, state="completed")) is not None
                and current.get("phase") == "publishing"
                and isinstance(current.get("candidate_delivery"), dict)
                and current["candidate_delivery"].get("reached") is True
                else None
            ),
            process,
            timeout_seconds=120,
            description="formal_objects candidate delivery run",
            interval_seconds=0.5,
        )
        pr_ref = _require_reference(merge_run, "github_pull_request")
        if int(str(pr_ref["external_id"])) != pr.number:
            raise RuntimeError(f"formal_objects candidate delivery pr mismatch: {pr_ref['external_id']} != {pr.number}")
        _wait_for(
            lambda: _await_session_state_object(session_state_path, "run", merge_identifier, state="completed"),
            process,
            timeout_seconds=15,
            description="formal_objects candidate delivery persisted",
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
            description="formal_objects second restart",
        )
        events = open_events_stream(f"{base_url}/api/v1/events")
        _await_sse_event(events, process, expected_event="snapshot", timeout_seconds=15, description="formal_objects second snapshot")
        restored_merge = _wait_for(
            lambda: (
                current
                if (current := _find_object_by_source_identifier(base_url, "run", merge_identifier, state="completed")) is not None
                and current.get("phase") == "publishing"
                and isinstance(current.get("candidate_delivery"), dict)
                and current["candidate_delivery"].get("reached") is True
                else None
            ),
            process,
            timeout_seconds=60,
            description="formal_objects restored candidate delivery run",
            interval_seconds=0.5,
        )
        restored_pr_ref = _require_reference(restored_merge, "github_pull_request")
        if int(str(restored_pr_ref["external_id"])) != pr.number:
            raise RuntimeError(f"formal_objects restored pr mismatch: {restored_pr_ref['external_id']} != {pr.number}")

        merge_pull_request(repo, pr.number)
        resources.pull_request_numbers.remove(pr.number)
        _wait_for(
            lambda: _read_sse_event(events, "object_changed"),
            process,
            timeout_seconds=60,
            description="formal_objects merged intervention object_changed",
        )
        merge_job = _wait_for(
            lambda: _find_object_by_source_identifier(base_url, "job", merge_identifier, state="intervention_required"),
            process,
            timeout_seconds=180,
            description="formal_objects merged intervention job",
            interval_seconds=0.5,
        )
        merge_run = _wait_for(
            lambda: _find_object_by_source_identifier(base_url, "run", merge_identifier, state="intervention_required"),
            process,
            timeout_seconds=180,
            description="formal_objects merged intervention run",
            interval_seconds=0.5,
        )
        intervention = _wait_for(
            lambda: (
                related
                if (related := _find_related_object(base_url, merge_run, relation_type="run.intervention", target_type="intervention")) is not None
                and related.get("state") == "open"
                else None
            ),
            process,
            timeout_seconds=180,
            description="formal_objects merged open intervention",
            interval_seconds=0.5,
        )
        intervention_reason = _require_reason_from_object(intervention, "run.blocked.intervention_required")
        if intervention_reason.get("details", {}).get("cause") != "merged_pr_source_not_terminal":
            raise RuntimeError(f"unexpected formal_objects merge intervention reason: {intervention_reason}")
        _wait_for(
            lambda: _await_session_state_object(session_state_path, "job", merge_identifier, state="intervention_required"),
            process,
            timeout_seconds=15,
            description="formal_objects merged intervention persisted",
            interval_seconds=0.2,
        )
        _wait_for(
            lambda: recorder.find(path="/webhook", identifier=merge_identifier, event_type="issue_intervention_required") or None,
            process,
            timeout_seconds=60,
            description="formal_objects webhook intervention notification after merge",
            interval_seconds=0.5,
        )
        _wait_for(
            lambda: recorder.find(path="/slack", identifier=merge_identifier, event_type="issue_intervention_required") or None,
            process,
            timeout_seconds=60,
            description="formal_objects slack intervention notification after merge",
            interval_seconds=0.5,
        )
        if merge_job.get("state") != "intervention_required":
            raise RuntimeError(f"merged job state mismatch: {merge_job}")

        linear.update_issue_state(str(merge_issue["id"]), context.done_state_id)
        _wait_for(
            lambda: _await_linear_done(linear, str(merge_issue["id"])),
            process,
            timeout_seconds=180,
            description="formal_objects issue done after external source close",
        )
        _wait_for(
            lambda: _read_sse_event(events, "object_changed"),
            process,
            timeout_seconds=60,
            description="formal_objects completed object_changed",
        )
        completed_job = _wait_for(
            lambda: _find_object_by_source_identifier(base_url, "job", merge_identifier, state="completed"),
            process,
            timeout_seconds=180,
            description="formal_objects completed job",
            interval_seconds=0.5,
        )
        completed_outcome = _wait_for(
            lambda: _find_object_by_source_identifier(base_url, "outcome", merge_identifier, state="succeeded"),
            process,
            timeout_seconds=180,
            description="formal_objects completed outcome",
            interval_seconds=0.5,
        )
        _wait_for(
            lambda: _await_session_state_object(session_state_path, "outcome", merge_identifier, state="succeeded"),
            process,
            timeout_seconds=15,
            description="formal_objects completed persisted",
            interval_seconds=0.2,
        )
        _wait_for(
            lambda: recorder.find(path="/webhook", identifier=merge_identifier, event_type="issue_completed") or None,
            process,
            timeout_seconds=60,
            description="formal_objects webhook completed notification",
            interval_seconds=0.5,
        )
        _wait_for(
            lambda: recorder.find(path="/slack", identifier=merge_identifier, event_type="issue_completed") or None,
            process,
            timeout_seconds=60,
            description="formal_objects slack completed notification",
            interval_seconds=0.5,
        )
        if completed_job.get("state") != "completed" or completed_outcome.get("state") != "succeeded":
            raise RuntimeError(f"completed objects mismatch: job={completed_job}, outcome={completed_outcome}")

        linear.update_issue_state(str(persistence_issue["id"]), context.canceled_state_id)
        resources.issue_ids.remove(str(persistence_issue["id"]))
        resources.issue_ids.remove(str(merge_issue["id"]))
        return f"{persistence_identifier}/intervention_required + {merge_identifier}/candidate_delivery_then_intervention -> outcome.succeeded recovered from session_state"
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
            lambda: _await_service_mode(base_url, expected_mode="degraded", reason_code="service.degraded.notification_delivery"),
            process,
            timeout_seconds=120,
            description="notification_degraded state",
            interval_seconds=0.5,
        )
        _wait_for(
            lambda: _find_object_by_source_identifier(base_url, "job", identifier, state="intervention_required"),
            process,
            timeout_seconds=120,
            description="notification_degraded intervention_required job",
            interval_seconds=0.5,
        )
        discovery_payload = fetch_json(f"{base_url}/api/v1/discovery")
        _assert_discovery_surface(discovery_payload)
        _assert_service_surface_consistency(discovery_payload, degraded_state)
        service_reason = _require_reason_from_list(degraded_state.get("reasons"), "service.degraded.notification_delivery")
        channel_ids = service_reason["details"].get("channel_ids")
        if channel_ids != ["local-slack"]:
            raise RuntimeError(f"notification_degraded channel_ids mismatch: {service_reason}")
        return f"{identifier} kept serving formal object flow while service_mode=degraded(channel_ids={channel_ids})"
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
        session_state_path = base_dir / "local" / "runtime-state.json"
        config = SmokeConfig(
            base_dir=base_dir,
            port=port,
            namespace=f"{branch_namespace}-unavailable",
            repo_url=repo_url,
            linear_api_key=linear.api_key,
            linear_project_slug=project_slug,
            linear_branch_scope=branch_scope,
            codex_command=codex_command,
            session_state_path=session_state_path,
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
            lambda: _load_ledger(session_state_path) if session_state_path.exists() else None,
            process,
            timeout_seconds=10,
            description="ledger_unavailable ledger exists",
            interval_seconds=0.2,
        )

        if session_state_path.exists():
            session_state_path.unlink()
        session_state_path.mkdir(parents=True, exist_ok=True)

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
        rejected_control = _post_refresh(base_url)
        _assert_control_result(rejected_control, expected_status="rejected")
        _require_reason_from_list(unavailable_state.get("reasons"), "service.unavailable.core_dependency")
        return f"{issue['identifier']} drove service_mode=unavailable via session_state write failure"
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


FORMAL_OBJECT_TYPES = {
    "job",
    "run",
    "intervention",
    "outcome",
    "artifact",
    "action",
    "instance",
    "reference",
}

REASON_CATEGORIES = {
    "action",
    "api",
    "capability",
    "checkpoint",
    "config",
    "control",
    "intervention",
    "job",
    "outcome",
    "record",
    "reference",
    "run",
    "runtime",
    "security",
    "service",
}

SERVICE_MODES = {"serving", "degraded", "unavailable"}
EVENT_TYPES_WITH_OBJECTS = {"snapshot", "object_changed"}


def _await_session_state_object(
    path: Path,
    object_type: str,
    identifier: str,
    *,
    state: str,
) -> dict[str, object] | None:
    if not path.exists() or path.is_dir():
        return None
    payload = _load_ledger(path)
    _assert_session_state_surface(payload)
    if _find_session_state_object(payload, object_type, identifier, state=state) is None:
        return None
    return payload


def _assert_discovery_surface(payload: dict[str, object]) -> None:
    _require_keys(payload, ["api_version", "instance", "domain_id", "source", "capabilities"], "discovery")
    if payload["api_version"] != "v1":
        raise RuntimeError(f"discovery api_version mismatch: {payload}")
    _require_keys(payload["instance"], ["id", "name", "version"], "discovery.instance")
    _require_keys(payload["source"], ["kind", "name"], "discovery.source")
    if not str(payload["domain_id"]).strip():
        raise RuntimeError(f"discovery domain_id missing: {payload}")
    _assert_static_capability_surface(payload["capabilities"], "discovery.capabilities")


def _assert_discovery_source(payload: dict[str, object], *, kind: str, name: str) -> None:
    source = _require_mapping(payload.get("source"), "discovery.source")
    if source.get("kind") != kind or source.get("name") != name:
        raise RuntimeError(f"discovery source mismatch: {source} != kind={kind} name={name}")


def _assert_state_surface(payload: dict[str, object]) -> None:
    _require_keys(payload, ["generated_at", "service_mode", "recovery_in_progress", "reasons", "instance", "capabilities"], "state")
    for legacy_key in ["recovered_pending", "recovering", "retrying", "alerts", "service", "health", "observations", "counts", "records", "completed_window", "limits", "source"]:
        if legacy_key in payload:
            raise RuntimeError(f"/api/v1/state still exposes legacy top-level field {legacy_key}")
    if payload["service_mode"] not in SERVICE_MODES:
        raise RuntimeError(f"state service_mode mismatch: {payload}")
    _assert_reason_list(payload["reasons"], "state.reasons")
    instance = _require_mapping(payload["instance"], "state.instance")
    _require_keys(instance, ["id", "name", "version", "role"], "state.instance")
    if instance["role"] not in {"leader", "standby"}:
        raise RuntimeError(f"state.instance.role mismatch: {instance}")
    _assert_available_capability_surface(payload["capabilities"], "state.capabilities")


def _assert_session_state_surface(payload: dict[str, object]) -> None:
    _require_keys(payload, ["version", "identity", "saved_at", "service", "jobs", "formal_objects"], "session_state")
    for legacy_key in ["records", "completed_window", "retrying", "recovering", "awaiting_merge", "awaiting_intervention"]:
        if legacy_key in payload:
            raise RuntimeError(f"session_state still exposes legacy top-level field {legacy_key}")
    _assert_ledger_identity_shape(payload["identity"])
    if not isinstance(payload["version"], int):
        raise RuntimeError(f"session_state.version is not int: {payload}")
    jobs = _require_list(payload["jobs"], "session_state.jobs")
    for index, job in enumerate(jobs):
        _assert_session_state_job_surface(job, f"session_state.jobs[{index}]")
    _assert_formal_objects_snapshot_surface(payload["formal_objects"], "session_state.formal_objects")


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
    _require_keys(payload, ["event_id", "event_type", "timestamp", "contract_version", "domain_id", "service_mode", "reason"], "events")
    if payload["event_type"] != expected_event:
        raise RuntimeError(f"SSE event_type mismatch: {payload}")
    if payload["service_mode"] not in SERVICE_MODES:
        raise RuntimeError(f"SSE service_mode mismatch: {payload}")
    if payload.get("contract_version") != "v1":
        raise RuntimeError(f"SSE contract_version mismatch: {payload}")
    if not str(payload.get("domain_id", "")).strip():
        raise RuntimeError(f"SSE domain_id missing: {payload}")
    if "record_ids" in payload:
        raise RuntimeError(f"SSE still exposes legacy record_ids: {payload}")
    if expected_event in EVENT_TYPES_WITH_OBJECTS:
        objects = _require_list(payload.get("objects"), "events.objects")
        for index, item in enumerate(objects):
            _assert_event_object_surface(item, f"events.objects[{index}]")
    elif "objects" in payload and payload["objects"] is not None:
        objects = _require_list(payload["objects"], "events.objects")
        for index, item in enumerate(objects):
            _assert_event_object_surface(item, f"events.objects[{index}]")
    reason = payload["reason"]
    if reason is not None:
        _assert_reason_surface(_require_mapping(reason, "events.reason"), "events.reason")


def _assert_service_surface_consistency(discovery_payload: dict[str, object], state_payload: dict[str, object]) -> None:
    discovery_instance = _require_mapping(discovery_payload.get("instance"), "discovery.instance")
    state_instance = _require_mapping(state_payload.get("instance"), "state.instance")
    for key in ["id", "name", "version"]:
        if discovery_instance.get(key) != state_instance.get(key):
            raise RuntimeError(
                f"discovery/state instance.{key} mismatch: discovery={discovery_instance.get(key)!r} state={state_instance.get(key)!r}"
            )


def _assert_static_capability_surface(value: object, label: str) -> None:
    payload = _require_mapping(value, label)
    capabilities = _require_list(payload.get("capabilities"), f"{label}.capabilities")
    for index, capability in enumerate(capabilities):
        current = _require_mapping(capability, f"{label}.capabilities[{index}]")
        _require_keys(current, ["name", "category", "summary", "supported"], f"{label}.capabilities[{index}]")
        if not isinstance(current["supported"], bool):
            raise RuntimeError(f"{label}.capabilities[{index}].supported is not bool: {current}")


def _assert_available_capability_surface(value: object, label: str) -> None:
    payload = _require_mapping(value, label)
    capabilities = _require_list(payload.get("capabilities"), f"{label}.capabilities")
    for index, capability in enumerate(capabilities):
        current = _require_mapping(capability, f"{label}.capabilities[{index}]")
        _require_keys(current, ["name", "category", "summary", "available"], f"{label}.capabilities[{index}]")
        if not isinstance(current["available"], bool):
            raise RuntimeError(f"{label}.capabilities[{index}].available is not bool: {current}")
        if "reasons" in current and current["reasons"] is not None:
            _assert_reason_list(current["reasons"], f"{label}.capabilities[{index}].reasons")


def _assert_formal_objects_snapshot_surface(value: object, label: str) -> None:
    payload = _require_mapping(value, label)
    _require_keys(payload, ["records"], label)
    records = _require_list(payload["records"], f"{label}.records")
    for index, item in enumerate(records):
        current = _require_mapping(item, f"{label}.records[{index}]")
        _require_keys(current, ["object_type", "object_id", "storage_tier", "lifecycle", "updated_at", "payload"], f"{label}.records[{index}]")
        object_type = str(current["object_type"]).strip()
        if object_type not in FORMAL_OBJECT_TYPES:
            raise RuntimeError(f"{label}.records[{index}].object_type mismatch: {current}")
        if current["storage_tier"] not in {"hot", "archive"}:
            raise RuntimeError(f"{label}.records[{index}].storage_tier mismatch: {current}")
        if current["lifecycle"] not in {"active", "terminated", "invalidated", "archived"}:
            raise RuntimeError(f"{label}.records[{index}].lifecycle mismatch: {current}")
        payload_item = _require_mapping(current["payload"], f"{label}.records[{index}].payload")
        _assert_formal_object_surface(payload_item, f"{label}.records[{index}].payload", expected_object_type=object_type)
        if payload_item["id"] != current["object_id"]:
            raise RuntimeError(f"{label}.records[{index}] object_id mismatch: {current}")


def _assert_session_state_job_surface(value: object, label: str) -> None:
    payload = _require_mapping(value, label)
    _require_keys(payload, ["job_id", "updated_at"], label)
    for forbidden in ["record_id", "source_ref", "durable_refs", "result", "observation", "last_known_issue", "last_known_issue_state"]:
        if forbidden in payload:
            raise RuntimeError(f"{label} still exposes legacy canonical field {forbidden}: {payload}")


def _assert_object_list_response(payload: dict[str, object], object_type: str) -> None:
    _require_keys(payload, ["object_type", "items"], "objects.list")
    if payload["object_type"] != object_type:
        raise RuntimeError(f"object list type mismatch: {payload}")
    items = _require_list(payload["items"], "objects.list.items")
    for index, item in enumerate(items):
        _assert_formal_object_surface(_require_mapping(item, f"objects.list.items[{index}]"), f"objects.list.items[{index}]", expected_object_type=object_type)


def _assert_object_query_response(payload: dict[str, object], object_type: str) -> None:
    _require_keys(payload, ["object_type", "item"], "objects.query")
    if payload["object_type"] != object_type:
        raise RuntimeError(f"object query type mismatch: {payload}")
    _assert_formal_object_surface(_require_mapping(payload["item"], "objects.query.item"), "objects.query.item", expected_object_type=object_type)


def _assert_formal_object_surface(value: object, label: str, *, expected_object_type: str | None = None) -> None:
    payload = _require_mapping(value, label)
    _require_keys(payload, ["id", "object_type", "domain_id", "visibility", "contract_version", "created_at", "updated_at"], label)
    for forbidden in ["record_id", "source_ref", "durable_refs", "result", "observation", "last_known_issue", "last_known_issue_state"]:
        if forbidden in payload:
            raise RuntimeError(f"{label} still exposes legacy field {forbidden}: {payload}")
    object_type = str(payload["object_type"]).strip()
    if expected_object_type is not None and object_type != expected_object_type:
        raise RuntimeError(f"{label}.object_type mismatch: {payload}")
    if object_type not in FORMAL_OBJECT_TYPES:
        raise RuntimeError(f"{label}.object_type invalid: {payload}")
    if payload.get("contract_version") != "v1":
        raise RuntimeError(f"{label}.contract_version mismatch: {payload}")
    if payload.get("visibility") not in {"summary", "restricted", "sensitive"}:
        raise RuntimeError(f"{label}.visibility mismatch: {payload}")
    _assert_object_context_surface(payload, label)
    required_by_type = {
        "job": ["state", "job_type", "action_summary"],
        "run": ["state", "phase", "attempt"],
        "intervention": ["state", "template_id", "summary", "required_inputs", "allowed_actions"],
        "outcome": ["state", "summary", "completed_at"],
        "artifact": ["state", "kind", "role"],
        "action": ["state", "type", "summary"],
        "instance": ["state", "name", "version", "role", "static_capabilities", "available_capabilities"],
        "reference": ["state", "type", "system", "locator"],
    }
    _require_keys(payload, required_by_type[object_type], label)
    if object_type == "job":
        action_summary = _require_mapping(payload["action_summary"], f"{label}.action_summary")
        _require_keys(action_summary, ["has_pending_external_actions", "pending_count"], f"{label}.action_summary")
    elif object_type == "run":
        if not isinstance(payload["attempt"], int):
            raise RuntimeError(f"{label}.attempt is not int: {payload}")
        if "candidate_delivery" in payload and payload["candidate_delivery"] is not None:
            candidate = _require_mapping(payload["candidate_delivery"], f"{label}.candidate_delivery")
            _require_keys(candidate, ["kind", "reached", "reached_at", "summary", "artifact_ids"], f"{label}.candidate_delivery")
        if "review_gate" in payload and payload["review_gate"] is not None:
            review_gate = _require_mapping(payload["review_gate"], f"{label}.review_gate")
            _require_keys(review_gate, ["status", "required", "max_fix_rounds"], f"{label}.review_gate")
    elif object_type == "instance":
        _assert_static_capability_surface(payload["static_capabilities"], f"{label}.static_capabilities")
        _assert_available_capability_surface(payload["available_capabilities"], f"{label}.available_capabilities")


def _assert_object_context_surface(payload: dict[str, object], label: str) -> None:
    if "relations" in payload and payload["relations"] is not None:
        relations = _require_list(payload["relations"], f"{label}.relations")
        for index, item in enumerate(relations):
            relation = _require_mapping(item, f"{label}.relations[{index}]")
            _require_keys(relation, ["type", "target_id", "target_type"], f"{label}.relations[{index}]")
    if "references" in payload and payload["references"] is not None:
        references = _require_list(payload["references"], f"{label}.references")
        for index, item in enumerate(references):
            _assert_reference_surface(item, f"{label}.references[{index}]")
    if "reasons" in payload and payload["reasons"] is not None:
        _assert_reason_list(payload["reasons"], f"{label}.reasons")
    if "decision" in payload and payload["decision"] is not None:
        decision = _require_mapping(payload["decision"], f"{label}.decision")
        _require_keys(decision, ["decision_code", "category", "recommended_actions"], f"{label}.decision")
    if "error_code" in payload and payload["error_code"] is not None and not str(payload["error_code"]).strip():
        raise RuntimeError(f"{label}.error_code is blank: {payload}")


def _assert_reference_surface(value: object, label: str) -> None:
    payload = _require_mapping(value, label)
    _require_keys(payload, ["id", "object_type", "domain_id", "visibility", "contract_version", "created_at", "updated_at", "state", "type", "system", "locator"], label)
    if payload["object_type"] != "reference":
        raise RuntimeError(f"{label}.object_type mismatch: {payload}")


def _assert_event_object_surface(value: object, label: str) -> None:
    payload = _require_mapping(value, label)
    _require_keys(payload, ["object_type", "object_id"], label)
    if str(payload["object_type"]).strip() not in FORMAL_OBJECT_TYPES:
        raise RuntimeError(f"{label}.object_type mismatch: {payload}")


def _list_objects(base_url: str, object_type: str) -> list[dict[str, object]]:
    payload = fetch_json(f"{base_url}/api/v1/objects/{object_type}")
    _assert_object_list_response(payload, object_type)
    return [_require_mapping(item, f"objects.list.items[{index}]") for index, item in enumerate(_require_list(payload["items"], "objects.list.items"))]


def _get_object(base_url: str, object_type: str, object_id: str) -> dict[str, object]:
    payload = fetch_json(f"{base_url}/api/v1/objects/{object_type}/{object_id}")
    _assert_object_query_response(payload, object_type)
    return _require_mapping(payload["item"], "objects.query.item")


def _find_object_by_source_identifier(
    base_url: str,
    object_type: str,
    identifier: str,
    *,
    state: str | None = None,
) -> dict[str, object] | None:
    for item in _list_objects(base_url, object_type):
        if not _object_has_source_identifier(item, identifier):
            continue
        if state is not None and item.get("state") != state:
            continue
        return item
    return None


def _object_has_source_identifier(item: dict[str, object], identifier: str) -> bool:
    references = item.get("references")
    if not isinstance(references, list):
        return False
    for reference in references:
        if not isinstance(reference, dict):
            continue
        if reference.get("type") == "linear_issue" and reference.get("locator") == identifier:
            return True
    return False


def _find_related_object(
    base_url: str,
    item: dict[str, object],
    *,
    relation_type: str,
    target_type: str,
) -> dict[str, object] | None:
    relations = item.get("relations")
    if not isinstance(relations, list):
        return None
    for relation in relations:
        if not isinstance(relation, dict):
            continue
        if relation.get("type") != relation_type or relation.get("target_type") != target_type:
            continue
        target_id = str(relation.get("target_id", "")).strip()
        if not target_id:
            continue
        return _get_object(base_url, target_type, target_id)
    return None


def _find_session_state_object(
    payload: dict[str, object],
    object_type: str,
    identifier: str,
    *,
    state: str | None = None,
) -> dict[str, object] | None:
    formal_objects = _require_mapping(payload.get("formal_objects"), "session_state.formal_objects")
    records = _require_list(formal_objects.get("records"), "session_state.formal_objects.records")
    for record in records:
        envelope = _require_mapping(record, "session_state.formal_objects.records[]")
        if envelope.get("object_type") != object_type:
            continue
        item = _require_mapping(envelope.get("payload"), "session_state.formal_objects.records[].payload")
        if not _object_has_source_identifier(item, identifier):
            continue
        if state is not None and item.get("state") != state:
            continue
        return item
    return None


def _assert_reason_surface(value: object, label: str) -> None:
    payload = _require_mapping(value, label)
    _require_keys(payload, ["reason_code", "category", "details"], label)
    if payload["category"] not in REASON_CATEGORIES:
        raise RuntimeError(f"{label} category mismatch: {payload}")
    if payload["details"] is not None:
        _require_mapping(payload["details"], f"{label}.details")


def _assert_reason_list(value: object, label: str) -> None:
    reasons = _require_list(value, label)
    for index, reason in enumerate(reasons):
        _assert_reason_surface(reason, f"{label}[{index}]")


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


def _require_reference(item: dict[str, object], reference_type: str) -> dict[str, object]:
    references = _require_list(item.get("references"), "object.references")
    for reference in references:
        current = _require_mapping(reference, "object.references[]")
        if current.get("type") == reference_type:
            _assert_reference_surface(current, "object.references[]")
            return current
    raise RuntimeError(f"reference_type {reference_type!r} not found in {item!r}")


def _require_reason_from_object(item: dict[str, object], reason_code: str) -> dict[str, object]:
    return _require_reason_from_list(item.get("reasons"), reason_code)


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
