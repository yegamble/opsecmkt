import unittest
from pathlib import Path

from scripts.tor_network_address import app_backend_ipv4


NETWORK_ID = "7d86d31b1478e7cca9ebed7e73aa0fdeec46c5ca29497431d3007d2d9e15ed99"
APP_ID = "19a4d5d687db25203351ed79d478946f861258f018fe384f229f2efa4b23513c"
REPOSITORY = Path(__file__).resolve().parents[1]


def network_with(address="172.19.0.2/16", **overrides):
    network = {
        "Id": NETWORK_ID,
        "Internal": True,
        "Containers": {
            APP_ID: {"Name": "a144-app-1", "IPv4Address": address, "IPv6Address": ""},
            "b" * 64: {"Name": "a144-db-1", "IPv4Address": "172.19.0.3/16", "IPv6Address": ""},
        },
    }
    network.update(overrides)
    return network


class TorNetworkAddressTests(unittest.TestCase):
    def test_uses_exact_container_entry_and_strips_cidr_prefix(self):
        self.assertEqual(app_backend_ipv4(network_with(), APP_ID, NETWORK_ID), "172.19.0.2")

    def test_refuses_a_container_not_listed_on_that_network(self):
        with self.assertRaisesRegex(ValueError, "not attached"):
            app_backend_ipv4(network_with(), "c" * 64, NETWORK_ID)

    def test_refuses_network_identity_or_internal_guard_mismatch(self):
        with self.assertRaisesRegex(ValueError, "different network identity"):
            app_backend_ipv4(network_with(), APP_ID, "different-network")
        with self.assertRaisesRegex(ValueError, "not internal"):
            app_backend_ipv4(network_with(Internal=False), APP_ID, NETWORK_ID)

    def test_refuses_missing_malformed_and_non_private_addresses(self):
        for address in (None, "", "not-an-address/16", "2001:db8::2/64", "8.8.8.8/24",
                        "127.0.0.1/8", "0.0.0.0/0", "224.0.0.1/24"):
            with self.subTest(address=address), self.assertRaises(ValueError):
                app_backend_ipv4(network_with(address), APP_ID, NETWORK_ID)

    def test_does_not_treat_a_prefix_as_the_container_identity(self):
        with self.assertRaisesRegex(ValueError, "canonical full app container ID"):
            app_backend_ipv4(network_with(), APP_ID[:12], NETWORK_ID)

    def test_missing_endpoint_diagnostic_has_only_bounded_identity_fields(self):
        with self.assertRaisesRegex(
            ValueError,
            rf"container_id={'c' * 64} container_id_length=64 network_id={NETWORK_ID} endpoint_ids={APP_ID},{'b' * 64}",
        ):
            app_backend_ipv4(network_with(), "c" * 64, NETWORK_ID)

    def test_rehearsal_wires_network_inspection_without_weakening_isolation_checks(self):
        script = (REPOSITORY / "scripts" / "test-tor-upgrade-rollback.sh").read_text()
        self.assertIn(".HostConfig.PortBindings", script)
        self.assertIn("assert set(service_networks) == {\"backend\"}", script)
        self.assertIn('backend.get("internal") is True', script)
        self.assertIn("docker inspect --format '{{.Id}}' \"$compose_container\"", script)
        self.assertIn("docker network inspect --verbose --format '{{json .}}'", script)
        self.assertIn("app_state status={{.State.Status}} restarting={{.State.Restarting}} exit_code={{.State.ExitCode}} restart_count={{.RestartCount}}", script)
        self.assertNotIn(".Config.Env", script)
        self.assertIn('scripts/tor_network_address.py', script)
        self.assertNotIn('network["IPAddress"]', script)


if __name__ == "__main__":
    unittest.main()
