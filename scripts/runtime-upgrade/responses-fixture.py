#!/usr/bin/env python3
"""Local-only deterministic model fixture for the retained-worker upgrade proof."""
import json
import os
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

root = Path(os.environ.get('DORF_RESPONSES_FIXTURE_ROOT', '/workspace/upgrade-worker-proof'))
root.mkdir(parents=True, exist_ok=True)

class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_GET(self):
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b'ready')

    def do_POST(self):
        request = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        with (root / 'requests.jsonl').open('a') as stream:
            stream.write(json.dumps(request) + '\n')
        response = {'id': 'resp_proof', 'object': 'response', 'status': 'completed',
                    'output': [{'type': 'message', 'role': 'assistant', 'id': 'msg_proof',
                                'status': 'completed', 'content': [{'type': 'output_text',
                                'text': 'Synthetic retained-worker upgrade check passed', 'annotations': []}]}],
                    'usage': {'input_tokens': 1, 'output_tokens': 1, 'total_tokens': 2}}
        events = [{'type': 'response.created', 'response': dict(response, status='in_progress', output=[])},
                  {'type': 'response.output_item.added', 'output_index': 0,
                   'item': dict(response['output'][0], status='in_progress', content=[])},
                  {'type': 'response.output_item.done', 'output_index': 0, 'item': response['output'][0]},
                  {'type': 'response.completed', 'response': response}]
        data = ''.join('event: '+event['type']+'\ndata: '+json.dumps(event)+'\n\n' for event in events).encode()
        self.send_response(200)
        self.send_header('Content-Type', 'text/event-stream')
        self.send_header('Content-Length', str(len(data)))
        self.end_headers()
        self.wfile.write(data)

port = int(os.environ.get('DORF_RESPONSES_FIXTURE_PORT', '18997'))
ThreadingHTTPServer(('127.0.0.1', port), Handler).serve_forever()
