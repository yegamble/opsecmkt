"""The simulated Monero escrow daemon and wallet-rpc (tests/e2e/fixtures/xmr_escrow_rpc.py, WR-escrow Phase 3 X-g2).
SIMULATED: not chain evidence. These tests hold it to monerod and monero-wallet-rpc 0.18.5.1: its keys and addresses are
the ones the real wallet derives, the recorder that recorded the real nodes' reply shapes (xmr_record_shapes.py) records
the same shapes from it, and the escrow behaviour the app depends on (confirmations, freeze, transfer, relay, faults)."""
from http.client import RemoteDisconnected
from http.server import ThreadingHTTPServer
import hashlib
import importlib.util
import json
from pathlib import Path
import threading
import unittest
import urllib.error
import urllib.request

FIXTURES = Path(__file__).resolve().parent / 'e2e' / 'fixtures'


def load(name):
    spec = importlib.util.spec_from_file_location(name, FIXTURES / (name + '.py'))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


xmr = load('xmr_escrow_rpc')
recorder = load('xmr_record_shapes')
OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))

# monero-wallet-rpc 0.18.5.1 (generate_from_keys with spend secret 1 and the view secret Hs(1), on a fakechain): the
# primary address and the subaddresses (major, minor) it gave. The wallet refuses keys that do not match the address.
REAL_KEYS_OF_ONE = {
    (0, 0): '44yQXfkWZNmJ8QgRfFWTzmJ8QgRfFWTzmJ8QgRfFWTzmJ9HskYAEKgjVy4kd3K4MaxERLtGa7FFrVNLF3jYWyjHCFA3tNHB',
    (0, 1): '83hiZbSjZ6zZT7UKafjSAje7anx3A5Vid8mcs2Dfi3Bviy3RSvSoEu1aS6V4bnnyytTocWnaCqgM1aapt8dAM98Y2FyDiXM',
    (1, 0): '8C5bWd37CCpdB7aA9u9bm2TSgHTa5qUnaZ4ZT5PUFbdShVNhM14VvcFJx2Q8sdxBYVVbZs2NMSK7jAgYt5dpkZm5RRABnsw',
    (2, 0): '83P382YET2mVdxXzn82prPCvZ5MEs3Q2KEpYiBP5LV9B8eH5HpJZVKwFt341M8qUrahKH9niWHwSDTULaAjW9D7mKTTyw9Z',
    (2, 1): '862N6z6Bxt9M5ATVWdu7a6EhRCv4X9iFE5oGQrHE8hUTHPLPbHEYV7E3ErZ4uGpMZJjAiTN6DHhQNi4PvTwpyzwJQfVLyi9',
}


class Nodes:
    """A simulated fakechain daemon with two wallet-rpcs on it (the escrow one and a funder), on loopback ports."""

    def __init__(self):
        self.chain = xmr.Chain()
        self.escrow, self.funder = xmr.WalletRPC(self.chain), xmr.WalletRPC(self.chain)
        self.servers = [ThreadingHTTPServer(('127.0.0.1', 0), h) for h in
                        (xmr.make_daemon_handler(self.chain), xmr.make_wallet_handler(self.escrow), xmr.make_wallet_handler(self.funder))]
        for s in self.servers:
            threading.Thread(target=s.serve_forever, daemon=True).start()
        self.daemon_url, self.escrow_url, self.funder_url = ('http://127.0.0.1:%d' % s.server_address[1] for s in self.servers)

    def close(self):
        for s in self.servers:
            s.shutdown()
            s.server_close()

    def post(self, url, body):
        req = urllib.request.Request(url, json.dumps(body).encode(), {'Content-Type': 'application/json'})
        try:
            with OPENER.open(req, timeout=30) as r:
                return r.status, json.loads(r.read())
        except urllib.error.HTTPError as e:
            return e.code, json.loads(e.read())

    def wallet(self, method, params=None, url=None):
        body = {'jsonrpc': '2.0', 'id': '7', 'method': method}
        if params is not None:
            body['params'] = params
        return self.post((url or self.escrow_url) + '/json_rpc', body)[1]

    def ok(self, method, params=None, url=None):
        reply = self.wallet(method, params, url)
        if 'error' in reply:
            raise AssertionError('%s: %s' % (method, reply['error']))
        return reply['result']

    def control(self, path, body):
        status, reply = self.post(self.escrow_url + path, body)
        if status != 200:
            raise AssertionError('%s: %s' % (path, reply))
        return reply

    def other(self, path, body):
        return self.post(self.daemon_url + path, body)[1]


class CryptoTest(unittest.TestCase):
    def test_keccak_is_monero_cn_fast_hash(self):
        self.assertEqual(xmr.keccak256(b'').hex(), 'c5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470')
        for n in (0, 1, 135, 136, 137, 271, 272, 500):  # the permutation, checked through SHA3-256's padding
            data = bytes(i % 251 for i in range(n))
            self.assertEqual(xmr.sponge256(data, 0x06), hashlib.sha3_256(data).digest(), n)

    def test_keys_and_addresses_are_the_real_wallets(self):
        keys = xmr.Keys(1)
        self.assertEqual(xmr.compress(xmr.G).hex(), '58' + '66' * 31)  # the ed25519 base point
        for index, address in REAL_KEYS_OF_ONE.items():
            self.assertEqual(keys.subaddress(*index), address, index)
            self.assertTrue(xmr.keys_prime_order(address))
        self.assertEqual(xmr.parse_address(REAL_KEYS_OF_ONE[(0, 0)])[0], xmr.TAG_STANDARD)
        self.assertEqual(xmr.parse_address(REAL_KEYS_OF_ONE[(1, 0)])[0], xmr.TAG_SUBADDRESS)
        broken = REAL_KEYS_OF_ONE[(1, 0)][:-1] + ('2' if REAL_KEYS_OF_ONE[(1, 0)][-1] != '2' else '3')
        self.assertIsNone(xmr.parse_address(broken))  # checksum

    def test_small_order_component_passes_check_key_but_is_not_prime_order(self):
        torsion = (xmr._SQRT_M1, 0, 1, 0)  # (sqrt(-1), 0) has order 4
        self.assertTrue(xmr.is_identity(xmr.scalar_mult(4, torsion)))
        keys = xmr.Keys(5)
        spend = xmr.compress(xmr.point_add(keys.B, torsion))
        address = xmr.encode_address(xmr.TAG_STANDARD, spend, keys.view_pub)
        self.assertIsNotNone(xmr.parse_address(address))  # what monero-wallet-rpc's validate_address accepts
        self.assertFalse(xmr.keys_prime_order(address))   # what the app refuses (A-417)

    def test_mnemonic_round_trip_and_checksum(self):
        for secret in (1, xmr.L - 1, xmr.hash_to_scalar(b'seed')):
            words = xmr.mnemonic_of(secret)
            self.assertEqual(len(words.split()), 25)
            self.assertEqual(xmr.secret_of(words), secret)
        words = xmr.mnemonic_of(12345).split()
        other = next(w for w in xmr.WORDS if w != words[24])
        self.assertIsNone(xmr.secret_of(' '.join(words[:24] + [other])))


class ShapesTest(unittest.TestCase):
    """The recorder of the real nodes' replies (xmr_record_shapes.py), run against the simulator, records the same file."""

    def test_recorder_gives_the_real_shapes(self):
        nodes = Nodes()
        self.addCleanup(nodes.close)
        got = recorder.Recorder(nodes.daemon_url, nodes.escrow_url, nodes.funder_url).run()
        want = json.loads((FIXTURES / 'xmr_rpc_shapes_0.18.5.1.json').read_text())
        self.assertEqual((got['monero'], got['wallet_rpc_version']), (want['monero'], want['wallet_rpc_version']))
        self.assertEqual(sorted(got['cases']), sorted(want['cases']))
        for case in sorted(want['cases']):
            self.assertEqual(got['cases'][case], want['cases'][case], case)


class EscrowBehaviourTest(unittest.TestCase):
    def setUp(self):
        self.n = Nodes()
        self.addCleanup(self.n.close)

    def escrow_account(self):
        self.n.ok('create_wallet', {'filename': 'escrow-209901-01', 'password': 'pw', 'language': 'English'})
        return self.n.ok('create_account')

    def test_network_and_wallets(self):
        info = self.n.post(self.n.daemon_url + '/json_rpc', {'jsonrpc': '2.0', 'id': 1, 'method': 'get_info'})[1]['result']
        self.assertEqual((info['nettype'], info['mainnet'], info['stagenet'], info['testnet']), ('fakechain', False, False, False))
        self.assertLess(info['height'], 1_000_000)
        hf = self.n.post(self.n.daemon_url + '/json_rpc', {'jsonrpc': '2.0', 'id': 1, 'method': 'hard_fork_info'})[1]['result']
        self.assertGreaterEqual(hf['version'], 16)
        self.assertEqual(self.n.wallet('get_height')['error']['code'], -13)
        self.n.ok('create_wallet', {'filename': 'escrow-209901-01', 'password': 'one', 'language': 'English'})
        a = self.n.ok('get_address', {'account_index': 0, 'address_index': [0]})['address']
        self.n.ok('create_wallet', {'filename': 'escrow-209901-02', 'password': 'two', 'language': 'English'})
        b = self.n.ok('get_address', {'account_index': 0, 'address_index': [0]})['address']
        self.assertNotEqual(a, b)
        self.assertEqual(self.n.wallet('open_wallet', {'filename': 'escrow-209901-01', 'password': 'two'})['error']['code'], -1)
        self.assertEqual(self.n.ok('get_address', {'account_index': 0, 'address_index': [0]})['address'], b)  # still the open one
        self.n.ok('open_wallet', {'filename': 'escrow-209901-01', 'password': 'one'})
        self.assertEqual(self.n.ok('get_address', {'account_index': 0, 'address_index': [0]})['address'], a)
        self.assertEqual(len(self.n.ok('query_key', {'key_type': 'mnemonic'})['key'].split()), 25)
        self.n.ok('refresh')
        self.assertEqual(self.n.ok('get_height')['height'], info['height'])

    def test_funding_freeze_build_relay_and_confirm(self):
        acct = self.escrow_account()
        self.assertEqual(acct['account_index'], 1)
        dep1 = self.n.control('/test/xmr/deposit', {'address': acct['address'], 'amount': 500_000_000_000})['txid']
        self.n.ok('refresh')
        pool = self.n.ok('get_transfers', {'in': True, 'pool': True, 'all_accounts': True})
        self.assertEqual([(e['txid'], e['type'], e['subaddr_index']) for e in pool['pool']], [(dep1, 'pool', {'major': 1, 'minor': 0})])
        self.assertNotIn('confirmations', pool['pool'][0])
        self.n.control('/test/xmr/mine', {'blocks': 9})
        self.n.ok('refresh')
        entry = self.n.ok('get_transfers', {'in': True, 'pool': True, 'all_accounts': True})['in'][0]
        self.assertEqual((entry['confirmations'], entry['locked']), (9, True))
        self.assertFalse(self.n.ok('incoming_transfers', {'transfer_type': 'available', 'account_index': 1, 'verbose': True})['transfers'][0]['unlocked'])
        self.n.control('/test/xmr/mine', {'blocks': 1})
        self.n.ok('refresh')
        self.assertEqual(self.n.ok('get_transfers', {'in': True, 'all_accounts': True})['in'][0]['confirmations'], 10)
        top_up = self.n.control('/test/xmr/deposit', {'address': acct['address'], 'amount': 70_000_000_000})['txid']
        self.n.control('/test/xmr/mine', {'blocks': 10})
        self.n.ok('refresh')
        outs = self.n.ok('incoming_transfers', {'transfer_type': 'available', 'account_index': 1, 'verbose': True})['transfers']
        self.assertEqual(sorted(o['tx_hash'] for o in outs), sorted([dep1, top_up]))
        self.assertTrue(all(o['unlocked'] and len(o['key_image']) == 64 for o in outs))
        claimed = next(o for o in outs if o['tx_hash'] == dep1)
        other = next(o for o in outs if o['tx_hash'] == top_up)
        self.n.ok('freeze', {'key_image': other['key_image']})
        claimant = self.n.control('/test/xmr/address', {})['address']
        fee_to = self.n.control('/test/xmr/address', {'label': 'fee'})['address']
        built = self.n.ok('transfer', {'destinations': [{'address': claimant, 'amount': 487_500_000_000}, {'address': fee_to, 'amount': 12_500_000_000}],
                                       'account_index': 1, 'priority': 0, 'subtract_fee_from_outputs': [0], 'do_not_relay': True,
                                       'get_tx_metadata': True, 'get_tx_hex': True})
        self.assertEqual(built['spent_key_images']['key_images'], [claimed['key_image']])  # exactly the unfrozen output
        self.assertEqual(built['fee'], 1_874_400_000)                                       # 0.0018744 XMR, one input and two outputs
        self.assertEqual(built['amounts_by_dest']['amounts'], [487_500_000_000 - 1_874_400_000, 12_500_000_000])
        self.assertEqual(built['amount'], sum(built['amounts_by_dest']['amounts']))
        tx = built['tx_hash']
        self.assertEqual(self.n.other('/get_transactions', {'txs_hashes': [tx], 'decode_as_json': True})['missed_tx'], [tx])
        self.assertEqual(self.n.other('/is_key_image_spent', {'key_images': [claimed['key_image']]})['spent_status'], [0])
        self.assertEqual(self.n.ok('relay_tx', {'hex': built['tx_metadata']})['tx_hash'], tx)
        self.assertEqual(self.n.ok('relay_tx', {'hex': built['tx_metadata']})['tx_hash'], tx)  # idempotent
        found = self.n.other('/get_transactions', {'txs_hashes': [tx], 'decode_as_json': True})['txs'][0]
        self.assertTrue(found['in_pool'])
        decoded = json.loads(found['as_json'])
        self.assertEqual((decoded['rct_signatures']['type'], [len(v['key']['key_offsets']) for v in decoded['vin']]), (6, [16]))
        self.assertEqual(self.n.other('/is_key_image_spent', {'key_images': [claimed['key_image'], other['key_image']]})['spent_status'], [2, 0])
        self.assertEqual(self.n.ok('get_transfer_by_txid', {'txid': tx, 'account_index': 1})['transfer']['type'], 'pending')
        self.n.control('/test/xmr/mine', {'blocks': 10})
        self.n.ok('refresh')
        found = self.n.other('/get_transactions', {'txs_hashes': [tx], 'decode_as_json': True})['txs'][0]
        self.assertEqual((found['in_pool'], found['confirmations']), (False, 10))
        self.assertEqual(self.n.other('/is_key_image_spent', {'key_images': [claimed['key_image']]})['spent_status'], [1])
        self.assertEqual(self.n.ok('get_transfer_by_txid', {'txid': tx, 'account_index': 1})['transfer']['type'], 'out')
        paid = self.n.post(self.n.escrow_url + '/test/xmr/state', {})  # state is a GET; a POST is no control
        self.assertEqual(paid[0], 400)
        with OPENER.open(self.n.escrow_url + '/test/xmr/state', timeout=10) as r:
            state = json.loads(r.read())
        self.assertIn({'address': claimant, 'amount': 487_500_000_000 - 1_874_400_000}, state['txs'][tx]['outputs'])
        left = self.n.ok('incoming_transfers', {'transfer_type': 'available', 'account_index': 1, 'verbose': True})['transfers']
        self.assertEqual([(o['tx_hash'], o['frozen']) for o in left], [(top_up, True)])

    def test_refused_relay_and_crashed_transfer(self):
        acct = self.escrow_account()
        self.n.control('/test/xmr/deposit', {'address': acct['address'], 'amount': 300_000_000_000})
        self.n.control('/test/xmr/mine', {'blocks': 10})
        self.n.ok('refresh')
        to = self.n.control('/test/xmr/address', {})['address']
        params = {'destinations': [{'address': to, 'amount': 300_000_000_000}], 'account_index': 1, 'priority': 0, 'subtract_fee_from_outputs': [0],
                  'do_not_relay': True, 'get_tx_metadata': True, 'get_tx_hex': True}
        built = self.n.ok('transfer', params)
        self.n.control('/test/xmr/refuse-relay', {'count': 1})
        self.assertEqual(self.n.wallet('relay_tx', {'hex': built['tx_metadata']})['error'], {'code': -4, 'message': 'Failed to commit tx.'})
        self.assertEqual(self.n.other('/get_transactions', {'txs_hashes': [built['tx_hash']]})['missed_tx'], [built['tx_hash']])
        self.n.control('/test/xmr/crash-transfer', {'count': 1})
        with self.assertRaises((RemoteDisconnected, ConnectionError, urllib.error.URLError)):
            self.n.wallet('transfer', params)
        self.assertEqual(self.n.wallet('get_height')['error']['code'], -13)  # it came back with no wallet open
        self.n.ok('open_wallet', {'filename': 'escrow-209901-01', 'password': 'pw'})
        again = self.n.ok('transfer', params)
        self.assertEqual(again['spent_key_images'], built['spent_key_images'])
        self.assertEqual(self.n.ok('relay_tx', {'hex': again['tx_metadata']})['tx_hash'], again['tx_hash'])
        # The first build's key image is spent now: relaying it is refused.
        self.assertEqual(self.n.wallet('relay_tx', {'hex': built['tx_metadata']})['error']['code'], -4)

    def test_small_order_destination_crashes_the_wallet_rpc(self):
        acct = self.escrow_account()
        self.n.control('/test/xmr/deposit', {'address': acct['address'], 'amount': 300_000_000_000})
        self.n.control('/test/xmr/mine', {'blocks': 10})
        self.n.ok('refresh')
        keys = xmr.Keys(9)
        bad = xmr.encode_address(xmr.TAG_STANDARD, xmr.compress(xmr.point_add(keys.B, (xmr._SQRT_M1, 0, 1, 0))), keys.view_pub)
        self.assertTrue(self.n.ok('validate_address', {'address': bad, 'any_net_type': False})['valid'])
        with self.assertRaises((RemoteDisconnected, ConnectionError, urllib.error.URLError)):
            self.n.wallet('transfer', {'destinations': [{'address': bad, 'amount': 100_000_000_000}], 'account_index': 1, 'do_not_relay': True})
        self.assertEqual(self.n.wallet('get_height')['error']['code'], -13)
        status, reply = self.n.post(self.n.escrow_url + '/test/xmr/deposit', {'address': bad, 'amount': 1})
        self.assertEqual(status, 400, reply)

    def test_lookahead_and_restore(self):
        self.n.ok('create_wallet', {'filename': 'escrow-209902-01', 'password': 'pw', 'language': 'English'})
        for _ in range(60):
            last = self.n.ok('create_account')
        seed = self.n.ok('query_key', {'key_type': 'mnemonic'})['key']
        primary = self.n.ok('get_address', {'account_index': 0, 'address_index': [0]})['address']
        self.n.control('/test/xmr/deposit', {'address': last['address'], 'amount': 200_000_000_000})
        self.n.control('/test/xmr/mine', {'blocks': 10})
        restored = self.n.ok('restore_deterministic_wallet', {'filename': 'escrow-209902-01-restored', 'password': 'pw', 'seed': seed, 'restore_height': 0,
                                                             'language': 'English', 'autosave_current': False})
        self.assertEqual(restored['address'], primary)
        self.n.ok('refresh')
        self.assertNotIn('per_subaddress', self.n.ok('get_balance', {'all_accounts': True}))  # account 60 is past the default lookahead of 50
        self.n.ok('set_subaddress_lookahead', {'password': 'pw', 'major_idx': 70, 'minor_idx': 1})
        self.n.ok('rescan_blockchain', {'hard': False})
        per = self.n.ok('get_balance', {'all_accounts': True})['per_subaddress']
        self.assertEqual([(r['account_index'], r['balance']) for r in per], [(60, 200_000_000_000)])


if __name__ == '__main__':
    unittest.main()
