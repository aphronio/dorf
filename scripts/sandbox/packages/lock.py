#!/usr/bin/env python3
"""Refresh package inputs on a development machine with uv, Python, and npm."""
import base64
import hashlib
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import urllib.request

root = Path(__file__).resolve().parent
pins = json.loads((root / 'packages.json').read_text())

if sys.argv[1:] == ['browser']:
    browser = pins['browser']
    with tempfile.TemporaryDirectory() as temporary:
        report = Path(temporary) / 'report.json'
        subprocess.run(['uv', 'run', '--no-project', '--python', '3.12', '--with', 'pip',
                        'python', '-m', 'pip', 'install', '--dry-run', '--ignore-installed',
                        '--only-binary=:all:', '--report', str(report),
                        f"browser-use=={browser['version']}",
                        f"browser-harness=={browser['harness_version']}"], check=True)
        wheels = []
        for package in json.loads(report.read_text())['install']:
            info = package['download_info']
            if not info['url'].endswith('.whl'):
                raise SystemExit('Expected a prebuilt browser dependency wheel')
            digest = bytes.fromhex(info['archive_info']['hashes']['sha256'])
            wheels.append({'url': info['url'], 'hash': 'sha256-' + base64.b64encode(digest).decode()})
    (root / 'browser-wheels.json').write_text(json.dumps(sorted(wheels, key=lambda item: item['url']), indent=2) + '\n')
elif sys.argv[1:] == ['pi']:
    subprocess.run(['npm', 'install', '--package-lock-only', '--ignore-scripts', '--omit=dev'], cwd=root / 'pi', check=True)
    path = root / 'pi/package-lock.json'
    lock = json.loads(path.read_text())
    # Pi's published shrinkwrap omits integrity for some registry dependencies.
    # Record their actual archive hashes so Nix's dependency fetcher can verify them.
    for name, package in lock['packages'].items():
        if not name or package.get('integrity'):
            continue
        url = package.get('resolved', '')
        if not url.startswith('https://registry.npmjs.org/'):
            raise SystemExit(f'Unpinned non-registry dependency: {name}')
        with urllib.request.urlopen(url, timeout=60) as response:
            digest = hashlib.file_digest(response, 'sha512').digest()
        package['integrity'] = 'sha512-' + base64.b64encode(digest).decode()
    path.write_text(json.dumps(lock, indent=2) + '\n')
else:
    raise SystemExit('usage: lock.py browser|pi')
