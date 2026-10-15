import hashlib
import io
import json
import os
import stat
import subprocess
import sys
import tarfile
import tempfile
import unittest
from pathlib import Path


REPOSITORY = Path(__file__).resolve().parents[1]
SCRIPT = REPOSITORY / "scripts" / "test-tor-upgrade-rollback.sh"
EXTRACTOR = REPOSITORY / "scripts" / "a144_source_archive.sh"
ATTRIBUTES = REPOSITORY / ".gitattributes"
PACKAGER = REPOSITORY / "scripts" / "package-deployment.py"
INSTALLER_SOURCE = Path("scripts") / "install.sh"


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


class SourceArchiveFixture:
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
            "web/templates/pages/orders.html", INSTALLER_SOURCE.as_posix(),
        )
        for name in self.excluded + self.included:
            path = self.repository / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("tracked fixture\n")
        (self.repository / INSTALLER_SOURCE).chmod(0o755)
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


class SourceArchiveExclusionsTests(SourceArchiveFixture, unittest.TestCase):
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
                self.assertTrue(stat.S_IMODE((destination / INSTALLER_SOURCE).stat().st_mode) & stat.S_IXOTH)
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


class DeploymentArchiveTests(SourceArchiveFixture, unittest.TestCase):
    def setUp(self):
        super().setUp()
        self.runtime = (
            ".dockerignore", ".env.example", "go.sum", "LICENSE", "compose.tor.yaml",
            "compose.clearnet.yaml", "compose.internal-db.yaml", "compose.nodes.yaml",
            "compose.nodes-tor.yaml", "compose.external-egress.yaml", "compose.scanner.yaml",
            "compose.admin-tor.yaml", "deploy/torrc-admin", "deploy/tor-admin-entrypoint.sh",
            "compose.escrow-xmr.yaml", "internal/market/app.go",
            "internal/market/schema.sql", "internal/market/migrations/001_fixture.sql",
            "deploy/.dockerignore", "deploy/Dockerfile.tor", "deploy/torrc", "deploy/torrc-socks", "deploy/clamd.conf",
            "scripts/backup.sh", "scripts/restore.sh", "scripts/postgres-tool.py",
            "scripts/postgres-policy.py", "scripts/tor_network_address.py",
            "docs/operator-guide.md", "docs/install/tor.md", "docs/install/clearnet.md",
            "docs/testnet-runbook.md", "docs/escrow.md", "SECURITY.md", "UPGRADING.md",
        )
        self.auxiliary = (
            "docs/evidence/public-testnet/2026-01-01-fixture/cloud-resource.json",
            "docs/evidence/sparrow/hand-off.json", "deploy/docs/cloudflare/wrangler.jsonc",
            "deploy/docs/Dockerfile", ".github/workflows/ci.yaml", ".codex/config.toml",
            ".agents/guides/AGENTS.md", "CLAUDE.md", "AGENTS.md", "tests/payment.json",
            "cmd/server/main_test.go", "internal/market/test_test.go", "scripts/test-install.py",
            "scripts/ci-smoke.sh", "scripts/mkdocs_hooks.py", "scripts/docs-onion.sh",
            "scripts/setup-local-chains.py", "mkdocs.yml", "requirements-docs.txt",
            "requirements-docs.in", "package.json", "package-lock.json", "playwright.config.ts",
            "playwright.escrow.config.ts", "deploy/testdata/clamav-test.ndb", "site/index.html",
            "internal/market/testdata/escrow-wallets/fixture.psbt", "scripts/escrow-wallets.py",
            "scripts/escrow-electrum-bridge.py",
            "security_best_practices_report.md", ".env", ".env.production",
            "private/hs_ed25519_secret_key", "backups/database.dump.age", "artifacts/report.json",
        )
        for name in self.runtime + self.auxiliary:
            path = self.repository / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("committed fixture: " + name + "\n")
        self.git("add", ".")
        self.git("-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "Deployment fixture")
        self.deployment_ref = self.git("rev-parse", "HEAD").strip()

    def run_packager(self, output, *arguments, check=True):
        return subprocess.run(
            [sys.executable, str(PACKAGER), "--repo", str(self.repository),
             "--ref", self.deployment_ref, "--output", str(output), *arguments],
            env=self.git_env, check=check, capture_output=True, text=True,
        )

    def test_deployment_bundle_excludes_auxiliary_files_and_keeps_operating_sources(self):
        # Uncommitted and untracked files, including private runtime files, never
        # change a committed deployment artifact. Keep truthful source timestamps.
        (self.repository / "go.mod").write_text("uncommitted source change\n")
        (self.repository / "local-private-file").write_text("untracked secret\n")
        (self.repository / ".gitattributes").write_text("go.mod export-ignore\n")
        self.git("config", "tar.umask", "0077")
        committed_at = int(self.git("show", "-s", "--format=%ct", self.deployment_ref))
        output = self.temp / "market-source.tar.gz"
        self.run_packager(output)
        with tarfile.open(output, "r:gz") as source:
            names = set(source.getnames())
            self.assertTrue(set(self.included + self.runtime).issubset(names))
            self.assertTrue(set(self.excluded + self.auxiliary).isdisjoint(names))
            self.assertNotIn("local-private-file", names)
            self.assertEqual(source.extractfile("go.mod").read(), b"tracked fixture\n")
            self.assertEqual(source.getmember(INSTALLER_SOURCE.as_posix()).mode, 0o755)
            self.assertEqual(source.getmember("go.mod").mode, 0o644)
            self.assertTrue(all(entry.mtime == committed_at for entry in source))
        self.assertTrue((self.repository / self.auxiliary[0]).is_file())
        # Existing public archives and historical rehearsal helpers keep their
        # broader behavior. They must not inherit optional deployment exclusions.
        with tarfile.open(self.archive(self.deployment_ref)) as source:
            public_names = set(source.getnames())
        self.assertIn(self.auxiliary[0], public_names)
        self.assertIn(".github/workflows/ci.yaml", public_names)
        self.assertIn("compose.clearnet.yaml", public_names)

    def test_manifest_and_checksums_describe_the_delivered_committed_files(self):
        first = self.temp / "market-source.tar.gz"
        second = self.temp / "second-source.tar.gz"
        self.run_packager(first)
        self.run_packager(second)
        self.assertEqual(first.read_bytes(), second.read_bytes())
        manifest_path = first.with_name(first.name + ".manifest.json")
        manifest = json.loads(manifest_path.read_text())
        self.assertEqual(manifest["source_commit"], self.deployment_ref)
        self.assertEqual(manifest["profile"], "standard")
        self.assertEqual(manifest_path.read_bytes(), second.with_name(second.name + ".manifest.json").read_bytes())
        # Omitted evidence filenames are not reintroduced through the inventory.
        self.assertNotIn("2026-01-01-fixture", manifest_path.read_text())
        with tarfile.open(first, "r:gz") as source:
            self.assertEqual({entry["path"] for entry in manifest["entries"]}, set(source.getnames()))
            for entry in manifest["entries"]:
                if entry["type"] == "file":
                    contents = source.extractfile(entry["path"]).read()
                    self.assertEqual(entry["sha256"], hashlib.sha256(contents).hexdigest())
                    self.assertEqual(entry["size"], len(contents))
        checksums = first.with_name(first.name + ".SHA256SUMS").read_text().splitlines()
        self.assertEqual(len(checksums), 2)
        for line in checksums:
            digest, name = line.split("  ", 1)
            self.assertEqual(digest, hashlib.sha256((self.temp / name).read_bytes()).hexdigest())

    def test_tor_only_profile_is_optional(self):
        output = self.temp / "tor-source.tar.gz"
        self.run_packager(output, "--tor-only")
        with tarfile.open(output, "r:gz") as source:
            names = set(source.getnames())
        self.assertNotIn("compose.clearnet.yaml", names)
        self.assertIn("compose.nodes-tor.yaml", names)
        self.assertIn("compose.tor.yaml", names)
        self.assertIn("docs/install/clearnet.md", names)
        manifest = json.loads(output.with_name(output.name + ".manifest.json").read_text())
        self.assertEqual(manifest["profile"], "tor-only")

    def test_missing_core_sources_and_existing_outputs_are_refused(self):
        output = self.temp / "market-source.tar.gz"
        output.write_bytes(b"existing artifact")
        result = self.run_packager(output, check=False)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(output.read_bytes(), b"existing artifact")
        self.assertFalse(output.with_name(output.name + ".manifest.json").exists())
        output.unlink()
        (self.repository / ".gitattributes").write_text("go.mod export-ignore\n")
        self.git("add", ".gitattributes")
        self.git("-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "Missing core source")
        self.deployment_ref = self.git("rev-parse", "HEAD").strip()
        result = self.run_packager(output, check=False)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Required deployment sources are missing: go.mod", result.stderr)
        self.assertFalse(output.exists())
        self.assertFalse(output.with_name(output.name + ".manifest.json").exists())


class DockerBuildContextHygieneTests(unittest.TestCase):
    def rules(self, relative):
        return {line.strip() for line in (REPOSITORY / relative).read_text().splitlines()
                if line.strip() and not line.lstrip().startswith("#")}

    def test_application_context_omits_proof_and_workflow_material(self):
        rules = self.rules(".dockerignore")
        self.assertTrue({"docs/evidence", "deploy/docs", "CLAUDE.md", "AGENTS.md",
                         "security_best_practices_report.md", ".codex", ".agents",
                         ".claude", ".ralph", ".github"}.issubset(rules))
        # Docker's COPY inputs and retained operating guides must remain available.
        self.assertTrue({"go.mod", "go.sum", "cmd", "internal", "web", "docs"}.isdisjoint(rules))

    def test_tor_context_has_its_own_exclusions_and_keeps_copy_inputs(self):
        rules = self.rules("deploy/.dockerignore")
        self.assertTrue({"docs", "testdata", ".wrangler", "**/.wrangler"}.issubset(rules))
        dockerfile = (REPOSITORY / "deploy/Dockerfile.tor").read_text()
        copies = [line.split()[1] for line in dockerfile.splitlines() if line.startswith("COPY ")]
        self.assertTrue(copies)
        for name in copies:
            with self.subTest(source=name):
                self.assertTrue((REPOSITORY / "deploy" / name).is_file())
                self.assertNotIn(name, rules)


if __name__ == "__main__":
    unittest.main()
