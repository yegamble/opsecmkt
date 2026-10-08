package market

import (
	"context"
	"strings"
	"testing"
)

func TestPooledDepositIdentifiersHiddenFromVendor(t *testing.T) {
	p := newPayEnv(t)
	txid := strings.Repeat("a", 64)
	p.fake.Deposit(p.addr, txid, 0, 40000, 1)
	p.poll()
	// An open payment flag permits independent staff to read this otherwise private order.
	agExec(p.testEnv, "UPDATE payments SET flagged=true WHERE order_id=$1", p.order)
	for _, viewer := range []struct {
		name, sess string
		hidden     bool
	}{
		{"buyer", p.buyerSess, false},
		{"vendor", p.vendorSess, true},
		{"moderator", agSession(p.testEnv, p.mod.ID), false},
		{"administrator", p.adminSess, false},
	} {
		body := p.page("/order?id="+p.order, viewer.sess)
		if viewer.hidden {
			mustNotContain(t, body, p.addr, txid, truncate(txid, 20), "bitcoin:", "payment-qr")
			mustContain(t, body, "0.0004 BTC", "1", "Waiting for confirmations", "publicly traceable")
		} else {
			mustContain(t, body, p.addr, txid)
		}
	}
}

func TestXMRPooledDepositIdentifiersHiddenFromVendor(t *testing.T) {
	p := newPayEnv(t)
	xmr := newFakeProvider("XMR", 3)
	p.A.payments["XMR"] = xmr
	order := p.testEnv.order(p.buyer.ID, p.productID, "XMR", stateAwaitingPayment)
	addr, err := xmr.NewAddress(context.Background(), order)
	if err != nil {
		t.Fatal(err)
	}
	agExec(p.testEnv, "INSERT INTO payment_addresses(order_id,currency,address,provider) VALUES($1,'XMR',$2,'fake')", order, addr)
	txid := strings.Repeat("f", 64)
	xmr.Deposit(addr, txid, 0, 100000000000, 1)
	p.poll()
	buyer := p.page("/order?id="+order, p.buyerSess)
	vendor := p.page("/order?id="+order, p.vendorSess)
	mustContain(t, buyer, addr, txid)
	mustNotContain(t, vendor, addr, txid, truncate(txid, 20), "payment-qr", "publicly traceable")
	mustContain(t, vendor, "0.1 XMR", "Waiting for confirmations")
}

func TestPooledPrivacyLeavesEscrowVerificationVisible(t *testing.T) {
	x := newEscrowEnv(t)
	addr := x.mustCheckout(x.order, regtestAddr("a84 escrow refund"))
	page := x.page("/order?id="+x.order, x.vendorSess)
	mustContain(t, page, addr, "Escrow descriptor", "Load it into your own wallet")
}

func TestHistoricalPooledNotesAndNotificationsRedactedForVendor(t *testing.T) {
	p := newPayEnv(t)
	txid := strings.Repeat("b", 64)
	p.fake.Deposit(p.addr, txid, 0, 40000, 1)
	p.poll()
	agExec(p.testEnv, "UPDATE orders SET state='disputed' WHERE id=$1", p.order)
	legacy := "Legacy deposit " + p.addr + " transaction " + txid + " abbreviated " + truncate(txid, 20)
	agExec(p.testEnv, "INSERT INTO order_events(order_id,from_state,to_state,note) VALUES($1,'disputed','disputed',$2)", p.order, legacy)
	agExec(p.testEnv, "INSERT INTO disputes(id,order_id,reason,status,resolution) VALUES($1,$2,$3,'Resolved — refund to buyer',$3)", randomToken(), p.order, legacy)
	for _, uid := range []string{p.vendor.ID, p.buyer.ID, p.mod.ID} {
		agExec(p.testEnv, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), uid, "Order "+shortID(p.order)+": "+legacy)
	}
	for _, path := range []string{"/order?id=" + p.order, "/disputes", "/notifications"} {
		vendor := p.page(path, p.vendorSess)
		mustNotContain(t, vendor, p.addr, txid, truncate(txid, 20))
		mustContain(t, vendor, "deposit address shown only to the buyer", "transaction ID shown only to the buyer")
		mustContain(t, p.page(path, p.buyerSess), p.addr, txid)
		mustContain(t, p.page(path, agSession(p.testEnv, p.mod.ID)), p.addr, txid)
	}
	if got := p.str("SELECT note FROM order_events WHERE order_id=$1 AND note=$2", p.order, legacy); got != legacy {
		t.Fatal("render redaction rewrote signed order history")
	}
}

func TestPooledLegacyNotificationCandidatesNeedNoOrderPrefix(t *testing.T) {
	p := newPayEnv(t)
	full := strings.Repeat("1", 64)
	short := strings.Repeat("2", 64)
	for _, txid := range []string{full, short} {
		agExec(p.testEnv, "INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations) VALUES($1,'BTC',$2,0,$3,10000,1)", p.order, txid, p.addr)
	}
	bodies := []string{"Old address " + p.addr, "Full identifier " + full, "Truncated identifier " + truncate(short, 20)}
	for _, body := range bodies {
		for _, uid := range []string{p.vendor.ID, p.buyer.ID} {
			agExec(p.testEnv, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), uid, body)
		}
	}
	vendor := p.page("/notifications", p.vendorSess)
	mustNotContain(t, vendor, p.addr, full, truncate(short, 20))
	mustContain(t, vendor, pooledAddressHidden, pooledTxidHidden)
	buyer := p.page("/notifications", p.buyerSess)
	mustContain(t, buyer, p.addr, full, truncate(short, 20))
}

func TestPooledRegressionNoticeKeepsVendorTxidPrivate(t *testing.T) {
	p := newPayEnv(t)
	txid := strings.Repeat("c", 64)
	p.paidByWatcher(p.order, p.addr, txid)
	p.fake.Drop(txid)
	p.poll()
	buyer := p.str("SELECT body FROM notifications WHERE user_id=$1 AND body LIKE 'Order %credited %deposit%'", p.buyer.ID)
	vendor := p.str("SELECT body FROM notifications WHERE user_id=$1 AND body LIKE 'Order %credited %deposit%'", p.vendor.ID)
	staff := p.str("SELECT body FROM notifications WHERE user_id=$1 AND body LIKE 'Payment review needed%'", p.mod.ID)
	if !strings.Contains(buyer, truncate(txid, 20)) || !strings.Contains(staff, truncate(txid, 20)) || strings.Contains(vendor, txid) || strings.Contains(vendor, truncate(txid, 20)) {
		t.Fatalf("regression notices: buyer=%q vendor=%q staff=%q", buyer, vendor, staff)
	}
	mustContain(t, vendor, "credited", "deposit", "market staff have been notified")
	mustNotContain(t, p.page("/order?id="+p.order, p.vendorSess), txid, truncate(txid, 20))
	mustContain(t, p.page("/order?id="+p.order, p.buyerSess), truncate(txid, 20))
	mustContain(t, p.page("/order?id="+p.order, agSession(p.testEnv, p.mod.ID)), truncate(txid, 20))
}

func TestPooledFundedDepositNoticeKeepsVendorTxidPrivate(t *testing.T) {
	p := newPayEnv(t)
	p.paidByWatcher(p.order, p.addr, strings.Repeat("d", 64))
	extra := strings.Repeat("e", 64)
	p.fake.Deposit(p.addr, extra, 1, 50000, 3)
	p.poll()
	query := "SELECT body FROM notifications WHERE user_id=$1 AND body LIKE '%received after this order was funded%'"
	buyer, vendor := p.str(query, p.buyer.ID), p.str(query, p.vendor.ID)
	if !strings.Contains(buyer, truncate(extra, 20)) || strings.Contains(vendor, extra) || strings.Contains(vendor, truncate(extra, 20)) {
		t.Fatalf("funded-deposit notices: buyer=%q vendor=%q", buyer, vendor)
	}
	mustContain(t, vendor, "deposit", "received after this order was funded")
}
