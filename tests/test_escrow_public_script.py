"""The Bitcoin escrow operator package (A-446, P4-R16), offline: scripts/escrow-node-setup.sh against a fake bitcoin-cli
and a fake node, and scripts/test-escrow-public.sh against a fake node, a fake `go` and a fake `docker` in a throwaway
git repository. No real node, no network: the public run itself is the owner's (A-447).

What is pinned here: the setup script's rpcwhitelist lists are exactly the Go adapters' (escrowRPCMethods,
pooledRPCMethods); it refuses mainnet and testnet3 and a node on another chain before any wallet call, creates the arbiter
and watch-only wallets as the app needs them (or keeps checked existing ones), and prints an rpcauth line that verifies
against the password in the ESCROW_BTC_RPC_URL line it prints, writing nothing; --verify passes only when the pooled
user is refused listdescriptors, gethdkeys and walletprocesspsbt on the arbiter wallet. The public script refuses
regtest, mainnet, testnet3 and a private signet (any challenge but public signet's) before go runs; its manifest holds
the commit, the image digest, chain and signet_challenge, heights, txids per leg and the transcript's sha256; and a
record that holds a password, an rpcauth salt or hash, a passphrase or a cookie is never written.
"""
from http.server import BaseHTTPRequestHandler, HTTPServer
import base64
import hashlib
import hmac
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import threading
import unittest

REPOSITORY = Path(__file__).resolve().parents[1]
SETUP = REPOSITORY / 'scripts' / 'escrow-node-setup.sh'
PUBLIC = REPOSITORY / 'scripts' / 'test-escrow-public.sh'
PUBLIC_SIGNET = ('512103ad5e0edad18cb1f0fc0d28a3d4f1f3e445640337489abb10404f2d1e086be430210359ef5021964fe22d6f8e05b2463c9'
                 '540ce96883fe3b278760f048f5189f2e6c452ae')
TXIDS = {('claim', 'funding'): 'a1' * 32, ('claim', 'settlement'): 'b2' * 32,
         ('refund', 'funding'): 'c3' * 32, ('refund', 'settlement'): 'd4' * 32}
DIGEST = 'bitcoin/bitcoin@sha256:' + 'e5' * 32


def go_list(path, name):
    source = (REPOSITORY / path).read_text()
    block = re.search(r'var %s = \[\]string\{(.*?)\n\}' % name, source, re.S)
    assert block, name
    return re.findall(r'"([a-z]+)"', block.group(1))


ESCROW_METHODS = go_list('internal/market/escrow_rpc.go', 'escrowRPCMethods')
POOLED_METHODS = go_list('internal/market/payments_bitcoin.go', 'pooledRPCMethods')


def scan_for_secrets(text, passwords):
    """Independent of the script's own scan: what a committed record must never hold."""
    found = [p for p in passwords if p and p in text]
    for label, pattern in (('rpcpassword', r'rpcpassword'), ('rpcauth', r'rpcauth'), ('salt$hash', r'[0-9a-f]{32}\$[0-9a-f]{64}'),
                           ('passphrase', r'passphrase'), ('cookie', r'cookie'), ('url password', r'://[^/@\s:]+:[^/@\s]+@'),
                           ('private key', r'[tx]prv[1-9A-HJ-NP-Za-km-z]{50,}')):
        if re.search(pattern, text, re.I):
            found.append(label)
    return found


class FakeNode(BaseHTTPRequestHandler):
    """A JSON-RPC node: chain and signet_challenge from the server's attributes; per-user rpcwhitelists (None =
    unrestricted) answer HTTP 403 for any other method, as Core does; wallet calls answer per wallet path."""

    def do_POST(self):
        srv = self.server
        body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        method = body['method']
        auth = self.headers.get('Authorization', '')
        user = base64.b64decode(auth[6:]).decode().split(':', 1)[0] if auth.startswith('Basic ') else ''
        srv.calls.append((user, self.path, method))
        allowed = srv.whitelists.get(user)
        if allowed is not None and method not in allowed:
            self.send_response(403)
            self.send_header('Content-Length', '0')
            self.end_headers()
            return
        wallet = self.path[len('/wallet/'):] if self.path.startswith('/wallet/') else ''
        result, error = None, None
        if method == 'getblockchaininfo':
            result = {'chain': srv.chain, 'blocks': srv.height, 'headers': srv.height}
            if srv.chain == 'signet':
                result['signet_challenge'] = srv.challenge
        elif method == 'getnetworkinfo':
            result = {'version': 310100, 'subversion': '/Satoshi:31.1.0/', 'connections': 8}
        elif method == 'getwalletinfo':
            if wallet not in srv.wallets:
                error = {'code': -18, 'message': 'Requested wallet does not exist or is not loaded'}
            else:
                result = {'walletname': wallet, 'descriptors': True, 'private_keys_enabled': srv.wallets[wallet]}
        elif wallet and wallet not in srv.wallets:
            error = {'code': -18, 'message': 'Requested wallet does not exist or is not loaded'}
        elif method in ('listdescriptors', 'gethdkeys'):
            result = {'descriptors': [{'desc': 'wpkh(tpubD6NzVbkrYhZ4Xexample/84h/1h/0h/0/*)#abcdefgh'}]}
        elif method == 'walletprocesspsbt':
            error = {'code': -22, 'message': 'TX decode failed'}
        else:
            error = {'code': -32601, 'message': 'Method not found'}
        reply = json.dumps({'result': result, 'error': error, 'id': body['id']}, separators=(',', ':')).encode()
        self.send_response(500 if error else 200)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(reply)))
        self.end_headers()
        self.wfile.write(reply)

    def log_message(self, *args):
        pass


class NodeCase(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.server = HTTPServer(('127.0.0.1', 0), FakeNode)
        threading.Thread(target=cls.server.serve_forever, daemon=True).start()
        cls.port = cls.server.server_port

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        cls.server.server_close()

    def setUp(self):
        s = self.server
        s.chain, s.challenge, s.height, s.calls = 'signet', PUBLIC_SIGNET, 250000, []
        s.whitelists = {'opsecmkt-escrow': set(ESCROW_METHODS), 'opsecmkt-pooled': set(POOLED_METHODS), 'operator': None}
        s.wallets = {'opsecmkt-arbiter': True, 'opsecmkt-escrow-watch': False, 'funding': True}

    def url(self, user, password):
        return 'http://%s:%s@127.0.0.1:%d' % (user, password, self.port)


class ConsistencyTest(unittest.TestCase):
    def test_setup_whitelists_are_the_go_adapters_lists(self):
        text = SETUP.read_text()
        self.assertEqual(re.search(r'^escrow_methods=(\S+)$', text, re.M).group(1).split(','), ESCROW_METHODS)
        self.assertEqual(re.search(r'^pooled_methods=(\S+)$', text, re.M).group(1).split(','), POOLED_METHODS)
        for key_method in ('listdescriptors', 'gethdkeys', 'dumpprivkey', 'dumpwallet', 'backupwallet'):
            self.assertNotIn(key_method, ESCROW_METHODS)
            self.assertNotIn(key_method, POOLED_METHODS)
        # A-440: the three methods the app's self-check probes are never the pooled user's.
        for probe in ('listdescriptors', 'gethdkeys', 'walletprocesspsbt'):
            self.assertNotIn(probe, POOLED_METHODS)

    def test_public_signet_challenge_is_the_same_everywhere(self):
        go = re.search(r'const publicSignetChallenge = "([0-9a-f]+)"', (REPOSITORY / 'internal/market/escrow_public_test.go').read_text())
        self.assertEqual(go.group(1), PUBLIC_SIGNET)
        self.assertIn('PUBLIC_SIGNET_CHALLENGE=%s\n' % PUBLIC_SIGNET, PUBLIC.read_text())
        self.assertIn('public_signet_challenge=%s\n' % PUBLIC_SIGNET, SETUP.read_text())

    def test_chain_table_matches_the_go_test(self):
        # The shell chain_refusal and Go publicChainRefusal accept exactly testnet4 and public signet.
        cases = [('testnet4', '', True), ('signet', PUBLIC_SIGNET, True), ('signet', '51', False), ('signet', '', False),
                 ('signet', PUBLIC_SIGNET.upper(), False), ('regtest', '', False), ('main', '', False),
                 ('test', '', False), ('testnet3', '', False), ('', '', False)]
        for chain, challenge, accepted in cases:
            r = subprocess.run(['bash', '-c', 'source "$1"; chain_refusal "$2" "$3"', 'bash', str(PUBLIC), chain, challenge],
                               capture_output=True, text=True, timeout=30)
            self.assertEqual(r.returncode, 0, r.stderr)
            self.assertEqual(r.stdout.strip() == '', accepted, (chain, challenge, r.stdout))
        go = (REPOSITORY / 'internal/market/escrow_public_test.go').read_text()
        self.assertIn('{"regtest", ""}, {"main", ""}, {"test", ""}, {"signet", "51"}', go)

    def test_env_lines_the_setup_prints_are_existing_settings(self):
        # The set-up prints no new setting: each .env key it prints is in .env.example and passed by compose.yaml.
        example = (REPOSITORY / '.env.example').read_text()
        compose = (REPOSITORY / 'compose.yaml').read_text()
        printed = set(re.findall(r'^ *(?:echo ")?([A-Z][A-Z0-9_]+)=(?:http|\$)', SETUP.read_text(), re.M))
        self.assertEqual(printed, {'ESCROW_BTC_RPC_URL', 'ESCROW_BTC_ARBITER_WALLET', 'ESCROW_BTC_WATCH_WALLET', 'BITCOIN_RPC_URL'})
        for key in printed:
            self.assertRegex(example, r'(?m)^%s=$' % key)
            self.assertIn('%s: ${%s:-}' % (key, key), compose)

    def test_node_bump_checklist_names_both_escrow_scripts(self):
        guide = (REPOSITORY / 'docs' / 'operator-guide.md').read_text()
        checklist = guide.split('Before changing a tag (default or `.env`):', 1)[1].split('**Runtime wallet check.**', 1)[0]
        self.assertIn('scripts/escrow-node-setup.sh', checklist)
        self.assertIn('scripts/test-escrow-public.sh', checklist)


FAKE_CLI = r'''#!/usr/bin/env python3
import json, os, sys
state_path = os.environ['FAKE_CLI_STATE']
state = json.load(open(state_path))
args = sys.argv[1:]
state["calls"].append(list(args))
chain = wallet = None
named = False
while args and args[0].startswith('-'):
    a = args.pop(0)
    if a.startswith('-chain='): chain = a[7:]
    elif a.startswith('-rpcwallet='): wallet = a[11:]
    elif a == '-named': named = True
method, params = args[0], args[1:]
def out(v):
    json.dump(state, open(state_path, 'w'))
    print(v if isinstance(v, str) else json.dumps(v, indent=2))
    sys.exit(0)
def fail(msg):
    json.dump(state, open(state_path, 'w'))
    print('error code: -4\nerror message:\n' + msg, file=sys.stderr)
    sys.exit(4)
if chain != state['flag']:
    fail('wrong chain flag')
if method == 'getblockchaininfo':
    info = {'chain': state['chain'], 'blocks': 120}
    if state['chain'] == 'signet': info['signet_challenge'] = state['challenge']
    out(info)
if method == 'listwallets':
    out([n for n, w in state['wallets'].items() if w['loaded']])
if method == 'listwalletdir':
    out({'wallets': [{'name': n, 'warnings': []} for n in state['wallets']]})
if method == 'createwallet':
    kv = dict(p.split('=', 1) for p in params)
    name = kv['wallet_name']
    if name in state['wallets']: fail('Wallet file verification failed. Failed to create database path')
    state['wallets'][name] = {'loaded': True, 'keys': kv.get('disable_private_keys') != 'true', 'encrypted': False, 'kv': kv}
    out({'name': name})
if method == 'loadwallet':
    state['wallets'][params[0]]['loaded'] = True
    out({'name': params[0]})
w = state['wallets'].get(wallet)
if w is None or not w['loaded']:
    fail('Requested wallet does not exist or is not loaded')
if method == 'getwalletinfo':
    info = {'walletname': wallet, 'descriptors': True, 'private_keys_enabled': w['keys'], 'txcount': 0}
    if w['encrypted']: info['unlocked_until'] = 0
    out(info)
if method == 'getrawchangeaddress':
    out('bcrt1qchange0000000000000000000000000000000')
if method == 'getaddressinfo':
    out({'address': params[0], 'ismine': True, 'hdmasterfingerprint': 'd34db33f'})
fail('unexpected ' + method)
'''


class SetupScriptTest(NodeCase):
    def run_setup(self, *args, chain='regtest', wallets=None, challenge='51', env_extra=None):
        with tempfile.TemporaryDirectory() as tmp:
            cli = Path(tmp, 'fake-bitcoin-cli')
            cli.write_text(FAKE_CLI)
            cli.chmod(0o755)
            state = Path(tmp, 'state.json')
            flag = dict(a.split('=', 1) for a in ['chain=' + chain]).get('chain')
            for i, a in enumerate(args):
                if a == '--chain':
                    flag = args[i + 1]
            state.write_text(json.dumps({'chain': chain, 'challenge': challenge, 'flag': flag, 'wallets': wallets or {}, 'calls': []}))
            work = Path(tmp, 'work')
            work.mkdir()
            env = dict(os.environ, FAKE_CLI_STATE=str(state), HOME=str(work))
            env.pop('ESCROW_BTC_RPC_URL', None)
            env.pop('BITCOIN_RPC_URL', None)
            env.update(env_extra or {})
            r = subprocess.run(['bash', str(SETUP), '--cli', str(cli)] + list(args), cwd=work, env=env,
                               capture_output=True, text=True, timeout=60)
            left = sorted(p.name for p in work.iterdir())
            return r, json.loads(state.read_text()), left

    def test_refuses_mainnet_and_testnet3_before_any_node_call(self):
        for chain in ('main', 'mainnet', 'test', 'testnet', 'testnet3', 'bogus'):
            r, state, _ = self.run_setup('--chain', chain, chain=chain)
            self.assertEqual(r.returncode, 1, (chain, r.stdout, r.stderr))
            self.assertIn('Refusing', r.stderr)
            self.assertEqual(state['calls'], [], chain)
            self.assertNotIn('rpcauth=', r.stdout)

    def test_refuses_a_node_on_another_chain_before_any_wallet_call(self):
        for node in ('main', 'test', 'signet'):
            r, state, _ = self.run_setup('--chain', 'regtest', chain=node)
            self.assertEqual(r.returncode, 1, r.stdout)
            self.assertIn("Refusing: the node reports chain '%s', not regtest." % node, r.stderr)
            self.assertEqual([c[-1] for c in state['calls']], ['getblockchaininfo'])
            self.assertEqual(r.stdout, '')

    def lines(self, out):
        return [l for l in out.splitlines() if l and not l.startswith('#')]

    def test_creates_wallets_and_prints_matching_lines_once_writing_nothing(self):
        r, state, left = self.run_setup('--chain', 'regtest')
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(left, [])  # nothing written where it ran
        arb, watch = state['wallets']['opsecmkt-arbiter'], state['wallets']['opsecmkt-escrow-watch']
        self.assertEqual(arb['kv'], {'wallet_name': 'opsecmkt-arbiter', 'load_on_startup': 'true'})
        self.assertEqual(watch['kv'], {'wallet_name': 'opsecmkt-escrow-watch', 'disable_private_keys': 'true',
                                       'blank': 'true', 'load_on_startup': 'true'})
        self.assertTrue(all(c[0] == '-chain=regtest' for c in state['calls']))
        self.assertIn('master key fingerprint d34db33f', r.stderr)
        lines = self.lines(r.stdout)
        auth = [l for l in lines if l.startswith('rpcauth=')]
        self.assertEqual(len(auth), 2)
        self.assertIn('rpcwhitelistdefault=0', lines)
        self.assertIn('rpcwhitelist=opsecmkt-escrow:' + ','.join(ESCROW_METHODS), lines)
        self.assertIn('rpcwhitelist=opsecmkt-pooled:' + ','.join(POOLED_METHODS), lines)
        self.assertEqual(len([l for l in lines if l.startswith('rpcwhitelist=')]), 2)
        urls = dict(l.split('=', 1) for l in lines if l.startswith(('ESCROW_BTC_', 'BITCOIN_RPC_URL')))
        self.assertEqual(urls['ESCROW_BTC_ARBITER_WALLET'], 'opsecmkt-arbiter')
        self.assertEqual(urls['ESCROW_BTC_WATCH_WALLET'], 'opsecmkt-escrow-watch')
        for user, key in (('opsecmkt-escrow', 'ESCROW_BTC_RPC_URL'), ('opsecmkt-pooled', 'BITCOIN_RPC_URL')):
            m = re.fullmatch(r'http://%s:([0-9a-f]{64})@bitcoin:18443' % user, urls[key])
            self.assertTrue(m, urls[key])
            line = next(l for l in auth if l.startswith('rpcauth=%s:' % user))
            salt, digest = line.split(':', 1)[1].split('$')
            self.assertRegex(salt, '^[0-9a-f]{32}$')
            # Core's share/rpcauth/rpcauth.py: HMAC-SHA256 keyed with the salt over the password.
            self.assertEqual(digest, hmac.new(salt.encode(), m.group(1).encode(), hashlib.sha256).hexdigest())
            self.assertNotIn(m.group(1), r.stderr)
        # A second run never repeats a password.
        r2, _, _ = self.run_setup('--chain', 'regtest')
        self.assertNotEqual(re.findall('[0-9a-f]{64}@', r.stdout), re.findall('[0-9a-f]{64}@', r2.stdout))

    def test_chain_ports_host_and_no_pooled(self):
        for chain, port in (('signet', 38332), ('testnet4', 48332)):
            r, _, _ = self.run_setup('--chain', chain, '--rpc-host', 'escrow-node', '--no-pooled', chain=chain, challenge=PUBLIC_SIGNET)
            self.assertEqual(r.returncode, 0, r.stderr)
            self.assertRegex(r.stdout, r'(?m)^ESCROW_BTC_RPC_URL=http://opsecmkt-escrow:[0-9a-f]{64}@escrow-node:%d$' % port)
            self.assertNotIn('opsecmkt-pooled', r.stdout)
            self.assertNotIn('BITCOIN_RPC_URL=', r.stdout)
        self.assertIn("signet_challenge is public signet's", self.run_setup('--chain', 'signet', chain='signet', challenge=PUBLIC_SIGNET)[0].stderr)
        self.assertIn('NOT public signet', self.run_setup('--chain', 'signet', chain='signet', challenge='51')[0].stderr)

    def test_existing_wallets_are_checked_and_kept_never_recreated(self):
        good = {'opsecmkt-arbiter': {'loaded': True, 'keys': True, 'encrypted': False},
                'opsecmkt-escrow-watch': {'loaded': False, 'keys': False, 'encrypted': False}}
        r, state, _ = self.run_setup('--chain', 'regtest', wallets=good)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertNotIn('createwallet', [c[-1] for c in state['calls']] + sum(state['calls'], []))
        self.assertIn(['-chain=regtest', 'loadwallet', 'opsecmkt-escrow-watch', 'true'], state['calls'])
        for name, bad, why in (('opsecmkt-arbiter', {'encrypted': True}, 'is encrypted'),
                               ('opsecmkt-arbiter', {'keys': False}, 'private keys disabled'),
                               ('opsecmkt-escrow-watch', {'keys': True}, 'holds private keys')):
            wallets = json.loads(json.dumps(good))
            wallets[name].update(bad)
            r, _, _ = self.run_setup('--chain', 'regtest', wallets=wallets)
            self.assertEqual(r.returncode, 1, (name, bad))
            self.assertIn(why, r.stderr)
            self.assertNotIn('rpcauth=', r.stdout)  # no password is shown for a node that failed its checks

    def test_verify_passes_with_the_documented_whitelists(self):
        self.server.chain = 'regtest'
        env = {'ESCROW_BTC_RPC_URL': self.url('opsecmkt-escrow', 'escrow-not-real-1'),
               'BITCOIN_RPC_URL': self.url('opsecmkt-pooled', 'pooled-not-real-2')}
        r = subprocess.run(['bash', str(SETUP), '--chain', 'regtest', '--verify'], env=dict(os.environ, **env),
                           capture_output=True, text=True, timeout=60)
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertIn('PASS', r.stdout)
        for probe in ('listdescriptors', 'gethdkeys', 'walletprocesspsbt'):
            self.assertIn(('opsecmkt-pooled', '/wallet/opsecmkt-arbiter', probe), self.server.calls)
        self.assertNotIn('not-real', r.stdout + r.stderr)

    def test_verify_fails_when_the_pooled_user_can_read_the_arbiter_wallet(self):
        self.server.chain = 'regtest'
        self.server.whitelists['opsecmkt-pooled'] = None  # unrestricted, as the guide used to leave it
        env = {'ESCROW_BTC_RPC_URL': self.url('opsecmkt-escrow', 'x1'), 'BITCOIN_RPC_URL': self.url('opsecmkt-pooled', 'x2')}
        r = subprocess.run(['bash', str(SETUP), '--chain', 'regtest', '--verify'], env=dict(os.environ, **env),
                           capture_output=True, text=True, timeout=60)
        self.assertEqual(r.returncode, 1)
        self.assertIn('FAILED: pooled user: listdescriptors on opsecmkt-arbiter refused', r.stderr)
        self.assertIn('FAILED: pooled user: gethdkeys', r.stderr)
        # walletprocesspsbt allowed: the empty PSBT answers -22, which is not a refusal.
        self.assertIn('FAILED: pooled user: walletprocesspsbt', r.stderr)

    def test_verify_refuses_mainnet_and_a_widened_escrow_user(self):
        self.server.chain = 'main'
        env = {'ESCROW_BTC_RPC_URL': self.url('opsecmkt-escrow', 'x1')}
        r = subprocess.run(['bash', str(SETUP), '--chain', 'regtest', '--verify'], env=dict(os.environ, **env),
                           capture_output=True, text=True, timeout=60)
        self.assertEqual(r.returncode, 1)
        self.assertIn('Refusing: the escrow node reports chain main', r.stderr)
        self.server.chain = 'regtest'
        self.server.whitelists['opsecmkt-escrow'] = set(ESCROW_METHODS) | {'listdescriptors'}
        r = subprocess.run(['bash', str(SETUP), '--chain', 'regtest', '--verify'], env=dict(os.environ, **env),
                           capture_output=True, text=True, timeout=60)
        self.assertEqual(r.returncode, 1)
        self.assertIn('FAILED: escrow user: listdescriptors on opsecmkt-arbiter refused with HTTP 403', r.stderr)

    def test_verify_refuses_another_test_chain_or_none_before_any_wallet_call(self):
        """QA5W3A-9: as set-up mode does, --verify asks nothing more of a node on another test chain, or of one that
        names no chain (its getblockchaininfo refused with HTTP 403, or no node answering at all)."""
        env = {'ESCROW_BTC_RPC_URL': self.url('opsecmkt-escrow', 'x1'), 'BITCOIN_RPC_URL': self.url('opsecmkt-pooled', 'x2')}
        for name, node_chain, whitelist, url, why in (
                ('another test chain', 'signet', None, None, "reports chain 'signet' (HTTP 200), not regtest"),
                ('getblockchaininfo refused', 'regtest', set(ESCROW_METHODS) - {'getblockchaininfo'}, None,
                 "reports chain 'nothing' (HTTP 403), not regtest"),
                ('no node answering', 'regtest', None, 'http://opsecmkt-escrow:x1@127.0.0.1:9', "reports chain 'nothing'")):
            self.server.chain, self.server.calls = node_chain, []
            self.server.whitelists['opsecmkt-escrow'] = whitelist or set(ESCROW_METHODS)
            r = subprocess.run(['bash', str(SETUP), '--chain', 'regtest', '--verify'],
                               env={**os.environ, **env, **({'ESCROW_BTC_RPC_URL': url} if url else {})},
                               capture_output=True, text=True, timeout=60)
            self.assertEqual(r.returncode, 1, (name, r.stdout, r.stderr))
            self.assertIn('Refusing: the escrow node ' + why, r.stderr, name)
            self.assertIn('nothing else was asked', r.stderr, name)
            self.assertEqual([c for c in self.server.calls if c[2] != 'getblockchaininfo'], [], name)
            self.assertNotIn('PASS', r.stdout, name)


FAKE_GO ='''#!/usr/bin/env bash
if [ "$1" = version ]; then echo "go version go1.27.1 linux/amd64"; exit 0; fi
touch "$FAKE_GO_RAN"
env > "$FAKE_GO_ENV"
cat "$FAKE_GO_OUTPUT"
exit "$FAKE_GO_STATUS"
'''
FAKE_DOCKER = '''#!/usr/bin/env bash
[ "$1 $2" = "image inspect" ] || exit 1
echo "$FAKE_DIGEST"
'''


def passing_output(extra=()):
    lines = ['=== RUN   TestEscrowPublicSmoke',
             '    escrow_public_test.go:180: escrow-public: escrow on; arbiter wallet escrow-smoke-1234abcd-arbiter',
             '    escrow_public_test.go:211: escrow-public: leg practice-sign passed (buyer and vendor keys activated)']
    for (leg, role), txid in TXIDS.items():
        if role == 'funding':
            lines.append('    escrow_public_test.go:243: escrow-public: txid %s %s %s' % (leg, role, txid))
    for (leg, role), txid in TXIDS.items():
        if role == 'settlement':
            lines.append('    escrow_public_test.go:300: escrow-public: txid %s %s %s' % (leg, role, txid))
    lines += ['    escrow_public_test.go:350: escrow-public: leg claim passed (confirmed)',
              '    escrow_public_test.go:352: escrow-public: leg refund passed (confirmed)'] + list(extra)
    lines += ['--- PASS: TestEscrowPublicSmoke (1234.56s)', 'PASS', 'ok  \topsecmkt/internal/market\t1235.000s']
    return lines


class PublicScriptTest(NodeCase):
    def setUp(self):
        super().setUp()
        self.tmp = tempfile.mkdtemp()
        self.repo = Path(self.tmp, 'repo')
        (self.repo / 'scripts').mkdir(parents=True)
        shutil.copy(PUBLIC, self.repo / 'scripts' / 'test-escrow-public.sh')
        Path(self.repo, '.gitignore').write_text('artifacts/\n')
        git = ['git', '-C', str(self.repo), '-c', 'user.name=t', '-c', 'user.email=t@example.invalid']
        subprocess.run(['git', 'init', '-q', str(self.repo)], check=True)
        subprocess.run(git + ['add', '.'], check=True)
        subprocess.run(git + ['commit', '-qm', 'fixture'], check=True)
        self.commit = subprocess.run(['git', '-C', str(self.repo), 'rev-parse', 'HEAD'], capture_output=True, text=True).stdout.strip()
        self.bin = Path(self.tmp, 'bin')
        self.bin.mkdir()
        for name, text in (('go', FAKE_GO), ('docker', FAKE_DOCKER)):
            Path(self.bin, name).write_text(text)
            Path(self.bin, name).chmod(0o755)
        self.passwords = ['operator-not-real-3141', 'escrow-not-real-2718', 'db-not-real-1618']

    def tearDown(self):
        shutil.rmtree(self.tmp)

    def run_public(self, output=None, status=0, **extra):
        out = Path(self.tmp, 'go-output')
        out.write_text(''.join(l + '\n' for l in (output if output is not None else passing_output())))
        ran = Path(self.tmp, 'go-ran')
        if ran.exists():
            ran.unlink()
        env = dict(os.environ, PATH=str(self.bin) + os.pathsep + os.environ['PATH'], FAKE_GO_OUTPUT=str(out),
                   FAKE_GO_STATUS=str(status), FAKE_GO_RAN=str(ran), FAKE_GO_ENV=str(Path(self.tmp, 'go-env')),
                   FAKE_DIGEST=DIGEST,
                   ESCROW_PUBLIC_OPERATOR_RPC_URL=self.url('operator', self.passwords[0]),
                   ESCROW_PUBLIC_APP_RPC_URL=self.url('opsecmkt-escrow', self.passwords[1]),
                   ESCROW_PUBLIC_FUNDING_WALLET='funding', ESCROW_PUBLIC_NODE_IMAGE='bitcoin/bitcoin:31.1',
                   TEST_DATABASE_URL='postgres://smoke:%s@127.0.0.1:5432/disposable?sslmode=disable' % self.passwords[2])
        env.pop('ESCROW_PUBLIC_LOG', None)
        env.update(extra)
        r = subprocess.run(['bash', str(self.repo / 'scripts' / 'test-escrow-public.sh')], env=env,
                           capture_output=True, text=True, timeout=60)
        return r.returncode, r.stdout + r.stderr, ran.exists()

    def records(self):
        root = self.repo / 'docs' / 'evidence' / 'public-testnet'
        return sorted(p for p in root.rglob('*') if p.is_file()) if root.exists() else []

    def test_refuses_regtest_mainnet_testnet3_and_a_private_signet_before_go_runs(self):
        for chain, challenge in (('regtest', ''), ('main', ''), ('test', ''), ('signet', '51'), ('signet', ''),
                                 ('signet', PUBLIC_SIGNET[:-2] + '00'), ('testnet3', '')):
            self.server.chain, self.server.challenge = chain, challenge
            code, text, ran = self.run_public()
            self.assertEqual(code, 1, (chain, challenge, text))
            self.assertIn('Refusing: the node at ESCROW_PUBLIC_OPERATOR_RPC_URL reports chain %s' % chain, text)
            self.assertFalse(ran, 'go ran against %s' % chain)
            self.assertEqual(self.records(), [])
            self.assertEqual([c for c in self.server.calls if c[2] != 'getblockchaininfo'], [])

    def test_refuses_an_unreachable_node(self):
        code, text, ran = self.run_public(ESCROW_PUBLIC_APP_RPC_URL='http://u:p@127.0.0.1:9')
        self.assertEqual(code, 1)
        self.assertIn('ESCROW_PUBLIC_APP_RPC_URL did not answer', text)
        self.assertFalse(ran)

    def test_refuses_urls_reaching_different_chains(self):
        other = HTTPServer(('127.0.0.1', 0), FakeNode)
        other.chain, other.challenge, other.height, other.calls = 'testnet4', '', 1, []
        other.whitelists, other.wallets = {}, {}
        threading.Thread(target=other.serve_forever, daemon=True).start()
        try:
            code, text, ran = self.run_public(ESCROW_PUBLIC_APP_RPC_URL='http://a:b@127.0.0.1:%d' % other.server_port)
        finally:
            other.shutdown()
            other.server_close()
        self.assertEqual(code, 1)
        self.assertIn('different chains', text)
        self.assertFalse(ran)

    def test_writes_the_manifest_on_public_signet_and_testnet4(self):
        for chain, challenge in (('signet', PUBLIC_SIGNET), ('testnet4', '')):
            self.server.chain, self.server.challenge = chain, challenge
            code, text, ran = self.run_public()
            self.assertEqual(code, 0, text)
            self.assertTrue(ran)
            files = [p for p in self.records() if p.parent.name == chain]
            self.assertEqual(sorted(p.name for p in files), ['manifest.json', 'transcript.log'])
            folder = files[0].parent
            self.assertRegex(folder.parent.name, r'^\d{4}-\d{2}-\d{2}-%s$' % self.commit[:12])
            m = json.loads((folder / 'manifest.json').read_text())
            self.assertEqual(m['commit'], self.commit)
            self.assertEqual(m['chain'], chain)
            self.assertEqual(m['signet_challenge'], challenge)
            self.assertEqual(m['network'], 'public ' + chain)
            self.assertEqual(m['node'], {'version': '/Satoshi:31.1.0/', 'image': 'bitcoin/bitcoin:31.1', 'image_digest': DIGEST})
            self.assertEqual(m['heights'], {'start': 250000, 'end': 250000})
            self.assertEqual(m['confirmations'], 1)
            self.assertEqual(m['fee_rate_sat_vb'], 2)
            self.assertEqual(m['go'], 'go version go1.27.1 linux/amd64')
            for leg in ('claim', 'refund'):
                self.assertEqual(m['legs'][leg], {'result': 'passed', 'txids': {r: TXIDS[(leg, r)] for r in ('funding', 'settlement')}})
            self.assertEqual(m['legs']['practice-sign'], {'result': 'passed', 'txids': {}})
            transcript = (folder / 'transcript.log').read_bytes()
            self.assertEqual(m['transcript'], {'file': 'transcript.log', 'sha256': hashlib.sha256(transcript).hexdigest()})
            self.assertRegex(m['started'], r'^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$')
            # The secret scan over the produced record: no password, rpcauth salt or hash, passphrase or cookie.
            for p in files:
                self.assertEqual(scan_for_secrets(p.read_text(), self.passwords), [], p)
        # The go test got the run's settings and the URLs, from this script's environment.
        goenv = Path(self.tmp, 'go-env').read_text()
        self.assertIn('ESCROW_PUBLIC_CONFIRMATIONS=1\n', goenv)
        self.assertIn('ESCROW_PUBLIC_FEE_RATE=2\n', goenv)
        # A second run on the same day and commit never overwrites a record.
        code, text, ran = self.run_public()
        self.assertEqual(code, 1)
        self.assertIn('exists (a record is never overwritten)', text)
        self.assertFalse(ran)

    def test_confirmations_and_fee_rate_are_configurable_and_bounded(self):
        code, text, _ = self.run_public(ESCROW_PUBLIC_CONFIRMATIONS='3', ESCROW_PUBLIC_FEE_RATE='5')
        self.assertEqual(code, 0, text)
        m = json.loads(next(p for p in self.records() if p.name == 'manifest.json').read_text())
        self.assertEqual((m['confirmations'], m['fee_rate_sat_vb']), (3, 5))
        self.assertIn('ESCROW_PUBLIC_CONFIRMATIONS=3\n', Path(self.tmp, 'go-env').read_text())
        for bad in ({'ESCROW_PUBLIC_CONFIRMATIONS': '0'}, {'ESCROW_PUBLIC_CONFIRMATIONS': '101'},
                    {'ESCROW_PUBLIC_FEE_RATE': 'x'}, {'ESCROW_PUBLIC_WAIT_MINUTES': '5'}):
            code, text, ran = self.run_public(**bad)
            self.assertEqual(code, 2, bad)
            self.assertFalse(ran)

    def test_a_leaked_secret_in_the_transcript_writes_no_record(self):
        leaks = ['    app.go:1: dialing http://operator:%s@127.0.0.1:38332' % self.passwords[0],
                 '    leaked the database password %s' % self.passwords[2],
                 '    rpcauth=opsecmkt-escrow:' + '0f' * 16 + '$' + 'ab' * 32,
                 '    salt and hash ' + '0f' * 16 + '$' + 'ab' * 32,
                 '    rpcpassword=hunter2',
                 '    walletpassphrase "correct horse" 60',
                 '    read /home/bitcoin/.bitcoin/signet/.cookie',
                 '    __cookie__:' + 'ab' * 32,
                 '    tprv8ZgxMBicQKsPd' + 'A' * 94,
                 '    wif cVt4o7BGAig1UXywgGSmARhxMdzP5qvQsxKkSsc1XEkw3tDTQFpy']
        for leak in leaks:
            code, text, ran = self.run_public(passing_output([leak]))
            self.assertEqual(code, 1, leak)
            self.assertIn('secret scan', text)
            self.assertEqual(self.records(), [], leak)
            for p in self.passwords:
                self.assertNotIn(p, text.replace(leak, ''))  # the report names what was found, never the secret

    def test_a_failed_skipped_or_incomplete_run_writes_no_record(self):
        for output, status in ((passing_output()[:-3] + ['--- FAIL: TestEscrowPublicSmoke (9.00s)', 'FAIL'], 1),
                               (['--- SKIP: TestEscrowPublicSmoke (0.00s)', 'PASS'], 0),
                               ([l for l in passing_output() if 'leg refund passed' not in l], 0),
                               ([l for l in passing_output() if 'txid claim settlement' not in l], 0),
                               (passing_output(), 1)):
            code, text, _ = self.run_public(output, status)
            self.assertEqual(code, 1, text)
            self.assertEqual(self.records(), [])

    def test_a_dirty_checkout_is_refused(self):
        Path(self.repo, 'scripts', 'test-escrow-public.sh').write_text(PUBLIC.read_text() + '\n# local edit\n')
        code, text, ran = self.run_public()
        self.assertEqual(code, 1)
        self.assertIn('uncommitted changes', text)
        self.assertFalse(ran)


class ManifestWriterTest(unittest.TestCase):
    """write_manifest driven directly with fixture values (the script sourced, main not run)."""

    FIELDS = {'M_COMMIT': 'f' * 40, 'M_STARTED': '2026-10-07T01:02:03Z', 'M_FINISHED': '2026-10-07T03:04:05Z',
              'M_CHAIN': 'signet', 'M_CHALLENGE': PUBLIC_SIGNET, 'M_NODE_VERSION': '/Satoshi:31.1.0/',
              'M_NODE_IMAGE': 'bitcoin/bitcoin:31.1', 'M_NODE_DIGEST': DIGEST, 'M_GO_VERSION': 'go version go1.27.1 linux/amd64',
              'M_CONFIRMATIONS': '2', 'M_FEE_RATE': '3', 'M_START_HEIGHT': '271000', 'M_END_HEIGHT': '271004'}

    def write(self, transcript_lines, **override):
        with tempfile.TemporaryDirectory() as tmp:
            log = Path(tmp, 'transcript')
            log.write_text(''.join(l + '\n' for l in transcript_lines))
            out = Path(tmp, 'record', 'signet')
            env = dict(os.environ, **dict(self.FIELDS, **override))
            r = subprocess.run(['bash', '-c', 'source "$1"; write_manifest "$2" "$3"', 'bash', str(PUBLIC), str(out), str(log)],
                               env=env, capture_output=True, text=True, timeout=30)
            manifest = json.loads((out / 'manifest.json').read_text()) if (out / 'manifest.json').exists() else None
            return r, manifest, out.exists()

    def test_fixture_manifest_fields(self):
        r, m, _ = self.write(passing_output())
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(set(m), {'record', 'network', 'commit', 'started', 'finished', 'chain', 'signet_challenge', 'node',
                                  'go', 'confirmations', 'fee_rate_sat_vb', 'heights', 'legs', 'transcript', 'secret_scan'})
        self.assertEqual(m['heights'], {'start': 271000, 'end': 271004})
        self.assertEqual(m['legs']['claim']['txids']['settlement'], TXIDS[('claim', 'settlement')])
        self.assertEqual(m['legs']['refund']['txids']['funding'], TXIDS[('refund', 'funding')])
        self.assertEqual(scan_for_secrets(json.dumps(m), []), [])

    def test_refuses_a_non_public_chain_or_a_malformed_value(self):
        for override in ({'M_CHAIN': 'regtest', 'M_CHALLENGE': ''}, {'M_CHAIN': 'main', 'M_CHALLENGE': ''},
                         {'M_CHAIN': 'test', 'M_CHALLENGE': ''}, {'M_CHALLENGE': '51'},
                         {'M_COMMIT': 'abc'}, {'M_NODE_DIGEST': 'bitcoin/bitcoin:31.1'}, {'M_START_HEIGHT': '12a'},
                         {'M_NODE_VERSION': '/Satoshi:31.1.0/"injected'}, {'M_END_HEIGHT': '270999'}):
            r, m, exists = self.write(passing_output(), **override)
            self.assertEqual(r.returncode, 1, override)
            self.assertFalse(exists, override)

    def test_refuses_a_transcript_holding_a_secret(self):
        # The last: a URL carrying a password this run was never given (QA5W3A-10), so only the URL pattern can catch it.
        for leak in ('rpcauth=u:' + '1a' * 16 + '$' + '2b' * 32, 'passphrase=secret', 'x __cookie__ y',
                     'dialling http://operator:n0t-given-to-this-run@10.0.0.5:38332/ again'):
            r, m, exists = self.write(passing_output([leak]))
            self.assertEqual(r.returncode, 1, leak)
            self.assertIn('secret scan', r.stderr)
            self.assertFalse(exists)


if __name__ == '__main__':
    unittest.main()
