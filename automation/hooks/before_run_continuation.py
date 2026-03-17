from __future__ import annotations

import os
from pathlib import Path
import shutil
import subprocess


def remove_path(path: Path) -> None:
    if path.is_symlink() or path.is_file():
        path.unlink()
        return
    shutil.rmtree(path)


def clone_repo(root: Path) -> None:
    repo_url = os.environ["SYMPHONY_GIT_REPO_URL"]
    for child in root.iterdir():
        remove_path(child)
    subprocess.run(["git", "clone", "--depth", "1", repo_url, "."], check=True)


def main() -> None:
    root = Path.cwd()
    if not (root / ".git").is_dir():
        clone_repo(root)
        return
    subprocess.run(["git", "status", "--short"], check=True)
    subprocess.run(["git", "fetch", "--all", "--prune"], check=False)


if __name__ == "__main__":
    main()
