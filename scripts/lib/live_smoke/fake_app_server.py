#!/usr/bin/env python3
"""用于 live smoke 的最小 app-server stub。"""

from __future__ import annotations

import json
import sys


def write_json(payload: dict[str, object]) -> None:
    sys.stdout.write(json.dumps(payload, ensure_ascii=False) + "\n")
    sys.stdout.flush()


def main() -> int:
    thread_count = 0
    turn_count = 0

    for raw_line in sys.stdin:
        raw_line = raw_line.strip()
        if not raw_line:
            continue
        try:
            message = json.loads(raw_line)
        except json.JSONDecodeError:
            continue

        method = message.get("method")
        request_id = message.get("id")

        if method == "initialize":
            write_json({"id": request_id, "result": {"ok": True}})
        elif method == "thread/start":
            thread_count += 1
            write_json({"id": request_id, "result": {"thread": {"id": f"fake-thread-{thread_count}"}}})
        elif method == "turn/start":
            turn_count += 1
            write_json({"id": request_id, "result": {"turn": {"id": f"fake-turn-{turn_count}"}}})
            write_json({"method": "turn/completed", "params": {"message": "fake app server completed turn"}})

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
