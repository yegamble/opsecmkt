# Installing OPSEC Market

Pick a method, follow its page, then do the [first steps](first-steps.md).

> **Prerelease:** `v0.1.0-alpha.2`. A new install takes **no payments** until you set up escrow. Its local nodes
> default to **mainnet (real funds)**; test networks are an option. Mainnet payment and settlement workflows remain
> unverified; deployment and readiness checks do not establish real-funds settlement. See [Implementation status](../implementation-status.md).

## Choose a method

| Method | Use it to | Address | Time |
| --- | --- | --- | --- |
| [Production with managed PostgreSQL](production.md) | Deploy separate application and upload hosts, private TLS, backups and recovery | Public HTTPS or `.onion` | Allow time for infrastructure and recovery verification |
| [Read-only preview](preview.md) | Look around with sample data | `127.0.0.1:8080` | 1 min |
| [Local marketplace](local.md) | Try the full app on your machine | `127.0.0.1:8080` | 5 min |
| [Onion service](tor.md) | Run a market over Tor (recommended) | `.onion` | 10 min + node sync |
| [Public domain](clearnet.md) | Run a market behind your HTTPS proxy | `https://YOUR-DOMAIN` | 15 min + node sync |

New to OPSEC Market? Start with **local**. For a real instance, follow the **production** procedure and choose Tor or HTTPS ingress there.

The native production procedure's base example disables payments. For an instance
that needs escrow, prepare the separate Monero services and payment flags before
the single initialization. Native initialization defaults to stagenet. The legacy
shell methods below have different local-node defaults.

## Mainnet or test networks

`init --tor` and `init --clearnet` with local nodes (the default) write mainnet. Mainnet payment and settlement workflows remain unverified;
deployment and readiness checks do not establish real-funds settlement.

| You want | Pass | Writes |
| --- | --- | --- |
| Mainnet (default) | nothing | `MONERO_NETWORK=mainnet`, blank `MONERO_NETWORK_FLAG=` |
| Test networks | `--monero-network stagenet` | `MONERO_NETWORK=stagenet`, `MONERO_NETWORK_FLAG=--stagenet` |
| No nodes | `--nodes none`, or `--local` | Nothing for payments |

Details and the other options: [Enabling payments](enable-payments.md#choose-a-network). Upgrading never changes an
existing install's network ([UPGRADING](../../UPGRADING.md#64-new-installs-default-to-mainnet-a-523)).

> **Note:** nodes cannot be added after install. To take payments with a bundled node, choose them when you
> install with Tor or public domain.

## The path

1. [Requirements](requirements.md)
2. [Harden the servers](server-hardening.md): approve source addresses, prepare recovery access and verify encrypted storage before restoring sensitive state.
3. Choose the payment configuration before initialization, then install with your method's page. For native production escrow, prepare the remote services and include the payment flags in that single initialization.
4. [First steps](first-steps.md): administrator, health check, backups
5. [Verify payment readiness and deliberately enable intake](enable-payments.md) (optional). Configuring remote escrow and opening payment intake are separate steps.
6. [Production checklist](production-checklist.md) before inviting users

## Reference

- [Installer reference](installer-reference.md)
- [Production deployment](production.md)
- [Server hardening](server-hardening.md)
- [Existing deployment security review](security-review.md)
- [Existing deployment storage assessment](storage-security-assessment.md)
- [External PostgreSQL](external-database.md)
- [Troubleshooting](troubleshooting.md)
- [Upgrading](../../UPGRADING.md)
