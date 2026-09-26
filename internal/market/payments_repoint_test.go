package market

import (
	"context"
	"errors"
	"html"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// A-121: saving a payout address never moves an unsent payout that already has one (a stolen password must not
// re-point it). The owner is told which orders still use the previous address, and an administrator moves a held
// or definite-failed payout to the account's current address only after checking it with the owner. A payout that
// is being sent or may already have been broadcast is never moved.

// repoint submits the "use the account's current address" action for payout id, with the addresses the form showed.
func (p *payEnv) repoint(id, from, to string, extra url.Values) (int, string) {
	p.t.Helper()
	f := url.Values{"payout_id": {id}, "op": {"repoint"}, "from_address": {from}, "to_address": {to}, "password": {testPassword}, "address_checked": {"confirmed"}}
	for k, v := range extra {
		f[k] = v
	}
	w := p.do("POST", "/admin/payout", p.adminSess, f)
	return w.Code, w.Body.String()
}

// saveVendorAddress saves the vendor's BTC payout address through /account/payout with a fresh session (a
// suspension ends the vendor's sessions) and returns the notification it produced.
func (p *payEnv) saveVendorAddress(addr string) string {
	p.t.Helper()
	p.check(p.do("POST", "/account/payout", agSession(p.testEnv, p.vendor.ID), url.Values{"currency": {"BTC"}, "address": {addr}, "password": {testPassword}}), 303)
	return p.str("SELECT body FROM notifications WHERE user_id=$1 AND body LIKE 'Your BTC payout address was changed%' ORDER BY created DESC LIMIT 1", p.vendor.ID)
}

func (p *payEnv) payoutAddress(order string) string {
	p.t.Helper()
	_, _, addr, _ := p.payout(order)
	return addr
}

func TestAdminMovesHeldPayoutToCurrentAddress(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-old")
	// A definite wallet rejection (nothing broadcast), then a payout held for an account suspension.
	failed, failedID := p.failPayout("tx-rp-failed", &rpcError{Method: "fake RPC send", Code: -6, Message: "Insufficient funds"})
	held := p.completedWithPayout("tx-rp-held")
	p.check(p.do("POST", "/admin/suspend", p.adminSess, suspendForm("suspend", p.vendor.Handle)), 303)
	p.check(p.do("POST", "/admin/suspend", p.adminSess, suspendForm("restore", p.vendor.Handle)), 303)
	p.suspendedHeld("after suspension", held)
	heldID := p.payoutID(held)

	p.saveVendorAddress("fake-testnet-new")
	for _, order := range []string{failed, held} {
		if a := p.payoutAddress(order); a != "fake-testnet-old" {
			t.Fatalf("saving an address moved the payout for %s to %q", order[:8], a)
		}
	}
	changed := p.str(`SELECT to_char(payout_btc_changed,'YYYY-MM-DD HH24:MI "UTC"') FROM users WHERE id=$1`, p.vendor.ID)

	// /admin shows both addresses and when the saved one last changed, behind the address check.
	admin := html.UnescapeString(p.page("/admin", p.adminSess))
	for _, id := range []string{heldID, failedID} {
		s := resolveSection(t, admin, id)
		mustContain(t, s, `value="repoint"`, `name="from_address" value="fake-testnet-old"`, `name="to_address" value="fake-testnet-new"`,
			"fake-testnet-old", "fake-testnet-new", "last changed "+changed, `name="address_checked" value="confirmed" required`,
			"Use the account's current address")
	}

	unchanged := func(when string) {
		t.Helper()
		for _, order := range []string{failed, held} {
			if a := p.payoutAddress(order); a != "fake-testnet-old" {
				t.Fatalf("%s: payout for %s moved to %q", when, order[:8], a)
			}
		}
		p.suspendedHeld(when, held)
		if n := p.count("SELECT count(*) FROM audit_events WHERE action LIKE 'Moved payout%'"); n != 0 {
			t.Fatalf("%s: %d moves audited", when, n)
		}
		if n := p.count("SELECT count(*) FROM order_events WHERE note LIKE 'Administrator moved%'"); n != 0 {
			t.Fatalf("%s: %d moves recorded in order history", when, n)
		}
	}
	// Missing or wrong confirmation: 400. Wrong password: 401. Addresses changed since the page loaded: 409.
	for _, bad := range []url.Values{{"address_checked": {""}}, {"address_checked": {"yes"}}} {
		if code, body := p.repoint(heldID, "fake-testnet-old", "fake-testnet-new", bad); code != 400 || !strings.Contains(body, "Nothing was changed") {
			t.Fatalf("move without the address check (%v): %d %s", bad, code, body)
		}
	}
	unchanged("after a missing confirmation")
	if code, _ := p.repoint(heldID, "fake-testnet-old", "fake-testnet-new", url.Values{"password": {"wrong-password-here"}}); code != 401 {
		t.Fatalf("move with a wrong password: %d", code)
	}
	unchanged("after a wrong password")
	for _, c := range [][2]string{{"fake-testnet-old", "fake-testnet-other"}, {"fake-testnet-other", "fake-testnet-new"}, {"fake-testnet-old", ""}} {
		if code, body := p.repoint(heldID, c[0], c[1], nil); code != 409 || !strings.Contains(body, "Reload the admin page") {
			t.Fatalf("move with stale addresses %v: %d %s", c, code, body)
		}
	}
	unchanged("after stale addresses")

	// The held payout moves and stays held for the suspension: nothing is sent until it is released.
	if code, body := p.repoint(heldID, "fake-testnet-old", "fake-testnet-new", nil); code != 303 {
		t.Fatalf("move the held payout: %d %s", code, body)
	}
	if a := p.payoutAddress(held); a != "fake-testnet-new" {
		t.Fatalf("held payout address after the move: %q", a)
	}
	p.suspendedHeld("after the move", held)
	if n := p.count("SELECT count(*) FROM audit_events WHERE action LIKE $1", "Moved payout "+heldID+" (0.001 BTC, held) for order "+held[:8]+" from fake-testnet-old to the account's current address fake-testnet-new; administrator checked the address with the account owner (confirmed with password)"); n != 1 {
		t.Fatalf("audit of the move old -> new: %d", n)
	}
	for _, s := range []string{
		p.str("SELECT note FROM order_events WHERE order_id=$1 AND note LIKE 'Administrator moved%'", held),
		p.str("SELECT body FROM notifications WHERE user_id=$1 AND body LIKE $2", p.vendor.ID, "Order "+held[:8]+": an administrator moved%"),
	} {
		mustContain(t, s, "0.001 BTC")
		mustNotContain(t, s, "fake-testnet", "http", "/")
	}
	// A second move of the same payout (repeated submission) changes nothing.
	if code, _ := p.repoint(heldID, "fake-testnet-old", "fake-testnet-new", nil); code != 409 {
		t.Fatalf("second move: %d", code)
	}
	if n := p.count("SELECT count(*) FROM audit_events WHERE action LIKE 'Moved payout%'"); n != 1 {
		t.Fatalf("moves audited after a repeated submission: %d", n)
	}
	p.poll()
	if len(p.fake.Sends()) != 0 {
		t.Fatalf("the move sent a held payout: %+v", p.fake.Sends())
	}

	// The definite failure moves and stays failed; the administrator's requeue sends it once, to the new address.
	// (Another administrator: the first has used its ten password confirmations for this window.)
	_, p.adminSess = p.user("padmin2_"+randomToken()[:6], "admin")
	if code, body := p.repoint(failedID, "fake-testnet-old", "fake-testnet-new", nil); code != 303 {
		t.Fatalf("move the failed payout: %d %s", code, body)
	}
	if st, _, a, _ := p.payout(failed); st != "failed" || a != "fake-testnet-new" {
		t.Fatalf("failed payout after the move: %s %q", st, a)
	}
	p.poll()
	if len(p.fake.Sends()) != 0 {
		t.Fatalf("the move sent a failed payout: %+v", p.fake.Sends())
	}
	if code, body := p.payoutAction(p.adminSess, failedID, "requeue", "", testPassword); code != 303 {
		t.Fatalf("requeue after the move: %d %s", code, body)
	}
	p.check(p.do("POST", "/admin/payout", p.adminSess, url.Values{"payout_id": {heldID}, "op": {"release"}, "password": {testPassword}, "address_checked": {"confirmed"}}), 303)
	p.poll()
	p.poll()
	if p.sendsTo("fake-testnet-new") != 2 || p.sendsTo("fake-testnet-old") != 0 {
		t.Fatalf("sends after the moves: %+v", p.fake.Sends())
	}
}

// Saving a new address leaves every payout that already has one on it, and the notification names the orders
// (no address, no link): the held and definite-failed ones an administrator can move, and the others separately.
func TestAddressSaveDoesNotRepointUnsentPayouts(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-old")
	// restored: held by a restore from backup. suspended: held for an account suspension, with an address.
	restored := p.completedWithPayout("tx-rs-restored")
	if _, err := p.DB.Exec("UPDATE payouts SET state='held',error=$2 WHERE order_id=$1", restored, restoredHold); err != nil {
		t.Fatal(err)
	}
	suspended := p.completedWithPayout("tx-rs-suspended")
	p.check(p.do("POST", "/admin/suspend", p.adminSess, suspendForm("suspend", p.vendor.Handle)), 303)
	p.check(p.do("POST", "/admin/suspend", p.adminSess, suspendForm("restore", p.vendor.Handle)), 303)
	p.suspendedHeld("after suspension", suspended)
	definite, _ := p.failPayout("tx-rs-definite", &rpcError{Method: "fake RPC send", Code: -6, Message: "Insufficient funds"})
	ambiguous, _ := p.failPayout("tx-rs-ambiguous", errors.New("bitcoin RPC unreachable: timed out"))
	sending := p.completedWithPayout("tx-rs-sending")
	if _, err := p.DB.Exec("UPDATE payouts SET state='sending' WHERE order_id=$1", sending); err != nil {
		t.Fatal(err)
	}
	pending := p.completedWithPayout("tx-rs-pending")
	all := []string{restored, suspended, definite, ambiguous, sending, pending}
	before := map[string]string{}
	for _, o := range all {
		before[o], _ = p.payoutError(o)
	}

	note := p.saveVendorAddress("fake-testnet-new")
	for _, o := range all {
		st, _ := p.payoutError(o)
		if a := p.payoutAddress(o); a != "fake-testnet-old" || st != before[o] {
			t.Fatalf("saving an address changed the payout for %s: %s %q (was %s)", o[:8], st, a, before[o])
		}
	}
	p.suspendedHeld("after the owner saved a new address", suspended)
	mustContain(t, note, "Your BTC payout address was changed",
		"2 unsent payout(s) still use your previous address (orders "+suspended[:8]+", "+definite[:8]+"); an administrator must confirm the change",
		"4 other unsent payout(s) are queued, being sent or may already have been sent, and stay on your previous address (orders "+restored[:8]+", "+ambiguous[:8]+", "+sending[:8]+", "+pending[:8]+")",
		"If you did not make this change")
	mustNotContain(t, note, "fake-testnet", "http", "/", ".onion")
	if p.str("SELECT (payout_btc_changed IS NOT NULL)::text FROM users WHERE id=$1", p.vendor.ID) != "true" {
		t.Fatal("the address change time was not recorded")
	}
	// Saving it again names nothing new: the payouts still use the previous address.
	mustContain(t, p.saveVendorAddress("fake-testnet-new"), "2 unsent payout(s) still use your previous address")
}

// A payout that is being sent, may already have been broadcast, or is not held or definitely failed is never moved,
// whatever the form says; the admin page offers the move only where it applies.
func TestRepointRefusedUnlessHeldOrDefiniteFailed(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-old")
	ambiguous, ambiguousID := p.failPayout("tx-rr-ambiguous", errors.New("bitcoin RPC unreachable: timed out"))
	restoredFail, restoredFailID := p.failPayout("tx-rr-restored-failed", &rpcError{Method: "fake RPC send", Code: -6, Message: "Insufficient funds"})
	for _, q := range restorePayoutSQL[1:] {
		if _, err := p.DB.Exec(q+" AND order_id=$1", restoredFail); err != nil {
			t.Fatal(err)
		}
	}
	restoredHeld := p.completedWithPayout("tx-rr-restored-held")
	if _, err := p.DB.Exec("UPDATE payouts SET state='held',error=$2 WHERE order_id=$1", restoredHeld, restoredHold+" "+suspendedHold); err != nil {
		t.Fatal(err)
	}
	stuck := p.completedWithPayout("tx-rr-stuck")
	if _, err := p.DB.Exec("UPDATE payouts SET state='sending',updated=now()-interval '1 hour' WHERE order_id=$1", stuck); err != nil {
		t.Fatal(err)
	}
	sending := p.completedWithPayout("tx-rr-sending")
	if _, err := p.DB.Exec("UPDATE payouts SET state='sending' WHERE order_id=$1", sending); err != nil {
		t.Fatal(err)
	}
	sent := p.completedWithPayout("tx-rr-sent")
	if _, err := p.DB.Exec("UPDATE payouts SET state='sent',txid='tx-out' WHERE order_id=$1", sent); err != nil {
		t.Fatal(err)
	}
	pending := p.completedWithPayout("tx-rr-pending")
	p.saveVendorAddress("fake-testnet-new")

	admin := html.UnescapeString(p.page("/admin", p.adminSess))
	for _, id := range []string{ambiguousID, restoredFailID, p.payoutID(restoredHeld), p.payoutID(stuck)} {
		mustNotContain(t, resolveSection(t, admin, id), `value="repoint"`)
	}
	for _, c := range []struct{ name, order, reason string }{
		{"ambiguous failure", ambiguous, "may already have been broadcast"},
		{"failure restored from backup", restoredFail, "may already have been broadcast"},
		{"hold restored from backup", restoredHeld, "may already have been broadcast"},
		{"stuck send", stuck, "is being sent"},
		{"send in progress", sending, "is being sent"},
		{"pending", pending, "only a held payout or one the wallet definitely rejected"},
		{"sent", sent, "only a held payout or one the wallet definitely rejected"},
	} {
		st, e := p.payoutError(c.order)
		code, body := p.repoint(p.payoutID(c.order), "fake-testnet-old", "fake-testnet-new", url.Values{"not_broadcast": {"confirmed"}})
		if code != 409 || !strings.Contains(body, c.reason) || !strings.Contains(body, "Nothing was changed") {
			t.Fatalf("%s: move answered %d %s", c.name, code, body)
		}
		if st2, e2 := p.payoutError(c.order); st2 != st || e2 != e || p.payoutAddress(c.order) != "fake-testnet-old" {
			t.Fatalf("%s: refused move changed the payout: %s %q", c.name, st2, p.payoutAddress(c.order))
		}
	}
	if code, _ := p.repoint("999999999", "fake-testnet-old", "fake-testnet-new", nil); code != 404 {
		t.Fatalf("move of an unknown payout: %d", code)
	}
	// With no saved address there is nothing to move to.
	held := p.completedWithPayout("tx-rr-held")
	if _, err := p.DB.Exec("UPDATE payouts SET state='held',error=$2 WHERE order_id=$1", held, suspendedHold); err != nil {
		t.Fatal(err)
	}
	p.setPayoutAddress(p.vendor, "")
	if code, body := p.repoint(p.payoutID(held), "fake-testnet-new", "", nil); code != 409 || !strings.Contains(body, "no saved BTC payout address") {
		t.Fatalf("move with no saved address: %d %s", code, body)
	}
	if n := p.count("SELECT count(*) FROM audit_events WHERE action LIKE 'Moved payout%'"); n != 0 {
		t.Fatalf("refused moves audited: %d", n)
	}
}

// The move is a compare-and-set on the payout's state and address: of two concurrent moves one succeeds, and a
// move waiting behind a concurrent release finds the payout queued and changes nothing.
func TestRepointIsCompareAndSet(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-old")
	order := p.completedWithPayout("tx-rc-held")
	if _, err := p.DB.Exec("UPDATE payouts SET state='held',error=$2 WHERE order_id=$1", order, suspendedHold); err != nil {
		t.Fatal(err)
	}
	p.saveVendorAddress("fake-testnet-new")
	id := p.payoutID(order)
	codes := make([]int, 2)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Go(func() {
			f := url.Values{"payout_id": {id}, "op": {"repoint"}, "from_address": {"fake-testnet-old"}, "to_address": {"fake-testnet-new"}, "password": {testPassword}, "address_checked": {"confirmed"}}
			codes[i] = p.do("POST", "/admin/payout", p.adminSess, f).Code
		})
	}
	wg.Wait()
	if !(codes[0] == 303 && codes[1] == 409 || codes[0] == 409 && codes[1] == 303) {
		t.Fatalf("concurrent moves answered %v", codes)
	}
	if n := p.count("SELECT count(*) FROM audit_events WHERE action LIKE 'Moved payout%'"); n != 1 {
		t.Fatalf("concurrent moves audited %d times", n)
	}

	// A release holding the payout's row lock commits first: the waiting move sees it queued and refuses.
	order2 := p.completedWithPayout("tx-rc-race")
	if _, err := p.DB.Exec("UPDATE payouts SET state='held',error=$2,address='fake-testnet-old' WHERE order_id=$1", order2, suspendedHold); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	rel, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rel.Rollback()
	var pid int
	if err = rel.QueryRow("UPDATE payouts SET state='pending',error='' WHERE order_id=$1 AND state='held' RETURNING pg_backend_pid()", order2).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() {
		code, _ := p.repoint(p.payoutID(order2), "fake-testnet-old", "fake-testnet-new", nil)
		done <- code
	}()
	waitFor(t, func() bool {
		return p.count("SELECT count(*) FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid))", pid) > 0
	})
	if err = rel.Commit(); err != nil {
		t.Fatal(err)
	}
	if code := <-done; code != 409 {
		t.Fatalf("move behind a concurrent release: %d", code)
	}
	if st, _, a, _ := p.payout(order2); st != "pending" || a != "fake-testnet-old" {
		t.Fatalf("payout after the refused move: %s %q", st, a)
	}
}
