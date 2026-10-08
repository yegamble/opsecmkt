import os
import stat
import subprocess
import tempfile
import unittest
from pathlib import Path


REPOSITORY = Path(__file__).resolve().parents[1]
SCRIPT = REPOSITORY / "scripts" / "test-tor-upgrade-rollback.sh"
EXTRACTOR = REPOSITORY / "scripts" / "a144_source_archive.sh"


class TorArchivePermissionsTests(unittest.TestCase):
    def test_both_rehearsal_extraction_paths_preserve_public_source_modes(self):
        script = SCRIPT.read_text()
        self.assertEqual(script.count('extract_a144_source "$archive" "$deploy"'), 1)
        self.assertEqual(script.count('extract_a144_source "$old_archive" "$deploy"'), 1)

        with tempfile.TemporaryDirectory() as temporary:
            temp = Path(temporary)
            archive = temp / "source.tar"
            with archive.open("wb") as stream:
                subprocess.run(
                    ["git", "archive", "--format=tar", "HEAD", "web/templates"],
                    cwd=REPOSITORY,
                    check=True,
                    stdout=stream,
                )

            control = temp / "old-extract"
            control.mkdir(mode=0o700)
            subprocess.run(
                ["bash", "-c", 'umask 077; tar -xf "$1" -C "$2"', "tar-mode-control", str(archive), str(control)],
                check=True,
                cwd=REPOSITORY,
            )
            old_template = control / "web" / "templates" / "pages" / "orders.html"
            self.assertFalse(stat.S_IMODE(old_template.stat().st_mode) & stat.S_IROTH)
            self.assertFalse(stat.S_IMODE((control / "web").stat().st_mode) & stat.S_IXOTH)

            for label in ("initial", "replacement"):
                destination = temp / label
                destination.mkdir(mode=0o700)
                private_env = destination / ".env"
                private_env.write_text("TOKEN=not-for-container\n")
                private_env.chmod(0o600)
                subprocess.run(
                    [
                        "bash", "-c",
                        'set -euo pipefail; umask 077; source "$1"; extract_a144_source "$2" "$3"',
                        "tor-archive-test", str(EXTRACTOR), str(archive), str(destination),
                    ],
                    check=True,
                    cwd=REPOSITORY,
                )
                template = destination / "web" / "templates" / "pages" / "orders.html"
                self.assertTrue(template.is_file())
                self.assertTrue(stat.S_IMODE(template.stat().st_mode) & stat.S_IROTH)
                self.assertTrue(stat.S_IMODE((destination / "web").stat().st_mode) & stat.S_IXOTH)
                self.assertTrue(stat.S_IMODE((destination / "web" / "templates").stat().st_mode) & stat.S_IXOTH)
                self.assertEqual(stat.S_IMODE(private_env.stat().st_mode), 0o600)


if __name__ == "__main__":
    unittest.main()
