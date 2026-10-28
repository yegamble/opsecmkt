"""The release workflow: prerelease flag and notes (A-511), dry-run dispatch, main-only tags and build identity (A-696).

Static checks on .github/workflows/release.yaml (text, like tests/test_ci_tor_socks.py), plus runs of its own step
shells with stub `gh`, `go` and `git` programs or a throwaway git repository: a tag with a suffix is created with
--prerelease, a plain tag is not, and the notes lead with the test-network warning and carry the reproducibility
wording and the Go version; a manual dispatch builds under the fixed dry-run tag and can never reach
`gh release create`; a tag whose commit is not on main is refused, and so is a checkout without main (fail closed).
The image scan (A-708) reads the OCI layout unpacked from the very opsecmkt.oci.tar that is gzip'd and published:
Trivy's --input opens a Docker archive tar or an OCI layout directory, never an OCI tar. The unpack step runs against
BuildKit-shaped archives built here, and packaging refuses an archive that changed after its scan.
docs/ci-cd.md must say the same things.
"""
from pathlib import Path
import hashlib
import json
import os
import re
import stat
import subprocess
import tarfile
import tempfile
import unittest

REPOSITORY = Path(__file__).resolve().parents[1]
WORKFLOW = (REPOSITORY / '.github' / 'workflows' / 'release.yaml').read_text()
CI_CD = ' '.join((REPOSITORY / 'docs' / 'ci-cd.md').read_text().split())
NOTES_STEP = 'Write release notes'
STEP = 'Create private draft release'
DRY_RUN_TAG = 'v0.0.0-dry-run'

WARNING = 'Pre-release. Test networks only. No real funds. Install: git checkout '
REPRODUCIBILITY = (
    'the OCI archive is for inspection and registry import',
    'scripts/install.sh builds the application from source on your host and is not bit-reproducible',
)
CAPTCHA_LINE = 'The default image CAPTCHA has no audio or text alternative.'
BUILD_LINE = "go build -trimpath -buildvcs=false -ldflags='-s -w -buildid=' -o verify-audit-OS-ARCH ./cmd/verify-audit"


def step_named(name):
    match = re.search(r'^      - name: %s\n(.*?)(?=^      - |^  [a-z]|\Z)' % re.escape(name), WORKFLOW, re.M | re.S)
    assert match, 'no step %r in release.yaml' % name
    return match.group(1)


def release_step():
    return step_named(STEP)


def job(name):
    match = re.search(r'^  %s:\n(.*?)(?=^  [a-z][a-z-]*:\n|\Z)' % re.escape(name), WORKFLOW, re.M | re.S)
    assert match, 'no job %r in release.yaml' % name
    return match.group(1)


def shell_of(step):
    """The step's run: block with the YAML indentation removed."""
    body = step.split('        run: |\n', 1)[1]
    lines = []
    for line in body.splitlines():
        if line.strip() and not line.startswith(' ' * 10):
            break  # the block ends at the first less-indented line (a comment before the next step)
        lines.append(line[10:])
    return '\n'.join(lines) + '\n'


def stub(bin_dir, name, body):
    path = bin_dir / name
    path.write_text('#!/bin/sh\n' + body)
    path.chmod(path.stat().st_mode | stat.S_IXUSR)


class ReleaseNotesStaticTest(unittest.TestCase):
    def setUp(self):
        self.step = release_step()
        self.notes = step_named(NOTES_STEP)

    def test_prerelease_flag_is_conditional_on_a_tag_suffix(self):
        self.assertRegex(self.step, r'if \[\[ "\$RELEASE_TAG" == \*-\* \]\]; then flags=\(--prerelease\); fi')
        create = self.step[self.step.index('gh release create'):]
        self.assertIn('"${flags[@]}"', create)
        self.assertEqual(self.step.count('--prerelease'), 1)
        self.assertIn('--draft', create)

    def test_notes_lead_with_the_test_network_warning(self):
        self.assertIn(WARNING + "%s; see UPGRADING.md.", self.notes)
        self.assertLess(self.notes.index(WARNING), self.notes.index("cat >> release-notes.md"))
        self.assertIn('> release-notes.md', self.notes)

    def test_notes_carry_the_reproducibility_wording_and_build_line(self):
        for text in REPRODUCIBILITY:
            self.assertIn(text, self.notes)
        self.assertIn(BUILD_LINE, self.notes)
        self.assertIn('go version', self.notes)

    def test_application_binaries_are_standalone_and_checksummed(self):
        build = WORKFLOW[WORKFLOW.index('Build standalone application'):WORKFLOW.index('Package checksums')]
        self.assertIn('for goarch in amd64 arm64', build)
        self.assertIn('CGO_ENABLED=0 GOOS=linux GOARCH=$goarch', build)
        self.assertIn('./cmd/server', build)
        self.assertIn('"opsecmkt-linux-$goarch"', build)
        self.assertIn('./opsecmkt-linux-amd64 --help', build)
        self.assertIn('build.json opsecmkt-linux-* verify-audit-* > SHA256SUMS', WORKFLOW)
        self.assertIn('build.json opsecmkt-linux-* verify-audit-*', self.step)
        self.assertIn('docs/install/binary.md', self.notes)
        self.assertIn('application_binaries', WORKFLOW)

    def test_notes_state_the_captcha_limit(self):
        self.assertIn(CAPTCHA_LINE, self.notes)

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
    """Run the notes and release steps' shells with stub gh and go; nothing is contacted."""

    def run_step(self, tag, event='push', ref=None):
        with tempfile.TemporaryDirectory() as work:
            work = Path(work)
            bin_dir = work / 'bin'
            bin_dir.mkdir()
            stub(bin_dir, 'gh', 'printf "%s\\n" "$@" > "$STUB_ARGS"\n')
            stub(bin_dir, 'go', 'echo "go version go9.9.9 linux/amd64"\n')
            env = dict(os.environ, PATH='%s:%s' % (bin_dir, os.environ['PATH']), RELEASE_TAG=tag,
                       STUB_ARGS=str(work / 'args.txt'), GH_TOKEN='unused', GITHUB_EVENT_NAME=event,
                       GITHUB_REF=ref if ref is not None else 'refs/tags/' + tag)
            for name, step in (('notes.sh', NOTES_STEP), ('release.sh', STEP)):
                (work / name).write_text(shell_of(step_named(step)))
            subprocess.run(['bash', '-e', str(work / 'notes.sh')], cwd=work, env=env, check=True)
            notes = (work / 'release-notes.md').read_text()
            result = subprocess.run(['bash', '-e', str(work / 'release.sh')], cwd=work, env=env,
                                    capture_output=True, text=True)
            args = (work / 'args.txt').read_text().split('\n') if (work / 'args.txt').exists() else None
            return result, args, notes

    def test_suffixed_tag_is_a_prerelease(self):
        result, args, notes = self.run_step('v0.1.0-alpha.2')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('--prerelease', args)
        self.assertIn('--draft', args)
        self.assertTrue(notes.startswith(WARNING + 'v0.1.0-alpha.2; see UPGRADING.md.\n'), notes[:200])
        self.assertTrue(notes.rstrip().endswith('Go version: go version go9.9.9 linux/amd64'), notes[-200:])
        self.assertIn('\n' + CAPTCHA_LINE + '\n', notes)

    def test_plain_tag_is_not_a_prerelease(self):
        result, args, notes = self.run_step('v0.1.0')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn('--prerelease', args)
        self.assertIn('--draft', args)
        self.assertIn('not bit-reproducible', notes)
        self.assertIn(BUILD_LINE, notes)
        self.assertIn('\n' + CAPTCHA_LINE + '\n', notes)

    def test_release_step_refuses_anything_but_the_tag_push(self):
        # Even if its job condition were removed, a dispatch or a mismatched ref never reaches gh release create.
        for event, ref in (('workflow_dispatch', 'refs/heads/main'), ('push', 'refs/heads/main'),
                           ('push', 'refs/tags/v9.9.9')):
            with self.subTest(event=event, ref=ref):
                result, args, _ = self.run_step('v0.1.0', event=event, ref=ref)
                self.assertNotEqual(result.returncode, 0)
                self.assertIsNone(args, 'gh was called')
                self.assertIn('only a tag push creates a release', result.stderr)


class ReleaseIdentityTest(unittest.TestCase):
    """The identity job's shells: the tag a run builds under, and the main-branch check, against real git."""

    def identity(self, event, ref, ref_name):
        with tempfile.TemporaryDirectory() as work:
            out = Path(work) / 'out'
            out.write_text('')
            env = dict(os.environ, EVENT=event, REF=ref, REF_NAME=ref_name, GITHUB_OUTPUT=str(out))
            result = subprocess.run(['bash', '-e', '-c', shell_of(step_named('Release identity'))], env=env,
                                    capture_output=True, text=True)
            return result, out.read_text()

    def test_a_tag_push_builds_under_its_tag(self):
        result, out = self.identity('push', 'refs/tags/v0.1.0-alpha.2', 'v0.1.0-alpha.2')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(out, 'tag=v0.1.0-alpha.2\n')

    def test_a_dispatch_builds_under_the_fixed_dry_run_tag(self):
        result, out = self.identity('workflow_dispatch', 'refs/heads/feature', 'feature')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(out, 'tag=%s\n' % DRY_RUN_TAG)

    def test_other_refs_and_events_are_refused(self):
        for event, ref, name in (('push', 'refs/heads/main', 'main'), ('push', 'refs/tags/release-1', 'release-1'),
                                 ('push', 'refs/tags/v1.2', 'v1.2'), ('pull_request', 'refs/pull/1/merge', '1/merge')):
            with self.subTest(event=event, ref=ref):
                result, out = self.identity(event, ref, name)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(out, '')

    def git(self, repo, *args):
        return subprocess.run(['git', '-C', str(repo), '-c', 'user.name=t', '-c', 'user.email=t@example.invalid',
                               '-c', 'commit.gpgsign=false', *args], check=True, capture_output=True, text=True).stdout.strip()

    def on_main(self, sha, with_main=True):
        with tempfile.TemporaryDirectory() as work:
            repo = Path(work)
            self.git(repo, 'init', '-q', '-b', 'main')
            self.git(repo, 'commit', '-q', '--allow-empty', '-m', 'main')
            main = self.git(repo, 'rev-parse', 'HEAD')
            self.git(repo, 'checkout', '-q', '-b', 'side')
            self.git(repo, 'commit', '-q', '--allow-empty', '-m', 'side')
            side = self.git(repo, 'rev-parse', 'HEAD')
            if with_main:
                self.git(repo, 'update-ref', 'refs/remotes/origin/main', main)
            env = dict(os.environ, GITHUB_SHA={'main': main, 'side': side}[sha])
            return subprocess.run(['bash', '-e', '-c', shell_of(step_named('Require the tagged commit on main'))],
                                  cwd=repo, env=env, capture_output=True, text=True)

    def test_a_commit_on_main_passes(self):
        result = self.on_main('main')
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_a_commit_off_main_is_refused(self):
        result = self.on_main('side')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('is not on main', result.stderr)

    def test_a_checkout_without_main_is_refused(self):
        result = self.on_main('main', with_main=False)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('origin/main is missing', result.stderr)


class ReleaseWorkflowShapeTest(unittest.TestCase):
    def test_dispatch_is_a_dry_run_and_only_tag_pushes_release(self):
        on = WORKFLOW[WORKFLOW.index('\non:\n'):WORKFLOW.index('\npermissions:')]
        self.assertIn("tags: ['v*']", on)
        self.assertIn('workflow_dispatch:', on)
        self.assertNotIn('inputs:', on)  # the dry-run tag is fixed, never chosen by the dispatcher
        release = job('release')
        self.assertIn("if: github.event_name == 'push' && startsWith(github.ref, 'refs/tags/v')", release)
        self.assertEqual(WORKFLOW.count('gh release create'), 1)
        self.assertIn('gh release create', release)
        self.assertNotIn('git push', WORKFLOW)
        self.assertNotIn('git tag', WORKFLOW)

    def test_only_the_release_job_can_write(self):
        self.assertIn('\npermissions:\n  contents: read\njobs:', WORKFLOW)
        self.assertEqual(WORKFLOW.count('contents: write'), 1)
        self.assertIn('    permissions:\n      contents: write\n', job('release'))
        for name in ('identity', 'package'):
            self.assertIn('    permissions:\n      contents: read\n', job(name))

    def test_jobs_run_in_order_and_main_is_checked_before_ci(self):
        self.assertIn('needs: identity', job('verify'))
        self.assertIn('needs: [identity, verify]', job('package'))
        self.assertIn('needs: [identity, package]', job('release'))
        identity = job('identity')
        self.assertIn('fetch-depth: 0', identity)
        self.assertIn('persist-credentials: false', identity)
        self.assertIn("if: github.event_name == 'push'", step_named('Require the tagged commit on main'))

    def test_package_uploads_and_release_verifies_the_files(self):
        package = job('package')
        upload = package[package.index('actions/upload-artifact@'):]
        for name in ('SHA256SUMS', 'build.json', 'opsecmkt.oci.tar.gz', 'opsecmkt-linux-*', 'verify-audit-*', 'release-notes.md'):
            self.assertIn(name, upload)
        self.assertIn('if-no-files-found: error', upload)
        release = job('release')
        self.assertRegex(release, r'actions/download-artifact@[0-9a-f]{40} # v')
        self.assertIn('sha256sum --check --strict SHA256SUMS', release)
        self.assertNotIn('actions/checkout', release)

    def test_actions_are_pinned_by_sha(self):
        for use in re.findall(r'uses: (\S+)', WORKFLOW):
            if use.startswith('./'):
                continue
            self.assertRegex(use, r'@[0-9a-f]{40}$')

    def test_build_identity_reaches_the_image_and_binaries(self):
        package = job('package')
        build = package[package.index('docker/build-push-action@'):package.index('Scan the actual release')]
        self.assertIn('BUILD_VERSION=${{ needs.identity.outputs.tag }}', build)
        self.assertIn('BUILD_COMMIT=${{ github.sha }}', build)
        self.assertIn('no-cache: true', build)
        binaries = step_named('Build standalone application and deployment CLI')
        self.assertIn('-X main.buildVersion=$RELEASE_TAG -X main.buildCommit=$GITHUB_SHA', binaries)
        self.assertIn('RELEASE_TAG: ${{ needs.identity.outputs.tag }}', binaries)
        self.assertIn('test "$(./opsecmkt-linux-amd64 version)" = "opsecmkt $RELEASE_TAG commit $GITHUB_SHA"', binaries)
        self.assertNotIn('github.ref_name', package)

    def test_ci_cd_doc_describes_the_dry_run_and_main_check(self):
        release = CI_CD[CI_CD.index('## Release a tested version'):CI_CD.index('## Tag-time checklist')]
        for text in ('workflow_dispatch', DRY_RUN_TAG, 'not on `main`', 'workflow artifact', 'never creates a release',
                     'version prints'):
            self.assertTrue(text in release, 'docs/ci-cd.md "Release a tested version" lacks %r' % text)
        self.assertTrue('Only the release job, which runs for tag pushes alone, receives `contents: write`' in CI_CD)


UNPACK = 'Unpack the release OCI archive for scanning'
SCAN = 'Scan the actual release OCI archive'
PACKAGE = 'Package checksums and build record'
LAYOUT = '${{ runner.temp }}/opsecmkt-oci'
OCI_INDEX = 'application/vnd.oci.image.index.v1+json'
OCI_MANIFEST = 'application/vnd.oci.image.manifest.v1+json'


def write_blob(layout, data, media_type, **extra):
    if not isinstance(data, bytes):
        data = json.dumps(data).encode()
    digest = hashlib.sha256(data).hexdigest()
    blobs = layout / 'blobs' / 'sha256'
    blobs.mkdir(parents=True, exist_ok=True)
    (blobs / digest).write_bytes(data)
    return dict({'mediaType': media_type, 'digest': 'sha256:' + digest, 'size': len(data)}, **extra)


def buildkit_archive(path, order=('image', 'attestation'), os_name='linux', arch='amd64'):
    """An OCI archive shaped like docker/build-push-action's type=oci output with provenance and an SBOM:
    index.json -> one image index (named by the tag) -> the platform image manifest, then its attestation manifest."""
    with tempfile.TemporaryDirectory() as work:
        layout = Path(work)
        (layout / 'oci-layout').write_text('{"imageLayoutVersion":"1.0.0"}')
        children = {}
        for kind, config in (('image', {'os': os_name, 'architecture': arch, 'rootfs': {'type': 'layers'}}),
                             ('attestation', {'os': 'unknown', 'architecture': 'unknown'})):
            layer = write_blob(layout, kind.encode() + b' layer', 'application/vnd.oci.image.layer.v1.tar+gzip')
            manifest = {'schemaVersion': 2, 'mediaType': OCI_MANIFEST, 'layers': [layer],
                        'config': write_blob(layout, config, 'application/vnd.oci.image.config.v1+json')}
            platform = {'os': config['os'], 'architecture': config['architecture']}
            children[kind] = write_blob(layout, manifest, OCI_MANIFEST, platform=platform)
        child = write_blob(layout, {'schemaVersion': 2, 'mediaType': OCI_INDEX,
                                    'manifests': [children[kind] for kind in order]}, OCI_INDEX,
                           annotations={'org.opencontainers.image.ref.name': DRY_RUN_TAG})
        (layout / 'index.json').write_text(json.dumps({'schemaVersion': 2, 'mediaType': OCI_INDEX, 'manifests': [child]}))
        with tarfile.open(path, 'w') as archive:
            for entry in sorted(layout.iterdir()):
                archive.add(entry, arcname=entry.name)


class ReleaseScanTest(unittest.TestCase):
    """A-708: the scan reads the published archive in a form Trivy v0.76 opens, and nothing else is published."""

    def test_the_scan_reads_the_layout_unpacked_from_the_published_archive(self):
        package = job('package')
        build = package.index('docker/build-push-action@')
        self.assertIn('outputs: type=oci,dest=opsecmkt.oci.tar\n', package)
        self.assertLess(build, package.index('- name: ' + UNPACK))
        self.assertLess(package.index('- name: ' + UNPACK), package.index('- name: ' + SCAN))
        unpack = shell_of(step_named(UNPACK))
        self.assertIn('layout="$RUNNER_TEMP/opsecmkt-oci"', unpack)
        self.assertIn('tar -xf opsecmkt.oci.tar -C "$layout"', unpack)
        self.assertIn('sha256sum opsecmkt.oci.tar > "$RUNNER_TEMP/scanned-oci.sha256"', unpack)
        scan = step_named(SCAN)
        self.assertIn('image-archive: %s\n' % LAYOUT, scan)
        self.assertNotIn('image-archive: opsecmkt.oci.tar', WORKFLOW)
        packaging = shell_of(step_named(PACKAGE))
        self.assertLess(packaging.index('sha256sum --check --strict "$RUNNER_TEMP/scanned-oci.sha256"'),
                        packaging.index('gzip -n opsecmkt.oci.tar'))
        self.assertIn('sha256sum opsecmkt.oci.tar.gz build.json opsecmkt-linux-* verify-audit-* > SHA256SUMS', packaging)

    def test_the_scan_action_still_rejects_fixable_high_and_critical(self):
        action = (REPOSITORY / '.github' / 'actions' / 'scan-image' / 'action.yml').read_text()
        self.assertIn('OCI layout directory', action)
        self.assertIn('input: ${{ inputs.image-archive }}', action)
        self.assertIn('--exit-code 1 --ignore-unfixed --severity HIGH,CRITICAL', action)
        self.assertIn('trivy image "${args[@]}" --input "$IMAGE_ARCHIVE"', action)

    def unpack(self, work, **archive):
        workspace, temp = work / 'workspace', work / 'temp'
        workspace.mkdir()
        temp.mkdir()
        buildkit_archive(workspace / 'opsecmkt.oci.tar', **archive)
        env = dict(os.environ, RUNNER_TEMP=str(temp))
        result = subprocess.run(['bash', '-e', '-c', shell_of(step_named(UNPACK))], cwd=workspace, env=env,
                                capture_output=True, text=True)
        return result, workspace, temp

    def test_a_buildkit_archive_unpacks_to_the_layout_trivy_opens(self):
        with tempfile.TemporaryDirectory() as work:
            result, workspace, temp = self.unpack(Path(work))
            self.assertEqual(result.returncode, 0, result.stderr)
            for name in ('oci-layout', 'index.json', 'blobs'):
                self.assertTrue((temp / 'opsecmkt-oci' / name).exists(), name)
            recorded = (temp / 'scanned-oci.sha256').read_text().split()
            self.assertEqual(recorded, [hashlib.sha256((workspace / 'opsecmkt.oci.tar').read_bytes()).hexdigest(),
                                        'opsecmkt.oci.tar'])
            self.assertIn('linux/amd64', result.stdout)

    def test_a_layout_that_would_scan_the_wrong_manifest_is_refused(self):
        # Trivy v0.76 follows the first descriptor of each index; an attestation there would scan no packages.
        for archive in ({'order': ('attestation', 'image')}, {'order': ()}, {'arch': 'arm64'}):
            with self.subTest(**archive), tempfile.TemporaryDirectory() as work:
                result, _, _ = self.unpack(Path(work), **archive)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('release OCI layout', result.stderr)

    def package(self, change_after_scan):
        with tempfile.TemporaryDirectory() as work:
            result, workspace, temp = self.unpack(Path(work))
            self.assertEqual(result.returncode, 0, result.stderr)
            if change_after_scan:
                with open(workspace / 'opsecmkt.oci.tar', 'ab') as archive:
                    archive.write(b'\0' * 512)
            for name in ('opsecmkt-linux-amd64', 'verify-audit-linux-amd64'):
                (workspace / name).write_text(name)
            env = dict(os.environ, RUNNER_TEMP=str(temp), RELEASE_TAG=DRY_RUN_TAG, GITHUB_REPOSITORY='o/r',
                       GITHUB_SHA='0' * 40, GITHUB_SERVER_URL='https://github.invalid', GITHUB_RUN_ID='1')
            result = subprocess.run(['bash', '-e', '-c', shell_of(step_named(PACKAGE))], cwd=workspace, env=env,
                                    capture_output=True, text=True)
            return result, sorted(path.name for path in workspace.iterdir()), (workspace / 'SHA256SUMS')

    def test_packaging_publishes_the_scanned_archive_and_refuses_a_changed_one(self):
        result, files, sums = self.package(change_after_scan=False)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('opsecmkt.oci.tar.gz', files)
        self.assertNotIn('opsecmkt.oci.tar', files)
        result, files, _ = self.package(change_after_scan=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn('opsecmkt.oci.tar.gz', files)
        self.assertNotIn('SHA256SUMS', files)

    def test_ci_cd_doc_describes_the_scanned_layout(self):
        for text in ('Trivy `--input` opens a Docker archive or an OCI layout directory',
                     'the same `opsecmkt.oci.tar` that is then compressed', 'checksum'):
            self.assertTrue(text in CI_CD, 'docs/ci-cd.md lacks %r' % text)


if __name__ == '__main__':
    unittest.main()
