package market

import (
	"encoding/json"
	"errors"
	"html"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Escrow Phase 3, final cross-slice QA (WR-escrow A-248; A-424 to A-426): DB integration tests against the simulated
// wallet-rpc and monerod of escrow_xmr_fake_test.go (simulated RPC, not chain evidence; scripts/test-escrow-xmr-regtest.sh
// has the real-chain legs).

// A-424 (QA's probe through expiry): a deposit the wallet reads as conflicted (double_spend_seen), or no longer lists,
// when the payment window ends does not count, so the order is cancelled unpaid and no refund is requested. When that
// transaction is mined after all, the deposit is left out and flagged, the buyer and the staff are told, and a stray
// refund returns it, less the network fee, to the refund address pinned at checkout.
func TestXMREscrowConflictedDepositThroughExpiry(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	for _, c := range []struct {
		what       string
		lose, back func(addr, txid string)
	}{
		{"double spend seen", func(_, txid string) {
			x.fake.setTransfer(txid, func(t *fakeXMRTransfer) { t.DoubleSpend = true })
		}, func(_, txid string) {
			x.fake.setTransfer(txid, func(t *fakeXMRTransfer) { t.DoubleSpend, t.Confs, t.Pool = false, 20, false })
		}},
		{"no longer listed", func(_, txid string) {
			x.fake.edit(func(f *fakeXMREscrow) {
				for _, w := range f.files {
					w.transfers = slices.DeleteFunc(w.transfers, func(t *fakeXMRTransfer) bool { return t.TxID == txid })
				}
			})
		}, func(addr, txid string) {
			x.fake.payAddress(addr, txid, xmrListingPrice, 20)
		}},
	} {
		order, addr := x.freshCheckout(x.digital(x.vendor))
		buyer := agStrings(t, x.testEnv, "SELECT buyer_id FROM orders WHERE id=$1", order)[0]
		txid := hex64("conflicted " + order)
		x.fake.payAddress(addr, txid, xmrListingPrice, 0)
		x.mustPoll()
		c.lose(addr, txid)
		x.mustPoll()
		agExec(x.testEnv, "UPDATE escrows SET created=now()-interval '3 days' WHERE order_id=$1", order)
		x.mustPoll()
		if x.state(order) != stateCancelled || len(x.xmrSettlements(order)) != 0 {
			t.Fatalf("%s: at the end of the window: %s, settlements %v", c.what, x.state(order), x.xmrSettlements(order))
		}
		c.back(addr, txid)
		x.mustPoll()
		if got := agStrings(t, x.testEnv, "SELECT confirmations||' '||credited||' '||flagged||' '||after_close||' '||escrow_left_out FROM payments WHERE order_id=$1", order); !slices.Equal(got, []string{"20 false true false true"}) ||
			x.notified(buyer, "did not count for the order") != 1 || x.staffFlags(order) == 0 {
			t.Fatalf("%s: mined after the cancellation: deposit %v, buyer notices %d, staff flags %d", c.what, got, x.notified(buyer, "did not count for the order"), x.staffFlags(order))
		}
		x.mustPoll()
		stray := x.settlementOf(order, "stray_refund")
		refund := agStrings(t, x.testEnv, "SELECT refund_address FROM escrows WHERE order_id=$1", order)[0]
		if want := []string{"claimant:" + refund + ":" + strconv.FormatInt(xmrListingPrice-stray.Fee, 10)}; stray.State != "relayed" || !slices.Equal(x.outputsOf(stray.ID), want) ||
			x.notified(buyer, "Stray refund requested") != 1 {
			t.Fatalf("%s: stray refund %+v outputs %v, want %v", c.what, stray, x.outputsOf(stray.ID), want)
		}
		x.mustPoll()
		if x.notified(buyer, "did not count for the order") != 1 || len(x.xmrSettlements(order)) != 1 {
			t.Fatalf("%s: told again, or another settlement: %v", c.what, x.xmrSettlements(order))
		}
	}

	// A cancelled order whose refund is requested keeps its deposits for that refund: here the wallet refuses the refund
	// address, so the refund stays requested, and the deposit's row changes after the cancellation.
	order, addr := x.freshCheckout(x.physical(x.vendor))
	txid := hex64("paid " + order)
	x.fake.payAddress(addr, txid, xmrListingPrice, 10)
	x.mustPoll()
	refund := agStrings(t, x.testEnv, "SELECT refund_address FROM escrows WHERE order_id=$1", order)[0]
	x.fake.edit(func(f *fakeXMREscrow) { f.invalid[refund] = true })
	vsess := x.sessOf(order, roleVendor)
	for _, f := range x.orderForms(order, vsess) {
		if f.action == "/orders/cancel" {
			if w := x.do("POST", f.action, vsess, f.values); w.Code != 303 {
				t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
			}
		}
	}
	x.mustPoll()
	x.fake.setConfs(txid, 11)
	x.mustPoll()
	if got := x.xmrSettlements(order); x.state(order) != stateCancelled || !slices.Equal(got, []string{"refund requested " + refund}) ||
		agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND escrow_left_out", order) != 0 {
		t.Fatalf("a requested refund's deposit: %s, settlements %v, deposits %v", x.state(order), got,
			agStrings(t, x.testEnv, "SELECT confirmations||' '||escrow_left_out FROM payments WHERE order_id=$1", order))
	}
}

// A-426: monero-wallet-rpc answers -4 for any failure of commit_tx, a lost connection to its daemon included, so a
// refusal of a stored transaction that the daemon does not have and whose coins nobody spent is relayed again, with
// the same stored metadata, on the next passes; only xmrRelayAttempts refusals in a row record it failed, flagged once.
// transfer is never called again. A stored key image spent by another transaction is external at once.
func TestXMREscrowRelayRefusalRetried(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	relayed := func() []string {
		var metas []string
		for _, c := range x.fake.callLog() {
			if c.Method == "relay_tx" {
				metas = append(metas, c.Params)
			}
		}
		return metas
	}

	// Refused once, then relayed on the next pass, and confirmed.
	order, _, _ := x.completed()
	id, _ := x.release(order)
	x.fake.fault("relay_tx", fakeXMRRejected)
	x.mustPoll()
	if s := x.settlement(id); s.State != "built" || x.waitClass(id) != xmrRelayRefusing+" 1" || x.staffFlags(order) != 0 {
		t.Fatalf("refused once: %s %q, staff flags %d", s.State, x.waitClass(id), x.staffFlags(order))
	}
	if p := html.UnescapeString(x.panel(order, x.sessOf(order, roleBuyer))); !strings.Contains(p, xmrSettleWaitText(xmrRelayRefusing+" 1", "release", 10)) {
		t.Fatalf("the order page does not say the relay is tried again:\n%s", p)
	}
	x.mustPoll()
	if s := x.settlement(id); s.State != "relayed" || x.waitClass(id) != "" || x.fake.countOf("transfer") != 1 || len(relayed()) != 2 || relayed()[0] != relayed()[1] {
		t.Fatalf("relayed on the next pass: %s %q, transfer %d, relay_tx %v", s.State, x.waitClass(id), x.fake.countOf("transfer"), relayed())
	}
	x.fake.mine(10)
	x.mustPoll()
	if x.settlement(id).State != "confirmed" || x.staffFlags(order) != 0 {
		t.Fatalf("after 10 blocks: %s", x.settlement(id).State)
	}

	// The daemon lost a relayed transaction and refuses it once: still relayed. The count ends when the daemon has it
	// again (here it arrived meanwhile, so nothing is relayed), or when the next relay takes effect.
	order, _, _ = x.completed()
	id, _ = x.release(order)
	x.mustPoll()
	hash := x.settlement(id).TxHash
	for _, back := range []bool{true, false} {
		x.fake.flushPool(hash)
		x.fake.fault("relay_tx", fakeXMRRejected)
		x.mustPoll()
		if x.settlement(id).State != "relayed" || x.waitClass(id) != xmrRelayRefusing+" 1" {
			t.Fatalf("a relayed transaction refused once: %s %q", x.settlement(id).State, x.waitClass(id))
		}
		relays := len(relayed())
		if back {
			x.fake.edit(func(f *fakeXMREscrow) { f.relay(f.built[hexMeta(f, hash)]) })
		}
		x.mustPoll()
		if _, ok := x.fake.tx(hash); x.settlement(id).State != "relayed" || x.waitClass(id) != "" || !ok || (len(relayed()) == relays) != back {
			t.Fatalf("in the daemon again (it arrived itself: %v): %s %q, in the daemon %v, relay_tx %d", back, x.settlement(id).State, x.waitClass(id), ok, len(relayed())-relays)
		}
	}

	// Refused on xmrRelayAttempts passes in a row: failed and flagged once; the same metadata each time, transfer once.
	order, _, _ = x.completed()
	id, _ = x.release(order)
	transfers, relays := x.fake.countOf("transfer"), len(relayed())
	for range xmrRelayAttempts {
		x.fake.fault("relay_tx", fakeXMRRejected)
	}
	for n := 1; n < xmrRelayAttempts; n++ {
		x.mustPoll()
		if s := x.settlement(id); s.State != "built" || x.waitClass(id) != xmrRelayRefusing+" "+strconv.Itoa(n) || x.staffFlags(order) != 0 {
			t.Fatalf("refused %d times: %s %q, staff flags %d", n, s.State, x.waitClass(id), x.staffFlags(order))
		}
	}
	x.mustPoll()
	x.mustPoll()
	metas := relayed()[relays:]
	if s := x.settlement(id); s.State != "failed" || x.waitClass(id) != xmrRelayRefused || x.staffFlags(order) != 1 || x.fake.countOf("transfer") != transfers+1 ||
		len(metas) != xmrRelayAttempts || metas[0] != metas[1] || metas[1] != metas[2] {
		t.Fatalf("refused %d times: %s %q, staff flags %d, transfer %d, relay_tx %d", xmrRelayAttempts, s.State, x.waitClass(id), x.staffFlags(order),
			x.fake.countOf("transfer")-transfers, len(metas))
	}

	// Refused once, then its coins spent by another transaction: external at the next refusal, not counted.
	order, _, _ = x.completed()
	id, _ = x.release(order)
	x.fake.fault("relay_tx", fakeXMRRejected)
	x.mustPoll()
	x.fake.spendOutside(x.settlement(id).KeyImages...)
	x.mustPoll()
	if x.settlement(id).State != "external" || x.staffFlags(order) != 1 {
		t.Fatalf("spent outside after a refusal: %s, staff flags %d", x.settlement(id).State, x.staffFlags(order))
	}

	// Closing QA F7: under the restore gate, a held settlement whose relay an administrator confirmed and that the daemon
	// then refuses keeps the confirmation with its count: still listed confirmed on /admin (not confirmable again), and
	// relayed again on the next pass, the gate still set.
	order, _, _ = x.completed()
	id, _ = x.release(order)
	x.fake.fault("relay_tx", fakeXMRNotOpen)
	if err := x.poll(); err == nil || x.settlement(id).State != "built" {
		t.Fatalf("relay_tx refused -13: %v %s", err, x.settlement(id).State)
	}
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES('payments_recovery_required','true') ON CONFLICT (key) DO UPDATE SET value='true'")
	agExec(x.testEnv, "UPDATE xmr_settlements SET send_error=$2 WHERE id=$1", id, restoredSettlementHold)
	_, adminSess := x.user("xq_admin_"+randomToken()[:6], "admin")
	confirm := func() (int, string) {
		w := x.do("POST", "/admin/escrow-xmr-rebroadcast", adminSess, url.Values{"settlement_id": {strconv.FormatInt(id, 10)}, "not_sent": {"confirmed"}, "password": {testPassword}})
		return w.Code, w.Body.String()
	}
	if code, _ := confirm(); code != 303 || x.waitClass(id) != sendRebroadcastOK {
		t.Fatalf("confirming the held relay: %d %q", code, x.waitClass(id))
	}
	relays = len(relayed())
	x.fake.fault("relay_tx", fakeXMRRejected)
	x.mustPoll()
	if s := x.settlement(id); s.State != "built" || !strings.HasPrefix(x.waitClass(id), xmrRelayRefusing+" 1") || !xmrRelayConfirmed(x.waitClass(id)) || len(relayed()) != relays+1 {
		t.Fatalf("a confirmed relay refused once under the gate: %s %q, relay_tx %d", s.State, x.waitClass(id), len(relayed())-relays)
	}
	if page := html.UnescapeString(x.page("/admin", adminSess)); !strings.Contains(page, shortID(order)) || !strings.Contains(page, "Confirmed: the market relays it") {
		t.Fatal("the confirmed settlement left the /admin held list")
	}
	if code, body := confirm(); code != 409 || !strings.Contains(body, "already confirmed") {
		t.Fatalf("confirmed again: %d %s", code, body)
	}
	if p := html.UnescapeString(x.panel(order, x.sessOf(order, roleBuyer))); !strings.Contains(p, xmrSettleWaitText(x.waitClass(id), "release", 10)) {
		t.Fatalf("the order page:\n%s", p)
	}
	x.mustPoll()
	if s := x.settlement(id); s.State != "relayed" || x.waitClass(id) != "" || len(relayed()) != relays+2 || relayed()[relays] != relayed()[relays+1] {
		t.Fatalf("the confirmed relay under the gate after a refusal: %s %q, relay_tx %d", s.State, x.waitClass(id), len(relayed())-relays)
	}
	agExec(x.testEnv, "DELETE FROM settings WHERE key='payments_recovery_required'")
}

// A-425 (M22): a free slot is written only when get_address of the new account's address 0 is the address
// create_account returned; otherwise no slot row is written and the pass reports the mismatch.
func TestXMREscrowTopUpAddressChecked(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	lie := testXMRAddr("not the new account", true)
	created := fakeXMRWalletMethods["create_account"]
	x.fake.handle("create_account", func(f *fakeXMREscrow, p json.RawMessage) (any, int64, string) {
		res, code, msg := created(f, p)
		res.(map[string]any)["address"] = lie
		return res, code, msg
	})
	agExec(x.testEnv, "UPDATE xmr_escrow_slots SET state='retired' WHERE id=(SELECT min(id) FROM xmr_escrow_slots WHERE state='free')")
	if err := x.poll(); !errors.Is(err, errXMRAccountMismatch) {
		t.Fatalf("a create_account address that get_address does not give: %v", err)
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM xmr_escrow_slots WHERE address=$1", lie); n != 0 || x.slots("escrow-202610-01", "free") != 4 {
		t.Fatalf("%d slots written at create_account's address, %d free", n, x.slots("escrow-202610-01", "free"))
	}
}

// A-425 (M14): an administrator's re-request waits while the restore gate is set: it is not decided and nothing is
// superseded or requested; once the gate is cleared it is carried out.
func TestXMREscrowReRequestWaitsForGate(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order, id := x.failedRelease()
	_, adminSess := x.user("xq_admin_"+randomToken()[:6], "admin")
	if code := x.reRequest(adminSess, order, id, testPassword); code != 303 {
		t.Fatalf("re-request: %d", code)
	}
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES('payments_recovery_required','true') ON CONFLICT (key) DO UPDATE SET value='true'")
	x.mustPoll()
	if got := x.reRequests(id); !slices.Equal(got, []string{"requested::0"}) || x.settlement(id).State != "failed" || len(x.xmrSettlements(order)) != 1 {
		t.Fatalf("under the restore gate: re-requests %v, settlement %s, settlements %v", got, x.settlement(id).State, x.xmrSettlements(order))
	}
	agExec(x.testEnv, "DELETE FROM settings WHERE key='payments_recovery_required'")
	x.mustPoll()
	if next := x.settlementOf(order, "release"); next.Supersedes != id || next.State != "relayed" || x.settlement(id).State != "superseded" {
		t.Fatalf("after the gate: old %s, new %+v", x.settlement(id).State, next)
	}
}

// A-425 (M6): each wallet's pass requests the stray refunds of its own orders only. Here an order's deposit becomes
// due for a stray refund after its wallet's pass (left out at the end of a pass, A-424), and on the next pass that
// wallet does not open: the other wallet's pass requests nothing for it, which the market could not build there, and
// tells the buyer nothing; its own wallet's next pass requests, builds and relays it.
func TestXMREscrowStraysPerWallet(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order, addr := x.freshCheckout(x.digital(x.vendor))
	buyer := agStrings(t, x.testEnv, "SELECT buyer_id FROM orders WHERE id=$1", order)[0]
	txid := hex64("conflicted " + order)
	x.fake.payAddress(addr, txid, xmrListingPrice, 0)
	x.fake.setTransfer(txid, func(t *fakeXMRTransfer) { t.DoubleSpend = true })
	x.mustPoll()
	agExec(x.testEnv, "UPDATE escrows SET created=now()-interval '3 days' WHERE order_id=$1", order)
	x.now = time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	x.mustPoll()
	if got := agStrings(t, x.testEnv, "SELECT name FROM xmr_escrow_wallets WHERE state IN ('active','closing','retained') ORDER BY id"); x.state(order) != stateCancelled ||
		!slices.Equal(got, []string{"escrow-202610-01", "escrow-202611-01"}) {
		t.Fatalf("cancelled %s, wallets %v", x.state(order), x.wallets())
	}
	x.fake.setTransfer(txid, func(t *fakeXMRTransfer) { t.DoubleSpend, t.Confs, t.Pool = false, 20, false })
	x.mustPoll()
	if agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND escrow_left_out", order) != 1 || len(x.xmrSettlements(order)) != 0 {
		t.Fatalf("left out at the end of the pass: settlements %v", x.xmrSettlements(order))
	}
	x.fake.fault("open_wallet", fakeXMRDrop) // October's wallet, the first one opened
	if err := x.poll(); err == nil {
		t.Fatal("a wallet that did not open passed")
	}
	if got := x.xmrSettlements(order); len(got) != 0 || x.notified(buyer, "Stray refund requested") != 0 {
		t.Fatalf("another wallet's pass requested a stray refund: %v", got)
	}
	x.mustPoll()
	if s := x.settlementOf(order, "stray_refund"); s.State != "relayed" || x.notified(buyer, "Stray refund requested") != 1 {
		t.Fatalf("its own wallet's pass: %+v", s)
	}
}
