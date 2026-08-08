from __future__ import annotations

import json

import pytest

from dorf.github_app import (
    GitHubAppConfig,
    GitHubAppManifestFlow,
    GitHubAppManifestFlowError,
    GitHubAppTokenClient,
    GitHubRepositoryClient,
    GitHubRepositoryError,
    _Redirect,
    load_github_app_config,
    save_github_app_config,
)


class Response:
    def __enter__(self):
        return self

    def __exit__(self, *args):
        pass

    def read(self):
        return b'{"token":"scoped","permissions":{"contents":"read"}}'


class FakeManifestClient:
    def convert_code(self, code: str):
        assert code == "manifest-code"
        return {
            "id": 123,
            "slug": "dorf-local",
            "pem": "-----BEGIN PRIVATE KEY-----\ntest\n-----END PRIVATE KEY-----\n",
        }


class FakeTokenClient(GitHubAppTokenClient):
    def __init__(self) -> None:
        self.seen: list[GitHubAppConfig] = []

    def mint_installation_token(
        self,
        config: GitHubAppConfig,
        *,
        config_home=None,
        private_key_path=None,
    ) -> str:
        self.seen.append(config)
        if private_key_path is not None:
            assert private_key_path.read_text().startswith("-----BEGIN PRIVATE KEY-----")
        return "installation-token"


def test_installation_token_can_be_repository_read_only(monkeypatch, tmp_path) -> None:
    seen = {}
    monkeypatch.setattr("dorf.github_app.create_github_app_jwt", lambda *args: "jwt")
    def open_request(request, timeout):
        seen["body"] = request.data
        return Response()

    monkeypatch.setattr("dorf.github_app.urllib.request.urlopen", open_request)
    token = GitHubAppTokenClient().mint_installation_token(
        GitHubAppConfig("1", "2"),
        private_key_path=tmp_path / "unused.pem",
        repositories=["repo"],
        permissions={"contents": "read"},
    )
    assert token.token == "scoped"
    assert json.loads(seen["body"]) == {
        "repositories": ["repo"],
        "permissions": {"contents": "read"},
    }


def test_repository_client_get_issue_includes_non_empty_comments(monkeypatch) -> None:
    client = GitHubRepositoryClient("installation-token")
    first_page = [{"body": "First useful clarification."}]
    first_page.extend({"body": "   "} for _ in range(99))
    responses = {
        "/repos/example/repo/issues/62": {
            "number": 62,
            "title": "One primary agent",
            "body": "Build the issue-backed start path.",
        },
        "/repos/example/repo/issues/62/comments?per_page=100&page=1": first_page,
        "/repos/example/repo/issues/62/comments?per_page=100&page=2": [
            {"body": "Second useful clarification."},
        ],
    }
    monkeypatch.setattr(client, "_request_json", lambda method, path: responses[path])

    issue = client.get_issue("example/repo", 62)

    assert issue.number == 62
    assert issue.title == "One primary agent"
    assert issue.body == "Build the issue-backed start path."
    assert issue.comments == (
        "First useful clarification.",
        "Second useful clarification.",
    )


def test_repository_client_get_issue_rejects_pull_request_payload(monkeypatch) -> None:
    client = GitHubRepositoryClient("installation-token")
    responses = {
        "/repos/example/repo/issues/62": {
            "number": 62,
            "title": "A pull request",
            "body": "This is not an issue-backed task.",
            "pull_request": {"url": "https://api.github.com/repos/example/repo/pulls/62"},
        },
        "/repos/example/repo/issues/62/comments?per_page=100&page=1": [],
    }
    monkeypatch.setattr(client, "_request_json", lambda method, path: responses[path])

    with pytest.raises(GitHubRepositoryError, match="#62 is a pull request, not an issue"):
        client.get_issue("example/repo", 62)


def test_manifest_requests_read_access_to_issues() -> None:
    permissions = GitHubAppManifestFlow().manifest()["default_permissions"]

    assert permissions == {
        "metadata": "read",
        "contents": "write",
        "pull_requests": "write",
        "issues": "read",
    }


def test_manifest_flow_callback_converts_code_then_install_callback_stores_config(
    tmp_path,
    monkeypatch,
) -> None:
    monkeypatch.setenv("XDG_CONFIG_HOME", str(tmp_path / "config"))
    token_client = FakeTokenClient()
    flow = GitHubAppManifestFlow(client=FakeManifestClient(), token_client=token_client)
    flow._server = type("Server", (), {"server_address": ("127.0.0.1", 49152)})()

    with pytest.raises(_Redirect) as redirect:
        flow.handle_callback({"state": [flow.state], "code": ["manifest-code"]})

    assert redirect.value.url == (
        f"https://github.com/apps/dorf-local/installations/select_target?state={flow.state}"
    )

    flow.handle_installed(
        {
            "state": [flow.state],
            "installation_id": ["456"],
            "setup_action": ["install"],
        }
    )

    config = load_github_app_config(config_home=tmp_path / "config")
    assert config == GitHubAppConfig(
        app_id="123",
        installation_id="456",
        app_slug="dorf-local",
    )
    assert token_client.seen == [config]


def test_manifest_flow_does_not_persist_config_before_installation(
    tmp_path,
    monkeypatch,
) -> None:
    monkeypatch.setenv("XDG_CONFIG_HOME", str(tmp_path / "config"))
    existing = GitHubAppConfig(
        app_id="old-app",
        installation_id="old-installation",
        app_slug="old-dorf",
    )
    save_github_app_config(existing, "old-private-key", config_home=tmp_path / "config")
    flow = GitHubAppManifestFlow(client=FakeManifestClient(), token_client=FakeTokenClient())
    flow._server = type("Server", (), {"server_address": ("127.0.0.1", 49152)})()

    with pytest.raises(_Redirect):
        flow.handle_callback({"state": [flow.state], "code": ["manifest-code"]})

    assert load_github_app_config(config_home=tmp_path / "config") == existing

    flow.handle_installed({"state": [flow.state], "setup_action": ["request"]})

    assert load_github_app_config(config_home=tmp_path / "config") == existing
    assert isinstance(flow._error, GitHubAppManifestFlowError)


def test_manifest_flow_rejects_install_callback_state_mismatch(
    tmp_path,
    monkeypatch,
) -> None:
    monkeypatch.setenv("XDG_CONFIG_HOME", str(tmp_path / "config"))
    flow = GitHubAppManifestFlow(client=FakeManifestClient(), token_client=FakeTokenClient())
    flow._server = type("Server", (), {"server_address": ("127.0.0.1", 49152)})()

    with pytest.raises(_Redirect):
        flow.handle_callback({"state": [flow.state], "code": ["manifest-code"]})

    flow.handle_installed(
        {
            "state": ["wrong"],
            "installation_id": ["456"],
            "setup_action": ["install"],
        }
    )

    assert isinstance(flow._error, GitHubAppManifestFlowError)
    assert "state mismatch" in str(flow._error)
    with pytest.raises(Exception, match="metadata not found"):
        load_github_app_config(config_home=tmp_path / "config")


def test_manifest_flow_rejects_callback_state_mismatch() -> None:
    flow = GitHubAppManifestFlow(client=FakeManifestClient(), token_client=FakeTokenClient())

    flow.handle_callback({"state": ["wrong"], "code": ["manifest-code"]})

    assert isinstance(flow._error, GitHubAppManifestFlowError)
    assert "state mismatch" in str(flow._error)


def test_manifest_flow_opens_browser_and_prints_fallback_url(monkeypatch) -> None:
    opened: list[str] = []

    monkeypatch.setattr(
        "dorf.github_app.webbrowser.open",
        lambda url: opened.append(url) or True,
    )
    flow = GitHubAppManifestFlow()
    flow._server = type("Server", (), {"server_address": ("127.0.0.1", 49152)})()
    messages: list[str] = []

    start_url = flow.local_url("/start")
    assert opened == []

    assert flow.announce_start(messages.append, start_url) is None
    assert opened == [start_url]
    assert "Opened GitHub App setup in your browser" in messages[0]
    assert start_url in messages[0]
