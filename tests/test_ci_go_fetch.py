"""A transient proxy failure is retried inside the download step, and nothing weakens where modules come from (A-782 SRE113-2).

scripts/test-go-fetch.sh runs the helper against a local proxy; this checks that the build and the vulnerability job use it, that
the test runs in CI, and that no workflow, build file or script reaches for a setting that would turn a proxy failure into an
unverified download: GOPROXY falling back to direct on any error (`|direct`), or the checksum database or go.sum turned off.
"""
from pathlib import Path
import re
import sys
import unittest

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / 'tests'))

from test_ci_go_split import job_block  # noqa: E402

WEAKENING = (
    ('GOPROXY with direct on a pipe', re.compile(r'GOPROXY\b[^\n]*\|\s*direct')),
    ('GOSUMDB=off', re.compile(r'GOSUMDB\b["\']?\s*[=:]\s*["\']?off')),
    ('GONOSUMDB', re.compile(r'\bGONOSUMDB\b')),
    ('GONOSUMCHECK', re.compile(r'\bGONOSUMCHECK\b')),
    ('GOFLAGS=-mod=mod', re.compile(r'GOFLAGS\b[^\n]*-mod=mod')),
)


def weakenings(text):
    return [name for name, pattern in WEAKENING if pattern.search(text)]


class GoFetchTest(unittest.TestCase):
    def test_the_patterns_catch_each_form(self):
        for name, text in (('GOPROXY with direct on a pipe', 'GOPROXY=https://proxy.golang.org|direct'),
                           ('GOPROXY with direct on a pipe', '  GOPROXY: "https://proxy.golang.org|direct"'),
                           ('GOSUMDB=off', 'GOSUMDB=off go build'),
                           ('GOSUMDB=off', '  GOSUMDB: off'),
                           ('GOSUMDB=off', '  GOSUMDB: "off"'),
                           ('GONOSUMDB', 'GONOSUMDB=*'),
                           ('GONOSUMCHECK', 'GONOSUMCHECK=1'),
                           ('GOFLAGS=-mod=mod', 'GOFLAGS=-mod=mod')):
            self.assertIn(name, weakenings(text), text)
        self.assertEqual(weakenings('GOPROXY=https://proxy.golang.org,direct\nGOSUMDB=sum.golang.org\nGOFLAGS=-modcacherw'), [])

    def test_nothing_weakens_the_module_source(self):
        files = sorted((ROOT / '.github' / 'workflows').iterdir())
        files += [ROOT / 'Dockerfile', ROOT / 'Makefile', ROOT / 'scripts' / 'go-fetch.sh', ROOT / 'scripts' / 'quality.sh']
        files += sorted(ROOT.glob('compose*.yaml'))
        self.assertGreater(len(files), 8)
        for path in files:
            self.assertEqual(weakenings(path.read_text()), [], path.relative_to(ROOT))

    def test_the_helper_sets_no_module_source(self):
        helper = re.sub(r'(?m)^\s*#.*$', '', (ROOT / 'scripts' / 'go-fetch.sh').read_text())
        self.assertNotRegex(helper, r'\b(GOPROXY|GOSUMDB|GOFLAGS|GONOPROXY|GOPRIVATE|GOINSECURE)\b')

    def test_the_build_and_the_vulnerability_job_use_it(self):
        dockerfile = (ROOT / 'Dockerfile').read_text()
        self.assertNotRegex(dockerfile, r'(?m)^RUN go mod download')
        # Before `COPY . .`, so an unrelated source change keeps the cached download layer.
        self.assertRegex(dockerfile, r'COPY go\.mod go\.sum \./\n(?:#[^\n]*\n)*COPY scripts/go-fetch\.sh [^\n]+\n(?:#[^\n]*\n)*ARG GO_FETCH_TIMEOUT=600\nRUN sh [^\n]*go-fetch\.sh\nCOPY \. \.')
        self.assertNotRegex(dockerfile, r'\bENV\s+GO(PROXY|SUMDB|FLAGS)')
        self.assertNotIn('scripts', (ROOT / '.dockerignore').read_text().split())
        # A package built from the release's source archive runs the same Dockerfile.
        self.assertIn('"scripts/go-fetch.sh"', (ROOT / 'scripts' / 'package-deployment.py').read_text())
        vuln = re.search(r'(?ms)^  vuln\)\n(.*?)^    ;;', (ROOT / 'scripts' / 'quality.sh').read_text()).group(1)
        self.assertLess(vuln.index('go-fetch.sh'), vuln.index('tool govulncheck'))
        self.assertIn('-modfile=tools/go.mod', vuln)

    def test_a_slow_link_can_raise_the_limit(self):
        # SRE113-8: as a build argument through the installer's .env, with no Dockerfile edit, and said where it is needed.
        self.assertRegex((ROOT / 'compose.yaml').read_text(), r'(?m)^        GO_FETCH_TIMEOUT: \$\{GO_FETCH_TIMEOUT:-600\}$')
        self.assertIn('${GO_FETCH_TIMEOUT:-', (ROOT / 'scripts' / 'go-fetch.sh').read_text())
        guide = (ROOT / 'docs' / 'install' / 'troubleshooting.md').read_text()
        first_install = guide[guide.index('## First install failed'):guide.index('## 403 "Origin')]
        self.assertIn('add `GO_FETCH_TIMEOUT=3600` to `.env`', first_install)
        self.assertIn('--build-arg GO_FETCH_TIMEOUT=3600', first_install)

    def test_three_stalled_attempts_fit_the_jobs_that_build_the_image(self):
        # Three attempts at the limit and the 10 s and 20 s waits, in every job that builds the application's Dockerfile.
        for workflow, job, limit in (('ci.yaml', 'container', 600), ('ci.yaml', 'installer', 600),
                                     ('release.yaml', 'package', 600), ('security.yaml', 'images', 180)):
            block = job_block((ROOT / '.github/workflows' / workflow).read_text(), job)
            minutes = int(re.search(r'(?m)^    timeout-minutes: (\d+)$', block).group(1))
            self.assertLess(3 * limit + 30, minutes * 60, (workflow, job))
        images = job_block((ROOT / '.github/workflows/security.yaml').read_text(), 'images')
        self.assertIn('build_args: GO_FETCH_TIMEOUT=180', images)
        self.assertIn('build-args: ${{ matrix.build_args }}', images)

    def test_operations_runs_it(self):
        self.assertIn('run: bash scripts/test-go-fetch.sh', job_block((ROOT / '.github/workflows/ci.yaml').read_text(), 'operations'))


if __name__ == '__main__':
    unittest.main()
