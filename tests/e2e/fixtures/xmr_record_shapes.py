#!/usr/bin/env python3
"""Records the reply shapes of a real monerod and monero-wallet-rpc on a local fakechain (WR-escrow Phase 3, X-g2).

The simulated Monero escrow wallet-rpc (xmr_escrow_rpc.py) is checked against the file this writes
(xmr_rpc_shapes_0.18.5.1.json, tests/test_xmr_escrow_rpc_fixture.py): every call the Monero escrow adapter makes
(internal/market/escrow_xmr_rpc.go), with the adapter's exact params, and the error answers it reads, as the real
nodes give them. A shape keeps each reply's field names and JSON types, never a value (no address, key, seed or hash),
and an error's code and message with the wallet names replaced by placeholders.

FAKECHAIN ONLY, manual, not run in CI: it refuses a daemon that does not report nettype "fakechain" with hard fork 16
or later before any wallet call. It needs a monerod --regtest and two monero-wallet-rpc instances attached to it
(the escrow wallet-rpc and a funder), as scripts/test-escrow-xmr-regtest.sh describes, for example:

  python3 tests/e2e/fixtures/xmr_record_shapes.py --daemon http://127.0.0.1:29081 \\
    --wallet http://127.0.0.1:29082 --funder http://127.0.0.1:29083 \\
    --out tests/e2e/fixtures/xmr_rpc_shapes_0.18.5.1.json

It mines about 150 blocks and creates wallets named escrow-2099MM-NN (a random month) on the escrow wallet-rpc.
"""
import argparse
import json
import random
import sys
import time
import urllib.request

OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))  # loopback nodes: never through a proxy


def post(url, body, timeout=300):
    req = urllib.request.Request(url, json.dumps(body).encode(), {'Content-Type': 'application/json'})
    with OPENER.open(req, timeout=timeout) as r:
        return json.loads(r.read())


def shape(v):
    """The JSON type tree of v: field names and types, no values."""
    if isinstance(v, bool):
        return 'boolean'
    if isinstance(v, int):
        return 'integer'
    if isinstance(v, float):
        return 'number'
    if isinstance(v, str):
        return 'string'
    if v is None:
        return 'null'
    if isinstance(v, list):
        merged = None
        for e in v:
            merged = merge(merged, shape(e))
        return [] if merged is None else [merged]
    return {k: shape(v[k]) for k in sorted(v)}


def merge(a, b):
    if a is None:
        return b
    if isinstance(a, dict) and isinstance(b, dict):
        return {k: merge(a.get(k), b.get(k)) if k in a and k in b else a.get(k, b.get(k)) for k in sorted(set(a) | set(b))}
    return a


class Recorder:
    def __init__(self, daemon, wallet, funder):
        self.daemon, self.wallet, self.funder = daemon.rstrip('/'), wallet.rstrip('/'), funder.rstrip('/')
        self.cases = {}
        self.raw = {}
        self.redact = {}

    def rpc(self, base, method, params=None, case=None, allow_error=False):
        body = {'jsonrpc': '2.0', 'id': '0', 'method': method}
        if params is not None:
            body['params'] = params
        reply = post(base + '/json_rpc', body)
        if 'error' in reply and not allow_error:
            raise SystemExit('%s %s failed: %s' % (base, method, reply['error']))
        if case:
            self.record(case, method, params, reply)
        return reply.get('result'), reply.get('error')

    def other(self, path, body, case=None):
        reply = post(self.daemon + path, body)
        if reply.get('status') != 'OK':
            raise SystemExit('%s failed: %s' % (path, reply.get('status')))
        if case:
            self.record(case, path, body, reply)
            for tx in reply.get('txs', [])[:1]:  # the adapter decodes as_json (vin key_offsets, k_image, rct type)
                self.cases[case + ' as_json'] = {'method': path, 'params': ['decode_as_json'], 'result': shape(json.loads(tx['as_json']))}
        return reply

    def record(self, case, method, params, reply):
        entry = {'method': method, 'params': sorted(params) if isinstance(params, dict) else ([] if params is None else ['<positional>'])}
        if 'error' in reply and reply['error']:
            message = reply['error'].get('message', '')
            for real, placeholder in self.redact.items():
                message = message.replace(real, placeholder)
            entry['error'] = {'code': reply['error'].get('code'), 'message': message}
        else:
            entry['result'] = shape(reply.get('result', reply))
        self.cases[case] = entry
        self.raw[case] = reply

    def mine(self, n, address):
        for _ in range(0, n, 20):
            self.rpc(self.daemon, 'generateblocks', {'amount_of_blocks': min(20, n), 'wallet_address': address}, case='daemon generateblocks')
            n -= 20

    def height(self):
        return self.rpc(self.daemon, 'get_info')[0]['height']

    def synced(self):
        for _ in range(120):
            self.rpc(self.wallet, 'refresh')
            if self.rpc(self.wallet, 'get_height')[0]['height'] == self.height():
                return
            time.sleep(0.5)
        raise SystemExit('the escrow wallet did not reach the daemon height')

    def run(self):
        info, _ = self.rpc(self.daemon, 'get_info', case='daemon get_info')
        hf, _ = self.rpc(self.daemon, 'hard_fork_info', case='daemon hard_fork_info')
        if info.get('nettype') != 'fakechain' or hf.get('version', 0) < 16:
            raise SystemExit('refusing: the daemon is not a fakechain with hard fork 16 or later')
        month = '2099%02d' % random.randint(1, 12)
        name, other, missing = 'escrow-%s-%02d' % (month, random.randint(1, 49)), 'escrow-%s-%02d' % (month, random.randint(50, 98)), 'escrow-%s-99' % month
        self.redact = {name: '<wallet>', other: '<other wallet>', missing: '<missing wallet>'}
        funder = 'funder-%d' % random.randint(0, 10**9)
        self.rpc(self.funder, 'create_wallet', {'filename': funder, 'password': '', 'language': 'English'})
        fund_addr = self.rpc(self.funder, 'get_address', {'account_index': 0})[0]['address']
        self.mine(120, fund_addr)  # coinbase unlocks after 60 blocks; 120 also give ring 16 its decoys
        w = self.wallet
        self.rpc(w, 'get_version', case='wallet get_version')
        self.rpc(w, 'get_height', case='wallet get_height (no wallet open)', allow_error=True)
        self.rpc(w, 'create_wallet', {'filename': name, 'password': 'pw-one', 'language': 'English'}, case='wallet create_wallet')
        self.rpc(w, 'create_wallet', {'filename': name, 'password': 'pw-one', 'language': 'English'}, case='wallet create_wallet (exists)', allow_error=True)
        self.rpc(w, 'query_key', {'key_type': 'mnemonic'}, case='wallet query_key mnemonic')
        self.rpc(w, 'get_height', case='wallet get_height')
        self.rpc(w, 'get_address', {'account_index': 0, 'address_index': [0]}, case='wallet get_address account 0')
        self.rpc(w, 'create_account', case='wallet create_account')
        self.rpc(w, 'create_account')
        acct1 = self.rpc(w, 'get_address', {'account_index': 1, 'address_index': [0]}, case='wallet get_address account 1')[0]['addresses'][0]['address']
        acct2 = self.rpc(w, 'get_address', {'account_index': 2, 'address_index': [0]})[0]['addresses'][0]['address']
        self.rpc(w, 'get_address', {'account_index': 99, 'address_index': [0]}, case='wallet get_address (account out of bound)', allow_error=True)
        self.rpc(w, 'get_address', {'account_index': 1, 'address_index': [5]}, case='wallet get_address (address out of bound)', allow_error=True)
        self.rpc(w, 'incoming_transfers', {'transfer_type': 'available', 'account_index': 1, 'verbose': True}, case='wallet incoming_transfers (none)')
        self.rpc(w, 'get_transfers', {'in': True, 'pool': True, 'all_accounts': True}, case='wallet get_transfers (none)')
        # Two deposits to account 1 (two transactions) and one to account 2 in the first of them.
        self.rpc(self.funder, 'refresh')
        dep1 = self.rpc(self.funder, 'transfer', {'destinations': [{'address': acct1, 'amount': 2_000_000_000_000}, {'address': acct2, 'amount': 500_000_000_000}],
                                                  'account_index': 0, 'priority': 0, 'ring_size': 16})[0]['tx_hash']
        self.synced()
        self.rpc(w, 'get_transfers', {'in': True, 'pool': True, 'all_accounts': True}, case='wallet get_transfers (pool)')
        self.mine(1, fund_addr)
        self.synced()
        self.rpc(w, 'get_transfers', {'in': True, 'pool': True, 'all_accounts': True}, case='wallet get_transfers (in)')
        self.rpc(w, 'get_transfer_by_txid', {'txid': dep1, 'account_index': 1}, case='wallet get_transfer_by_txid (in)')
        self.rpc(w, 'get_transfer_by_txid', {'txid': 'ab' * 32, 'account_index': 1}, case='wallet get_transfer_by_txid (unknown)', allow_error=True)
        self.rpc(w, 'transfer', {'destinations': [{'address': fund_addr, 'amount': 100_000_000_000}], 'account_index': 1, 'priority': 0, 'subtract_fee_from_outputs': [0],
                                 'do_not_relay': True, 'get_tx_metadata': True, 'get_tx_hex': True}, case='wallet transfer (coins locked)', allow_error=True)
        self.mine(10, fund_addr)
        self.rpc(self.funder, 'refresh')
        self.rpc(self.funder, 'transfer', {'destinations': [{'address': acct1, 'amount': 300_000_000_000}], 'account_index': 0, 'priority': 0, 'ring_size': 16})
        self.mine(11, fund_addr)
        self.synced()
        self.rpc(w, 'get_accounts', case='wallet get_accounts')
        self.rpc(w, 'get_balance', {'all_accounts': True}, case='wallet get_balance all_accounts')
        outs = self.rpc(w, 'incoming_transfers', {'transfer_type': 'available', 'account_index': 1, 'verbose': True}, case='wallet incoming_transfers')[0]['transfers']
        big = next(o for o in outs if o['amount'] == 2_000_000_000_000)
        small = next(o for o in outs if o['amount'] == 300_000_000_000)
        self.rpc(w, 'freeze', {'key_image': small['key_image']}, case='wallet freeze')
        self.rpc(w, 'freeze', {'key_image': 'cd' * 32}, case='wallet freeze (unknown key image)', allow_error=True)
        self.rpc(w, 'thaw', {'key_image': small['key_image']}, case='wallet thaw')
        self.rpc(w, 'freeze', {'key_image': small['key_image']})
        claimant = self.rpc(self.funder, 'create_address', {'account_index': 0}, case='wallet create_address')[0]['address']
        dests = [{'address': claimant, 'amount': 1_500_000_000_000}, {'address': fund_addr, 'amount': 500_000_000_000}]
        params = {'destinations': dests, 'account_index': 1, 'priority': 0, 'subtract_fee_from_outputs': [0],
                  'do_not_relay': True, 'get_tx_metadata': True, 'get_tx_hex': True}  # the adapter's exact params (buildTransfer)
        built, _ = self.rpc(w, 'transfer', params, case='wallet transfer do_not_relay')
        if built['spent_key_images']['key_images'] != [big['key_image']]:
            raise SystemExit('transfer spent other than the unfrozen output')
        self.other('/get_transactions', {'txs_hashes': [built['tx_hash']], 'decode_as_json': True}, case='daemon /get_transactions (missed)')
        self.rpc(w, 'relay_tx', {'hex': built['tx_metadata']}, case='wallet relay_tx')
        self.rpc(w, 'relay_tx', {'hex': built['tx_metadata']}, case='wallet relay_tx (again)')
        self.other('/get_transactions', {'txs_hashes': [built['tx_hash']], 'decode_as_json': True}, case='daemon /get_transactions (pool)')
        self.other('/is_key_image_spent', {'key_images': [big['key_image'], small['key_image']]}, case='daemon /is_key_image_spent')
        self.rpc(w, 'get_transfer_by_txid', {'txid': built['tx_hash'], 'account_index': 1}, case='wallet get_transfer_by_txid (pending)')
        self.rpc(w, 'get_transfers', {'in': True, 'out': True, 'pending': True, 'failed': True, 'pool': True, 'all_accounts': True}, case='wallet get_transfers (every kind)')
        self.mine(1, fund_addr)
        self.synced()
        self.other('/get_transactions', {'txs_hashes': [built['tx_hash']], 'decode_as_json': True}, case='daemon /get_transactions (mined)')
        self.rpc(w, 'get_transfer_by_txid', {'txid': built['tx_hash'], 'account_index': 1}, case='wallet get_transfer_by_txid (out)')
        self.rpc(w, 'get_transfers', {'in': True, 'out': True, 'pending': True, 'failed': True, 'pool': True, 'all_accounts': True}, case='wallet get_transfers (every kind, mined)')
        # A transaction whose output another transaction spent: relay_tx of its stored metadata is refused.
        self.mine(10, fund_addr)
        self.synced()
        self.rpc(w, 'thaw', {'key_image': small['key_image']})
        stale, _ = self.rpc(w, 'transfer', {**params, 'destinations': [{'address': claimant, 'amount': 200_000_000_000}]})
        self.rpc(w, 'transfer', {'destinations': [{'address': claimant, 'amount': 200_000_000_000}], 'account_index': 1, 'priority': 0, 'subtract_fee_from_outputs': [0]})
        self.rpc(w, 'relay_tx', {'hex': stale['tx_metadata']}, case='wallet relay_tx (refused: key images spent)', allow_error=True)
        self.rpc(w, 'relay_tx', {'hex': 'abcd'}, case='wallet relay_tx (bad metadata)', allow_error=True)
        self.rpc(w, 'transfer', {**params, 'destinations': [{'address': claimant, 'amount': 10**15}]}, case='wallet transfer (not enough money)', allow_error=True)
        bad = claimant[:-1] + ('2' if claimant[-1] != '2' else '3')  # one character changed: the checksum fails
        self.redact[bad] = '<destination>'
        self.rpc(w, 'transfer', {**params, 'destinations': [{'address': bad, 'amount': 100_000_000}]}, case='wallet transfer (invalid destination)', allow_error=True)
        self.rpc(w, 'transfer', {**params, 'account_index': 99, 'destinations': [{'address': claimant, 'amount': 100_000_000}]}, case='wallet transfer (account out of bound)', allow_error=True)
        self.rpc(w, 'validate_address', {'address': claimant, 'any_net_type': False}, case='wallet validate_address')
        self.rpc(w, 'validate_address', {'address': '4' + '1' * 94, 'any_net_type': False}, case='wallet validate_address (invalid)')
        self.rpc(w, 'refresh', case='wallet refresh')
        self.rpc(w, 'store', case='wallet store')
        mnemonic = self.rpc(w, 'query_key', {'key_type': 'mnemonic'})[0]['key']
        self.rpc(w, 'change_wallet_password', {'old_password': 'pw-one', 'new_password': 'pw-two'}, case='wallet change_wallet_password')
        self.rpc(w, 'change_wallet_password', {'old_password': 'pw-one', 'new_password': 'pw-three'}, case='wallet change_wallet_password (wrong old password)', allow_error=True)
        self.rpc(w, 'set_subaddress_lookahead', {'password': 'pw-two', 'major_idx': 60, 'minor_idx': 1}, case='wallet set_subaddress_lookahead')
        self.rpc(w, 'set_subaddress_lookahead', {'password': 'wrong', 'major_idx': 60, 'minor_idx': 1}, case='wallet set_subaddress_lookahead (wrong password)', allow_error=True)
        self.rpc(w, 'open_wallet', {'filename': missing, 'password': 'pw-two'}, allow_error=True)
        self.rpc(w, 'get_height', case='wallet get_height (after a failed open_wallet)', allow_error=True)
        self.rpc(w, 'rescan_blockchain', {'hard': False}, case='wallet rescan_blockchain')
        self.rpc(w, 'close_wallet', case='wallet close_wallet')
        self.rpc(w, 'get_height', case='wallet get_height (closed)', allow_error=True)
        self.rpc(w, 'open_wallet', {'filename': name, 'password': 'wrong'}, case='wallet open_wallet (wrong password)', allow_error=True)
        self.rpc(w, 'open_wallet', {'filename': missing, 'password': 'pw-two'}, case='wallet open_wallet (no such file)', allow_error=True)
        self.rpc(w, 'open_wallet', {'filename': name, 'password': 'pw-two'}, case='wallet open_wallet')
        self.rpc(w, 'close_wallet')
        self.rpc(w, 'restore_deterministic_wallet', {'filename': other, 'password': '', 'seed': mnemonic, 'restore_height': 0, 'language': 'English',
                                                       'autosave_current': False}, case='wallet restore_deterministic_wallet')
        self.rpc(w, 'restore_deterministic_wallet', {'filename': other, 'password': '', 'seed': mnemonic, 'restore_height': 0, 'language': 'English',
                                                       'autosave_current': False}, case='wallet restore_deterministic_wallet (exists)', allow_error=True)
        self.rpc(w, 'close_wallet')
        return {'monero': info.get('version'), 'wallet_rpc_version': self.rpc(w, 'get_version')[0]['version'], 'cases': self.cases}


def main():
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument('--daemon', required=True)
    p.add_argument('--wallet', required=True)
    p.add_argument('--funder', required=True)
    p.add_argument('--out', required=True)
    p.add_argument('--raw', help='also write the raw replies here (values included: keep it out of the repository)')
    a = p.parse_args()
    r = Recorder(a.daemon, a.wallet, a.funder)
    shapes = r.run()
    with open(a.out, 'w') as f:
        json.dump(shapes, f, indent=1, sort_keys=True)
        f.write('\n')
    if a.raw:
        with open(a.raw, 'w') as f:
            json.dump(r.raw, f, indent=1, sort_keys=True)
    print('recorded %d cases from monerod %s' % (len(shapes['cases']), shapes['monero']), file=sys.stderr)


if __name__ == '__main__':
    main()
