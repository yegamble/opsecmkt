#!/usr/bin/env python3
"""Offline backup-expiry regressions; only disposable synthetic archives are used."""
from datetime import datetime, timedelta, timezone
import importlib.util
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("backup_retention", ROOT / "scripts/backup-retention.py")
retention = importlib.util.module_from_spec(spec)
spec.loader.exec_module(retention)
NOW = datetime(2026, 10, 14, 12, 0, 0, tzinfo=timezone.utc)


class BackupRetentionTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="opsecmkt-backup-retention-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name).resolve()
        self.backups = self.root / "backups"
        self.copies = self.root / "copies"
        self.backups.mkdir(mode=0o700)
        self.copies.mkdir(mode=0o700)

    def archive(self, name="old", age=31, directory=None):
        target = (directory or self.backups) / (name + ".dump.age")
        target.write_bytes(retention.AGE_HEADER + ("synthetic encrypted payload " + name).encode())
        created = NOW - timedelta(days=age)
        retention.write_metadata(target, target, retention.format_time(created), NOW)
        return target

    def metadata(self, archive):
        return Path(str(archive) + retention.SIDECAR)

    def audit_pair(self, *, age=181, encrypted=False, upto=100):
        suffix = ".asc" if encrypted else ""
        data = self.backups / (f"audit-events-upto-{upto}.jsonl" + suffix)
        signature = self.backups / (f"audit-events-upto-{upto}.sig" + suffix)
        data.write_bytes((f"synthetic encrypted audit data {upto}" if encrypted else json.dumps({"id": upto, "action": "synthetic audit event"}) + "\n").encode())
        signature.write_bytes(b"synthetic encrypted detached signature" if encrypted else b"synthetic detached signature\n")
        files = [{"name": path.name, "sha256": hashlib.sha256(path.read_bytes()).hexdigest()} for path in (data, signature)]
        created = NOW - timedelta(days=age)
        self.metadata(data).write_text(json.dumps({"version": 1, "kind": "audit_export",
            "sha256": hashlib.sha256((files[0]["sha256"] + "\n" + files[1]["sha256"]).encode("ascii")).hexdigest(),
            "created_at": retention.format_time(created), "expires_at": retention.format_time(created + timedelta(days=180)), "files": files}))
        return data, signature

    def hold(self, archive, *, overdue=False, released=False):
        metadata = json.loads(self.metadata(archive).read_text())
        reviewed = NOW - timedelta(days=31 if overdue else 1)
        return {"case_id": "case-synthetic", "kind": metadata["kind"], "archive_path": str(archive), "sha256": metadata["sha256"],
                "created_at": metadata["created_at"], "expires_at": metadata["expires_at"],
                "reason": "Synthetic preservation duty", "owner": "Test owner", "authorized_readers": ["Test handler"],
                "permitted_use": "Case recovery only", "started_at": retention.format_time(NOW - timedelta(days=40)),
                "reviewed_at": retention.format_time(reviewed), "next_review_at": retention.format_time(reviewed + timedelta(days=30)),
                "release_condition": "Synthetic case closes", "released_at": retention.format_time(NOW) if released else None}

    def register(self, holds):
        path = self.root / "backup-preservations.json"
        path.write_text(json.dumps({"version": 1, "holds": holds}))
        path.chmod(0o600)
        return path

    def test_deadline_dry_run_then_expiry_at_exact_thirty_days(self):
        old = self.archive(age=30)
        recent = self.archive("recent", age=29)
        counts = retention.expire([self.backups], None, NOW)
        self.assertEqual(counts["expired"], 1)
        self.assertEqual(counts["deleted"], 0)
        self.assertTrue(old.exists())
        counts = retention.expire([self.backups], None, NOW, delete=True)
        self.assertEqual(counts["deleted"], 1)
        self.assertFalse(old.exists())
        self.assertFalse(self.metadata(old).exists())
        self.assertTrue(recent.exists())

    def test_copy_and_touch_do_not_reset_expiry(self):
        original = self.archive()
        copied = self.copies / "new-name.dump.age"
        shutil.copyfile(original, copied)
        shutil.copyfile(self.metadata(original), self.metadata(copied))
        os.utime(original, (NOW.timestamp(), NOW.timestamp()))
        os.utime(copied, (NOW.timestamp() + 100000, NOW.timestamp() + 100000))
        counts = retention.expire([self.backups, self.copies], None, NOW, delete=True)
        self.assertEqual(counts["deleted"], 2)
        self.assertFalse(original.exists())
        self.assertFalse(copied.exists())

    def test_preservation_applies_only_to_registered_copy(self):
        original = self.archive()
        copied = self.copies / "copy.dump.age"
        shutil.copyfile(original, copied)
        shutil.copyfile(self.metadata(original), self.metadata(copied))
        register = self.register([self.hold(original)])
        counts = retention.expire([self.backups, self.copies], register, NOW, delete=True)
        self.assertEqual(counts["held"], 1)
        self.assertEqual(counts["deleted"], 1)
        self.assertTrue(original.exists())
        self.assertFalse(copied.exists())

    def test_overdue_preservation_review_never_releases_copy(self):
        archive = self.archive()
        counts = retention.expire([self.backups], self.register([self.hold(archive, overdue=True)]), NOW, delete=True)
        self.assertEqual(counts["held_overdue_review"], 1)
        self.assertTrue(archive.exists())

    def test_released_hold_uses_original_deadline(self):
        archive = self.archive()
        counts = retention.expire([self.backups], self.register([self.hold(archive, released=True)]), NOW, delete=True)
        self.assertEqual(counts["deleted"], 1)
        self.assertFalse(archive.exists())

    def test_invalid_hold_prevents_all_deletion(self):
        archive = self.archive()
        hold = self.hold(archive)
        hold["reason"] = ""
        with self.assertRaises(ValueError):
            retention.expire([self.backups], self.register([hold]), NOW, delete=True)
        self.assertTrue(archive.exists())

    def test_active_hold_with_wrong_digest_refuses_every_deletion(self):
        ordinary = self.archive("aaa_unheld")
        held = self.archive("zzz_held")
        hold = self.hold(held)
        hold["sha256"] = "0" * 64
        with self.assertRaisesRegex(ValueError, "artifact kind or digest"):
            retention.expire([self.backups], self.register([hold]), NOW, delete=True)
        self.assertTrue(ordinary.exists())
        self.assertTrue(held.exists())

    def test_active_hold_with_wrong_kind_refuses_every_deletion(self):
        ordinary = self.archive("aaa_unheld")
        held = self.archive("zzz_held")
        hold = self.hold(held)
        hold["kind"] = "audit_export"
        hold["expires_at"] = retention.format_time(retention.timestamp(hold["created_at"]) + timedelta(days=180))
        with self.assertRaisesRegex(ValueError, "artifact kind or digest"):
            retention.expire([self.backups], self.register([hold]), NOW, delete=True)
        self.assertTrue(ordinary.exists())
        self.assertTrue(held.exists())

    def test_mismatched_hold_outside_scanned_locations_does_not_block_expiry(self):
        ordinary = self.archive("ordinary")
        outside = self.archive("outside")
        shutil.move(str(outside), self.copies / outside.name)
        shutil.move(str(self.metadata(outside)), self.copies / self.metadata(outside).name)
        hold = self.hold(self.copies / outside.name)
        hold["sha256"] = "0" * 64
        counts = retention.expire([self.backups], self.register([hold]), NOW, delete=True)
        self.assertEqual(counts["deleted"], 1)
        self.assertFalse(ordinary.exists())

    def test_hold_naming_managed_signature_or_metadata_cannot_be_silently_ignored(self):
        for secondary in ("signature", "metadata"):
            with self.subTest(secondary=secondary):
                data, signature = self.audit_pair(upto=200 if secondary == "signature" else 201)
                hold = self.hold(data)
                hold["archive_path"] = str(signature if secondary == "signature" else self.metadata(data))
                with self.assertRaisesRegex(ValueError, "must name the primary artifact"):
                    retention.expire([self.backups], self.register([hold]), NOW, delete=True)
                self.assertTrue(data.exists())
                self.assertTrue(signature.exists())

    def test_preservation_register_requires_private_permissions(self):
        archive = self.archive()
        register = self.register([self.hold(archive)])
        register.chmod(0o644)
        with self.assertRaises(ValueError):
            retention.expire([self.backups], register, NOW, delete=True)
        self.assertTrue(archive.exists())

    def test_hold_clocks_must_match_original_metadata(self):
        archive = self.archive()
        hold = self.hold(archive)
        hold["created_at"] = retention.format_time(NOW - timedelta(days=30))
        hold["expires_at"] = retention.format_time(NOW)
        with self.assertRaises(ValueError):
            retention.expire([self.backups], self.register([hold]), NOW, delete=True)
        self.assertTrue(archive.exists())

    def test_hold_symlink_alias_cannot_silently_miss_protected_copy(self):
        archive = self.archive()
        alias = self.root / "alias"
        alias.symlink_to(self.backups, target_is_directory=True)
        hold = self.hold(archive)
        hold["archive_path"] = str(alias / archive.name)
        with self.assertRaises(ValueError):
            retention.expire([self.backups], self.register([hold]), NOW, delete=True)
        self.assertTrue(archive.exists())

    def test_missing_sidecar_stops_scan_before_expired_archive_is_deleted(self):
        old = self.archive()
        unknown = self.archive("unknown")
        self.metadata(unknown).unlink()
        with self.assertRaises(ValueError):
            retention.expire([self.backups], None, NOW, delete=True)
        self.assertTrue(old.exists())
        self.assertTrue(unknown.exists())

    def test_corrupt_archive_stops_scan_before_any_deletion(self):
        old = self.archive("a-old")
        corrupt = self.archive("z-corrupt")
        corrupt.write_bytes(retention.AGE_HEADER + b"changed synthetic payload")
        with self.assertRaises(ValueError):
            retention.expire([self.backups], None, NOW, delete=True)
        self.assertTrue(old.exists())
        self.assertTrue(corrupt.exists())

    def test_same_archive_with_refreshed_metadata_is_refused(self):
        original = self.archive()
        copied = self.copies / "copy.dump.age"
        shutil.copyfile(original, copied)
        retention.write_metadata(copied, copied, retention.format_time(NOW), NOW)
        with self.assertRaises(ValueError):
            retention.expire([self.backups, self.copies], None, NOW, delete=True)
        self.assertTrue(original.exists())
        self.assertTrue(copied.exists())

    def test_wallet_and_recovery_material_are_never_deleted(self):
        archive = self.archive()
        material = [self.backups / "wallet.keys", self.backups / "wallet.dump", self.backups / "wallet-volume.tar.age"]
        for path in material:
            path.write_bytes(b"synthetic funds recovery material")
        counts = retention.expire([self.backups], None, NOW, delete=True)
        self.assertEqual(counts["deleted"], 1)
        self.assertFalse(archive.exists())
        self.assertTrue(all(path.exists() for path in material))

    def test_symlinked_archive_and_directory_are_refused(self):
        original = self.archive()
        link = self.copies / "copy.dump.age"
        link.symlink_to(original)
        shutil.copyfile(self.metadata(original), self.metadata(link))
        with self.assertRaises(ValueError):
            retention.expire([self.backups, self.copies], None, NOW, delete=True)
        self.assertTrue(original.exists())
        link.unlink()
        self.metadata(link).unlink()
        (self.copies / "linked-directory").symlink_to(self.backups, target_is_directory=True)
        with self.assertRaises(ValueError):
            retention.expire([self.copies], None, NOW, delete=True)
        self.assertTrue(original.exists())

    def test_sidecar_cannot_be_overwritten_to_refresh_clock(self):
        archive = self.archive()
        original = self.metadata(archive).read_bytes()
        with self.assertRaises(FileExistsError):
            retention.write_metadata(archive, archive, retention.format_time(NOW), NOW)
        self.assertEqual(self.metadata(archive).read_bytes(), original)
        self.assertEqual(list(self.backups.glob("*.tmp.*")), [])

    def test_unbounded_or_duplicate_metadata_is_refused(self):
        archive = self.archive()
        metadata = json.loads(self.metadata(archive).read_text())
        metadata["expires_at"] = retention.format_time(NOW + timedelta(days=365))
        self.metadata(archive).write_text(json.dumps(metadata))
        with self.assertRaises(ValueError):
            retention.expire([self.backups], None, NOW, delete=True)
        self.metadata(archive).write_text('{"version": 1, "version": 2}')
        with self.assertRaises(ValueError):
            retention.expire([self.backups], None, NOW, delete=True)
        self.assertTrue(archive.exists())

    def test_destructive_cli_requires_explicit_preservation_register(self):
        archive = self.archive()
        result = subprocess.run(["python3", str(ROOT / "scripts/backup-retention.py"), "expire", str(self.backups), "--delete"],
                                text=True, capture_output=True, timeout=10)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("requires --holds", result.stderr)
        self.assertTrue(archive.exists())

    def test_audit_data_and_signature_expire_together_at_earliest_event_deadline(self):
        data, signature = self.audit_pair(age=180)
        recent, recent_signature = self.audit_pair(age=179, upto=101)
        os.utime(data, (NOW.timestamp(), NOW.timestamp()))
        counts = retention.expire([self.backups], None, NOW, delete=True)
        self.assertEqual(counts["deleted"], 1)
        self.assertFalse(data.exists())
        self.assertFalse(signature.exists())
        self.assertFalse(self.metadata(data).exists())
        self.assertTrue(recent.exists())
        self.assertTrue(recent_signature.exists())

    def test_encrypted_audit_pair_inherits_original_deadline(self):
        data, signature = self.audit_pair(encrypted=True)
        counts = retention.expire([self.backups], None, NOW, delete=True)
        self.assertEqual(counts["deleted"], 1)
        self.assertFalse(data.exists())
        self.assertFalse(signature.exists())

    def test_audit_copies_keep_original_clocks(self):
        data, signature = self.audit_pair()
        for path in (data, signature, self.metadata(data)):
            copied = self.copies / path.name
            shutil.copyfile(path, copied)
            os.utime(copied, (NOW.timestamp(), NOW.timestamp()))
        counts = retention.expire([self.backups, self.copies], None, NOW, delete=True)
        self.assertEqual(counts["deleted"], 2)
        self.assertEqual(list(self.copies.iterdir()), [])

    def test_missing_audit_signature_refuses_every_deletion(self):
        old = self.archive()
        data, signature = self.audit_pair()
        signature.unlink()
        with self.assertRaises(OSError):
            retention.expire([self.backups], None, NOW, delete=True)
        self.assertTrue(old.exists())
        self.assertTrue(data.exists())

    def test_changed_audit_signature_refuses_every_deletion(self):
        old = self.archive()
        data, signature = self.audit_pair()
        signature.write_bytes(b"changed signature")
        with self.assertRaises(ValueError):
            retention.expire([self.backups], None, NOW, delete=True)
        self.assertTrue(old.exists())
        self.assertTrue(data.exists())
        self.assertTrue(signature.exists())

    def test_audit_pair_cannot_target_other_files_or_extend_deadline(self):
        data, signature = self.audit_pair()
        metadata = json.loads(self.metadata(data).read_text())
        metadata["files"][1]["name"] = "../wallet.keys"
        self.metadata(data).write_text(json.dumps(metadata))
        with self.assertRaises(ValueError):
            retention.expire([self.backups], None, NOW, delete=True)
        metadata["files"][1]["name"] = signature.name
        metadata["expires_at"] = retention.format_time(NOW + timedelta(days=180))
        self.metadata(data).write_text(json.dumps(metadata))
        with self.assertRaises(ValueError):
            retention.expire([self.backups], None, NOW, delete=True)
        self.assertTrue(data.exists())
        self.assertTrue(signature.exists())

    def test_audit_hold_preserves_both_files_only(self):
        old = self.archive()
        data, signature = self.audit_pair()
        register = self.register([self.hold(data)])
        counts = retention.expire([self.backups], register, NOW, delete=True)
        self.assertEqual(counts["held"], 1)
        self.assertEqual(counts["deleted"], 1)
        self.assertFalse(old.exists())
        self.assertTrue(data.exists())
        self.assertTrue(signature.exists())

    def test_released_audit_hold_uses_original_event_deadline(self):
        data, signature = self.audit_pair()
        counts = retention.expire([self.backups], self.register([self.hold(data, released=True)]), NOW, delete=True)
        self.assertEqual(counts["deleted"], 1)
        self.assertFalse(data.exists())
        self.assertFalse(signature.exists())


class BackupPublicationTests(unittest.TestCase):
    """Exercise the actual backup pipeline with offline age/client substitutes."""
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="opsecmkt-backup-publication-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name).resolve()
        self.bin = self.root / "bin"
        self.bin.mkdir()
        self.environment = {**os.environ, "PATH": str(self.bin) + os.pathsep + os.environ["PATH"],
                            "AGE_RECIPIENT": "synthetic-public-recipient", "BACKUP_DATABASE_URL": "postgres://test@127.0.0.1/test?sslmode=disable"}
        self.environment.pop("BACKUP_INTERNAL_DATABASE", None)
        self.environment.pop("BACKUP_RECOVERY_SOURCE", None)
        self.executable("psql", """import sys
if '--file=-' in sys.argv:
    sys.stdin.read()
    print('false')
else:
    print(1024)
""")
        self.executable("pg_dump", """import os, pathlib, sys
if '--version' in sys.argv:
    print('pg_dump (PostgreSQL) 17.6')
else:
    if os.environ.get('RACE_OUTPUT'):
        pathlib.Path(os.environ['RACE_OUTPUT']).write_bytes(b'concurrent archive')
    sys.stdout.buffer.write(b'synthetic database dump')
""")
        self.executable("age", """import sys
sys.stdout.buffer.write(b'age-encryption.org/v1\\n' + sys.stdin.buffer.read())
""")

    def executable(self, name, code):
        target = self.bin / name
        target.write_text("#!/usr/bin/env python3\n" + code + "\n")
        target.chmod(0o700)

    def run_backup(self, path, **extra):
        return subprocess.run(["bash", str(ROOT / "scripts/backup.sh"), str(path)], env={**self.environment, **extra},
                              text=True, capture_output=True, timeout=20)

    def test_publishes_archive_and_bound_original_clock_together(self):
        archive = self.root / "test.dump.age"
        result = self.run_backup(archive)
        self.assertEqual(result.returncode, 0, result.stderr)
        metadata, _, _ = retention.metadata_for(archive, datetime.now(timezone.utc))
        self.assertEqual(retention.timestamp(metadata["expires_at"]) - retention.timestamp(metadata["created_at"]), timedelta(days=30))
        self.assertEqual(archive.stat().st_mode & 0o777, 0o600)
        self.assertEqual(Path(str(archive) + retention.SIDECAR).stat().st_mode & 0o777, 0o600)
        self.assertNotIn("postgres://", result.stdout + result.stderr)
        self.assertEqual(list(self.root.glob("*.tmp.*")), [])

    def test_failed_encryption_leaves_no_archive_or_sidecar(self):
        self.executable("age", "raise SystemExit(1)")
        archive = self.root / "test.dump.age"
        result = self.run_backup(archive)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(list(self.root.glob("test.dump.age*")), [])

    def test_concurrent_publication_never_overwrites_archive(self):
        archive = self.root / "test.dump.age"
        result = self.run_backup(archive, RACE_OUTPUT=str(archive))
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(archive.read_bytes(), b"concurrent archive")
        self.assertFalse(Path(str(archive) + retention.SIDECAR).exists())
        self.assertEqual(list(self.root.glob("*.tmp.*")), [])

    def test_derived_recovery_copy_inherits_exact_original_deadline(self):
        original = self.root / "original.dump.age"
        original.write_bytes(retention.AGE_HEADER + b"old encrypted original")
        created = retention.format_time(datetime.now(timezone.utc) - timedelta(days=45))
        retention.write_metadata(original, original, created, datetime.now(timezone.utc))
        output = self.root / "converted.dump.age"
        result = self.run_backup(output, BACKUP_RECOVERY_SOURCE=str(original))
        self.assertEqual(result.returncode, 0, result.stderr)
        before, _, _ = retention.metadata_for(original, datetime.now(timezone.utc))
        after, _, _ = retention.metadata_for(output, datetime.now(timezone.utc))
        self.assertEqual((after["created_at"], after["expires_at"]), (before["created_at"], before["expires_at"]))
        self.assertNotEqual(after["sha256"], before["sha256"])
        self.assertLess(retention.timestamp(after["expires_at"]), datetime.now(timezone.utc))
        self.assertIn("no new retention period", result.stdout)

    def test_derived_recovery_copy_refuses_missing_or_corrupt_original_metadata(self):
        for corrupt in (False, True):
            with self.subTest(corrupt=corrupt):
                original = self.root / ("corrupt.dump.age" if corrupt else "unregistered.dump.age")
                original.write_bytes(retention.AGE_HEADER + b"synthetic original")
                if corrupt:
                    retention.write_metadata(original, original, retention.format_time(datetime.now(timezone.utc)), datetime.now(timezone.utc))
                    original.write_bytes(retention.AGE_HEADER + b"tampered original")
                output = self.root / "refused-copy.dump.age"
                result = self.run_backup(output, BACKUP_RECOVERY_SOURCE=str(original))
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(list(self.root.glob("refused-copy.dump.age*")), [])


if __name__ == "__main__":
    unittest.main()
