"""Offline verification uses exact signed bytes and an external public pin."""
import importlib.util
import json
from pathlib import Path
import shutil
import stat
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / 'scripts' / 'verify-moderation-escalation.py'
spec = importlib.util.spec_from_file_location('moderation_packet_verifier', SCRIPT)
verifier = importlib.util.module_from_spec(spec)
spec.loader.exec_module(verifier)


def payload_bytes():
    # Non-canonical spacing, escaped HTML and literal Unicode are intentional:
    # rebuilding JSON before verification would invalidate this signature.
    return ('{ "version" : "1", "decision": {"proposal":{"summary":"review \\u003cdata\\u003e café"}},'
            '"context":{},"evidence_digest":"' + 'a' * 64 + '",'
            '"expires_utc":"2026-10-25T01:00:00Z","default_action":"HOLD",'
            '"alternatives":["preserve current state"] }').encode('utf-8')


@unittest.skipUnless(shutil.which('openssl'), 'OpenSSL required for offline signature interoperability')
class OfflineEscalationVerifierTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='moderation-verifier-test-')
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.key = self.root / 'key.pem'
        subprocess.run(['openssl', 'genpkey', '-algorithm', 'ED25519', '-out', str(self.key)],
                       check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        public = subprocess.check_output(['openssl', 'pkey', '-in', str(self.key), '-pubout', '-outform', 'DER'],
                                         stderr=subprocess.DEVNULL)
        self.assertEqual(public[:12], verifier.PUBLIC_DER_PREFIX)
        self.pin = self.root / 'pin'
        self.pin.write_text(public[12:].hex() + '\n')
        self.payload = payload_bytes()
        message = self.root / 'message'
        message.write_bytes(self.payload)
        signature = subprocess.check_output(['openssl', 'pkeyutl', '-sign', '-rawin', '-inkey', str(self.key),
                                             '-in', str(message)], stderr=subprocess.DEVNULL)
        self.signature = signature.hex()
        self.public = public[12:].hex()
        self.packet = self.root / 'packet'
        self.packet.write_bytes(self.envelope(self.payload))
        self.output = self.root / 'verified.json'

    def envelope(self, payload, signature=None, public=None):
        # The outer key order is deliberately different from the Go worker.
        return (b'{"signature":"' + (signature or self.signature).encode('ascii') +
                b'","payload":' + payload + b',"public_key":"' +
                (public or self.public).encode('ascii') + b'"}\n')

    def invoke(self):
        return subprocess.run([sys.executable, str(SCRIPT), '--packet', str(self.packet),
                               '--public-key', str(self.pin), '--out', str(self.output)],
                              capture_output=True, text=True, timeout=15)

    def test_exact_payload_and_public_pin_interoperate_with_openssl(self):
        result = self.invoke()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.output.read_bytes(), self.payload)
        self.assertEqual(stat.S_IMODE(self.output.stat().st_mode), 0o600)
        self.assertNotIn('café', result.stdout)

    def test_payload_mutation_and_reserialization_are_rejected(self):
        for changed in (self.payload.replace(b'preserve current state', b'release all funds'),
                        json.dumps(json.loads(self.payload), separators=(',', ':')).encode('utf-8')):
            with self.subTest(changed=changed[:40]):
                self.packet.write_bytes(self.envelope(changed))
                self.assertNotEqual(self.invoke().returncode, 0)
                self.assertFalse(self.output.exists())

    def test_packet_supplied_key_cannot_replace_external_pin(self):
        self.pin.write_text('0' * 64 + '\n')
        self.assertNotEqual(self.invoke().returncode, 0)
        self.assertFalse(self.output.exists())

    def test_refuses_overwrite(self):
        self.output.write_bytes(b'preserve existing file')
        self.assertNotEqual(self.invoke().returncode, 0)
        self.assertEqual(self.output.read_bytes(), b'preserve existing file')

    def test_proposal_text_is_not_printed_or_executed(self):
        marker = self.root / 'should-not-exist'
        body = json.loads(self.payload)
        body['decision']['proposal']['summary'] = '$(touch ' + str(marker) + ')'
        payload = json.dumps(body, ensure_ascii=False, separators=(',', ':')).encode('utf-8')
        message = self.root / 'message'
        message.write_bytes(payload)
        signature = subprocess.check_output(['openssl', 'pkeyutl', '-sign', '-rawin', '-inkey', str(self.key),
                                             '-in', str(message)], stderr=subprocess.DEVNULL)
        self.packet.write_bytes(self.envelope(payload, signature=signature.hex()))
        result = self.invoke()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(marker.exists())
        self.assertNotIn('touch', result.stdout + result.stderr)
        self.assertEqual(self.output.read_bytes(), payload)

    def test_bounded_packet_and_public_key_inputs(self):
        self.packet.write_bytes(b' ' * (verifier.MAX_PACKET_BYTES + 1))
        self.assertNotEqual(self.invoke().returncode, 0)
        self.assertFalse(self.output.exists())
        self.packet.write_bytes(self.envelope(self.payload))
        self.pin.write_text('a' * 128)
        self.assertNotEqual(self.invoke().returncode, 0)
        self.assertFalse(self.output.exists())


class EscalationEnvelopeParsingTests(unittest.TestCase):
    def test_rejects_duplicate_unknown_null_and_nonfinite_fields(self):
        payload = payload_bytes()
        valid = (b'{"payload":' + payload + b',"signature":"' + b'b' * 128 +
                 b'","public_key":"' + b'c' * 64 + b'"}')
        invalid = [valid[:-1] + b',"unknown":true}',
                   valid[:-1] + b',"public_key":"' + b'c' * 64 + b'"}',
                   valid.replace(b'"context":{}', b'"context":{"x":1,"x":2}'),
                   valid.replace(b'"context":{}', b'"context":null'),
                   valid.replace(b'"context":{}', b'"context":{"x":NaN}'),
                   valid.replace(b'"context":{}', b'"context":{"x":1e999}'),
                   valid + b'{}', valid[:-1] + b',}',
                   valid.replace(b'"version" : "1"', b'"version" : "2"'),
                   valid.replace(b'2026-10-25T01:00:00Z', b'2026-10-25T01:00:00+00:00')]
        for raw in invalid:
            with self.subTest(raw=raw[:60]):
                with self.assertRaises(ValueError):
                    verifier.parse_envelope(raw)

    def test_outer_whitespace_and_nested_payload_named_field_preserve_bytes(self):
        payload = payload_bytes().replace(b'"context":{}', b'"context":{"payload":"nested data"}')
        raw = b' \n{ "public_key":"' + b'c' * 64 + b'", "payload" : ' + payload + b', "signature":"' + b'b' * 128 + b'" }\t'
        _, exact = verifier.parse_envelope(raw)
        self.assertEqual(exact, payload)


if __name__ == '__main__':
    unittest.main()
