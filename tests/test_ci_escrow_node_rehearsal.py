"""The CI job that rehearses the bundled-node Bitcoin escrow set-up from the installer's own printed commands (A-513).

Static checks on .github/workflows/ci.yaml and scripts/test-escrow-node-rehearsal.py (text only, like
tests/test_ci_escrow_xmr_regtest.py): the job runs the script, is bounded, keeps its evidence, is in the `required`
aggregator, and the script runs what the installer prints rather than a copy of it: it parses the installer's output, uses
regtest only, and never logs the set-up script's output (which holds the generated RPC passwords).
"""
from pathlib import Path
import re
import unittest

REPOSITORY = Path(__file__).resolve().parents[1]
WORKFLOW = (REPOSITORY / '.github' / 'workflows' / 'ci.yaml').read_text()
SCRIPT = (REPOSITORY / 'scripts' / 'test-escrow-node-rehearsal.py').read_text()
INSTALLER = (REPOSITORY / 'scripts' / 'install.sh').read_text()
JOB = 'escrow-node-rehearsal'


def job_block(text, name):
    jobs = text.split('\njobs:\n', 1)[1]
    match = re.search(r'^  %s:\n(.*?)(?=^  [a-z][a-z0-9-]*:\n|\Z)' % re.escape(name), jobs, re.M | re.S)
    assert match, 'no job %s in ci.yaml' % name
    return match.group(1)


class EscrowNodeRehearsalJobTest(unittest.TestCase):
    def setUp(self):
        self.block = job_block(WORKFLOW, JOB)

    def test_job_runs_the_script_within_a_bounded_time_and_keeps_its_evidence(self):
        self.assertIn('run: python3 scripts/test-escrow-node-rehearsal.py', self.block)
        self.assertRegex(self.block, r'timeout-minutes: (1[0-9]|20)\n')
        self.assertIn('name: escrow-node-rehearsal-results', self.block)
        self.assertIn('path: artifacts/escrow-node-rehearsal/', self.block)
        self.assertRegex(self.block, r'uses: actions/upload-artifact@[^\n]*\n\s+if: always\(\)\n')
        self.assertNotIn(':latest', self.block)

    def test_required_aggregator_waits_for_the_job(self):
        match = re.search(r'^    needs: \[([^\]]+)\]$', job_block(WORKFLOW, 'required'), re.M)
        self.assertTrue(match)
        self.assertIn(JOB, [name.strip() for name in match.group(1).split(',')])

    def test_script_runs_the_printed_commands_on_regtest_only(self):
        self.assertIn("'--bitcoin-network', 'regtest'", SCRIPT)
        for public in ('signet', 'testnet4'):
            self.assertNotIn("'%s'" % public, SCRIPT)
        # It takes the commands from the installer's output, in the block the installer prints.
        self.assertIn("line.startswith('# 1. ')", SCRIPT)
        self.assertIn("lines[i].startswith('Details: ')", SCRIPT)
        for marker in ("echo '# 1. ", "echo 'Details: docs/operator-guide.md"):
            self.assertIn(marker, INSTALLER)
        # The verify it runs is the printed one (with --via-compose), and its PASS is required.
        self.assertIn("re.search(r'^PASS: '", SCRIPT)
        self.assertIn('--via-compose app', INSTALLER)

    def test_script_logs_no_set_up_output(self):
        # sh() writes output to the evidence log only with show=True; the set-up script's call never passes it.
        self.assertIn('def sh(script, directory, environment, log_path, timeout=600, show=False):', SCRIPT)
        setup_call = re.search(r'sh\(setup\[0\][^\n]*\n', SCRIPT).group(0)
        self.assertNotIn('show=True', setup_call)
        self.assertEqual(len(re.findall(r'show=True', SCRIPT)), 3)  # --verify, signer init (no secrets), install check


if __name__ == '__main__':
    unittest.main()
