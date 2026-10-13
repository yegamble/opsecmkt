# External PostgreSQL

Use your own PostgreSQL server instead of the internal container.

## Requirements

- **Empty** database (or the one belonging to the market you are restoring).
- **UTF8** encoding. Other encodings are refused at startup.
- Reachable from the app container. CI tests PostgreSQL 17.

Create it:

```sql
CREATE DATABASE opsecmkt ENCODING 'UTF8' TEMPLATE template0;
```

## Install

```sh
./scripts/install.sh init --tor --external-db 'postgres://USER:PASSWORD@HOST:5432/opsecmkt?sslmode=verify-full'
```

Works with `--tor` or `--clearnet`. Not with `--local`.

> **Note:** a flag value is visible to other host users while the installer runs. To hide the URL, run
> `./scripts/install.sh` with no options and paste it at the "External PostgreSQL URL" prompt.

## Behaviour

- Database already holds a set-up market: the installer stops and starts nothing. Restore that market's `.env`
  instead.
- Tor installs: the app gets direct outbound access. Inbound stays onion-only.
- Every connection uses `client_encoding=UTF8` and `TimeZone=UTC`.
- Backups and upgrades need `BACKUP_DATABASE_URL` and matching client tools
  ([Backups and recovery](../operator-guide.md#backups-and-recovery)).
- Using a pooler? Read [Connection poolers](../operator-guide.md#connection-poolers).

## Startup errors

| Message | Fix |
| --- | --- |
| `authentication failed (SQLSTATE 28P01)` / `(SQLSTATE 28000)` | Check user, password, `pg_hba.conf`. |
| `database missing (SQLSTATE 3D000)` | Create the database. |
| `host not found`, `connection refused`, `timed out` | Check host, port, firewall. |
| `TLS required` | Set `sslmode=require` or `verify-full`. |
| `the database encoding is SQL_ASCII, not UTF8` | Create a UTF8 database. |

## What now?

- Back to [Onion service](tor.md) or [Public domain](clearnet.md).
