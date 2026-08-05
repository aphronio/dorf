from __future__ import annotations

import base64
import html
import json
import os
import secrets
import stat
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
import webbrowser
from contextlib import contextmanager
from dataclasses import dataclass
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any

GITHUB_APP_DIRNAME = "github-app"
GITHUB_APP_METADATA_FILENAME = "app.json"
GITHUB_APP_PRIVATE_KEY_FILENAME = "private-key.pem"
GITHUB_API_URL = "https://api.github.com"
GITHUB_WEB_URL = "https://github.com"


@dataclass(frozen=True)
class GitHubAppConfig:
    app_id: str
    installation_id: str
    app_slug: str | None = None


@dataclass(frozen=True)
class GitHubInstallationToken:
    token: str
    expires_at: str | None = None
    permissions: dict[str, str] | None = None


@dataclass(frozen=True)
class GitHubIssue:
    number: int
    title: str
    body: str
    comments: tuple[str, ...]


@dataclass(frozen=True)
class GitHubAppPaths:
    directory: Path
    metadata_path: Path
    private_key_path: Path


class GitHubAppConfigError(RuntimeError):
    pass


class GitHubAppVerificationError(RuntimeError):
    pass


class GitHubAppManifestFlowError(RuntimeError):
    pass


class GitHubRepositoryError(RuntimeError):
    pass


@contextmanager
def temporary_private_key(private_key: str):
    with tempfile.NamedTemporaryFile("w", delete=True) as key_file:
        key_path = Path(key_file.name)
        key_file.write(private_key)
        key_file.flush()
        key_path.chmod(0o600)
        yield key_path


def github_app_paths(config_home: Path | None = None) -> GitHubAppPaths:
    root = config_home or default_config_home()
    directory = root / "dorf" / GITHUB_APP_DIRNAME
    return GitHubAppPaths(
        directory=directory,
        metadata_path=directory / GITHUB_APP_METADATA_FILENAME,
        private_key_path=directory / GITHUB_APP_PRIVATE_KEY_FILENAME,
    )


def default_config_home() -> Path:
    configured = os.environ.get("XDG_CONFIG_HOME")
    if configured:
        return Path(configured)
    return Path.home() / ".config"


def save_github_app_config(
    config: GitHubAppConfig,
    private_key: str,
    *,
    config_home: Path | None = None,
) -> GitHubAppPaths:
    paths = github_app_paths(config_home)
    paths.directory.mkdir(parents=True, exist_ok=True)
    paths.directory.chmod(0o700)
    metadata = {
        "app_id": config.app_id,
        "installation_id": config.installation_id,
    }
    if config.app_slug is not None:
        metadata["app_slug"] = config.app_slug
    replace_file_atomically(
        paths.metadata_path,
        json.dumps(metadata, indent=2, sort_keys=True) + "\n",
        mode=0o600,
    )
    replace_file_atomically(paths.private_key_path, private_key, mode=0o600)
    return paths


def replace_file_atomically(path: Path, content: str, *, mode: int) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.NamedTemporaryFile(
        "w",
        dir=path.parent,
        delete=False,
        prefix=f".{path.name}.",
    ) as temp_file:
        temp_path = Path(temp_file.name)
        temp_file.write(content)
        temp_file.flush()
        os.fsync(temp_file.fileno())
    temp_path.chmod(mode)
    temp_path.replace(path)


def load_github_app_config(*, config_home: Path | None = None) -> GitHubAppConfig:
    paths = github_app_paths(config_home)
    if not paths.metadata_path.is_file():
        raise GitHubAppConfigError(f"GitHub App metadata not found: {paths.metadata_path}")
    if not paths.private_key_path.is_file():
        raise GitHubAppConfigError(f"GitHub App private key not found: {paths.private_key_path}")
    try:
        data = json.loads(paths.metadata_path.read_text())
    except json.JSONDecodeError as error:
        raise GitHubAppConfigError(f"Invalid GitHub App metadata: {error}") from error
    app_id = data.get("app_id")
    installation_id = data.get("installation_id")
    app_slug = data.get("app_slug")
    if not isinstance(app_id, str) or not app_id:
        raise GitHubAppConfigError("GitHub App metadata is missing app_id")
    if not isinstance(installation_id, str) or not installation_id:
        raise GitHubAppConfigError("GitHub App metadata is missing installation_id")
    if app_slug is not None and not isinstance(app_slug, str):
        raise GitHubAppConfigError("GitHub App metadata app_slug must be a string")
    return GitHubAppConfig(app_id=app_id, installation_id=installation_id, app_slug=app_slug)


def private_key_permissions_are_locked_down(*, config_home: Path | None = None) -> bool:
    paths = github_app_paths(config_home)
    mode = stat.S_IMODE(paths.private_key_path.stat().st_mode)
    return mode & 0o077 == 0


class GitHubAppTokenClient:
    def __init__(self, *, api_url: str = GITHUB_API_URL) -> None:
        self.api_url = api_url.rstrip("/")

    def mint_installation_token(
        self,
        config: GitHubAppConfig,
        *,
        config_home: Path | None = None,
        private_key_path: Path | None = None,
    ) -> GitHubInstallationToken:
        key_path = private_key_path or github_app_paths(config_home).private_key_path
        jwt = create_github_app_jwt(config.app_id, key_path)
        request = urllib.request.Request(
            f"{self.api_url}/app/installations/{config.installation_id}/access_tokens",
            method="POST",
            headers={
                "Accept": "application/vnd.github+json",
                "Authorization": f"Bearer {jwt}",
                "User-Agent": "dorf",
                "X-GitHub-Api-Version": "2022-11-28",
            },
        )
        try:
            with urllib.request.urlopen(request, timeout=30) as response:
                payload = json.loads(response.read().decode("utf-8"))
        except urllib.error.HTTPError as error:
            body = error.read().decode("utf-8", errors="replace")
            raise GitHubAppVerificationError(
                f"GitHub token request failed: HTTP {error.code}: {body}"
            ) from error
        except OSError as error:
            raise GitHubAppVerificationError(f"GitHub token request failed: {error}") from error
        token = payload.get("token")
        if not isinstance(token, str) or not token:
            raise GitHubAppVerificationError("GitHub token response did not include a token")
        expires_at = payload.get("expires_at")
        if expires_at is not None and not isinstance(expires_at, str):
            expires_at = None
        permissions = payload.get("permissions")
        if not isinstance(permissions, dict) or not all(
            isinstance(name, str) and isinstance(level, str)
            for name, level in permissions.items()
        ):
            permissions = None
        return GitHubInstallationToken(
            token=token,
            expires_at=expires_at,
            permissions=permissions,
        )


class GitHubRepositoryClient:
    def __init__(self, token: str, *, api_url: str = GITHUB_API_URL) -> None:
        self.token = token
        self.api_url = api_url.rstrip("/")

    def get_branch_sha(self, repo_full_name: str, branch: str) -> str:
        payload = self._request_json(
            "GET",
            f"/repos/{repo_full_name}/git/ref/heads/{urllib.parse.quote(branch, safe='/')}",
        )
        obj = payload.get("object")
        if not isinstance(obj, dict):
            raise GitHubRepositoryError("GitHub branch response did not include an object")
        sha = obj.get("sha")
        if not isinstance(sha, str) or not sha:
            raise GitHubRepositoryError("GitHub branch response did not include a SHA")
        return sha

    def get_repository_permissions(self, repo_full_name: str) -> dict[str, bool]:
        """Return the token's exact repository-level authority flags."""
        payload = self._request_json("GET", f"/repos/{repo_full_name}")
        permissions = payload.get("permissions") if isinstance(payload, dict) else None
        if not isinstance(permissions, dict):
            raise GitHubRepositoryError(
                "GitHub repository response did not include token permissions"
            )
        return {
            name: value
            for name, value in permissions.items()
            if isinstance(name, str) and isinstance(value, bool)
        }

    def get_issue(self, repo_full_name: str, issue_number: int) -> GitHubIssue:
        payload = self._request_json("GET", f"/repos/{repo_full_name}/issues/{issue_number}")
        if not isinstance(payload, dict):
            raise GitHubRepositoryError("GitHub issue response was not an object")
        if "pull_request" in payload:
            raise GitHubRepositoryError(
                f"GitHub number #{issue_number} is a pull request, not an issue"
            )
        title = payload.get("title")
        if not isinstance(title, str) or not title.strip():
            raise GitHubRepositoryError("GitHub issue response did not include a title")
        body = payload.get("body")
        if body is None:
            body = ""
        if not isinstance(body, str):
            raise GitHubRepositoryError("GitHub issue body was not a string")
        comments_payload = self._request_json_pages(
            f"/repos/{repo_full_name}/issues/{issue_number}/comments",
        )
        if not isinstance(comments_payload, list):
            raise GitHubRepositoryError("GitHub issue comment response was not a list")
        comments = tuple(
            comment_body.strip()
            for comment in comments_payload
            if isinstance(comment, dict)
            and isinstance((comment_body := comment.get("body")), str)
            and comment_body.strip()
        )
        return GitHubIssue(
            number=issue_number,
            title=title.strip(),
            body=body.strip(),
            comments=comments,
        )

    def _request_json_pages(self, path: str) -> list[Any]:
        items: list[Any] = []
        page = 1
        while True:
            payload = self._request_json("GET", f"{path}?per_page=100&page={page}")
            if not isinstance(payload, list):
                raise GitHubRepositoryError("GitHub paginated response was not a list")
            items.extend(payload)
            if len(payload) < 100:
                return items
            page += 1

    def create_branch(self, repo_full_name: str, branch: str, sha: str) -> None:
        self._request_json(
            "POST",
            f"/repos/{repo_full_name}/git/refs",
            body={"ref": f"refs/heads/{branch}", "sha": sha},
        )

    def delete_branch(self, repo_full_name: str, branch: str) -> None:
        self._request_json(
            "DELETE",
            f"/repos/{repo_full_name}/git/refs/heads/{urllib.parse.quote(branch, safe='/')}",
        )

    def list_pull_requests_for_branch(
        self,
        repo_full_name: str,
        branch: str,
        *,
        state: str = "open",
    ) -> list[dict[str, Any]]:
        owner = repo_full_name.split("/", 1)[0]
        query = urllib.parse.urlencode({"state": state, "head": f"{owner}:{branch}"})
        payload = self._request_json("GET", f"/repos/{repo_full_name}/pulls?{query}")
        if not isinstance(payload, list):
            raise GitHubRepositoryError("GitHub pull request list response was not a list")
        return [item for item in payload if isinstance(item, dict)]

    def get_pull_request(self, repo_full_name: str, pull_number: int) -> dict[str, Any]:
        return self._request_json("GET", f"/repos/{repo_full_name}/pulls/{pull_number}")

    def create_pull_request(
        self,
        repo_full_name: str,
        *,
        title: str,
        body: str,
        head: str,
        base: str,
        draft: bool,
    ) -> dict[str, Any]:
        return self._request_json(
            "POST",
            f"/repos/{repo_full_name}/pulls",
            body={
                "title": title,
                "body": body,
                "head": head,
                "base": base,
                "draft": draft,
            },
        )

    def update_pull_request(
        self,
        repo_full_name: str,
        pull_number: int,
        *,
        title: str,
        body: str,
        base: str,
    ) -> dict[str, Any]:
        return self._request_json(
            "PATCH",
            f"/repos/{repo_full_name}/pulls/{pull_number}",
            body={
                "title": title,
                "body": body,
                "base": base,
            },
        )

    def mark_pull_request_ready(self, repo_full_name: str, pull_number: int) -> None:
        pull = self.get_pull_request(repo_full_name, pull_number)
        if pull.get("draft") is not True:
            return
        node_id = pull.get("node_id")
        if not isinstance(node_id, str) or not node_id:
            return
        self._request_json(
            "POST",
            "/graphql",
            body={
                "query": (
                    "mutation($id: ID!) { "
                    "markPullRequestReadyForReview(input: {pullRequestId: $id}) { "
                    "pullRequest { number } } }"
                ),
                "variables": {"id": node_id},
            },
        )

    def add_pull_request_comment(self, repo_full_name: str, pr_number: int, body: str) -> None:
        self._request_json(
            "POST",
            f"/repos/{repo_full_name}/issues/{pr_number}/comments",
            body={"body": body},
        )

    def add_pull_request_review_reply(
        self,
        repo_full_name: str,
        pr_number: int,
        comment_id: int,
        body: str,
    ) -> None:
        self._request_json(
            "POST",
            f"/repos/{repo_full_name}/pulls/{pr_number}/comments/{comment_id}/replies",
            body={"body": body},
        )

    def list_pull_request_comments(
        self,
        repo_full_name: str,
        pr_number: int,
    ) -> list[dict[str, Any]]:
        payload = self._request_json("GET", f"/repos/{repo_full_name}/issues/{pr_number}/comments")
        if not isinstance(payload, list):
            raise GitHubRepositoryError("GitHub PR comment response was not a list")
        return [item for item in payload if isinstance(item, dict)]

    def graphql(self, query: str, variables: dict[str, Any]) -> dict[str, Any]:
        payload = self._request_json(
            "POST",
            "/graphql",
            body={"query": query, "variables": variables},
        )
        if not isinstance(payload, dict):
            raise GitHubRepositoryError("GitHub GraphQL response was not an object")
        return payload

    def _request_json(
        self,
        method: str,
        path: str,
        *,
        body: dict[str, Any] | None = None,
    ) -> Any:
        data = None
        headers = {
            "Accept": "application/vnd.github+json",
            "Authorization": f"Bearer {self.token}",
            "User-Agent": "dorf",
            "X-GitHub-Api-Version": "2022-11-28",
        }
        if body is not None:
            data = json.dumps(body).encode("utf-8")
            headers["Content-Type"] = "application/json"
        request = urllib.request.Request(
            f"{self.api_url}{path}",
            method=method,
            headers=headers,
            data=data,
        )
        try:
            with urllib.request.urlopen(request, timeout=30) as response:
                raw = response.read().decode("utf-8")
        except urllib.error.HTTPError as error:
            body_text = error.read().decode("utf-8", errors="replace")
            raise GitHubRepositoryError(
                f"GitHub repository request failed: HTTP {error.code}: {body_text}"
            ) from error
        except OSError as error:
            raise GitHubRepositoryError(f"GitHub repository request failed: {error}") from error
        if not raw:
            return {}
        return json.loads(raw)


class GitHubAppManifestClient:
    def __init__(self, *, api_url: str = GITHUB_API_URL) -> None:
        self.api_url = api_url.rstrip("/")

    def convert_code(self, code: str) -> dict[str, Any]:
        request = urllib.request.Request(
            f"{self.api_url}/app-manifests/{urllib.parse.quote(code)}/conversions",
            method="POST",
            headers={
                "Accept": "application/vnd.github+json",
                "User-Agent": "dorf",
                "X-GitHub-Api-Version": "2022-11-28",
            },
        )
        try:
            with urllib.request.urlopen(request, timeout=30) as response:
                return json.loads(response.read().decode("utf-8"))
        except urllib.error.HTTPError as error:
            body = error.read().decode("utf-8", errors="replace")
            raise GitHubAppManifestFlowError(
                f"GitHub manifest conversion failed: HTTP {error.code}: {body}"
            ) from error
        except OSError as error:
            raise GitHubAppManifestFlowError(
                f"GitHub manifest conversion failed: {error}"
            ) from error


@dataclass(frozen=True)
class GitHubAppManifestFlowResult:
    config: GitHubAppConfig
    paths: GitHubAppPaths


@dataclass(frozen=True)
class ConvertedGitHubAppManifest:
    app_id: str
    app_slug: str
    private_key: str


class GitHubAppManifestFlow:
    def __init__(
        self,
        *,
        web_url: str = GITHUB_WEB_URL,
        host: str = "127.0.0.1",
        port: int = 0,
        org: str | None = None,
        client: GitHubAppManifestClient | None = None,
        token_client: GitHubAppTokenClient | None = None,
        timeout_seconds: int = 3600,
    ) -> None:
        self.web_url = web_url.rstrip("/")
        self.host = host
        self.port = port
        self.org = org
        self.client = client or GitHubAppManifestClient()
        self.token_client = token_client or GitHubAppTokenClient()
        self.timeout_seconds = timeout_seconds
        self.state = secrets.token_urlsafe(24)
        self.app_name = f"Dorf Local {secrets.token_hex(4)}"
        self._done = threading.Event()
        self._result: GitHubAppManifestFlowResult | None = None
        self._error: Exception | None = None
        self._server: ThreadingHTTPServer | None = None
        self._converted_app: ConvertedGitHubAppManifest | None = None

    def run(self, *, announce) -> GitHubAppManifestFlowResult:
        handler = self._make_handler()
        with ThreadingHTTPServer((self.host, self.port), handler) as server:
            self._server = server
            thread = threading.Thread(target=server.serve_forever, daemon=True)
            thread.start()
            start_url = self.local_url("/start")
            self.announce_start(announce, start_url)
            if not self._done.wait(self.timeout_seconds):
                raise GitHubAppManifestFlowError("Timed out waiting for GitHub App setup")
            server.shutdown()
            thread.join(timeout=5)
        if self._error is not None:
            raise GitHubAppManifestFlowError(str(self._error)) from self._error
        if self._result is None:
            raise GitHubAppManifestFlowError("GitHub App setup did not complete")
        return self._result

    def announce_start(self, announce, start_url: str) -> None:
        if webbrowser.open(start_url):
            announce(
                "Opened GitHub App setup in your browser. "
                f"If it did not open, use this URL:\n{start_url}"
            )
            return
        announce(f"Open this URL to create and install the Dorf GitHub App:\n{start_url}")

    def local_url(self, path: str) -> str:
        if self._server is None:
            port = self.port
        else:
            port = self._server.server_address[1]
        return f"http://{self.host}:{port}{path}"

    def github_new_app_url(self) -> str:
        if self.org:
            org = urllib.parse.quote(self.org)
            return f"{self.web_url}/organizations/{org}/settings/apps/new"
        return f"{self.web_url}/settings/apps/new"

    def manifest(self) -> dict[str, Any]:
        return {
            "name": self.app_name,
            "url": "https://github.com",
            "redirect_url": self.local_url("/callback"),
            "callback_urls": [self.local_url("/installed")],
            "setup_url": self.local_url("/installed"),
            "setup_on_update": True,
            "public": False,
            "default_permissions": {
                "metadata": "read",
                "contents": "write",
                "pull_requests": "write",
                "issues": "read",
            },
            "default_events": [],
        }

    def install_url(self, slug: str) -> str:
        return (
            f"{self.web_url}/apps/{urllib.parse.quote(slug)}/installations/select_target"
            f"?state={urllib.parse.quote(self.state)}"
        )

    def _make_handler(self):
        flow = self

        class Handler(BaseHTTPRequestHandler):
            def do_GET(self) -> None:
                parsed = urllib.parse.urlparse(self.path)
                query = urllib.parse.parse_qs(parsed.query)
                if parsed.path == "/start":
                    self._send_html(flow.start_page())
                    return
                if parsed.path == "/callback":
                    try:
                        flow.handle_callback(query)
                    except _Redirect as redirect:
                        self.send_response(302)
                        self.send_header("Location", redirect.url)
                        self.end_headers()
                    return
                if parsed.path == "/installed":
                    flow.handle_installed(query)
                    self._send_html("<h1>Dorf GitHub App setup complete.</h1>")
                    return
                self.send_error(404)

            def log_message(self, format: str, *args) -> None:
                return

            def _send_html(self, body: str) -> None:
                content = (
                    "<!doctype html><html><head><meta charset='utf-8'>"
                    "<meta name='viewport' content='width=device-width, initial-scale=1'>"
                    "<title>Dorf GitHub Setup</title>"
                    "<style>"
                    ":root{color-scheme:light dark}"
                    "body{font-family:system-ui,-apple-system,BlinkMacSystemFont,"
                    "'Segoe UI',sans-serif;"
                    "margin:0;min-height:100vh;display:grid;place-items:center;"
                    "background:#f6f7f9;color:#161b22}"
                    "main{width:min(560px,calc(100vw - 32px));padding:32px;"
                    "border:1px solid #d0d7de;"
                    "border-radius:8px;background:#fff;box-shadow:0 12px 36px rgba(27,31,36,.08)}"
                    "h1{font-size:24px;margin:0 0 12px}p{line-height:1.5;margin:0 0 16px}"
                    "button{appearance:none;border:0;border-radius:6px;background:#24292f;color:#fff;"
                    "font:inherit;font-weight:600;padding:10px 14px;cursor:pointer}"
                    "small{display:block;margin-top:16px;color:#57606a}"
                    "@media(prefers-color-scheme:dark){body{background:#0d1117;color:#e6edf3}"
                    "main{background:#161b22;border-color:#30363d}"
                    "button{background:#238636}small{color:#8b949e}}"
                    "</style></head><body>"
                    f"{body}</body></html>"
                ).encode()
                self.send_response(200)
                self.send_header("Content-Type", "text/html; charset=utf-8")
                self.send_header("Content-Length", str(len(content)))
                self.end_headers()
                self.wfile.write(content)

        return Handler

    def start_page(self) -> str:
        manifest = html.escape(json.dumps(self.manifest()), quote=True)
        action = html.escape(f"{self.github_new_app_url()}?state={self.state}", quote=True)
        return (
            "<main>"
            "<h1>Dorf GitHub App Setup</h1>"
            "<p>Dorf is redirecting you to GitHub to create a private app with "
            "metadata, contents, issue, and pull request permissions for isolated coding Jobs.</p>"
            f"<form action='{action}' method='post'>"
            f"<input type='hidden' name='manifest' value='{manifest}'>"
            "<button type='submit'>Continue to GitHub</button>"
            "</form>"
            "<small>This local page only submits the generated manifest to GitHub. "
            "The setup command is waiting for GitHub to redirect back here.</small>"
            "</main>"
            "<script>setTimeout(function(){document.forms[0].submit()}, 600)</script>"
        )

    def handle_callback(self, query: dict[str, list[str]]) -> None:
        if self._single(query, "state") != self.state:
            self._fail(GitHubAppManifestFlowError("GitHub manifest callback state mismatch"))
            return
        code = self._single(query, "code")
        if code is None:
            self._fail(GitHubAppManifestFlowError("GitHub manifest callback missing code"))
            return
        try:
            converted = self.client.convert_code(code)
            app_id = converted.get("id")
            pem = converted.get("pem")
            slug = converted.get("slug")
            if not isinstance(app_id, int) or not isinstance(pem, str) or not isinstance(slug, str):
                raise GitHubAppManifestFlowError(
                    "GitHub manifest conversion response missing id, slug, or pem"
                )
            self._converted_app = ConvertedGitHubAppManifest(
                app_id=str(app_id),
                app_slug=slug,
                private_key=pem,
            )
            install_url = self.install_url(slug)
        except Exception as error:
            self._fail(error)
            return
        self._redirect(install_url)

    def handle_installed(self, query: dict[str, list[str]]) -> None:
        if self._single(query, "state") != self.state:
            self._fail(GitHubAppManifestFlowError("GitHub App install callback state mismatch"))
            return
        installation_id = self._single(query, "installation_id")
        setup_action = self._single(query, "setup_action")
        if setup_action == "request":
            self._fail(
                GitHubAppManifestFlowError(
                    "GitHub App installation requires approval before setup can complete"
                )
            )
            return
        if installation_id is None:
            self._fail(
                GitHubAppManifestFlowError("GitHub App install callback missing installation_id")
            )
            return
        try:
            if self._converted_app is None:
                raise GitHubAppManifestFlowError(
                    "GitHub App install callback arrived before manifest conversion"
                )
            config = GitHubAppConfig(
                app_id=self._converted_app.app_id,
                installation_id=installation_id,
                app_slug=self._converted_app.app_slug,
            )
            with temporary_private_key(self._converted_app.private_key) as private_key_path:
                self.token_client.mint_installation_token(
                    config,
                    private_key_path=private_key_path,
                )
            paths = save_github_app_config(config, self._converted_app.private_key)
            self._result = GitHubAppManifestFlowResult(config=config, paths=paths)
            self._done.set()
        except Exception as error:
            self._fail(error)

    def _redirect(self, url: str) -> None:
        raise _Redirect(url)

    def _fail(self, error: Exception) -> None:
        self._error = error
        self._done.set()

    @staticmethod
    def _single(query: dict[str, list[str]], key: str) -> str | None:
        values = query.get(key)
        if not values:
            return None
        return values[0]


class _Redirect(RuntimeError):
    def __init__(self, url: str) -> None:
        super().__init__(url)
        self.url = url


def create_github_app_jwt(app_id: str, private_key_path: Path) -> str:
    now = int(time.time())
    header = {"alg": "RS256", "typ": "JWT"}
    payload = {"iat": now - 60, "exp": now + 540, "iss": app_id}
    signing_input = ".".join(
        [
            base64url_json(header),
            base64url_json(payload),
        ]
    )
    try:
        result = subprocess.run(
            ["openssl", "dgst", "-sha256", "-sign", str(private_key_path)],
            input=signing_input.encode("ascii"),
            capture_output=True,
            check=False,
        )
    except FileNotFoundError as error:
        raise GitHubAppVerificationError("openssl command not found") from error
    if result.returncode != 0:
        message = (
            result.stderr.decode("utf-8", errors="replace").strip()
            or result.stdout.decode("utf-8", errors="replace").strip()
            or "openssl signing failed"
        )
        raise GitHubAppVerificationError(message)
    signature = base64url_bytes(result.stdout)
    return f"{signing_input}.{signature}"


def base64url_json(value: dict[str, object]) -> str:
    return base64url_bytes(json.dumps(value, separators=(",", ":")).encode("utf-8"))


def base64url_bytes(value: bytes) -> str:
    return base64.urlsafe_b64encode(value).decode("ascii").rstrip("=")
