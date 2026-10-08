package market

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestSuspendedVendorRefusesNewDraftAndPooledPayment(t *testing.T) {
	w := newOrderWorld(t)
	_, admin := w.e.user("suspended_intake_admin", "admin")
	product := w.e.product(w.vendorID, "digital")
	stale := map[string]string{}
	awaiting := map[string]string{}
	wallets := map[string]*stubProvider{}
	for _, coin := range []string{"BTC", "XMR"} {
		wallets[coin] = &stubProvider{currency: coin}
		w.e.A.payments[coin] = wallets[coin]
		stale[coin] = w.e.order(w.buyerID, product, coin, stateDraft)
		existingProduct := w.e.product(w.vendorID, "digital")
		awaiting[coin] = w.e.order(w.buyerID, existingProduct, coin, stateDraft)
		w.expect("/orders/pay", w.buyer, form("order_id", awaiting[coin]), 303, "")
	}
	paid := w.e.order(w.buyerID, w.e.product(w.vendorID, "physical"), "BTC", stateShipped)
	if r := w.e.do("POST", "/admin/suspend", admin, suspendForm("suspend", agStr(w.e, "SELECT handle FROM users WHERE id=$1", w.vendorID))); r.Code != 303 {
		t.Fatalf("suspend: %d %s", r.Code, r.Body.String())
	}
	if w.count("SELECT count(*) FROM products WHERE vendor_id=$1 AND NOT archived", w.vendorID) != 4 {
		t.Fatal("suspension archived listings")
	}
	for _, path := range []string{"/product?id=" + product, "/checkout?id=" + product} {
		body := w.page(path, w.buyer, 200)
		if !strings.Contains(body, "Unavailable.") || !strings.Contains(body, sellerSuspended) || strings.Contains(body, `action="/orders"`) || strings.Contains(body, `href="/checkout?id=`) {
			t.Fatalf("%s offers a new draft: %s", path, body)
		}
	}
	for _, coin := range []string{"BTC", "XMR"} {
		beforeWallet := len(wallets[coin].created)
		beforeStock := w.stock(product)
		newDraft := w.post("/orders", w.other, form("product_id", product, "currency", coin))
		if newDraft.Code != 409 || !strings.Contains(newDraft.Body.String(), "Unavailable. "+sellerSuspended) {
			t.Fatalf("new %s draft: %d %s", coin, newDraft.Code, newDraft.Body.String())
		}
		body := w.page("/order?id="+stale[coin], w.buyer, 200)
		if !strings.Contains(body, "<strong>Unavailable.</strong> "+sellerSuspended) || strings.Contains(body, `action="/orders/pay"`) || strings.Contains(body, `action="/orders/escrow"`) || strings.Contains(body, `action="/orders/escrow-xmr"`) {
			t.Fatalf("stale %s draft offers payment: %s", coin, body)
		}
		pay := w.post("/orders/pay", w.buyer, form("order_id", stale[coin]))
		if pay.Code != 409 || !strings.Contains(pay.Body.String(), "Unavailable. "+sellerSuspended) {
			t.Fatalf("stale %s payment: %d %s", coin, pay.Code, pay.Body.String())
		}
		if w.state(stale[coin]) != stateDraft || w.stock(product) != beforeStock || len(wallets[coin].created) != beforeWallet || w.count("SELECT count(*) FROM payment_addresses WHERE order_id=$1", stale[coin]) != 0 {
			t.Fatal("suspended refusal reserved stock or requested a wallet address")
		}
		w.expect("/orders/cancel", w.buyer, form("order_id", awaiting[coin], "from", stateAwaitingPayment), 303, "")
	}
	w.expect("/orders/complete", w.buyer, payoutConfirmed(form("order_id", paid), 0), 303, "")
	if w.state(paid) != stateCompleted {
		t.Fatal("existing paid order stopped after suspension")
	}
	if r := w.e.do("POST", "/admin/suspend", admin, suspendForm("restore", agStr(w.e, "SELECT handle FROM users WHERE id=$1", w.vendorID))); r.Code != 303 {
		t.Fatalf("restore: %d %s", r.Code, r.Body.String())
	}
	if body := w.page("/checkout?id="+product, w.buyer, 200); !strings.Contains(body, `action="/orders"`) || strings.Contains(body, sellerSuspended) {
		t.Fatal("restoring the vendor did not reopen new drafts")
	}
	w.expect("/orders", w.other, form("product_id", product, "currency", "BTC"), 303, "")
	for _, coin := range []string{"BTC", "XMR"} {
		w.expect("/orders/pay", w.buyer, form("order_id", stale[coin]), 303, "")
	}
}

func TestSuspendedVendorRefusesBothEscrowCheckouts(t *testing.T) {
	t.Run("BTC", func(t *testing.T) {
		x := newEscrowEnv(t)
		order := x.draft()
		agExec(x.testEnv, "UPDATE users SET suspended_at=now() WHERE id=$1", x.vendor)
		page := x.page("/order?id="+order, x.buyerSess)
		if !strings.Contains(page, "<strong>Unavailable.</strong> "+sellerSuspended) || strings.Contains(page, `action="/orders/escrow"`) {
			t.Fatal("BTC draft page offered escrow after suspension")
		}
		before := len(x.core.calls)
		w := x.checkout(order, regtestAddr("suspended refund"))
		if w.Code != 409 || !strings.Contains(w.Body.String(), "Unavailable. "+sellerSuspended) {
			t.Fatalf("BTC checkout: %d %s", w.Code, w.Body.String())
		}
		x.nothingWritten(order)
		if len(x.core.calls) != before {
			t.Fatal("BTC escrow called Core after suspension")
		}
	})
	t.Run("XMR", func(t *testing.T) {
		x := newXMREscrowEnv(t)
		x.mustPoll()
		order := x.draft(x.digital(x.vendor))
		agExec(x.testEnv, "UPDATE users SET suspended_at=now() WHERE id=$1", x.vendor)
		page := x.page("/order?id="+order, x.buyerSess)
		if !strings.Contains(page, "<strong>Unavailable.</strong> "+sellerSuspended) || strings.Contains(page, `action="/orders/escrow-xmr"`) {
			t.Fatal("XMR draft page offered escrow after suspension")
		}
		before := x.fake.count()
		w := x.checkout(x.buyerSess, order, testXMRAddr("suspended refund", true))
		if w.Code != 409 || !strings.Contains(w.Body.String(), "Unavailable. "+sellerSuspended) {
			t.Fatalf("XMR checkout: %d %s", w.Code, w.Body.String())
		}
		if x.state(order) != stateDraft || agInt(x.testEnv, "SELECT count(*) FROM escrows WHERE order_id=$1", order) != 0 || agInt(x.testEnv, "SELECT count(*) FROM xmr_escrow_slots WHERE state='claimed'") != 0 || x.fake.count() != before {
			t.Fatal("XMR escrow claimed a slot or made RPC after suspension")
		}
	})
}

func TestSuspensionCommitsBeforePooledAddressRequest(t *testing.T) {
	w := newOrderWorld(t)
	product := w.e.product(w.vendorID, "digital")
	order := w.e.order(w.buyerID, product, "BTC", stateDraft)
	wallet := &stubProvider{currency: "BTC"}
	w.e.A.payments["BTC"] = wallet
	tx, err := w.e.DB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec("UPDATE users SET suspended_at=now() WHERE id=$1", w.vendorID); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	result := make(chan *intakeResponse, 1)
	go func() {
		r := w.e.do("POST", "/orders/pay", w.buyer, url.Values{"order_id": {order}})
		result <- &intakeResponse{r.Code, r.Body.String()}
	}()
	select {
	case r := <-result:
		tx.Rollback()
		t.Fatalf("payment passed suspension's uncommitted account lock: %d %s", r.code, r.body)
	case <-time.After(150 * time.Millisecond):
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-result:
		if r.code != 409 || !strings.Contains(r.body, "Unavailable. "+sellerSuspended) {
			t.Fatalf("payment after suspension: %d %s", r.code, r.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("payment did not resume after suspension committed")
	}
	if w.state(order) != stateDraft || len(wallet.created) != 0 || w.stock(product) != 5 {
		t.Fatal("suspension race issued a wallet address or held stock")
	}
}

type intakeResponse struct {
	code int
	body string
}
