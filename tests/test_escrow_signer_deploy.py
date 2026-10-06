"""Static checks on the escrow signer's deployment (A-454); no Docker needed. CI's compose job checks the effective
configuration (`docker compose config`): the signer's user, hardening, volumes and environment."""
from pathlib import Path
import re
import unittest

REPOSITORY = Path(__file__).resolve().parents[1]
COMPOSE_FILES = sorted(p.name for p in REPOSITORY.glob('compose*.yaml'))


def service_block(text, name):
    """The text of one service in a Compose file (up to the next service or top-level key)."""
    services = text.split('\nservices:\n', 1)[1]
    blocks = re.split(r'^  ([a-z][a-z0-9-]*):\n', re.split(r'^\S', services, maxsplit=1, flags=re.M)[0], flags=re.M)[1:]
    return dict(zip(blocks[::2], blocks[1::2])).get(name, '')


class EscrowSignerDeployTests(unittest.TestCase):
    def test_dockerfile_builds_the_signer_beside_the_server(self):
        dockerfile = (REPOSITORY / 'Dockerfile').read_text()
        self.assertIn('-o /escrow-signer ./cmd/escrow-signer', dockerfile)
        self.assertIn('COPY --from=build /escrow-signer /app/escrow-signer', dockerfile)
        # The seed directory belongs to the signer's user alone; the socket directory's group is the app's.
        self.assertIn('install -d -o 65533 -g 65533 -m 0700 /var/lib/escrow-signer', dockerfile)
        self.assertIn('install -d -o 65533 -g 65532 -m 0750 /run/escrow-signer', dockerfile)

    def test_signer_service_has_its_own_profile_and_volumes(self):
        nodes = (REPOSITORY / 'compose.nodes.yaml').read_text()
        signer = service_block(nodes, 'escrow-signer')
        self.assertIn('    profiles: [escrow-signer]\n', signer)
        self.assertIn('    entrypoint: [/app/escrow-signer]\n', signer)
        self.assertIn("    user: '65533:65532'\n", signer)
        self.assertIn('      - escrow_signer_seed:/var/lib/escrow-signer\n', signer)
        self.assertIn('      - escrow_signer_socket:/run/escrow-signer\n', signer)
        self.assertNotRegex(signer, r'(?m)^    (ports|expose):')
        app = service_block(nodes, 'app')
        self.assertIn('      - escrow_signer_socket:/run/escrow-signer\n', app)
        for name in COMPOSE_FILES:
            text = (REPOSITORY / name).read_text()
            with self.subTest(file=name):
                # The seed volume is mounted in the signer alone.
                self.assertEqual(text.count('escrow_signer_seed:/'), 1 if name == 'compose.nodes.yaml' else 0)

    def test_seed_is_in_no_environment_and_no_backup(self):
        example = (REPOSITORY / '.env.example').read_text()
        keys = [line.split('=', 1)[0] for line in example.splitlines() if line and not line.startswith('#')]
        self.assertIn('ESCROW_SIGNER_SOCKET', keys)
        self.assertIn('ESCROW_SIGNER_RPC_URL', keys)
        self.assertFalse([k for k in keys if k.startswith('ESCROW_SIGNER') and k not in ('ESCROW_SIGNER_SOCKET', 'ESCROW_SIGNER_RPC_URL')])
        for name in COMPOSE_FILES:
            text = (REPOSITORY / name).read_text()
            with self.subTest(file=name):
                for key in re.findall(r'(?m)^      (ESCROW_SIGNER_[A-Z_]+):', text):
                    self.assertIn(key, ('ESCROW_SIGNER_SOCKET', 'ESCROW_SIGNER_RPC_URL', 'ESCROW_SIGNER_SEED_FILE'))
        # The app gets the socket path only, never the signer's RPC URL.
        app = service_block((REPOSITORY / 'compose.yaml').read_text(), 'app')
        self.assertIn('ESCROW_SIGNER_SOCKET:', app)
        self.assertNotIn('ESCROW_SIGNER_RPC_URL', app)
        # scripts/backup.sh dumps the database only: no signer volume or seed file.
        backup = (REPOSITORY / 'scripts' / 'backup.sh').read_text()
        for word in ('escrow_signer', 'escrow-signer', 'ESCROW_SIGNER', '/var/lib/escrow-signer'):
            self.assertNotIn(word, backup)

    def test_operator_guide_documents_the_signer(self):
        guide = (REPOSITORY / 'docs' / 'operator-guide.md').read_text()
        section = guide.split('#### Escrow signer', 1)[1].split('\n#### ', 1)[0]
        for text in ('escrow-signer init', 'escrow-signer backup', 'escrow-signer restore', 'rpcwhitelist=opsecmkt-signer:decodepsbt,descriptorprocesspsbt',
                     'escrow_signer_fingerprint', 'escrow_signer_xpub_hmac', 'Break-glass', 'Upgrade order'):
            self.assertIn(text, section)


if __name__ == '__main__':
    unittest.main()
