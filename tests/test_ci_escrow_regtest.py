"""The CI job that runs scripts/test-escrow-regtest.sh against a real Bitcoin Core regtest node (A-438, P4-R11).

Static checks on .github/workflows/ci.yaml (text only, so no YAML library is needed on the runner): the job exists,
runs the script, uses the node image the installer pins, passes the app user and the pooled user (A-440) the whitelists
the script documents, generates its RPC passwords per run and masks them, and is in the `required` aggregator, which must list every job
(a job missing from it could fail without blocking the merge).
"""
from pathlib import Path
import re
import unittest

REPOSITORY = Path(__file__).resolve().parents[1]
WORKFLOW = (REPOSITORY / '.github' / 'workflows' / 'ci.yaml').read_text()
SCRIPT = (REPOSITORY / 'scripts' / 'test-escrow-regtest.sh').read_text()
JOB = 'escrow-regtest'


def job_ids(text):
    return re.findall(r'^  ([a-z][a-z0-9-]*):\n', text.split('\njobs:\n', 1)[1], re.M)


def job_block(text, name):
    jobs = text.split('\njobs:\n', 1)[1]
    match = re.search(r'^  %s:\n(.*?)(?=^  [a-z][a-z0-9-]*:\n|\Z)' % re.escape(name), jobs, re.M | re.S)
    assert match, 'no job %s in ci.yaml' % name
    return match.group(1)


class EscrowRegtestJobTest(unittest.TestCase):
    def test_job_runs_the_script_against_the_pinned_image_and_a_postgres_service(self):
        block = job_block(WORKFLOW, JOB)
        self.assertIn('bash scripts/test-escrow-regtest.sh', block)
        pinned = re.search(r"^bitcoin_image_default='([^']+)'", (REPOSITORY / 'scripts' / 'install.sh').read_text(), re.M)
        self.assertTrue(pinned)
        self.assertIn(pinned.group(1), block)
        self.assertNotIn(':latest', block)
        self.assertIn('image: postgres:17-alpine', block)
        self.assertRegex(block, r'(?m)^    timeout-minutes: \d+$')
        # The script refuses to start without each of these; the job must hand it each.
        for name in ('ESCROW_REGTEST_OPERATOR_RPC_URL', 'ESCROW_REGTEST_APP_RPC_URL', 'ESCROW_REGTEST_POOLED_RPC_URL',
                     'ESCROW_REGTEST_SIGNER_RPC_URL', 'TEST_DATABASE_URL'):
            self.assertIn(name, block)

    def test_job_gives_the_app_user_the_documented_whitelist_and_nothing_else(self):
        block = job_block(WORKFLOW, JOB)
        documented = re.search(r'-rpcwhitelist=opsecmkt-escrow:(\S+)', SCRIPT.split('set -euo pipefail')[0])
        self.assertTrue(documented)
        self.assertIn('whitelist=%s\n' % documented.group(1), block)
        self.assertIn('"-rpcwhitelist=opsecmkt-escrow:$whitelist"', block)
        self.assertIn('-rpcwhitelistdefault=0', block)
        self.assertIn('-regtest', block)
        self.assertIn('-fallbackfee=0.0002', block)
        # The escrow signer's user (A-454): the script's documented whitelist, exactly.
        signer = re.search(r'-rpcwhitelist=opsecmkt-signer:(\S+)', SCRIPT.split('set -euo pipefail')[0])
        self.assertTrue(signer)
        self.assertEqual(signer.group(1), 'decodepsbt,descriptorprocesspsbt')
        self.assertIn('-rpcwhitelist=opsecmkt-signer:%s\n' % signer.group(1), block)
        # The pooled user (A-440): the script's documented whitelist, exactly, and never a key-export method.
        pooled = re.search(r'-rpcwhitelist=opsecmkt-pooled:(\S+)', SCRIPT.split('set -euo pipefail')[0])
        self.assertTrue(pooled)
        self.assertIn('pooled_whitelist=%s\n' % pooled.group(1), block)
        self.assertIn('"-rpcwhitelist=opsecmkt-pooled:$pooled_whitelist"', block)
        for method in ('listdescriptors', 'dumpwallet', 'backupwallet', 'dumpprivkey'):
            self.assertNotIn(method, pooled.group(1).split(','))

    def test_rpc_passwords_are_generated_masked_and_never_committed_or_on_a_command_line(self):
        block = job_block(WORKFLOW, JOB)
        self.assertIn('openssl rand', block)
        for name in ('operator_pass', 'app_pass', 'pooled_pass', 'signer_pass'):
            self.assertIn('echo "::add-mask::$%s"' % name, block)
        self.assertNotIn('-rpcpassword', block)
        self.assertNotRegex(block, r'http://\w+:[^$@%\s]+@')  # an RPC URL with a literal password
        self.assertNotIn('set -x', block)

    def test_node_is_published_on_loopback_only(self):
        block = job_block(WORKFLOW, JOB)
        self.assertRegex(block, r'-p "127\.0\.0\.1:\$BITCOIN_RPC_PORT:18443"')

    def test_node_is_removed_with_its_volume_even_when_a_step_fails(self):
        block = job_block(WORKFLOW, JOB)
        self.assertRegex(block, r'if: always\(\)\n(?:.*\n)*?\s+run: docker rm -f -v ')

    def test_required_aggregator_lists_every_job(self):
        ids = job_ids(WORKFLOW)
        self.assertIn(JOB, ids)
        match = re.search(r'^    needs: \[([^\]]+)\]$', job_block(WORKFLOW, 'required'), re.M)
        self.assertTrue(match)
        needed = [name.strip() for name in match.group(1).split(',')]
        self.assertEqual(sorted(needed), sorted(i for i in ids if i != 'required'))


if __name__ == '__main__':
    unittest.main()
