#!/usr/bin/env python3
"""Exercise rendered ingress with real nftables in disposable network namespaces.

Run as root on Linux with iproute2/nftables/Python installed. Never runs host apply
or changes the caller's network namespace. Tests packet policy, not systemd boot,
SSH authentication, disk encryption, Docker itself, or Tor availability.
"""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time


def run(*args, **kwargs):
    return subprocess.run(args, check=True, **kwargs)


def main():
    if len(sys.argv) not in (2, 3):
        raise SystemExit("usage: test-host-bootstrap.py /absolute/path/to/opsecmkt")
    binary = str(Path(sys.argv[1]).resolve())
    if os.geteuid() != 0:
        raise SystemExit("root required for disposable network namespaces")
    if len(sys.argv) == 2:
        # Always isolate before any address, route or firewall mutation.
        run("unshare", "--net", sys.executable, str(Path(__file__).resolve()), binary, "--isolated")
        return
    if sys.argv[2] != "--isolated":
        raise SystemExit("unsupported argument")
    peers = "opsecmkt-peer-" + str(os.getpid())
    backend = "opsecmkt-backend-" + str(os.getpid())
    children = []
    created = []
    try:
        for ns in (peers, backend):
            run("ip", "netns", "add", ns)
            created.append(ns)
            run("ip", "-n", ns, "link", "set", "lo", "up")
        run("ip", "link", "set", "lo", "up")
        for local, remote, ns, hostaddr, peeraddr in (
            ("pub0", "pub1", peers, "192.0.2.1/24", "192.0.2.10/24"),
            ("priv0", "priv1", peers, "10.20.0.10/24", "10.20.0.20/24"),
            ("bridge0", "back0", backend, "172.20.0.1/24", "172.20.0.2/24"),
        ):
            run("ip", "link", "add", local, "type", "veth", "peer", "name", remote)
            run("ip", "link", "set", remote, "netns", ns)
            run("ip", "addr", "add", hostaddr, "dev", local)
            run("ip", "link", "set", local, "up")
            run("ip", "-n", ns, "addr", "add", peeraddr, "dev", remote)
            run("ip", "-n", ns, "link", "set", remote, "up")
        run("ip", "-n", peers, "addr", "add", "192.0.2.11/24", "dev", "pub1")
        run("ip", "-n", peers, "addr", "add", "10.20.0.21/24", "dev", "priv1")
        run("ip", "addr", "add", "2001:db8::1/64", "dev", "pub0", "nodad")
        for address in ("2001:db8::10/64", "2001:db8::11/64"):
            run("ip", "-n", peers, "addr", "add", address, "dev", "pub1", "nodad")
        run("ip", "-n", backend, "route", "add", "default", "via", "172.20.0.1")
        run("ip", "-n", peers, "route", "add", "172.20.0.0/24", "via", "10.20.0.10")
        Path("/proc/sys/net/ipv4/ip_forward").write_text("1\n")
        with tempfile.TemporaryDirectory(prefix="opsecmkt-nft-") as tmp:
            config = {"version": 1, "os": "ubuntu-24.04", "role": "app", "operator": "operator",
                      "ssh_port": 22, "operator_sources": ["192.0.2.10/32", "2001:db8::10/128"],
                      "uplinks": ["pub0", "priv0"], "app_address": "10.20.0.10",
                      "gateway_address": "10.20.0.20", "app_ports": [8080, 8443]}
            recipe = Path(tmp) / "host.json"
            recipe.write_text(json.dumps(config))
            run(binary, "host", "plan", "--config", str(recipe), "--out", tmp + "/plan")
            rules = Path(tmp) / "plan/ingress.nft"
            # Another manager's table must survive loading/replacing our table.
            run("nft", "add", "table", "inet", "unrelated")
            run("nft", "-c", "-f", str(rules))
            run("nft", "-f", str(rules))
            nat = 'table ip fixture_nat {\n chain prerouting {\n  type nat hook prerouting priority dstnat;\n  ip daddr 10.20.0.10 tcp dport 8080 dnat to 172.20.0.2:8080\n }\n}\n'
            run("nft", "-f", "-", input=nat, text=True)
            server = """import socket,threading,time

def serve(addr,port,family):
 s=socket.socket(family);s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1)
 if family==socket.AF_INET6:s.setsockopt(socket.IPPROTO_IPV6,socket.IPV6_V6ONLY,1)
 s.bind((addr,port));s.listen()
 while True:
  c,_=s.accept();c.sendall(b'OK');c.close()
for addr,port,family in [('0.0.0.0',22,socket.AF_INET),('::',22,socket.AF_INET6),('0.0.0.0',8443,socket.AF_INET),('0.0.0.0',4443,socket.AF_INET),('0.0.0.0',8080,socket.AF_INET)]:
 threading.Thread(target=serve,args=(addr,port,family),daemon=True).start()
time.sleep(300)
"""
            children.append(subprocess.Popen([sys.executable, "-c", server]))
            children.append(subprocess.Popen(["ip", "netns", "exec", backend, sys.executable, "-c", server]))
            time.sleep(0.3)
            probe = """import socket,sys
s=socket.socket(socket.AF_INET6 if ':' in sys.argv[1] else socket.AF_INET)
s.settimeout(0.6);s.bind((sys.argv[1],0))
try:
 s.connect((sys.argv[2],int(sys.argv[3])));ok=s.recv(2)==b'OK'
except OSError:ok=False
finally:s.close()
sys.exit(0 if ok == (sys.argv[4]=='allow') else 1)
"""
            cases = [
                ("192.0.2.10", "192.0.2.1", 22, "allow"),
                ("192.0.2.11", "192.0.2.1", 22, "deny"),
                ("2001:db8::10", "2001:db8::1", 22, "allow"),
                ("2001:db8::11", "2001:db8::1", 22, "deny"),
                ("10.20.0.20", "10.20.0.10", 8443, "allow"),
                ("10.20.0.21", "10.20.0.10", 8443, "deny"),
                ("10.20.0.20", "10.20.0.10", 8080, "allow"),
                ("10.20.0.21", "10.20.0.10", 8080, "deny"),
                ("10.20.0.20", "172.20.0.2", 8080, "deny"),
                ("10.20.0.20", "10.20.0.10", 4443, "deny"),
                ("192.0.2.10", "192.0.2.1", 8443, "deny"),
            ]
            for source, destination, port, result in cases:
                run("ip", "netns", "exec", peers, sys.executable, "-c", probe,
                    source, destination, str(port), result)
            run("nft", "-f", "-", input="delete table inet opsecmkt_host\n" + rules.read_text(), text=True)
            run("nft", "list", "table", "inet", "unrelated", stdout=subprocess.DEVNULL)
            print("PASS: 11 IPv4/IPv6 INPUT, DNAT and direct-route packet cases; unrelated table retained")
    finally:
        for process in children:
            process.terminate()
            process.wait(timeout=5)
        for ns in reversed(created):
            subprocess.run(["ip", "netns", "delete", ns], check=False)


if __name__ == "__main__":
    main()
