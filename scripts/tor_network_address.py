#!/usr/bin/env python3
"""Resolve a rehearsal container's address from Docker's network inspect result."""

from __future__ import annotations

import ipaddress
import json
import sys
from typing import Any


def app_backend_ipv4(network: dict[str, Any], container_id: str, expected_network_id: str) -> str:
    """Return the exact container endpoint's private IPv4 address or refuse the rehearsal."""
    if not expected_network_id or network.get("Id") != expected_network_id:
        raise ValueError("Docker network inspect returned a different network identity")
    if network.get("Internal") is not True:
        raise ValueError("The app runtime backend network is not internal")

    containers = network.get("Containers")
    if not isinstance(containers, dict):
        raise ValueError("Docker network inspect did not return its attached containers")
    endpoint = containers.get(container_id)
    if not isinstance(endpoint, dict):
        raise ValueError("The app container is not attached to its backend network")

    endpoint_address = endpoint.get("IPv4Address")
    if not isinstance(endpoint_address, str) or not endpoint_address:
        raise ValueError("Docker network inspect did not return the app IPv4 address")
    try:
        address = ipaddress.ip_interface(endpoint_address).ip
    except ValueError as error:
        raise ValueError("Docker returned an invalid app backend IPv4 address") from error
    if not isinstance(address, ipaddress.IPv4Address) or not address.is_private or address.is_loopback \
            or address.is_unspecified or address.is_multicast:
        raise ValueError("The app backend address is not a private IPv4 endpoint")
    return str(address)


def main() -> int:
    if len(sys.argv) != 3:
        print("Usage: tor_network_address.py CONTAINER_ID NETWORK_ID", file=sys.stderr)
        return 2
    try:
        network = json.load(sys.stdin)
        if not isinstance(network, dict):
            raise ValueError("Docker network inspect did not return an object")
        address = app_backend_ipv4(network, sys.argv[1], sys.argv[2])
    except (json.JSONDecodeError, OSError, ValueError) as error:
        print(f"Could not resolve the app backend address: {error}", file=sys.stderr)
        return 1
    print(address)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
