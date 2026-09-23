package market

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// A-24: moderators and administrators notified by flagOrder can read the flagged order (read-only) and find
// it on the moderation desk's payment-review list.

// orderFor inserts an awaiting-payment order by buyer on product with a provider-issued deposit address.
func (p *payEnv) orderFor(buyerID, productID string) (string, string) {
	p.t.Helper()
	id := p.testEnv.order(buyerID, productID, "BTC", stateAwaitingPayment)
	addr, err := p.fake.NewAddress(context.Background(), id)
	if err != nil {
		p.t.Fatal(err)
	}
	if _, err = p.DB.Exec("INSERT INTO payment_addresses(order_id,currency,address,provider) VALUES($1,'BTC',$2,'fake')", id, addr); err != nil {
		p.t.Fatal(err)
	}
	return id, addr
}

func longTxid(prefix string) string {
	return prefix + strings.Repeat("0123456789abcdef", 4)[len(prefix):]
}

func TestFlaggedOrderReadableByNotifiedStaff(t *testing.T) {
	p := newPayEnv(t)
	suffix := randomToken()[:6]
	_, modSess := p.user("fmod_"+suffix, "moderator")
	partyModID, partyModSess := p.user("fpartymod_"+suffix, "moderator")
	staff := map[string]string{"moderator": modSess, "admin": p.adminSess}

	// A physical order paid by the watcher whose credited deposit later conflicts.
	txid := longTxid("aa")
	p.paidByWatcher(p.order, p.addr, txid)
	for name, sess := range staff {
		if w := p.do("GET", "/order?id="+p.order, sess, nil); w.Code != 404 {
			t.Fatalf("%s opened an unflagged, undisputed order: %d", name, w.Code)
		}
	}
	p.fake.SetConfirmations(txid, -1)
	p.poll()
	if n := p.count("SELECT count(*) FROM payments WHERE order_id=$1 AND flagged", p.order); n != 1 {
		t.Fatalf("conflicted deposit not flagged: %d", n)
	}

	// An unflagged, undisputed paid order stays hidden from staff.
	clean, cleanAddr := p.newOrder(stateAwaitingPayment)
	p.paidByWatcher(clean, cleanAddr, longTxid("bb"))

	for name, sess := range staff {
		body := p.page("/order?id="+p.order, sess)
		mustContain(t, body,
			"Moderator review (read-only)",
			"is now conflicted or missing from the wallet", // flag note in the history
			txid+":0", // deposit txid in full
			"a payment on this order was flagged for review",
			`href="/moderator#payment-review"`,
		)
		mustNotContain(t, body, `action="/orders/`, `action="/reviews"`, `action="/disputes"`, "Mark as shipped", "Cancel order", "Send your shipping address")
		// The actions themselves still refuse a non-party.
		if w := p.do("POST", "/orders/ship", sess, form("order_id", p.order)); w.Code != 404 && w.Code != 403 {
			t.Fatalf("%s acted on a flagged order: %d", name, w.Code)
		}
		if w := p.do("GET", "/order?id="+clean, sess, nil); w.Code != 404 {
			t.Fatalf("%s opened an unflagged, undisputed order: %d", name, w.Code)
		}
	}
	if s := p.state(p.order); s != statePaid {
		t.Fatalf("staff view changed the order: %s", s)
	}

	// A locked transfer flags an order still awaiting payment: staff see the address, not the buyer's instructions.
	awaiting, awaitingAddr := p.newOrder(stateAwaitingPayment)
	p.fake.outputs = append(p.fake.outputs, Incoming{Address: awaitingAddr, TxID: longTxid("ab"), Amount: 5000, Confirmations: 3, Locked: true})
	p.poll()
	for _, sess := range staff {
		body := p.page("/order?id="+awaiting, sess)
		mustContain(t, body, "Moderator review (read-only)", "has an unlock time", awaitingAddr, "Deposit address issued to the buyer")
		mustNotContain(t, body, "top up only this remaining amount", `action="/orders/`)
	}
	mustContain(t, p.page("/moderator", modSess), "Locked transfer (unlock time)", longTxid("ab"))

	// A moderator who is party to a flagged order keeps the party view, with the party's forms.
	partyOrder, partyAddr := p.orderFor(partyModID, p.productID)
	partyTx := longTxid("cc")
	p.paidByWatcher(partyOrder, partyAddr, partyTx)
	p.fake.SetConfirmations(partyTx, -1)
	p.poll()
	body := p.page("/order?id="+partyOrder, partyModSess)
	mustContain(t, body, "you are the buyer", `action="/disputes"`)
	mustNotContain(t, body, "Moderator review (read-only)")

	// Digital delivery content is withheld from staff until the order is disputed.
	digital := p.product(p.vendor.ID, "digital")
	dOrder, dAddr := p.orderFor(p.buyer.ID, digital)
	dTx := longTxid("dd")
	p.paidByWatcher(dOrder, dAddr, dTx)
	if _, err := p.DB.Exec("INSERT INTO deliveries(order_id,content) VALUES($1,'licence-key-FLAG-42')", dOrder); err != nil {
		t.Fatal(err)
	}
	p.move(dOrder, statePaid, stateDelivered, p.vendor)
	p.fake.SetConfirmations(dTx, -1)
	p.poll()
	for _, sess := range staff {
		body := p.page("/order?id="+dOrder, sess)
		mustContain(t, body, "Moderator review (read-only)", "shown to moderators only if the order is disputed")
		mustNotContain(t, body, "licence-key-FLAG-42", "Nothing has been delivered")
	}
	mustContain(t, p.page("/order?id="+dOrder, p.buyerSess), "licence-key-FLAG-42")
	if w := p.do("POST", "/disputes", p.buyerSess, form("order_id", dOrder, "reason", "The licence key does not activate the product.")); w.Code != 303 {
		t.Fatalf("dispute: %d %s", w.Code, w.Body.String())
	}
	for _, sess := range staff {
		mustContain(t, p.page("/order?id="+dOrder, sess), "licence-key-FLAG-42")
	}
}

func TestModeratorDeskListsPaymentReviews(t *testing.T) {
	p := newPayEnv(t)
	_, modSess := p.user("dmod_"+randomToken()[:6], "moderator")

	// Empty state.
	body := p.page("/moderator", modSess)
	mustContain(t, body, `id="payment-review"`, "No payments are flagged for review.")

	first := longTxid("e1")
	p.paidByWatcher(p.order, p.addr, first)
	p.fake.SetConfirmations(first, -1)
	p.poll()
	second, secondAddr := p.newOrder(stateAwaitingPayment)
	secondTx := longTxid("e2")
	p.paidByWatcher(second, secondAddr, secondTx)
	p.fake.SetConfirmations(secondTx, -1)
	p.poll()

	for _, sess := range []string{modSess, p.adminSess} {
		body = p.page("/moderator", sess)
		mustContain(t, body, "Payment review", `href="/order?id=`+p.order+`"`, p.order, second, first, secondTx,
			amount(100000, currencyDecimals("BTC"))+" BTC", "Credited deposit conflicted or missing")
		// The flag time comes from the watcher's flag event.
		mustNotContain(t, body, "No payments are flagged for review.", "Not recorded")
		// Newest flag first.
		if strings.Index(body, secondTx) > strings.Index(body, first) {
			t.Fatal("payment-review list is not newest first")
		}
	}
	// Buyers cannot open the desk; the disputes page carries no payment-review list.
	if w := p.do("GET", "/moderator", p.buyerSess, nil); w.Code != 403 {
		t.Fatalf("buyer opened the moderation desk: %d", w.Code)
	}
	mustNotContain(t, p.page("/disputes", p.buyerSess), `id="payment-review"`)

	// The list is capped with a truncation notice.
	for i := range paymentReviewLimit + 1 {
		if _, err := p.DB.Exec(`INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations,flagged) VALUES($1,'BTC',$2,0,$3,1,3,true)`,
			second, fmt.Sprintf("bulk-%03d", i), secondAddr); err != nil {
			t.Fatal(err)
		}
	}
	body = p.page("/moderator", modSess)
	mustContain(t, body, fmt.Sprintf("Showing the %d most recently flagged payments", paymentReviewLimit))
	if n := strings.Count(body, `<tr class="payment-review-row">`); n != paymentReviewLimit {
		t.Fatalf("payment-review rows: %d", n)
	}
}
