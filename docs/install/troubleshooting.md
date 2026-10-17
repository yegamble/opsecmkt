# Troubleshooting

Start here:

```sh
./scripts/install.sh status
docker compose logs app
```

## Installer errors

| Message | Fix |
| --- | --- |
| `Install Docker with Compose v2 first.` / `Docker Compose v2 is required.` | Install Docker Engine with the Compose plugin. |
| `OpenSSL is required to generate secrets.` | Install OpenSSL. |
| Refuses because `.env` or volumes exist | An install is already here. See [First install failed](#first-install-failed). Never delete `.env` to bypass this. |
| `The malware scanner needs a Docker host with at least 3 GiB of memory` | Add memory, or drop `--uploads yes`. |
| `Refusing to install: the external database already belongs to a market` | Use an empty database, or restore that market's `.env`. |
| `The services did not become healthy.` | See [First install failed](#first-install-failed). |
| Retired Bitcoin installer option | This source accepts Monero only. Remove the Bitcoin option; choose `--monero-network mainnet` for real funds or `--monero-network stagenet` for a test market. |
| Unsupported network | Use `mainnet` for Monero real funds, or `stagenet` for a test market. |
| `WARNING: the Docker data directory … has … GB free` | Not a refusal; the install continues. Free space before the nodes sync, or they stop when the disk is full ([estimates](requirements.md#disk-for-nodes)). |

## First install failed

`.env` and data are kept. Find the cause, fix it, then retry with the same `.env`:

```sh
docker compose ps
docker compose logs
./scripts/install.sh check
docker compose up -d --build --wait
```

> **Warning:** don't delete `.env` or volumes. They may already hold this install's secrets and data.

## 403 "Origin does not match Host"

Your proxy rewrites `Host`. nginx: add `proxy_set_header Host $host;`.
See [Configure the reverse proxy](clearnet.md#step-3-configure-the-reverse-proxy).

## Can't sign in (public domain)

Cookies are Secure. Use `https://YOUR-DOMAIN`, not `http://127.0.0.1:8080`.

## Onion doesn't answer

First distinguish an application failure from Tor publication or client
authorization. A saved hostname or healthy container does not prove onion
reachability. For native production, follow
[the onion timeout procedure](tor.md#diagnosing-a-timeout-after-entering-the-client-key).
Record the exact error before changing a key or restarting a service.

The checkout's `.env`/Compose lifecycle applies only to the legacy installer.
After an intentional configuration change, use its reviewed
[Tor restart procedure](../operator-guide.md#clearnet-and-onion-deployment),
then verify both routes again. Do not use forced-recreation loops as a general
remedy for descriptor errors. Preserve the existing identities and authorization.

## Lost setup token

```sh
./scripts/install.sh setup-token        # show it
./scripts/install.sh setup-token --new  # issue a new one
```

Only works while setup is pending.

## Database errors at startup

| Message | Fix |
| --- | --- |
| `authentication failed (SQLSTATE 28P01)` / `(SQLSTATE 28000)` | Check user, password, `pg_hba.conf`. |
| `database missing (SQLSTATE 3D000)` | Create the database. |
| `host not found`, `connection refused`, `timed out` | Check host, port, firewall. |
| `TLS required` | Set `sslmode=require` or `verify-full`. |
| `the database encoding is SQL_ASCII, not UTF8` | Use a [UTF8 database](external-database.md). |

## User reports "Reference: 0bbdc379"

```sh
docker compose logs app | grep ref=0bbdc379
```

Fields explained: [operator guide troubleshooting](../operator-guide.md#troubleshooting).

## Still stuck?

- [Operator guide](../operator-guide.md)
- Security issue: [security policy](../../SECURITY.md)
- [GitHub issues](https://github.com/yegamble/opsecmkt/issues). Include `./scripts/install.sh check` output. It
  names keys, never secrets.
