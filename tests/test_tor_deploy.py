"""Static checks on the Tor image, its torrc and the onion probe (A-179); no Docker or network needed. The node
overlay's checks (A-442) read `docker compose config` and are skipped without Docker Compose."""
import ipaddress
import json
import os
from pathlib import Path
import re
import shutil
import stat
import subprocess
import tempfile
import unittest

REPOSITORY = Path(__file__).resolve().parents[1]
TORRC = REPOSITORY / 'deploy' / 'torrc'
DOCKERFILE = REPOSITORY / 'deploy' / 'Dockerfile.tor'
PROBE = REPOSITORY / 'scripts' / 'onion-probe.sh'
# A-442: local nodes over Tor, through a SOCKS-only Tor client of their own (compose.nodes-tor.yaml).
TORRC_SOCKS = REPOSITORY / 'deploy' / 'torrc-socks'
BASE_FILES = ('compose.yaml', 'compose.tor.yaml', 'compose.nodes.yaml')
NODES_TOR = 'compose.nodes-tor.yaml'
NODE_SERVICES = ('bitcoin', 'monero')
# Everything deploy/torrc-socks may set (NIT-2: an allowlist, not a denylist).
TORRC_SOCKS_KEYWORDS = {'DataDirectory', 'Log', 'MaxMemInQueues', 'ControlPort', 'SocksPort', 'SocksPolicy'}
# Every key tor-socks may have once Compose has resolved it (`command` resolves to null: the entrypoint runs tor).
TOR_SOCKS_KEYS = {'build', 'cap_drop', 'command', 'cpu_period', 'cpu_quota', 'entrypoint', 'healthcheck', 'logging',
                  'mem_limit', 'networks', 'pids_limit', 'profiles', 'read_only', 'restart', 'security_opt', 'volumes'}
# The two flags compose.nodes-tor.yaml adds to each node command; everything before them is compose.nodes.yaml's.
BITCOIN_FLAGS = ['-proxy=tor-socks:9050', '-privatebroadcast']
MONERO_FLAG = ' --proxy=10.199.42.2:9050'
# Overlays an install can add beside the node overlay (database, scanner, Monero escrow), with the profiles they need.
OTHER_OVERLAYS = (
    (('compose.internal-db.yaml',), ('internal-db',)),
    (('compose.external-egress.yaml',), ()),
    (('compose.internal-db.yaml', 'compose.scanner.yaml'), ('internal-db', 'scanner')),
    (('compose.external-egress.yaml', 'compose.scanner.yaml', 'compose.escrow-xmr.yaml'), ('scanner',)),
)
ONION = 'abcd' * 14 + '.onion'
# The Tor Project's archive signing key, as published on its Debian repository instructions.
TOR_ARCHIVE_KEY = 'A3C4F0F979CAA22CDBA8F512EE8CBC9E886DDD89'

# Per-service options and the values A-179 requires; each must follow the (single) HiddenServiceDir line.
SERVICE_OPTIONS = {
    'HiddenServicePoWDefensesEnabled': '1',
    'HiddenServicePoWQueueRate': '60',
    'HiddenServicePoWQueueBurst': '600',
    'HiddenServiceEnableIntroDoSDefense': '1',
    'HiddenServiceEnableIntroDoSRatePerSec': '25',
    'HiddenServiceEnableIntroDoSBurstPerSec': '200',
    'HiddenServiceMaxStreams': '32',
    'HiddenServiceMaxStreamsCloseCircuit': '1',
}


def torrc_lines(path=TORRC):
    """(keyword, value) for each configuration line, in order, without comments."""
    lines = []
    for raw in path.read_text().splitlines():
        line = raw.split('#', 1)[0].strip()
        if line:
            keyword, _, value = line.partition(' ')
            lines.append((keyword, value.strip()))
    return lines


def version_tuple(text):
    return tuple(int(part) for part in re.findall(r'\d+', text)[:4])


class TorrcTests(unittest.TestCase):
    def test_one_onion_service_with_every_defence_after_its_directory(self):
        lines = torrc_lines()
        keywords = [keyword for keyword, _ in lines]
        self.assertEqual(keywords.count('HiddenServiceDir'), 1, 'tor and tor-mirror each run exactly one service')
        start = keywords.index('HiddenServiceDir')
        service = dict(lines[start + 1:])
        for keyword, value in SERVICE_OPTIONS.items():
            self.assertIn(keyword, keywords, f'{keyword} missing from deploy/torrc')
            self.assertGreater(keywords.index(keyword), start, f'{keyword} must follow HiddenServiceDir')
            self.assertEqual(keywords.count(keyword), 1, f'{keyword} set more than once')
            self.assertEqual(service[keyword], value, keyword)
        self.assertEqual(service['HiddenServicePort'], '80 app:8080')

    def test_global_options_precede_the_service(self):
        lines = torrc_lines()
        keywords = [keyword for keyword, _ in lines]
        start = keywords.index('HiddenServiceDir')
        options = dict(lines)
        for keyword, value in {'HeartbeatPeriod': '30 minutes', 'SocksPort': '0', 'MaxMemInQueues': '128 MB',
                               'DataDirectory': '/var/lib/tor', 'Log': 'notice stdout'}.items():
            self.assertLess(keywords.index(keyword), start, f'{keyword} is global and belongs before HiddenServiceDir')
            self.assertEqual(options[keyword], value, keyword)

    def test_queue_memory_stays_inside_the_container_limit(self):
        # compose.tor.yaml gives each Tor container 256m; Tor's queues must give up circuits before that.
        options = dict(torrc_lines())
        queues = int(options['MaxMemInQueues'].split()[0])
        compose = (REPOSITORY / 'compose.tor.yaml').read_text()
        self.assertEqual(compose.count('    mem_limit: 256m\n'), 2)
        self.assertLessEqual(queues, 256 // 2)
        self.assertEqual(compose.count('      args: [TOR_VERSION]\n'), 2, 'TOR_VERSION in .env overrides the pin')

    def test_intro_point_limit_is_looser_than_the_pow_queue(self):
        # Introduction points do not check PoW, so a tighter intro limit would drop clients who solved a puzzle.
        # Tor rejects a burst below its rate (hs_config.c config_validate_service).
        options = dict(torrc_lines())
        rate, burst = int(options['HiddenServicePoWQueueRate']), int(options['HiddenServicePoWQueueBurst'])
        self.assertGreaterEqual(burst, rate)
        intro_points = int(options.get('HiddenServiceNumIntroductionPoints', 3))
        self.assertGreater(intro_points * int(options['HiddenServiceEnableIntroDoSRatePerSec']), rate)
        self.assertGreaterEqual(intro_points * int(options['HiddenServiceEnableIntroDoSBurstPerSec']), burst)


class DockerfileTests(unittest.TestCase):
    def setUp(self):
        self.text = DOCKERFILE.read_text()

    def test_base_image_is_pinned_by_digest(self):
        match = re.search(r'^FROM\s+(\S+)', self.text, re.M)
        self.assertIsNotNone(match)
        self.assertRegex(match.group(1), r'^debian:[a-z0-9.-]+@sha256:[0-9a-f]{64}$')

    def test_tor_is_pinned_from_the_tor_project_repository(self):
        match = re.search(r'^ARG TOR_VERSION=(\S+)$', self.text, re.M)
        self.assertIsNotNone(match, 'Dockerfile.tor must pin TOR_VERSION')
        version = match.group(1)
        self.assertGreaterEqual(version_tuple(version), (0, 4, 8), 'PoW needs tor 0.4.8 or later')
        self.assertRegex(version, r'~d\d+\.[a-z]+\+\d+$', 'a deb.torproject.org build, not the Debian archive one')
        self.assertIn('tor="${TOR_VERSION}"', self.text)
        self.assertIn('https://deb.torproject.org/torproject.org', self.text)
        self.assertIn('Signed-By: /usr/share/keyrings/deb.torproject.org-keyring.gpg', self.text)
        self.assertIn(TOR_ARCHIVE_KEY, self.text)
        # The downloaded key is refused unless its fingerprint is the pinned one.
        self.assertRegex(self.text, r'gpg --show-keys --with-colons')

    def test_withdrawn_pin_falls_back_only_to_a_newer_build(self):
        # deb.torproject.org drops old builds, so a fresh install would otherwise fail to build Tor. The fallback
        # installs the repository's candidate only when dpkg says it is the same or newer than the pin.
        self.assertIn('if ! apt-get install -y --no-install-recommends tor="${TOR_VERSION}"; then', self.text)
        self.assertIn('dpkg --compare-versions "$candidate" ge "$TOR_VERSION"', self.text)
        self.assertIn('tor="${candidate}"', self.text)
        self.assertIn('no longer published and the repository offers no newer build', self.text)

    def test_only_the_tor_project_build_is_installed(self):
        # SRE-179-2: Debian's archive also publishes tor (an older build). An apt preference keeps tor's candidate, and
        # so the withdrawn-pin fallback, on deb.torproject.org (the first matching stanza wins). apt installs an
        # explicitly requested version whatever its priority, so the build also refuses an installed tor that
        # deb.torproject.org does not publish, e.g. TOR_VERSION=0.4.9.11-0+deb12u1 from the Debian archive.
        preference = ("printf '%s\\n' 'Package: tor tor-geoipdb' 'Pin: origin deb.torproject.org' 'Pin-Priority: 500' '' \\\n"
                      "      'Package: tor tor-geoipdb' 'Pin: version *' 'Pin-Priority: -1' > /etc/apt/preferences.d/tor;")
        self.assertIn(preference, self.text)
        self.assertLess(self.text.index(preference), self.text.index('tor="${TOR_VERSION}"'))
        check = ("apt-cache madison tor | awk -F '|' -v version=\"$installed\" \\\n"
                 "      '{ gsub(/ /, \"\", $2) } $2 == version && $3 ~ /^ https:\\/\\/deb\\.torproject\\.org\\/torproject\\.org /"
                 " { found = 1 } END { exit !found }' \\\n"
                 "      || { echo \"tor ${installed} is not a build deb.torproject.org publishes")
        self.assertIn("installed=$(dpkg-query --show --showformat '${Version}' tor);", self.text)
        self.assertIn(check, self.text)
        # After either install path, and before the package lists it reads are deleted.
        self.assertGreater(self.text.index(check), self.text.index('tor="${candidate}"'))
        self.assertLess(self.text.index(check), self.text.index('rm -rf /var/lib/apt/lists/*'))

    def test_build_fails_without_the_pow_module(self):
        self.assertRegex(self.text, r"tor --list-modules \| grep -qx 'pow: yes'")

    def test_runs_unprivileged_with_the_repository_torrc(self):
        self.assertIn('COPY torrc /etc/tor/torrc', self.text)
        self.assertRegex(self.text, r'(?m)^USER debian-tor$')
        self.assertIn('ENTRYPOINT ["tor", "-f", "/etc/tor/torrc"]', self.text)
        self.assertIn('chmod 700 /var/lib/tor', self.text)


class TorSocksTorrcTests(unittest.TestCase):
    """deploy/torrc-socks, the nodes' own Tor client (A-442): SOCKS only, no onion service, no control port."""

    def test_socks_only(self):
        lines = torrc_lines(TORRC_SOCKS)
        keywords = [keyword for keyword, _ in lines]
        self.assertEqual([k for k in keywords if k.startswith('HiddenService')], [], "the nodes' Tor runs no onion service")
        self.assertEqual([value for keyword, value in lines if keyword == 'ControlPort'], ['0'])
        self.assertEqual(keywords.count('SocksPort'), 1)
        policy = [value for keyword, value in lines if keyword == 'SocksPolicy']
        self.assertEqual(policy[-1:], ['reject *'], 'whatever the policy does not accept is refused')
        self.assertEqual(sum(value.startswith('accept') for value in policy), 1, "one accepted network: the nodes'")
        self.assertEqual(dict(lines)['DataDirectory'], '/var/lib/tor')
        # An allowlist: any other option (a relay or metrics port, a control socket, an upstream proxy) is a review.
        self.assertEqual(set(keywords), TORRC_SOCKS_KEYWORDS, 'a client with one SOCKS port and nothing else')

    def test_onion_container_keeps_its_own_torrc_without_socks(self):
        self.assertNotEqual(TORRC_SOCKS.read_text(), TORRC.read_text())
        self.assertEqual([value for keyword, value in torrc_lines() if keyword == 'SocksPort'], ['0'],
                         'the onion container keeps SocksPort 0')


def compose_available():
    if not shutil.which('docker'):
        return False
    return subprocess.run(['docker', 'compose', 'version'], capture_output=True).returncode == 0


def compose_config(*files, profiles=('bitcoin', 'monero'), interpolate=True, bitcoin_rpc_auth=True):
    """`docker compose config` as parsed JSON, reading no .env and no COMPOSE_*, BITCOIN_* or MONERO_* variable;
    interpolate=False keeps every ${...} as written (--no-interpolate)."""
    arguments = ['docker', 'compose', '--env-file', os.devnull, '-p', 'opsecmkt-nodes-tor-test']
    for name in files:
        arguments += ['-f', name]
    for profile in profiles:
        arguments += ['--profile', profile]
    environment = {key: value for key, value in os.environ.items()
                   if not key.startswith(('COMPOSE_', 'BITCOIN_', 'MONERO_'))}
    environment['DATABASE_URL'] = 'postgres://unused@db/unused'
    if bitcoin_rpc_auth:
        environment['BITCOIN_RPC_AUTH'] = ('marketplace:' + '01' * 16 + '$' + 'ab' * 32)
    command = ['config', '--format', 'json'] + ([] if interpolate else ['--no-interpolate'])
    result = subprocess.run(arguments + command, cwd=REPOSITORY, env=environment,
                            capture_output=True, text=True, check=False, timeout=60)
    if result.returncode != 0:
        raise AssertionError(result.stderr)
    return json.loads(result.stdout)


@unittest.skipUnless(compose_available(), 'Docker Compose is not installed')
class NodesTorComposeTests(unittest.TestCase):
    """compose.nodes-tor.yaml on compose.yaml, compose.tor.yaml and compose.nodes.yaml, as Compose resolves it (A-442)."""

    @classmethod
    def setUpClass(cls):
        cls.base = compose_config(*BASE_FILES)
        cls.config = compose_config(*BASE_FILES, NODES_TOR)
        cls.services = cls.config['services']
        cls.socks = cls.services['tor-socks']
        cls.node_network = 'node_egress'

    def members(self, network):
        return sorted(name for name, service in self.services.items() if network in (service.get('networks') or {}))

    def socks_address(self):
        (ipam,) = self.config['networks'][self.node_network]['ipam']['config']
        address = ipaddress.ip_address(self.socks['networks'][self.node_network]['ipv4_address'])
        return address, ipaddress.ip_network(ipam['subnet']), ipaddress.ip_network(ipam['ip_range'])

    def gateway(self):
        # Explicit: with an ip_range and no gateway, Docker 29 takes the range's first address as the gateway (A-442 run).
        (ipam,) = self.config['networks'][self.node_network]['ipam']['config']
        return ipaddress.ip_address(ipam['gateway'])

    def test_tor_socks_is_hardened_and_publishes_nothing(self):
        socks = self.socks
        # An allowlist (NIT-2): pid, dns, extra_hosts, ports, user, environment or anything new is a review.
        self.assertEqual(set(socks), TOR_SOCKS_KEYS)
        self.assertIsNone(socks['command'])
        for name, service in self.services.items():
            self.assertNotIn('ports', service, f'{name} publishes a port')
        want = {'read_only': True, 'cap_drop': ['ALL'], 'security_opt': ['no-new-privileges:true'],
                'restart': 'unless-stopped', 'entrypoint': ['tor', '-f', '/etc/tor/torrc-socks']}
        for key, value in want.items():
            self.assertEqual(socks.get(key), value, key)
        self.assertEqual(sorted(socks['profiles']), ['bitcoin', 'monero'])
        self.assertTrue(socks['healthcheck']['test'])
        self.assertFalse(socks['healthcheck'].get('disable'))
        # The onion container's pinned build, with its own torrc mounted read-only; the onion's torrc is not touched.
        self.assertEqual(socks['build'], self.services['tor']['build'])
        binds = [(Path(v['source']).relative_to(REPOSITORY).as_posix(), v['target'], v.get('read_only'))
                 for v in socks['volumes'] if v['type'] == 'bind']
        self.assertEqual(binds, [('deploy/torrc-socks', '/etc/tor/torrc-socks', True)])
        named = [(v['source'], v['target']) for v in socks['volumes'] if v['type'] == 'volume']
        self.assertEqual(named, [('tor_socks_data', '/var/lib/tor')])
        for name, service in self.services.items():
            if name != 'tor-socks':
                self.assertNotIn('tor_socks_data', [v.get('source') for v in service.get('volumes', [])], name)

    def test_only_the_nodes_reach_tor_socks_and_only_tor_socks_reaches_out(self):
        self.assertEqual(sorted(self.socks['networks']), [self.node_network, 'tor_socks_egress'])
        self.assertEqual(self.members(self.node_network), ['bitcoin', 'monero', 'tor-socks'])
        self.assertEqual(self.members('tor_socks_egress'), ['tor-socks'])
        networks = self.config['networks']
        self.assertIsNot(networks['tor_socks_egress'].get('internal'), True)
        for name in ('app', 'tor', 'db', 'escrow-signer', 'monero-wallet'):
            if name in self.services:
                self.assertFalse(set(self.services[name].get('networks') or {}) & set(self.socks['networks']), name)
        for name in NODE_SERVICES:
            self.assertEqual(self.services[name]['depends_on']['tor-socks']['condition'], 'service_started', name)
            # Every network a node joins is internal, so nothing but tor-socks gives it a route out.
            for network in self.services[name]['networks']:
                self.assertIs(networks[network].get('internal'), True, f'{name} on {network}')

    def test_socks_port_and_policy_admit_only_the_node_network(self):
        address, subnet, dynamic = self.socks_address()
        gateway = self.gateway()  # the bridge's address: the host
        self.assertIn(address, subnet)
        self.assertIn(gateway, subnet)
        self.assertNotIn(address, dynamic, 'Docker could hand the address to a node first')
        self.assertNotIn(gateway, dynamic, 'the gateway must not be an address a node could have')
        self.assertNotEqual(address, gateway)
        lines = torrc_lines(TORRC_SOCKS)
        self.assertEqual([value for keyword, value in lines if keyword == 'SocksPort'], [f'{address}:9050'])
        self.assertEqual([value for keyword, value in lines if keyword == 'SocksPolicy'],
                         [f'reject {gateway}', f'accept {subnet}', 'reject *'])
        self.assertIn(f'/dev/tcp/{address}/9050', ' '.join(self.socks['healthcheck']['test']))

    def test_bitcoin_verifier_gate_is_profile_scoped(self):
        # Compose must still render external/disabled-node installs with no local verifier. A bitcoin-profile
        # service with no verifier fails before it could start with cookie-only RPC.
        for files in (('compose.yaml', 'compose.clearnet.yaml', 'compose.nodes.yaml'),
                      (*BASE_FILES, NODES_TOR)):
            with self.subTest(files=files):
                compose_config(*files, profiles=(), bitcoin_rpc_auth=False)
                with self.assertRaisesRegex(AssertionError, 'BITCOIN_RPC_AUTH-is-blank'):
                    compose_config(*files, profiles=('bitcoin',), bitcoin_rpc_auth=False)

    def test_private_dotenv_verifier_config_escapes_dollar_without_password(self):
        verifier = 'marketplace:' + '01' * 16 + '$' + 'ab' * 32
        password = 'disposable-test-password'
        with tempfile.TemporaryDirectory(prefix='opsecmkt-rpcauth-compose-') as directory:
            dotenv = Path(directory) / '.env'
            dotenv.write_text(f"DATABASE_URL='postgres://unused@db/unused'\n"
                              f"BITCOIN_RPC_PASSWORD='{password}'\n"
                              f"BITCOIN_RPC_AUTH='{verifier}'\n"
                              f"BITCOIN_RPC_URL='http://marketplace:{password}@bitcoin:8332'\n")
            for files in (('compose.yaml', 'compose.clearnet.yaml', 'compose.nodes.yaml'),
                          (*BASE_FILES, NODES_TOR)):
                with self.subTest(files=files):
                    arguments = ['docker', 'compose', '--env-file', str(dotenv), '-p', 'opsecmkt-rpcauth-test']
                    for name in files:
                        arguments += ['-f', name]
                    arguments += ['--profile', 'bitcoin', 'config', '--format', 'json']
                    environment = {key: value for key, value in os.environ.items()
                                   if not key.startswith(('COMPOSE_', 'BITCOIN_', 'MONERO_', 'DATABASE_URL'))}
                    result = subprocess.run(arguments, cwd=REPOSITORY, env=environment,
                                            capture_output=True, text=True, check=True)
                    command = json.loads(result.stdout)['services']['bitcoin']['command']
                    # Compose config re-escapes a literal dollar as $$ in its reusable rendered model.
                    # The input .env still has one $, and the node process must be checked on a real launch.
                    self.assertIn('-rpcauth=' + verifier.replace('$', '$$'), command)
                    self.assertFalse(any(password in argument for argument in command))
                    self.assertFalse(any(argument.startswith(('-rpcuser=', '-rpcpassword=')) for argument in command))

    def test_bitcoind_sends_through_tor_socks(self):
        base, nodes = self.base['services']['bitcoin']['command'], self.services['bitcoin']['command']
        self.assertEqual(nodes, base + BITCOIN_FLAGS,
                         "the overlay repeats compose.nodes.yaml's command and adds exactly the two flags")
        self.assertEqual(len([arg for arg in nodes if arg.startswith('-rpcauth=marketplace:')]), 1)
        self.assertFalse(any(arg.startswith(('-rpcuser=', '-rpcpassword=')) for arg in nodes))
        for argument in nodes:
            self.assertIsNone(re.match(r'-(no)?(connect|onlynet|onion|listenonion|seednode|addnode)\b', argument),
                              f'{argument}: never -connect or -onlynet; -proxy alone covers every network')

    def test_monerod_sends_through_tor_socks(self):
        address, _, _ = self.socks_address()
        base, nodes = self.base['services']['monero']['command'], self.services['monero']['command']
        # monerod's --proxy takes an IP address, not a host name (monerod --help: <socks-ip:port>).
        self.assertEqual(nodes, base + [f'--proxy={address}:9050'])
        self.assertEqual(MONERO_FLAG, f' --proxy={address}:9050')

    def test_node_commands_match_compose_nodes_yaml_as_written(self):
        # SHOULD-1: compared after interpolation only, a variable renamed in compose.nodes.yaml (and not here) passes
        # while it resolves to the same default. Uninterpolated, each command must be compose.nodes.yaml's plus the flags.
        base = compose_config(*BASE_FILES, interpolate=False)['services']
        nodes = compose_config(*BASE_FILES, NODES_TOR, interpolate=False)['services']
        self.assertEqual(nodes['bitcoin']['command'], base['bitcoin']['command'] + BITCOIN_FLAGS)
        self.assertIsInstance(base['monero']['command'], str)
        self.assertEqual(nodes['monero']['command'], base['monero']['command'] + MONERO_FLAG)

    def test_nodes_have_no_route_out_with_any_other_overlay(self):
        # SHOULD-2: another overlay must not give a node a network with a route out, before or after this one.
        for extra, profiles in OTHER_OVERLAYS:
            for files in ((*BASE_FILES, *extra, NODES_TOR), (*BASE_FILES, NODES_TOR, *extra)):
                with self.subTest(files=files):
                    config = compose_config(*files, profiles=('bitcoin', 'monero', *profiles))
                    for name in NODE_SERVICES:
                        for network in config['services'][name]['networks']:
                            self.assertIs(config['networks'][network].get('internal'), True, f'{name} on {network}')
                    members = sorted(n for n, s in config['services'].items() if 'node_egress' in (s.get('networks') or {}))
                    self.assertEqual(members, ['bitcoin', 'monero', 'tor-socks'])

    def test_overlay_changes_nothing_else(self):
        self.assertEqual(set(self.services), set(self.base['services']) | {'tor-socks'})
        for name, service in self.base['services'].items():
            if name not in NODE_SERVICES:
                self.assertEqual(self.services[name], service, name)
                continue
            mine = {k: v for k, v in self.services[name].items() if k not in ('command', 'networks', 'depends_on')}
            theirs = {k: v for k, v in service.items() if k not in ('command', 'networks', 'depends_on')}
            self.assertEqual(mine, theirs, name)
            self.assertEqual(set(self.services[name]['networks']), set(service['networks']), name)

    def test_no_tor_client_without_a_local_node(self):
        self.assertNotIn('tor-socks', compose_config(*BASE_FILES, NODES_TOR, profiles=())['services'])
        self.assertIn('tor-socks', compose_config(*BASE_FILES, NODES_TOR, profiles=('monero',))['services'])


FAKE_CURL = r'''#!/usr/bin/env bash
# Records its arguments; answers with $FAKE_CURL_RESULTS (one "code seconds exit" per call, in order).
printf '%s\n' "$*" >> "$FAKE_CURL_LOG"
call=$(wc -l < "$FAKE_CURL_LOG")
read -r code seconds status <<< "$(sed -n "${call}p" <<< "$FAKE_CURL_RESULTS")"
printf '%s %s' "$code" "$seconds"
exit "${status:-0}"
'''


class OnionProbeTests(unittest.TestCase):
    def run_probe(self, *arguments, results='200 1.5 0'):
        with tempfile.TemporaryDirectory() as directory:
            curl = Path(directory) / 'curl'
            curl.write_text(FAKE_CURL)
            curl.chmod(curl.stat().st_mode | stat.S_IXUSR)
            log = Path(directory) / 'calls'
            log.write_text('')
            env = {**os.environ, 'PATH': f'{directory}:{os.environ["PATH"]}', 'FAKE_CURL_LOG': str(log),
                   'FAKE_CURL_RESULTS': results}
            done = subprocess.run(['bash', str(PROBE), *arguments], env=env, capture_output=True, text=True,
                                  timeout=30)
            return done, log.read_text().splitlines()

    def test_syntax(self):
        subprocess.run(['bash', '-n', str(PROBE)], check=True)

    def test_probes_canary_through_socks_with_a_fresh_circuit_each_time(self):
        done, calls = self.run_probe('--count', '2', '--interval', '0', f'http://{ONION}/',
                                     results='200 2.5 0\n200 3.5 0')
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertEqual(len(calls), 2)
        users = set()
        for call in calls:
            self.assertIn(f'http://{ONION}/canary', call)
            match = re.search(r'--proxy socks5h://([^:@ ]+):[^@ ]*@127\.0\.0\.1:9050\b', call)
            self.assertIsNotNone(match, call)
            users.add(match.group(1))
            self.assertIn('--max-time 120', call)
        self.assertEqual(len(users), 2, 'each probe must use its own SOCKS credentials, so Tor builds a new circuit')
        self.assertIn('2/2 ok', done.stdout)

    def test_failure_exits_one(self):
        done, _ = self.run_probe('--count', '2', '--interval', '0', '--socks', '127.0.0.1:9150', ONION,
                                 results='200 2.0 0\n000 120.0 28')
        self.assertEqual(done.returncode, 1)
        self.assertIn('1/2 ok', done.stdout)
        self.assertIn('FAIL', done.stdout)

    def test_a_non_200_answer_is_a_failure(self):
        done, _ = self.run_probe('--count', '1', ONION, results='503 0.8 0')
        self.assertEqual(done.returncode, 1)

    def test_refuses_anything_but_a_v3_onion(self):
        for address in ('example.com', 'abcd.onion', f'https://{ONION}', f'{ONION}:8080'):
            done, calls = self.run_probe(address)
            self.assertEqual(done.returncode, 2, address)
            self.assertEqual(calls, [], address)

    def test_refuses_bad_numbers(self):
        for arguments in (('--count', '0'), ('--count', 'x'), ('--timeout', '-1'), ('--interval', '1.5'),
                          ('--socks', 'localhost')):
            done, calls = self.run_probe(*arguments, ONION)
            self.assertEqual(done.returncode, 2, arguments)
            self.assertEqual(calls, [], arguments)


if __name__ == '__main__':
    unittest.main()
