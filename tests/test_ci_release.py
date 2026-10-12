"""The release workflow's prerelease flag and notes (A-511).

Static checks on .github/workflows/release.yaml (text, like tests/test_ci_tor_socks.py), plus a run of the release
step's own shell with stub `gh` and `go` programs: a tag with a suffix is created with --prerelease, a plain tag is
not, and the notes lead with the test-network warning and carry the reproducibility wording and the Go version.
docs/ci-cd.md must say the same things.
"""
from pathlib import Path
import os
import re
import stat
import subprocess
import tempfile
import unittest

REPOSITORY = Path(__file__).resolve().parents[1]
WORKFLOW = (REPOSITORY / '.github' / 'workflows' / 'release.yaml').read_text()
CI_CD = ' '.join((REPOSITORY / 'docs' / 'ci-cd.md').read_text().split())
STEP = 'Create private draft release'

WARNING = 'Pre-release. Test networks only. No real funds. Install: git checkout '
REPRODUCIBILITY = (
    'the OCI archive is for inspection and registry import',
    'scripts/install.sh builds the application from source on your host and is not bit-reproducible',
)
CAPTCHA_LINE = 'The default image CAPTCHA has no audio or text alternative.'
BUILD_LINE = "go build -trimpath -buildvcs=false -ldflags='-s -w -buildid=' -o verify-audit-OS-ARCH ./cmd/verify-audit"


def release_step():
    match = re.search(r'^      - name: %s\n(.*?)(?=^      - |\Z)' % re.escape(STEP), WORKFLOW, re.M | re.S)
    assert match, 'no step %r in release.yaml' % STEP
    return match.group(1)


def shell_of(step):
    """The step's run: block with the YAML indentation removed."""
    body = step.split('        run: |\n', 1)[1]
    return '\n'.join(line[10:] for line in body.splitlines()) + '\n'


class ReleaseNotesStaticTest(unittest.TestCase):
    def setUp(self):
        self.step = release_step()

    def test_prerelease_flag_is_conditional_on_a_tag_suffix(self):
        self.assertRegex(self.step, r'if \[\[ "\$RELEASE_TAG" == \*-\* \]\]; then flags=\(--prerelease\); fi')
        create = self.step[self.step.index('gh release create'):]
        self.assertIn('"${flags[@]}"', create)
        self.assertEqual(self.step.count('--prerelease'), 1)
        self.assertIn('--draft', create)

    def test_notes_lead_with_the_test_network_warning(self):
        self.assertIn(WARNING + "%s; see UPGRADING.md.", self.step)
        self.assertLess(self.step.index(WARNING), self.step.index("cat >> release-notes.md"))
        self.assertIn('> release-notes.md', self.step)

    def test_notes_carry_the_reproducibility_wording_and_build_line(self):
        for text in REPRODUCIBILITY:
            self.assertIn(text, self.step)
        self.assertIn(BUILD_LINE, self.step)
        self.assertIn('go version', self.step)

    def test_notes_state_the_captcha_limit(self):
        self.assertIn(CAPTCHA_LINE, self.step)

    def test_notes_build_line_matches_the_build_step(self):
        build = WORKFLOW[WORKFLOW.index('Build the offline audit export verifier'):WORKFLOW.index('Package checksums')]
        self.assertIn("CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch go build -trimpath -buildvcs=false -ldflags='-s -w -buildid=' \\", build)
        self.assertIn('./cmd/verify-audit', build)

    def test_ci_cd_doc_mirrors_the_wording(self):
        self.assertIn('--prerelease', CI_CD)
        self.assertIn(WARNING + '<tag>; see UPGRADING.md.', CI_CD)
        for text in REPRODUCIBILITY:
            self.assertIn(text, CI_CD)
        self.assertIn(BUILD_LINE, CI_CD)
        self.assertIn(CAPTCHA_LINE, CI_CD)
        checklist = CI_CD[CI_CD.index('Tag-time checklist'):]
        self.assertIn('"%s"' % CAPTCHA_LINE, checklist)


class ReleaseStepRunTest(unittest.TestCase):
    """Run the step's shell with stub gh and go; nothing is contacted."""

    def run_step(self, tag):
        with tempfile.TemporaryDirectory() as work:
            work = Path(work)
            bin_dir = work / 'bin'
            bin_dir.mkdir()
            (bin_dir / 'gh').write_text('#!/bin/sh\nprintf "%s\\n" "$@" > "$STUB_ARGS"\n')
            (bin_dir / 'go').write_text('#!/bin/sh\necho "go version go9.9.9 linux/amd64"\n')
            for stub in ('gh', 'go'):
                (bin_dir / stub).chmod((bin_dir / stub).stat().st_mode | stat.S_IXUSR)
            env = dict(os.environ, PATH='%s:%s' % (bin_dir, os.environ['PATH']), RELEASE_TAG=tag,
                       STUB_ARGS=str(work / 'args.txt'), GH_TOKEN='unused')
            script = work / 'step.sh'
            script.write_text(shell_of(release_step()))
            subprocess.run(['bash', '-e', str(script)], cwd=work, env=env, check=True)
            return (work / 'args.txt').read_text().split('\n'), (work / 'release-notes.md').read_text()

    def test_suffixed_tag_is_a_prerelease(self):
        args, notes = self.run_step('v0.1.0-alpha.2')
        self.assertIn('--prerelease', args)
        self.assertIn('--draft', args)
        self.assertTrue(notes.startswith(WARNING + 'v0.1.0-alpha.2; see UPGRADING.md.\n'), notes[:200])
        self.assertTrue(notes.rstrip().endswith('Go version: go version go9.9.9 linux/amd64'), notes[-200:])
        self.assertIn('\n' + CAPTCHA_LINE + '\n', notes)

    def test_plain_tag_is_not_a_prerelease(self):
        args, notes = self.run_step('v0.1.0')
        self.assertNotIn('--prerelease', args)
        self.assertIn('--draft', args)
        self.assertIn('not bit-reproducible', notes)
        self.assertIn(BUILD_LINE, notes)
        self.assertIn('\n' + CAPTCHA_LINE + '\n', notes)


if __name__ == '__main__':
    unittest.main()
