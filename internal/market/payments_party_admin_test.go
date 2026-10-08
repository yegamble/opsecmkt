package market

import (
	"html"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// A-219: only a sole administrator can review a large or suspension hold on their own order.
func a219HeldPayout(t *testing.T, p *payEnv, kind, hold string) (order, id string) {
	t.Helper()
	state, recipient, address := stateCompleted, p.buyer.ID, "fake-testnet-buyer"
	switch kind {
	case "return":
		// Migration 073 requires the vendor's release beside every buyer return.
		order = p.insertPayout(stateCompleted, "release", p.vendor.ID, "pending", "fake-testnet-vendor", "", false, "")
		if _, err := p.DB.Exec(`INSERT INTO payouts(order_id,kind,user_id,currency,amount,address,state,error)
			VALUES($1,'return',$2,'BTC',250000,$3,'held',$4)`, order, p.buyer.ID, address, hold); err != nil {
			t.Fatal(err)
		}
		id = p.str("SELECT id::text FROM payouts WHERE order_id=$1 AND kind='return'", order)
		return
	case "refund":
		state = stateCancelled
	case "release":
		recipient, address = p.vendor.ID, "fake-testnet-vendor"
	}
	order = p.insertPayout(state, kind, recipient, "held", address, hold, false, "")
	id = p.str("SELECT id::text FROM payouts WHERE order_id=$1", order)
	return
}

func a219Release(id string) url.Values {
	return url.Values{"payout_id": {id}, "op": {"release"}, "password": {testPassword},
		"large_reviewed": {"confirmed"}, "address_checked": {"confirmed"}}
}

func TestPartyAdminCannotReviewOwnOrderHold(t *testing.T) {
	for _, tc := range []struct {
		name, actor, kind, hold string
	}{
		{"buyer large return", "buyer", "return", largeHold},
		{"vendor large return", "vendor", "return", largeHold},
		{"buyer suspension release", "buyer", "release", suspendedHold},
		{"vendor suspension refund", "vendor", "refund", suspendedHold},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPayEnv(t)
			party, session := p.buyer, p.buyerSess
			if tc.actor == "vendor" {
				party, session = p.vendor, p.vendorSess
			}
			if _, err := p.DB.Exec("UPDATE users SET role='admin' WHERE id=$1", party.ID); err != nil {
				t.Fatal(err)
			}
			order, id := a219HeldPayout(t, p, tc.kind, tc.hold)
			w := p.do("POST", "/admin/payout", session, a219Release(id))
			if w.Code != 409 || !strings.Contains(w.Body.String(), "Another administrator is available") {
				t.Fatalf("party review: status=%d body=%s", w.Code, w.Body.String())
			}
			if state := p.str("SELECT state FROM payouts WHERE id=$1::bigint", id); state != "held" ||
				p.count("SELECT count(*) FROM order_events WHERE order_id=$1", order) != 0 ||
				p.count("SELECT count(*) FROM audit_events WHERE action LIKE $1", "Released held payout "+id+"%") != 0 {
				t.Fatal("refused party review changed payout or wrote history")
			}
			p.check(p.do("POST", "/admin/payout", p.adminSess, a219Release(id)), 303)
			if state := p.str("SELECT state FROM payouts WHERE id=$1::bigint", id); state != "pending" ||
				p.count("SELECT count(*) FROM order_events WHERE order_id=$1", order) != 1 {
				t.Fatal("independent administrator did not release exactly once")
			}
			if p.count("SELECT count(*) FROM audit_events WHERE action LIKE '%released by a party to the order%'") != 0 {
				t.Fatal("independent review was labelled as a party release")
			}
		})
	}
}

func TestSolePartyAdminCanReleaseHeldPayout(t *testing.T) {
	for _, tc := range []struct {
		name, actor, hold string
		suspendOther      bool
	}{
		{"sole buyer large", "buyer", largeHold, false},
		{"sole vendor large", "vendor", largeHold, false},
		{"other suspended buyer large", "buyer", largeHold, true},
		{"sole buyer suspension", "buyer", suspendedHold, false},
		{"other suspended vendor suspension", "vendor", suspendedHold, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPayEnv(t)
			party, session := p.buyer, p.buyerSess
			if tc.actor == "vendor" {
				party, session = p.vendor, p.vendorSess
			}
			if _, err := p.DB.Exec("UPDATE users SET role='admin' WHERE id=$1", party.ID); err != nil {
				t.Fatal(err)
			}
			otherState := "role='buyer'"
			if tc.suspendOther {
				otherState = "suspended_at=now()"
			}
			if _, err := p.DB.Exec("UPDATE users SET "+otherState+" WHERE id=(SELECT id FROM users WHERE role='admin' AND id<>$1 LIMIT 1)", party.ID); err != nil {
				t.Fatal(err)
			}
			_, id := a219HeldPayout(t, p, "return", tc.hold)
			p.check(p.do("POST", "/admin/payout", session, a219Release(id)), 303)
			if state := p.str("SELECT state FROM payouts WHERE id=$1::bigint", id); state != "pending" {
				t.Fatalf("party release left payout %s", state)
			}
			if n := p.count("SELECT count(*) FROM audit_events WHERE action LIKE $1", "Released held payout "+id+"%released by a party to the order (confirmed with password)"); n != 1 {
				t.Fatalf("party audit suffix count=%d", n)
			}
		})
	}
}

func TestPartyAdminHoldReviewConcurrent(t *testing.T) {
	p := newPayEnv(t)
	if _, err := p.DB.Exec("UPDATE users SET role='admin' WHERE id=$1", p.buyer.ID); err != nil {
		t.Fatal(err)
	}
	order, id := a219HeldPayout(t, p, "return", largeHold)
	start := make(chan struct{})
	results := make(chan struct {
		party bool
		code  int
	}, 2)
	var wg sync.WaitGroup
	for _, actor := range []struct {
		party bool
		sess  string
	}{{true, p.buyerSess}, {false, p.adminSess}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- struct {
				party bool
				code  int
			}{actor.party, p.do("POST", "/admin/payout", actor.sess, a219Release(id)).Code}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	for got := range results {
		want := 303
		if got.party {
			want = 409
		}
		if got.code != want {
			t.Fatalf("party=%v status=%d want=%d", got.party, got.code, want)
		}
	}
	if p.str("SELECT state FROM payouts WHERE id=$1::bigint", id) != "pending" ||
		p.count("SELECT count(*) FROM order_events WHERE order_id=$1", order) != 1 {
		t.Fatal("concurrent reviews changed the payout more than once")
	}
}

func TestLargeHoldNoticesSkipPartyAdminsAndHelpRecipient(t *testing.T) {
	p := newPayEnv(t)
	for _, id := range []string{p.buyer.ID, p.vendor.ID} {
		if _, err := p.DB.Exec("UPDATE users SET role='admin' WHERE id=$1", id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.DB.Exec("INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value", settingStaffContact, "help-admin"); err != nil {
		t.Fatal(err)
	}
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	p.setPayoutAddress(p.buyer, "fake-testnet-buyer")
	p.paidByWatcherAmount(350000)
	p.move(p.order, statePaid, stateShipped, p.vendor)
	p.move(p.order, stateShipped, stateCompleted, p.buyer)
	independent := p.str("SELECT id FROM users WHERE role='admin' AND id<>$1 AND id<>$2", p.buyer.ID, p.vendor.ID)
	for _, tc := range []struct {
		id   string
		want int
	}{{p.buyer.ID, 0}, {p.vendor.ID, 0}, {independent, 1}} {
		if got := p.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE 'Payout review needed for order%'", tc.id); got != tc.want {
			t.Fatalf("review notice for %s: %d want %d", tc.id, got, tc.want)
		}
	}
	if p.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%Nothing is needed from you; if it is still held after a few days%'", p.buyer.ID) != 1 {
		t.Fatal("recipient did not get the held payout help")
	}
	for _, page := range []string{p.page("/account", p.buyerSess), p.page("/order?id="+p.order, p.buyerSess)} {
		body := html.UnescapeString(page)
		mustContain(t, body, "Nothing is needed from you; if it is still held after a few days, message an administrator", "Contact the market staff: help-admin")
	}
	mustNotContain(t, html.UnescapeString(p.page("/order?id="+p.order, p.vendorSess)), "Nothing is needed from you; if it is still held after a few days")
}

func TestLargeHoldNoticeReachesSolePartyAdmin(t *testing.T) {
	p := newPayEnv(t)
	if _, err := p.DB.Exec("UPDATE users SET role='admin' WHERE id=$1", p.buyer.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := p.DB.Exec("UPDATE users SET suspended_at=now() WHERE role='admin' AND id<>$1", p.buyer.ID); err != nil {
		t.Fatal(err)
	}
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	p.setPayoutAddress(p.buyer, "fake-testnet-buyer")
	p.paidByWatcherAmount(350000)
	p.move(p.order, statePaid, stateShipped, p.vendor)
	p.move(p.order, stateShipped, stateCompleted, p.buyer)
	if n := p.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE 'Payout review needed for order%'", p.buyer.ID); n != 1 {
		t.Fatalf("sole party administrator review notices=%d", n)
	}
}

func TestLargeHoldWithoutAddressStillAsksRecipientToSaveOne(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	p.paidByWatcherAmount(350000)
	p.move(p.order, statePaid, stateShipped, p.vendor)
	p.move(p.order, stateShipped, stateCompleted, p.buyer)
	notice := p.str("SELECT body FROM notifications WHERE user_id=$1 AND body LIKE 'Order %more than the order price%' LIMIT 1", p.buyer.ID)
	if !strings.Contains(notice, "Save a BTC payout address") || strings.Contains(notice, "Nothing is needed from you") {
		t.Fatalf("recipient notice missed address action: %q", notice)
	}
	page := html.UnescapeString(p.page("/account", p.buyerSess))
	mustContain(t, page, "No BTC payout address is saved yet: save one on your account page")
	mustNotContain(t, page, "Nothing is needed from you; if it is still held")
	suspended := recipientPayout(p.order, "refund", "BTC", "fake", "0.0025", "", "held", suspendedHold, false, false, false)
	if !strings.Contains(suspended.NextStep, "checks your payout address") ||
		!strings.Contains(suspended.NextStep, "save one on your account page") || strings.Contains(suspended.NextStep, "Nothing is needed from you") {
		t.Fatalf("suspension address copy: %q", suspended.NextStep)
	}
}
