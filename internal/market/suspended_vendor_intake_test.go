package market

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The administrator owns the listing and suspends its buyer. confirmedTxWith first locks the administrator's
// account row, then suspendAction locks the buyer's row. A simultaneous payment request must not wait on the
// buyer and subsequently wait on the administrator, completing the cycle in the opposite order.
func TestAdminSuspensionDoesNotDeadlockWithNewIntake(t *testing.T) {
	w := newOrderWorld(t)
	var adminID, admin string
	for n := 0; n < 64; n++ {
		adminID, admin = w.e.user("race_admin_"+randomToken()[:8], "admin")
		if w.buyerID < adminID {
			break
		}
	}
	if w.buyerID >= adminID {
		t.Fatal("could not arrange buyer-before-seller account order")
	}
	product := w.e.product(adminID, "digital")
	order := w.e.order(w.buyerID, product, "BTC", stateDraft)
	wallet := &stubProvider{currency: "BTC"}
	w.e.A.payments["BTC"] = wallet
	tx, err := w.e.DB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("SELECT 1 FROM users WHERE id=$1 FOR NO KEY UPDATE", w.buyerID); err != nil {
		t.Fatal(err)
	}
	buyerHandle := agStr(w.e, "SELECT handle FROM users WHERE id=$1", w.buyerID)
	intake := make(chan *intakeResponse, 1)
	go func() {
		r := w.e.do("POST", "/orders/pay", w.buyer, form("order_id", order))
		intake <- &intakeResponse{r.Code, r.Body.String()}
	}()
	// Give intake the first attempt on the buyer row; with the old waiting lock, it queues here.
	time.Sleep(100 * time.Millisecond)
	suspension := make(chan *intakeResponse, 1)
	go func() {
		r := w.e.do("POST", "/admin/suspend", admin, suspendForm("suspend", buyerHandle))
		suspension <- &intakeResponse{r.Code, r.Body.String()}
	}()
	actorLocked := false
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		var id string
		err := w.e.DB.QueryRow("SELECT id FROM users WHERE id=$1 FOR NO KEY UPDATE NOWAIT", adminID).Scan(&id)
		if pgCode(err) == "55P03" {
			actorLocked = true
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	var early *intakeResponse
	select {
	case early = <-intake:
	case <-time.After(200 * time.Millisecond):
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if !actorLocked {
		t.Fatal("the real admin suspension never held its actor account row")
	}
	if early == nil {
		select {
		case early = <-intake:
		case <-time.After(5 * time.Second):
			t.Fatal("payment request remained stuck after releasing buyer row")
		}
		t.Fatalf("payment waited on buyer while the admin action held its actor row: %d %s", early.code, early.body)
	}
	if early.code != 503 || !strings.Contains(early.body, errBusyMsg) {
		t.Fatalf("contended intake should return retryable busy: %d %s", early.code, early.body)
	}
	select {
	case got := <-suspension:
		if got.code != 303 {
			t.Fatalf("administrator could not suspend buyer: %d %s", got.code, got.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("administrator was blocked by payment request")
	}
	if w.state(order) != stateDraft || w.stock(product) != 5 || len(wallet.created) != 0 || w.count("SELECT count(*) FROM users WHERE id=$1 AND suspended_at IS NOT NULL", w.buyerID) != 1 {
		t.Fatal("race issued an address, held stock, or failed to suspend the buyer")
	}
}

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

func TestSuspensionInProgressReturnsBusyBeforePooledAddressRequest(t *testing.T) {
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
		r := w.e.do("POST", "/orders/pay", w.buyer, form("order_id", order))
		result <- &intakeResponse{r.Code, r.Body.String()}
	}()
	select {
	case r := <-result:
		if r.code != 503 || !strings.Contains(r.body, errBusyMsg) {
			t.Fatalf("payment during suspension: %d %s", r.code, r.body)
		}
	case <-time.After(2 * time.Second):
		tx.Rollback()
		t.Fatal("payment waited on suspension's uncommitted account lock")
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	r := w.e.do("POST", "/orders/pay", w.buyer, form("order_id", order))
	if r.Code != 409 || !strings.Contains(r.Body.String(), "Unavailable. "+sellerSuspended) {
		t.Fatalf("payment after suspension: %d %s", r.Code, r.Body.String())
	}
	if w.state(order) != stateDraft || len(wallet.created) != 0 || w.stock(product) != 5 {
		t.Fatal("suspension race issued a wallet address or held stock")
	}
}

type intakeResponse struct {
	code int
	body string
}
