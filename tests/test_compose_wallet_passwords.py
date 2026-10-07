"""Compose refuses a blank Monero wallet-rpc password (war room A-466).

The bundled monero-wallet and monero-escrow-wallet services must never start without their login password
(MONERO_WALLET_RPC_PASSWORD, ESCROW_XMR_WALLET_RPC_PASSWORD), while an install whose COMPOSE_PROFILES leaves them
out keeps working with neither set: compose.nodes.yaml is in every install's COMPOSE_FILE. The text checks need
nothing; the resolved-configuration checks run `docker compose config` and are skipped without Docker Compose.
"""
import json
import os
from pathlib import Path
import shutil
import subprocess
import unittest

REPOSITORY = Path(__file__).resolve().parents[1]
NODES = REPOSITORY / 'compose.nodes.yaml'
FILES = ('compose.yaml', 'compose.clearnet.yaml', 'compose.nodes.yaml', 'compose.escrow-xmr.yaml')
# service: (profile, password key, login user)
WALLETS = {
    'monero-wallet': ('monero-wallet', 'MONERO_WALLET_RPC_PASSWORD', 'marketplace'),
    'monero-escrow-wallet': ('monero-escrow-wallet', 'ESCROW_XMR_WALLET_RPC_PASSWORD', 'escrow'),
}


def compose_available():
    if not shutil.which('docker'):
        return False
    return subprocess.run(['docker', 'compose', 'version'], capture_output=True).returncode == 0


class WalletPasswordTextTests(unittest.TestCase):
    def test_header_comment_states_the_refusal(self):
        header = NODES.read_text().split('\nservices:\n', 1)[0]
        flat = ' '.join(line.lstrip('# ') for line in header.splitlines())
        self.assertIn('Compose refuses to create or start monero-wallet without MONERO_WALLET_RPC_PASSWORD', flat)
        self.assertIn('monero-escrow-wallet without ESCROW_XMR_WALLET_RPC_PASSWORD', flat)
        self.assertNotIn("monero-wallet-rpc gets --rpc-login ':'", flat)

    def test_login_user_follows_the_password(self):
        text = NODES.read_text()
        for service, (_, key, user) in WALLETS.items():
            with self.subTest(service):
                self.assertIn(f"--rpc-login '${{{key}:+{user}}}:${{{key}:-}}'", text)
                self.assertNotIn(f"--rpc-login '{user}:", text)


@unittest.skipUnless(compose_available(), 'Docker Compose is not installed')
class WalletPasswordConfigTests(unittest.TestCase):
    def config(self, profiles, **values):
        arguments = ['docker', 'compose', '--env-file', os.devnull, '-p', 'opsecmkt-wallet-password-test']
        for name in FILES:
            arguments += ['-f', name]
        for profile in profiles:
            arguments += ['--profile', profile]
        environment = {key: value for key, value in os.environ.items()
                       if not key.startswith('COMPOSE_') and key not in ('MONERO_WALLET_RPC_PASSWORD',
                                                                       'ESCROW_XMR_WALLET_RPC_PASSWORD')}
        environment['DATABASE_URL'] = 'postgres://unused@db/unused'
        environment.update(values)
        return subprocess.run(arguments + ['config', '--format', 'json'], cwd=REPOSITORY, env=environment,
                              capture_output=True, text=True, check=False)

    def test_blank_or_missing_password_is_refused(self):
        for service, (profile, key, _) in WALLETS.items():
            for values in ({}, {key: ''}):
                with self.subTest(service=service, set=bool(values)):
                    other = {k: 'other-password' for _, k, _ in WALLETS.values() if k != key}
                    result = self.config(('monero', profile), **values, **other)
                    self.assertNotEqual(result.returncode, 0, result.stdout)
                    self.assertIn(f'{key}-is-blank', result.stderr)

    def test_set_password_starts_with_its_login(self):
        values = {'MONERO_WALLET_RPC_PASSWORD': 'pooled-pass', 'ESCROW_XMR_WALLET_RPC_PASSWORD': 'escrow-pass'}
        result = self.config(('monero', 'monero-wallet', 'monero-escrow-wallet'), **values)
        self.assertEqual(result.returncode, 0, result.stderr)
        services = json.loads(result.stdout)['services']
        for service, (_, key, user) in WALLETS.items():
            with self.subTest(service):
                command = services[service]['command']
                self.assertEqual(command[command.index('--rpc-login') + 1], f'{user}:{values[key]}')
                # The refusal adds nothing to the container's environment.
                self.assertFalse(services[service].get('environment'), services[service].get('environment'))

    def test_installs_without_the_wallets_need_neither_password(self):
        for profiles in ((), ('bitcoin',), ('monero',), ('bitcoin', 'monero', 'escrow-signer')):
            with self.subTest(profiles=profiles):
                result = self.config(profiles)
                self.assertEqual(result.returncode, 0, result.stderr)
                services = json.loads(result.stdout)['services']
                self.assertNotIn('monero-wallet', services)
                self.assertNotIn('monero-escrow-wallet', services)

    def test_each_password_is_checked_only_for_its_own_service(self):
        result = self.config(('monero', 'monero-wallet'), MONERO_WALLET_RPC_PASSWORD='pooled-pass')
        self.assertEqual(result.returncode, 0, result.stderr)
        result = self.config(('monero', 'monero-escrow-wallet'), ESCROW_XMR_WALLET_RPC_PASSWORD='escrow-pass')
        self.assertEqual(result.returncode, 0, result.stderr)


if __name__ == '__main__':
    unittest.main()
