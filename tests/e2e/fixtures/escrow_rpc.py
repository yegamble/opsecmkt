"""Deterministic simulated Bitcoin Core for the Bitcoin escrow browser journey (WR-escrow Phase 1, A-246).

Simulated RPC, never chain evidence: no node, no real coins, no real signatures. It answers the escrow adapter's
methods (internal/market/escrow_rpc.go, escrowRPCMethods) with Core's shapes where the app depends on them, as the Go
fake escrow Core (internal/market/escrow_fake_test.go) does: real PSBT and transaction bytes, real BIP 380 descriptor
checksums and bech32 regtest addresses, child keys that are points on secp256k1 (derived from a hash, not BIP32), and
"signatures" that are keyed hashes shaped as DER (fake_sig), which only this simulator accepts. An extended key with no
origin (a rooted per-order cosigner "tpub.../0/0", or a throwaway tprv followed by a path) derives by real BIP32 public
derivation (A-455), so the keys the app derives in Go and the ones this node derives agree as Core's would. It knows regtest only:
getblockchaininfo reports chain "regtest", and a mainnet or other-network address, or a mainnet extended key, is
refused as Core refuses an address of another network (-5).

Test-only endpoints (POST, JSON), for the browser test playing the parties' wallets and the chain:
  /test/escrow/key      {"name"}                 a deterministic tpub with origin, [fp/48h/1h/0h/2h]tpub...
  /test/escrow/address  {}                       a fresh regtest address of nobody's wallet (refund or claim address)
  /test/escrow/deposit  {"address", "sats"}      a payment to a known escrow address (0 confirmations)
  /test/escrow/mine     {"blocks"}               every deposit and broadcast transaction gains confirmations
  /test/escrow/sign     {"psbt", "fingerprint", "sighash"}   the party's wallet: a signature on every input from the
                                                 key the PSBT derives at that fingerprint (sighash 1 = ALL)
GET /test/escrow/state returns the deposits and broadcast transactions.
"""
import base64
from decimal import Decimal
import functools
import hashlib
import hmac
import json
import re
import struct
import threading

P = 2**256 - 2**32 - 977  # secp256k1 field prime
ARBITER_WALLET = '/wallet/opsecmkt-arbiter'
WATCH_WALLET = '/wallet/opsecmkt-escrow-watch'



def sha256(b):
    return hashlib.sha256(b).digest()


def dsha256(b):
    return sha256(sha256(b))


def on_curve(pub):
    if len(pub) != 33 or pub[0] not in (2, 3):
        return False
    x = int.from_bytes(pub[1:], 'big')
    if x >= P:
        return False
    rhs = (pow(x, 3, P) + 7) % P
    return rhs == 0 or pow(rhs, (P - 1) // 2, P) == 1


def point(label):
    """A compressed public key on the curve whose x is a hash of label: nobody knows its private key."""
    i = 0
    while True:
        pub = b'\x02' + sha256((label + "'" * i).encode())
        if on_curve(pub):
            return pub
        i += 1


# --- secp256k1 and BIP32 public derivation (A-455: rooted per-order keys) -------------------------------------------

N = 0xFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEBAAEDCE6AF48A03BBFD25E8CD0364141
G = (0x79BE667EF9DCBBAC55A06295CE870B07029BFCDB2DCE28D959F2815B16F81798,
     0x483ADA7726A3C4655DA4FBFC0E1108A8FD17B448A68554199C47D08FFB10D4B8)


def ec_add(a, b):
    if a is None:
        return b
    if b is None:
        return a
    if a[0] == b[0] and (a[1] + b[1]) % P == 0:
        return None
    if a == b:
        m = 3 * a[0] * a[0] * pow(2 * a[1], P - 2, P) % P
    else:
        m = (b[1] - a[1]) * pow(b[0] - a[0], P - 2, P) % P
    x = (m * m - a[0] - b[0]) % P
    return (x, (m * (a[0] - x) - a[1]) % P)


def _jac_double(p):
    x, y, z = p
    if y == 0:
        return (0, 1, 0)
    s = 4 * x * y * y % P
    m = 3 * x * x % P
    x3 = (m * m - 2 * s) % P
    return (x3, (m * (s - x3) - 8 * pow(y, 4, P)) % P, 2 * y * z % P)


def _jac_add(p, q):
    if p[2] == 0:
        return q
    if q[2] == 0:
        return p
    z1z1, z2z2 = p[2] * p[2] % P, q[2] * q[2] % P
    u1, u2 = p[0] * z2z2 % P, q[0] * z1z1 % P
    s1, s2 = p[1] * q[2] * z2z2 % P, q[1] * p[2] * z1z1 % P
    if u1 == u2:
        return _jac_double(p) if s1 == s2 else (0, 1, 0)
    h, r = (u2 - u1) % P, (s2 - s1) % P
    h2 = h * h % P
    h3 = h * h2 % P
    x3 = (r * r - h3 - 2 * u1 * h2) % P
    return (x3, (r * (u1 * h2 - x3) - s1 * h3) % P, h * p[2] * q[2] % P)


def ec_mul(k, pt):
    """k·pt in Jacobian coordinates (one inversion at the end: the A-456b journey derives many per-order keys)."""
    out, acc = (0, 1, 0), (pt[0], pt[1], 1)
    while k:
        if k & 1:
            out = _jac_add(out, acc)
        acc = _jac_double(acc)
        k >>= 1
    if out[2] == 0:
        return None
    zi = pow(out[2], P - 2, P)
    return (out[0] * zi * zi % P, out[1] * zi * zi * zi % P)


def compress(pt):
    return bytes([2 + (pt[1] & 1)]) + pt[0].to_bytes(32, 'big')


def decompress(pub):
    x = int.from_bytes(pub[1:], 'big')
    y = pow((pow(x, 3, P) + 7) % P, (P + 1) // 4, P)
    if y & 1 != pub[0] & 1:
        y = P - y
    return (x, y)


def ripemd160(data):
    """RIPEMD-160 in pure Python, for a Python whose OpenSSL lacks it (OpenSSL 3.0 before 3.0.7 moved it to the legacy
    provider): the app checks the escrow signer's fingerprint (A-456b), so HASH160 must be real."""
    def rol(x, n):
        return ((x << n) | (x >> (32 - n))) & 0xffffffff
    f = [lambda x, y, z: x ^ y ^ z, lambda x, y, z: (x & y) | (~x & z), lambda x, y, z: (x | ~y) ^ z,
         lambda x, y, z: (x & z) | (y & ~z), lambda x, y, z: x ^ (y | ~z)]
    k1 = [0x00000000, 0x5A827999, 0x6ED9EBA1, 0x8F1BBCDC, 0xA953FD4E]
    k2 = [0x50A28BE6, 0x5C4DD124, 0x6D703EF3, 0x7A6D76E9, 0x00000000]
    r1 = [0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 7, 4, 13, 1, 10, 6, 15, 3, 12, 0, 9, 5, 2, 14, 11, 8,
          3, 10, 14, 4, 9, 15, 8, 1, 2, 7, 0, 6, 13, 11, 5, 12, 1, 9, 11, 10, 0, 8, 12, 4, 13, 3, 7, 15, 14, 5, 6, 2,
          4, 0, 5, 9, 7, 12, 2, 10, 14, 1, 3, 8, 11, 6, 15, 13]
    r2 = [5, 14, 7, 0, 9, 2, 11, 4, 13, 6, 15, 8, 1, 10, 3, 12, 6, 11, 3, 7, 0, 13, 5, 10, 14, 15, 8, 12, 4, 9, 1, 2,
          15, 5, 1, 3, 7, 14, 6, 9, 11, 8, 12, 2, 10, 0, 4, 13, 8, 6, 4, 1, 3, 11, 15, 0, 5, 12, 2, 13, 9, 7, 10, 14,
          12, 15, 10, 4, 1, 5, 8, 7, 6, 2, 13, 14, 0, 3, 9, 11]
    s1 = [11, 14, 15, 12, 5, 8, 7, 9, 11, 13, 14, 15, 6, 7, 9, 8, 7, 6, 8, 13, 11, 9, 7, 15, 7, 12, 15, 9, 11, 7, 13, 12,
          11, 13, 6, 7, 14, 9, 13, 15, 14, 8, 13, 6, 5, 12, 7, 5, 11, 12, 14, 15, 14, 15, 9, 8, 9, 14, 5, 6, 8, 6, 5, 12,
          9, 15, 5, 11, 6, 8, 13, 12, 5, 12, 13, 14, 11, 8, 5, 6]
    s2 = [8, 9, 9, 11, 13, 15, 15, 5, 7, 7, 8, 11, 14, 14, 12, 6, 9, 13, 15, 7, 12, 8, 9, 11, 7, 7, 12, 7, 6, 15, 13, 11,
          9, 7, 15, 11, 8, 6, 6, 14, 12, 13, 5, 14, 13, 13, 7, 5, 15, 5, 8, 11, 14, 14, 6, 14, 6, 9, 12, 9, 12, 5, 15, 8,
          8, 5, 12, 9, 12, 5, 14, 6, 8, 13, 6, 5, 15, 13, 11, 11]
    h = [0x67452301, 0xEFCDAB89, 0x98BADCFE, 0x10325476, 0xC3D2E1F0]
    msg = data + b'\x80' + b'\x00' * ((55 - len(data)) % 64) + struct.pack('<Q', len(data) * 8)
    for off in range(0, len(msg), 64):
        x = struct.unpack('<16I', msg[off:off + 64])
        al, bl, cl, dl, el = h
        ar, br, cr, dr, er = h
        for j in range(80):
            r = j // 16
            t = (rol((al + (f[r](bl, cl, dl) & 0xffffffff) + x[r1[j]] + k1[r]) & 0xffffffff, s1[j]) + el) & 0xffffffff
            al, el, dl, cl, bl = el, dl, rol(cl, 10), bl, t
            t = (rol((ar + (f[4 - r](br, cr, dr) & 0xffffffff) + x[r2[j]] + k2[r]) & 0xffffffff, s2[j]) + er) & 0xffffffff
            ar, er, dr, cr, br = er, dr, rol(cr, 10), br, t
        h = [(h[1] + cl + dr) & 0xffffffff, (h[2] + dl + er) & 0xffffffff, (h[3] + el + ar) & 0xffffffff,
             (h[4] + al + br) & 0xffffffff, (h[0] + bl + cr) & 0xffffffff]
    return struct.pack('<5I', *h)


def fingerprint(pub):
    """BIP32's key fingerprint: the first four bytes of HASH160 (RIPEMD-160 of SHA-256)."""
    try:
        return hashlib.new('ripemd160', sha256(pub)).digest()[:4]
    except ValueError:
        return ripemd160(sha256(pub))[:4]


@functools.lru_cache(maxsize=4096)  # A-456b: the same per-order keys are derived for checkout, its imports and settlements
def ckd_pub(chain, pub, i):
    """BIP32 CKDpub of (chain code, compressed key) at unhardened index i (an invalid child is not handled: 2^-127)."""
    mac = hmac.new(chain, pub + struct.pack('>I', i), hashlib.sha512).digest()
    return mac[32:], compress(ec_add(ec_mul(int.from_bytes(mac[:32], 'big'), G), decompress(pub)))


def real_tpub(tprv):
    """The depth-0 tpub of a depth-0 tprv (k*G, same chain code), as Core's getdescriptorinfo writes it."""
    raw = b58decode_check(tprv)
    return b58check(bytes.fromhex('043587cf') + bytes(9) + raw[13:45] + compress(ec_mul(int.from_bytes(raw[46:78], 'big'), G)))


def real_child(el):
    """(pubkey, derivation value) of "tpub...[/a/b...]" with no origin: Core's derivation and its derivation entry."""
    xpub, _, path = el.partition('/')
    raw = b58decode_check(xpub)
    chain, pub = raw[13:45], raw[45:78]
    deriv = fingerprint(pub)
    for step in [s for s in path.split('/') if s]:
        chain, pub = ckd_pub(chain, pub, int(step))
        deriv += struct.pack('<I', int(step))
    return pub, deriv


# --- base58check, bech32, BIP 380 descriptor checksums -------------------------------------------------------------

B58 = '123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz'


def b58check(payload):
    raw = payload + dsha256(payload)[:4]
    n = int.from_bytes(raw, 'big')
    out = ''
    while n:
        n, r = divmod(n, 58)
        out = B58[r] + out
    return '1' * (len(raw) - len(raw.lstrip(b'\0'))) + out


def b58decode_check(s):
    n = 0
    for c in s:
        n = n * 58 + B58.index(c)
    raw = n.to_bytes((n.bit_length() + 7) // 8, 'big')
    raw = b'\0' * (len(s) - len(s.lstrip('1'))) + raw
    if dsha256(raw[:-4])[:4] != raw[-4:]:
        raise ValueError('checksum')
    return raw[:-4]


BECH32 = 'qpzry9x8gf2tvdw0s3jn54khce6mua7l'


def _polymod(values):
    gen = [0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3]
    chk = 1
    for v in values:
        b = chk >> 25
        chk = (chk & 0x1ffffff) << 5 ^ v
        for i in range(5):
            chk ^= gen[i] if (b >> i) & 1 else 0
    return chk


def _hrp_expand(hrp):
    return [ord(c) >> 5 for c in hrp] + [0] + [ord(c) & 31 for c in hrp]


def _convertbits(data, frombits, tobits, pad=True):
    acc = bits = 0
    out = []
    for v in data:
        acc = (acc << frombits) | v
        bits += frombits
        while bits >= tobits:
            bits -= tobits
            out.append((acc >> bits) & ((1 << tobits) - 1))
    if pad and bits:
        out.append((acc << (tobits - bits)) & ((1 << tobits) - 1))
    elif not pad and (bits >= frombits or (acc << (tobits - bits)) & ((1 << tobits) - 1)):
        raise ValueError('padding')
    return out


def segwit_address(program, hrp='bcrt'):
    """A version 0 segwit address (bech32, BIP 173)."""
    data = [0] + _convertbits(program, 8, 5)
    chk = _polymod(_hrp_expand(hrp) + data + [0] * 6) ^ 1
    return hrp + '1' + ''.join(BECH32[d] for d in data + [(chk >> 5 * (5 - i)) & 31 for i in range(6)])


def address_script(addr):
    """The scriptPubKey of a regtest version 0 address; ValueError for anything else (another network included)."""
    if addr.lower() != addr or not addr.startswith('bcrt1'):
        raise ValueError('not a regtest address')
    data = [BECH32.index(c) for c in addr[5:]]
    if _polymod(_hrp_expand('bcrt') + data) != 1 or not data or data[0] != 0:
        raise ValueError('bad bech32 v0 address')
    prog = bytes(_convertbits(data[1:-6], 5, 8, False))
    if len(prog) not in (20, 32):
        raise ValueError('bad program')
    return bytes([0, len(prog)]) + prog


DESC_INPUT = "0123456789()[],'/*abcdefgh@:$%{}IJKLMNOPQRSTUVWXYZ&+-.;<=>?!^_|~ijklmnopqrstuvwxyzABCDEFGH`#\"\\ "
DESC_GEN = [0xf5dee51989, 0xa9fdca3312, 0x1bab10e32d, 0x3706b1677a, 0x644d626ffd]


def desc_checksum(s):
    def polymod(symbols):
        chk = 1
        for v in symbols:
            top = chk >> 35
            chk = (chk & 0x7ffffffff) << 5 ^ v
            for i in range(5):
                chk ^= DESC_GEN[i] if (top >> i) & 1 else 0
        return chk
    symbols, groups = [], []
    for c in s:
        v = DESC_INPUT.find(c)
        if v < 0:
            raise ValueError('character')
        symbols.append(v & 31)
        groups.append(v >> 5)
        if len(groups) == 3:
            symbols.append(groups[0] * 9 + groups[1] * 3 + groups[2])
            groups = []
    if len(groups) == 1:
        symbols.append(groups[0])
    elif len(groups) == 2:
        symbols.append(groups[0] * 3 + groups[1])
    c = polymod(symbols + [0] * 8) ^ 1
    return ''.join(BECH32[(c >> 5 * (7 - i)) & 31] for i in range(8))


# --- extended keys --------------------------------------------------------------------------------------------------

def make_tpub(name, path):
    """A deterministic test-network extended public key (tpub) at the given hardened path, with its origin."""
    steps = [int(s[:-1]) | 0x80000000 for s in path.split('/')]
    fp = sha256(('fp:' + name).encode())[:4]
    payload = bytes.fromhex('043587cf') + bytes([len(steps)]) + sha256(('parent:' + name).encode())[:4]
    payload += struct.pack('>I', steps[-1]) + sha256(('chaincode:' + name).encode()) + point('account:' + name)
    return '[' + fp.hex() + '/' + path + ']' + b58check(payload)


def fake_sig(pub, txid, sh):
    """The simulator's "signature" of txid by pub with sighash byte sh: a valid strict-DER shape, sighash byte last."""
    r = bytearray(sha256(b'r' + txid.encode() + pub))
    s = bytearray(sha256(b's' + txid.encode() + bytes([sh]) + pub))
    r[0] = r[0] & 0x7f | 1
    s[0] = s[0] & 0x7f | 1
    return bytes([0x30, 0x44, 0x02, 0x20]) + bytes(r) + bytes([0x02, 0x20]) + bytes(s) + bytes([sh])


# --- PSBT and transactions ------------------------------------------------------------------------------------------

def compact(n):
    if n < 0xfd:
        return bytes([n])
    if n <= 0xffff:
        return b'\xfd' + struct.pack('<H', n)
    return b'\xfe' + struct.pack('<I', n)


def read_compact(b, o):
    n = b[o]
    if n < 0xfd:
        return n, o + 1
    if n == 0xfd:
        return struct.unpack_from('<H', b, o + 1)[0], o + 3
    return struct.unpack_from('<I', b, o + 1)[0], o + 5


def parse_tx(b, witness=True):
    """A transaction: version, inputs (txid hex as displayed, vout), outputs (sats, script), witnesses, lock time."""
    o = 4
    segwit = witness and b[4] == 0 and b[5] == 1
    if segwit:
        o = 6
    n, o = read_compact(b, o)
    ins = []
    for _ in range(n):
        txid = b[o:o + 32][::-1].hex()
        vout = struct.unpack_from('<I', b, o + 32)[0]
        sl, o = read_compact(b, o + 36)
        o += sl + 4
        ins.append((txid, vout))
    n, o = read_compact(b, o)
    outs = []
    for _ in range(n):
        sats = struct.unpack_from('<Q', b, o)[0]
        sl, o = read_compact(b, o + 8)
        outs.append((sats, b[o:o + sl]))
        o += sl
    wits = []
    if segwit:
        for _ in ins:
            k, o = read_compact(b, o)
            items = []
            for _ in range(k):
                ln, o = read_compact(b, o)
                items.append(b[o:o + ln])
                o += ln
            wits.append(items)
    return {'ins': ins, 'outs': outs, 'wits': wits, 'segwit': segwit}


def strip_witness(b):
    """The serialization without witnesses (what the txid hashes)."""
    t = parse_tx(b)
    if not t['segwit']:
        return b
    o = 6
    n, o2 = read_compact(b, o)
    # Find the end of the outputs by re-walking the non-witness part.
    o = o2
    for _ in range(n):
        sl, o = read_compact(b, o + 36)
        o += sl + 4
    n, o = read_compact(b, o)
    for _ in range(n):
        sl, o = read_compact(b, o + 8)
        o += sl
    return b[:4] + b[6:o] + b[-4:]


def txid_of(b):
    return dsha256(strip_witness(b))[::-1].hex()


class PSBT:
    """A PSBT version 0 as key-value lists: global, one map per input, one per output."""

    def __init__(self, raw):
        if not raw.startswith(b'psbt\xff'):
            raise ValueError('not a PSBT')
        self.o, self.raw = 5, raw
        self.globals = self._map()
        self.tx = next(v for k, v in self.globals if k == b'\x00')
        t = parse_tx(self.tx, witness=False)
        self.ins, self.outs = t['ins'], t['outs']
        self.inputs = [self._map() for _ in self.ins]
        self.outputs = [self._map() for _ in self.outs]

    def _map(self):
        m = []
        while True:
            kl, self.o = read_compact(self.raw, self.o)
            if kl == 0:
                return m
            k = self.raw[self.o:self.o + kl]
            self.o += kl
            vl, self.o = read_compact(self.raw, self.o)
            m.append((k, self.raw[self.o:self.o + vl]))
            self.o += vl

    @classmethod
    def b64(cls, s):
        return cls(base64.b64decode(s, validate=True))

    def txid(self):
        return dsha256(self.tx)[::-1].hex()

    def get(self, i, key):
        return next((v for k, v in self.inputs[i] if k == key), None)

    def add(self, i, key, value):
        if not any(k == key for k, _ in self.inputs[i]):
            self.inputs[i].append((key, value))

    def serialize(self):
        out = b'psbt\xff'
        for m in [self.globals] + self.inputs + self.outputs:
            for k, v in m:
                out += compact(len(k)) + k + compact(len(v)) + v
            out += b'\x00'
        return out

    def encode(self):
        return base64.b64encode(self.serialize()).decode()

    def sigs(self, i):
        return [(k[1:], v) for k, v in self.inputs[i] if k[0] == 0x02]

    def derivations(self, i):
        return [(k[1:], v[:4].hex(), v[4:]) for k, v in self.inputs[i] if k[0] == 0x06]


def multi_script(keys):
    keys = sorted(keys)
    return b'\x52' + b''.join(b'\x21' + k for k in keys) + b'\x53\xae'


AMOUNT = re.compile(r'"__amount:([0-9.]+)__"')


def amount(sats):
    """An amount as Core prints it (a JSON number with 8 decimals), via a placeholder replaced in reply()."""
    return '__amount:%d.%08d__' % (sats // 100_000_000, sats % 100_000_000)


def sats_of(value):
    whole, _, frac = str(value).partition('.')
    return int(whole) * 100_000_000 + int((frac + '00000000')[:8])


class RPCError(Exception):
    def __init__(self, code, message):
        super().__init__(message)
        self.code, self.message = code, message


TPRV = re.compile(r'tprv[1-9A-HJ-NP-Za-km-z]+')
TPRV_PATH = re.compile(r'(tprv[1-9A-HJ-NP-Za-km-z]+)(/[0-9/]+)')
MULTI = re.compile(r'^wsh\(sortedmulti\(2,([^,()]+),([^,()]+),([^,()]+)\)\)$')
WPKH = re.compile(r'^wpkh\(([^()]+)\)$')


class EscrowCore:
    """The simulated node and its two wallets (the arbiter wallet and the escrow watch-only wallet)."""

    def __init__(self):
        self.lock = threading.Lock()
        self.arbiter_key = make_tpub('opsecmkt-arbiter', '84h/1h/0h')
        self.arbiter_fp = self.arbiter_key[1:9]
        self.change = segwit_address(sha256(b'arbiter-change')[:20])
        self.spk = {}          # escrow address -> scriptPubKey, for addresses this node derived
        self.labels = {}       # watch-only wallet address book
        self.deposits = []     # {'address','txid','vout','sats','confs'}
        self.spent = set()     # 'txid:vout'
        self.sent = {}         # txid -> {'hex','confs'}
        self.addresses = 0
        self.imports = []

    # Keys in descriptors -------------------------------------------------------------------------------------------
    def key_of(self, el):
        """(pubkey, derivation value or None) of one key expression in a descriptor."""
        if el.startswith('['):
            end = el.index(']')
            origin, rest = el[1:end], el[end + 1:]
            fp, path = origin[:8], origin[9:]
            xpub, _, suffix = rest.partition('/')
            if not xpub.startswith('tpub'):
                raise RPCError(-5, 'key is not valid for this network')  # mainnet keys refused
            b58decode_check(xpub)
            steps = []
            for s in [s for s in (path + '/' + suffix).split('/') if s]:
                hard = s.endswith(('h', "'"))
                steps.append(int(s.rstrip("h'")) | (0x80000000 if hard else 0))
            deriv = bytes.fromhex(fp) + b''.join(struct.pack('<I', s) for s in steps)
            if rest.count('/') >= 3:  # A-456b: a per-order key (/<i>/0/0 or a sibling) derives as Core derives it
                return real_child(rest)[0], deriv
            return point('child:' + rest), deriv
        if el.startswith('tpub'):  # A-455: an extended key with no origin, derived as Core derives it
            return real_child(el)
        if el.startswith('fakepub'):
            pub = bytes.fromhex(el[7:])
            return pub, pub[1:5]
        if el.startswith(('xpub', 'xprv')):
            raise RPCError(-5, 'key is not valid for this network')
        return bytes.fromhex(el), None

    def public(self, desc):
        desc = TPRV_PATH.sub(lambda m: real_tpub(m.group(1)) + m.group(2), desc)  # A-455: a throwaway key's child
        return TPRV.sub(lambda m: 'fakepub' + point('pub:' + m.group(0)).hex(), desc)

    def derive(self, body):
        if body.startswith('addr(') and body.endswith(')'):  # A-456b: a sibling address imported labelled
            address_script(body[5:-1])
            return body[5:-1]
        m = MULTI.match(body)
        if m:
            ws = multi_script([self.key_of(el)[0] for el in m.groups()])
            addr = segwit_address(sha256(ws))
            self.spk[addr] = b'\x00\x20' + sha256(ws)
            return addr
        m = WPKH.match(body)
        if m:
            return segwit_address(sha256(self.key_of(m.group(1))[0])[:20])
        raise RPCError(-5, 'descriptor not supported by the simulator')

    # Chain state ----------------------------------------------------------------------------------------------------
    def deposit_at(self, txid, vout):
        return next((d for d in self.deposits if d['txid'] == txid and d['vout'] == vout), None)

    def unspent(self, txid, vout):
        return self.deposit_at(txid, vout) is not None and '%s:%d' % (txid, vout) not in self.spent

    # RPC ------------------------------------------------------------------------------------------------------------
    def handle(self, path, method, p):
        with self.lock:
            return getattr(self, 'rpc_' + method, self.rpc_unknown)(path, p)

    def rpc_unknown(self, path, p):
        raise RPCError(-32601, 'Method not found')

    def rpc_getblockchaininfo(self, path, p):
        return {'chain': 'regtest', 'blocks': 200 + max([d['confs'] for d in self.deposits] or [0]), 'initialblockdownload': False}

    def rpc_getrawchangeaddress(self, path, p):
        if path != ARBITER_WALLET:
            raise RPCError(-18, 'Requested wallet does not exist or is not loaded')
        return self.change

    def rpc_getaddressinfo(self, path, p):
        if p[0] == self.change and path == ARBITER_WALLET:
            return {'ismine': True, 'hdmasterfingerprint': self.arbiter_fp, 'parent_desc': 'wpkh(' + self.arbiter_key + '/1/*)#' + desc_checksum('wpkh(' + self.arbiter_key + '/1/*)')}
        if path == WATCH_WALLET:
            # As Core 31.1 answers on a watch-only descriptor wallet: an imported escrow address is ismine, with its label.
            label = self.labels.get(p[0], '')
            return {'address': p[0], 'ismine': bool(label), 'iswatchonly': False, 'labels': [label] if label else []}
        return {'ismine': path == ARBITER_WALLET}  # the arbiter's key range covers every market child here

    def rpc_getwalletinfo(self, path, p):
        if path == ARBITER_WALLET:
            return {'walletname': 'opsecmkt-arbiter', 'descriptors': True, 'private_keys_enabled': True, 'txcount': 0}
        if path == WATCH_WALLET:
            return {'walletname': 'opsecmkt-escrow-watch', 'descriptors': True, 'private_keys_enabled': False, 'txcount': len(self.deposits)}
        raise RPCError(-18, 'Requested wallet does not exist or is not loaded')

    def rpc_getdescriptorinfo(self, path, p):
        desc = p[0]
        if desc.startswith('addr('):
            try:
                address_script(desc[5:-1])
            except ValueError:
                raise RPCError(-5, 'Address is not valid')
        public = self.public(desc)
        if not desc.startswith('addr('):
            m = MULTI.match(public) or WPKH.match(public)
            if not m:
                raise RPCError(-5, 'descriptor not supported by the simulator')
            for el in m.groups():
                self.key_of(el)  # refuses a mainnet key as Core refuses a key of another network
        return {'descriptor': public + '#' + desc_checksum(public), 'checksum': desc_checksum(public), 'hasprivatekeys': public != desc}

    def rpc_deriveaddresses(self, path, p):
        body, sep, chk = p[0].partition('#')
        if not sep:
            raise RPCError(-5, 'Missing checksum')
        if chk != desc_checksum(body):
            raise RPCError(-5, 'Provided checksum does not match computed checksum')
        rng = p[1] if len(p) > 1 else None
        if ('*' in body) != bool(rng):
            raise RPCError(-8, 'Range should (only) be specified for a ranged descriptor')
        if rng:  # A-456b: a per-order wallet's ranged descriptor, one address per index
            return [self.derive(body.replace('*', str(k))) for k in range(rng[0], rng[1] + 1)]
        return [self.derive(body)]

    def rpc_importdescriptors(self, path, p):
        out = []
        for req in p[0]:
            body = req['desc'].partition('#')[0]
            if req.get('range'):  # A-456b: the order wallet's ranged descriptors (Core refuses a label beside a range)
                if req.get('label'):
                    raise RPCError(-8, 'Ranged descriptors should not have a label')
                for k in range(req['range'][0], req['range'][1] + 1):
                    self.derive(body.replace('*', str(k)))
                if path == WATCH_WALLET:
                    self.imports.append(req['desc'])
                out.append({'success': True})
                continue
            addr = self.derive(body)
            if path == WATCH_WALLET:
                self.imports.append(req['desc'])
                if req.get('label'):
                    self.labels[addr] = req['label']
            out.append({'success': True})
        return out

    def rpc_keypoolrefill(self, path, p):
        return None

    def rpc_gettxout(self, path, p):
        d = self.deposit_at(p[0], p[1])
        if d is None or not self.unspent(p[0], p[1]):
            return None
        return {'confirmations': d['confs'], 'value': amount(d['sats']), 'scriptPubKey': {'address': d['address']}}

    def rpc_createpsbt(self, path, p):
        tx = struct.pack('<I', 2) + compact(len(p[0]))
        for i in p[0]:
            tx += bytes.fromhex(i['txid'])[::-1] + struct.pack('<I', i['vout']) + b'\x00' + b'\xfd\xff\xff\xff'
        tx += compact(len(p[1]))
        for o in p[1]:
            (addr, value), = o.items()
            try:
                spk = address_script(addr)
            except ValueError:
                raise RPCError(-5, 'Invalid Bitcoin address: ' + addr)
            tx += struct.pack('<Q', sats_of(value)) + compact(len(spk)) + spk
        tx += struct.pack('<I', 0)
        raw = b'psbt\xff' + b'\x01\x00' + compact(len(tx)) + tx + b'\x00' + b'\x00' * (len(p[0]) + len(p[1]))
        return base64.b64encode(raw).decode()

    def rpc_utxoupdatepsbt(self, path, p):
        ps = PSBT.b64(p[0])
        for i, (txid, vout) in enumerate(ps.ins):
            d = self.deposit_at(txid, vout)
            if ps.get(i, b'\x01') is None and d and self.unspent(txid, vout):
                spk = address_script(d['address'])
                ps.add(i, b'\x01', struct.pack('<Q', d['sats']) + compact(len(spk)) + spk)
        m = MULTI.match(p[1][0].partition('#')[0])
        if not m:
            raise RPCError(-5, 'descriptor not supported by the simulator')
        entries = [self.key_of(el) for el in m.groups()]
        ws = multi_script([pub for pub, _ in entries])
        want = b'\x00\x20' + sha256(ws)
        for i in range(len(ps.ins)):
            u = ps.get(i, b'\x01')
            if u is None or u[9:] != want:
                continue  # Core adds the script and derivations only to an input paying the descriptor's script
            ps.add(i, b'\x05', ws)
            for pub, deriv in entries:
                ps.add(i, b'\x06' + pub, deriv if deriv is not None else pub[1:5])
        return ps.encode()

    def rpc_descriptorprocesspsbt(self, path, p):
        ps = PSBT.b64(p[0])
        m = TPRV.search(p[1][0])
        if not m or p[2] != 'ALL':
            raise RPCError(-8, 'the simulator signs only a throwaway descriptor key with ALL')
        pub = point('pub:' + m.group(0))
        mp = TPRV_PATH.search(p[1][0])
        if mp:  # A-455: the throwaway key's child along its path (a per-order practice)
            pub = real_child(real_tpub(mp.group(1)) + mp.group(2))[0]
        for i in range(len(ps.ins)):
            ps.add(i, b'\x02' + pub, fake_sig(pub, ps.txid(), 1))
        return {'psbt': ps.encode(), 'complete': False}

    def rpc_walletprocesspsbt(self, path, p):
        if path != ARBITER_WALLET:
            raise RPCError(-18, 'Requested wallet does not exist or is not loaded')
        ps = PSBT.b64(p[0])
        if p[1] is not True or p[2] != 'ALL':
            raise RPCError(-8, 'the simulator signs only with ALL')
        for i in range(len(ps.ins)):
            for pub, fp, _ in ps.derivations(i):
                if fp == self.arbiter_fp:
                    ps.add(i, b'\x02' + pub, fake_sig(pub, ps.txid(), 1))
        return {'psbt': ps.encode(), 'complete': False}

    def rpc_combinepsbt(self, path, p):
        first = PSBT.b64(p[0][0])
        for other in p[0][1:]:
            op = PSBT.b64(other)
            if op.txid() != first.txid():
                raise RPCError(-22, 'PSBTs not compatible (different transactions)')
            for i in range(len(op.ins)):
                for pub, sig in op.sigs(i):
                    first.add(i, b'\x02' + pub, sig)
        return first.encode()

    def final_tx(self, ps):
        """The finalized transaction, or None unless every input has two valid signatures from its script's keys."""
        wits = []
        for i in range(len(ps.ins)):
            ws = ps.get(i, b'\x05')
            if ws is None or len(ws) != 105 or ps.get(i, b'\x07') is not None or ps.get(i, b'\x08') is not None:
                return None
            keys = [ws[2:35], ws[36:69], ws[70:103]]
            sigs = []
            for k in keys:
                for pub, sig in ps.sigs(i):
                    if pub == k:
                        if sig != fake_sig(pub, ps.txid(), sig[-1]):
                            return None
                        sigs.append(sig)
            if len(sigs) < 2:
                return None
            items = [b'', sigs[0], sigs[1], ws]
            wits.append(compact(4) + b''.join(compact(len(x)) + x for x in items))
        u = ps.tx
        return u[:4] + b'\x00\x01' + u[4:-4] + b''.join(wits) + u[-4:]

    def rpc_finalizepsbt(self, path, p):
        ps = PSBT.b64(p[0])
        raw = self.final_tx(ps)
        if raw is None:
            return {'psbt': p[0], 'complete': False}
        return {'hex': raw.hex(), 'complete': True}

    def settlement(self, hexraw):
        """The parsed transaction when every input spends a deposit this node knows, else None."""
        try:
            raw = bytes.fromhex(hexraw)
            t = parse_tx(raw)
        except (ValueError, IndexError, struct.error):
            return None, None
        if not t['ins'] or any(self.deposit_at(txid, vout) is None for txid, vout in t['ins']):
            return None, None
        return raw, t

    def rpc_testmempoolaccept(self, path, p):
        raw, t = self.settlement(p[0][0])
        if t is None:
            return [{'txid': '', 'allowed': False, 'reject-reason': 'missing-inputs'}]
        txid = txid_of(raw)
        if any('%s:%d' % i in self.spent for i in t['ins']):
            return [{'txid': txid, 'allowed': False, 'reject-reason': 'missing-inputs'}]
        return [{'txid': txid, 'allowed': True}]

    def rpc_sendrawtransaction(self, path, p):
        raw, t = self.settlement(p[0])
        if t is None:
            raise RPCError(-25, 'bad-txns-inputs-missingorspent')
        txid = txid_of(raw)
        if txid in self.sent:
            if self.sent[txid]['confs'] > 0:
                raise RPCError(-27, 'Transaction outputs already in utxo set')
            return txid
        if any('%s:%d' % i in self.spent for i in t['ins']):
            raise RPCError(-25, 'bad-txns-inputs-missingorspent')
        for i in t['ins']:
            self.spent.add('%s:%d' % i)
        self.sent[txid] = {'hex': p[0], 'confs': 0}
        return txid

    def rpc_listreceivedbyaddress(self, path, p):
        if path != WATCH_WALLET:
            raise RPCError(-18, 'Requested wallet does not exist or is not loaded')
        out, seen = [], []
        for d in self.deposits:
            if d['address'] not in seen:
                seen.append(d['address'])
        for a in seen:
            txids = []
            for d in self.deposits:
                if d['address'] == a and d['txid'] not in txids:
                    txids.append(d['txid'])
            total = sum(d['sats'] for d in self.deposits if d['address'] == a)
            out.append({'address': a, 'txids': txids, 'amount': amount(total), 'label': self.labels.get(a, ''), 'involvesWatchonly': True})
        if len(p) > 1 and p[1]:
            for a in sorted(self.labels):
                if a not in seen:
                    out.append({'address': a, 'txids': [], 'amount': amount(0), 'label': self.labels[a]})
        return out

    def rpc_listtransactions(self, path, p):
        if path != WATCH_WALLET:
            raise RPCError(-18, 'Requested wallet does not exist or is not loaded')
        out = [{'txid': d['txid'], 'category': 'receive', 'address': d['address'], 'vout': d['vout']} for d in self.deposits]
        out += [{'txid': t, 'category': 'send', 'confirmations': s['confs']} for t, s in sorted(self.sent.items())]
        return out

    def rpc_gettransaction(self, path, p):
        if path != WATCH_WALLET:
            raise RPCError(-18, 'Requested wallet does not exist or is not loaded')
        if p[0] in self.sent:
            s = self.sent[p[0]]
            return {'confirmations': s['confs'], 'walletconflicts': [], 'details': [], 'hex': s['hex']}
        details = [{'address': d['address'], 'category': 'receive', 'amount': amount(d['sats']), 'vout': d['vout'], 'involvesWatchonly': True}
                   for d in self.deposits if d['txid'] == p[0]]
        if not details:
            raise RPCError(-5, 'Invalid or non-wallet transaction id')
        confs = next(d['confs'] for d in self.deposits if d['txid'] == p[0])
        return {'confirmations': confs, 'walletconflicts': [], 'details': details, 'hex': ''}

    # Test endpoints -------------------------------------------------------------------------------------------------
    def test(self, path, data):
        with self.lock:
            if path == '/test/escrow/key':
                name = str(data.get('name', ''))
                if not re.fullmatch(r'[a-z0-9_-]{1,40}', name):
                    raise ValueError('name: 1-40 of a-z 0-9 _ -')
                return {'key': make_tpub(name, '48h/1h/0h/2h')}
            if path == '/test/escrow/address':
                self.addresses += 1
                return {'address': segwit_address(sha256(b'address:%d' % self.addresses)[:20])}
            if path == '/test/escrow/deposit':
                addr, sats = data.get('address'), data.get('sats')
                if addr not in self.spk or not isinstance(sats, int) or sats <= 0:
                    raise ValueError('unknown escrow address or invalid amount')
                txid = sha256(b'deposit:%d' % len(self.deposits)).hex()
                self.deposits.append({'address': addr, 'txid': txid, 'vout': 0, 'sats': sats, 'confs': 0})
                return {'txid': txid, 'vout': 0}
            if path == '/test/escrow/mine':
                n = data.get('blocks', 1)
                if not isinstance(n, int) or not 1 <= n <= 200:
                    raise ValueError('blocks: 1-200')
                for d in self.deposits:
                    d['confs'] += n
                for s in self.sent.values():
                    s['confs'] += n
                return {'ok': True}
            if path == '/test/escrow/sign':
                ps = PSBT.b64(data['psbt'])
                fp, sh = data.get('fingerprint', ''), data.get('sighash', 1)
                if not isinstance(sh, int) or not 0 < sh < 256:
                    raise ValueError('sighash: a byte')
                signed = 0
                for i in range(len(ps.ins)):
                    for pub, dfp, _ in ps.derivations(i):
                        if dfp == fp:
                            ps.add(i, b'\x02' + pub, fake_sig(pub, ps.txid(), sh))
                            signed += 1
                if signed == 0:
                    raise ValueError('the PSBT derives no key at that fingerprint')
                return {'psbt': ps.encode()}
            raise ValueError('unknown test endpoint')

    def state(self):
        with self.lock:
            return {'deposits': self.deposits, 'sent': self.sent, 'imports': self.imports}


class FakeSigner:
    """The market's escrow signer (cmd/escrow-signer, A-454) SIMULATED for the per-order journey (A-456b): its socket
    protocol (one JSON line each way), a rooted account key whose per-order keys derive by real BIP32 public derivation,
    and cmd/escrow-signer's refusals, made before it signs (sighash, the 2-of-3 script, witness_utxo, one script per
    request, exactly one of its keys at index/0/k or index/1/k with k < 20). Its "signature" is fake_sig, which only this
    simulated node accepts. Not chain evidence: scripts/test-escrow-regtest.sh runs the real signer."""

    def __init__(self):
        self.lock = threading.Lock()
        self.chain = sha256(b'e2e-escrow-signer-chain')
        self.pub = point('e2e-escrow-signer-account')
        self.tpub = b58check(bytes.fromhex('043587cf') + bytes(9) + self.chain + self.pub)
        self.fp = fingerprint(self.pub).hex()
        self.requests = []

    def keys(self, index):
        chain, pub = ckd_pub(self.chain, self.pub, index)
        out = set()
        for branch in (0, 1):
            bc, bp = ckd_pub(chain, pub, branch)
            for k in range(20):
                out.add(ckd_pub(bc, bp, k)[1])
        return out

    def sign(self, psbt, index):
        try:
            ps = PSBT.b64(psbt)
        except (ValueError, IndexError, struct.error, StopIteration):
            return {'error': 'bad-psbt'}
        if not ps.ins:
            return {'error': 'bad-psbt'}
        script = None
        for i in range(len(ps.ins)):
            sh = ps.get(i, b'\x03')
            if sh is not None and sh != struct.pack('<I', 1):
                return {'error': 'sighash'}
            ws = ps.get(i, b'\x05')
            if ws is None or len(ws) != 105 or ws[0] != 0x52 or ws[103:] != b'\x53\xae' or not ws[1] == ws[35] == ws[69] == 33:
                return {'error': 'not-escrow-script'}
            keys = [ws[2:35], ws[36:69], ws[70:103]]
            if not keys[0] < keys[1] < keys[2] or not all(on_curve(k) for k in keys):
                return {'error': 'not-escrow-script'}
            u = ps.get(i, b'\x01')
            if u is None or u[9:] != b'\x00\x20' + sha256(ws):
                return {'error': 'witness-utxo'}
            if script is None:
                script = ws
            elif ws != script:
                return {'error': 'mixed-scripts'}
        ours = self.keys(index)
        mine = [k for k in (script[2:35], script[36:69], script[70:103]) if k in ours]
        if not mine:
            return {'error': 'key-not-found'}
        if len(mine) > 1:
            return {'error': 'two-signer-keys'}
        for i in range(len(ps.ins)):
            ps.add(i, b'\x02' + mine[0], fake_sig(mine[0], ps.txid(), 1))
        return {'psbt': ps.encode()}

    def handle(self, line):
        try:
            req = json.loads(line)
        except ValueError:
            return {'error': 'malformed'}
        with self.lock:
            self.requests.append({k: v for k, v in req.items() if k != 'psbt'})
        if not isinstance(req, dict) or req.get('version') != 1:
            return {'error': 'version'}
        if req.get('op') == 'info':
            return {'version': 1, 'tpub': self.tpub, 'fingerprint': self.fp}
        if req.get('op') == 'sign':
            index = req.get('index')
            if not isinstance(index, int) or isinstance(index, bool) or not 0 <= index < 2**31:
                return {'error': 'bad-index'}
            return self.sign(req.get('psbt', ''), index)
        return {'error': 'unknown-op'}


def serve_signer(path, signer):
    """Serves signer on the Unix socket path (a stale socket left there is removed), in a daemon thread."""
    import os
    import socketserver

    class Handler(socketserver.StreamRequestHandler):
        def handle(self):
            for line in self.rfile:
                if line.strip():
                    self.wfile.write(json.dumps(signer.handle(line)).encode() + b'\n')
                    self.wfile.flush()

    if os.path.exists(path):
        os.remove(path)
    server = socketserver.ThreadingUnixStreamServer(path, Handler)
    server.daemon_threads = True
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server


def reply(handler, result, status=200):
    body = AMOUNT.sub(r'\1', json.dumps(result)).encode()
    handler.send_response(status)
    handler.send_header('Content-Type', 'application/json')
    handler.send_header('Content-Length', str(len(body)))
    handler.end_headers()
    handler.wfile.write(body)


def make_handler(core):
    from http.server import BaseHTTPRequestHandler

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass

        def do_GET(self):
            if self.path == '/health':
                return reply(self, {'ok': True})
            reply(self, core.state())

        def do_POST(self):
            # Amounts arrive as JSON numbers with 8 decimals; Decimal keeps them exact (a float could print 1e-05).
            data = json.loads(self.rfile.read(int(self.headers['Content-Length'])) or b'{}', parse_float=Decimal)
            if self.path.startswith('/test/'):
                try:
                    return reply(self, core.test(self.path, data))
                except (ValueError, KeyError) as e:
                    return reply(self, {'error': str(e)}, 400)
            method, params = data.get('method', ''), data.get('params') or []
            try:
                result = core.handle(self.path.rstrip('/') or '/', method, params)
            except RPCError as e:
                return reply(self, {'result': None, 'error': {'code': e.code, 'message': e.message}, 'id': data.get('id')})
            reply(self, {'result': result, 'error': None, 'id': data.get('id')})

    return Handler
