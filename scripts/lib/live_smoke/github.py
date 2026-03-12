"""Git / GitHub 辅助。"""

from __future__ import annotations

from dataclasses import dataclass
import json
from pathlib import Path
import time

from live_smoke.shell import run


@dataclass
class PullRequest:
    number: int
    url: str
    state: str
    head_ref_name: str
    merge_state_status: str | None = None


def ensure_gh_auth() -> None:
    run(["gh", "auth", "status"])


def git_clone(repo_url: str, target_dir: Path) -> None:
    run(["git", "clone", repo_url, str(target_dir)])


def prepare_pull_request(
    repo: str,
    repo_url: str,
    branch: str,
    title: str,
    body: str,
    work_root: Path,
) -> PullRequest:
    clone_dir = work_root / "repo"
    git_clone(repo_url, clone_dir)
    run(["git", "checkout", "-b", branch], cwd=clone_dir)
    marker = clone_dir / f"{branch.replace('/', '-')}.txt"
    marker.write_text(f"live smoke {int(time.time())}\n", encoding="utf-8")
    run(["git", "add", marker.name], cwd=clone_dir)
    run(["git", "commit", "-m", f"test: live smoke {branch}"], cwd=clone_dir)
    run(["git", "push", "-u", "origin", branch], cwd=clone_dir)
    result = run(
        [
            "gh",
            "pr",
            "create",
            "--repo",
            repo,
            "--base",
            "main",
            "--head",
            branch,
            "--title",
            title,
            "--body",
            body,
        ],
        cwd=clone_dir,
    )
    pr = view_pull_request(repo, branch=branch)
    if result.stdout.strip() and not pr.url:
        pr.url = result.stdout.strip()
    return pr


def view_pull_request(repo: str, *, number: int | None = None, branch: str | None = None) -> PullRequest:
    if number is None and not branch:
        raise ValueError("number or branch is required")
    if number is not None:
        result = run(
            ["gh", "pr", "view", str(number), "--repo", repo, "--json", "number,url,state,headRefName,mergeStateStatus"]
        )
        item = json.loads(result.stdout)
    else:
        result = run(
            [
                "gh",
                "pr",
                "list",
                "--repo",
                repo,
                "--state",
                "all",
                "--head",
                branch or "",
                "--json",
                "number,url,state,headRefName,mergeStateStatus",
            ]
        )
        payload = json.loads(result.stdout or "[]")
        if not payload:
            raise RuntimeError(f"no pull request found for branch {branch!r}")
        item = payload[0]
    return PullRequest(
        number=int(item["number"]),
        url=str(item["url"]),
        state=str(item["state"]),
        head_ref_name=str(item["headRefName"]),
        merge_state_status=item.get("mergeStateStatus"),
    )


def merge_pull_request(repo: str, number: int) -> None:
    run(["gh", "pr", "merge", str(number), "--repo", repo, "--squash", "--delete-branch"])


def close_pull_request(repo: str, number: int) -> None:
    run(["gh", "pr", "close", str(number), "--repo", repo, "--delete-branch"], check=False)
