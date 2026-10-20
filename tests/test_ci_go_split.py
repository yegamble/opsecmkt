"""The Go tests run as three CI jobs that must together run every top-level test exactly once (A-465, MOD72-9).

Static checks on .github/workflows/ci.yaml (text only, so no YAML library is needed on the runner). `go-escrow` and
`go-desk` each run `go test -run '^(A|B|...)'`, where every alternative is a literal test-name prefix; `go` runs
`go test -skip` with the single pattern '^(' + the escrow alternatives + '|' + the desk alternatives + ')', compared as a
string. With literal prefixes, a top-level name matched by both -run patterns would start with one alternative of each,
so one alternative would be a prefix of the other; the tests refuse that, so the -run jobs are disjoint and `go` runs
exactly what neither of them runs. All three keep -race, -count=1 and the same timeout; each uploads its own coverage
file; all three are in the `required` aggregator.

The same module also checks the partition against actual test names: the unittest reads every top-level
Test/Fuzz/Example function in the module's *_test.go files, and

    go test -list . ./... | python3 tests/test_ci_go_split.py --list

checks the names the Go toolchain itself lists. Either fails on a name selected by no job or by two.
"""
from pathlib import Path
import os
import re
import sys
import unittest

REPOSITORY = Path(__file__).resolve().parents[1]
WORKFLOW = (REPOSITORY / '.github' / 'workflows' / 'ci.yaml').read_text()
# The jobs whose -run patterns `go` skips, in the order their alternatives appear in its -skip pattern.
RUN_JOBS = ('go-escrow', 'go-desk')
COVERAGE = {'go': ('coverage.out', 'go-coverage'), 'go-escrow': ('coverage-escrow.out', 'go-escrow-coverage'),
            'go-desk': ('coverage-desk.out', 'go-desk-coverage')}
TEST_NAME = re.compile(r'^(?:Test|Fuzz|Example)(?![a-z])\w*$')


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


def alternatives(pattern):
    """The literal name prefixes of a '^(A|B|...)' pattern, or None if the pattern has any other shape."""
    match = re.fullmatch(r'\^\(([A-Za-z0-9_]+(?:\|[A-Za-z0-9_]+)*)\)', pattern or '')
    return None if match is None else match.group(1).split('|')


def selectors():
    """{job: (flag, pattern)} for the three Go test jobs."""
    result = {}
    for job in ('go',) + RUN_JOBS:
        commands = go_test_commands(job_block(WORKFLOW, job))
        assert len(commands) == 1, (job, commands)
        flag = 'skip' if job == 'go' else 'run'
        result[job] = (flag, flag_value(commands[0], flag))
    return result


def jobs_selecting(name, selected=None):
    """The jobs whose `go test` would run the top-level test `name` (Go's -run/-skip use RE2; these are literals)."""
    selected = selected or selectors()
    jobs = []
    for job, (flag, pattern) in selected.items():
        matched = re.search(pattern, name) is not None
        if matched == (flag == 'run'):
            jobs.append(job)
    return jobs


def module_test_names():
    """Top-level test, fuzz and example functions in the packages `./...` covers (Go's ignore rules for directories).

    Files of every GOOS are read, so this is the Linux runner's set plus any other platform's; TestMain is no test."""
    names = []
    for directory, subdirectories, files in os.walk(REPOSITORY):
        subdirectories[:] = [d for d in subdirectories if not d.startswith(('.', '_')) and d not in ('testdata', 'node_modules')]
        for file in files:
            if file.endswith('_test.go') and not file.startswith(('.', '_')):
                source = (Path(directory) / file).read_text()
                names += [n for n in re.findall(r'^func (\w+)\(', source, re.M) if TEST_NAME.match(n) and n != 'TestMain']
    return names


class GoSplitTest(unittest.TestCase):
    def setUp(self):
        self.commands = {job: go_test_commands(job_block(WORKFLOW, job)) for job in ('go',) + RUN_JOBS}

    def test_each_job_runs_exactly_one_go_test_command(self):
        for job, commands in self.commands.items():
            self.assertEqual(len(commands), 1, (job, commands))

    def test_run_patterns_are_anchored_literal_prefixes(self):
        for job in RUN_JOBS:
            pattern = flag_value(self.commands[job][0], 'run')
            self.assertTrue(pattern, 'the %s job has no -run pattern: %s' % (job, self.commands[job][0]))
            self.assertIsNotNone(alternatives(pattern),
                                 '%s: -run must be ^(A|B|...) of test-name prefixes (no slash, no metacharacters): %s' % (job, pattern))

    def test_go_skips_exactly_the_union_of_the_run_patterns(self):
        skip = flag_value(self.commands['go'][0], 'skip')
        union = [a for job in RUN_JOBS for a in alternatives(flag_value(self.commands[job][0], 'run'))]
        self.assertEqual(skip, '^(' + '|'.join(union) + ')')

    def test_run_patterns_are_disjoint(self):
        escrow, desk = (alternatives(flag_value(self.commands[job][0], 'run')) for job in RUN_JOBS)
        for a in escrow:
            for b in desk:
                self.assertFalse(a.startswith(b) or b.startswith(a),
                                 '%r and %r overlap: a test named after the longer one would run in two jobs' % (a, b))

    def test_no_job_has_a_second_selector_that_would_break_the_partition(self):
        self.assertIsNone(flag_value(self.commands['go'][0], 'run'), 'the go job must not also narrow with -run')
        for job in RUN_JOBS:
            self.assertIsNone(flag_value(self.commands[job][0], 'skip'), 'the %s job must not also -skip' % job)
        for commands in self.commands.values():
            self.assertNotRegex(commands[0], r'-(short|failfast|shuffle|list)\b')

    def test_every_test_in_the_module_runs_in_exactly_one_job(self):
        names = module_test_names()
        self.assertGreater(len(names), 1000, 'the source scan found too few tests to be the whole module')
        selected = selectors()
        wrong = {name: jobs for name in set(names) if len(jobs := jobs_selecting(name, selected)) != 1}
        self.assertEqual(wrong, {}, 'top-level tests run by no job or by more than one')

    def test_every_run_alternative_selects_a_test(self):
        names = set(module_test_names())
        for job in RUN_JOBS:
            for prefix in alternatives(flag_value(self.commands[job][0], 'run')):
                if (job, prefix) == ('go-escrow', 'FuzzEscrow'):
                    continue  # A-465's pattern reserves it, so a future escrow fuzz target runs with the escrow tests
                self.assertTrue(any(n.startswith(prefix) for n in names),
                                '%s: %r selects no test (a typo would leave its tests in go)' % (job, prefix))

    def test_all_jobs_keep_race_count_and_the_same_timeout(self):
        for commands in self.commands.values():
            self.assertRegex(commands[0], r'\bgo test -race -count=1 -timeout 17m ')
            self.assertTrue(commands[0].endswith(' ./...'), commands[0])

    def test_each_job_writes_and_uploads_its_own_coverage_file(self):
        for job, (path, artifact) in COVERAGE.items():
            self.assertEqual(flag_value(self.commands[job][0], 'coverprofile'), path)
            self.assertRegex(job_block(WORKFLOW, job), r'name: %s\n\s+path: %s\n' % (re.escape(artifact), re.escape(path)))

    def test_run_jobs_have_the_same_database_service_and_pins_as_the_go_job(self):
        go_block = job_block(WORKFLOW, 'go')
        for job in RUN_JOBS:
            block = job_block(WORKFLOW, job)
            for fragment in (
                'image: postgres:17-alpine',
                'TEST_DATABASE_URL: postgres://test:test-ci-only@127.0.0.1:5432/postgres?sslmode=disable',
                'runs-on: ubuntu-24.04',
                'timeout-minutes: 20\n',
            ):
                self.assertIn(fragment, go_block)
                self.assertIn(fragment, block, job)
            for action in ('actions/checkout@', 'actions/setup-go@', 'actions/upload-artifact@'):
                self.assertEqual(re.findall(re.escape(action) + r'\S+ # \S+', go_block),
                                 re.findall(re.escape(action) + r'\S+ # \S+', block), job)

    def test_go_job_keeps_the_checks_that_run_once(self):
        go_block = job_block(WORKFLOW, 'go')
        for fragment in ('gofmt -l cmd internal', 'go mod verify', 'go vet ./...', 'actionlint', 'govulncheck', 'libjpeg-turbo-progs'):
            self.assertIn(fragment, go_block)
            for job in RUN_JOBS:
                self.assertNotIn(fragment, job_block(WORKFLOW, job), job)

    def test_the_only_cjpeg_test_stays_in_the_job_that_installs_cjpeg(self):
        images = (REPOSITORY / 'internal' / 'market' / 'images_test.go').read_text()
        names = re.findall(r'^func (Test\w+)\(', images, re.M)
        users = [n for n in names if 'cjpeg' in re.search(r'^func %s\(.*?^}' % n, images, re.M | re.S).group(0)]
        self.assertEqual(users, ['TestDecodeAllocationWithinEstimate'])
        self.assertEqual(jobs_selecting(users[0]), ['go'])

    def test_required_aggregator_waits_for_every_go_job(self):
        match = re.search(r'^    needs: \[([^\]]+)\]$', job_block(WORKFLOW, 'required'), re.M)
        self.assertTrue(match)
        needs = [name.strip() for name in match.group(1).split(',')]
        for job in ('go',) + RUN_JOBS:
            self.assertIn(job, needs)


def check_listing(lines):
    """Exit status for `go test -list` output on stdin: 1 if any listed test runs in no job or in two, or if a package
    failed to build (its tests would be missing from the listing; the pipe hides go's own exit status)."""
    selected = selectors()
    counts = {job: 0 for job in selected}
    wrong = []
    lines = [line.strip() for line in lines]
    failed = [line for line in lines if line.startswith('FAIL')]
    if failed:
        print('go test -list failed:\n  ' + '\n  '.join(failed))
        return 1
    names = [line for line in lines if TEST_NAME.match(line)]
    for name in names:
        jobs = jobs_selecting(name, selected)
        if len(jobs) == 1:
            counts[jobs[0]] += 1
        else:
            wrong.append((name, jobs))
    print('%d top-level tests listed: %s' % (len(names), ', '.join('%s %d' % item for item in counts.items())))
    print('%d in no job or more than one%s' % (len(wrong), ''.join('\n  %s: %s' % (n, j or 'none') for n, j in wrong)))
    return 1 if wrong or not names else 0


if __name__ == '__main__':
    if sys.argv[1:] == ['--list']:
        sys.exit(check_listing(sys.stdin))
    unittest.main()
