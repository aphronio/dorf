"""Exercise the tools an agent receives on a fresh VM; owns all probe processes."""
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from threading import Thread

def run(argv, **kwargs):
    return subprocess.run(argv, check=True, text=True, capture_output=True, timeout=120, **kwargs).stdout


class Fixture(BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.send_header('Content-Type', 'text/html')
        self.end_headers()
        self.wfile.write(b'<title>Dorf workstation proof</title><button onclick="this.textContent=\'Clicked\'">Ready</button>')

    def log_message(self, *_args):
        pass


metadata = json.loads(Path('/usr/local/share/dorf/image.json').read_text())
profile = Path('/nix/var/nix/profiles/dorf-tools').resolve()
assert str(profile) == metadata['workstation']['store_path']
assert not Path('/etc/systemd/system/browser-use-vm-browser.service').exists()
assert importlib.util.find_spec('playwright') is None
assert not (profile / 'bin/playwright').exists()
assert Path('/root/.codex/skills/browser-use/SKILL.md').read_text() == run(['browser-use', 'skill'])
for command in Path('/proc').glob('[0-9]*/cmdline'):
    try:
        executable = command.read_bytes().split(b'\0')[0].decode()
    except (OSError, UnicodeDecodeError):
        continue
    assert Path(executable).name not in {'chrome', 'chromium', 'chrome-headless-shell'}, 'browser already running at boot'
for name in ['pi', 'python3', 'node', 'go', 'uv', 'gcc', 'git', 'jq', 'browser-use', 'chromium', 'browser-python']:
    assert str(Path('/usr/local/bin', name).resolve()).startswith('/nix/store/'), name
for command in [['pi', '--version'], ['python3', '--version'], ['node', '--version'],
                ['go', 'version'], ['uv', '--version'], ['browser-use', '--version']]:
    print(run(command).strip())
with tempfile.TemporaryDirectory(prefix='dorf-workstation-proof-') as temporary:
    temporary = Path(temporary)
    source = temporary / 'main.c'
    source.write_text('#include <stdio.h>\nint main(void) { puts("native compiler ready"); }\n')
    run(['gcc', str(source), '-o', str(temporary / 'native')])
    assert run([str(temporary / 'native')]).strip() == 'native compiler ready'
    run(['uv', 'venv', '--python', 'python3', str(temporary / 'venv')])
    run([str(temporary / 'venv/bin/python'), '-c', 'import ssl, sqlite3; print("python ready")'])
    server = ThreadingHTTPServer(('127.0.0.1', 0), Fixture)
    thread = Thread(target=server.serve_forever, daemon=True)
    thread.start()
    url = f'http://127.0.0.1:{server.server_port}/'
    # Start the preinstalled browser directly, as the agent would.
    with (temporary / 'chromium.log').open('w+') as browser_log:
        browser = subprocess.Popen(['chromium', '--headless=new', '--no-sandbox',
                                    '--remote-debugging-address=127.0.0.1', '--remote-debugging-port=0',
                                    f'--user-data-dir={temporary / "browser"}', 'about:blank'],
                                   stdout=browser_log, stderr=browser_log)
        try:
            port_file = temporary / 'browser/DevToolsActivePort'
            deadline = time.monotonic() + 30
            while True:
                try:
                    port = int(port_file.read_text().splitlines()[0])
                    endpoint = f'http://127.0.0.1:{port}'
                    with urllib.request.urlopen(endpoint + '/json/version', timeout=1) as response:
                        assert json.load(response)['webSocketDebuggerUrl']
                    break
                except (OSError, ValueError, IndexError):
                    if browser.poll() is not None or time.monotonic() >= deadline:
                        browser_log.seek(0)
                        raise RuntimeError('Chromium failed to become ready: ' + browser_log.read())
                    time.sleep(0.1)
            environment = {**os.environ, 'BU_CDP_URL': endpoint,
                           'BU_NAME': 'dorf-workstation-proof', 'BH_TAB_MARKER': '0',
                           'BH_AGENT_WORKSPACE': str(temporary / 'agent-workspace')}
            try:
                script = (f'new_tab({url!r})\nprint(page_info())\n'
                          'js("document.querySelector(\'button\').click()")\n'
                          'print(js("document.querySelector(\'button\').textContent"))\n')
                output = run(['browser-use'], input=script, env=environment)
                assert 'Dorf workstation proof' in output, output
                assert 'Clicked' in output, output
                print('browser-use connected to agent-started Chromium')
            finally:
                run(['browser-use', '--reload'], env=environment)
        finally:
            browser.terminate()
            try:
                browser.wait(timeout=10)
            except subprocess.TimeoutExpired:
                browser.kill()
                browser.wait(timeout=10)
            server.shutdown()
            server.server_close()
            thread.join()
print(json.dumps({'workstation': str(profile), 'tools': metadata['tools'], 'result': 'passed'}))
