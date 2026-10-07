#!/usr/bin/env python3
"""Wallet legs of scripts/test-escrow-wallets.sh (A-450, ruling P4-R12): real Bitcoin Core regtest plus a real
Electrum used offline as a library. Manual, never in CI; run it only through that script, which verifies the Electrum
release, installs it into a throwaway venv, starts a regtest node with no network and removes everything afterwards.

Legs (each prints PASS or FAIL with its evidence):
  core-shape-a      a Core party wallet holding a BIP48 account signs the shape (a) escrow PSBT the market serves
                    today (bare cosigner child keys, the party's own origin only); the market's Core wallet signs its
                    copy; combinepsbt (market first, then the stored PSBT plus only the party's signatures),
                    finalizepsbt, testmempoolaccept and sendrawtransaction succeed with the unsigned txid.
  electrum-shape-b  an Electrum keystore restored from a fresh BIP39 mnemonic at m/48h/1h/0h/2h/i signs the per-order
                    (shape (b)) variants: bare cosigners; its own origin only; every derivation with rooted global
                    xpubs. The market signs its pristine stored copy through Core descriptorprocesspsbt (per-order
                    tprv, sighash ALL); each variant then combines, finalizes and passes testmempoolaccept, one is
                    broadcast and confirmed, and every final txid equals the unsigned one.
  electrum-seed-type Electrum's own seed type ("Create a new seed", segwit) restored as a multisig keystore derives at
                    m/1h, and keystore.from_seed takes no derivation: such a wallet cannot follow a per-order path.
The ES-2 leg (the paste check accepting the Electrum output since A-456b relaxed P4-R13) runs in Go from the files this writes (go test
-run TestEscrowWalletFixtures with ESCROW_WALLETS_FIXTURES set).

Writes to ESCROW_WALLETS_OUT: the signed PSBTs (base64; regtest, throwaway keys, no seed or extended private key) and
manifest.json naming, for each, the public keys the Go check needs. Seeds, mnemonics and private keys stay in memory
and on the throwaway node; none is printed or written.

Environment: ESCROW_WALLETS_CONTAINER (the regtest container), ESCROW_WALLETS_ELECTRUM_SRC (the verified, extracted
Electrum source tree), ESCROW_WALLETS_OUT (an empty directory).
"""
import base64
import hashlib
import inspect
import io
import json
import os
import re
import secrets
import struct
import subprocess
import sys
import tempfile
import traceback

CONTAINER = os.environ.get('ESCROW_WALLETS_CONTAINER', '')
SRC = os.environ.get('ESCROW_WALLETS_ELECTRUM_SRC', '')
OUT = os.environ.get('ESCROW_WALLETS_OUT', '')
H = 0x80000000
ACCOUNT = [48 | H, 1 | H, 0 | H, 2 | H]  # BIP48 testnet account 0, script type 2 (p2wsh)
ORDER_INDEX = 7  # the per-order index i of shape (b): one shared counter, unhardened (ruling P4-R2)


# --- Bitcoin Core through docker exec (the node has no network, so no RPC port is published).

class RPCError(Exception):
    pass


def cli(*args, wallet=None):
    cmd = ['docker', 'exec', '-u', 'bitcoin', CONTAINER, 'bitcoin-cli', '-regtest', '-datadir=/home/bitcoin/.bitcoin']
    if wallet:
        cmd.append('-rpcwallet=' + wallet)
    cmd += [a if isinstance(a, str) else json.dumps(a) for a in args]
    r = subprocess.run(cmd, capture_output=True, text=True, timeout=300)
    if r.returncode:
        # Never echo the arguments, and redact any extended private key Core quotes back: a descriptor may carry a
        # throwaway tprv.
        raise RPCError('%s: %s' % (args[0], re.sub(r'[tx]prv[1-9A-HJ-NP-Za-km-z]+', '<redacted>', r.stderr.strip()[:300])))
    s = r.stdout.strip()
    try:
        return json.loads(s)
    except ValueError:
        return s


def checksummed(desc):
    return cli('getdescriptorinfo', desc)['descriptor']


def with_checksum(desc):
    return desc + '#' + cli('getdescriptorinfo', desc)['checksum']


# --- A minimal BIP 174 (version 0) map editor.

def _rd_cs(f):
    n = f.read(1)[0]
    if n < 0xfd:
        return n
    return struct.unpack({0xfd: '<H', 0xfe: '<I', 0xff: '<Q'}[n], f.read({0xfd: 2, 0xfe: 4, 0xff: 8}[n]))[0]


def _wr_cs(n):
    if n < 0xfd:
        return bytes([n])
    if n <= 0xffff:
        return b'\xfd' + struct.pack('<H', n)
    return b'\xfe' + struct.pack('<I', n)


def _rd_map(f):
    m = []
    while True:
        kl = _rd_cs(f)
        if kl == 0:
            return m
        k = f.read(kl)
        v = f.read(_rd_cs(f))
        m.append((k, v))


def _tx_counts(raw):
    f = io.BytesIO(raw)
    f.read(4)
    nin = _rd_cs(f)
    for _ in range(nin):
        f.read(36)
        f.read(_rd_cs(f))
        f.read(4)
    return nin, _rd_cs(f)


def psbt_parse(b64):
    raw = base64.b64decode(b64)
    assert raw[:5] == b'psbt\xff', 'not a PSBT'
    f = io.BytesIO(raw[5:])
    g = _rd_map(f)
    nin, nout = _tx_counts([v for k, v in g if k == b'\x00'][0])
    return g, [_rd_map(f) for _ in range(nin)], [_rd_map(f) for _ in range(nout)]


def psbt_ser(g, ins, outs):
    out = b'psbt\xff'
    for m in [g] + ins + outs:
        for k, v in sorted(m):
            out += _wr_cs(len(k)) + k + _wr_cs(len(v)) + v
        out += b'\x00'
    return base64.b64encode(out).decode()


def deriv_value(fp, path):
    return bytes.fromhex(fp) + b''.join(struct.pack('<I', s) for s in path)


def path_text(path):
    return 'm/' + '/'.join('%d%s' % (s & ~H, 'h' if s & H else '') for s in path) if path else 'm'


def describe(b64, labels):
    """The fields of a PSBT, one per line, keys named by labels (pubkey hex -> name)."""
    g, ins, outs = psbt_parse(b64)
    lab = lambda pk: labels.get(pk.hex(), pk.hex()[:16] + '..')
    lines = []
    for k, v in g:
        if k[0] == 1:
            lines.append('  global XPUB depth=%d fp=%s origin=%s:%s' % (k[5], k[6:10].hex(), v[:4].hex(),
                                                                     path_text(struct.unpack('<%dI' % ((len(v) - 4) // 4), v[4:]))))
        elif k[0] != 0:
            lines.append('  global 0x%02x' % k[0])
    for i, m in enumerate(ins):
        for k, v in m:
            if k[0] == 6:
                lines.append('  in%d BIP32_DERIVATION key=%s %s:%s' % (i, lab(k[1:]), v[:4].hex(),
                                                                     path_text(struct.unpack('<%dI' % ((len(v) - 4) // 4), v[4:]))))
            elif k[0] == 2:
                lines.append('  in%d PARTIAL_SIG key=%s sighash=0x%02x' % (i, lab(k[1:]), v[-1]))
            else:
                lines.append('  in%d %s' % (i, {0: 'NON_WITNESS_UTXO', 1: 'WITNESS_UTXO', 3: 'SIGHASH_TYPE', 5: 'WITNESS_SCRIPT',
                                                 7: 'FINAL_SCRIPTSIG', 8: 'FINAL_SCRIPTWITNESS'}.get(k[0], '0x%02x' % k[0])))
    return '\n'.join(lines)


def strip_derivations(b64):
    g, ins, outs = psbt_parse(b64)
    return psbt_ser(g, [[(k, v) for k, v in m if k[0] != 6] for m in ins], outs)


def with_party_sigs(stored, signed, pub):
    """The stored PSBT plus only pub's partial signatures from signed (the market's withPartySigs)."""
    g, ins, outs = psbt_parse(stored)
    _, sins, _ = psbt_parse(signed)
    for m, sm in zip(ins, sins):
        sigs = [(k, v) for k, v in sm if k[0] == 2 and k[1:] == pub]
        assert len(sigs) == 1, 'the party did not sign every input'
        m.extend(sigs)
    return psbt_ser(g, ins, outs)


def signed_by(b64, pub):
    """Whether every input carries exactly one partial signature, pub's, ending in SIGHASH_ALL."""
    _, ins, _ = psbt_parse(b64)
    return all([(k[1:], v[-1]) for k, v in m if k[0] == 2] == [(pub, 1)] for m in ins)


def unsigned_txid(b64):
    return cli('decodepsbt', b64)['tx']['txid']


def settle(stored, market_signed, party_copy, broadcast):
    """combinepsbt (market first), finalizepsbt, testmempoolaccept and, if asked, sendrawtransaction plus one block:
    the final txid and a description."""
    combined = cli('combinepsbt', [market_signed, party_copy])
    fin = cli('finalizepsbt', combined)
    if not fin['complete']:
        raise AssertionError('finalizepsbt: not complete')
    tma = cli('testmempoolaccept', [fin['hex']])[0]
    if not tma['allowed']:
        raise AssertionError('testmempoolaccept: %s' % tma.get('reject-reason'))
    dec = cli('decoderawtransaction', fin['hex'])
    sighashes = sorted({w[1][-2:] for w in (vin['txinwitness'] for vin in dec['vin'])} |
                       {w[2][-2:] for w in (vin['txinwitness'] for vin in dec['vin'])})
    if dec['txid'] != unsigned_txid(stored):
        raise AssertionError('the final txid %s is not the unsigned %s' % (dec['txid'], unsigned_txid(stored)))
    text = 'combine (market first) + finalize complete, testmempoolaccept allowed (vsize %d), witness sighash bytes %s, final txid %s = unsigned txid' % (
        tma['vsize'], sighashes, dec['txid'])
    if broadcast:
        sent = cli('sendrawtransaction', fin['hex'])
        cli('generatetoaddress', 1, MINER_ADDR)
        conf = cli('gettxout', sent, 0)['confirmations']
        text += '; broadcast %s, %d confirmation' % (sent, conf)
    return dec['txid'], text


def fund(addr, amounts):
    ins = []
    for amt in amounts:
        txid = cli('sendtoaddress', addr, amt, wallet='miner')
        vout = [o['n'] for o in cli('getrawtransaction', txid, True)['vout'] if o['scriptPubKey'].get('address') == addr][0]
        ins.append({'txid': txid, 'vout': vout})
    cli('generatetoaddress', 1, MINER_ADDR)
    return ins


def build_stored(ins, bare_desc, total):
    """The market's stored copy: createpsbt, utxoupdatepsbt with the bare-key descriptor, Core's derivations removed
    (escrow_settle.go buildSettlement)."""
    claim = cli('getnewaddress', wallet='miner')
    psbt = cli('createpsbt', ins, [{claim: round(total - 0.0002, 8)}])
    return strip_derivations(cli('utxoupdatepsbt', psbt, [bare_desc]))


MANIFEST = []
MINER_ADDR = None


def write_fixture(name, b64, **fields):
    with open(os.path.join(OUT, name), 'w') as f:
        f.write(b64 + '\n')
    MANIFEST.append(dict(file=name, **fields))


# --- Leg 1: a Core party signs shape (a).

def leg_core_shape_a():
    s = secrets.token_hex(4)
    seed, party, vendor, arbiter = 'seed-' + s, 'party-' + s, 'vendor-' + s, 'arbiter-' + s
    for w in (seed, vendor, arbiter):
        cli('createwallet', w)
    cli('createwallet', party, False, True)  # blank: it holds only the BIP48 account, as in escrow_regtest_test.go
    root = [d['desc'] for d in cli('listdescriptors', True, wallet=seed)['descriptors']
            if d['desc'].startswith('wpkh(tprv') and not d['internal']][0][5:].split('/', 1)[0]
    imports = [{'desc': with_checksum('wpkh(%s/48h/1h/0h/2h/%d/*)' % (root, c)), 'active': True, 'internal': bool(c),
                'range': [0, 100], 'timestamp': 'now'} for c in (0, 1)]
    if not all(r['success'] for r in cli('importdescriptors', imports, wallet=party)):
        raise AssertionError('importdescriptors refused the BIP48 account')
    del root, imports
    account = [d['desc'] for d in cli('listdescriptors', wallet=party)['descriptors'] if not d['internal']][0]
    account = account[5:].split('/0/*', 1)[0]  # [fp/48h/1h/0h/2h]tpub…
    child = 5
    paddr = cli('deriveaddresses', checksummed('wpkh(%s/0/%d)' % (account, child)))[0]
    pinfo = cli('getaddressinfo', paddr, wallet=party)
    ppub = bytes.fromhex(pinfo['pubkey'])
    pfp = pinfo['hdmasterfingerprint']
    ppath = ACCOUNT + [0, child]
    if pinfo['hdkeypath'] not in ("m/48h/1h/0h/2h/0/%d" % child, "m/48'/1'/0'/2'/0/%d" % child):
        raise AssertionError('party key path %s' % pinfo['hdkeypath'])
    vpub = bytes.fromhex(cli('getaddressinfo', cli('getnewaddress', wallet=vendor), wallet=vendor)['pubkey'])
    minfo = cli('getaddressinfo', cli('getnewaddress', wallet=arbiter), wallet=arbiter)
    mpub, mfp = bytes.fromhex(minfo['pubkey']), minfo['hdmasterfingerprint']
    mpath = [int(x[:-1]) | H if x[-1] in "h'" else int(x) for x in minfo['hdkeypath'].split('/')[1:]]
    labels = {ppub.hex(): 'party', vpub.hex(): 'vendor', mpub.hex(): 'market'}
    bare = checksummed('wsh(sortedmulti(2,%s,%s,%s))' % (ppub.hex(), vpub.hex(), mpub.hex()))
    addr = cli('deriveaddresses', bare)[0]
    stored = build_stored(fund(addr, [0.3, 0.2]), bare, 0.5)
    g, ins, outs = psbt_parse(stored)
    copy = psbt_ser(g, [m + [(b'\x06' + ppub, deriv_value(pfp, ppath))] for m in ins], outs)  # settlementCopy
    signed = cli('walletprocesspsbt', copy, True, 'ALL', True, False, wallet=party)['psbt']
    print('core-shape-a: escrow %s (bare keys); party BIP48 origin %s:%s\n  served copy:\n%s\n  Core party wallet output:\n%s' % (
        addr, pfp, path_text(ppath), describe(copy, labels), describe(signed, labels)))
    if not signed_by(signed, ppub):
        raise AssertionError('the Core party wallet did not sign every input with its key and SIGHASH_ALL')
    g, ins, outs = psbt_parse(stored)
    mcopy = psbt_ser(g, [m + [(b'\x06' + mpub, deriv_value(mfp, mpath))] for m in ins], outs)
    msigned = cli('walletprocesspsbt', mcopy, True, 'ALL', True, False, wallet=arbiter)['psbt']
    if not signed_by(msigned, mpub):
        raise AssertionError('the market wallet did not sign')
    txid, text = settle(stored, msigned, with_party_sigs(stored, signed, ppub), broadcast=True)
    write_fixture('core-shape-a.psbt', signed, wallet='Bitcoin Core 31.1', shape='a', variant='own origin only',
                  party_pubkey=ppub.hex(), market_pubkey=mpub.hex(), market_fingerprint=mfp, txid=txid, want='accepted')
    return 'a Core party wallet (BIP48 account) signed the shape (a) copy; ' + text


# --- Leg 2: Electrum (BIP39 root keystore) signs the shape (b) variants.

def bip39_mnemonic(wordlist):
    entropy = secrets.token_bytes(16)
    bits = bin(int.from_bytes(entropy, 'big'))[2:].zfill(128) + bin(hashlib.sha256(entropy).digest()[0])[2:].zfill(8)[:4]
    return ' '.join(wordlist[int(bits[i:i + 11], 2)] for i in range(0, 132, 11))


def leg_electrum_shape_b(E):
    bip32, keystore, WalletDB, Wallet, SimpleConfig, tx_from_any, Mnemonic = (
        E['bip32'], E['keystore'], E['WalletDB'], E['Wallet'], E['SimpleConfig'], E['tx_from_any'], E['Mnemonic'])
    words = bip39_mnemonic(Mnemonic('en').wordlist)
    if not keystore.bip39_is_checksum_valid(words)[0]:
        raise AssertionError('the generated mnemonic is not a valid BIP39 mnemonic')
    roots = {'buyer': bip32.BIP32Node.from_rootseed(keystore.bip39_to_seed(words, passphrase=''), xtype='standard'),
             'vendor': bip32.BIP32Node.from_rootseed(secrets.token_bytes(32), xtype='standard'),
             'market': bip32.BIP32Node.from_rootseed(secrets.token_bytes(32), xtype='standard')}
    path = ACCOUNT + [ORDER_INDEX]
    ptext = path_text(path)
    fps = {p: n.calc_fingerprint_of_this_node().hex() for p, n in roots.items()}
    per = {p: n.subkey_at_private_derivation(path) for p, n in roots.items()}  # per-order nodes account/i
    tpub = {p: n.subkey_at_public_derivation([]).to_xpub() for p, n in per.items()}
    vpub = {p: n._replace(xtype='p2wsh').to_xpub() for p, n in per.items()}  # SLIP-132 Vpub, what Electrum imports
    child = {p: n.subkey_at_public_derivation([0, 0]) for p, n in per.items()}
    pk = {p: c.eckey.get_public_key_bytes(compressed=True) for p, c in child.items()}
    labels = {pk[p].hex(): p for p in pk}

    # The buyer's Electrum wallet: a 2-of-3 p2wsh wallet whose own keystore is restored from the BIP39 mnemonic at
    # the per-order path (what the wizard does for a BIP39 seed), the cosigners imported as bare per-order Vpubs.
    tmp = tempfile.mkdtemp()  # under the script's temporary directory, removed with it
    db = WalletDB('', storage=None, upgrade=True)
    db.put('wallet_type', '2of3')
    for i, p in enumerate(('buyer', 'vendor', 'market'), 1):
        ks = (keystore.from_bip43_rootseed(keystore.bip39_to_seed(words, passphrase=''), derivation=ptext, xtype='p2wsh')
              if p == 'buyer' else keystore.from_xpub(vpub[p]))
        db.put('x%d' % i, ks.dump())
    del words
    wallet = Wallet(db, config=SimpleConfig({'electrum_path': tmp, 'offline': True}))
    addr = wallet.get_receiving_addresses()[0]
    bare = checksummed('wsh(sortedmulti(2,%s,%s,%s))' % (pk['buyer'].hex(), pk['vendor'].hex(), pk['market'].hex()))
    if cli('deriveaddresses', bare)[0] != addr:
        raise AssertionError('the Electrum wallet address is not the bare-key escrow address')
    stored = build_stored(fund(addr, [0.6, 0.4]), bare, 1.0)
    txid0 = unsigned_txid(stored)
    print('electrum-shape-b: escrow %s = Electrum wallet /0/0 = Core bare descriptor; per-order path %s for every party' % (addr, ptext))

    def variant(own, foreign, gxpub):
        g, ins, outs = psbt_parse(stored)
        for m in ins:
            if own:
                m.append((b'\x06' + pk['buyer'], deriv_value(fps['buyer'], path + [0, 0])))
            if foreign:
                for p in ('vendor', 'market'):
                    m.append((b'\x06' + pk[p], deriv_value(per[p].calc_fingerprint_of_this_node().hex(), [0, 0])))
        if gxpub:
            for p in ('buyer', 'vendor', 'market'):
                node = bip32.BIP32Node.from_xkey(tpub[p])
                if p == 'buyer':
                    g.append((b'\x01' + node.to_bytes(), deriv_value(fps['buyer'], path)))
                else:  # rooted: depth 0, parent fingerprint 0, child 0 (ruling P4-R2)
                    node = node._replace(depth=0, fingerprint=b'\x00' * 4, child_number=b'\x00' * 4)
                    g.append((b'\x01' + node.to_bytes(), deriv_value(node.calc_fingerprint_of_this_node().hex(), [])))
        return psbt_ser(g, ins, outs)

    variants = [('bare', 'bare cosigners, no derivation at all', variant(False, False, False)),
                ('own-origin', "the party's own origin only, as the market serves its copies today", variant(True, False, False)),
                ('all-derivations-global-xpubs', 'a derivation for every key and rooted global xpubs', variant(True, True, True))]
    # The market signs its pristine stored copy through Core with the per-order tprv (the recorded signer design).
    mdesc = 'wsh(sortedmulti(2,%s/0/0,%s/0/0,%s/0/0))' % (tpub['buyer'], tpub['vendor'], per['market'].to_xprv())
    m = cli('descriptorprocesspsbt', stored, [mdesc], 'ALL', True, False)
    del mdesc
    if m['complete'] or not signed_by(m['psbt'], pk['market']):
        raise AssertionError('descriptorprocesspsbt did not add exactly the market signature')
    results = []
    for n, (name, what, served) in enumerate(variants):
        tx = tx_from_any(served)
        wallet.sign_transaction(tx, None)
        signed = base64.b64encode(tx.serialize_as_bytes()).decode()
        print('  variant %s (%s)\n  served:\n%s\n  Electrum 4.8.2 output:\n%s' % (name, what, describe(served, labels), describe(signed, labels)))
        if not signed_by(signed, pk['buyer']):
            raise AssertionError('Electrum did not sign variant %s on every input with SIGHASH_ALL' % name)
        if unsigned_txid(signed) != txid0:
            raise AssertionError('Electrum changed the transaction of variant %s' % name)
        txid, text = settle(stored, m['psbt'], with_party_sigs(stored, signed, pk['buyer']), broadcast=(n == len(variants) - 1))
        print('  variant %s: %s' % (name, text))
        write_fixture('electrum-shape-b-%s.psbt' % name, signed, wallet='Electrum 4.8.2', shape='b', variant=what,
                      party_pubkey=pk['buyer'].hex(), market_pubkey=pk['market'].hex(), market_fingerprint=fps['market'],
                      txid=txid, want='accepted')
        results.append(name)
    return ('Electrum (BIP39 root keystore at %s) signed variants %s; the market signed through descriptorprocesspsbt; '
            'each combined, finalized and passed testmempoolaccept, the last was broadcast and confirmed, txid %s unchanged') % (
        ptext, ', '.join(results), txid0)


# --- Leg 3: Electrum's own seed type derives at m/1h and takes no path.

def leg_electrum_seed_type(E):
    keystore, Mnemonic = E['keystore'], E['Mnemonic']
    seed = Mnemonic('en').make_seed(seed_type='segwit')
    ms = keystore.from_seed(seed, passphrase='', for_multisig=True)
    ss = keystore.from_seed(seed, passphrase='', for_multisig=False)
    seed_type = E['calc_seed_type'](seed)
    del seed
    params = list(inspect.signature(keystore.from_seed).parameters)
    lines = []
    for rel, pattern in (('electrum/keystore.py', r'^def from_seed|der = "m/1\'/" if for_multisig'),
                         ('electrum/wizard.py', r'def needs_derivation_path|seed_variant\'\] in \[\'bip39\', \'slip39\'\]')):
        with open(os.path.join(SRC, rel)) as f:
            for no, line in enumerate(f, 1):
                if re.search(pattern, line):
                    lines.append('    %s:%d: %s' % (rel, no, line.strip()))
    print('electrum-seed-type: a fresh Electrum seed of type %r; keystore.from_seed parameters %s\n'
          '  multisig keystore derivation %s (xpub prefix %s), single-sig %s\n  source:\n%s' % (
              seed_type, params, ms.get_derivation_prefix(), ms.get_master_public_key()[:4], ss.get_derivation_prefix(), '\n'.join(lines)))
    if seed_type != 'segwit' or ms.get_derivation_prefix() != 'm/1h' or 'derivation' in params or len(lines) < 4:
        raise AssertionError('not the recorded behaviour')
    return ("Electrum's own seed type (segwit) restored for multisig derives at %s and keystore.from_seed takes no "
            'derivation (parameters %s): it cannot follow a per-order path') % (ms.get_derivation_prefix(), ', '.join(params))


def main():
    global MINER_ADDR
    if not (CONTAINER and SRC and OUT):
        raise SystemExit('Run this through scripts/test-escrow-wallets.sh.')
    if cli('getblockchaininfo')['chain'] != 'regtest':
        raise SystemExit('Refusing: the node is not on regtest.')
    sys.path.extend([SRC, os.path.join(SRC, 'packages')])  # after site-packages: the venv's compiled electrum_ecc wins
    from electrum import constants
    constants.BitcoinRegtest.set_as_network()
    from electrum import bip32, keystore, util, version
    from electrum.mnemonic import Mnemonic, calc_seed_type
    from electrum.simple_config import SimpleConfig
    from electrum.transaction import tx_from_any
    from electrum.wallet import Wallet
    from electrum.wallet_db import WalletDB
    import electrum_ecc
    util.create_and_start_event_loop()
    if version.ELECTRUM_VERSION != os.environ.get('ESCROW_WALLETS_ELECTRUM_VERSION') or electrum_ecc.__file__.startswith(SRC):
        raise SystemExit('Refusing: not the verified Electrum release, or electrum_ecc without its compiled libsecp256k1.')
    print('Electrum %s from the verified source tree; electrum_ecc (libsecp256k1) from the venv' % version.ELECTRUM_VERSION)
    E = dict(bip32=bip32, keystore=keystore, Mnemonic=Mnemonic, calc_seed_type=calc_seed_type, SimpleConfig=SimpleConfig,
             tx_from_any=tx_from_any, Wallet=Wallet, WalletDB=WalletDB)
    cli('createwallet', 'miner')
    MINER_ADDR = cli('getnewaddress', wallet='miner')
    cli('generatetoaddress', 101, MINER_ADDR)
    failed = 0
    for name, leg in (('core-shape-a', leg_core_shape_a), ('electrum-shape-b', lambda: leg_electrum_shape_b(E)),
                      ('electrum-seed-type', lambda: leg_electrum_seed_type(E))):
        print('=== LEG %s' % name, flush=True)
        try:
            print('PASS: %s: %s' % (name, leg()), flush=True)
        except Exception as e:
            traceback.print_exc()
            print('FAIL: %s: %s' % (name, e), flush=True)
            failed += 1
    with open(os.path.join(OUT, 'manifest.json'), 'w') as f:
        json.dump({'note': 'Signed escrow settlement PSBTs from scripts/test-escrow-wallets.sh: regtest, throwaway keys, '
                           'public keys only.',
                   'generated_by': 'scripts/test-escrow-wallets.sh at commit %s' % os.environ.get('ESCROW_WALLETS_COMMIT', '?'),
                   'entries': MANIFEST}, f, indent=1)
        f.write('\n')
    sys.stdout.flush()
    os._exit(1 if failed else 0)  # Electrum's event loop thread would keep the process alive


if __name__ == '__main__':
    main()
