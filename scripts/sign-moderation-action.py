#!/usr/bin/env python3
"""Sign an existing moderation review token on an offline workstation.

Requires OpenSSL with Ed25519 support. The private key never leaves OpenSSL.
The output only authorizes a review annotation, not any marketplace action.
"""
import argparse
import json
import os
from pathlib import Path
import stat
import subprocess
import sys
import tempfile

DOMAIN = b"opsecmkt-moderation-action-v1\n"
FIELDS = {"version", "audience", "suggestion_id", "content_hash", "action", "expires_at", "nonce"}


def unique(pairs):
    result = {}
    for name, value in pairs:
        if name in result:
            raise ValueError("duplicate field")
        result[name] = value
    return result


def read_token(path):
    # Refuse nonregular paths before opening them. O_NONBLOCK also prevents a
    # replacement FIFO from blocking between this check and the descriptor check.
    if not stat.S_ISREG(os.stat(path).st_mode):
        raise ValueError("regular token file required")
    descriptor = os.open(path, os.O_RDONLY | getattr(os, "O_NONBLOCK", 0))
    with os.fdopen(descriptor, "rb") as source:
        if not stat.S_ISREG(os.fstat(source.fileno()).st_mode):
            raise ValueError("regular token file required")
        raw = source.read(2049)
    if len(raw) > 2048:
        raise ValueError("token too large")
    return raw


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--token", required=True, help="review JSON object to inspect and sign")
    parser.add_argument("--key", required=True, help="offline Ed25519 PEM private key")
    parser.add_argument("--out", required=True, help="new private signed-envelope file")
    args = parser.parse_args()
    raw = read_token(args.token)
    token = json.loads(raw, object_pairs_hook=unique)
    if not isinstance(token, dict) or set(token) != FIELDS or token["version"] != 1 or token["action"] not in ("accept", "dismiss", "hold"):
        raise ValueError("invalid review token fields")
    canonical = json.dumps(token, ensure_ascii=True, separators=(",", ":")).encode("ascii")
    # Ed25519 is a one-shot operation: OpenSSL requires a seekable message.
    # Only the public token enters this temporary file, never the private key.
    with tempfile.TemporaryDirectory(prefix="moderation-sign-") as directory:
        message = Path(directory) / "message"
        message.write_bytes(DOMAIN + canonical)
        message.chmod(0o600)
        result = subprocess.run(["openssl", "pkeyutl", "-sign", "-rawin", "-inkey", args.key,
                                 "-in", str(message)], stdout=subprocess.PIPE,
                                stderr=subprocess.DEVNULL, check=True)
    if len(result.stdout) != 64:
        raise ValueError("unexpected Ed25519 signature size")
    envelope = b'{"token":' + canonical + b',"signature":"' + result.stdout.hex().encode("ascii") + b'"}\n'
    fd = os.open(args.out, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "wb") as output:
        output.write(envelope)
    print("Signed review token written. Keep the private key offline.")


if __name__ == "__main__":
    try:
        main()
    except (ValueError, OSError, subprocess.CalledProcessError):
        print("Could not sign review token; check the token, offline key and new output path.", file=sys.stderr)
        sys.exit(1)
