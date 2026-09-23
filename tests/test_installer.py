"""Exercise installer decisions in disposable directories with no real Docker."""
import os
from pathlib import Path
import shutil
import stat
import subprocess
import sys
import tempfile
import unittest

INSTALLER = Path(__file__).resolve().parents[1] / 'scripts' / 'install.sh'


class InstallerTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix='opsecmkt-installer-test-')
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        (self.root / 'scripts').mkdir()
        shutil.copyfile(INSTALLER, self.root / 'scripts' / 'install.sh')
        self.bin = self.root / 'bin'
        self.bin.mkdir()
        self.log = self.root / 'docker-calls.log'
        self.stub('docker', '''
import os
from pathlib import Path
import sys
args = sys.argv[1:]
with Path(os.environ['INSTALLER_TEST_LOG']).open('a') as log:
    log.write(' '.join(args) + '\\n')
if args == ['compose', 'version']:
    sys.exit(0)
if args == ['compose', 'config', '--quiet']:
    assert Path('.env').is_file(), 'Configuration must exist before validation'
    sys.exit(42 if os.environ.get('INSTALLER_TEST_BAD_CONFIG') == '1' else 0)
if args == ['compose', 'up', '-d', '--build']:
    sys.exit(0)
sys.exit('Unexpected docker command')
''')
        self.stub('openssl', '''
import sys
assert sys.argv[1:3] == ['rand', '-hex']
print('a1' * int(sys.argv[3]))
''')

    def stub(self, name, body):
        path = self.bin / name
        path.write_text(f'#!{sys.executable}\n' + body)
        path.chmod(0o700)

    def run_installer(self, answers, bad_config=False):
        # Do not source or copy the user's environment file. Compose is a stub;
        # all writes and generated credentials stay inside this temporary repo.
        env = {
            'PATH': str(self.bin) + os.pathsep + os.defpath,
            'HOME': str(self.root),
            'INSTALLER_TEST_LOG': str(self.log),
            'INSTALLER_TEST_BAD_CONFIG': '1' if bad_config else '0',
        }
        return subprocess.run(
            ['/bin/bash', str(self.root / 'scripts' / 'install.sh')],
            input='\n'.join(answers) + '\n', cwd=self.root,
            env=env, capture_output=True, text=True, timeout=10, check=False,
        )

    def config(self):
        path = self.root / '.env'
        self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)
        return {key: value[1:-1] for key, value in (
            line.split('=', 1) for line in path.read_text().splitlines()
        )}

    def calls(self):
        return self.log.read_text().splitlines() if self.log.exists() else []

    def assert_started(self, result):
        self.assertEqual(result.returncode, 0, 'Installer unexpectedly failed')
        self.assertEqual(self.calls(), [
            'compose version', 'compose config --quiet', 'compose up -d --build',
        ])
        self.assertEqual(list(self.root.glob('.env.install.*')), [])

    def test_internal_clearnet_defaults(self):
        result = self.run_installer(['', '', '', '', ''])
        self.assert_started(result)
        config = self.config()
        self.assertEqual(config['APP_MODE'], 'clearnet')
        self.assertEqual(config['COOKIE_SECURE'], 'true')
        self.assertEqual(config['COMPOSE_PROFILES'], 'internal-db')
        self.assertEqual(config['COMPOSE_FILE'],
                         'compose.yaml:compose.clearnet.yaml:compose.nodes.yaml:compose.internal-db.yaml')
        self.assertIn('@db:5432/opsecmkt', config['DATABASE_URL'])
        self.assertEqual(len(config['POSTGRES_PASSWORD']), 48)
        self.assertEqual(len(config['SETUP_TOKEN']), 64)
        self.assertNotIn(config['SETUP_TOKEN'], result.stdout + result.stderr)

    def test_external_clearnet_local_http(self):
        url = 'postgresql://test:local-only@database:5432/scratch?sslmode=require'
        result = self.run_installer(['clearnet', url, 'yes', '', ''])
        self.assert_started(result)
        config = self.config()
        self.assertEqual(config['DATABASE_URL'], url)
        self.assertEqual(config['COOKIE_SECURE'], 'false')
        self.assertEqual(config['COMPOSE_PROFILES'], '')
        self.assertEqual(config['COMPOSE_FILE'], 'compose.yaml:compose.clearnet.yaml:compose.nodes.yaml')
        self.assertNotIn(url, result.stdout + result.stderr)

    def test_internal_tor_stays_without_direct_egress(self):
        result = self.run_installer(['tor', '', '', ''])
        self.assert_started(result)
        config = self.config()
        self.assertEqual(config['COOKIE_SECURE'], 'false')
        self.assertEqual(config['COMPOSE_PROFILES'], 'internal-db')
        self.assertIn('compose.tor.yaml', config['COMPOSE_FILE'])
        self.assertNotIn('compose.external-egress.yaml', config['COMPOSE_FILE'])

    def test_external_tor_explicitly_enables_egress(self):
        result = self.run_installer(['tor', 'postgres://test@database/scratch', '', ''])
        self.assert_started(result)
        config = self.config()
        self.assertEqual(config['COMPOSE_PROFILES'], '')
        self.assertNotIn('compose.internal-db.yaml', config['COMPOSE_FILE'])
        self.assertTrue(config['COMPOSE_FILE'].endswith(':compose.external-egress.yaml'))
        self.assertIn('direct network egress is enabled', result.stdout)

    def test_external_rpc_also_enables_tor_egress(self):
        result = self.run_installer(['tor', '', 'external', 'https://rpc.example.test', ''])
        self.assert_started(result)
        config = self.config()
        self.assertEqual(config['BITCOIN_RPC_URL'], 'https://rpc.example.test')
        self.assertEqual(config['COMPOSE_PROFILES'], 'internal-db')
        self.assertIn('compose.external-egress.yaml', config['COMPOSE_FILE'])

    def test_local_node_profiles_and_images(self):
        result = self.run_installer([
            'tor', '', 'local', 'reviewed-bitcoin:test', 'local', 'reviewed-monero:test',
        ])
        self.assert_started(result)
        config = self.config()
        self.assertEqual(config['COMPOSE_PROFILES'], 'internal-db,bitcoin,monero')
        self.assertEqual(config['BITCOIN_IMAGE'], 'reviewed-bitcoin:test')
        self.assertEqual(config['MONERO_IMAGE'], 'reviewed-monero:test')
        self.assertIn('@bitcoin:8332', config['BITCOIN_RPC_URL'])
        self.assertEqual(config['MONERO_RPC_URL'], 'http://monero:18081')
        self.assertNotIn('compose.external-egress.yaml', config['COMPOSE_FILE'])

    def test_existing_env_is_never_changed(self):
        path = self.root / '.env'
        original = b'EXISTING_TEST_VALUE=preserve-me\n'
        path.write_bytes(original)
        result = self.run_installer([])
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(path.read_bytes(), original)
        self.assertEqual(self.calls(), ['compose version'])

    def test_failed_compose_validation_never_starts_services(self):
        result = self.run_installer(['tor', '', '', ''], bad_config=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.calls(), ['compose version', 'compose config --quiet'])
        self.config()  # The retained configuration still has restrictive permissions.

    def test_invalid_input_leaves_no_configuration_or_services(self):
        scenarios = [
            ['invalid-mode'],
            ['tor', 'https://not-postgres'],
            ['tor', '', 'invalid-node'],
            ['tor', '', 'external', 'ftp://invalid-rpc'],
            ['tor', '', 'local', ''],
            ['tor', "postgres://test:quote'@database/scratch", '', ''],
        ]
        for answers in scenarios:
            with self.subTest(case=scenarios.index(answers)):
                self.log.unlink(missing_ok=True)
                result = self.run_installer(answers)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(self.calls(), ['compose version'])
                self.assertFalse((self.root / '.env').exists())
                self.assertEqual(list(self.root.glob('.env.install.*')), [])


if __name__ == '__main__':
    unittest.main()
