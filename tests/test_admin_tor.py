"""Private administrator deployment checks: auth must fail closed and no HTTP privilege comes from headers."""
import base64
from pathlib import Path
import subprocess
import tempfile
import unittest

from test_compose_exposure import exposure_problems, resolved, compose_available
from test_compose_hardening import hardening_problems

REPOSITORY = Path(__file__).resolve().parents[1]
ENTRYPOINT = REPOSITORY / 'deploy/tor-admin-entrypoint.sh'
VALID = 'descriptor:x25519:' + base64.b32encode(bytes(range(32))).decode().rstrip('=')


class AdminClientAuthTests(unittest.TestCase):
    def check(self, files):
        with tempfile.TemporaryDirectory() as directory:
            clients = Path(directory) / 'clients'
            clients.mkdir()
            for name, content in files.items():
                (clients / name).write_text(content)
            return subprocess.run(['sh', str(ENTRYPOINT), str(clients), str(Path(directory) / 'authorized_clients'),
                                   'sh', '-c', 'printf started'],
                                  capture_output=True, text=True)

    def test_valid_single_and_multiple_clients(self):
        for files in ({'operator.auth': VALID}, {'alice.auth': VALID + '\n', 'bob.auth': VALID}):
            done = self.check(files)
            self.assertEqual(done.returncode, 0, done.stderr)
            self.assertEqual(done.stdout, 'started')

    def test_empty_or_malformed_allowlist_never_starts_tor(self):
        for files in ({}, {'.auth': VALID}, {'ignored.txt': VALID}, {'client.auth': ''}, {'client.auth': VALID + '\n\n'},
                      {'client.auth': VALID.replace('descriptor:', 'invalid:')},
                      {'client.auth': VALID[:-1] + 'B'}, {'client.auth': VALID.lower()},
                      {'client.auth': VALID + '\r\n'}, {'operator.auth_private': VALID},
                      {'good.auth': VALID, 'bad.auth': 'malformed'}):
            with self.subTest(files=list(files)):
                done = self.check(files)
                self.assertNotEqual(done.returncode, 0)
                self.assertEqual(done.stdout, '')
                self.assertNotIn(VALID, done.stderr)

    def test_symlink_is_refused(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            clients = root / 'clients'
            clients.mkdir()
            public = root / 'key'
            public.write_text(VALID)
            (clients / 'client.auth').symlink_to(public)
            done = subprocess.run(['sh', str(ENTRYPOINT), str(clients), '--check'], capture_output=True)
            self.assertNotEqual(done.returncode, 0)

    def test_snapshot_replaces_revoked_clients_and_is_independent_of_host_edits(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source, snapshot = root / 'source', root / 'authorized_clients'
            source.mkdir()
            snapshot.mkdir()
            (snapshot / 'revoked.auth').write_text(VALID)
            (source / 'operator.auth').write_text(VALID + '\n')
            done = subprocess.run(['sh', str(ENTRYPOINT), str(source), str(snapshot), 'true'], capture_output=True)
            self.assertEqual(done.returncode, 0, done.stderr)
            self.assertEqual(sorted(p.name for p in snapshot.iterdir()), ['operator.auth'])
            self.assertEqual(snapshot.stat().st_mode & 0o777, 0o700)
            self.assertEqual((snapshot / 'operator.auth').stat().st_mode & 0o777, 0o600)
            (source / 'operator.auth').write_text('malformed')
            self.assertEqual((snapshot / 'operator.auth').read_text(), VALID + '\n')
            done = subprocess.run(['sh', str(ENTRYPOINT), str(source), str(snapshot), 'true'], capture_output=True)
            self.assertNotEqual(done.returncode, 0)

    def test_private_directory_initialized_only_after_authorization_passes(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source, service = root / 'source', root / 'admin'
            source.mkdir()
            arguments = ['sh', str(ENTRYPOINT), str(source), str(service / 'authorized_clients'), 'true']
            done = subprocess.run(arguments, capture_output=True)
            self.assertNotEqual(done.returncode, 0)
            self.assertFalse(service.exists())
            (source / 'operator.auth').write_text(VALID + '\n')
            done = subprocess.run(arguments, capture_output=True)
            self.assertEqual(done.returncode, 0, done.stderr)
            self.assertEqual(service.stat().st_mode & 0o777, 0o700)
            self.assertEqual((service / 'authorized_clients/operator.auth').read_text(), VALID + '\n')

    def test_shared_tor_image_does_not_prepopulate_identity_root(self):
        # Storefront/mirror identity import and restore require a fresh empty named volume. Creating admin
        # directories in the shared image would copy them into every fresh volume and break that refusal.
        dockerfile = (REPOSITORY / 'deploy/Dockerfile.tor').read_text()
        self.assertNotIn('/var/lib/tor/admin', dockerfile)
        self.assertIn('COPY tor-admin-entrypoint.sh /usr/local/bin/tor-admin-entrypoint', dockerfile)

    def test_private_torrc_has_only_socket_backend_and_no_client_or_control_port(self):
        lines = [line.split(None, 1) for line in (REPOSITORY / 'deploy/torrc-admin').read_text().splitlines()
                 if line and not line.startswith('#')]
        self.assertIn(['HiddenServicePort', '80 unix:/run/market-admin/http.sock'], lines)
        self.assertEqual([v for k, v in lines if k == 'HiddenServiceVersion'], ['3'])
        self.assertEqual([v for k, v in lines if k in ('SocksPort', 'ControlPort')], ['0', '0'])
        self.assertEqual([v for k, v in lines if k == 'HiddenServiceDir'], ['/var/lib/tor/admin'])
        self.assertEqual([v for k, v in lines if k == 'HiddenServicePort'], ['80 unix:/run/market-admin/http.sock'])

    def test_ci_validates_private_tor_config_with_real_image_and_no_network(self):
        workflow = (REPOSITORY / '.github/workflows/ci.yaml').read_text()
        step = workflow.split('      - name: Private administrator onion configuration and authorization preflight\n', 1)[1].split('      - name:', 1)[0]
        for text in ('docker run --rm --network none --read-only --cap-drop ALL',
                     "--tmpfs '/tmp:rw,nosuid,nodev,noexec,mode=1777'",
                     '--entrypoint /usr/local/bin/tor-admin-entrypoint opsecmkt-tor:ci',
                     '/run/tor-admin-clients /var/lib/tor/admin/authorized_clients tor -f /etc/tor/torrc-admin',
                     'if output=$("${tor[@]}" --verify-config 2>&1); then',
                     '"${tor[@]}" --verify-config', "'HiddenServicePort 80 unix:/run/market-admin/http.sock'"):
            self.assertIn(text, step)


@unittest.skipUnless(compose_available(), 'Docker Compose is not installed')
class PrivateAdminComposeTests(unittest.TestCase):
    def test_admin_tor_only_shares_socket_with_app_and_requires_auth_directory(self):
        with tempfile.TemporaryDirectory() as directory:
            config = resolved(['-f', 'compose.yaml', '-f', 'compose.tor.yaml', '-f', 'compose.admin-tor.yaml'], [],
                              {'TOR_ADMIN_AUTH_DIR': directory})
            self.assertEqual(exposure_problems(config, 'tor'), [])
            self.assertEqual(hardening_problems(config), [])
            services = config['services']
            private = services['tor-admin']
            self.assertEqual(sorted(private['networks']), ['tor_egress'])
            self.assertEqual(private['group_add'], ['65532'])
            self.assertEqual(services['app']['environment']['ADMIN_SOCKET'], '/run/market-admin/http.sock')
            self.assertEqual(private['entrypoint'], ['/usr/local/bin/tor-admin-entrypoint'])
            self.assertEqual(private['command'], ['/run/tor-admin-clients', '/var/lib/tor/admin/authorized_clients', 'tor', '-f', '/etc/tor/torrc-admin'])
            self.assertEqual(private['depends_on']['app']['condition'], 'service_healthy')
            holders = [name for name, service in services.items()
                       if any(v.get('source') == 'admin_socket' for v in service.get('volumes', []))]
            self.assertEqual(sorted(holders), ['app', 'tor-admin'])
            binds = [v for v in private['volumes'] if v['type'] == 'bind']
            self.assertEqual(len(binds), 1)
            self.assertEqual(binds[0]['source'], directory)
            self.assertTrue(binds[0]['read_only'])
            # Compose versions may omit false fields from their normalized JSON.
            self.assertFalse(binds[0]['bind'].get('create_host_path', False))
            self.assertEqual(binds[0]['target'], '/run/tor-admin-clients')
            socket = next(v for v in private['volumes'] if v.get('source') == 'admin_socket')
            self.assertTrue(socket['read_only'])

    def test_missing_auth_directory_setting_refuses_compose(self):
        with self.assertRaisesRegex(AssertionError, 'TOR_ADMIN_AUTH_DIR'):
            resolved(['-f', 'compose.yaml', '-f', 'compose.tor.yaml', '-f', 'compose.admin-tor.yaml'], [],
                     {'TOR_ADMIN_AUTH_DIR': ''})
