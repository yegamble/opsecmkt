#!/usr/bin/env python3
"""Validate non-secret inference artifacts before an operator starts Compose.

This checks files and configuration, not the safety of reviewed image bytes.
No network calls, subprocesses, credentials or automatic downloads are used.
"""
import hashlib
import os
from pathlib import Path
import re
import stat
import sys


def validate(environment):
    image = environment.get('MODERATION_RUNTIME_IMAGE', '')
    if not re.fullmatch(r'[a-z0-9][a-z0-9._:/-]*@sha256:[0-9a-f]{64}', image):
        raise ValueError('runtime image must be pinned by a complete sha256 digest')
    if '://' in image or '//' in image or image.startswith('/'):
        raise ValueError('runtime image reference is invalid')
    digest = environment.get('MODERATION_WEIGHTS_SHA256', '')
    if not re.fullmatch(r'[0-9a-f]{64}', digest):
        raise ValueError('weights pin must be a lowercase sha256 digest')
    weights = Path(environment.get('MODERATION_WEIGHTS_FILE', ''))
    socket_dir = Path(environment.get('MODERATION_SOCKET_DIR', ''))
    if not weights.is_absolute() or not socket_dir.is_absolute():
        raise ValueError('weights and socket directory paths must be absolute')
    if weights.is_symlink() or not weights.is_file():
        raise ValueError('weights must be an existing regular file, not a symlink')
    weights_state = weights.stat()
    readable = bool(weights_state.st_mode & stat.S_IROTH)
    readable |= weights_state.st_uid == 65532 and bool(weights_state.st_mode & stat.S_IRUSR)
    readable |= weights_state.st_gid == 65532 and bool(weights_state.st_mode & stat.S_IRGRP)
    if not readable:
        raise ValueError('weights must be readable by the isolated runtime uid 65532')
    if socket_dir.is_symlink() or not socket_dir.is_dir():
        raise ValueError('socket directory must exist and not be a symlink')
    directory_state = socket_dir.stat()
    if (directory_state.st_uid, directory_state.st_gid) != (65532, 65532):
        raise ValueError('socket directory must be owned by 65532:65532')
    if stat.S_IMODE(directory_state.st_mode) not in (0o770, 0o2770):
        raise ValueError('socket directory mode must be 0770 or 02770')
    if any(socket_dir.iterdir()):
        raise ValueError('socket directory must be dedicated and empty before startup')
    if weights.resolve().is_relative_to(socket_dir.resolve()):
        raise ValueError('weights must be outside the writable socket directory')
    actual = hashlib.sha256()
    with weights.open('rb') as source:
        for block in iter(lambda: source.read(1024 * 1024), b''):
            actual.update(block)
    if actual.hexdigest() != digest:
        raise ValueError('weights pin mismatch')


if __name__ == '__main__':
    try:
        validate(os.environ)
    except (ValueError, OSError) as error:
        # File-system errors can include operator paths; emit a fixed class.
        detail = str(error) if isinstance(error, ValueError) else 'artifact access failed'
        print('moderation preflight: ' + detail, file=sys.stderr)
        sys.exit(1)
    print('moderation preflight: configuration and weights pin verified')
