"""The Go tests run as two CI jobs that must together run every top-level test exactly once (A-465).

Static checks on .github/workflows/ci.yaml (text only, so no YAML library is needed on the runner): `go` runs
`go test -skip <P>` and `go-escrow` runs `go test -run <P>` with the identical pattern string, so neither a test run by
both nor a test run by neither can appear by editing one side. Both keep -race and -count=1 and the same timeout; each
uploads its own coverage file; both are in the `required` aggregator.
"""
from pathlib import Path
import re
import unittest

REPOSITORY = Path(__file__).resolve().parents[1]
WORKFLOW = (REPOSITORY / '.github' / 'workflows' / 'ci.yaml').read_text()


def job_block(text, name):
    jobs = text.split('\njobs:\n', 1)[1]
    match = re.search(r'^  %s:\n(.*?)(?=^  [a-z][a-z0-9-]*:\n|\Z)' % re.escape(name), jobs, re.M | re.S)
    assert match, 'no job %s in ci.yaml' % name
    return match.group(1)


def go_test_commands(block):
    """Every `go test ...` command line in a job block (ignoring comments)."""
    return [line.strip() for line in block.splitlines() if re.search(r'\bgo test\b', line) and not line.lstrip().startswith('#')]


def flag_value(command, flag):
    match = re.search(r"(?<=\s)-%s[ =](?:'([^']*)'|(\S+))" % flag, command)
    return None if match is None else (match.group(1) if match.group(1) is not None else match.group(2))


class GoSplitTest(unittest.TestCase):
    def setUp(self):
        self.go = go_test_commands(job_block(WORKFLOW, 'go'))
        self.escrow = go_test_commands(job_block(WORKFLOW, 'go-escrow'))

    def test_each_job_runs_exactly_one_go_test_command(self):
        self.assertEqual(len(self.go), 1, self.go)
        self.assertEqual(len(self.escrow), 1, self.escrow)

    def test_run_and_skip_patterns_are_the_same_string(self):
        skip = flag_value(self.go[0], 'skip')
        run = flag_value(self.escrow[0], 'run')
        self.assertTrue(skip, 'the go job has no -skip pattern: %s' % self.go[0])
        self.assertTrue(run, 'the go-escrow job has no -run pattern: %s' % self.escrow[0])
        self.assertEqual(skip, run)

    def test_neither_job_has_a_second_selector_that_would_break_the_complement(self):
        self.assertIsNone(flag_value(self.go[0], 'run'), 'the go job must not also narrow with -run')
        self.assertIsNone(flag_value(self.escrow[0], 'skip'), 'the go-escrow job must not also -skip')
        self.assertNotRegex(self.escrow[0] + self.go[0], r'-(short|failfast|shuffle)\b')

    def test_pattern_is_anchored_and_selects_by_top_level_name(self):
        pattern = flag_value(self.escrow[0], 'run')
        self.assertTrue(pattern.startswith('^'), 'an unanchored pattern also matches names in the middle')
        self.assertNotIn('/', pattern, 'a slash would select subtests, not the same top-level tests in both jobs')

    def test_both_jobs_keep_race_count_and_the_same_timeout(self):
        for command in self.go + self.escrow:
            self.assertRegex(command, r'\bgo test -race -count=1 -timeout 17m ')
            self.assertTrue(command.endswith(' ./...'), command)

    def test_each_job_writes_and_uploads_its_own_coverage_file(self):
        go_block, escrow_block = job_block(WORKFLOW, 'go'), job_block(WORKFLOW, 'go-escrow')
        self.assertEqual(flag_value(self.go[0], 'coverprofile'), 'coverage.out')
        self.assertEqual(flag_value(self.escrow[0], 'coverprofile'), 'coverage-escrow.out')
        self.assertRegex(go_block, r'name: go-coverage\n\s+path: coverage\.out\n')
        self.assertRegex(escrow_block, r'name: go-escrow-coverage\n\s+path: coverage-escrow\.out\n')

    def test_escrow_job_has_the_same_database_service_and_pins_as_the_go_job(self):
        go_block, escrow_block = job_block(WORKFLOW, 'go'), job_block(WORKFLOW, 'go-escrow')
        for fragment in (
            'image: postgres:17-alpine',
            'TEST_DATABASE_URL: postgres://test:test-ci-only@127.0.0.1:5432/postgres?sslmode=disable',
            'runs-on: ubuntu-24.04',
        ):
            self.assertIn(fragment, go_block)
            self.assertIn(fragment, escrow_block)
        for action in ('actions/checkout@', 'actions/setup-go@', 'actions/upload-artifact@'):
            self.assertEqual(re.findall(re.escape(action) + r'\S+ # \S+', go_block),
                             re.findall(re.escape(action) + r'\S+ # \S+', escrow_block))
        self.assertRegex(escrow_block, r'(?m)^    timeout-minutes: \d+$')

    def test_go_job_keeps_the_checks_that_run_once(self):
        go_block, escrow_block = job_block(WORKFLOW, 'go'), job_block(WORKFLOW, 'go-escrow')
        for fragment in ('gofmt -l cmd internal', 'go mod verify', 'go vet ./...', 'actionlint', 'govulncheck', 'libjpeg-turbo-progs'):
            self.assertIn(fragment, go_block)
            self.assertNotIn(fragment, escrow_block)

    def test_the_only_cjpeg_test_stays_in_the_job_that_installs_cjpeg(self):
        images = (REPOSITORY / 'internal' / 'market' / 'images_test.go').read_text()
        names = re.findall(r'^func (Test\w+)\(', images, re.M)
        users = [n for n in names if 'cjpeg' in re.search(r'^func %s\(.*?^}' % n, images, re.M | re.S).group(0)]
        self.assertEqual(users, ['TestDecodeAllocationWithinEstimate'])
        self.assertIsNone(re.match(flag_value(self.escrow[0], 'run'), users[0]))

    def test_required_aggregator_waits_for_the_escrow_job(self):
        match = re.search(r'^    needs: \[([^\]]+)\]$', job_block(WORKFLOW, 'required'), re.M)
        self.assertTrue(match)
        self.assertIn('go-escrow', [name.strip() for name in match.group(1).split(',')])


if __name__ == '__main__':
    unittest.main()
