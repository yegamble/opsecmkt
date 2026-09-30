"""scripts/regtest-wallet-restore.py refuses anything but regtest before touching a wallet (no real node needed)."""
from http.server import BaseHTTPRequestHandler, HTTPServer
import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import threading
import unittest

SCRIPT = Path(__file__).resolve().parents[1] / 'scripts' / 'regtest-wallet-restore.py'
SECRET = 'not-a-real-password'


class FakeNode(BaseHTTPRequestHandler):
    chain = 'main'
    methods = []

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        FakeNode.methods.append(body['method'])
        result = {'chain': FakeNode.chain} if body['method'] == 'getblockchaininfo' else None
        reply = json.dumps({'result': result, 'error': None, 'id': body['id']}).encode()
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(reply)))
        self.end_headers()
        self.wfile.write(reply)

    def log_message(self, *args):
        pass


class RegtestWalletRestoreTests(unittest.TestCase):
    def run_script(self, url):
        env = {'PATH': os.environ.get('PATH', '/usr/bin')}
        if url is not None:
            env['BITCOIN_REGTEST_RPC_URL'] = url
        return subprocess.run([sys.executable, str(SCRIPT)], env=env, capture_output=True, text=True, timeout=60)

    def test_requires_the_url(self):
        result = self.run_script(None)
        self.assertEqual(result.returncode, 2)
        self.assertIn('Set BITCOIN_REGTEST_RPC_URL', result.stderr)

    def test_refuses_other_chains_before_any_wallet_call(self):
        server = HTTPServer(('127.0.0.1', 0), FakeNode)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        for chain in ('main', 'test', 'testnet4', 'signet'):
            with self.subTest(chain=chain):
                FakeNode.chain, FakeNode.methods = chain, []
                result = self.run_script('http://smoke:%s@127.0.0.1:%d' % (SECRET, server.server_port))
                self.assertEqual(result.returncode, 1)
                self.assertIn('Refusing: the node at BITCOIN_REGTEST_RPC_URL is not on regtest', result.stderr)
                self.assertEqual(FakeNode.methods, ['getblockchaininfo'])
                self.assertNotIn(SECRET, result.stdout + result.stderr)

    def test_refuses_an_unreachable_node(self):
        with socket.socket() as probe:  # a port nothing listens on
            probe.bind(('127.0.0.1', 0))
            port = probe.getsockname()[1]
        result = self.run_script('http://smoke:%s@127.0.0.1:%d' % (SECRET, port))
        self.assertEqual(result.returncode, 1)
        self.assertIn('Refusing', result.stderr)
        self.assertNotIn(SECRET, result.stdout + result.stderr)


if __name__ == '__main__':
    unittest.main()
