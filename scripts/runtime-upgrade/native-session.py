#!/usr/bin/env python3
"""Offline real-Codex session create/resume oracle; fixture data only."""
import argparse
import json
import os
from pathlib import Path
import queue
import subprocess
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('operation', choices=['create', 'resume'])
    parser.add_argument('--state', default='/workspace/upgrade-proof')
    parser.add_argument('--codex', default='codex')
    args = parser.parse_args()
    root = Path(args.state)
    root.mkdir(parents=True, exist_ok=True)
    marker = 'DORF_UPGRADE_SYNTHETIC_CONTEXT_732'
    requests = []

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def do_POST(self):
            request = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
            requests.append(request)
            response = {'id': 'resp_proof', 'object': 'response', 'status': 'completed',
                        'output': [{'type': 'message', 'role': 'assistant', 'id': 'msg_proof',
                                    'status': 'completed', 'content': [{'type': 'output_text',
                                    'text': 'Synthetic upgrade check passed', 'annotations': []}]}],
                        'usage': {'input_tokens': 1, 'output_tokens': 1, 'total_tokens': 2}}
            events = [{'type': 'response.created', 'response': dict(response, status='in_progress', output=[])},
                      {'type': 'response.output_item.added', 'output_index': 0,
                       'item': dict(response['output'][0], status='in_progress', content=[])},
                      {'type': 'response.output_item.done', 'output_index': 0, 'item': response['output'][0]},
                      {'type': 'response.completed', 'response': response}]
            data = ''.join('event: '+e['type']+'\ndata: '+json.dumps(e)+'\n\n' for e in events).encode()
            self.send_response(200)
            self.send_header('Content-Type', 'text/event-stream')
            self.send_header('Content-Length', str(len(data)))
            self.end_headers()
            self.wfile.write(data)

    server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    env = dict(os.environ, CODEX_HOME=str(root / 'codex-home'))
    Path(env['CODEX_HOME']).mkdir(parents=True, exist_ok=True)
    proc = subprocess.Popen([args.codex, 'app-server', '--listen', 'stdio://',
        '-c', 'model_provider="proof"', '-c', 'model="gpt-6-astra"',
        '-c', f'model_providers.proof={{name="proof",base_url="http://127.0.0.1:{server.server_port}/v1",wire_api="responses"}}',
        '-c', 'features.shell_tool=false', '-c', 'web_search="disabled"'],
        cwd=root, env=env, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    received = queue.Queue()
    errors = []
    def read_errors():
        for line in proc.stderr:
            errors.append(line)
    threading.Thread(target=read_errors, daemon=True).start()
    def read():
        for line in proc.stdout:
            try:
                received.put(json.loads(line))
            except json.JSONDecodeError:
                errors.append(line)
        received.put({'process_exited': True})
    threading.Thread(target=read, daemon=True).start()
    sequence = 0
    def call(method, params):
        nonlocal sequence
        sequence += 1
        proc.stdin.write(json.dumps({'id': sequence, 'method': method, 'params': params})+'\n')
        proc.stdin.flush()
        deadline = time.monotonic() + 45
        while True:
            try:
                data = received.get(timeout=max(0.01, deadline-time.monotonic()))
            except queue.Empty:
                raise RuntimeError(f'{method} timed out: {"".join(errors)[-2000:]}')
            if data.get('process_exited'):
                proc.wait(timeout=5)
                raise RuntimeError(f'{method}: app-server exited ({proc.returncode}): {"".join(errors)[-2000:]}')
            if data.get('id') == sequence:
                if 'error' in data:
                    raise RuntimeError(f'{method} failed: {data["error"]}')
                return data['result']
    def complete():
        deadline = time.monotonic() + 45
        while True:
            try:
                data = received.get(timeout=max(0.01, deadline-time.monotonic()))
            except queue.Empty:
                raise RuntimeError(f'turn/completed timed out: {"".join(errors)[-2000:]}')
            if data.get('method') == 'turn/completed':
                turn = data['params']['turn']
                if turn.get('status') != 'completed':
                    raise RuntimeError('Native turn did not complete')
                return
    try:
        call('initialize', {'clientInfo': {'name': 'upgrade_proof', 'version': '1'}, 'capabilities': {'experimentalApi': True}})
        proc.stdin.write(json.dumps({'method': 'initialized'})+'\n')
        proc.stdin.flush()
        if args.operation == 'create':
            started = call('thread/start', {'cwd': str(root), 'approvalPolicy': 'never', 'sandbox': 'read-only'})
            thread_id = started['thread']['id']
            (root / 'thread-id').write_text(thread_id)
        else:
            thread_id = (root / 'thread-id').read_text().strip()
            call('thread/resume', {'threadId': thread_id, 'cwd': str(root)})
        call('turn/start', {'threadId': thread_id, 'input': [{'type': 'text', 'text': marker if args.operation == 'create' else 'Resume the synthetic upgrade check.'}]})
        complete()
        history = call('thread/read', {'threadId': thread_id, 'includeTurns': True})
        assert requests and marker in json.dumps(requests[-1]), 'Original context absent from model request'
        assert marker in json.dumps(history), 'Original context absent from retained thread'
        latest = history['thread']['turns'][-1]
        assert any(item.get('type') == 'agentMessage' and item.get('text', '').strip()
                   for item in latest['items']), 'Native turn completed without a substantive reply'
        print(json.dumps({'operation': args.operation, 'thread_id': thread_id, 'context_preserved': True,
                          'version': subprocess.check_output([args.codex, '--version'], text=True).strip()}))
    finally:
        proc.terminate()
        proc.wait(timeout=10)
        server.shutdown()


if __name__ == '__main__':
    main()
