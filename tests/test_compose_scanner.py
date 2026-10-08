"""The malware scanner's Compose service and clamd configuration (war room A-184, rulings C1 and C6).

The text checks need nothing; the resolved-configuration checks run `docker compose config` and are skipped
without Docker Compose. scripts/test-scanner.sh runs the container itself.
"""
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import unittest

REPOSITORY = Path(__file__).resolve().parents[1]
COMPOSE = REPOSITORY / 'compose.scanner.yaml'
CLAMD_CONF = REPOSITORY / 'deploy' / 'clamd.conf'
SIZE = re.compile(r'^(\d+)([KM]?)$')


def service_block(text, name):
    """The lines of one service in a Compose file as this repository writes them (two-space indents)."""
    services = text.split('\nservices:\n', 1)[1]
    match = re.search(rf'^  {re.escape(name)}:\n((?:    .*\n|\n)*)', services, re.M)
    if not match:
        raise AssertionError(f'no service {name}')
    return match[1]


def clamd_options():
    """deploy/clamd.conf as {option: value}; an option set twice fails."""
    options = {}
    for line in CLAMD_CONF.read_text().splitlines():
        if line and not line.startswith('#'):
            key, _, value = line.partition(' ')
            if key in options:
                raise AssertionError(f'{key} is set twice')
            options[key] = value
    return options


def size_bytes(value):
    number, unit = SIZE.match(value).groups()
    return int(number) * {'': 1, 'K': 1024, 'M': 1024 ** 2}[unit]


def compose_available():
    if not shutil.which('docker'):
        return False
    return subprocess.run(['docker', 'compose', 'version'], capture_output=True).returncode == 0


class ScannerComposeTextTests(unittest.TestCase):
    def setUp(self):
        self.text = COMPOSE.read_text()
        self.clamd = service_block(self.text, 'clamd')

    def test_clamd_hardening_keys(self):
        for line in ('profiles: [scanner]', 'restart: unless-stopped', 'networks: [scanner]', 'entrypoint: [clamd]',
                     'command: [--foreground, --config-file=/etc/clamav/clamd.conf]', 'user: clamav',
                     'read_only: true', 'cap_drop: [ALL]', 'security_opt: [no-new-privileges:true]', 'mem_limit: 3g',
                     'cpu_period: 100000', 'cpu_quota: 200000', 'pids_limit: 256', 'logging: *logging'):
            with self.subTest(line=line):
                self.assertIn(f'\n    {line}\n', '\n' + self.clamd)
        healthcheck = re.search(r'^    healthcheck:\n((?:      .*\n)*)', self.clamd, re.M)[1]
        self.assertIn('test: [CMD, clamdcheck.sh]', healthcheck)
        self.assertIn('start_period: 10m', healthcheck)

    def test_clamd_has_no_ports_egress_dependencies_or_secrets(self):
        for key in ('ports', 'expose', 'network_mode', 'depends_on', 'environment', 'env_file', 'cpus', 'privileged',
                    'cap_add', 'extra_hosts', 'dns'):
            with self.subTest(key=key):
                self.assertNotRegex(self.clamd, rf'(?m)^    {key}:')
        settings = [line for line in self.clamd.splitlines() if not line.lstrip().startswith('#')]
        self.assertFalse([line for line in settings if 'freshclam' in line.lower()])
        # The app gains the scanner network only; it never waits for clamd.
        app = service_block(self.text, 'app')
        self.assertEqual(app, '    networks: [scanner]\n')

    def test_clamd_mounts_only_its_configuration_read_only(self):
        volumes = re.search(r'^    volumes:\n((?:      .*\n)*)', self.clamd, re.M)[1]
        self.assertEqual(volumes, '      - type: bind\n        source: ./deploy/clamd.conf\n'
                                  '        target: /etc/clamav/clamd.conf\n        read_only: true\n')
        # No named volume anywhere: signatures come from the image (no seed trap, ruling C1).
        self.assertNotRegex(self.text, r'(?m)^volumes:')

    def test_scanner_network_is_internal(self):
        self.assertIn('\nnetworks:\n', self.text)
        networks = self.text.split('\nnetworks:\n', 1)[1]
        self.assertRegex(networks, r'(?m)^  scanner:\n    internal: true\n')
        self.assertEqual(re.findall(r'(?m)^  ([a-z_-]+):', networks), ['scanner'])
        for name in ('compose.yaml', 'compose.tor.yaml', 'compose.clearnet.yaml', 'compose.nodes.yaml',
                     'compose.internal-db.yaml', 'compose.external-egress.yaml'):
            with self.subTest(name):
                text = (REPOSITORY / name).read_text()
                self.assertNotRegex(text, r'(?m)^\s+scanner:|networks: \[[^\]]*scanner')
                self.assertNotIn('clamd:\n', text)

    def test_tmpfs_holds_two_streams(self):
        tmpfs = re.search(r"^    tmpfs: \['/tmp:size=(\d+)m'\]$", self.clamd, re.M)
        self.assertIsNotNone(tmpfs, 'clamd needs exactly a sized /tmp tmpfs')
        self.assertGreaterEqual(int(tmpfs[1]) * 1024 ** 2, 2 * size_bytes(clamd_options()['StreamMaxLength']))

    def test_route_check_tolerates_missing_ipv6_procfs_and_keeps_ipv4_check(self):
        script = (REPOSITORY / 'scripts' / 'test-scanner.sh').read_text()
        start = script.index('routes=$(docker exec')
        end = script.index('\n[[ -z $routes ]]', start)
        command = script[start:end]
        self.assertIn('if [ -r /proc/net/ipv6_route ]; then cat /proc/net/ipv6_route; fi', command)
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            net = root / 'net'
            net.mkdir()
            route_file = net / 'route'
            route_file.write_text('Iface Destination Gateway Flags RefCnt Use Metric Mask MTU Window IRTT\n')
            docker = root / 'docker'
            docker.write_text('#!/bin/sh\n[ "$1" = exec ] || exit 90\nshift 2\nexec "$@"\n')
            docker.chmod(0o755)
            command = command.replace('/proc/net/route', str(route_file)).replace(
                '/proc/net/ipv6_route', str(net / 'ipv6_route'))
            command = command.replace('routes=$(docker exec "$name"', 'routes=$(docker exec scanner')
            command += '; printf "%s" "$routes"'
            env = {**os.environ, 'PATH': str(root) + os.pathsep + os.environ.get('PATH', '')}
            safe = subprocess.run(['bash', '-c', 'set -eo pipefail; ' + command], env=env,
                                  capture_output=True, text=True)
            self.assertEqual(safe.returncode, 0, safe.stderr)
            self.assertEqual(safe.stdout, '')
            route_file.write_text('Iface Destination Gateway Flags RefCnt Use Metric Mask MTU Window IRTT\n'
                                  'eth0 00000000 0100000A 0003 0 0 0 00000000 0 0 0\n')
            unsafe = subprocess.run(['bash', '-c', 'set -eo pipefail; ' + command], env=env,
                                    capture_output=True, text=True)
            self.assertEqual(unsafe.returncode, 0, unsafe.stderr)
            self.assertEqual(unsafe.stdout.strip(), 'eth0')
            route_file.unlink()
            route_file.mkdir()
            unreadable = subprocess.run(['bash', '-c', 'set -eo pipefail; ' + command], env=env,
                                        capture_output=True, text=True)
            self.assertNotEqual(unreadable.returncode, 0)

    def test_image_is_a_pinned_release_with_signatures(self):
        image = re.search(r'^    image: \$\{CLAMAV_IMAGE:-([^}]+)\}$', self.clamd, re.M)[1]
        repository, _, tag = image.partition('@')[0].rpartition(':')
        self.assertEqual(repository, 'clamav/clamav')
        # MAJOR.MINOR.PATCH, optionally a platform variant; never latest/stable or _base (no signatures).
        self.assertRegex(tag, r'^\d+\.\d+\.\d+(-debian)?$')
        example = dict(line.split('=', 1) for line in (REPOSITORY / '.env.example').read_text().splitlines()
                       if line and not line.startswith('#'))
        self.assertEqual(example['CLAMAV_IMAGE'], image)
        installer = (REPOSITORY / 'scripts' / 'install.sh').read_text()
        self.assertIn(f"\nclamav_image_default='{image}'\n", installer)


class ClamdConfTests(unittest.TestCase):
    def test_clamd_conf_thorough_limits(self):
        options = clamd_options()
        expected = {
            'TCPSocket': '3310', 'TCPAddr': '0.0.0.0', 'Foreground': 'yes',
            'EnableShutdownCommand': 'no', 'EnableReloadCommand': 'no', 'EnableStatsCommand': 'no',
            'DatabaseDirectory': '/var/lib/clamav', 'SelfCheck': '600', 'ConcurrentDatabaseReload': 'no',
            'TemporaryDirectory': '/tmp', 'LeaveTemporaryFiles': 'no', 'LogTime': 'yes', 'LogClean': 'no',
            'StreamMaxLength': '9M', 'AlertExceedsMax': 'yes',
            'ScanImage': 'yes', 'ScanImageFuzzyHash': 'no', 'AlertBrokenMedia': 'no',
        }
        for key, value in expected.items():
            with self.subTest(key=key):
                self.assertEqual(options.get(key), value)
        # Every limit sits at or above the stream cap, so the stream cap is what a large upload meets.
        stream = size_bytes(options['StreamMaxLength'])
        for key in ('MaxFileSize', 'MaxScanSize'):
            with self.subTest(key=key):
                self.assertGreaterEqual(size_bytes(options[key]), stream)
        # Nothing that needs a writable path or another user: logs go to Docker, no socket file, no pid file.
        for key in ('LogFile', 'LocalSocket', 'PidFile', 'User', 'LogSyslog'):
            with self.subTest(key=key):
                self.assertNotIn(key, options)
        self.assertEqual(sorted(options), sorted(set(expected) | {'MaxFileSize', 'MaxScanSize'}))

    def test_every_option_is_commented(self):
        # Each group of options follows its comment directly (a blank line would detach it).
        previous = ''
        for line in CLAMD_CONF.read_text().splitlines():
            if line and not line.startswith('#'):
                self.assertTrue(previous, f'{line!r} has no comment above its group')
            previous = line


@unittest.skipUnless(compose_available(), 'Docker Compose is not installed')
class ScannerComposeConfigTests(unittest.TestCase):
    def config(self, *files, profiles=('scanner',)):
        arguments = ['docker', 'compose', '--env-file', os.devnull, '-p', 'opsecmkt-scanner-test']
        for name in files:
            arguments += ['-f', name]
        for profile in profiles:
            arguments += ['--profile', profile]
        environment = {key: value for key, value in os.environ.items() if not key.startswith('COMPOSE_')}
        environment['DATABASE_URL'] = 'postgres://unused@db/unused'
        # Compose refuses the monero-wallet profile without its password (A-466).
        environment['MONERO_WALLET_RPC_PASSWORD'] = 'unused'
        result = subprocess.run(arguments + ['config', '--format', 'json'], cwd=REPOSITORY, env=environment,
                                capture_output=True, text=True, check=False)
        self.assertEqual(result.returncode, 0, result.stderr)
        return json.loads(result.stdout)

    def test_scanner_network_members_in_every_mode(self):
        for mode, extra in (('clearnet', ()), ('tor', ()), ('tor', ('compose.external-egress.yaml',))):
            with self.subTest(mode=mode, extra=extra):
                files = ('compose.yaml', f'compose.{mode}.yaml', 'compose.nodes.yaml', 'compose.internal-db.yaml',
                         *extra, 'compose.scanner.yaml')
                config = self.config(*files, profiles=('scanner', 'internal-db', 'bitcoin', 'monero', 'monero-wallet', 'mirror'))
                services = config['services']
                members = sorted(name for name, service in services.items() if 'scanner' in (service.get('networks') or {}))
                self.assertEqual(members, ['app', 'clamd'])
                self.assertEqual(list(services['clamd']['networks']), ['scanner'])
                self.assertIs(config['networks']['scanner'].get('internal'), True)
                self.assertNotIn('clamd', services['app'].get('depends_on', {}))
                for name, service in services.items():
                    self.assertNotIn('clamd', service.get('depends_on', {}), name)
                clamd = services['clamd']
                self.assertNotIn('ports', clamd)
                self.assertEqual([volume['type'] for volume in clamd['volumes']], ['bind'])

    def test_scanner_off_leaves_no_trace(self):
        config = self.config('compose.yaml', 'compose.tor.yaml', 'compose.scanner.yaml', profiles=())
        self.assertNotIn('clamd', config['services'])
        config = self.config('compose.yaml', 'compose.tor.yaml', profiles=('scanner',))
        self.assertNotIn('clamd', config['services'])
        self.assertNotIn('scanner', config.get('networks', {}))


if __name__ == '__main__':
    unittest.main()
