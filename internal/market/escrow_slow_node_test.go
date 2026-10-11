package market

import (
	"context"
	"database/sql"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

// A-493 (PR #15 review, A-177, on escrow): an escrow checkout holds its buyer's account row, the payment-intake setting
// and the listing row while it asks the escrow node to build and import the escrow (escrowCheckoutAction, after every
// check). With a slow node the writers queued for those rows wait for it (waitForWalletLocks raises the pool's 3 s
// lock_timeout and 8 s statement_timeout to walletLockWait) instead of failing at 3 s; and an escrow watcher pass that
// cannot get a pool connection gives up at watcherPassTimeout instead of waiting for the pool. Fake escrow Core:
// simulated RPC, not chain evidence.

// slowNodeHold is how long the fake node takes over the first import after slowImport: longer than the pool's 3 s
// lock_timeout, shorter than walletLockWait and the 10 s RPC timeout.
const slowNodeHold = 5 * time.Second

// slowImport makes the next importdescriptors call (the last RPC of an escrow checkout, made with every row lock held)
// take slowNodeHold; entered receives once that call has started. Later imports answer at once.
func (x *escrowEnv) slowImport() <-chan struct{} {
	entered := make(chan struct{}, 1)
	var used atomic.Bool
	x.core.mu.Lock()
	x.core.beforeImport = func() {
		if used.Swap(true) {
			return
		}
		entered <- struct{}{}
		time.Sleep(slowNodeHold)
	}
	x.core.mu.Unlock()
	return entered
}

func TestEscrowCheckoutSlowNodeLetsQueuedWritersWait(t *testing.T) {
	x := newEscrowEnv(t)
	s := randomToken()[:6]
	_, admin1 := x.user("eslow_admin1_"+s, "admin") // confirmed staff actions serialise on their own actor row (A-222)
	_, admin2 := x.user("eslow_admin2_"+s, "admin")
	buyer := func(name, fp string, n byte) (string, string) {
		id, sess := x.user("eslow_"+name+"_"+s, "buyer")
		x.activeKey(id, altEscrowKey(t, testBIP48Key, fp, n))
		return id, sess
	}
	slowBuyer, slowSess := buyer("slow", "b0b0b0b0", 5)
	otherBuyer, otherSess := buyer("other", "c0c0c0c0", 6)
	lateBuyer, lateSess := buyer("late", "d0d0d0d0", 7)
	checkout := func(sess, order, label string) *httptest.ResponseRecorder {
		return x.do("POST", "/orders/escrow", sess, url.Values{"order_id": {order}, "refund_address": {regtestAddr("eslow-refund-" + label + "-" + s)}})
	}
	stock := func(product string) int { return agInt(x.testEnv, "SELECT stock FROM products WHERE id=$1", product) }
	type result struct {
		name string
		code int
		body string
		took time.Duration
	}
	// behind starts a checkout of order by sess whose node import is slow and, once the node is answering it, runs the
	// competitors at once; every competitor must wait for the checkout's rows, longer than the pool's own 3 s
	// lock_timeout, and then succeed.
	behind := func(sess, order string, competitors map[string]func() *httptest.ResponseRecorder) {
		t.Helper()
		entered := x.slowImport()
		first := make(chan *httptest.ResponseRecorder, 1)
		go func() { first <- checkout(sess, order, "slow-"+order[:8]) }()
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			t.Fatal("the checkout never reached the node's import")
		}
		started := time.Now()
		results := make(chan result, len(competitors))
		for name, run := range competitors {
			go func() {
				w := run()
				results <- result{name, w.Code, w.Body.String(), time.Since(started)}
			}()
		}
		for range competitors {
			r := <-results
			if r.code != 303 {
				t.Errorf("%s behind a checkout waiting on the node: %d %q after %v", r.name, r.code, r.body, r.took)
			} else if r.took < 3*time.Second {
				t.Errorf("%s did not wait for the checkout's rows (%v): this leg no longer exercises the lock wait", r.name, r.took)
			}
		}
		if w := <-first; w.Code != 303 {
			t.Fatalf("the slow checkout: %d %s", w.Code, w.Body.String())
		}
		if t.Failed() {
			t.FailNow()
		}
	}

	// Leg 1, the listing and the buyer: another buyer checking out the same listing (its stock reservation), the buyer
	// of an unpaid escrow of that listing cancelling it (the stock comes back), and an administrator suspending the
	// slow checkout's buyer.
	p := x.autoListing(x.vendor)
	cancelled := x.testEnv.order(x.buyer, p, "BTC", stateDraft)
	x.mustCheckout(cancelled, regtestAddr("eslow-refund-cancelled-"+s))
	slowOrder := x.testEnv.order(slowBuyer, p, "BTC", stateDraft)
	otherOrder := x.testEnv.order(otherBuyer, p, "BTC", stateDraft)
	behind(slowSess, slowOrder, map[string]func() *httptest.ResponseRecorder{
		"another buyer's checkout of the listing": func() *httptest.ResponseRecorder { return checkout(otherSess, otherOrder, "other") },
		"cancelling an unpaid escrow of the listing": func() *httptest.ResponseRecorder {
			return x.do("POST", "/orders/cancel", x.buyerSess, url.Values{"order_id": {cancelled}, "from": {stateAwaitingPayment}})
		},
		"suspending the checkout's buyer": func() *httptest.ResponseRecorder {
			return x.do("POST", "/admin/suspend", admin1, suspendForm("suspend", agStr(x.testEnv, "SELECT handle FROM users WHERE id=$1", slowBuyer)))
		},
	})
	if x.state(slowOrder) != stateAwaitingPayment || x.state(otherOrder) != stateAwaitingPayment || x.state(cancelled) != stateCancelled || stock(p) != 3 ||
		agInt(x.testEnv, "SELECT count(*) FROM users WHERE id=$1 AND suspended_at IS NOT NULL", slowBuyer) != 1 {
		t.Fatalf("after leg 1: checkouts %s %s, cancel %s, stock %d", x.state(slowOrder), x.state(otherOrder), x.state(cancelled), stock(p))
	}

	// Leg 2, the listing and the intake setting: the vendor's stock edit (stock_seen is the stock once the checkout has
	// reserved its unit, so only an edit that waited for the checkout succeeds) and an administrator pausing new Bitcoin
	// escrow checkouts.
	q := x.autoListing(x.vendor)
	lateOrder := x.testEnv.order(lateBuyer, q, "BTC", stateDraft)
	edit := inventoryForm(q, map[string]string{"kind": "digital", "category": "Digital", "price_btc": "0.001", "delivery_content": escrowDelivery, "stock": "9", "stock_seen": "4"})
	behind(lateSess, lateOrder, map[string]func() *httptest.ResponseRecorder{
		"the vendor's stock edit": func() *httptest.ResponseRecorder { return x.do("POST", "/listings/update", x.vendorSess, edit) },
		"pausing escrow checkouts": func() *httptest.ResponseRecorder {
			return x.do("POST", "/admin/payment-intake", admin2, url.Values{"currency": {"BTC"}, "enabled": {intakePaused}, "password": {testPassword}})
		},
	})
	if x.state(lateOrder) != stateAwaitingPayment || stock(q) != 9 || agStr(x.testEnv, "SELECT value FROM settings WHERE key=$1", paymentIntakeKey("BTC")) != intakePaused {
		t.Fatalf("after leg 2: checkout %s, stock %d", x.state(lateOrder), stock(q))
	}
}

// A-177: a watcher pass that cannot get a pool connection gives up at its deadline and logs the class, instead of
// waiting until the pool frees up.
func TestEscrowWatcherPassIsBounded(t *testing.T) {
	x := newEscrowEnv(t)
	defer func(d time.Duration) { watcherPassTimeout = d }(watcherPassTimeout)
	watcherPassTimeout = 500 * time.Millisecond
	logs := captureLog(t)
	var held []*sql.Conn
	for range 12 { // the application pool's size (app.go)
		c, err := x.A.db.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, c)
	}
	defer func() {
		for _, c := range held {
			c.Close()
		}
	}()
	stop := runBackground(t, x.A)
	defer stop()
	// Without the bound the pass waits for a connection until the pool frees up, here when the test ends.
	if line := waitForLine(t, logs, "escrow watcher: "); line != "escrow watcher: timeout" {
		t.Fatalf("escrow watcher line %q, want the timeout class only", line)
	}
}
