"""What the resolved Compose configuration publishes to the host (war room A-518 (d), from A-38 and A-502).

docs/acceptance.md says no internal database port or blockchain RPC port is published, that the application cannot
reach the Docker socket, and that a Tor-only deployment publishes no application host port. Each overlay is correct
on its own in the text checks; this resolves `docker compose config --format json` for every mode and profile
combination that .github/workflows/ci.yaml's compose job validates (clearnet and Tor; internal and external
database; Tor mirror; nodes over Tor; each also with the malware scanner and with the Monero escrow wallet) and
checks the merged result:

* in clearnet mode only `app` publishes a port, and only on 127.0.0.1 (the reverse proxy on the same host is the
  public face);
* in Tor mode nothing publishes a port (the onion service reaches `app` over the Compose network);
* no service shares the host network, and nothing mounts the Docker socket.

It also reads the monerod command each node overlay resolves to and checks that the core RPC server and the restricted
one (the port the app and monero-escrow-wallet use) bind different ports on mainnet and on stagenet (A-524: with both
on 18081, as on mainnet before, monerod exits at startup with "Failed to bind IPv4").

The checks skip only when `docker compose` is absent; the CI operations job runs every test here and fails on any
skip. They need no Docker daemon and start nothing.
"""
import itertools
import json
import os
import re
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

REPOSITORY = Path(__file__).resolve().parents[1]

# Values the CI compose job supplies: public, synthetic, for configuration validation only.
ENVIRONMENT = {
    'DATABASE_URL': 'postgres://test:test@db/test',
    'SETUP_TOKEN': 'ci-validation-only-not-a-real-bootstrap-secret',
    'BITCOIN_RPC_AUTH': 'marketplace:0123456789abcdef0123456789abcdef$f25a796e58dd70ff7eb5b96b54de079cc7e3d6f40eb8a0fa55fd05f26a53635e',
    'ESCROW_XMR_WALLET_RPC_PASSWORD': 'ci-validation-only',
}
EXTRAS = {
    'scanner': (['-f', 'compose.scanner.yaml'], ['--profile', 'scanner'], {}),
    'monero escrow wallet': (['-f', 'compose.escrow-xmr.yaml'], ['--profile', 'monero-escrow-wallet'],
                             {'ESCROW_XMR': 'on', 'ESCROW_XMR_SEED_KEY': '/etc/opsecmkt/escrow-xmr-seed-key.asc'}),
}


def compose_available():
    if not shutil.which('docker'):
        return False
    return subprocess.run(['docker', 'compose', 'version'], capture_output=True).returncode == 0


def combinations():
    """The compose job's matrix: mode x database x mirror x nodes_tor, less the combinations it excludes."""
    for mode, database, mirror, nodes_tor in itertools.product(('clearnet', 'tor'), ('internal', 'external'),
                                                               (False, True), (False, True)):
        if mode == 'clearnet' and (mirror or nodes_tor):
            continue
        files = ['-f', 'compose.yaml', '-f', f'compose.{mode}.yaml', '-f', 'compose.nodes.yaml']
        profiles = ['--profile', 'bitcoin', '--profile', 'monero', '--profile', 'escrow-signer']
        if database == 'internal':
            files += ['-f', 'compose.internal-db.yaml']
            profiles += ['--profile', 'internal-db']
        elif mode == 'tor':
            files += ['-f', 'compose.external-egress.yaml']
        if mirror:
            profiles += ['--profile', 'mirror']
        if nodes_tor:
            files += ['-f', 'compose.nodes-tor.yaml']
        name = f'{mode}, {database} database' + (', mirror' if mirror else '') + (', nodes over Tor' if nodes_tor else '')
        yield name, mode, files, profiles


def resolved(files, profiles, environment=None):
    """`docker compose config --format json` for files and profiles, as the compose job runs it (no .env file)."""
    env = {key: value for key, value in os.environ.items() if not key.startswith(('COMPOSE_', 'APP_'))}
    env.update(ENVIRONMENT)
    env.update(environment or {})
    with tempfile.TemporaryDirectory() as directory:
        empty = Path(directory) / 'env'
        empty.write_text('')
        done = subprocess.run(['docker', 'compose', '--env-file', str(empty), *files, *profiles, 'config', '--format', 'json'],
                              cwd=REPOSITORY, env=env, capture_output=True, text=True)
    if done.returncode:
        raise AssertionError(f'docker compose config failed for {files} {profiles}: {done.stderr}')
    return json.loads(done.stdout)


def exposure_problems(config, mode):
    """What the resolved configuration publishes or mounts that it must not."""
    problems = []
    for name, service in sorted(config['services'].items()):
        for port in service.get('ports') or []:
            if mode == 'tor':
                problems.append(f'{name} publishes {port} in Tor mode')
            elif name != 'app':
                problems.append(f'{name} publishes {port}: only app may, and only on loopback')
            elif port.get('host_ip') != '127.0.0.1':
                problems.append(f'app publishes {port}, not bound to 127.0.0.1')
        if service.get('network_mode') == 'host':
            problems.append(f'{name} shares the host network')
        for volume in service.get('volumes') or []:
            if isinstance(volume, dict) and 'docker.sock' in f"{volume.get('source', '')} {volume.get('target', '')}":
                problems.append(f'{name} mounts the Docker socket: {volume}')
    if 'docker.sock' in json.dumps(config):
        problems.append('docker.sock appears in the resolved configuration')
    if mode == 'clearnet' and not (config['services']['app'].get('ports') or []):
        problems.append('the clearnet app publishes nothing, so the reverse proxy cannot reach it')
    return problems


# monerod's core RPC port when --rpc-bind-port is not given (cryptonote_config.h, v0.18.5.1).
MONEROD_DEFAULT_RPC_PORT = {'mainnet': 18081, 'stagenet': 38081, 'testnet': 28081}


def monerod_rpc_ports(command):
    """(network, core RPC address, core port, restricted port) that monerod binds for a resolved command list. The
    address is None unless --rpc-bind-ip pins it: the image's monerod, given the old command, bound 0.0.0.0."""
    options, network = {}, 'mainnet'
    for index, argument in enumerate(command):
        if argument in ('--stagenet', '--testnet'):
            network = argument[2:]
        elif argument.startswith('--rpc'):
            name, separator, value = argument.partition('=')
            if not separator and index + 1 < len(command) and not command[index + 1].startswith('--'):
                value = command[index + 1]
            options[name] = value
    core = int(options.get('--rpc-bind-port') or MONEROD_DEFAULT_RPC_PORT[network])
    restricted = int(options['--rpc-restricted-bind-port']) if '--rpc-restricted-bind-port' in options else None
    return network, options.get('--rpc-bind-ip'), core, restricted


class ExposureCheckTests(unittest.TestCase):
    """The check itself, on small configurations: it needs no Docker."""

    def test_flags_what_it_must(self):
        app = {'ports': [{'host_ip': '127.0.0.1', 'published': '8080', 'target': 8080}]}
        self.assertEqual(exposure_problems({'services': {'app': app}}, 'clearnet'), [])
        self.assertEqual(exposure_problems({'services': {'app': {}}}, 'tor'), [])
        self.assertTrue(exposure_problems({'services': {'app': {'ports': [{'published': '8080', 'target': 8080}]}}}, 'clearnet'))
        self.assertTrue(exposure_problems({'services': {'app': {'ports': [{'host_ip': '0.0.0.0', 'target': 8080}]}}}, 'clearnet'))
        self.assertTrue(exposure_problems({'services': {'app': app, 'bitcoin': {'ports': [{'host_ip': '127.0.0.1', 'target': 8332}]}}}, 'clearnet'))
        self.assertTrue(exposure_problems({'services': {'app': app}}, 'tor'))
        self.assertTrue(exposure_problems({'services': {'app': {}}}, 'clearnet'))
        self.assertTrue(exposure_problems({'services': {'app': app, 'x': {'network_mode': 'host'}}}, 'clearnet'))
        sock = {'type': 'bind', 'source': '/var/run/docker.sock', 'target': '/var/run/docker.sock'}
        self.assertTrue(exposure_problems({'services': {'app': {**app, 'volumes': [sock]}}}, 'clearnet'))
        self.assertTrue(exposure_problems({'services': {'app': {**app, 'environment': {'DOCKER_HOST': 'unix:///var/run/docker.sock'}}}}, 'clearnet'))

    def test_the_compose_job_matrix_is_the_one_enumerated_here(self):
        """The combinations above follow ci.yaml's compose job; this fails when the job gains or loses a dimension."""
        workflow = (REPOSITORY / '.github' / 'workflows' / 'ci.yaml').read_text()
        job = workflow[workflow.index('\n  compose:\n'):workflow.index('\n  container:\n')]
        for line in ("mode: [clearnet, tor]", "database: [internal, external]", "mirror: ['off', 'on']",
                     "nodes_tor: ['off', 'on']", '- mode: clearnet\n            mirror: \'on\'',
                     '- mode: clearnet\n            nodes_tor: \'on\''):
            self.assertIn(line, job)

    def test_monerod_port_reader_sees_the_mainnet_collision(self):
        """The command before A-524: on mainnet the core server's default port is the restricted one."""
        before = ['--data-dir=/home/monero/.bitmonero', '--restricted-rpc', '--rpc-restricted-bind-ip=0.0.0.0',
                  '--rpc-restricted-bind-port=18081', '--confirm-external-bind']
        self.assertEqual(monerod_rpc_ports(before), ('mainnet', None, 18081, 18081))
        self.assertEqual(monerod_rpc_ports(['--stagenet', *before]), ('stagenet', None, 38081, 18081))
        self.assertEqual(monerod_rpc_ports(['--rpc-bind-port', '18089', *before])[2], 18089)


@unittest.skipUnless(compose_available(), 'docker compose is not available')
class ResolvedComposeExposureTests(unittest.TestCase):
    def test_every_mode_and_profile_combination_publishes_only_the_loopback_app(self):
        seen = 0
        for name, mode, files, profiles in combinations():
            variants = [('', [], [], {})] + [(f' + {extra}', f, p, e) for extra, (f, p, e) in EXTRAS.items()]
            for suffix, extra_files, extra_profiles, environment in variants:
                with self.subTest(name + suffix):
                    config = resolved(files + extra_files, profiles + extra_profiles, environment)
                    self.assertEqual(exposure_problems(config, mode), [])
                    for service_name in ('db', 'bitcoin', 'monero', 'monero-escrow-wallet'):
                        if service_name not in config['services']:
                            continue
                        service = config['services'][service_name]
                        self.assertIs(service.get('read_only'), True, service_name)
                        self.assertEqual(service.get('cap_drop'), ['ALL'], service_name)
                        self.assertEqual(set(service.get('cap_add', [])),
                                         {'CHOWN', 'DAC_OVERRIDE', 'FOWNER', 'SETGID', 'SETUID'}
                                         if service_name == 'bitcoin' else set(), service_name)
                        self.assertIn('no-new-privileges:true', service.get('security_opt', []), service_name)
                        self.assertIn('/tmp', service.get('tmpfs', []), service_name)
                        if service_name == 'monero-escrow-wallet':
                            command = service['command']
                            self.assertIn('--shared-ringdb-dir=/home/monero/wallet/.shared-ringdb', command)
                            self.assertIn('--max-log-file-size=1048576', command)
                            self.assertIn('--max-log-files=2', command)
                    seen += 1
        self.assertEqual(seen, 10 * 3, 'the combinations enumerated no longer match the compose job matrix')

    def test_monerod_rpc_ports_distinct(self):
        """A-524: monerod's core RPC (loopback only) and its restricted RPC never share a port, on mainnet (a blank
        MONERO_NETWORK_FLAG) or stagenet, in compose.nodes.yaml and in compose.nodes-tor.yaml's repeat of its command.
        The restricted port stays 18081, which the app (ESCROW_XMR_DAEMON_URL) and monero-escrow-wallet use."""
        overlays = {'compose.nodes.yaml': 'clearnet, internal database',
                    'compose.nodes-tor.yaml': 'tor, external database, nodes over Tor'}
        matrix = {name: (files, profiles) for name, _, files, profiles in combinations()}
        for overlay, combination in overlays.items():
            for flag, network in (('', 'mainnet'), ('--stagenet', 'stagenet')):
                with self.subTest(overlay=overlay, MONERO_NETWORK_FLAG=flag):
                    files, profiles = matrix[combination]
                    self.assertIn(overlay, files)
                    services = resolved(files, profiles + ['--profile', 'monero-escrow-wallet'],
                                        {'MONERO_NETWORK_FLAG': flag})['services']
                    command = services['monero']['command']
                    self.assertIn('--restricted-rpc', command)
                    self.assertIn('--rpc-restricted-bind-ip=0.0.0.0', command)
                    seen, core_ip, core, restricted = monerod_rpc_ports(command)
                    self.assertEqual(seen, network)
                    self.assertEqual((core_ip, core, restricted), ('127.0.0.1', 18089, 18081))
                    self.assertNotEqual(core, restricted)
                    wallet = services['monero-escrow-wallet']['command']
                    self.assertEqual(wallet[wallet.index('--daemon-address') + 1], 'monero:18081')

    def test_internal_database_has_no_default_password_and_uses_a_digest(self):
        files = ['-f', 'compose.yaml', '-f', 'compose.internal-db.yaml']
        config = resolved(files, ['--profile', 'internal-db'], {'POSTGRES_PASSWORD': ''})
        db = config['services']['db']
        self.assertEqual(db['environment']['POSTGRES_PASSWORD'], '')
        self.assertRegex(db['image'], r'^postgres:17-alpine@sha256:[0-9a-f]{64}$')
        self.assertIn('/var/run/postgresql', db['tmpfs'])
        supplied = resolved(files, ['--profile', 'internal-db'], {'POSTGRES_PASSWORD': 'explicit-test-password'})
        self.assertEqual(supplied['services']['db']['environment']['POSTGRES_PASSWORD'], 'explicit-test-password')

    def test_application_base_images_are_pinned(self):
        images = re.findall(r'^FROM\s+(\S+)', (REPOSITORY / 'Dockerfile').read_text(), re.M)
        self.assertEqual(len(images), 2)
        for image in images:
            self.assertRegex(image, r'@sha256:[0-9a-f]{64}$')


if __name__ == '__main__':
    unittest.main()
