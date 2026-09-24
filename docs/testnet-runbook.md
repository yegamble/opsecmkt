# Test-network payments runbook

End-to-end operator steps for turning on Bitcoin (testnet4/signet/regtest) and Monero (stagenet/testnet)
payments with the compose deployment. **Test networks only: these coins have no value and the application
refuses mainnet.** Commands run from the repository directory on the Docker host, where `.env` lives.

What the application does on its own, so you know what to expect while following these steps:

- A configured node that is unreachable, still starting, or whose wallet is missing does **not** stop the
  site. The admin page shows that currency as *Unavailable — retried every poll* with the error, payment
  requests for it are refused ("Payment unavailable for BTC"), and every watcher pass
  (`PAYMENT_POLL_INTERVAL`, default 30 s) tries again: it reconnects, loads the Bitcoin wallet (`loadwallet`)
  or opens the Monero wallet (`open_wallet`) if the node was restarted, and then enables the currency.
- Before a currency is used, the node's network is checked. A mainnet node or wallet stops startup; one that
  appears later (for example a node swapped under a running site) disables that currency with *Refused — not
  a test network* until the node is fixed and the app restarted.
- While the Bitcoin node is in initial block download, or the Monero daemon's `target_height` is above its
  `height`, the watcher skips that currency entirely: no deposits are read, no order expires and no payout is
  sent. The admin page shows "Node is still syncing". The Monero sync check needs `MONERO_RPC_URL`; without
  it only the wallet is checked. Only `target_height` above `height` counts as syncing (monerod reports a
  target of 0 once it considers itself synchronized), so a daemon that reports no target never blocks
  payments; other signals such as `synchronized` are deliberately not used.
- If reading the wallet fails during a pass, orders whose addresses were not read do not expire in that pass.

## 1. Configure

Run `scripts/install.sh` on a fresh host and choose, for each currency, `local` (the default: a node in
Docker from the default image or a reviewed image you supply), `external` or `disabled`. Local nodes are
**pruned** unless you answer `no` to "Prune the … node": a pruned Bitcoin node needs about 5 GB (testnet4)
to 8 GB (signet), and the first sync of either coin takes hours. Sizes, full nodes and the wallet-restore
trade-off: [Local node pruning](operator-guide.md#local-node-pruning). For local nodes it writes:

- `BITCOIN_RPC_PASSWORD` and `BITCOIN_RPC_URL=http://marketplace:<password>@bitcoin:8332`, `BITCOIN_CHAIN=testnet4`,
  `BITCOIN_PRUNE_MB=2000` (`0` for a full node);
- `MONERO_WALLET_RPC_PASSWORD`, `MONERO_WALLET_RPC_URL=http://marketplace:<password>@monero-wallet:18083`
  (the `monero-wallet` service runs with `--rpc-login marketplace:<password>`; the app answers its HTTP
  Digest challenge), `MONERO_RPC_URL=http://monero:18081`, `MONERO_NETWORK=stagenet` and
  `MONERO_PRUNE_FLAGS='--prune-blockchain --sync-pruned-blocks'` (blank for a full node).

For external nodes `BITCOIN_CHAIN` and `MONERO_NETWORK` are left blank, so the app accepts whichever test
network the node reports; set them in `.env` if you want the app to insist on one. If you choose an external
Monero daemon but leave the wallet RPC URL blank, the installer warns that Monero payments stay disabled.
Upgrading an existing deployment? Follow [UPGRADING.md](../UPGRADING.md) first.

Confirmation settings (in `.env`, applied on restart):

| Key | Default | Notes |
| --- | --- | --- |
| `PAYMENT_CONFIRMATIONS_BTC` | 3 | Use 1 on regtest, where you mine blocks yourself. |
| `PAYMENT_CONFIRMATIONS_XMR` | 10 | Monero outputs are spendable after 10 blocks anyway. |
| `PAYMENT_POLL_INTERVAL` | 30s | 1s to 1h. |
| `PAYMENT_EXPIRY` | 24h | Unpaid orders with no deposit seen are cancelled after this. |

Wait for the nodes to sync before expecting deposits to be seen (`docker compose logs -f bitcoin monero`).

## 2. Create the Bitcoin wallet

The compose service runs `bitcoind -chain=${BITCOIN_CHAIN:-testnet4} -datadir=/data -prune=${BITCOIN_PRUNE_MB:-2000}
-rpcport=8332 -rpcuser=marketplace -rpcpassword=...`. `bitcoin-cli` inside the container must be told the same port and user, otherwise it looks
for the chain's default port (48332 on testnet4) and a cookie file that does not exist. The password is fed
on standard input (`-stdinrpcpass`) so it does not appear in process arguments:

```sh
chain=testnet4   # the value of BITCOIN_CHAIN, or testnet4 when it is blank
btc() {
  sed -n "s/^BITCOIN_RPC_PASSWORD='\(.*\)'$/\1/p" .env |
    docker compose exec -T bitcoin bitcoin-cli -chain="$chain" -rpcport=8332 -rpcuser=marketplace -stdinrpcpass "$@"
}
btc getblockchaininfo                      # "chain" must be your test chain; watch "initialblockdownload"; "pruned"
btc -named createwallet wallet_name=opsecmkt load_on_startup=true
btc -rpcwallet=opsecmkt getwalletinfo
btc -rpcwallet=opsecmkt getnewaddress funding bech32   # fund the pooled wallet from a faucet
```

`load_on_startup=true` makes bitcoind reload the wallet after a restart; the app would also load it on its
next pass. Use the wallet name from `BITCOIN_WALLET` if you changed it. `compose.nodes.yaml` sets
`-fallbackfee=0.0002` because test networks often have too little fee data for estimation, which would
otherwise make every payout fail.

For an external node, run the same RPCs with your own `bitcoin-cli` configuration.

## 3. Create the Monero wallet

The app opens a wallet file named `opsecmkt` with an empty password in `monero-wallet-rpc`'s
`--wallet-dir` (`/wallet`, the `monero_wallet` volume) when none is open. Create it once with the
`create_wallet` RPC. The wallet RPC publishes no port, so call it from inside the deployment network. The
credentials are passed to curl on standard input, not in its arguments:

```sh
xmr() {  # xmr <method> '<json params>' (always pass params, '{}' for none)
  printf 'user = "marketplace:%s"\n' "$(sed -n "s/^MONERO_WALLET_RPC_PASSWORD='\(.*\)'$/\1/p" .env)" |
    docker compose exec -T monero-wallet curl -sS --digest --config - \
      -H 'Content-Type: application/json' \
      --data "{\"jsonrpc\":\"2.0\",\"id\":\"0\",\"method\":\"$1\",\"params\":$2}" \
      http://127.0.0.1:18083/json_rpc
}
xmr create_wallet '{"filename":"opsecmkt","password":"","language":"English"}'
xmr get_address '{"account_index":0}'    # primary address: starts with 5 (stagenet) or 9/A (testnet)
xmr get_height '{}'
```

This needs `curl` in your reviewed Monero image. If it has none, run the same `curl` command from a reviewed
curl image attached to the backend network instead of `docker compose exec -T monero-wallet curl`:
`docker run --rm -i --network opsecmkt_backend <reviewed-curl-image@sha256:...> -sS --digest --config - ...
http://monero-wallet:18083/json_rpc`.

Fund the primary address from a stagenet faucet. Keep a small buffer: Monero payout fees are paid by the
pooled wallet on top of each payout.

For an external `monero-wallet-rpc`, start it with `--rpc-login marketplace:<password>` (never
`--disable-rpc-login`) and put the same credentials in `MONERO_WALLET_RPC_URL`.

## 4. Watch the admin payments panel

Open **Admin → Payment providers**. For each currency:

| Status | Meaning | What to do |
| --- | --- | --- |
| Enabled | Checks passed; *Last poll* updates after every completed pass. *Last error* shows the latest wallet error, "Wallet check failed; watcher pass skipped …" when the node or wallet stopped answering, or "Node is still syncing". | Nothing, unless the error persists. |
| Unavailable — retried every poll | Configured, but the node is unreachable, the credentials are rejected or the wallet cannot be loaded/opened. Payment requests are refused. | Fix the node or create the wallet; it is picked up within one poll interval, no restart needed. |
| Refused — not a test network | The node or wallet reported mainnet (or a different chain than before) after startup. The currency is off. | Fix the node configuration, then restart the app. |
| Disabled | The URL is blank. | Set it in `.env` and restart. |

The app logs the same transitions (`docker compose logs app | grep payments`).

Now request payment on a test order: the order page shows a `TESTNET <network>` deposit address. Send test
coins; the order moves to *Paid* once the deposit reaches the confirmation threshold.

## 5. Recover a held or failed payout

Each payout is sent once. The **Payouts** table on the admin page marks rows that need attention and lists
every one of them first, oldest first, under a count ("N payouts need attention"); other payouts are limited
to the 50 most recent. An order ID links to the order page where the administrator can open it (an order they are party to, a
disputed or resolved order, or one with a payment flagged for review); otherwise it is plain text.

- **Failed — not retried; the wallet reported a pre-broadcast error, nothing broadcast**: the wallet
  answered the send with a pre-broadcast error (for example insufficient or locked funds), so no
  transaction exists. For Bitcoin this is any `sendtoaddress` error: Bitcoin Core stores the transaction
  before relaying it, and a failed relay is not returned as an error. For Monero it is only a `transfer`
  error raised before the wallet submits to the daemon: -2 (wrong address), -16 (transaction not
  possible), -17 (not enough money), -18 (transaction too large), -19 (not enough outputs to mix), -20 (no
  destination) or -37 (not enough unlocked money).
- **Failed — not retried; outcome unknown, may have been broadcast**: the wallet call ended without a
  definite answer (no reply within 30 s, a dropped connection or an unreadable reply after the request was
  sent), or monero-wallet-rpc returned any other error code. In particular -38 (no connection to daemon)
  is also returned when `sendrawtransaction` timed out after monerod may already have relayed the
  transaction, and -4 or -1 can follow a submission. The wallet may still have broadcast the transaction.
  Failures recorded before this distinction existed are shown this way too.
- **Stuck in sending**: claimed more than 5 minutes ago with no recorded outcome (crash or database error
  after the wallet call); it may have been broadcast.
- **Held**: a credited deposit is conflicted or re-confirming (the watcher releases these itself once the
  deposit is confirmed again). While the deposit is still below the threshold the row offers no *Release*
  form; it names the deposit (transaction ID and output), its deposit address and its confirmations and links
  to the order. Only *Mark sent* is offered, for a payout you already sent by hand.
- **Held after a restore from backup — may already have been sent**: the payout was pending, sending,
  blocked or held in a restored dump (its error starts `Restored from backup:`). It is never released
  automatically, and it may have been broadcast after the backup was taken.
- **Failed after a restore from backup — may have been requeued and sent after the backup**: the payout
  was failed in a restored dump. Even a definite wallet rejection may have been requeued and sent after the
  backup was taken, so the restore marks it as possibly sent (its error starts `Restored from backup:`,
  followed by the failure recorded before the backup).

Ordinary wallet reads time out after 10 s; a payout send is allowed 30 s, so a slow wallet that broadcasts
after 10 s is still recorded as sent.

First check the wallet for a transaction to the payout's address and amount, covering everything since
**before** the failed send (for a payout held or marked by a restore, since before the backup was taken).
Nothing short of that is a check: `listtransactions "*" 50` shows only the newest 50 wallet entries (every
deposit and send counts), so a payout followed by more entries is missing from it.

Bitcoin: list every wallet transaction since a block mined before the failure. Pick a height safely earlier
(testnet4 and signet mine about 144 blocks a day; a larger margin only lengthens the list):

```sh
btc getblockcount                                         # the current height
from=$(btc getblockhash HEIGHT_BEFORE)                    # a height mined before the failure or the backup
btc -rpcwallet=opsecmkt listsinceblock "$from"            # look for "category": "send" to the payout address
btc -rpcwallet=opsecmkt listtransactions "*" 100000       # the alternative: effectively the whole wallet
```

`listsinceblock` also lists unconfirmed sends and, under `removed`, transactions a reorganisation took out.

Monero: `get_transfers` builds its `pending` list before it updates the transaction pool, and the wallet
refreshes by itself only every 20 s, so a single call can miss a send that has just reached the pool. Refresh,
list, wait at least 20 s and do both again:

```sh
xmr refresh '{}'
xmr get_transfers '{"out":true,"pending":true,"pool":true}'
sleep 30
xmr refresh '{}'
xmr get_transfers '{"out":true,"pending":true,"pool":true}'
```

Look for the payout's address and amount in `destinations` of the `out`, `pending` and `pool` entries. A send
the daemon relayed but the wallet reported as an error is recorded from the pool **without `destinations`**,
with `amount` the total it spent less change (the payout plus the fee). Treat any `pending` or `out` transfer
created after the failure (its `timestamp`) that has no destinations as **this payout** until you have proven
otherwise, for example by matching it to another payout's transaction ID; never requeue or release while one
is unexplained.

Then open *Resolve payout N* on that row. Every action asks for your password (and authenticator code when
enrolled), is recorded in the audit trail and the order history, and applies only if the payout is still in
the state you saw, so a double click or a second administrator cannot queue it twice:

- **Mark sent** (failed, held or stuck): the wallet shows the transaction. Paste its 64-character
  transaction ID. The recipient is notified; nothing is sent.
- **Requeue payout** (failed or stuck): the wallet shows **no** such transaction. The payout goes back to
  the queue and the next pass sends it once. For an *outcome unknown* failure, a failure restored from
  backup or a stuck send the form also asks you to tick "I checked the wallet ... no transaction ... was
  broadcast"; the server refuses the requeue without it and records the confirmation in the audit trail.
  Include pending and pool transfers in that check, and if the wallet shows the transaction use **Mark
  sent** instead.
- **Release held payout** (held): the payout goes back to the queue and the next pass sends it once.
  Refused (409, nothing changed) while a credited deposit for the order is still conflicted or below the
  threshold. The watcher releases its own hold once the deposit confirms again; a hold from a restore or a
  suspension stays until you release it, which works once the deposit has confirmed again (the watcher keeps
  reading that order's deposits while the payout is held, whatever its age). For a payout
  held by a restore the form also asks you to tick "I checked the wallet ... no transaction ... was
  broadcast"; the server refuses the release without it and records the confirmation in the audit trail
  and the order history. If the wallet shows the transaction use **Mark sent** instead. A payout that was
  held for an account suspension when the backup was taken keeps that hold behind the restore marker
  (*Held after a restore from backup and for an account suspension*): releasing it needs both the wallet
  confirmation and "Payout address checked".

Each of these forms repeats the wallet check above in one line next to the checkbox.

### Handle a payment-review flag

The watcher flags a deposit (`payments.flagged`): it writes a system note ending "Moderator review
required." to the order history and notifies every moderator and administrator who is not suspended
("Payment review needed for order <first 8 characters of the order ID>"). A locked transfer or a deposit after settlement is flagged once.
A credited deposit that falls back below the threshold (conflicted, missing, or at a lower depth after a
reorg) is announced once per episode while the order is open or its payout unsent: the buyer and the vendor
are notified too (staff who are party to the order get that notification instead of the review one), and if
it confirms again and later regresses again it is announced again. Nothing is announced while the node is
behind the highest tip recorded (a restarted node catching up). Open **Moderation desk → Payment review**
(`/moderator#payment-review`): it lists every open flag first, oldest first and never cut, then the 100 most
recently flagged others with their count, each with the full order ID linking to the order, amount, full
transaction ID, reason and flag time (the latest flag note for that deposit). A flag is open while its deposit
is a locked transfer or a deposit after settlement, or while a credited deposit's regression has not confirmed
again; the reason is read from the deposit's current confirmations. A moderator or administrator who is not
party to the order can open its page read-only: history with the flag note, the deposit ledger (while the currency's wallet is configured) and any payout. No buyer or
vendor action is offered, the order actions refuse staff, and digital delivery content stays hidden unless
the order is disputed.

| Reason on the desk | What happened | What the application already did |
| --- | --- | --- |
| Credited deposit conflicted or missing | A deposit counted toward payment was double-spent, replaced, reorganised away or is no longer in the wallet. | Order state unchanged. Any unsent payout for the order is held (one queued later starts held). If the deposit confirms again the watcher lifts its own hold. |
| Credited deposit below threshold (N of T confirmations) | A deposit counted toward payment is back at a lower depth, usually a chain reorganisation (it may sit at 0 confirmations in the mempool). | As above: order unchanged, unsent payout held until it reaches the threshold again. |
| Credited deposit confirmed again (N confirmations) | The flagged regression ended: the deposit reached the threshold again. Listed after the open flags. | The watcher lifted its own hold; a pending payout is sent on the next pass that reads the order. Nothing to do. |
| Locked transfer (unlock time) | A Monero transfer with a non-zero `unlock_time`. | Never counted toward payment or paid out. The buyer was told to send an ordinary transfer. |
| Deposit confirmed after settlement, not paid out | Extra funds confirmed after the order's single release or refund was queued (completed or resolved orders, or a cancellation that already queued a refund). | Not paid out: an order has exactly one payout. |

What staff can do:

1. Look the transaction up in the wallet (the `btc` and `xmr` helpers from steps 2 and 3; for incoming funds use
   `btc -rpcwallet=opsecmkt gettransaction <txid>` or
   `xmr get_transfer_by_txid '{"txid":"<txid>"}'`).
2. A conflicted or below-threshold deposit that confirms again needs nothing. One that is gone for good
   means the order was never fully funded: leave its held payout held (/admin offers no *Release* while the
   deposit is below the threshold, and the server refuses one) and talk to both parties through Messages. If
   either party opens a dispute, the moderator resolves it on the desk as usual; the resulting payout is held
   too.
3. Locked transfers and extra funds after settlement sit in the pooled wallet. They normally belong to the
   buyer. Agree a return address with the buyer through Messages (ideally signed with their verified PGP key),
   send the funds back **by hand from the wallet** (`sendtoaddress` / `transfer`), and record it.

There is no web action to dismiss a flag or pay out a flagged deposit, and an open flag stays on the desk. A
manual refund is recorded as a **written note** (an order-history event plus an audit row), **never as a
payout row**: `payouts.order_id` is unique, so the order's single release or refund owns that row; do not
insert a payout and do not use *Mark sent* on the order's payout for a manual transfer. Record the note with
`psql` (internal-db shown; replace the order ID, your staff handle and the note text):

```sh
docker compose exec -T db psql -X -v ON_ERROR_STOP=1 -v order_id=FULL_ORDER_ID -v handle=YOUR_HANDLE \
  -v note='Manual TESTNET refund: returned 0.0005 BTC from deposit <txid>:<n> to the buyer by hand in <refund txid>.' \
  -U opsecmkt -d opsecmkt <<'SQL'
WITH ev AS (
  INSERT INTO order_events(order_id,from_state,to_state,actor_id,note)
  SELECT o.id,o.state,o.state,u.id,:'note' FROM orders o JOIN users u ON u.handle=:'handle' AND u.role IN ('moderator','admin')
  WHERE o.id=:'order_id' RETURNING order_id,actor_id)
INSERT INTO audit_events(user_id,action) SELECT actor_id,'Order '||order_id||': '||:'note' FROM ev;
SQL
```

It must report `INSERT 0 1`; `INSERT 0 0` means the order ID or staff handle was wrong and nothing was
written. The note appears in the order history, which the buyer and vendor also read, so keep addresses and
anything private out of it.

## 6. Back up the wallets

Database dumps (`scripts/backup.sh`) do not contain the wallets. Back them up separately and encrypt the
copies like any other secret:

```sh
btc -rpcwallet=opsecmkt backupwallet /data/opsecmkt-wallet.bak
btc getblockcount                          # record this height with the backup (pruned nodes, below)
docker compose cp bitcoin:/data/opsecmkt-wallet.bak ./opsecmkt-wallet.bak
xmr query_key '{"key_type":"mnemonic"}'   # write the seed down offline; or copy the /wallet volume while stopped
```

**The local Bitcoin node is pruned by default, which limits how old a restorable wallet backup can be.** A
backup restored with `restorewallet` loads only if the height recorded with it is at or above the node's
current `pruneheight` (`btc getblockchaininfo`); otherwise bitcoind refuses with "Prune: last wallet
synchronisation goes beyond pruned data". Take wallet backups often enough that the latest one stays inside
the prune window. For an older backup, restore the whole `bitcoin_data` volume from a copy taken with
bitcoind stopped, or run the node unpruned (a full re-download) until the wallet has loaded and rescanned:
see [Local node pruning](operator-guide.md#local-node-pruning). Monero wallets restore and refresh through a
pruned daemon (upstream documentation; not rehearsed here).

### Reconcile a restored database before enabling payouts

`scripts/restore.sh` sets `payments_recovery_required=true` in application settings. This persistent gate
pauses **all outbound payouts**, including payouts created after restoration; the watcher still records
deposits and moves funded orders to *Paid*. The script also converts pending, sending, blocked and held payouts
to manual recovery holds; reconfirming a deposit or saving an address cannot release these holds
automatically. Failed payouts stay failed but are marked as possibly sent (`send_ambiguous`, error prefixed
`Restored from backup:`): one the wallet had rejected before the backup may have been requeued and sent after
it, so requeueing it also needs the wallet confirmation. A payout held for an account suspension keeps that
text behind the restore marker, so releasing it still needs the payout address check. The script prints how
many payouts it held and how many failed payouts it marked, and adds one audit row recording the gate and those
counts.

**Only `scripts/restore.sh` applies this protection.** A host or volume snapshot, a managed-database
point-in-time recovery or a manual `pg_restore` brings payouts back as `pending` with no gate, and the
application sends them as soon as it starts, even those already paid after that point in time. After any such
restore, keep the application stopped and apply the protection SQL in
[UPGRADING.md, section 6](../UPGRADING.md#6-backups-now-need-the-wallets-too) to the restored database
**before starting the application**, then follow the steps below.

The restored database knows nothing that happened after the backup. An order that was completed, cancelled or
resolved **and paid out** after the backup comes back in its earlier state with **no payout row**. If its buyer
completes it again (or a moderator resolves it again) once the gate is cleared, the application queues and sends
a second payout. Follow these steps in order and keep the gate set until step 8. There is no automatic timeout
and no web action that clears the gate or records a lost payout.

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
     openssl genpkey -algorithm x25519 -out operator-auth.pem   # private key; delete it in step 9
     pub=$(openssl pkey -in operator-auth.pem -pubout -outform DER | tail -c 32 | base32 | tr -d '=')
     printf 'descriptor:x25519:%s\n' "$pub" | docker compose exec -T tor sh -c \
       'mkdir -p -m 700 /var/lib/tor/marketplace/authorized_clients && cat > /var/lib/tor/marketplace/authorized_clients/operator.auth'
     docker compose restart tor
     openssl pkey -in operator-auth.pem -outform DER | tail -c 32 | base32 | tr -d '='   # the key Tor Browser asks for
     ```

     Tor Browser asks for this key when you open the onion address; visitors without it cannot connect.
2. **Start the app for yourself only** (`docker compose up -d app`) and let the watcher catch up: wait until
   *Last poll* on **Admin → Payment providers** is later than the start for every configured currency, so
   deposits made after the backup are in the ledger.
3. **Resolve the payouts the restore held or marked** on the admin page, with the wallet checks in
   [section 5](#5-recover-a-held-or-failed-payout) (*Mark sent*, *Release held payout*, *Requeue payout*).
   Nothing is sent while the gate is set.
4. **List every wallet send since the backup was taken** and match each to a payout by transaction ID, with the
   complete checks from [section 5](#5-recover-a-held-or-failed-payout) (`from` is a block mined before the
   backup; the Monero listing runs twice, 20 s or more apart, after a `refresh`):

   ```sh
   btc -rpcwallet=opsecmkt listsinceblock "$from"                       # "send" entries after the backup time
   xmr refresh '{}'
   xmr get_transfers '{"out":true,"pending":true,"pool":true}'         # "out", "pending" and "pool" transfers
   docker compose exec -T db psql -X -U opsecmkt -d opsecmkt_restored \
     -c "SELECT currency, txid, order_id, state FROM payouts WHERE txid <> '' ORDER BY currency, txid"
   ```

   (`-d` names the database in `DATABASE_URL`.) A send with no payout row is either a manual refund recorded as
   a written note (see "Handle a payment-review flag"; leave it alone) or a **payout the restore lost**. Find
   the order of each lost payout from the amount sent (Bitcoin amounts ×100 000 000 in satoshi, Monero
   destination amounts are already in atomic units; fees are separate) and the address it went to, in step 6.
   A Monero `pending` or `out` transfer after the backup with no `destinations` is an unmatched send too (its
   `amount` includes the fee): it counts as a lost payout until you prove otherwise.
   If a send cannot be matched to exactly one order, an order settled after the backup is missing from the
   restored database, or the order already has a `pending` payout queued since the restore, keep the gate set:
   restore a newer backup or get operator assistance.
5. **Stop the app** (`docker compose stop app`) and keep users cut off.
6. **Record each payout the restore lost**, as below.
7. **Back up the reconciled database**
   (`AGE_RECIPIENT=age1YOUR_PUBLIC_RECIPIENT ./scripts/backup.sh backups/reconciled-YYYYMMDD.dump.age`, with
   `DATABASE_URL` in `.env` already naming the restored database) and keep it with the wallet transaction IDs
   and this reconciliation record.
8. **Clear the gate**, with the app still stopped, as below. It must end "Cleared the payout recovery gate;
   committed."
9. **Restart and let users back in**:
   - Clearnet: `docker compose up -d`, then start the reverse proxy again (`sudo systemctl start caddy`).
   - Tor: remove the client authorization
     (`docker compose exec -T tor rm /var/lib/tor/marketplace/authorized_clients/operator.auth`), run
     `docker compose up -d` (which also starts the mirror if you run one) and `docker compose restart tor`, and
     delete `operator-auth.pem`.

#### Record a payout sent after the backup (step 6)

Run these with `psql` against the restored database while the app is stopped. The internal-db service is
shown; replace `opsecmkt_restored` with the database name in `DATABASE_URL`. With an external database, make
`db_sql` run your own `psql -X -v ON_ERROR_STOP=1` connected to the restored database (for example
`psql service=recovery` with the password in `~/.pgpass`), never with the password in its arguments.

```sh
db_sql() { docker compose exec -T db psql -X -v ON_ERROR_STOP=1 -U opsecmkt -d opsecmkt_restored "$@"; }
```

Find the order from a lost send: the orders in that currency with no payout whose counted deposits add up to
the amount sent, with the vendor's and the buyer's current payout addresses. Match the address the wallet sent
to (a recipient may have changed their address after the backup, in which case use the order history and
messages, and ask for help when unsure):

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
`release`/`refund` and `resolved` (a dispute resolved that way); the amount in satoshi or piconero; the address
and the 64-character transaction ID from the wallet; and your administrator handle.

<!-- runbook-sql: record-payout -->
```sh
cat > record-payout.sql <<'SQL'
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
      WHEN :'amount'::bigint <> (SELECT coalesce(sum(amount),0) FROM payments WHERE order_id=o.id AND NOT locked AND (credited OR confirmations>=0))
        THEN 'counted deposits do not add up to the amount'
      WHEN :'address' = '' THEN 'address is blank'
      WHEN :'txid' !~ '^[0-9a-f]{64}$' THEN 'transaction ID must be 64 lower-case hexadecimal characters'
      WHEN EXISTS (SELECT 1 FROM payouts WHERE txid=:'txid') THEN 'that transaction ID is already recorded on another payout'
      ELSE 'none' END AS problem
  FROM (SELECT 1) AS one LEFT JOIN orders o ON o.id=:'order_id' LEFT JOIN products p ON p.id=o.product_id
    LEFT JOIN users v ON v.id=p.vendor_id LEFT JOIN users b ON b.id=o.buyer_id) AS c;
\gset c_
\if :c_ready
\if :record
WITH ord AS (
  SELECT o.id, o.state, o.currency, o.buyer_id, o.product_id, p.vendor_id,
    (SELECT id FROM users WHERE handle=:'handle' AND role='admin') AS admin_id,
    trim_scale(:'amount'::bigint / CASE o.currency WHEN 'BTC' THEN 1e8 ELSE 1e12 END) || ' ' || o.currency AS label
  FROM orders o JOIN products p ON p.id=o.product_id WHERE o.id=:'order_id'),
pay AS (
  INSERT INTO payouts(order_id,kind,user_id,currency,amount,address,state,txid)
  SELECT id, :'kind', CASE :'kind' WHEN 'release' THEN vendor_id ELSE buyer_id END, currency, :'amount'::bigint, :'address', 'sent', :'txid'
  FROM ord ON CONFLICT (order_id) DO NOTHING RETURNING id, order_id),
note AS (
  SELECT 'Reconciled after a restore from backup: the TESTNET ' || :'kind' || ' of ' || label || ' to the '
    || CASE :'kind' WHEN 'release' THEN 'vendor' ELSE 'buyer' END || ' was sent after the backup was taken, in transaction '
    || :'txid' || '. Recorded by an administrator from the wallet; nothing was sent now.' AS body FROM ord),
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
    || :'txid' || ' for order ' || left(ord.id, 8) || '; restored from backup, order ' || ord.state || ' -> ' || :'state'
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
only when `problem` is `none`, `ready` is `t`, `payouts` is `0` and `deposits` equals `amount`, and it ends
"Check passed; nothing was written". Otherwise it ends "Refused; nothing was written:" with the reason (an order
that already has a payout is resolved on the admin page, step 3). Then run `record_payout yes`: it repeats
the check in one transaction and ends "Recorded the payout as sent; committed." after writing, together:

- a `sent` payout row with the wallet's transaction ID (`payouts.order_id` is unique, so this refuses an order
  that already has one, and a transaction ID already recorded is refused too);
- the order moved to its final state if the restored snapshot was behind, with an order-history entry by your
  account ("Reconciled after a restore from backup: ..."); a resolved dispute gets that outcome, and a
  cancellation returns the reserved unit to stock as the application would;
- the counted deposits marked credited, so the watcher does not flag them as arriving after settlement;
- one new audit row ("Recorded payout N ... sent after the backup"). Existing audit rows are never changed.

The recipient is not notified; tell them through Messages if needed. The SQL refuses to run once the gate is
cleared: if you find a lost payout later, stop the app and set the gate again first (the `settings` and
`audit_events` statements in [UPGRADING.md](../UPGRADING.md#6-backups-now-need-the-wallets-too), between its
`BEGIN` and `COMMIT`).

#### Clear the gate (step 8)

Only after every step above, with the app stopped and the reconciled database backed up. Clearing the gate is
your assertion that reconciliation is complete; it does not discover missing settlements or release individual
holds. Give your administrator handle: the SQL clears the gate and adds one audit row naming you ("Payout
recovery gate cleared by ...") in one transaction, and changes nothing for a handle that is not an
administrator or when the gate is not set.

<!-- runbook-sql: clear-gate -->
```sh
db_sql -v handle=YOUR_ADMIN_HANDLE <<'SQL'
BEGIN;
SELECT c.problem, c.problem = 'none' AS ready FROM (SELECT CASE
  WHEN NOT EXISTS (SELECT 1 FROM users WHERE handle=:'handle' AND role='admin') THEN 'no administrator with that handle'
  WHEN NOT EXISTS (SELECT 1 FROM settings WHERE key='payments_recovery_required' AND value='true') THEN 'the recovery gate is not set'
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

It must print `INSERT 0 1` and end "Cleared the payout recovery gate; committed."; then go on with step 9.
"Refused; nothing was changed:" gives the reason (a mistyped or non-administrator handle, or a gate already
cleared) and rolls back.

## 7. Manual regtest check (Bitcoin)

`scripts/regtest-smoke.sh` exercises the Bitcoin adapter against a local `bitcoind -regtest` that you run
yourself; it is not part of CI. It refuses any node that is not on regtest.

```sh
bitcoind -regtest -daemon -rpcuser=smoke -rpcpassword=smoke-pass -fallbackfee=0.0002
BITCOIN_REGTEST_RPC_URL=http://smoke:smoke-pass@127.0.0.1:18443 ./scripts/regtest-smoke.sh
bitcoin-cli -regtest -rpcuser=smoke -rpcpassword=smoke-pass stop
```

The script creates or loads the `opsecmkt` wallet, mines 101 blocks to it, pays a fresh order address, mines
one block and checks that the adapter reports the output with its confirmation. To try the whole site
against the same node, start the app with `BITCOIN_RPC_URL=http://smoke:smoke-pass@127.0.0.1:18443`,
`BITCOIN_CHAIN=regtest` and `PAYMENT_CONFIRMATIONS_BTC=1`, request payment on an order, pay it with
`bitcoin-cli -regtest -rpcwallet=opsecmkt sendtoaddress <address> <amount>` and mine a block with
`generatetoaddress 1 <any address>`; within one poll interval the order is *Paid*. Stopping `bitcoind`
while the app runs makes each pass skip BTC with "Wallet check failed" as the last error; starting it again
resumes without a restart. Starting the app while `bitcoind` is down shows BTC as *Unavailable* until it is
reachable.

## 8. Isolated real-chain browser testing

For a reproducible local Bitcoin regtest and offline Monero stagenet environment,
see [real isolated-chain browser tests](local-chain-testing.md). The maintained
setup script starts verified native nodes, mines disposable coins and funds the
market wallets. The browser suite uses real RPC adapters and wallet transactions;
it is separate from the simulated-wallet suite and from public testnet validation.
