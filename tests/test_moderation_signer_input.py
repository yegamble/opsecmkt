"""The offline signer bounds untrusted input before reading or signing it."""
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / 'scripts' / 'sign-moderation-action.py'


class ModerationSignerInputTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='moderation-signer-input-')
        self.addCleanup(self.temp.cleanup)
        self.directory = Path(self.temp.name)
        self.output = self.directory / 'signed.json'
        self.openssl_called = self.directory / 'openssl-called'
        fake_openssl = self.directory / 'openssl'
        fake_openssl.write_text('#!/bin/sh\nprintf called > "$MODERATION_SIGNER_TEST_MARKER"\nexit 99\n')
        fake_openssl.chmod(0o755)
        self.environment = {**os.environ, 'PATH': str(self.directory),
                            'MODERATION_SIGNER_TEST_MARKER': str(self.openssl_called)}

    def command(self, token):
        return [str(SCRIPT), '--token', str(token), '--key', str(self.directory / 'unused-key.pem'),
                '--out', str(self.output)]

    def assert_refused(self, result):
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn('Could not sign review token', result.stderr)
        self.assertNotIn('Traceback', result.stderr)
        self.assertFalse(self.openssl_called.exists(), 'input refusal invoked OpenSSL')
        self.assertFalse(self.output.exists(), 'input refusal created a signed output')

    @unittest.skipUnless(sys.platform.startswith('linux'), 'requires Linux RLIMIT_AS enforcement')
    def test_sparse_oversized_file_is_refused_under_bounded_child_memory(self):
        token = self.directory / 'oversized.json'
        with token.open('wb') as source:
            source.truncate(256 * 1024 * 1024)
        # A full read of this sparse file cannot fit in the child address space.
        # Bounded rejection must still produce the normal refusal, not MemoryError.
        wrapper = ('import resource,runpy,sys; '
                   'resource.setrlimit(resource.RLIMIT_AS,(96*1024*1024,96*1024*1024)); '
                   'sys.argv=sys.argv[1:]; runpy.run_path(sys.argv[0],run_name="__main__")')
        result = subprocess.run([sys.executable, '-c', wrapper, *self.command(token)],
                                env=self.environment, capture_output=True, text=True, timeout=5)
        self.assert_refused(result)

    @unittest.skipUnless(hasattr(os, 'mkfifo'), 'requires a POSIX FIFO')
    def test_nonregular_fifo_is_refused_without_waiting_for_a_writer(self):
        token = self.directory / 'token.fifo'
        os.mkfifo(token, 0o600)
        try:
            result = subprocess.run([sys.executable, *self.command(token)], env=self.environment,
                                    capture_output=True, text=True, timeout=2)
        except subprocess.TimeoutExpired:
            self.fail('signer blocked opening a nonregular input')
        self.assert_refused(result)

    def test_directory_input_is_refused_before_signing(self):
        result = subprocess.run([sys.executable, *self.command(self.directory)], env=self.environment,
                                capture_output=True, text=True, timeout=5)
        self.assert_refused(result)

    def test_first_byte_over_limit_is_refused(self):
        token = self.directory / 'over-limit.json'
        token.write_bytes(b' ' * 2049)
        result = subprocess.run([sys.executable, *self.command(token)], env=self.environment,
                                capture_output=True, text=True, timeout=5)
        self.assert_refused(result)


if __name__ == '__main__':
    unittest.main()
