"""scripts/test-escrow-regtest.sh reports each leg by its own `--- PASS`/`--- FAIL`/`--- SKIP` line (a failing leg
never makes a passing one read as failed), fails unless every leg passed and go test exited 0, and documents the
bitcoind command with -fallbackfee (A-321, QA31-6). A fake `go` and a fake regtest node; no real node needed."""
from http.server import BaseHTTPRequestHandler, HTTPServer
import json
import os
from pathlib import Path
import subprocess
import tempfile
import threading
import unittest

SCRIPT = Path(__file__).resolve().parents[1] / 'scripts' / 'test-escrow-regtest.sh'
LEGS = ('TestEscrowRegtestPracticeSign', 'TestEscrowRegtestFunding', 'TestEscrowRegtestSettlement', 'TestEscrowRegtestCosign',
        'TestEscrowRegtestRecovery', 'TestEscrowRegtestGate', 'TestEscrowRegtestBump', 'TestEscrowRegtestPhysical',
        'TestEscrowRegtestDeadline', 'TestEscrowRegtestReserve', 'TestEscrowRegtestConflicted', 'TestEscrowRegtestBIP32',
        'TestEscrowRegtestSigner', 'TestEscrowRegtestPooled', 'TestEscrowRegtestArbiterRestore',
        'TestEscrowRegtestArbiterDeposit', 'TestEscrowRegtestPrivateBroadcast',
        'TestEscrowRegtestPerOrderPractice', 'TestEscrowRegtestPerOrderEscrow', 'TestEscrowRegtestPerOrderRepair',
        'TestEscrowRegtestSmoke')


class FakeRegtest(BaseHTTPRequestHandler):
    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        reply = json.dumps({'result': {'chain': 'regtest'}, 'error': None, 'id': body['id']}, separators=(',', ':')).encode()
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(reply)))
        self.end_headers()
        self.wfile.write(reply)

    def log_message(self, *args):
        pass


class EscrowRegtestScriptTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.server = HTTPServer(('127.0.0.1', 0), FakeRegtest)
        threading.Thread(target=cls.server.serve_forever, daemon=True).start()
        cls.url = 'http://user:not-a-real-password@127.0.0.1:%d' % cls.server.server_port

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        cls.server.server_close()

    def run_script(self, output, status):
        with tempfile.TemporaryDirectory() as tmp:
            fake = Path(tmp, 'go')
            fake.write_text('#!/usr/bin/env bash\ncat "$FAKE_GO_OUTPUT"\nexit "$FAKE_GO_STATUS"\n')
            fake.chmod(0o755)
            out = Path(tmp, 'output')
            out.write_text(''.join(line + '\n' for line in output))
            env = dict(os.environ, PATH=tmp + os.pathsep + os.environ['PATH'], FAKE_GO_OUTPUT=str(out),
                       FAKE_GO_STATUS=str(status), ESCROW_REGTEST_OPERATOR_RPC_URL=self.url,
                       ESCROW_REGTEST_APP_RPC_URL=self.url, ESCROW_REGTEST_POOLED_RPC_URL=self.url,
                       ESCROW_REGTEST_SIGNER_RPC_URL=self.url, ESCROW_REGTEST_PRIVATE_OPERATOR_RPC_URL=self.url,
                       ESCROW_REGTEST_PRIVATE_APP_RPC_URL=self.url, TEST_DATABASE_URL='postgres://unused/none',
                       ESCROW_REGTEST_LOG=str(Path(tmp, 'transcript.log')))
            env.pop('NO_PROXY', None)
            r = subprocess.run(['bash', str(SCRIPT)], env=env, capture_output=True, text=True, timeout=60)
            return r.returncode, r.stdout + r.stderr

    def summary(self, text):
        """The script's own report lines (not the transcript go test printed)."""
        return [l for l in text.splitlines() if not l.startswith(('---', '===', 'ok', 'FAIL\t', 'PASS\n'))]

    def test_one_leg_failing_reports_the_other_as_passed(self):
        code, text = self.run_script(['--- PASS: %s (1.0s)' % LEGS[0], '--- FAIL: %s (2.0s)' % LEGS[1], 'FAIL'], 1)
        self.assertEqual(code, 1)
        report = '\n'.join(self.summary(text))
        self.assertIn('PASS: %s' % LEGS[0], report)
        self.assertIn('FAILED: %s' % LEGS[1], report)
        self.assertNotIn('%s; see' % LEGS[0], report.replace('PASS: %s' % LEGS[0], ''))

    def test_both_legs_pass(self):
        code, text = self.run_script(['--- PASS: %s (1.0s)' % l for l in LEGS] + ['PASS'], 0)
        self.assertEqual(code, 0, text)
        for leg in LEGS:
            self.assertIn('PASS: %s' % leg, '\n'.join(self.summary(text)))

    def test_skipped_or_missing_leg_fails(self):
        code, text = self.run_script(['--- PASS: %s (1.0s)' % LEGS[0], '--- SKIP: %s (0.0s)' % LEGS[1], 'PASS'], 0)
        self.assertEqual(code, 1)
        self.assertIn('SKIPPED: %s' % LEGS[1], text)
        code, text = self.run_script(['# opsecmkt/internal/market', 'build failed'], 1)
        self.assertEqual(code, 1)
        for leg in LEGS:
            self.assertIn('NOT RUN: %s' % leg, text)
        # go test exits 0 when -run matches no test: that is not a pass either.
        code, text = self.run_script(['testing: warning: no tests to run', 'PASS'], 0)
        self.assertEqual(code, 1)
        for leg in LEGS:
            self.assertIn('NOT RUN: %s' % leg, text)

    def test_failed_leg_fails_whatever_go_exits(self):
        code, text = self.run_script(['--- PASS: %s (1.0s)' % LEGS[0], '--- FAIL: %s (2.0s)' % LEGS[1]], 0)
        self.assertEqual(code, 1)
        self.assertIn('FAILED: %s' % LEGS[1], text)

    def test_nonzero_go_status_fails_even_when_legs_pass(self):
        code, text = self.run_script(['--- PASS: %s (1.0s)' % l for l in LEGS], 1)
        self.assertEqual(code, 1)
        self.assertIn('go test exited 1', text)

    def test_transcript_with_a_tprv_fails(self):
        tprv = 'tprv8ZgxMBicQKsPd' + 'A' * 94  # shaped like a serialized test-network private key (not a real one)
        code, text = self.run_script(['--- PASS: %s (1.0s)' % l for l in LEGS] + ['    leg.go:1: root ' + tprv, 'PASS'], 0)
        self.assertEqual(code, 1)
        self.assertIn('holds a serialized tprv', text)

    def test_signer_pooled_and_private_node_urls_are_required(self):
        # A-440's pooled user, A-454's signer and A-441's private-broadcast node: without any one the script refuses to start.
        for missing in ('ESCROW_REGTEST_SIGNER_RPC_URL', 'ESCROW_REGTEST_POOLED_RPC_URL',
                        'ESCROW_REGTEST_PRIVATE_OPERATOR_RPC_URL', 'ESCROW_REGTEST_PRIVATE_APP_RPC_URL'):
            with tempfile.TemporaryDirectory() as tmp:
                env = dict(os.environ, ESCROW_REGTEST_OPERATOR_RPC_URL=self.url, ESCROW_REGTEST_APP_RPC_URL=self.url,
                           ESCROW_REGTEST_POOLED_RPC_URL=self.url, ESCROW_REGTEST_SIGNER_RPC_URL=self.url,
                           ESCROW_REGTEST_PRIVATE_OPERATOR_RPC_URL=self.url, ESCROW_REGTEST_PRIVATE_APP_RPC_URL=self.url,
                           TEST_DATABASE_URL='postgres://unused/none', ESCROW_REGTEST_LOG=str(Path(tmp, 't.log')))
                env.pop(missing)
                r = subprocess.run(['bash', str(SCRIPT)], env=env, capture_output=True, text=True, timeout=60)
                self.assertEqual(r.returncode, 2, missing)
                self.assertIn(missing, r.stderr)

    def test_documented_node_has_fallbackfee(self):
        head = SCRIPT.read_text().split('set -euo pipefail')[0]
        self.assertIn('bitcoind -regtest -fallbackfee=0.0002', head)
        # The escrow signer's own user, whitelisted to exactly what it calls (A-454).
        self.assertIn('-rpcwhitelist=opsecmkt-signer:decodepsbt,descriptorprocesspsbt,getdescriptorinfo \\\n', head)
        # The pooled user, whitelisted to the pooled methods only (A-440; the Go test pins the list to the code).
        self.assertIn('"-rpcauth=opsecmkt-pooled:', head)
        self.assertRegex(head, r'-rpcwhitelist=opsecmkt-pooled:[a-z,]+ \\\n')
        # The A-441 node: private broadcast through a proxy nobody answers; never -connect or -onlynet, never public.
        self.assertIn('-privatebroadcast -proxy=127.0.0.1:9 -listen=0', head)
        self.assertNotRegex(head, r'-(connect|onlynet|addnode|seednode)=')


class PrivateSignetModeTest(unittest.TestCase):
    """A-443: --private-signet starts its own two signet nodes; checked here without Docker or a node (the real run is
    manual: docs/testnet-runbook.md section 11)."""

    def body(self):
        text = SCRIPT.read_text()
        return text[text.index('private_signet() {'):text.index('\ncase "${1:-}" in')]

    def run_script(self, args, env_extra=None):
        env = dict(os.environ, **(env_extra or {}))
        env.pop('TEST_DATABASE_URL', None)
        return subprocess.run(['bash', str(SCRIPT)] + args, env=env, capture_output=True, text=True, timeout=60)

    def test_needs_a_database_and_refuses_unknown_arguments(self):
        r = self.run_script(['--private-signet'])
        self.assertEqual(r.returncode, 2)
        self.assertIn('TEST_DATABASE_URL', r.stderr)
        r = self.run_script(['--signet'])
        self.assertEqual(r.returncode, 2)
        self.assertIn('Usage', r.stderr)

    def test_nodes_are_a_private_signet_with_no_route_off_the_host(self):
        body = self.body()
        self.assertIn('docker network create --internal', body)
        self.assertIn('-chain=signet -signetchallenge=51 -dnsseed=0 -fixedseeds=0', body)
        self.assertNotRegex(body, r'docker (run|create)[^\n]* -p ')  # no published port
        self.assertNotRegex(body, r'-(onlynet|addnode|seednode)=')
        # Node A makes no outbound connection; node B connects to node A only.
        self.assertIn('-connect=0 -listen=1', body)
        self.assertIn('"-connect=$a:38333" -listen=0', body)
        # Both refuse anything but signet with challenge 51 before the legs run.
        self.assertIn('\'"signet_challenge":"51"\'', body)
        # Containers, their volumes and the network go on every exit.
        self.assertIn("docker rm -f -v '$a' '$b'", body)
        self.assertIn("docker network rm '$net'", body)

    def test_runs_the_legs_without_regtest_only_rpcs_and_labels_everything(self):
        body = self.body()
        legs = body.split('local legs=(')[1].split(')')[0].split()
        self.assertNotIn('Recovery', legs)
        self.assertNotIn('Conflicted', legs)
        self.assertNotIn('PrivateBroadcast', legs)
        self.assertIn('Isolated', legs)
        for leg in LEGS:
            name = leg[len('TestEscrowRegtest'):]
            if name not in legs:
                self.assertIn('NOT RUN on the private signet: %s (' % leg, body)
        self.assertIn('label="private signet, not public"', body)
        self.assertIn('sed -u "s/^/[$label] /"', body)
        # The header: node version, commit, redacted command line, finish time.
        for want in ('# node version:', '# commit:', 'credentials redacted', '<redacted>', 'finished:'):
            self.assertIn(want, body)
        self.assertNotRegex(body, r'echo "# command line[^\n]*_pass')


if __name__ == '__main__':
    unittest.main()
