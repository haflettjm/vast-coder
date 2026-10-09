#!/usr/bin/env python3
"""Offline watchdog checks. All HTTP, SSH and systemctl calls are mocked."""
import importlib.machinery
import importlib.util
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import MagicMock, patch
import contextlib
import io
import json
import shlex
import subprocess
import urllib.error
import http.client

ROOT = Path(__file__).resolve().parent


class HealthTests(unittest.TestCase):
    def setUp(self):
        self.assertTrue((ROOT / 'bin/health').exists(), 'bin/health must exist')
        loader = importlib.machinery.SourceFileLoader('health', str(ROOT / 'bin/health'))
        spec = importlib.util.spec_from_loader(loader.name, loader)
        assert spec is not None
        self.health = importlib.util.module_from_spec(spec)
        loader.exec_module(self.health)
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.settings = dict(LOCAL_PORT=8000, REMOTE_PORT=8080, HEALTH_TIMEOUT=5,
                             HEALTH_COOLDOWN=120, HEALTH_FAILURES=1, STATE_DIR=self.tmp.name)
        self.health.RESTART_WAIT = 0
        patcher = patch.object(self.health, 'listening', return_value=False)
        patcher.start()
        self.addCleanup(patcher.stop)

    def test_ready_needs_only_local_http(self):
        with patch.object(self.health, 'probe', return_value=200), \
                patch.object(self.health, 'remote_probe') as remote:
            result = self.health.check(self.settings)
        self.assertEqual(result['status'], 'inference-ready')
        self.assertTrue(result['tunnel-connected'])
        self.assertTrue(result['inference-ready'])
        self.assertFalse(result['unreachable'])
        remote.assert_not_called()

    def test_loading_and_unreachable_never_restart(self):
        for local, remote_status, status, connected in [
                (503, None, 'model-loading', True),
                (None, 503, 'model-loading', False),
                (None, None, 'unreachable', False),
                (500, 500, 'unreachable', True)]:
            with self.subTest(local=local, remote=remote_status), \
                    patch.object(self.health, 'probe', return_value=local), \
                    patch.object(self.health, 'remote_probe', return_value=remote_status) as remote, \
                    patch.object(self.health, 'restart_bridge', create=True) as restart:
                result = self.health.check(self.settings, repair=True)
                self.assertEqual(result['status'], status)
                self.assertEqual(result['tunnel-connected'], connected)
                self.assertFalse(result['inference-ready'])
                restart.assert_not_called()
                self.assertEqual(remote.call_count, 0 if local == 503 else 1)

    def test_repair_is_opt_in_and_cooldown_survives_restart(self):
        with patch.object(self.health, 'probe', return_value=None), \
                patch.object(self.health, 'remote_probe', return_value=200), \
                patch('subprocess.run') as run:
            result = self.health.check(self.settings)
            self.assertTrue(result['tunnel-degraded'])
            run.assert_not_called()
            run.return_value.returncode = 0
            result = self.health.check(self.settings, repair=True)
            self.assertEqual(result['repair'], 'restarted')
            self.assertFalse(result['inference-ready'])
            self.assertEqual(result['status'], 'unreachable')
            run.assert_called_once_with(
                ['systemctl', '--user', 'restart', 'vast-coder-bridge.service'],
                capture_output=True, timeout=15)
            result = self.health.check(self.settings, repair=True)
            self.assertEqual(result['repair'], 'cooldown')
            self.assertEqual(run.call_count, 1)

    def test_successful_restart_requires_fresh_http_200(self):
        with patch.object(self.health, 'probe', side_effect=[None, 200]) as probe, \
                patch.object(self.health, 'remote_probe', return_value=200), \
                patch('subprocess.run') as run:
            run.return_value.returncode = 0
            result = self.health.check(self.settings, repair=True)
        self.assertTrue(result['inference-ready'])
        self.assertEqual(probe.call_count, 2)

    def test_accepting_port_with_stuck_http_is_connected_but_degraded(self):
        with patch.object(self.health, 'listening', return_value=True), \
                patch.object(self.health, 'probe', return_value=None), \
                patch.object(self.health, 'remote_probe', return_value=200):
            result = self.health.check(self.settings)
        self.assertTrue(result['tunnel-connected'])
        self.assertTrue(result['tunnel-degraded'])
        self.assertFalse(result['inference-ready'])

    def test_congested_tunnel_is_not_restarted_until_failures_repeat(self):
        self.settings['HEALTH_FAILURES'] = 3
        with patch.object(self.health, 'probe', return_value=None), \
                patch.object(self.health, 'remote_probe', return_value=200), \
                patch('subprocess.run') as run:
            run.return_value.returncode = 0
            actions = [self.health.check(self.settings, repair=True)['repair'] for _ in range(3)]
            self.assertEqual(actions, ['waiting', 'waiting', 'restarted'])
            self.assertEqual(run.call_count, 1)

    def test_recovery_resets_failure_count(self):
        self.settings['HEALTH_FAILURES'] = 2
        with patch.object(self.health, 'remote_probe', return_value=200), patch('subprocess.run') as run:
            with patch.object(self.health, 'probe', return_value=None):
                self.assertEqual(self.health.check(self.settings, repair=True)['repair'], 'waiting')
            with patch.object(self.health, 'probe', return_value=200):
                self.assertEqual(self.health.check(self.settings, repair=True)['consecutive-failures'], 0)
            with patch.object(self.health, 'probe', return_value=None):
                self.assertEqual(self.health.check(self.settings, repair=True)['repair'], 'waiting')
            run.assert_not_called()

    def test_restart_waits_for_ssh_to_reconnect(self):
        self.health.RESTART_WAIT = 10
        with patch.object(self.health, 'probe', side_effect=[None, None, None, 200]) as probe, \
                patch.object(self.health, 'remote_probe', return_value=200), \
                patch.object(self.health.time, 'sleep') as sleep, patch('subprocess.run') as run:
            run.return_value.returncode = 0
            result = self.health.check(self.settings, repair=True)
        self.assertTrue(result['inference-ready'])
        self.assertEqual(probe.call_count, 4)
        self.assertEqual(sleep.call_count, 2)

    def test_http_probe_uses_status_only_and_timeout(self):
        response = MagicMock()
        response.__enter__.return_value.status = 200
        with patch('urllib.request.build_opener') as build:
            opener = build.return_value
            opener.open.return_value = response
            self.assertEqual(self.health.probe('http://127.0.0.1:8000/health', 5), 200)
            opener.open.assert_called_once_with('http://127.0.0.1:8000/health', timeout=5)
            response.read.assert_not_called()
            for error, expected in [
                    (urllib.error.HTTPError('url', 503, 'loading', {}, None), 503),
                    (TimeoutError(), None), (urllib.error.URLError('offline'), None),
                    (http.client.BadStatusLine('bad'), None)]:
                opener.open.side_effect = error
                self.assertEqual(self.health.probe('http://127.0.0.1:8000/health', 5), expected)

    def test_remote_probe_is_cached_bounded_and_status_only(self):
        with patch('subprocess.run') as run:
            run.return_value = subprocess.CompletedProcess([], 0, '200\n', '')
            self.assertEqual(self.health.remote_probe(self.settings), 200)
            args, kwargs = run.call_args
            self.assertEqual(args[0][1:3], ['--cached', '-o'])
            self.assertIn('BatchMode=yes', args[0])
            self.assertEqual(kwargs['timeout'], 10)
            command = shlex.split(args[0][-1])
            self.assertEqual(command[:2], ['python3', '-c'])
            for error, expected in [(None, '200'),
                    (urllib.error.HTTPError('url', 503, 'loading', {}, None), '503'),
                    (TimeoutError(), '0')]:
                with patch('urllib.request.build_opener') as build, contextlib.redirect_stdout(io.StringIO()) as out:
                    build.return_value.open.return_value.__enter__.return_value.status = 200
                    build.return_value.open.side_effect = error
                    exec(command[2], {})
                    self.assertEqual(out.getvalue().strip(), expected)
            for completed in [subprocess.CompletedProcess([], 1, '200', 'private'),
                              subprocess.CompletedProcess([], 0, 'garbage', ''),
                              subprocess.CompletedProcess([], 0, '0', '')]:
                run.return_value = completed
                self.assertIsNone(self.health.remote_probe(self.settings))
            run.side_effect = subprocess.TimeoutExpired('ssh', 10)
            self.assertIsNone(self.health.remote_probe(self.settings))

    def test_settings_and_cli_are_bounded_and_explicit(self):
        with patch('subprocess.run') as run:
            run.return_value = subprocess.CompletedProcess([], 0, '8000\n8080\n5\n120\n3\n', '')
            settings = self.health.load_settings()
            self.assertEqual(settings['HEALTH_TIMEOUT'], 5)
            self.assertEqual(run.call_args.kwargs['timeout'], 15)
            run.return_value.stdout = '8000\n8080\n0\n120\n3\n'
            with self.assertRaises(ValueError):
                self.health.load_settings()
        with patch.object(self.health, 'load_settings', return_value=self.settings), \
                patch.object(self.health, 'probe', return_value=503), \
                contextlib.redirect_stdout(io.StringIO()) as out:
            self.assertEqual(self.health.main([]), 1)
            result = json.loads(out.getvalue())
            self.assertTrue(result['model-loading'])
            self.assertEqual(result['health-timeout'], 5)
            self.assertEqual(result['repair-cooldown'], 120)
        with patch.object(self.health, 'load_settings', side_effect=ValueError('secret')), \
                contextlib.redirect_stdout(io.StringIO()) as out:
            self.assertEqual(self.health.main([]), 1)
            self.assertTrue(json.loads(out.getvalue())['unreachable'])
            self.assertNotIn('secret', out.getvalue())

    def test_failed_restart_and_lock_contention_cannot_storm(self):
        for failure in [subprocess.TimeoutExpired('systemctl', 15), OSError('unavailable')]:
            state = Path(self.tmp.name) / 'health-repair'
            state.unlink(missing_ok=True)
            with patch('subprocess.run', side_effect=failure) as run:
                self.assertIn(self.health.restart_bridge(self.settings), ('restart-failed', 'repair-error'))
                self.assertEqual(self.health.restart_bridge(self.settings), 'cooldown')
                self.assertEqual(run.call_count, 1)
        with state.open('r+') as lock, patch('subprocess.run') as run:
            self.health.fcntl.flock(lock, self.health.fcntl.LOCK_EX | self.health.fcntl.LOCK_NB)
            self.assertEqual(self.health.restart_bridge(self.settings), 'repair-busy')
            run.assert_not_called()
        state.write_text('nan')
        with patch('subprocess.run') as run:
            self.assertEqual(self.health.restart_bridge(self.settings), 'repair-error')
            run.assert_not_called()

    def test_actual_config_loading_does_not_resolve_secrets(self):
        root = Path(self.tmp.name)
        (root / 'bin').mkdir()
        (root / 'bin/_common.sh').write_text((ROOT / 'bin/_common.sh').read_text())
        (root / 'vast-coder.env').write_text('LOCAL_PORT=8001\nREMOTE_PORT=8081\nHEALTH_TIMEOUT=3\nHEALTH_COOLDOWN=90\nHEALTH_FAILURES=2\nVAST_KEY_REF=unused\n')
        with patch.object(self.health, 'ROOT', root):
            settings = self.health.load_settings()
        self.assertEqual(settings, dict(LOCAL_PORT=8001, REMOTE_PORT=8081, HEALTH_TIMEOUT=3,
                                        HEALTH_COOLDOWN=90, HEALTH_FAILURES=2, STATE_DIR=str(root / '.state')))
        self.assertEqual(list((root / '.state').iterdir()), [])


if __name__ == '__main__':
    unittest.main()
