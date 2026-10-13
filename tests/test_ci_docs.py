"""Static checks on the documentation site job in .github/workflows/ci.yaml (text only, like the other CI tests):
the job builds mkdocs.yml in strict mode from the hash-locked requirements, and the `required` gate waits for it."""

import pathlib
import re
import unittest

REPOSITORY = pathlib.Path(__file__).resolve().parents[1]
WORKFLOW = (REPOSITORY / '.github' / 'workflows' / 'ci.yaml').read_text()
DEPLOY = (REPOSITORY / '.github' / 'workflows' / 'docs-deploy.yaml').read_text()


def job_block(workflow, name):
    match = re.search(r'^  %s:\n(.*?)(?=^  [a-z0-9-]+:\n|\Z)' % re.escape(name), workflow, re.M | re.S)
    assert match, 'no job %s in ci.yaml' % name
    return match.group(1)


class DocsJobTests(unittest.TestCase):
    def test_docs_job_builds_strictly_from_hash_locked_requirements(self):
        block = job_block(WORKFLOW, 'docs')
        self.assertIn('--require-hashes', block)
        self.assertIn('-r requirements-docs.txt', block)
        self.assertIn('mkdocs build', block)
        self.assertRegex(block, r'timeout-minutes: \d+')
        self.assertIn('strict: true', (REPOSITORY / 'mkdocs.yml').read_text())

    def test_requirements_keep_mkdocs_below_2_and_hashed(self):
        lock = (REPOSITORY / 'requirements-docs.txt').read_text()
        self.assertRegex(lock, r'(?m)^mkdocs==1\.')
        for line in lock.splitlines():
            if re.match(r'^[A-Za-z0-9_.-]+==', line):
                self.assertTrue(line.endswith('\\'), 'unhashed requirement: ' + line)

    def test_required_aggregator_waits_for_the_docs_job(self):
        match = re.search(r'^    needs: \[([^\]]+)\]$', job_block(WORKFLOW, 'required'), re.M)
        self.assertTrue(match)
        self.assertIn('docs', [name.strip() for name in match.group(1).split(',')])


    def test_docs_job_runs_the_worker_tests(self):
        self.assertIn('node --test deploy/docs/cloudflare/worker.test.mjs', job_block(WORKFLOW, 'docs'))


class DocsDeployTests(unittest.TestCase):
    def test_deploy_is_off_until_configured_and_pins_its_actions(self):
        self.assertIn("if: vars.CLOUDFLARE_ACCOUNT_ID != ''", DEPLOY)
        self.assertIn('permissions:\n  contents: read', DEPLOY)
        for use in re.findall(r'uses: (\S+)', DEPLOY):
            self.assertRegex(use, r'@[0-9a-f]{40}$', use)

    def test_deploy_builds_strictly_and_installs_wrangler_from_the_lockfile(self):
        self.assertIn('--require-hashes', DEPLOY)
        self.assertIn('mkdocs build', DEPLOY)
        self.assertIn('npm ci --ignore-scripts', DEPLOY)
        self.assertTrue((REPOSITORY / 'deploy' / 'docs' / 'cloudflare' / 'package-lock.json').exists())

    def test_deploy_refuses_a_malformed_onion_address(self):
        self.assertIn("grep -Eqx '[a-z2-7]{56}\\.onion'", DEPLOY)
        self.assertIn('--var "DOCS_ONION:$DOCS_ONION"', DEPLOY)


if __name__ == '__main__':
    unittest.main()
