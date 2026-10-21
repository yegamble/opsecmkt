"""Optional inference sandbox: resolved configuration and offline preflight.

The executable boundary probe is scripts/test-moderation-isolation.sh. These
checks do not claim that a selected runtime or model has been deployed/tested.
"""
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import stat
import subprocess
import tempfile
import types
import unittest
from unittest.mock import patch

from test_compose_exposure import compose_available

ROOT = Path(__file__).resolve().parents[1]
specification = importlib.util.spec_from_file_location('moderation_preflight', ROOT / 'deploy/moderation/preflight.py')
preflight = importlib.util.module_from_spec(specification)
specification.loader.exec_module(preflight)


class PreflightTests(unittest.TestCase):
    def setUp(self):
        self.work = tempfile.TemporaryDirectory()
        self.addCleanup(self.work.cleanup)
        self.directory = Path(self.work.name)
        self.weights = self.directory / 'model.gguf'
        self.weights.write_bytes(b'synthetic weights')
        self.weights.chmod(0o444)
        self.socket = self.directory / 'socket'
        self.socket.mkdir(mode=0o770)
        self.environment = {
            'MODERATION_RUNTIME_IMAGE': 'local/reviewed-runtime@sha256:' + 'a' * 64,
            'MODERATION_WEIGHTS_FILE': str(self.weights),
            'MODERATION_WEIGHTS_SHA256': hashlib.sha256(self.weights.read_bytes()).hexdigest(),
            'MODERATION_SOCKET_DIR': str(self.socket),
        }

    def valid_socket_owner(self):
        actual_stat = Path.stat
        socket_dir = self.socket

        def selected_stat(path, *args, **kwargs):
            state = actual_stat(path, *args, **kwargs)
            if path == socket_dir:
                return types.SimpleNamespace(st_uid=65532, st_gid=65532, st_mode=stat.S_IFDIR | 0o770)
            return state
        return patch.object(Path, 'stat', selected_stat)

    def test_pinned_local_artifacts_pass(self):
        with self.valid_socket_owner():
            preflight.validate(self.environment)

    def test_unpinned_images_and_invalid_digests_fail(self):
        for image in ['llama:latest', 'local/llama@sha256:abcd', 'https://local/a@sha256:'+'a'*64]:
            with self.subTest(image=image), self.assertRaises(ValueError):
                preflight.validate({**self.environment, 'MODERATION_RUNTIME_IMAGE': image})

    def test_weights_mismatch_and_symbolic_links_fail(self):
        with self.valid_socket_owner(), self.assertRaisesRegex(ValueError, 'pin mismatch'):
            preflight.validate({**self.environment, 'MODERATION_WEIGHTS_SHA256': '0' * 64})
        link = self.directory / 'symlink.gguf'
        link.symlink_to(self.weights)
        with self.assertRaisesRegex(ValueError, 'symlink'):
            preflight.validate({**self.environment, 'MODERATION_WEIGHTS_FILE': str(link)})

    def test_socket_must_be_empty_and_private(self):
        (self.socket / 'inference.sock').touch()
        with self.valid_socket_owner(), self.assertRaisesRegex(ValueError, 'empty'):
            preflight.validate(self.environment)
        with patch.object(Path, 'stat', return_value=types.SimpleNamespace(st_uid=0, st_gid=0, st_mode=stat.S_IFDIR | 0o777)):
            with self.assertRaises(ValueError):
                preflight.validate(self.environment)

    def test_launcher_uses_fixed_runtime_and_replaces_environment(self):
        launcher = (ROOT / 'deploy/moderation/entrypoint.sh').read_text()
        self.assertIn('exec env -i', launcher)
        self.assertIn('/app/llama-server', launcher)
        self.assertIn('--host /run/moderation/inference.sock', launcher)
        self.assertIn('weights pin mismatch', launcher)
        self.assertNotIn('eval ', launcher)
        self.assertNotIn('curl ', launcher)
        self.assertNotIn('wget ', launcher)
        self.assertTrue((ROOT / 'deploy/moderation/entrypoint.sh').stat().st_mode & stat.S_IROTH)

    def test_weights_must_be_readable_by_runtime_identity(self):
        self.weights.chmod(0o000)
        with self.assertRaisesRegex(ValueError, 'readable'):
            preflight.validate(self.environment)


@unittest.skipUnless(compose_available(), 'docker compose is not available')
class ResolvedSandboxTests(unittest.TestCase):
    def test_only_weights_socket_and_launcher_are_mounted(self):
        with tempfile.TemporaryDirectory() as directory:
            environment = {**os.environ,
                'DATABASE_URL': 'synthetic-secret-must-not-be-inherited',
                'APP_SECRET_KEY': 'synthetic-secret-must-not-be-inherited',
                'MODERATION_RUNTIME_IMAGE': 'local/reviewed@sha256:' + 'a' * 64,
                'MODERATION_WEIGHTS_FILE': directory + '/model.gguf',
                'MODERATION_WEIGHTS_SHA256': 'b' * 64,
                'MODERATION_SOCKET_DIR': directory + '/socket',
            }
            result = subprocess.run(['docker', 'compose', '--env-file', '/dev/null', '-f', 'deploy/moderation/compose.yaml',
                                     '--profile', 'moderation-inference', 'config', '--format', 'json'],
                                    cwd=ROOT, env=environment, capture_output=True, text=True, check=True)
        config = json.loads(result.stdout)
        self.assertEqual(set(config['services']), {'moderation-inference'})
        service = config['services']['moderation-inference']
        self.assertEqual(service['network_mode'], 'none')
        self.assertEqual(service['user'], '65532:65532')
        self.assertTrue(service['read_only'])
        self.assertEqual(service['cap_drop'], ['ALL'])
        self.assertEqual(service['security_opt'], ['no-new-privileges:true'])
        self.assertFalse(service.get('ports'))
        self.assertFalse(service.get('networks'))
        self.assertFalse(service.get('env_file'))
        self.assertEqual(service['pull_policy'], 'never')
        self.assertEqual(service['logging']['driver'], 'none')
        self.assertEqual({volume['target'] for volume in service['volumes']},
                         {'/models/model.gguf', '/run/moderation', '/opt/moderation/entrypoint.sh'})
        for volume in service['volumes']:
            self.assertFalse(volume['bind'].get('create_host_path', False))
            self.assertEqual(volume.get('read_only', False), volume['target'] != '/run/moderation')
        self.assertNotIn('DATABASE_URL', service['environment'])
        self.assertNotIn('APP_SECRET_KEY', service['environment'])
        self.assertEqual(service['environment']['HF_HUB_OFFLINE'], '1')
        self.assertTrue(all(not value for name, value in service['environment'].items() if name.lower().endswith('_proxy')))
        self.assertGreater(int(service['mem_limit']), 0)
        self.assertGreater(service['pids_limit'], 0)
        self.assertEqual(service['memswap_limit'], service['mem_limit'])


if __name__ == '__main__':
    unittest.main()
