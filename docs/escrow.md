# Per-order Monero escrow

The marketplace supports Monero (XMR) only. Each order claims a dedicated account in a monthly wallet. Escrow is custodial: the market holds the wallet keys, builds and relays settlements, and can move its funds. There is no implemented Monero multisig or buyer-held escrow key.

Monero conceals sender, recipient and amount details on-chain, reducing exposure to public blockchain observers. It does not hide order or payment metadata from the operator, prevent traffic correlation, or protect keys on a compromised host. Tor relay configuration and the wallet's custody model remain separate requirements.

Escrow is off by default. Native `opsecmkt deploy init` defaults to stagenet; the legacy `scripts/install.sh` bundled nodes default to mainnet unless stagenet is chosen. Mainnet has real funds and no recorded real-funds transaction. See [Enabling Monero payments](install/enable-payments.md), [operator configuration](operator-guide.md#monero-escrow) and [verification evidence](verification.md). The details below describe the implemented Monero lifecycle; older phase names are retained as anchors for evidence links.

## Phase 3: Monero escrow on a local fakechain

**Local fakechain, stagenet or mainnet; off by default; custodial.** `ESCROW_XMR` enables Monero escrow. A fakechain is a local `monerod --regtest` chain with coins of no value; it uses mainnet's address format (refund/claim addresses begin with 4 or 8). The daemon must report the expected network and a supported hard fork. The dedicated wallet-rpc must be attached to the same daemon and within the permitted height span. The market offers escrow for digital listings with automatic delivery and physical listings with a dispatch window.

**The custody sentence.** Every Monero escrow page, notice and document says the same thing: the coins are *held by
this market in an account used only for this order, inside a wallet the market opens each month; the market can move
these coins*. There is no key of the buyer's or the vendor's and no signature to paste: the
market holds the wallet keys and builds, stores and sends every transaction itself.

**What it does.**

- **Monthly wallets, one account per order.** A watcher of its own, with its own lock and its own `monero-wallet-rpc`
  (one that holds only escrow wallets), creates a wallet named `escrow-YYYYMM-NN` for each
  month. Its password is derived from `MONERO_WALLET_SECRET` and its name, so the database holds no wallet password. Its
  25-word seed is encrypted to an OpenPGP public key whose private half the operator keeps offline (`ESCROW_XMR_SEED_KEY`,
  never the canary key) and stored with the wallet's primary address and restore height before the wallet may take an
  order. Next month's wallet is created from 7 days before its month starts, so that a daily backup holds its seal before it
  takes an order; it becomes active at the start of the month (UTC), and the old one becomes *closing*, with its free
  accounts retired. A first run, or a market that was down past a month's start, creates and activates that month's wallet
  at once, with no lead. The watcher keeps `ESCROW_XMR_SLOTS` (20 by default) free accounts ready on the active wallet.
  An account the wallet lists with no row in the database is never made free (A-556): a `create_account` whose answer was
  lost, or a database restored to an earlier point without `scripts/restore.sh`, after which the wallet may already have
  given that account, and accounts the records show free, to buyers. On the active wallet, outside a restore, the watcher
  then retires every free account of that wallet with the unrecorded ones in one transaction and tells the administrators
  once (a notification and an audit row); the next pass creates fresh accounts. A lost answer costs one batch of free
  accounts, never a pause. After each start of the market, checkout claims no account until the watcher's first full
  pass has read and reconciled the active wallet (PAY66-9), so the protection covers every checkout from then on; while
  the escrow daemon is not synced or the wallet cannot be opened, checkout stays closed. The mark is held in memory, so a
  second application process on the same database whose watcher never holds the watcher lock refuses Monero escrow
  checkout, and so does a new process during a redeploy until the old one has stopped. It does not detect an account
  claimed after a restore point when the wallet created none after it: after a restore not done by `scripts/restore.sh`,
  the protection in UPGRADING.md section 6 is still required. Coins at a retired account (one no order claimed) alert the
  administrators once per account.
  Account 0 of a wallet is never an escrow and is never spent from; coins that arrive there alert the administrators, once per episode.
- **The slot claim at checkout.** The buyer of an XMR draft on an eligible listing pays into a Monero escrow, giving a refund address from their own wallet (a standard address or subaddress; integrated
  addresses, other networks, addresses whose keys are not valid, market-controlled addresses, and external addresses the buyer account already used are refused;
  it is checked in Go, with no wallet call, and cannot be changed afterwards). Checkout makes no RPC: in one
  transaction it moves the order out of draft, claims a free account of the active wallet by compare-and-set and pins the
  refund address, the market fee in force (`ESCROW_XMR_FEE_BPS`) and, for a physical order, the release deadline length and
  dispatch window. With no free account, before the watcher's first full pass after a start, while the restore gate is set, after a restore until the sweep below has run, or
  while Monero escrow is unavailable, it answers 503 and the order stays a draft.
- **Funding at 10 or more confirmations.** The watcher reads each wallet's incoming transfers (`get_transfers` over all
  accounts). A deposit counts from 10 confirmations (or `PAYMENT_CONFIRMATIONS_XMR` when that is higher) with no unlock
  time and no double-spend seen; a locked deposit is flagged for staff and never counted. A digital order completes on
  funding with its content delivered; a physical order becomes paid. An order that received nothing in its payment window is
  cancelled; one that is underpaid (every deposit confirmed, together short of the price, and no top-up for a whole payment
  window) is cancelled too, which requests its refund.
- **Settlements: built, stored and relayed by the watcher, never rebuilt.** Only the watcher builds a settlement, and
  only once the order has its outcome (a release on a completed order or one decided for the vendor, a refund on a
  cancelled order or one decided for the buyer, a stray refund on any closed order). It checks that the open wallet's
  primary address is the one on record and that the order's account has the escrow address, freezes every output of that
  account the settlement does not claim, and asks for one transfer from that account (`account_index` is never 0 and never
  absent) with `do_not_relay`, taking the network fee from the claimant's output alone. It then checks the wallet's
  answer: exactly the claimed outputs' key images, amounts as planned, a fee at most `ESCROW_XMR_MAX_FEE` and the
  claimant's output at least 0.001 XMR; otherwise the transaction is discarded unrelayed, the settlement waits and staff
  are told, with the deposits it could spend flagged so they can open the order (the note names them, so the desk
  dates the flags: QA462E-3); a deposit recorded after the order
  closed is left to the late-deposit check, which flags it when it announces it to the buyer and staff, so a waiting
  stray refund never silences that notice (QA462C-4). A checked transaction is **stored first** (its hash, metadata, blob, fee and key images) and only then relayed,
  with the stored metadata. After a restart or a lost answer the watcher looks the transaction up in the daemon and the
  wallet, and relays the same stored metadata again if neither has it: that spends the same key images, so it cannot pay
  twice. Nothing builds a replacement automatically. A relayed settlement is *confirmed* at the confirmation threshold and
  followed to 100 confirmations; one a reorganisation removes goes back to *relayed* and is relayed again. If another
  transaction spent its key images it is recorded *external* and staff are told, at once. Otherwise a refusal is
  relayed again, as the same stored metadata, on the next pass (`monero-wallet-rpc` gives the same answer when it lost its
  connection to the daemon), and only after 3 refusals in a row is the settlement *failed*; staff are then told, and an
  administrator may re-request it (below). A held settlement whose relay an administrator confirmed after a restore keeps
  that confirmation through those refusals, so it is relayed again while the restore gate is still set. Stored metadata
  the wallet cannot parse (-27, which only a damaged record explains) fails the settlement at once, flagged once, and is
  never relayed again. When building, the wallet's -37 (the coins are not unlocked yet) only waits, never flagged; -17
  (not enough money) waits once and is flagged if it persists, except for a refund whose coins are too few for the
  network fee (below, A-467). These are the codes `monero-wallet-rpc` 0.18.5.1 gave
  when recorded (`tests/e2e/fixtures/xmr_rpc_shapes_0.18.5.1.json`).
- **Tor mode (A-515).** With `APP_MODE=tor`, `relay_tx` would send a settlement through the escrow daemon from the
  host's IP address, so nothing is relayed (a stored one is not relayed again either) unless `COMPOSE_FILE` includes
  `compose.nodes-tor.yaml` or `ESCROW_XMR_TOR_RELAY=clearnet-accepted`; settlements are still built, checked and stored,
  with one startup log line and an `/admin/payments` line ("Monero relays held: Tor mode without the node Tor overlay — see operator
  guide"). Both are read at startup; after a restart with either, the stored transaction is relayed unchanged and never
  rebuilt. Monero over Tor itself is unverified (A-95). `TestXMREscrowTorModeWithoutNodeProxyHoldsRelay` (simulated
  wallet-rpc and PostgreSQL, not chain evidence) covers holding, both ways to resume, a lost relayed transaction while
  held, clearnet mode and an invalid value refused at startup; see the
  [operator guide](operator-guide.md#monero-escrow) (*Tor mode*).
- **Refunds and strays.** A refund goes to the address pinned at checkout, with the network fee taken from the amount.
  A release pays the vendor's claim address the escrow less the market fee (fixed at checkout; a fee below 0.001 XMR is
  added to the vendor's output) and less any amount paid above the price, which goes back to the buyer's refund address
  (also added to the vendor's output when below 0.001 XMR). A settlement spends the deposits that count when it is built; a
  top-up (a deposit not credited toward the funding) still confirming then, or arriving after the build, is left out and
  returned to the buyer's refund address by a *stray refund* once the order has closed. A deposit credited toward the
  funding is handled differently (A-554): while a reorganisation has one below the confirmation threshold again, the
  release or refund is not built (*waiting-confirmations*), so it never pays from the confirmed part of the funding. One
  the wallet no longer lists (popped and not mined again, or read as a double spend) is waited on (*deposit-missing*)
  for twice the threshold in blocks from the escrow wallet's height when it was first found missing; a listing in
  between ends that episode, so a later disappearance waits the full time again. The settlement is then built from the
  deposits that remain, with an order note naming the one it was built without. Should the wallet list that deposit
  again, what happens depends on the settlement: after a release it stays in the escrow account for staff (it is never
  left out, so no stray refund pays the vendor's coins to the buyer); after a refund the coins were the buyer's anyway,
  so it is left out, with a note that it was credited and listed again, and the stray refund returns it to the buyer's
  refund address (PAY66-6). Each such deposit is flagged to staff once per episode.
  Limits: a regression is noticed and flagged only once a release or refund of the order is being built (not while the
  order is paid or shipped and no settlement is due); and once staff have recorded *no action* on the flag, a later
  regression of the same deposit writes a new order note and staff notice but does not reopen the flag on the
  payment-review desk. A
  settlement whose credited deposits are all missing waits for them rather than failing *nothing-to-settle*, which is
  left for a settlement with no credited deposit at all (a failed settlement without a transaction cannot be
  re-requested). Such a settlement stays requested (*deposit-missing*) for as long as none of those deposits returns,
  and while it does, its order's monthly wallet is never destroyed: the wallet and its sealed seed are kept past their
  90 days. There is no web action that ends it; staff see it as the order's wait line, an open flag of the missing
  deposit on the payment-review desk ("Credited deposit conflicted or missing"), the moderator desk's alert for a
  settlement requested for more than 24 hours, and, past the 90 days, the administrators' notice that a retained
  wallet is kept. Resolving it is an operator decision taken by hand (PAY66-7; a code change is a separate ledger item).
  A deposit that did not count when its order was cancelled (the wallet read it as a double spend, or
  no longer listed it) and has confirmed since is left out and returned by a stray refund too, when no refund spends it: it is flagged
  and the buyer told. The order page lists a left-out deposit on its own line, not counted as received, and a cancelled
  order's status follows the stray refund that returns it: requested or built, sent, confirmed (A-435). A
  stray refund whose coins are too few for the network fee and an output of at least 0.001 XMR stays requested,
  staff told once (the status of a deposit that did not count then says the market cannot send it), and is built once
  a further deposit makes the coins enough. A refund too few so, on a cancelled order or one decided for the buyer,
  ends instead once no deposit it would spend is still confirming (failed, too-small): its deposits are left out and
  flagged, with one note and the parties and staff told, so the one stray refund returns them together with a later
  deposit, or one that did not count at the cancellation or the decision and has confirmed since, once together they
  pay the fee. Before, the refund waited for good while a later deposit's stray refund waited too or paid without its
  coins (A-437). Coins at or below the network fee itself never reach that check: asked to take the fee from the
  refund, `monero-wallet-rpc` 0.18.5.1 refuses to build it, with -17 (not enough money; 0.001 and 0.0015 XMR on a young
  fakechain), -16 (tx not possible; 0.0018 XMR) or -46 (destination amount is zero: exactly at the fee); each code was
  recorded from the real wallet at those amounts (`xmr_rpc_shapes_0.18.5.1.json`, QA5W1-5). Those codes also mean other things
  (coins the wallet no longer sees as unlocked or confirmed, a wallet behind the chain), so on them, for a refund or a
  stray refund, the market reads the fee the wallet would take, with calls that build nothing: the priority the wallet
  uses for `transfer` at priority 0 (`get_default_fee_priority`; its -1 means priority 1), the weight it estimates for
  the inputs claimed and two outputs (`estimate_tx_size_and_weight`, ring 16) and the escrow daemon's fee per byte at
  that priority with its quantization mask (`get_fee_estimate`, on monerod's restricted RPC). The fee is wallet2's own
  formula, weight times fee per byte rounded up to the mask; no margin is added, because wallet2's weight estimate is
  already above the built transaction's (on the fakechain, A-467 probe: estimated 1536 for one input, built 1507, so
  0.0018432 XMR against 0.0018084 XMR). When the refund less that fee is below 0.001 XMR, the refund is too few as
  above (ended, or for a stray refund waiting, too-small), whatever the wallet's reason; otherwise the refusal keeps its
  class (-17 waits once and is then an outputs mismatch, flagged; -16 and -46 a refusing wallet). The estimate only
  decides among refusals: a refund the wallet builds is checked as before, and a refund classed too few by a slightly
  high estimate is returned by the stray refund, which the wallet then builds. A fee estimate that fails never classes
  the refund too few: it waits on that settlement alone (`fee-estimate-waiting`), is flagged to staff once if the next
  pass fails the same way (`fee-estimate-unavailable`), and is tried again on each pass, while the wallet's pass goes
  on with its other orders, their payment windows and the wallet's retention (QA5W1-4). A failed estimate and a refusal
  count as one run of passes without a build: when the estimate fails only on some passes, the second pass in a row is
  flagged as whichever class it records (the refusal, or the unavailable estimate) (QA5W2-1). Within that run, which
  ends only when the settlement is built or leaves its waiting state, staff are told once of each distinct flagged
  class it records (the unavailable estimate, an outputs mismatch, a refusing wallet with its code, too few): a class
  already told that recurs, after another class or a wait never flagged such as -37's `outputs-waiting`, is told
  nothing new, while a different diagnosis is (the wallet seeing fewer coins than it listed, or refusing, once the
  estimate answers; a stray refund's coins found too few). What was told is read from the settlement's own order notes,
  so a restart re-announces nothing; a stray refund already flagged too-small that a restart makes read a failing
  estimate is told of the unavailable estimate once, and not of too-small again (QA5W3B-3, QA5W3B-4). A stray refund waiting
  too-small that the wallet keeps refusing is logged once, and its fee is not read again while it claims the same
  outputs; `transfer` is still asked on each pass, so a later deposit that lets it build is seen (QA5W1-6). A release
  too few still waits, and its refusals keep their classes: the
  coins are the vendor's. After a close, deposits are refunded automatically until the wallet is
  destroyed; after that, the order page says Monero sent to its address since then cannot be recovered (ES-8).
- **Confirm receipt and deadlines.** For a digital order the vendor claims the escrow with a fresh Monero address and their
  password once the order has completed on funding. For a physical order the vendor's *Mark as shipped* takes the claim
  address and their password, requests the release to it and fixes the release deadline; nothing is built until the
  order is completed or its dispute is decided for the vendor. The buyer's **Confirm receipt**, with their password,
  completes the order, after which it can no longer be disputed. If the buyer does neither before the deadline (14 days
  by default; the shared release-deadline setting applies, see the limits) and no dispute is open, the order
  completes by the system and the release is built. The buyer is reminded 72 hours before, in the market only, and
  `/orders` shows a banner. The deadlines run in the Monero watcher, so they apply whenever Monero escrow is configured, even while the
  escrow wallet-rpc is down. The no-ship cancel of Phase 2 is shared: a buyer may cancel a paid physical order that was not
  shipped once the dispatch window plus 48 hours has passed, and the escrow is refunded.
- **Disputes: decisions are final.** An administrator decides a dispute on a Monero escrow order with their password; a
  moderator's resolve is refused. A Monero decision cannot be changed once made, because the
  market builds and sends it on its next pass; the resolve form says so before submission. A decision for the buyer
  supersedes a release that has no stored transaction yet and is refused (409) once the release has one.
- **Retention and destruction.** A wallet whose orders have all closed is *retained* and is still read on every pass, so
  a late deposit is refunded. 90 days after its last order closed, and only when no order of it is open, no settlement
  of it is live or still followed to 100 confirmations, its balance and unlocked balance are zero across all accounts
  and nothing of it is in the pool, the watcher replaces its password with 32 random bytes that are never stored, closes
  it, wipes its sealed seed and records it destroyed. Only those preconditions destroy a wallet automatically. The
  watcher records that it began destroying a wallet (migration 086) before it replaces the password, so a destruction
  interrupted after the new password took effect (a crash, or a restore from a backup taken in between) ends destroyed
  on a later pass, once the wallet-rpc refuses that wallet as "Invalid password", provided staff were never flagged that
  it holds something and neither the restore gate nor the sweep marker is set. A retained wallet past its 90 days that
  still holds coins (an unclaimed paid account, coins at account 0, a failed settlement) is not destroyed, and staff are
  flagged once. The market cannot reach the wallet volume: `/admin/payments` lists each destroyed wallet's two files for the
  operator to delete.
- **Restore holds and the orphan sweep.** `scripts/restore.sh` (or the manual SQL in
  [UPGRADING.md](../UPGRADING.md#6-backups-now-need-the-wallets-too)) holds every restored Monero settlement that has a
  stored transaction until an administrator confirms its relay on `/admin/payments`; retires every free account; moves every
  active or sealed wallet to *closing*, so no account of a restored wallet, one the restored records lack included, is
  offered to a new order; asks for a release deadline recheck (the deadline of a shipped order that had passed in the
  restored records moves 72 hours later, and the buyer is told); and sets `xmr_escrow_sweep_required`. Until the
  watcher's sweep has run, checkout answers 503 and no wallet or account is created. The sweep looks for wallets created
  after the backup: in each month from 12 months back through next month, the names after that month's newest row. One
  that opens with the derived password is sealed to the seed key as an *orphan*, kept, never offered an account, and its
  funded accounts are listed to the administrators; one that does not open is recorded as an orphan and never opened. It
  also gives a wallet older than the database a longer subaddress lookahead and rescans it, so deposits to accounts it
  does not know are seen. Under the gate nothing is built or relayed; deposits are still read.
- **If a wallet cannot be opened.** A wallet the escrow wallet-rpc refuses to open (its file gone, or a password that no
  longer opens it) is recorded *missing*, keeps its sealed seed, its free accounts are retired and the administrators
  are told; it is tried on every pass. "Invalid password" alone never wipes a seal: a wallet restored from its seed with
  a mistyped secret, another install's file of the same name and a damaged keys file are refused alike, so only a
  destruction the market itself began (above) ends destroyed without an administrator. A database restored from a
  backup taken after that start keeps it, so such a wallet restored from its seed with a mistyped secret is recorded
  destroyed too once the gate and the sweep marker clear: the operator restores it only with the password derived from
  the unchanged secret and checks in a scratch wallet restored from the seed that it holds nothing before deleting the
  files listed on `/admin/payments`. A wallet another wallet program
  has open is temporarily unavailable: it keeps its state and its accounts, waits, and is logged once until it is closed
  there. A missing wallet past its 90 days is listed on `/admin/payments`, where an administrator confirms its destruction
  (password step-up, audited) once the operator has checked that the file is gone and nothing is owed. A wallet that
  would not open while the sweep ran counts as checked, so after its file is restored the operator sets the sweep marker
  again for its lookahead. A changed `MONERO_WALLET_SECRET` disables Monero escrow with one fixed log line and no wallet
  is created or opened; a changed `ESCROW_XMR_SEED_KEY` stops wallet creation only, until the operator re-pins it.

**What Phase 3 protects, on a local fakechain.** Every transaction out of an order's account names that account, and the
transaction the wallet builds is checked to spend exactly the outputs claimed to exactly the planned destinations, so one
order's coins are not paid out in another's settlement. The database refuses a second outcome per order, a stored
transaction that changes, a transaction stored before the order's state fits the settlement (migration 081's rule, carried
into 084), and any deleted escrow record. A settlement is stored before it is relayed and is relayed again only as the
same stored transaction; it is never rebuilt by the market on its own, and the administrator's re-request is allowed only
when the stored transaction is in neither the daemon's pool nor the chain and none of its coins is spent, builds exactly
the stored coins to the stored plan, and supersedes the old row in the same database transaction, so at most one of the two
transactions can be mined. Seeds are sealed to a key the application only holds the public half of, and no wallet
password is kept anywhere. The refund address is fixed at checkout. An address whose keys are not in ed25519's prime-order
subgroup, which the wallet's own check accepts, is refused offline, because the wallet-rpc used here (0.18.5.1) crashed
when asked to build a transfer to one. After a restore, no restored
wallet takes an order again and no settlement is relayed until an administrator has checked it.

**What Phase 3 does not protect.**

- **It is custodial: the market can move these coins.** The server holds every escrow wallet's keys, opened with a
  password it derives itself. The operator, and anyone who takes over the server or copies the wallet volume and
  `MONERO_WALLET_SECRET`, can spend every escrow's coins. Accounts keep orders apart and remove a shared balance; they do
  not stop the operator. There is no Monero multisig and none is claimed.
- **It was built and tested on a fakechain.** Phase 3 refused every daemon but a local fakechain; A-449 admits stagenet
  ([below](#monero-escrow-on-stagenet-a-449)), checked on an offline fork and in one public digital claim
  ([record](evidence/public-testnet/2026-10-10-ac71878dfde4/stagenet-xmr/README.md)). That sample verifies actual funding, fees and claim confirmations; public refunds,
  physical deadlines, fee/ring-member stress and reorganisations remain unverified. Testnet is refused; mainnet (A-460) is the installer's default for local nodes (A-523), with no recorded run.
- **Backups keep wallet copies until they expire.** A database dump holds each wallet's sealed seed (anyone with the
  OpenPGP private key can open it), and a backup of the escrow wallet volume holds its files. Destroying a wallet reaches
  neither: the dumps and volume copies taken before it stay as they are, and the operator decides how long they are kept.
- **Destroyed wallet files must be deleted by the operator.** The market cannot reach the wallet volume. It only makes a
  wallet unopenable (a password nobody keeps) and wipes its sealed seed from the database; the files stay on the volume
  until the operator deletes them.
- **A wallet created after the last backup can be lost (A-403).** If the database and the escrow wallet volume are both
  lost before the next backup, a wallet created in between is lost with the orders it took. Losing only one of them keeps
  the coins recoverable: the sealed seed is in the database and the files are in the volume (the restore sweep lists the
  funded accounts of a wallet the restored database lacks), although a restored database has lost the orders recorded after
  its backup, as with any restore. The 7-day lead means a wallet takes no order
  before a daily backup holds its seal, but only for a wallet created ahead of its month; a first run, or a wallet created
  after the market was down past a month's start, takes orders at once. This assumes a database backup at least daily
  ([operator guide](operator-guide.md#monero-escrow)).
- **Monero dispute decisions are final.** A wrong decision cannot be undone from the market.
- **A buyer who neither confirms receipt nor disputes before the deadline loses the choice**, as described above
  (ES-11), and an administrator decides disputes from what the parties write; it does not check that goods exist or
  arrived. The release deadline length is one setting for both coins: its form on `/admin/payments`, like a physical listing's
  dispatch window, is offered while `ESCROW_XMR` is on, and each escrow keeps the length in force at
  its checkout.
- **The market fee is set only by `ESCROW_XMR_FEE_BPS` and `ESCROW_XMR_FEE_ADDRESS`** on the app host (no form or settings row
  on any `/admin` page, so an administrator web account cannot change or redirect it; A-550); each escrow keeps the fee in
  force at its checkout. *Admin → Payments* shows the rate and full address read-only, and each start that finds them
  changed from the last values it recorded writes one audit row and notifies every administrator once
  ([operator guide](operator-guide.md#monero-escrow), *Market fee*).
- **One wallet is open at a time, and every wallet that may hold coins is opened on every pass**, until the wallet is
  destroyed. PAYE-E2 measured this on one host, on a local fakechain only ([verification](verification.md#monero-escrow-wallet-scale-paye-e2-war-room-a-429-p3-r12--2026-10-04)):
  a pass over 5 wallets of 10,000 accounts each took 27.6 s against the 10-minute pass timeout, but a catch-up refresh
  over 110 blocks holding 200 deposits took 2.5 minutes, past the 2-minute bound it then had. Refresh now has its own
  8-minute bound, a refresh that times out is finished by the wallet-rpc and stored by the next `open_wallet`, and each
  new wallet looks one address ahead per account instead of 200: with that, the same catch-up took 1.0 s and
  `create_account` 7.5 ms instead of 390 ms (median). A month with far more orders, a slower host or a public network has
  not been measured.
- **A wallet-rpc that crashes building a settlement** (0.18.5.1 does on some destinations) is handled but not cured: a
  settlement whose build drops the connection twice is set aside, staff are told, and an administrator must ask for the
  build again; other settlements are not held up.
- **The orphan sweep is bounded.** It probes wallet names month by month, from 12 months before the current month
  through next month, starting after each month's newest wallet row, and stops each month at its first name with no
  file, so a wallet of an earlier month is not found; an orphan's restore height is unknown (recorded as 0), so
  restoring one from its seed scans the whole chain.

**How it was tested.** Each tier proves only what it says:

- Unit and `httptest` tests, no database. The adapter's exact parameters (account index never 0 or absent) and result
  shapes (`TestXMREscrowRPCPinsParams`, `TestXMREscrowRPCResults`, `TestXMREscrowRPCErrorClasses`,
  `TestXMREscrowAccountZeroRefusedOffline`, `TestXMREscrowNetworkCheck`); every configuration refusal
  (`TestXMREscrowConfigFromEnv`, `TestXMREscrowRefusedOnLiveMarket`, `TestXMREscrowFeeAddressKeysRefuseStartup`,
  `TestXMRWalletSecretRefusals`, `TestXMRSeedKeyRefusals`); the password derivation and the seed seal
  (`TestXMRWalletPasswordDerivation`, `TestXMRSealSeedRoundTrip`); the checks on a built transaction
  (`TestCheckXMRBuild`); and offline address parsing against Monero's own vectors, RFC 8032 keys and small-order points
  (`TestXMRBase58MoneroVectors`, `TestXMRAddressExternalVectors`, `TestXMRAddressRefusals`, `TestXMRAddressKeysRefused`,
  `TestXMRPointValid`, `TestXMRPointPrimeOrder`).
- Go tests with PostgreSQL against a **fake** escrow wallet-rpc and daemon (simulated RPC, not chain evidence), for
  every state change and refusal. Schema: `TestXMREscrowMigration084`, `TestXMREscrowMigration085`, `TestXMREscrowMigration086`,
  `TestXMREscrowPayoutRefused`, `TestXMREscrowIdxNoCollision`. Wallets, slots and funding:
  `TestXMREscrowFirstRun`, `TestXMREscrowWalletRotation`, `TestXMREscrowSlotClaimRace`, `TestXMREscrowNoRPCAtCheckout`,
  `TestXMREscrowAccountZeroNeverSpent`, `TestXMREscrowFundingThreshold`, `TestXMREscrowTwoAccountsOneTx`,
  `TestXMREscrowSecretMismatch`, `TestXMREscrowSeedKeyRepinRefused`, `TestXMREscrowTopUpAddressChecked`. Settlements:
  `TestXMREscrowStoredBeforeRelay`, `TestXMREscrowNeverRebuilt`, `TestXMREscrowAmbiguousRelay`,
  `TestXMREscrowFreezeExcludesTopUp`, `TestXMREscrowPrimaryAddressMismatch`, `TestXMREscrowKeyImagesExact`,
  `TestXMREscrowFeeCapAndFloor`, `TestXMREscrowReorgRelaysAgain`, `TestXMREscrowExternalSpend`,
  `TestXMREscrowReRequestGuards`, `TestXMREscrowReRequestWaitsForGate`, `TestXMREscrowRelayRefusalRetried`,
  `TestXMREscrowBuildSetAside`, `TestXMREscrowGatePauses`, `TestXMREscrowConflictedDepositThroughExpiry`,
  `TestXMREscrowStraysPerWallet`, `TestXMREscrowLeftOutDepositLine`, `TestXMREscrowStrayTooSmallOnce`,
  `TestXMREscrowCreditedRegressionHoldsRelease`, `TestXMREscrowMissingDepositHoldsRelease`,
  `TestXMREscrowDoubleSpentDepositBoundedWait`, `TestXMREscrowRefundWithoutMissingDepositReturnsIt`,
  `TestXMREscrowMissingDepositWaitRestartsWhenListed`. Releases, deadlines and
  disputes: `TestXMREscrowNoBuildBeforeOutcome`, `TestXMREscrowConfirmReceiptStepUp`, `TestXMREscrowShipStepUp`,
  `TestXMREscrowDeadlineWithBTCOff`, `TestXMREscrowDisputeSupersedes`, `TestXMREscrowNoShipCancel`,
  `TestXMREscrowRestoreExtendsDeadline`. Retention and restore: `TestXMREscrowDestroyPreconditions`,
  `TestXMREscrowRestoredPastDestroy`, `TestXMREscrowUnopenableKeepsSeal`, `TestXMREscrowConfirmDestroyed`,
  `TestXMREscrowRestoreRetiresSlots`, `TestXMREscrowRestoreNoUnrecordedSlot`,
  `TestXMREscrowRowlessAccountRetiresFreeSlots`, `TestXMREscrowRowlessAccountNormalCrashBurnsBatch`,
  `TestXMREscrowRecordedAccountsKeepFreeSlots`, `TestXMREscrowRestoredCheckoutWaitsForReconcile`,
  `TestXMREscrowCheckoutWaitsForFirstPass`, `TestXMREscrowRetiredAccountDepositAlerts`, `TestXMREscrowOrphanSwept`, `TestXMREscrowLookaheadForOlderWallet`, `TestXMREscrowHeldSettlementConfirm`.
  `scripts/test-backup-restore.sh` (run in CI) restores a real dump through `scripts/restore.sh` with Monero escrow rows
  and checks the holds, the retired slots, the closed wallets and the sweep marker.
- Real `monerod` and `monero-wallet-rpc` 0.18.5.1 on a local fakechain, run by the `escrow-xmr-regtest` CI job (A-498) or by hand
  (`scripts/test-escrow-xmr-regtest.sh`, [runbook](testnet-runbook.md#9-manual-monero-escrow-fakechain-check); the app
  runs inside the Go test against the real daemon and wallet-rpc, and the script refuses any daemon that is not a
  fakechain). Sixteen legs, each a Go test:
  1. `TestXMREscrowRegtestWallets`: the first pass creates the month's wallet, seals its seed (the seal decrypts, and a
     wallet restored from it at the stored restore height has the stored primary address), activates it and keeps its free
     accounts; account 0 holds nothing; the next month's wallet is created sealed ahead of its month and becomes active at
     its start.
  2. `TestXMREscrowRegtestFunding`: a deposit is recorded at 0 confirmations in the pool, counts at 10 and not at 9, and
     completes a digital order.
  3. `TestXMREscrowRegtestTwoAccountsOneTx`: one transaction paying two escrows becomes one payments row per account, each
     its own order's.
  4. `TestXMREscrowRegtestPhysical`: four physical orders funded by one transaction; the watcher makes no `transfer` or
     `relay_tx` before an outcome; then Confirm receipt, the deadline, a dispute for the buyer and a no-ship cancel each
     have their settlement relayed and confirmed to the right address.
  5. `TestXMREscrowRegtestRestartMidSend`: relay's answer is lost and the app and wallet-rpc restart; the same stored
     transaction is relayed, with one `transfer` call in all.
  6. `TestXMREscrowRegtestRelayDropped`: a lost relay answer; the next pass finds the transaction in the pool and records
     it relayed, with no second `relay_tx` or `transfer`.
  7. `TestXMREscrowRegtestTopUp`: a top-up still confirming at the build is left out and returned by a stray refund.
  8. `TestXMREscrowRegtestReorg`: a confirmed release is popped from the chain, goes back to relayed, is relayed again
     and confirms again.
  9. `TestXMREscrowRegtestExternal`: a wallet restored from the sealed seed sweeps the account outside the market while
     the release is built; the daemon refuses the market's transaction and it is recorded external and flagged.
  10. `TestXMREscrowRegtestReRequest`: monerod is stopped around each relay of a release, so the wallet-rpc cannot commit
      it (-4) while it is stored and its coins unspent; the same stored transaction is relayed again on the next passes
      with no second `transfer`, and the third refusal records it failed; the wallet still lists its outputs as
      available; an administrator's re-request spends exactly the stored key images (the daemon's decoded inputs) to the
      stored plan, and it confirms.
  11. `TestXMREscrowRegtestRestore`: a restore (with `scripts/restore.sh`'s own Monero SQL) holds a built release, retires
      the free accounts and closes the wallet; the sweep finds a wallet created after the backup and seals it as an
      orphan; after an administrator's confirmation the held release is relayed once and confirms; the next checkout gets
      an account of a new wallet.
  12. `TestXMREscrowRegtestLookahead`: a deposit to account 300, unknown to the wallet, is missed with the default
      lookahead and seen after the sweep raises it and rescans.
  13. `TestXMREscrowRegtestWrongSecret`: with a changed `MONERO_WALLET_SECRET`, no wallet is created or opened and checkout
      answers 503.
  14. `TestXMREscrowRegtestDestroy`: a deposit on day 89 to a closed order's escrow is refunded; on day 91 the retained
      wallet is destroyed and `open_wallet` with the derived password is refused.
  15. `TestXMREscrowRegtestRefundBelowFee` (A-467): refunds of 0.001, 0.0015 and 0.0018 XMR decided for the buyer are
      refused by the wallet (-17, -17, -16: the coins are at or below the network fee); each ends too few on the
      market's fee estimate, its deposit is left out, and the one stray refund returns it with a later deposit, relayed
      and confirmed; no note says the outputs mismatched or the wallet refused.
  16. `TestXMREscrowRegtestFundingReorg` (A-554): an order funded by two transactions in separate blocks has its release
      requested, then `pop_blocks` takes out the block of the shallower funding deposit. Kept in the pool and mined
      again, that deposit holds the release (no `transfer`) until it has 10 confirmations; the release then spends both
      deposits. Flushed from the pool, it holds a second order's release for 20 blocks, after which the release is built
      from the other deposit alone with a note naming the missing one; no deposit is left out and no stray refund is
      requested. Added with A-554; its evidence is the `escrow-xmr-regtest` CI job.

  Every settlement the daemon holds is decoded (RingCT type 6, 16 ring members per input, a fee above zero, the stored one,
  within `ESCROW_XMR_MAX_FEE`). Each leg puts the watcher's clock in a month of its own, so the legs may run again against
  the same nodes; no leg waits in real time for a deadline or a month.
- A browser journey in Chromium at 320 px with JavaScript off against the real server and a **simulated** Monero escrow
  wallet-rpc and daemon is added in this phase (`tests/e2e/escrow-xmr-journey.spec.ts`), its last test covering a wallet
  destroyed after its retention (the order page's ES-8 line) and a missing wallet confirmed destroyed on `/admin/payments`; its
  command and result are not recorded here. A simulation is not chain evidence.
- PAYE-E2 (`scripts/test-escrow-xmr-payee2.sh`, manual) on a real `monerod` and `monero-wallet-rpc` 0.18.5.1 local
  fakechain, one host: one wallet of 10,000 accounts with 200 funded, its calls timed through the app's adapter, a seed
  restore that finds every funded account, and a watcher pass over 5 such wallets within the pass timeout. The numbers
  and their limits are in [verification](verification.md#monero-escrow-wallet-scale-paye-e2-war-room-a-429-p3-r12--2026-10-04).

The [2026-10-10 public stagenet digital-claim record](evidence/public-testnet/2026-10-10-ac71878dfde4/stagenet-xmr/README.md) covers one funded digital claim. Other public stagenet
legs remain untested; A-449's offline fork cannot run them. Not tested: testnet and mainnet, Monero multisig, the escrow pages in Firefox or WebKit, deadlines of 7 to 30
days in real time, a wallet with thousands of accounts on a public network (PAYE-E2 measured one on a local fakechain), a restore of a real backup into a
running market with Phase 3 orders on a real chain (`scripts/test-backup-restore.sh` restores a real dump and checks its
rows; the fakechain `Restore` leg simulates the database restore in SQL), and a deployment of any of it.


## Monero escrow on stagenet (A-449)

Phase 4 work (war-room ruling P4-R15, owner decision 2026-10-05), first released in `v0.1.0-alpha.2`. Monero escrow
(still off by default, still custodial) accepts stagenet as well as a local fakechain. The [2026-10-10 public stagenet digital-claim record](evidence/public-testnet/2026-10-10-ac71878dfde4/stagenet-xmr/README.md) passed with real funding and a confirmed vendor release.
The broader network/failure checks below retain their simulated-RPC or offline-fork evidence tiers.

The staff alert for a requested Monero settlement older than 24 hours is a visibility threshold, not a measured settlement
or refund-time bound. The public digital claim's request-to-confirmed sample was 27m44s; public refund waits remain unmeasured, and one sample does not validate the alert threshold.

- **Network check and address tags.** The escrow daemon must report nettype `fakechain` (no network flag set) or
  `stagenet` (the stagenet flag set), and a hard fork version of at least 16; testnet and inconsistent flags are
  refused, and so is mainnet unless the market is a mainnet market (`MONERO_NETWORK=mainnet`, A-460,
  [below](#mainnet-escrow-a-460)), which then requires it. A daemon still syncing (synchronized false, or a target height above its height) is not refused for a lower
  hard fork, which it reports for the height it has reached: Monero escrow stays disabled and is checked again every
  pass, so a fresh stagenet node delays Monero escrow, not the market's startup. The address forms Monero escrow accepts follow the network it verified, never a configured one, and the two
  changed together: on stagenet the standard address (tag 24, `5…`) and subaddress (36, `7…`), on a fakechain the mainnet
  format it uses (18, `4…`; 42, `8…`); integrated addresses (19, 25) are refused as such, and every other tag as another
  network's. This holds for refund and claim addresses, `ESCROW_XMR_FEE_ADDRESS` (a fee address of the other network stops
  startup) and every address the escrow wallet-rpc returns (a stagenet wallet's reply is refused on a fakechain escrow and
  the reverse). Before the network is verified no address parses; a vendor's claim while the daemon is unreachable uses the
  network pin.
- **Network pin, replacing the height heuristic.** Phase 3 told a fakechain from a public network by the daemon's and the
  wallet's height (below 1,000,000). That is gone: the database pins the escrow daemon's network (`settings` key
  `escrow_xmr_network`). While the database holds no Monero escrow and no escrow wallet (a wallet's free accounts are
  addresses of one network) the pin follows the daemon. Once it holds either, a daemon on the other network leaves Monero
  escrow disabled, writes nothing, logs one fixed line and shows both networks under *Escrow status and payment intake* on `/admin/payments`; it
  is retried every pass. A database from before the pin that holds Monero escrow state is pinned `fakechain` when the
  daemon reports fakechain, and refused otherwise. The wallet-rpc must still be within 10 blocks of the daemon.
- **Public pages name the verified network (A-472).** The transparency page (`/canary`), the footer, catalog, listing and
  draft notices and the vendor desk tile name the network by the rule `/admin/payments` uses: "network not verified yet" until a
  daemon was verified or the pin was read, "Monero stagenet, a public test network" on stagenet, "a local Monero
  fakechain" on a fakechain. Public copy follows the configured Monero network and preserves the custody warning. `TestCanaryMoneroCopyFollowsConfiguredNetwork` checks stagenet and fakechain wording; browser previews check the XMR-only forms with JavaScript disabled. These are layout/simulated-RPC checks, not public-network evidence.

- **Daemon sync guard.** The Monero sync rule (the check before it read only `target_height`: A-439 found the hole): a daemon is not synced while `get_info` reports `synchronized` false or
  a `target_height` above its height, or, on a public network, it has no peers or its top block (`get_last_block_header`,
  the one RPC the app did not call before; get_info carries no block time) is older than `MONERO_MAX_TIP_AGE` (default 1h,
  20m to 24h). "No peers" is `offline`, or no connections on an unrestricted RPC: monerod 0.18.5.1 reports both
  connection counts as 0 on a restricted RPC (`--restricted-rpc`, as the bundled `monero` service and most remote nodes
  serve) whatever its peers, so there the counts are not read and the tip's age covers a daemon cut off from the network.
  While the escrow daemon is not synced the Monero escrow pass opens no wallet, reads no deposit, funds or expires no order
  and builds, relays or follows no settlement (deadlines still apply).
  An order held open past its payment window that way (no deposit waiting for confirmations) says on its page that the
  window ended and the order stays open while the market waits for its Monero node, not that it will be cancelled at a
  time already past (QA4W1-5; `TestXMREscrowWindowPassedWhileNotSynced`).
  Stateless; one log line per change and a line under *Escrow status and payment intake*. A fakechain is exempt from the peer and tip-age
  checks and `get_last_block_header` is not called on it.
- **Compose service (INFRA-11).** `compose.nodes.yaml` has a `monero-escrow-wallet` service: the escrow wallet-rpc on
  stagenet, sharing the bundled `monero` daemon, in its own profile and volume with its own login password; the installer's
  `--monero-escrow yes` (or its question beside the Monero questions) generates that password and `MONERO_WALLET_SECRET`,
  asks for the seed key file and mounts it through `compose.escrow-xmr.yaml`
  ([operator guide](operator-guide.md#monero-escrow)). Checked with `docker compose config` and the
  installer's tests (a stub Docker); the Compose service has not been started against a public stagenet daemon. The recorded public smoke used separately launched Docker containers, so it does not verify this deployment path.

How it was tested. Simulated RPC and PostgreSQL, not chain evidence: `TestXMRAddressTagsPerNettype`,
`TestXMRAddressStagenetVectors` (real stagenet addresses from a disposable wallet and Monero's own test table),
`TestMoneroNodeNotSynced`, `TestParseMoneroMaxTipAge` (unit); `TestXMREscrowStagenetTags` (a stagenet escrow's wallets,
checkout, claim, notes and pages), `TestXMREscrowFakechainTags`, `TestXMREscrowFeeAddressFollowsNetwork`,
`TestXMREscrowNetworkPinMismatch`, `TestXMREscrowNetworkPinBackfillAndRepin`, `TestXMREscrowNoHeightHeuristic`,
`TestXMREscrowSkipsWhileNotSynced` (each condition, restricted RPC included: no wallet opened, no expiry, no funding, no
build, then the work on the next synced pass at a lower height), `TestXMREscrowFakechainExemptFromPeerAndTipChecks`. Real chains (in CI since A-498): the Phase 3 legs
again on a fakechain (`scripts/test-escrow-xmr-regtest.sh`), and on an **offline stagenet fork, not the public stagenet**
(`scripts/test-escrow-xmr-stagenet.sh`: stagenet daemons built from the genesis block with the local monerod 0.18.5.1
image, peered only with each other on a Docker network with no route out, local mining at fixed difficulty) the legs such
a chain can run. Built from genesis it is at hard fork version 1 (version 16 starts at height 1,151,720), so Monero escrow
refuses to start on it, as it must, and none of the sixteen legs can run there; the stagenet legs show that refusal names
the hard fork, not the network; that a real stagenet wallet's primary address, subaddress and integrated address are read
under the stagenet tags (the integrated one refused) and refused under the fakechain's, by the parser and by the adapter;
and that the escrow sync guard reads a restricted stagenet daemon with a peer and a fresh tip as synced and an
`--offline` daemon as having no peers. No funded escrow leg can run on that fork (hard fork 1, and `generateblocks`, which
the fakechain legs mine with, works only on a fakechain), so **funded Monero escrow on stagenet is proven only by the
recorded public stagenet digital-claim run (A-447/A-491)** ([evidence](evidence/public-testnet/2026-10-10-ac71878dfde4/stagenet-xmr/README.md)); the offline fork proves the stagenet address formats, the old-fork
refusal and the sync guards, and the network pin rests on the simulated-daemon database tests above. Not tested: the
other public stagenet workflows, fee stress, ring-member stress and reorganisations, a daemon that loses its
peers on a public network, and `ESCROW_XMR_MAX_FEE` against stagenet fees. On a restricted RPC a daemon cut off from its
peers looks synced until its tip is older than `MONERO_MAX_TIP_AGE` (QA4W3-5, accepted and documented in the
[operator guide](operator-guide.md#payments): a deposit made in that window is not seen, its order can be cancelled as
unpaid, and the deposit is then returned by a stray refund). Since QA4W3-3 an unreadable tip is tested as not synced
(`TestXMREscrowSkipsWhileTipUnreadable`).


## Mainnet escrow (A-460)

Mainnet requires `MONERO_NETWORK=mainnet`, matching daemon and wallet-rpc configuration, a fresh database and mainnet addresses. Escrow remains custodial with real funds. No mainnet run is recorded; mainnet evidence is unit, database and simulated-RPC only. Development, tests and CI never use mainnet.
