# First steps

Do these right after installing: create the administrator, verify, back up.

## Step 1: Create the administrator

Open the setup link the installer printed:

| Method | Setup link |
| --- | --- |
| Local | `http://127.0.0.1:8080/setup` |
| Onion service | `http://YOUR-ONION.onion/setup` in Tor Browser |
| Public domain | `https://YOUR-DOMAIN/setup` |

Fill in:

- **Setup token**: from the installer. Lost it? `./scripts/install.sh setup-token`.
- **Marketplace name**: up to 80 characters. Changeable later in `/admin`.
- **Handle**: 3–32 characters. **Permanent** and visible to others. Don't reuse one from elsewhere.
- **Password**: 12–72 bytes. No reset email exists, so use a password manager.

Select **Initialize marketplace**. The token then stops working.

Installed with `--admin`? Skip this and sign in at `/login`.

## Step 2: Secure the account

- Enrol an authenticator app. Store the recovery codes offline.
- Optional: add a PGP key for PGP sign-in and encrypted messages.

Locked out? Reset from the host with `-reset-admin-password` or `-reset-admin-factors`
([Authentication](../operator-guide.md#authentication)).

## Step 3: Verify

```sh
./scripts/install.sh status
```

Expect every service healthy and setup complete. Otherwise: `docker compose logs app` and
[Troubleshooting](troubleshooting.md).

Confirm the network you meant. Installs with local nodes default to mainnet (real funds); no mainnet run is recorded;
mainnet is covered by unit, database and simulated-RPC tests only. A mainnet market's pages say MAINNET:

```sh
grep -E '^MONERO_NETWORK=' .env
```

`mainnet` selects real funds; `stagenet` and a blank Monero network select test funds. Once
escrow has pinned a network it does not change: start over on a new database
([Choose a network](enable-payments.md#choose-a-network)).

## Step 4: Back up

`.env` holds every secret. Without it, a database backup cannot be fully restored. On Tor, the onion address
exists only as keys in the `tor_identity` volume.

1. Create an age key offline: `age-keygen -o backup-key.txt`. Keep it apart from backups.
2. Back up `.env` and onion keys:
   [Secrets, .env and onion identities](../operator-guide.md#secrets-env-and-onion-identities).
3. Schedule database backups: [Backups and recovery](../operator-guide.md#backups-and-recovery).

## Step 5: Configure

Open `/admin` for access, registration, roles, users, CAPTCHA, the staff list, the canary and audit exports.
Details: [Feature configuration](../operator-guide.md#feature-configuration).

Before sharing the address, choose who can browse and create accounts:

1. Open **Admin → Site access and registration**.
2. Select **Public** for guest browsing, or **Members only** to require sign-in
   for listings and other site content.
3. Set **New registrations** to **Open** or **Closed** independently.
4. Enter your current password and authenticator code, if enabled, then select
   **Save access settings**.

The settings take effect without restarting. Closing registration keeps existing
accounts working; first-administrator setup is always available before
initialization. Verify the result in a signed-out browser session: members-only
pages should lead to sign-in, and closed registration should show a notice
without an account-creation form. Then verify that an existing account can sign
in and browse.

For automated setup, pass `--site-access members --registrations closed` to the
native `deploy init` or `serve` command. These are first-start defaults; later
administrator choices are stored in PostgreSQL and survive restarts. See
[the CLI reference](cli-reference.md#additional-runtime-settings).

## What now?

- [Enabling payments](enable-payments.md)
- [Production checklist](production-checklist.md)
- [Upgrading](../../UPGRADING.md)
