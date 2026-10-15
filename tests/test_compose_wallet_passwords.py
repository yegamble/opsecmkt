"""The bundled Monero wallet-RPC is reserved for escrow after pooled removal."""
from pathlib import Path
import unittest

REPOSITORY = Path(__file__).resolve().parents[1]
NODES = REPOSITORY / 'compose.nodes.yaml'


class WalletPasswordTextTests(unittest.TestCase):
    def test_only_escrow_wallet_rpc_is_configured(self):
        text = NODES.read_text()
        header = text.split('\nservices:\n', 1)[0]
        flat = ' '.join(line.lstrip('# ') for line in header.splitlines())
        self.assertNotIn('bitcoin:', text)
        self.assertIn('Escrow wallet startup requires ESCROW_XMR_WALLET_RPC_PASSWORD', flat)
        self.assertNotIn('monero-wallet:', text)
        self.assertNotIn('MONERO_WALLET_RPC_PASSWORD', text)
        self.assertIn('ESCROW_XMR_WALLET_RPC_PASSWORD-is-blank', text)

    def test_no_bitcoin_services_or_signer_binary_are_shipped(self):
        for filename in ('compose.nodes.yaml', 'compose.nodes-tor.yaml'):
            text = (REPOSITORY / filename).read_text()
            self.assertNotIn('  bitcoin:', text)
            self.assertNotIn('  escrow-signer:', text)
            self.assertNotIn('escrow_signer_socket:', text)
        self.assertNotIn('/app/escrow-signer', (REPOSITORY / 'Dockerfile').read_text())
        self.assertFalse((REPOSITORY / 'cmd/escrow-signer').exists())

    def test_escrow_wallet_login_follows_its_password(self):
        text = NODES.read_text()
        self.assertIn("--rpc-login '${ESCROW_XMR_WALLET_RPC_PASSWORD:+escrow}:${ESCROW_XMR_WALLET_RPC_PASSWORD:-}'", text)
        self.assertNotIn("--rpc-login 'escrow:", text)

    def test_historical_wallet_volume_is_declared_for_recovery(self):
        text = NODES.read_text().split('\nvolumes:\n', 1)[1].split('\nnetworks:\n', 1)[0]
        self.assertRegex(text, r'(?m)^  monero_wallet:$')
        self.assertNotIn('monero_wallet:/home/monero/wallet', NODES.read_text())

    def test_legacy_payment_mappings_stay_blank_by_default_for_startup_guard(self):
        compose = (REPOSITORY / 'compose.yaml').read_text()
        for key in ('BITCOIN_RPC_URL', 'BITCOIN_WALLET', 'MONERO_RPC_URL', 'MONERO_WALLET_RPC_URL'):
            with self.subTest(key=key):
                self.assertIn(f'{key}: ${{{key}:-}}', compose)


if __name__ == '__main__':
    unittest.main()
