package market

import (
	"regexp"
	"strings"
	"testing"
)

// A-211: a deposit to a closed (cancelled, completed or resolved) order's address is recorded for 30 days after
// the address was issued, flagged for staff, never credited and never paid out automatically. The buyer's order
// page labels it and leaves it out of the Confirmed figure; the buyer is told once per order (the amount, no
// transaction ID; the vendor is not told) and staff once per order, however many transactions arrive.

const lateLabel = "Arrived after the order closed — not paid out; market staff notified"

// lateNotices counts the buyer notifications announcing a deposit that arrived after order closed.
func (p *payEnv) lateNotices(user, order string) int {
	p.t.Helper()
	return p.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE $2", user, "Order "+order[:8]+": %arrived at this order's deposit address after the order closed%")
}

// confirmedFigure checks the payment panel's Confirmed figure.
func confirmedFigure(t *testing.T, body, want string) {
	t.Helper()
	re := regexp.MustCompile(`Confirmed \(at least \d+ confirmations\)</dt>\s*<dd>([0-9.]+) BTC`)
	m := re.FindStringSubmatch(body)
	if m == nil || m[1] != want {
		t.Fatalf("Confirmed figure %v, want %s", m, want)
	}
}

// checkLateDisclosed asserts one late deposit txid of amt on order: flagged and not credited, labelled on the
// buyer's page and left out of the Confirmed figure (confirmed), one buyer notice naming amt without the txid,
// nothing to the vendor since vendorBefore, one review notice per staff member.
func (p *payEnv) checkLateDisclosed(order, txid, amt, confirmed string, vendorBefore int) {
	t := p.t
	t.Helper()
	if n := p.count("SELECT count(*) FROM payments WHERE order_id=$1 AND txid=$2 AND flagged AND NOT credited", order, txid); n != 1 {
		t.Fatalf("late deposit %s flagged and not credited: %d rows", txid, n)
	}
	body := p.page("/order?id="+order, p.buyerSess)
	mustContain(t, body, lateLabel)
	confirmedFigure(t, body, confirmed)
	if n := p.lateNotices(p.buyer.ID, order); n != 1 {
		t.Fatalf("buyer late-deposit notices %d", n)
	}
	var notice string
	if err := p.DB.QueryRow("SELECT body FROM notifications WHERE user_id=$1 AND body LIKE $2", p.buyer.ID, "Order "+order[:8]+": %arrived at this order's deposit address after the order closed%").Scan(&notice); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(notice, amt+" BTC") || strings.Contains(notice, txid) || !strings.Contains(notice, "not refunded automatically") {
		t.Fatalf("buyer notice %q", notice)
	}
	if n := p.count("SELECT count(*) FROM notifications WHERE user_id=$1", p.vendor.ID); n != vendorBefore {
		t.Fatalf("vendor notified of the late deposit: %d notifications, was %d", n, vendorBefore)
	}
	if n := p.allReviewNotices(order); n != 2 {
		t.Fatalf("staff review notices %d, want one each for the moderator and the administrator", n)
	}
}

func TestLateTopUpAfterUnderpaidRefundIsDisclosed(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.buyer, "fake-testnet-buyer")
	p.fake.Deposit(p.addr, "tx-under", 0, 40000, 3)
	p.poll()
	p.ageDeposits(p.order)
	p.poll()
	if st, _, _, amt := p.payout(p.order); p.state(p.order) != stateCancelled || st != "sent" || amt != 40000 {
		t.Fatalf("state %s refund %s %d", p.state(p.order), st, amt)
	}
	vendorBefore := p.count("SELECT count(*) FROM notifications WHERE user_id=$1", p.vendor.ID)
	// The buyer tops up the remaining amount the page last showed.
	p.fake.Deposit(p.addr, "tx-topup", 0, 60000, 3)
	p.poll()
	p.poll()
	p.checkLateDisclosed(p.order, "tx-topup", "0.0006", "0.0004", vendorBefore)
	if n := p.count("SELECT count(*) FROM payouts WHERE order_id=$1", p.order); n != 1 || len(p.fake.Sends()) != 1 {
		t.Fatalf("payouts %d sends %d", n, len(p.fake.Sends()))
	}
	if n := p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE '%deposit tx-topup (0.0006 BTC) arrived after this order was settled and was not paid out. Moderator review required.'", p.order); n != 1 {
		t.Fatalf("flag notes %d", n)
	}
	// The catalog no longer promises that deposits after a cancellation are refunded.
	catalog := p.page("/", p.buyerSess)
	mustContain(t, catalog, "after the order was cancelled, completed or resolved, within 30 days of the address being issued, is recorded and shown on the order page but not refunded automatically")
	mustNotContain(t, catalog, "including a deposit that confirms after the order was cancelled")
}

// The same holds on a completed order whose overpayment return was sent, and on a paid order cancelled with a refund.
func TestLateDepositOnSettledOrderIsDisclosed(t *testing.T) {
	t.Run("completed with a return", func(t *testing.T) {
		p := newPayEnv(t)
		p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
		p.setPayoutAddress(p.buyer, "fake-testnet-buyer")
		p.paidByWatcherAmount(150000)
		p.complete(p.order)
		p.poll()
		if st, _, amt := p.returnPayout(p.order); st != "sent" || amt != 50000 || len(p.fake.Sends()) != 2 {
			t.Fatalf("return %s %d sends %d", st, amt, len(p.fake.Sends()))
		}
		vendorBefore := p.count("SELECT count(*) FROM notifications WHERE user_id=$1", p.vendor.ID)
		p.fake.Deposit(p.addr, "tx-late", 0, 20000, 3)
		p.poll()
		p.checkLateDisclosed(p.order, "tx-late", "0.0002", "0.0015", vendorBefore)
		if len(p.fake.Sends()) != 2 || p.count("SELECT count(*) FROM payouts WHERE order_id=$1", p.order) != 2 {
			t.Fatal("late deposit paid out")
		}
	})
	t.Run("paid then cancelled", func(t *testing.T) {
		p := newPayEnv(t)
		p.setPayoutAddress(p.buyer, "fake-testnet-buyer")
		p.paidByWatcher(p.order, p.addr, "tx-paid")
		p.move(p.order, statePaid, stateCancelled, p.vendor)
		p.poll()
		if st, _, _, amt := p.payout(p.order); st != "sent" || amt != 100000 {
			t.Fatalf("refund %s %d", st, amt)
		}
		vendorBefore := p.count("SELECT count(*) FROM notifications WHERE user_id=$1", p.vendor.ID)
		p.fake.Deposit(p.addr, "tx-late", 0, 20000, 3)
		p.poll()
		p.checkLateDisclosed(p.order, "tx-late", "0.0002", "0.001", vendorBefore)
		if len(p.fake.Sends()) != 1 {
			t.Fatal("late deposit refunded")
		}
	})
}

// A-7 unchanged: an extra confirmed deposit on a paid order is counted and settled with the order.
func TestExtraDepositOnPaidOrderIsStillCounted(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	p.setPayoutAddress(p.buyer, "fake-testnet-buyer")
	p.paidByWatcher(p.order, p.addr, "tx-paid")
	p.fake.Deposit(p.addr, "tx-extra", 0, 20000, 3)
	p.poll()
	body := p.page("/order?id="+p.order, p.buyerSess)
	mustNotContain(t, body, lateLabel)
	confirmedFigure(t, body, "0.0012")
	if n := p.count("SELECT count(*) FROM payments WHERE order_id=$1 AND flagged", p.order); n != 0 {
		t.Fatalf("extra deposit on a paid order flagged: %d", n)
	}
	p.complete(p.order)
	if n := p.count("SELECT COALESCE(sum(amount),0) FROM payouts WHERE order_id=$1", p.order); n != 120000 {
		t.Fatalf("settlement pays %d, want 120000", n)
	}
}

// A-106: a top-up more than 24 hours after an underpaid order was cancelled and refunded is still recorded.
func TestTopUpAfterUnderpaidExpiryOutsideDayIsRecorded(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.buyer, "fake-testnet-buyer")
	p.fake.Deposit(p.addr, "tx-under", 0, 40000, 3)
	p.poll()
	p.ageDeposits(p.order)
	p.poll()
	if st, _, _, _ := p.payout(p.order); p.state(p.order) != stateCancelled || st != "sent" {
		t.Fatalf("state %s refund %s", p.state(p.order), st)
	}
	if _, err := p.DB.Exec("UPDATE orders SET updated=now()-interval '25 hours' WHERE id=$1", p.order); err != nil {
		t.Fatal(err)
	}
	vendorBefore := p.count("SELECT count(*) FROM notifications WHERE user_id=$1", p.vendor.ID)
	p.fake.Deposit(p.addr, "tx-topup", 0, 60000, 3)
	p.poll()
	p.checkLateDisclosed(p.order, "tx-topup", "0.0006", "0.0004", vendorBefore)
	if len(p.fake.Sends()) != 1 {
		t.Fatal("late top-up refunded")
	}
}

// Many small transactions to a closed order's address give one buyer notice and one notice per staff member. A
// deposit first recorded after the order closed is not refunded even when nothing was refunded at the
// cancellation (here: an unpaid order cancelled at the end of the payment window).
func TestLateDepositFloodIsAnnouncedOnce(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.buyer, "fake-testnet-buyer")
	p.expireAddress(p.order)
	p.poll()
	if s := p.state(p.order); s != stateCancelled {
		t.Fatalf("unpaid order %s", s)
	}
	if _, err := p.DB.Exec("UPDATE orders SET updated=now()-interval '25 hours' WHERE id=$1", p.order); err != nil {
		t.Fatal(err)
	}
	vendorBefore := p.count("SELECT count(*) FROM notifications WHERE user_id=$1", p.vendor.ID)
	for _, tx := range []string{"tx-f1", "tx-f2", "tx-f3"} {
		p.fake.Deposit(p.addr, tx, 0, 20000, 3)
	}
	p.poll()
	for _, tx := range []string{"tx-f4", "tx-f5"} {
		p.fake.Deposit(p.addr, tx, 0, 20000, 3)
		p.poll()
	}
	p.poll()
	if n := p.count("SELECT count(*) FROM payments WHERE order_id=$1 AND flagged AND NOT credited", p.order); n != 5 {
		t.Fatalf("flagged late deposits %d", n)
	}
	if n := p.count("SELECT count(*) FROM payouts WHERE order_id=$1", p.order); n != 0 || len(p.fake.Sends()) != 0 {
		t.Fatalf("late deposits refunded: %d payouts", n)
	}
	if n := p.lateNotices(p.buyer.ID, p.order); n != 1 {
		t.Fatalf("buyer notices %d", n)
	}
	if n := p.allReviewNotices(p.order); n != 2 {
		t.Fatalf("staff notices %d", n)
	}
	if n := p.count("SELECT count(*) FROM notifications WHERE user_id=$1", p.vendor.ID); n != vendorBefore {
		t.Fatal("vendor notified")
	}
	body := p.page("/order?id="+p.order, p.buyerSess)
	if n := strings.Count(body, lateLabel); n != 5 {
		t.Fatalf("labelled deposits %d", n)
	}
	confirmedFigure(t, body, "0")
	// Every flagged deposit is named in an order note, so the desk shows when it was flagged.
	for _, tx := range []string{"tx-f1", "tx-f2", "tx-f3", "tx-f4", "tx-f5"} {
		if n := p.count("SELECT count(*) FROM order_events WHERE order_id=$1 AND note LIKE $2", p.order, "%deposit "+tx+" (%Moderator review required."); n != 1 {
			t.Fatalf("flag notes for %s: %d", tx, n)
		}
	}
}
