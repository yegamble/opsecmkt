#!/usr/bin/env python3
"""A-457 manual-test bridge: two BIP39 Electrum parties, keys held only in this process.

Run only from test-escrow-wallets.sh with its verified Electrum source and throwaway venv.
One JSON request and response per line on stdin/stdout. Never print or write roots,
mnemonics, private extended keys, or wallet files.
"""

import hashlib
import json
import os
import secrets
import sys
import tempfile

SRC = os.environ.get('ESCROW_WALLETS_ELECTRUM_SRC', '')
if not SRC:
    raise SystemExit('Run through scripts/test-escrow-wallets.sh')
sys.path.extend([SRC, os.path.join(SRC, 'packages')])

from electrum import constants, version  # noqa: E402
constants.BitcoinRegtest.set_as_network()

from electrum import bip32, keystore, util  # noqa: E402
from electrum.mnemonic import Mnemonic  # noqa: E402
from electrum.simple_config import SimpleConfig  # noqa: E402
from electrum.transaction import tx_from_any  # noqa: E402
from electrum.wallet import Wallet  # noqa: E402
from electrum.wallet_db import WalletDB  # noqa: E402
import electrum_ecc  # noqa: E402

util.create_and_start_event_loop()
if version.ELECTRUM_VERSION != os.environ.get('ESCROW_WALLETS_ELECTRUM_VERSION') or electrum_ecc.__file__.startswith(SRC):
    raise SystemExit('Refusing: Electrum release or compiled signing library differs from the verified installation')

H = 0x80000000
ACCOUNT = [48 | H, 1 | H, 0 | H, 2 | H]
ROOTS = {}


def bip39_words():
    entropy = secrets.token_bytes(16)
    checksum = hashlib.sha256(entropy).digest()[0] >> 4
    bits = (int.from_bytes(entropy, 'big') << 4) | checksum
    words = Mnemonic('en').wordlist
    return ' '.join(words[(bits >> (121 - i * 11)) & 2047] for i in range(12))


def create_party(name):
    phrase = bip39_words()
    if not keystore.bip39_is_checksum_valid(phrase)[0]:
        raise ValueError('BIP39 checksum')
    rootseed = keystore.bip39_to_seed(phrase, passphrase='')
    root = bip32.BIP32Node.from_rootseed(rootseed, xtype='standard')
    account = root.subkey_at_private_derivation(ACCOUNT)._replace(xtype='p2wsh')
    ROOTS[name] = rootseed
    return '[' + root.calc_fingerprint_of_this_node().hex() + '/48h/1h/0h/2h]' + account.to_xpub()


KEYS = {name: create_party(name) for name in ('buyer', 'vendor')}


def sign(req):
    name = req['party']
    if name not in ROOTS:
        raise ValueError('unknown party')
    index = req['index']
    if type(index) is not int or not 2 <= index < 2**31:
        raise ValueError('invalid per-order index')
    cosigners = req['cosigners']
    if len(cosigners) != 2 or not all(isinstance(x, str) and x.startswith('Vpub') for x in cosigners):
        raise ValueError('two rooted Vpub cosigners required')
    path = 'm/48h/1h/0h/2h/' + str(index)
    db = WalletDB('', storage=None, upgrade=True)
    db.put('wallet_type', '2of3')
    own = keystore.from_bip43_rootseed(ROOTS[name], derivation=path, xtype='p2wsh')
    for n, ks in enumerate((own, keystore.from_xpub(cosigners[0]), keystore.from_xpub(cosigners[1])), 1):
        db.put('x%d' % n, ks.dump())
    # WalletDB and SimpleConfig hold the wallet only in memory. This temporary directory is empty.
    with tempfile.TemporaryDirectory() as tmp:
        wallet = Wallet(db, config=SimpleConfig({'electrum_path': tmp, 'offline': True}))
        branch, child = req.get('branch', 0), req.get('child', 0)
        if branch != 0 or type(child) is not int or not 0 <= child < 20:
            raise ValueError('invalid receiving address')
        addresses = wallet.get_receiving_addresses()
        if len(addresses) <= child or addresses[child] != req['address']:
            raise ValueError('Electrum wallet address differs from the market page')
        tx = tx_from_any(req['psbt'])
        wallet.sign_transaction(tx, None)
        return {'psbt': __import__('base64').b64encode(tx.serialize_as_bytes()).decode(),
                'address': addresses[child]}


for line in sys.stdin:
    try:
        req = json.loads(line)
        if req.get('op') == 'keys':
            answer = {'keys': KEYS, 'version': version.ELECTRUM_VERSION}
        elif req.get('op') == 'sign':
            answer = sign(req)
        else:
            answer = {'error': 'unknown operation'}
    except Exception as exc:
        # Keep failures actionable without ever echoing a request, seed, or library exception text.
        answer = {'error': type(exc).__name__}
    print(json.dumps(answer, separators=(',', ':')), flush=True)

# Electrum's background event-loop thread can outlive stdin. The bridge owns only throwaway in-memory wallets; exit
# immediately at EOF so the test can reap it without waiting for Electrum's application shutdown machinery.
os._exit(0)
