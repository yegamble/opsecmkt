#!/usr/bin/env python3
"""Verify a decrypted moderation packet against an independently pinned key.

Run on the offline review workstation after OpenPGP decryption. Requires
OpenSSL with Ed25519 support. This writes verified proposal data only; it never
executes a marketplace action or trusts a key supplied by the packet alone.
"""
import argparse
from datetime import datetime
import hmac
import json
import math
import os
from pathlib import Path
import re
import stat
import subprocess
import sys
import tempfile

MAX_PACKET_BYTES = 256 * 1024
MAX_PAYLOAD_BYTES = 192 * 1024
OUTER_FIELDS = {"payload", "signature", "public_key"}
PAYLOAD_FIELDS = {"version", "decision", "context", "evidence_digest",
                  "expires_utc", "default_action", "alternatives"}
PUBLIC_DER_PREFIX = bytes.fromhex("302a300506032b6570032100")


def unique_object(pairs):
    result = {}
    for name, value in pairs:
        if name in result:
            raise ValueError("duplicate JSON field")
        result[name] = value
    return result


def reject_constant(_):
    raise ValueError("non-finite JSON number")


def finite_number(value):
    number = float(value)
    if not math.isfinite(number):
        raise ValueError("non-finite JSON number")
    return number


def read_bounded_regular(path, limit):
    with open(path, "rb") as source:
        if not stat.S_ISREG(os.fstat(source.fileno()).st_mode):
            raise ValueError("regular input file required")
        value = source.read(limit + 1)
    if not value or len(value) > limit:
        raise ValueError("input exceeds bounds")
    return value


def parse_envelope(raw):
    """Decode strict JSON while retaining the exact signed payload bytes.

    JSON reserialization changes whitespace, Unicode and escaping. Locate the
    payload with the decoder at the actual outer-object value boundary instead
    of looking for a substring that an untrusted nested field could imitate.
    """
    if not raw or len(raw) > MAX_PACKET_BYTES:
        raise ValueError("packet exceeds bounds")
    text = raw.decode("utf-8")
    decoder = json.JSONDecoder(object_pairs_hook=unique_object,
                               parse_constant=reject_constant, parse_float=finite_number)

    def whitespace(position):
        while position < len(text) and text[position] in " \t\r\n":
            position += 1
        return position

    position = whitespace(0)
    if position >= len(text) or text[position] != "{":
        raise ValueError("packet object required")
    position = whitespace(position + 1)
    envelope = {}
    payload = None
    while True:
        name, position = decoder.raw_decode(text, position)
        if not isinstance(name, str) or name not in OUTER_FIELDS or name in envelope:
            raise ValueError("invalid packet field")
        position = whitespace(position)
        if position >= len(text) or text[position] != ":":
            raise ValueError("invalid packet separator")
        start = whitespace(position + 1)
        value, position = decoder.raw_decode(text, start)
        envelope[name] = value
        if name == "payload":
            payload = text[start:position].encode("utf-8")
        position = whitespace(position)
        if position >= len(text):
            raise ValueError("incomplete packet")
        delimiter = text[position]
        position = whitespace(position + 1)
        if delimiter == "}":
            break
        if delimiter != ",":
            raise ValueError("invalid packet separator")
    if position != len(text) or set(envelope) != OUTER_FIELDS:
        raise ValueError("invalid packet fields")
    if payload is None or len(payload) > MAX_PAYLOAD_BYTES:
        raise ValueError("payload exceeds bounds")
    body = envelope["payload"]
    if not isinstance(body, dict) or set(body) != PAYLOAD_FIELDS:
        raise ValueError("invalid payload fields")
    if body["version"] != "1" or body["default_action"] != "HOLD":
        raise ValueError("unsupported packet policy")
    if not isinstance(body["decision"], dict) or not isinstance(body["context"], dict):
        raise ValueError("invalid proposal context")
    if not isinstance(body["evidence_digest"], str) or re.fullmatch(r"[0-9a-f]{64}", body["evidence_digest"]) is None:
        raise ValueError("invalid evidence digest")
    expiry = body["expires_utc"]
    if not isinstance(expiry, str) or re.fullmatch(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z", expiry) is None:
        raise ValueError("UTC packet expiry required")
    datetime.strptime(expiry, "%Y-%m-%dT%H:%M:%SZ")
    alternatives = body["alternatives"]
    if not isinstance(alternatives, list) or len(alternatives) > 8 or any(not isinstance(value, str) or len(value) > 1000 for value in alternatives):
        raise ValueError("invalid alternatives")
    for name, length in (("signature", 128), ("public_key", 64)):
        value = envelope[name]
        if not isinstance(value, str) or re.fullmatch(r"[0-9a-f]{" + str(length) + r"}", value) is None:
            raise ValueError("invalid packet signature metadata")
    return envelope, payload


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--packet", required=True, help="decrypted packet JSON file")
    parser.add_argument("--public-key", required=True, help="independently pinned worker public key, 64 lowercase hex")
    parser.add_argument("--out", required=True, help="new private file for the verified payload JSON")
    args = parser.parse_args()
    envelope, payload = parse_envelope(read_bounded_regular(args.packet, MAX_PACKET_BYTES))
    pinned = read_bounded_regular(args.public_key, 65).removesuffix(b"\n")
    if re.fullmatch(rb"[0-9a-f]{64}", pinned) is None:
        raise ValueError("invalid external public pin")
    if not hmac.compare_digest(envelope["public_key"].encode("ascii"), pinned):
        raise ValueError("packet signer does not match external pin")
    with tempfile.TemporaryDirectory(prefix="moderation-verify-") as directory:
        root = Path(directory)
        (root / "payload").write_bytes(payload)
        (root / "payload").chmod(0o600)
        (root / "signature").write_bytes(bytes.fromhex(envelope["signature"]))
        (root / "public.der").write_bytes(PUBLIC_DER_PREFIX + bytes.fromhex(pinned.decode("ascii")))
        subprocess.run(["openssl", "pkeyutl", "-verify", "-rawin", "-pubin", "-keyform", "DER",
                        "-inkey", str(root / "public.der"), "-sigfile", str(root / "signature"),
                        "-in", str(root / "payload")], check=True, timeout=10,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    fd = os.open(args.out, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "wb") as destination:
        destination.write(payload)
    print("Verified escalation payload written.")


if __name__ == "__main__":
    try:
        main()
    except (ValueError, OSError, RecursionError, subprocess.SubprocessError):
        print("Could not verify escalation; check the packet, pinned public key and new output path.", file=sys.stderr)
        sys.exit(1)
