# Production checklist

Check every item before inviting users.

## Install

- [ ] Installed from a [release tag](https://github.com/yegamble/opsecmkt/releases), not `main`.
- [ ] `./scripts/install.sh check` passes.
- [ ] `./scripts/install.sh status` shows every service healthy and setup complete.
- [ ] Database and node RPC ports are not reachable from the internet.
- [ ] Public domain only: forms work through the proxy (no 403 "Origin does not match Host").

## Accounts

- [ ] Administrator has an authenticator app enrolled and recovery codes stored offline.
- [ ] You know how to reset an administrator from the host
  ([Authentication](../operator-guide.md#authentication)).

## Backups

- [ ] `.env` backed up, encrypted with age
  ([Secrets, .env and onion identities](../operator-guide.md#secrets-env-and-onion-identities)).
- [ ] Tor only: onion keys backed up.
- [ ] Database backups scheduled with `scripts/backup.sh` and copied off the host.
- [ ] A backup restored into a disposable database at least once
  ([Backups and recovery](../operator-guide.md#backups-and-recovery)).
- [ ] The age private key is stored apart from the backups.

## Payments

- [ ] Escrow is on a **test network** unless you deliberately opted in to [mainnet](../operator-guide.md#mainnet).
- [ ] Bitcoin escrow: `escrow-node-setup.sh --verify` prints `PASS`.
- [ ] Escrow signer fingerprint recorded and its seed backed up
  ([Escrow signer](../operator-guide.md#escrow-signer)).
- [ ] Escrow wallets backed up
  ([runbook](../testnet-runbook.md#back-up-and-restore-the-escrow-arbiter-and-watch-only-wallets)).
- [ ] You have read [what escrow protects and what it does not](../escrow.md).

## Publishing

- [ ] Tor only: onion address (and mirror) published in an operator-signed document.
- [ ] Canary published from `/admin`.
- [ ] Uploads on: scanner signatures current (`./scripts/install.sh status`).

## Maintenance

- [ ] You know the upgrade path: check out the new tag, then `AGE_RECIPIENT=age1... ./scripts/install.sh upgrade`
  ([Upgrading](../../UPGRADING.md)).
- [ ] Uploads on: `./scripts/install.sh refresh-scanner` scheduled at least monthly.
