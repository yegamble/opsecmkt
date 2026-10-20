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
	if n := w.resolverNotices(w.modID); n != 1 {
		t.Fatalf("independent moderator got %d dispute notices for an escrow dispute, want 1", n)
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

	// A concession on a second such dispute tells administrators and read-only moderator observers.
	second, secondDispute := w.disputedFor(buyerID, buyer, adminID, "", buyer, "The second licence key never arrived either.")
	e.check(e.do("POST", "/disputes/concede", buyer, payoutConfirmed(url.Values{"id": {secondDispute}, "outcome": {"release"}, "password": {testPassword}}, xmrListingPrice)), 303)
	conceded := "Dispute on order " + shortID(second) + " is now %"
	if n := w.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE $2", adminID, conceded); n != 0 {
		t.Fatalf("party administrator received duplicate staff concession notices=%d, want 0", n)
	}
	if n := w.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE $2", w.modID, conceded); n != 1 {
		t.Fatalf("moderator concession notices=%d, want 1", n)
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
	if n := w.resolverNotices(w.modID); n != 3 {
		t.Fatalf("moderator dispute notices=%d, want 3", n)
	}
	if n := w.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE 'No independent resolver was available%'", third); n != 0 {
		t.Fatal("no-resolver note written although an independent administrator exists")
	}
}

// A-584 (STAFF66-V1): a party's page says who decides the dispute. On a Monero escrow order only an administrator
// decides, so the open-dispute help (here and on /disputes), the reply help, the contact panel and the decision heading say so, label each
// listed staff member by what they can do and advise messaging an independent administrator first; the panel lists
// the same staff as before. A historical order without escrow, which a moderator may still decide, keeps the
// moderator wording.
func TestXMREscrowDisputePartyCopy(t *testing.T) {
	w := newDisputeOrderWorld(t) // w.mod is an independent moderator
	e := w.e
	modHandle := agStr(e, "SELECT handle FROM users WHERE id=$1", w.modID)
	adminHandle := "copy_admin_" + randomToken()[:6]
	adminID, admin := e.user(adminHandle, "admin")
	_, public := testPGPKey(t, "staff") // a usable staff key: the reply help then points to the contact panel
	agExec(e, "UPDATE users SET pgp=$1 WHERE id=$2", public, adminID)
	const (
		escrowPanel    = "Only an administrator decides a dispute on this escrow order. Send sensitive evidence as an encrypted message to an independent administrator first"
		escrowPassOn   = "a moderator can keep it and pass it on to an administrator"
		escrowReply    = "The administrator who decides this dispute reads your reply beside the buyer’s reason."
		moderatorReply = "The moderator who resolves this dispute reads your reply beside the buyer’s reason."
		historicPanel  = "Send sensitive evidence as an encrypted message to an independent moderator or administrator."
	)
	adminLabel := adminHandle + " · administrator: decides escrow disputes</summary>"
	modLabel := modHandle + " · moderator: can pass evidence on to an administrator</summary>"

	order := w.fundedFor(w.buyerID, w.buyer, w.vendorID, w.vendor)
	open := w.page("/order?id="+order, w.buyer, 200)
	mustContain(t, open, "An administrator who is not party to this order decides it and records a release or refund outcome.",
		"send those encrypted to an independent administrator listed under Contact dispute staff")
	mustNotContain(t, open, "A moderator who is not party to this order reviews it", "The moderator can read")
	disputesPage := w.page("/disputes", w.buyer, 200)
	mustContain(t, disputesPage, "on an escrow order only an administrator who is not a party decides it",
		"until staff record an outcome (on an escrow order, only an administrator)")
	mustNotContain(t, disputesPage, "for moderator review", "until a moderator records an outcome")
	dispute := w.openDispute(order, w.buyer, "The licence key was never delivered to me.")

	buyerPage := w.page("/order?id="+order, w.buyer, 200)
	mustContain(t, buyerPage, escrowPanel, escrowPassOn, adminLabel, modLabel)
	mustNotContain(t, buyerPage, historicPanel, "The moderator who resolves")
	vendorPage := w.page("/order?id="+order, w.vendor, 200)
	mustContain(t, vendorPage, escrowReply, "message an independent administrator listed under Contact dispute staff", adminLabel, modLabel)
	mustNotContain(t, vendorPage, moderatorReply, "The moderator who resolves", "message an independent moderator or administrator")

	e.check(e.do("POST", "/resolve", admin, resolverForm(dispute, "refund", resolverSeen(w, order))), 303)
	resolved := w.page("/order?id="+order, w.buyer, 200)
	mustContain(t, resolved, "<h3>Administrator decision</h3>", escrowPanel, adminLabel, modLabel)
	mustNotContain(t, resolved, "Moderator decision")

	// A historical order without escrow, opened by the buyer: the moderator wording stays, and the roles are as before.
	historical := e.order(w.buyerID, e.product(w.vendorID, "digital"), "XMR", stateDisputed)
	agExec(e, "INSERT INTO disputes(id,order_id,reason) VALUES($1,$2,'Imported dispute on an order without escrow.')", randomToken(), historical)
	agExec(e, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,$2,$3,$4,'')", historical, statePaid, stateDisputed, w.buyerID)
	oldBuyer := w.page("/order?id="+historical, w.buyer, 200)
	mustContain(t, oldBuyer, historicPanel, adminHandle+" · admin</summary>", modHandle+" · moderator</summary>")
	mustNotContain(t, oldBuyer, escrowPanel, "decides escrow disputes", "can pass evidence on")
	oldVendor := w.page("/order?id="+historical, w.vendor, 200)
	mustContain(t, oldVendor, moderatorReply, "message an independent moderator or administrator listed under Contact dispute staff")
	mustNotContain(t, oldVendor, escrowReply)
	agExec(e, "UPDATE disputes SET resolution='Imported decision.' WHERE order_id=$1", historical)
	mustContain(t, w.page("/order?id="+historical, w.buyer, 200), "<h3>Moderator decision</h3>")
}
