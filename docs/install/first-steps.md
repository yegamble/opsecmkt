# First steps

Do these right after installing: create the administrator, verify, back up.

## Native production deployment

Use this section after the [production deployment procedure](production.md).
The native controller keeps private configuration and key files in the chosen
installation directory; the legacy `.env` commands below do not manage it.

For production Tor, first authorize **desktop Tor Browser**:

1. Open the separate administrator onion and select its gray key icon or
   authentication prompt.
2. Read the saved `.auth_private` file locally. Enter only its final
   **52-character Base32 private-key field**, after `:descriptor:x25519:`.
   The prompt does not take the whole file record or an OpenPGP recovery key.
3. Continue to `/login` if host bootstrap already created the administrator, or
   `/setup` for a new market. Tor authorization and application sign-in are
   separate steps.

Use the [complete browser and persistent-client instructions](tor.md#bind-the-client-key-and-verify-ingress)
for credential recovery, error codes and the file-based alternative. Then complete
the setup below.

1. Open the intended administrator setup page. For HTTPS, use the public HTTPS
   `/setup` URL. For production Tor, configure the administrator's authorization
   key and open `/setup` on the **separate administrator onion**; the public
   onion must return `404`. Follow the
   [native separate-onion procedure](tor.md#native-production-with-separate-onions).
   Read the token locally from `INSTALL_DIR/secrets/setup-token` and keep it out
   of chat, logs and reports. Create a marketplace name of at most 80 characters,
   a permanent 3–32 character handle using letters, digits or underscores, and
   a password of 12–72 bytes. Select **Initialize marketplace**. If the host
   bootstrap command already completed setup, sign in instead.
2. Enroll an authenticator app and store the recovery codes offline. Optionally
   add a PGP key for PGP sign-in and encrypted messages. Host-side account
   recovery is covered in [Authentication](../operator-guide.md#authentication).
3. Run `./opsecmkt deploy status --dir INSTALL_DIR`. Confirm application health,
   then exercise account, listing and upload journeys through the real gateway
   and verify private administrator access and public denials. Follow the
   [production journey checks](production.md#6-complete-first-run-setup-and-verify-journeys).
   Inspect `settings.MONERO_NETWORK` in the private `operator.json`; native
   initialization defaults to stagenet and leaves escrow off unless explicitly
   enabled. Mainnet uses real funds. Keep payment intake paused until the
   separate payment and recovery checks pass.
4. Encrypt and retain the private installation, current external retention
   ledger, gateway configuration and identities. Preserve separate public and
   administrator onion keys and administrator client authorization. Add remote
   wallet state when payments are used. Schedule encrypted database backups and
   rehearse an isolated restore using the
   [production backup procedure](production.md#7-establish-backups-and-recovery).
   A managed database backup alone does not preserve these keys and state.
5. Choose [browsing, registration and branding](#choose-browsing-registration-and-branding),
   then configure roles, users, CAPTCHA, staff visibility, canary and audit exports
   through the intended administrator ingress. Complete the
   [production checklist](production-checklist.md) before inviting users, and
   use the [native app upgrade procedure](production.md#upgrade-the-application)
   for later application releases.

## Choose browsing, registration and branding

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

To change the site's identity, open **Admin → Market name and logo**. Save the
market name, then authorize and upload a custom logo if wanted. The header and
footer use the same fixed display bounds. See
[changing the market name and logo](../operator-guide.md#changing-the-market-name-and-logo)
for SVG support and private-admin gateway setup.


## Legacy shell installation

The following steps apply to `scripts/install.sh`, the checkout's `.env` and
its Compose services. Use the native production section above for an installation
created with `opsecmkt deploy init --mode prod`.

### Step 1: Create the administrator

Open the setup link the installer printed:

| Method | Setup link |
| --- | --- |
| Local | `http://127.0.0.1:8080/setup` |
| Onion service | `http://YOUR-ONION.onion/setup` in Tor Browser |
| Public domain | `https://YOUR-DOMAIN/setup` |

Fill in:

- **Setup token**: from the installer. Lost it? `./scripts/install.sh setup-token`.
- **Marketplace name**: up to 80 characters. Changeable later in `/admin/site`.
- **Handle**: 3–32 characters. **Permanent** and visible to others. Don't reuse one from elsewhere.
- **Password**: 12–72 bytes. No reset email exists, so use a password manager.

Select **Initialize marketplace**. The token then stops working.

Installed with `--admin`? Skip this and sign in at `/login`.

### Step 2: Secure the account

- Enrol an authenticator app. Store the recovery codes offline.
- Optional: add a PGP key for PGP sign-in and encrypted messages.

Locked out? Reset from the host with `-reset-admin-password` or `-reset-admin-factors`
([Authentication](../operator-guide.md#authentication)).

### Step 3: Verify

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

### Step 4: Back up

The legacy `.env` holds application secrets needed for a complete database
recovery. Its Tor service uses the active keys in the `tor_identity` volume;
retain any imported source identity too. A database dump does not include them.

1. Create an age key offline: `age-keygen -o backup-key.txt`. Keep it apart from backups.
2. Back up `.env` and onion keys:
   [Secrets, .env and onion identities](../operator-guide.md#secrets-env-and-onion-identities).
3. Schedule database backups: [Backups and recovery](../operator-guide.md#backups-and-recovery).

### Step 5: Configure

Open `/admin` for the setup checklist and the administration sections: Users (roles, second-factor resets, suspension), Payments, Security & access (site access, registration, CAPTCHA), Site & branding, Transparency (staff list, canary, audit exports) and System & audit. See [Administration sections](../operator-guide.md#administration-sections).
Details: [Feature configuration](../operator-guide.md#feature-configuration).

Use [the shared access and branding steps](#choose-browsing-registration-and-branding)
after setup; they apply to both native and legacy installations.

### What now?

- [Enabling payments](enable-payments.md)
- [Production checklist](production-checklist.md)
- [Upgrading](../../UPGRADING.md)
