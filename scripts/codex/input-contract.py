#!/usr/bin/env python3
"""Probe real Codex input semantics with isolated state and a local synthetic model.

No user credentials, live Sessions, external model calls, or provider resources.
Run with the Codex version being considered for a supported profile.
"""

import argparse
import json
import os
from pathlib import Path
import queue
import subprocess
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


class Native:
    def __init__(self, binary, root, endpoint):
        self.events = []
        self.incoming = queue.Queue()
        self.sequence = 0
        self.stderr = (root / 'server.log').open('a')
        self.proc = subprocess.Popen(
            [binary, 'app-server', '--listen', 'stdio://',
             '-c', 'model_provider="proof"', '-c', 'model="gpt-6-astra"',
             '-c', f'model_providers.proof={{name="proof",base_url="{endpoint}",wire_api="responses"}}',
             '-c', 'features.shell_tool=false', '-c', 'web_search="disabled"'],
            cwd=root,
            env={'PATH': os.environ['PATH'], 'CODEX_HOME': str(root / 'codex-home')},
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=self.stderr, text=True,
        )
        threading.Thread(target=self.read, daemon=True).start()
        self.call('initialize', {'clientInfo': {'name': 'dorf_input_proof', 'version': '1'},
                                'capabilities': {'experimentalApi': True}})
        self.write({'method': 'initialized'})

    def read(self):
        for line in self.proc.stdout:
            self.incoming.put(json.loads(line))
        self.incoming.put({'exited': True})

    def write(self, value):
        self.proc.stdin.write(json.dumps(value) + '\n')
        self.proc.stdin.flush()

    def receive(self, deadline):
        item = self.incoming.get(timeout=max(0.01, deadline - time.monotonic()))
        if item.get('exited'):
            raise RuntimeError('app-server exited; inspect the isolated server.log')
        return item

    def call(self, method, params):
        self.sequence += 1
        self.write({'id': self.sequence, 'method': method, 'params': params})
        deadline = time.monotonic() + 30
        while True:
            item = self.receive(deadline)
            if item.get('id') == self.sequence:
                if 'error' in item:
                    raise RuntimeError(f'{method}: {item["error"]}')
                return item['result']
            self.events.append(item)

    def complete(self, turn):
        deadline = time.monotonic() + 30
        while True:
            for item in self.events:
                if item.get('method') == 'turn/completed' and item['params']['turn']['id'] == turn:
                    assert item['params']['turn']['status'] == 'completed', item
                    return
            self.events.append(self.receive(deadline))

    def stop(self):
        if self.proc.poll() is None:
            self.proc.kill()
        self.proc.wait(timeout=10)
        self.stderr.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--codex', default='codex')
    args = parser.parse_args()
    gate = threading.Event()
    arrived = threading.Event()
    calls = []

    class Model(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def do_POST(self):
            calls.append(json.loads(self.rfile.read(int(self.headers['Content-Length']))))
            arrived.set()
            if not gate.wait(30):
                self.send_error(504)
                return
            item = {'type': 'message', 'role': 'assistant', 'id': f'msg_{len(calls)}',
                    'status': 'completed', 'content': [{'type': 'output_text',
                    'text': 'Synthetic reply', 'annotations': []}]}
            response = {'id': f'resp_{len(calls)}', 'object': 'response', 'status': 'completed',
                        'output': [item], 'usage': {'input_tokens': 1, 'output_tokens': 1, 'total_tokens': 2}}
            events = [
                {'type': 'response.created', 'response': dict(response, status='in_progress', output=[])},
                {'type': 'response.output_item.added', 'output_index': 0,
                 'item': dict(item, status='in_progress', content=[])},
                {'type': 'response.output_item.done', 'output_index': 0, 'item': item},
                {'type': 'response.completed', 'response': response},
            ]
            body = ''.join('event: ' + e['type'] + '\ndata: ' + json.dumps(e) + '\n\n' for e in events).encode()
            try:
                self.send_response(200)
                self.send_header('Content-Type', 'text/event-stream')
                self.send_header('Content-Length', str(len(body)))
                self.end_headers()
                self.wfile.write(body)
            except (BrokenPipeError, ConnectionResetError):
                pass

    server = ThreadingHTTPServer(('127.0.0.1', 0), Model)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    endpoint = f'http://127.0.0.1:{server.server_port}/v1'
    native = None
    try:
        with tempfile.TemporaryDirectory(prefix='dorf-native-input-') as temporary:
            root = Path(temporary)
            (root / 'codex-home').mkdir()
            native = Native(args.codex, root, endpoint)
            thread = native.call('thread/start', {'cwd': temporary, 'approvalPolicy': 'never',
                                                 'sandbox': 'read-only'})['thread']['id']

            def send(identity, text):
                return native.call('turn/start', {'threadId': thread, 'clientUserMessageId': identity,
                                   'input': [{'type': 'text', 'text': text}]})['turn']['id']

            def history():
                return native.call('thread/read', {'threadId': thread, 'includeTurns': True})

            first = send('proof-first', 'Synthetic initial input')
            assert arrived.wait(15), 'synthetic model was not called'
            second = send('proof-second', 'Synthetic additional input')
            assert first == second, 'active input did not join the same Turn'
            gate.set()
            native.complete(first)
            saved = history()
            assert 'Synthetic additional input' in json.dumps(saved), saved

            repeated = send('proof-first', 'Synthetic initial input')
            native.complete(repeated)
            assert repeated != first, 'repeated client ID unexpectedly deduplicated'
            saved = history()
            native.stop()
            native = Native(args.codex, root, endpoint)
            cold = history()
            assert len(cold['thread']['turns']) >= 2, cold
            assert 'Synthetic additional input' in json.dumps(cold), cold

            native.call('thread/resume', {'threadId': thread})
            gate.clear()
            arrived.clear()
            active = send('proof-crash-start', 'Synthetic work before process loss')
            assert arrived.wait(15)
            pending = send('proof-crash-pending', 'Synthetic pending input at process loss')
            assert pending == active
            before = history()
            native.stop()
            native = Native(args.codex, root, endpoint)
            after = history()
            assert 'Synthetic additional input' in json.dumps(after), after
            marker = 'Synthetic pending input at process loss'
            summary = {
                'version': subprocess.check_output([args.codex, '--version'], text=True).strip(),
                'active_turn_start_steers': True,
                'repeated_client_id_starts_new_turn': True,
                'completed_history_survives_server_restart': True,
                'pending_input_visible_before_kill': marker in json.dumps(before),
                'pending_input_visible_after_kill': marker in json.dumps(after),
            }
            print(json.dumps(summary, indent=2))
    finally:
        gate.set()
        if native is not None:
            native.stop()
        server.shutdown()
        server.server_close()


if __name__ == '__main__':
    main()
