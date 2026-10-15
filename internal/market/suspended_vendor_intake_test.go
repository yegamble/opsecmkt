package market

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestSuspendedVendorRefusesMoneroEscrowCheckout(t *testing.T) {
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

type intakeResponse struct {
	code int
	body string
}

// A confirmed administrator action locks its actor's account row and then its target's (routes.go, suspendAction); an
// escrow checkout locks the buyer's and the seller's rows in id order (lockNewIntake). When the administrator is one
// party and suspends the other, the two take the same rows in opposite orders: a checkout that waited for its first row
// while the suspension held the administrator's would, once that row came free, wait for the administrator's row while
// the suspension waited for it. Both account locks therefore use NOWAIT: the checkout answers the retryable busy 503
// at once, before asking the escrow node anything, and the suspension completes. An outside transaction holds the
// checkout's first row, as the old pooled test (TestAdminSuspensionDoesNotDeadlockWithNewIntake) did, so the checkout
// meets it before the administrator's. Leg "seller": the administrator sells and suspends the buyer (buyer row first);
// leg "buyer": the administrator buys and suspends the vendor (vendor row first). Fake escrow Core: simulated RPC.

func TestAdminSuspensionDoesNotDeadlockWithEscrowCheckout(t *testing.T) {
	for _, leg := range []string{"seller", "buyer"} {
		t.Run(leg, func(t *testing.T) {
			x := newXMREscrowEnv(t)
			x.mustPoll()
			// The checkout's first row is the administrator's counterparty (the buyer the administrator sells to, or the
			// vendor the administrator buys from): accounts are made in pairs until its id sorts before the administrator's.
			role := map[string]string{"seller": "buyer", "buyer": "vendor"}[leg]
			var first, firstSess, adminID, admin string
			for range 64 {
				first, firstSess = x.user("erace_"+role+"_"+randomToken()[:8], role)
				if adminID, admin = x.user("erace_admin_"+randomToken()[:8], "admin"); first < adminID {
					break
				}
			}
			if first >= adminID {
				t.Fatal("could not arrange the counterparty's account before the administrator's")
			}

			order, checkoutSess := x.testEnv.order(first, x.digital(adminID), "XMR", stateDraft), firstSess
			if leg == "buyer" {
				order, checkoutSess = x.testEnv.order(adminID, x.digital(first), "XMR", stateDraft), admin
			}
			target := agStr(x.testEnv, "SELECT handle FROM users WHERE id=$1", first)
			calls := x.fake.count()
			tx, err := x.DB.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := tx.Exec("SELECT 1 FROM users WHERE id=$1 FOR NO KEY UPDATE", first); err != nil {
				t.Fatal(err)
			}
			intake := make(chan intakeResponse, 1)
			go func() {
				w := x.do("POST", "/orders/escrow-xmr", checkoutSess, url.Values{"order_id": {order}, "refund_address": {testXMRAddr("erace-refund-"+leg, true)}})
				intake <- intakeResponse{w.Code, w.Body.String()}
			}()
			// Give the checkout the first attempt at the held row; a waiting lock would queue it there.
			time.Sleep(100 * time.Millisecond)
			suspension := make(chan intakeResponse, 1)
			go func() {
				w := x.do("POST", "/admin/suspend", admin, suspendForm("suspend", target))
				suspension <- intakeResponse{w.Code, w.Body.String()}
			}()
			actorLocked := false
			for deadline := time.Now().Add(4 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
				var id string
				err := x.DB.QueryRow("SELECT id FROM users WHERE id=$1 FOR NO KEY UPDATE NOWAIT", adminID).Scan(&id)
				if pgCode(err) == "55P03" {
					actorLocked = true
					break
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			var early *intakeResponse
			select {
			case r := <-intake:
				early = &r
			case <-time.After(200 * time.Millisecond):
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			if !actorLocked {
				t.Fatal("the administrator's suspension never held its own account row")
			}
			if early == nil {
				select {
				case r := <-intake:
					t.Fatalf("the checkout waited for %s's row while the suspension held the administrator's: %d %s", target, r.code, r.body)
				case <-time.After(15 * time.Second):
					t.Fatal("the checkout stayed stuck after the held row was released")
				}
			}
			if early.code != 503 || !strings.Contains(early.body, errBusyMsg) {
				t.Fatalf("contended checkout: %d %s, want the retryable busy 503", early.code, early.body)
			}
			select {
			case got := <-suspension:
				if got.code != 303 {
					t.Fatalf("the administrator could not suspend %s: %d %s", target, got.code, got.body)
				}
			case <-time.After(15 * time.Second):
				t.Fatal("the checkout blocked the administrator's suspension")
			}
			if x.state(order) != stateDraft || agInt(x.testEnv, "SELECT count(*) FROM escrows WHERE order_id=$1", order) != 0 {
				t.Fatal("refused Monero checkout wrote an escrow")
			}
			if agInt(x.testEnv, "SELECT count(*) FROM users WHERE id=$1 AND suspended_at IS NOT NULL", first) != 1 {
				t.Fatal("the suspension did not take effect")
			}
			if got := x.fake.count(); got != calls {
				t.Fatalf("refused Monero checkout made %d wallet calls", got-calls)
			}
		})
	}
}
