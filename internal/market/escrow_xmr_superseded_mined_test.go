package market

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// A-566 (PAY66-3): a failed Monero settlement with a stored transaction is re-requested, and the superseded
// transaction, which the daemon had refused or lost, reaches the network after all. Both spend exactly the same key
// images, so only one can be mined: the market records that its own earlier transaction was sent (the superseded row
// back to relayed, then followed to confirmed and 100 confirmations, the deposit claims back on it) and closes the
// re-request without building or relaying it again, with notes that say so. A spend by a transaction the market did not
// make is still external. DB integration against the simulated wallet RPC (not chain evidence; the real fakechain leg is
// TestXMREscrowRegtestSupersededMined).

// supersededSentNote is part of the order note the market writes when a superseded transaction is found on the network.
const supersededSentNote = "is on the network after all"

// reRequested is a completed order whose release the daemon refused for good, re-requested by an administrator and
// carried out on the next pass with prepare run first (to hold the new settlement at a step), and the failed row and
// the new one.
func (x *xmrEscrowEnv) reRequested(prepare func()) (order string, old, next xmrSettlement) {
	x.t.Helper()
	order, id := x.failedRelease()
	old = x.settlement(id)
	_, adminSess := x.user("a566_admin_"+randomToken()[:6], "admin")
	if code := x.reRequest(adminSess, order, id, testPassword); code != 303 {
		x.t.Fatalf("re-request: %d", code)
	}
	if prepare != nil {
		prepare()
	}
	x.poll() // a held step may report its error (a relay that answered -13)
	next = x.settlementOf(order, "release")
	if next.Supersedes != id || x.settlement(id).State != "superseded" {
		x.t.Fatalf("re-request not carried out: old %+v new %+v", x.settlement(id), next)
	}
	return order, old, next
}

// sendStored puts settlement s's stored transaction in the daemon's pool (as a peer that kept it would send it back) and
// mines it into a block; spent false leaves the wallet's outputs listed as unspent (a wallet behind the chain).
func (x *xmrEscrowEnv) sendStored(s xmrSettlement, spent bool) {
	x.fake.edit(func(f *fakeXMREscrow) {
		tx := f.built[hexMeta(f, s.TxHash)]
		if spent {
			f.relay(tx)
		} else {
			tx.Height = 0
			f.txs[tx.Hash] = tx
		}
	})
	x.fake.mine(1)
}

// adopted checks that order's superseded settlement old is the one recorded sent (state) and next is closed, with every
// deposit claimed by old, one accurate order note, no external note, no outside-spend notice to the parties and the
// re-requesting administrator told.
func (x *xmrEscrowEnv) adopted(order string, old, next xmrSettlement, state string) {
	x.t.Helper()
	got, closed := x.settlement(old.ID), x.settlement(next.ID)
	if got.State != state || got.TxHash != old.TxHash || closed.State != "superseded" || closed.Supersedes != old.ID {
		x.t.Fatalf("after the superseded transaction was mined: old %+v (want %s), new %+v; settlements %v", got, state, closed, x.xmrSettlements(order))
	}
	if x.claimsOf(old.ID) != 1 || x.claimsOf(next.ID) != 0 {
		x.t.Fatalf("deposit claims: old %d, new %d", x.claimsOf(old.ID), x.claimsOf(next.ID))
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND strpos(note,$2)>0", order, supersededSentNote); n != 1 {
		x.t.Fatalf("%d order notes say the superseded transaction is on the network", n)
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND strpos(note,'did not make')>0", order); n != 0 {
		x.t.Fatalf("%d order notes say a transaction the market did not make spent the coins", n)
	}
	note := agStr(x.testEnv, "SELECT note FROM order_events WHERE order_id=$1 AND strpos(note,$2)>0", order, supersededSentNote)
	for _, want := range []string{"settlement " + strconv.FormatInt(old.ID, 10), old.TxHash, "settlement " + strconv.FormatInt(next.ID, 10), "Nothing is paid twice", "the vendor (the claim, less the network fee): 0."} {
		if !strings.Contains(note, want) {
			x.t.Fatalf("the order note does not say %q:\n%s", want, note)
		}
	}
	mustNotContain(x.t, note, "rpc:", "error", "wallet-rpc", "monerod", "refused", "Moderator review required")
	buyer := agStrings(x.t, x.testEnv, "SELECT buyer_id FROM orders WHERE id=$1", order)[0]
	if x.notified(buyer, "did not make") != 0 || x.notified(buyer, "on the network after all") != 1 {
		x.t.Fatalf("buyer's notices: outside spend %d, sent after all %d", x.notified(buyer, "did not make"), x.notified(buyer, "on the network after all"))
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM notifications n JOIN users u ON u.id=n.user_id WHERE u.role='admin' AND n.audience='admin' AND strpos(n.body,$1)>0", supersededSentNote); n != 1 {
		x.t.Fatalf("%d administrator notices of the superseded transaction sent", n)
	}
	if p := x.panel(order, x.sessOf(order, roleVendor)); !strings.Contains(p, "Closed: the earlier transaction of settlement "+strconv.FormatInt(old.ID, 10)) ||
		!strings.Contains(p, old.TxHash) || strings.Contains(p, "Spent outside the market") {
		x.t.Fatalf("vendor's panel:\n%s", p)
	}
}

// The superseded transaction is mined while its re-request is at each step: requested (the wallet already lists the
// coins spent, and one behind the chain that still lists them), built and not relayed, and relayed but lost by the
// daemon. Each time the old row is recorded sent and the new one closed; the new one is never built, nor relayed, from
// then on. Mined to the threshold and then to 100 confirmations, the old row is confirmed and reorganisation-safe.
func TestXMREscrowSupersededMinedAfterReRequest(t *testing.T) {
	t.Run("requested", func(t *testing.T) {
		x := newXMREscrowEnv(t)
		x.mustPoll()
		restore := func() {}
		order, old, next := x.reRequested(func() { restore = x.refuseTransfer(-4, "transaction was rejected by daemon") })
		restore()
		if next.State != "requested" {
			t.Fatalf("held re-request: %+v", next)
		}
		x.sendStored(old, true)
		transfers, relays := x.fake.countOf("transfer"), x.fake.countOf("relay_tx")
		x.mustPoll()
		if x.fake.countOf("transfer") != transfers || x.fake.countOf("relay_tx") != relays {
			t.Fatalf("transfer %d, relay_tx %d after the superseded transaction was mined", x.fake.countOf("transfer")-transfers, x.fake.countOf("relay_tx")-relays)
		}
		x.adopted(order, old, next, "relayed")
		if c := x.waitClass(next.ID); c != "" {
			t.Fatalf("closed re-request keeps wait class %q", c)
		}
		// Repeated passes record nothing more; mined to the threshold and past 100 the old row is confirmed and safe.
		x.mustPoll()
		x.mustPoll()
		x.adopted(order, old, next, "relayed")
		x.fake.mine(12)
		x.mustPoll()
		x.adopted(order, old, next, "confirmed")
		x.fake.mine(100)
		x.mustPoll()
		if agInt(x.testEnv, "SELECT count(*) FROM xmr_settlements WHERE id=$1 AND reorg_safe_at IS NOT NULL", old.ID) != 1 {
			t.Fatal("the superseded transaction mined after all is not followed to 100 confirmations")
		}
		if x.fake.countOf("transfer") != transfers || x.fake.countOf("relay_tx") != relays {
			t.Fatal("the closed re-request was built or relayed")
		}
	})
	t.Run("requested, wallet behind", func(t *testing.T) {
		x := newXMREscrowEnv(t)
		x.mustPoll()
		restore := func() {}
		order, old, next := x.reRequested(func() { restore = x.refuseTransfer(-4, "transaction was rejected by daemon") })
		restore()
		x.sendStored(old, false) // the wallet still lists the coins as unspent
		transfers, relays := x.fake.countOf("transfer"), x.fake.countOf("relay_tx")
		x.mustPoll()
		if x.fake.countOf("transfer") != transfers || x.fake.countOf("relay_tx") != relays {
			t.Fatalf("transfer %d, relay_tx %d: the re-request was built although its superseded transaction was mined", x.fake.countOf("transfer")-transfers, x.fake.countOf("relay_tx")-relays)
		}
		x.adopted(order, old, next, "relayed")
	})
	t.Run("built", func(t *testing.T) {
		x := newXMREscrowEnv(t)
		x.mustPoll()
		order, old, next := x.reRequested(func() { x.fake.fault("relay_tx", fakeXMRNotOpen) })
		if next.State != "built" {
			t.Fatalf("re-request not held built: %+v", next)
		}
		x.sendStored(old, true)
		relays := x.fake.countOf("relay_tx")
		x.mustPoll()
		if x.fake.countOf("relay_tx") != relays {
			t.Fatalf("relay_tx %d: the built re-request was relayed although its superseded transaction was mined", x.fake.countOf("relay_tx")-relays)
		}
		x.adopted(order, old, next, "relayed")
	})
	t.Run("relayed", func(t *testing.T) {
		x := newXMREscrowEnv(t)
		x.mustPoll()
		order, old, next := x.reRequested(nil)
		if next.State != "relayed" {
			t.Fatalf("re-request not relayed: %+v", next)
		}
		x.fake.flushPool(next.TxHash)
		x.sendStored(old, true)
		relays := x.fake.countOf("relay_tx")
		x.mustPoll()
		if x.fake.countOf("relay_tx") != relays {
			t.Fatal("the lost re-request was relayed again")
		}
		x.adopted(order, old, next, "relayed")
	})
}

// A stray refund's re-request that was never built is closed the same way (migration 144 lets a re-request without a
// transaction be superseded whatever its kind).
func TestXMREscrowSupersededMinedStrayReRequest(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, admin := x.user("a566s_admin_"+randomToken()[:6], "admin")
	order, _, _, _, first := x.strayFailed()
	again := x.waitingStrayReRequest(admin, order, first)
	old := x.settlement(first)
	x.sendStored(old, true)
	transfers := x.fake.countOf("transfer")
	x.mustPoll()
	if s, n := x.settlement(first), x.settlement(again); s.State != "relayed" || n.State != "superseded" || x.claimsOf(first) != 2 || x.claimsOf(again) != 0 ||
		x.fake.countOf("transfer") != transfers {
		t.Fatalf("stray refund: old %+v, new %+v, claims %d/%d, transfer %d", s, n, x.claimsOf(first), x.claimsOf(again), x.fake.countOf("transfer")-transfers)
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND strpos(note,$2)>0", order, supersededSentNote); n != 1 {
		t.Fatalf("%d notes", n)
	}
}

// PAY566-1: the superseded stray refund is revived while a later deposit's first-generation stray refund waits
// requested (A-616 allows the two side by side). Migration 144's one_open_stray covers requested and built only, so the
// revival to relayed is recorded on the first pass with no error; the later stray refund is untouched.
func TestXMREscrowSupersededMinedStrayBesideLaterStray(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, admin := x.user("a566l_admin_"+randomToken()[:6], "admin")
	order, addr, _, _, first := x.strayFailed()
	restore := x.refuseTransfer(-4, "transaction was rejected by daemon") // the later stray refund keeps waiting
	d2, later := x.laterStray(order, addr, false)
	restore()
	again := x.waitingStrayReRequest(admin, order, first)
	restore = x.refuseTransfer(-4, "transaction was rejected by daemon")
	defer restore()
	x.sendStored(x.settlement(first), true)
	if err := x.poll(); err != nil {
		t.Fatalf("the pass that finds the superseded stray refund mined: %v", err)
	}
	if s, n, l := x.settlement(first), x.settlement(again), x.settlement(later); s.State != "relayed" || n.State != "superseded" || l.State != "requested" ||
		x.claimsOf(first) != 2 || x.claimsOf(again) != 0 || x.claimsOf(later) != 0 ||
		agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND txid=$2 AND xmr_settlement_id IS NULL", order, d2) != 1 {
		t.Fatalf("old %+v, re-request %+v, later %+v, claims %d/%d/%d; settlements %v", s, n, l, x.claimsOf(first), x.claimsOf(again), x.claimsOf(later), x.xmrSettlements(order))
	}
	if err := x.poll(); err != nil {
		t.Fatalf("a later pass: %v", err)
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND strpos(note,$2)>0", order, supersededSentNote); n != 1 {
		t.Fatalf("%d notes", n)
	}
}

// A-566 acceptance 4: after a re-request, a spend by a transaction the market did not make (not the superseded one's)
// is still recorded external on the re-request, as before; the superseded row stays superseded.
func TestXMREscrowReRequestGenuinelyExternal(t *testing.T) {
	for _, step := range []string{"built", "relayed"} {
		t.Run(step, func(t *testing.T) {
			x := newXMREscrowEnv(t)
			x.mustPoll()
			var prepare func()
			if step == "built" {
				prepare = func() { x.fake.fault("relay_tx", fakeXMRNotOpen) }
			}
			order, old, next := x.reRequested(prepare)
			if next.State != step {
				t.Fatalf("re-request: %+v", next)
			}
			if step == "relayed" {
				x.fake.flushPool(next.TxHash)
			}
			x.fake.spendOutside(next.KeyImages...)
			x.mustPoll()
			if got := x.settlement(next.ID); got.State != "external" || x.settlement(old.ID).State != "superseded" {
				t.Fatalf("outside spend: new %+v, old %+v", got, x.settlement(old.ID))
			}
			buyer := agStrings(t, x.testEnv, "SELECT buyer_id FROM orders WHERE id=$1", order)[0]
			if x.notified(buyer, "spent by a transaction this market did not make") != 1 ||
				agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND strpos(note,$2)>0", order, supersededSentNote) != 0 ||
				agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND flagged AND xmr_settlement_id=$2", order, next.ID) != 1 {
				t.Fatal("the outside spend was not recorded as before")
			}
		})
	}
}

// Concurrent watcher steps that each find the superseded transaction on the network record one outcome: one note, one
// notice each, the rows moved once.
func TestXMREscrowSupersededMinedConcurrent(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order, old, next := x.reRequested(func() { x.fake.fault("relay_tx", fakeXMRNotOpen) })
	x.sendStored(old, true)
	row := &xmrSettleRow{ID: next.ID, OrderID: order, Kind: next.Kind, State: next.State, Supersedes: old.ID, TxHash: next.TxHash, KeyImages: next.KeyImages}
	var wg sync.WaitGroup
	results := make([]bool, 6)
	errs := make([]error, 6)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := *row
			results[i], errs[i] = x.A.xmrSupersededSent(context.Background(), &r)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil || !results[i] {
			t.Fatalf("call %d: %v %v", i, results[i], err)
		}
	}
	x.adopted(order, old, next, "relayed")
	x.mustPoll()
	x.adopted(order, old, next, "relayed")
}

// The database half (migration 144): a superseded settlement's stored transaction is recorded relayed again only once
// every re-request after it is closed, and a built or relayed re-request is closed only when, by commit, a settlement it
// re-requests is recorded relayed or confirmed; a built settlement that re-requests nothing is never superseded.
func TestMigration144SupersededSent(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	if n := agInt(x.testEnv, "SELECT count(*) FROM schema_migrations WHERE version=144 AND name='xmr_superseded_sent'"); n != 1 {
		t.Fatalf("migration 144 recorded %d times", n)
	}
	_, old, next := x.reRequested(func() { x.fake.fault("relay_tx", fakeXMRNotOpen) })
	exec := func(q string, args ...any) error {
		tx, err := x.DB.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		for _, s := range strings.Split(q, ";") {
			if _, err = tx.Exec(s, args...); err != nil {
				return err
			}
		}
		return tx.Commit()
	}
	if err := exec("UPDATE xmr_settlements SET state='relayed',relayed_at=now() WHERE id=$1 AND $2::bigint>0", old.ID, next.ID); err == nil || !strings.Contains(err.Error(), "cannot move") {
		t.Fatalf("superseded -> relayed while its re-request is built: %v", err)
	}
	if err := exec("UPDATE xmr_settlements SET state='superseded' WHERE id=$1 AND $2::bigint>0", next.ID, old.ID); err == nil || !strings.Contains(err.Error(), "relayed or confirmed") {
		t.Fatalf("a built re-request closed alone: %v", err)
	}
	if err := exec("UPDATE xmr_settlements SET state='superseded' WHERE id=$1 AND $2::bigint>0;UPDATE xmr_settlements SET state='relayed',relayed_at=COALESCE(relayed_at,now()) WHERE id=$2 AND $1::bigint>0", next.ID, old.ID); err != nil {
		t.Fatalf("closing the re-request and recording the superseded transaction sent: %v", err)
	}
	other, _, _ := x.completed()
	id, _ := x.release(other)
	x.fake.fault("relay_tx", fakeXMRNotOpen)
	x.poll() // the held relay reports -13
	if x.settlement(id).State != "built" {
		t.Fatalf("release: %+v", x.settlement(id))
	}
	if _, err := x.DB.Exec("UPDATE xmr_settlements SET state='superseded' WHERE id=$1", id); err == nil || !strings.Contains(err.Error(), "cannot move") {
		t.Fatalf("a built first-generation release superseded: %v", err)
	}
}
