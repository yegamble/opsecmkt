# Local marketplace

Run the full app on `127.0.0.1` over HTTP. Internal database, no nodes, no payments. For evaluation only.

## Before you begin

- Git, Docker with Compose v2 and OpenSSL ([requirements](requirements.md)).
- Port 8080 free on `127.0.0.1`, or set `APP_PORT`.

## Step 1: Get the release

```sh
git clone https://github.com/yegamble/opsecmkt.git
cd opsecmkt
git checkout v0.1.0-alpha.2
```

Use the latest tag from [Releases](https://github.com/yegamble/opsecmkt/releases).

## Step 2: Run the installer

```sh
./scripts/install.sh --local
```

No questions. Another port: `APP_PORT=8081 ./scripts/install.sh --local`.

Output:

```text
Ready. Open http://127.0.0.1:8080/setup to name your marketplace and create its administrator.
Setup token: 3f1c…
```

> **Tip:** `./scripts/install.sh --local --admin YOUR_HANDLE` creates the administrator in the terminal instead.

## Step 3: Open the setup page

Open the setup link, then follow [First steps](first-steps.md).

## Manage it

| Task | Command |
| --- | --- |
| Stop, keep data | `docker compose down` |
| Start | `docker compose up -d` |
| Health | `./scripts/install.sh status` |
| Logs | `docker compose logs app` |
| Delete everything | `docker compose down --volumes`, then delete `.env` |

> **Warning:** `docker compose down --volumes` deletes the database. It cannot be undone.

## Limits

- No nodes, and none can be added. To try payments, reinstall with [Tor](tor.md) or
  [public domain](clearnet.md). Their local nodes default to mainnet (real funds; no mainnet run is recorded; mainnet
  is covered by unit, database and simulated-RPC tests only), so add
  `--monero-network stagenet` to try test coins.
- Plain HTTP: cookies are not Secure.

## What now?

- [First steps](first-steps.md)
