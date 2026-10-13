# Requirements

What the host needs. The installer checks Docker, Compose and OpenSSL, but not disk space.

## Software

| Software | Needed for | Version |
| --- | --- | --- |
| Git | All methods | Recent |
| Docker Engine + Compose v2 | Local, Tor, public domain | Compose v2; **2.17+ for Tor** |
| OpenSSL | The installer | Recent |
| Go | [Preview](preview.md) and development | 1.26.9+ |
| HTTPS reverse proxy (Caddy, nginx) | [Public domain](clearnet.md) | Must forward `Host` |
| Tor Browser | Opening an onion install | Current |
| [age](https://age-encryption.org/), Python 3 | Backups and upgrades | Recent |

PostgreSQL 17 runs in a container. No host PostgreSQL needed unless you use an
[external database](external-database.md).

## Memory

| Setup | Docker host memory |
| --- | --- |
| Default | Not yet measured |
| Photo uploads on (ClamAV scanner) | **3 GiB minimum**, 4 GiB advised. The installer refuses below 3 GiB. |

ClamAV checks uploads for known malware; it does not review picture content.

## Disk for nodes

Optional. Pruned by default, but the first sync downloads the whole test chain and can take hours.

| Node | Pruned (default) | Full |
| --- | --- | --- |
| Bitcoin signet (default) | ~8 GB | ~28 GB |
| Bitcoin testnet4 | ~5 GB | ~33 GB |
| Monero stagenet | ~20 GB (estimate) | Not published |
| Bitcoin regtest | Minimal, no sync | — |

Both default nodes together: 25–30 GB. Details: [Local node pruning](../operator-guide.md#local-node-pruning).

## Network

| Method | Needs |
| --- | --- |
| Local | Nothing. Listens on `127.0.0.1:8080`. |
| Onion service | Outbound access. **No inbound port.** |
| Public domain | Domain, DNS, ports 80/443 for the proxy. App listens on `127.0.0.1:8080`. |

All methods need outbound access to pull images and sync nodes.

> **Warning:** never expose the database or node RPC ports. The Compose files publish neither.

## What now?

- [Choose a method](README.md#choose-a-method)
