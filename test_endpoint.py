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
    # Proxy route is preferred and the direct IP is kept as the alternate.
    path.unlink(missing_ok=True)
    instance = json.dumps([{'id': 123, 'label': 'vast-coder', 'ssh_host': 'ssh9.vast.ai', 'ssh_port': 11088}])
    def fake_vast(args, timeout=1):
        return instance if args[0] == 'show' else 'ssh://root@203.0.113.7:26655\n'
    with patch('sys.argv', ['endpoint']), patch.dict(g, {'vast': fake_vast}):
        ns['main']()
    saved = json.loads(path.read_text())
    assert (saved['host'], saved['port']) == ('ssh9.vast.ai', 11088), saved
    assert (saved['alt_host'], saved['alt_port']) == ('203.0.113.7', 26655), saved
    assert ns['cached'](path, 'vast-coder', 60) == saved
    # A malformed proxy record falls back to the direct endpoint only.
    bad = json.dumps([{'id': 123, 'label': 'vast-coder', 'ssh_host': '-oProxyCommand=x', 'ssh_port': 1}])
    path.unlink(missing_ok=True)
    with patch('sys.argv', ['endpoint']), patch.dict(g, {'vast': lambda a, t=1: bad if a[0] == 'show' else 'ssh://root@203.0.113.7:26655\n'}):
        ns['main']()
    assert 'alt_host' not in json.loads(path.read_text())
    # Unsafe alternate hosts in a cached record are rejected.
    path.write_text(json.dumps(dict(saved, alt_host='-oProxyCommand=x')))
    try:
        ns['cached'](path, 'vast-coder', 60)
        raise AssertionError('unsafe alternate host accepted')
    except SystemExit:
        pass
print('Endpoint checks passed: cache TTL, timeout fallback, authoritative absence and duplicate refusal')
