from __future__ import annotations

import ipaddress
import json
import os
import secrets
import signal
import subprocess
import sys
import threading
import time
from collections import deque
from collections.abc import Callable, Iterator
from contextlib import contextmanager
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Protocol

from dorf import __version__
from dorf.runtime import (
    AgentConversationInspection,
    AgentDriverError,
    AgentTurnRecovery,
    AgentUnavailableError,
    ConversationMissingError,
    JobBinding,
    WorkerAgentTurn,
    WorkerBinding,
    WorkerTurnOutcome,
)
from dorf.runtime.reporting import (
    assignment_reporting_instructions,
    job_report_root,
)

APP_SERVER_PORT = 4500
APP_SERVER_TOKEN_PATH = "/tmp/dorf/codex-app-server.token"
APP_SERVER_START_TIMEOUT_SECONDS = 15
APP_SERVER_MAX_MESSAGE_BYTES = 64 * 1024 * 1024
CLIENT_VERSION = __version__
_PRIVATE_IPV4_NETWORKS = tuple(
    ipaddress.ip_network(value) for value in ("10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16")
)


class AgentProcessEnvironment(Protocol):
    def execute(
        self,
        binding: WorkerBinding | JobBinding,
        argv: list[str],
        *,
        cwd: str | None = None,
        env: dict[str, str] | None = None,
        input: str | None = None,
    ): ...

    def process_command(
        self,
        binding: WorkerBinding | JobBinding,
        argv: list[str],
        *,
        cwd: str | None = None,
        env: dict[str, str] | None = None,
    ) -> list[str]: ...


class AppServerConnection(Protocol):
    def send(self, message: str) -> None: ...

    def recv(self, timeout: float | None = None) -> str | bytes: ...

    def close(self) -> None: ...


class _SynchronizedBinaryWriter:
    def __init__(self, output) -> None:
        self._output = output
        self._lock = threading.Lock()

    def write(self, data: bytes) -> int:
        with self._lock:
            return self._output.write(data)


class CodexDriverError(AgentDriverError):
    exit_code = 126
    category = "driver"

    def diagnostic(self) -> str:
        return f"Codex app-server {self.category} failure: {self}"


class CodexStartupError(CodexDriverError):
    category = "startup"


class CodexAuthenticationError(CodexDriverError):
    exit_code = 77
    category = "authentication"


class CodexTransportError(CodexDriverError):
    exit_code = 69
    category = "transport"


class CodexProtocolError(CodexDriverError):
    exit_code = 65
    category = "protocol"


class CodexRequestError(CodexProtocolError):
    def __init__(self, method: str, native_error: Any) -> None:
        self.method = method
        self.native_error = native_error
        native = json.dumps(native_error, sort_keys=True, ensure_ascii=False)
        super().__init__(f"{method} request failed: {native}")


class CodexThreadNotFound(ConversationMissingError, AgentDriverError):
    """The recorded native conversation is no longer available to Codex."""

    category = "thread-missing"


class CodexTurnFailed(CodexDriverError):
    exit_code = 1
    category = "turn"

    def __init__(self, native_error: dict[str, Any]) -> None:
        self.native_error = native_error
        message = native_error.get("message")
        details = native_error.get("additionalDetails")
        parts = [value for value in (message, details) if isinstance(value, str) and value]
        if not parts:
            parts = [json.dumps(native_error, sort_keys=True)]
        super().__init__(": ".join(parts))


class CodexThreadActive(CodexDriverError):
    exit_code = 75
    category = "active-turn"


@dataclass(frozen=True)
class CodexTurnOutcome:
    thread_id: str
    turn_id: str
    status: str


class CodexAppServerProtocol:
    """The version-sensitive subset of Codex app-server used by the first turn."""

    def __init__(
        self,
        connection: AppServerConnection,
        *,
        timeout_seconds: float | None = None,
    ) -> None:
        if timeout_seconds is not None and timeout_seconds <= 0:
            raise ValueError("app-server protocol timeout must be positive")
        self._connection = connection
        self._pending_notifications: deque[dict[str, Any]] = deque()
        self._deadline = (
            time.monotonic() + timeout_seconds if timeout_seconds is not None else None
        )

    def run_initial_turn(
        self,
        *,
        prompt: str,
        workspace: str,
        model: str,
        reasoning_effort: str,
        conversation_started: Callable[[str], None],
        developer_instructions: str | None = None,
        turn_prepared: Callable[[str | None], None] | None = None,
        turn_started: Callable[[str], None] | None = None,
    ) -> CodexTurnOutcome:
        self._initialize()
        thread_params = {
            "cwd": workspace,
            "model": model,
            "approvalPolicy": "never",
            "sandbox": "danger-full-access",
        }
        if developer_instructions is not None:
            thread_params["developerInstructions"] = developer_instructions
        self._send(
            {
                "method": "thread/start",
                "id": 1,
                "params": thread_params,
            }
        )
        thread_result = self._response(1, "thread/start")
        thread = thread_result.get("thread")
        thread_id = thread.get("id") if isinstance(thread, dict) else None
        if not isinstance(thread_id, str) or not thread_id:
            raise CodexProtocolError("thread/start response is missing result.thread.id")
        conversation_started(thread_id)
        if turn_prepared is not None:
            turn_prepared(None)

        self._send(
            {
                "method": "turn/start",
                "id": 2,
                "params": {
                    "threadId": thread_id,
                    "input": [{"type": "text", "text": prompt}],
                    "cwd": workspace,
                    "model": model,
                    "effort": reasoning_effort,
                    "approvalPolicy": "never",
                    "sandboxPolicy": {"type": "dangerFullAccess"},
                },
            }
        )
        turn_result = self._response(2, "turn/start")
        turn = turn_result.get("turn")
        turn_id = turn.get("id") if isinstance(turn, dict) else None
        if not isinstance(turn_id, str) or not turn_id:
            raise CodexProtocolError("turn/start response is missing result.turn.id")
        if turn_started is not None:
            turn_started(turn_id)

        return self._observe_turn(thread_id, turn_id)

    def run_turn(
        self,
        *,
        thread_id: str,
        prompt: str,
        workspace: str,
        model: str,
        reasoning_effort: str,
        turn_prepared: Callable[[str | None], None],
        turn_started: Callable[[str], None],
    ) -> CodexTurnOutcome:
        """Submit one turn to an existing exact native thread."""
        self._initialize()
        thread = self._read_thread(thread_id, request_id=1)
        status = thread.get("status")
        status_type = status.get("type") if isinstance(status, dict) else None
        if status_type == "active":
            raise CodexThreadActive(f"Codex thread {thread_id} already has an active turn")
        if status_type not in {"notLoaded", "idle", "systemError"}:
            raise CodexProtocolError(f"thread/read returned an unknown thread status: {status!r}")
        request_id = 2
        if status_type != "idle":
            thread = self._resume_thread(thread_id, request_id=2, read_request_id=3)
            request_id = 4
            resumed_status = thread.get("status")
            resumed_status_type = (
                resumed_status.get("type") if isinstance(resumed_status, dict) else None
            )
            if resumed_status_type == "active":
                raise CodexThreadActive(f"Codex thread {thread_id} already has an active turn")
            if resumed_status_type != "idle":
                raise CodexProtocolError(
                    f"thread/resume did not make the bound thread idle: {resumed_status!r}"
                )
        turns = thread["turns"]
        baseline = turns[-1].get("id") if turns and isinstance(turns[-1], dict) else None
        if baseline is not None and not isinstance(baseline, str):
            raise CodexProtocolError("thread/read returned a turn with an invalid id")
        turn_prepared(baseline)
        self._send(
            {
                "method": "turn/start",
                "id": request_id,
                "params": {
                    "threadId": thread_id,
                    "input": [{"type": "text", "text": prompt}],
                    "cwd": workspace,
                    "model": model,
                    "effort": reasoning_effort,
                    "approvalPolicy": "never",
                    "sandboxPolicy": {"type": "dangerFullAccess"},
                },
            }
        )
        turn_result = self._response(request_id, "turn/start")
        turn = turn_result.get("turn")
        native_turn_id = turn.get("id") if isinstance(turn, dict) else None
        if not isinstance(native_turn_id, str) or not native_turn_id:
            raise CodexProtocolError("turn/start response is missing result.turn.id")
        turn_started(native_turn_id)
        return self._observe_turn(thread_id, native_turn_id)

    def interrupt_turn(self, thread_id: str, turn_id: str) -> CodexTurnOutcome:
        """Explicitly interrupt one exact native turn and observe its terminal state."""
        self._initialize()
        self._send(
            {
                "method": "turn/interrupt",
                "id": 1,
                "params": {"threadId": thread_id, "turnId": turn_id},
            }
        )
        self._response(1, "turn/interrupt")
        return self._observe_turn(thread_id, turn_id)

    def _observe_turn(self, thread_id: str, turn_id: str) -> CodexTurnOutcome:
        while True:
            message = self._next_notification()
            method = message.get("method")
            if not isinstance(method, str):
                native = _bounded_native_message(message)
                if "id" in message:
                    raise CodexProtocolError(
                        f"unexpected terminal response id {message.get('id')!r}: {native}"
                    )
                raise CodexProtocolError(f"terminal message without id or method: {native}")
            if method == "error":
                params = message.get("params")
                if not isinstance(params, dict):
                    raise CodexProtocolError("error notification is missing params")
                if params.get("threadId") != thread_id:
                    raise CodexProtocolError("error notification references an unexpected thread")
                if params.get("turnId") != turn_id:
                    raise CodexProtocolError("error notification references an unexpected turn")
                will_retry = params.get("willRetry")
                if not isinstance(will_retry, bool):
                    raise CodexProtocolError("error notification is missing params.willRetry")
                native_error = params.get("error")
                if not isinstance(native_error, dict):
                    raise CodexProtocolError("error notification is missing params.error")
                if not will_retry:
                    raise CodexTurnFailed(native_error)
            if method != "turn/completed":
                continue
            params = message.get("params")
            if not isinstance(params, dict):
                raise CodexProtocolError("turn/completed notification is missing params")
            if params.get("threadId") != thread_id:
                raise CodexProtocolError(
                    "turn/completed notification references an unexpected thread"
                )
            completed_turn = params.get("turn")
            if not isinstance(completed_turn, dict):
                raise CodexProtocolError("turn/completed notification is missing params.turn")
            completed_turn_id = completed_turn.get("id")
            if completed_turn_id != turn_id:
                raise CodexProtocolError(
                    "turn/completed notification references an unexpected turn"
                )
            status = completed_turn.get("status")
            if status == "failed":
                native_error = completed_turn.get("error")
                if isinstance(native_error, dict):
                    raise CodexTurnFailed(native_error)
                raise CodexTurnFailed({"message": "Codex turn failed"})
            if status not in {"completed", "interrupted"}:
                raise CodexProtocolError(
                    f"turn/completed notification has unknown status: {status!r}"
                )
            return CodexTurnOutcome(thread_id, turn_id, status)

    def inspect_thread(self, thread_id: str) -> dict[str, Any]:
        """Resume one exact native thread and return Codex-owned history and status."""
        self._initialize()
        thread = self._read_thread(thread_id, request_id=1)
        status = thread.get("status")
        status_type = status.get("type") if isinstance(status, dict) else None
        if status_type == "active":
            return thread
        if status_type not in {"notLoaded", "idle", "systemError"}:
            raise CodexProtocolError(f"thread/read returned an unknown thread status: {status!r}")
        return self._resume_thread(thread_id, request_id=2, read_request_id=3)

    def _resume_thread(
        self, thread_id: str, *, request_id: int, read_request_id: int
    ) -> dict[str, Any]:
        self._send(
            {
                "method": "thread/resume",
                "id": request_id,
                "params": {"threadId": thread_id},
            }
        )
        try:
            resumed = self._response(request_id, "thread/resume")
        except CodexRequestError as error:
            self._raise_thread_request(error, thread_id)
        resumed_thread = resumed.get("thread")
        resumed_id = resumed_thread.get("id") if isinstance(resumed_thread, dict) else None
        if resumed_id != thread_id:
            raise CodexProtocolError("thread/resume response does not contain the requested thread")
        return self._read_thread(thread_id, request_id=read_request_id)

    def _initialize(self) -> None:
        self._send(
            {
                "method": "initialize",
                "id": 0,
                "params": {
                    "clientInfo": {
                        "name": "dorf",
                        "title": "Dorf",
                        "version": CLIENT_VERSION,
                    }
                },
            }
        )
        self._response(0, "initialize")
        self._send({"method": "initialized", "params": {}})

    def _read_thread(self, thread_id: str, *, request_id: int) -> dict[str, Any]:
        self._send(
            {
                "method": "thread/read",
                "id": request_id,
                "params": {"threadId": thread_id, "includeTurns": True},
            }
        )
        try:
            result = self._response(request_id, "thread/read")
        except CodexRequestError as error:
            self._raise_thread_request(error, thread_id)
        thread = result.get("thread")
        if not isinstance(thread, dict) or thread.get("id") != thread_id:
            raise CodexProtocolError("thread/read response does not contain the requested thread")
        if not isinstance(thread.get("turns"), list):
            raise CodexProtocolError("thread/read response is missing native turn history")
        return thread

    def _raise_thread_request(self, error: CodexRequestError, thread_id: str) -> None:
        native = json.dumps(error.native_error, sort_keys=True).lower()
        if "not found" in native or "does not exist" in native or "missing" in native:
            raise CodexThreadNotFound(f"Codex thread {thread_id} is missing") from error
        raise error

    def _send(self, message: dict[str, Any]) -> None:
        self._connection.send(json.dumps(message, separators=(",", ":")))

    def _response(self, request_id: int, method: str) -> dict[str, Any]:
        while True:
            message = self._receive()
            if message.get("id") != request_id:
                if isinstance(message.get("method"), str):
                    self._pending_notifications.append(message)
                elif "id" in message:
                    raise CodexProtocolError(
                        f"unexpected response id {message.get('id')!r} "
                        f"while waiting for {request_id}"
                    )
                else:
                    native = _bounded_native_message(message)
                    raise CodexProtocolError(
                        f"app-server message without id or method while waiting for "
                        f"{method}: {native}"
                    )
                continue
            error = message.get("error")
            if error is not None:
                raise CodexRequestError(method, error)
            result = message.get("result")
            if not isinstance(result, dict):
                raise CodexProtocolError(f"{method} response is missing result")
            return result

    def _next_notification(self) -> dict[str, Any]:
        if self._pending_notifications:
            return self._pending_notifications.popleft()
        return self._receive()

    def _receive(self) -> dict[str, Any]:
        timeout = None
        if self._deadline is not None:
            timeout = self._deadline - time.monotonic()
            if timeout <= 0:
                raise CodexTransportError("timed out waiting for app-server turn completion")
        raw = self._connection.recv() if timeout is None else self._connection.recv(timeout=timeout)
        if isinstance(raw, bytes):
            try:
                raw = raw.decode()
            except UnicodeDecodeError as error:
                raise CodexProtocolError(
                    f"received a non-UTF-8 app-server message: {raw[:200]!r}"
                ) from error
        if not isinstance(raw, str):
            raise CodexProtocolError(f"received a non-text app-server message: {raw!r}")
        try:
            message = json.loads(raw)
        except json.JSONDecodeError as error:
            raise CodexProtocolError(
                f"received malformed app-server message: {raw[:1000]}"
            ) from error
        if not isinstance(message, dict):
            raise CodexProtocolError(f"received non-object app-server message: {raw[:1000]}")
        return message


def _bounded_native_message(message: dict[str, Any]) -> str:
    return json.dumps(message, sort_keys=True, ensure_ascii=False)[:1000]


def _default_connector(endpoint: str, **kwargs):
    from websockets.sync.client import connect

    return connect(endpoint, **kwargs)


def _http_status_code(error: Exception) -> int | None:
    direct = getattr(error, "status_code", None)
    if isinstance(direct, int):
        return direct
    response = getattr(error, "response", None)
    response_status = getattr(response, "status_code", None)
    return response_status if isinstance(response_status, int) else None


class _CodexTransportConnection:
    """Translate connection I/O failures without exposing transport details."""

    def __init__(self, connection: AppServerConnection) -> None:
        self._connection = connection

    def send(self, message: str) -> None:
        try:
            self._connection.send(message)
        except Exception as error:
            method = None
            try:
                envelope = json.loads(message)
                method = envelope.get("method") if isinstance(envelope, dict) else None
            except (json.JSONDecodeError, TypeError):
                pass
            detail = f" {method}" if isinstance(method, str) else ""
            raise CodexTransportError(f"could not send{detail}: {error}") from error

    def recv(self, timeout: float | None = None) -> str | bytes:
        try:
            return (
                self._connection.recv()
                if timeout is None
                else self._connection.recv(timeout=timeout)
            )
        except TimeoutError as error:
            raise CodexTransportError(
                "timed out waiting for app-server turn completion"
            ) from error
        except Exception as error:
            raise CodexTransportError(f"disconnected before turn completion: {error}") from error

    def close(self) -> None:
        self._connection.close()


@dataclass(frozen=True)
class _CodexAppServerClient:
    """Authenticate and exchange protocol messages with one app-server endpoint."""

    endpoint: str
    token: str = field(repr=False)
    connector: Callable[..., AppServerConnection] | None = field(
        default=None, repr=False, compare=False
    )

    @contextmanager
    def connect_existing(self) -> Iterator[CodexAppServerProtocol]:
        connection = self._connect()
        try:
            yield CodexAppServerProtocol(connection)
        finally:
            self._close(connection)

    @contextmanager
    def launch_and_connect(
        self,
        command: list[str],
        *,
        diagnostic_output=None,
        timeout_seconds: float | None = None,
    ) -> Iterator[CodexAppServerProtocol]:
        try:
            process = subprocess.Popen(
                command,
                stdin=subprocess.DEVNULL,
                stdout=(subprocess.PIPE if diagnostic_output is not None else subprocess.DEVNULL),
                stderr=subprocess.STDOUT,
                start_new_session=True,
            )
        except OSError as error:
            raise CodexStartupError(f"could not launch app-server: {error}") from error

        output_thread = (
            threading.Thread(
                target=_copy_app_server_output,
                args=(process.stdout, diagnostic_output),
                daemon=True,
            )
            if diagnostic_output is not None
            else None
        )
        if output_thread is not None:
            output_thread.start()
        connection = None
        try:
            connection = self._await_connection(process)
            yield CodexAppServerProtocol(connection, timeout_seconds=timeout_seconds)
        finally:
            if connection is not None:
                self._close(connection)
            _terminate_process_group(process)
            if output_thread is not None:
                output_thread.join(timeout=1)

    @staticmethod
    def _close(connection: AppServerConnection) -> None:
        try:
            connection.close()
        except Exception:
            pass

    def _connect(self) -> AppServerConnection:
        connector = self.connector or _default_connector
        try:
            return _CodexTransportConnection(
                connector(
                    self.endpoint,
                    additional_headers={"Authorization": f"Bearer {self.token}"},
                    proxy=None,
                    open_timeout=2,
                    close_timeout=2,
                    max_size=APP_SERVER_MAX_MESSAGE_BYTES,
                )
            )
        except Exception as error:
            status_code = _http_status_code(error)
            if status_code in {401, 403}:
                raise CodexAuthenticationError(str(error)) from error
            raise CodexTransportError(f"could not connect to {self.endpoint}: {error}") from error

    def _await_connection(self, process: subprocess.Popen) -> AppServerConnection:
        deadline = time.monotonic() + APP_SERVER_START_TIMEOUT_SECONDS
        last_error = None
        while time.monotonic() < deadline:
            exit_code = process.poll()
            if exit_code is not None:
                process.wait()
                raise CodexStartupError(
                    f"app-server exited before accepting connections (exit code {exit_code})"
                )
            try:
                return self._connect()
            except CodexAuthenticationError:
                raise
            except CodexTransportError as error:
                last_error = error
                time.sleep(0.1)
        detail = f": {last_error}" if last_error is not None else ""
        raise CodexTransportError(f"app-server did not accept a connection in time{detail}")


@dataclass(frozen=True)
class _CodexAppServerLaunch:
    client: _CodexAppServerClient
    command: list[str]
    guest_token_path: str = APP_SERVER_TOKEN_PATH

    def write_handoff(
        self,
        launch_path: Path,
        token_path: Path,
        payload: dict[str, Any],
    ) -> None:
        try:
            _write_private_file(token_path, self.client.token + "\n")
            _write_private_file(
                launch_path,
                json.dumps(
                    {
                        **payload,
                        "app_server_command": self.command,
                        "endpoint": self.client.endpoint,
                        "token_path": str(token_path),
                    },
                    sort_keys=True,
                ),
            )
        except BaseException:
            self.cleanup_handoff(launch_path, token_path)
            raise

    @staticmethod
    def cleanup_handoff(launch_path: Path, token_path: Path) -> None:
        launch_path.unlink(missing_ok=True)
        token_path.unlink(missing_ok=True)


class _CodexAppServerSupervisor:
    """Room-bound endpoint, authentication, reuse, and reconnect policy."""

    def __init__(self, environment: AgentProcessEnvironment) -> None:
        self._environment = environment

    def check_ready(self, binding: WorkerBinding | JobBinding) -> None:
        """Prove the Room can launch an authenticated Codex app-server."""
        self._endpoint(binding)

    def prepare_launch(
        self,
        binding: WorkerBinding | JobBinding,
        *,
        workspace: str | None = None,
        process_env: dict[str, str] | None = None,
    ) -> _CodexAppServerLaunch:
        endpoint = self._launch_endpoint(self._endpoint(binding), process_env=process_env)
        return self._prepare_launch_for_endpoint(
            binding,
            endpoint,
            workspace=workspace,
            process_env=process_env,
        )

    def run_initial_turn(
        self,
        binding: WorkerBinding | JobBinding,
        turn: WorkerAgentTurn,
        *,
        conversation_started: Callable[[str], None],
        turn_prepared: Callable[[str | None], None],
        turn_started: Callable[[str], None],
        developer_instructions: str | None,
        process_env: dict[str, str],
        timeout_seconds: float | None = None,
    ) -> CodexTurnOutcome:
        launch = self.prepare_launch(
            binding,
            workspace=binding.workspace,
            process_env=process_env,
        )
        turn.output_path.parent.mkdir(parents=True, exist_ok=True)
        token_path = turn.output_path.parent / "app-server.token"
        launch_path = turn.output_path.parent / "app-server-launch.json"
        try:
            launch.write_handoff(launch_path, token_path, {})
            with turn.output_path.open("ab", buffering=0) as raw_output:
                output = _SynchronizedBinaryWriter(raw_output)
                _append_diagnostic(
                    output,
                    f"Starting Codex Worker conversation at {launch.client.endpoint}",
                )
                with launch.client.launch_and_connect(
                    launch.command,
                    diagnostic_output=output,
                    **(
                        {"timeout_seconds": timeout_seconds}
                        if timeout_seconds is not None
                        else {}
                    ),
                ) as protocol:
                    outcome = protocol.run_initial_turn(
                        prompt=turn.prompt,
                        workspace=binding.workspace,
                        model=turn.model,
                        reasoning_effort=turn.reasoning_effort,
                        conversation_started=conversation_started,
                        developer_instructions=developer_instructions,
                        turn_prepared=turn_prepared,
                        turn_started=turn_started,
                    )
                _append_diagnostic(
                    output,
                    f"Codex turn {outcome.turn_id} {outcome.status}",
                )
                return outcome
        finally:
            launch.cleanup_handoff(launch_path, token_path)
            self._cleanup_guest_token(binding, launch)

    def run_turn(
        self,
        binding: WorkerBinding | JobBinding,
        turn: WorkerAgentTurn,
        *,
        thread_id: str,
        workspace: str,
        model: str,
        reasoning_effort: str,
        turn_prepared: Callable[[str | None], None],
        turn_started: Callable[[str], None],
        process_env: dict[str, str] | None = None,
    ) -> CodexTurnOutcome:
        launch = self.prepare_launch(
            binding,
            workspace=workspace,
            process_env=process_env,
        )
        turn.output_path.parent.mkdir(parents=True, exist_ok=True)
        token_path = turn.output_path.parent / "app-server.token"
        launch_path = turn.output_path.parent / "app-server-launch.json"
        try:
            launch.write_handoff(launch_path, token_path, {})
            with turn.output_path.open("ab", buffering=0) as raw_output:
                output = _SynchronizedBinaryWriter(raw_output)
                _append_diagnostic(
                    output,
                    f"Continuing Codex thread {thread_id} at {launch.client.endpoint}",
                )
                with launch.client.launch_and_connect(
                    launch.command, diagnostic_output=output
                ) as protocol:
                    outcome = protocol.run_turn(
                        thread_id=thread_id,
                        prompt=turn.prompt,
                        workspace=workspace,
                        model=model,
                        reasoning_effort=reasoning_effort,
                        turn_prepared=turn_prepared,
                        turn_started=turn_started,
                    )
                _append_diagnostic(
                    output,
                    f"Codex turn {outcome.turn_id} {outcome.status}",
                )
                return outcome
        finally:
            launch.cleanup_handoff(launch_path, token_path)
            self._cleanup_guest_token(binding, launch)

    def interrupt(
        self,
        binding: WorkerBinding | JobBinding,
        *,
        thread_id: str,
        turn_id: str,
        process_env: dict[str, str],
    ) -> CodexTurnOutcome:
        launch = self.prepare_launch(
            binding,
            workspace=binding.workspace,
            process_env=process_env,
        )
        try:
            with launch.client.launch_and_connect(launch.command) as protocol:
                return protocol.interrupt_turn(thread_id, turn_id)
        finally:
            self._cleanup_guest_token(binding, launch)

    def inspect(
        self,
        binding: WorkerBinding | JobBinding,
        runs: list,
        thread_id: str,
        *,
        process_env: dict[str, str] | None = None,
    ) -> AgentConversationInspection:
        try:
            endpoint = self._endpoint(binding)
        except Exception as error:
            raise AgentUnavailableError(str(error)) from error

        worker_process = bool(process_env and process_env.get("DORF_WORKER_NAME"))
        live = self._load_live(
            runs,
            expected_endpoint=None if worker_process else endpoint,
            expected_prefix=(f"{endpoint.rsplit(':', 1)[0]}:" if worker_process else None),
        )
        if live is not None:
            try:
                with live.connect_existing() as protocol:
                    native = protocol.inspect_thread(thread_id)
                return AgentConversationInspection("connected", native)
            except (CodexTransportError, CodexAuthenticationError):
                pass
            except CodexProtocolError as error:
                raise AgentUnavailableError(
                    f"live Codex app-server protocol failure: {error}"
                ) from error

        launch = None
        try:
            launch = self._prepare_launch_for_endpoint(
                binding,
                self._launch_endpoint(endpoint, process_env=process_env),
                workspace=binding.workspace,
                process_env=process_env,
            )
            with launch.client.launch_and_connect(launch.command) as protocol:
                native = protocol.inspect_thread(thread_id)
        except ConversationMissingError:
            raise
        except Exception as error:
            raise AgentUnavailableError(
                f"could not reconnect to Codex app-server: {error}"
            ) from error
        finally:
            if launch is not None:
                self._cleanup_guest_token(binding, launch)
        return AgentConversationInspection("restarted", native)

    def _endpoint(self, binding: WorkerBinding | JobBinding) -> str:
        self._check_app_server(binding)
        self._check_authentication(binding)
        return f"ws://{self._private_ipv4(binding)}:{APP_SERVER_PORT}"

    @staticmethod
    def _launch_endpoint(endpoint: str, *, process_env: dict[str, str] | None) -> str:
        if not process_env or not process_env.get("DORF_WORKER_NAME"):
            return endpoint
        host = endpoint.rsplit(":", 1)[0]
        return f"{host}:{46000 + secrets.randbelow(19000)}"

    def _prepare_launch_for_endpoint(
        self,
        binding: WorkerBinding | JobBinding,
        endpoint: str,
        *,
        workspace: str | None = None,
        process_env: dict[str, str] | None = None,
    ) -> _CodexAppServerLaunch:
        token = secrets.token_urlsafe(32)
        guest_token_path = (
            f"/tmp/dorf/codex-app-server-{secrets.token_hex(8)}.token"
            if process_env and process_env.get("DORF_WORKER_NAME")
            else APP_SERVER_TOKEN_PATH
        )
        self._write_guest_token(binding, token, guest_token_path)
        return _CodexAppServerLaunch(
            _CodexAppServerClient(endpoint, token),
            self._app_server_command(
                binding,
                endpoint,
                workspace=workspace,
                process_env=process_env,
                guest_token_path=guest_token_path,
            ),
            guest_token_path,
        )

    def _load_live(
        self,
        runs: list,
        *,
        expected_endpoint: str | None,
        expected_prefix: str | None = None,
    ) -> _CodexAppServerClient | None:
        for run in runs:
            if run.status != "running":
                continue
            turn_key = getattr(run, "message_id", getattr(run, "input_id", None))
            if turn_key is None:
                continue
            if not run.output_path:
                continue
            launch_path = Path(run.output_path).parent / "app-server-launch.json"
            try:
                launch = json.loads(launch_path.read_text())
                endpoint = launch.get("endpoint")
                token_path_value = launch.get("token_path")
                if not isinstance(endpoint, str) or not isinstance(token_path_value, str):
                    continue
                if expected_endpoint is not None and endpoint != expected_endpoint:
                    continue
                if expected_prefix is not None and not endpoint.startswith(expected_prefix):
                    continue
                token = Path(token_path_value).read_text().strip()
            except (OSError, ValueError, TypeError):
                continue
            if token:
                return _CodexAppServerClient(endpoint, token)
        return None

    def _app_server_command(
        self,
        binding: WorkerBinding | JobBinding,
        endpoint: str,
        *,
        workspace: str | None = None,
        process_env: dict[str, str] | None = None,
        guest_token_path: str = APP_SERVER_TOKEN_PATH,
    ) -> list[str]:
        resolved_workspace = workspace or binding.workspace
        resolved_env = process_env or {"DORF_WORKSPACE": resolved_workspace}
        return self._environment.process_command(
            binding,
            [
                "codex",
                "app-server",
                "--listen",
                endpoint,
                "--ws-auth",
                "capability-token",
                "--ws-token-file",
                guest_token_path,
            ],
            cwd=resolved_workspace,
            env=resolved_env,
        )

    def _check_app_server(self, binding: WorkerBinding | JobBinding) -> None:
        command = [
            "bash",
            "-lc",
            "command -v codex >/dev/null && codex app-server --help >/dev/null",
        ]
        result = self._environment.execute(binding, command)
        if result.returncode != 0:
            diagnostic = _command_message(result)
            raise RuntimeError(
                "Codex app-server is unavailable in the Worker Room; configure "
                "incus.template with a Codex-capable image"
                + (f": {diagnostic}" if diagnostic else "")
            )

    def _check_authentication(self, binding: WorkerBinding | JobBinding) -> None:
        route_probe = getattr(self._environment, "check_codex_authentication", None)
        if callable(route_probe):
            route_probe(binding)
            return
        result = self._environment.execute(binding, ["codex", "login", "status"])
        if result.returncode != 0:
            diagnostic = _command_message(result) or "Codex is not logged in"
            raise RuntimeError(f"Codex authentication is unavailable: {diagnostic}")

    def _private_ipv4(self, binding: WorkerBinding | JobBinding) -> str:
        result = self._environment.execute(binding, ["ip", "-4", "route", "get", "1.1.1.1"])
        if result.returncode == 0:
            route = result.stdout.split()
            source_values = [
                route[index + 1] for index, value in enumerate(route[:-1]) if value == "src"
            ]
            for value in source_values:
                try:
                    address = ipaddress.ip_address(value)
                except ValueError:
                    continue
                if isinstance(address, ipaddress.IPv4Address) and any(
                    address in network for network in _PRIVATE_IPV4_NETWORKS
                ):
                    return str(address)
        diagnostic = _command_message(result)
        suffix = f": {diagnostic}" if diagnostic else ""
        raise RuntimeError(
            "Worker Room default route did not report an RFC1918 private IPv4 address" + suffix
        )

    def _write_guest_token(
        self, binding: WorkerBinding | JobBinding, token: str, guest_token_path: str
    ) -> None:
        result = self._environment.execute(
            binding,
            [
                "bash",
                "-lc",
                f"umask 077; mkdir -p /tmp/dorf; cat > {guest_token_path}",
            ],
            input=token + "\n",
        )
        if result.returncode != 0:
            diagnostic = _command_message(result) or "could not write capability token"
            raise RuntimeError(f"Could not prepare Codex app-server authentication: {diagnostic}")

    def _cleanup_guest_token(
        self, binding: WorkerBinding | JobBinding, launch: _CodexAppServerLaunch
    ) -> None:
        if launch.guest_token_path == APP_SERVER_TOKEN_PATH:
            return
        try:
            self._environment.execute(binding, ["rm", "-f", launch.guest_token_path])
        except Exception:
            pass


class CodexDriver:
    agent_type = "codex"

    def __init__(self, environment: AgentProcessEnvironment) -> None:
        self._environment = environment
        self._app_server = _CodexAppServerSupervisor(environment)

    def prepare(self, binding: WorkerBinding | JobBinding) -> None:
        self._app_server.check_ready(binding)

    def start_conversation(
        self,
        binding: WorkerBinding,
        turn: WorkerAgentTurn,
        *,
        conversation_started: Callable[[str], None],
        turn_prepared: Callable[[str | None], None],
        turn_started: Callable[[str], None],
        timeout_seconds: float | None = None,
    ) -> WorkerTurnOutcome:
        outcome = self._app_server.run_initial_turn(
            binding,
            turn,
            conversation_started=conversation_started,
            turn_prepared=turn_prepared,
            turn_started=turn_started,
            developer_instructions=None,
            process_env={
                "DORF_WORKER_NAME": binding.worker.name,
                "DORF_WORKSPACE": binding.room.workspace,
            },
            timeout_seconds=timeout_seconds,
        )
        return WorkerTurnOutcome(outcome.turn_id, outcome.status)

    def interrupt_conversation_turn(self, binding, turn) -> WorkerTurnOutcome:
        if not binding.agent_conversation_id or not turn.native_turn_id:
            raise ConversationMissingError("Worker native turn identity is unavailable")
        outcome = self._app_server.interrupt(
            binding,
            thread_id=binding.agent_conversation_id,
            turn_id=turn.native_turn_id,
            process_env={
                "DORF_WORKER_NAME": binding.worker.name,
                "DORF_WORKSPACE": binding.room.workspace,
            },
        )
        return WorkerTurnOutcome(outcome.turn_id, outcome.status)

    def recover_conversation_turn(self, binding, turns, turn) -> AgentTurnRecovery:
        thread_id = binding.agent_conversation_id
        if not thread_id:
            return AgentTurnRecovery("not-submitted")
        inspection = self._app_server.inspect(
            binding,
            turns,
            thread_id,
            process_env={
                "DORF_WORKER_NAME": binding.worker.name,
                "DORF_WORKSPACE": binding.room.workspace,
            },
        )
        return _native_turn_recovery(inspection.native, turns, turn)

    def continue_conversation(
        self,
        binding: WorkerBinding,
        turn: WorkerAgentTurn,
        *,
        turn_prepared: Callable[[str | None], None],
        turn_started: Callable[[str], None],
    ) -> WorkerTurnOutcome:
        thread_id = binding.agent_conversation_id
        if not thread_id:
            raise ConversationMissingError(
                f"Worker {binding.worker.name} has no bound Codex thread"
            )
        outcome = self._app_server.run_turn(
            binding,
            turn,
            thread_id=thread_id,
            workspace=binding.room.workspace,
            model=turn.model,
            reasoning_effort=turn.reasoning_effort,
            turn_prepared=turn_prepared,
            turn_started=turn_started,
            process_env={
                "DORF_WORKER_NAME": binding.worker.name,
                "DORF_WORKSPACE": binding.room.workspace,
            },
        )
        return WorkerTurnOutcome(outcome.turn_id, outcome.status)

    def start_job_conversation(
        self,
        binding: JobBinding,
        turn: WorkerAgentTurn,
        *,
        conversation_started: Callable[[str], None],
        turn_prepared: Callable[[str | None], None],
        turn_started: Callable[[str], None],
        timeout_seconds: float | None = None,
    ) -> WorkerTurnOutcome:
        process_env = self._job_process_env(binding)
        outcome = self._app_server.run_initial_turn(
            binding,
            turn,
            conversation_started=conversation_started,
            turn_prepared=turn_prepared,
            turn_started=turn_started,
            developer_instructions=assignment_reporting_instructions(
                binding.job.name,
                binding.assignment.id,
                binding.job.goal_version,
            ),
            process_env=process_env,
            timeout_seconds=timeout_seconds,
        )
        return WorkerTurnOutcome(outcome.turn_id, outcome.status)

    def interrupt_job_conversation_turn(self, binding, turn) -> WorkerTurnOutcome:
        if not binding.agent_conversation_id or not turn.native_turn_id:
            raise ConversationMissingError("Job native turn identity is unavailable")
        outcome = self._app_server.interrupt(
            binding,
            thread_id=binding.agent_conversation_id,
            turn_id=turn.native_turn_id,
            process_env=self._job_process_env(binding),
        )
        return WorkerTurnOutcome(outcome.turn_id, outcome.status)

    def recover_job_conversation_turn(self, binding, turns, turn) -> AgentTurnRecovery:
        thread_id = binding.agent_conversation_id
        if not thread_id:
            return AgentTurnRecovery("not-submitted")
        inspection = self._app_server.inspect(
            binding,
            turns,
            thread_id,
            process_env=self._job_process_env(binding),
        )
        return _native_turn_recovery(inspection.native, turns, turn)

    def continue_job_conversation(
        self,
        binding: JobBinding,
        turn: WorkerAgentTurn,
        *,
        turn_prepared: Callable[[str | None], None],
        turn_started: Callable[[str], None],
    ) -> WorkerTurnOutcome:
        thread_id = binding.agent_conversation_id
        if not thread_id:
            raise ConversationMissingError(f"Job {binding.job.name} has no bound Codex thread")
        outcome = self._app_server.run_turn(
            binding,
            turn,
            thread_id=thread_id,
            workspace=binding.assignment.workspace,
            model=turn.model,
            reasoning_effort=turn.reasoning_effort,
            turn_prepared=turn_prepared,
            turn_started=turn_started,
            process_env=self._job_process_env(binding),
        )
        return WorkerTurnOutcome(outcome.turn_id, outcome.status)

    def inspect_job_conversation(
        self, binding: JobBinding, turns: list
    ) -> AgentConversationInspection:
        thread_id = binding.agent_conversation_id
        if not thread_id:
            raise ConversationMissingError(f"Job {binding.job.name} has no bound Codex thread")
        inspection = self._app_server.inspect(
            binding,
            turns,
            thread_id,
            process_env=self._job_process_env(binding),
        )
        status = inspection.native.get("status")
        active_flags = status.get("activeFlags") if isinstance(status, dict) else None
        attention_status = (
            "pending-approval"
            if isinstance(active_flags, list) and "waitingOnApproval" in active_flags
            else None
        )
        return AgentConversationInspection(
            inspection.connection_status,
            inspection.native,
            attention_status=attention_status,
        )

    @staticmethod
    def _job_process_env(binding: JobBinding) -> dict[str, str]:
        return {
            "DORF_WORKER_NAME": binding.worker.name,
            "DORF_JOB_NAME": binding.job.name,
            "DORF_ASSIGNMENT_ID": binding.assignment.id,
            "DORF_REPORT_ROOT": job_report_root(binding.job.name),
            "DORF_WORKSPACE": binding.assignment.workspace,
        }

    def inspect_conversation(
        self, binding: WorkerBinding, turns: list
    ) -> AgentConversationInspection:
        thread_id = binding.agent_conversation_id
        if not thread_id:
            raise ConversationMissingError(
                f"Worker {binding.worker.name} has no bound Codex thread"
            )
        inspection = self._app_server.inspect(
            binding,
            turns,
            thread_id,
            process_env={
                "DORF_WORKER_NAME": binding.worker.name,
                "DORF_WORKSPACE": binding.room.workspace,
            },
        )
        status = inspection.native.get("status")
        active_flags = status.get("activeFlags") if isinstance(status, dict) else None
        attention_status = (
            "pending-approval"
            if isinstance(active_flags, list) and "waitingOnApproval" in active_flags
            else None
        )
        return AgentConversationInspection(
            inspection.connection_status,
            inspection.native,
            attention_status=attention_status,
        )


def _native_turn_recovery(native: dict, local_turns: list, target) -> AgentTurnRecovery:
    native_turns = native.get("turns")
    if not isinstance(native_turns, list):
        return AgentTurnRecovery("uncertain", error="Native conversation has no turn history")
    candidate = None
    if target.native_turn_id is not None:
        candidate = next(
            (
                item
                for item in native_turns
                if isinstance(item, dict) and item.get("id") == target.native_turn_id
            ),
            None,
        )
        if candidate is None:
            return AgentTurnRecovery(
                "uncertain",
                target.native_turn_id,
                "Recorded native turn is missing from the bound conversation",
            )
    else:
        known = {
            item.native_turn_id
            for item in local_turns
            if item.id != target.id and item.native_turn_id is not None
        }
        baseline_index = -1
        if target.native_turn_baseline_id is not None:
            baseline_index = next(
                (
                    index
                    for index, item in enumerate(native_turns)
                    if isinstance(item, dict) and item.get("id") == target.native_turn_baseline_id
                ),
                -2,
            )
            if baseline_index == -2:
                return AgentTurnRecovery(
                    "uncertain", error="Recorded native turn baseline is missing"
                )
        candidates = [
            item
            for item in native_turns[baseline_index + 1 :]
            if isinstance(item, dict)
            and isinstance(item.get("id"), str)
            and item.get("id") not in known
        ]
        if not candidates:
            return AgentTurnRecovery("not-submitted")
        if len(candidates) != 1:
            return AgentTurnRecovery(
                "uncertain", error="Multiple unbound native turns follow the recorded baseline"
            )
        candidate = candidates[0]
    native_turn_id = candidate.get("id")
    status = candidate.get("status")
    if status == "completed":
        items = candidate.get("items")
        has_response = isinstance(items, list) and any(
            isinstance(item, dict)
            and item.get("type") == "agentMessage"
            and isinstance(item.get("text"), str)
            and bool(item["text"])
            for item in items
        )
        prior_error = getattr(target, "error", None)
        if (
            getattr(target, "status", None) == "recovery-required"
            and prior_error
            and not has_response
        ):
            return AgentTurnRecovery("failed", native_turn_id, prior_error)
        return AgentTurnRecovery("completed", native_turn_id)
    if status == "interrupted":
        return AgentTurnRecovery("interrupted", native_turn_id)
    if status == "failed":
        error = candidate.get("error")
        return AgentTurnRecovery(
            "failed",
            native_turn_id,
            json.dumps(error, sort_keys=True) if isinstance(error, dict) else "Native turn failed",
        )
    thread_status = native.get("status")
    active_flags = thread_status.get("activeFlags") if isinstance(thread_status, dict) else None
    if isinstance(active_flags, list) and "waitingOnApproval" in active_flags:
        return AgentTurnRecovery(
            "pending-approval", native_turn_id, "Native turn is waiting on approval"
        )
    if status in {"inProgress", "active"}:
        return AgentTurnRecovery("active", native_turn_id, "Native turn remains active")
    return AgentTurnRecovery("uncertain", native_turn_id, f"Unknown native turn status: {status!r}")


def _write_private_file(path: Path, value: str) -> None:
    descriptor = os.open(
        path,
        os.O_WRONLY | os.O_CREAT | os.O_TRUNC | getattr(os, "O_NOFOLLOW", 0),
        0o600,
    )
    try:
        os.fchmod(descriptor, 0o600)
        file = os.fdopen(descriptor, "w")
        descriptor = -1
        with file:
            file.write(value)
    except BaseException:
        path.unlink(missing_ok=True)
        raise
    finally:
        if descriptor >= 0:
            os.close(descriptor)


def _copy_app_server_output(source, diagnostic_output) -> None:
    if source is None:
        return
    try:
        while data := source.read(65536):
            diagnostic_output.write(data)
            _write_console(data)
    except OSError:
        pass
    finally:
        source.close()


def _append_diagnostic(output, message: str) -> None:
    data = f"[dorf] {message}\n".encode()
    output.write(data)
    _write_console(data)


def _write_console(data: bytes) -> None:
    stream = getattr(sys.stdout, "buffer", None)
    if stream is None:
        return
    try:
        stream.write(data)
        stream.flush()
    except OSError:
        pass


def _append_output_best_effort(output_path: Path, message: str) -> None:
    try:
        output_path.parent.mkdir(parents=True, exist_ok=True)
        with output_path.open("ab") as output:
            _append_diagnostic(output, message)
    except OSError:
        pass


def _terminate_process_group(process: subprocess.Popen) -> None:
    if process.poll() is not None:
        process.wait()
        return
    try:
        os.killpg(process.pid, signal.SIGTERM)
        process.wait(timeout=5)
    except ProcessLookupError:
        process.wait()
    except subprocess.TimeoutExpired:
        os.killpg(process.pid, signal.SIGKILL)
        process.wait()


def _interrupt(signum: int, frame: object) -> None:
    raise KeyboardInterrupt


def _command_message(result: subprocess.CompletedProcess[str]) -> str:
    return result.stderr.strip() or result.stdout.strip()
