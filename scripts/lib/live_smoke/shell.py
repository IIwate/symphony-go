"""子进程与命令辅助。"""

from __future__ import annotations

from dataclasses import dataclass
import os
import shutil
import signal
import subprocess
import threading
from pathlib import Path


@dataclass
class CommandResult:
    stdout: str
    stderr: str
    returncode: int


class CommandError(RuntimeError):
    def __init__(self, command: list[str], result: CommandResult):
        rendered = " ".join(command)
        message = result.stderr.strip() or result.stdout.strip() or f"command failed with exit code {result.returncode}"
        super().__init__(f"{rendered}: {message}")
        self.command = command
        self.result = result


def require_command(name: str) -> None:
    if shutil.which(name) is None:
        raise RuntimeError(f"missing required command: {name}")


def run(
    command: list[str],
    *,
    cwd: Path | None = None,
    env: dict[str, str] | None = None,
    check: bool = True,
) -> CommandResult:
    completed = subprocess.run(
        command,
        cwd=str(cwd) if cwd is not None else None,
        env=env,
        text=True,
        encoding="utf-8",
        errors="replace",
        capture_output=True,
        check=False,
    )
    result = CommandResult(
        stdout=completed.stdout,
        stderr=completed.stderr,
        returncode=completed.returncode,
    )
    if check and completed.returncode != 0:
        raise CommandError(command, result)
    return result


class ManagedProcess:
    def __init__(self, command: list[str], *, cwd: Path | None = None, env: dict[str, str] | None = None, echo: bool = True):
        creationflags = 0
        if os.name == "nt":
            creationflags = subprocess.CREATE_NEW_PROCESS_GROUP
        self._proc = subprocess.Popen(
            command,
            cwd=str(cwd) if cwd is not None else None,
            env=env,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
            encoding="utf-8",
            errors="replace",
            bufsize=1,
            creationflags=creationflags,
        )
        self._echo = echo
        self._lines: list[str] = []
        self._reader = threading.Thread(target=self._read_output, daemon=True)
        self._reader.start()

    def _read_output(self) -> None:
        if self._proc.stdout is None:
            return
        for line in self._proc.stdout:
            text = line.rstrip("\n")
            self._lines.append(text)
            if self._echo:
                print(text)

    def is_running(self) -> bool:
        return self._proc.poll() is None

    def returncode(self) -> int | None:
        return self._proc.poll()

    def tail(self, size: int = 80) -> str:
        return "\n".join(self._lines[-size:])

    def stop(self, timeout_seconds: float = 10.0) -> None:
        if not self.is_running():
            return
        try:
            if os.name == "nt":
                self._proc.send_signal(signal.CTRL_BREAK_EVENT)
            else:
                self._proc.send_signal(signal.SIGINT)
            self._proc.wait(timeout=timeout_seconds)
        except Exception:
            self._proc.kill()
            self._proc.wait(timeout=timeout_seconds)

    def ensure_running(self) -> None:
        if self.is_running():
            return
        raise RuntimeError(f"process exited unexpectedly: {self.returncode()}\n{self.tail()}")
