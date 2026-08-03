from __future__ import annotations

import os
import subprocess
import time
from collections.abc import Callable, Mapping
from pathlib import Path

from dorf.adapters.environments.incus import (
    IncusConfig,
    IncusDoctor,
    IncusRunnerProbe,
    command_message,
)
from dorf.runtime import JobBinding, WorkerBinding

INCUS_GUEST_WORKSPACE = "/workspace"


class UnsafeEnvironmentIdentityError(RuntimeError):
    pass


class IncusEnvironment:
    """Operate one Worker's current Incus Room."""

    environment_type = "incus-vm"
    workspace = INCUS_GUEST_WORKSPACE

    def __init__(
        self,
        config: IncusConfig | None = None,
        probe: IncusRunnerProbe | None = None,
        sleep: Callable[[float], None] = time.sleep,
    ) -> None:
        self.config = config or IncusConfig()
        self._probe = probe or IncusRunnerProbe()
        self._sleep = sleep

    def check_prerequisites(self) -> list[str]:
        result = IncusDoctor().fast_check(self.config)
        return [failure.message for failure in result.failures]

    def initial_metadata(self, worker_name: str) -> dict[str, str]:
        return {
            "template": self.config.template,
            "network": self.config.network,
            "root_disk_size": self.config.root_disk_size,
        }

    def environment_id(self, worker_name: str) -> str:
        return vm_name_for_worker(worker_name)

    def create(self, binding: WorkerBinding) -> None:
        vm_name = self.vm_name(binding)
        self._run_checked(
            [
                "incus",
                "init",
                self.config.template,
                vm_name,
                "--vm",
                "--network",
                self.config.network,
                "-d",
                f"root,size={self.config.root_disk_size}",
            ]
        )
        self._run_checked(["incus", "start", vm_name])
        if not self._wait_for_guest_agent(vm_name):
            raise RuntimeError("Incus guest agent did not become ready for exec")
        self._run_checked(["incus", "exec", vm_name, "--", "mkdir", "-p", self.workspace])

    def execute(
        self,
        binding: WorkerBinding | JobBinding,
        argv: list[str],
        *,
        cwd: str | None = None,
        env: Mapping[str, str] | None = None,
        input: str | None = None,
        timeout_seconds: float | None = None,
    ) -> subprocess.CompletedProcess[str]:
        command = self.process_command(binding, argv, cwd=cwd, env=env)
        if timeout_seconds is None:
            return self._probe.run(command, input=input)
        return self._probe.run(command, input=input, timeout_seconds=timeout_seconds)

    def attach(self, binding: WorkerBinding, *, cwd: str) -> int:
        command = [
            "incus",
            "exec",
            self.vm_name(binding),
            "--cwd",
            cwd,
            "--mode",
            "interactive",
            "--",
            "bash",
        ]
        return self._probe.attach(command).returncode

    def pull_file(
        self,
        binding: WorkerBinding | JobBinding,
        room_path: str,
        destination: Path,
        *,
        max_bytes: int,
    ) -> None:
        if not room_path.startswith("/") or ".." in Path(room_path).parts or max_bytes < 1:
            raise ValueError("Room file transfer request is invalid")
        destination.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
        if destination.exists() or destination.is_symlink():
            raise ValueError("Room file transfer destination already exists")
        stream_script = "\n".join(
            (
                "import errno, os, stat, sys",
                "try:",
                "    fd = os.open(sys.argv[1], os.O_RDONLY | os.O_NOFOLLOW)",
                "except OSError as error:",
                "    raise SystemExit(65 if error.errno == errno.ELOOP else 66)",
                "if not stat.S_ISREG(os.fstat(fd).st_mode): raise SystemExit(65)",
                "while True:",
                "    chunk = os.read(fd, 1048576)",
                "    if not chunk: break",
                "    sys.stdout.buffer.write(chunk)",
                "    sys.stdout.buffer.flush()",
            )
        )
        command = [
            "incus",
            "exec",
            self.vm_name(binding),
            "--",
            "python3",
            "-c",
            stream_script,
            room_path,
        ]
        result = self._probe.pull_file(command, destination, max_bytes=max_bytes)
        if result.returncode != 0:
            destination.unlink(missing_ok=True)
            message = command_message(result) or "Room file stream failed"
            if result.returncode == 65 or "exceeds" in message:
                raise ValueError(f"Room file was rejected: {message}")
            raise RuntimeError(f"Could not pull Room file: {message}")
        try:
            metadata = destination.lstat()
        except OSError as error:
            raise ValueError(f"Pulled Room file is unavailable: {error}") from error
        if not os.path.isfile(destination) or os.path.islink(destination):
            destination.unlink(missing_ok=True)
            raise ValueError("Room artifact must be a regular file")
        if metadata.st_size > max_bytes:
            destination.unlink(missing_ok=True)
            raise ValueError("Room artifact exceeds transfer limit")

    def process_command(
        self,
        binding: WorkerBinding | JobBinding,
        argv: list[str],
        *,
        cwd: str | None = None,
        env: Mapping[str, str] | None = None,
    ) -> list[str]:
        command = ["incus", "exec", self.vm_name(binding)]
        if cwd is not None:
            command.extend(["--cwd", cwd])
        command.append("--")
        if env:
            command.extend(["env", *[f"{name}={value}" for name, value in env.items()]])
        return [*command, *argv]

    def restore(self, binding: WorkerBinding) -> str:
        """Restore the exact recorded VM when its durable disk still exists."""
        vm_name = self.vm_name(binding)
        info = self._probe.run(["incus", "info", vm_name])
        if info.returncode != 0:
            if _incus_instance_absent(info):
                return "absent"
            raise RuntimeError(command_message(info) or f"Could not inspect Incus VM {vm_name}")
        if "status: stopped" in info.stdout.lower():
            restore = self._probe.run(["incus", "start", vm_name])
        else:
            available = self._probe.run(["incus", "exec", vm_name, "--", "true"])
            if available.returncode == 0:
                return "usable"
            restore = self._probe.run(["incus", "restart", vm_name, "--force"])
        if restore.returncode != 0:
            raise RuntimeError(command_message(restore) or f"Could not restore Incus VM {vm_name}")
        if not self._wait_for_guest_agent(vm_name):
            raise RuntimeError("Incus guest agent did not become ready during Room recovery")
        return "restored"

    def stop(self, binding: WorkerBinding) -> str:
        vm_name = self._cleanup_vm_name(binding)
        info = self._probe.run(["incus", "info", vm_name])
        if info.returncode != 0:
            if _incus_instance_absent(info):
                return "absent"
            message = command_message(info) or "incus info failed"
            raise RuntimeError(f"Could not inspect Incus VM {vm_name}: {message}")
        stop = self._probe.run(["incus", "stop", vm_name, "--force"])
        if stop.returncode != 0:
            message = command_message(stop) or "incus stop failed"
            if "already stopped" not in message.lower():
                raise RuntimeError(f"Could not stop Incus VM {vm_name}: {message}")
        return "stopped"

    def destroy(self, binding: WorkerBinding) -> str:
        vm_name = self._cleanup_vm_name(binding)
        delete = self._probe.run(["incus", "delete", vm_name, "--force"])
        if delete.returncode != 0:
            if _incus_instance_absent(delete):
                return "absent"
            message = command_message(delete) or "incus delete failed"
            raise RuntimeError(f"Could not delete Incus VM {vm_name}: {message}")
        return "deleted"

    def vm_name(self, binding: WorkerBinding | JobBinding) -> str:
        recorded = binding.environment_id
        if not recorded:
            raise UnsafeEnvironmentIdentityError(
                "recorded Room provider identity is missing; refusing Incus operation"
            )
        return recorded

    def _cleanup_vm_name(self, binding: WorkerBinding) -> str:
        expected = self.environment_id(binding.worker.name)
        recorded = binding.environment_id
        if not recorded:
            raise UnsafeEnvironmentIdentityError(
                "recorded Room provider identity is missing; refusing Incus cleanup"
            )
        if recorded != expected:
            raise UnsafeEnvironmentIdentityError(
                f"recorded Room identity does not match this Worker: {recorded} != {expected}"
            )
        return recorded

    def _run_checked(self, argv: list[str]) -> subprocess.CompletedProcess[str]:
        result = self._probe.run(argv)
        if result.returncode != 0:
            raise RuntimeError(command_message(result))
        return result

    def _wait_for_guest_agent(self, vm_name: str) -> bool:
        for attempt in range(30):
            result = self._probe.run(["incus", "exec", vm_name, "--", "true"])
            if result.returncode == 0:
                return True
            if attempt < 29:
                self._sleep(1)
        return False


def vm_name_for_worker(worker_name: str) -> str:
    return f"dorf-{worker_name}"


def _incus_instance_absent(result: subprocess.CompletedProcess[str]) -> bool:
    message = command_message(result).lower()
    return "instance not found" in message or "instance does not exist" in message
