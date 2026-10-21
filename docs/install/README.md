# Installing OPSEC Market

Start with the standalone CLI for guided setup or explicit flags. Choose a topology, prepare the required services,
then follow the printed administrator setup instructions. The shell installer remains available for older checkouts.

> **Prerelease software.** The native CLI changes are unreleased; build the current checkout to use them.
> New installs take **no payments** until escrow is configured and enabled. Native setup defaults to stagenet;
> the legacy shell installer's local nodes default to **mainnet (real funds)**. Mainnet payment and settlement workflows remain unverified;
> deployment and readiness checks do not establish real-funds settlement. See [Implementation status](../implementation-status.md).

## Choose a method

| Method | Use it to | Start |
| --- | --- | --- |
| [Native app-host bootstrap](server-bootstrap.md) | Prepare a supported Ubuntu host and export a reusable firewall recipe | Run `./opsecmkt host --help` |
| [Production with managed PostgreSQL](production.md) | Deploy prepared separate services with verified TLS, encrypted storage, backups and recovery | Follow the production procedure |
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

For production, use [native app-host bootstrap](server-bootstrap.md) where supported and [harden the servers](server-hardening.md) and prepare remote service endpoints before initialization.
If escrow is needed, include the payment flags in that single initialization; configuring escrow and enabling intake are separate steps.

### Legacy shell installer defaults

`scripts/install.sh init --tor` and `init --clearnet` with local nodes (the default) write mainnet. Mainnet payment and settlement workflows remain unverified;
deployment and readiness checks do not establish real-funds settlement.

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

For production, follow [the complete deployment procedure](production.md) in order:

1. Choose separate roles and a private managed PostgreSQL endpoint. Record exact approved operator and service source addresses.
2. [Harden each host](server-hardening.md), install encrypted roots and prove off-host manual unlock before transferring production secrets.
3. Verify private TLS and database permissions. Prepare payment services now if needed; select their network before the single initialization.
4. [Build, publish and inspect the application image](binary.md#building-and-publishing-the-application-image). Pass its full repository/digest reference as `--app-image`, then initialize the app once.
5. Complete [private administrator setup](first-steps.md#native-production-deployment), enroll MFA and keep payments disabled or verify paused intake before opening public ingress. Choose [browsing, registration and branding](first-steps.md#choose-browsing-registration-and-branding).
6. Verify public and private allowed/denied paths, uploads, coordinated encrypted backups, isolated restore and runtime return after a locked reboot.
7. Complete the [production checklist](production-checklist.md). [Enable payment intake deliberately](enable-payments.md) only after its separate readiness and recovery checks pass.

## Reference

- [Complete CLI reference and Bash migration](cli-reference.md)
- [Worker reuse and app-only redeployment](service-topology.md#reusing-services-when-rebuilding-the-app)
- [RAM secrets and manual incident response](ram-secrets-and-emergency.md)
- [Legacy installer reference](installer-reference.md)
- [Production deployment](production.md)
- [Server hardening](server-hardening.md)
- [Existing deployment security review](security-review.md)
- [Existing deployment storage assessment](storage-security-assessment.md)
- [External PostgreSQL](external-database.md)
- [Troubleshooting](troubleshooting.md)
- [Upgrading](../../UPGRADING.md)
