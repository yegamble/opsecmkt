package market

import (
	"context"
	"maps"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Escrow Phase 3, slice X-c: the real-chain legs of Monero escrow (scripts/test-escrow-xmr-regtest.sh; not run in CI).
// They run a real App and its Monero escrow watcher against monerod and monero-wallet-rpc 0.18.5.1 on a local fakechain
// (monerod --regtest): the App's own escrow wallet-rpc (ESCROW_XMR_REGTEST_WALLET_URL), a funder wallet-rpc standing in
// for the buyer's own wallet (ESCROW_XMR_REGTEST_FUNDER_URL) and a wallet-rpc used only to restore a sealed seed
// (ESCROW_XMR_REGTEST_RESTORE_URL). Each leg refuses unless the daemon reports nettype fakechain with hard fork 16 or
// later, before any wallet call. Each leg puts the watcher's clock in a month of its own (far in the future), so its
// wallet names never meet another leg's or run's files on the shared wallet-rpc. No seed or password is ever logged.

// xmrRegtest is one leg's view of the local fakechain.
type xmrRegtest struct {
	*xmrEscrowEnv
	daemon, funder, restore *rpcClient
	funderAddr              string
}

func (r *xmrRegtest) call(c *rpcClient, method string, params, out any) {
	r.t.Helper()
	if err := c.call(context.Background(), "/json_rpc", method, params, out); err != nil {
		r.t.Fatalf("%s: %s", method, watcherCause(err))
	}
}

// newXMRRegtest checks the daemon, makes and funds the funder's wallet, and starts an App with ESCROW_XMR on against the
// escrow wallet-rpc, its clock in a random month of the years 2200 to 2999.
func newXMRRegtest(t *testing.T) *xmrRegtest {
	t.Helper()
	daemonURL, walletURL := os.Getenv("ESCROW_XMR_REGTEST_DAEMON_URL"), os.Getenv("ESCROW_XMR_REGTEST_WALLET_URL")
	funderURL, restoreURL := os.Getenv("ESCROW_XMR_REGTEST_FUNDER_URL"), os.Getenv("ESCROW_XMR_REGTEST_RESTORE_URL")
	if daemonURL == "" || walletURL == "" || funderURL == "" || restoreURL == "" {
		t.Skip("set ESCROW_XMR_REGTEST_DAEMON_URL, _WALLET_URL, _FUNDER_URL and _RESTORE_URL (scripts/test-escrow-xmr-regtest.sh) to run against monerod --regtest")
	}
	r := &xmrRegtest{xmrEscrowEnv: &xmrEscrowEnv{}}
	var err error
	for _, c := range []struct {
		dst      **rpcClient
		name, at string
	}{{&r.daemon, "fakechain daemon", daemonURL}, {&r.funder, "funder wallet-rpc", funderURL}, {&r.restore, "restore wallet-rpc", restoreURL}} {
		if *c.dst, err = newRPCClient(c.name, c.at, "2.0", true); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	var info struct {
		NetType string `json:"nettype"`
		Height  int64  `json:"height"`
	}
	var hf struct {
		Version int64 `json:"version"`
	}
	if err = r.daemon.call(ctx, "/json_rpc", "get_info", nil, &info); err != nil || info.NetType != "fakechain" {
		t.Fatalf("refusing: the daemon is not on a fakechain (or is unreachable): %q", info.NetType)
	}
	if err = r.daemon.call(ctx, "/json_rpc", "hard_fork_info", nil, &hf); err != nil || hf.Version < 16 {
		t.Fatalf("refusing: the daemon's hard fork version %d is below 16", hf.Version)
	}
	r.testEnv = &testEnv{t: t} // until the App starts below
	funder := "funder-" + randomToken()[:10]
	r.call(r.funder, "create_wallet", map[string]any{"filename": funder, "password": "", "language": "English"}, nil)
	var addr struct {
		Address string `json:"address"`
	}
	r.call(r.funder, "get_address", map[string]any{"account_index": 0}, &addr)
	r.funderAddr = addr.Address
	r.mine(100) // coinbase unlocks after 60 blocks; 100 also give ring 16 its decoys
	r.call(r.funder, "refresh", nil, nil)
	t.Logf("fakechain at height %d (hard fork %d); funder wallet mined 100 blocks", info.Height+100, hf.Version)

	key, pub := testSeedKey(t, nil)
	r.seedKey, r.keyPath = key, writeSeedKey(t, pub)
	for k, v := range map[string]string{"ESCROW_XMR": "on", "ESCROW_XMR_WALLET_RPC_URL": walletURL, "ESCROW_XMR_DAEMON_URL": daemonURL,
		"MONERO_WALLET_SECRET": testXMRSecret, "ESCROW_XMR_SEED_KEY": r.keyPath, "ESCROW_XMR_FEE_BPS": "", "ESCROW_XMR_FEE_ADDRESS": "",
		"ESCROW_XMR_SLOTS": "5", "ESCROW_XMR_MAX_FEE": "", "PAYMENT_CONFIRMATIONS_XMR": "", "MONERO_WALLET_RPC_URL": "", "MONERO_NETWORK": "",
		"ESCROW_BTC": "", "BITCOIN_RPC_URL": "", "BITCOIN_CHAIN": ""} {
		t.Setenv(k, v)
	}
	r.testEnv = newTestApp(t)
	if !r.A.xmrEscrowReady() {
		t.Fatalf("Monero escrow did not start against the fakechain: %s", r.A.escrowXMR.startState)
	}
	r.now = time.Date(2200+rand.IntN(800), time.Month(1+rand.IntN(12)), 15, 12, 0, 0, 0, time.UTC)
	r.clock()
	r.buyer, r.buyerSess = r.user("xr_buyer_"+randomToken()[:6], "buyer")
	r.vendor, r.vendorSess = r.user("xr_vendor_"+randomToken()[:6], "vendor")
	r.admin, _ = r.user("xr_admin_"+randomToken()[:6], "admin")
	r.product = r.digital(r.vendor)
	return r
}

// mine adds n blocks, their coinbase to the funder.
func (r *xmrRegtest) mine(n int) {
	r.t.Helper()
	r.call(r.daemon, "generateblocks", map[string]any{"amount_of_blocks": n, "wallet_address": r.funderAddr}, nil)
}

// pay sends one transaction from the funder paying each address its amount (piconero) and returns its hash.
func (r *xmrRegtest) pay(amounts map[string]int64) string {
	r.t.Helper()
	r.call(r.funder, "refresh", nil, nil)
	var dests []map[string]any
	for _, addr := range slices.Sorted(maps.Keys(amounts)) {
		dests = append(dests, map[string]any{"address": addr, "amount": amounts[addr]})
	}
	var res struct {
		TxHash string `json:"tx_hash"`
	}
	r.call(r.funder, "transfer", map[string]any{"destinations": dests, "account_index": 0, "priority": 0, "ring_size": 16}, &res)
	return res.TxHash
}

// walletName is the leg's month's first wallet name, or its next month's.
func (r *xmrRegtest) walletName(next bool) string {
	m := monthStart(r.now)
	if next {
		m = m.AddDate(0, 1, 0)
	}
	return "escrow-" + m.Format("200601") + "-01"
}

// TestXMREscrowRegtestWallets: the first pass creates this month's escrow wallet on the real wallet-rpc, seals its
// seed (which decrypts with the test's private seed key and restores, with restore_deterministic_wallet from the
// stored height, to the stored primary address), activates it and keeps 5 free slots, each a real account whose address
// 0 is the slot's; account 0 is no slot and holds nothing. The clock then reaches 6 days before the next month (its
// wallet is created sealed, not active) and the next month's start (it is active, the first closing with its free
// slots retired).
func TestXMREscrowRegtestWallets(t *testing.T) {
	r := newXMRRegtest(t)
	r.mustPoll()
	first := r.walletName(false)
	if got := r.wallets(); !slices.Equal(got, []string{first + ":active"}) {
		t.Fatalf("after the first pass: %v", got)
	}
	var primary string
	var height int64
	if err := r.DB.QueryRow("SELECT primary_address,restore_height FROM xmr_escrow_wallets WHERE name=$1", first).Scan(&primary, &height); err != nil {
		t.Fatal(err)
	}
	seed := r.unseal(first)
	if len(strings.Fields(seed)) != 25 {
		t.Fatalf("the sealed seed has %d words", len(strings.Fields(seed)))
	}
	var restored struct {
		Address string `json:"address"`
	}
	r.call(r.restore, "restore_deterministic_wallet", map[string]any{"filename": "restored-" + randomToken()[:10], "password": "", "seed": seed,
		"restore_height": height, "language": "English", "autosave_current": false}, &restored)
	if restored.Address != primary {
		t.Fatalf("the restored seed gives %s, the wallet row has %s", restored.Address, primary)
	}
	t.Logf("wallet %s: seed sealed to key %s, decrypted with its private key and restored from height %d to the stored primary address %s…", first, r.A.escrowXMR.seedKey.fpr, height, primary[:12])

	ctx := context.Background()
	x := r.A.escrowXMR
	slots := agStrings(t, r.testEnv, "SELECT s.account_index||' '||s.address FROM xmr_escrow_slots s JOIN xmr_escrow_wallets w ON w.id=s.wallet_id WHERE w.name=$1 AND s.state='free' ORDER BY s.account_index", first)
	if len(slots) != 5 {
		t.Fatalf("free slots: %v", slots)
	}
	for _, s := range slots {
		account, _ := strconv.ParseInt(strings.Fields(s)[0], 10, 64)
		if account < 1 {
			t.Fatalf("slot at account %d", account)
		}
		got, err := x.rpc.address(ctx, account, 0)
		if err != nil || got != strings.Fields(s)[1] {
			t.Fatalf("account %d: the wallet's address 0 is %q (%v), the slot's %s", account, got, err, strings.Fields(s)[1])
		}
	}
	accounts, err := x.rpc.accounts(ctx)
	if err != nil || len(accounts) != 6 || accounts[0].Index != 0 || accounts[0].Balance != 0 || accounts[0].BaseAddress != primary {
		t.Fatalf("accounts %+v (%v)", accounts, err)
	}
	t.Logf("wallet %s: accounts 1 to 5 are the 5 free slots (get_address agrees); account 0 is the primary address and holds 0", first)

	next := r.walletName(true)
	r.now = monthStart(r.now).AddDate(0, 1, -6)
	r.mustPoll()
	if got := r.wallets(); !slices.Equal(got, []string{first + ":active", next + ":sealed"}) {
		t.Fatalf("6 days before the next month: %v", got)
	}
	r.now = monthStart(r.now).AddDate(0, 1, 0)
	r.mustPoll()
	if got := r.wallets(); !slices.Equal(got, []string{first + ":closing", next + ":active"}) || r.slots(first, "free") != 0 || r.slots(first, "retired") != 5 || r.slots(next, "free") != 5 {
		t.Fatalf("at the next month's start: %v", got)
	}
	if r.unseal(next) == seed {
		t.Fatal("two wallets share a seed")
	}
	t.Logf("rotation: %s created sealed 6 days ahead, active at its month's start; %s closing with its 5 free slots retired", next, first)
}

// TestXMREscrowRegtestFunding: a buyer takes a Monero escrow through the real checkout form (no RPC) and the funder pays
// its price in one transaction. In the pool it is recorded at 0 confirmations; after 9 blocks it is recorded at 9 and
// nothing counts; after the 10th the digital order completes on funding (content delivered, no payout).
func TestXMREscrowRegtestFunding(t *testing.T) {
	r := newXMRRegtest(t)
	r.mustPoll()
	order := r.draft(r.product)
	addr := r.mustCheckout(order)
	tx := r.pay(map[string]int64{addr: xmrListingPrice})
	t.Logf("order %s: escrow address %s…, funding transaction %s", shortID(order), addr[:12], tx)
	confs := func() string {
		return strings.Join(agStrings(t, r.testEnv, "SELECT confirmations||' '||credited FROM payments WHERE order_id=$1", order), ",")
	}
	r.mustPoll()
	if got := confs(); got != "0 false" || r.state(order) != stateAwaitingPayment {
		t.Fatalf("in the pool: %q %s", got, r.state(order))
	}
	r.mine(9)
	r.mustPoll()
	if got := confs(); got != "9 false" || r.state(order) != stateAwaitingPayment {
		t.Fatalf("after 9 blocks: %q %s", got, r.state(order))
	}
	t.Log("after 9 blocks: recorded at 9 confirmations, not counted, order awaiting payment")
	r.mine(1)
	r.mustPoll()
	if got := confs(); got != "10 true" || r.state(order) != stateCompleted || agInt(r.testEnv, "SELECT count(*) FROM deliveries WHERE order_id=$1", order) != 1 ||
		agInt(r.testEnv, "SELECT count(*) FROM payouts WHERE order_id=$1", order) != 0 {
		t.Fatalf("after 10 blocks: %q %s", got, r.state(order))
	}
	t.Log("after 10 blocks: credited, the digital order completed on funding, content delivered, no payout")
}

// TestXMREscrowRegtestTwoAccountsOneTx: one transaction pays two escrows of the same wallet; after 10 blocks there are
// two payments rows of that transaction, one per account (idx 2^62 + wallet·2^32 + account), each its own order's, and
// both orders complete.
func TestXMREscrowRegtestTwoAccountsOneTx(t *testing.T) {
	r := newXMRRegtest(t)
	r.mustPoll()
	a, addrA := r.freshCheckout(r.digital(r.vendor))
	b, addrB := r.freshCheckout(r.digital(r.vendor))
	tx := r.pay(map[string]int64{addrA: xmrListingPrice, addrB: xmrListingPrice})
	r.mine(10)
	r.mustPoll()
	wallet := int64(agInt(r.testEnv, "SELECT id FROM xmr_escrow_wallets WHERE name=$1", r.walletName(false)))
	account := func(order string) int64 {
		return int64(agInt(r.testEnv, "SELECT s.account_index FROM escrows es JOIN xmr_escrow_slots s ON s.id=es.xmr_slot_id WHERE es.order_id=$1", order))
	}
	want := []string{a + " " + strconv.FormatInt(xmrPaymentIdx(wallet, account(a)), 10) + " " + strconv.FormatInt(xmrListingPrice, 10),
		b + " " + strconv.FormatInt(xmrPaymentIdx(wallet, account(b)), 10) + " " + strconv.FormatInt(xmrListingPrice, 10)}
	slices.Sort(want)
	got := agStrings(t, r.testEnv, "SELECT order_id||' '||idx||' '||amount FROM payments WHERE txid=$1 ORDER BY order_id", tx)
	if !slices.Equal(got, want) {
		t.Fatalf("payments rows of transaction %s:\n got %v\nwant %v", tx, got, want)
	}
	if r.state(a) != stateCompleted || r.state(b) != stateCompleted {
		t.Fatalf("orders %s %s", r.state(a), r.state(b))
	}
	t.Logf("transaction %s paid accounts %d and %d of wallet %d: two payments rows, orders %s and %s completed", tx, account(a), account(b), wallet, shortID(a), shortID(b))
}
