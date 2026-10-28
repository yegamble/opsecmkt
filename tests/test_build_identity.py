"""Build identity and cache-busting builds (A-696; DA95-1, DA95-7, QA95-4).

Every container build used to report "development" with no commit, so /admin/system could not say what serves, and
the documented operator build could reuse a stale `apt-get upgrade` layer. These checks need no Docker:

* the Dockerfile's own `go build` RUN line is run with a stub `go`: empty build arguments keep "development" and no
  commit, a tag and commit reach `-X main.buildVersion`/`-X main.buildCommit`, anything else is refused;
* the installer's `build_identity` function is run in throwaway git checkouts (clean and tagged, untagged, dirty,
  not a checkout) and compose.yaml passes its values to the image build;
* CI's image build passes the commit and scripts/ci-smoke.sh checks `version` and /admin/system against it;
* the documented app and Tor image builds pass `--pull` and the build day, and the gateway's public Tor image can be
  replaced without touching its identity.
"""
from pathlib import Path
import os
import shutil
import re
import stat
import subprocess
import tempfile
import unittest

REPOSITORY = Path(__file__).resolve().parents[1]
DOCKERFILE = (REPOSITORY / 'Dockerfile').read_text()
TOR_DOCKERFILE = (REPOSITORY / 'deploy' / 'Dockerfile.tor').read_text()
CI = (REPOSITORY / '.github' / 'workflows' / 'ci.yaml').read_text()
COMPOSE = (REPOSITORY / 'compose.yaml').read_text()
INSTALLER = (REPOSITORY / 'scripts' / 'install.sh').read_text()
SMOKE = (REPOSITORY / 'scripts' / 'ci-smoke.sh').read_text()
BINARY = (REPOSITORY / 'docs' / 'install' / 'binary.md').read_text()
TOR = (REPOSITORY / 'docs' / 'install' / 'tor.md').read_text()
COMMIT = '0123456789abcdef0123456789abcdef01234567'
REFRESH = '--build-arg SECURITY_REFRESH="$(date -u +%F)"'


def build_run():
    """The Dockerfile's RUN instruction that builds cmd/server, joined into one shell command."""
    runs = [m for m in re.findall(r'^RUN ((?:.*\\\n)*.*)$', DOCKERFILE, re.M) if 'go build' in m and './cmd/server' in m]
    assert len(runs) == 1, 'no single RUN go build ./cmd/server in Dockerfile'
    return runs[0].replace('\\\n', '')  # as Docker joins a continued instruction


def shell_commands(text):
    """Every command line in the ```sh blocks of a Markdown text, with backslash continuations joined."""
    blocks = re.findall(r'```(?:sh|bash)\n(.*?)```', text, re.S)
    return [line.strip() for block in blocks for line in block.replace('\\\n', ' ').splitlines() if line.strip()]


class DockerfileBuildIdentityTest(unittest.TestCase):
    def run_build(self, **build_args):
        with tempfile.TemporaryDirectory() as work:
            work = Path(work)
            go = work / 'go'
            go.write_text('#!/bin/sh\nprintf "%s\\n" "$@" > "$STUB_ARGS"\n')
            go.chmod(go.stat().st_mode | stat.S_IXUSR)
            env = dict(os.environ, PATH='%s:%s' % (work, os.environ['PATH']), STUB_ARGS=str(work / 'args'),
                       BUILD_VERSION='', BUILD_COMMIT='')
            env.update(build_args)
            result = subprocess.run(['sh', '-e', '-c', build_run()], env=env, capture_output=True, text=True)
            args = (work / 'args').read_text().splitlines() if (work / 'args').exists() else None
            return result, args

    def ldflags(self, args):
        flags = [a for a in args if a.startswith('-ldflags=')]
        self.assertEqual(len(flags), 1, args)
        return flags[0]

    def test_args_are_declared_empty_after_the_source_copy(self):
        build_stage = DOCKERFILE[:DOCKERFILE.index('\nFROM ', 1)]
        for arg in ('ARG BUILD_VERSION=\n', 'ARG BUILD_COMMIT=\n'):
            self.assertIn(arg, build_stage)
            # After COPY . . (which changes on every commit anyway), so a new commit invalidates no earlier layer.
            self.assertLess(build_stage.index('COPY . .'), build_stage.index(arg))
            self.assertLess(build_stage.index(arg), build_stage.index('go build'))
        self.assertIn('ARG SECURITY_REFRESH=\n', DOCKERFILE)
        self.assertIn('ARG SECURITY_REFRESH=\n', TOR_DOCKERFILE)

    def test_empty_args_keep_development_and_no_commit(self):
        result, args = self.run_build()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.ldflags(args), '-ldflags=-s -w -X main.buildVersion=development -X main.buildCommit=')
        self.assertIn('./cmd/server', args)
        self.assertIn('-trimpath', args)

    def test_tag_and_commit_reach_the_linker(self):
        for commit in (COMMIT, 'ab' * 32):
            with self.subTest(commit=len(commit)):
                result, args = self.run_build(BUILD_VERSION='v0.1.0-alpha.2', BUILD_COMMIT=commit)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(self.ldflags(args),
                                 '-ldflags=-s -w -X main.buildVersion=v0.1.0-alpha.2 -X main.buildCommit=' + commit)

    def test_other_values_are_refused_before_go_runs(self):
        for version, commit in (('v1 -X main.other=1', ''), ('v1"', ''), ('v' * 65, ''), ('ci', 'abc'),
                                ('ci', COMMIT.upper()), ('ci', COMMIT + ' -X x=y'), ('ci', COMMIT[:39] + 'g')):
            with self.subTest(version=version, commit=commit):
                result, args = self.run_build(BUILD_VERSION=version, BUILD_COMMIT=commit)
                self.assertNotEqual(result.returncode, 0)
                self.assertIsNone(args, 'go ran with a refused build identity')


class InstallerBuildIdentityTest(unittest.TestCase):
    def function(self):
        match = re.search(r'^build_identity\(\) \{\n.*?^\}\n', INSTALLER, re.M | re.S)
        self.assertIsNotNone(match, 'scripts/install.sh has no build_identity function')
        return match.group(0)

    def git(self, repo, *args):
        subprocess.run(['git', '-C', str(repo), '-c', 'user.name=t', '-c', 'user.email=t@example.invalid',
                        '-c', 'commit.gpgsign=false', '-c', 'tag.gpgsign=false', *args], check=True, capture_output=True)

    def identity(self, directory, path=None):
        script = self.function() + 'build_identity\nprintf "%s|%s|%s\\n" "${OPSECMKT_BUILD_VERSION-unset}" ' \
                                   '"${OPSECMKT_BUILD_COMMIT-unset}" "$OPSECMKT_SECURITY_REFRESH"\n'
        env = dict(os.environ, OPSECMKT_BUILD_VERSION='stale', OPSECMKT_BUILD_COMMIT='stale')
        if path is not None:
            env['PATH'] = path
        result = subprocess.run([shutil.which('bash'), '-e', '-u', '-c', script], cwd=directory, env=env, capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        version, commit, day = result.stdout.strip().split('|')
        self.assertRegex(day, r'^\d{4}-\d{2}-\d{2}$')
        return version, commit

    def repository(self, work):
        repo = Path(work)
        self.git(repo, 'init', '-q', '-b', 'main')
        (repo / 'file').write_text('one\n')
        self.git(repo, 'add', 'file')
        self.git(repo, 'commit', '-q', '-m', 'one')
        head = subprocess.run(['git', '-C', str(repo), 'rev-parse', 'HEAD'], check=True, capture_output=True,
                              text=True).stdout.strip()
        return repo, head

    def test_a_clean_tagged_checkout_passes_its_tag_and_commit(self):
        with tempfile.TemporaryDirectory() as work:
            repo, head = self.repository(work)
            self.git(repo, 'tag', '-a', '-m', 'release', 'v0.1.0-alpha.2')
            (repo / 'untracked').write_text('an untracked file is not part of the build identity\n')
            self.assertEqual(self.identity(repo), ('v0.1.0-alpha.2', head))

    def test_an_untagged_checkout_passes_only_its_commit(self):
        with tempfile.TemporaryDirectory() as work:
            repo, head = self.repository(work)
            self.assertEqual(self.identity(repo), ('unset', head))

    def test_uncommitted_changes_pass_no_commit(self):
        with tempfile.TemporaryDirectory() as work:
            repo, _ = self.repository(work)
            self.git(repo, 'tag', 'v0.1.0')
            (repo / 'file').write_text('changed\n')
            self.assertEqual(self.identity(repo), ('unset', 'unset'))

    def test_no_checkout_or_no_git_passes_nothing(self):
        with tempfile.TemporaryDirectory() as work:
            self.assertEqual(self.identity(work), ('unset', 'unset'))
            repo, _ = self.repository(work)
            empty = Path(work) / 'empty-path'
            empty.mkdir()
            date = subprocess.run(['sh', '-c', 'command -v date'], capture_output=True, text=True).stdout.strip()
            (empty / 'date').symlink_to(date)
            self.assertEqual(self.identity(repo, path=str(empty)), ('unset', 'unset'))

    def test_every_installer_compose_up_gets_the_identity(self):
        up_wait = INSTALLER[INSTALLER.index('\nup_wait() {'):]
        up_wait = up_wait[:up_wait.index('\n}\n')]
        self.assertIn('build_identity', up_wait)
        self.assertLess(up_wait.index('build_identity'), up_wait.index('docker compose up'))
        self.assertNotRegex(INSTALLER, r'docker compose (?:up|build)[^\n]*--build-arg')

    def test_compose_passes_them_to_the_app_image(self):
        app = COMPOSE[COMPOSE.index('\n  app:\n'):]
        app = app[:app.index('\n    restart:')]
        for line in ('BUILD_VERSION: ${OPSECMKT_BUILD_VERSION:-}', 'BUILD_COMMIT: ${OPSECMKT_BUILD_COMMIT:-}',
                     'SECURITY_REFRESH: ${OPSECMKT_SECURITY_REFRESH:-}'):
            self.assertIn(line, app)
        self.assertIn('context: .', app)


class CIBuildIdentityTest(unittest.TestCase):
    def container_job(self):
        return CI[CI.index('\n  container:\n'):CI.index('\n  installer:\n')]

    def test_ci_image_carries_the_commit_and_the_smoke_checks_it(self):
        job = self.container_job()
        build = job[job.index('docker/build-push-action@'):job.index('cache-to:')]
        for arg in ('SECURITY_REFRESH=${{ steps.refresh.outputs.day }}', 'BUILD_VERSION=ci', 'BUILD_COMMIT=${{ github.sha }}'):
            self.assertIn(arg, build)
        smoke = job[job.index('Bootstrap production image with a real database'):job.index('Malware scanner leg')]
        self.assertIn('EXPECT_BUILD_VERSION: ci', smoke)
        self.assertIn('EXPECT_BUILD_COMMIT: ${{ github.sha }}', smoke)

    def test_ci_smoke_asserts_version_and_admin_system(self):
        self.assertIn('docker run --rm --network none opsecmkt:ci version', SMOKE)
        self.assertIn('EXPECT_BUILD_COMMIT', SMOKE)
        self.assertIn("'/admin/system'", SMOKE)
        self.assertIn('This running build; commit ', SMOKE)
        self.assertIn('OPSEC Market (this app)', SMOKE)


class DocumentedBuildsTest(unittest.TestCase):
    def builds(self, text):
        return [c for c in shell_commands(text) if c.startswith('docker build ')]

    def test_documented_image_builds_pull_and_refresh(self):
        builds = self.builds(BINARY)
        app = [c for c in builds if c.endswith(' .')]
        tor = [c for c in builds if c.endswith('-f deploy/Dockerfile.tor deploy')]
        self.assertEqual(len(app), 1, builds)
        self.assertEqual(len(tor), 1, builds)
        for command in app + tor:
            self.assertIn(' --pull ', command)
            self.assertIn(REFRESH, command)
        self.assertIn('--build-arg BUILD_VERSION="$APP_TAG"', app[0])
        self.assertIn('--build-arg BUILD_COMMIT="$APP_COMMIT"', app[0])
        self.assertNotIn('BUILD_COMMIT', tor[0])
        commands = shell_commands(BINARY)
        self.assertIn("APP_COMMIT=$(git rev-parse --verify 'HEAD^{commit}')", commands)
        self.assertIn('test -z "$(git status --porcelain --untracked-files=no)"', commands)
        self.assertIn('docker run --rm --network none "$APP_REPOSITORY:$APP_TAG" version', commands)
        self.assertIn('opsecmkt YOUR_REVIEWED_RELEASE_OR_COMMIT commit', ' '.join(BINARY.split()))

    def test_tor_guide_points_to_the_tor_build_and_keeps_the_identity(self):
        self.assertIn('binary.md#building-and-publishing-the-tor-image', TOR)
        self.assertIn('## Building and publishing the Tor image', BINARY)
        section = TOR[TOR.index('### Replace the gateway\'s public Tor image'):]
        section = section[:section.index('\n### ', 1)]
        flat = ' '.join(section.split())
        for text in ('Keep the identity', 'HiddenServiceDir', 'hostname', '--verify-config', 'Never delete'):
            self.assertIn(text, flat)
        self.assertNotIn('docker volume rm', section)
        self.assertNotIn('keys generate onion', section)


if __name__ == '__main__':
    unittest.main()
