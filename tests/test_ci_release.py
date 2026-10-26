"""The release workflow's prerelease flag and notes (A-511) and its integrity checks (A-519).

Static checks on .github/workflows/release.yaml (text, like tests/test_ci_tor_socks.py), plus a run of the release
step's own shell with stub `gh` and `go` programs: a tag with a suffix is created with --prerelease, a plain tag is
not, and the notes lead with the test-network warning and carry the reproducibility wording and the Go version.
docs/ci-cd.md must say the same things.

A-519: the packaging job refuses a tag whose commit is not on main (run against a scratch repository), verifies the
committed signed export with the verifier it built (run with a real build of cmd/verify-audit), records the Go
version and the digest-pinned Dockerfile bases in build.json (run with a stub `go`), and attests the archive and
binaries with a SHA-pinned actions/attest-build-provenance before the draft release exists.
"""
from pathlib import Path
import hashlib
import json
import os
import re
import shutil
import stat
import subprocess
import tempfile
import unittest

from test_ci_go_split import job_block

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


def release_step(name=STEP):
    match = re.search(r'^      - name: %s\n(.*?)(?=^      [-#] |\Z)' % re.escape(name), WORKFLOW, re.M | re.S)
    assert match, 'no step %r in release.yaml' % name
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

    def test_application_binaries_are_standalone_and_checksummed(self):
        build = WORKFLOW[WORKFLOW.index('Build standalone application'):WORKFLOW.index('Package checksums')]
        self.assertIn('for goarch in amd64 arm64', build)
        self.assertIn('CGO_ENABLED=0 GOOS=linux GOARCH=$goarch', build)
        self.assertIn('./cmd/server', build)
        self.assertIn('"opsecmkt-linux-$goarch"', build)
        self.assertIn('./opsecmkt-linux-amd64 --help', build)
        self.assertIn('build.json opsecmkt-linux-* verify-audit-* > SHA256SUMS', WORKFLOW)
        self.assertIn('build.json opsecmkt-linux-* verify-audit-*', self.step)
        self.assertIn('docs/install/binary.md', self.step)
        self.assertIn('application_binaries', WORKFLOW)

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
                       STUB_ARGS=str(work / 'args.txt'), GH_TOKEN='unused', GITHUB_REPOSITORY='owner/opsecmkt')
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

    def test_notes_say_how_to_check_the_attestation(self):
        _, notes = self.run_step('v0.1.0')
        self.assertIn('\n' + ATTESTATION_LINE % 'owner/opsecmkt' + '\n', notes)
        self.assertLess(notes.index(ATTESTATION_LINE % 'owner/opsecmkt'), notes.index('Go version: '))


# A-519: release integrity.
GUARD = 'Refuse a tag that is not on main'
FIXTURE = 'Verify the committed signed export with the release verifier'
RECORD = 'Package checksums and build record'
ATTEST = 'Attest build provenance for the archive and binaries'
ATTEST_ACTION = 'uses: actions/attest-build-provenance@4d101475d8b20a2381f78447822ac1eab6504dd8 # v4.2.2'
ATTESTATION_LINE = ('Each archive and binary has a GitHub artifact attestation (Sigstore-signed SLSA build provenance '
                    'naming this repository, workflow and commit). Check one with: gh attestation verify FILE --repo %s')
FIXTURE_DIR = REPOSITORY / 'cmd' / 'verify-audit' / 'testdata'
PINNED_IMAGE = r'[^@\s]+:[^@\s]+@sha256:[0-9a-f]{64}'


def step_names():
    return re.findall(r'^      - (?:name: (.+)|uses: (\S+).*)$', WORKFLOW, re.M)


def step_index(name):
    names = [named or used for named, used in step_names()]
    return names.index(name)


def run_shell(name, work, extra_env, path_dirs=()):
    env = dict(os.environ, RUNNER_TEMP=str(work / 'runner-temp'), **extra_env)
    env['PATH'] = os.pathsep.join([str(d) for d in path_dirs] + [os.environ['PATH']])
    (work / 'runner-temp').mkdir(exist_ok=True)
    script = work / 'runner-temp' / 'step.sh'
    script.write_text(shell_of(release_step(name)))
    return subprocess.run(['bash', '-e', str(script)], cwd=work, env=env, capture_output=True, text=True)


def executable(path, text):
    path.write_text(text)
    path.chmod(path.stat().st_mode | stat.S_IXUSR)


class ReleaseIntegrityStaticTest(unittest.TestCase):
    def setUp(self):
        self.package = job_block(WORKFLOW, 'package')

    def test_every_action_is_pinned_by_full_commit_sha(self):
        uses = re.findall(r'^\s*(?:- )?uses: (.+)$', WORKFLOW, re.M)
        external = [use for use in uses if not use.startswith('./')]
        self.assertIn(ATTEST_ACTION[len('uses: '):], external)
        for use in external:
            self.assertRegex(use, r'^[\w.-]+/[\w.-]+(/[\w.-]+)*@[0-9a-f]{40} # v\d+(\.\d+)*$', use)

    def test_archive_and_binaries_are_attested_before_the_draft_exists(self):
        step = release_step(ATTEST)
        self.assertIn(ATTEST_ACTION, step)
        subjects = re.search(r'subject-path: \|\n((?:            \S.*\n)+)', step).group(1).split()
        self.assertEqual(subjects, ['opsecmkt.oci.tar.gz', 'opsecmkt-linux-*', 'verify-audit-*'])
        self.assertNotIn('continue-on-error', step)
        self.assertLess(step_index(RECORD), step_index(ATTEST))
        self.assertLess(step_index(ATTEST), step_index(STEP))
        permissions = re.search(r'^    permissions:\n((?:      .*\n)+)', self.package, re.M).group(1).split('\n')
        self.assertEqual(sorted(p.split('#')[0].strip() for p in permissions if p.split('#')[0].strip()),
                         ['attestations: write', 'contents: write', 'id-token: write'])
        # Only the packaging job may mint an OIDC token; the workflow default stays read-only.
        self.assertEqual(WORKFLOW.count('id-token: write'), 1)
        self.assertIn('\npermissions:\n  contents: read\njobs:\n', WORKFLOW)

    def test_tag_must_be_on_main_before_anything_is_built(self):
        checkout = self.package[:self.package.index('Require a version tag')]
        self.assertIn('fetch-depth: 0', checkout)
        self.assertIn('persist-credentials: false', checkout)
        self.assertIn('git merge-base --is-ancestor HEAD refs/remotes/origin/main', release_step(GUARD))
        first_build = min(step_index(name) for name in ('Build OCI archive with provenance and SBOM',
                                                         'Build the offline audit export verifier'))
        self.assertLess(step_index(GUARD), first_build)

    def test_release_verifier_checks_the_committed_fixture(self):
        step = release_step(FIXTURE)
        self.assertIn('fixture=cmd/verify-audit/testdata', step)
        self.assertIn('./verify-audit-linux-amd64 -pub "$pub" "$fixture/export.jsonl" "$fixture/export.sig"', step)
        self.assertLess(step_index('Build the offline audit export verifier'), step_index(FIXTURE))
        self.assertLess(step_index(FIXTURE), step_index(RECORD))
        for name in ('export.jsonl', 'export.sig', 'export.pub'):
            self.assertTrue((FIXTURE_DIR / name).is_file(), name)

    def test_build_record_names_go_version_and_base_digests(self):
        step = release_step(RECORD)
        self.assertIn("'go_version': go_version", step)
        self.assertIn("'base_images': base_images", step)
        self.assertIn("['go', 'env', 'GOVERSION']", step)

    def test_every_dockerfile_base_is_pinned_by_tag_and_digest(self):
        tracked = subprocess.run(['git', 'ls-files', '--', 'Dockerfile', '*/Dockerfile', '*/Dockerfile.*'],
                                 cwd=REPOSITORY, check=True, capture_output=True, text=True).stdout.split()
        self.assertIn('Dockerfile', tracked)
        self.assertIn('deploy/Dockerfile.tor', tracked)
        for name in tracked:
            images = re.findall(r'^FROM\s+(\S+)', (REPOSITORY / name).read_text(), re.M)
            self.assertTrue(images, name)
            for image in images:
                self.assertRegex(image, '^%s$' % PINNED_IMAGE, name)

    def test_ci_cd_doc_describes_the_integrity_checks(self):
        for text in ('gh attestation verify', 'actions/attest-build-provenance', 'not on `main`', 'go_version',
                     'base_images', 'cmd/verify-audit/testdata'):
            self.assertIn(text, CI_CD)
        self.assertNotIn('does not create GitHub artifact attestations', CI_CD)


class TagOnMainRunTest(unittest.TestCase):
    """The guard step against a scratch repository whose origin/main is set by hand."""

    def git(self, work, *args):
        return subprocess.run(['git', '-c', 'user.name=t', '-c', 'user.email=t@example.invalid', *args], cwd=work,
                              check=True, capture_output=True, text=True).stdout.strip()

    def repository(self, work):
        self.git(work, 'init', '-q', '-b', 'main')
        for name in ('first', 'second'):
            (work / name).write_text(name)
            self.git(work, 'add', name)
            self.git(work, 'commit', '-q', '-m', name)
        main = self.git(work, 'rev-parse', 'HEAD')
        older = self.git(work, 'rev-parse', 'HEAD~1')
        self.git(work, 'checkout', '-q', '-b', 'feature', older)
        (work / 'side').write_text('side')
        self.git(work, 'add', 'side')
        self.git(work, 'commit', '-q', '-m', 'side')
        return main, older, self.git(work, 'rev-parse', 'HEAD')

    def guard(self, work, commit, origin_main):
        self.git(work, 'checkout', '-q', '--detach', commit)
        if origin_main:
            self.git(work, 'update-ref', 'refs/remotes/origin/main', origin_main)
        return run_shell(GUARD, work, {'RELEASE_TAG': 'v0.1.0'})

    def test_commits_on_main_pass_and_others_are_refused(self):
        with tempfile.TemporaryDirectory() as scratch:
            work = Path(scratch)
            main, older, side = self.repository(work)
            self.assertEqual(self.guard(work, main, main).returncode, 0)
            self.assertEqual(self.guard(work, older, main).returncode, 0)
            refused = self.guard(work, side, main)
            self.assertEqual(refused.returncode, 1)
            self.assertIn('Refusing: tag v0.1.0 (%s) is not on main' % side, refused.stderr)

    def test_missing_main_ref_is_refused(self):
        with tempfile.TemporaryDirectory() as scratch:
            work = Path(scratch)
            main, _, _ = self.repository(work)
            self.git(work, 'branch', '-q', '-D', 'main')
            self.assertNotEqual(self.guard(work, main, None).returncode, 0)


class FixtureRunTest(unittest.TestCase):
    """The fixture step with a real build of cmd/verify-audit (the CI job that runs this has Go)."""

    @classmethod
    def setUpClass(cls):
        cls.build = tempfile.TemporaryDirectory()
        cls.verifier = Path(cls.build.name) / 'verify-audit'
        subprocess.run(['go', 'build', '-o', str(cls.verifier), './cmd/verify-audit'], cwd=REPOSITORY, check=True)

    @classmethod
    def tearDownClass(cls):
        cls.build.cleanup()

    def run_fixture(self, verifier=None, export=None):
        with tempfile.TemporaryDirectory() as scratch:
            work = Path(scratch)
            shutil.copytree(FIXTURE_DIR, work / 'cmd' / 'verify-audit' / 'testdata')
            if export is not None:
                (work / 'cmd' / 'verify-audit' / 'testdata' / 'export.jsonl').write_text(export)
            if verifier is None:
                shutil.copy(self.verifier, work / 'verify-audit-linux-amd64')
            else:
                executable(work / 'verify-audit-linux-amd64', verifier)
            return run_shell(FIXTURE, work, {})

    def test_real_verifier_accepts_the_fixture_and_refuses_a_tampered_copy(self):
        result = self.run_fixture()
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_a_verifier_that_accepts_everything_fails_the_step(self):
        export = (FIXTURE_DIR / 'export.jsonl').read_bytes()
        line = 'OK: 4 events (last id 9), sha256 %s, signed by the pinned key' % hashlib.sha256(export).hexdigest()
        result = self.run_fixture(verifier='#!/bin/sh\necho "%s"\n' % line)
        self.assertNotEqual(result.returncode, 0)

    def test_a_verifier_reporting_other_events_fails_the_step(self):
        # Refuses the tampered copy for the right reason, but reports the genuine export wrongly.
        verifier = ('#!/bin/sh\ncase "$4" in *tampered*) echo "FAIL: export sha256 does not match the signature file" >&2;'
                    ' exit 1;; esac\necho "OK: 3 events (last id 9), sha256 0, signed by the pinned key"\n')
        self.assertNotEqual(self.run_fixture(verifier=verifier).returncode, 0)

    def test_a_changed_fixture_fails_the_step(self):
        export = (FIXTURE_DIR / 'export.jsonl').read_text().replace('vendor1', 'vendor2')
        self.assertNotEqual(self.run_fixture(export=export).returncode, 0)


class BuildRecordRunTest(unittest.TestCase):
    """The checksum and build.json step with a stub `go`; nothing is built."""

    def run_record(self, dockerfile):
        with tempfile.TemporaryDirectory() as scratch:
            work = Path(scratch)
            stubs = work / 'stubs'
            stubs.mkdir()
            executable(stubs / 'go', '#!/bin/sh\n[ "$1 $2" = "env GOVERSION" ] || exit 2\necho go9.9.9\n')
            for name in ('opsecmkt.oci.tar', 'opsecmkt-linux-amd64', 'opsecmkt-linux-arm64',
                         'verify-audit-linux-amd64', 'verify-audit-windows-amd64.exe'):
                (work / name).write_text(name)
            (work / 'Dockerfile').write_text(dockerfile)
            result = run_shell(RECORD, work, {
                'RELEASE_TAG': 'v0.1.0', 'GITHUB_REPOSITORY': 'owner/opsecmkt', 'GITHUB_SHA': 'a' * 40,
                'GITHUB_SERVER_URL': 'https://github.com', 'GITHUB_RUN_ID': '42'}, [stubs])
            record = json.loads((work / 'build.json').read_text()) if (work / 'build.json').exists() else None
            sums = (work / 'SHA256SUMS').read_text() if (work / 'SHA256SUMS').exists() else ''
            return result, record, sums

    def test_record_carries_go_version_and_the_dockerfile_bases(self):
        dockerfile = (REPOSITORY / 'Dockerfile').read_text()
        result, record, sums = self.run_record(dockerfile)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(record['go_version'], 'go9.9.9')
        self.assertEqual(record['base_images'], re.findall(r'^FROM\s+(\S+)', dockerfile, re.M))
        self.assertEqual(len(record['base_images']), 2)
        self.assertEqual(record['commit'], 'a' * 40)
        self.assertEqual(record['verify_audit'], ['verify-audit-linux-amd64', 'verify-audit-windows-amd64.exe'])
        for name in ('opsecmkt.oci.tar.gz', 'build.json', 'opsecmkt-linux-arm64', 'verify-audit-linux-amd64'):
            self.assertIn('  ' + name + '\n', sums)

    def test_an_unpinned_base_is_refused(self):
        dockerfile = re.sub(r'@sha256:[0-9a-f]{64}', '', (REPOSITORY / 'Dockerfile').read_text(), count=1)
        result, record, _ = self.run_record(dockerfile)
        self.assertNotEqual(result.returncode, 0)
        self.assertIsNone(record)
        self.assertIn('Refusing: every Dockerfile FROM', result.stderr)


if __name__ == '__main__':
    unittest.main()
