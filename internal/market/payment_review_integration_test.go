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
	// A-98: the regression is announced to the party moderator as the buyer, not as a reviewer; non-party staff
	// get the review notification.
	short := partyOrder[:8]
	if a, b := p.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE 'Order '||$2||': credited TESTNET deposit%'", partyModID, short),
		p.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE 'Payment review needed for order '||$2||'%'", partyModID, short); a != 1 || b != 0 {
		t.Fatalf("party moderator notifications: party %d, review %d", a, b)
	}
	if n := p.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE 'Payment review needed for order '||$2||'%'", p.mod.ID, short); n != 1 {
		t.Fatalf("non-party moderator review notifications: %d", n)
	}

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
		// Open flags (both deposits are still conflicted) are listed oldest first.
		if strings.Index(body, secondTx) < strings.Index(body, first) {
			t.Fatal("open payment-review flags are not oldest first")
		}
	}
	// Buyers cannot open the desk; the disputes page carries no payment-review list.
	if w := p.do("GET", "/moderator", p.buyerSess, nil); w.Code != 403 {
		t.Fatalf("buyer opened the moderation desk: %d", w.Code)
	}
	mustNotContain(t, p.page("/disputes", p.buyerSess), `id="payment-review"`)

}

// A-98: every open (unhandled) flag is listed, oldest first and never capped, before the most recent other flags
// (credited deposits that confirmed again), which are capped with a count. A flag's reason comes from its
// deposit's current confirmations.
func TestPaymentReviewListsEveryOpenFlagOldestFirst(t *testing.T) {
	p := newPayEnv(t)
	_, modSess := p.user("omod_"+randomToken()[:6], "moderator")
	flag := func(txid string, credited bool, confs int, ago string) {
		t.Helper()
		if _, err := p.DB.Exec(`INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations,credited,flagged) VALUES($1,'BTC',$2,0,$3,1,$4,$5,true)`,
			p.order, txid, p.addr, confs, credited); err != nil {
			t.Fatal(err)
		}
		if _, err := p.DB.Exec(`INSERT INTO order_events(order_id,from_state,to_state,actor_id,note,created) VALUES($1,'paid','paid',NULL,$2,now()-$3::interval)`,
			p.order, "TESTNET deposit "+txid+" arrived after this order was settled and was not paid out. Moderator review required.", ago); err != nil {
			t.Fatal(err)
		}
	}
	// One old open flag, 101 newer open flags and 101 flags whose credited deposit confirmed again.
	flag("open-old", false, 3, "30 days")
	for i := range paymentReviewLimit + 1 {
		flag(fmt.Sprintf("open-%03d", i), false, 3, fmt.Sprintf("%d minutes", 200-i))
		flag(fmt.Sprintf("again-%03d", i), true, 5, fmt.Sprintf("%d minutes", 400-i))
	}
	body := p.page("/moderator", modSess)
	rows := strings.Split(body, `<tr class="payment-review-row">`)[1:]
	for i, r := range rows {
		rows[i] = r[:strings.Index(r, "</tr>")]
	}
	if len(rows) != paymentReviewLimit+2+paymentReviewLimit {
		t.Fatalf("payment-review rows: %d", len(rows))
	}
	mustContain(t, rows[0], "open-old:0", "Open", "Deposit confirmed after settlement, not paid out")
	for i := range paymentReviewLimit + 1 {
		mustContain(t, rows[1+i], fmt.Sprintf("open-%03d:0", i), "Open")
	}
	// Then the 100 most recently flagged others, newest first; the oldest one is cut and counted.
	others := rows[paymentReviewLimit+2:]
	for i, r := range others {
		mustContain(t, r, fmt.Sprintf("again-%03d:0", paymentReviewLimit-i), "Credited deposit confirmed again (5 confirmations)")
		mustNotContain(t, r, "Open")
	}
	mustNotContain(t, body, "again-000:0")
	mustContain(t, body, fmt.Sprintf("%d open flags", paymentReviewLimit+2),
		fmt.Sprintf("Showing the %d most recent of %d other flags", paymentReviewLimit, paymentReviewLimit+1))
}
