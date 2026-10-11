#!/usr/bin/env python3
"""Loopback-only escrow RPC/signing simulators; no pooled wallet, chain or real coins."""
import argparse
import threading
from http.server import ThreadingHTTPServer

parser = argparse.ArgumentParser()
parser.add_argument('--escrow-port', type=int)
parser.add_argument('--xmr-wallet-port', type=int)
parser.add_argument('--xmr-daemon-port', type=int)
parser.add_argument('--signer-socket')
args = parser.parse_args()
if (args.xmr_wallet_port is None) != (args.xmr_daemon_port is None):
    parser.error('--xmr-wallet-port and --xmr-daemon-port go together')
if args.signer_socket and not args.escrow_port:
    parser.error('--signer-socket needs --escrow-port')
if not (args.escrow_port or args.xmr_wallet_port):
    parser.error('at least one escrow simulator is required')
if args.escrow_port:
    import escrow_rpc
    server = ThreadingHTTPServer(('127.0.0.1', args.escrow_port), escrow_rpc.make_handler(escrow_rpc.EscrowCore()))
    threading.Thread(target=server.serve_forever, daemon=True).start()
    if args.signer_socket:
        escrow_rpc.serve_signer(args.signer_socket, escrow_rpc.FakeSigner())
if args.xmr_wallet_port:
    import xmr_escrow_rpc
    xmr_escrow_rpc.serve(args.xmr_wallet_port, args.xmr_daemon_port)
threading.Event().wait()
