#!/usr/bin/env python3
"""Manual Bitcoin Core regtest check of the runbook's "Restore the wallets" step (A-142). Not run in CI.

A wallet restored from a backup older than the database hands out addresses the database has already issued;
payment_addresses.address is UNIQUE, so each payment request that gets one fails. This replays
docs/testnet-runbook.md on regtest with two wallets of its own: backupwallet, issue addresses (a list stands in
for payment_addresses), restorewallet, show the collision, apply the runbook's step (getnewaddress N + 100
times, then rescanblockchain from the backup height) and check that every issued address is below the next
index and the next address is fresh. It refuses any node that is not on regtest and never prints the URL.

  BITCOIN_REGTEST_RPC_URL=http://smoke:smoke-pass@127.0.0.1:18443 python3 scripts/regtest-wallet-restore.py

scripts/regtest-smoke.sh runs it after the adapter smoke test. backupwallet writes on the node's filesystem:
BITCOIN_REGTEST_BACKUP_DIR (default /tmp) must be a directory bitcoind can write; the backup file is left there.
"""
import base64
import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

BEFORE_BACKUP = 2
# More than the node's default -keypool of 1000, so the last address is outside the backup's key range.
AFTER_BACKUP = 1100
ROLLED_BACK = 20  # handed out by the wallet but never stored (a payment request that failed after the call)
MARGIN = 100      # the runbook's margin over the issued count


class RPCError(Exception):
    pass


class Node:
    def __init__(self, url):
        parts = urllib.parse.urlsplit(url)
        if parts.scheme not in ('http', 'https') or not parts.hostname:
            raise SystemExit('BITCOIN_REGTEST_RPC_URL must be http://user:password@host:port')
        credentials = '%s:%s' % (urllib.parse.unquote(parts.username or ''), urllib.parse.unquote(parts.password or ''))
        self.auth = 'Basic ' + base64.b64encode(credentials.encode()).decode()
        port = ':%d' % parts.port if parts.port else ''
        self.base = '%s://%s%s' % (parts.scheme, parts.hostname if ':' not in parts.hostname else '[%s]' % parts.hostname, port)
        # A local node: never through a proxy from the environment.
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def post(self, wallet, body):
        path = '/wallet/' + urllib.parse.quote(wallet, safe='') if wallet else '/'
        request = urllib.request.Request(self.base + path, data=json.dumps(body).encode(),
                                         headers={'Authorization': self.auth, 'Content-Type': 'application/json'})
        try:
            with self.opener.open(request, timeout=120) as response:
                return json.load(response)
        except urllib.error.HTTPError as err:  # bitcoind answers RPC errors with 404/500 and a JSON body
            try:
                return json.load(err)
            except ValueError:
                raise RPCError('HTTP %d' % err.code) from None
        except (urllib.error.URLError, OSError) as err:
            raise RPCError('node unreachable (%s)' % type(err).__name__) from None

    def call(self, method, *params, wallet=None):
        reply = self.post(wallet, {'jsonrpc': '1.0', 'id': method, 'method': method, 'params': list(params)})
        if reply.get('error'):
            raise RPCError('%s: %s' % (method, reply['error'].get('message')))
        return reply['result']

    def batch(self, method, params_list, wallet=None):
        replies = self.post(wallet, [{'jsonrpc': '1.0', 'id': i, 'method': method, 'params': list(p)}
                                     for i, p in enumerate(params_list)])
        replies.sort(key=lambda r: r['id'])
        for reply in replies:
            if reply.get('error'):
                raise RPCError('%s: %s' % (method, reply['error'].get('message')))
        return [reply['result'] for reply in replies]


def key_index(info):
    """The last step of an address's hdkeypath (m/84h/1h/0h/0/INDEX), or None when the wallet does not know it."""
    path = info.get('hdkeypath', '')
    return int(path.rsplit('/', 1)[1]) if '/' in path else None


def main():
    url = os.environ.get('BITCOIN_REGTEST_RPC_URL', '')
    if not url:
        print('Set BITCOIN_REGTEST_RPC_URL=http://user:password@127.0.0.1:18443 (bitcoind -regtest).', file=sys.stderr)
        return 2
    node = Node(url)
    try:
        chain = node.call('getblockchaininfo').get('chain')
    except RPCError:
        chain = None
    if chain != 'regtest':
        print('Refusing: the node at BITCOIN_REGTEST_RPC_URL is not on regtest (or is unreachable).', file=sys.stderr)
        return 1

    name = 'restore-check-%d' % int(time.time())
    restored = name + '-restored'
    backup = os.path.join(os.environ.get('BITCOIN_REGTEST_BACKUP_DIR', '/tmp'), name + '.bak')
    loaded = []
    try:
        node.call('createwallet', name)
        loaded.append(name)
        miner = node.call('getnewaddress', wallet=name)  # a funding address, as the runbook's section 2 makes
        issue = lambda n: node.batch('getnewaddress', [['order:check', 'bech32']] * n, wallet=name)
        issued = issue(BEFORE_BACKUP)
        node.call('backupwallet', backup, wallet=name)
        backup_height = node.call('getblockcount')
        print('backupwallet at height %d after %d issued addresses' % (backup_height, len(issued)))
        issued += issue(AFTER_BACKUP)
        node.batch('getnewaddress', [['order:rolled-back', 'bech32']] * ROLLED_BACK, wallet=name)
        paid, last = issued[600], issued[-1]
        # The last address is paid first: when restorewallet's scan reaches that block, the address is still
        # outside the backup's key range. The later deposit to issued[600] moves the restored index past it.
        node.call('generatetoaddress', 1, last)
        node.call('generatetoaddress', 1, paid)
        node.call('generatetoaddress', 1, miner)
        print('issued %d addresses after the backup, handed out %d more that were never stored, paid #600 and #%d'
              % (AFTER_BACKUP, ROLLED_BACK, len(issued) - 1))
        issued_set = set(issued)

        node.call('restorewallet', restored, backup, False)
        loaded.append(restored)
        received = lambda a: node.call('getreceivedbyaddress', a, 0, True, wallet=restored)
        print('restorewallet: received by #600 %s, by the last issued address %s (0 expected with -keypool=1000)'
              % (received(paid), received(last)))

        collision = node.call('getnewaddress', 'order:check', 'bech32', wallet=restored)
        if collision not in issued_set:
            print('FAIL: the restored wallet handed out a fresh address without the runbook step', file=sys.stderr)
            return 1
        print('COLLISION without the runbook step: the restored wallet handed out issued address #%d again'
              % issued.index(collision))
        node.call('keypoolrefill', 1000, wallet=restored)
        again = node.call('getnewaddress', 'order:check', 'bech32', wallet=restored)
        print('keypoolrefill does not skip issued addresses: the next one is %s'
              % ('issued address #%d again' % issued.index(again) if again in issued_set else 'fresh'))

        count = len(issued)  # SELECT count(*) FROM payment_addresses WHERE currency='BTC'
        node.batch('getnewaddress', [['restore-skip', 'bech32']] * (count + MARGIN), wallet=restored)
        scan = node.call('rescanblockchain', backup_height, wallet=restored)
        print('runbook step: getnewaddress %d times (%d issued + %d), rescanblockchain %d..%d'
              % (count + MARGIN, count, MARGIN, scan['start_height'], scan['stop_height']))
        print('after the rescan: received by the last issued address %s' % received(last))
        if not received(last) > 0:
            print('FAIL: the rescan did not find the deposit to the last issued address', file=sys.stderr)
            return 1

        indexes = [key_index(info) for info in node.batch('getaddressinfo', [[a] for a in issued], wallet=restored)]
        missing = indexes.count(None)
        highest = max(i for i in indexes if i is not None)
        fresh = node.call('getnewaddress', 'order:check', 'bech32', wallet=restored)
        fresh_index = key_index(node.call('getaddressinfo', fresh, wallet=restored))
        print('check: highest issued index %d, next address index %d, issued addresses the wallet does not know: %d'
              % (highest, fresh_index, missing))
        if missing or fresh_index <= highest or fresh in issued_set:
            print('FAIL: the next address is not past every issued one', file=sys.stderr)
            return 1
        print('FRESH: the next address is past every issued address and not among them')
        print('PASS (backup file left on the node: %s)' % backup)
        return 0
    except RPCError as err:
        print('FAIL: %s' % err, file=sys.stderr)
        return 1
    finally:
        for wallet in loaded:
            try:
                node.call('unloadwallet', wallet)
            except RPCError:
                pass


if __name__ == '__main__':
    sys.exit(main())
