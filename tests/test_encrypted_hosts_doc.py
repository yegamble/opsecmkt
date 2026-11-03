"""The encrypted-host guide and its install script must stay in step (A-799)."""
import os
from pathlib import Path
import re
import subprocess
import unittest

ROOT = Path(__file__).resolve().parents[1]
GUIDE = (ROOT / 'docs/install/encrypted-hosts.md').read_text()
SCRIPT_PATH = ROOT / 'scripts/rescue-luks-install.sh'
SCRIPT = SCRIPT_PATH.read_text()
# Provider and region names the public docs must not carry (owner instruction, 2026-11-03).
PROVIDER = 'digital' + r' ?ocean|\bdoctl\b|\bdroplets?\b|frank' + r'furt|\bfra1\b'


class EncryptedHostsGuideTest(unittest.TestCase):
    def test_script_parses_and_is_executable(self):
        subprocess.run(['bash', '-n', str(SCRIPT_PATH)], check=True)
        self.assertTrue(os.access(SCRIPT_PATH, os.X_OK))

    def test_script_refuses_before_formatting(self):
        # Every refusal must come before the first mkfs.
        first_mkfs = SCRIPT.index('mkfs.')
        for refusal in ('cryptroot not open', 'not LUKS2', 'vda read-only', '/mnt already mounted', 'no debootstrap'):
            self.assertIn(refusal, SCRIPT)
            self.assertLess(SCRIPT.index(refusal), first_mkfs, refusal)
            # ...and before any network value is read, so nothing is consumed for a refused run.
            self.assertLess(SCRIPT.index(refusal), SCRIPT.index('read -r PUB_CIDR'), refusal)
            # Each refusal must stop the script, not warn and continue.
            line = SCRIPT[SCRIPT.index(refusal):].split('\n', 1)[0]
            self.assertIn('exit 1', line, refusal)

    def test_script_never_handles_a_passphrase(self):
        # The header tells the owner what to type in the console; the code itself must not.
        code = '\n'.join(line for line in SCRIPT.splitlines() if not line.lstrip().startswith('#'))
        for forbidden in ('luksFormat', 'luksAddKey', '--key-file'):
            self.assertNotIn(forbidden, code, forbidden)
        # No silent read in any flag spelling (read -s, read -rs, read -r -s ...).
        self.assertNotRegex(code, r'\bread\s+(?:-\w+\s+)*-\w*s')

    def test_script_writes_no_host_address(self):
        literal = set(re.findall(r'\b\d{1,3}(?:\.\d{1,3}){3}\b', SCRIPT))
        # Network values arrive on standard input; only loopback may appear literally.
        self.assertLessEqual(literal, {'127.0.0.1', '127.0.1.1'})

    def test_network_values_come_from_standard_input(self):
        self.assertIn('read -r PUB_CIDR', SCRIPT)
        self.assertIn('network values do not parse', SCRIPT)
        # The refusals run before anything is read or formatted.
        self.assertLess(SCRIPT.index('cryptroot not open'), SCRIPT.index('read -r PUB_CIDR'))

    def test_names_no_provider_or_region(self):
        for text in (GUIDE, SCRIPT, Path(__file__).read_text()):
            self.assertIsNone(re.search(PROVIDER, text, re.I))

    def test_repository_names_no_provider_or_region(self):
        # Owner instruction (2026-11-03): the whole public tree, not only this guide.
        files = subprocess.run(['git', 'ls-files'], cwd=ROOT, check=True, capture_output=True,
                               text=True).stdout.split()
        hits = []
        for name in files:
            path = ROOT / name
            if name == 'tests/test_encrypted_hosts_doc.py' or not path.is_file():
                continue
            try:
                text = path.read_text()
            except (UnicodeDecodeError, OSError):
                continue
            for number, line in enumerate(text.splitlines(), 1):
                if re.search(PROVIDER, line, re.I) or re.search(r'US\$ ?\d', line):
                    hits.append('%s:%d' % (name, number))
        self.assertEqual(hits, [], 'provider, region or price named in the tree')

    def test_initramfs_check_survives_pipefail(self):
        # grep -q closing the pipe made lsinitramfs fail under pipefail and report LACKS falsely.
        self.assertNotRegex(SCRIPT, r'lsinitramfs[^\n]*\|\s*grep -q')
        for item in ('cryptsetup', 'dropbear', 'crypttab'):
            self.assertIn('initramfs has %s' % item, SCRIPT)
            self.assertIn('INITRAMFS LACKS %s' % item, SCRIPT)

    def test_preboot_unlock_is_restricted(self):
        self.assertIn('command=\\"/usr/bin/cryptroot-unlock\\"', SCRIPT)
        self.assertIn('no-port-forwarding', SCRIPT)

    def test_guide_names_the_script_and_its_safety_steps(self):
        self.assertIn('scripts/rescue-luks-install.sh', GUIDE)
        flat = ' '.join(GUIDE.split())
        for text in ('random root password', 'passwd -l root', 'Permission denied (publickey)',
                     'StrictHostKeyChecking yes', 'INITRAMFS LACKS', 'luksHeaderBackup',
                     'PermitRootLogin no', '(0, 0)', 'docker.asc', 'sslmode=verify-full',
                     'What this does not protect'):
            self.assertIn(text, flat, text)

    def test_guide_is_in_the_nav(self):
        self.assertIn('install/encrypted-hosts.md', (ROOT / 'mkdocs.yml').read_text())


if __name__ == '__main__':
    unittest.main()
