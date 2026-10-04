"""Simulated monerod and monero-wallet-rpc 0.18.5.1 on a fakechain, for the Monero escrow browser journey (WR-escrow
Phase 3, A-248, slice X-g2).

SIMULATED: not chain evidence. No Monero node runs and no coins exist; real-chain legs are
scripts/test-escrow-xmr-regtest.sh. It answers every call the Monero escrow adapter makes
(internal/market/escrow_xmr_rpc.go) with the params the adapter sends, in the reply shapes and with the error codes and
messages a real monerod and monero-wallet-rpc 0.18.5.1 give on a fakechain. Those were recorded from the real nodes by
xmr_record_shapes.py into xmr_rpc_shapes_0.18.5.1.json, and tests/test_xmr_escrow_rpc_fixture.py runs the same recorder
against this simulator and requires the same file: a shape that drifts fails that test. The Go test
TestXMREscrowFixtureAdapter (internal/market) drives the app's adapter against it.

What is real: keys and addresses. Each wallet has a spend secret b and the view secret a = Hs(b), its primary address
encodes b·G and a·G, and subaddress (i, j) encodes D = b·G + Hs("SubAddr\\0" || a || i || j)·G and a·D, as Monero derives
them, in Monero base58 with the Keccak-256 checksum and the mainnet tags a fakechain uses (18 standard, 42 subaddress).
Every key is a scalar times the ed25519 base point, so it is a point of the prime-order subgroup (A-417). The ed25519
arithmetic (RFC 8032) and Keccak are implemented here with plain integers. The 25-word seed uses Monero's mnemonic encoding
and checksum word, over a stand-in word list (the English list is not in the standard library), so it round-trips here
(restore_deterministic_wallet) but no real wallet can restore it.

What is modelled: the chain (blocks, a pool, coinbase outputs to generateblocks' address unlocking after 60 blocks), and
each wallet's view of it, derived from the chain as a refreshed wallet sees it: an output is the wallet's when it pays one
of its subaddresses within the subaddress lookahead (50 accounts, 200 addresses each, set_subaddress_lookahead), a
received output expands the wallet's accounts, an output is spent once a transaction with its key image is in the pool or
a block, and unlocks at 10 confirmations. Several wallet files with passwords, one open at a time per wallet-rpc; freeze
and thaw; transfer with do_not_relay, subtract_fee_from_outputs and amounts_by_dest, its fee from the measured weight
model (0.0018744 XMR for one input and two outputs); relay_tx (idempotent; refused, -4, when another transaction spent a
key image); the daemon's /get_transactions (missed_tx, ring 16, RingCT type 6) and /is_key_image_spent. A transfer to an
address whose keys pass check_key but have a small-order component makes the wallet-rpc go away with no answer and no
wallet open, as 0.18.5.1 crashes (A-417).

What is not: key images, output keys, ring members, signatures and proofs are hashes, not curve points; the tx blob and
metadata are this simulator's own encoding; there is no reorganisation, no double-spend in the pool and no timing.

Test-only controls (POST, JSON) on the escrow wallet-rpc's port, which the app never calls:
  /test/xmr/address        {"label"?}                 a standard address of a wallet outside this wallet-rpc
  /test/xmr/deposit        {"address", "amount"} or {"outputs": [{"address", "amount"}]}
                                                       a transaction from outside paying them (piconero), in the pool
  /test/xmr/mine           {"blocks"}                 blocks: the first takes every pooled transaction
  /test/xmr/refuse-relay   {"count"}                  the daemon refuses the next count transactions relayed to it
  /test/xmr/crash-transfer {"count"}                  the next count transfer calls end the connection unanswered and
                                                       leave no wallet open (a wallet-rpc that crashed and restarted)
GET /test/xmr/state returns the chain (height, every transaction's outputs) and the wallet-rpc's wallets.
"""
import json
import threading
import zlib

# --- Keccak-256 (Monero's cn_fast_hash: Keccak with the original 0x01 padding, not SHA3's 0x06) -------------------------

_M64 = (1 << 64) - 1
_RC = [0x0000000000000001, 0x0000000000008082, 0x800000000000808A, 0x8000000080008000, 0x000000000000808B, 0x0000000080000001,
       0x8000000080008081, 0x8000000000008009, 0x000000000000008A, 0x0000000000000088, 0x0000000080008009, 0x000000008000000A,
       0x000000008000808B, 0x800000000000008B, 0x8000000000008089, 0x8000000000008003, 0x8000000000008002, 0x8000000000000080,
       0x000000000000800A, 0x800000008000000A, 0x8000000080008081, 0x8000000000008080, 0x0000000080000001, 0x8000000080008008]
_ROT = [[0, 36, 3, 41, 18], [1, 44, 10, 45, 2], [62, 6, 43, 15, 61], [28, 55, 25, 21, 56], [27, 20, 39, 8, 14]]  # [x][y]


def _rol(v, n):
    return ((v << n) | (v >> (64 - n))) & _M64 if n else v


def _keccak_f(a):
    for rc in _RC:
        c = [a[x] ^ a[x + 5] ^ a[x + 10] ^ a[x + 15] ^ a[x + 20] for x in range(5)]
        d = [c[(x - 1) % 5] ^ _rol(c[(x + 1) % 5], 1) for x in range(5)]
        a = [a[i] ^ d[i % 5] for i in range(25)]
        b = [0] * 25
        for x in range(5):
            for y in range(5):
                b[y + 5 * ((2 * x + 3 * y) % 5)] = _rol(a[x + 5 * y], _ROT[x][y])
        a = [b[i] ^ ((~b[(i % 5 + 1) % 5 + 5 * (i // 5)]) & b[(i % 5 + 2) % 5 + 5 * (i // 5)]) for i in range(25)]
        a[0] ^= rc
    return a


def sponge256(data, pad):
    """Keccak[512] with a 136-byte rate and the given domain padding byte: 0x01 is Keccak-256, 0x06 is SHA3-256."""
    msg = bytearray(data) + bytes([pad])
    msg += bytes(-len(msg) % 136)
    msg[-1] |= 0x80
    st = [0] * 25
    for off in range(0, len(msg), 136):
        for i in range(17):
            st[i] ^= int.from_bytes(msg[off + 8 * i:off + 8 * i + 8], 'little')
        st = _keccak_f(st)
    return b''.join(st[i].to_bytes(8, 'little') for i in range(4))


def keccak256(data):
    return sponge256(data, 0x01)


# --- ed25519 (RFC 8032 section 5.1), points in extended coordinates (X, Y, Z, T) ---------------------------------------

P = 2**255 - 19
L = 2**252 + 27742317777372353535851937790883648493  # the prime order of the base point's subgroup
_D = -121665 * pow(121666, P - 2, P) % P
_SQRT_M1 = pow(2, (P - 1) // 4, P)


def _recover_x(y, sign):
    if y >= P:
        return None
    x2 = (y * y - 1) * pow(_D * y * y + 1, P - 2, P) % P
    if x2 == 0:
        return None if sign else 0
    x = pow(x2, (P + 3) // 8, P)
    if (x * x - x2) % P:
        x = x * _SQRT_M1 % P
    if (x * x - x2) % P:
        return None
    return P - x if x & 1 != sign else x


_GY = 4 * pow(5, P - 2, P) % P
_GX = _recover_x(_GY, 0)
G = (_GX, _GY, 1, _GX * _GY % P)
IDENTITY = (0, 1, 1, 0)


def point_add(p, q):
    a = (p[1] - p[0]) * (q[1] - q[0]) % P
    b = (p[1] + p[0]) * (q[1] + q[0]) % P
    c = 2 * p[3] * q[3] * _D % P
    d = 2 * p[2] * q[2] % P
    e, f, g, h = b - a, d - c, d + c, b + a
    return (e * f % P, g * h % P, f * g % P, e * h % P)


def scalar_mult(s, p):
    q = IDENTITY
    while s:
        if s & 1:
            q = point_add(q, p)
        p = point_add(p, p)
        s >>= 1
    return q


def compress(p):
    zinv = pow(p[2], P - 2, P)
    x, y = p[0] * zinv % P, p[1] * zinv % P
    return (y | (x & 1) << 255).to_bytes(32, 'little')


def decompress(b):
    """The point a 32-byte key encodes, or None: RFC 8032 5.1.3, which is Monero's check_key (ge_frombytes_vartime)."""
    y = int.from_bytes(b, 'little')
    sign, y = y >> 255, y & ((1 << 255) - 1)
    x = _recover_x(y, sign)
    return None if x is None else (x, y, 1, x * y % P)


def is_identity(p):
    return p[0] % P == 0 and (p[1] - p[2]) % P == 0


def prime_order(p):
    """A point of the prime-order subgroup other than the identity: l·P is the identity (what the app requires, A-417)."""
    return not is_identity(p) and is_identity(scalar_mult(L, p))


def hash_to_scalar(data):
    return int.from_bytes(keccak256(data), 'little') % L


# --- Monero base58 and addresses ---------------------------------------------------------------------------------------

_B58 = '123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz'
_BLOCK_SIZES = [0, 2, 3, 5, 6, 7, 9, 10, 11]  # encoded length of an n-byte block
TAG_STANDARD, TAG_INTEGRATED, TAG_SUBADDRESS = 18, 19, 42  # mainnet tags: a fakechain uses the mainnet address format


def b58encode(data):
    out = []
    for i in range(0, len(data), 8):
        block = data[i:i + 8]
        n, chars = int.from_bytes(block, 'big'), []
        for _ in range(_BLOCK_SIZES[len(block)]):
            n, r = divmod(n, 58)
            chars.append(_B58[r])
        out.append(''.join(reversed(chars)))
    return ''.join(out)


def b58decode(s):
    full, rest = divmod(len(s), 11)
    if rest not in _BLOCK_SIZES:
        return None
    out = b''
    for i in range(full + 1):
        block, size = s[i * 11:i * 11 + 11], 8 if i < full else _BLOCK_SIZES.index(rest)
        if not block:
            continue
        n = 0
        for c in block:
            if c not in _B58:
                return None
            n = n * 58 + _B58.index(c)
        if n >> (8 * size):
            return None
        out += n.to_bytes(size, 'big')
    return out


def encode_address(tag, spend, view):
    data = bytes([tag]) + spend + view
    return b58encode(data + keccak256(data)[:4])


def parse_address(s):
    """(tag, spend key, view key) of a mainnet-format address whose checksum matches and whose keys pass check_key, or None."""
    data = b58decode(s) if isinstance(s, str) else None
    if not data or len(data) not in (69, 77) or keccak256(data[:-4])[:4] != data[-4:]:
        return None
    tag = data[0]
    if (tag, len(data)) not in ((TAG_STANDARD, 69), (TAG_SUBADDRESS, 69), (TAG_INTEGRATED, 77)):
        return None
    spend, view = data[1:33], data[33:65]
    if decompress(spend) is None or decompress(view) is None:
        return None
    return tag, spend, view


def keys_prime_order(s):
    """Both keys of address s are points of the prime-order subgroup (what the app checks offline, A-417)."""
    parsed = parse_address(s)
    return parsed is not None and all(prime_order(decompress(k)) for k in parsed[1:])


class Keys:
    """A Monero key pair from its spend secret: the view secret is Hs(spend secret), as deterministic wallets derive it."""

    def __init__(self, spend_secret):
        self.b = spend_secret % L
        self.a = hash_to_scalar(self.b.to_bytes(32, 'little'))
        self.B = scalar_mult(self.b, G)
        self.spend_pub, self.view_pub = compress(self.B), compress(scalar_mult(self.a, G))
        self.primary = encode_address(TAG_STANDARD, self.spend_pub, self.view_pub)
        self.cache = {(0, 0): self.primary}

    def subaddress(self, major, minor):
        if (major, minor) not in self.cache:
            m = hash_to_scalar(b'SubAddr\x00' + self.a.to_bytes(32, 'little') + major.to_bytes(4, 'little') + minor.to_bytes(4, 'little'))
            d = point_add(self.B, scalar_mult(m, G))
            self.cache[(major, minor)] = encode_address(TAG_SUBADDRESS, compress(d), compress(scalar_mult(self.a, d)))
        return self.cache[(major, minor)]


# --- the 25-word seed: Monero's mnemonic encoding over a stand-in list of 1626 words ------------------------------------

_SYLLABLES = [c + v for c in 'bdfghjklmnprstvz' for v in 'aeiou']
WORDS = [_SYLLABLES[i // 80] + _SYLLABLES[i % 80] + _SYLLABLES[(i * 37) % 80] for i in range(1626)]
_INDEX = {w: i for i, w in enumerate(WORDS)}


def _checksum_index(words):
    """Monero's checksum word: crc32 of the words' 3-letter prefixes, modulo the word count."""
    return zlib.crc32(''.join(w[:3] for w in words).encode()) % len(words)


def mnemonic_of(secret):
    """The 25 words of a 32-byte spend secret: 24 words for 8 little-endian uint32 values, then the checksum word."""
    n, words = len(WORDS), []
    raw = secret.to_bytes(32, 'little')
    for i in range(8):
        x = int.from_bytes(raw[4 * i:4 * i + 4], 'little')
        w1 = x % n
        w2 = (x // n + w1) % n
        w3 = (x // n // n + w2) % n
        words += [WORDS[w1], WORDS[w2], WORDS[w3]]
    return ' '.join(words + [words[_checksum_index(words)]])


def secret_of(mnemonic):
    """The spend secret a 25-word seed encodes, or None when a word is unknown or the checksum word does not match."""
    words = mnemonic.split()
    if len(words) != 25 or any(w not in _INDEX for w in words) or words[_checksum_index(words[:24])] != words[24]:
        return None
    n, raw = len(WORDS), b''
    for i in range(8):
        w1, w2, w3 = (_INDEX[w] for w in words[3 * i:3 * i + 3])
        x = w1 + n * ((n - w1 + w2) % n) + n * n * ((n - w2 + w3) % n)
        if x >= 1 << 32:
            return None
        raw += x.to_bytes(4, 'little')
    return int.from_bytes(raw, 'little')


def outside_keys(label):
    """Keys of a wallet outside every simulated wallet-rpc (a buyer's or vendor's own wallet, the market fee address)."""
    return Keys(hash_to_scalar(b'opsecmkt simulated outside wallet ' + label.encode()))


# --- the chain (monerod) ------------------------------------------------------------------------------------------------

COINBASE_REWARD = 17_592_186_044_415  # piconero: a fakechain block's reward near its start
COINBASE_UNLOCK = 60                   # blocks before a coinbase output can be spent (CRYPTONOTE_MINED_MONEY_UNLOCK_WINDOW)
SPENDABLE_AGE = 10                     # confirmations before any other output can be spent
FEE_PER_BYTE = 1_200_000               # piconero per byte of weight at priority 0 on a young fakechain (measured)
START_HEIGHT = 150
TIME0 = 1_791_000_000


def tx_weight(n_in, n_out):
    """The weight of a ring-16 CLSAG, Bulletproofs+ transaction: 1562 for one input and two outputs, 2166 for two inputs
    and two outputs (measured on monerod 0.18.5.1), about 690 more per output past two."""
    return 1562 + 604 * (n_in - 1) + 690 * (max(n_out, 2) - 2)


def tx_fee(n_in, n_out):
    return tx_weight(n_in, n_out) * FEE_PER_BYTE


class RPCError(Exception):
    def __init__(self, code, message):
        super().__init__(message)
        self.code, self.message = code, message


class Crash(Exception):
    """The wallet-rpc went away while handling the call: the connection ends with no answer."""


class Tx:
    def __init__(self, chain, inputs, outputs, fee, coinbase_height=None):
        self.inputs = list(inputs)            # key images
        self.outputs = [dict(o) for o in outputs]  # {'address', 'amount'} with 'pubkey' and 'global_index' once mined
        self.fee, self.coinbase = fee, coinbase_height is not None
        self.unlock_time = coinbase_height + COINBASE_UNLOCK if self.coinbase else 0
        chain.seq += 1
        salt = chain.seq.to_bytes(8, 'little')
        self.rings = [[1 + keccak256(salt + ki.encode() + bytes([j]))[0] % 40 for j in range(16)] for ki in self.inputs]
        body = {'version': 2, 'unlock_time': self.unlock_time, 'inputs': self.inputs, 'rings': self.rings, 'fee': fee, 'salt': salt.hex(),
                'outputs': [[o['address'], o['amount']] for o in self.outputs], 'coinbase': coinbase_height}
        self.blob = json.dumps(body, sort_keys=True, separators=(',', ':')).encode()
        self.hash = keccak256(self.blob).hex()
        for i, o in enumerate(self.outputs):
            o['pubkey'] = keccak256(b'output key' + bytes.fromhex(self.hash) + bytes([i])).hex()
        self.height = None                    # the block it is in; None while in the pool or never sent
        self.received = None
        self.weight = tx_weight(max(len(self.inputs), 1), len(self.outputs))

    @property
    def metadata(self):
        """Self-contained, as a real tx_metadata is: any wallet-rpc can relay it."""
        return '01' + self.blob.hex()

    def as_json(self):
        h = bytes.fromhex(self.hash)
        filler = lambda tag, i=0: keccak256(h + tag + bytes([i])).hex()
        if self.coinbase:
            vin = [{'gen': {'height': self.unlock_time - COINBASE_UNLOCK}}]
        else:
            vin = [{'key': {'amount': 0, 'key_offsets': ring, 'k_image': ki}} for ki, ring in zip(self.inputs, self.rings)]
        body = {'version': 2, 'unlock_time': self.unlock_time, 'vin': vin,
                'vout': [{'amount': o['amount'] if self.coinbase else 0, 'target': {'tagged_key': {'key': o['pubkey'], 'view_tag': o['pubkey'][:2]}}} for o in self.outputs],
                'extra': list(keccak256(h + b'extra')),
                'rct_signatures': {'type': 0} if self.coinbase else {'type': 6, 'txnFee': self.fee,
                                                                     'ecdhInfo': [{'amount': filler(b'ecdh', i)[:16]} for i in range(len(self.outputs))],
                                                                     'outPk': [filler(b'outpk', i) for i in range(len(self.outputs))]}}
        if not self.coinbase:
            body['rctsig_prunable'] = {'nbp': 1, 'bpp': [{k: filler(k.encode()) for k in ('A', 'A1', 'B', 'r1', 's1', 'd1')} | {'L': [filler(b'L', i) for i in range(7)], 'R': [filler(b'R', i) for i in range(7)]}],
                                       'CLSAGs': [{'s': [filler(b's' + ki.encode()[:8], j) for j in range(16)], 'c1': filler(b'c1' + ki.encode()[:8]), 'D': filler(b'D' + ki.encode()[:8])} for ki in self.inputs],
                                       'pseudoOuts': [filler(b'pseudo', i) for i in range(len(self.inputs))]}
        return json.dumps(body, indent=2)


class Chain:
    """The fakechain monerod: blocks, the pool and every transaction, and the subaddresses derived so far (which wallet
    can see which output)."""

    def __init__(self):
        self.lock = threading.RLock()
        self.seq = 0
        self.height = START_HEIGHT
        self.blocks = [keccak256(b'simulated fakechain block %d' % h).hex() for h in range(START_HEIGHT)]
        self.txs = {}            # hash -> Tx in the pool or a block
        self.pool = []           # hashes, in arrival order
        self.built = {}          # metadata -> Tx built and not relayed (transfer with do_not_relay)
        self.owners = {}         # address -> (spend public key, major, minor): who can see an output to it
        self.next_global = 1000
        self.refuse = 0          # the next refuse relayed transactions are refused (/test/xmr/refuse-relay)
        self.outside = 0
        self.wallets = 0

    def derive(self, keys, major, minor):
        address = keys.subaddress(major, minor)
        self.owners[address] = (keys.spend_pub, major, minor)
        return address

    def timestamp(self, height):
        return TIME0 + 120 * height

    def mined(self):
        """Every mined transaction in chain order."""
        return sorted((t for t in self.txs.values() if t.height is not None), key=lambda t: (t.height, t.received))

    def spender(self, key_image):
        return next((t for t in self.txs.values() if key_image in t.inputs), None)

    def submit(self, tx):
        """Puts tx in the pool. A transaction already known is answered as relayed (relay_tx is idempotent); one whose key
        image another transaction spent, or one /test/xmr/refuse-relay refuses, is rejected."""
        if tx.hash in self.txs:
            return
        if self.refuse > 0:
            self.refuse -= 1
            raise RPCError(-4, 'Failed to commit tx.')
        if any(self.spender(ki) for ki in tx.inputs):
            raise RPCError(-4, 'Failed to commit tx.')
        tx.received = self.timestamp(self.height) + len(self.pool)
        self.txs[tx.hash] = tx
        self.pool.append(tx.hash)

    def mine(self, blocks, coinbase_to=None):
        hashes = []
        for _ in range(blocks):
            txs = [self.txs[h] for h in self.pool]
            self.pool = []
            if coinbase_to:
                cb = Tx(self, [], [{'address': coinbase_to, 'amount': COINBASE_REWARD}], 0, coinbase_height=self.height)
                cb.received = self.timestamp(self.height)
                self.txs[cb.hash] = cb
                txs.insert(0, cb)
            for t in txs:
                t.height = self.height
                for o in t.outputs:
                    o['global_index'] = self.next_global
                    self.next_global += 1
            self.blocks.append(keccak256(self.blocks[-1].encode() + ''.join(t.hash for t in txs).encode()).hex())
            hashes.append(self.blocks[-1])
            self.height += 1
        return hashes

    def outside_payment(self, outputs):
        """A transaction from a wallet outside the simulator paying outputs: one input, the outputs and its change."""
        self.outside += 1
        keys = outside_keys('funder %d' % self.outside)
        total = sum(o['amount'] for o in outputs)
        fee = tx_fee(1, len(outputs) + 1)
        tx = Tx(self, [keccak256(b'outside key image %d' % self.outside).hex()],
                outputs + [{'address': keys.primary, 'amount': max(total // 3, 1)}], fee)
        self.submit(tx)
        return tx

    # The daemon's JSON-RPC methods and its other endpoints.
    def rpc(self, method, p):
        with self.lock:
            if method == 'get_info':
                return {'adjusted_time': self.timestamp(self.height), 'alt_blocks_count': 0, 'block_size_limit': 600000, 'block_size_median': 300000,
                        'block_weight_limit': 600000, 'block_weight_median': 300000, 'bootstrap_daemon_address': '', 'busy_syncing': False, 'credits': 0,
                        'cumulative_difficulty': self.height, 'cumulative_difficulty_top64': 0, 'database_size': 81920 + 4096 * self.height, 'difficulty': 1,
                        'difficulty_top64': 0, 'free_space': 8_000_000_000, 'grey_peerlist_size': 0, 'height': self.height,
                        'height_without_bootstrap': self.height, 'incoming_connections_count': 0, 'mainnet': False, 'nettype': 'fakechain',
                        'offline': True, 'outgoing_connections_count': 0, 'restricted': False, 'rpc_connections_count': 1, 'stagenet': False,
                        'start_time': TIME0, 'status': 'OK', 'synchronized': True, 'target': 120, 'target_height': 0, 'testnet': False,
                        'top_block_hash': self.blocks[-1], 'top_hash': '', 'tx_count': len(self.txs) - len(self.pool), 'tx_pool_size': len(self.pool),
                        'untrusted': False, 'update_available': False, 'version': '0.18.5.1-release', 'was_bootstrap_ever_used': False,
                        'white_peerlist_size': 0, 'wide_cumulative_difficulty': hex(self.height), 'wide_difficulty': '0x1'}
            if method == 'hard_fork_info':
                return {'credits': 0, 'earliest_height': 1, 'enabled': True, 'state': 0, 'status': 'OK', 'threshold': 0, 'top_hash': '',
                        'untrusted': False, 'version': 16, 'votes': 0, 'voting': 16, 'window': 1}
            if method == 'generateblocks':
                n, address = p.get('amount_of_blocks'), p.get('wallet_address', '')
                parsed = parse_address(address)
                if not isinstance(n, int) or n < 1 or parsed is None or parsed[0] != TAG_STANDARD:
                    raise RPCError(-2, 'Failed to parse wallet address')
                return {'blocks': self.mine(n, address), 'height': self.height, 'status': 'OK', 'untrusted': False}
            raise RPCError(-32601, 'Method not found')

    def other(self, path, q):
        with self.lock:
            if path == '/get_transactions':
                found, missed = [], []
                for h in q.get('txs_hashes') or []:
                    (found if h in self.txs else missed).append(h)
                decode = bool(q.get('decode_as_json'))
                res = {'credits': 0, 'status': 'OK', 'top_hash': '', 'untrusted': False, 'missed_tx': missed, 'txs': [],
                       'txs_as_hex': [], 'txs_as_json': []}
                for h in found:
                    t = self.txs[h]
                    e = {'as_hex': t.blob.hex(), 'as_json': t.as_json() if decode else '', 'double_spend_seen': False, 'in_pool': t.height is None,
                         'prunable_as_hex': '', 'prunable_hash': keccak256(t.blob + b'prunable').hex(), 'pruned_as_hex': '', 'tx_hash': h}
                    if t.height is None:
                        e |= {'received_timestamp': t.received, 'relayed': True}
                    else:
                        e |= {'block_height': t.height, 'block_timestamp': self.timestamp(t.height), 'confirmations': self.height - t.height,
                              'output_indices': [o['global_index'] for o in t.outputs]}
                    res['txs'].append(e)
                    res['txs_as_hex'].append(e['as_hex'])
                    if decode:
                        res['txs_as_json'].append(e['as_json'])
                return res
            if path == '/is_key_image_spent':
                status = []
                for ki in q.get('key_images') or []:
                    t = self.spender(ki)
                    status.append(0 if t is None else 2 if t.height is None else 1)
                return {'credits': 0, 'spent_status': status, 'status': 'OK', 'top_hash': '', 'untrusted': False}
            return None

    def state(self):
        with self.lock:
            return {'height': self.height, 'pool': list(self.pool),
                    'txs': {h: {'height': t.height, 'fee': t.fee, 'inputs': t.inputs, 'outputs': [{'address': o['address'], 'amount': o['amount']} for o in t.outputs]}
                            for h, t in self.txs.items()}}


# --- wallet files and monero-wallet-rpc ---------------------------------------------------------------------------------

class WalletFile:
    def __init__(self, chain, name, password, keys, height):
        self.name, self.password, self.keys = name, password, keys
        self.minors = [1]                     # addresses per account; account 0 is the primary account
        self.labels = {}
        self.frozen = set()                   # key images
        self.lookahead = (50, 200)
        self.restore_height = height
        self.height = height                  # the wallet's blockchain height: blocks below it are scanned
        chain.derive(keys, 0, 0)

    def address(self, chain, major, minor):
        return chain.derive(self.keys, major, minor)

    def key_image(self, tx_hash, index):
        return keccak256(b'simulated key image' + self.keys.b.to_bytes(32, 'little') + bytes.fromhex(tx_hash) + index.to_bytes(4, 'little')).hex()

    def sees(self, major, minor):
        if major >= len(self.minors) + self.lookahead[0]:
            return False
        return minor < (self.minors[major] if major < len(self.minors) else 0) + self.lookahead[1]


class Output:
    def __init__(self, w, tx, index, major, minor):
        self.tx, self.index, self.major, self.minor = tx, index, major, minor
        self.amount = tx.outputs[index]['amount']
        self.key_image = w.key_image(tx.hash, index)
        self.spent_by = None


class View:
    """What wallet w sees of the chain at its height: its outputs (mined below its height, or in the pool) and the
    transactions that spend them. Scanning a received output's subaddress expands the wallet's accounts, as wallet2 does."""

    def __init__(self, chain, w):
        self.chain, self.w = chain, w
        txs = [t for t in chain.mined() if t.height < w.height] + [chain.txs[h] for h in chain.pool]
        self.outputs, by_ki = [], {}
        for t in txs:
            for i, o in enumerate(t.outputs):
                own = chain.owners.get(o['address'])
                if not own or own[0] != w.keys.spend_pub or not w.sees(own[1], own[2]):
                    continue
                major, minor = own[1], own[2]
                while len(w.minors) <= major:
                    w.address(chain, len(w.minors), 0)
                    w.minors.append(1)
                w.minors[major] = max(w.minors[major], minor + 1)
                out = Output(w, t, i, major, minor)
                self.outputs.append(out)
                by_ki[out.key_image] = out
        self.sent = []                        # transactions spending the wallet's outputs
        for t in txs:
            spent = [by_ki[ki] for ki in t.inputs if ki in by_ki]
            for out in spent:
                out.spent_by = t
            if spent:
                self.sent.append((t, spent))
        self.sent_hashes = {t.hash for t, _ in self.sent}

    def confirmations(self, t):
        return self.w.height - t.height if t.height is not None else 0

    def unlocked(self, out):
        c = self.confirmations(out.tx)
        return out.tx.height is not None and c >= SPENDABLE_AGE and (not out.tx.coinbase or c >= COINBASE_UNLOCK)

    def unspent(self, out):
        return out.spent_by is None and out.tx.height is not None


def _threshold(amount):
    return max(1, -(-amount // COINBASE_REWARD))


class WalletRPC:
    """One monero-wallet-rpc: its wallet directory and the one wallet it has open. It answers one request at a time."""

    NO_WALLET = ('create_wallet', 'open_wallet', 'restore_deterministic_wallet', 'get_version')

    def __init__(self, chain, wallet_dir='/tmp/w'):
        self.chain, self.dir = chain, wallet_dir
        self.lock = threading.Lock()
        self.files = {}
        self.open = None
        self.crash = 0                        # the next crash transfer calls crash (/test/xmr/crash-transfer)

    def handle(self, method, p):
        with self.lock, self.chain.lock:
            if method not in self.NO_WALLET and not hasattr(self, 'm_' + method):
                raise RPCError(-32601, 'Method not found')
            if method not in self.NO_WALLET and self.open is None:
                raise RPCError(-13, 'No wallet file')
            return getattr(self, 'm_' + method)(p if isinstance(p, dict) else {})

    def _replace_open(self, w):
        self.open = w                         # the wallet open before is stored and closed (autosave_current)

    def m_get_version(self, p):
        return {'release': True, 'version': (1 << 16) | 31}

    def m_create_wallet(self, p):
        name = p.get('filename', '')
        if name in self.files:
            raise RPCError(-21, 'Cannot create wallet. Already exists.')
        self.chain.wallets += 1  # every wallet created on any wallet-rpc of this chain gets its own keys
        secret = hash_to_scalar(b'simulated wallet ' + name.encode() + b'/%d' % self.chain.wallets)
        w = self.files[name] = WalletFile(self.chain, name, p.get('password', ''), Keys(secret), self.chain.height)
        self._replace_open(w)
        return {}

    def m_open_wallet(self, p):
        name = p.get('filename', '')
        w = self.files.get(name)
        if w is None:
            raise RPCError(-1, 'Failed to open wallet : file not found "%s/%s.keys"' % (self.dir, name))
        if w.password != p.get('password', ''):
            raise RPCError(-1, 'Failed to open wallet : Invalid password.')
        self._replace_open(w)                 # a failed open leaves the wallet that was open, open
        return {}

    def m_restore_deterministic_wallet(self, p):
        name = p.get('filename', '')
        if name in self.files:
            raise RPCError(-1, 'Wallet already exists.')
        secret = secret_of(p.get('seed', ''))
        if secret is None:
            raise RPCError(-1, 'Electrum-style word list failed verification')
        height = p.get('restore_height', 0)
        w = self.files[name] = WalletFile(self.chain, name, p.get('password', ''), Keys(secret), min(int(height), self.chain.height))
        self._replace_open(w)
        return {'address': w.keys.primary, 'info': 'Wallet has been restored successfully.', 'seed': mnemonic_of(w.keys.b), 'was_deprecated': False}

    def m_close_wallet(self, p):
        self.open = None
        return {}

    def m_store(self, p):
        return {}

    def m_refresh(self, p):
        w = self.open
        before = w.height
        w.height = self.chain.height
        received = any(o.tx.height is not None and o.tx.height >= before for o in View(self.chain, w).outputs)
        return {'blocks_fetched': w.height - before, 'received_money': received}

    def m_rescan_blockchain(self, p):
        w = self.open
        w.frozen.clear()                      # a rescan forgets what the chain cannot tell, frozen outputs included
        w.height = self.chain.height
        View(self.chain, w)
        return {}

    def m_get_height(self, p):
        return {'height': self.open.height}

    def m_query_key(self, p):
        w, kind = self.open, p.get('key_type')
        if kind == 'mnemonic':
            return {'key': mnemonic_of(w.keys.b)}
        if kind == 'spend_key':
            return {'key': w.keys.b.to_bytes(32, 'little').hex()}
        if kind == 'view_key':
            return {'key': w.keys.a.to_bytes(32, 'little').hex()}
        raise RPCError(-29, 'key_type ' + str(kind) + ' not found')

    def m_change_wallet_password(self, p):
        if p.get('old_password', '') != self.open.password:
            raise RPCError(-22, 'Invalid original password.')
        self.open.password = p.get('new_password', '')
        return {}

    def m_set_subaddress_lookahead(self, p):
        if p.get('password', '') != self.open.password:
            raise RPCError(-22, 'Invalid password.')
        major, minor = p.get('major_idx'), p.get('minor_idx')
        if not isinstance(major, int) or not isinstance(minor, int) or major < 1 or minor < 1:
            raise RPCError(-1, 'Invalid lookahead')
        self.open.lookahead = (major, minor)
        return {}

    def m_create_account(self, p):
        w = self.open
        w.minors.append(1)
        major = len(w.minors) - 1
        if p.get('label'):
            w.labels[(major, 0)] = p['label']
        return {'account_index': major, 'address': w.address(self.chain, major, 0)}

    def m_create_address(self, p):
        w, major = self.open, p.get('account_index', 0)
        if major >= len(w.minors):
            raise RPCError(-14, 'account index is out of bound')
        indices = []
        for _ in range(max(1, p.get('count', 1))):
            indices.append(w.minors[major])
            w.minors[major] += 1
        addresses = [w.address(self.chain, major, i) for i in indices]
        return {'address': addresses[0], 'address_index': indices[0], 'address_indices': indices, 'addresses': addresses}

    def _label(self, major, minor):
        return self.open.labels.get((major, minor), 'Primary account' if (major, minor) == (0, 0) else '')

    def m_get_address(self, p):
        w, v, major = self.open, View(self.chain, self.open), p.get('account_index', 0)
        if not isinstance(major, int) or major < 0 or major >= len(w.minors):
            raise RPCError(-14, 'account index is out of bound')
        indices = p.get('address_index') or list(range(w.minors[major]))
        if any(not isinstance(i, int) or i < 0 or i >= w.minors[major] for i in indices):
            raise RPCError(-15, 'address index is out of bound')
        used = {(o.major, o.minor) for o in v.outputs}
        addresses = [{'address': w.address(self.chain, major, i), 'address_index': i, 'label': self._label(major, i), 'used': (major, i) in used} for i in indices]
        return {'address': w.address(self.chain, major, 0), 'addresses': addresses}

    def _balances(self, v):
        """(balance, unlocked, blocks to unlock, outputs) per (major, minor): unspent mined outputs, and the change of the
        wallet's own transactions in the pool, as wallet2's balance counts them."""
        per = {}
        for o in v.outputs:
            pending_change = o.tx.height is None and o.tx.hash in v.sent_hashes
            if not (v.unspent(o) or pending_change and o.spent_by is None):
                continue
            b = per.setdefault((o.major, o.minor), [0, 0, 0, 0])
            b[0] += o.amount
            b[3] += 1
            if v.unlocked(o):
                b[1] += o.amount
            else:
                need = COINBASE_UNLOCK if o.tx.coinbase else SPENDABLE_AGE
                b[2] = max(b[2], need - v.confirmations(o.tx) if o.tx.height is not None else need)
        return per

    def m_get_accounts(self, p):
        w, v = self.open, View(self.chain, self.open)
        per = self._balances(v)
        accounts = []
        for major in range(len(w.minors)):
            bal = sum(b[0] for (m, _), b in per.items() if m == major)
            unl = sum(b[1] for (m, _), b in per.items() if m == major)
            accounts.append({'account_index': major, 'balance': bal, 'base_address': w.address(self.chain, major, 0), 'label': self._label(major, 0),
                             'tag': '', 'unlocked_balance': unl})
        return {'subaddress_accounts': accounts, 'total_balance': sum(a['balance'] for a in accounts),
                'total_unlocked_balance': sum(a['unlocked_balance'] for a in accounts)}

    def m_get_balance(self, p):
        v = View(self.chain, self.open)
        per = self._balances(v)
        if not p.get('all_accounts'):
            per = {k: b for k, b in per.items() if k[0] == p.get('account_index', 0)}
        rows = [{'account_index': m, 'address': self.open.address(self.chain, m, n), 'address_index': n, 'balance': b[0], 'blocks_to_unlock': b[2],
                 'label': self._label(m, n), 'num_unspent_outputs': b[3], 'time_to_unlock': 0, 'unlocked_balance': b[1]} for (m, n), b in sorted(per.items())]
        return {'balance': sum(r['balance'] for r in rows), 'blocks_to_unlock': max([r['blocks_to_unlock'] for r in rows], default=0),
                'multisig_import_needed': False, 'per_subaddress': rows, 'time_to_unlock': 0, 'unlocked_balance': sum(r['unlocked_balance'] for r in rows)}

    def _entries(self, v):
        """get_transfers' entries of view v: in and pool (received, one per transaction and subaddress), out and pending
        (sent by this wallet: its own outputs are change, never listed as received)."""
        w, out = self.open, []
        received = {}
        for o in v.outputs:
            if o.tx.hash not in v.sent_hashes:
                received.setdefault((o.tx.hash, o.major, o.minor), []).append(o)
        for (h, major, minor), outs in received.items():
            t = outs[0].tx
            amount = sum(o.amount for o in outs)
            e = {'address': w.address(self.chain, major, minor), 'amount': amount, 'amounts': [o.amount for o in outs], 'double_spend_seen': False,
                 'fee': t.fee, 'height': t.height or 0, 'locked': not all(v.unlocked(o) for o in outs), 'note': '', 'payment_id': '0000000000000000',
                 'subaddr_index': {'major': major, 'minor': minor}, 'subaddr_indices': [{'major': major, 'minor': minor}],
                 'suggested_confirmations_threshold': _threshold(amount), 'timestamp': self.chain.timestamp(t.height) if t.height is not None else t.received,
                 'txid': h, 'type': 'in' if t.height is not None else 'pool', 'unlock_time': t.unlock_time}
            if t.height is not None:
                e['confirmations'] = v.confirmations(t)
            out.append(e)
        for t, spent in v.sent:
            mine = {(o.tx.hash, o.index) for o in v.outputs if o.tx is t}
            dests = [{'address': o['address'], 'amount': o['amount']} for i, o in enumerate(t.outputs) if (t.hash, i) not in mine]
            major = spent[0].major
            amount = sum(d['amount'] for d in dests)
            e = {'address': w.address(self.chain, major, 0), 'amount': amount, 'destinations': dests, 'double_spend_seen': False, 'fee': t.fee,
                 'height': t.height or 0, 'locked': t.height is None or v.confirmations(t) < SPENDABLE_AGE, 'note': '', 'payment_id': '0000000000000000',
                 'subaddr_index': {'major': major, 'minor': 0}, 'subaddr_indices': [{'major': m, 'minor': n} for m, n in sorted({(o.major, o.minor) for o in spent})],
                 'suggested_confirmations_threshold': _threshold(amount), 'timestamp': self.chain.timestamp(t.height) if t.height is not None else t.received,
                 'txid': t.hash, 'type': 'out' if t.height is not None else 'pending', 'unlock_time': 0}
            if t.height is not None:
                e['confirmations'] = v.confirmations(t)
            out.append(e)
        return out

    def m_get_transfers(self, p):
        v = View(self.chain, self.open)
        res = {k: [] for k in ('in', 'out', 'pending', 'failed', 'pool') if p.get(k)}
        for e in self._entries(v):
            if e['type'] in res and (p.get('all_accounts') or e['subaddr_index']['major'] == p.get('account_index', 0)):
                res[e['type']].append(e)
        for k in res:
            res[k].sort(key=lambda e: (e['height'] or 1 << 62, e['timestamp'], e['txid'], e['subaddr_index']['minor']))
        return res

    def m_get_transfer_by_txid(self, p):
        v = View(self.chain, self.open)
        account = p.get('account_index')
        found = [e for e in self._entries(v) if e['txid'] == p.get('txid') and (account is None or e['subaddr_index']['major'] == account)]
        if not found:
            raise RPCError(-8, 'Transaction not found.')
        return {'transfer': found[0], 'transfers': found}

    def m_incoming_transfers(self, p):
        v, kind, major = View(self.chain, self.open), p.get('transfer_type', 'all'), p.get('account_index', 0)
        rows = []
        for o in v.outputs:
            if o.tx.height is None or o.major != major:
                continue
            spent = o.spent_by is not None
            if kind == 'available' and spent or kind == 'unavailable' and not spent:
                continue
            rows.append({'amount': o.amount, 'block_height': o.tx.height, 'frozen': o.key_image in self.open.frozen,
                         'global_index': o.tx.outputs[o.index]['global_index'], 'key_image': o.key_image if p.get('verbose') else '',
                         'pubkey': o.tx.outputs[o.index]['pubkey'], 'spent': spent, 'subaddr_index': {'major': o.major, 'minor': o.minor},
                         'tx_hash': o.tx.hash, 'unlocked': v.unlocked(o)})
        return {'transfers': rows}

    def _set_frozen(self, p, frozen):
        ki = p.get('key_image', '')
        if not any(o.key_image == ki and o.tx.height is not None for o in View(self.chain, self.open).outputs):
            raise RPCError(-1, 'Key image not found')
        (self.open.frozen.add if frozen else self.open.frozen.discard)(ki)
        return {}

    def m_freeze(self, p):
        return self._set_frozen(p, True)

    def m_thaw(self, p):
        return self._set_frozen(p, False)

    def m_validate_address(self, p):
        parsed = parse_address(p.get('address', ''))
        if parsed is None:
            return {'integrated': False, 'nettype': '', 'openalias_address': '', 'subaddress': False, 'valid': False}
        return {'integrated': parsed[0] == TAG_INTEGRATED, 'nettype': 'mainnet', 'openalias_address': '', 'subaddress': parsed[0] == TAG_SUBADDRESS, 'valid': True}

    def m_transfer(self, p):
        w = self.open
        if self.crash > 0:
            self.crash -= 1
            self.open = None
            raise Crash()
        dests = p.get('destinations') or []
        for d in dests:
            parsed = parse_address(d.get('address', ''))
            if parsed is None:
                raise RPCError(-2, 'WALLET_RPC_ERROR_CODE_WRONG_ADDRESS: ' + str(d.get('address', '')))
            if not all(prime_order(decompress(k)) for k in parsed[1:]):
                self.open = None              # keys with a small-order component: 0.18.5.1 crashes building the transfer (A-417)
                raise Crash()
            if not isinstance(d.get('amount'), int) or d['amount'] <= 0:
                raise RPCError(-20, 'No destinations for this transfer')
        if not dests:
            raise RPCError(-20, 'No destinations for this transfer')
        account = p.get('account_index', 0)
        subtract = sorted(set(p.get('subtract_fee_from_outputs') or []))
        v = View(self.chain, w)
        mine = [o for o in v.outputs if o.major == account and v.unspent(o) and o.key_image not in w.frozen]
        usable = sorted((o for o in mine if v.unlocked(o)), key=lambda o: (-o.tx.height, -o.tx.outputs[o.index]['global_index']))
        want = sum(d['amount'] for d in dests)
        picked, have = [], 0
        for o in usable:
            fee = tx_fee(max(len(picked), 1), len(dests) + 1)
            if picked and have >= want + (0 if subtract else fee):
                break
            picked.append(o)
            have += o.amount
        n_out = len(dests) + 1
        fee = tx_fee(max(len(picked), 1), n_out)
        need = want + (0 if subtract else fee)
        if not picked or have < need:
            if sum(o.amount for o in mine) >= need:
                raise RPCError(-37, 'not enough unlocked money')
            raise RPCError(-17, 'not enough money')
        change = have - need
        n_out = max(2, len(dests) + (1 if change else 0))
        fee = tx_fee(len(picked), n_out)
        amounts = [d['amount'] for d in dests]
        if subtract:
            share, rest = divmod(fee, len(subtract))
            for k, i in enumerate(subtract):
                amounts[i] -= share + (rest if k == 0 else 0)
            if any(a <= 0 for a in amounts):
                raise RPCError(-4, 'transaction amount too small to pay the fee')
            change = have - want
        else:
            change = have - want - fee
        outputs = [{'address': d['address'], 'amount': a} for d, a in zip(dests, amounts)]
        if change or len(outputs) < 2:
            outputs.append({'address': w.address(self.chain, account, 0), 'amount': change})
        tx = Tx(self.chain, [o.key_image for o in picked], outputs, fee)
        tx.account, tx.wallet = account, w.name
        if p.get('do_not_relay'):
            self.chain.built[tx.metadata] = tx
        else:
            self.chain.submit(tx)
        return {'amount': sum(amounts), 'amounts_by_dest': {'amounts': amounts}, 'fee': fee, 'multisig_txset': '',
                'spent_key_images': {'key_images': list(tx.inputs)}, 'tx_blob': tx.blob.hex() if p.get('get_tx_hex') else '', 'tx_hash': tx.hash,
                'tx_key': keccak256(b'tx key' + tx.blob).hex() if p.get('get_tx_key') else '',
                'tx_metadata': tx.metadata if p.get('get_tx_metadata') else '', 'unsigned_txset': '', 'weight': tx.weight}

    def m_relay_tx(self, p):
        tx = self.chain.built.get(p.get('hex', ''))
        if tx is None:
            tx = next((t for t in self.chain.txs.values() if t.metadata == p.get('hex')), None)
        if tx is None:
            raise RPCError(-27, 'Failed to parse tx metadata.')
        self.chain.submit(tx)
        return {'tx_hash': tx.hash}

    def state(self):
        with self.lock, self.chain.lock:
            return {'wallets': sorted(self.files), 'open': self.open.name if self.open else None}

    def control(self, path, data):
        """The test-only controls (see the module comment)."""
        with self.lock, self.chain.lock:
            c = self.chain
            if path == '/test/xmr/address':
                label = data.get('label')
                if label is None:
                    c.outside += 1
                    label = 'address %d' % c.outside
                return {'address': outside_keys(str(label)).primary}
            if path == '/test/xmr/deposit':
                outputs = data.get('outputs') or [{'address': data.get('address'), 'amount': data.get('amount')}]
                for o in outputs:
                    if not isinstance(o.get('amount'), int) or o['amount'] <= 0 or not keys_prime_order(o.get('address')):
                        raise ValueError('every output needs a valid address and a positive amount in piconero')
                return {'txid': c.outside_payment([{'address': o['address'], 'amount': o['amount']} for o in outputs]).hash}
            if path == '/test/xmr/mine':
                n = data.get('blocks', 1)
                if not isinstance(n, int) or not 1 <= n <= 1000:
                    raise ValueError('blocks: 1-1000')
                c.mine(n)
                return {'height': c.height}
            if path in ('/test/xmr/refuse-relay', '/test/xmr/crash-transfer'):
                n = data.get('count', 1)
                if not isinstance(n, int) or not 0 <= n <= 100:
                    raise ValueError('count: 0-100')
                if path == '/test/xmr/refuse-relay':
                    c.refuse = n
                else:
                    self.crash = n
                return {'ok': True}
            raise ValueError('unknown test endpoint')


# --- HTTP ---------------------------------------------------------------------------------------------------------------

def _epee(v):
    """monero's epee serializer leaves out an empty array (get_transfers with nothing answers {})."""
    if isinstance(v, dict):
        return {k: _epee(x) for k, x in v.items() if not (isinstance(x, list) and not x)}
    if isinstance(v, list):
        return [_epee(x) for x in v]
    return v


def _reply(handler, body, status=200):
    raw = json.dumps(body, indent=2).encode()
    handler.send_response(status)
    handler.send_header('Content-Type', 'application/json')
    handler.send_header('Content-Length', str(len(raw)))
    handler.end_headers()
    handler.wfile.write(raw)


def _read(handler):
    n = int(handler.headers.get('Content-Length') or 0)
    try:
        return json.loads(handler.rfile.read(n) or b'{}')
    except ValueError:
        return None


def _json_rpc(handler, data, call):
    if not isinstance(data, dict):
        return _reply(handler, {'error': {'code': -32700, 'message': 'Parse error'}, 'id': 0, 'jsonrpc': '2.0'})
    rid = data.get('id')
    try:
        result = call(data.get('method', ''), data.get('params'))
    except RPCError as e:
        return _reply(handler, {'error': {'code': e.code, 'message': e.message}, 'id': rid, 'jsonrpc': '2.0'})
    _reply(handler, {'id': rid, 'jsonrpc': '2.0', 'result': _epee(result)})


def make_wallet_handler(rpc):
    from http.server import BaseHTTPRequestHandler

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass

        def do_GET(self):
            if self.path == '/health':
                return _reply(self, {'ok': True})
            if self.path == '/test/xmr/state':
                return _reply(self, rpc.chain.state() | rpc.state())
            _reply(self, {'error': 'not found'}, 404)

        def do_POST(self):
            data = _read(self)
            if self.path.startswith('/test/'):
                try:
                    return _reply(self, rpc.control(self.path, data if isinstance(data, dict) else {}))
                except (ValueError, KeyError, TypeError, AttributeError) as e:
                    return _reply(self, {'error': str(e)}, 400)
            if self.path != '/json_rpc':
                return _reply(self, {'error': 'not found'}, 404)
            try:
                _json_rpc(self, data, rpc.handle)
            except Crash:
                self.close_connection = True  # no answer: the request was read and the connection just ends

    return Handler


def make_daemon_handler(chain):
    from http.server import BaseHTTPRequestHandler

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass

        def do_GET(self):
            if self.path == '/health':
                return _reply(self, {'ok': True})
            _reply(self, {'error': 'not found'}, 404)

        def do_POST(self):
            data = _read(self)
            if self.path == '/json_rpc':
                return _json_rpc(self, data, lambda m, p: chain.rpc(m, p if isinstance(p, dict) else {}))
            res = chain.other(self.path, data if isinstance(data, dict) else {})
            if res is None:
                return _reply(self, {'error': 'not found'}, 404)
            _reply(self, _epee(res))

    return Handler


def serve(wallet_port, daemon_port, host='127.0.0.1'):
    """Starts the daemon and one escrow wallet-rpc on loopback in background threads; returns (chain, wallet-rpc, servers)."""
    from http.server import ThreadingHTTPServer
    chain = Chain()
    rpc = WalletRPC(chain)
    servers = [ThreadingHTTPServer((host, daemon_port), make_daemon_handler(chain)), ThreadingHTTPServer((host, wallet_port), make_wallet_handler(rpc))]
    for s in servers:
        threading.Thread(target=s.serve_forever, daemon=True).start()
    return chain, rpc, servers


if __name__ == '__main__':
    import argparse
    parser = argparse.ArgumentParser(description='Simulated Monero escrow daemon and wallet-rpc (SIMULATED: not chain evidence).')
    parser.add_argument('--print-address', metavar='LABEL', help='print the standard address of the outside wallet LABEL and exit')
    args = parser.parse_args()
    if args.print_address:
        print(outside_keys(args.print_address).primary)
