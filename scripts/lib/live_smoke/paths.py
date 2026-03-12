"""路径辅助函数。"""

from __future__ import annotations

import os
from pathlib import Path


def repo_root() -> Path:
    return Path(__file__).resolve().parents[3]


def temp_root() -> Path:
    return repo_root() / ".codex-tmp" / "live-smoke"


def to_bash_path(path: Path) -> str:
    resolved = path.resolve()
    text = str(resolved)
    if os.name != "nt":
        return text
    drive, tail = os.path.splitdrive(text)
    drive_letter = drive.rstrip(":").lower()
    tail = tail.replace("\\", "/")
    return f"/{drive_letter}{tail}"


def bash_single_quote(value: str) -> str:
    return "'" + value.replace("'", "'\"'\"'") + "'"
