#!/usr/bin/env python3
"""Offline cache/discovery checks. No cloud calls or credentials."""
import json
from pathlib import Path
import runpy
import subprocess
import tempfile
import time
from unittest.mock import patch

source = Path(__file__).resolve().parent
ns = runpy.run_path(str(source / 'bin/endpoint'))
g = ns['main'].__globals__
with tempfile.TemporaryDirectory(prefix='vast-endpoint-') as directory:
    root = Path(directory)
    (root / '.state').mkdir()
    path = root / '.state/ssh_endpoint.json'
    row = {'id': 123, 'label': 'vast-coder', 'host': '127.0.0.1', 'port': 22, 'updated_at': time.time()}
    g['ROOT'] = root
    g['settings'] = lambda: ('vast-coder', 1, 60)
    path.write_text(json.dumps(row))
    assert ns['cached'](path, 'vast-coder', 60) == row
    expired = dict(row, updated_at=time.time()-120)
    path.write_text(json.dumps(expired))
    try:
        ns['cached'](path, 'vast-coder', 60)
        raise AssertionError('expired cache accepted')
    except SystemExit:
        pass
    path.write_text(json.dumps(row))
    with patch('sys.argv', ['endpoint']), patch.dict(g, {'vast': lambda *a: (_ for _ in ()).throw(subprocess.TimeoutExpired('fixture', 1))}):
        ns['main']()  # An unavailable API may use a valid cached endpoint.
    with patch('sys.argv', ['endpoint']), patch.dict(g, {'vast': lambda *a: '[]'}):
        try:
            ns['main']()
            raise AssertionError('absent instance reused cache')
        except SystemExit:
            pass
    assert not path.exists()
    path.write_text(json.dumps(row))
    duplicate = json.dumps([{'id': 123, 'label': 'vast-coder'}, {'id': 124, 'label': 'vast-coder'}])
    with patch('sys.argv', ['endpoint']), patch.dict(g, {'vast': lambda *a: duplicate}):
        try:
            ns['main']()
            raise AssertionError('duplicate label accepted')
        except SystemExit:
            pass
    assert not path.exists()
print('Endpoint checks passed: cache TTL, timeout fallback, authoritative absence and duplicate refusal')
