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

## Step 4: Back up

`.env` holds every secret. Without it, a database backup cannot be fully restored. On Tor, the onion address
exists only as keys in the `tor_identity` volume.

1. Create an age key offline: `age-keygen -o backup-key.txt`. Keep it apart from backups.
2. Back up `.env` and onion keys:
   [Secrets, .env and onion identities](../operator-guide.md#secrets-env-and-onion-identities).
3. Schedule database backups: [Backups and recovery](../operator-guide.md#backups-and-recovery).

## Step 5: Configure

Open `/admin` for roles, users, CAPTCHA, the staff list, the canary and audit exports.
Details: [Feature configuration](../operator-guide.md#feature-configuration).

## What now?

- [Enabling payments](enable-payments.md)
- [Production checklist](production-checklist.md)
- [Upgrading](../../UPGRADING.md)
