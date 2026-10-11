package market

import (
	"strings"
	"testing"
)

// Historical pooled identifiers remain readable to the buyer and authorized staff, but not the vendor.
func TestHistoricalPooledDepositIdentifiersHiddenFromVendor(t *testing.T) {
	for _, cur := range []string{"BTC", "XMR"} {
		t.Run(cur, func(t *testing.T) {
			p := newPayEnv(t)
			order := p.testEnv.order(p.buyer.ID, p.productID, cur, statePaid)
			addr := "legacy-" + cur + "-" + order[:12]
			txid := strings.Repeat("a", 64)
			agExec(p.testEnv, "INSERT INTO payment_addresses(order_id,currency,address,provider) VALUES($1,$2,$3,$4)", order, cur, addr, "historical-test")
			agExec(p.testEnv, "INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations,flagged) VALUES($1,$2,$3,0,$4,100000,1,true)", order, cur, txid, addr)
			for _, viewer := range []struct {
				sess   string
				hidden bool
			}{{p.buyerSess, false}, {p.vendorSess, true}, {agSession(p.testEnv, p.mod.ID), false}, {p.adminSess, false}} {
				body := p.page("/order?id="+order, viewer.sess)
				if viewer.hidden {
					mustNotContain(t, body, addr, txid, truncate(txid, 20), "payment-qr")
				} else {
					mustContain(t, body, addr, txid)
				}
			}
		})
	}
}

func TestHistoricalPooledNotesAndNotificationsRedactedForVendor(t *testing.T) {
	p := newPayEnv(t)
	agExec(p.testEnv, "UPDATE orders SET state='disputed' WHERE id=$1", p.order)
	addr, txid := "legacy-address-"+p.order[:12], strings.Repeat("b", 64)
	agExec(p.testEnv, "INSERT INTO payment_addresses(order_id,currency,address,provider) VALUES($1,'BTC',$2,'historical-test')", p.order, addr)
	agExec(p.testEnv, "INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations,flagged) VALUES($1,'BTC',$2,0,$3,40000,1,true)", p.order, txid, addr)
	note := "Legacy deposit " + addr + " transaction " + txid + " abbreviated " + truncate(txid, 20)
	agExec(p.testEnv, "INSERT INTO order_events(order_id,from_state,to_state,note) VALUES($1,'disputed','disputed',$2)", p.order, note)
	agExec(p.testEnv, "INSERT INTO disputes(id,order_id,reason,status,resolution) VALUES($1,$2,$3,'Resolved — refund to buyer',$3)", randomToken(), p.order, note)
	for _, uid := range []string{p.vendor.ID, p.buyer.ID, p.mod.ID} {
		agExec(p.testEnv, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), uid, "Order "+shortID(p.order)+": "+note)
	}
	for _, path := range []string{"/order?id=" + p.order, "/disputes", "/notifications"} {
		vendor := p.page(path, p.vendorSess)
		mustNotContain(t, vendor, addr, txid, truncate(txid, 20))
		mustContain(t, vendor, pooledAddressHidden, pooledTxidHidden)
		mustContain(t, p.page(path, p.buyerSess), addr, txid)
		mustContain(t, p.page(path, agSession(p.testEnv, p.mod.ID)), addr, txid)
	}
	if got := p.str("SELECT note FROM order_events WHERE order_id=$1 AND note=$2", p.order, note); got != note {
		t.Fatal("render redaction changed signed history")
	}
}

func TestHistoricalNotificationCandidatesNeedNoOrderPrefix(t *testing.T) {
	p := newPayEnv(t)
	addr, txid := "old-address-"+p.order[:12], strings.Repeat("c", 64)
	agExec(p.testEnv, "INSERT INTO payment_addresses(order_id,currency,address,provider) VALUES($1,'BTC',$2,'historical-test')", p.order, addr)
	agExec(p.testEnv, "INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations) VALUES($1,'BTC',$2,0,$3,10000,1)", p.order, txid, addr)
	for _, body := range []string{"Old address " + addr, "Full identifier " + txid, "Truncated identifier " + truncate(txid, 20)} {
		for _, uid := range []string{p.vendor.ID, p.buyer.ID} {
			agExec(p.testEnv, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), uid, body)
		}
	}
	vendor := p.page("/notifications", p.vendorSess)
	mustNotContain(t, vendor, addr, txid, truncate(txid, 20))
	mustContain(t, vendor, pooledAddressHidden, pooledTxidHidden)
	mustContain(t, p.page("/notifications", p.buyerSess), addr, txid, truncate(txid, 20))
}

func TestHistoricalPrivacyLeavesEscrowVerificationVisible(t *testing.T) {
	x := newEscrowEnv(t)
	addr := x.mustCheckout(x.order, regtestAddr("a84 escrow refund"))
	mustContain(t, x.page("/order?id="+x.order, x.vendorSess), addr, "Escrow descriptor", "Load it into your own wallet")
}
