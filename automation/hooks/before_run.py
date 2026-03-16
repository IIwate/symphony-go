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


def main() -> None:
    repo_url = os.environ["SYMPHONY_GIT_REPO_URL"]
    root = Path.cwd()
    for child in root.iterdir():
        remove_path(child)
    subprocess.run(["git", "clone", "--depth", "1", repo_url, "."], check=True)


if __name__ == "__main__":
    main()
