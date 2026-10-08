package market

import (
	"errors"
	"html"
	"strings"
	"testing"
)

// A-120: a payout's recipient sees every payout to them that is not sent, with a fixed reason and next step per
// state (never the wallet's error text) and the payout's destination; nobody else sees that destination.

// insertPayout queues a payout directly (bypassing the watcher) on a new order in orderState and returns the order.
// age backdates updated (a stuck send), e.g. "10 minutes"; "" leaves it now.
func (p *payEnv) insertPayout(orderState, kind, userID, state, address, errText string, ambiguous bool, age string) string {
	p.t.Helper()
	order, _ := p.newOrder(orderState)
	if age == "" {
		age = "0 seconds"
	}
	if _, err := p.DB.Exec(`INSERT INTO payouts(order_id,kind,user_id,currency,amount,address,state,error,send_ambiguous,updated)
		VALUES($1,$2,$3,'BTC',100000,$4,$5,$6,$7,now()-$8::interval)`, order, kind, userID, address, state, errText, ambiguous, age); err != nil {
		p.t.Fatal(err)
	}
	return order
}

// unsentList returns the viewer's list of unsent payouts on /account (the table under #payout).
func unsentList(t *testing.T, page string) string {
	t.Helper()
	i := strings.Index(page, `id="payout"`)
	if i < 0 {
		t.Fatal("no #payout panel on /account")
	}
	page = page[i:]
	i = strings.Index(page, `id="unsent-payouts"`)
	if i < 0 {
		t.Fatal("no unsent payout list in the #payout panel")
	}
	page = page[i:]
	return page[:strings.Index(page, "</table>")]
}

// unsentRow returns the row of the unsent payout list linking to order ("" when it is not listed).
func unsentRow(list, order string) string {
	i := strings.Index(list, `href="/order?id=`+order+`"`)
	if i < 0 {
		return ""
	}
	start := strings.LastIndex(list[:i], "<tr")
	end := strings.Index(list[i:], "</tr>")
	return list[start : i+end]
}

func TestRecipientSeesEveryUnsentPayout(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor-dest")
	// Two real watcher failures, oldest: a definite wallet rejection and an ambiguous one.
	definite, _ := p.failPayout("tx-r-def", &rpcError{Method: "fake RPC send", Code: -6, Message: "Insufficient funds RAW-WALLET-DEFINITE"})
	ambiguous, _ := p.failPayout("tx-r-amb", errors.New("bitcoin RPC unreachable: RAW-WALLET-AMBIGUOUS"))
	// "administrators have been notified" is true for a failure: the watcher notifies them with the failure record.
	for _, order := range []string{definite, ambiguous} {
		if n := p.count("SELECT count(*) FROM notifications n JOIN users u ON u.id=n.user_id WHERE u.role='admin' AND n.body LIKE $1", "Payout failed for order "+order[:8]+"%"); n != 1 {
			t.Fatalf("administrator notifications for the failure of %s: %d", order[:8], n)
		}
	}
	v := p.vendor.ID
	blocked := p.insertPayout(stateCompleted, "release", v, "blocked", "", "", false, "")
	watcher := p.insertPayout(stateCompleted, "release", v, "held", "fake-testnet-vendor-dest", heldReason, false, "")
	suspended := p.insertPayout(stateCompleted, "release", v, "held", "fake-testnet-vendor-dest", suspendedHold, false, "")
	restored := p.insertPayout(stateCompleted, "release", v, "held", "fake-testnet-vendor-dest", restoredHoldPrefix+" verify in the wallet before releasing; this payout may already have been sent.", false, "")
	stuck := p.insertPayout(stateCompleted, "release", v, "sending", "fake-testnet-vendor-dest", "", false, "10 minutes")
	sending := p.insertPayout(stateCompleted, "release", v, "sending", "fake-testnet-vendor-dest", "", false, "")
	pending := p.insertPayout(stateCompleted, "release", v, "pending", "fake-testnet-vendor-dest", "", false, "")
	sent := p.insertPayout(stateCompleted, "release", v, "sent", "fake-testnet-vendor-dest", "", false, "")
	// The buyer's refund is theirs alone.
	refund := p.insertPayout(stateCancelled, "refund", p.buyer.ID, "pending", "fake-testnet-buyer-refund", "", false, "")
	// More than 100 newer unsent payouts: nothing older drops off.
	var newer []string
	for range 101 {
		newer = append(newer, p.insertPayout(stateCompleted, "release", v, "pending", "fake-testnet-vendor-dest", "", false, ""))
	}
	oldest := []string{definite, ambiguous, blocked, watcher, suspended, restored, stuck, sending, pending}

	page := html.UnescapeString(p.page("/account", p.vendorSess))
	list := unsentList(t, page)
	definitePayoutID := p.str("SELECT id::text FROM payouts WHERE order_id=$1", definite)
	mustContain(t, list, `<th scope="col">Payout ID</th>`, `<td class="mono payout-id">#`+definitePayoutID+`</td>`)
	if n := strings.Count(list, `href="/order?id=`); n != len(oldest)+len(newer) {
		t.Fatalf("unsent payouts listed: %d, want %d", n, len(oldest)+len(newer))
	}
	last := -1
	for _, order := range append(oldest, newer...) {
		i := strings.Index(list, `href="/order?id=`+order+`"`)
		if i < 0 {
			t.Fatalf("unsent payout for %s is not listed", order[:8])
		}
		if i < last {
			t.Fatalf("payout for %s is listed before an older one", order[:8])
		}
		last = i
	}
	if unsentRow(list, sent) != "" || unsentRow(list, refund) != "" {
		t.Fatal("a sent payout or another account's payout is listed")
	}
	mustContain(t, list, "0.001 BTC", "Release")
	// Fixed copy per state; the wallet's error text and the stored reasons never appear.
	for order, want := range map[string][]string{
		blocked:   {"Save a BTC payout address"},
		definite:  {"The market wallet refused the send before broadcasting; administrators have been notified", "fake-testnet-vendor-dest"},
		ambiguous: {"may have been broadcast; an administrator checks the wallet before any resend"},
		stuck:     {"may have been broadcast; an administrator checks the wallet before any resend"},
		watcher:   {"re-confirming", "releases the payout by itself"},
		suspended: {"account was suspended", "checks your payout address"},
		restored:  {"restored", "checks the market wallet"},
		sending:   {"Being sent"},
		pending:   {"Queued"},
	} {
		row := unsentRow(list, order)
		mustContain(t, row, want...)
		mustContain(t, row, order[:8])
	}
	mustNotContain(t, unsentRow(list, sending), "may have been broadcast")
	mustNotContain(t, page, "RAW-WALLET", "Insufficient funds", heldReason, suspendedHold, "Suspended account:", "Restored from backup:", "pre-broadcast error", "fake-testnet-buyer-refund")
	// The buyer sees their refund and its destination, and nothing of the vendor's.
	buyerList := unsentList(t, html.UnescapeString(p.page("/account", p.buyerSess)))
	if n := strings.Count(buyerList, `href="/order?id=`); n != 1 || !strings.Contains(unsentRow(buyerList, refund), "fake-testnet-buyer-refund") {
		t.Fatalf("buyer's unsent payouts: %s", buyerList)
	}
	mustNotContain(t, buyerList, `<td class="mono payout-id">#`+definitePayoutID+`</td>`)
	otherPartyOrder := html.UnescapeString(p.page("/order?id="+definite, p.buyerSess))
	mustNotContain(t, otherPartyOrder, "payout-id", "Payout ID")
	mustContain(t, buyerList, "Refund")
	mustNotContain(t, buyerList, "fake-testnet-vendor-dest")

	// The vendor desk shows one count linking to the list.
	desk := html.UnescapeString(p.page("/vendor-dashboard", p.vendorSess))
	mustContain(t, desk, `href="/account#payout"`, "110 unsent payout(s) to you")
}

func TestOrderPageShowsPayoutNextStepToRecipientOnly(t *testing.T) {
	p := newPayEnv(t)
	modSess := agSession(p.testEnv, p.mod.ID)
	// A cancelled order whose refund waits for the buyer's address.
	refund := p.insertPayout(stateCancelled, "refund", p.buyer.ID, "blocked", "", "", false, "")
	buyer := html.UnescapeString(p.page("/order?id="+refund, p.buyerSess))
	mustNotContain(t, buyer, "No further changes are possible")
	mustContain(t, buyer, "Your refund of 0.001 BTC <span class=\"badge testnet\">TESTNET fake</span> has not been sent", "Save a BTC payout address", `href="/account#payout"`)
	vendor := html.UnescapeString(p.page("/order?id="+refund, p.vendorSess))
	mustContain(t, vendor, "No further changes are possible")
	mustNotContain(t, vendor, "has not been sent", `href="/account#payout"`)

	// A resolved order (readable by staff) whose release to the vendor is held for a suspension.
	release := p.insertPayout(stateResolved, "release", p.vendor.ID, "held", "fake-testnet-vendor-dest", suspendedHold, false, "")
	vendor = html.UnescapeString(p.page("/order?id="+release, p.vendorSess))
	mustNotContain(t, vendor, "No further changes are possible", "deposit conflicted")
	mustContain(t, vendor, "Your release of 0.001 BTC <span class=\"badge testnet\">TESTNET fake</span> has not been sent", "checks your payout address", "fake-testnet-vendor-dest", `href="/account#payout"`)
	// The buyer and staff see a neutral label, never the destination or the reason.
	for who, sess := range map[string]string{"buyer": p.buyerSess, "moderator": modSess, "administrator": p.adminSess} {
		page := html.UnescapeString(p.page("/order?id="+release, sess))
		mustContain(t, page, "Held — waiting for an administrator check")
		for _, bad := range []string{"fake-testnet-vendor-dest", "suspended", "deposit conflicted", `href="/account#payout"`, "has not been sent"} {
			if strings.Contains(page, bad) {
				t.Fatalf("%s view of the order contains %q", who, bad)
			}
		}
	}

	// A sent payout: its recipient sees the destination, the other party does not; the order is closed for both.
	sent := p.insertPayout(stateCompleted, "release", p.vendor.ID, "sent", "fake-testnet-vendor-sent", "", false, "")
	vendor = html.UnescapeString(p.page("/order?id="+sent, p.vendorSess))
	mustContain(t, vendor, "fake-testnet-vendor-sent", "No further changes are possible")
	buyer = html.UnescapeString(p.page("/order?id="+sent, p.buyerSess))
	mustContain(t, buyer, "No further changes are possible")
	mustNotContain(t, buyer, "fake-testnet-vendor-sent")

	// A definite failure: the recipient's order page gives the fixed failure copy, not the wallet's text.
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor-dest")
	failed, _ := p.failPayout("tx-o-def", &rpcError{Method: "fake RPC send", Code: -6, Message: "RAW-WALLET-TEXT"})
	vendor = html.UnescapeString(p.page("/order?id="+failed, p.vendorSess))
	mustContain(t, vendor, "The market wallet refused the send before broadcasting; administrators have been notified")
	mustNotContain(t, vendor, "RAW-WALLET-TEXT", "No further changes are possible")
}

func TestSuspensionNoticesCountHeldPayouts(t *testing.T) {
	p := newPayEnv(t)
	for range 2 {
		p.insertPayout(stateCompleted, "release", p.vendor.ID, "pending", "fake-testnet-vendor-dest", "", false, "")
	}
	p.check(p.do("POST", "/admin/suspend", p.adminSess, suspendForm("suspend", p.vendor.Handle)), 303)
	mustContain(t, p.str("SELECT body FROM notifications WHERE user_id=$1 AND body LIKE 'An administrator suspended%'", p.vendor.ID),
		"2 unsent payout(s) to you are held until an administrator checks your payout address")
	p.check(p.do("POST", "/admin/suspend", p.adminSess, suspendForm("restore", p.vendor.Handle)), 303)
	mustContain(t, p.str("SELECT body FROM notifications WHERE user_id=$1 AND body LIKE 'An administrator restored%'", p.vendor.ID),
		"2 payout(s) to you stay held until an administrator checks your payout address", "account page")
	// Saving an address records nothing in the order histories (A-120: no order event per payout per save).
	events := p.count("SELECT count(*) FROM order_events")
	p.saveVendorAddress("fake-testnet-vendor-new")
	if n := p.count("SELECT count(*) FROM order_events"); n != events {
		t.Fatalf("saving a payout address added %d order events", n-events)
	}
}

// A-214: on Bitcoin a payout's transaction ID reveals its recipient's payout address, and a vendor can force a
// refund on demand, so only a payout's recipient and staff reviewers see its txid: never the other party, on the
// payouts list, in the order history (including notes stored before this rule), a dispute decision or a
// notification.
func TestPayoutTxidShownOnlyToRecipient(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	p.setPayoutAddress(p.buyer, "fake-testnet-buyer")
	modSess := agSession(p.testEnv, p.mod.ID)
	pages := func(order, sess string) string {
		return html.UnescapeString(p.page("/order?id="+order, sess) + p.page("/disputes", sess) + p.page("/notifications", sess))
	}
	noticed := func(u *User, txid string) bool {
		return p.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND strpos(body,$2)>0", u.ID, txid) > 0
	}
	// shownTo checks that txid appears to the recipient and a staff reviewer and never to the other party.
	shownTo := func(what, order, txid string, recipient, other *User, recipientSess, otherSess string) {
		t.Helper()
		if txid == "" {
			t.Fatalf("%s has no txid", what)
		}
		// A flagged payment lets a moderator who is not a party read the order (staffReviewer).
		if _, err := p.DB.Exec("UPDATE payments SET flagged=true WHERE order_id=$1", order); err != nil {
			t.Fatal(err)
		}
		if page := html.UnescapeString(p.page("/order?id="+order, recipientSess)); !strings.Contains(page, txid) {
			t.Fatalf("%s: the recipient's order page does not show its txid", what)
		}
		if !noticed(recipient, txid) {
			t.Fatalf("%s: the recipient's notification has no txid", what)
		}
		if page := html.UnescapeString(p.page("/order?id="+order, modSess)); !strings.Contains(page, txid) {
			t.Fatalf("%s: the staff reviewer does not see its txid", what)
		}
		page := pages(order, otherSess)
		if strings.Contains(page, txid) || strings.Contains(page, truncate(txid, 20)) {
			t.Fatalf("%s: the other party sees its txid %s", what, txid)
		}
		if noticed(other, txid) || noticed(other, truncate(txid, 20)) {
			t.Fatalf("%s: the other party was notified of its txid %s", what, txid)
		}
	}

	// An overpaid order completed: the release to the vendor and the return of the excess to the buyer are sent.
	p.paidByWatcherAmount(150000)
	p.complete(p.order)
	p.poll()
	release := p.str("SELECT txid FROM payouts WHERE order_id=$1 AND kind='release' AND state='sent'", p.order)
	ret := p.str("SELECT txid FROM payouts WHERE order_id=$1 AND kind='return' AND state='sent'", p.order)
	shownTo("release", p.order, release, p.vendor, p.buyer, p.vendorSess, p.buyerSess)
	shownTo("return", p.order, ret, p.buyer, p.vendor, p.buyerSess, p.vendorSess)

	// A vendor-forced refund: the vendor cancels an order it saw paid, and the refund is sent to the buyer.
	forced, addr := p.newOrder(stateAwaitingPayment)
	p.paidByWatcher(forced, addr, "tx-forced")
	p.move(forced, statePaid, stateCancelled, p.vendor)
	p.poll()
	refund := p.str("SELECT txid FROM payouts WHERE order_id=$1 AND kind='refund' AND state='sent'", forced)
	shownTo("vendor-forced refund", forced, refund, p.buyer, p.vendor, p.buyerSess, p.vendorSess)

	// A release an administrator records as sent after checking the wallet, with a 64-hex txid.
	failed, id := p.failPayout("tx-marked", &rpcError{Method: "fake RPC send", Code: -6, Message: "Insufficient funds"})
	marked := strings.Repeat("ab", 32)
	if code, body := p.payoutAction(p.adminSess, id, "sent", marked, testPassword); code != 303 {
		t.Fatalf("mark sent: %d %s", code, body)
	}
	shownTo("release marked sent", failed, marked, p.vendor, p.buyer, p.vendorSess, p.buyerSess)
	// New order-history notes name no payout txid at all (the watcher's sent notes and the administrator's).
	if n := p.count("SELECT count(*) FROM order_events e JOIN payouts py ON py.order_id=e.order_id WHERE py.txid<>'' AND strpos(e.note,py.txid)>0"); n != 0 {
		t.Fatalf("%d order-history notes name a payout txid", n)
	}
	if n := p.count("SELECT count(*) FROM order_events WHERE order_id=ANY($1) AND (note LIKE '%payout of % sent; the network fee was taken from it.' OR note LIKE '%recorded as sent by an administrator after wallet reconciliation.')", []string{p.order, forced, failed}); n != 4 {
		t.Fatalf("sent notes %d, want 4", n)
	}

	// Text stored before this rule named the txid in the order history (the watcher's and the administrator's
	// notes, in full or truncated) and in a dispute decision recorded from the runbook: the other party still
	// never sees it; the recipient does.
	legacy := strings.Repeat("cd", 32)
	legacyOrder := p.insertPayout(stateResolved, "refund", p.buyer.ID, "sent", "fake-testnet-buyer", "", false, "")
	if _, err := p.DB.Exec("UPDATE payouts SET txid=$2 WHERE order_id=$1", legacyOrder, legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := p.DB.Exec("INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations,credited) VALUES($1,'BTC','tx-legacy',0,'fake-legacy',100000,3,true)", legacyOrder); err != nil {
		t.Fatal(err)
	}
	for _, note := range []string{"TESTNET payout of 0.001 BTC sent: " + legacy, "TESTNET payout of 0.001 BTC recorded as sent by an administrator after wallet reconciliation: " + legacy, "Payout " + truncate(legacy, 20) + " noted."} {
		if _, err := p.DB.Exec("INSERT INTO order_events(order_id,from_state,to_state,note) VALUES($1,'resolved','resolved',$2)", legacyOrder, note); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.DB.Exec("INSERT INTO disputes(id,order_id,reason,status,outcome,resolution) VALUES($1,$2,'Nothing arrived at all','Resolved — refund to buyer','refund',$3)",
		randomToken(), legacyOrder, "Reconciled after a restore from backup: the refund was sent in transaction "+legacy+"."); err != nil {
		t.Fatal(err)
	}
	if _, err := p.DB.Exec("INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), p.buyer.ID, "Order "+legacyOrder[:8]+": TESTNET payout of 0.001 BTC sent: "+legacy); err != nil {
		t.Fatal(err)
	}
	shownTo("refund in stored notes", legacyOrder, legacy, p.buyer, p.vendor, p.buyerSess, p.vendorSess)
	if page := pages(legacyOrder, p.vendorSess); !strings.Contains(page, payoutTxidHidden) {
		t.Fatal("the vendor's order history does not say the transaction ID is hidden")
	}
}
