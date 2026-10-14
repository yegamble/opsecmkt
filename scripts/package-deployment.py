#!/usr/bin/env python3
"""Package a committed source tree for deployment, with an inventory and checksums.

This optional bundle leaves the public repository, release archives and upgrade
rehearsals unchanged. Its exclusions reduce auxiliary deployment files; they do
not anonymize source code or remove identifiers from the guides that are kept.
"""

import argparse
import gzip
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import subprocess
import sys
import tarfile
import tempfile


EXCLUDED_DIRECTORIES = frozenset({
    ".git", ".github", ".claude", ".ralph", ".codex", ".agents", ".wrangler",
    "design-reference", "node_modules", "__pycache__", ".venv-docs",
    "backups", "artifacts", "playwright-report", "playwright-wallet-report",
    "test-results", "test-results-wallet",
})
EXCLUDED_TREES = ("docs/evidence", "deploy/docs", "deploy/testdata", "internal/market/testdata", "tests", "site")
EXCLUDED_FILES = frozenset({
    ".ralphrc", "CLAUDE.md", "AGENTS.md", "security_best_practices_report.md",
    "package.json", "package-lock.json", "mkdocs.yml", "requirements-docs.in",
    "requirements-docs.txt", "scripts/mkdocs_hooks.py", "scripts/docs-onion.sh",
    "scripts/ci-smoke.sh", "scripts/setup-local-chains.py", "scripts/escrow-wallets.py",
    "scripts/escrow-electrum-bridge.py",
})
INSTALLER_SOURCE = PurePosixPath("scripts") / "install.sh"
# Fail before publishing a bundle if committed export rules remove core sources.
# The full inventory remains available for reviewing the other included files.
REQUIRED_FILES = frozenset({
    "Dockerfile", ".dockerignore", ".env.example", "go.mod", "go.sum", "LICENSE",
    "compose.yaml", "compose.tor.yaml", "cmd/server/main.go",
    "cmd/escrow-signer/main.go", "internal/market/schema.sql", INSTALLER_SOURCE.as_posix(),
    "scripts/backup.sh", "scripts/restore.sh", "scripts/postgres-tool.py",
    "scripts/postgres-policy.py", "deploy/Dockerfile.tor", "deploy/torrc",
})
REQUIRED_TREES = ("internal/market/migrations", "web/templates")


def excluded(name, tor_only=False):
    path = PurePosixPath(name)
    if any(part in EXCLUDED_DIRECTORIES for part in path.parts):
        return True
    if any(name == tree or name.startswith(tree + "/") for tree in EXCLUDED_TREES):
        return True
    if name in EXCLUDED_FILES or path.name in {"CLAUDE.md", "AGENTS.md", ".ralphrc"}:
        return True
    if name.startswith("scripts/test-") or name.startswith("playwright."):
        return True
    if path.name.endswith("_test.go"):
        return True
    if path.name.startswith(".env") and path.name != ".env.example":
        return True
    if path.name.startswith("hs_ed25519_") or path.suffix in {".dump", ".age"}:
        return True
    return tor_only and name == "compose.clearnet.yaml"


def git(repository, *arguments):
    return subprocess.check_output(["git", "-C", str(repository), *arguments], text=True).strip()


def digest_file(path):
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def checked_name(name):
    path = PurePosixPath(name)
    if path.is_absolute() or ".." in path.parts or not path.parts:
        raise ValueError("Unsafe source archive path: " + name)
    return path.as_posix()


def package(repository, ref, output, tor_only=False):
    revision = git(repository, "rev-parse", "--verify", "--end-of-options", ref + "^{commit}")
    committed_at = int(git(repository, "show", "-s", "--format=%ct", revision))
    manifest_path = output.with_name(output.name + ".manifest.json")
    checksum_path = output.with_name(output.name + ".SHA256SUMS")
    destinations = (output, manifest_path, checksum_path)
    if any(path.exists() or path.is_symlink() for path in destinations):
        raise ValueError("An output already exists; choose a new --output path")
    if any("\n" in path.name or "\r" in path.name or "\\" in path.name for path in destinations):
        raise ValueError("Output filenames must not contain newlines or backslashes")
    if not output.parent.is_dir():
        raise ValueError("The output directory must already exist")

    with tempfile.TemporaryDirectory(prefix="deployment-package-", dir=output.parent) as directory:
        temporary = Path(directory)
        source_path = temporary / "source.tar"
        with source_path.open("wb") as stream:
            # Keep the Git executable bit and public container readability even
            # when the operator has configured a private Git tar.umask.
            subprocess.run(["git", "-C", str(repository), "-c", "tar.umask=0022",
                            "archive", "--format=tar", revision],
                           check=True, stdout=stream)
        inventory = []
        files = set()
        archive_path = temporary / output.name
        with tarfile.open(source_path, "r:") as source, archive_path.open("wb") as raw:
            # Git's committed source timestamp is kept in the tar members and gzip
            # header. No author names, host paths or invocation time are added.
            with gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=committed_at) as compressed:
                with tarfile.open(fileobj=compressed, mode="w", format=tarfile.PAX_FORMAT) as archive:
                    for entry in source:
                        name = checked_name(entry.name)
                        if excluded(name, tor_only):
                            continue
                        if not (entry.isfile() or entry.isdir() or entry.issym()):
                            raise ValueError("Unsupported source archive entry: " + name)
                        record = {"path": name, "mode": format(entry.mode, "04o"), "mtime": entry.mtime}
                        if entry.isfile():
                            record.update(type="file", size=entry.size)
                            digest = hashlib.sha256()
                            with source.extractfile(entry) as contents:
                                for block in iter(lambda: contents.read(1024 * 1024), b""):
                                    digest.update(block)
                            record["sha256"] = digest.hexdigest()
                            with source.extractfile(entry) as contents:
                                archive.addfile(entry, contents)
                            files.add(name)
                        elif entry.issym():
                            target = PurePosixPath(name).parent / entry.linkname
                            if PurePosixPath(entry.linkname).is_absolute() or ".." in target.parts:
                                raise ValueError("Symlink leaves the source tree: " + name)
                            if excluded(target.as_posix(), tor_only):
                                raise ValueError("Symlink targets an excluded source path: " + name)
                            record.update(type="symlink", target=entry.linkname)
                            archive.addfile(entry)
                        else:
                            record["type"] = "directory"
                            archive.addfile(entry)
                        inventory.append(record)
        required = REQUIRED_FILES | ({"compose.clearnet.yaml"} if not tor_only else set())
        missing = sorted(required - files)
        missing.extend(tree + "/" for tree in REQUIRED_TREES
                       if not any(name.startswith(tree + "/") for name in files))
        if missing:
            raise ValueError("Required deployment sources are missing: " + ", ".join(missing))

        manifest = {
            "format": "opsecmkt-deployment-source-v1",
            "source_commit": revision,
            "source_commit_timestamp": committed_at,
            "profile": "tor-only" if tor_only else "standard",
            "exclusions": {
                "directory_names": sorted(EXCLUDED_DIRECTORIES),
                "trees": list(EXCLUDED_TREES),
                "files": sorted(EXCLUDED_FILES),
                "patterns": ["scripts/test-*", "playwright.*", "*_test.go", ".env* (except .env.example)",
                             "hs_ed25519_*", "*.dump", "*.age"],
                "tor_only": ["compose.clearnet.yaml"] if tor_only else [],
            },
            "scope": "Committed sources only, with the selected commit's export-ignore rules. "
                     "Public source and included guides retain identifiers and truthful timestamps. "
                     "Omitted evidence remains in the public repository; this is not anonymization.",
            "entries": inventory,
        }
        temporary_manifest = temporary / manifest_path.name
        temporary_manifest.write_text(json.dumps(manifest, indent=2, sort_keys=True) + "\n", encoding="utf-8")
        temporary_checksums = temporary / checksum_path.name
        temporary_checksums.write_text(
            "".join(digest_file(path) + "  " + path.name + "\n"
                    for path in (archive_path, temporary_manifest)), encoding="utf-8")
        # Exclusive publication refuses to replace an existing artifact, including
        # one created by another invocation after the initial existence check.
        published = []
        try:
            for path, destination in zip((archive_path, temporary_manifest, temporary_checksums), destinations):
                os.link(path, destination)
                published.append(destination)
        except OSError:
            for destination in published:
                destination.unlink()
            raise
    return destinations


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo", type=Path, default=Path(__file__).resolve().parents[1],
                        help="source Git repository (default: this script's repository)")
    parser.add_argument("--ref", default="HEAD", help="committed revision to package (default: HEAD)")
    parser.add_argument("--output", type=Path, required=True, help="new .tar.gz output path")
    parser.add_argument("--tor-only", action="store_true", help="also omit compose.clearnet.yaml")
    arguments = parser.parse_args()
    if not arguments.output.name.endswith(".tar.gz"):
        parser.error("--output must end in .tar.gz")
    try:
        paths = package(arguments.repo, arguments.ref, arguments.output.absolute(), arguments.tor_only)
    except (OSError, ValueError, subprocess.CalledProcessError) as error:
        parser.exit(1, "Packaging failed: " + str(error) + "\n")
    for path in paths:
        print(path)


if __name__ == "__main__":
    main()
