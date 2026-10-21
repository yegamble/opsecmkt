#!/usr/bin/env python3
"""Loopback-only Monero wallet/daemon simulators; no chain or real coins.

A port of 0 lets the kernel choose a free one. The bound ports are printed as the first stdout line,
"xmr-wallet-port=<n> xmr-daemon-port=<n>", so a caller never has to pick and release a port that another
process could take before this one binds it (A-585)."""
import argparse
import threading
import xmr_escrow_rpc

parser = argparse.ArgumentParser()
parser.add_argument('--xmr-wallet-port', required=True, type=int)
parser.add_argument('--xmr-daemon-port', required=True, type=int)
args = parser.parse_args()
_, _, (daemon, wallet) = xmr_escrow_rpc.serve(args.xmr_wallet_port, args.xmr_daemon_port)
print(f'xmr-wallet-port={wallet.server_address[1]} xmr-daemon-port={daemon.server_address[1]}', flush=True)
threading.Event().wait()
