# Installing OPSEC Market

Start with the standalone CLI for guided setup or explicit flags. Choose a topology, prepare the required services,
then follow the printed administrator setup instructions. The shell installer remains available for older checkouts.

> **Prerelease software.** The native CLI changes are unreleased; build the current checkout to use them.
> New installs take **no payments** until escrow is configured and enabled. Native setup defaults to stagenet;
> the legacy shell installer's local nodes default to **mainnet (real funds)**. No mainnet run is recorded;
> mainnet is covered by unit, database and simulated-RPC tests only. See [Implementation status](../implementation-status.md).

## Choose a method

| Method | Use it to | Start |
| --- | --- | --- |
| [Guided native setup](cli-reference.md#guided-interactive-setup) | Create a development or prepared production deployment, import/generate keys, or manage an installation | Run `./opsecmkt` in a terminal |
| [Native flags](binary.md) | Automate deployment or run directly against existing services | Run `./opsecmkt --help` |
| [Read-only preview](preview.md) | Look around with sample data and no database | Run `./opsecmkt serve --preview` |
| [Production Tor](tor.md#native-production-with-separate-onions) | Separate public upload ingress from client-authorized administrator ingress | Prepare [separate hosts](service-topology.md) first |
| Legacy shell: [local](local.md), [Tor](tor.md#legacy-single-host-checkout-walkthrough), [public domain](clearnet.md) | Use the existing source-checkout installer | Follow the matching legacy guide |

New to OPSEC Market? Start with **native development setup**. The guide previews the command and asks separately
before saving configuration and starting services. Managed deployments require Docker Engine, Compose 2.17+ and
reviewed digest-pinned images. Direct `serve` requires PostgreSQL. The binary embeds UI assets and migrations;
it does not embed dependency servers or provision remote hosts. See [binary requirements and build options](binary.md).

## Choosing a network

Native setup defaults to stagenet and disabled escrow. Select mainnet explicitly only when you intend real funds;
development and verification use test networks. Preserve the selected network when reusing wallets or upgrading.

### Legacy shell installer defaults

`scripts/install.sh init --tor` and `init --clearnet` with local nodes (the default) write mainnet. No mainnet run
is recorded; mainnet is covered by unit, database and simulated-RPC tests only.

| You want | Pass | Writes |
| --- | --- | --- |
| Mainnet (default) | nothing | `MONERO_NETWORK=mainnet`, blank `MONERO_NETWORK_FLAG=` |
| Test networks | `--monero-network stagenet` | `MONERO_NETWORK=stagenet`, `MONERO_NETWORK_FLAG=--stagenet` |
| No nodes | `--nodes none`, or `--local` | Nothing for payments |

Details and the other options: [Enabling payments](enable-payments.md#choose-a-network). Upgrading never changes an
existing install's network ([UPGRADING](../../UPGRADING.md#64-new-installs-default-to-mainnet-a-523)).

For the legacy shell installer, nodes cannot be added after install. To take payments with a bundled node, choose
them when you install with Tor or public domain. This limitation does not prevent a native deployment from reusing
independently prepared services.

## The path

1. Read [binary requirements](binary.md) and, for production, [service topology and sizing](service-topology.md).
2. Follow guided setup or your method's flag-based procedure.
3. Complete the printed administrator setup instructions, check health and verify backups. For production Tor,
   setup uses the private administrator onion; [legacy first steps](first-steps.md) describe the older installer.
4. [Enabling payments](enable-payments.md) (optional)
5. [Production checklist](production-checklist.md) before inviting users

## Reference

- [Complete CLI reference and Bash migration](cli-reference.md)
- [Worker reuse and app-only redeployment](service-topology.md#reusing-services-when-rebuilding-the-app)
- [RAM secrets and manual incident response](ram-secrets-and-emergency.md)
- [Legacy installer reference](installer-reference.md)
- [External PostgreSQL](external-database.md)
- [Troubleshooting](troubleshooting.md)
- [Upgrading](../../UPGRADING.md)
