#!/usr/bin/env python3
"""Stop registered historical pooled-chain processes while preserving their data.

New pooled-chain setup is retired. Use the escrow test harnesses for this build;
use the prior drain-capable revision for historical pooled tests.
"""
import argparse
import json
import os
from pathlib import Path
import signal
import subprocess
import time


def stop(state):
    """Refuse to signal a reused PID or a process outside this setup's registry."""
    remaining = []
    for process in reversed(state.get('processes', [])):
        pid = process['pid']

        def matches():
            result = subprocess.run(['ps', '-p', str(pid), '-o', 'args='], capture_output=True, text=True)
            return result.returncode == 0 and all(part in result.stdout for part in (process['binary'], process['marker']))

        if not matches():
            continue
        try:
            os.killpg(pid, signal.SIGTERM)
            deadline = time.monotonic() + 30
            while matches() and time.monotonic() < deadline:
                time.sleep(0.2)
            if matches():
                os.killpg(pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        except OSError:
            remaining.append(pid)
    if remaining:
        raise RuntimeError('Could not stop registered process IDs: ' + str(remaining))


def main():
    os.umask(0o077)
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--bitcoin-bin', help='Directory containing verified bitcoind')
    parser.add_argument('--monero-bin', help='Directory containing verified monerod and monero-wallet-rpc')
    parser.add_argument('--state-dir', default='artifacts/local-chains', help='New empty directory for disposable chain data')
    parser.add_argument('--first-port', type=int, metavar='PORT',
                        help='Allocate loopback ports consecutively from PORT (skipping ports in use) instead of any free port')
    parser.add_argument('--stop', metavar='STATE_DIR', help='Stop registered processes without deleting their data')
    args = parser.parse_args()
    if args.stop:
        state = json.loads((Path(args.stop).expanduser().resolve() / 'runtime.json').read_text())
        stop(state)
        print('Registered local chain processes stopped; data preserved.')
    else:
        parser.error('Pooled-chain setup is retired; use scripts/test-escrow-regtest.sh or scripts/test-escrow-xmr-regtest.sh. Historical tests require the prior drain-capable release. --stop STATE_DIR preserves existing data.')


def interrupted(_signal, _frame):
    raise KeyboardInterrupt('Local-chain setup interrupted')


if __name__ == '__main__':
    signal.signal(signal.SIGTERM, interrupted)
    main()
