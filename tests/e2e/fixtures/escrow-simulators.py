#!/usr/bin/env python3
"""Loopback-only Monero wallet/daemon simulators; no chain or real coins."""
import argparse
import threading
import xmr_escrow_rpc

parser = argparse.ArgumentParser()
parser.add_argument('--xmr-wallet-port', required=True, type=int)
parser.add_argument('--xmr-daemon-port', required=True, type=int)
args = parser.parse_args()
xmr_escrow_rpc.serve(args.xmr_wallet_port, args.xmr_daemon_port)
threading.Event().wait()
