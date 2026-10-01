package market

import (
	"html"
	"net/http"
	"net/url"
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
