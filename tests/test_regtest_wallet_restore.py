"""scripts/regtest-wallet-restore.py refuses anything but regtest before touching a wallet, and leaves the top issued
addresses unpaid so that only the runbook's advance moves the restored index past them (no real node needed)."""
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


class FakeRegtest(BaseHTTPRequestHandler):
    """A regtest node that hands out numbered addresses per wallet and records what generatetoaddress pays. Its
    restored wallet hands out a fresh address, so the script stops (FAIL, exit 1) right after the deposits."""
    issued = {}
    paid = []

    def reply(self, call):
        method, params = call['method'], call.get('params', [])
        wallet = self.path.rsplit('/', 1)[-1] if self.path.startswith('/wallet/') else ''
        if method == 'getblockchaininfo':
            return {'chain': 'regtest'}
        if method == 'getnewaddress':
            n = FakeRegtest.issued.get(wallet, 0)
            FakeRegtest.issued[wallet] = n + 1
            return '%s-addr-%d' % (wallet, n)
        if method == 'generatetoaddress':
            FakeRegtest.paid.append(params[1])
            return ['block']
        if method in ('getblockcount', 'getreceivedbyaddress'):
            return 0
        return None

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        calls = body if isinstance(body, list) else [body]
        replies = [{'result': self.reply(c), 'error': None, 'id': c['id']} for c in calls]
        reply = json.dumps(replies if isinstance(body, list) else replies[0]).encode()
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

    def test_leaves_the_top_issued_addresses_unpaid(self):
        # A-290: when the highest issued address is paid, the rescan alone moves the restored wallet's index past
        # every issued address and the check passes without the runbook's getnewaddress advance. The deposits must
        # leave the top issued addresses unpaid, and the highest paid one must still lie beyond the backup's
        # keypool (1000 keys after the funding address and the addresses issued before the backup).
        server = HTTPServer(('127.0.0.1', 0), FakeRegtest)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        FakeRegtest.issued, FakeRegtest.paid = {}, []
        result = self.run_script('http://smoke:%s@127.0.0.1:%d' % (SECRET, server.server_port))
        self.assertEqual(result.returncode, 1)  # the fake restored wallet hands out a fresh address
        self.assertIn('without the runbook step', result.stderr)
        wallet = next(w for w in FakeRegtest.issued if not w.endswith('-restored'))
        issued_count = FakeRegtest.issued[wallet]
        paid = [int(a.rsplit('-', 1)[1]) for a in FakeRegtest.paid if a.startswith(wallet + '-addr-')]
        # index 0 funds the miner; the rolled-back addresses (never stored) are the last ones handed out
        order_addresses = [i for i in paid if i != 0]
        self.assertEqual(len(order_addresses), 2, FakeRegtest.paid)
        top = issued_count - 20 - 1  # the highest issued (stored) address; 20 rolled-back ones follow it
        self.assertLessEqual(max(order_addresses), top - 50, 'the top issued addresses must stay unpaid')
        self.assertGreater(max(order_addresses), 1000 + 1 + 2, 'the highest paid address must lie beyond the backup keypool')
        self.assertNotIn(SECRET, result.stdout + result.stderr)


if __name__ == '__main__':
    unittest.main()
