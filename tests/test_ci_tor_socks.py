"""The CI step that parses deploy/torrc-socks with the pinned Tor image (A-442, CHAIR-A442-1), and the Compose job's
nodes_tor matrix entry and its nodes-over-Tor check (A-485 SHOULD-3).

Static checks on .github/workflows/ci.yaml (text only, like tests/test_ci_escrow_regtest.py): the Tor image job runs the
image it builds the way compose.nodes-tor.yaml runs tor-socks (no network, read-only root, no capabilities, no new
privileges, a fresh data volume, deploy/torrc-socks mounted read-only where the overlay's entrypoint reads it) with
--verify-config, and checks the SOCKS-only values tor loaded.
"""
from pathlib import Path
import re
import unittest

REPOSITORY = Path(__file__).resolve().parents[1]
WORKFLOW = (REPOSITORY / '.github' / 'workflows' / 'ci.yaml').read_text()
OVERLAY = (REPOSITORY / 'compose.nodes-tor.yaml').read_text()
TORRC_SOCKS = (REPOSITORY / 'deploy' / 'torrc-socks').read_text()
STEP = "Nodes' Tor client configuration (deploy/torrc-socks)"


def job_block(text, name):
    jobs = text.split('\njobs:\n', 1)[1]
    match = re.search(r'^  %s:\n(.*?)(?=^  [a-z][a-z0-9-]*:\n|\Z)' % re.escape(name), jobs, re.M | re.S)
    assert match, 'no job %s in ci.yaml' % name
    return match.group(1)


def step_block(job, name):
    match = re.search(r'^      - name: %s\n(.*?)(?=^      - |\Z)' % re.escape(name), job, re.M | re.S)
    assert match, 'no step %r in the job' % name
    return match.group(1)


class TorSocksConfigStepTest(unittest.TestCase):
    def setUp(self):
        self.job = job_block(WORKFLOW, 'tor-image')
        self.step = step_block(self.job, STEP)
        self.flat = ' '.join(self.step.split())

    def test_step_runs_the_built_image_as_the_overlay_runs_tor_socks(self):
        self.assertLess(self.job.index('tags: opsecmkt-tor:ci'), self.job.index(STEP), 'the step needs the built image')
        for flag in ('docker run --rm --network none', '--read-only', '--cap-drop ALL',
                     '--security-opt no-new-privileges:true', '-v /var/lib/tor', '--entrypoint tor opsecmkt-tor:ci',
                     '-f /etc/tor/torrc-socks', '"${tor[@]}" --verify-config'):
            self.assertIn(flag, self.flat, flag)
        # The same file at the same path as the overlay mounts it, and the path the overlay's entrypoint reads.
        self.assertIn('- ./deploy/torrc-socks:/etc/tor/torrc-socks:ro\n', OVERLAY)
        self.assertIn('entrypoint: [tor, -f, /etc/tor/torrc-socks]\n', OVERLAY)
        self.assertIn('-v "$PWD/deploy/torrc-socks:/etc/tor/torrc-socks:ro"', self.flat)
        self.assertNotIn('deploy/torrc:', self.flat, "the onion service's torrc is checked by the steps above")

    def test_step_checks_the_loaded_socks_values_against_the_torrc(self):
        socks = re.search(r'^SocksPort (\S+)$', TORRC_SOCKS, re.M).group(1)
        policy = re.findall(r'^SocksPolicy (.+)$', TORRC_SOCKS, re.M)
        self.assertIn('--dump-config short', self.flat)
        for line in [f'SocksPort {socks}', 'ControlPort 0'] + [f'SocksPolicy {value}' for value in policy]:
            self.assertIn(f"'{line}'", self.flat, line)
        self.assertIn("grep -q '^HiddenService'", self.flat)
        # Whole lines (a substring would pass SocksPort 10.199.42.2:90500), a failed --verify-config fails the step, and
        # tor runs as the image's user, as in Compose.
        self.assertIn('grep -qxF "$line" <<< "$config"', self.flat)
        self.assertNotIn('grep -qF ', self.flat)
        self.assertIn('\n          "${tor[@]}" --verify-config\n', self.step)
        self.assertNotIn('|| true', self.step)
        self.assertNotIn('--user', self.step)
        self.assertNotIn(' -u ', self.flat)


class ComposeNodesTorJobTest(unittest.TestCase):
    """The Compose job validates compose.nodes-tor.yaml in every Tor-mode combination (A-485 SHOULD-3)."""

    def setUp(self):
        self.job = job_block(WORKFLOW, 'compose')
        self.step = step_block(self.job, 'Validate all deployment profiles')

    def test_matrix_runs_the_overlay_in_tor_mode_only(self):
        self.assertIn("        nodes_tor: ['off', 'on']\n", self.job)
        exclude = re.search(r'^        exclude:\n((?:          .*\n)*)', self.job, re.M).group(1)
        self.assertEqual(exclude, "          - mode: clearnet\n            mirror: 'on'\n"
                                  "          - mode: clearnet\n            nodes_tor: 'on'\n")
        self.assertIn('NODES_TOR: ${{ matrix.nodes_tor }}', self.step)

    def test_overlay_is_added_after_the_nodes_and_checked_under_the_same_condition(self):
        condition = 'if [ "$NODES_TOR" = on ]; then\n'
        self.assertEqual(self.step.count('"$NODES_TOR"'), 3, 'two conditions and the healthcheck argument')
        self.assertEqual(self.step.count(condition), 2)
        first, second = (match.start() for match in re.finditer(re.escape(condition), self.step))
        self.assertTrue(self.step[first + len(condition):].startswith('            files+=(-f compose.nodes-tor.yaml)\n'))
        self.assertLess(self.step.index('-f compose.nodes.yaml)'), first)
        self.assertTrue(self.step[second + len(condition):].startswith(
            '            docker compose "${files[@]}" "${profiles[@]}" config --format json | python3 -c \''))
        check = self.step[second:self.step.index('\n          fi\n', second)]
        for text in ('bitcoin[-2:] != ["-proxy=tor-socks:9050", "-privatebroadcast"]',
                     'monero[-1:] != ["--proxy=10.199.42.2:9050"]',
                     'if (members != ["bitcoin", "monero", "tor-socks"] or networks["node_egress"].get("internal") is not True',
                     'sorted(socks["networks"]) != ["node_egress", "tor_socks_egress"]',
                     'for name in ("bitcoin", "monero") for network in services[name]["networks"]',
                     'if networks[network].get("internal") is not True)\n          if routes:\n              sys.exit('):
            self.assertIn(text, check, text)


if __name__ == '__main__':
    unittest.main()
