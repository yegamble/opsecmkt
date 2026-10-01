"""The simulated escrow node for the escrow browser journey (tests/e2e/fixtures/escrow_rpc.py): regtest only, refusing
mainnet and other-network addresses and keys; deterministic; and its PSBT methods compose into a full settlement (build,
party signature, arbiter signature, combine, finalize, mempool check, broadcast, confirmations). Simulated RPC, not
chain evidence."""
from http.server import ThreadingHTTPServer
import base64
import importlib.util
import json
from pathlib import Path
import threading
import unittest
import urllib.request

spec = importlib.util.spec_from_file_location('escrow_rpc', Path(__file__).resolve().parent / 'e2e' / 'fixtures' / 'escrow_rpc.py')
escrow_rpc = importlib.util.module_from_spec(spec)
spec.loader.exec_module(escrow_rpc)
W, A = escrow_rpc.WATCH_WALLET, escrow_rpc.ARBITER_WALLET


def rpc(core, method, *params, path='/'):
    return core.handle(path, method, list(params))


class EscrowFixtureTest(unittest.TestCase):
    def setUp(self):
        self.core = escrow_rpc.EscrowCore()
        self.buyer = self.core.test('/test/escrow/key', {'name': 'buyer'})['key']
        self.vendor = self.core.test('/test/escrow/key', {'name': 'vendor'})['key']

    def escrow(self):
        """The escrow descriptor of buyer, vendor and market child 5 (as the app builds it) and its address."""
        body = 'wsh(sortedmulti(2,%s/0/1,%s/0/1,%s/0/5))' % (self.buyer, self.vendor, self.core.arbiter_key)
        desc = body + '#' + escrow_rpc.desc_checksum(body)
        return desc, rpc(self.core, 'deriveaddresses', desc)[0]

    def test_regtest_only(self):
        self.assertEqual(rpc(self.core, 'getblockchaininfo')['chain'], 'regtest')
        for addr in ('bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4', 'tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx',
                     '1BvBMSEYstWetqTFn5Au4m4GFg7xJaNVN2'):
            with self.assertRaises(escrow_rpc.RPCError) as e:
                rpc(self.core, 'getdescriptorinfo', 'addr(%s)' % addr)
            self.assertEqual(e.exception.code, -5, addr)
        self.assertTrue(rpc(self.core, 'getdescriptorinfo', 'addr(%s)' % escrow_rpc.segwit_address(bytes(20)))['descriptor'])
        mainnet = 'xpub661MyMwAqRbcFtXgS5sYJABqqG9YLmC4Q1Rdap9gSE8NqtwybGhePY2gZ29ESFjqJoCu1Rupje8YtGqsefD265TMg7usUDFdp6W1EGMcet8'
        body = 'wpkh([00000000/84h/0h/0h]%s/0/1)' % mainnet
        for method, arg in (('getdescriptorinfo', body), ('deriveaddresses', body + '#' + escrow_rpc.desc_checksum(body))):
            with self.assertRaises(escrow_rpc.RPCError) as e:
                rpc(self.core, method, arg)
            self.assertEqual(e.exception.code, -5, method)
        with self.assertRaises(escrow_rpc.RPCError):
            rpc(self.core, 'createpsbt', [{'txid': '00' * 32, 'vout': 0}], [{'bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4': '0.001'}], 0, True)

    def test_deterministic(self):
        other = escrow_rpc.EscrowCore()
        self.assertEqual(other.arbiter_key, self.core.arbiter_key)
        self.assertEqual(other.test('/test/escrow/key', {'name': 'buyer'})['key'], self.buyer)
        desc, addr = self.escrow()
        self.assertEqual(rpc(other, 'deriveaddresses', desc), [addr])
        self.assertTrue(addr.startswith('bcrt1q') and len(addr) == 64)
        info = rpc(self.core, 'getaddressinfo', rpc(self.core, 'getrawchangeaddress', 'bech32', path=A), path=A)
        self.assertEqual(info['hdmasterfingerprint'], self.core.arbiter_key[1:9])
        self.assertTrue(info['parent_desc'].startswith('wpkh(' + self.core.arbiter_key + '/1/*)#'))

    def test_settlement_end_to_end(self):
        desc, addr = self.escrow()
        rpc(self.core, 'importdescriptors', [{'desc': desc, 'timestamp': 'now', 'active': False, 'label': 'escrow:o1'}], path=W)
        dep = self.core.test('/test/escrow/deposit', {'address': addr, 'sats': 100000})
        self.core.test('/test/escrow/mine', {'blocks': 3})
        utxo = rpc(self.core, 'gettxout', dep['txid'], 0, True)
        self.assertEqual((utxo['confirmations'], utxo['value'], utxo['scriptPubKey']['address']), (3, escrow_rpc.amount(100000), addr))
        claim = self.core.test('/test/escrow/address', {})['address']
        unsigned = rpc(self.core, 'createpsbt', [{'txid': dep['txid'], 'vout': 0}], [{claim: escrow_rpc.Decimal('0.00099000')}], 0, True)
        filled = rpc(self.core, 'utxoupdatepsbt', unsigned, [desc])
        ps = escrow_rpc.PSBT.b64(filled)
        self.assertEqual(len(ps.derivations(0)), 3)
        self.assertEqual(ps.outs[0][0], 99000)
        # The vendor's wallet signs; the arbiter wallet signs the same PSBT; combined and finalized, it is accepted.
        vfp = self.vendor[1:9]
        vendor = self.core.test('/test/escrow/sign', {'psbt': filled, 'fingerprint': vfp, 'sighash': 1})['psbt']
        self.assertFalse(rpc(self.core, 'finalizepsbt', vendor, True)['complete'])
        market = rpc(self.core, 'walletprocesspsbt', filled, True, 'ALL', True, False, path=A)['psbt']
        final = rpc(self.core, 'finalizepsbt', rpc(self.core, 'combinepsbt', [market, vendor]), True)
        self.assertTrue(final['complete'])
        self.assertEqual(escrow_rpc.txid_of(bytes.fromhex(final['hex'])), ps.txid())
        self.assertTrue(rpc(self.core, 'testmempoolaccept', [final['hex']], escrow_rpc.Decimal('0.001'))[0]['allowed'])
        txid = rpc(self.core, 'sendrawtransaction', final['hex'], escrow_rpc.Decimal('0.001'))
        self.assertEqual(txid, ps.txid())
        self.assertIsNone(rpc(self.core, 'gettxout', dep['txid'], 0, True))
        self.assertEqual(rpc(self.core, 'sendrawtransaction', final['hex'], escrow_rpc.Decimal('0.001')), txid)  # idempotent
        self.assertEqual(rpc(self.core, 'gettransaction', txid, True, path=W)['confirmations'], 0)
        self.core.test('/test/escrow/mine', {'blocks': 3})
        self.assertEqual(rpc(self.core, 'gettransaction', txid, True, path=W)['confirmations'], 3)
        with self.assertRaises(escrow_rpc.RPCError) as e:
            rpc(self.core, 'sendrawtransaction', final['hex'], escrow_rpc.Decimal('0.001'))
        self.assertEqual(e.exception.code, -27)
        self.assertIn({'txid': txid, 'category': 'send', 'confirmations': 3}, rpc(self.core, 'listtransactions', '*', 100, 0, True, path=W))

    def test_bad_signature_does_not_finalize(self):
        desc, addr = self.escrow()
        dep = self.core.test('/test/escrow/deposit', {'address': addr, 'sats': 50000})
        claim = self.core.test('/test/escrow/address', {})['address']
        filled = rpc(self.core, 'utxoupdatepsbt', rpc(self.core, 'createpsbt', [{'txid': dep['txid'], 'vout': 0}], [{claim: escrow_rpc.Decimal('0.0004')}], 0, True), [desc])
        ps = escrow_rpc.PSBT.b64(filled)
        pub = next(p for p, fp, _ in ps.derivations(0) if fp == self.vendor[1:9])
        ps.add(0, b'\x02' + pub, escrow_rpc.fake_sig(pub, 'another transaction', 1))
        market = rpc(self.core, 'walletprocesspsbt', filled, True, 'ALL', True, False, path=A)['psbt']
        self.assertFalse(rpc(self.core, 'finalizepsbt', rpc(self.core, 'combinepsbt', [market, ps.encode()]), True)['complete'])
        with self.assertRaises(ValueError):
            self.core.test('/test/escrow/deposit', {'address': escrow_rpc.segwit_address(bytes(20)), 'sats': 1})

    def test_amounts_are_json_numbers_over_http(self):
        server = ThreadingHTTPServer(('127.0.0.1', 0), escrow_rpc.make_handler(self.core))
        threading.Thread(target=server.serve_forever, daemon=True).start()
        try:
            _, addr = self.escrow()
            dep = self.core.test('/test/escrow/deposit', {'address': addr, 'sats': 1000})
            body = json.dumps({'method': 'gettxout', 'params': [dep['txid'], 0, True], 'id': 1}).encode()
            req = urllib.request.Request('http://127.0.0.1:%d/' % server.server_port, body, {'Content-Type': 'application/json'})
            opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
            text = opener.open(req, timeout=5).read().decode()
            self.assertIn('"value": 0.00001000', text)
            body = json.dumps({'method': 'getblockchaininfo', 'params': [], 'id': 2}).encode()
            req = urllib.request.Request('http://127.0.0.1:%d/' % server.server_port, body, {'Content-Type': 'application/json'})
            self.assertEqual(json.loads(opener.open(req, timeout=5).read())['result']['chain'], 'regtest')
        finally:
            server.shutdown()
            server.server_close()


if __name__ == '__main__':
    unittest.main()
