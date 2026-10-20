package market

import (
	"encoding/json"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Escrow Phase 3, slice X-c fix round (QA on escrow/phase3 @5c84afd): the Monero escrow watcher never adopts an
// orphan wallet file, keeps its stated guarantees under faults, and the checkout and order paths it owns. DB
// integration tests against the simulated wallet-rpc of escrow_xmr_fake_test.go (simulated RPC, not chain evidence).

// a402Custody is the custody sentence of owner decision O-2 (A-402), written out so that a changed constant fails.
const a402Custody = "held by this market in an account used only for this order, inside a wallet the market opens each month; the market can move these coins"

// QA F1 (P3-R10): a wallet file named like a row and opened with the market's derived password (a database restored
// from an older backup) is never taken as the month's wallet: not on a new row, and not on a row resumed from creating
// when the file holds anything create_wallet would not have left (another account, a past transfer). It is sealed and
// retained as an orphan (slice X-f), so it never offers a slot; the administrators are told and the month's next wallet
// is used. A file exactly as create_wallet leaves it (a crash after create_wallet) is resumed and sealed.
func TestXMREscrowOrphanFileNeverAdopted(t *testing.T) {
	const name = "escrow-202610-01"
	for _, c := range []struct {
		what     string
		resume   bool
		fill     func(w *fakeXMRWalletFile)
		orphaned bool
	}{
		{"a new row, a file left by a lost database", false, func(w *fakeXMRWalletFile) {}, true},
		{"a resumed row, a file with another account", true, func(w *fakeXMRWalletFile) {
			w.accounts = append(w.accounts, testXMRAddr("lost order's account", true))
		}, true},
		{"a resumed row, a file with a past transfer and no balance", true, func(w *fakeXMRWalletFile) {
			w.transfers = append(w.transfers, &fakeXMRTransfer{TxID: hex64("lost"), Amount: 5000000000, Confs: 40, DoubleSpend: true})
		}, true},
		{"a resumed row, a file with a balance", true, func(w *fakeXMRWalletFile) {
			w.transfers = append(w.transfers, &fakeXMRTransfer{TxID: hex64("lost-2"), Amount: 5000000000, Confs: 40})
		}, true},
		{"a resumed row, a file as create_wallet left it", true, func(w *fakeXMRWalletFile) {}, false},
	} {
		t.Run(c.what, func(t *testing.T) {
			x := newXMREscrowEnv(t)
			x.fake.edit(func(f *fakeXMREscrow) { c.fill(f.newFile(name, xmrWalletPassword(testXMRSecret, name))) })
			if c.resume {
				agExec(x.testEnv, "INSERT INTO xmr_escrow_wallets(name,month,state) VALUES($1,'2026-10-01','creating')", name)
			}
			x.mustPoll()
			sealed := agInt(x.testEnv, "SELECT count(*) FROM xmr_escrow_wallets WHERE name=$1 AND seed_sealed IS NOT NULL", name)
			if c.orphaned {
				x.mustPoll() // the orphan is read as any retained wallet
				file, _ := x.fake.file(name)
				if got := x.wallets(); !slices.Equal(got, []string{name + ":retained", "escrow-202610-02:active"}) || sealed != 1 || x.unseal(name) != file.mnemonic {
					t.Fatalf("wallets %v, orphan sealed %d", got, sealed)
				}
				if x.notified(x.admin, "wallet "+name+" was found") != 1 || x.slots(name, "free") != 0 || x.slots(name, "claimed") != 0 {
					t.Fatal("the orphan was not reported, or got a free slot")
				}
				return
			}
			if got := x.wallets(); !slices.Equal(got, []string{name + ":active"}) || sealed != 1 || x.slots(name, "free") != 5 {
				t.Fatalf("a genuine crash after create_wallet: %v, sealed %d", got, sealed)
			}
		})
	}
}

// QA F1 / Q33: when the network re-check refuses (a wallet-rpc 11 blocks from the daemon) on the create and seal path,
// the row stays creating and nothing is sealed.
func TestXMREscrowCreateRecheckRefused(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.fake.edit(func(f *fakeXMREscrow) { f.walletLag = 11 })
	if err := x.poll(); err == nil {
		t.Fatal("a refused re-check passed")
	}
	if got := x.wallets(); !slices.Equal(got, []string{"escrow-202610-01:creating"}) ||
		agInt(x.testEnv, "SELECT count(*) FROM xmr_escrow_wallets WHERE seed_sealed IS NOT NULL OR primary_address IS NOT NULL") != 0 {
		t.Fatalf("after a refused re-check: %v", got)
	}
}

// Q7: a free slot that receives coins is retired (no order starts funded), the administrators are told once, and a
// new free slot replaces it.
func TestXMREscrowFundedFreeSlotRetired(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	account := agInt(x.testEnv, "SELECT min(account_index) FROM xmr_escrow_slots WHERE state='free'")
	x.fake.pay("escrow-202610-01", int64(account), hex64("stray"), 1000000000, 0)
	x.mustPoll()
	x.mustPoll()
	if got := agStrings(t, x.testEnv, "SELECT state FROM xmr_escrow_slots WHERE account_index=$1", account); !slices.Equal(got, []string{"retired"}) {
		t.Fatalf("the funded free slot is %v", got)
	}
	if x.notified(x.admin, "which no order had claimed") != 1 || x.slots("escrow-202610-01", "free") != 5 {
		t.Fatalf("alerts %d, free slots %d", x.notified(x.admin, "which no order had claimed"), x.slots("escrow-202610-01", "free"))
	}
}

// Q5, Q23: an account with no row (a create_account whose answer was lost) is retired, not made free, when it is
// reconciled under the restore gate; and when get_address does not return its address, no slot is written at all.
func TestXMREscrowReconcileGuards(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	x.freshCheckout(x.digital(x.vendor))
	x.fake.fault("create_account", fakeXMRDrop)
	x.poll()
	lost := int64(agInt(x.testEnv, "SELECT max(account_index) FROM xmr_escrow_slots") + 1)
	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES('payments_recovery_required','true')")
	x.mustPoll()
	if got := agStrings(t, x.testEnv, "SELECT state FROM xmr_escrow_slots WHERE account_index=$1", lost); !slices.Equal(got, []string{"retired"}) {
		t.Fatalf("an unrecorded account reconciled under the gate is %v", got)
	}
	// Under the gate the free slots are left as they are (restore.sh retires them) and nobody is told (A-556).
	if x.slots("escrow-202610-01", "free") != 4 || x.notified(x.admin, rowlessNotice) != 0 {
		t.Fatalf("under the gate: free %d, notices %d", x.slots("escrow-202610-01", "free"), x.notified(x.admin, rowlessNotice))
	}
	agExec(x.testEnv, "UPDATE settings SET value='false' WHERE key='payments_recovery_required'")

	// Another unrecorded account whose get_address answer differs from its base address.
	var odd int64
	x.fake.edit(func(f *fakeXMREscrow) {
		w := f.files["escrow-202610-01"]
		odd = int64(len(w.accounts))
		w.accounts = append(w.accounts, testXMRAddr("odd account", true))
	})
	real := fakeXMRWalletMethods["get_address"]
	x.fake.handle("get_address", func(f *fakeXMREscrow, p json.RawMessage) (any, int64, string) {
		var q struct {
			Account int64 `json:"account_index"`
		}
		json.Unmarshal(p, &q)
		if q.Account == odd {
			other := testXMRAddr("not the odd account", true)
			return map[string]any{"address": other, "addresses": []any{map[string]any{"address": other, "address_index": 0}}}, 0, ""
		}
		return real(f, p)
	})
	if err := x.poll(); err == nil {
		t.Fatal("a get_address mismatch passed")
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM xmr_escrow_slots WHERE account_index=$1", odd); n != 0 {
		t.Fatalf("%d slots written for an account whose address does not match", n)
	}
}

// Q22: each wallet pass ends with store.
func TestXMREscrowPassStores(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	before := x.fake.countOf("store")
	x.mustPoll()
	if got := x.fake.countOf("store") - before; got != 1 {
		t.Fatalf("one wallet pass called store %d times", got)
	}
}

// NIT 1: a market down from the lead window until after the next month closes the wallet it sealed for a month that
// never became active, so it is not left sealed (and read) for good. With no order, both closed wallets are retained
// (slice X-f, P3-R9).
func TestXMREscrowStrandedSealedCloses(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	x.now = time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	x.mustPoll()
	x.now = time.Date(2026, 12, 3, 0, 0, 0, 0, time.UTC)
	x.mustPoll()
	if got := x.wallets(); !slices.Equal(got, []string{"escrow-202610-01:retained", "escrow-202611-01:retained", "escrow-202612-01:active"}) {
		t.Fatalf("after a gap: %v", got)
	}
}

// NIT 2: a deposit's stored confirmations stop at the ceiling (100), so a deep deposit's row is not rewritten on every
// block; a shallower reading after a reorganisation is still stored; the page says "or more" at the ceiling.
func TestXMREscrowConfirmationsCeiling(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order := x.draft(x.digital(x.vendor))
	addr := x.mustCheckout(order)
	x.fake.payAddress(addr, hex64("deep"), xmrListingPrice, 150)
	x.mustPoll()
	row := func() string {
		return agStrings(t, x.testEnv, "SELECT confirmations||' '||updated FROM payments WHERE order_id=$1", order)[0]
	}
	first := row()
	if !strings.HasPrefix(first, "100 ") || x.state(order) != stateCompleted {
		t.Fatalf("a deposit at 150 confirmations is stored as %q (%s)", first, x.state(order))
	}
	x.fake.setConfs(hex64("deep"), 160)
	x.mustPoll()
	if got := row(); got != first {
		t.Fatalf("the row changed above the ceiling: %q then %q", first, got)
	}
	if p := x.panel(order, x.buyerSess); !strings.Contains(p, "Confirmed (100 confirmations or more)") {
		t.Fatalf("panel:\n%s", p)
	}
	x.fake.setConfs(hex64("deep"), 40)
	x.mustPoll()
	if got := row(); !strings.HasPrefix(got, "40 ") {
		t.Fatalf("a reorganised deposit: %q", got)
	}
}

// F2 (A-405): a funded Monero escrow order on a listing an administrator took down is not delivered, and its history
// note names the market's refund, never a signature.
func TestXMREscrowTakedownNote(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	p := x.digital(x.vendor)
	order := x.draft(p)
	addr := x.mustCheckout(order)
	agExec(x.testEnv, "UPDATE products SET archived=true,admin_archive_reason='Taken down for review.' WHERE id=$1", p)
	x.fake.payAddress(addr, hex64("taken-down"), xmrListingPrice, 10)
	x.mustPoll()
	notes := strings.Join(agStrings(t, x.testEnv, "SELECT note FROM order_events WHERE order_id=$1", order), "\n")
	if x.state(order) != statePaid || !strings.Contains(notes, "refunded to the buyer's refund address by a transaction the market builds and sends") || strings.Contains(notes, "sign") {
		t.Fatalf("%s:\n%s", x.state(order), notes)
	}
}

// Q27: cancelling a Monero escrow order that holds a deposit requests its refund in xmr_settlements, to the refund
// address pinned at checkout.
func TestXMREscrowCancelRequestsRefund(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order := x.draft(x.digital(x.vendor))
	addr := x.mustCheckout(order)
	x.fake.payAddress(addr, hex64("part"), xmrListingPrice/4, 12)
	x.mustPoll()
	w := x.do("POST", "/orders/cancel", x.buyerSess, url.Values{"order_id": {order}, "from": {stateAwaitingPayment},
		"amount_seen": {strconv.FormatInt(xmrListingPrice/4, 10)}, "payout_confirmed": {"confirmed"}})
	if w.Code != 303 || x.state(order) != stateCancelled {
		t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
	}
	if got := agStrings(t, x.testEnv, "SELECT kind||' '||state||' '||destination FROM xmr_settlements WHERE order_id=$1", order); !slices.Equal(got, []string{"refund requested " + testXMRAddr("refund "+order, true)}) {
		t.Fatalf("settlements %v", got)
	}
}

// Q28, Q24, Q25: physical Monero escrow checkout is refused (A-83) while the vendor cannot receive an encrypted
// shipping address. Another account's claim history is private and cannot block
// this buyer's refund address.
func TestXMREscrowCheckoutRefusalsMore(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	x.setKey(x.vendor, "") // intentional legacy vendor missing the now-required public key
	phys := x.testEnv.product(x.vendor, "physical")
	agExec(x.testEnv, "UPDATE products SET dispatch_days=3 WHERE id=$1", phys)
	order := x.draft(phys)
	if w := x.checkout(x.buyerSess, order, testXMRAddr("phys refund", true)); w.Code != 409 || !strings.Contains(w.Body.String(), "Vendors must keep a usable PGP public key") || x.state(order) != stateDraft {
		t.Fatalf("a vendor with no encryption key: %d %s", w.Code, w.Body.String())
	}

	_, restoredPublic := testPGPKey(t, "restored-vendor-fixture")
	x.pgpSession(x.vendorSess, restoredPublic)
	first := x.draft(x.digital(x.vendor))
	x.mustCheckout(first)
	claim := testXMRAddr("a vendor's claim", true)
	agExec(x.testEnv, "INSERT INTO xmr_settlements(order_id,kind,destination) VALUES($1,'release',$2)", first, claim)
	for what, addr := range map[string]string{"a settlement destination": claim} {
		order := x.draft(x.digital(x.vendor))
		if w := x.checkout(x.buyerSess, order, addr); w.Code != 303 || x.state(order) != stateAwaitingPayment {
			t.Fatalf("%s: %d %s", what, w.Code, w.Body.String())
		}
	}
}
