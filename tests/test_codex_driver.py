import io
import json
import subprocess
from collections import deque
from contextlib import contextmanager
from types import SimpleNamespace

import pytest

from dorf.adapters.agents import codex
from dorf.adapters.agents.codex import (
    CodexAppServerProtocol,
    CodexAuthenticationError,
    CodexDriver,
    CodexProtocolError,
    CodexStartupError,
    CodexThreadActive,
    CodexThreadNotFound,
    CodexTransportError,
    CodexTurnFailed,
)
from dorf.runtime import (
    Assignment,
    Job,
    JobBinding,
    JobConversation,
    Room,
    Worker,
    WorkerAgentTurn,
    WorkerBinding,
)


class RecordingEnvironment:
    def __init__(self, *, login_exit_code: int = 0, private_ip: str = "10.42.0.19") -> None:
        self.login_exit_code = login_exit_code
        self.private_ip = private_ip
        self.executed = []
        self.processed = []
        self.spawned = []

    def execute(self, binding, argv, *, cwd=None, env=None, input=None):
        self.executed.append((binding, argv, cwd, env, input))
        if argv == [
            "bash",
            "-lc",
            "command -v codex >/dev/null && codex app-server --help >/dev/null",
        ]:
            return subprocess.CompletedProcess(argv, 0, "", "")
        if argv == ["codex", "login", "status"]:
            return subprocess.CompletedProcess(
                argv,
                self.login_exit_code,
                "Logged in using ChatGPT\n" if self.login_exit_code == 0 else "",
                "Not logged in" if self.login_exit_code else "",
            )
        if argv == ["ip", "-4", "route", "get", "1.1.1.1"]:
            return subprocess.CompletedProcess(
                argv,
                0,
                f"1.1.1.1 via 10.42.0.1 dev enp5s0 src {self.private_ip} uid 0\n",
                "",
            )
        return subprocess.CompletedProcess(argv, 0, "", "")

    def process_command(self, binding, argv, *, cwd=None, env=None):
        self.processed.append((binding, argv, cwd, env))
        command = ["environment-exec", binding.environment_id, cwd or "", "--"]
        if env:
            command.extend(["env", *[f"{key}={value}" for key, value in env.items()]])
        return [*command, *argv]


def test_codex_prepare_uses_room_route_probe_instead_of_local_login_state() -> None:
    class RoutedEnvironment(RecordingEnvironment):
        def __init__(self) -> None:
            super().__init__(login_exit_code=1)
            self.route_checks = []

        def check_codex_authentication(self, binding) -> None:
            self.route_checks.append(binding)

    environment = RoutedEnvironment()
    binding = object()

    CodexDriver(environment).prepare(binding)

    assert environment.route_checks == [binding]
    assert all(argv != ["codex", "login", "status"] for _, argv, *_ in environment.executed)


def app_server_client(*, connector=None) -> codex._CodexAppServerClient:
    return codex._CodexAppServerClient(
        "ws://10.42.0.19:4500",
        "secret-token",
        connector,
    )


def test_app_server_client_repr_redacts_capability_token() -> None:
    client = app_server_client()

    assert "secret-token" not in repr(client)
    assert "ws://10.42.0.19:4500" in repr(client)
    assert client != codex._CodexAppServerClient(client.endpoint, "different-token")


class FakeConnection:
    def __init__(self, messages) -> None:
        self.messages = deque(messages)
        self.sent = []

    def send(self, message: str) -> None:
        self.sent.append(json.loads(message))

    def recv(self):
        if not self.messages:
            raise EOFError("fake app-server disconnected")
        message = self.messages.popleft()
        if isinstance(message, BaseException):
            raise message
        return message if isinstance(message, str) else json.dumps(message)


def install_restarted_server(monkeypatch, messages):
    connection = FakeConnection(messages)
    launches = []
    terminated = []

    class RunningProcess:
        pid = 1234
        stdout = io.BytesIO()

        def poll(self):
            return None

    def popen(command, **kwargs):
        process = RunningProcess()
        launches.append((command, kwargs, process))
        return process

    monkeypatch.setattr(codex.subprocess, "Popen", popen)
    monkeypatch.setattr(
        codex,
        "_default_connector",
        lambda endpoint, **kwargs: connection,
    )
    monkeypatch.setattr(
        codex,
        "_terminate_process_group",
        terminated.append,
    )
    return connection, launches, terminated


def install_initial_turn(monkeypatch, operation):
    class InitialTurnProtocol:
        def run_initial_turn(self, **kwargs):
            return operation(**kwargs)

    @contextmanager
    def launch_and_connect(self, command, *, diagnostic_output=None):
        yield InitialTurnProtocol()

    monkeypatch.setattr(
        codex._CodexAppServerClient,
        "launch_and_connect",
        launch_and_connect,
    )


def successful_messages(*, turn_status: str = "completed", turn_error=None):
    turn = {
        "id": "turn-456",
        "status": turn_status,
        "items": [{"type": "agentMessage", "text": "not persisted by Dorf"}],
    }
    if turn_error is not None:
        turn["error"] = turn_error
    return [
        {"id": 0, "result": {"userAgent": "codex-cli/0.144.6"}},
        {
            "id": 1,
            "result": {
                "thread": {"id": "thread-123", "extra": "ignored"},
                "model": "gpt-5.6-sol",
                "reasoningEffort": "high",
            },
        },
        {"id": 2, "result": {"turn": {"id": "turn-456", "status": "inProgress"}}},
        {"method": "item/completed", "params": {"item": {"large": "ignored"}}},
        {"method": "turn/completed", "params": {"threadId": "thread-123", "turn": turn}},
    ]


def inspection_messages(*, status: str = "notLoaded"):
    thread = {
        "id": "thread-123",
        "status": {"type": status},
        "turns": [
            {
                "id": "turn-456",
                "status": "completed",
                "items": [{"type": "agentMessage", "text": "native history"}],
            }
        ],
    }
    return [
        {"id": 0, "result": {"userAgent": "codex-cli/0.144.6"}},
        {"id": 1, "result": {"thread": thread}},
        {"id": 2, "result": {"thread": {"id": "thread-123"}}},
        {
            "id": 3,
            "result": {"thread": {**thread, "status": {"type": "idle"}}},
        },
    ]


def continued_turn_messages():
    return [
        {"id": 0, "result": {"userAgent": "codex-cli/0.144.6"}},
        {
            "id": 1,
            "result": {
                "thread": {
                    "id": "thread-123",
                    "status": {"type": "idle"},
                    "turns": [{"id": "turn-initial", "status": "completed", "items": []}],
                }
            },
        },
        {"id": 2, "result": {"turn": {"id": "turn-repair", "status": "inProgress"}}},
        {
            "method": "turn/completed",
            "params": {
                "threadId": "thread-123",
                "turn": {"id": "turn-repair", "status": "completed", "items": []},
            },
        },
    ]


def test_protocol_starts_native_thread_and_turn_with_resolved_config() -> None:
    connection = FakeConnection(successful_messages())
    conversations = []
    prepared = []
    started = []
    protocol = CodexAppServerProtocol(connection)

    outcome = protocol.run_initial_turn(
        prompt="Implement issue #90",
        workspace="/workspace",
        model="gpt-5.6-sol",
        reasoning_effort="high",
        conversation_started=conversations.append,
        turn_prepared=prepared.append,
        turn_started=started.append,
    )

    assert outcome.thread_id == "thread-123"
    assert outcome.turn_id == "turn-456"
    assert outcome.status == "completed"
    assert conversations == ["thread-123"]
    assert prepared == [None]
    assert started == ["turn-456"]
    assert connection.sent == [
        {
            "method": "initialize",
            "id": 0,
            "params": {
                "clientInfo": {
                    "name": "dorf",
                    "title": "Dorf",
                    "version": codex.CLIENT_VERSION,
                }
            },
        },
        {"method": "initialized", "params": {}},
        {
            "method": "thread/start",
            "id": 1,
            "params": {
                "cwd": "/workspace",
                "model": "gpt-5.6-sol",
                "approvalPolicy": "never",
                "sandbox": "danger-full-access",
            },
        },
        {
            "method": "turn/start",
            "id": 2,
            "params": {
                "threadId": "thread-123",
                "input": [{"type": "text", "text": "Implement issue #90"}],
                "cwd": "/workspace",
                "model": "gpt-5.6-sol",
                "effort": "high",
                "approvalPolicy": "never",
                "sandboxPolicy": {"type": "dangerFullAccess"},
            },
        },
    ]


def test_protocol_starts_worker_general_thread_without_job_guidance() -> None:
    connection = FakeConnection(successful_messages())
    protocol = CodexAppServerProtocol(connection)

    outcome = protocol.run_initial_turn(
        prompt="Tell me what you can help with",
        workspace="/workspace",
        model="gpt-5.6-sol",
        reasoning_effort="high",
        developer_instructions=None,
        conversation_started=lambda thread_id: None,
        turn_prepared=lambda baseline: None,
        turn_started=lambda turn_id: None,
    )

    assert outcome.status == "completed"
    thread_start = next(
        message for message in connection.sent if message.get("method") == "thread/start"
    )
    assert "developerInstructions" not in thread_start["params"]
    assert all("DORF_JOB_NAME" not in json.dumps(message) for message in connection.sent)


def test_protocol_bounds_all_receives_by_one_turn_deadline() -> None:
    class TimedConnection(FakeConnection):
        def __init__(self, messages) -> None:
            super().__init__(messages)
            self.timeouts = []

        def recv(self, timeout=None):
            self.timeouts.append(timeout)
            return super().recv()

    connection = TimedConnection(successful_messages())

    outcome = CodexAppServerProtocol(connection, timeout_seconds=10).run_initial_turn(
        prompt="Prove the app-server path",
        workspace="/workspace",
        model="gpt-5.6-sol",
        reasoning_effort="high",
        conversation_started=lambda thread_id: None,
    )

    assert outcome.status == "completed"
    assert connection.timeouts
    assert all(timeout is not None and 0 < timeout <= 10 for timeout in connection.timeouts)


def test_codex_driver_starts_worker_general_conversation_without_job_environment(
    tmp_path,
    monkeypatch,
) -> None:
    captured = {}

    def initial_turn(**kwargs):
        captured.update(kwargs)
        kwargs["conversation_started"]("thread-general")
        kwargs["turn_prepared"](None)
        kwargs["turn_started"]("turn-general")
        return codex.CodexTurnOutcome("thread-general", "turn-general", "completed")

    install_initial_turn(monkeypatch, initial_turn)
    environment = RecordingEnvironment()
    driver = CodexDriver(environment)
    worker = Worker(
        id=1,
        name="researcher",
        harness_type="codex",
        provenance="caller",
        lifecycle_policy="caller-managed",
        status="ready",
        error=None,
        current_room_id="room-1",
        general_conversation_id=None,
        created_at="2026-01-01T00:00:00+00:00",
        updated_at="2026-01-01T00:00:00+00:00",
    )
    room = Room(
        id="room-1",
        worker_name="researcher",
        room_type="incus-vm",
        provider_id="dorf-researcher",
        workspace="/workspace",
        status="ready",
        error=None,
        metadata={},
        created_at="2026-01-01T00:00:00+00:00",
        updated_at="2026-01-01T00:00:00+00:00",
    )
    conversations = []
    prepared = []
    started = []

    outcome = driver.start_conversation(
        WorkerBinding(worker, room),
        WorkerAgentTurn(
            "Hello",
            tmp_path / "runs" / "output.log",
            "gpt-5.6-sol",
            "high",
        ),
        conversation_started=conversations.append,
        turn_prepared=prepared.append,
        turn_started=started.append,
    )

    assert outcome.status == "completed"
    assert conversations == ["thread-general"]
    assert prepared == [None]
    assert started == ["turn-general"]
    assert captured["developer_instructions"] is None
    _, app_server_argv, cwd, process_env = environment.processed[-1]
    listen_endpoint = app_server_argv[app_server_argv.index("--listen") + 1]
    assert listen_endpoint.startswith("ws://10.42.0.19:")
    assert listen_endpoint != "ws://10.42.0.19:4500"
    guest_token_path = app_server_argv[app_server_argv.index("--ws-token-file") + 1]
    assert guest_token_path.startswith("/tmp/dorf/codex-app-server-")
    assert guest_token_path != codex.APP_SERVER_TOKEN_PATH
    assert cwd == "/workspace"
    assert process_env == {
        "DORF_WORKER_NAME": "researcher",
        "DORF_WORKSPACE": "/workspace",
    }
    assert environment.executed[-1][1] == ["rm", "-f", guest_token_path]


def test_codex_driver_starts_job_thread_in_assignment_workspace_with_typed_env(
    tmp_path,
    monkeypatch,
) -> None:
    captured = {}

    def initial_turn(**kwargs):
        captured.update(kwargs)
        kwargs["conversation_started"]("thread-job")
        kwargs["turn_prepared"](None)
        kwargs["turn_started"]("turn-goal")
        return codex.CodexTurnOutcome("thread-job", "turn-goal", "completed")

    install_initial_turn(monkeypatch, initial_turn)
    environment = RecordingEnvironment()
    driver = CodexDriver(environment)
    worker = Worker(
        1,
        "researcher",
        "codex",
        "caller",
        "caller-managed",
        "assigned",
        None,
        "room-1",
        None,
        "created",
        "updated",
    )
    room = Room(
        "room-1",
        "researcher",
        "incus-vm",
        "dorf-researcher",
        "/workspace",
        "ready",
        None,
        {},
        "created",
        "updated",
    )
    binding = JobBinding(
        Job(1, "checkout-perf", "open", 1, "Make checkout instant", "created", "updated"),
        Assignment(
            "assignment-1",
            "checkout-perf",
            "researcher",
            1,
            "open",
            "room-1",
            "/workspace/jobs/checkout-perf",
            "created",
            None,
        ),
        JobConversation(
            "conversation-1",
            "checkout-perf",
            None,
            "gpt-5.6-sol",
            "high",
            "idle",
            None,
            "created",
            "updated",
        ),
        worker,
        room,
    )

    outcome = driver.start_job_conversation(
        binding,
        WorkerAgentTurn(
            "Make checkout instant",
            tmp_path / "runs" / "output.log",
            "gpt-5.6-sol",
            "high",
        ),
        conversation_started=lambda value: None,
        turn_prepared=lambda value: None,
        turn_started=lambda value: None,
    )

    assert outcome.status == "completed"
    assert "Assignment\nassignment-1" in captured["developer_instructions"]
    assert "/run/dorf/jobs/checkout-perf/context/1" in captured["developer_instructions"]
    assert captured["prompt"] == "Make checkout instant"
    _, app_server_argv, cwd, process_env = environment.processed[-1]
    assert cwd == "/workspace/jobs/checkout-perf"
    assert process_env == {
        "DORF_WORKER_NAME": "researcher",
        "DORF_JOB_NAME": "checkout-perf",
        "DORF_ASSIGNMENT_ID": "assignment-1",
        "DORF_REPORT_ROOT": "/run/dorf/jobs/checkout-perf/outbox",
        "DORF_WORKSPACE": "/workspace/jobs/checkout-perf",
    }
    guest_token_path = app_server_argv[app_server_argv.index("--ws-token-file") + 1]
    assert guest_token_path.startswith("/tmp/dorf/codex-app-server-")


def test_protocol_resumes_bound_thread_and_reads_native_history() -> None:
    connection = FakeConnection(inspection_messages())

    thread = CodexAppServerProtocol(connection).inspect_thread("thread-123")

    assert thread["id"] == "thread-123"
    assert thread["status"] == {"type": "idle"}
    assert thread["turns"][0]["items"][0]["text"] == "native history"
    assert connection.sent == [
        {
            "method": "initialize",
            "id": 0,
            "params": {
                "clientInfo": {
                    "name": "dorf",
                    "title": "Dorf",
                    "version": codex.CLIENT_VERSION,
                }
            },
        },
        {"method": "initialized", "params": {}},
        {
            "method": "thread/read",
            "id": 1,
            "params": {"threadId": "thread-123", "includeTurns": True},
        },
        {
            "method": "thread/resume",
            "id": 2,
            "params": {"threadId": "thread-123"},
        },
        {
            "method": "thread/read",
            "id": 3,
            "params": {"threadId": "thread-123", "includeTurns": True},
        },
    ]


def test_codex_recovery_distinguishes_unsubmitted_and_completed_native_turns() -> None:
    target = SimpleNamespace(
        id=2,
        native_turn_id=None,
        native_turn_baseline_id="turn-before",
    )
    previous = SimpleNamespace(id=1, native_turn_id="turn-before")
    native = {
        "status": {"type": "idle"},
        "turns": [{"id": "turn-before", "status": "completed"}],
    }

    assert codex._native_turn_recovery(native, [previous, target], target).status == (
        "not-submitted"
    )

    native["turns"].append({"id": "turn-recovered", "status": "completed"})
    recovered = codex._native_turn_recovery(native, [previous, target], target)
    assert (recovered.status, recovered.native_turn_id) == (
        "completed",
        "turn-recovered",
    )


def test_codex_recovery_preserves_known_failure_when_native_turn_has_no_response() -> None:
    target = SimpleNamespace(
        id=1,
        native_turn_id="turn-failed",
        native_turn_baseline_id=None,
        status="recovery-required",
        error="Worker harness failure: authentication expired",
    )
    native = {
        "status": {"type": "idle"},
        "turns": [{"id": "turn-failed", "status": "completed", "items": []}],
    }

    recovered = codex._native_turn_recovery(native, [target], target)

    assert recovered.status == "failed"
    assert recovered.native_turn_id == "turn-failed"
    assert recovered.error == "Worker harness failure: authentication expired"


def test_protocol_runs_continued_turn_on_exact_bound_thread_with_conversation_config() -> None:
    connection = FakeConnection(continued_turn_messages())
    prepared = []
    started = []

    outcome = CodexAppServerProtocol(connection).run_turn(
        thread_id="thread-123",
        prompt="Repair the failing check",
        workspace="/workspace",
        model="gpt-5.6-sol",
        reasoning_effort="high",
        turn_prepared=prepared.append,
        turn_started=started.append,
    )

    assert outcome.thread_id == "thread-123"
    assert outcome.turn_id == "turn-repair"
    assert outcome.status == "completed"
    assert prepared == ["turn-initial"]
    assert started == ["turn-repair"]
    assert connection.sent[-1] == {
        "method": "turn/start",
        "id": 2,
        "params": {
            "threadId": "thread-123",
            "input": [{"type": "text", "text": "Repair the failing check"}],
            "cwd": "/workspace",
            "model": "gpt-5.6-sol",
            "effort": "high",
            "approvalPolicy": "never",
            "sandboxPolicy": {"type": "dangerFullAccess"},
        },
    }


def test_protocol_does_not_submit_when_bound_thread_is_active() -> None:
    messages = inspection_messages(status="active")[:2]
    connection = FakeConnection(messages)
    prepared = []
    started = []

    with pytest.raises(CodexThreadActive, match="already has an active turn"):
        CodexAppServerProtocol(connection).run_turn(
            thread_id="thread-123",
            prompt="Repair the failing check",
            workspace="/workspace",
            model="gpt-5.6-sol",
            reasoning_effort="high",
            turn_prepared=prepared.append,
            turn_started=started.append,
        )

    assert prepared == []
    assert started == []
    assert [message["method"] for message in connection.sent] == [
        "initialize",
        "initialized",
        "thread/read",
    ]


def test_protocol_reports_active_turn_without_resuming_or_replacing_it() -> None:
    messages = inspection_messages(status="active")[:2]
    messages[1]["result"]["thread"]["status"] = {
        "type": "active",
        "activeFlags": ["waitingOnApproval"],
    }
    connection = FakeConnection(messages)

    thread = CodexAppServerProtocol(connection).inspect_thread("thread-123")

    assert thread["status"] == {
        "type": "active",
        "activeFlags": ["waitingOnApproval"],
    }
    assert [message["method"] for message in connection.sent] == [
        "initialize",
        "initialized",
        "thread/read",
    ]


def test_protocol_interrupts_the_exact_recorded_turn() -> None:
    connection = FakeConnection(
        [
            {"id": 0, "result": {"userAgent": "codex-cli/0.144.6"}},
            {"id": 1, "result": {}},
            {
                "method": "turn/completed",
                "params": {
                    "threadId": "thread-123",
                    "turn": {"id": "turn-active", "status": "interrupted"},
                },
            },
        ]
    )

    outcome = CodexAppServerProtocol(connection).interrupt_turn("thread-123", "turn-active")

    assert outcome == codex.CodexTurnOutcome("thread-123", "turn-active", "interrupted")
    assert connection.sent[-1] == {
        "method": "turn/interrupt",
        "id": 1,
        "params": {"threadId": "thread-123", "turnId": "turn-active"},
    }


def test_protocol_reports_missing_bound_thread_distinctly() -> None:
    connection = FakeConnection(
        [
            {"id": 0, "result": {"userAgent": "codex-cli/0.144.6"}},
            {
                "id": 1,
                "error": {"code": -32602, "message": "thread thread-123 not found"},
            },
        ]
    )

    with pytest.raises(CodexThreadNotFound, match="thread-123"):
        CodexAppServerProtocol(connection).inspect_thread("thread-123")


def test_protocol_reports_disconnect_after_durably_exposing_thread() -> None:
    messages = successful_messages()[:3]
    connection = codex._CodexTransportConnection(FakeConnection(messages))
    conversations = []

    with pytest.raises(CodexTransportError, match="disconnected before turn completion"):
        CodexAppServerProtocol(connection).run_initial_turn(
            prompt="Implement",
            workspace="/workspace",
            model="gpt-5.6-sol",
            reasoning_effort="low",
            conversation_started=conversations.append,
        )

    assert conversations == ["thread-123"]


def test_protocol_keeps_completion_that_arrives_before_turn_response() -> None:
    messages = successful_messages()
    messages[2], messages[4] = messages[4], messages[2]
    connection = FakeConnection(messages)

    outcome = CodexAppServerProtocol(connection).run_initial_turn(
        prompt="Implement",
        workspace="/workspace",
        model="gpt-5.6-sol",
        reasoning_effort="low",
        conversation_started=lambda thread_id: None,
    )

    assert outcome.status == "completed"


def test_protocol_rejects_unexpected_response_id() -> None:
    messages = successful_messages()
    messages.insert(0, {"id": 99, "result": {}})

    with pytest.raises(CodexProtocolError, match="unexpected response id 99.*waiting for 0"):
        CodexAppServerProtocol(FakeConnection(messages)).run_initial_turn(
            prompt="Implement",
            workspace="/workspace",
            model="gpt-5.6-sol",
            reasoning_effort="low",
            conversation_started=lambda thread_id: None,
        )


def test_protocol_rejects_envelope_without_response_id_or_notification_method() -> None:
    messages = successful_messages()
    messages.insert(0, {"params": {"unexpected": True}})

    with pytest.raises(CodexProtocolError, match="without id or method.*initialize"):
        CodexAppServerProtocol(FakeConnection(messages)).run_initial_turn(
            prompt="Implement",
            workspace="/workspace",
            model="gpt-5.6-sol",
            reasoning_effort="low",
            conversation_started=lambda thread_id: None,
        )


def test_protocol_rejects_unclassifiable_terminal_envelope_with_bounded_payload() -> None:
    messages = successful_messages()
    messages[3] = {"params": {"unexpected": "x" * 2000}}

    with pytest.raises(CodexProtocolError, match="terminal message without id or method") as raised:
        CodexAppServerProtocol(FakeConnection(messages)).run_initial_turn(
            prompt="Implement",
            workspace="/workspace",
            model="gpt-5.6-sol",
            reasoning_effort="low",
            conversation_started=lambda thread_id: None,
        )

    diagnostic = str(raised.value)
    assert '"unexpected"' in diagnostic
    assert len(diagnostic) <= 1100


def test_protocol_rejects_late_response_after_turn_start() -> None:
    messages = successful_messages()
    messages[3] = {"id": 99, "result": {"incidental": "x" * 2000}}

    with pytest.raises(CodexProtocolError, match="unexpected terminal response id 99") as raised:
        CodexAppServerProtocol(FakeConnection(messages)).run_initial_turn(
            prompt="Implement",
            workspace="/workspace",
            model="gpt-5.6-sol",
            reasoning_effort="low",
            conversation_started=lambda thread_id: None,
        )

    diagnostic = str(raised.value)
    assert '"incidental"' in diagnostic
    assert len(diagnostic) <= 1100


def test_protocol_rejects_malformed_messages_with_native_payload() -> None:
    connection = FakeConnection(["not-json"])

    with pytest.raises(CodexProtocolError, match="not-json"):
        CodexAppServerProtocol(connection).run_initial_turn(
            prompt="Implement",
            workspace="/workspace",
            model="gpt-5.6-sol",
            reasoning_effort="low",
            conversation_started=lambda thread_id: None,
        )


@pytest.mark.parametrize(
    ("status_code", "on_response"),
    [(401, False), (403, False), (403, True)],
)
def test_authenticated_connection_reports_handshake_failure(status_code, on_response) -> None:
    class AuthenticationRejected(Exception):
        pass

    class Response:
        pass

    rejection = AuthenticationRejected(f"HTTP {status_code} Unauthorized")
    if on_response:
        rejection.response = Response()
        rejection.response.status_code = status_code
    else:
        rejection.status_code = status_code

    def reject(endpoint, **kwargs):
        assert endpoint == "ws://10.42.0.19:4500"
        assert kwargs["additional_headers"] == {"Authorization": "Bearer secret-token"}
        assert kwargs["proxy"] is None
        raise rejection

    with pytest.raises(CodexAuthenticationError, match=str(status_code)):
        app_server_client(connector=reject)._connect()


def test_authenticated_connection_bounds_incoming_message_size() -> None:
    connection = object()
    options = {}

    def connect(endpoint, **kwargs):
        options.update(kwargs)
        return connection

    translated = app_server_client(connector=connect)._connect()
    assert translated._connection is connection
    assert options["max_size"] == 64 * 1024 * 1024


def test_protocol_retains_native_agent_failure() -> None:
    error = {
        "message": "usage limit exhausted",
        "additionalDetails": "retry after 12:00 UTC",
        "codexErrorInfo": "usageLimitExceeded",
    }
    connection = FakeConnection(successful_messages(turn_status="failed", turn_error=error))

    with pytest.raises(CodexTurnFailed) as raised:
        CodexAppServerProtocol(connection).run_initial_turn(
            prompt="Implement",
            workspace="/workspace",
            model="gpt-5.6-sol",
            reasoning_effort="low",
            conversation_started=lambda thread_id: None,
        )

    assert raised.value.native_error == error
    assert "usage limit exhausted" in str(raised.value)
    assert "retry after 12:00 UTC" in str(raised.value)


def test_protocol_retains_fatal_error_notification() -> None:
    error = {
        "message": "sandbox setup failed",
        "additionalDetails": "mount namespace unavailable",
        "codexErrorInfo": "sandboxUnavailable",
    }
    messages = successful_messages()
    messages[3] = {
        "method": "error",
        "params": {
            "threadId": "thread-123",
            "turnId": "turn-456",
            "willRetry": False,
            "error": error,
        },
    }

    with pytest.raises(CodexTurnFailed) as raised:
        CodexAppServerProtocol(FakeConnection(messages)).run_initial_turn(
            prompt="Implement",
            workspace="/workspace",
            model="gpt-5.6-sol",
            reasoning_effort="low",
            conversation_started=lambda thread_id: None,
        )

    assert raised.value.native_error == error
    assert "mount namespace unavailable" in str(raised.value)


@pytest.mark.parametrize(
    ("notification", "expected_error"),
    [
        ({"method": "error"}, "error notification is missing params$"),
        (
            {
                "method": "error",
                "params": {
                    "threadId": "thread-123",
                    "turnId": "turn-456",
                    "error": {"message": "retrying"},
                },
            },
            "params.willRetry",
        ),
        (
            {
                "method": "error",
                "params": {
                    "threadId": "thread-123",
                    "turnId": "turn-456",
                    "willRetry": True,
                },
            },
            "params.error",
        ),
        (
            {
                "method": "error",
                "params": {
                    "threadId": "thread-other",
                    "turnId": "turn-456",
                    "willRetry": True,
                    "error": {"message": "retrying"},
                },
            },
            "unexpected thread",
        ),
        (
            {
                "method": "error",
                "params": {
                    "threadId": "thread-123",
                    "turnId": "turn-other",
                    "willRetry": True,
                    "error": {"message": "retrying"},
                },
            },
            "unexpected turn",
        ),
    ],
)
def test_protocol_rejects_malformed_error_notification(notification, expected_error) -> None:
    messages = successful_messages()
    messages[3] = notification

    with pytest.raises(CodexProtocolError, match=expected_error):
        CodexAppServerProtocol(FakeConnection(messages)).run_initial_turn(
            prompt="Implement",
            workspace="/workspace",
            model="gpt-5.6-sol",
            reasoning_effort="low",
            conversation_started=lambda thread_id: None,
        )


def test_protocol_continues_after_well_formed_retryable_error() -> None:
    messages = successful_messages()
    messages[3] = {
        "method": "error",
        "params": {
            "threadId": "thread-123",
            "turnId": "turn-456",
            "willRetry": True,
            "error": {"message": "temporary transport failure"},
        },
    }

    outcome = CodexAppServerProtocol(FakeConnection(messages)).run_initial_turn(
        prompt="Implement",
        workspace="/workspace",
        model="gpt-5.6-sol",
        reasoning_effort="low",
        conversation_started=lambda thread_id: None,
    )

    assert outcome.status == "completed"


@pytest.mark.parametrize(
    ("malformation", "expected_error"),
    [
        ("thread", "unexpected thread"),
        ("turn", "unexpected turn"),
        ("status", "unknown status"),
    ],
)
def test_protocol_validates_completed_turn_identity_and_status(
    malformation, expected_error
) -> None:
    messages = successful_messages()
    completion = messages[-1]["params"]
    if malformation == "thread":
        completion["threadId"] = "thread-other"
    elif malformation == "turn":
        completion["turn"]["id"] = "turn-other"
    else:
        completion["turn"]["status"] = "mystery"

    with pytest.raises(CodexProtocolError, match=expected_error):
        CodexAppServerProtocol(FakeConnection(messages)).run_initial_turn(
            prompt="Implement",
            workspace="/workspace",
            model="gpt-5.6-sol",
            reasoning_effort="low",
            conversation_started=lambda thread_id: None,
        )


def test_app_server_start_failure_retains_native_launch_error(tmp_path, monkeypatch) -> None:
    def fail_to_start(*args, **kwargs):
        raise OSError("incus exec: guest agent unavailable")

    monkeypatch.setattr(codex.subprocess, "Popen", fail_to_start)

    with (tmp_path / "output.log").open("wb") as output:
        with pytest.raises(CodexStartupError, match="guest agent unavailable"):
            with app_server_client().launch_and_connect(
                ["incus", "exec", "vm", "--", "codex"],
                diagnostic_output=output,
            ):
                pytest.fail("launch failure must not yield a protocol")


def test_app_server_connection_timeout_retains_last_transport_error(monkeypatch) -> None:
    class RunningProcess:
        def poll(self):
            return None

    ticks = iter([0.0, 0.0, 16.0])
    monkeypatch.setattr(codex.time, "monotonic", lambda: next(ticks))
    monkeypatch.setattr(codex.time, "sleep", lambda seconds: None)

    def refuse_connection(endpoint, **kwargs):
        raise OSError("connection refused")

    with pytest.raises(CodexTransportError, match="connection refused"):
        app_server_client(connector=refuse_connection)._await_connection(RunningProcess())


def test_app_server_early_exit_retains_native_exit_code() -> None:
    class ExitedProcess:
        def __init__(self):
            self.waited = False

        def poll(self):
            return 17

        def wait(self):
            self.waited = True

    process = ExitedProcess()

    with pytest.raises(CodexStartupError, match="exit code 17"):
        app_server_client(
            connector=lambda *args, **kwargs: pytest.fail("must not connect")
        )._await_connection(process)

    assert process.waited
