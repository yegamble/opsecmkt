package market

import (
	"html"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Escrow Phase 3, slice X-d (WR-escrow A-248, P3-R6, P3-R8, A-401): Monero escrow settlements built, stored, relayed
// and followed by the watcher, against the simulated wallet-rpc and monerod of escrow_xmr_fake_test.go. DB integration
// tests (simulated RPC, not chain evidence; scripts/test-escrow-xmr-regtest.sh has the real-chain legs).

// xmrSettlement is one Monero settlement as a test reads it.
type xmrSettlement struct {
	ID                  int64
	Kind, State, TxHash string
	Fee, InputTotal     int64
	KeyImages           []string
	Supersedes          int64
}

func (x *xmrEscrowEnv) settlement(id int64) xmrSettlement {
	x.t.Helper()
	s := xmrSettlement{ID: id}
	var keyImages string
	if err := x.DB.QueryRow(`SELECT kind,state,COALESCE(tx_hash,''),COALESCE(fee,0),COALESCE(input_total,0),COALESCE(array_to_string(key_images,','),''),COALESCE(supersedes,0)
		FROM xmr_settlements WHERE id=$1`, id).Scan(&s.Kind, &s.State, &s.TxHash, &s.Fee, &s.InputTotal, &keyImages, &s.Supersedes); err != nil {
		x.t.Fatal(err)
	}
	if keyImages != "" {
		s.KeyImages = strings.Split(keyImages, ",")
	}
	return s
}

// settlementOf is order's newest settlement of kind.
func (x *xmrEscrowEnv) settlementOf(order, kind string) xmrSettlement {
	x.t.Helper()
	ids := agStrings(x.t, x.testEnv, "SELECT id::text FROM xmr_settlements WHERE order_id=$1 AND kind=$2 ORDER BY id DESC LIMIT 1", order, kind)
	if len(ids) == 0 {
		x.t.Fatalf("order %s has no %s settlement", shortID(order), kind)
	}
	id, _ := strconv.ParseInt(ids[0], 10, 64)
	return x.settlement(id)
}

// outputsOf lists settlement id's stored outputs as role:address:amount.
func (x *xmrEscrowEnv) outputsOf(id int64) []string {
	return agStrings(x.t, x.testEnv, "SELECT role||':'||address||':'||amount FROM xmr_settlement_outputs WHERE settlement_id=$1 ORDER BY n", id)
}

// completed is a digital order funded with the price in one deposit at 10 confirmations, completed on funding, and its
// escrow address and deposit transaction.
func (x *xmrEscrowEnv) completed() (order, addr, txid string) {
	x.t.Helper()
	order, addr = x.freshCheckout(x.digital(x.vendor))
	txid = hex64("deposit " + order)
	x.fake.payAddress(addr, txid, xmrListingPrice, 10)
	x.mustPoll()
	if x.state(order) != stateCompleted {
		x.t.Fatalf("funded order: %s", x.state(order))
	}
	return order, addr, txid
}

// release requests order's release to a fresh vendor claim address, as the vendor's claim does (slice X-e), and
// returns the settlement and the address.
func (x *xmrEscrowEnv) release(order string) (int64, string) {
	x.t.Helper()
	claim := testXMRAddr("claim "+order, true)
	var id int64
	if err := x.DB.QueryRow("INSERT INTO xmr_settlements(order_id,kind,destination) VALUES($1,'release',$2) RETURNING id", order, claim).Scan(&id); err != nil {
		x.t.Fatal(err)
	}
	return id, claim
}

// waitClass is settlement id's send_error, its wait class ("" for none).
func (x *xmrEscrowEnv) waitClass(id int64) string {
	return agStrings(x.t, x.testEnv, "SELECT send_error FROM xmr_settlements WHERE id=$1", id)[0]
}

// reRequests lists settlement id's re-requests, oldest first, as state:reason:new settlement.
func (x *xmrEscrowEnv) reRequests(id int64) []string {
	return agStrings(x.t, x.testEnv, "SELECT state||':'||reason||':'||COALESCE(new_settlement_id,0) FROM xmr_settlement_rerequests WHERE settlement_id=$1 ORDER BY id", id)
}

// staffFlags counts the staff notifications that name order.
func (x *xmrEscrowEnv) staffFlags(order string) int {
	return agInt(x.testEnv, "SELECT count(*) FROM notifications n JOIN users u ON u.id=n.user_id WHERE u.role IN ('moderator','admin') AND strpos(n.body,$1)>0", shortID(order))
}

// The transaction is stored (built, with its outputs and its deposit claimed) before relay_tx is sent, and then relayed
// with the stored metadata; the release pays the plan: the vendor the price less the network fee, the market fee exact.
func TestXMREscrowStoredBeforeRelay(t *testing.T) {
	x := newXMREscrowEnv(t)
	fee := testXMRAddr("market fee", false)
	t.Setenv("ESCROW_XMR_FEE_BPS", "250")
	t.Setenv("ESCROW_XMR_FEE_ADDRESS", fee)
	x.restart()
	x.mustPoll()
	order, _, deposit := x.completed()
	id, claim := x.release(order)
	var atRelay xmrSettlement
	var outputsAtRelay []string
	var claimedAtRelay int
	x.fake.hook("relay_tx", func() {
		atRelay, outputsAtRelay = x.settlement(id), x.outputsOf(id)
		claimedAtRelay = agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND xmr_settlement_id=$2", order, id)
	})
	x.mustPoll()
	if atRelay.State != "built" || atRelay.TxHash == "" || claimedAtRelay != 1 || len(outputsAtRelay) != 2 {
		t.Fatalf("at relay_tx the database held %+v, outputs %v, %d claimed", atRelay, outputsAtRelay, claimedAtRelay)
	}
	s := x.settlement(id)
	tx, ok := x.fake.tx(s.TxHash)
	if s.State != "relayed" || !ok || tx.Height != 0 || x.fake.countOf("transfer") != 1 || x.fake.countOf("relay_tx") != 1 {
		t.Fatalf("after the pass: %+v in pool %v", s, ok)
	}
	marketFee := marketFeeSats(xmrListingPrice, 250)
	want := []string{"claimant:" + claim + ":" + strconv.FormatInt(xmrListingPrice-marketFee-s.Fee, 10), "market_fee:" + fee + ":" + strconv.FormatInt(marketFee, 10)}
	if got := x.outputsOf(id); !slices.Equal(got, want) || s.InputTotal != xmrListingPrice || s.Fee != fakeXMRFee(1, 3) {
		t.Fatalf("outputs %v (want %v), input total %d, fee %d", got, want, s.InputTotal, s.Fee)
	}
	outs := x.fake.outputs(agStrings(t, x.testEnv, "SELECT address FROM escrows WHERE order_id=$1", order)[0])
	if len(outs) != 1 || outs[0].TxID != deposit || !slices.Equal(s.KeyImages, []string{outs[0].KeyImage}) || outs[0].SpentBy != s.TxHash {
		t.Fatalf("key images %v, outputs %+v", s.KeyImages, outs)
	}
	if x.notified(x.vendor, "was sent to the fakechain network") != 1 {
		t.Fatal("the vendor was not told the release was sent")
	}
	// Followed to its confirmations, then to 100.
	x.fake.mine(9)
	x.mustPoll()
	if x.settlement(id).State != "relayed" {
		t.Fatalf("9 confirmations: %s", x.settlement(id).State)
	}
	x.fake.mine(1)
	x.mustPoll()
	if x.settlement(id).State != "confirmed" || agInt(x.testEnv, "SELECT count(*) FROM xmr_settlements WHERE id=$1 AND confirmed_at IS NOT NULL AND reorg_safe_at IS NULL", id) != 1 {
		t.Fatalf("10 confirmations: %s", x.settlement(id).State)
	}
	x.fake.mine(90)
	x.mustPoll()
	if agInt(x.testEnv, "SELECT count(*) FROM xmr_settlements WHERE id=$1 AND reorg_safe_at IS NOT NULL", id) != 1 {
		t.Fatal("not reorg-safe at 100 confirmations")
	}
	if x.fake.countOf("transfer") != 1 || x.fake.countOf("relay_tx") != 1 {
		t.Fatalf("transfer %d, relay_tx %d", x.fake.countOf("transfer"), x.fake.countOf("relay_tx"))
	}
}

// After any error on a built settlement (relay lost, the wallet's lookup failing, the daemon unreachable, a definite
// refusal), transfer is never called for it again; only its stored metadata is relayed.
func TestXMREscrowNeverRebuilt(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order, _, _ := x.completed()
	id, _ := x.release(order)
	x.fake.fault("relay_tx", fakeXMRNotOpen)
	if err := x.poll(); err == nil || x.settlement(id).State != "built" {
		t.Fatalf("relay refused with -13: %v %s", err, x.settlement(id).State)
	}
	stored := x.settlement(id)
	x.fake.fault("get_transfer_by_txid", fakeXMRNotOpen)
	if err := x.poll(); err == nil || x.fake.countOf("relay_tx") != 1 {
		t.Fatalf("a failed wallet lookup was read as not found: %v, relay_tx %d", err, x.fake.countOf("relay_tx"))
	}
	x.fake.fault("/get_transactions", fakeXMRDrop)
	if err := x.poll(); err == nil || x.fake.countOf("relay_tx") != 1 {
		t.Fatalf("an unreachable daemon: %v, relay_tx %d", err, x.fake.countOf("relay_tx"))
	}
	x.mustPoll()
	if s := x.settlement(id); s.State != "relayed" || s.TxHash != stored.TxHash || x.fake.countOf("transfer") != 1 || x.fake.countOf("relay_tx") != 2 {
		t.Fatalf("after the errors: %+v, transfer %d", s, x.fake.countOf("transfer"))
	}

	// A definite refusal of a transaction whose coins nobody spent: failed, staff told; never built again.
	order2, _, _ := x.completed()
	id2, _ := x.release(order2)
	x.fake.fault("relay_tx", fakeXMRRejected)
	x.mustPoll()
	transfers := x.fake.countOf("transfer")
	if s := x.settlement(id2); s.State != "failed" || s.TxHash == "" || x.waitClass(id2) != xmrRelayRefused || x.staffFlags(order2) == 0 ||
		agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND flagged AND xmr_settlement_id=$2", order2, id2) != 1 {
		t.Fatalf("refused: %+v %q", s, x.waitClass(id2))
	}
	x.mustPoll()
	x.mustPoll()
	if x.fake.countOf("transfer") != transfers || x.settlement(id2).State != "failed" {
		t.Fatal("a failed settlement was built again")
	}
}

// A relay whose answer is lost after it took effect: the next pass finds the transaction in the daemon and records it
// relayed without sending anything; when the daemon lost it meanwhile, the same stored metadata is relayed again and
// answers the same hash. transfer runs once.
func TestXMREscrowAmbiguousRelay(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order, _, _ := x.completed()
	id, _ := x.release(order)
	x.fake.fault("relay_tx", fakeXMRDrop)
	if err := x.poll(); err == nil {
		t.Fatal("a lost relay answer passed")
	}
	s := x.settlement(id)
	if _, ok := x.fake.tx(s.TxHash); s.State != "built" || !ok {
		t.Fatalf("after the lost answer: %s, in the daemon %v", s.State, ok)
	}
	x.mustPoll()
	if x.settlement(id).State != "relayed" || x.fake.countOf("relay_tx") != 1 {
		t.Fatalf("found in the pool: %s, relay_tx %d", x.settlement(id).State, x.fake.countOf("relay_tx"))
	}

	order2, _, _ := x.completed()
	id2, _ := x.release(order2)
	relays := x.fake.countOf("relay_tx")
	x.fake.fault("relay_tx", fakeXMRDrop)
	x.poll()
	s2 := x.settlement(id2)
	x.fake.flushPool(s2.TxHash)
	x.mustPoll()
	if got := x.settlement(id2); got.State != "relayed" || got.TxHash != s2.TxHash || x.fake.countOf("relay_tx") != relays+2 {
		t.Fatalf("relayed again: %+v, relay_tx %d", got, x.fake.countOf("relay_tx")-relays)
	}
	if _, ok := x.fake.tx(s2.TxHash); !ok {
		t.Fatal("the same transaction is not in the daemon")
	}
	var metas []string
	for _, c := range x.fake.calls {
		if c.Method == "relay_tx" {
			metas = append(metas, c.Params)
		}
	}
	if metas[len(metas)-1] != metas[len(metas)-2] {
		t.Fatalf("relayed different metadata: %v", metas[len(metas)-2:])
	}
	if x.fake.countOf("transfer") != 2 {
		t.Fatalf("transfer called %d times for two settlements", x.fake.countOf("transfer"))
	}
}

// A deposit to a completed order is not the order's: the release freezes it, so the wallet spends only the deposits the
// release claims; the deposit is then returned by a stray refund, which thaws it.
func TestXMREscrowFreezeExcludesTopUp(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order, addr, deposit := x.completed()
	x.fake.payAddress(addr, hex64("late "+order), 7000000000, 12)
	x.mustPoll() // recorded after close and flagged; a stray refund is requested for it
	id, _ := x.release(order)
	x.mustPoll()
	s := x.settlement(id)
	stray := x.settlementOf(order, "stray_refund")
	outs := x.fake.outputs(addr)
	refund := agStrings(t, x.testEnv, "SELECT refund_address FROM escrows WHERE order_id=$1", order)[0]
	if s.State != "relayed" || outs[0].TxID != deposit || !slices.Equal(s.KeyImages, []string{outs[0].KeyImage}) || outs[0].SpentBy != s.TxHash {
		t.Fatalf("release %+v, outputs %+v", s, outs)
	}
	if stray.State != "relayed" || !slices.Equal(stray.KeyImages, []string{outs[1].KeyImage}) || outs[1].SpentBy != stray.TxHash ||
		!slices.Equal(x.outputsOf(stray.ID), []string{"claimant:" + refund + ":" + strconv.FormatInt(7000000000-stray.Fee, 10)}) {
		t.Fatalf("stray refund %+v, outputs %+v", stray, x.outputsOf(stray.ID))
	}
	// The release froze the deposit after close and the stray refund thawed it.
	if x.fake.countOf("freeze") != 1 || x.fake.countOf("thaw") != 1 || outs[1].Frozen {
		t.Fatalf("freeze %d, thaw %d", x.fake.countOf("freeze"), x.fake.countOf("thaw"))
	}
}

// The open wallet's primary address must be the row's, and the account's address 0 the escrow address: otherwise
// nothing is read for a settlement, frozen, built or relayed; an escrow address mismatch is flagged to staff.
func TestXMREscrowPrimaryAddressMismatch(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order, addr, _ := x.completed()
	id, _ := x.release(order)
	x.fake.edit(func(f *fakeXMREscrow) { f.newFile("escrow-209901-01", "x") })
	x.fake.fault("open_wallet", fakeXMRWrongWallet)
	if err := x.poll(); err == nil {
		t.Fatal("another wallet was used")
	}
	for _, m := range []string{"incoming_transfers", "freeze", "thaw", "transfer", "relay_tx"} {
		if x.fake.countOf(m) != 0 {
			t.Fatalf("%s called on the wrong wallet", m)
		}
	}
	name, account := x.fake.accountOf(addr)
	x.fake.edit(func(f *fakeXMREscrow) { f.files[name].accounts[account] = testXMRAddr("moved account", true) })
	x.mustPoll()
	if x.settlement(id).State != "requested" || x.waitClass(id) != xmrAddressMismatch || x.staffFlags(order) != 1 ||
		x.fake.countOf("incoming_transfers")+x.fake.countOf("transfer") != 0 {
		t.Fatalf("escrow address mismatch: %s %q, %d flags", x.settlement(id).State, x.waitClass(id), x.staffFlags(order))
	}
	x.mustPoll()
	if x.staffFlags(order) != 1 {
		t.Fatal("staff were told twice")
	}
	x.fake.edit(func(f *fakeXMREscrow) { f.files[name].accounts[account] = addr })
	x.mustPoll()
	if x.settlement(id).State != "relayed" || x.waitClass(id) != "" {
		t.Fatalf("once the address matches: %s", x.settlement(id).State)
	}
}

// Under the restore gate nothing is built or relayed: a requested settlement is not even read, and a built one is not
// relayed; once the gate is cleared both proceed.
func TestXMREscrowGatePauses(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order, _, _ := x.completed()
	order2, _, _ := x.completed()
	built, _ := x.release(order)
	x.fake.fault("relay_tx", fakeXMRNotOpen)
	x.poll()
	if x.settlement(built).State != "built" {
		t.Fatalf("built: %s", x.settlement(built).State)
	}
	requested, _ := x.release(order2)
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES('payments_recovery_required','true') ON CONFLICT (key) DO UPDATE SET value='true'")
	calls := map[string]int{}
	for _, m := range []string{"incoming_transfers", "freeze", "thaw", "transfer", "relay_tx", "get_transfer_by_txid"} {
		calls[m] = x.fake.countOf(m)
	}
	x.mustPoll()
	x.mustPoll()
	for m, n := range calls {
		if x.fake.countOf(m) != n {
			t.Fatalf("%s called under the restore gate", m)
		}
	}
	if x.settlement(built).State != "built" || x.settlement(requested).State != "requested" {
		t.Fatalf("a settlement moved under the restore gate: %s %s", x.settlement(built).State, x.settlement(requested).State)
	}
	if p := x.panel(order2, x.sessOf(order2, roleBuyer)); !strings.Contains(p, "Paused: "+restoreWhy) {
		t.Fatalf("the order page does not say the settlement is paused:\n%s", p)
	}
	agExec(x.testEnv, "DELETE FROM settings WHERE key='payments_recovery_required'")
	x.mustPoll()
	if x.settlement(built).State != "relayed" || x.settlement(requested).State != "relayed" {
		t.Fatalf("after the gate: %s %s", x.settlement(built).State, x.settlement(requested).State)
	}
}

// A network fee above ESCROW_XMR_MAX_FEE, or a claimant's output below the Monero floor, discards the build unrelayed;
// the settlement waits and staff are told once.
func TestXMREscrowFeeCapAndFloor(t *testing.T) {
	x := newXMREscrowEnv(t)
	t.Setenv("ESCROW_XMR_MAX_FEE", strconv.FormatInt(fakeXMRFee(1, 2)-1, 10))
	x.restart()
	x.mustPoll()
	order, _, _ := x.completed()
	id, _ := x.release(order)
	x.mustPoll()
	x.mustPoll()
	if x.settlement(id).State != "requested" || x.waitClass(id) != xmrFeeCap || x.fake.countOf("relay_tx") != 0 || x.staffFlags(order) != 1 {
		t.Fatalf("fee cap: %s %q, relay_tx %d, staff %d", x.settlement(id).State, x.waitClass(id), x.fake.countOf("relay_tx"), x.staffFlags(order))
	}
	t.Setenv("ESCROW_XMR_MAX_FEE", "")
	x.restart()
	x.mustPoll()
	if x.settlement(id).State != "relayed" {
		t.Fatalf("within the cap: %s", x.settlement(id).State)
	}

	// 0.0025 XMR less the fake's fee of about 0.0019 XMR leaves the vendor less than the 0.001 XMR floor.
	p := x.digital(x.vendor)
	agExec(x.testEnv, "UPDATE products SET xmr=2500000000 WHERE id=$1", p)
	small, addr := x.freshCheckout(p)
	x.fake.payAddress(addr, hex64("small "+small), 2500000000, 10)
	x.mustPoll()
	sid, _ := x.release(small)
	x.mustPoll()
	if x.settlement(sid).State != "requested" || x.waitClass(sid) != xmrTooSmall || x.fake.countOf("relay_tx") != 1 || x.staffFlags(small) != 1 {
		t.Fatalf("below the floor: %s %q", x.settlement(sid).State, x.waitClass(sid))
	}
}

// checkXMRBuild, case by case.
func TestCheckXMRBuild(t *testing.T) {
	plan := []settleOutput{{"claimant", "v", 1000}, {"market_fee", "f", 200}}
	ok := xmrBuiltTx{Fee: 10, Amount: 1190, SpentKeyImages: []string{"b", "a"}, AmountsByDest: []int64{990, 200}}
	for name, c := range map[string]struct {
		edit func(*xmrBuiltTx)
		want string
	}{
		"exact":             {func(*xmrBuiltTx) {}, ""},
		"key image subset":  {func(tx *xmrBuiltTx) { tx.SpentKeyImages = []string{"a"} }, xmrBadBuild},
		"key image super":   {func(tx *xmrBuiltTx) { tx.SpentKeyImages = []string{"a", "b", "c"} }, xmrBadBuild},
		"key image other":   {func(tx *xmrBuiltTx) { tx.SpentKeyImages = []string{"a", "c"} }, xmrBadBuild},
		"key image twice":   {func(tx *xmrBuiltTx) { tx.SpentKeyImages = []string{"a", "a"} }, xmrBadBuild},
		"claimant short":    {func(tx *xmrBuiltTx) { tx.AmountsByDest[0], tx.Amount = 989, 1189 }, xmrBadBuild},
		"fee from fee":      {func(tx *xmrBuiltTx) { tx.AmountsByDest = []int64{1000, 190} }, xmrBadBuild},
		"output missing":    {func(tx *xmrBuiltTx) { tx.AmountsByDest, tx.Amount = []int64{990}, 990 }, xmrBadBuild},
		"amount wrong":      {func(tx *xmrBuiltTx) { tx.Amount = 1200 }, xmrBadBuild},
		"fee above cap":     {func(tx *xmrBuiltTx) { tx.Fee, tx.AmountsByDest[0], tx.Amount = 60, 940, 1140 }, xmrFeeCap},
		"claimant too low":  {func(tx *xmrBuiltTx) { tx.Fee, tx.AmountsByDest[0], tx.Amount = 45, 955, 1155 }, xmrTooSmall},
		"fee at cap passes": {func(tx *xmrBuiltTx) { tx.Fee, tx.AmountsByDest[0], tx.Amount = 50, 950, 1150 }, xmrTooSmall},
	} {
		tx := ok
		tx.SpentKeyImages, tx.AmountsByDest = slices.Clone(ok.SpentKeyImages), slices.Clone(ok.AmountsByDest)
		c.edit(&tx)
		if got := checkXMRBuild(tx, plan, []string{"a", "b"}, 50, 960); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
}

// A wallet whose transfer reply does not pay the plan is not trusted: the build is discarded unrelayed and flagged.
func TestXMREscrowAmountsByDestChecked(t *testing.T) {
	x := newXMREscrowEnv(t)
	t.Setenv("ESCROW_XMR_FEE_BPS", "250")
	t.Setenv("ESCROW_XMR_FEE_ADDRESS", testXMRAddr("market fee", false))
	x.restart()
	x.mustPoll()
	for name, lie := range map[string]func(res map[string]any){
		"claimant paid more": func(res map[string]any) {
			a := res["amounts_by_dest"].(map[string]any)["amounts"].([]int64)
			a[0]++
		},
		"fee taken from the market fee": func(res map[string]any) {
			a := res["amounts_by_dest"].(map[string]any)["amounts"].([]int64)
			fee := res["fee"].(int64)
			a[0], a[1] = a[0]+fee, a[1]-fee
		},
		"an output dropped": func(res map[string]any) {
			a := res["amounts_by_dest"].(map[string]any)["amounts"].([]int64)
			res["amounts_by_dest"] = map[string]any{"amounts": a[:1]}
		},
	} {
		order, _, _ := x.completed()
		id, _ := x.release(order)
		x.fake.edit(func(f *fakeXMREscrow) { f.transferLie = lie })
		relays := x.fake.countOf("relay_tx")
		x.poll()
		x.fake.edit(func(f *fakeXMREscrow) { f.transferLie = nil })
		if s := x.settlement(id); s.State != "requested" || s.TxHash != "" || x.fake.countOf("relay_tx") != relays || len(x.outputsOf(id)) != 0 ||
			x.waitClass(id) != xmrBadBuild && name != "an output dropped" || x.staffFlags(order) != 1 && name != "an output dropped" {
			t.Fatalf("%s: %+v %q, staff %d", name, s, x.waitClass(id), x.staffFlags(order))
		}
		x.mustPoll()
		if x.settlement(id).State != "relayed" {
			t.Fatalf("%s: honest wallet again: %s", name, x.settlement(id).State)
		}
	}
}

// spent_key_images must be exactly the claimed outputs' key images: a subset or a superset is discarded.
func TestXMREscrowKeyImagesExact(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	for name, lie := range map[string]func(res map[string]any){
		"subset": func(res map[string]any) {
			k := res["spent_key_images"].(map[string]any)["key_images"].([]string)
			res["spent_key_images"] = map[string]any{"key_images": k[:1]}
		},
		"superset": func(res map[string]any) {
			k := res["spent_key_images"].(map[string]any)["key_images"].([]string)
			res["spent_key_images"] = map[string]any{"key_images": append(k, hex64("another output"))}
		},
	} {
		order, addr := x.freshCheckout(x.digital(x.vendor))
		x.fake.payAddress(addr, hex64("half a "+order), xmrListingPrice/2, 10)
		x.fake.payAddress(addr, hex64("half b "+order), xmrListingPrice/2, 10)
		x.mustPoll()
		id, _ := x.release(order)
		x.fake.edit(func(f *fakeXMREscrow) { f.transferLie = lie })
		x.mustPoll()
		x.fake.edit(func(f *fakeXMREscrow) { f.transferLie = nil })
		if s := x.settlement(id); s.State != "requested" || x.waitClass(id) != xmrBadBuild {
			t.Fatalf("%s: %+v %q", name, s, x.waitClass(id))
		}
		x.mustPoll()
		if s := x.settlement(id); s.State != "relayed" || len(s.KeyImages) != 2 {
			t.Fatalf("%s: honest wallet: %+v", name, s)
		}
	}
}

// A confirmed settlement that a reorganisation takes out of the chain goes back to relayed; once the daemon lost it,
// the same stored transaction is relayed again; it confirms again and is followed to 100 confirmations.
func TestXMREscrowReorgRelaysAgain(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order, _, _ := x.completed()
	id, _ := x.release(order)
	x.mustPoll()
	x.fake.mine(12)
	x.mustPoll()
	s := x.settlement(id)
	if s.State != "confirmed" {
		t.Fatalf("mined: %s", s.State)
	}
	x.fake.popBlocks(12)
	x.mustPoll()
	if x.settlement(id).State != "relayed" || agInt(x.testEnv, "SELECT count(*) FROM xmr_settlements WHERE id=$1 AND confirmed_at IS NULL", id) != 1 ||
		agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND strpos(note,'reorganisation')>0", order) != 1 {
		t.Fatalf("after the reorganisation: %s", x.settlement(id).State)
	}
	relays := x.fake.countOf("relay_tx")
	x.mustPoll() // still in the pool: nothing to send
	x.fake.flushPool(s.TxHash)
	x.mustPoll()
	if x.fake.countOf("relay_tx") != relays+1 || x.fake.countOf("transfer") != 1 {
		t.Fatalf("relay_tx %d, transfer %d", x.fake.countOf("relay_tx")-relays, x.fake.countOf("transfer"))
	}
	if tx, ok := x.fake.tx(s.TxHash); !ok || tx.Height != 0 {
		t.Fatal("the stored transaction is not back in the pool")
	}
	x.fake.mine(100)
	x.mustPoll()
	x.mustPoll()
	if got := x.settlement(id); got.State != "confirmed" || got.TxHash != s.TxHash || agInt(x.testEnv, "SELECT count(*) FROM xmr_settlements WHERE id=$1 AND reorg_safe_at IS NOT NULL", id) != 1 {
		t.Fatalf("confirmed again: %+v", got)
	}
}

// Coins spent by a transaction the market did not make (a wallet restored from the seed): a relayed settlement the
// daemon lost while its key image is spent, and a built one whose relay the daemon refuses for that reason, are
// external and flagged; neither is sent again.
func TestXMREscrowExternalSpend(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order, _, _ := x.completed()
	id, _ := x.release(order)
	x.mustPoll()
	s := x.settlement(id)
	x.fake.flushPool(s.TxHash)
	x.fake.spendOutside(s.KeyImages...)
	relays := x.fake.countOf("relay_tx")
	x.mustPoll()
	if x.settlement(id).State != "external" || x.fake.countOf("relay_tx") != relays || x.staffFlags(order) != 1 ||
		x.notified(agStrings(t, x.testEnv, "SELECT buyer_id FROM orders WHERE id=$1", order)[0], "spent by a transaction this market did not make") != 1 ||
		agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND flagged", order) != 1 {
		t.Fatalf("relayed then spent outside: %s", x.settlement(id).State)
	}

	order2, addr2, _ := x.completed()
	id2, _ := x.release(order2)
	x.fake.fault("relay_tx", fakeXMRNotOpen)
	x.poll()
	x.fake.spendOutside(x.settlement(id2).KeyImages...)
	x.mustPoll()
	if x.settlement(id2).State != "external" || x.fake.countOf("transfer") != 2 {
		t.Fatalf("built then spent outside: %s", x.settlement(id2).State)
	}
	if outs := x.fake.outputs(addr2); outs[0].SpentBy == x.settlement(id2).TxHash {
		t.Fatal("the market's transaction was sent")
	}
}

// failedRelease is a completed order whose release the daemon refused for good (failed, its transaction stored).
func (x *xmrEscrowEnv) failedRelease() (string, int64) {
	x.t.Helper()
	order, _, _ := x.completed()
	id, _ := x.release(order)
	x.fake.fault("relay_tx", fakeXMRRejected)
	x.mustPoll()
	if x.settlement(id).State != "failed" {
		x.t.Fatalf("refused release: %s", x.settlement(id).State)
	}
	return order, id
}

// reRequest posts the administrator's re-request of settlement id from the order page.
func (x *xmrEscrowEnv) reRequest(sess, order string, id int64, password string) int {
	return x.do("POST", "/admin/escrow-xmr-rerequest", sess, url.Values{"order_id": {order}, "settlement_id": {strconv.FormatInt(id, 10)}, "password": {password}}).Code
}

// The administrator's re-request (A-401): administrators only, with a password step-up, audited; the watcher refuses it
// while the stored transaction is in the pool or the chain or a stored key image is spent; carried out, the failed
// settlement is superseded and a new one spends exactly the stored key images to the stored plan.
func TestXMREscrowReRequestGuards(t *testing.T) {
	x := newXMREscrowEnv(t)
	fee := testXMRAddr("market fee", false)
	t.Setenv("ESCROW_XMR_FEE_BPS", "250")
	t.Setenv("ESCROW_XMR_FEE_ADDRESS", fee)
	x.restart()
	x.mustPoll()
	order, id := x.failedRelease()
	old := x.settlement(id)
	oldOutputs := x.outputsOf(id)
	_, adminSess := x.user("xd_admin_"+randomToken()[:6], "admin")
	_, modSess := x.user("xd_mod_"+randomToken()[:6], "moderator")
	if code := x.reRequest(modSess, order, id, testPassword); code != 403 {
		t.Fatalf("a moderator: %d", code)
	}
	if code := x.reRequest(x.vendorSess, order, id, testPassword); code != 403 {
		t.Fatalf("the vendor: %d", code)
	}
	if code := x.reRequest(adminSess, order, id, ""); code == 303 {
		t.Fatal("re-requested without a password")
	}
	if code := x.reRequest(adminSess, order, id, "not the password"); code == 303 {
		t.Fatal("re-requested with a wrong password")
	}
	if got := x.reRequests(id); len(got) != 0 {
		t.Fatalf("a refused form recorded a request: %v", got)
	}
	// The order page offers the form to an administrator, and to nobody else.
	if page := x.page("/order?id="+order, adminSess); !strings.Contains(page, `action="/admin/escrow-xmr-rerequest"`) {
		t.Fatal("no re-request form for the administrator")
	}
	for _, viewer := range []string{roleBuyer, roleVendor} {
		if strings.Contains(x.page("/order?id="+order, x.sessOf(order, viewer)), "/admin/escrow-xmr-rerequest") {
			t.Fatal("a party is offered the re-request")
		}
	}

	var want []string
	refused := func(class string) {
		t.Helper()
		if code := x.reRequest(adminSess, order, id, testPassword); code != 303 {
			t.Fatalf("re-request: %d", code)
		}
		x.mustPoll()
		want = append(want, "refused:"+class+":0")
		if got := x.reRequests(id); !slices.Equal(got, want) ||
			x.settlement(id).State != "failed" || agInt(x.testEnv, "SELECT count(*) FROM xmr_settlements WHERE supersedes=$1", id) != 0 {
			t.Fatalf("%s: %v %s", class, got, x.settlement(id).State)
		}
		if p := html.UnescapeString(x.page("/order?id="+order, adminSess)); !strings.Contains(p, xmrReRequestRefusal(class)) {
			t.Fatalf("%s: the order page does not say why", class)
		}
	}
	// In the pool, then on the chain (a reorganisation and a flushed pool put it back to nowhere).
	x.fake.edit(func(f *fakeXMREscrow) { f.relay(f.built[hexMeta(f, old.TxHash)]) })
	refused(xmrReRequestInPool)
	x.fake.mine(1)
	refused(xmrReRequestOnChain)
	x.fake.popBlocks(1)
	x.fake.flushPool(old.TxHash)
	// A key image spent by another transaction.
	outside := x.fake.spendOutside(old.KeyImages...)
	refused(xmrReRequestSpent)
	x.fake.flushPool(outside)

	// Carried out, with the stored plan although a fresh plan would now differ (the order's price changed).
	agExec(x.testEnv, "UPDATE orders SET amount=amount/2 WHERE id=$1", order)
	if code := x.reRequest(adminSess, order, id, testPassword); code != 303 {
		t.Fatalf("re-request: %d", code)
	}
	if code := x.reRequest(adminSess, order, id, testPassword); code != 409 {
		t.Fatalf("a second request while one waits: %d", code)
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM audit_events WHERE strpos(action,$1)>0", "Re-requested the failed Monero escrow settlement "+strconv.FormatInt(id, 10)); n != 4 {
		t.Fatalf("audit rows: %d", n)
	}
	x.mustPoll()
	next := x.settlementOf(order, "release")
	if x.settlement(id).State != "superseded" || x.settlement(id).TxHash != old.TxHash || next.Supersedes != id || next.State != "relayed" ||
		!slices.Equal(slices.Sorted(slices.Values(next.KeyImages)), slices.Sorted(slices.Values(old.KeyImages))) || next.InputTotal != old.InputTotal {
		t.Fatalf("carried out: old %+v new %+v", x.settlement(id), next)
	}
	got := x.outputsOf(next.ID)
	if len(got) != len(oldOutputs) || got[1] != oldOutputs[1] || strings.Split(got[0], ":")[1] != strings.Split(oldOutputs[0], ":")[1] {
		t.Fatalf("plan changed:\n old %v\n new %v", oldOutputs, got)
	}
	if got := x.reRequests(id); agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND xmr_settlement_id=$2", order, next.ID) != 1 ||
		!slices.Equal(got, append(want, "done::"+strconv.FormatInt(next.ID, 10))) ||
		agInt(x.testEnv, "SELECT count(*) FROM xmr_settlement_rerequests r JOIN users u ON u.id=r.requested_by WHERE r.settlement_id=$1 AND u.role='admin' AND r.decided_at IS NOT NULL", id) != 4 {
		t.Fatalf("the deposit claim did not move, or the request was not recorded done: %v", got)
	}
	if x.reRequest(adminSess, order, id, testPassword) != 409 {
		t.Fatal("a superseded settlement was re-requested")
	}
}

// hexMeta is the metadata of the built transaction hash. f.mu is held.
func hexMeta(f *fakeXMREscrow, hash string) string {
	for meta, tx := range f.built {
		if tx.Hash == hash {
			return meta
		}
	}
	f.t.Fatalf("no built transaction %s", hash)
	return ""
}

// Refunds go to the pinned refund address with the fee from the amount: an underpaid order at the end of its payment
// window, a cancelled paid order, a dispute decided for the buyer; a deposit after close comes back by a stray refund,
// and one recorded before close but not spent by the release is left out and comes back the same way.
func TestXMREscrowRefundPaths(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	refundOf := func(order string) string {
		return agStrings(t, x.testEnv, "SELECT refund_address FROM escrows WHERE order_id=$1", order)[0]
	}
	paysRefund := func(what, order, kind string, amount int64) {
		t.Helper()
		x.mustPoll()
		s := x.settlementOf(order, kind)
		if want := []string{"claimant:" + refundOf(order) + ":" + strconv.FormatInt(amount-s.Fee, 10)}; s.State != "relayed" || !slices.Equal(x.outputsOf(s.ID), want) {
			t.Fatalf("%s: %+v outputs %v, want %v", what, s, x.outputsOf(s.ID), want)
		}
	}

	// Underpaid: cancelled at the end of its window, refund requested and sent.
	under, uaddr := x.freshCheckout(x.digital(x.vendor))
	x.fake.payAddress(uaddr, hex64("under "+under), xmrListingPrice/2, 12)
	x.mustPoll()
	agExec(x.testEnv, "UPDATE escrows SET created=now()-interval '3 days' WHERE order_id=$1", under)
	agExec(x.testEnv, "UPDATE payments SET created=now()-interval '3 days' WHERE order_id=$1", under)
	x.mustPoll()
	if x.state(under) != stateCancelled {
		t.Fatalf("underpaid: %s", x.state(under))
	}
	paysRefund("underpaid", under, "refund", xmrListingPrice/2)

	// Cancelled by the vendor while paid.
	phys := x.physical(x.vendor)
	cancelled, caddr := x.freshCheckout(phys)
	x.fake.payAddress(caddr, hex64("paid "+cancelled), xmrListingPrice, 10)
	x.mustPoll()
	vsess := x.sessOf(cancelled, roleVendor)
	var cancel *pageForm
	for _, f := range x.orderForms(cancelled, vsess) {
		if f.action == "/orders/cancel" {
			cancel = &f
		}
	}
	if cancel == nil {
		t.Fatal("no cancel form for the vendor")
	}
	if w := x.do("POST", cancel.action, vsess, cancel.values); w.Code != 303 || x.state(cancelled) != stateCancelled {
		t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
	}
	paysRefund("cancelled", cancelled, "refund", xmrListingPrice)

	// A dispute decided for the buyer.
	disputed, daddr := x.freshCheckout(phys)
	x.fake.payAddress(daddr, hex64("disputed "+disputed), xmrListingPrice, 10)
	x.mustPoll()
	dispute := x.disputeAndResolve(disputed, "refund")
	if x.state(disputed) != stateResolved {
		t.Fatalf("resolved: %s (%s)", x.state(disputed), dispute)
	}
	paysRefund("decided for the buyer", disputed, "refund", xmrListingPrice)

	// Left out, then after close: the price at 10 confirmations and a top-up still confirming complete the order; the
	// release spends the price only; the top-up is left out once it confirms and returned by a stray refund.
	order, addr := x.freshCheckout(x.digital(x.vendor))
	x.fake.payAddress(addr, hex64("price "+order), xmrListingPrice, 10)
	x.fake.payAddress(addr, hex64("top-up "+order), 3000000000, 4)
	x.mustPoll()
	release, _ := x.release(order)
	x.mustPoll()
	if s := x.settlement(release); s.State != "relayed" || s.InputTotal != xmrListingPrice {
		t.Fatalf("release: %+v", s)
	}
	x.fake.setConfs(hex64("top-up "+order), 12)
	x.mustPoll()
	if agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND escrow_left_out AND NOT after_close", order) != 1 ||
		x.notified(agStrings(t, x.testEnv, "SELECT buyer_id FROM orders WHERE id=$1", order)[0], "does not spend them") != 1 {
		t.Fatal("the top-up was not left out")
	}
	paysRefund("left out", order, "stray_refund", 3000000000)
	for _, o := range []string{under, cancelled, disputed, order} {
		for _, n := range agStrings(t, x.testEnv, "SELECT note FROM order_events WHERE order_id=$1", o) {
			if strings.Contains(n, "BTC") || strings.Contains(n, "sign") {
				t.Fatalf("a Monero escrow note: %q", n)
			}
		}
	}
}

// disputeAndResolve has the buyer of a paid order dispute it and an administrator decide outcome; it returns the dispute.
func (x *xmrEscrowEnv) disputeAndResolve(order, outcome string) string {
	x.t.Helper()
	bsess := x.sessOf(order, roleBuyer)
	if w := x.do("POST", "/disputes", bsess, url.Values{"order_id": {order}, "reason": {"The parcel never arrived at the address I sent."}}); w.Code != 303 {
		x.t.Fatalf("dispute: %d %s", w.Code, w.Body.String())
	}
	dispute := agStrings(x.t, x.testEnv, "SELECT id FROM disputes WHERE order_id=$1", order)[0]
	_, adminSess := x.user("xd_judge_"+randomToken()[:6], "admin")
	seen := agStrings(x.t, x.testEnv, "SELECT COALESCE(sum(amount),0)::text FROM payments WHERE order_id=$1 AND confirmations>=0 AND NOT locked AND NOT after_close", order)[0]
	w := x.do("POST", "/resolve", adminSess, url.Values{"id": {dispute}, "outcome": {outcome}, "resolution": {"Decided from the evidence both parties sent."},
		"payout_confirmed": {"confirmed"}, "amount_seen": {seen}, "reply_seen": {"0"}, "password": {testPassword}})
	if w.Code != 303 {
		x.t.Fatalf("resolve: %d %s", w.Code, w.Body.String())
	}
	return dispute
}

// A dispute on a Monero escrow order decided for the vendor writes a Monero note (A-405): no Bitcoin, no signatures.
func TestXMREscrowVendorDecisionNote(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order, addr := x.freshCheckout(x.physical(x.vendor))
	x.fake.payAddress(addr, hex64("paid "+order), xmrListingPrice, 10)
	x.mustPoll()
	x.disputeAndResolve(order, "release")
	notes := agStrings(t, x.testEnv, "SELECT note FROM order_events WHERE order_id=$1 AND strpos(note,'decided for the vendor')>0", order)
	if len(notes) != 1 || !strings.Contains(notes[0], "0.5 XMR") || !strings.Contains(notes[0], "Monero address") {
		t.Fatalf("decision notes: %q", notes)
	}
	for _, banned := range []string{"BTC", "Bitcoin", "sign", "co-sign"} {
		if strings.Contains(notes[0], banned) {
			t.Fatalf("the note says %q: %s", banned, notes[0])
		}
	}
	// With a release requested at shipment, the note says the market builds and sends it.
	o2, addr2 := x.freshCheckout(x.physical(x.vendor))
	x.fake.payAddress(addr2, hex64("paid "+o2), xmrListingPrice, 10)
	x.mustPoll()
	x.release(o2)
	x.disputeAndResolve(o2, "release")
	note := agStrings(t, x.testEnv, "SELECT note FROM order_events WHERE order_id=$1 AND strpos(note,'decided for the vendor')>0", o2)[0]
	if !strings.Contains(note, "requested when the order was shipped") || strings.Contains(note, "sign") || strings.Contains(note, "BTC") {
		t.Fatalf("decision note with a release: %s", note)
	}
	x.mustPoll()
	if x.settlementOf(o2, "release").State != "relayed" {
		t.Fatalf("the decided release: %s", x.settlementOf(o2, "release").State)
	}
}

// The settlement on the order page: its kind, state and transaction, each output in XMR to 12 decimals, the network
// fee and the confirmations; an output's address only to the party it pays; no Bitcoin words, no script, no inline
// widths (the Bitcoin settlement's 320 px-safe markup).
func TestXMREscrowSettlementPanel(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order, _, _ := x.completed()
	id, claim := x.release(order)
	buyerSess, vendorSess := x.sessOf(order, roleBuyer), x.sessOf(order, roleVendor)
	if p := x.panel(order, vendorSess); !strings.Contains(p, "Settlement "+strconv.FormatInt(id, 10)+": The vendor's claim") || !strings.Contains(p, "Requested: the market builds") {
		t.Fatalf("requested:\n%s", p)
	}
	x.mustPoll()
	s := x.settlement(id)
	vendor := x.panel(order, vendorSess)
	for _, want := range []string{"Sent to the fakechain network: transaction " + s.TxHash, `id="xmr-settlement-` + strconv.FormatInt(id, 10) + `"`, claim,
		amount(xmrListingPrice-s.Fee, 12) + " XMR", "0.0018744 XMR", "Fewer than 10", `<ol class="settlement-outputs"`, `<span class="settlement-address">`} {
		if !strings.Contains(vendor, want) {
			t.Fatalf("vendor's panel lacks %q:\n%s", want, vendor)
		}
	}
	buyer := x.panel(order, buyerSess)
	if strings.Contains(buyer, claim) || !strings.Contains(buyer, "(shown only to its recipient)") {
		t.Fatalf("the buyer sees the vendor's claim address:\n%s", buyer)
	}
	for _, banned := range []string{"Bitcoin", "BTC", "regtest", "PSBT", "sign", "<script", "style=", "width="} {
		if strings.Contains(vendor, banned) || strings.Contains(buyer, banned) {
			t.Fatalf("the Monero settlement says %q", banned)
		}
	}
	x.fake.mine(100)
	x.mustPoll()
	x.mustPoll()
	if p := x.panel(order, vendorSess); !strings.Contains(p, "Confirmed on the fakechain network") || !strings.Contains(p, "At least 100 (no longer followed)") {
		t.Fatalf("confirmed:\n%s", p)
	}
}

// The wallet refusing a build (A-415 fix round): -2 refuses a destination for good, named by validate_address,
// flagged once and not tried again (a refused fee address is replaced once ESCROW_XMR_FEE_ADDRESS names another the
// wallet accepts); another wallet error is recorded with its code and flagged when it persists; a timeout is retried
// unflagged.
func TestXMREscrowBuildRefusals(t *testing.T) {
	x := newXMREscrowEnv(t)
	badFee := testXMRAddr("market fee", false)
	t.Setenv("ESCROW_XMR_FEE_BPS", "250")
	t.Setenv("ESCROW_XMR_FEE_ADDRESS", badFee)
	x.restart()
	x.mustPoll()
	transfers := func() int { return x.fake.countOf("transfer") }

	// -2 on the fee output.
	order, _, _ := x.completed()
	id, _ := x.release(order)
	x.fake.edit(func(f *fakeXMREscrow) { f.invalid[badFee] = true })
	x.mustPoll()
	n := transfers()
	x.mustPoll()
	if x.settlement(id).State != "requested" || x.waitClass(id) != xmrDestRefused+"-market_fee" || x.staffFlags(order) != 1 || transfers() != n {
		t.Fatalf("fee refused: %s %q, staff %d, transfer %d then %d", x.settlement(id).State, x.waitClass(id), x.staffFlags(order), n, transfers())
	}
	if p := x.panel(order, x.sessOf(order, roleVendor)); !strings.Contains(p, "ESCROW_XMR_FEE_ADDRESS") {
		t.Fatalf("the order page does not say why:\n%s", p)
	}
	// The operator sets a fee address the wallet refuses too: still not built; then one it accepts: built and sent.
	alsoBad := testXMRAddr("market fee 2", false)
	x.fake.edit(func(f *fakeXMREscrow) { f.invalid[alsoBad] = true })
	t.Setenv("ESCROW_XMR_FEE_ADDRESS", alsoBad)
	x.restart()
	x.mustPoll()
	if x.settlement(id).State != "requested" || transfers() != n {
		t.Fatalf("a refused new fee address was used: %s, transfer %d", x.settlement(id).State, transfers())
	}
	good := testXMRAddr("market fee 3", false)
	t.Setenv("ESCROW_XMR_FEE_ADDRESS", good)
	x.restart()
	x.mustPoll()
	if out := x.outputsOf(id); x.settlement(id).State != "relayed" || len(out) != 2 || !strings.HasPrefix(out[1], "market_fee:"+good+":") || x.waitClass(id) != "" {
		t.Fatalf("with the corrected fee address: %s %v", x.settlement(id).State, out)
	}

	// -2 on the buyer's refund address (a cancelled order's refund).
	refunded, _, _ := x.completed()
	agExec(x.testEnv, "UPDATE orders SET state='cancelled' WHERE id=$1", refunded)
	var rid int64
	if err := x.DB.QueryRow("INSERT INTO xmr_settlements(order_id,kind,destination) SELECT order_id,'refund',refund_address FROM escrows WHERE order_id=$1 RETURNING id", refunded).Scan(&rid); err != nil {
		t.Fatal(err)
	}
	refund := agStrings(t, x.testEnv, "SELECT refund_address FROM escrows WHERE order_id=$1", refunded)[0]
	x.fake.edit(func(f *fakeXMREscrow) { f.invalid[refund] = true })
	x.mustPoll()
	n = transfers()
	x.mustPoll()
	if x.waitClass(rid) != xmrDestRefused+"-claimant" || x.staffFlags(refunded) != 1 || transfers() != n {
		t.Fatalf("refund address refused: %q, staff %d", x.waitClass(rid), x.staffFlags(refunded))
	}
	if p := x.panel(refunded, x.sessOf(refunded, roleBuyer)); !strings.Contains(p, "refund address given at checkout as invalid") || !strings.Contains(p, "contact the buyer") {
		t.Fatalf("the buyer is not told:\n%s", p)
	}

	// Another wallet error (-4) twice: recorded with its code, flagged on the second pass only; then built.
	o4, _, _ := x.completed()
	id4, _ := x.release(o4)
	x.fake.fault("transfer", fakeXMRRejected)
	x.mustPoll()
	if x.waitClass(id4) != xmrWalletRefusing+" rpc:-4" || x.staffFlags(o4) != 0 {
		t.Fatalf("-4 once: %q, staff %d", x.waitClass(id4), x.staffFlags(o4))
	}
	x.fake.fault("transfer", fakeXMRRejected)
	x.mustPoll()
	x.fake.fault("transfer", fakeXMRRejected)
	x.mustPoll()
	if x.waitClass(id4) != xmrWalletRefused+" rpc:-4" || x.staffFlags(o4) != 1 {
		t.Fatalf("-4 persisting: %q, staff %d", x.waitClass(id4), x.staffFlags(o4))
	}
	x.mustPoll()
	if x.settlement(id4).State != "relayed" {
		t.Fatalf("after -4: %s", x.settlement(id4).State)
	}

	// A timeout: an error of the pass, no wait class, no flag; the next pass builds again and sends.
	o5, _, _ := x.completed()
	id5, _ := x.release(o5)
	x.fake.fault("transfer", fakeXMRDrop)
	if err := x.poll(); err == nil || x.waitClass(id5) != "" || x.staffFlags(o5) != 0 || x.settlement(id5).State != "requested" {
		t.Fatalf("timeout: %v %q, staff %d", err, x.waitClass(id5), x.staffFlags(o5))
	}
	x.mustPoll()
	if x.settlement(id5).State != "relayed" || x.staffFlags(o5) != 0 {
		t.Fatalf("after the timeout: %s", x.settlement(id5).State)
	}
}
