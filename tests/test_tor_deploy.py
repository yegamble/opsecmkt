"""Static checks on the Tor image, its torrc and the onion probe (A-179); no Docker or network needed."""
import os
from pathlib import Path
import re
import stat
import subprocess
import tempfile
import unittest

REPOSITORY = Path(__file__).resolve().parents[1]
TORRC = REPOSITORY / 'deploy' / 'torrc'
DOCKERFILE = REPOSITORY / 'deploy' / 'Dockerfile.tor'
PROBE = REPOSITORY / 'scripts' / 'onion-probe.sh'
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


def torrc_lines():
    """(keyword, value) for each configuration line, in order, without comments."""
    lines = []
    for raw in TORRC.read_text().splitlines():
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

    def test_build_fails_without_the_pow_module(self):
        self.assertRegex(self.text, r"tor --list-modules \| grep -qx 'pow: yes'")

    def test_runs_unprivileged_with_the_repository_torrc(self):
        self.assertIn('COPY torrc /etc/tor/torrc', self.text)
        self.assertRegex(self.text, r'(?m)^USER debian-tor$')
        self.assertIn('ENTRYPOINT ["tor", "-f", "/etc/tor/torrc"]', self.text)
        self.assertIn('chmod 700 /var/lib/tor', self.text)


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
