package market

import (
	"html"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
)

// A-349 and A-350: what the signing copy says. A pasted signature is only checked for its shape, so every text that
// reports a signature says its validity is checked when the market finalizes the transaction. When the second party
// signs, the market does not sign: the note, the other party's notification and the signer's notice say it
// broadcasts the transaction the two parties signed, not "once one party has signed, the market co-signs".
func TestEscrowSignedCopySaysWhenValidityIsChecked(t *testing.T) {
	x := newEscrowEnv(t)
	const checked = "validity is checked when the market finalizes the transaction"
	order, id := x.built(regtestAddr("refund-copy"), regtestAddr("claim-copy"), 100000)
	// signAndRead signs as party and reads the order page the redirect leads to, with the notice cookie it set.
	signAndRead := func(party string) (code, page string) {
		sess, fp := x.partySess(party)
		w := x.do("POST", "/escrow/sign", sess, url.Values{"order_id": {order}, "settlement_id": {strconv.FormatInt(id, 10)}, "psbt": {walletSign(t, x.partyCopy(order, sess, id), fp, 1)}})
		if w.Code != 303 {
			t.Fatalf("%s signs: %d %s", party, w.Code, w.Body.String())
		}
		code = x.cookie(w, "notice")
		r := x.do("GET", "/order?id="+order, sess, nil, &http.Cookie{Name: "notice", Value: code})
		// The notice alone: the page's timeline carries the earlier notes.
		_, rest, _ := strings.Cut(html.UnescapeString(r.Body.String()), `<div class="alert" role="status">`)
		notice, _, _ := strings.Cut(rest, "</div>")
		return code, notice
	}
	lastNote := func() string { e := x.events(order); return e[len(e)-1] }
	notified := func(uid, like string) string {
		return strings.Join(agStrings(t, x.testEnv, "SELECT body FROM notifications WHERE user_id=$1 AND body LIKE $2 ORDER BY created", uid, like), "\n")
	}

	code, page := signAndRead(roleVendor)
	if code != "escrow-signed" || !strings.Contains(page, "Signature accepted.") || !strings.Contains(page, checked) || !strings.Contains(page, "the market co-signs and broadcasts it on its next pass") {
		t.Fatalf("first signer's notice %q:\n%s", code, page)
	}
	buyerPage := html.UnescapeString(x.page("/order?id="+order, x.buyerSess))
	if i := strings.Index(buyerPage, "Signed by the vendor"); i < 0 || !strings.Contains(buyerPage[i:i+400], checked) {
		t.Fatalf("the party_signed status does not say when validity is checked:\n%s", buyerPage)
	}
	if note := lastNote(); !strings.Contains(note, "The vendor signed the escrow settlement") || !strings.Contains(note, checked) || !strings.Contains(note, "once one party has signed, the market co-signs") {
		t.Fatalf("the first signature's note: %s", note)
	}
	if got := notified(x.buyer, "%vendor signed its escrow settlement%"); !strings.Contains(got, "the vendor signed its escrow settlement") || !strings.Contains(got, checked) || !strings.Contains(got, "once one party has signed, the market co-signs") {
		t.Fatalf("the buyer's notification: %s", got)
	}

	code, page = signAndRead(roleBuyer)
	if code != "escrow-signed-both" || !strings.Contains(page, "Signature accepted.") || !strings.Contains(page, checked) ||
		!strings.Contains(page, "the market does not sign it, and broadcasts the transaction the two parties signed") || strings.Contains(page, "once one party has signed") || strings.Contains(page, "the market co-signs and broadcasts it on its next pass") {
		t.Fatalf("second signer's notice %q:\n%s", code, page)
	}
	for name, text := range map[string]string{"the note": lastNote(), "the vendor's notification": notified(x.vendor, "%buyer also signed its escrow settlement%")} {
		if !strings.Contains(text, "buyer also signed") || !strings.Contains(text, "the market does not sign it") || !strings.Contains(text, checked) ||
			!strings.Contains(text, "broadcasts the transaction the two parties signed") || strings.Contains(text, "once one party has signed") || strings.Contains(text, "market co-signs the settlement") {
			t.Fatalf("%s for the second signature: %s", name, text)
		}
	}
}

// A-352: an administrator decides a dispute on an escrow order, so its muted state line names one; a pooled order's
// still names a moderator.
func TestEscrowDisputedStateLineNamesAnAdministrator(t *testing.T) {
	x := newEscrowEnv(t)
	escrowOrder, _ := x.disputedEscrow(regtestAddr("state-line"))
	pooled := x.testEnv.order(x.buyer, x.testEnv.product(x.vendor, "digital"), "BTC", stateDisputed)
	// A-166 staff reads are keyed to the dispute record that an ordinary dispute transition writes with this state.
	agExec(x.testEnv, "INSERT INTO disputes(id,order_id,reason) VALUES($1,$2,'The parties referred this order for staff review.')", randomToken(), pooled)
	for _, sess := range []string{x.buyerSess, x.vendorSess} {
		page := x.page("/order?id="+escrowOrder, sess)
		if !strings.Contains(page, "An administrator will review the dispute and record an outcome.") || strings.Contains(page, "A moderator will review") {
			t.Fatalf("a disputed escrow order's state line:\n%s", page)
		}
		page = x.page("/order?id="+pooled, sess)
		if !strings.Contains(page, "Historical pooled order. Its payment and payout actions are retired") ||
			strings.Contains(page, "A moderator will review") || strings.Contains(page, "An administrator will review") {
			t.Fatalf("a disputed historical pooled order's state line:\n%s", page)
		}
	}
	// A-354: every staff viewer, moderator and administrator, reads that an administrator decides an escrow dispute;
	// historical pooled disputes remain read-only.
	_, mod := x.user("slmod", "moderator")
	_, admin := x.user("sladmin", "admin")
	// The state line itself names an administrator as the decider on an escrow order, for both; pooled history has no
	// moderator action.
	const escrowStaffLine = `<p class="muted">An administrator decides this escrow dispute and records the outcome on the moderation desk. The order page is read-only for market staff.</p>`
	const pooledStaffLine = `<p class="muted">Record the outcome on the moderation desk. The order page is read-only for moderators.</p>`
	for _, sess := range []string{mod, admin} {
		page := oneSpace(html.UnescapeString(x.page("/order?id="+escrowOrder, sess)))
		if !strings.Contains(page, "Only an administrator decides this escrow dispute") || strings.Contains(page, "record the outcome on the") ||
			!strings.Contains(page, escrowStaffLine) || strings.Contains(page, "Record the outcome on the moderation desk") {
			t.Fatalf("a staff viewer's line on a disputed escrow order:\n%s", page)
		}
		page = oneSpace(html.UnescapeString(x.page("/order?id="+pooled, sess)))
		if !strings.Contains(page, "Historical pooled order. Its payment and payout actions are retired") ||
			strings.Contains(page, pooledStaffLine) || strings.Contains(page, "An administrator decides this escrow dispute") {
			t.Fatalf("a staff viewer's line on a disputed pooled order:\n%s", page)
		}
	}
	if page := oneSpace(x.page("/order?id="+pooled, mod)); strings.Contains(page, "Only an administrator decides") {
		t.Fatalf("a moderator's action copy on a historical pooled order:\n%s", page)
	}
}

// A-353: once both parties have signed, the party_signed status says the market does not sign and broadcasts the
// parties' transaction, never that it co-signs; the waiting status names both paths.
func TestEscrowBothSignedStatusSaysTheMarketDoesNotSign(t *testing.T) {
	x := newEscrowEnv(t)
	order, id := x.built(regtestAddr("refund-both-status"), regtestAddr("claim-both-status"), 100000)
	waiting := html.UnescapeString(x.page("/order?id="+order, x.buyerSess))
	if !strings.Contains(waiting, "once one party has signed, the market co-signs the settlement on its next pass and broadcasts it to the regtest network; once both parties have signed, the market does not sign it and broadcasts the transaction they signed") {
		t.Fatalf("waiting status:\n%s", waiting)
	}
	x.signAs(order, roleVendor, id)
	if page := html.UnescapeString(x.page("/order?id="+order, x.buyerSess)); !strings.Contains(page, "The market co-signs it and broadcasts it on its next pass") {
		t.Fatalf("one signature:\n%s", page)
	}
	x.signAs(order, roleBuyer, id)
	for _, sess := range []string{x.buyerSess, x.vendorSess} {
		page := html.UnescapeString(x.page("/order?id="+order, sess))
		if !strings.Contains(page, "Signed by the vendor and the buyer") || !strings.Contains(page, "The market does not sign it, and broadcasts the transaction the two parties signed on its next pass. Nothing has been sent yet.") ||
			strings.Contains(page, "The market co-signs it and broadcasts it on its next pass") {
			t.Fatalf("both signed:\n%s", page)
		}
	}
	// Paused by the restore gate, it still says the market does not sign it.
	x.setGate(true)
	if page := html.UnescapeString(x.page("/order?id="+order, x.buyerSess)); !strings.Contains(page, "Signed by the vendor and the buyer") ||
		!strings.Contains(page, "so the market (which does not sign it) broadcasts the transaction the two parties signed only once the operator has finished") || strings.Contains(page, "co-signs it only once") {
		t.Fatalf("both signed, paused:\n%s", page)
	}
}

// A-355: every escrow statement of who completes a settlement covers both paths: one party's signature, which the market
// co-signs, and both parties' signatures, which the market broadcasts without signing.
func TestEscrowStatementsCoverBothPaths(t *testing.T) {
	for file, want := range map[string][]string{
		"web/templates/partials/order-escrow.html":  {"the market co-signs it and broadcasts it to the {{template \"btc-escrow-network\" $.EscrowNet}}; when both have signed it, the market does not sign it and broadcasts what they signed"},
		"web/templates/pages/order.html":            {"the market then co-signs it and broadcasts it to the {{template \"btc-escrow-network\" $.EscrowNet}} (when both sign it, the market does not sign it and broadcasts what they signed)"},
		"web/templates/partials/disputes-list.html": {"which the buyer or the vendor signs and the market co-signs (when both sign it, the market does not sign it and broadcasts what they signed)"},
		"docs/implementation-status.md":             {"a party signs, and the market then co-signs and broadcasts (when both parties sign it, the market broadcasts it without signing)"},
		"docs/operator-guide.md":                    {"which the market then co-signs and broadcasts (when both sign it, the market broadcasts it without signing)"},
	} {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		text := strings.Join(strings.Fields(string(b)), " ")
		for _, w := range want {
			if !strings.Contains(text, w) {
				t.Errorf("%s lacks %q", file, w)
			}
		}
	}
}
