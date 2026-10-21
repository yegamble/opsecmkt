package market

import "testing"

// A-636: who staff notices and reads reach on Monero escrow orders, the legs A-565 left without a Monero test. DB
// integration tests against the simulated Monero wallet-rpc of escrow_xmr_fake_test.go: simulated RPC, not chain
// evidence.

// An archived, taken-down listing's page opens to non-party staff through a Monero escrow order only while that order
// has an open staff alert (escrowStaffAlert, A-470): here the vendor's claim, requested more than 24 hours ago and not
// yet built, with no deposit flagged and no dispute, so the alert is the only reason. The page has the Archived
// badge and no checkout, delivery content or takedown reason; anonymous visitors, an unrelated buyer and another vendor
// get 404 throughout. Once the watcher builds the release the alert clears and staff get 404 again.
func TestXMRArchivedListingThroughStaffAlert(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	_, modSess := x.user("xalert_mod_"+randomToken()[:6], "moderator")
	_, adminSess := x.user("xalert_admin_"+randomToken()[:6], "admin")
	strangerID, stranger := x.user("xalert_buyer_"+randomToken()[:6], "buyer")
	otherVendorID, otherVendor := x.user("xalert_vendor_"+randomToken()[:6], "vendor")
	x.enableOrderPGP(strangerID, otherVendorID)
	order, _, _ := x.completed()
	product := agStr(x.testEnv, "SELECT product_id FROM orders WHERE id=$1", order)
	claim := testXMRAddr("alert claim "+order, true)
	if code, body := x.xmrClaim(order, x.vendorSess, claim, testPassword); code != 303 {
		t.Fatalf("claim: %d %s", code, body)
	}
	id := x.settlementOf(order, "release").ID
	const reason = "Taken down for review of its delivery terms."
	agExec(x.testEnv, "UPDATE products SET archived=true,archived_at=now(),admin_archive_reason=$2 WHERE id=$1", product, reason)

	status := func(sess string) int { return x.do("GET", "/product?id="+product, sess, nil).Code }
	outsiders := map[string]string{"anonymous": "", "unrelated buyer": stranger, "other vendor": otherVendor}
	expect := func(stage string, staff int) {
		t.Helper()
		for name, sess := range map[string]string{"moderator": modSess, "administrator": adminSess} {
			if got := status(sess); got != staff {
				t.Errorf("%s: %s opening the archived listing: %d, want %d", stage, name, got, staff)
			}
		}
		for name, sess := range outsiders {
			if got := status(sess); got != 404 {
				t.Errorf("%s: %s opening the archived listing: %d, want 404", stage, name, got)
			}
		}
		for _, viewer := range []string{roleBuyer, roleVendor} {
			if got := status(x.sessOf(order, viewer)); got != 200 {
				t.Errorf("%s: the order's %s opening the archived listing: %d, want 200", stage, viewer, got)
			}
		}
	}
	expect("before any alert", 404)

	// The release stays requested, unbuilt, for more than a day (the wallet-rpc was unreachable): no pass runs, and its
	// request is dated back as a day passing would.
	agExec(x.testEnv, "UPDATE xmr_settlements SET created=now()-interval '25 hours' WHERE id=$1", id)
	if x.settlement(id).State != "requested" || !x.alerting(order) {
		t.Fatalf("settlement %d is not a day-old request with an open alert: %s", id, x.settlement(id).State)
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND flagged", order) + agInt(x.testEnv, "SELECT count(*) FROM disputes WHERE order_id=$1", order); n != 0 {
		t.Fatalf("a flagged deposit or a dispute also opens the order to staff (%d)", n)
	}
	expect("with the alert open", 200)
	for _, sess := range []string{modSess, adminSess} {
		body := x.page("/product?id="+product, sess)
		mustContain(t, body, archivedListingBadge, "accepts no new orders")
		mustNotContain(t, body, `href="/checkout?id=`, "Review order draft", "licence key XC-1234", reason)
	}

	x.mustPoll() // the wallet-rpc answers again: the release is built and sent
	if x.settlement(id).State == "requested" || x.alerting(order) {
		t.Fatalf("the alert is still open after the release was built: %s", x.settlement(id).State)
	}
	expect("after the alert cleared", 404)
}

// A Monero escrow deposit with an unlock time is flagged for payment review by the watcher (flagLockedXMR), and the
// review notice goes to each administrator who is active: not to a suspended or a closed administrator, nor to a
// moderator. Restoring the suspended administrator has the next such notice reach them too.
func TestXMRPaymentReviewSkipsSuspendedAdministrators(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	activeID, _ := x.user("xreview_admin_"+randomToken()[:6], "admin")
	suspendedID, _ := x.user("xreview_suspended_"+randomToken()[:6], "admin")
	closedID, _ := x.user("xreview_closed_"+randomToken()[:6], "admin")
	modID, _ := x.user("xreview_mod_"+randomToken()[:6], "moderator")
	agExec(x.testEnv, "UPDATE users SET suspended_at=now() WHERE id=$1", suspendedID)
	agExec(x.testEnv, "UPDATE users SET closed_at=now() WHERE id=$1", closedID)
	lockedDeposit := func() string {
		t.Helper()
		order, addr := x.freshCheckout(x.digital(x.vendor))
		txid := hex64("locked " + order)
		x.fake.payAddress(addr, txid, xmrListingPrice, 10)
		x.fake.setTransfer(txid, func(tr *fakeXMRTransfer) { tr.UnlockTime = 4000000 })
		x.mustPoll()
		if n := agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND locked AND flagged", order); n != 1 || x.state(order) != stateAwaitingPayment {
			t.Fatalf("the locked deposit: %d flagged, order %s", n, x.state(order))
		}
		return order
	}
	notices := func(uid, order string) int {
		return agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1 AND audience='admin' AND strpos(body,$2)=1", uid, "Payment review needed for order "+shortID(order)+":")
	}
	order := lockedDeposit()
	for who, c := range map[string]struct {
		uid  string
		want int
	}{"the fixture administrator": {x.admin, 1}, "another active administrator": {activeID, 1}, "a suspended administrator": {suspendedID, 0},
		"a closed administrator": {closedID, 0}, "a moderator": {modID, 0}} {
		if got := notices(c.uid, order); got != c.want {
			t.Errorf("%s got %d payment-review notices, want %d", who, got, c.want)
		}
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=ANY($1) AND strpos(body,$2)>0", "{"+suspendedID+","+closedID+","+modID+"}", shortID(order)); n != 0 {
		t.Fatalf("%d notices about the order reached a suspended or closed administrator or a moderator", n)
	}

	agExec(x.testEnv, "UPDATE users SET suspended_at=NULL WHERE id=$1", suspendedID)
	second := lockedDeposit()
	if got := notices(suspendedID, second); got != 1 {
		t.Fatalf("the restored administrator got %d payment-review notices, want 1", got)
	}
	if notices(activeID, second) != 1 || notices(closedID, second) != 0 {
		t.Fatal("the second deposit's notice did not reach exactly the active administrators")
	}
}
