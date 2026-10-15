"""The CI job that runs the Monero escrow browser journey (A-474).

Static checks on .github/workflows/ci.yaml (text only, so no YAML library is needed on the runner). No CI job ran an
escrow Playwright config, so a wallet-fixture fault that stopped Bitcoin escrow from starting failed every escrow
browser journey on an integration commit without CI noticing. The job must run both npm scripts (which must still map
to the two escrow configs), give each suite its own database, upload what those configs write, and be in `required`.
"""
import json
from pathlib import Path
import re
import unittest

REPOSITORY = Path(__file__).resolve().parents[1]
WORKFLOW = (REPOSITORY / '.github' / 'workflows' / 'ci.yaml').read_text()
PACKAGE = json.loads((REPOSITORY / 'package.json').read_text())
JOB = 'browser-escrow'
SUITES = (
    # npm script, config file, database variable the config reads, database, report and trace directories it writes
    ('test:e2e:escrow-xmr', 'playwright.escrow-xmr.config.ts', 'E2E_ESCROW_XMR_DATABASE_URL', 'e2e_escrow_xmr',
     'playwright-escrow-xmr-report', 'test-results-escrow-xmr'),
)


def job_block(text, name):
    jobs = text.split('\njobs:\n', 1)[1]
    match = re.search(r'^  %s:\n(.*?)(?=^  [a-z][a-z0-9-]*:\n|\Z)' % re.escape(name), jobs, re.M | re.S)
    assert match, 'no job %s in ci.yaml' % name
    return match.group(1)


def code_lines(block):
    lines = [line.strip() for line in block.splitlines() if not line.lstrip().startswith('#')]
    return [re.sub(r'^(- )?run: ', '', line) for line in lines]


class BrowserEscrowJobTest(unittest.TestCase):
    @property
    def block(self):
        return job_block(WORKFLOW, JOB)

    def test_job_exists_and_runs_each_escrow_npm_script(self):
        commands = code_lines(self.block)
        for script, *_ in SUITES:
            self.assertIn('npm run %s' % script, commands, 'the %s job does not run npm run %s' % (JOB, script))

    def test_npm_scripts_still_run_the_escrow_configs(self):
        for script, config, *_ in SUITES:
            self.assertEqual(PACKAGE['scripts'].get(script), 'playwright test --config=%s' % config)
            self.assertTrue((REPOSITORY / config).is_file())

    def test_each_suite_gets_its_own_fresh_database_on_the_service(self):
        self.assertIn('image: postgres:17-alpine', self.block)
        self.assertIn('POSTGRES_CONTAINER: ${{ job.services.postgres.id }}', self.block)
        urls = []
        for _, config, variable, database, *_ in SUITES:
            self.assertIn(variable, (REPOSITORY / config).read_text(), 'the config no longer reads %s' % variable)
            match = re.search(r'^\s+%s: (postgres://\S+)$' % variable, self.block, re.M)
            self.assertTrue(match, 'the job does not set %s' % variable)
            self.assertIn('/%s?sslmode=disable' % database, match.group(1))
            self.assertIn('docker exec "$POSTGRES_CONTAINER" createdb -U test %s' % database, self.block)
            urls.append(match.group(1))
        self.assertEqual(len(set(urls)), len(SUITES))

    def test_each_suite_runs_after_its_database_is_created(self):
        for script, _, _, database, *_ in SUITES:
            self.assertLess(self.block.index('createdb -U test %s' % database), self.block.index('npm run %s' % script))

    def test_job_follows_the_browser_job_conventions(self):
        browser = job_block(WORKFLOW, 'browser')
        self.assertIn('runs-on: ubuntu-24.04', self.block)
        self.assertRegex(self.block, r'(?m)^    timeout-minutes: \d+$')
        for action in ('actions/checkout@', 'actions/setup-go@', 'actions/setup-node@', 'actions/upload-artifact@'):
            used = re.findall(re.escape(action) + r'[0-9a-f]{40} # \S+', self.block)
            self.assertTrue(used, 'the job does not use %s pinned to a commit' % action)
            self.assertTrue(set(used) <= set(re.findall(re.escape(action) + r'\S+ # \S+', browser)),
                            '%s differs from the browser job pin' % action)
        self.assertEqual(self.block.count('persist-credentials: false'), 1)
        self.assertIn('- run: npm ci', self.block)
        # The apt mirror workaround, then Chromium only.
        self.assertIn("sudo sed -i '\\|^http://azure\\.archive\\.ubuntu\\.com/ubuntu/|d' /etc/apt/apt-mirrors.txt", self.block)
        install = [line for line in code_lines(self.block) if 'playwright install' in line]
        self.assertEqual(install, ['npx playwright install --with-deps chromium'])
        self.assertLess(self.block.index('apt-mirrors.txt'), self.block.index('playwright install'))

    def test_failure_artifact_matches_what_the_configs_write(self):
        upload = self.block.split('actions/upload-artifact@', 1)[1]
        self.assertIn('if: always()', self.block.split('actions/upload-artifact@', 1)[0].rsplit('- name:', 1)[1])
        for _, config, _, _, report, results in SUITES:
            text = (REPOSITORY / config).read_text()
            self.assertIn("outputFolder: '%s'" % report, text)
            self.assertIn("outputDir: '%s'" % results, text)
            self.assertRegex(upload, r'(?m)^\s+%s/$' % re.escape(report))
            self.assertRegex(upload, r'(?m)^\s+%s/$' % re.escape(results))
        self.assertIn('name: playwright-escrow-results', upload)
        self.assertIn('retention-days: 14', upload)
        # A different name from the browser job's artifact, which would otherwise collide on upload.
        self.assertNotIn('name: playwright-results\n', upload)

    def test_required_aggregator_waits_for_the_job(self):
        match = re.search(r'^    needs: \[([^\]]+)\]$', job_block(WORKFLOW, 'required'), re.M)
        self.assertTrue(match)
        self.assertIn(JOB, [name.strip() for name in match.group(1).split(',')])


if __name__ == '__main__':
    unittest.main()
