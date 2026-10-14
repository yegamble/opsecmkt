# Production checklist

Check every item before inviting users.

Review [Security and OPSEC operations](../security-operations.md) first. Check
the threat boundaries against your deployment; encryption of backups does not
encrypt every database record or protect secrets on a running compromised host.

## Install

- [ ] Installed from a [release tag](https://github.com/yegamble/opsecmkt/releases), not `main`.
- [ ] `./scripts/install.sh check` passes.
- [ ] `./scripts/install.sh status` shows every service healthy and setup complete.
- [ ] Database and node RPC ports are not reachable from the internet.
- [ ] Public domain only: forms work through the proxy (no 403 "Origin does not match Host").
- [ ] Tor with private admin ingress: completed the
  [private administrator onion](tor.md#private-administrator-onion) procedure;
  a Tor client without its authorization key cannot connect, and `/setup`,
  `/admin` and audit exports return 404 through every public onion or mirror.
- [ ] Private admin ingress: forged Host or role headers cannot grant access;
  administrator sessions are refused on public shared staff routes, and
  moderator workflows still work. An empty or invalid client allowlist refuses
  Tor admin startup.

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
- [ ] Sensitive audit exports: `AUDIT_EXPORT_RECIPIENT` is configured with only
  the public key; an export and its signature were decrypted and verified
  offline ([instructions](../operator-guide.md#encrypted-audit-exports)).

## Payments

- [ ] You know which network this market runs: `grep -E '^(BITCOIN_CHAIN|MONERO_NETWORK)=' .env`. The installer's
  default is **mainnet (real funds)**; test networks need `--bitcoin-network` / `--monero-network`. No mainnet run is
  recorded; mainnet is covered by unit, database and simulated-RPC tests only ([Mainnet](../operator-guide.md#mainnet)).
- [ ] Mainnet only: you rehearsed set-up, backups and a restore on signet and stagenet first, on a database no test
  market used.
- [ ] Mainnet only: `ESCROW_BTC_FEE_ADDRESS` is the fee wallet's watch-only descriptor (a plain address is refused), and
  `ESCROW_BTC_FEE_RATE` and `PAYMENT_CONFIRMATIONS_BTC` are reviewed.
- [ ] Mainnet only: you accept that Monero escrow, if on, is custodial with real funds.
- [ ] Mainnet only: the node disk had room (the installer's disk check only warns).
- [ ] Bitcoin escrow: `escrow-node-setup.sh --verify` prints `PASS`.
- [ ] Escrow signer fingerprint recorded and its seed backed up
  ([Escrow signer](../operator-guide.md#escrow-signer)).
- [ ] Escrow wallets backed up
  ([runbook](../testnet-runbook.md#back-up-and-restore-the-escrow-arbiter-and-watch-only-wallets)).
- [ ] You have read [what escrow protects and what it does not](../escrow.md).

## Publishing

- [ ] Deployment source manifest reviewed; evidence, cloud documentation
  publishing tooling and CI/AI artifacts excluded from the optional
  [deployment package](../security-operations.md#package-and-publish-deliberately).
- [ ] Publication copies contain no production credentials, provider account
  identifiers or unnecessary operator details. Protected originals and
  authentic verification dates are retained.
- [ ] Tor only: onion address (and mirror) published in an operator-signed document.
- [ ] Canary published from `/admin`.
- [ ] Uploads on: scanner signatures current (`./scripts/install.sh status`).

## Maintenance

- [ ] You know the upgrade path: check out the new tag, then `AGE_RECIPIENT=age1... ./scripts/install.sh upgrade`
  ([Upgrading](../../UPGRADING.md)).
- [ ] Uploads on: `./scripts/install.sh refresh-scanner` scheduled at least monthly.
- [ ] Incident response names the owner, traffic and payment shutdown steps,
  evidence preservation and recovery checks. Destructive actions require
  approval of concrete targets and consequences.
