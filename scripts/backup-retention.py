#!/usr/bin/env python3
"""Expire registered PostgreSQL archives and issued audit pairs by original clocks.

Only .dump.age archives with matching .retention.json sidecars are managed. Copy
both files together; copying/touching an archive never restarts its deadline.
Unknown archives, mismatched copies, symlinks, malformed metadata or an invalid
preservation register stop the entire scan before deletion. Issued audit JSONL
and signature pairs expire together 180 days after their earliest event, even
when recipient encrypted. Wallet files and
other recovery material are deliberately outside this tool's deletion scope.
"""
import argparse
from datetime import datetime, timedelta, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import sys
import tempfile


AGE_HEADER = b"age-encryption.org/v1\n"
SIDECAR = ".retention.json"
RETENTION_DAYS = 30
RETENTION_PERIODS = {"postgresql_dump": RETENTION_DAYS, "audit_export": 180}
AUDIT_NAME = r"audit-events-(?:from-[0-9]+-)?upto-[0-9]+"
MAX_JSON_BYTES = 4 * 1024 * 1024
METADATA_FIELDS = {"version", "kind", "sha256", "created_at", "expires_at"}
HOLD_FIELDS = {"case_id", "kind", "archive_path", "sha256", "created_at", "expires_at", "reason", "owner", "authorized_readers",
               "permitted_use", "started_at", "reviewed_at", "next_review_at", "release_condition", "released_at"}


def timestamp(value):
    if not isinstance(value, str) or not re.fullmatch(r"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z", value):
        raise ValueError("timestamps must use YYYY-MM-DDTHH:MM:SSZ")
    parsed = datetime.strptime(value, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc)
    if parsed.year < 1970:
        raise ValueError("timestamps must be on or after 1970-01-01")
    return parsed


def format_time(value):
    return value.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def absolute(path):
    return Path(os.path.abspath(path))


def regular_file(path):
    info = path.lstat()
    if not stat.S_ISREG(info.st_mode):
        raise ValueError(f"refusing non-regular file: {path}")
    return info


def fingerprint(info):
    return (info.st_dev, info.st_ino, info.st_size, info.st_mtime_ns)


def read_json(path, private=False):
    info = regular_file(path)
    if info.st_size > MAX_JSON_BYTES:
        raise ValueError(f"JSON file too large: {path}")
    if private and stat.S_IMODE(info.st_mode) & 0o077:
        raise ValueError(f"preservation register must have restricted permissions (0600): {path}")
    # Duplicate keys must not allow one reader to interpret another deadline/hold.
    def object_pairs(pairs):
        result = {}
        for key, value in pairs:
            if key in result:
                raise ValueError(f"duplicate JSON key in {path}")
            result[key] = value
        return result
    descriptor = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
    with os.fdopen(descriptor, "r", encoding="utf-8") as stream:
        if fingerprint(os.fstat(stream.fileno())) != fingerprint(info):
            raise ValueError(f"JSON changed during validation: {path}")
        data = json.load(stream, object_pairs_hook=object_pairs)
        if fingerprint(os.fstat(stream.fileno())) != fingerprint(info):
            raise ValueError(f"JSON changed during validation: {path}")
    return data, fingerprint(info)


def file_digest(path, required_header=None):
    info = regular_file(path)
    digest = hashlib.sha256()
    # O_NOFOLLOW also protects the open against replacing the file with a symlink.
    descriptor = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
    with os.fdopen(descriptor, "rb") as stream:
        if fingerprint(os.fstat(stream.fileno())) != fingerprint(info):
            raise ValueError(f"archive changed during validation: {path}")
        if required_header is not None:
            header = stream.read(len(required_header))
            if header != required_header:
                raise ValueError(f"not an age-encrypted archive: {path}")
            digest.update(header)
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(block)
        if fingerprint(os.fstat(stream.fileno())) != fingerprint(info):
            raise ValueError(f"archive changed during validation: {path}")
    return digest.hexdigest(), fingerprint(info)


def archive_digest(path):
    return file_digest(path, AGE_HEADER)


def is_audit_file(name, extension="(?:jsonl|sig)"):
    return re.fullmatch(AUDIT_NAME + r"\." + extension + r"(?:\.asc)?", name) is not None


def valid_digest(value):
    return isinstance(value, str) and re.fullmatch(r"[0-9a-f]{64}", value)


def artifact_metadata(primary, now):
    metadata_path = Path(str(primary) + SIDECAR)
    metadata, metadata_fingerprint = read_json(metadata_path)
    if not isinstance(metadata, dict) or not isinstance(metadata.get("kind"), str) or metadata["kind"] not in RETENTION_PERIODS:
        raise ValueError(f"invalid archive metadata: {metadata_path}")
    kind = metadata["kind"]
    if set(metadata) != METADATA_FIELDS | ({"files"} if kind == "audit_export" else set()):
        raise ValueError(f"invalid archive metadata: {metadata_path}")
    if type(metadata["version"]) is not int or metadata["version"] != 1:
        raise ValueError(f"unsupported archive metadata: {metadata_path}")
    created = timestamp(metadata["created_at"])
    expiry = timestamp(metadata["expires_at"])
    if created > now or expiry != created + timedelta(days=RETENTION_PERIODS[kind]):
        raise ValueError(f"invalid original archive deadline: {metadata_path}")
    if not valid_digest(metadata["sha256"]):
        raise ValueError(f"invalid archive digest: {metadata_path}")
    if kind == "postgresql_dump":
        if not primary.name.endswith(".dump.age"):
            raise ValueError("PostgreSQL archive metadata must belong to .dump.age")
        digest, archive_fingerprint = archive_digest(primary)
        files = [(primary, archive_fingerprint)]
    else:
        if not is_audit_file(primary.name, "jsonl"):
            raise ValueError("audit metadata must belong to its issued audit-events JSONL file")
        signature = primary.name.replace(".jsonl", ".sig")
        records = metadata["files"]
        if not isinstance(records, list) or len(records) != 2:
            raise ValueError("audit metadata must list its original data/signature pair")
        files, digests = [], []
        for record, name in zip(records, (primary.name, signature)):
            if not isinstance(record, dict) or set(record) != {"name", "sha256"} or record["name"] != name or not valid_digest(record["sha256"]):
                raise ValueError("audit metadata must match the issued data/signature basenames")
            path = primary.parent / name
            file_hash, file_fingerprint = file_digest(path)
            if record["sha256"] != file_hash:
                raise ValueError(f"audit pair does not match its metadata: {path}")
            digests.append(file_hash)
            files.append((path, file_fingerprint))
        digest = hashlib.sha256((digests[0] + "\n" + digests[1]).encode("ascii")).hexdigest()
    if digest != metadata["sha256"]:
        raise ValueError(f"artifact does not match its metadata: {primary}")
    return metadata, files, metadata_fingerprint


def metadata_for(archive, now):
    metadata, files, metadata_fingerprint = artifact_metadata(archive, now)
    return metadata, files[0][1], metadata_fingerprint


def recovery_copy_clock(original, now):
    """A conversion/re-encryption copy inherits its validated original deadline."""
    metadata, _, _ = metadata_for(absolute(original), now)
    if metadata["kind"] != "postgresql_dump":
        raise ValueError("recovery copy source must be a registered encrypted PostgreSQL archive")
    return metadata["created_at"]


def write_metadata(source, output, created_at, now):
    output = absolute(output)
    if not output.name.endswith(".dump.age"):
        raise ValueError("managed backup names must end in .dump.age")
    created = timestamp(created_at)
    if created > now:
        raise ValueError("original creation cannot be in the future")
    digest, _ = archive_digest(absolute(source))
    metadata = {"version": 1, "kind": "postgresql_dump", "sha256": digest,
                "created_at": format_time(created), "expires_at": format_time(created + timedelta(days=RETENTION_DAYS))}
    target = Path(str(output) + SIDECAR)
    descriptor, temporary = tempfile.mkstemp(prefix=target.name + ".tmp.", dir=target.parent)
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
            json.dump(metadata, stream, sort_keys=True)
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
        # Exclusive publication; never overwrite an older original clock.
        os.link(temporary, target)
    finally:
        os.unlink(temporary)
    return metadata


def nonempty(value, field):
    if not isinstance(value, str) or not value.strip() or len(value) > 2048:
        raise ValueError(f"preservation {field} must be a nonempty bounded string")


def preservation_holds(path, now):
    if path is None:
        return {}
    register, _ = read_json(absolute(path), private=True)
    if not isinstance(register, dict) or set(register) != {"version", "holds"} or type(register["version"]) is not int or register["version"] != 1:
        raise ValueError("preservation register requires version 1 and holds")
    if not isinstance(register["holds"], list):
        raise ValueError("preservation holds must be a list")
    active = {}
    for hold in register["holds"]:
        if not isinstance(hold, dict) or set(hold) != HOLD_FIELDS or not valid_digest(hold["sha256"]):
            raise ValueError("invalid preservation hold")
        if not isinstance(hold["kind"], str) or hold["kind"] not in RETENTION_PERIODS:
            raise ValueError("unsupported preservation artifact kind")
        for field in ("case_id", "reason", "owner", "permitted_use", "release_condition", "archive_path"):
            nonempty(hold[field], field)
        if not Path(hold["archive_path"]).is_absolute() or str(absolute(hold["archive_path"])) != hold["archive_path"]:
            raise ValueError("held archive_path must be an absolute normalized path")
        if absolute(hold["archive_path"]).resolve() != absolute(hold["archive_path"]):
            raise ValueError("held archive_path must use the real copy location without symlink aliases")
        readers = hold["authorized_readers"]
        if not isinstance(readers, list) or not readers:
            raise ValueError("preservation authorized_readers must be a nonempty list")
        for reader in readers:
            nonempty(reader, "authorized_readers")
        started, reviewed, review = (timestamp(hold[field]) for field in ("started_at", "reviewed_at", "next_review_at"))
        if not started <= reviewed <= now or not reviewed < review <= reviewed + timedelta(days=30):
            raise ValueError("preservation review must follow a recorded review by at most 30 days")
        created, expiry = timestamp(hold["created_at"]), timestamp(hold["expires_at"])
        if created > now or expiry != created + timedelta(days=RETENTION_PERIODS[hold["kind"]]):
            raise ValueError("preservation hold must carry the archive's original creation and expiry")
        released = hold["released_at"]
        if released is not None:
            if not started <= timestamp(released) <= now:
                raise ValueError("invalid preservation release time")
            continue
        key = (hold["kind"], hold["archive_path"], hold["sha256"])
        # An overdue review never releases a legal duty automatically.
        active.setdefault(key, []).append(hold)
    return active


def scan(roots, holds_path, now):
    holds = preservation_holds(holds_path, now)
    held_paths = {}
    for (kind, path, digest), records in holds.items():
        held_paths.setdefault(path, []).append((kind, digest, records))
    primaries = set()
    actual_files = set()
    sidecars = set()
    for requested in roots:
        root = absolute(requested)
        if root.resolve() != root or not root.is_dir():
            raise ValueError(f"managed directory must exist without symlinks: {root}")
        for directory, directories, files in os.walk(root, followlinks=False):
            # Do not silently omit copies behind symlinked directories.
            if any(Path(directory, name).is_symlink() for name in directories):
                raise ValueError("symlinked directories in managed backup locations must be listed as real directories separately")
            for name in files:
                path = Path(directory, name)
                if name.endswith(".dump.age"):
                    primaries.add(path)
                    actual_files.add(path)
                elif is_audit_file(name):
                    primaries.add(path if is_audit_file(name, "jsonl") else Path(directory, name.replace(".sig", ".jsonl")))
                    actual_files.add(path)
                elif name.endswith(SIDECAR) and (name[:-len(SIDECAR)].endswith(".dump.age") or is_audit_file(name[:-len(SIDECAR)], "jsonl")):
                    sidecars.add(path)
    if sidecars != {Path(str(path) + SIDECAR) for path in primaries}:
        raise ValueError("missing/orphaned retention metadata; copy each archive or audit pair with its .retention.json sidecar; enroll legacy archives using original pg_dump creation time, never file mtime")
    secondaries = (actual_files | sidecars) - primaries
    if any(Path(path) in secondaries for path in held_paths):
        raise ValueError("active preservation hold must name the primary artifact, not its signature or metadata")
    by_digest = {}
    planned = []
    for primary in sorted(primaries):
        metadata, files, metadata_fingerprint = artifact_metadata(primary, now)
        clocks = metadata["created_at"], metadata["expires_at"]
        previous = by_digest.setdefault((metadata["kind"], metadata["sha256"]), clocks)
        if previous != clocks:
            raise ValueError("identical artifact copies disagree on original creation/expiry; nothing was deleted")
        expired = timestamp(metadata["expires_at"]) <= now
        if any(kind != metadata["kind"] or digest != metadata["sha256"]
               for kind, digest, _ in held_paths.get(str(primary), [])):
            raise ValueError("active preservation hold disagrees with the artifact kind or digest at its registered path")
        held = holds.get((metadata["kind"], str(primary), metadata["sha256"]))
        if held and any(hold["created_at"] != metadata["created_at"] or hold["expires_at"] != metadata["expires_at"] for hold in held):
            raise ValueError("preservation hold disagrees with the archive's original clocks")
        status = "held_overdue_review" if held and any(timestamp(hold["next_review_at"]) <= now for hold in held) else "held" if held else "expired" if expired else "retained"
        planned.append((primary, metadata, files, metadata_fingerprint, status))
    if actual_files != {path for _, _, files, _, _ in planned for path, _ in files}:
        raise ValueError("unregistered archive or audit pair files; nothing was deleted")
    return planned


def expire(roots, holds_path, now, delete=False):
    holds_fingerprint = fingerprint(regular_file(absolute(holds_path))) if holds_path is not None else None
    planned = scan(roots, holds_path, now)
    counts = {"retained": 0, "held": 0, "held_overdue_review": 0, "expired": 0, "deleted": 0}
    for primary, _, files, metadata_fingerprint, status in planned:
        counts[status] += 1
        if status != "expired" or not delete:
            continue
        metadata_path = Path(str(primary) + SIDECAR)
        if holds_path is not None and fingerprint(regular_file(absolute(holds_path))) != holds_fingerprint:
            raise ValueError("preservation register changed after validation; refusing further deletion")
        if (any(fingerprint(regular_file(path)) != file_fingerprint for path, file_fingerprint in files) or
                fingerprint(regular_file(metadata_path)) != metadata_fingerprint):
            raise ValueError("backup changed after validation; refusing further deletion")
        for path, _ in files:
            path.unlink()
        metadata_path.unlink()
        counts["deleted"] += 1
    # Only aggregate counts: no archive paths, case reasons, database identifiers or private bodies.
    return counts


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    subparsers = parser.add_subparsers(dest="command", required=True)
    record = subparsers.add_parser("record", help="record original creation (also enroll legacy archives from their pg_dump header)")
    record.add_argument("source", help="encrypted archive, possibly its temporary path during backup")
    record.add_argument("output", help="final .dump.age path; existing metadata is never overwritten")
    clock = record.add_mutually_exclusive_group(required=True)
    clock.add_argument("--created-at", help="original pg_dump creation time in UTC; never file mtime")
    clock.add_argument("--copy-of", help="validate this original .dump.age and sidecar, then inherit their exact deadline for a derived recovery copy")
    expiry = subparsers.add_parser("expire", help="scan all local backup copies; default is a dry run")
    expiry.add_argument("directories", nargs="+", help="explicit ordinary database-backup and audit-export directories")
    expiry.add_argument("--holds", help="external per-copy preservation register JSON")
    expiry.add_argument("--delete", action="store_true", help="delete validated expired archives and sidecars")
    arguments = parser.parse_args()
    if arguments.command == "expire" and arguments.delete and arguments.holds is None:
        parser.error("--delete requires --holds pointing to the current preservation register (an explicit empty version-1 holds list is allowed)")
    now = datetime.now(timezone.utc).replace(microsecond=0)
    if arguments.command == "record":
        created_at = recovery_copy_clock(arguments.copy_of, now) if arguments.copy_of else arguments.created_at
        write_metadata(arguments.source, arguments.output, created_at, now)
    else:
        counts = expire(arguments.directories, arguments.holds, now, arguments.delete)
        print(json.dumps(counts, sort_keys=True))
        if counts["held_overdue_review"]:
            print("Preservation review is overdue; protected copies remain held.", file=sys.stderr)
            return 2
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (ValueError, OSError, OverflowError) as error:
        print(f"Backup retention refused: {error}", file=sys.stderr)
        sys.exit(1)
