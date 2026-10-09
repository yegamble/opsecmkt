"""Offline guard and record tests for the manual public-stagenet smoke."""

import importlib.util
import io
import json
from datetime import datetime, timezone
from pathlib import Path
from types import SimpleNamespace
import unittest
from unittest.mock import patch


HERE = Path(__file__).resolve().parent.parent
spec = importlib.util.spec_from_file_location("xmr_public", HERE / "scripts/test-escrow-xmr-public-stagenet.py")
xmr = importlib.util.module_from_spec(spec)
spec.loader.exec_module(xmr)


class PublicXMRScriptTests(unittest.TestCase):
    def setUp(self):
        self.now = datetime(2026, 10, 9, 12, 0, tzinfo=timezone.utc)
        self.info = {"nettype": "stagenet", "stagenet": True, "mainnet": False, "testnet": False,
                     "height": 1000, "target_height": 1000, "synchronized": True, "offline": False,
                     "restricted": False, "outgoing_connections_count": 1, "incoming_connections_count": 0}
        self.fork = {"version": 16}
        self.header = {"block_header": {"timestamp": int(self.now.timestamp())}}

    def test_preflight_refuses_wrong_or_unreadable_public_node(self):
        self.assertEqual(xmr.daemon_refusal(self.info, self.fork, self.header, self.now), "")
        cases = (
            ("nettype", "mainnet"), ("nettype", "testnet"), ("nettype", "fakechain"),
            ("stagenet", False), ("mainnet", True), ("restricted", True),
            ("offline", True), ("synchronized", False), ("target_height", 1001),
            ("outgoing_connections_count", 0), ("height", None),
        )
        for field, value in cases:
            with self.subTest(field=field, value=value):
                info = dict(self.info, **{field: value})
                self.assertTrue(xmr.daemon_refusal(info, self.fork, self.header, self.now))
        for fork in (15, None):
            self.assertTrue(xmr.daemon_refusal(self.info, {"version": fork}, self.header, self.now))
        for offset in (-3601, 301):
            head = {"block_header": {"timestamp": int(self.now.timestamp()) + offset}}
            self.assertTrue(xmr.daemon_refusal(self.info, self.fork, head, self.now))

    def test_independent_recent_public_block_anchor_refuses_private_fork(self):
        block_hash = "a" * 64
        source = "https://stagenet.xmrchain.net/block/" + block_hash
        block = {"block_header": {"height": 990, "hash": block_hash}}
        self.assertEqual(xmr.anchor_refusal(1000, 990, block_hash, source, block, True), "")
        for current, returned, given_source in (
            (1011, block, source),
            (1000, {"block_header": {"height": 990, "hash": "b" * 64}}, source),
            (1000, block, "https://xmrchain.net/block/" + block_hash),
            (1000, block, "https://user:pass@stagenet.xmrchain.net/block/" + block_hash),
        ):
            with self.subTest(current=current, returned=returned, source=given_source):
                self.assertTrue(xmr.anchor_refusal(current, 990, block_hash, given_source, returned, True))
        self.assertEqual(xmr.anchor_refusal(1011, 990, block_hash, source, block, False), "")

    def test_preflight_checks_anchor_from_daemon_before_any_wallet_work(self):
        block_hash = "a" * 64
        source = "https://stagenet.xmrchain.net/block/" + block_hash
        answers = {"get_info": self.info, "hard_fork_info": self.fork,
                   "get_last_block_header": self.header,
                   "get_block_header_by_height": {"block_header": {"height": 990, "hash": block_hash}}}
        called = []

        def rpc(_raw, method, params=None):
            called.append((method, params))
            return answers[method]

        with patch.object(xmr, "rpc", side_effect=rpc), patch.object(xmr, "datetime") as clock:
            clock.now.return_value = self.now
            xmr.preflight("http://127.0.0.1:38081", 990, block_hash, source, True)
        self.assertEqual(called, [("get_info", None), ("hard_fork_info", None),
                                  ("get_last_block_header", None),
                                  ("get_block_header_by_height", {"height": 990})])
        answers["get_block_header_by_height"] = {"block_header": {"height": 990, "hash": "b" * 64}}
        with patch.object(xmr, "rpc", side_effect=rpc), patch.object(xmr, "datetime") as clock:
            clock.now.return_value = self.now
            with self.assertRaises(xmr.Refused):
                xmr.preflight("http://127.0.0.1:38081", 990, block_hash, source, True)

    def test_loopback_and_distinct_endpoint_contract(self):
        self.assertTrue(xmr.loopback_url("http://user:secret@127.0.0.1:38081"))
        self.assertTrue(xmr.authenticated_loopback_url("http://user:secret@127.0.0.1:38081"))
        self.assertFalse(xmr.authenticated_loopback_url("http://127.0.0.1:38081"))
        self.assertFalse(xmr.authenticated_loopback_url("http://user:@127.0.0.1:38081"))
        self.assertTrue(xmr.loopback_url("http://[::1]:38081"))
        self.assertTrue(xmr.loopback_url("postgres://u:p@localhost:59954/public_xmr?sslmode=disable", ("postgres",)))
        for raw in ("http://example.org:18081", "http://8.8.8.8:18081", "https://127.0.0.1:18081",
                    "http://127.0.0.1:18081/json_rpc", "http://127.0.0.1:18081?x=1", "http://127.0.0.1"):
            self.assertFalse(xmr.loopback_url(raw), raw)

    def test_rpc_uses_curl_digest_with_private_stdin_login_and_bounded_direct_ipv6(self):
        seen = {}

        class Input:
            def write(self, data):
                seen["config"] = data

            def close(self):
                pass

        class Proc:
            stdin = Input()
            stdout = io.BytesIO(b'{"result":{"version":16}}')

            def wait(self, timeout=None):
                return 0

            def poll(self):
                return 0

        def popen(args, **kwargs):
            seen["args"] = args
            seen["kwargs"] = kwargs
            return Proc()

        with patch.object(xmr.subprocess, "Popen", side_effect=popen):
            self.assertEqual(xmr.rpc("http://user:secret@[::1]:38081", "hard_fork_info"), {"version": 16})
        self.assertEqual(seen["args"][-1], "http://[::1]:38081/json_rpc")
        self.assertEqual(seen["args"][1], "--disable")
        self.assertIn("--digest", seen["args"])
        self.assertEqual(seen["args"][seen["args"].index("--noproxy") + 1], "*")
        self.assertEqual(seen["args"][seen["args"].index("--proto") + 1], "=http")
        self.assertNotIn("--location", seen["args"])
        self.assertNotIn("--max-redirs", seen["args"])
        self.assertEqual(seen["args"][seen["args"].index("--max-filesize") + 1], str(xmr.MAX_RPC_BYTES))
        self.assertNotIn("secret", " ".join(seen["args"]))
        self.assertIn(b'user:secret', seen["config"])
        self.assertIs(seen["kwargs"]["stderr"], xmr.subprocess.DEVNULL)
        with self.assertRaises(xmr.Refused):
            xmr.curl_config_value("bad\nsecret")

        class FailedProc(Proc):
            stdout = io.BytesIO(b'{"error":"private-secret-should-not-print"}')

            def wait(self, timeout=None):
                return 22

        with patch.object(xmr.subprocess, "Popen", return_value=FailedProc()):
            with self.assertRaisesRegex(xmr.Refused, "daemon hard_fork_info is unreadable") as cm:
                xmr.rpc("http://user:secret@[::1]:38081", "hard_fork_info")
            self.assertNotIn("private-secret", str(cm.exception))

    def test_fetched_public_explorer_anchor_is_independent_and_exact(self):
        block_hash = "a" * 64
        seen = {}

        class Response(io.BytesIO):
            status = 200

        class Opener:
            def __init__(self, payload):
                self.payload = payload

            def open(self, req, timeout):
                seen["url"] = req.full_url
                seen["timeout"] = timeout
                return Response(json.dumps(self.payload).encode())

        def fetch(payload):
            with patch.object(xmr, "build_opener", side_effect=lambda *handlers: (
                    seen.update(handlers=handlers) or Opener(payload))):
                return xmr.fetch_public_anchor(990, block_hash)

        good = {"status": "success", "data": {"block_height": 990, "hash": block_hash}}
        proof = fetch(good)
        self.assertEqual(proof["api_url"], "https://stagenet.xmrchain.net/api/block/990")
        self.assertEqual(proof["height"], 990)
        self.assertEqual(proof["hash"], block_hash)
        self.assertEqual(seen["url"], proof["api_url"])
        self.assertEqual(seen["timeout"], 15)
        self.assertTrue(any(isinstance(h, xmr.NoRedirect) for h in seen["handlers"]))
        self.assertTrue(any(isinstance(h, xmr.ProxyHandler) and h.proxies == {} for h in seen["handlers"]))
        for payload in ({"status": "success", "data": {"block_height": 991, "hash": block_hash}},
                        {"status": "success", "data": {"block_height": 990, "hash": "b" * 64}},
                        {"status": "failed", "data": {"block_height": 990, "hash": block_hash}}):
            with self.subTest(payload=payload), self.assertRaises(xmr.Refused):
                fetch(payload)
        with self.assertRaisesRegex(xmr.Refused, "too large"):
            fetch({**good, "padding": "x" * xmr.MAX_EXPLORER_BYTES})

    def test_manifest_proof_rejects_missing_secret_or_impossible_timing(self):
        events = {}
        for i, name in enumerate(xmr.EVENTS):
            events[name] = {"at": f"2026-10-09T12:0{i}:00Z", "height": 1000 + i}
            if name != "settlement_requested":
                events[name]["txid"] = "a" * 64 if name.startswith("funding") else "b" * 64
        proof = {"network": "stagenet", "hard_fork": 16, "start_height": 1000,
                 "anchor_height": 990, "anchor_hash": "c" * 64,
                 "anchor_source": "https://stagenet.xmrchain.net/block/" + "c" * 64,
                 "escrow_wallet_rpc_version": 305419896, "funder_wallet_rpc_version": 305419896,
                 "funding_confirmations": 10, "settlement_confirmations": 10,
                 "claim_received_piconero": 9_000_000_000, **events}
        xmr.validate_proof(proof)
        for change in ({"network": "fakechain"}, {"funding_confirmations": 0},
                       {"claim_received_piconero": 0}, {"private_seed": "x"}):
            with self.subTest(change=change):
                with self.assertRaises(xmr.Refused):
                    xmr.validate_proof(dict(proof, **change))
        reordered = dict(proof, settlement_built={**events["settlement_built"], "at": "2026-10-09T11:00:00Z"})
        with self.assertRaises(xmr.Refused):
            xmr.validate_proof(reordered)
        for text in ("password-here", "rpcpassword=abc", "http://user:pass@127.0.0.1:38081",
                     "-----BEGIN PGP PRIVATE KEY BLOCK-----"):
            with self.assertRaises(xmr.Refused):
                xmr.secret_scan(text, ["password-here"])

    def test_running_images_and_fresh_wallet_mount_are_bound_to_record(self):
        image_id = "sha256:" + "a" * 64
        digest = "repo/wallet@sha256:" + "b" * 64
        image = SimpleNamespace(stdout=f"{image_id}|{digest}\n")
        ports = {"18082/tcp": [{"HostIp": "127.0.0.1", "HostPort": "38082"}]}
        mounts = [{"Type": "bind", "Source": "/tmp/fresh-wallet", "Destination": "/wallet"}]
        mounted = SimpleNamespace(stdout=f'{image_id}|true|{json.dumps(mounts)}|{json.dumps(ports)}\n')
        command = SimpleNamespace(stdout='["--wallet-dir","/wallet","--rpc-login","private:secret"]\n')
        rpc_url = "http://user:secret@127.0.0.1:38082"
        with patch.object(xmr.subprocess, "run", side_effect=(image, mounted, command)):
            self.assertEqual(xmr.docker_metadata("repo/wallet:v1", "escrow-wallet", rpc_url, Path("/tmp/fresh-wallet")), digest)
        wrong_image = SimpleNamespace(stdout=f'sha256:{"c" * 64}|true|[]|{json.dumps(ports)}\n')
        with patch.object(xmr.subprocess, "run", side_effect=(image, wrong_image)):
            with self.assertRaises(xmr.Refused):
                xmr.docker_metadata("repo/wallet:v1", "escrow-wallet", rpc_url)
        with patch.object(xmr.subprocess, "run", side_effect=(image, mounted)):
            with self.assertRaises(xmr.Refused):
                xmr.docker_metadata("repo/wallet:v1", "funder-wallet", rpc_url, forbidden_dir=Path("/tmp/fresh-wallet"))
        with patch.object(xmr.subprocess, "run", side_effect=(image, mounted)):
            with self.assertRaises(xmr.Refused):
                xmr.docker_metadata("repo/wallet:v1", "escrow-wallet", "http://127.0.0.1:39000")
        unrelated = [{"Type": "bind", "Source": "/tmp/fresh-wallet", "Destination": "/other"}]
        wrong_mount = SimpleNamespace(stdout=f'{image_id}|true|{json.dumps(unrelated)}|{json.dumps(ports)}\n')
        with patch.object(xmr.subprocess, "run", side_effect=(image, wrong_mount, command)):
            with self.assertRaises(xmr.Refused):
                xmr.docker_metadata("repo/wallet:v1", "escrow-wallet", rpc_url, Path("/tmp/fresh-wallet"))


if __name__ == "__main__":
    unittest.main()
