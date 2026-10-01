package market

import (
	"net/url"
	"strings"
	"testing"
)

// A-288: a wallet restored from an older backup hands out an address the database already issued to another
// order. The buyer gets a specific refusal (not the generic 500), the order stays a draft with no address row, the
// operator log names only the currency, and /admin shows a per-currency warning pointing at the runbook until an
// administrator dismisses it.
func TestPaymentAddressCollisionRefusedAndReported(t *testing.T) {
	for _, cur := range []string{"BTC", "XMR"} {
		t.Run(cur, func(t *testing.T) {
			p := newPayEnv(t)
			wallet := newFakeProvider(cur, 3)
			p.A.payments[cur] = wallet
			logs := captureLog(t)
			draft := p.testEnv.order(p.buyer.ID, p.productID, cur, stateDraft)
			// The fake wallet derives the address from the order id: give that address to an existing order, as a
			// database that issued it before the wallet was restored would have.
			reissued := "fake-testnet-" + draft[:8]
			original := p.addr
			if _, err := p.DB.Exec("UPDATE payment_addresses SET address=$1 WHERE order_id=$2", reissued, p.order); err != nil {
				t.Fatal(err)
			}
			stock := p.count("SELECT stock FROM products WHERE id=$1", p.productID)
			for attempt := 1; attempt <= 2; attempt++ {
				w := p.do("POST", "/orders/pay", p.buyerSess, url.Values{"order_id": {draft}})
				body := w.Body.String()
				if w.Code != 503 || !strings.Contains(body, cur+" payment addresses are being restored on this market. Nothing was charged and your order is still a draft; try again later.") ||
					strings.Contains(body, "Unable to save changes") || w.Header().Get("Retry-After") == "" {
					t.Fatalf("attempt %d: status %d body %q Retry-After %q", attempt, w.Code, body, w.Header().Get("Retry-After"))
				}
				if p.state(draft) != stateDraft || p.count("SELECT count(*) FROM payment_addresses WHERE order_id=$1", draft) != 0 ||
					p.count("SELECT stock FROM products WHERE id=$1", p.productID) != stock || p.count("SELECT count(*) FROM order_events WHERE order_id=$1", draft) != 0 {
					t.Fatalf("attempt %d: a refused payment request changed the order, its address, events or stock", attempt)
				}
			}
			if got := logs.lines("payments: " + cur + " wallet returned a payment address already issued"); len(got) != 2 || !strings.Contains(got[1], "collisions=2") || !strings.Contains(got[1], "Restore the wallets") {
				t.Fatalf("collision log lines: %q", got)
			}
			if got := logs.lines("http 5xx kind=action name=/orders/pay status=503"); len(got) != 2 || !strings.HasSuffix(got[0], " cause=payment-address-already-issued") {
				t.Fatalf("http 5xx lines: %q", got)
			}
			for _, raw := range []string{reissued, "payment_addresses_address_key", "duplicate key", "23505", draft[:8]} {
				if strings.Contains(logs.String(), raw) {
					t.Fatalf("log names %q:\n%s", raw, logs.String())
				}
			}
			warning := cur + " wallet handed out payment addresses this market had already issued: 2 payment requests refused, last at "
			admin := p.page("/admin", p.adminSess)
			if !strings.Contains(admin, warning) || !strings.Contains(admin, "Restore the wallets") || !strings.Contains(admin, "docs/testnet-runbook.md#restore-the-wallets") ||
				!strings.Contains(admin, "Dismiss the "+cur+" warning") {
				t.Fatal("/admin does not show the issued-address warning with the runbook section")
			}
			// The operator advances the wallet: the next request succeeds, and the warning stays until dismissed.
			if _, err := p.DB.Exec("UPDATE payment_addresses SET address=$1 WHERE order_id=$2", original, p.order); err != nil {
				t.Fatal(err)
			}
			p.check(p.do("POST", "/orders/pay", p.buyerSess, url.Values{"order_id": {draft}}), 303)
			if p.state(draft) != stateAwaitingPayment || p.str("SELECT address FROM payment_addresses WHERE order_id=$1", draft) != reissued {
				t.Fatal("payment request after the wallet was advanced did not issue the address")
			}
			if !strings.Contains(p.page("/admin", p.adminSess), warning) {
				t.Fatal("the warning cleared without an administrator dismissing it")
			}
			dismiss := func(session, seen string) int {
				return p.do("POST", "/admin/payment-address-collisions", session, url.Values{"currency": {cur}, "seen": {seen}}).Code
			}
			if dismiss(p.buyerSess, "2") != 403 || dismiss(p.adminSess, "1") != 409 || dismiss(p.adminSess, "x") != 400 {
				t.Fatal("dismissal did not require an administrator and the warning as shown")
			}
			if !strings.Contains(p.page("/admin", p.adminSess), warning) {
				t.Fatal("a refused dismissal cleared the warning")
			}
			if dismiss(p.adminSess, "2") != 303 {
				t.Fatal("administrator could not dismiss the warning")
			}
			if strings.Contains(p.page("/admin", p.adminSess), cur+" wallet handed out payment addresses") {
				t.Fatal("the warning is still shown after it was dismissed")
			}
			if p.count("SELECT count(*) FROM audit_events WHERE action=$1", "Dismissed the "+cur+" issued-address warning (2 payment requests refused)") != 1 {
				t.Fatal("dismissal was not audited once")
			}
			// A-303: with the count back at 0 there is no warning to dismiss; "0" is refused, not a second dismissal.
			if code := dismiss(p.adminSess, "0"); code != 400 {
				t.Fatalf("dismissing a count of 0 after the dismissal: %d, want 400", code)
			}
			// Any other failure saving the address is still a server error, and is not counted as a collision.
			other := p.testEnv.order(p.buyer.ID, p.productID, cur, stateDraft)
			if _, err := p.DB.Exec("INSERT INTO payment_addresses(order_id,currency,address,provider) VALUES($1,$2,$3,'fake')", other, cur, "held-"+other[:8]); err != nil {
				t.Fatal(err)
			}
			w := p.do("POST", "/orders/pay", p.buyerSess, url.Values{"order_id": {other}})
			if w.Code != 500 || !strings.Contains(w.Body.String(), "Unable to save changes") {
				t.Fatalf("another unique violation: status %d body %q", w.Code, w.Body.String())
			}
			if p.count("SELECT address_collisions FROM payment_status WHERE currency=$1", cur) != 0 {
				t.Fatal("another unique violation was counted as an issued-address collision")
			}
		})
	}
}
