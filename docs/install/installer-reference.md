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
  `local`: pruned Bitcoin signet and Monero stagenet nodes. `none`: no nodes. `custom`: ask per coin.
- `--bitcoin local|disabled`<br>
  Run a Bitcoin node.
- `--bitcoin-network signet|testnet4|regtest`<br>
  Default `signet`. Fixed after first use.
- `--bitcoin-image IMAGE`<br>
  A release tag or `@sha256` digest, not `latest`.
- `--bitcoin-prune yes|no`<br>
  Default `yes`.
- `--monero local|disabled`<br>
  Run a Monero node.
- `--monero-network stagenet`<br>
  The only Monero network.
- `--monero-image IMAGE`<br>
  A release tag or `@sha256` digest, not `latest`.
- `--monero-prune yes|no`<br>
  Default `yes`.
- `--monero-escrow yes|no`<br>
  Custodial Monero escrow on the local stagenet node. Default `no`.
- `--monero-escrow-seed-key FILE`<br>
  With Monero escrow: the armored OpenPGP **public** key that wallet seeds are sealed to.

A node does not turn on escrow. See [Enabling payments](enable-payments.md).

### Other flags

- `--admin HANDLE`<br>
  Create the administrator in the terminal. Prints no setup token.
- `--site-name NAME`<br>
  With `--admin`: the marketplace name.
- `--uploads yes|no`<br>
  Add the malware scanner for photo uploads. Needs 3 GiB of memory. Default `no`.

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
| BITCOIN escrow network | `signet` | `signet` |
| Prune the BITCOIN node? | `yes` | `yes` |
| MONERO node (local/disabled) | `local` | `local` to take Monero |
| MONERO Docker image | pinned default | default |
| MONERO escrow network | `stagenet` | `stagenet` |
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

**Command:** `./scripts/install.sh add-mirror`

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
