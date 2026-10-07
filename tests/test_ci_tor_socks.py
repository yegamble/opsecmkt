"""The CI step that parses deploy/torrc-socks with the pinned Tor image (A-442, CHAIR-A442-1).

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


if __name__ == '__main__':
    unittest.main()
