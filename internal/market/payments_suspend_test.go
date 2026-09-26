package market

import (
	"context"
	"html"
	"net/url"
	"strings"
	"testing"
	"time"
)

// A-102: suspending an account (A-27) holds its unsent payouts, so a payout address changed by someone with
// the account's password is not paid after the administrator steps in. Such a hold is released only by an
// administrator who confirms the payout address was checked; restoring the account and the watcher leave it.

// sendsTo counts the fake wallet's sends to addr.
func (p *payEnv) sendsTo(addr string) int {
	n := 0
	for _, s := range p.fake.Sends() {
		if s.To == addr {
			n++
		}
	}
	return n
}

// payoutError returns the payout's state and error for order.
func (p *payEnv) payoutError(order string) (state, errText string) {
	p.t.Helper()
	if err := p.DB.QueryRow("SELECT state,error FROM payouts WHERE order_id=$1", order).Scan(&state, &errText); err != nil {
		p.t.Fatal(err)
	}
	return
}

// suspendedHeld fails unless order's payout is held for the account suspension.
func (p *payEnv) suspendedHeld(when string, orders ...string) {
	p.t.Helper()
	for _, order := range orders {
		if st, e := p.payoutError(order); st != "held" || !strings.HasPrefix(e, "Suspended account:") {
			p.t.Fatalf("%s: payout for %s is %s %q, want held for the suspension", when, order[:8], st, e)
		}
	}
}

func TestPayoutAddressChangeNotifiesOwner(t *testing.T) {
	p := newPayEnv(t)
	p.paidByWatcher(p.order, p.addr, "tx-notify")
	p.move(p.order, statePaid, stateShipped, p.vendor)
	p.move(p.order, stateShipped, stateCompleted, p.buyer)
	if st, _, _, _ := p.payout(p.order); st != "blocked" {
		t.Fatalf("payout without an address: %s", st)
	}
	notes := func() []string {
		t.Helper()
		rows, err := p.DB.Query("SELECT body FROM notifications WHERE user_id=$1 AND body LIKE 'Your BTC payout address%' ORDER BY created", p.vendor.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var b string
			if err = rows.Scan(&b); err != nil {
				t.Fatal(err)
			}
			out = append(out, b)
		}
		return out
	}
	p.check(p.do("POST", "/account/payout", p.vendorSess, url.Values{"currency": {"BTC"}, "address": {"fake-testnet-changed"}, "password": {testPassword}}), 303)
	got := notes()
	if len(got) != 1 {
		t.Fatalf("notifications after saving a payout address: %q", got)
	}
	for _, bad := range []string{"fake-testnet-changed", "http", "/", ".onion"} {
		if strings.Contains(got[0], bad) {
			t.Fatalf("payout address notification contains %q: %s", bad, got[0])
		}
	}
	mustContain(t, got[0], "Your BTC payout address was changed", "1 waiting payout(s) now use it", "If you did not make this change")
	// A refused save (wrong password) changes nothing and notifies nobody.
	p.check(p.do("POST", "/account/payout", p.vendorSess, url.Values{"currency": {"BTC"}, "address": {"fake-testnet-other"}, "password": {"wrong-password-here"}}), 401)
	if len(notes()) != 1 {
		t.Fatal("a refused payout address change notified the owner")
	}
	// Removing it is announced too.
	p.check(p.do("POST", "/account/payout", p.vendorSess, url.Values{"currency": {"BTC"}, "address": {""}, "password": {testPassword}}), 303)
	if got = notes(); len(got) != 2 || !strings.Contains(got[1], "Your BTC payout address was removed") {
		t.Fatalf("notifications after removing the payout address: %q", got)
	}
}

func TestSuspensionHoldsPayoutsUntilAddressChecked(t *testing.T) {
	p := newPayEnv(t)
	// Blocked (no address), pending and watcher-held releases to the vendor; a pending refund to the buyer; and
	// a shipped order completed after the suspension. Every deposit is confirmed before any payout is queued,
	// so no watcher pass runs while a payout is pending.
	orders := map[string]string{}
	for _, name := range []string{"blocked", "pending", "watcherHeld", "refund", "later"} {
		order, addr := p.newOrder(stateAwaitingPayment)
		p.paidByWatcher(order, addr, "tx-s-"+name)
		if name != "refund" {
			p.move(order, statePaid, stateShipped, p.vendor)
		}
		orders[name] = order
	}
	blocked, pending, watcherHeld, refund, later := orders["blocked"], orders["pending"], orders["watcherHeld"], orders["refund"], orders["later"]
	p.move(blocked, stateShipped, stateCompleted, p.buyer)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	p.move(pending, stateShipped, stateCompleted, p.buyer)
	p.move(watcherHeld, stateShipped, stateCompleted, p.buyer)
	if _, err := p.DB.Exec("UPDATE payouts SET state='held',error=$2 WHERE order_id=$1", watcherHeld, heldReason); err != nil {
		t.Fatal(err)
	}
	p.setPayoutAddress(p.buyer, "fake-testnet-buyer")
	p.move(refund, statePaid, stateCancelled, p.vendor)
	for order, want := range map[string]string{blocked: "blocked", pending: "pending", refund: "pending"} {
		if st, _, _, _ := p.payout(order); st != want {
			t.Fatalf("payout for %s before suspension: %s, want %s", order[:8], st, want)
		}
	}

	p.check(p.do("POST", "/admin/suspend", p.adminSess, suspendForm("suspend", p.vendor.Handle)), 303)
	p.suspendedHeld("after suspension", blocked, pending, watcherHeld)
	if st, _, _, _ := p.payout(refund); st != "pending" {
		t.Fatalf("suspension held another account's payout: %s", st)
	}
	if n := p.count("SELECT count(*) FROM audit_events WHERE action LIKE $1", "Suspended account "+p.vendor.Handle+"; ended % session(s) and any pending sign-ins; held 3 unsent payout(s)%"); n != 1 {
		t.Fatalf("suspension audit naming the held payouts: %d", n)
	}
	// Two watcher passes: the buyer's refund goes, nothing goes to the vendor, the holds stay (the watcher-held
	// payout's deposit is confirmed, so the watcher's own hold would have been lifted).
	p.poll()
	p.poll()
	if p.sendsTo("fake-testnet-buyer") != 1 || p.sendsTo("fake-testnet-vendor") != 0 || len(p.fake.Sends()) != 1 {
		t.Fatalf("sends after suspension: %+v", p.fake.Sends())
	}
	p.suspendedHeld("after two watcher passes", blocked, pending, watcherHeld)

	// An order completed after the suspension queues its payout already held.
	p.move(later, stateShipped, stateCompleted, p.buyer)
	p.suspendedHeld("completed after suspension", later)
	if n := p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE '%held until an administrator checks the payout address%'", later); n != 1 {
		t.Fatalf("order history for the held payout: %d", n)
	}
	p.poll()
	if p.sendsTo("fake-testnet-vendor") != 0 {
		t.Fatalf("payout queued after suspension was sent: %+v", p.fake.Sends())
	}

	// /admin names the hold and asks for the address check (showing the address) before releasing.
	pendingID := p.payoutID(pending)
	admin := html.UnescapeString(p.page("/admin", p.adminSess))
	mustContain(t, admin, "Held: account suspended — check the payout address before releasing")
	s := resolveSection(t, admin, pendingID)
	mustContain(t, s, `name="address_checked" value="confirmed" required`, "fake-testnet-vendor", "Release held payout")
	mustNotContain(t, s, "not_broadcast")

	// Restoring the account releases nothing.
	p.check(p.do("POST", "/admin/suspend", p.adminSess, suspendForm("restore", p.vendor.Handle)), 303)
	p.poll()
	p.suspendedHeld("after restore", blocked, pending, watcherHeld, later)
	if p.sendsTo("fake-testnet-vendor") != 0 {
		t.Fatalf("restore released a payout: %+v", p.fake.Sends())
	}

	// A hold with no payout address has nothing to check: no release form, and a release is refused until the
	// recipient saves one, which the payout then gains while staying held.
	blockedID := p.payoutID(blocked)
	s = resolveSection(t, html.UnescapeString(p.page("/admin", p.adminSess)), blockedID)
	mustContain(t, s, "has no payout address")
	mustNotContain(t, s, `value="release"`, "address_checked")
	w := p.do("POST", "/admin/payout", p.adminSess, url.Values{"payout_id": {blockedID}, "op": {"release"}, "password": {testPassword}, "address_checked": {"confirmed"}})
	if w.Code != 409 || !strings.Contains(w.Body.String(), "no payout address") {
		t.Fatalf("release of a hold with no address: %d %s", w.Code, w.Body.String())
	}
	p.check(p.do("POST", "/account/payout", agSession(p.testEnv, p.vendor.ID), url.Values{"currency": {"BTC"}, "address": {"fake-testnet-owner"}, "password": {testPassword}}), 303)
	p.suspendedHeld("after the owner saved an address", blocked)
	if _, _, addr, _ := p.payout(blocked); addr != "fake-testnet-owner" {
		t.Fatalf("held payout did not gain the saved address: %q", addr)
	}

	// Release without the address check (with the password, or with the wrong checkbox): 409, nothing changes.
	for _, extra := range []url.Values{{}, {"not_broadcast": {"confirmed"}}, {"address_checked": {"yes"}}} {
		f := url.Values{"payout_id": {pendingID}, "op": {"release"}, "password": {testPassword}}
		for k, v := range extra {
			f[k] = v
		}
		w := p.do("POST", "/admin/payout", p.adminSess, f)
		if w.Code != 409 || !strings.Contains(w.Body.String(), "payout address") {
			t.Fatalf("release without the address check (%v): %d %s", extra, w.Code, w.Body.String())
		}
	}
	p.suspendedHeld("after refused releases", pending)
	if n := p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE 'Administrator%'", pending); n != 0 {
		t.Fatalf("refused release recorded %d order events", n)
	}
	if n := p.count("SELECT count(*) FROM audit_events WHERE action LIKE 'Released held payout%'"); n != 0 {
		t.Fatalf("refused release audited %d times", n)
	}
	// A wrong password with the check is refused too.
	w = p.do("POST", "/admin/payout", p.adminSess, url.Values{"payout_id": {pendingID}, "op": {"release"}, "password": {"wrong-password-here"}, "address_checked": {"confirmed"}})
	if w.Code != 401 {
		t.Fatalf("release with a wrong password: %d", w.Code)
	}
	p.suspendedHeld("after a wrong password", pending)

	// With the check: queued, audited with the address, sent once.
	w = p.do("POST", "/admin/payout", p.adminSess, url.Values{"payout_id": {pendingID}, "op": {"release"}, "password": {testPassword}, "address_checked": {"confirmed"}})
	if w.Code != 303 {
		t.Fatalf("release with the address check: %d %s", w.Code, w.Body.String())
	}
	if st, _, _, _ := p.payout(pending); st != "pending" {
		t.Fatalf("released payout is %s", st)
	}
	if n := p.count("SELECT count(*) FROM audit_events WHERE action LIKE $1", "Released held payout "+pendingID+" %held for an account suspension, administrator checked the payout address fake-testnet-vendor%(confirmed with password)"); n != 1 {
		t.Fatalf("audit for the checked release: %d", n)
	}
	if n := p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE 'Administrator checked the payout address%'", pending); n != 1 {
		t.Fatalf("order history for the checked release: %d", n)
	}
	p.poll()
	p.poll()
	if p.sendsTo("fake-testnet-vendor") != 1 {
		t.Fatalf("released payout sends: %+v", p.fake.Sends())
	}
	if st, _, _, _ := p.payout(pending); st != "sent" {
		t.Fatalf("released payout is %s after the watcher", st)
	}
	p.suspendedHeld("the other holds after one release", blocked, watcherHeld, later)
	// Releasing it again changes nothing.
	w = p.do("POST", "/admin/payout", p.adminSess, url.Values{"payout_id": {pendingID}, "op": {"release"}, "password": {testPassword}, "address_checked": {"confirmed"}})
	if w.Code != 409 {
		t.Fatalf("second release: %d", w.Code)
	}
}

// A payout queued while the account is being suspended is held: whether queued by an order transition or by
// the watcher (a refund for a deposit that confirmed after cancellation), it waits for the suspension's lock on
// the account and then sees the suspension. On the watcher path nothing locks the account row before
// enqueuePayout's own read, so that case fails if the read does not lock.
func TestPayoutQueuedDuringSuspensionIsHeld(t *testing.T) {
	p := newPayEnv(t)
	ctx := context.Background()
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	p.setPayoutAddress(p.buyer, "fake-testnet-buyer")
	completed := p.order
	p.paidByWatcher(completed, p.addr, "tx-s-race")
	p.move(completed, statePaid, stateShipped, p.vendor)
	refund, refundAddr := p.newOrder(stateCancelled)
	for _, c := range []struct {
		name, order string
		recipient   *User
		queue       func() error
	}{
		{"completion", completed, p.vendor, func() error {
			tx, err := p.DB.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			if _, err = p.A.transition(ctx, tx, completed, stateShipped, stateCompleted, p.buyer, "test"); err != nil {
				return err
			}
			return tx.Commit()
		}},
		{"watcher refund", refund, p.buyer, func() error {
			p.fake.Deposit(refundAddr, "tx-s-late-refund", 0, 100000, 3)
			return p.A.pollOnce(ctx)
		}},
	} {
		susp, err := p.DB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		var pid int
		// The lock adminSuspendAction takes on the account row.
		if err = susp.QueryRow("SELECT pg_backend_pid() FROM users WHERE id=$1 FOR NO KEY UPDATE", c.recipient.ID).Scan(&pid); err != nil {
			t.Fatal(err)
		}
		if _, err = susp.Exec("UPDATE users SET suspended_at=now() WHERE id=$1", c.recipient.ID); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- c.queue() }()
		// Wait until the queueing blocks on the suspension's lock on the account (or finishes without waiting).
		finished := false
		deadline := time.Now().Add(10 * time.Second)
		for !finished && p.count("SELECT count(*) FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid))", pid) == 0 {
			select {
			case err = <-done:
				finished = true
			case <-time.After(20 * time.Millisecond):
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: neither waited for the account lock nor finished", c.name)
			}
		}
		if err = susp.Commit(); err != nil {
			t.Fatal(err)
		}
		if !finished {
			err = <-done
		}
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		p.suspendedHeld(c.name+" during suspension", c.order)
	}
	if len(p.fake.Sends()) != 0 {
		t.Fatalf("payouts queued during suspension were sent: %+v", p.fake.Sends())
	}
}

// Suspending holds the account's payouts after locking its row. A watcher pass that already holds one of those
// payouts and then records a notification for the account (a row referencing it) must not deadlock with it:
// the suspension's lock on the account does not exclude such inserts.
func TestSuspensionDoesNotDeadlockWithWatcherHoldingPayout(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	order := p.completedWithPayout("tx-s-deadlock")
	ctx := context.Background()
	w, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Rollback()
	var pid int
	if err = w.QueryRow("UPDATE payouts SET updated=now() WHERE order_id=$1 RETURNING pg_backend_pid()", order).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() {
		done <- p.do("POST", "/admin/suspend", p.adminSess, suspendForm("suspend", p.vendor.Handle)).Code
	}()
	waitFor(t, func() bool {
		return p.count("SELECT count(*) FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid))", pid) > 0
	})
	if _, err = w.Exec("INSERT INTO notifications(id,user_id,body) VALUES($1,$2,'test')", randomToken(), p.vendor.ID); err != nil {
		t.Fatalf("watcher-side insert while a suspension waits: %v", err)
	}
	if err = w.Commit(); err != nil {
		t.Fatal(err)
	}
	if code := <-done; code != 303 {
		t.Fatalf("suspension: %d", code)
	}
	p.suspendedHeld("after the suspension", order)
}

// A-112: a restore from backup holds a payout already held for an account suspension again. scripts/restore.sh
// (and the manual SQL in UPGRADING.md) keep the suspension text after the restore prefix, so releasing it still
// needs the payout address check as well as the wallet check, and restoring twice changes nothing more.
func TestRestoreKeepsSuspensionHold(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	order := p.completedWithPayout("tx-restore-suspended")
	p.check(p.do("POST", "/admin/suspend", p.adminSess, suspendForm("suspend", p.vendor.Handle)), 303)
	p.suspendedHeld("after suspension", order)
	for range 2 {
		for _, q := range restorePayoutSQL {
			if _, err := p.DB.Exec(q); err != nil {
				t.Fatal(err)
			}
		}
	}
	restoredSuspended := restoredHold + " " + suspendedHold
	if st, e := p.payoutError(order); st != "held" || e != restoredSuspended {
		t.Fatalf("restored suspension hold: %s %q", st, e)
	}
	id := p.payoutID(order)
	admin := html.UnescapeString(p.page("/admin", p.adminSess))
	mustContain(t, admin, "Held after a restore from backup and for an account suspension — may already have been sent; check the wallet and the payout address before releasing")
	s := resolveSection(t, admin, id)
	mustContain(t, s, `name="address_checked" value="confirmed" required`, `name="not_broadcast" value="confirmed" required`,
		"fake-testnet-vendor", "listsinceblock", "get_transfers")
	release := func(extra url.Values) (int, string) {
		t.Helper()
		f := url.Values{"payout_id": {id}, "op": {"release"}, "password": {testPassword}}
		for k, v := range extra {
			f[k] = v
		}
		w := p.do("POST", "/admin/payout", p.adminSess, f)
		return w.Code, w.Body.String()
	}
	if code, body := release(url.Values{"not_broadcast": {"confirmed"}}); code != 409 || !strings.Contains(body, "payout address") {
		t.Fatalf("release with only the wallet check: %d %s", code, body)
	}
	if code, body := release(url.Values{"address_checked": {"confirmed"}}); code != 400 || !strings.Contains(body, "restore from backup") {
		t.Fatalf("release with only the address check: %d %s", code, body)
	}
	if st, e := p.payoutError(order); st != "held" || e != restoredSuspended {
		t.Fatalf("refused releases changed the payout: %s %q", st, e)
	}
	if code, body := release(url.Values{"not_broadcast": {"confirmed"}, "address_checked": {"confirmed"}}); code != 303 {
		t.Fatalf("release with both checks: %d %s", code, body)
	}
	if st, _, _, _ := p.payout(order); st != "pending" {
		t.Fatalf("released payout is %s", st)
	}
	if n := p.count("SELECT count(*) FROM audit_events WHERE action LIKE $1", "Released held payout "+id+" %; restored from backup, administrator confirmed the wallet shows no broadcast transaction; held for an account suspension, administrator checked the payout address fake-testnet-vendor%"); n != 1 {
		t.Fatalf("audit for the release with both checks: %d", n)
	}
	if n := p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE 'Administrator confirmed the wallet shows no broadcast transaction%and checked the payout address%'", order); n != 1 {
		t.Fatalf("order history for the release with both checks: %d", n)
	}
}
