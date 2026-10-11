# Test-network payments runbook

**Current build:** per-order escrow is the only payment path, off by default. Bitcoin supports regtest, signet and
testnet4; Monero supports fakechain or stagenet. Mainnet and pooled RPC settings are refused. The pooled wallet is
removed: this runbook no longer provisions, operates or backs up one. A database that still holds unresolved pooled
obligations cannot cross migration 107; see
[UPGRADING.md section 58](../UPGRADING.md#58-pooled-wallet-removal-for-fresh-installs-a-448).

Everything below applies to this build, in this reading path:

1. **Restoring from a backup** (`scripts/restore.sh` always sets the payout recovery gate):
   [back up and restore the escrow arbiter and watch-only wallets](#back-up-and-restore-the-escrow-arbiter-and-watch-only-wallets),
   then [reconcile the restored database](#reconcile-a-restored-database-before-enabling-payouts), then
   [clear the gate](#clear-the-gate). If escrow checkout stays paused afterwards, see
   [an orphan whose keys cannot be read](#an-orphan-whose-keys-cannot-be-read-escrow-checkout-stays-paused).
2. **Moderation desk:** [handle a payment-review flag](#handle-a-payment-review-flag).
3. **Chain checks:** section 9 is the local Monero fakechain regression, section 10 documents one recorded public
   Monero stagenet digital claim and clearly scoped unknowns, and section 11 documents the frozen public Bitcoin
   signet smoke. Those records establish only the journeys named in them; they do not establish other public-chain
   journeys or deployment readiness. The section numbers 9 to 11 are kept so that existing links and records keep
   resolving; the sections that once had numbers 1 to 8 described pooled provisioning and were deleted.

For current operator setup use [Bitcoin escrow](operator-guide.md#bitcoin-escrow-test-networks-only)
and [Monero escrow](operator-guide.md#monero-escrow-fakechain-or-stagenet), or leave intake paused.
Commands run from the repository directory on the Docker host, where `.env` lives.

## Back up and restore the escrow arbiter and watch-only wallets

Only for a market running [Bitcoin escrow](operator-guide.md#bitcoin-escrow-test-networks-only) (regtest, signet or
testnet4). Database dumps (`scripts/backup.sh`) contain no wallet file, and its node keeps two wallets that they do
not copy: the arbiter wallet
(`ESCROW_BTC_ARBITER_WALLET`, default `opsecmkt-arbiter`), whose seed is the market's escrow key, and the watch-only
wallet (`ESCROW_BTC_WATCH_WALLET`, default `opsecmkt-escrow-watch`), which holds every escrow's public descriptor and
address, labelled `escrow:<order ID>`. Never put either inside the database backup, `scripts/backup.sh`'s output or
the same file: a single stolen copy would then hold both the key and the records. Losing the arbiter key loses no
escrowed coins (the buyer and the vendor can still sign together), but the market can then neither decide a dispute nor
co-sign a settlement only one party signed.

Take the backups as the operator user. The app's escrow user is refused `backupwallet` and `listdescriptors`, as it
should be (*The app's RPC user* in the operator guide). Below, `opcli` is `bitcoin-cli -regtest` (or `-signet`, `-testnet4`) with the operator
user's credentials kept in a mode-600 file (for example `-conf=$HOME/.opsecmkt-operator-cli.conf` holding `rpcuser`
and `rpcpassword`), and `NODE` runs a command on the node's machine (for a container: `docker exec -i <name>`).
Encrypt each copy to the operator's key with `age` as it leaves the node, as `scripts/backup.sh` does for the
database, so no plain copy is written anywhere else:

```sh
opcli getblockcount                                         # record this height with the backups
opcli -rpcwallet=opsecmkt-arbiter backupwallet /tmp/arbiter.bak         # paths on the node
opcli -rpcwallet=opsecmkt-escrow-watch backupwallet /tmp/escrow-watch.bak
NODE cat /tmp/arbiter.bak | age -r "$AGE_RECIPIENT" > arbiter.bak.age
NODE cat /tmp/escrow-watch.bak | age -r "$AGE_RECIPIENT" > escrow-watch.bak.age
NODE rm /tmp/arbiter.bak /tmp/escrow-watch.bak
opcli -rpcwallet=opsecmkt-arbiter listdescriptors true | age -r "$AGE_RECIPIENT" > arbiter-descriptors.json.age
```

The last line keeps the arbiter's private descriptors for the fallback below; like the backup file, it is the
market's escrow key. Back the arbiter wallet up once after you create it (its descriptors are ranged, so one backup
covers every key the market derives later) and again whenever you change its descriptors. The watch-only wallet gains
an escrow at every checkout: back it up with every database backup, at least as often, and record the height each time.

**Restore the arbiter wallet.** With the app stopped. If `opcli listwalletdir` still lists the wallet, it survived:
load it (`opcli loadwallet opsecmkt-arbiter`) and restore nothing. Otherwise decrypt the backup onto the node and
restore it:

```sh
age -d -i operator-key.txt arbiter.bak.age | NODE sh -c 'cat > /tmp/arbiter.bak'
opcli restorewallet opsecmkt-arbiter /tmp/arbiter.bak true      # true: load it again after a node restart
NODE rm /tmp/arbiter.bak
```

A pruned node refuses a backup whose last synchronisation is older than its `pruneheight` ("Prune: last wallet
synchronisation goes beyond pruned data"). The arbiter wallet never holds a transaction, so nothing needs rescanning:
create a blank wallet and import the exported private descriptors with timestamp `now` instead:

```sh
opcli -named createwallet wallet_name=opsecmkt-arbiter blank=true load_on_startup=true
age -d -i operator-key.txt arbiter-descriptors.json.age |
  python3 -c 'import json, sys; print(json.dumps([{k: d[k] for k in ("desc", "active", "internal", "range", "next_index") if k in d} | {"timestamp": "now"} for d in json.load(sys.stdin)["descriptors"]]))' |
  opcli -stdin -rpcwallet=opsecmkt-arbiter importdescriptors           # every entry must say "success": true
```

`-stdin` keeps the descriptors off the command line, where other users of the machine could read them. A wallet rebuilt
this way has another `birthtime`, so the app's next start reads it whole once to check it has never spent (the
checkpoint in `escrow_btc_arbiter_checked` belongs to the old wallet); a `restorewallet` copy keeps the checkpoint.

**Check the fingerprint pin.** Start the app. The arbiter wallet must have the fingerprint this database pinned at its
first escrow start (`escrow_btc_arbiter_fingerprint` in `settings`): the log says "escrow: Bitcoin escrow on (test
networks only)", not "… the arbiter wallet's key fingerprint differs from the one pinned in this database". To compare by hand
first, `opcli -rpcwallet=opsecmkt-arbiter getaddressinfo "$(opcli -rpcwallet=opsecmkt-arbiter getrawchangeaddress bech32)"`
shows the wallet's `hdmasterfingerprint`. A different fingerprint means a different key: find the right backup; never
delete the pin to let another wallet in, since every open escrow needs the pinned key.

**Restore the watch-only wallet** the same way (`restorewallet opsecmkt-escrow-watch /tmp/escrow-watch.bak true`), then
rescan from the height recorded with it (`opcli -rpcwallet=opsecmkt-escrow-watch rescanblockchain HEIGHT`; on a pruned
node not below `pruneheight`). It holds no private key, but encrypt it anyway: its labels tie escrow addresses to
orders. An escrow created after its backup is not in the restored wallet, and the app imports a Bitcoin Core escrow's
descriptor only at checkout, so deposits to such an escrow are not seen until its descriptor is imported again by hand;
this build has no tool for that and it is not rehearsed. Import it with the escrow's descriptor
(`wsh(sortedmulti(2,<buyer key>,<vendor key>,<market key>))` with the three keys of its order, labelled
`escrow:<order ID>`), never as `addr(<address>)`: Core 31.1 reports an address that an `addr(…)` import held first as
unsolvable, with no keys, for good (importing the descriptor afterwards does not change it), and after a later database
restore the orphan sweep cannot read such an escrow's keys, so escrow checkout stays paused (below) until the watch-only
wallet is restored again and given the descriptor. A per-order escrow (A-456a) is the exception: on the first watcher
pass after the app starts, the app imports again what the wallet lacks of that escrow's checkout imports, from the
escrow's creation time, in one import for every such escrow ([per-order
escrow](escrow.md#per-order-escrow-creation-and-watching-a-456a-shape-b) says exactly what), so restart the app after
restoring the watch-only wallet. The repair rebuilds every order wallet from the keys stored at checkout, the market's
included (QA5W5A-1), so it does not need the escrow signer. While the wallet is rescanning (the `rescanblockchain` above,
the repair's own import, which rescans from the earliest per-order escrow's creation, or a repair import that took longer
than its two minutes) the app waits and imports nothing; the log reads "watch-only wallet repair waits". Core refuses
every import while it rescans, so an escrow checkout (of either kind) meanwhile is refused with "the market's escrow
wallet is rescanning the chain … try again in a few minutes", the order still a draft and nothing stored (QA5W5A-2):
after a restore, expect that for as long as the rescan takes. Then follow the reconciliation below.

The arbiter steps were rehearsed on Core 31.1 regtest by `TestEscrowRegtestArbiterRestore` (`scripts/test-escrow-regtest.sh`):
`backupwallet` before any escrow, the wallet unloaded with a vendor-signed claim waiting, `restorewallet` under a new
name (the harness cannot delete files inside the node), and a blank wallet given the exported descriptors with
`importdescriptors` and timestamp `now`; each co-signed a claim to confirmation with the fingerprint pin unchanged. The
shell lines above, `age` (1.1.1) included, were run once by hand against a Core 31.1 regtest node in a container
(`NODE` as `docker exec -i`), restoring under a new name; the fingerprint was the same after `restorewallet` and after
the fallback. Both ran on unpruned nodes: the pruned node's refusal that the fallback answers was not reproduced. The
watch-only wallet's `restorewallet` runs in `TestEscrowRegtestReserve`; its steps here (encryption, rescan) are not
rehearsed.

**Monero escrow wallets.** A database dump holds their sealed seeds, not their wallet files, which live in the escrow
wallet-rpc volume. Back up and restore that volume as the
[operator guide](operator-guide.md#monero-escrow-fakechain-or-stagenet) describes (its *Restore* paragraph), with the
same `.env` secrets, then follow the reconciliation below.

## Reconcile a restored database before enabling payouts

`scripts/restore.sh` sets `payments_recovery_required=true` in application settings. This persistent gate
pauses **every outbound send**, including those created after restoration; the escrow watchers still read
deposits and follow broadcast settlements to their confirmations. The script prints what it held and adds one audit
row recording the gate and those counts.

On a market running [Bitcoin escrow](operator-guide.md#bitcoin-escrow-test-networks-only) (test networks only), the gate
pauses escrow: the escrow watcher builds, co-signs and broadcasts no settlement while it is set, escrow checkout is
refused ("Bitcoin escrow is paused while this market restores its records from a backup. Nothing was charged and
your order is still a draft; try again later.", 503, the order stays a draft), and the script holds every restored
escrow settlement that has a stored final transaction, and every restored fee bump of one that has (it prints how many
of each, and the audit row counts them); while the gate is set no fee bump is built, co-signed or sent, and no physical
order is released at its deadline. The script also sets `escrow_release_recheck`: the escrow watcher's next pass moves
every release deadline of a shipped physical order that has passed in the restored records to 72 hours later and
tells each buyer. Confirming a
held settlement also releases its fee bumps, and the market then sends only the highest-fee stored transaction. Before
clearing the gate, check each held settlement listed under *Admin → Payment providers → Bitcoin escrow recovery and
reconciliation* on the chain and in the escrow watch-only wallet, and confirm it there if it was not already sent
or replaced; the market then broadcasts it once, and while the gate is set it refuses a second confirmation after
that send. The confirmation needs `ESCROW_BTC=on`: with escrow off the section lists held settlements without the
form. A held settlement stays held after the gate is cleared until it is confirmed. On its daily check (not at once) the escrow watcher lists in the same section every address the
watch-only wallet holds (`listreceivedbyaddress` with empty addresses included) that has no escrow row: escrows
created after the backup, and imports whose checkout did not complete, each with the order ID of its wallet label
and what it received. The market builds no settlement for them.

The restored database counts each party's next escrow child key and the market's escrow key index from where the
backup left them, so a key an escrow created after the backup used could be handed out again, even inside another
escrow address, which would link the two escrows on the chain (A-366). The script therefore also sets
`escrow_sweep_required`: escrow checkout is refused ("Bitcoin escrow is paused until this market has checked its escrow
wallet after restoring its records from a backup. …", 503, the order stays a draft) until the escrow watcher's orphan
sweep has run. While the setting is there the sweep runs on every watcher pass, gate or not: it reads the three public
keys of every address the watch-only wallet holds with no escrow row (`getaddressinfo`) and reserves them
(`escrow_reserved_keys`). An address Core reports unsolvable (held by an `addr(…)` import only) has no keys to read: it
counts as read only as a lost per-order escrow's sibling address, when another orphan with the same `escrow:<order ID>`
label gave its three keys; any other (a Bitcoin Core escrow imported again by hand as `addr(…)`, above) keeps the sweep
incomplete. Once it has read every one it removes the setting; if a read fails, the setting stays and the next pass
tries again (the app logs "orphan sweep incomplete"). For a watch-only wallet restored from the same
backup, which knows no lost escrow either, the script also moves every account's next escrow child index
(`escrow_index_floors`) and the market's escrow key index 50 past the restored records. A checkout then skips any
child key that an escrow on record holds or the sweep reserved, moving on only that party's index (or the market
index), up to 100 tries before it is refused (409); and it still asks the watch-only wallet whether it already holds
the composed address, skipping up to 20 such addresses (503 after that; the app logs "every address tried is already
in the watch-only wallet"). Clearing the gate (below) lets escrow checkout run again once the sweep has run. A
party's Core wallet signs only keys inside its descriptor range: their escrow key page advises `keypoolrefill` once
their next child index passes 900, and the order page does the same for their child key in that escrow.

On a market running [Monero escrow](operator-guide.md#monero-escrow-fakechain-or-stagenet) (a local fakechain or stagenet,
`ESCROW_XMR=on`), the gate pauses it too: the Monero escrow watcher builds and relays no settlement while it is set, and
Monero escrow checkout is refused ("Monero escrow is paused while this market restores its records from a backup. Nothing
was charged and your order is still a draft; try again later.", 503, the order stays a draft). The script holds every
restored Monero settlement with a stored transaction, built or relayed (it prints how many, and the audit row counts them),
retires every free escrow account, moves every active or sealed escrow wallet to closing so that no account of a restored
wallet takes an order again, and sets `escrow_xmr_release_recheck` and `xmr_escrow_sweep_required`. Before clearing the gate, check each
held settlement listed under *Admin → Payment providers → Monero escrow recovery and retention (local fakechain* or *Monero stagenet)* in the escrow
daemon and the escrow wallet and confirm it there if it was not already relayed or replaced; the market then relays the same
stored transaction once. Checkout stays refused ("Monero escrow is paused until this market has checked its escrow wallets
after restoring its records from a backup. …") until the Monero escrow watcher's sweep has probed for escrow wallets created
after the backup (each found is sealed to the seed key and kept, never offered to a new order, and its funded accounts are
listed to the administrators), read every wallet and raised the subaddress lookahead of any wallet older than the database;
the sweep needs the escrow wallet-rpc and the seed key and logs `escrow: Monero escrow's restore sweep is complete …` when it
has run. If only the escrow wallet volume was restored from an older backup, set the sweep marker by hand
([operator guide](operator-guide.md#monero-escrow-fakechain-or-stagenet)).

**Only `scripts/restore.sh` applies this protection.** A host or volume snapshot, a managed-database
point-in-time recovery or a manual `pg_restore` brings the database back with no gate and no held settlements, so
the escrow watchers would build, co-sign and broadcast as soon as the application starts, including a settlement that
was already sent after that point in time. After any such restore, keep the application stopped and apply the protection SQL in
[UPGRADING.md, section 6](../UPGRADING.md#6-backups-now-need-the-wallets-too) to the restored database
**before starting the application**, then follow the steps below.

The restored database knows nothing that happened after the backup. An escrow settlement that was built, signed or
broadcast after it is missing from the restored records, and an escrow wallet restored from an older copy lacks later
escrows; that is why the restore holds every restored settlement with a stored transaction until you have checked the
chain, and why checkout stays paused until the sweeps above have run. Follow these steps in order and keep the gate set
until step 7; there is no automatic timeout.

`db_sql` runs `psql` against the restored database while the app is stopped. The internal-db service is shown; replace
`opsecmkt_restored` with the database name in `DATABASE_URL`. With an external database, make `db_sql` run your own
`psql -X -v ON_ERROR_STOP=1` connected to the restored database (for example `psql service=recovery` with the password
in `~/.pgpass`), never with the password in its arguments.

```sh
db_sql() { docker compose exec -T db psql -X -v ON_ERROR_STOP=1 -U opsecmkt -d opsecmkt_restored "$@"; }
```

1. **Keep the application stopped and cut users off.** Restore with the app stopped (`docker compose stop app`),
   and before starting it on the restored database make sure only you can reach it:
   - Clearnet: stop the HTTPS reverse proxy (for the host Caddy from the operator guide,
     `sudo systemctl stop caddy`). The app itself publishes only `127.0.0.1:${APP_PORT:-8080}` on the host;
     reach it from your workstation through an SSH tunnel, `ssh -N -L 8080:127.0.0.1:8080 you@your-host`
     (the second 8080 is the host's `APP_PORT`; change it if you chose another port), and
     open `http://127.0.0.1:8080` in a browser that keeps secure cookies on `127.0.0.1` (Chromium-based
     browsers do).
   - Tor: stop the mirror if you run one (`docker compose stop tor-mirror`) and restrict the main onion
     service to your own Tor Browser with client authorization, which keeps the onion address:

     ```sh
     openssl genpkey -algorithm x25519 -out operator-auth.pem   # private key; delete it in step 8
     pub=$(openssl pkey -in operator-auth.pem -pubout -outform DER | tail -c 32 | base32 | tr -d '=')
     printf 'descriptor:x25519:%s\n' "$pub" | docker compose exec -T tor sh -c \
       'mkdir -p -m 700 /var/lib/tor/marketplace/authorized_clients && cat > /var/lib/tor/marketplace/authorized_clients/operator.auth'
     docker compose restart tor
     openssl pkey -in operator-auth.pem -outform DER | tail -c 32 | base32 | tr -d '='   # the key Tor Browser asks for
     ```

     Tor Browser asks for this key when you open the onion address; visitors without it cannot connect.

   Then, if an escrow wallet was lost or is older than this database (a new host, a lost node or escrow wallet-rpc
   volume, a wallet or volume brought back from a copy), restore it **before the app starts**: the
   [escrow arbiter and watch-only wallets](#back-up-and-restore-the-escrow-arbiter-and-watch-only-wallets) above, and
   the Monero escrow wallet volume as the operator guide says.
2. **Start the app for yourself only** (`docker compose up -d app`) and let the escrow watchers run their first
   passes, so deposits made after the backup are read. The Bitcoin orphan sweep and the Monero restore sweep described
   above run on every pass, gate or not; the Monero one logs `escrow: Monero escrow's restore sweep is complete …`.
3. **Confirm each held escrow settlement** under *Admin → Payment providers* (*Bitcoin escrow recovery and
   reconciliation*, *Monero escrow recovery and retention*), only after checking it on the chain and in the escrow
   watch-only wallet or escrow daemon as described above. Nothing is sent while the gate is set, except that a
   confirmed settlement is broadcast or relayed once.
4. **Check that the dump holds no pooled payouts to resolve.** If `scripts/restore.sh` printed a count above zero
   for restored payouts it held or marked, the dump holds pooled payout rows, which this build neither sends nor
   resolves, and migration 107 refuses a database with unresolved pooled obligations: do not continue. Restore a newer
   dump taken after the pooled drain, or use the prior build and its own recovery procedure
   ([UPGRADING.md section 58](../UPGRADING.md#58-pooled-wallet-removal-for-fresh-installs-a-448)).
5. **Stop the app** (`docker compose stop app`) and keep users cut off.
6. **Back up the reconciled database**
   (`AGE_RECIPIENT=age1YOUR_PUBLIC_RECIPIENT ./scripts/backup.sh backups/reconciled-YYYYMMDD.dump.age`, with
   `DATABASE_URL` in `.env` already naming the restored database) and keep it with this reconciliation record.
7. **Clear the gate**, with the app still stopped, as [below](#clear-the-gate). It must end "Cleared the payout
   recovery gate; committed."
8. **Restart and let users back in**:
   - Clearnet: `docker compose up -d`, then start the reverse proxy again (`sudo systemctl start caddy`).
   - Tor: remove the client authorization
     (`docker compose exec -T tor rm /var/lib/tor/marketplace/authorized_clients/operator.auth`),
     run `docker compose up -d` and `docker compose restart tor`, and delete `operator-auth.pem`. If you run the
     onion mirror, `up -d` starts it again only when `mirror` is in `COMPOSE_PROFILES` in `.env`; add it first if
     it is missing (see [Optional onion mirror](operator-guide.md#optional-onion-mirror)), and check that
     `docker compose ps tor-mirror` shows it running.

### Clear the gate

Only after steps 1 to 6 above, with the app stopped and the reconciled database backed up. Clearing the gate is
your assertion that reconciliation is complete; it refuses while any pooled payment-review flag is still open and
directs you to `/moderator` to reconcile it. It does not discover missing settlements or release individual holds.
On a market running Bitcoin escrow it also lets escrow checkout run again, with only the address check
described above under *Reconcile a restored database before enabling payouts*. Give your administrator handle: the SQL clears the gate and adds one audit row naming you ("Payout
recovery gate cleared by ...") in one transaction, and changes nothing for a handle that is not an
administrator or when the gate is not set.

<!-- runbook-sql: clear-gate -->
```sh
db_sql -v handle=YOUR_ADMIN_HANDLE <<'SQL'
BEGIN;
SELECT c.problem, c.problem = 'none' AS ready FROM (SELECT CASE
  WHEN NOT EXISTS (SELECT 1 FROM users WHERE handle=:'handle' AND role='admin') THEN 'no administrator with that handle'
  WHEN NOT EXISTS (SELECT 1 FROM settings WHERE key='payments_recovery_required' AND value='true') THEN 'the recovery gate is not set'
  WHEN EXISTS (SELECT 1 FROM pooled_drain_blockers() WHERE kind='flagged_payment') THEN 'open pooled payment reviews remain; reconcile them on /moderator before clearing the recovery gate'
  ELSE 'none' END AS problem) AS c
\gset c_
\if :c_ready
WITH cleared AS (
  UPDATE settings SET value='false' FROM users u
  WHERE settings.key='payments_recovery_required' AND settings.value='true' AND u.handle=:'handle' AND u.role='admin'
  RETURNING u.id, u.handle)
INSERT INTO audit_events(user_id,action)
SELECT id, 'Payout recovery gate cleared by ' || handle || ' after reconciling the restored database with the wallets' FROM cleared;
COMMIT;
\echo 'Cleared the payout recovery gate; committed.'
\else
ROLLBACK;
\echo 'Refused; nothing was changed:' :c_problem
\endif
SQL
```

It must print `INSERT 0 1` and end "Cleared the payout recovery gate; committed."; then go on with step 8.
"Refused; nothing was changed:" gives the reason (a mistyped or non-administrator handle, or a gate already
cleared) and rolls back.

### An orphan whose keys cannot be read: escrow checkout stays paused

For a market running [Bitcoin escrow](operator-guide.md#bitcoin-escrow-test-networks-only) (test networks only). After a restore
by `scripts/restore.sh` (or the manual SQL in UPGRADING.md section 6) escrow checkout answers 503 ("Bitcoin escrow is
paused until this market has checked its escrow wallet after restoring its records from a backup. …") until the escrow
watcher's orphan sweep has read the keys of every address the watch-only wallet holds with no escrow row. If the node
cannot answer for one of them, or answers in a shape the app does not accept, the setting `escrow_sweep_required`
stays and the sweep tries again on every pass. The log then repeats, once per pass, a line like

```text
escrow watcher: escrow: orphan sweep incomplete: getaddressinfo rpc:-1
```

(the part after the colon is the failing call and its class: `rpc:<code>` for a node error, `bad-reply` for an
answer that is not a P2WSH 2-of-3 whose three keys match its script). It names no address. A pause that lasts only
while the node or the watch-only wallet is down ends by itself on the first pass that reads every orphan. One that
lasts needs you to look at the orphan. The app has no other way to clear it.

1. List the orphans that have no reserved key yet (the app lists orphans in `escrow_orphans`, refreshed on every pass
   while the sweep runs):

   ```sh
   db_sql -c "SELECT address, order_id, received, first_seen FROM escrow_orphans o
     WHERE NOT EXISTS (SELECT 1 FROM escrow_reserved_keys k WHERE k.address = o.address) ORDER BY first_seen"
   ```

   An address the watch-only wallet does not hold as its own, or whose script is not a P2WSH, is read without error
   and never gets a reservation row, so it can appear here without being the cause.
2. Ask the watch-only wallet about each one by hand, as the operator user (the app's RPC user is allowed the same
   call):

   ```sh
   bitcoin-cli -regtest -rpcwallet=opsecmkt-escrow-watch getaddressinfo ADDRESS
   ```

   The app accepts the answer when `ismine` is true, `scriptPubKey` is a 34-byte witness-v0 script (`0020` and 32
   bytes) and `pubkeys` (or the `desc` of `wsh(multi(2,…))` or `wsh(sortedmulti(2,…))`) lists three distinct
   compressed keys whose 2-of-3 script hashes to it. An error answer (an unloaded wallet, a node still starting) is
   a node problem to fix first; the sweep then completes on its own. An answer that is complete but still not accepted
   is the case this section is for. An answer with `"solvable": false` and no keys is an address held by an `addr(…)`
   import only (the log line ends "an address held without its keys (addr(…)) is no lost per-order escrow's sibling"):
   the app accepts it only as a lost per-order escrow's sibling, whose escrow address (same label) gave its keys. If
   it is an escrow imported by hand as `addr(…)`, Core keeps it unsolvable even after its descriptor is imported:
   restore the watch-only wallet again, import the escrow's descriptor instead, and the sweep reads its keys; or, if
   you hold that descriptor, take its three keys for step 3.
3. When the answer shows the three keys, reserve them by hand, with the address they came from. Any key a lost escrow
   used is then never composed into a new escrow:

   ```sh
   db_sql -c "INSERT INTO escrow_reserved_keys(pubkey, address) VALUES
     ('KEY1_66_HEX', 'ADDRESS'), ('KEY2_66_HEX', 'ADDRESS'), ('KEY3_66_HEX', 'ADDRESS')
     ON CONFLICT (pubkey) DO NOTHING"
   ```

   The sweep skips an address that has reserved keys, so once every unreadable orphan has some, the next pass
   removes `escrow_sweep_required` itself and checkout resumes.
4. When the keys cannot be had, or the address is not an escrow of this market (for example a descriptor an
   operator imported by hand), you may instead clear the requirement. **The privacy consequence:** nothing then stops
   a later escrow from composing a child key that a lost escrow behind this address used, or a market key index it
   used. The two escrows do not merge funds (their addresses differ), but the repeated public key links them on the
   blockchain for anyone who sees both, including the other party of each. Clear it only after steps 1 to 3, with
   one audit row, and run it while the app runs or stopped:

   ```sh
   db_sql <<'SQL'
   BEGIN;
   DELETE FROM settings WHERE key = 'escrow_sweep_required';
   INSERT INTO audit_events(user_id, action) VALUES (NULL, 'Escrow orphan sweep requirement cleared by hand (docs/testnet-runbook.md)');
   COMMIT;
   SQL
   ```

   The sweep then runs only on the escrow watcher's daily check, which logs the same line each day while that orphan
   stays unreadable; reserving its keys by hand (step 3) stops it.

Three limits of the key protection, each stated in [per-order escrow](escrow.md#phase-2-bitcoin-escrow-for-physical-orders-on-regtest):

- A dump taken before migration 083 has no `escrow_index_floors` table when `scripts/restore.sh` runs, so that restore
  gets the sweep (the exact layer) but not the margin of 50 child indices per account and for the market key. If the
  watch-only wallet was also restored from an older backup, it holds none of the lost escrows, the sweep finds
  nothing to reserve, and no layer is left against a reused key beyond the check of the whole composed address.
- With the margin, a party whose last 150 or more escrows were all lost (created after the backup), or a market that
  lost 150 or more escrows in all (the market's own key index), gets "another escrow on this market already holds one
  of its keys …" (409) at checkout instead of a reused key: the margin skips 50 indices and a checkout gives up when
  it has met 100 held keys. Without the margin (a dump from before migration 083) the figure is 100. A refusal caused
  by a party's own keys comes back on every try; one caused by the market's key index clears on a later try, because
  each refused attempt moves that index on by about 100.
- When the watch-only wallet is restored from the same backup too, an account that registered its escrow key after the
  backup (A-398) has no row in the restored database: no index floor, and if it registers the same extended key
  again its next child index starts at `/0/1`, reusing child keys its lost escrows used. The market cannot detect
  this; it is inherent to restoring records that never saw the key. Tell every party who registers again after a
  restore to use a fresh wallet account.

## Handle a payment-review flag

This section is shared with the moderation desk. A flag on an escrow order is one of the escrow rows of the table below;
rows and steps that speak of the pooled wallet, a payout or the minimum automatic payout describe deposits of the removed
pooled build and stay only for the historical flags the desk can still list. Escrow deposits are never refunded by hand
from a market wallet (there is none): a stray refund from the escrow returns them.

The watcher flags a deposit (`payments.flagged`): it writes a system note ending "Moderator review
required." to the order history and notifies every moderator and administrator who is not suspended
("Payment review needed for order <first 8 characters of the order ID>"). A locked transfer, a deposit after settlement or after the order closed, or the deposits of a payout below the minimum automatic payout are flagged once. Deposits after settlement or after the order closed are announced once per episode however many arrive: one order note per watcher pass names the deposits it flagged, but the buyer (the amount, no transaction ID; the vendor is not told) and each staff member are notified only when no late flag of the order is open on the desk yet (a flag open below of a deposit neither credited, nor below the minimum, nor locked, nor spent outside the market: not yet reviewed, or escalated to an administrator; see "Record the review" below). The buyer's order page shows each as "Arrived after the order closed — not paid out; market staff notified".
A credited deposit that falls back below the threshold (conflicted, missing, or at a lower depth after a
reorg) is announced once per episode while the order is open or any of its payouts unsent (a release already sent with its overpayment return still unsent counts): the buyer and the vendor
are notified too (staff who are party to the order get that notification instead of the review one), and if
it confirms again and later regresses again it is announced again. Nothing is announced while the node is
behind the highest tip recorded (a restarted node catching up). Open **Moderation desk → Payment review**
(`/moderator#payment-review`): it lists every open flag first, oldest first and never cut, then the 100 most
recently flagged others with their count, each with the full order ID linking to the order, amount, full
transaction ID, reason and flag time (the latest flag note for that deposit). A flag is open, until staff record
its review (below), while its deposit is a locked transfer, a deposit after settlement or after the order closed,
or part of a payout below the minimum, or while a credited deposit's regression has not confirmed again (on an
escrow order also a deposit spent outside the market or left out of its settlement, credited or not; an escrow
deposit that counts for the order is never open). The desk and *Record the review* read this one rule, and only a flag it opens holds the late-deposit notice (QA462C). The
reason is read from the deposit's current confirmations. A new event on a deposit whose review you already recorded
(left out again after a changed decision, spent outside the market, its stray refund spent outside or refused, a
payout below the minimum) flags it anew with no review: it is open again with *Record the review*, dated by the note
of the new event, which names the deposit; the order history keeps the earlier review (QA462E-1, QA462F). A moderator or administrator who is not
party to the order can open its page read-only: history with the flag note, the deposit ledger (while the currency's wallet is configured) and any payout. No buyer or
vendor action is offered, the order actions refuse staff, and digital delivery content stays hidden unless
the order is disputed.

| Reason on the desk | What happened | What the application already did |
| --- | --- | --- |
| Credited deposit conflicted or missing | A deposit counted toward payment was double-spent, replaced, reorganised away or is no longer in the wallet. | Order state unchanged. Any unsent payout for the order is held (one queued later starts held). If the deposit confirms again the watcher lifts its own hold. |
| Credited deposit below threshold (N of T confirmations) | A deposit counted toward payment is back at a lower depth, usually a chain reorganisation (it may sit at 0 confirmations in the mempool). | As above: order unchanged, unsent payout held until it reaches the threshold again. |
| Credited deposit confirmed again (N confirmations) | The flagged regression ended: the deposit reached the threshold again. Listed after the open flags. | The watcher lifted its own hold; a pending payout is sent on the next pass that reads the order. Nothing to do. |
| Locked transfer (unlock time) | A Monero transfer with a non-zero `unlock_time`. | Never counted toward payment or paid out. The buyer was told to send an ordinary transfer. |
| Deposit after settlement or after the order closed, not paid out | Extra funds confirmed after the order's settlement (its release and any return, or its refund) was queued (completed or resolved orders, or a cancellation that already queued a refund), or funds first seen after the order was cancelled, completed or resolved (for example a top-up after an underpaid order was cancelled at the end of the payment window). | Not paid out or refunded: an order is settled once, and anything sent after it closed is never refunded automatically. The buyer was told the amount arrived after the order closed and was not paid out. |
| Deposit to an escrow after the order closed, not counted; a stray refund from the escrow returns it to the buyer | Coins sent to a closed escrow order's address. | Flagged and announced to the buyer and staff once per episode; never refunded by hand (it is not in a market wallet): the stray refund returns it. Record *no action* once it is returned, or escalate. |
| Escrow deposit left out of its settlement, not counted; a stray refund from the escrow returns it to the buyer | A deposit the order's release or refund does not spend: a top-up after it was built, one that did not count when the order was cancelled or decided, or one a refund too few could not spend (credited or not). | Flagged with an order note naming it; the stray refund returns it. Record *no action* once it is returned, or escalate. |
| Escrow deposit spent by a transaction this market did not make, … | Another transaction spent the escrow deposit (the parties signing together, or a key compromise). | Flagged with notices to the parties and staff; the coins are gone from the escrow. Record what you found. It does not hold the late-deposit notice episode. |
| Escrow deposit counted for the order (credited toward its funding); the order's release or refund settles it | A credited escrow deposit flagged only so staff can open the order (a release too few, A-461; a settlement spent outside the market). Not open. | Nothing to record: the order's release or refund settles it. |
| Escrow deposit counted for the order, though not credited toward its funding … | A deposit recorded before the order closed but still confirming at funding, flagged by another check. Not open. | Nothing to record: it counts for the order. |
| Below the minimum automatic payout, not paid out | The order's release or refund counted less than the minimum automatic payout: 0.0001 BTC for any Bitcoin payout (the network fee is taken from the amount, so a smaller one can never be sent), 0.001 XMR for any Monero payout (the fee is taken from the amount, so a smaller one is not worth sending). Usually a stray deposit to a cancelled order's address. | No payout row was queued; the order kept its final state. The buyer and the vendor were told it was not sent. A deposit recorded before the order closed that confirms later and lifts the sum to the minimum is paid out with it and closes the flag; one first seen after the order closed is not counted (previous row). Otherwise see step 4 below. |

What staff can do:

1. Look the transaction up on the chain and in the escrow watch-only wallet (Bitcoin: `opcli -rpcwallet=opsecmkt-escrow-watch
   gettransaction <txid>`, with `opcli` as in the
   [escrow wallet backup](#back-up-and-restore-the-escrow-arbiter-and-watch-only-wallets); Monero: `get_transfer_by_txid`
   on the escrow wallet-rpc).
2. A conflicted or below-threshold deposit that confirms again needs nothing. One that is gone for good
   means the order was never fully funded: leave its held payout held (/admin offers no *Release* while the
   deposit is below the threshold, and the server refuses one) and talk to both parties through Messages. If
   either party opens a dispute, the moderator resolves it on the desk as usual; the resulting payout is held
   too.
3. Locked transfers and extra funds after settlement or after the order closed sit in the pooled wallet. They normally belong to the
   buyer. Agree a return address with the buyer through Messages (ideally signed with their verified PGP key),
   send the funds back **by hand from the wallet** (`sendtoaddress` / `transfer`), and record it on the desk as
   *refunded by hand* (below).
4. Deposits below the minimum automatic payout are too small to send on their own: a Bitcoin amount at or
   below the fee cannot be sent with the fee taken from it, and on Monero (releases included since A-232) the fee
   would take too large a share. Decide with the parties whether to leave them in the pooled wallet or send them
   by hand (a refund to the buyer, or for a release below the minimum the vendor) with the wallet paying the fee
   (`sendtoaddress` with `subtractfeefromamount` false, or `transfer`), and record whatever you did on the desk
   (below). A flag stays
   open until a payout includes the deposits or its review is recorded.

**Record the review** (A-31). Each open flag on the desk has *Record the review* for a moderator or
administrator who is not party to the order (a party sees who must do it instead): the link in the flag's row
goes to its *Record the review of TXID:INDEX* section under the table, which opens the forms. Choose one, confirmed with
your password (and authenticator code when enrolled); each flag takes one review, which cannot be changed:

- **No action**, with the reason (10–1000 characters), for example a double spend that left the order unpaid or
  funds the buyer agreed to leave in the pooled wallet.
- **Refunded by hand**, for a deposit that is not counted toward the order's payout (a locked transfer, a deposit
  after settlement or after the order closed, or a deposit below the minimum automatic payout), after you sent
  the refund from the wallet as in steps 3 and 4: the refund's 64-character transaction ID and the amount sent
  (at most the deposit), with an optional note. The market sends nothing and writes **no payout row**: each order
  has at most one payout of each kind (`payouts` `UNIQUE (order_id, kind)`, migration 064) and never both a release and a refund (`payouts_one_settlement`, migration 069) or a refund and a return (`payouts_refund_or_return`, migration 070), and never a return without its release (`payouts_return_needs_release`, migration 073), owned by its release
  and any return, or its refund, so do not insert a payout and do not use *Mark sent* on the order's payout for a
  manual transfer. A deposit recorded as refunded by hand is never counted toward a payout afterwards, even if a
  later deposit lifts a below-minimum sum over the minimum. A credited deposit (counted toward the payout) is
  refused (409): it is settled through that payout.
- **Escalate to an administrator** (moderators only), with an optional note: every administrator who is not
  party to the order is notified, the flag stays open on the desk marked "escalated to an administrator" and only
  an administrator can then record no action or a refund by hand.

Each review writes one order-history event (the buyer and the vendor read it, with the reason or note, so keep
addresses and anything private out of it), one audit row and a notification to the buyer. A refund's transaction
ID is in the event, the audit row and the buyer's notification; on the order page only the buyer and staff
reading the order see it (the vendor reads "(transaction ID shown only to its recipient)": on Bitcoin it reveals
the buyer's address). The buyer's order page then reads "Refunded by hand on DATE: AMOUNT, transaction TXID",
or for a deposit after the order closed "…; reviewed by market staff on DATE: no action" or "…; escalated to an
administrator on DATE", instead of "market staff notified". A reviewed flag (no action or refunded) leaves the
desk; once every late-deposit flag of an order is reviewed, a later late deposit is announced to the buyer and
staff again. A credited deposit reviewed during a regression that confirms and later regresses again is a new,
unreviewed flag. A second review of the same flag is refused (409); so is one by staff who are party to the order
(403).

## 9. Manual Monero escrow fakechain check

`scripts/test-escrow-xmr-regtest.sh` runs the fifteen legs of Monero escrow ([per-order escrow](escrow.md#phase-3-monero-escrow-on-a-local-fakechain))
against a real `monerod --regtest` and real `monero-wallet-rpc` 0.18.5.1. CI runs it too (the `escrow-xmr-regtest` job, A-498). **It is for a local
fakechain only**: it refuses any daemon that does not report nettype `fakechain` with hard fork version 16 or later before any
wallet call, and each Go leg checks the same again. It needs Docker for the nodes, Go, `curl` and a disposable PostgreSQL
database (never a real deployment's). The legs run the app inside the Go test: this is not a deployment check.

Start a fakechain daemon and three wallet-rpc instances on one Docker network (the app's escrow wallet-rpc, a funder
standing in for the buyer's wallet, and one used only to restore a sealed seed). The wallet-rpcs listen without RPC login on
`127.0.0.1` only; the image's entrypoint adds `--non-interactive` and the bind address itself, and
`RPC_EXPOSE_UNAUTHENTICATED=1` lets the wallet-rpc answer on the published loopback port:

```sh
docker network create xmr-escrow-regtest
docker run -d --name xmr-escrow-monerod --network xmr-escrow-regtest -p 127.0.0.1:28381:18081 \
  ghcr.io/sethforprivacy/simple-monerod:v0.18.5.1 --regtest --keep-fakechain --offline --fixed-difficulty 1 \
  --rpc-bind-ip 0.0.0.0 --rpc-bind-port 18081 --confirm-external-bind --no-igd
for w in escrow:28382 funder:28383 restore:28384; do
  docker run -d --name "xmr-escrow-wallet-${w%%:*}" --network xmr-escrow-regtest -p "127.0.0.1:${w##*:}:18083" \
    -e RPC_EXPOSE_UNAUTHENTICATED=1 ghcr.io/sethforprivacy/simple-monero-wallet-rpc:v0.18.5.1 \
    --daemon-address xmr-escrow-monerod:18081 --trusted-daemon --wallet-dir /tmp/w --disable-rpc-login \
    --rpc-bind-port 18083 --allow-mismatched-daemon-version
done
```

Wait until the four containers report healthy (`docker ps`), then run the script with a disposable database. The legs create
their own databases from it, so give it a database you can drop afterwards:

```sh
ESCROW_XMR_REGTEST_DAEMON_URL=http://127.0.0.1:28381 ESCROW_XMR_REGTEST_WALLET_URL=http://127.0.0.1:28382 \
ESCROW_XMR_REGTEST_FUNDER_URL=http://127.0.0.1:28383 ESCROW_XMR_REGTEST_RESTORE_URL=http://127.0.0.1:28384 \
ESCROW_XMR_REGTEST_WALLET_CONTAINER=xmr-escrow-wallet-escrow ESCROW_XMR_REGTEST_DAEMON_CONTAINER=xmr-escrow-monerod \
TEST_DATABASE_URL=postgres://USER:PASSWORD@HOST:PORT/a_disposable_database?sslmode=disable ./scripts/test-escrow-xmr-regtest.sh
docker rm -f xmr-escrow-monerod xmr-escrow-wallet-escrow xmr-escrow-wallet-funder xmr-escrow-wallet-restore
docker network rm xmr-escrow-regtest
```

`ESCROW_XMR_REGTEST_WALLET_CONTAINER` is optional: with it, `RestartMidSend` also restarts that container.
`ESCROW_XMR_REGTEST_DAEMON_CONTAINER` is required: `ReRequest` stops and starts that monerod container around each relay,
which is why monerod runs with `--keep-fakechain` (without it, monerod deletes the fakechain when it starts). The legs run
in sequence and took about 11 minutes in one run on a 4-CPU host (the script allows 45 minutes for the whole run); they may
run again against the same nodes, since each puts the watcher's clock in a month of its own. The transcript is written to
`ESCROW_XMR_REGTEST_LOG` (default `artifacts/real-chain/test-escrow-xmr-regtest-<UTC time>.log`, gitignored); no seed,
password or URL is printed. The script reports each leg from its own result line, so a failed, skipped or missing leg is
named and the run fails; it ends with one `PASS:` line listing what was shown. The legs are: `Wallets`, `Funding`,
`TwoAccountsOneTx`, `Physical`, `RestartMidSend`, `RelayDropped`, `TopUp`, `Reorg`, `External`, `ReRequest`, `Restore`, `Lookahead`,
`WrongSecret` and `Destroy`, each `TestXMREscrowRegtest<Leg>` in `internal/market/`; what each proves is listed in
[per-order escrow](escrow.md#phase-3-monero-escrow-on-a-local-fakechain). A pass is evidence for the code and the 0.18.5.1
nodes on a local fakechain, not for stagenet, testnet or mainnet.

## 10. Monero escrow on stagenet

Since A-449, Monero escrow (`ESCROW_XMR=on`, off by default, custodial) may run on Monero stagenet as well as on a local
fakechain: the escrow daemon must report nettype `stagenet` with hard fork 16 or later, the escrow wallet-rpc must run with
`--stagenet` against the same daemon, and refund, claim and fee addresses must be stagenet ones (5… or 7…). Setup, the
network pin and the sync guard are in the [operator guide](operator-guide.md#monero-escrow-fakechain-or-stagenet). The [public stagenet digital-claim record](evidence/public-testnet/2026-10-10-ac71878dfde4/stagenet-xmr/README.md) proves one funded digital claim with real peers, fees and confirmations.
Public refunds, physical deadlines, fee/ring-member stress and reorganisations remain unverified.

`scripts/test-escrow-xmr-stagenet.sh` (manual, not CI) runs the legs an **offline stagenet fork, not the public stagenet**,
can run: stagenet daemons built from the genesis block with the local monerod 0.18.5.1 image, peered only with each other
on a Docker network with no route out, mining locally at fixed difficulty, plus one `--offline` daemon and a stagenet
wallet-rpc. It refuses any daemon that is not stagenet below height 10,000 (the public stagenet is past 1,900,000). Such a
chain is at hard fork version 1 (version 2 starts at height 32,000, 16 at 1,151,720), so Monero escrow refuses to start on
it, as it must; the fifteen fakechain legs of section 9 need escrow to start and cannot run on it. The script's legs show
that refusal names the hard fork rather than the network, that a real stagenet wallet's addresses are accepted under the
stagenet tags and refused under the fakechain's, and that the Monero escrow sync guard reads a restricted stagenet
daemon (as the bundled `monero` service serves) as synced with a peer and a fresh tip and an `--offline` one as having no
peers. The commands are in the script's header comment; the transcript goes to `ESCROW_XMR_STAGENET_LOG` (default
`artifacts/real-chain/test-escrow-xmr-stagenet-<UTC time>.log`, gitignored).

**What the offline fork cannot prove (A-449 acceptance).** No funded Monero escrow leg can run on it: a stagenet built
from genesis is at hard fork version 1, which Monero escrow refuses, and the fakechain legs mine with `generateblocks`,
which monerod answers only on a fakechain (`--regtest`), so the fork has only the blocks its local miner finds. Funded
Monero escrow on stagenet (a deposit seen, an order funded, a settlement built, relayed and confirmed) is therefore proven
by the [2026-10-10 public stagenet digital-claim record](evidence/public-testnet/2026-10-10-ac71878dfde4/stagenet-xmr/README.md) (A-447/A-491), scoped to one digital claim. The
offline fork proves the stagenet address formats, the refusal of an old hard fork and the escrow sync guard
against real daemons; the network pin is covered by the database tests against the simulated daemon
(`TestXMREscrowNetworkPinMismatch`, `TestXMREscrowNetworkPinBackfillAndRepin`), not by the fork.

The separate manual public runner is `scripts/test-escrow-xmr-public-stagenet.py`; its guarded digital-claim journey
uses actual confirmations and a fetched public explorer anchor. See the [operator package](operator-guide.md#escrow-node-set-up-and-the-public-network-smoke)
and the script header for its dedicated funded wallet, empty escrow wallet directory and Docker/RPC requirements.
Select an exact HTTPS block URL on `stagenet.xmrchain.net` or `xmr-stagenet.librenode.com`; both the runner and Go
test fetch the selected host's block API before wallet work. An unavailable or mismatching source refuses the run.
Its source/offline checks are not a funded public result. The recorded public digital claim passed with ten confirmations on both funding and release; it does not establish public refund timing, physical-order deadlines or deployment readiness.

## 11. Bitcoin escrow on signet or testnet4

Since A-443, Bitcoin escrow (`ESCROW_BTC=on`, off by default) may run on signet or testnet4 as well as on regtest;
mainnet and testnet3 stay refused. **A public signet smoke is recorded** ([2026-10-09 proof](evidence/public-testnet/2026-10-09-9885beef29d3/signet/README.md)): Core practice signatures, a funded digital claim and underpaid buyer refund, confirmed through a Tor-proxied node. Public testnet4 and public-chain reorganisations remain untested. Try it only on
a new database (a database never changes chain once it holds an escrow) and only with every precondition below in
place; the [operator guide](operator-guide.md#bitcoin-escrow-test-networks-only) has the settings.

1. The escrow node runs Bitcoin Core 31.1 (the sync guard reads `getblockchaininfo`'s tip `time`) on `-signet` or
   `-testnet4`, synced, with peers. The app's escrow user has the whitelist line of *The app's RPC user*, which
   includes `getnetworkinfo`: on these chains each escrow watcher pass reads the peer count, and a refused
   `getnetworkinfo` fails the node check.
2. The sync guard (A-439) holds every escrow pass while the node is in initial block download, its headers are ahead,
   it has no peers or its tip is older than `BITCOIN_MAX_TIP_AGE` (default 2h): `/admin` then shows "Bitcoin escrow
   node not synced: …". On testnet4 a block can take longer than 2h; raise `BITCOIN_MAX_TIP_AGE` (up to 24h) rather
   than let the line come and go.
3. The chain pin (A-439): the first start pins `signet:<challenge>` or `testnet4` in `escrow_btc_chain`; check the
   value on `/admin` if the node is ever replaced.
4. The previous release's A-440 pooled-user identity check was removed with pooled payments. This release refuses a
   configured `BITCOIN_RPC_URL` and does not query that user; remove its old credentials and whitelist entry after
   draining the previous release. Configure the dedicated escrow RPC user and separate signer user using the
   [operator guide](operator-guide.md#the-apps-rpc-user), and back up the arbiter and watch-only wallets as
   [escrow wallet backup](#back-up-and-restore-the-escrow-arbiter-and-watch-only-wallets) says. The escrow startup check still
   verifies the arbiter wallet's key fingerprint and that its transactions show no spend.
5. In `APP_MODE=tor` the broadcast gate (A-441) sends settlements only through a node with `-proxy` on Bitcoin Core 31.1
   or later; the stall alert reports one not seen after 6 blocks in every mode.
6. Refund, claim and fee addresses are `tb1…` (or the test-network base58 forms); one given in capitals is stored in
   lower case.
7. The arbiter wallet check (A-471, section 38 of UPGRADING) applies as on regtest: `listsinceblock` must be on the
   escrow user's whitelist. Dust paid to the market's child keys is nearly free on these chains; the start check reads
   only what is newer than its checkpoint, and `/admin` says when a long first read keeps escrow off while it goes on.
   Fee bumps of a payment to those keys do not keep escrow off either (QA4W6-1): Core lists every unconfirmed
   replacement, and the check reads each entry key by key. A read grows with the square of the replacements only while
   they are unconfirmed; once one is mined the next read passes (the operator guide's arbiter wallet paragraph).
   **Public-network caveat:** on these chains fee bumps cost almost nothing, so anyone can repeat such a burst every
   block, and while it lasts each read costs the escrow node memory (3000 bumps on Core 31.1: a 605 MB answer and
   about 1.5 GB of the node's memory per read; a node capped at 5 GiB was killed by the third read in a row). The market
   paces its own reads (A-479: after a read that takes 5 s or more or times out, pass or failure, the next waits 1
   minute, doubling to at most 1 hour; a slow pass counts as a pass meanwhile, and a quick failure is read again every
   poll), so where slow reads failed during such a burst, escrow can stay off for up to an hour after it ends; restart
   the app to check at once.
   Before any public network, run escrow on a dedicated Bitcoin Core node with memory to spare, rather than sharing
   that node with unrelated workloads.
8. Fees (QA4W6-5). Every settlement is built at `ESCROW_BTC_FEE_RATE`, default 2 sat/vB, a whole number from 1 to 100
   read at startup (set it in `.env` and restart the app), and no settlement's network fee may exceed 0.001 BTC (one
   that would is not built until the rate is lowered). On testnet4 spam often lifts nodes' mempool minimum above
   2 sat/vB: before escrow orders settle, read `mempoolminfee` from `bitcoin-cli -testnet4 getmempoolinfo` (in BTC/kvB:
   0.00002 is 2 sat/vB) and set the rate above it, keeping it well below 100 (the A-388 note under
   `ESCROW_BTC_FEE_RATE` in the operator guide). A settlement already built keeps its rate: its transaction cannot
   change once a party has signed it. While the node's `testmempoolaccept` refuses it, the market does not broadcast it;
   it waits ("escrow: settlement of order … not sent yet: mempool-rejected" in the log, "The market's Bitcoin node did
   not accept the transaction yet." on the order page) and is tried again on every pass. A fee bump cannot help before
   then: the claimant can ask for one only while the settlement is broadcast and unconfirmed. The remedy is to wait
   until the node's mempool minimum falls to the settlement's rate (the watcher then broadcasts it unchanged, and the
   claimant may bump it from the order page if it is slow to confirm), or for the buyer and the vendor to move the
   escrow together from their own wallets. A larger `-maxmempool` on the escrow node (default 300 MB) lowers that
   node's own minimum, since it evicts less, but peers with smaller mempools may not relay the transaction until
   theirs falls too.

What has run instead of a public network: `./scripts/test-escrow-regtest.sh --private-signet` (manual, not CI; Linux
with Docker) starts two Core 31.1 nodes with `-signetchallenge=51` on a Docker network created with `--internal`, mines
node A's wallet to maturity (cached for later runs), and runs every escrow leg that needs no regtest-only RPC against
node B, plus the isolated-node leg (node B leaves the network: an expired order is not cancelled; back on the network:
its deposit is credited). It is private signet, not public: same code paths and the sync guard's peer and tip checks,
but no public peers, fees or relay. The legs it does not run are listed, with the reason, in its output and in the
script's header comment. Its transcript (default `artifacts/real-chain/test-escrow-private-signet-<UTC time>.log`,
gitignored) starts with the node version, the commit, the nodes' command line with credentials redacted and the finish
time, and every line carries "[private signet, not public]".

**The public run (A-446, A-447).** Set up the escrow node with `scripts/escrow-node-setup.sh --chain signet` (or
`testnet4`). It creates the arbiter and watch-only wallets and prints the fingerprint, the `bitcoin.conf` lines and the
`ESCROW_BTC_RPC_URL` line once. Check the node with `--verify` after the restart. Then run `scripts/test-escrow-public.sh`
from a clean checkout: practice signing, one escrow claimed and one refunded, each followed to
`ESCROW_PUBLIC_CONFIRMATIONS`. It refuses regtest, mainnet, testnet3 and a private signet, and when every leg passed it
writes the record to commit under `docs/evidence/public-testnet/`. Settings and what the record holds:
[Escrow node set-up and the public-network smoke](operator-guide.md#escrow-node-set-up-and-the-public-network-smoke).
The [2026-10-09 public signet record](evidence/public-testnet/2026-10-09-9885beef29d3/signet/README.md) contains a passing Core practice/claim/refund run and measured Tor delivery, sync and storage. Public testnet4 remains unrun.

## Prior build only: recording a pooled payout sent after a backup (never run with this build)

**Do not use this section with this build.** The pooled wallet is removed, a database that holds unresolved pooled
obligations cannot cross migration 107, and the current reading path above never sends you here. The two SQL blocks are
kept byte for byte only because `scripts/test-backup-restore.sh` still executes them against the retained `payouts`
history; they go when that history does (ledger A-492).

For the prior (migration-106) build only: run these with `psql` against the restored database while the app is stopped. The internal-db service is
shown; replace `opsecmkt_restored` with the database name in `DATABASE_URL`. With an external database, make
`db_sql` run your own `psql -X -v ON_ERROR_STOP=1` connected to the restored database (for example
`psql service=recovery` with the password in `~/.pgpass`), never with the password in its arguments.

```sh
db_sql() { docker compose exec -T db psql -X -v ON_ERROR_STOP=1 -U opsecmkt -d opsecmkt_restored "$@"; }
```

Find the order from a lost send: the orders in that currency with no payout whose counted deposits add up to
the payout's amount, with the vendor's and the buyer's current payout addresses. Every payout has the fee taken
from it (except a Monero release sent before A-232), so search with the destination amount plus the fee:
a 0.0015 BTC release the wallet shows as `"amount": -0.00148590, "fee": -0.00001410` is `amount=150000`. Match
the address the wallet sent to (a recipient may have changed their address after the backup, in which case use
the order history and messages, and ask for help when unsure):

<!-- runbook-sql: find-order -->
```sh
db_sql -v currency=BTC -v amount=150000 <<'SQL'
SELECT o.id AS order_id, o.state, sum(pm.amount) AS deposits,
  CASE o.currency WHEN 'BTC' THEN v.payout_btc ELSE v.payout_xmr END AS vendor_address,
  CASE o.currency WHEN 'BTC' THEN b.payout_btc ELSE b.payout_xmr END AS buyer_address
FROM orders o JOIN products p ON p.id=o.product_id JOIN users v ON v.id=p.vendor_id JOIN users b ON b.id=o.buyer_id
JOIN payments pm ON pm.order_id=o.id AND NOT pm.locked AND (pm.credited OR pm.confirmations>=0)
WHERE o.currency=:'currency' AND NOT EXISTS (SELECT 1 FROM payouts WHERE payouts.order_id=o.id)
GROUP BY o.id, o.state, v.payout_btc, v.payout_xmr, b.payout_btc, b.payout_xmr
HAVING sum(pm.amount)=:'amount'::bigint ORDER BY o.id;
SQL
```

Then save the recording SQL once and run it for each lost payout, first as a check (`no`) and then to record
it (`yes`). Set the variables in `record_payout` for that payout: the full order ID; `kind` and `state` as
`release` and `completed` (the buyer completed it), `refund` and `cancelled` (cancelled after payment) or
`release`/`refund` and `resolved` (a dispute resolved that way); the payout's amount in satoshi or piconero
(the destination amount plus the fee for a send with the fee taken from it, as in the search above); the address
and the 64-character transaction ID from the wallet; and your administrator handle. A release whose buyer
overpaid was sent as two transactions (migration 064): the price to the vendor and the excess back to the buyer.
Record both at once: `amount`, `address` and `txid` are the vendor's, and `return_amount`, `return_address` and
`return_txid` the buyer's (leave them out for any other payout), each the destination amount plus that
transaction's fee (on either coin both; for a Monero release sent before A-232, the vendor's destination amount
alone). When finding such
an order above, search with the two amounts added together. The note written to the order history names the
network class (`MAINNET` or `TESTNET`) from the order's deposit address.

<!-- runbook-sql: record-payout -->
```sh
cat > record-payout.sql <<'SQL'
\if :{?return_amount}
\else
\set return_amount 0
\endif
\if :{?return_address}
\else
\set return_address ''
\endif
\if :{?return_txid}
\else
\set return_txid ''
\endif
BEGIN;
SELECT c.*, c.problem = 'none' AS ready FROM (
  SELECT o.id AS order_id, o.state AS restored_state, :'state' AS final_state, :'kind' AS kind, o.currency,
    CASE :'kind' WHEN 'release' THEN v.handle ELSE b.handle END AS recipient, :'amount'::bigint AS amount,
    (SELECT coalesce(sum(amount),0) FROM payments WHERE order_id=o.id AND NOT locked AND (credited OR confirmations>=0)) AS deposits,
    (SELECT count(*) FROM payouts WHERE order_id=o.id) AS payouts,
    CASE
      WHEN o.id IS NULL THEN 'no such order'
      WHEN NOT EXISTS (SELECT 1 FROM users WHERE handle=:'handle' AND role='admin') THEN 'no administrator with that handle'
      WHEN NOT EXISTS (SELECT 1 FROM settings WHERE key='payments_recovery_required' AND value='true') THEN 'the recovery gate is not set'
      WHEN EXISTS (SELECT 1 FROM payouts WHERE order_id=o.id) THEN 'the order already has a payout; resolve it on the admin page'
      WHEN (:'state',:'kind') NOT IN (('completed','release'),('cancelled','refund'),('resolved','release'),('resolved','refund'))
        THEN 'kind does not match the final state'
      WHEN o.state <> :'state' AND (:'state',o.state) NOT IN (('completed','awaiting_payment'),('completed','paid'),('completed','shipped'),
        ('completed','delivered'),('cancelled','awaiting_payment'),('cancelled','paid'),('resolved','disputed'))
        THEN 'the order cannot have reached that final state'
      WHEN :'state'='resolved' AND NOT EXISTS (SELECT 1 FROM disputes WHERE order_id=o.id AND ((status='Open' AND outcome='') OR outcome=:'kind'))
        THEN 'the dispute has another outcome'
      WHEN NOT EXISTS (SELECT 1 FROM payment_addresses WHERE order_id=o.id) THEN 'no payment address was issued for the order'
      WHEN :'amount'::bigint <= 0 THEN 'amount must be positive'
      WHEN :'return_amount'::bigint < 0 THEN 'return_amount must not be negative'
      WHEN :'return_amount'::bigint > 0 AND :'kind' <> 'release' THEN 'only a release has a return'
      WHEN :'return_amount'::bigint > 0 AND :'amount'::bigint <> o.amount THEN 'with a return, amount must be the order price'
      WHEN :'amount'::bigint + :'return_amount'::bigint <> (SELECT coalesce(sum(amount),0) FROM payments WHERE order_id=o.id AND NOT locked AND (credited OR confirmations>=0))
        THEN 'counted deposits do not add up to the amount'
      WHEN :'address' = '' THEN 'address is blank'
      WHEN :'txid' !~ '^[0-9a-f]{64}$' THEN 'transaction ID must be 64 lower-case hexadecimal characters'
      WHEN EXISTS (SELECT 1 FROM payouts WHERE txid=:'txid') THEN 'that transaction ID is already recorded on another payout'
      WHEN :'return_amount'::bigint > 0 AND :'return_address' = '' THEN 'return_address is blank'
      WHEN :'return_amount'::bigint > 0 AND (:'return_txid' !~ '^[0-9a-f]{64}$' OR :'return_txid' = :'txid')
        THEN 'return_txid must be another 64 lower-case hexadecimal transaction ID'
      WHEN :'return_amount'::bigint > 0 AND EXISTS (SELECT 1 FROM payouts WHERE txid=:'return_txid') THEN 'that transaction ID is already recorded on another payout'
      ELSE 'none' END AS problem
  FROM (SELECT 1) AS one LEFT JOIN orders o ON o.id=:'order_id' LEFT JOIN products p ON p.id=o.product_id
    LEFT JOIN users v ON v.id=p.vendor_id LEFT JOIN users b ON b.id=o.buyer_id) AS c;
\gset c_
\if :c_ready
\if :record
WITH ord AS (
  SELECT o.id, o.state, o.currency, o.buyer_id, o.product_id, p.vendor_id,
    (SELECT id FROM users WHERE handle=:'handle' AND role='admin') AS admin_id,
    (SELECT CASE WHEN bool_or(split_part(provider,'-',2) IN ('main','mainnet')) THEN 'MAINNET' ELSE 'TESTNET' END
      FROM payment_addresses WHERE order_id=o.id) AS net,
    trim_scale(:'amount'::bigint / CASE o.currency WHEN 'BTC' THEN 1e8 ELSE 1e12 END) || ' ' || o.currency AS label,
    trim_scale(:'return_amount'::bigint / CASE o.currency WHEN 'BTC' THEN 1e8 ELSE 1e12 END) || ' ' || o.currency AS return_label
  FROM orders o JOIN products p ON p.id=o.product_id WHERE o.id=:'order_id'),
pay AS (
  INSERT INTO payouts(order_id,kind,user_id,currency,amount,address,state,txid)
  SELECT id, :'kind', CASE :'kind' WHEN 'release' THEN vendor_id ELSE buyer_id END, currency, :'amount'::bigint, :'address', 'sent', :'txid'
  FROM ord ON CONFLICT (order_id,kind) DO NOTHING RETURNING id, order_id),
ret AS (
  INSERT INTO payouts(order_id,kind,user_id,currency,amount,address,state,txid)
  SELECT ord.id, 'return', ord.buyer_id, ord.currency, :'return_amount'::bigint, :'return_address', 'sent', :'return_txid'
  FROM ord, pay WHERE :'return_amount'::bigint > 0 ON CONFLICT (order_id,kind) DO NOTHING RETURNING id),
note AS (
  SELECT 'Reconciled after a restore from backup: the ' || net || ' ' || :'kind' || ' of ' || label || ' to the '
    || CASE :'kind' WHEN 'release' THEN 'vendor' ELSE 'buyer' END || ' was sent after the backup was taken'
    || CASE WHEN :'return_amount'::bigint > 0 THEN ', and the return of ' || return_label
    || ' paid over the price to the buyer' ELSE '' END
    || '. Recorded by an administrator from the wallet; nothing was sent now.' AS body FROM ord),
moved AS (UPDATE orders SET state=:'state', updated=now() FROM pay WHERE orders.id=pay.order_id RETURNING 1),
closed AS (
  UPDATE disputes SET outcome=:'kind', resolution=note.body,
    status=CASE :'kind' WHEN 'release' THEN 'Resolved — release to vendor' ELSE 'Resolved — refund to buyer' END
  FROM pay, note WHERE disputes.order_id=pay.order_id AND disputes.status='Open' AND disputes.outcome='' AND :'state'='resolved' RETURNING 1),
restocked AS (
  UPDATE products SET stock=stock+1 FROM ord, pay
  WHERE products.id=ord.product_id AND :'state'='cancelled' AND ord.state IN ('awaiting_payment','paid') RETURNING 1),
credited AS (
  UPDATE payments SET credited=true FROM pay
  WHERE payments.order_id=pay.order_id AND NOT payments.locked AND (payments.credited OR payments.confirmations>=0) RETURNING 1),
event AS (
  INSERT INTO order_events(order_id,from_state,to_state,actor_id,note)
  SELECT ord.id, ord.state, :'state', ord.admin_id, note.body FROM ord, pay, note RETURNING 1),
audit AS (
  INSERT INTO audit_events(user_id,action)
  SELECT ord.admin_id, 'Recorded payout ' || pay.id || ' (' || ord.label || ', ' || :'kind' || ') sent after the backup with transaction '
    || :'txid' || CASE WHEN :'return_amount'::bigint > 0 THEN ' and its return (' || ord.return_label || ') with transaction ' || :'return_txid' ELSE '' END
    || ' for order ' || left(ord.id, 8) || '; restored from backup, order ' || ord.state || ' -> ' || :'state'
  FROM ord, pay RETURNING 1)
SELECT count(*) = 1 AS recorded FROM audit
\gset c_
\if :c_recorded
COMMIT;
\echo 'Recorded the payout as sent; committed.'
\else
ROLLBACK;
\echo 'Nothing was recorded; rolled back.'
\endif
\else
ROLLBACK;
\echo 'Check passed; nothing was written. Run it again with -v record=yes to record this payout.'
\endif
\else
ROLLBACK;
\echo 'Refused; nothing was written:' :c_problem
\endif
SQL
record_payout() {  # record_payout no (check) | yes (record)
  db_sql -v record="$1" -v order_id=FULL_ORDER_ID -v kind=release -v state=completed -v amount=150000 \
    -v address=RECIPIENT_ADDRESS -v txid=WALLET_TRANSACTION_ID -v handle=YOUR_ADMIN_HANDLE < record-payout.sql
}
record_payout no
```

The check prints exactly one row: the order, its restored and final state, the kind, currency, recipient
handle, your amount, the counted deposits (unlocked, not missing), existing payouts, `problem` and `ready`. Go on
only when `problem` is `none`, `ready` is `t`, `payouts` is `0` and `deposits` equals `amount` (plus `return_amount`), and it ends
"Check passed; nothing was written". Otherwise it ends "Refused; nothing was written:" with the reason (an order
that already has a payout is resolved on the admin page). Then run `record_payout yes`: it repeats
the check in one transaction and ends "Recorded the payout as sent; committed." after writing, together:

- a `sent` payout row with the wallet's transaction ID, and a `sent` return row with its own when you gave one
  (an order that already has a payout is refused, and so is a transaction ID already recorded);
- the order moved to its final state if the restored snapshot was behind, with an order-history entry by your
  account ("Reconciled after a restore from backup: ..."); a resolved dispute gets that outcome, and a
  cancellation returns the reserved unit to stock as the application would. Neither text names a transaction
  ID: both parties read them, and on Bitcoin a payout's transaction ID reveals its recipient's address. The
  order page shows each payout's transaction ID only to its recipient and to staff;
- the counted deposits marked credited, so the watcher does not flag them as arriving after settlement;
- one new audit row ("Recorded payout N ... sent after the backup"). Existing audit rows are never changed.

The recipient is not notified; tell them through Messages if needed. The SQL refuses to run once the gate is
cleared: if you find a lost payout later, stop the app and set the gate again first (the `settings` and
`audit_events` statements in [UPGRADING.md](../UPGRADING.md#6-backups-now-need-the-wallets-too), between its
`BEGIN` and `COMMIT`).
