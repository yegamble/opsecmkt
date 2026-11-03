"""The CI upgrade job rehearses an upgrade from every release that has a leg (A-800).

Static checks on .github/workflows/ci.yaml (text only, like the other tests/test_ci_*.py): the `upgrade` job runs
scripts/test-upgrade.sh once from v0.1.0-alpha.1 and once from v0.1.0-alpha.2, each step naming its starting tag
itself, so bumping the script's default (docs/ci-cd.md, tag-time step 8) can neither drop the alpha.1 leg nor run one
tag twice. The alpha.1 rehearsal stays until a decision to drop it is recorded. The job keeps the full history and tags
it builds the old servers from, and docs/operations-tests.md documents both legs.
"""
from pathlib import Path
import re
import unittest

REPOSITORY = Path(__file__).resolve().parents[1]
WORKFLOW = (REPOSITORY / '.github' / 'workflows' / 'ci.yaml').read_text()
SCRIPT = (REPOSITORY / 'scripts' / 'test-upgrade.sh').read_text()
OPERATIONS = (REPOSITORY / 'docs' / 'operations-tests.md').read_text()
REFS = ('v0.1.0-alpha.1', 'v0.1.0-alpha.2')


def job_block(text, name):
    jobs = text.split('\njobs:\n', 1)[1]
    match = re.search(r'^  %s:\n(.*?)(?=^  [a-z][a-z0-9-]*:\n|\Z)' % re.escape(name), jobs, re.M | re.S)
    assert match, 'no job %s in ci.yaml' % name
    return match.group(1)


def upgrade_runs(job):
    """The starting ref of every step that runs scripts/test-upgrade.sh, or None for a step that relies on the default."""
    runs = []
    for step in re.split(r'^      - ', job, flags=re.M)[1:]:
        run = re.search(r'^        run: (.*)$', step, re.M)
        if not run or 'scripts/test-upgrade.sh' not in run.group(1):
            continue
        ref = re.fullmatch(r'UPGRADE_FROM_REF=(\S+) bash scripts/test-upgrade\.sh', run.group(1).strip())
        runs.append(ref.group(1) if ref else None)
    return runs


class UpgradeJobTest(unittest.TestCase):
    def setUp(self):
        self.job = job_block(WORKFLOW, 'upgrade')

    def test_job_runs_each_release_leg_once_with_an_explicit_ref(self):
        runs = upgrade_runs(self.job)
        self.assertNotIn(None, runs, 'each upgrade step names its UPGRADE_FROM_REF; the script default moves at each tag')
        self.assertEqual(sorted(runs), sorted(REFS), 'the upgrade job must run the alpha.1 and the alpha.2 legs, once each')

    def test_job_name_and_history(self):
        name = re.search(r'^    name: (.*)$', self.job, re.M).group(1)
        for release in ('alpha.1', 'alpha.2'):
            self.assertIn(release, name)
        self.assertIn('fetch-depth: 0', self.job, 'the legs build the old servers from their tags')
        self.assertLessEqual(int(re.search(r'^    timeout-minutes: (\d+)$', self.job, re.M).group(1)), 45)

    def test_alpha1_leg_keeps_its_intermediate_and_tor_rehearsals(self):
        # The legs differ only in how the old release is populated and checked; alpha.1's extra rehearsals stay on it.
        self.assertIn("if [[ $from_layout == alpha1 ]]; then\n# A-41:", SCRIPT)
        self.assertIn('scripts/test-intermediate-upgrade.sh --disposable-cluster', SCRIPT)
        self.assertIn('bash scripts/test-tor-upgrade-rollback.sh "$from_ref"', SCRIPT)

    def test_operations_guide_documents_both_legs(self):
        for ref in REFS:
            self.assertIn('UPGRADE_FROM_REF=%s ./scripts/test-upgrade.sh' % ref, OPERATIONS)
        default = re.search(r'^from_ref=\$\{UPGRADE_FROM_REF:-(\S+)\}$', SCRIPT, re.M).group(1)
        self.assertIn(default, REFS, 'the default starting tag has a CI leg')


if __name__ == '__main__':
    unittest.main()
