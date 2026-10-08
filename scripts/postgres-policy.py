#!/usr/bin/env python3
"""Small, offline policy checks shared by the PostgreSQL backup scripts."""
import os
import re
import sys

MAX_BYTES = (1 << 63) - 1


def positive_integer(value, label, *, allow_zero=False):
    if not re.fullmatch(r"[0-9]+", value):
        raise ValueError(f"{label} must be a non-negative integer")
    result = int(value)
    if result > MAX_BYTES or (result == 0 and not allow_zero):
        raise ValueError(f"{label} is outside the supported range")
    return result


def required_backup_bytes(database_bytes, headroom_bytes):
    # Reserve 25% above the uncompressed database size for archive metadata and
    # estimation error, then add the operator-configurable fixed headroom.
    required = (database_bytes * 5 + 3) // 4 + headroom_bytes
    if required > MAX_BYTES:
        raise ValueError("estimated backup space is outside the supported range")
    return required


def parse_version(value):
    # pg_dump --version prints `pg_dump (PostgreSQL) 17.6`; the custom archive
    # header records just `17.6`. Ignore no suffixes: prerelease/unknown forms
    # fail closed rather than being ordered incorrectly.
    match = re.fullmatch(
        r"(?:pg_(?:dump|restore) \(PostgreSQL\) )?([0-9]+(?:\.[0-9]+){1,2})(?: \([^()\r\n]+\))?",
        value.strip(),
    )
    if not match:
        raise ValueError("unrecognized PostgreSQL client/archive version")
    return tuple(int(part) for part in match.group(1).split("."))


def restore_is_new_enough(dump_version, restore_version):
    dump = parse_version(dump_version)
    restore = parse_version(restore_version)
    width = max(len(dump), len(restore))
    return dump + (0,) * (width - len(dump)) <= restore + (0,) * (width - len(restore))


def main(argv):
    if len(argv) == 4 and argv[1] == "space":
        database_bytes = positive_integer(argv[2], "database size")
        available_kib = positive_integer(argv[3], "available space", allow_zero=True)
        if available_kib > MAX_BYTES // 1024:
            raise ValueError("available space is outside the supported range")
        headroom = positive_integer(
            os.environ.get("BACKUP_SPACE_HEADROOM_BYTES", "268435456"),
            "BACKUP_SPACE_HEADROOM_BYTES", allow_zero=True,
        )
        available_bytes = available_kib * 1024
        required = required_backup_bytes(database_bytes, headroom)
        if required > available_bytes:
            print("Insufficient free space for a conservative database backup estimate and configured headroom.", file=sys.stderr)
            return 1
        print(required)
        return 0
    if len(argv) == 4 and argv[1] == "compatible":
        if not restore_is_new_enough(argv[2], argv[3]):
            print("Refusing: selected pg_restore is older than the pg_dump client that created this archive.", file=sys.stderr)
            return 1
        return 0
    print("Usage: postgres-policy.py space DATABASE_BYTES AVAILABLE_KIB | compatible DUMP_VERSION RESTORE_VERSION", file=sys.stderr)
    return 2


if __name__ == "__main__":
    try:
        sys.exit(main(sys.argv))
    except (ValueError, OverflowError):
        print("Cannot safely verify PostgreSQL backup space or client-version compatibility.", file=sys.stderr)
        sys.exit(2)
