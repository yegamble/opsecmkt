# Requirements

What the host needs. The installer checks Docker, Compose and OpenSSL. With local nodes it also compares the free
space of Docker's data directory with the estimate below and **warns only**: it never refuses for disk.

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

Optional. Local nodes are pruned by default and **run mainnet unless you name a test network**
([Enabling payments](enable-payments.md#choose-a-network)). No mainnet run is recorded; mainnet is covered by unit,
database and simulated-RPC tests only. All figures are estimates.

| Node | Pruned (default) | Full | First sync |
| --- | --- | --- | --- |
| Monero mainnet (default) | 45 GB or more (45 GB in April 2022 per Monero's README, more today) | 130 GB or more (April 2022) | Days |
| Monero stagenet | ~20 GB | Not published | Hours |

Monero chain sizes grow over time. Reserve more than the historical mainnet figure; stagenet needs about 20 GB as an unverified estimate.
Details: [Local node pruning](../operator-guide.md#local-node-pruning).

> **Warning:** a full disk stops the nodes. The installer prints `WARNING` when Docker's data directory has less free
> space than the estimate, then continues. Free space before they sync.

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
