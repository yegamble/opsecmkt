"""Exercise installer decisions in disposable directories with no real Docker."""
import os
from pathlib import Path
import shutil
import signal
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
import os
from pathlib import Path
import sys
assert sys.argv[1:3] == ['rand', '-hex']
counter = Path(os.environ['INSTALLER_TEST_LOG'] + '.openssl')
call = int(counter.read_text()) + 1 if counter.exists() else 1
counter.write_text(str(call))
if str(call) == os.environ.get('INSTALLER_TEST_OPENSSL_EMPTY_CALL'):
    sys.exit(1)  # generation failure: no output
print('a1' * int(sys.argv[3]))
''')
        # The unhealthy path polls 60 times. These trivial stubs need no Python
        # interpreter startup on every curl/sleep invocation under CI load.
        self.stub('curl', '''
for endpoint do :; done
case "$endpoint" in */healthz) ;; *) exit 99 ;; esac
if [ "${INSTALLER_TEST_UNHEALTHY:-}" = 1 ]; then exit 7; fi
printf 'ok\\n'
''', interpreter='/bin/sh')
        self.stub('sleep', 'exit 0', interpreter='/bin/sh')

    def stub(self, name, body, interpreter=None):
        path = self.bin / name
        path.write_text(f'#!{interpreter or sys.executable}\n' + body)
        path.chmod(0o700)

    def run_installer(self, answers, bad_config=False, openssl_empty_call=None, arguments=None, extra_env=None, timeout=10):
        # Do not source or copy the user's environment file. Compose is a stub;
        # all writes and generated credentials stay inside this temporary repo.
        env = {
            'PATH': str(self.bin) + os.pathsep + os.defpath,
            'HOME': str(self.root),
            'INSTALLER_TEST_LOG': str(self.log),
            'INSTALLER_TEST_BAD_CONFIG': '1' if bad_config else '0',
            'INSTALLER_TEST_OPENSSL_EMPTY_CALL': str(openssl_empty_call or ''),
            # macOS's Python can redirect bytecode caches beneath the temporary
            # HOME. Stubs must not keep populating it while cleanup removes it.
            'PYTHONDONTWRITEBYTECODE': '1',
            **(extra_env or {}),
        }
        Path(str(self.log) + '.openssl').unlink(missing_ok=True)
        process = subprocess.Popen(
            ['/bin/bash', str(self.root / 'scripts' / 'install.sh'), *(arguments or [])],
            cwd=self.root, env=env, stdin=subprocess.PIPE,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
            start_new_session=True,
        )
        try:
            stdout, stderr = process.communicate('\n'.join(answers) + '\n', timeout=timeout)
        except subprocess.TimeoutExpired as expired:
            # Killing bash alone leaves Python stubs alive with open pipes and
            # a temporary HOME. Kill only this test's dedicated process group.
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            try:
                expired.output, expired.stderr = process.communicate(timeout=5)
            except subprocess.TimeoutExpired:
                # Never wait indefinitely even if an unexpected descendant has
                # escaped the process group while retaining an output pipe.
                process.stdout.close()
                process.stderr.close()
            raise expired
        return subprocess.CompletedProcess(process.args, process.returncode, stdout, stderr)

    def test_timeout_stops_stub_descendants_without_bytecode_writes(self):
        self.stub('docker', '''
import os
from pathlib import Path
import subprocess
import sys
import time
assert sys.dont_write_bytecode
child = subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(60)'])
Path(os.environ['INSTALLER_TEST_LOG'] + '.child').write_text(str(child.pid))
time.sleep(60)
''')
        with self.assertRaises(subprocess.TimeoutExpired):
            self.run_installer([], timeout=2)
        child = int(Path(str(self.log) + '.child').read_text())
        # A killed orphan may briefly await reaping; a zombie cannot write to
        # the temporary directory or keep captured output pipes open.
        state = subprocess.run(
            ['/bin/ps', '-o', 'stat=', '-p', str(child)],
            capture_output=True, text=True, timeout=5, check=False,
        ).stdout.strip()
        self.assertTrue(not state or state.startswith('Z'), state)
        self.assertEqual(list(self.root.rglob('*.pyc')), [])

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
        self.assertEqual(len(config['AUDIT_SIGNING_KEY']), 64)
        self.assertNotIn(config['AUDIT_SIGNING_KEY'], result.stdout + result.stderr)
        # No node chosen: nothing forces a chain, so a node added later may use any test network.
        self.assertEqual(config['BITCOIN_CHAIN'], '')
        self.assertEqual(config['MONERO_NETWORK'], '')
        self.assertNotIn('BITCOIN_RPC_URL', config)
        self.assertNotIn('MONERO_WALLET_RPC_URL', config)
        self.assertNotIn('docs/testnet-runbook.md', result.stdout)
        self.assertIn('Secure cookies require HTTPS', result.stdout)

    def test_local_shortcut_needs_no_input_and_disables_inherited_wallets(self):
        result = self.run_installer([], arguments=['--local'], extra_env={
            'APP_PORT': '18085', 'DATABASE_URL': 'postgres://unrelated/existing',
            'BITCOIN_RPC_URL': 'http://unrelated-wallet', 'COMPOSE_PROFILES': 'bitcoin',
        })
        self.assert_started(result)
        config = self.config()
        self.assertEqual(config['COOKIE_SECURE'], 'false')
        self.assertEqual(config['APP_PORT'], '18085')
        self.assertEqual(config['COMPOSE_PROFILES'], 'internal-db')
        self.assertIn('@db:5432/opsecmkt', config['DATABASE_URL'])
        for key in ('BITCOIN_RPC_URL', 'MONERO_RPC_URL', 'MONERO_WALLET_RPC_URL'):
            self.assertEqual(config[key], '')
        self.assertIn('http://127.0.0.1:18085/setup', result.stdout)
        self.assertNotIn('docs/testnet-runbook.md', result.stdout)
        self.assertNotIn(config['SETUP_TOKEN'], result.stdout + result.stderr)

    def test_unhealthy_local_app_does_not_report_ready(self):
        result = self.run_installer([], arguments=['--local'], extra_env={'INSTALLER_TEST_UNHEALTHY': '1'})
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('did not become healthy', result.stderr)
        self.assertNotIn('Ready.', result.stdout)
        self.config()  # Keep the generated configuration for diagnosis/restart.

    def test_invalid_local_port_starts_nothing(self):
        result = self.run_installer([], arguments=['--local'], extra_env={'APP_PORT': '65536'})
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('APP_PORT must', result.stderr)
        self.assertFalse((self.root / '.env').exists())
        self.assertEqual(self.calls(), ['compose version'])

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
        # External nodes are not forced onto one chain; the app accepts any test chain the node reports.
        self.assertEqual(config['BITCOIN_CHAIN'], '')
        self.assertEqual(config['COMPOSE_PROFILES'], 'internal-db')
        self.assertIn('compose.external-egress.yaml', config['COMPOSE_FILE'])
        self.assertIn('docs/testnet-runbook.md', result.stdout)

    def test_local_node_profiles_and_images(self):
        result = self.run_installer([
            'tor', '', 'local', 'reviewed-bitcoin:test', 'testnet4', 'local', 'reviewed-monero:test', 'stagenet',
        ])
        self.assert_started(result)
        config = self.config()
        self.assertEqual(config['COMPOSE_PROFILES'], 'internal-db,bitcoin,monero,monero-wallet')
        self.assertEqual(config['BITCOIN_IMAGE'], 'reviewed-bitcoin:test')
        self.assertEqual(config['MONERO_IMAGE'], 'reviewed-monero:test')
        self.assertEqual(config['MONERO_WALLET_IMAGE'], 'ghcr.io/sethforprivacy/simple-monero-wallet-rpc:latest')
        self.assertEqual(len(config['BITCOIN_RPC_PASSWORD']), 48)
        self.assertEqual(config['BITCOIN_RPC_URL'],
                         'http://marketplace:' + config['BITCOIN_RPC_PASSWORD'] + '@bitcoin:8332')
        self.assertEqual(config['BITCOIN_CHAIN'], 'testnet4')
        self.assertEqual(config['MONERO_NETWORK'], 'stagenet')
        self.assertEqual(config['MONERO_RPC_URL'], 'http://monero:18081')
        # monero-wallet-rpc runs with --rpc-login; the app authenticates with the same generated password.
        self.assertEqual(len(config['MONERO_WALLET_RPC_PASSWORD']), 48)
        self.assertEqual(config['MONERO_WALLET_RPC_URL'],
                         'http://marketplace:' + config['MONERO_WALLET_RPC_PASSWORD'] + '@monero-wallet:18083')
        self.assertNotIn(config['MONERO_WALLET_RPC_PASSWORD'], result.stdout + result.stderr)
        self.assertNotIn('compose.external-egress.yaml', config['COMPOSE_FILE'])

    def test_local_node_rejects_incomplete_or_malformed_image_before_compose(self):
        for image in ('@sha256', 'vendor/node@sha256:deadbeef', 'vendor/node:'):
            with self.subTest(image=image):
                self.log.unlink(missing_ok=True)
                result = self.run_installer(['clearnet', '', 'yes', 'local', image])
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('Invalid', result.stderr)
                self.assertEqual(self.calls(), ['compose version'])
                self.assertFalse((self.root / '.env').exists())

    def test_local_node_accepts_a_complete_digest_pinned_reference(self):
        digest_image = 'registry.example:5000/bitcoin/core@sha256:' + ('a' * 64)
        result = self.run_installer(['clearnet', '', 'yes', 'local', digest_image, 'testnet4', 'disabled'])
        self.assert_started(result)
        self.assertEqual(self.config()['BITCOIN_IMAGE'], digest_image)

    def test_local_node_defaults_to_current_trusted_images(self):
        result = self.run_installer(['tor', '', 'local', '', 'testnet4', 'local', '', 'stagenet'])
        self.assert_started(result)
        config = self.config()
        self.assertEqual(config['BITCOIN_IMAGE'], 'bitcoin/bitcoin:latest')
        self.assertEqual(config['MONERO_IMAGE'], 'ghcr.io/sethforprivacy/simple-monerod:latest')
        self.assertEqual(config['MONERO_WALLET_IMAGE'], 'ghcr.io/sethforprivacy/simple-monero-wallet-rpc:latest')

    def test_local_mainnet_choice_fails_closed_before_writing_config(self):
        result = self.run_installer(['tor', '', 'local', '', 'live'])
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('Live Bitcoin payments are disabled', result.stderr)
        self.assertFalse((self.root / '.env').exists())
        self.assertEqual(self.calls(), ['compose version'])

    def test_external_monero_wallet_rpc(self):
        daemon, wallet = 'https://monerod.example.test', 'http://user:pass@wallet.example.test:18083'
        result = self.run_installer(['tor', '', '', 'external', daemon, wallet])
        self.assert_started(result)
        config = self.config()
        self.assertEqual(config['MONERO_RPC_URL'], daemon)
        self.assertEqual(config['MONERO_WALLET_RPC_URL'], wallet)
        self.assertEqual(config['MONERO_NETWORK'], '')
        self.assertNotIn('MONERO_WALLET_RPC_PASSWORD', config)
        self.assertNotIn('WARNING', result.stderr)
        self.assertEqual(config['COMPOSE_PROFILES'], 'internal-db')
        self.assertIn('compose.external-egress.yaml', config['COMPOSE_FILE'])
        self.assertNotIn(wallet, result.stdout + result.stderr)

    def test_external_monero_daemon_without_wallet_warns(self):
        result = self.run_installer(['tor', '', '', 'external', 'https://monerod.example.test', ''])
        self.assert_started(result)
        config = self.config()
        self.assertNotIn('MONERO_WALLET_RPC_URL', config)
        self.assertIn('Monero payments stay disabled', result.stderr)
        # Nothing in the app would use the daemon, so Tor mode opens no direct egress for it.
        self.assertNotIn('compose.external-egress.yaml', config['COMPOSE_FILE'])

    def test_secret_generation_failure_starts_nothing(self):
        # openssl calls: 1 database password, 2 setup token, 3 audit key, then one per local node.
        scenarios = [
            (1, ['clearnet', '', '', '', '']),
            (4, ['tor', '', 'local', 'reviewed-bitcoin:test', 'testnet4', '']),
            (4, ['tor', '', '', 'local', 'reviewed-monero:test', 'stagenet']),
            (5, ['tor', '', 'local', 'reviewed-bitcoin:test', 'testnet4', 'local', 'reviewed-monero:test', 'stagenet']),
        ]
        for call, answers in scenarios:
            with self.subTest(call=call, answers=answers):
                self.log.unlink(missing_ok=True)
                result = self.run_installer(answers, openssl_empty_call=call)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('Could not generate a random secret', result.stderr)
                self.assertEqual(self.calls(), ['compose version'])
                self.assertFalse((self.root / '.env').exists())
                self.assertEqual(list(self.root.glob('.env.install.*')), [])

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
            ['tor', '', '', 'external', 'https://monerod.example.test', 'ftp://invalid-wallet'],
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
