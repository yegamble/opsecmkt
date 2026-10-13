import io
import os
import stat
import subprocess
import tarfile
import tempfile
import unittest
from pathlib import Path


REPOSITORY = Path(__file__).resolve().parents[1]
SCRIPT = REPOSITORY / "scripts" / "test-tor-upgrade-rollback.sh"
EXTRACTOR = REPOSITORY / "scripts" / "a144_source_archive.sh"
ATTRIBUTES = REPOSITORY / ".gitattributes"


class TorArchivePermissionsTests(unittest.TestCase):
    def test_both_rehearsal_extraction_paths_preserve_public_source_modes(self):
        script = SCRIPT.read_text()
        self.assertEqual(script.count('extract_a144_source "$archive" "$deploy"'), 1)
        self.assertEqual(script.count('extract_a144_source "$old_archive" "$deploy"'), 1)

        with tempfile.TemporaryDirectory() as temporary:
            temp = Path(temporary)
            archive = temp / "source.tar"
            # Test the archive's public modes without relying on this checkout having Git history.
            # Default tar permissions differ for root, so assert the helper's required outcome directly.
            with tarfile.open(archive, "w") as source:
                for directory in ("web", "web/templates", "web/templates/pages"):
                    entry = tarfile.TarInfo(directory)
                    entry.type = tarfile.DIRTYPE
                    entry.mode = 0o755
                    source.addfile(entry)
                contents = b"template fixture\n"
                entry = tarfile.TarInfo("web/templates/pages/orders.html")
                entry.mode = 0o644
                entry.size = len(contents)
                source.addfile(entry, io.BytesIO(contents))

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


class SourceArchiveExclusionsTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.temp = Path(self.temporary.name)
        self.repository = self.temp / "repository"
        self.repository.mkdir()
        self.git_env = dict(os.environ, GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL=os.devnull,
                            GIT_ATTR_NOSYSTEM="1")
        self.git("init", "--quiet")
        self.git("config", "user.name", "Source archive test")
        self.git("config", "user.email", "archive@example.invalid")
        self.excluded = (
            ".claude/war-room/ledger.md",
            ".claude/settings.json",
            ".ralph/fix_plan.md",
            ".ralphrc",
            "design-reference/index.css",
            "tests/test_claude_review_hook.py",
            ".wrangler/state/cache.sqlite",
            "deploy/docs/cloudflare/.wrangler/state/v3/trace.sqlite-wal",
        )
        self.included = (
            "go.mod", "cmd/server/main.go", "compose.yaml", "Dockerfile",
            "web/templates/pages/orders.html", "scripts/install.sh",
        )
        for name in self.excluded + self.included:
            path = self.repository / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("tracked fixture\n")
        (self.repository / "scripts/install.sh").chmod(0o755)
        self.git("add", ".")
        self.git("-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "Historical source")
        self.historical_ref = self.git("rev-parse", "HEAD").strip()
        (self.repository / ".gitattributes").write_text(ATTRIBUTES.read_text())
        self.git("add", ".gitattributes")
        self.git("-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "Release exclusions")
        self.release_ref = self.git("rev-parse", "HEAD").strip()

    def git(self, *arguments):
        return subprocess.run(["git", *arguments], cwd=self.repository, env=self.git_env,
                              check=True, capture_output=True, text=True).stdout

    def archive(self, ref):
        archive = self.temp / (ref + ".tar")
        with archive.open("wb") as stream:
            subprocess.run(["git", "archive", "--format=tar", ref], cwd=self.repository,
                           env=self.git_env, check=True, stdout=stream)
        return archive

    def test_git_archives_use_committed_export_rules_and_keep_runtime_sources(self):
        # A local edit must not change the release selected by its commit.
        (self.repository / ".gitattributes").write_text("go.mod export-ignore\n")
        with tarfile.open(self.archive(self.release_ref)) as source:
            names = set(source.getnames())
        self.assertTrue(set(self.included).issubset(names))
        self.assertTrue(set(self.excluded).isdisjoint(names))
        self.assertFalse(any(part in (".claude", ".ralph", "design-reference", ".wrangler")
                             for name in names for part in Path(name).parts))

    def test_historical_archives_are_filtered_and_preserve_source_modes(self):
        archive = self.archive(self.historical_ref)
        with tarfile.open(archive) as source:
            self.assertTrue(set(self.excluded).issubset(source.getnames()))

        for stream_input in (False, True):
            with self.subTest(stream_input=stream_input):
                destination = self.temp / ("stream" if stream_input else "file")
                destination.mkdir(mode=0o700)
                private_env = destination / ".env"
                private_env.write_text("TOKEN=private-test-value\n")
                private_env.chmod(0o600)
                with archive.open("rb") as stream:
                    subprocess.run(
                        ["bash", "-c",
                         'set -euo pipefail; umask 077; source "$1"; extract_a144_source "$2" "$3"',
                         "archive-exclusion-test", str(EXTRACTOR),
                         "-" if stream_input else str(archive), str(destination)],
                        cwd=self.repository, check=True, stdin=stream if stream_input else None,
                    )
                self.assertTrue(all((destination / name).is_file() for name in self.included))
                self.assertFalse(any((destination / name).exists() for name in self.excluded))
                self.assertTrue(stat.S_IMODE((destination / "scripts/install.sh").stat().st_mode) & stat.S_IXOTH)
                self.assertTrue(stat.S_IMODE((destination / "web/templates/pages/orders.html").stat().st_mode) & stat.S_IROTH)
                self.assertTrue(stat.S_IMODE((destination / "web/templates").stat().st_mode) & stat.S_IXOTH)
                self.assertEqual(stat.S_IMODE(private_env.stat().st_mode), 0o600)

    def test_upgrade_consumers_use_the_filtered_extractor(self):
        for name, invocation in (
            ("test-upgrade.sh", 'extract_a144_source - "$work/old"'),
            ("test-intermediate-upgrade.sh", 'extract_a144_source - "$old_source"'),
        ):
            with self.subTest(script=name):
                script = (REPOSITORY / "scripts" / name).read_text()
                self.assertIn('source "$root/scripts/a144_source_archive.sh"', script)
                self.assertIn(invocation, script)


if __name__ == "__main__":
    unittest.main()
