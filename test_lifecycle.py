#!/usr/bin/env python3
"""Offline lifecycle checks using an explicitly fake Vast CLI. No rentals."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile

SOURCE = Path(__file__).resolve().parent
with tempfile.TemporaryDirectory(prefix='vast-coder-test-') as directory:
    root = Path(directory)
    shutil.copytree(SOURCE / 'bin', root / 'bin')
    shutil.copy(SOURCE / 'vast-coder.env', root / 'vast-coder.env')
    mock = root / 'mock'
    mock.mkdir()
    cli = mock / 'vastai'
    cli.write_text('''#!/usr/bin/env python3
import json, os, pathlib, sys
args = sys.argv[3:]
if args[:2] == ['show', 'instances']:
    print(os.environ.get('TEST_INSTANCES', '[]'))
elif args[:2] == ['search', 'offers']:
    pathlib.Path(os.environ['TEST_QUERY']).write_text(args[2])
    print(os.environ['TEST_OFFERS'])
elif args[:2] == ['create', 'instance']:
    pathlib.Path(os.environ['TEST_CREATE']).write_text(json.dumps(args))
    print(json.dumps({'success': True, 'new_contract': 123, 'instance_api_key': 'fixture-do-not-emit'}))
else:
    raise SystemExit('Unexpected fake CLI call')
''')
    cli.chmod(0o700)
    env = dict(os.environ, PATH=str(mock) + ':' + os.environ['PATH'], VAST_API_KEY='offline-fixture',
               TEST_QUERY=str(root / 'query'), TEST_CREATE=str(root / 'create'),
               TEST_OFFERS=json.dumps([{'id': 2, 'dph_total': 0.34}, {'id': 1, 'dph_total': 0.30}]))

    def run(script, *args, **changes):
        return subprocess.run(['bash', str(root / 'bin' / script), *args], env=dict(env, **changes),
                              text=True, capture_output=True, timeout=10)

    assert run('up', TEST_INSTANCES='[{"id": 8, "label": "vast-coder"}]').returncode == 0
    assert not (root / 'create').exists()
    assert run('up', TEST_INSTANCES='[{"id": 8, "label": null}]').returncode != 0
    assert run('up', TEST_INSTANCES='[{"id": 8, "label": "vast-coder"}, {"id": 9, "label": "vast-coder"}]').returncode != 0
    assert run('up', TEST_OFFERS='[{"id": 1, "dph_total": 9}]').returncode != 0
    assert not (root / 'create').exists()
    assert run('offers', '--pick', COUNTRIES_OVERRIDE='US,CA').stdout.strip() == '1'
    assert 'geolocation in [US,CA]' in (root / 'query').read_text()
    assert run('offers', COUNTRIES_OVERRIDE='US;echo nope').returncode != 0
    result = run('up')
    assert result.returncode == 0, result.stderr
    assert 'fixture-do-not-emit' not in result.stdout
    args = json.loads((root / 'create').read_text())
    assert args[:3] == ['create', 'instance', '1']
    assert '--ssh' in args and '--direct' in args and '--jupyter' not in args
    startup = args[args.index('--onstart-cmd') + 1]
    assert 'MAX-MTP-Q5_K_S.gguf' in startup and '--host 127.0.0.1' in startup and '-ub 2048' in startup
    assert '--api-key-file /root/llm_api_key' in startup
    assert (root / '.state/llm_api_key').read_text().strip() not in startup
    assert (root / '.state/llm_api_key').stat().st_mode & 0o777 == 0o600
    assert subprocess.run(['bash', '-n'], input=startup, text=True).returncode == 0
    # vLLM engine: separate image, file and server, same secret isolation.
    (root / 'create').unlink()
    result = run('up', ENGINE='vllm')
    assert result.returncode == 0, result.stderr
    args = json.loads((root / 'create').read_text())
    assert args[args.index('--image') + 1] == 'vllm/vllm-openai:v0.31.0'
    startup = args[args.index('--onstart-cmd') + 1]
    assert 'MAX-MTP-Q5_K_S.gguf' in startup and 'vllm serve' in startup and 'llama-server' not in startup
    assert '--enable-auto-tool-choice' in startup and '--tool-call-parser qwen3_coder' in startup and '--tensor-parallel-size 2' in startup and '--host 127.0.0.1' in startup
    assert '--api-key' not in startup and (root / '.state/llm_api_key').read_text().strip() not in startup
    assert subprocess.run(['bash', '-n'], input=startup, text=True).returncode == 0
    guard = subprocess.run(['python3', str(root / 'bin/apply')], env=dict(env, ENGINE='vllm'), text=True,
                           capture_output=True, timeout=10)
    assert guard.returncode != 0 and 'llama.cpp' in guard.stderr  # apply only manages llama.cpp
    # An additional, differently labelled rental needs an explicit flag; unlabelled ones never pass.
    other = '[{"id": 8, "label": "vast-coder"}]'
    (root / 'create').unlink()
    assert run('up', LABEL='vast-coder-new', TEST_INSTANCES=other).returncode != 0
    assert not (root / 'create').exists()
    assert run('up', '--additional', LABEL='vast-coder-new', TEST_INSTANCES=other).returncode == 0
    assert '--label' in json.loads((root / 'create').read_text())
    (root / 'create').unlink()
    assert run('up', '--additional', LABEL='vast-coder-new', TEST_INSTANCES='[{"id": 8, "label": null}]').returncode != 0
    assert not (root / 'create').exists()
print('Offline checks passed: single rental, price cap, region filter, SSH-only startup, secret isolation')
