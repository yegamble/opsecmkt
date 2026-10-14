# Installer reference

Every command and option of `scripts/install.sh`. Run it from the repository root.

## init

Install a new market: write `.env`, start the containers, print the setup link and token. `init` is the default
command.

**Command:** `./scripts/install.sh init [options]`

Refuses when `.env` or the project's volumes already exist. Any question your options leave open is asked.

### Exposure flags

- `--local`<br>
  Loopback HTTP, internal database, no nodes. Asks nothing.
- `--tor`<br>
  Onion service. See [Onion service](tor.md).
- `--clearnet`<br>
  Public domain behind your HTTPS proxy. See [Public domain](clearnet.md).
- `--behind-proxy yes|no`<br>
  Clearnet only. `yes` (default) marks cookies Secure. `no` serves local HTTP.

### Database flags

- `--external-db URL`<br>
  Use your own PostgreSQL. Default: an internal PostgreSQL 17 container. See
  [External PostgreSQL](external-database.md).

### Node flags

- `--nodes local|none|custom`<br>
  `local`: pruned **mainnet** Bitcoin and Monero nodes (real funds) unless a network flag names a test network.
  `none`: no nodes. `custom`: ask per coin.
- `--bitcoin local|disabled`<br>
  Run a Bitcoin node.
- `--bitcoin-network main|signet|testnet4|regtest`<br>
  Default `main` (mainnet, real funds; no mainnet run is recorded). A test network here makes Monero default to `stagenet`. Fixed after first use.
- `--bitcoin-image IMAGE`<br>
  A release tag or `@sha256` digest, not `latest`.
- `--bitcoin-prune yes|no`<br>
  Default `yes`.
- `--monero local|disabled`<br>
  Run a Monero node.
- `--monero-network mainnet|stagenet`<br>
  Default `mainnet` (real funds; no mainnet run is recorded). `stagenet` makes Bitcoin default to `signet`. One coin on mainnet and the other on
  a test network is refused: a market is all mainnet or all test networks.
- `--monero-image IMAGE`<br>
  A release tag or `@sha256` digest, not `latest`.
- `--monero-prune yes|no`<br>
  Default `yes`.
- `--monero-escrow yes|no`<br>
  Custodial Monero escrow on the local Monero node. On mainnet the market holds real funds. Default `no`.
- `--monero-escrow-seed-key FILE`<br>
  With Monero escrow: the armored OpenPGP **public** key that wallet seeds are sealed to.

On mainnet the installer also writes a 15000 MiB Bitcoin prune target (about two months, an estimate) and 6 Bitcoin
confirmations, and prints the real-funds warnings first. `upgrade` never changes an installed market's network.
No mainnet run of this software is recorded.

A node does not turn on escrow. See [Enabling payments](enable-payments.md).

### Other flags

- `--admin HANDLE`<br>
  Create the administrator in the terminal. Prints no setup token.
- `--site-name NAME`<br>
  With `--admin`: the marketplace name.
- `--uploads yes|no`<br>
  Add the malware scanner for photo uploads. Needs 3 GiB of memory. Default `no`.

### Onion identity flags

- `--onion-key-dir DIR`<br>
  Tor only (implies `--tor`; refused with `--clearnet` or `--local`). Use an existing onion identity, such as a
  vanity address, instead of a new one. `DIR` is outside the checkout and holds `hs_ed25519_secret_key` (required)
  and `hs_ed25519_public_key` (optional), as regular files. A `hostname` file is ignored. See
  [Use your own onion address](tor.md#use-your-own-vanity-onion-address).
- `--onion-address ADDR.onion`<br>
  With `--onion-key-dir`: the address the key must have. Without it, the installer shows the address for you to
  confirm by typing `yes`, and refuses when there is no terminal.

### Environment

- `APP_PORT`<br>
  Host loopback port. Default `8080`.
- `COMPOSE_PROJECT_NAME`<br>
  Compose project name. Default `opsecmkt`. Change it to run a second copy on one host.

### Examples

```sh
./scripts/install.sh --local
./scripts/install.sh init --tor --nodes local
./scripts/install.sh init --clearnet --behind-proxy yes --nodes none
./scripts/install.sh init --tor --nodes custom --bitcoin local --bitcoin-network testnet4 --monero disabled
APP_PORT=8081 ./scripts/install.sh --local --admin alice
./scripts/install.sh init --tor --nodes local --onion-key-dir /root/onion-keys --onion-address ADDR.onion
```

> **Note:** values passed as flags, such as a database URL with a password, are visible to other users of the
> host while the installer runs. Answer the question instead to keep them private.

## Interactive questions

`./scripts/install.sh` with no options asks:

| Question | Default | Recommended |
| --- | --- | --- |
| Exposure (clearnet/tor) | `clearnet` | `tor` |
| External PostgreSQL URL | blank (internal) | blank |
| Local HTTP development only? (clearnet) | `no` | `no` |
| BITCOIN node (local/disabled) | `local` | `local` to take Bitcoin |
| BITCOIN Docker image | pinned default | default |
| BITCOIN escrow network | `main` | `main` for a real market; `signet` to test |
| Prune the BITCOIN node? | `yes` | `yes` |
| MONERO node (local/disabled) | `local` | `local` to take Monero |
| MONERO Docker image | pinned default | default |
| MONERO escrow network | `mainnet` | follows Bitcoin's class |
| Prune the MONERO node? | `yes` | `yes` |
| Turn on Monero escrow? | `no` | `no` |

## status

Show mode, service health, setup state, schema version, onion addresses and scanner state.

**Command:** `./scripts/install.sh status`

## check

Validate `.env` and the Compose configuration. Starts nothing. Messages name keys, never values.

**Command:** `./scripts/install.sh check`

## setup-token

Show the setup token while setup is pending.

**Command:** `./scripts/install.sh setup-token [--new]`

### Flags

- `--new`<br>
  Issue a new token and restart the app.

## add-mirror

Add a second onion address for the same market. Tor installs only.

**Command:** `./scripts/install.sh add-mirror [--mirror-onion-key-dir DIR [--mirror-onion-address ADDR.onion]]`

### Flags

- `--mirror-onion-key-dir DIR`<br>
  Use an existing onion identity for the mirror, as `--onion-key-dir` does for `init`. Only before the mirror
  first starts; refused when the key is the main onion's.
- `--mirror-onion-address ADDR.onion`<br>
  With `--mirror-onion-key-dir`: the address the key must have.

See [Optional onion mirror](../operator-guide.md#optional-onion-mirror).

## add-scanner

Add the ClamAV malware scanner so photo and avatar uploads work. Needs 3 GiB of Docker host memory.

**Command:** `./scripts/install.sh add-scanner`

## refresh-scanner

Pull a current scanner image to refresh its signatures. Run it at least monthly.

**Command:** `./scripts/install.sh refresh-scanner`

## upgrade

Upgrade after checking out a new release tag. Takes an encrypted backup first.

**Command:** `AGE_RECIPIENT=age1... ./scripts/install.sh upgrade [--no-backup]`

### Flags

- `--no-backup`<br>
  Skip the backup.

See [Upgrading](../../UPGRADING.md).

## Files

- `.env`<br>
  Every secret and setting, mode `0600`. Back it up encrypted. Never commit it.
- `.env.lock`<br>
  Held while a command changes `.env`. Remove a stale one with `rmdir .env.lock`.
