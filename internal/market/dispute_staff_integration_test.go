package market

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// A-8: opening a dispute tells the staff who can resolve it; a dispute nobody independent can resolve is
// still accepted, and unsuspended administrators are told what it needs. A-558: on an escrow order (every order the
// fixture funds) only an administrator who is not a party resolves it; a historical order without escrow keeps the
// moderator as a resolver (historicalDispute).

func (w *orderWorld) resolverNotices(uid string) int {
	return w.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE 'Dispute opened on order%'", uid)
}

// historicalDispute records an open dispute on a disputed order without an escrows row, as one from before escrow was
// the only checkout (such an order is read-only to the dispute form), and returns the order.
func (w *orderWorld) historicalDispute(buyerID, vendorID string) string {
	w.e.t.Helper()
	order := w.e.order(buyerID, w.e.product(vendorID, "digital"), "XMR", stateDisputed)
	agExec(w.e, "INSERT INTO disputes(id,order_id,reason) VALUES($1,$2,'Imported dispute on an order without escrow.')", randomToken(), order)
	return order
}

func TestDisputeNotifiesIndependentStaff(t *testing.T) {
	w := newDisputeOrderWorld(t)
	adminID, _ := w.e.user("admin_"+randomToken()[:6], "admin")
	order, _ := w.disputedFor(w.buyerID, w.buyer, w.vendorID, w.vendor, w.buyer, "The parcel arrived empty and the seal was broken.")

	want := "Dispute opened on order " + shortID(order) + ". Review it on the moderator desk."
	if n := w.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body=$2", adminID, want); n != 1 {
		t.Fatalf("independent administrator got %d resolver notices, want 1", n)
	}
	// The independent moderator cannot decide an escrow dispute, so it is not told to review it (A-558).
	for _, uid := range []string{w.buyerID, w.vendorID, w.otherID, w.modID} {
		if n := w.resolverNotices(uid); n != 0 {
			t.Fatalf("non-resolver %s got %d resolver notices", uid, n)
		}
	}
	// Checkout and funding have their own notices; the buyer opener gets no dispute transition notice.
	if n := w.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE 'Order % is now Disputed.%'", w.buyerID); n != 0 {
		t.Fatalf("buyer dispute transition notices=%d, want 0", n)
	}
	if n := w.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE 'No independent resolver%'", order); n != 0 {
		t.Fatal("no-resolver note written although a resolver exists")
	}
	mustNotContain(t, w.page("/moderator", w.mod, 200), "No eligible resolver")

	// A historical order without escrow keeps today's rule: the independent moderator is a resolver too. Such an order
	// is read-only to the dispute form, so its notice is sent directly.
	historical := w.historicalDispute(w.buyerID, w.vendorID)
	tx, err := w.e.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = notifyDisputeStaff(&actionCtx{R: httptest.NewRequest("POST", "/disputes", nil), Tx: tx}, &Order{ID: historical, BuyerID: w.buyerID, VendorID: w.vendorID}); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	want = "Dispute opened on order " + shortID(historical) + ". Review it on the moderator desk."
	for _, uid := range []string{w.modID, adminID} {
		if n := w.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body=$2", uid, want); n != 1 {
			t.Fatalf("staff %s got %d resolver notices for a historical dispute, want 1", uid, n)
		}
	}
}

func TestDisputePartyModeratorIsNotAResolver(t *testing.T) {
	w := newDisputeOrderWorld(t)
	adminID, _ := w.e.user("admin_"+randomToken()[:6], "admin")
	// The moderator buys from the vendor, so they are a party and cannot resolve this dispute.
	_, _ = w.disputedFor(w.modID, w.mod, w.vendorID, w.vendor, w.vendor, "The buyer claims non-delivery but tracking shows it arrived.")

	if n := w.resolverNotices(w.modID); n != 0 {
		t.Fatalf("party moderator got %d resolver notices", n)
	}
	if n := w.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE 'Order % is now Disputed.%'", w.modID); n != 1 {
		t.Fatalf("party moderator dispute transition notices=%d, want 1", n)
	}
	if n := w.resolverNotices(adminID); n != 1 {
		t.Fatalf("independent admin got %d resolver notices, want 1", n)
	}
	if n := w.count("SELECT count(*) FROM notifications WHERE body LIKE '%needs an%who is not a party%'"); n != 0 {
		t.Fatal("no-resolver notice sent although an independent administrator exists")
	}
}

func TestModeratorDisputePartyFilterIsExactAndStaffOnly(t *testing.T) {
	w := newDisputeOrderWorld(t)
	firstOrder, _ := w.disputedFor(w.buyerID, w.buyer, w.vendorID, w.vendor, w.buyer, "The parcel arrived empty and the seal was broken.")
	secondBuyerID, secondBuyer := w.e.user("party_filter_buyer_"+randomToken()[:6], "buyer")
	secondVendorID, _ := w.e.user("party_filter_vendor_"+randomToken()[:6], "vendor")
	secondOrder, _ := w.disputedFor(secondBuyerID, secondBuyer, secondVendorID, "", secondBuyer, "The parcel arrived empty and the seal was broken.")

	firstHandle := agStr(w.e, "SELECT handle FROM users WHERE id=$1", w.buyerID)
	firstDispute := agStr(w.e, "SELECT id FROM disputes WHERE order_id=$1", firstOrder)
	filtered := w.page("/moderator?party="+url.QueryEscape(firstHandle), w.mod, 200)
	if !strings.Contains(filtered, `id="dispute-`+firstDispute+`"`) || !strings.Contains(filtered, "Order "+firstOrder) || strings.Contains(filtered, secondOrder) {
		t.Fatalf("exact party filter did not isolate its dispute:\n%s", filtered)
	}
	caseMismatch := w.page("/moderator?party="+url.QueryEscape(strings.ToUpper(firstHandle)), w.mod, 200)
	if strings.Contains(caseMismatch, firstOrder) || strings.Contains(caseMismatch, secondOrder) {
		t.Fatalf("party filter was not exact-case:\n%s", caseMismatch)
	}
	if got := w.e.do("GET", "/moderator?party="+url.QueryEscape(firstHandle), secondBuyer, nil).Code; got != 403 {
		t.Fatalf("non-staff party filter response %d, want 403", got)
	}
	personal := w.page("/disputes?party="+url.QueryEscape(agStr(w.e, "SELECT handle FROM users WHERE id=$1", secondBuyerID)), w.buyer, 200)
	if !strings.Contains(personal, firstOrder) || strings.Contains(personal, secondOrder) {
		t.Fatalf("staff-only filter changed the buyer's own dispute view:\n%s", personal)
	}
}

func TestDisputeWithNoEligibleResolver(t *testing.T) {
	w := newDisputeOrderWorld(t)
	e := w.e
	suffix := randomToken()[:6]
	buyerID, buyer := e.user("buyer_"+suffix, "buyer")
	adminID, admin := e.user("admin_"+suffix, "admin")
	if _, err := e.DB.Exec("UPDATE users SET suspended_at=now() WHERE id=$1", w.modID); err != nil {
		t.Fatal(err)
	}
	// The only administrator is the vendor and no moderator exists.
	order, _ := w.disputedFor(buyerID, buyer, adminID, "", buyer, "The parcel never arrived at the agreed drop.")

	if w.state(order) != stateDisputed || w.count("SELECT count(*) FROM disputes WHERE order_id=$1 AND status='Open'", order) != 1 {
		t.Fatal("dispute refused although no independent resolver exists")
	}
	want := "Dispute opened on order " + shortID(order) + " needs an administrator who is not a party to resolve it; add one on the host with the role command (-set-role). No independent resolver was available when it opened."
	if n := w.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body=$2", adminID, want); n != 1 {
		t.Fatalf("admin no-resolver notices=%d, want 1", n)
	}
	if n := w.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND actor_id IS NULL AND from_state='disputed' AND to_state='disputed' AND note LIKE 'No independent resolver was available%'", order); n != 1 {
		t.Fatalf("history notes=%d, want 1", n)
	}
	if note := agStr(e, "SELECT note FROM order_events WHERE order_id=$1 AND actor_id IS NULL AND note LIKE 'No independent resolver was available%'", order); !strings.Contains(note, "no unsuspended administrator who was not a party") {
		t.Fatalf("no-resolver history note is inaccurate: %q", note)
	}
	if !strings.Contains(w.page("/order?id="+order, buyer, 200), "No independent resolver was available") {
		t.Fatal("order history does not show the no-resolver note")
	}

	const note = "No eligible resolver."
	mustContain(t, w.page("/moderator", admin, 200), note, "You are a party to this order")
	mustNotContain(t, w.page("/disputes", buyer, 200), note)

	// A moderator who is independent still cannot decide an escrow dispute, so the note stays (A-558); an independent
	// administrator can.
	modID, mod := e.user("mod_"+suffix, "moderator")
	mustContain(t, w.page("/moderator", admin, 200), note)
	mustNotContain(t, w.page("/moderator", mod, 200), `action="/resolve"`)
	independentAdminID, independentAdmin := e.user("independent_admin_"+suffix, "admin")
	mustNotContain(t, w.page("/moderator", admin, 200), note)
	mustContain(t, w.page("/moderator", independentAdmin, 200), `action="/resolve"`, `name="outcome" value="refund"`)

	// A-306: a staff buyer is a party too. The moderator buys from the administrator, so apart from the vendor the
	// only staff account besides the independent administrator is the buyer; the moderator cannot resolve it.
	staffOrder, _ := w.disputedFor(modID, mod, adminID, "", mod, "Bought as a customer; the parcel never arrived.")
	if n := w.resolverNotices(independentAdminID); n != 1 {
		t.Fatalf("independent administrator resolver notices=%d, want 1", n)
	}
	desk := w.page("/moderator", independentAdmin, 200)
	mustContain(t, desk, "Order "+staffOrder+"<", "Order "+order+"<")
	for _, entry := range strings.Split(desk, `<article class="message">`)[1:] {
		switch {
		case strings.Contains(entry, "Order "+staffOrder+"<"):
			mustNotContain(t, entry, note)
		case strings.Contains(entry, "Order "+order+"<"):
			mustNotContain(t, entry, note)
		}
	}
}

// A-118: a suspended account cannot sign in, so it is neither a resolver nor a dispute contact. With the only
// non-party moderator suspended a dispute on a historical order without escrow (where a moderator resolves; A-558) is
// marked as having no eligible resolver and the moderator is left out of the contacts; restoring the moderator
// reverses both. An escrow dispute is decided by an administrator only, so the moderator never clears its note.
func TestDisputeIgnoresSuspendedStaff(t *testing.T) {
	w := newDisputeOrderWorld(t)
	e := w.e
	suffix := randomToken()[:6]
	buyerID, buyer := e.user("buyer_"+suffix, "buyer")
	adminID, admin := e.user("admin_"+suffix, "admin")
	modHandle, lockedHandle := "mod_"+suffix, "locked_"+suffix
	modID, _ := e.user(modHandle, "moderator")
	// The web refuses to suspend an administrator; this row is marked directly so that the administrators'
	// no-resolver notice is pinned to accounts that are not suspended as well.
	lockedID, _ := e.user(lockedHandle, "admin")
	if _, err := e.DB.Exec("UPDATE users SET suspended_at=now() WHERE id=$1", lockedID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec("UPDATE users SET suspended_at=now() WHERE id=$1", w.modID); err != nil {
		t.Fatal(err)
	}
	e.check(e.do("POST", "/admin/suspend", admin, suspendForm("suspend", modHandle)), 303)

	// Every staff account that is not suspended is a party: the only such administrator is the vendor.
	order, _ := w.disputedFor(buyerID, buyer, adminID, "", buyer, "The parcel never arrived at the agreed drop.")
	historical := w.historicalDispute(buyerID, adminID)

	if w.state(order) != stateDisputed {
		t.Fatal("dispute refused")
	}
	noResolver := "Dispute opened on order " + shortID(order) + " needs an administrator who is not a party to resolve it;%"
	if n := w.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE $2", adminID, noResolver); n != 1 {
		t.Fatalf("admin no-resolver notices=%d, want 1", n)
	}
	if n := w.count("SELECT count(*) FROM notifications WHERE user_id=$1", lockedID); n != 0 {
		t.Fatalf("suspended administrator received %d notifications", n)
	}
	for _, uid := range []string{modID, lockedID} {
		if n := w.resolverNotices(uid); n != 0 {
			t.Fatalf("suspended staff %s got %d dispute notices", uid, n)
		}
	}
	if n := w.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND actor_id IS NULL AND note LIKE 'No independent resolver was available%'", order); n != 1 {
		t.Fatalf("history notes=%d, want 1", n)
	}
	const deskNote = "No eligible resolver."
	historicalCopy := "No unsuspended moderator or administrator who is not a party is available."
	entries := func() map[string]string {
		out := map[string]string{}
		for _, entry := range strings.Split(w.page("/moderator", admin, 200), `<article class="message">`)[1:] {
			for _, id := range []string{order, historical} {
				if strings.Contains(entry, "Order "+id+"<") {
					out[id] = entry
				}
			}
		}
		return out
	}
	desk := entries()
	mustContain(t, desk[order], deskNote, "No unsuspended administrator who is not a party is available, and only an administrator decides")
	mustContain(t, desk[historical], deskNote, historicalCopy)
	mustNotContain(t, desk[order], "Every moderator and administrator is a party", historicalCopy)
	body := w.page("/order?id="+order, buyer, 200)
	mustContain(t, body, "Contact dispute staff", "No independent moderator or administrator is available to contact.")
	mustNotContain(t, body, modHandle+" · ", lockedHandle)

	// Restoring the moderator makes it a contact again and a resolver of the historical dispute, not the escrow one.
	e.check(e.do("POST", "/admin/suspend", admin, suspendForm("restore", modHandle)), 303)
	desk = entries()
	mustContain(t, desk[order], deskNote)
	mustNotContain(t, desk[historical], deskNote)
	body = w.page("/order?id="+order, buyer, 200)
	mustContain(t, body, modHandle+" · moderator")
	mustNotContain(t, body, "No independent moderator or administrator is available to contact.", lockedHandle)
	second, _ := w.disputedFor(buyerID, buyer, adminID, "", buyer, "The second parcel never arrived either.")
	if n := w.resolverNotices(modID); n != 0 {
		t.Fatalf("restored moderator escrow dispute notices=%d, want 0", n)
	}
	if n := w.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE 'No independent resolver was available%'", second); n != 1 {
		t.Fatal("no-resolver note missing although only a moderator is independent of the escrow dispute")
	}
	if n := w.resolverNotices(lockedID); n != 0 {
		t.Fatalf("suspended administrator got %d dispute notices", n)
	}
}
