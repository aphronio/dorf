import json
import os
import socket
import traceback
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

import pytest

from dorf.provider_gateway import (
    ConsumerWireIncompatibleError,
    DeviceAuthorization,
    GatewayUnavailableError,
    ProviderAuthenticationStaleError,
    ProviderConnection,
    ProviderConnectionNotFoundError,
    ProviderGateway,
    ProviderSelectionUnsupportedError,
    ProviderUpstreamUnavailableError,
)

PINNED_VERSION = "7.2.104"


def _free_port() -> int:
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        return listener.getsockname()[1]


def _write_test_broker(path: Path) -> None:
    path.write_text(
        """#!/usr/bin/env python3
import http.server
import json
import os
import pathlib
import sys

VERSION = "7.2.104"

if "-h" in sys.argv:
    print(f"Provider broker version: {VERSION}")
    raise SystemExit

config_path = pathlib.Path(sys.argv[sys.argv.index("-config") + 1])
config = config_path.read_text()
host = next(line.split(":", 1)[1].strip().strip('"') for line in config.splitlines()
            if line.startswith("host:"))
port = int(next(line.split(":", 1)[1].strip() for line in config.splitlines()
                if line.startswith("port:")))
auth_dir = pathlib.Path(next(line.split(":", 1)[1].strip().strip('"')
                             for line in config.splitlines()
                             if line.startswith("auth-dir:")))
if "-codex-device-login" in sys.argv:
    print("Starting Codex device authentication...")
    print("Codex device URL: https://auth.openai.com/codex/device")
    print("Codex device code: ABCD-EFGH")
    auth_dir.mkdir(parents=True, exist_ok=True)
    saved_path = auth_dir / "codex-private-account@example.com.json"
    saved_path.write_text('{"type":"codex","access_token":"upstream-secret"}')
    print(f"Authentication saved to {saved_path}")
    print("Codex device authentication successful!")
    raise SystemExit
route_keys = {
    line.split("-", 1)[1].strip().strip('"')
    for line in config.splitlines()
    if line.startswith("  - ")
}
models_ready = False
with (config_path.parent / "broker-starts").open("a") as starts:
    starts.write("started\\n")

class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        global models_ready
        if self.path == "/terminate":
            self.send_response(200)
            self.end_headers()
            self.wfile.flush()
            os._exit(0)
        if self.path == "/v1/models":
            authorization = self.headers.get("Authorization", "")
            if authorization.removeprefix("Bearer ") not in route_keys:
                self.send_response(401)
                self.end_headers()
                return
            models = [{"id": "gpt-5.6-sol"}] if models_ready else []
            models_ready = True
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(json.dumps({"data": models}).encode())
            return
        if self.path == "/v0/management/auth-files":
            status_path = config_path.parent / "fake-auth-status.json"
            status = json.loads(status_path.read_text()) if status_path.exists() else {}
            files = [
                {
                    "name": path.name,
                    "provider": "codex",
                    "status": status.get("status", "active"),
                    "unavailable": status.get("unavailable", False),
                    "id_token": {
                        "plan_type": "pro",
                        "chatgpt_account_id": "private-account-identifier",
                    },
                }
                for path in auth_dir.glob("codex-*.json")
            ]
            if "codex-api-key:" in config:
                files.append(
                    {
                        "name": "configured-openai-platform",
                        "provider": "codex",
                        "runtime_only": True,
                        "status": status.get("status", "active"),
                        "unavailable": status.get("unavailable", False),
                    }
                )
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(json.dumps({"files": files}).encode())
            return
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(b'{"status":"up"}')

    def do_PUT(self):
        if self.path != "/v0/management/api-keys":
            self.send_response(404)
            self.end_headers()
            return
        length = int(self.headers.get("Content-Length", "0"))
        route_keys.clear()
        route_keys.update(json.loads(self.rfile.read(length)))
        self.send_response(200)
        self.end_headers()

    def log_message(self, format, *args):
        pass

http.server.ThreadingHTTPServer((host, port), Handler).serve_forever()
"""
    )
    path.chmod(0o700)


def test_facade_close_leaves_one_ready_broker_for_the_next_caller(tmp_path) -> None:
    executable = tmp_path / "provider-broker"
    _write_test_broker(executable)
    state_path = tmp_path / "gateway"
    port = _free_port()

    with ProviderGateway.open(
        state_path=state_path,
        executable_path=executable,
        port=port,
    ) as gateway:
        health = gateway.ensure_ready()

    assert health.status == "ready"
    assert health.backend_version == PINNED_VERSION
    assert health.bind_addresses == ("127.0.0.1",)
    with urllib.request.urlopen(f"http://127.0.0.1:{port}/", timeout=1) as response:
        assert response.status == 200

    with ProviderGateway.open(
        state_path=state_path,
        executable_path=executable,
        port=port,
    ) as gateway:
        assert gateway.ensure_ready() == health
        gateway.shutdown()

    assert not (state_path / "broker.pid").exists()
    assert os.stat(state_path).st_mode & 0o777 == 0o700


def test_gateway_rebinds_to_one_explicit_local_interface_without_wildcard(
    tmp_path,
) -> None:
    executable = tmp_path / "provider-broker"
    _write_test_broker(executable)
    state_path = tmp_path / "gateway"
    port = _free_port()

    with ProviderGateway.open(
        state_path=state_path,
        executable_path=executable,
        port=port,
    ) as gateway:
        assert gateway.ensure_ready().bind_addresses == ("127.0.0.1",)

    with ProviderGateway.open(
        state_path=state_path,
        executable_path=executable,
        port=port,
        bind_address="127.0.0.2",
    ) as gateway:
        assert gateway.ensure_ready().bind_addresses == ("127.0.0.2",)
        config = (state_path / "broker.yaml").read_text()
        assert 'host: "127.0.0.2"' in config
        assert 'host: "0.0.0.0"' not in config
        assert "  allow-remote: true" in config
        assert "  disable-control-panel: true" in config

    with ProviderGateway.open(
        state_path=state_path,
        executable_path=executable,
        port=port,
    ) as gateway:
        assert gateway.ensure_ready().bind_addresses == ("127.0.0.2",)

    (state_path / "broker.yaml").write_text(
        (state_path / "broker.yaml")
        .read_text()
        .replace(
            "  allow-remote: true",
            "  allow-remote: false",
        )
    )
    with ProviderGateway.open(
        state_path=state_path,
        executable_path=executable,
        port=port,
        bind_address="127.0.0.2",
    ) as gateway:
        gateway.ensure_ready()
        assert "  allow-remote: true" in (state_path / "broker.yaml").read_text()
        gateway.shutdown()

    with pytest.raises(ValueError, match="local IPv4"):
        ProviderGateway.open(
            state_path=state_path,
            executable_path=executable,
            port=port,
            bind_address="0.0.0.0",
        )


def test_default_acquisition_rejects_an_unverified_release_without_leaking_backend_details(
    tmp_path, monkeypatch
) -> None:
    data_home = tmp_path / "data"
    monkeypatch.setenv("XDG_DATA_HOME", str(data_home))

    def corrupt_download(*args, **kwargs):
        raise urllib.error.URLError("download failed for CLIProxyAPI with secret=route-token")

    monkeypatch.setattr(urllib.request, "urlopen", corrupt_download)

    with ProviderGateway.open(port=_free_port()) as gateway:
        with pytest.raises(GatewayUnavailableError) as caught:
            gateway.ensure_ready()

    message = str(caught.value)
    rendered = "".join(traceback.format_exception(caught.value))
    assert message == "Provider gateway executable could not be installed"
    assert "CLIProxyAPI" not in rendered
    assert "route-token" not in rendered
    state_path = data_home / "dorf" / "provider-gateway"
    assert os.stat(state_path).st_mode & 0o777 == 0o700
    assert not list(state_path.rglob("cli-proxy-api"))


def test_connection_state_rejects_a_credential_reference_outside_gateway_custody(
    tmp_path,
) -> None:
    executable = tmp_path / "provider-broker"
    _write_test_broker(executable)
    state_path = tmp_path / "gateway"
    state_path.mkdir()
    outside = tmp_path / "outside.key"
    outside.write_text("must remain untouched")
    (state_path / "connections.json").write_text(
        json.dumps(
            [
                {
                    "name": "work-openai",
                    "provider": "openai",
                    "auth_mode": "api_key",
                    "credential_ref": "../../outside.key",
                }
            ]
        )
    )

    with ProviderGateway.open(
        state_path=state_path,
        executable_path=executable,
        port=_free_port(),
    ) as gateway:
        with pytest.raises(
            GatewayUnavailableError,
            match="Provider gateway connection state is unreadable",
        ):
            gateway.list_connections()

    assert outside.read_text() == "must remain untouched"


def test_health_is_sanitized_before_during_and_after_broker_lifecycle(tmp_path) -> None:
    executable = tmp_path / "provider-broker"
    _write_test_broker(executable)
    state_path = tmp_path / "gateway"

    with ProviderGateway.open(
        state_path=state_path,
        executable_path=executable,
        port=_free_port(),
    ) as gateway:
        stopped = gateway.health()
        ready = gateway.ensure_ready()
        gateway.shutdown()
        stopped_again = gateway.health()

    assert stopped.status == stopped_again.status == "stopped"
    assert stopped.backend_present is True
    assert stopped.backend_version == PINNED_VERSION
    assert stopped.bind_addresses == ("127.0.0.1",)
    assert stopped.has_provider_connection is False
    assert ready.status == "ready"
    assert ready.backend_present is True
    assert ready.has_provider_connection is False


def test_ensure_ready_recovers_when_the_broker_dies_between_callers(tmp_path) -> None:
    executable = tmp_path / "provider-broker"
    _write_test_broker(executable)
    state_path = tmp_path / "gateway"
    port = _free_port()

    with ProviderGateway.open(
        state_path=state_path,
        executable_path=executable,
        port=port,
    ) as gateway:
        gateway.ensure_ready()
        try:
            urllib.request.urlopen(f"http://127.0.0.1:{port}/terminate", timeout=1).close()
        except urllib.error.URLError:
            pass

        assert gateway.ensure_ready().status == "ready"
        gateway.shutdown()

    assert (state_path / "broker-starts").read_text().splitlines() == [
        "started",
        "started",
    ]


def test_concurrent_facades_converge_on_one_broker(tmp_path) -> None:
    executable = tmp_path / "provider-broker"
    _write_test_broker(executable)
    state_path = tmp_path / "gateway"
    port = _free_port()

    def ensure_once() -> str:
        with ProviderGateway.open(
            state_path=state_path,
            executable_path=executable,
            port=port,
        ) as gateway:
            return gateway.ensure_ready().status

    with ThreadPoolExecutor(max_workers=6) as callers:
        statuses = list(callers.map(lambda _: ensure_once(), range(12)))

    assert statuses == ["ready"] * 12
    assert (state_path / "broker-starts").read_text().splitlines() == ["started"]
    with ProviderGateway.open(
        state_path=state_path,
        executable_path=executable,
        port=port,
    ) as gateway:
        gateway.shutdown()


def test_connection_route_is_accepted_then_rejected_after_revocation(tmp_path) -> None:
    executable = tmp_path / "provider-broker"
    _write_test_broker(executable)
    state_path = tmp_path / "gateway"
    port = _free_port()

    with ProviderGateway.open(
        state_path=state_path,
        executable_path=executable,
        port=port,
    ) as gateway:
        gateway.connect_chatgpt_subscription(
            name="personal-chatgpt",
            on_authorization=lambda challenge: None,
        )
        connections = gateway.list_connections()
        assert connections == (
            ProviderConnection(
                name="personal-chatgpt",
                provider="chatgpt",
                auth_mode="subscription",
                status="connected",
            ),
        )
        route = gateway.create_route("personal-chatgpt", consumer="test-client")

        assert route.connection_name == "personal-chatgpt"
        assert route.base_url == f"http://127.0.0.1:{port}/v1"
        assert route.api_key not in repr(route)
        assert gateway.route_for_consumer("test-client") == route
        assert gateway.route_for_consumer("missing-client") is None
        assert _model_status(route.base_url) == 401
        assert _model_status(route.base_url, route.api_key) == 200

    with ProviderGateway.open(
        state_path=state_path,
        executable_path=executable,
        port=port,
    ) as gateway:
        assert gateway.route_for_consumer("test-client") == route
        assert gateway.revoke_route(route.id) is True
        assert _model_status(route.base_url, route.api_key) == 401
        assert gateway.revoke_route(route.id) is False
        gateway.shutdown()

    assert os.stat(state_path / "routes.json").st_mode & 0o777 == 0o600
    assert os.stat(state_path / "broker.yaml").st_mode & 0o777 == 0o600


def test_openai_api_key_connection_has_a_durable_name_and_disconnects_cleanly(
    tmp_path,
) -> None:
    executable = tmp_path / "provider-broker"
    _write_test_broker(executable)
    state_path = tmp_path / "gateway"
    port = _free_port()
    api_key = "upstream-platform-secret"

    with ProviderGateway.open(
        state_path=state_path,
        executable_path=executable,
        port=port,
    ) as gateway:
        connected = gateway.connect_openai_api_key(
            name="work-openai",
            api_key=api_key,
        )

    assert connected == ProviderConnection(
        name="work-openai",
        provider="openai",
        auth_mode="api_key",
        status="connected",
    )
    assert api_key not in repr(connected)

    with ProviderGateway.open(
        state_path=state_path,
        executable_path=executable,
        port=port,
    ) as gateway:
        assert gateway.list_connections() == (connected,)
        reconnected = gateway.connect_openai_api_key(
            name="work-openai",
            api_key="rotated-platform-secret",
        )
        assert reconnected == connected
        assert gateway.list_connections() == (connected,)
        assert os.stat(state_path / "credentials").st_mode & 0o777 == 0o700
        assert (
            os.stat(next((state_path / "credentials").glob("openai-*.key"))).st_mode & 0o777
            == 0o600
        )
        assert gateway.connection_status("work-openai") == connected
        route = gateway.create_route("work-openai", consumer="api-client")
        assert route.connection_name == "work-openai"
        assert route.wire_api == "responses"
        assert route.base_url == f"http://127.0.0.1:{port}/v1"
        assert _model_status(route.base_url, route.api_key) == 200
        assert gateway.revoke_route(route.id) is True
        assert gateway.disconnect_connection("work-openai") is True
        assert gateway.list_connections() == ()
        assert gateway.disconnect_connection("work-openai") is False
        gateway.shutdown()

    assert os.stat(state_path / "connections.json").st_mode & 0o777 == 0o600


def test_chatgpt_subscription_connect_exposes_only_a_typed_device_challenge(
    tmp_path,
) -> None:
    executable = tmp_path / "provider-broker"
    _write_test_broker(executable)
    state_path = tmp_path / "gateway"
    port = _free_port()
    challenges: list[DeviceAuthorization] = []

    with ProviderGateway.open(
        state_path=state_path,
        executable_path=executable,
        port=port,
    ) as gateway:
        connected = gateway.connect_chatgpt_subscription(
            name="personal-chatgpt",
            on_authorization=challenges.append,
        )

        assert connected == ProviderConnection(
            name="personal-chatgpt",
            provider="chatgpt",
            auth_mode="subscription",
            status="connected",
        )
        assert gateway.list_connections() == (connected,)
        assert (state_path / "broker-starts").read_text().splitlines() == [
            "started",
            "started",
        ]
        assert gateway.disconnect_connection("personal-chatgpt") is True
        gateway.shutdown()

    assert challenges == [
        DeviceAuthorization(
            verification_url="https://auth.openai.com/codex/device",
            user_code="ABCD-EFGH",
        )
    ]
    assert "ABCD-EFGH" not in repr(challenges[0])
    assert not list((state_path / "auth").glob("*.json"))


def test_chatgpt_connect_adopts_one_existing_unnamed_bundle_without_reading_it(
    tmp_path,
) -> None:
    executable = tmp_path / "provider-broker"
    _write_test_broker(executable)
    state_path = tmp_path / "gateway"
    auth_path = state_path / "auth"
    auth_path.mkdir(parents=True)
    existing = auth_path / "codex-existing-private-account.json"
    existing.write_text("opaque credential content that must not be parsed")
    challenges: list[DeviceAuthorization] = []

    with ProviderGateway.open(
        state_path=state_path,
        executable_path=executable,
        port=_free_port(),
    ) as gateway:
        adopted = gateway.connect_chatgpt_subscription(
            name="personal-chatgpt",
            on_authorization=challenges.append,
        )

        assert adopted.name == "personal-chatgpt"
        assert challenges == []
        assert gateway.list_connections() == (adopted,)
        route = gateway.create_route("personal-chatgpt", consumer="test-client")
        assert _model_status(route.base_url, route.api_key) == 200
        gateway.revoke_route(route.id)
        gateway.disconnect_connection("personal-chatgpt")
        gateway.shutdown()

    assert not existing.exists()


def test_stale_subscription_status_blocks_routes_with_named_reconnect_remediation(
    tmp_path,
) -> None:
    executable = tmp_path / "provider-broker"
    _write_test_broker(executable)
    state_path = tmp_path / "gateway"
    port = _free_port()

    with ProviderGateway.open(
        state_path=state_path,
        executable_path=executable,
        port=port,
    ) as gateway:
        gateway.connect_chatgpt_subscription(
            name="personal-chatgpt",
            on_authorization=lambda challenge: None,
        )
        next((state_path / "auth").glob("codex-dorf-*.json")).unlink()

        stale = gateway.connection_status("personal-chatgpt")
        assert stale == ProviderConnection(
            name="personal-chatgpt",
            provider="chatgpt",
            auth_mode="subscription",
            status="authentication_stale",
            remediation=(
                "Run: dorf provider connect chatgpt --subscription --name personal-chatgpt"
            ),
        )
        with pytest.raises(ProviderAuthenticationStaleError) as caught:
            gateway.create_route("personal-chatgpt", consumer="test-client")

        assert caught.value.needs_human is True
        assert caught.value.connection_name == "personal-chatgpt"
        assert caught.value.remediation == stale.remediation
        assert not (state_path / "routes.json").exists()

        restored = gateway.connect_chatgpt_subscription(
            name="personal-chatgpt",
            on_authorization=lambda challenge: None,
        )
        assert restored.status == "connected"
        assert (state_path / "broker-starts").read_text().splitlines() == [
            "started",
            "started",
            "started",
        ]
        assert gateway.connection_status("personal-chatgpt").status == "connected"
        gateway.disconnect_connection("personal-chatgpt")
        gateway.shutdown()


def test_connection_status_reports_safe_plan_and_transient_upstream_unavailability(
    tmp_path,
) -> None:
    executable = tmp_path / "provider-broker"
    _write_test_broker(executable)
    state_path = tmp_path / "gateway"
    port = _free_port()

    with ProviderGateway.open(
        state_path=state_path,
        executable_path=executable,
        port=port,
    ) as gateway:
        gateway.connect_chatgpt_subscription(
            name="personal-chatgpt",
            on_authorization=lambda challenge: None,
        )

        healthy = gateway.connection_status("personal-chatgpt")
        assert healthy.status == "connected"
        assert healthy.plan == "pro"
        assert "private-account-identifier" not in repr(healthy)

        (state_path / "fake-auth-status.json").write_text(
            json.dumps({"status": "error", "unavailable": True})
        )
        unavailable = gateway.connection_status("personal-chatgpt")
        assert unavailable.status == "upstream_unavailable"
        assert unavailable.remediation == "Try personal-chatgpt again later"

        with pytest.raises(ProviderUpstreamUnavailableError) as caught:
            gateway.create_route("personal-chatgpt", consumer="test-client")
        assert caught.value.needs_human is False
        assert caught.value.connection_name == "personal-chatgpt"
        assert caught.value.remediation == unavailable.remediation
        assert not (state_path / "routes.json").exists()
        gateway.disconnect_connection("personal-chatgpt")
        gateway.shutdown()


def test_generic_backend_error_does_not_turn_valid_subscription_into_stale_auth(
    tmp_path,
) -> None:
    executable = tmp_path / "provider-broker"
    _write_test_broker(executable)
    state_path = tmp_path / "gateway"
    port = _free_port()

    with ProviderGateway.open(
        state_path=state_path,
        executable_path=executable,
        port=port,
    ) as gateway:
        gateway.connect_chatgpt_subscription(
            name="personal-chatgpt",
            on_authorization=lambda challenge: None,
        )
        (state_path / "fake-auth-status.json").write_text(
            json.dumps({"status": "error", "unavailable": False})
        )

        connection = gateway.connection_status("personal-chatgpt")
        assert connection.status == "connected"
        assert connection.remediation is None
        route = gateway.create_route(
            "personal-chatgpt",
            consumer="healthy-model-after-transient-error",
        )
        assert _model_status(route.base_url, route.api_key) == 200

        gateway.revoke_route(route.id)
        gateway.disconnect_connection("personal-chatgpt")
        gateway.shutdown()


def test_route_rejects_an_incompatible_consumer_wire_before_issuing_a_key(
    tmp_path,
) -> None:
    executable = tmp_path / "provider-broker"
    _write_test_broker(executable)
    state_path = tmp_path / "gateway"
    port = _free_port()

    with ProviderGateway.open(
        state_path=state_path,
        executable_path=executable,
        port=port,
    ) as gateway:
        gateway.connect_chatgpt_subscription(
            name="personal-chatgpt",
            on_authorization=lambda challenge: None,
        )

        with pytest.raises(ConsumerWireIncompatibleError) as caught:
            gateway.create_route(
                "personal-chatgpt",
                consumer="test-client",
                wire_api="chat_completions",
            )

        assert caught.value.connection_name == "personal-chatgpt"
        assert caught.value.requested_wire == "chat_completions"
        assert caught.value.remediation == "Use a Responses API consumer"
        assert not (state_path / "routes.json").exists()
        gateway.disconnect_connection("personal-chatgpt")
        gateway.shutdown()


def test_missing_connection_blocks_route_state_with_needs_human_connect_guidance(
    tmp_path,
) -> None:
    executable = tmp_path / "provider-broker"
    _write_test_broker(executable)
    state_path = tmp_path / "gateway"

    with ProviderGateway.open(
        state_path=state_path,
        executable_path=executable,
        port=_free_port(),
    ) as gateway:
        with pytest.raises(ProviderConnectionNotFoundError) as caught:
            gateway.create_route("personal-chatgpt", consumer="future-room")

    assert caught.value.connection_name == "personal-chatgpt"
    assert caught.value.needs_human is True
    assert caught.value.remediation == "Run: dorf provider connect --help"
    assert not (state_path / "routes.json").exists()


def test_disconnect_revokes_every_route_owned_by_the_connection(
    tmp_path,
) -> None:
    executable = tmp_path / "provider-broker"
    _write_test_broker(executable)
    state_path = tmp_path / "gateway"
    port = _free_port()

    with ProviderGateway.open(
        state_path=state_path,
        executable_path=executable,
        port=port,
    ) as gateway:
        gateway.connect_chatgpt_subscription(
            name="personal-chatgpt",
            on_authorization=lambda challenge: None,
        )
        route = gateway.create_route("personal-chatgpt", consumer="test-client")
        assert _model_status(route.base_url, route.api_key) == 200

        assert gateway.disconnect_connection("personal-chatgpt") is True
        assert gateway.list_connections() == ()
        assert _model_status(route.base_url, route.api_key) == 401
        assert json.loads((state_path / "routes.json").read_text()) == []
        gateway.shutdown()


def test_chatgpt_and_prefixed_deepseek_can_coexist(tmp_path) -> None:
    executable = tmp_path / "provider-broker"
    _write_test_broker(executable)
    state_path = tmp_path / "gateway"
    with ProviderGateway.open(
        state_path=state_path, executable_path=executable, port=_free_port()
    ) as gateway:
        gateway.connect_chatgpt_subscription(
            name="chatgpt", on_authorization=lambda challenge: None
        )
        gateway.connect_deepseek_api_key(name="deepseek", api_key="secret")
        assert gateway.create_route("chatgpt", consumer="coder")
        assert gateway.create_route("deepseek", consumer="reviewer")
        config = (state_path / "broker.yaml").read_text()
    assert "force-model-prefix: true" in config
    assert "openai-compatibility:" in config
    assert 'base-url: "https://api.deepseek.com/v1"' in config
    assert 'prefix: "deepseek"' in config
    assert 'alias: "deepseek-v4-flash"' in config


def test_route_fails_closed_instead_of_implicitly_pooling_multiple_connections(
    tmp_path,
) -> None:
    executable = tmp_path / "provider-broker"
    _write_test_broker(executable)
    state_path = tmp_path / "gateway"
    port = _free_port()

    with ProviderGateway.open(
        state_path=state_path,
        executable_path=executable,
        port=port,
    ) as gateway:
        for name in ("chatgpt-one", "chatgpt-two"):
            gateway.connect_chatgpt_subscription(
                name=name,
                on_authorization=lambda challenge: None,
            )
        connections = gateway.list_connections()

        with pytest.raises(
            ProviderSelectionUnsupportedError,
            match="Multiple provider connections are not supported",
        ):
            gateway.create_route("chatgpt-one", consumer="test-client")
        gateway.shutdown()

    assert len(connections) == 2
    assert not (state_path / "routes.json").exists()


def _model_status(base_url: str, api_key: str | None = None) -> int:
    request = urllib.request.Request(f"{base_url}/models")
    if api_key is not None:
        request.add_header("Authorization", f"Bearer {api_key}")
    try:
        with urllib.request.urlopen(request, timeout=1) as response:
            return response.status
    except urllib.error.HTTPError as error:
        return error.code
