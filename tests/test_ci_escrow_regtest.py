"""Bitcoin deployment/test jobs are retired; Monero remains covered."""
from pathlib import Path
import json
import unittest

ROOT = Path(__file__).resolve().parents[1]

class MoneroOnlyCITest(unittest.TestCase):
    def test_no_bitcoin_jobs_or_browser_script(self):
        workflow = (ROOT / '.github/workflows/ci.yaml').read_text()
        self.assertNotIn('\n  escrow-regtest:', workflow)
        self.assertNotIn('\n  escrow-node-rehearsal:', workflow)
        self.assertNotIn('npm run test:e2e:escrow\n', workflow)
        self.assertNotIn('test:e2e:escrow', json.loads((ROOT / 'package.json').read_text())['scripts'])
        self.assertIn('\n  escrow-xmr-regtest:', workflow)
        self.assertIn('npm run test:e2e:escrow-xmr', workflow)
