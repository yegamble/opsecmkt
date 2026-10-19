package market

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// A-558 (STAFF66-1, QA66-1): a dispute on an order with an escrows row moves coins from the escrow, so only an
// administrator who is not a party decides it. The same rule picks who is told about it and whether the desk says it has
// no eligible resolver. DB integration tests against the simulated Monero wallet RPC, not chain evidence.

// resolverSeen is the amount the resolve form states for order's escrow (the confirmed, unlocked deposits).
func resolverSeen(w *disputeOrderWorld, order string) int64 {
	return int64(w.count("SELECT COALESCE(sum(amount),0) FROM payments WHERE order_id=$1 AND confirmations>=0 AND NOT locked AND NOT after_close", order))
}

func resolverForm(dispute, outcome string, seen int64) url.Values {
	return resolveConfirmed(url.Values{"id": {dispute}, "outcome": {outcome}, "reply_seen": {"0"},
		"resolution": {"Decided from the evidence both parties sent to staff."}}, seen)
}

// An independent moderator's /resolve on a funded, disputed Monero escrow order is refused 403 and writes nothing, for
// both outcomes; an independent administrator then decides it. The two guards (resolveReceiptAuthorize, which every
// /resolve runs first, and resolveAction) are also called directly, so removing either one fails this test.
func TestXMREscrowModeratorCannotDecideDispute(t *testing.T) {
	w := newDisputeOrderWorld(t)
	adminID, admin := w.e.user("resolver_admin_"+randomToken()[:6], "admin")
	order, dispute := w.disputedFor(w.buyerID, w.buyer, w.vendorID, w.vendor, w.buyer, "The licence key was never delivered to me.")
	seen := resolverSeen(w, order)
	if seen != xmrListingPrice {
		t.Fatalf("counted amount %d", seen)
	}
	unchanged := func(step string) {
		t.Helper()
		if w.state(order) != stateDisputed ||
			w.count("SELECT count(*) FROM disputes WHERE id=$1 AND status='Open' AND outcome='' AND resolution=''", dispute) != 1 ||
			w.count("SELECT count(*) FROM xmr_settlements WHERE order_id=$1", order) != 0 ||
			w.count("SELECT count(*) FROM audit_events WHERE user_id=$1 OR action LIKE $2", w.modID, "Resolved dispute on order "+shortID(order)+"%") != 0 ||
			w.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND to_state=$2", order, stateResolved) != 0 {
			t.Fatalf("%s: a moderator's refused decision changed the order, dispute, settlements, history or audit", step)
		}
	}

	for _, outcome := range []string{"refund", "release"} {
		r := w.e.do("POST", "/resolve", w.mod, resolverForm(dispute, outcome, seen))
		if r.Code != 403 || !strings.Contains(r.Body.String(), errXMREscrowResolveAdmin.(*httpError).Msg) {
			t.Fatalf("moderator %s: %d %s", outcome, r.Code, r.Body.String())
		}
		unchanged("POST /resolve " + outcome)
	}

	// The receipt guard alone: every /resolve runs it before resolveAction.
	tx, err := w.e.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	err = resolveReceiptAuthorize(&actionCtx{A: w.e.A, Tx: tx, R: httptest.NewRequest("POST", "/resolve", nil), User: w.e.loadUser(w.modID), Form: url.Values{"id": {dispute}}})
	_ = tx.Rollback()
	if err != errXMREscrowResolveAdmin {
		t.Fatalf("resolveReceiptAuthorize for an independent moderator: %v", err)
	}
	// The action guard alone: no receipt flag, so confirmedTx does not run the receipt guard first.
	for _, outcome := range []string{"refund", "release"} {
		_, err = resolveAction(&actionCtx{A: w.e.A, R: httptest.NewRequest("POST", "/resolve", nil), User: w.e.loadUser(w.modID), Token: w.mod,
			Form: resolverForm(dispute, outcome, seen)})
		if err != errXMREscrowResolveAdmin {
			t.Fatalf("resolveAction %s for an independent moderator: %v", outcome, err)
		}
		unchanged("resolveAction " + outcome)
	}

	r := w.e.do("POST", "/resolve", admin, resolverForm(dispute, "refund", seen))
	if r.Code != 303 {
		t.Fatalf("independent administrator: %d %s", r.Code, r.Body.String())
	}
	if w.state(order) != stateResolved || w.count("SELECT count(*) FROM disputes WHERE id=$1 AND status<>'Open' AND outcome='refund'", dispute) != 1 ||
		w.count("SELECT count(*) FROM xmr_settlements WHERE order_id=$1 AND kind='refund'", order) != 1 ||
		w.count("SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE $2", adminID, "Resolved dispute on order "+shortID(order)+"%") != 1 {
		t.Fatal("the independent administrator's decision was not recorded")
	}
}

// With the sole administrator the vendor and an independent moderator present, an escrow dispute has no eligible
// resolver: the history says so, every administrator is told an administrator who is not a party is needed (added on
// the host), the desk says "No eligible resolver" to administrators, and the moderator is told nothing. A concession is
// reported to the same people. An independent administrator clears it. A historical order without escrow keeps the
// moderator as a resolver.
func TestXMREscrowDisputeNoAdminResolver(t *testing.T) {
	w := newDisputeOrderWorld(t) // newXMREscrowEnv with its administrator made a buyer; w.mod is an independent moderator
	e := w.e
	suffix := randomToken()[:6]
	buyerID, buyer := e.user("nr_buyer_"+suffix, "buyer")
	adminID, admin := e.user("nr_admin_"+suffix, "admin") // the only administrator, and the vendor
	if n := w.count("SELECT count(*) FROM users WHERE role='admin'"); n != 1 {
		t.Fatalf("administrators=%d, want 1", n)
	}
	order, _ := w.disputedFor(buyerID, buyer, adminID, "", buyer, "The licence key was never delivered to me.")
	if w.state(order) != stateDisputed || w.count("SELECT count(*) FROM disputes WHERE order_id=$1 AND status='Open'", order) != 1 {
		t.Fatal("dispute refused although no eligible resolver exists")
	}

	notes := agStrings(t, e, "SELECT note FROM order_events WHERE order_id=$1 AND actor_id IS NULL AND from_state='disputed' AND to_state='disputed' AND note LIKE 'No independent resolver was available%'", order)
	if len(notes) != 1 || notes[0] != noResolverEscrowNote {
		t.Fatalf("no-resolver history notes: %q", notes)
	}
	want := "Dispute opened on order " + shortID(order) + " needs an administrator who is not a party to resolve it; add one on the host with the role command (-set-role). No independent resolver was available when it opened."
	if n := w.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body=$2", adminID, want); n != 1 {
		t.Fatalf("administrator no-resolver notices=%d, want 1 of %q", n, want)
	}
	if n := w.resolverNotices(w.modID); n != 0 {
		t.Fatalf("independent moderator got %d dispute notices for an escrow dispute", n)
	}
	for _, body := range agStrings(t, e, "SELECT body FROM notifications") {
		if strings.Contains(body, "assign") || strings.Contains(body, "needs an unsuspended moderator") {
			t.Fatalf("a notice still offers a moderator: %q", body)
		}
	}
	mustContain(t, w.page("/order?id="+order, buyer, 200), "No independent resolver was available", "needs an administrator who is not a party")

	const deskNote = "No eligible resolver."
	desk := w.page("/moderator", admin, 200)
	mustContain(t, desk, deskNote, "needs an administrator who is not a party; add one on the host with the role command",
		"You are a party to this order; an administrator who is not a party must resolve it.")
	mustNotContain(t, desk, "assign an unsuspended moderator", "another moderator must resolve it")
	modDesk := w.page("/moderator", w.mod, 200)
	mustContain(t, modDesk, "Order "+order+"<", errXMREscrowResolveAdmin.(*httpError).Msg[:len("Only an administrator decides a dispute on a Monero escrow order")])
	mustNotContain(t, modDesk, `action="/resolve"`, deskNote)

	// A concession on a second such dispute tells the administrators, not the moderator.
	second, secondDispute := w.disputedFor(buyerID, buyer, adminID, "", buyer, "The second licence key never arrived either.")
	e.check(e.do("POST", "/disputes/concede", buyer, payoutConfirmed(url.Values{"id": {secondDispute}, "outcome": {"release"}, "password": {testPassword}}, xmrListingPrice)), 303)
	conceded := "Dispute on order " + shortID(second) + " is now %"
	if n := w.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE $2", adminID, conceded); n != 1 {
		t.Fatalf("administrator concession notices=%d, want 1", n)
	}
	if n := w.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE $2", w.modID, conceded); n != 0 {
		t.Fatalf("moderator concession notices=%d, want 0", n)
	}

	// A historical order without escrow keeps today's rule: the independent moderator resolves it.
	historical := e.order(buyerID, e.product(adminID, "digital"), "XMR", stateDisputed)
	agExec(e, "INSERT INTO disputes(id,order_id,reason) VALUES($1,$2,'Imported dispute on an order without escrow.')", randomToken(), historical)
	for _, entry := range strings.Split(w.page("/moderator", admin, 200), `<article class="message">`)[1:] {
		switch {
		case strings.Contains(entry, "Order "+historical+"<"):
			mustNotContain(t, entry, deskNote)
		case strings.Contains(entry, "Order "+order+"<"):
			mustContain(t, entry, deskNote)
		}
	}

	// An independent administrator is an eligible resolver: the note goes and it can decide.
	independentID, independent := e.user("nr_independent_"+suffix, "admin")
	mustNotContain(t, w.page("/moderator", admin, 200), deskNote)
	mustContain(t, w.page("/moderator", independent, 200), `action="/resolve"`)
	third, _ := w.disputedFor(buyerID, buyer, adminID, "", buyer, "The third licence key never arrived either.")
	if n := w.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body=$2", independentID, "Dispute opened on order "+shortID(third)+". Review it on the moderator desk."); n != 1 {
		t.Fatalf("independent administrator dispute notices=%d, want 1", n)
	}
	if n := w.resolverNotices(w.modID); n != 0 {
		t.Fatalf("moderator dispute notices=%d, want 0", n)
	}
	if n := w.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE 'No independent resolver was available%'", third); n != 0 {
		t.Fatal("no-resolver note written although an independent administrator exists")
	}
}
