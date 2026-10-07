package market

import (
	"slices"
	"strings"
	"testing"
)

// QA5W3B-5 (A-481 root cause): a form field that is not valid UTF-8 or holds a NUL byte is refused with 400 where
// ServeHTTP parses the form, before any handler runs. On a UTF8 database such text made the insert fail with a 500
// "Unable to save changes", and a confirmed action had accepted the password first. DB integration.

const invalidTextMsg = "not valid UTF-8 or contains a NUL character"

// The buyer's review (orders_actions.go reviewAction) of a completed order: neither value reaches the handler.
func TestFormRefusesInvalidTextReview(t *testing.T) {
	w := newOrderWorld(t)
	done := w.e.order(w.buyerID, w.e.product(w.vendorID, "physical"), "BTC", stateCompleted)
	for name, body := range map[string]string{"0xff": "Great \xff\xfe seller", "NUL": "Great \x00 seller"} {
		w.expect("/reviews", w.buyer, form("order_id", done, "rating", "5", "body", body), 400, invalidTextMsg)
		if n := w.count("SELECT count(*) FROM reviews WHERE order_id=$1", done); n != 0 {
			t.Fatalf("%s: %d reviews written", name, n)
		}
	}
	// Valid text in the same form is saved, so nothing else refused it.
	w.expect("/reviews", w.buyer, form("order_id", done, "rating", "5", "body", "Great seller, é and 東京."), 303, "")
}

// The A-480 disposition with the right password: refused with 400, the record stays open, no audit row, and none of
// the moderator's confirmation budget is spent.
func TestFormRefusesInvalidTextCollisionDisposition(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	modID, mod := x.user("encmod_"+randomToken()[:6], "moderator")
	order := x.draft(x.digital(x.vendor))
	addr := x.mustCheckout(order)
	tx := hex64("invalid text " + order)
	x.fake.payAddress(addr, tx, xmrListingPrice, 10)
	x.refusePass(order)
	id := agStrings(t, x.testEnv, "SELECT id::text FROM xmr_escrow_collisions WHERE order_id=$1", order)[0]
	budget := func() int { x.A.mu.Lock(); defer x.A.mu.Unlock(); return x.A.limits["confirm:"+modID].Count }
	for name, note := range map[string]string{"0xff": "Refunded by hand \xff\xfe in the wallet.", "NUL": "Refunded by hand \x00 in the wallet."} {
		if code, body := x.recordCollision(mod, id, note); code != 400 || !strings.Contains(body, invalidTextMsg) {
			t.Fatalf("%s note: %d %s", name, code, body)
		}
	}
	if got := x.collisions(order); !slices.Equal(got, []string{tx + " open"}) || x.collisionAudits(modID) != 0 || budget() != 0 {
		t.Fatalf("refused notes changed something: %q, %d audit rows, budget %d", got, x.collisionAudits(modID), budget())
	}
	if code, loc := x.recordCollision(mod, id, "Refunded by hand in the wallet."); code != 303 || loc != "/moderator" {
		t.Fatalf("a valid note: %d %s", code, loc)
	}
}

// A staff member who is a party to the order is refused before the password check, so twelve wrong passwords give
// twelve 403s and spend none of the confirmation budget (QA5W3B-7: without the check before confirmedTx they gave
// ten 401s and then 429).
func TestXMREscrowCollisionStaffPartyBudget(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order := x.draft(x.digital(x.vendor))
	addr := x.mustCheckout(order)
	x.fake.payAddress(addr, hex64("staff party budget "+order), xmrListingPrice, 10)
	x.refusePass(order)
	id := agStrings(t, x.testEnv, "SELECT id::text FROM xmr_escrow_collisions WHERE order_id=$1", order)[0]
	agExec(x.testEnv, "UPDATE users SET role='moderator' WHERE id=$1", x.vendor)
	var codes []int
	for i := 0; i < 12; i++ {
		c, _ := x.recordCollision(x.vendorSess, id, "Staff party tries to record it.", "password", "wrong-password-123")
		codes = append(codes, c)
	}
	x.A.mu.Lock()
	spent := x.A.limits["confirm:"+x.vendor].Count
	x.A.mu.Unlock()
	if !slices.Equal(codes, slices.Repeat([]int{403}, 12)) || spent != 0 {
		t.Fatalf("a staff party with a wrong password twelve times: %v, budget spent %d", codes, spent)
	}
	if c, body := x.recordCollision(x.vendorSess, id, "Staff party tries to record it."); c != 403 || !strings.Contains(body, "party") {
		t.Fatalf("then the right password: %d %s", c, body)
	}
}
