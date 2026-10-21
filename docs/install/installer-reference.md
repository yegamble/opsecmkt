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

- `--nodes local|none|custom`: `local` starts a pruned Monero daemon; `none` starts no node; `custom` asks for Monero settings.
- `--monero local|disabled`: run or disable the bundled daemon.
- `--monero-network mainnet|stagenet`: mainnet by default, with real funds. Use stagenet for a test market.
- `--monero-image IMAGE`: pinned release tag or immutable digest, never `latest`.
- `--monero-prune yes|no`: prune by default.
- `--monero-escrow yes|no`: enable custodial escrow with the local daemon; default `no`.
- `--monero-escrow-seed-key FILE`: armored OpenPGP public key used to encrypt wallet seeds. Keep its private half offline.

Bitcoin options are rejected. A node alone does not enable escrow. No mainnet run is recorded; see [Enabling Monero payments](enable-payments.md).

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
./scripts/install.sh init --tor --nodes local --monero-network stagenet
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
| MONERO node (local/disabled) | `local` | `local` to take Monero |
| MONERO Docker image | pinned default | default |
| MONERO escrow network | `mainnet` | `mainnet` for real funds; `stagenet` to test |
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

## restore-onion

Restore an onion identity backup, decrypted on stdin, into its empty volume before that Tor first starts. Tor installs
only; the private administrator onion (`tor_admin_identity`) is refused.

**Command:** `age -d -i "$AGE_IDENTITY" BACKUP | ./scripts/install.sh restore-onion tor_identity|tor_mirror_identity --onion-address ADDR.onion`

It refuses a compressed archive, any entry other than the key files and `hostname` (links, other names or paths,
duplicates), oversized files, keys that fail the import's checks or Tor's offline dry run, an address other than the
one you give or confirm, a `hostname` that is not the address the key derives, a secret key already in another onion's
volume (compared byte for byte), a running Tor service and a volume that is not empty or cannot be read. Nothing is
written until every check passes; `hostname` is never written. Containers that carry key bytes run with
`--log-driver none`. The last line is the derived address.

### Flags

- `--onion-address ADDR.onion`<br>
  The onion address you published for this identity; the key must derive it. Without it, the derived address is shown
  on the terminal (`/dev/tty`, as stdin holds the backup) for you to confirm with `yes`; with no terminal the restore
  refuses.

See [Secrets, .env and onion identities](../operator-guide.md#secrets-env-and-onion-identities).

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
