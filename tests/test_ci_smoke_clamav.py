"""scripts/ci-smoke.sh's throwaway ClamAV database build survives a 00-prefixed MD5 (A-708, the A-707 flake).

ClamAV 1.5 refuses a daily.cud whose tar.gz MD5 starts with 00 (ClamAV 1.1-1.4 cannot verify one) and retries once,
but within the same second it packs the same bytes again, so the container job failed about once in 256 runs.
The script now builds again under a new builder name, which changes the packed daily.info and so the MD5, only for
that refusal and only a bounded number of times. These tests run the script's in-container shell (the single-quoted
`bash -c` program of scanner_prepare) with its /build directory moved to a temporary one, against a stub sigtool
for the retry paths. Nothing here skips (the Operations job fails on any skipped Python test): the real sigtool runs
in the container job's scanner leg, inside the pinned clamd image, where scanner_prepare checks the header it wrote
(version 1, one signature, unsigned, a retry builder name, an MD5 that is not 00-prefixed) before the leg expects
"daily database 1 built <time>" on /admin/system. That header check is run here against a header the real
ClamAV 1.5.4 sigtool wrote with this program, and against altered copies.
"""
from pathlib import Path
import os
import re
import stat
import subprocess
import tempfile
import unittest

REPOSITORY = Path(__file__).resolve().parents[1]
SMOKE = (REPOSITORY / 'scripts' / 'ci-smoke.sh').read_text()
NDB = (REPOSITORY / 'deploy' / 'testdata' / 'clamav-test.ndb').read_bytes()
# Written by Homebrew ClamAV 1.5.4 sigtool (the pinned image's version) running this program on 2026-10-28.
REAL_HEADER = 'ClamAV-VDB:28 Oct 2026 08-20 -0400:1:1:234:119e9811928485d0d88ae0febef8a2a6:X:ci1:1793190007'
REFUSAL = 'The tar.gz MD5 starts with 00, which will fail to verify in ClamAV 1.1 -> 1.4: 00ab'

STUB = '''#!/bin/sh
# --info prints a line; --build fails with $STUB_MODE until $STUB_FAILURES builds have been refused.
case "$1" in --info) echo "Builder: stub"; exit 0 ;; esac
echo "${SIGNDUSER:-none}" >> "$STUB_LOG"
count=$(wc -l < "$STUB_LOG")
if [ "$count" -le "$STUB_FAILURES" ]; then
  if [ "$STUB_MODE" = 00 ]; then echo "%s"; echo "WARNING: Retrying the build for a chance at a better hash." >&2
  else echo "ERROR: build: Can't open file COPYING" >&2; fi
  exit 1
fi
echo "CUD built by $SIGNDUSER" > daily.cud
''' % REFUSAL


def container_program():
    match = re.search(r'--entrypoint bash "\$image" -c \'\n(.*?)\' < deploy/testdata/clamav-test.ndb', SMOKE, re.S)
    assert match, 'scanner_prepare no longer runs one bash -c program in the clamd image'
    return match.group(1)


class ClamavTestDatabaseBuildTest(unittest.TestCase):
    def run_program(self, path_prefix=None, **env):
        with tempfile.TemporaryDirectory() as work:
            work = Path(work)
            build = work / 'build'
            build.mkdir()
            program = container_program().replace('/build', str(build))
            environment = dict(os.environ, STUB_LOG=str(work / 'builds.log'), **env)
            if path_prefix:
                environment['PATH'] = '%s:%s' % (path_prefix, environment['PATH'])
            result = subprocess.run(['bash', '-c', program], input=NDB, capture_output=True, env=environment)
            builds = (work / 'builds.log').read_text().split() if (work / 'builds.log').exists() else []
            return result, builds

    def stubbed(self, mode, failures):
        with tempfile.TemporaryDirectory() as bin_dir:
            sigtool = Path(bin_dir) / 'sigtool'
            sigtool.write_text(STUB)
            sigtool.chmod(sigtool.stat().st_mode | stat.S_IXUSR)
            return self.run_program(bin_dir, STUB_MODE=mode, STUB_FAILURES=str(failures))

    def test_a_00_refusal_builds_again_under_a_new_builder_name(self):
        result, builds = self.stubbed('00', 2)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(builds, ['ci1', 'ci2', 'ci3'])
        self.assertEqual(result.stdout, b'CUD built by ci3\n')
        self.assertIn(REFUSAL.encode(), result.stderr)

    def test_retries_are_bounded(self):
        result, builds = self.stubbed('00', 100)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(len(builds), len(set(builds)))
        self.assertLessEqual(len(builds), 6)
        self.assertEqual(result.stdout, b'')

    def test_any_other_build_failure_is_not_retried(self):
        result, builds = self.stubbed('other', 1)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(builds, ['ci1'])
        self.assertIn(b"Can't open file COPYING", result.stderr)
        self.assertEqual(result.stdout, b'')

    def check_header(self, header):
        """scanner_prepare's host-side check of the built daily.cud, run on a file holding this header."""
        lines = SMOKE[SMOKE.index('  header=$(head -c 512'):].splitlines()[:3]
        self.assertTrue(lines[2].lstrip().startswith('[[ $header =~ $re'), lines)
        program = 'fail() { echo "FAIL: $*" >&2; exit 1; }\n' + '\n'.join(lines) + '\necho accepted\n'
        with tempfile.TemporaryDirectory() as work:
            (Path(work) / 'clamav').mkdir()
            (Path(work) / 'clamav' / 'daily.cud').write_bytes(header.ljust(512).encode() + b'\x1f\x8b')
            return subprocess.run(['bash', '-c', program], env=dict(os.environ, work=work), capture_output=True,
                                  text=True)

    def test_the_container_job_checks_the_real_sigtool_header(self):
        result = self.check_header(REAL_HEADER)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, 'accepted\n')
        fields = REAL_HEADER.split(':')
        for index, value in ((2, '2'), (3, '0'), (5, '00' + fields[5][2:]), (6, 'signature'), (7, 'ci'),
                             (7, 'ci7'), (7, 'clamav')):
            altered = ':'.join(fields[:index] + [value] + fields[index + 1:])
            with self.subTest(altered=altered):
                result = self.check_header(altered)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('unexpected test signature database header', result.stderr)

if __name__ == '__main__':
    unittest.main()
