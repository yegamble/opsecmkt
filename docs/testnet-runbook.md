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

Run `scripts/install.sh` on a fresh host and choose `local` (a reviewed node image you supply) or `external`
for each currency. For local nodes it writes:

- `BITCOIN_RPC_PASSWORD` and `BITCOIN_RPC_URL=http://marketplace:<password>@bitcoin:8332`, `BITCOIN_CHAIN=testnet4`;
- `MONERO_WALLET_RPC_PASSWORD`, `MONERO_WALLET_RPC_URL=http://marketplace:<password>@monero-wallet:18083`
  (the `monero-wallet` service runs with `--rpc-login marketplace:<password>`; the app answers its HTTP
  Digest challenge), `MONERO_RPC_URL=http://monero:18081` and `MONERO_NETWORK=stagenet`.

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

The compose service runs `bitcoind -chain=${BITCOIN_CHAIN:-testnet4} -rpcport=8332 -rpcuser=marketplace
-rpcpassword=...`. `bitcoin-cli` inside the container must be told the same port and user, otherwise it looks
for the chain's default port (48332 on testnet4) and a cookie file that does not exist. The password is fed
on standard input (`-stdinrpcpass`) so it does not appear in process arguments:

```sh
chain=testnet4   # the value of BITCOIN_CHAIN, or testnet4 when it is blank
btc() {
  sed -n "s/^BITCOIN_RPC_PASSWORD='\(.*\)'$/\1/p" .env |
    docker compose exec -T bitcoin bitcoin-cli -chain="$chain" -rpcport=8332 -rpcuser=marketplace -stdinrpcpass "$@"
}
btc getblockchaininfo                      # "chain" must be your test chain; watch "initialblockdownload"
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

Each payout is sent once. The **Payouts** table on the admin page marks rows that need attention:

- **Failed — not retried; rejected by the wallet, nothing broadcast**: the wallet answered the send with an
  error (for example insufficient or locked funds), so no transaction exists.
- **Failed — not retried; outcome unknown, may have been broadcast**: the wallet call ended without a
  definite answer (no reply within 30 s, a dropped connection or an unreadable reply after the request was
  sent). The wallet may still have broadcast the transaction. Failures recorded before this distinction
  existed are shown this way too.
- **Stuck in sending**: claimed more than 5 minutes ago with no recorded outcome (crash or database error
  after the wallet call); it may have been broadcast.
- **Held**: a credited deposit is conflicted or re-confirming (the watcher releases these itself once the
  deposit is confirmed again), or the payout was restored from a backup (never released automatically).

Ordinary wallet reads time out after 10 s; a payout send is allowed 30 s, so a slow wallet that broadcasts
after 10 s is still recorded as sent.

First check the wallet for a transaction to the payout's address and amount:

```sh
btc -rpcwallet=opsecmkt listtransactions "*" 50       # Bitcoin: look for "send" to the address
xmr get_transfers '{"out":true,"pending":true,"pool":true}'   # Monero
```

Then open *Resolve payout N* on that row. Every action asks for your password (and authenticator code when
enrolled), is recorded in the audit trail and the order history, and applies only if the payout is still in
the state you saw, so a double click or a second administrator cannot queue it twice:

- **Mark sent** (failed, held or stuck): the wallet shows the transaction. Paste its 64-character
  transaction ID. The recipient is notified; nothing is sent.
- **Requeue payout** (failed or stuck): the wallet shows **no** such transaction. The payout goes back to
  the queue and the next pass sends it once. For an *outcome unknown* failure or a stuck send the form also
  asks you to tick "I checked the wallet ... no transaction ... was broadcast"; the server refuses the
  requeue without it and records the confirmation in the audit trail. Include pending and pool transfers in
  that check, and if the wallet shows the transaction use **Mark sent** instead.
- **Release held payout** (held): after a restore, once you have confirmed it was not sent. Refused while a
  credited deposit for the order is still conflicted or below the threshold.

## 6. Back up the wallets

Database dumps (`scripts/backup.sh`) do not contain the wallets. Back them up separately and encrypt the
copies like any other secret:

```sh
btc -rpcwallet=opsecmkt backupwallet /data/opsecmkt-wallet.bak
docker compose cp bitcoin:/data/opsecmkt-wallet.bak ./opsecmkt-wallet.bak
xmr query_key '{"key_type":"mnemonic"}'   # write the seed down offline; or copy the /wallet volume while stopped
```

### Reconcile a restored database before enabling payouts

Stop the application before restoring and during direct SQL reconciliation. Keep the recovered instance
isolated from users until wallet history is reconciled. `scripts/restore.sh` sets `payments_recovery_required=true` in application settings. This
persistent gate pauses **all outbound payouts**, including payouts created after restoration. It also
converts pending, sending, blocked and held payouts to manual recovery holds; reconfirming a deposit or
saving an address cannot release these holds automatically. Deposits can still be monitored if the app is
started for operator-only inspection or the admin recovery actions below; do not admit other user writes
until reconciliation is complete.

Reconcile **every restored order and deposit** against wallet transactions since the backup, not just the
held payouts. An order may have been paid out after the backup even though its restored snapshot has no
payout row at all. Reconstruct those settled orders and their sent payout records with the original
transaction IDs before allowing their lifecycle to resume. If you cannot establish the complete settlement
history, keep the gate enabled and recover a newer database or obtain operator assistance. Resolve known
held/failed payouts using step 5; releasing an individual hold does not bypass the global gate.

Only after reconciliation, with the app stopped and a backup of the reconciled database, explicitly clear
the gate in that database using `psql` (there is no automatic timeout or web unlock):

```sql
BEGIN;
UPDATE settings SET value='false' WHERE key='payments_recovery_required';
COMMIT;
```

Check that the update affected one row, then restart the app. Clearing the gate is an operator assertion
that reconciliation is complete; it does not discover missing settlements or release individual holds.
Keep the reconciliation record and wallet transaction IDs with your recovery evidence.

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
