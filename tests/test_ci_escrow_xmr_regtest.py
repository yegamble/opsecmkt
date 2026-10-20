"""The CI job that runs scripts/test-escrow-xmr-regtest.sh against a real monerod fakechain (A-498).

Static checks on .github/workflows/ci.yaml (text only, like tests/test_ci_escrow_regtest.py): the job exists, runs the
script, starts the daemon and the three wallet-rpcs exactly as the script's header does with the images the installer
and compose.nodes.yaml pin, passes the container names and URLs the script requires, keeps the transcript as an
artifact, and is in the `required` aggregator. The script already fails a skipped, failed or missing leg.
"""
from pathlib import Path
import re
import unittest

REPOSITORY = Path(__file__).resolve().parents[1]
WORKFLOW = (REPOSITORY / '.github' / 'workflows' / 'ci.yaml').read_text()
SCRIPT = (REPOSITORY / 'scripts' / 'test-escrow-xmr-regtest.sh').read_text()
INSTALLER = (REPOSITORY / 'scripts' / 'install.sh').read_text()
JOB = 'escrow-xmr-regtest'


def job_block(text, name):
    jobs = text.split('\njobs:\n', 1)[1]
    match = re.search(r'^  %s:\n(.*?)(?=^  [a-z][a-z0-9-]*:\n|\Z)' % re.escape(name), jobs, re.M | re.S)
    assert match, 'no job %s in ci.yaml' % name
    return match.group(1)


class EscrowXMRRegtestJobTest(unittest.TestCase):
    def setUp(self):
        self.block = job_block(WORKFLOW, JOB)

    def test_job_runs_the_script_with_a_postgres_service_and_a_bounded_time(self):
        self.assertIn('bash scripts/test-escrow-xmr-regtest.sh', self.block)
        self.assertIn('image: postgres:17-alpine', self.block)
        self.assertIn('TEST_DATABASE_URL: postgres://test:test-ci-only@127.0.0.1:5432/postgres?sslmode=disable', self.block)
        self.assertRegex(self.block, r'timeout-minutes: (2[0-9])\n')

    def test_images_are_the_ones_the_installer_pins(self):
        daemon = re.search(r"^monero_image_default='([^']+)'", INSTALLER, re.M).group(1)
        wallet = re.search(r"^monero_wallet_image_default='([^']+)'", INSTALLER, re.M).group(1)
        self.assertEqual(daemon, 'ghcr.io/sethforprivacy/simple-monerod:v0.18.5.1')
        self.assertIn('MONERO_IMAGE: ' + daemon + '\n', self.block)
        self.assertIn('MONERO_WALLET_IMAGE: ' + wallet + '\n', self.block)
        self.assertNotIn(':latest', self.block)

    def test_nodes_start_as_the_script_header_documents(self):
        header = SCRIPT.split('set -euo pipefail')[0]
        for flag in ('--regtest --keep-fakechain --offline --fixed-difficulty 1', '--rpc-bind-ip 0.0.0.0 --rpc-bind-port 18081',
                     '--confirm-external-bind --no-igd', '--daemon-address xmr-escrow-monerod:18081', '--trusted-daemon',
                     '--wallet-dir /home/monero/wallet --disable-rpc-login', '--shared-ringdb-dir /home/monero/wallet/.shared-ringdb',
                     '-v "xmr-escrow-wallet-${w%%:*}:/home/monero/wallet"', '--rpc-bind-port 18083', '--allow-mismatched-daemon-version',
                     '-e RPC_EXPOSE_UNAUTHENTICATED=1', 'docker network create xmr-escrow-regtest',
                     '-p 127.0.0.1:28381:18081', 'for w in escrow:28382 funder:28383 restore:28384; do'):
            self.assertIn(flag, header, flag)
            self.assertIn(flag, self.block, flag)

    def test_script_gets_every_variable_it_requires(self):
        for name, value in (
            ('ESCROW_XMR_REGTEST_DAEMON_URL', 'http://127.0.0.1:28381'),
            ('ESCROW_XMR_REGTEST_WALLET_URL', 'http://127.0.0.1:28382'),
            ('ESCROW_XMR_REGTEST_FUNDER_URL', 'http://127.0.0.1:28383'),
            ('ESCROW_XMR_REGTEST_RESTORE_URL', 'http://127.0.0.1:28384'),
            ('ESCROW_XMR_REGTEST_WALLET_CONTAINER', 'xmr-escrow-wallet-escrow'),
            ('ESCROW_XMR_REGTEST_DAEMON_CONTAINER', 'xmr-escrow-monerod'),
        ):
            self.assertIn('%s: %s\n' % (name, value), self.block)
        self.assertIn('ESCROW_XMR_REGTEST_LOG: artifacts/escrow-xmr-regtest/transcript.log', self.block)

    def test_the_script_fails_a_leg_that_is_skipped_or_missing(self):
        # The job relies on this rather than a second grep: every leg must print its own PASS line.
        self.assertIn('--- SKIP: $leg', SCRIPT)
        self.assertIn('NOT RUN: $leg', SCRIPT)

    def test_transcript_is_kept_and_nodes_are_removed_even_when_a_step_fails(self):
        self.assertIn('name: escrow-xmr-regtest-results', self.block)
        self.assertRegex(self.block, r'uses: actions/upload-artifact@[^\n]*\n\s+if: always\(\)\n')
        self.assertRegex(self.block, r'if: always\(\)\n(?:.*\n)*?\s+run: (?:\|\n\s+)?docker rm -f -v ')
        self.assertIn('docker network rm xmr-escrow-regtest', self.block)
        # A-618: the wallet-rpcs keep their wallet directory and ring database on named volumes, removed with the nodes.
        self.assertIn('docker volume rm xmr-escrow-wallet-escrow xmr-escrow-wallet-funder xmr-escrow-wallet-restore', self.block)

    def test_required_aggregator_waits_for_the_job(self):
        match = re.search(r'^    needs: \[([^\]]+)\]$', job_block(WORKFLOW, 'required'), re.M)
        self.assertTrue(match)
        self.assertIn(JOB, [name.strip() for name in match.group(1).split(',')])


if __name__ == '__main__':
    unittest.main()
