# Onion service (Tor)

Run the market as a Tor onion service. No inbound port is opened on the host. Recommended for production.

**Time:** about 10 minutes, plus node sync. **Result:** a market at a `.onion` address with an administrator.

## Before you begin

- [Requirements](requirements.md) met, with **Docker Compose 2.17 or later**.
- A node choice. The installer cannot add nodes later:

| Choice | Result |
| --- | --- |
| `--nodes local` | Pruned Bitcoin signet and Monero stagenet nodes. 25–30 GB disk, hours to sync. |
| `--nodes custom` | Choose network, image and pruning per coin. |
| `--nodes none` | No nodes. No payments with a bundled node. |

## Step 1: Get the release

Use the latest tag from [Releases](https://github.com/yegamble/opsecmkt/releases), not `main`.

```sh
git clone https://github.com/yegamble/opsecmkt.git
cd opsecmkt
git checkout v0.1.0-alpha.2
```

## Step 2: Run the installer

```sh
./scripts/install.sh init --tor --nodes local
```

External database: add `--external-db URL` ([details](external-database.md)). All options:
[installer reference](installer-reference.md).

Output:

```text
Onion address: abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcd.onion
Its descriptor can take 1-3 more minutes to reach the Tor network.
Ready. Open http://abcdef….onion/setup in Tor Browser to name your marketplace and create its administrator.
Setup token: 3f1c…
```

With a Bitcoin node, escrow commands follow. Skip them for now. [Enabling payments](enable-payments.md) repeats
them.

## Step 3: Open the setup page

Wait 1–3 minutes. Open the setup link in Tor Browser, then follow [First steps](first-steps.md).

## Step 4: Back up the onion keys

The address exists only as keys in the `tor_identity` volume. Lose them and the address is gone. Back them up:
[Secrets, .env and onion identities](../operator-guide.md#secrets-env-and-onion-identities).

## Step 5: Verify

```sh
./scripts/install.sh status
```

Every service is healthy, setup is complete, and the onion address is listed.

## Optional

| Add | Command |
| --- | --- |
| Second onion address ([mirror](../operator-guide.md#optional-onion-mirror)) | `./scripts/install.sh add-mirror` |
| Photo uploads ([scanner](../operator-guide.md#photo-uploads-and-the-malware-scanner)) | `./scripts/install.sh add-scanner` |
| Nodes over Tor | [Local nodes over Tor](../operator-guide.md#local-nodes-over-tor) |

Onion DoS defences are on by default: [Onion-service defences](../operator-guide.md#onion-service-defences).

## Use your own (vanity) onion address

By default Tor creates a new, random address. To use one you already have, such as a vanity address, give its key
directory at install time:

```sh
./scripts/install.sh init --tor --nodes local --onion-key-dir /root/onion-keys --onion-address ADDR.onion
```

`/root/onion-keys` holds `hs_ed25519_secret_key` and, optionally, `hs_ed25519_public_key`, as `mkp224o` writes them.
Keep it outside the checkout.

- **Generate it yourself, offline.** A key made by anyone else, including a vanity service, is held by them: they can
  run your address. Use [mkp224o](https://github.com/cathugger/mkp224o) on a machine you control.
- **Checked before use.** The installer checks both files byte for byte, has Tor derive the address offline and
  compares it with `--onion-address` (or asks you to confirm it). A mismatch writes nothing.
- **First install only.** The key goes into a new, empty `tor_identity` volume before Tor first starts. The
  installer never replaces the address of a running install.
- **Afterwards:** back up the volume as in Step 4, then securely delete `/root/onion-keys`.

For the mirror: `./scripts/install.sh add-mirror --mirror-onion-key-dir DIR --mirror-onion-address ADDR.onion`,
before the mirror first runs.

## Good to know

- Only the Tor container has internet access. With an external database, the app does too.
- Local nodes sync over a direct connection unless routed through Tor.
- Tor protects inbound connections. It does not guarantee anonymity.
- Onion stopped answering? Recreate Tor. The address stays:

```sh
docker compose up -d --force-recreate tor
```

## What now?

- [First steps](first-steps.md)
- [Enabling payments](enable-payments.md)
- [Production checklist](production-checklist.md)
