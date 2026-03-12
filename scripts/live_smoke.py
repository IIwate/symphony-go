#!/usr/bin/env python3
"""live_smoke 入口脚本。"""

from __future__ import annotations

import sys
from pathlib import Path


def _configure_stdio() -> None:
    for stream_name in ("stdout", "stderr"):
        stream = getattr(sys, stream_name, None)
        reconfigure = getattr(stream, "reconfigure", None)
        if callable(reconfigure):
            reconfigure(encoding="utf-8", errors="replace")


def _bootstrap() -> None:
    script_dir = Path(__file__).resolve().parent
    lib_dir = script_dir / "lib"
    if str(lib_dir) not in sys.path:
        sys.path.insert(0, str(lib_dir))


def main() -> int:
    _configure_stdio()
    _bootstrap()
    from live_smoke.cli import main as cli_main

    return cli_main()


if __name__ == "__main__":
    raise SystemExit(main())
