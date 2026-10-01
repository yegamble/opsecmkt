package market

import (
	"context"
	"html"
	"net/url"
	"strings"
	"testing"
)

// A-331 (remainder): every escrow note, notice, status, validation error and audit text names the escrow node's chain
// (escrowNetwork, read at startup), never a fixed "regtest". Phase 1 runs on regtest only, so the chain is set to
// signet here by hand; a representative text from each file is checked. The template's "Regtest only" headline and
// regtestAddressScript stay as they are while escrow is regtest-only. DB integration with the simulated Core.
func TestEscrowTextsNameTheNodesChain(t *testing.T) {
	x := newEscrowEnv(t)
	x.A.escrow.Load().network = "signet"
	_, admin := x.user("netadmin", "admin")
	has := func(where, text string, want ...string) {
		t.Helper()
		text = html.UnescapeString(text)
		for _, w := range want {
			if !strings.Contains(text, w) {
				t.Errorf("%s lacks %q:\n%s", where, w, text)
			}
		}
		if strings.Contains(text, "regtest BTC") || strings.Contains(text, "regtest network") || strings.Contains(text, "(regtest)") ||
			strings.Contains(text, "regtest Bitcoin address") || strings.Contains(text, "regtest escrow funded") {
			t.Errorf("%s names regtest:\n%s", where, text)
		}
	}
	// onPage checks a whole page, which also carries the template's fixed "Regtest only" copy.
	onPage := func(where, page string, want ...string) {
		t.Helper()
		page = html.UnescapeString(page)
		for _, w := range want {
			if !strings.Contains(page, w) {
				t.Errorf("%s lacks %q:\n%s", where, w, page)
			}
		}
	}
	audit := func(like string) string {
		return strings.Join(agStrings(t, x.testEnv, "SELECT action FROM audit_events WHERE action LIKE $1", like), "\n")
	}

	// escrow_create.go: the refund address error and the audit suffix.
	w := x.do("POST", "/orders/escrow", x.buyerSess, url.Values{"order_id": {x.order}, "refund_address": {""}})
	has("escrow checkout without a refund address", w.Body.String(), "a fresh signet Bitcoin address")
	order, id := x.built(regtestAddr("refund-net"), regtestAddr("claim-net"), 100000)
	has("checkout audit", audit("Created Bitcoin escrow%"+shortID(order)+"%"), "Created Bitcoin escrow (signet) for order")
	// escrow_watch.go: the funded note.
	has("funded note", strings.Join(x.events(order), "\n"), "TESTNET signet escrow funded")
	// escrow_settle.go: the claim audit, a claim validation error, the waiting and broadcast statuses, the sign audit.
	has("claim audit", audit("Requested the escrow claim of order "+shortID(order)+"%"), "(signet)")
	other := x.completed(regtestAddr("refund-net2"), 100000)
	if code, body := x.claim(other, x.vendorSess, ""); code != 400 {
		t.Fatalf("claim without an address: %d", code)
	} else {
		has("claim without an address", body, "a fresh signet Bitcoin address")
	}
	onPage("waiting status", x.page("/order?id="+order, x.buyerSess), "broadcasts it to the signet network")
	x.signAs(order, roleVendor, id)
	has("sign audit", audit("Signed the escrow settlement of order "+shortID(order)+"%"), "(signet)")
	x.poll()
	// escrow_cosign.go: the broadcast and confirmed notes.
	has("broadcast note", strings.Join(x.events(order), "\n"), "was broadcast to the signet network")
	onPage("broadcast status", x.page("/order?id="+order, x.vendorSess), "Broadcast to the signet network")
	x.core.setTxConfs(x.settleByID(id).TxID, 3, "")
	x.poll()
	has("confirmed note", strings.Join(x.events(order), "\n"), "is confirmed on the signet network")
	onPage("confirmed status", x.page("/order?id="+order, x.vendorSess), "Confirmed on the signet network")
	// escrow_stray.go: the recover audit.
	if code, body := x.recover(order, x.buyerSess, testPassword); code != 303 {
		t.Fatalf("recover: %d %s", code, body)
	}
	has("recover audit", audit("Asked to recover coins at the escrow of order "+shortID(order)+"%"), "(signet)")

	// escrow_order.go: the cancel form's refund statement; escrow_watch.go: the window-end notice.
	under := x.draft()
	addr := x.mustCheckout(under, regtestAddr("refund-net3"))
	x.core.deposit(addr, txidOf("net-under"), 0, 40000, 5)
	x.poll()
	onPage("cancel form", x.page("/order?id="+under, x.buyerSess), "0.0004 signet BTC in this escrow")
	x.DB.Exec("UPDATE escrows SET created=now()-interval '25 hours' WHERE order_id=$1", under)
	x.DB.Exec("UPDATE payments SET created=now()-interval '25 hours' WHERE order_id=$1", under)
	x.poll()
	has("window-end notice", strings.Join(agStrings(t, x.testEnv, "SELECT body FROM notifications WHERE user_id=$1 AND body LIKE '%payment window ended%'", x.buyer), "\n"),
		"(TESTNET signet)", "broadcasts it to the signet network")

	// escrow_decision.go: the amount in the resolve form's statements.
	disputed, _ := x.disputedEscrow(regtestAddr("refund-net4"))
	o, err := scanOrder(x.DB.QueryRow(orderQuery+" WHERE o.id=$1", disputed))
	if err != nil {
		t.Fatal(err)
	}
	p, err := x.A.escrowDisputePreview(context.Background(), &o)
	if err != nil {
		t.Fatal(err)
	}
	has("resolve form", p.Release+" "+p.Refund, "0.001 signet BTC")

	// escrow_fee.go: the fee address error.
	w = x.do("POST", "/admin/escrow-fee", admin, url.Values{"fee_bps": {"100"}, "fee_address": {""}, "password": {testPassword}})
	has("fee without an address", w.Body.String(), "the signet Bitcoin address the market fee is paid to")
}
