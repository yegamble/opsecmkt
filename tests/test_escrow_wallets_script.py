"""scripts/test-escrow-wallets.sh and scripts/escrow-wallets.py (A-450) without Docker, Electrum or a node: the PSBT
helpers the legs rely on (the party copy the market combines keeps only the party's signatures, as withPartySigs
does), and the script's refusals before it downloads or starts anything (an unknown argument; a container name already
in use, which it never removes)."""
import base64
import importlib.util
import os
from pathlib import Path
import struct
import subprocess
import tempfile
import unittest

REPOSITORY = Path(__file__).resolve().parents[1]
SCRIPT = REPOSITORY / 'scripts' / 'test-escrow-wallets.sh'
spec = importlib.util.spec_from_file_location('escrow_wallets', REPOSITORY / 'scripts' / 'escrow-wallets.py')
legs = importlib.util.module_from_spec(spec)
spec.loader.exec_module(legs)


def unsigned_tx(inputs):
    tx = struct.pack('<i', 2) + bytes([inputs])
    for i in range(inputs):
        tx += bytes([i + 1]) * 32 + struct.pack('<I', i) + b'\x00' + struct.pack('<I', 0xfffffffd)
    return tx + b'\x01' + struct.pack('<q', 90000) + b'\x16\x00\x14' + b'\x22' * 20 + struct.pack('<I', 0)


PARTY, OTHER, MARKET = b'\x02' + b'\xaa' * 32, b'\x03' + b'\xbb' * 32, b'\x02' + b'\xcc' * 32
SIG = b'\x30\x06\x02\x01\x01\x02\x01\x01\x01'


class PSBTHelpersTest(unittest.TestCase):
    def stored(self):
        g = [(b'\x00', unsigned_tx(2))]
        ins = [[(b'\x01', b'\x00' * 8 + b'\x22\x00\x20' + b'\x11' * 32), (b'\x05', b'\x52' + b'\x00' * 104)] for _ in range(2)]
        return legs.psbt_ser(g, ins, [[]])

    def signed(self):
        """What a wallet like Electrum returns: the party's signature plus derivations for every key and a global xpub."""
        g, ins, outs = legs.psbt_parse(self.stored())
        g.append((b'\x01' + b'\x04\x35\x87\xcf' + b'\x00' * 41 + PARTY, legs.deriv_value('5cfaba73', [48 | legs.H])))
        for m in ins:
            m.append((b'\x02' + PARTY, SIG))
            for pub, fp in ((PARTY, '5cfaba73'), (OTHER, '3798bf66'), (MARKET, '5060212d')):
                m.append((b'\x06' + pub, legs.deriv_value(fp, [0, 0])))
        return legs.psbt_ser(g, ins, outs)

    def test_round_trip_and_strip(self):
        stored = self.stored()
        self.assertEqual(legs.psbt_ser(*legs.psbt_parse(stored)), stored)
        stripped = legs.psbt_parse(legs.strip_derivations(self.signed()))
        self.assertFalse([k for m in stripped[1] for k, _ in m if k[0] == 6])
        self.assertEqual([k for m in stripped[1] for k, _ in m if k[0] == 2], [b'\x02' + PARTY] * 2)

    def test_party_copy_keeps_only_the_party_signatures(self):
        stored, signed = self.stored(), self.signed()
        copy = legs.with_party_sigs(stored, signed, PARTY)
        g, ins, outs = legs.psbt_parse(copy)
        sg, sins, souts = legs.psbt_parse(stored)
        self.assertEqual(g, sg)
        for m, sm in zip(ins, sins):
            self.assertEqual(sorted(m), sorted(sm + [(b'\x02' + PARTY, SIG)]))
        self.assertTrue(legs.signed_by(copy, PARTY))
        self.assertFalse(legs.signed_by(stored, PARTY))
        with self.assertRaises(AssertionError):
            legs.with_party_sigs(stored, signed, OTHER)

    def test_describe_names_fields(self):
        text = legs.describe(self.signed(), {PARTY.hex(): 'buyer', MARKET.hex(): 'market'})
        self.assertIn('in0 PARTIAL_SIG key=buyer sighash=0x01', text)
        self.assertIn('in1 BIP32_DERIVATION key=market 5060212d:m/0/0', text)
        self.assertIn('global XPUB depth=0 fp=00000000 origin=5cfaba73:m/48h', text)

    def test_bip39_mnemonic_checksum(self):
        words = ['w%04d' % i for i in range(2048)]
        mnemonic = legs.bip39_mnemonic(words).split()
        self.assertEqual(len(mnemonic), 12)
        bits = ''.join(bin(words.index(w))[2:].zfill(11) for w in mnemonic)
        entropy = int(bits[:128], 2).to_bytes(16, 'big')
        import hashlib
        self.assertEqual(bits[128:], bin(hashlib.sha256(entropy).digest()[0])[2:].zfill(8)[:4])


class ScriptRefusalsTest(unittest.TestCase):
    def run_script(self, args, container_exists, database_url='postgres://unused/unused'):
        with tempfile.TemporaryDirectory() as tmp:
            calls = Path(tmp, 'docker-calls')
            for tool in ('curl', 'gpg', 'python3', 'go', 'git', 'tar', 'sha256sum'):
                Path(tmp, tool).write_text('#!/bin/sh\necho "unexpected $0" >&2\nexit 1\n')
            Path(tmp, 'docker').write_text('#!/bin/sh\necho "$@" >>"%s"\nexit %d\n' % (calls, 0 if container_exists else 1))
            for f in Path(tmp).iterdir():
                f.chmod(0o755)
            env = dict(os.environ, PATH=tmp + os.pathsep + '/usr/bin:/bin', ESCROW_WALLETS_CONTAINER='taken-name',
                       TEST_DATABASE_URL=database_url)
            r = subprocess.run(['bash', str(SCRIPT)] + args, env=env, capture_output=True, text=True, timeout=60)
            return r.returncode, r.stdout + r.stderr, calls.read_text() if calls.exists() else ''

    def test_unknown_argument(self):
        code, text, calls = self.run_script(['--mainnet'], False)
        self.assertEqual(code, 2)
        self.assertIn('usage:', text)
        self.assertEqual(calls, '')

    def test_existing_container_is_refused_and_left_alone(self):
        code, text, calls = self.run_script([], True)
        self.assertEqual(code, 2, text)
        self.assertIn('Refusing: a container named taken-name exists', text)
        self.assertEqual(calls.splitlines(), ['container inspect taken-name'])
        self.assertNotIn('unexpected', text)

    def test_missing_database_is_refused_before_docker(self):
        code, text, calls = self.run_script([], True, database_url='')
        self.assertEqual(code, 2, text)
        self.assertIn('TEST_DATABASE_URL must name a disposable local PostgreSQL database', text)
        self.assertEqual(calls, '')


if __name__ == '__main__':
    unittest.main()
