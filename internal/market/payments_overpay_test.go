package market

import (
	"context"
	"errors"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"rsc.io/qr"
)

// Overpayments: a release pays the vendor exactly the price and returns the excess to the buyer with the network
// fee taken from it; an excess below the minimum automatic refund goes to the vendor; a refund or return larger
// than the price (a likely mistake) waits for an administrator, while one up to the price is sent automatically;
// every payout (release, refund or return) is sent with the fee-from-amount call (A-232).

func TestSplitPayoutRules(t *testing.T) {
	o := &Order{BuyerID: "b", VendorID: "v", Currency: "BTC"}
	const price = 100000
	const xmrPrice = 500000000000 // 0.5 XMR
	for _, tc := range []struct {
		name, kind, currency string
		sum                  int64
		want                 []payoutPart
	}{
		{"exact release", "release", "BTC", price, []payoutPart{{"release", "v", "vendor", price, false}}},
		{"underpaid release", "release", "BTC", 90000, []payoutPart{{"release", "v", "vendor", 90000, false}}},
		{"excess below the floor stays with the vendor", "release", "BTC", price + minPayoutBTC - 1, []payoutPart{{"release", "v", "vendor", price + minPayoutBTC - 1, false}}},
		{"excess at the floor is returned", "release", "BTC", price + minPayoutBTC, []payoutPart{{"release", "v", "vendor", price, false}, {"return", "b", "buyer", minPayoutBTC, false}}},
		{"excess equal to the price is returned", "release", "BTC", 2 * price, []payoutPart{{"release", "v", "vendor", price, false}, {"return", "b", "buyer", price, false}}},
		{"excess above the price is held", "release", "BTC", 2*price + 1, []payoutPart{{"release", "v", "vendor", price, false}, {"return", "b", "buyer", price + 1, true}}},
		{"refund up to the price", "refund", "BTC", price, []payoutPart{{"refund", "b", "buyer", price, false}}},
		{"refund above the price is held", "refund", "BTC", price + 1, []payoutPart{{"refund", "b", "buyer", price + 1, true}}},
		{"XMR excess below its floor stays with the vendor", "release", "XMR", xmrPrice + minPayoutXMR - 1, []payoutPart{{"release", "v", "vendor", xmrPrice + minPayoutXMR - 1, false}}},
		{"XMR excess at its floor is returned", "release", "XMR", xmrPrice + minPayoutXMR, []payoutPart{{"release", "v", "vendor", xmrPrice, false}, {"return", "b", "buyer", minPayoutXMR, false}}},
		{"XMR excess above the price is held", "release", "XMR", 2*xmrPrice + 1, []payoutPart{{"release", "v", "vendor", xmrPrice, false}, {"return", "b", "buyer", xmrPrice + 1, true}}},
		{"XMR refund above the price is held", "refund", "XMR", xmrPrice + 1, []payoutPart{{"refund", "b", "buyer", xmrPrice + 1, true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o.Currency = tc.currency
			required := int64(price)
			if tc.currency == "XMR" {
				required = xmrPrice
			}
			got := splitPayout(tc.kind, o, tc.sum, required)
			if len(got) != len(tc.want) {
				t.Fatalf("got %+v", got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("part %d: got %+v want %+v", i, got[i], tc.want[i])
				}
			}
			var total int64
			for _, part := range got {
				total += part.amount
			}
			if total != tc.sum {
				t.Fatalf("parts pay %d of %d", total, tc.sum)
			}
		})
	}
}

// An overpayment worth returning: the vendor gets the price and the buyer the excess, each with the fee taken from it.
func TestOverpaymentReturnedToBuyer(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	p.setPayoutAddress(p.buyer, "fake-testnet-buyer")
	p.paidByWatcherAmount(150000)
	// A-215: the shipped order's page says, for this order, that completing returns the excess (fee deducted).
	buyerPage, vendorPage := p.overpaidPages()
	mustContain(t, buyerPage, "You sent 0.0005 BTC more than the price (test network). Completing releases the price to the vendor and returns the excess to your saved BTC payout address, with the network fee deducted.",
		"Release 0.001 BTC (test network) to "+p.vendor.Handle+" (the price) and return the 0.0005 BTC paid over the price to "+p.buyer.Handle+"; network fee deducted from each. This is final.")
	mustNotContain(t, buyerPage, "the return waits", "below the minimum automatic refund", "administrator reviews its return")
	mustContain(t, vendorPage, "The buyer sent 0.0005 BTC more than the price (test network). Completing releases the price to you and returns the excess to the buyer, with the network fee deducted.")
	mustNotContain(t, vendorPage, "your saved", "payout address saved", "the return waits")
	p.move(p.order, stateShipped, stateCompleted, p.buyer)
	if st, _, addr, amt := p.payout(p.order); st != "pending" || addr != "fake-testnet-vendor" || amt != 100000 {
		t.Fatalf("release %s %s %d", st, addr, amt)
	}
	if st, errText, amt := p.returnPayout(p.order); st != "pending" || errText != "" || amt != 50000 {
		t.Fatalf("return %s %q %d", st, errText, amt)
	}
	if n := p.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%0.0005 BTC you paid over the price is being returned%'", p.buyer.ID); n != 1 {
		t.Fatalf("buyer return notices %d", n)
	}
	p.poll()
	sends := p.fake.Sends()
	if len(sends) != 2 {
		t.Fatalf("sends %+v", sends)
	}
	for _, s := range sends {
		switch s.To {
		case "fake-testnet-vendor":
			if s.Amount != 100000 || !s.FeeFromAmount {
				t.Fatalf("release sent %+v", s)
			}
		case "fake-testnet-buyer":
			if s.Amount != 50000 || !s.FeeFromAmount {
				t.Fatalf("return sent %+v", s)
			}
		default:
			t.Fatalf("unexpected send %+v", s)
		}
	}
	// Repeated passes send nothing more; the buyer sees the return on the order page.
	p.poll()
	if len(p.fake.Sends()) != 2 {
		t.Fatal("payouts sent twice")
	}
	mustContain(t, p.page("/order?id="+p.order, p.buyerSess), "Return of the amount paid over the price", "Return to buyer", "Release to vendor")
}

// A dust excess is not sent back (it would cost more than it is worth); the vendor receives it with the release.
func TestSmallExcessStaysWithVendor(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	p.paidByWatcherAmount(100000 + minPayoutBTC - 1)
	// A-215: the page names the floor and says the excess is not returned; nothing waits for a payout address.
	buyerPage, vendorPage := p.overpaidPages()
	mustContain(t, buyerPage, "You sent 0.00009999 BTC more than the price (test network). Completing releases everything to the vendor: the excess is below the minimum automatic refund of 0.0001 BTC, so it is not returned.",
		"Release 0.00109999 BTC (test network) to "+p.vendor.Handle+" — 0.00009999 BTC more than the price, below the minimum automatic refund, so it is not returned; network fee deducted. This is final.")
	mustNotContain(t, buyerPage, "returns the excess", "the return waits", "your saved")
	mustContain(t, vendorPage, "The buyer sent 0.00009999 BTC more than the price (test network). Completing releases everything to you: the excess is below the minimum automatic refund of 0.0001 BTC, so it is not returned.")
	mustNotContain(t, vendorPage, "returns the excess", "your saved")
	p.move(p.order, stateShipped, stateCompleted, p.buyer)
	if st, _, _, amt := p.payout(p.order); st != "pending" || amt != 100000+minPayoutBTC-1 {
		t.Fatalf("release %s %d", st, amt)
	}
	if n := p.count("SELECT count(*) FROM payouts WHERE order_id=$1", p.order); n != 1 {
		t.Fatalf("payouts %d", n)
	}
	for _, u := range []*User{p.buyer, p.vendor} {
		// A-250: the notice names the payout floor and says the vendor bears the release's network fee.
		kept := "Order " + p.order[:8] + ": the TESTNET release of 0.00109999 BTC to the vendor includes 0.00009999 BTC more than the price, below the minimum automatic payout of 0.0001 BTC, so it is not returned; the network fee is taken from the release."
		if n := p.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body=$2", u.ID, kept); n != 1 {
			t.Fatalf("%s not told the excess is kept: %d", u.Handle, n)
		}
	}
}

// A return larger than the price is never sent automatically: administrators are told, and releasing it needs
// an explicit review confirmation.
func TestLargeReturnHeldUntilAdminReview(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	p.setPayoutAddress(p.buyer, "fake-testnet-buyer")
	adminID := p.str("SELECT id FROM users WHERE role='admin'")
	p.paidByWatcherAmount(350000)
	// A-215: the page says the return waits for an administrator, not that it goes back automatically.
	buyerPage, vendorPage := p.overpaidPages()
	mustContain(t, buyerPage, "You sent 0.0025 BTC more than the price (test network). Completing releases the price to the vendor. The excess is more than the price, so an administrator reviews its return to your saved BTC payout address (network fee deducted) before it is sent.",
		"return the 0.0025 BTC paid over the price to "+p.buyer.Handle+"; network fee deducted from each; the return is more than the price, so an administrator reviews it before it is sent. This is final.")
	mustNotContain(t, buyerPage, "returns the excess", "the return waits")
	mustContain(t, vendorPage, "The buyer sent 0.0025 BTC more than the price (test network). Completing releases the price to you. The excess is more than the price, so an administrator reviews its return to the buyer (network fee deducted) before it is sent.")
	mustNotContain(t, vendorPage, "returns the excess", "your saved")
	p.move(p.order, stateShipped, stateCompleted, p.buyer)
	if st, errText, amt := p.returnPayout(p.order); st != "held" || errText != largeHold || amt != 250000 {
		t.Fatalf("return %s %q %d", st, errText, amt)
	}
	if n := p.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE 'Payout review needed for order%more than the order price%'", adminID); n != 1 {
		t.Fatalf("administrator notices %d", n)
	}
	p.poll()
	p.poll()
	if s := p.fake.Sends(); len(s) != 1 || s[0].To != "fake-testnet-vendor" {
		t.Fatalf("held return sent: %+v", s)
	}
	admin := p.page("/admin", p.adminSess)
	mustContain(t, admin, "Held: larger than the order price — review before releasing", "Return of overpayment", `name="large_reviewed" value="confirmed" required`)
	// A-212: the hold catches likely mistakes above the price; it never claims to stop swaps (QA18-2 pins the copy,
	// which the errText == largeHold comparisons above cannot).
	if !strings.Contains(largeHold, "Refunds and returns up to the order price are sent automatically") || strings.Contains(largeHold, "not used to swap coins") {
		t.Fatalf("largeHold overclaims: %q", largeHold)
	}
	mustContain(t, admin, "This check does not stop swaps: refunds and returns up to the order price are sent automatically")
	id := p.str("SELECT id::text FROM payouts WHERE order_id=$1 AND kind='return'", p.order)
	release := url.Values{"payout_id": {id}, "op": {"release"}, "password": {testPassword}}
	if w := p.do("POST", "/admin/payout", p.adminSess, release); w.Code != 409 || !strings.Contains(w.Body.String(), "larger than the order price") || !strings.Contains(w.Body.String(), "this check does not stop swaps up to a listing's price") {
		t.Fatalf("release without review: %d %s", w.Code, w.Body.String())
	}
	if st, _, _ := p.returnPayout(p.order); st != "held" {
		t.Fatalf("unreviewed release changed the return: %s", st)
	}
	release.Set("large_reviewed", "confirmed")
	p.check(p.do("POST", "/admin/payout", p.adminSess, release), 303)
	if n := p.count("SELECT count(*) FROM audit_events WHERE action LIKE '%larger than the order price, administrator reviewed it%'"); n != 1 {
		t.Fatalf("audit rows %d", n)
	}
	p.poll()
	if st, _, amt := p.returnPayout(p.order); st != "sent" || amt != 250000 {
		t.Fatalf("return after review %s %d", st, amt)
	}
	if s := p.fake.Sends(); len(s) != 2 || s[1].To != "fake-testnet-buyer" || !s[1].FeeFromAmount {
		t.Fatalf("sends %+v", s)
	}
}

// Suspending the recipient keeps the large hold: releasing needs both the address check and the review.
func TestSuspensionKeepsLargeHold(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.buyer, "fake-testnet-buyer")
	p.paidByWatcherAmount(100000)
	p.move(p.order, statePaid, stateCancelled, p.vendor)
	if _, err := p.DB.Exec("UPDATE payouts SET amount=150000,state='held',error=$2 WHERE order_id=$1", p.order, largeHold); err != nil {
		t.Fatal(err)
	}
	p.check(p.do("POST", "/admin/suspend", p.adminSess, suspendForm("suspend", p.buyer.Handle)), 303)
	if st, _, _, _ := p.payout(p.order); st != "held" {
		t.Fatalf("state %s", st)
	}
	if got := p.str("SELECT error FROM payouts WHERE order_id=$1", p.order); got != suspendedHold+" "+largeHold {
		t.Fatalf("hold %q", got)
	}
	id := p.payoutID(p.order)
	release := url.Values{"payout_id": {id}, "op": {"release"}, "password": {testPassword}, "address_checked": {"confirmed"}}
	if w := p.do("POST", "/admin/payout", p.adminSess, release); w.Code != 409 || !strings.Contains(w.Body.String(), "larger than the order price") {
		t.Fatalf("release with only the address check: %d", w.Code)
	}
	release.Del("address_checked")
	release.Set("large_reviewed", "confirmed")
	if w := p.do("POST", "/admin/payout", p.adminSess, release); w.Code != 409 || !strings.Contains(w.Body.String(), "suspended") {
		t.Fatalf("release with only the review: %d", w.Code)
	}
	release.Set("address_checked", "confirmed")
	p.check(p.do("POST", "/admin/payout", p.adminSess, release), 303)
	if st, _, _, _ := p.payout(p.order); st != "pending" {
		t.Fatalf("state after both checks %s", st)
	}
}

// The large hold's documented bound (operator guide, "Overpayments"): a refund of up to the order price is not
// held, whether the price was paid in full and the order cancelled by the vendor or a deposit just short of it
// was cancelled by the buyer. Such refunds start pending and the watcher sends them without an administrator.
func TestRefundUpToPriceIsNotHeld(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.buyer, "fake-testnet-buyer")
	adminID := p.str("SELECT id FROM users WHERE role='admin'")
	refund := func(order string) (state, errText string, amt int64) {
		t.Helper()
		if err := p.DB.QueryRow("SELECT state,error,amount FROM payouts WHERE order_id=$1 AND kind='refund'", order).Scan(&state, &errText, &amt); err != nil {
			t.Fatalf("refund for %s: %v", order[:8], err)
		}
		return
	}
	// Equal to the price: paid in full, then cancelled by the vendor.
	p.paidByWatcherAmount(100000)
	p.check(p.do("POST", "/orders/cancel", p.vendorSess, payoutConfirmed(form("order_id", p.order, "from", statePaid), 100000)), 303)
	if st, errText, amt := refund(p.order); st != "pending" || errText != "" || amt != 100000 {
		t.Fatalf("refund of the price: %s %q %d", st, errText, amt)
	}
	// Just under the price: one confirmed deposit a satoshi short, then cancelled by the buyer.
	short, addr := p.newOrder(stateAwaitingPayment)
	p.fake.Deposit(addr, "tx-short", 0, 99999, 3)
	p.poll()
	if s := p.state(short); s != stateAwaitingPayment {
		t.Fatalf("underpaid order %s", s)
	}
	p.check(p.do("POST", "/orders/cancel", p.buyerSess, payoutConfirmed(form("order_id", short, "from", stateAwaitingPayment), 99999)), 303)
	if st, errText, amt := refund(short); st != "pending" || errText != "" || amt != 99999 {
		t.Fatalf("refund just under the price: %s %q %d", st, errText, amt)
	}
	if n := p.count("SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE 'Payout review needed%'", adminID); n != 0 {
		t.Fatalf("administrator review notices %d", n)
	}
	p.poll()
	if s := p.fake.Sends(); len(s) != 2 || s[0].Amount+s[1].Amount != 199999 {
		t.Fatalf("refunds up to the price not sent automatically: %+v", s)
	}
}

// A refund is handed to the provider's fee-from-amount send (SendFeeFromAmount) for the whole counted amount. The
// fake wallet only records the call; TestMoneroRefundSubtractsFee checks the Monero request, and Bitcoin's Send
// already subtracts the fee. That real wallets deduct it is not shown here.
func TestRefundIsSentWithFeeFromAmount(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.buyer, "fake-testnet-buyer")
	p.fake.Deposit(p.addr, "tx-part", 0, 40000, 3)
	p.poll()
	p.check(p.do("POST", "/orders/cancel", p.buyerSess, payoutConfirmed(form("order_id", p.order, "from", stateAwaitingPayment), 40000)), 303)
	p.poll()
	if s := p.fake.Sends(); len(s) != 1 || s[0].Amount != 40000 || !s[0].FeeFromAmount {
		t.Fatalf("refund sends %+v", s)
	}
}

// The payout kinds are constrained by the schema: one of each per order, never a release and a refund, never a
// refund and a return.
func TestPayoutKindsPerOrder(t *testing.T) {
	p := newPayEnv(t)
	insert := func(kind string) error {
		_, err := p.DB.Exec("INSERT INTO payouts(order_id,kind,user_id,currency,amount) VALUES($1,$2,$3,'BTC',1000)", p.order, kind, p.buyer.ID)
		return err
	}
	if err := insert("release"); err != nil {
		t.Fatal(err)
	}
	if err := insert("return"); err != nil {
		t.Fatal(err)
	}
	if err := insert("return"); err == nil {
		t.Fatal("second return for one order accepted")
	}
	if err := insert("bonus"); err == nil {
		t.Fatal("unknown payout kind accepted")
	}
	// One settlement per order (A-218, migration 069): a release and a refund for one order are refused in either
	// order, by the database itself.
	uniqueViolation := func(err error, index string) bool {
		var pe *pgconn.PgError
		return errors.As(err, &pe) && pe.Code == "23505" && pe.ConstraintName == index
	}
	if err := insert("refund"); !uniqueViolation(err, "payouts_one_settlement") {
		t.Fatalf("refund after a release: %v", err)
	}
	p.order, _ = p.newOrder(stateCancelled)
	if err := insert("refund"); err != nil {
		t.Fatal(err)
	}
	if err := insert("release"); !uniqueViolation(err, "payouts_one_settlement") {
		t.Fatalf("release after a refund: %v", err)
	}
	// A refund beside a return would pay the excess twice (A-276, migration 070).
	if err := insert("return"); !uniqueViolation(err, "payouts_refund_or_return") {
		t.Fatalf("return after a refund: %v", err)
	}
	p.order, _ = p.newOrder(stateCancelled)
	if err := insert("return"); err != nil {
		t.Fatal(err)
	}
	if err := insert("refund"); !uniqueViolation(err, "payouts_refund_or_return") {
		t.Fatalf("refund after a return: %v", err)
	}
	if n := p.count("SELECT count(*) FROM payouts WHERE order_id=$1", p.order); n != 1 {
		t.Fatalf("%d payouts", n)
	}
}

// settlementClashSQL is the SQL UPGRADING.md section 14 gives for settling a clash migration 069 or 070 refuses:
// list every payout of the named orders (returns included), keep the refund by deleting the release together with
// its return, or keep the release (and its return) by deleting the refund, and check that no order is left with
// two settlements or a refund beside a return. TestSettlementClashGuide runs it, with psql's :'order_id' bound as
// $1, and checks that UPGRADING.md carries it verbatim (whitespace aside).
var settlementClashSQL = struct{ list, keepRefund, keepRelease, check string }{
	list: `SELECT p.id, p.order_id, o.state AS order_state, p.kind, p.state, p.send_ambiguous, p.amount, p.address, p.txid, p.error
FROM payouts p JOIN orders o ON o.id = p.order_id
WHERE p.order_id IN (SELECT order_id FROM payouts GROUP BY order_id
  HAVING count(*) FILTER (WHERE kind IN ('release','refund')) > 1
      OR count(*) FILTER (WHERE kind IN ('refund','return')) > 1)
ORDER BY p.order_id, p.kind;`,
	keepRefund:  `DELETE FROM payouts WHERE order_id = :'order_id' AND kind IN ('release','return') RETURNING id, kind, state, txid;`,
	keepRelease: `DELETE FROM payouts WHERE order_id = :'order_id' AND kind = 'refund' RETURNING id, kind, state, txid;`,
	check: `SELECT order_id, string_agg(kind, ',' ORDER BY kind) AS kinds FROM payouts GROUP BY order_id
HAVING count(*) FILTER (WHERE kind IN ('release','refund')) > 1
    OR count(*) FILTER (WHERE kind IN ('refund','return')) > 1;`,
}

// unsettle removes migrations 069 and 070 (their indexes and records) so a test can insert the payouts they refuse,
// and returns a function applying both in one transaction as a startup does, after running pre (with args) in that
// transaction; a refused migration rolls everything back.
func (p *payEnv) unsettle() func(pre string, args ...any) error {
	p.t.Helper()
	if _, err := p.DB.Exec("DROP INDEX payouts_one_settlement; DROP INDEX payouts_refund_or_return; DELETE FROM schema_migrations WHERE version IN (69,70)"); err != nil {
		p.t.Fatal(err)
	}
	list, err := loadMigrations(migrationFiles)
	if err != nil {
		p.t.Fatal(err)
	}
	var settle []migration
	for _, m := range list {
		if m.Version == 69 || m.Version == 70 {
			settle = append(settle, m)
		}
	}
	if len(settle) != 2 {
		p.t.Fatalf("settlement migrations %+v", settle)
	}
	return func(pre string, args ...any) error {
		tx, err := p.DB.Begin()
		if err != nil {
			p.t.Fatal(err)
		}
		defer tx.Rollback()
		if pre != "" {
			if _, err = tx.Exec(pre, args...); err != nil {
				p.t.Fatal(err)
			}
		}
		if err = applyMigrations(context.Background(), tx, settle); err != nil {
			return err
		}
		return tx.Commit()
	}
}

func (p *payEnv) insertPayouts(order string, kinds ...string) {
	p.t.Helper()
	for _, kind := range kinds {
		if _, err := p.DB.Exec("INSERT INTO payouts(order_id,kind,user_id,currency,amount) VALUES($1,$2,$3,'BTC',100000)", order, kind, p.buyer.ID); err != nil {
			p.t.Fatal(err)
		}
	}
}

// settlementIndexes counts migration 069's and 070's indexes and records: 4 when both applied.
func (p *payEnv) settlementIndexes() int {
	p.t.Helper()
	return p.count("SELECT count(*) FROM pg_indexes WHERE indexname IN ('payouts_one_settlement','payouts_refund_or_return')") +
		p.count("SELECT count(*) FROM schema_migrations WHERE version IN (69,70)")
}

// A-218: migration 069 creates the one-settlement index. A database already holding a release and a refund for one
// order (possible only by hand since migration 064; enqueuePayout never queues a second settlement) stops the
// migration with an error naming the order, and nothing is changed or deleted; once staff settle the clash it applies.
func TestOneSettlementMigrationRefusesExistingClash(t *testing.T) {
	p := newPayEnv(t)
	apply := p.unsettle()
	p.insertPayouts(p.order, "release", "return", "refund")
	err := apply("")
	if err == nil || !strings.Contains(err.Error(), "migration 069_payout_one_settlement") || !strings.Contains(err.Error(), p.order) ||
		!strings.Contains(err.Error(), "both a release and a refund") || !strings.Contains(err.Error(), "UPGRADING.md") {
		t.Fatalf("migration over a clash: %v", err)
	}
	if n := p.count("SELECT count(*) FROM payouts WHERE order_id=$1", p.order); n != 3 {
		t.Fatalf("migration changed payouts: %d left", n)
	}
	if p.settlementIndexes() != 0 {
		t.Fatal("a refused migration left its index or record")
	}
	if err = apply("DELETE FROM payouts WHERE order_id=$1 AND kind='refund'", p.order); err != nil {
		t.Fatal(err)
	}
	if p.settlementIndexes() != 4 || p.count("SELECT count(*) FROM payouts WHERE order_id=$1", p.order) != 2 {
		t.Fatal("migration did not apply once the clash was settled")
	}
}

// A-276, A-268: UPGRADING.md section 14 settles a clash by deleting a whole settlement. Deleting only the release
// of a release+return+refund order (what the guide once said, "delete the other row") leaves the refund beside the
// release's return, which the watcher would send, paying the excess a second time: migration 070 refuses that,
// naming the order, and changes nothing. The guide's own SQL lists the return with the clash, deletes a release
// together with its return (or the refund alone), and its final check finds no order with a refund and a return;
// both migrations then apply.
func TestSettlementClashGuide(t *testing.T) {
	p := newPayEnv(t)
	apply := p.unsettle()
	keepRefund, keepRelease := p.order, p.testEnv.order(p.buyer.ID, p.productID, "BTC", stateCompleted)
	leftover := p.testEnv.order(p.buyer.ID, p.productID, "BTC", stateCancelled) // what the old guide left
	clean := p.testEnv.order(p.buyer.ID, p.productID, "BTC", stateCompleted)
	p.insertPayouts(keepRefund, "release", "return", "refund")
	p.insertPayouts(keepRelease, "release", "return", "refund")
	p.insertPayouts(leftover, "refund", "return")
	p.insertPayouts(clean, "release", "return")
	total := p.count("SELECT count(*) FROM payouts")
	if err := apply(""); err == nil || !strings.Contains(err.Error(), "migration 069_payout_one_settlement") {
		t.Fatalf("migrations over a release+refund clash: %v", err)
	}
	// The old guide: delete "the other row", the release only. Migration 069 then passes and 070 refuses.
	err := apply("DELETE FROM payouts WHERE kind='release' AND order_id=ANY($1)", []string{keepRefund, keepRelease})
	if err == nil || !strings.Contains(err.Error(), "migration 070_payout_refund_or_return") || !strings.Contains(err.Error(), "both a refund and a return") ||
		!strings.Contains(err.Error(), keepRefund) || !strings.Contains(err.Error(), leftover) || strings.Contains(err.Error(), clean) || !strings.Contains(err.Error(), "UPGRADING.md") {
		t.Fatalf("migrations over a refund beside a return: %v", err)
	}
	if p.count("SELECT count(*) FROM payouts") != total || p.settlementIndexes() != 0 {
		t.Fatal("a refused migration changed payouts or left its index or record")
	}

	// Step 1: the listing names every payout of the clashing orders, returns included, and no other order.
	rows, err := p.DB.Query(settlementClashSQL.list)
	if err != nil {
		t.Fatal(err)
	}
	cols, _ := rows.Columns()
	var listed []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err = rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		listed = append(listed, vals[1].(string)+":"+vals[3].(string))
	}
	rows.Close()
	want := []string{keepRefund + ":refund", keepRefund + ":release", keepRefund + ":return", keepRelease + ":refund", keepRelease + ":release", keepRelease + ":return", leftover + ":refund", leftover + ":return"}
	sort.Strings(listed)
	sort.Strings(want)
	if strings.Join(listed, ",") != strings.Join(want, ",") {
		t.Fatalf("clash listing %v, want %v", listed, want)
	}
	// Step 4: delete the settlement that does not stand, a release always with its return.
	del := func(stmt, order string, want int) {
		t.Helper()
		res, err := p.DB.Exec(strings.ReplaceAll(stmt, ":'order_id'", "$1"), order)
		if err != nil {
			t.Fatal(err)
		}
		if n, _ := res.RowsAffected(); n != int64(want) {
			t.Fatalf("deleted %d payouts of %s, want %d", n, order, want)
		}
	}
	del(settlementClashSQL.keepRefund, keepRefund, 2)
	del(settlementClashSQL.keepRelease, keepRelease, 1)
	del(settlementClashSQL.keepRefund, leftover, 1)
	// Step 5: the check finds nothing, and the migrations apply.
	if n := p.count("SELECT count(*) FROM (" + strings.TrimSuffix(settlementClashSQL.check, ";") + ") c"); n != 0 {
		t.Fatalf("check lists %d orders after the guide's deletions", n)
	}
	if n := p.count("SELECT count(*) FROM payouts f WHERE kind='refund' AND EXISTS (SELECT 1 FROM payouts r WHERE r.order_id=f.order_id AND r.kind='return')"); n != 0 {
		t.Fatalf("%d orders keep a refund and a return", n)
	}
	if err = apply(""); err != nil {
		t.Fatal(err)
	}
	kinds := func(order string) string {
		var k string
		if err := p.DB.QueryRow("SELECT coalesce(string_agg(kind, ',' ORDER BY kind), '') FROM payouts WHERE order_id=$1", order).Scan(&k); err != nil {
			t.Fatal(err)
		}
		return k
	}
	if p.settlementIndexes() != 4 || kinds(keepRefund) != "refund" || kinds(keepRelease) != "release,return" || kinds(leftover) != "refund" || kinds(clean) != "release,return" {
		t.Fatalf("after the guide: %s %s %s %s", kinds(keepRefund), kinds(keepRelease), kinds(leftover), kinds(clean))
	}

	doc, err := os.ReadFile("UPGRADING.md")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(string(doc)), " ")
	for _, q := range []string{settlementClashSQL.list, settlementClashSQL.keepRefund, settlementClashSQL.keepRelease, settlementClashSQL.check} {
		if q = strings.Join(strings.Fields(q), " "); !strings.Contains(text, q) {
			t.Fatalf("UPGRADING.md does not carry the settlement SQL %q", q)
		}
	}
}

// A-215: without a payout address in the order's currency the buyer is told the return waits until they save one;
// the vendor is not told about the buyer's addresses.
func TestOverpaidAlertWithoutBuyerPayoutAddress(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	if _, err := p.DB.Exec("UPDATE users SET payout_xmr='fake-testnet-xmr' WHERE id=$1", p.buyer.ID); err != nil {
		t.Fatal(err) // an address in another currency does not count
	}
	p.paidByWatcherAmount(150000)
	buyerPage, vendorPage := p.overpaidPages()
	mustContain(t, buyerPage, "Completing releases the price to the vendor and returns the excess to you, with the network fee deducted. You have no BTC payout address saved, so the return waits until you save one:",
		`<a class="text-link" href="/account#payout">save a payout address</a>`)
	mustNotContain(t, buyerPage, "your saved BTC payout address")
	mustContain(t, vendorPage, "Completing releases the price to you and returns the excess to the buyer, with the network fee deducted.")
	mustNotContain(t, vendorPage, "your saved", "payout address saved", "the return waits")
	p.move(p.order, stateShipped, stateCompleted, p.buyer)
	if st, _, amt := p.returnPayout(p.order); st != "blocked" || amt != 50000 {
		t.Fatalf("return %s %d", st, amt)
	}
}

// A-215, Monero: the alert names the XMR floor, and a Bitcoin payout address does not count for an XMR return.
func TestOverpaidXMRAlertNamesItsFloorAndAddress(t *testing.T) {
	p := newPayEnv(t)
	xf := newFakeProvider("XMR", 10)
	p.A.payments["XMR"] = xf
	p.setPayoutAddress(p.buyer, "fake-testnet-buyer")
	for _, tc := range []struct {
		deposit    int64
		want, deny string
	}{
		{500000000000 + minPayoutXMR - 1, "Completing releases everything to the vendor: the excess is below the minimum automatic refund of 0.001 XMR, so it is not returned.", "payout address saved"},
		{500000000000 + minPayoutXMR, "returns the excess to you, with the network fee deducted. You have no XMR payout address saved, so the return waits until you save one:", "your saved"},
	} {
		id := p.testEnv.order(p.buyer.ID, p.productID, "XMR", stateAwaitingPayment)
		addr, err := xf.NewAddress(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = p.DB.Exec("UPDATE orders SET amount=500000000000 WHERE id=$1", id); err != nil {
			t.Fatal(err)
		}
		if _, err = p.DB.Exec("INSERT INTO payment_addresses(order_id,currency,address,provider) VALUES($1,'XMR',$2,'fake')", id, addr); err != nil {
			t.Fatal(err)
		}
		xf.Deposit(addr, "xmr-"+strconv.FormatInt(tc.deposit, 10), 0, tc.deposit, 10)
		p.poll()
		p.move(id, statePaid, stateShipped, p.vendor)
		page := p.page("/order?id="+id, p.buyerSess)
		mustContain(t, page, tc.want)
		mustNotContain(t, page, tc.deny, "whole counted amount")
	}
}

// overpaidPages ships the paid fixture order and returns its buyer's and vendor's order pages; the buyer's shows
// the complete form. Neither says completing releases the whole counted amount (A-215).
func (p *payEnv) overpaidPages() (buyer, vendor string) {
	p.t.Helper()
	p.move(p.order, statePaid, stateShipped, p.vendor)
	buyer, vendor = p.page("/order?id="+p.order, p.buyerSess), p.page("/order?id="+p.order, p.vendorSess)
	mustContain(p.t, buyer, "Confirms you received the order. Completing queues the payouts stated below; the payment watcher sends them automatically unless the statement says otherwise.", `name="payout_confirmed"`)
	for _, page := range []string{buyer, vendor} {
		mustNotContain(p.t, page, "whole counted amount")
	}
	return buyer, vendor
}

// paidByWatcherAmount pays the fixture order with one confirmed deposit of amt satoshi.
func (p *payEnv) paidByWatcherAmount(amt int64) {
	p.t.Helper()
	p.fake.Deposit(p.addr, "tx-paid-"+strconv.FormatInt(amt, 10), 0, amt, 3)
	p.poll()
	if s := p.state(p.order); s != statePaid {
		p.t.Fatalf("order not paid: %s", s)
	}
}

func TestPaymentURI(t *testing.T) {
	if got := paymentURI("BTC", "tb1qdeposit", 60000); got != "bitcoin:tb1qdeposit?amount=0.0006" {
		t.Fatal(got)
	}
	if got := paymentURI("BTC", "tb1qdeposit", 100000000); got != "bitcoin:tb1qdeposit?amount=1" {
		t.Fatal(got)
	}
	if got := paymentURI("XMR", "7abc", 500000000001); got != "monero:7abc?tx_amount=0.500000000001" {
		t.Fatal(got)
	}
}

// The SVG draws exactly the encoder's dark modules, offset by the quiet zone, on a white square.
func TestQRSVGMatchesEncoding(t *testing.T) {
	text := "bitcoin:tb1qorderaddress000000000000000000000?amount=0.0006"
	svg, err := qrSVG(text, `pay "0.0006" <BTC>`)
	if err != nil {
		t.Fatal(err)
	}
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		t.Fatal(err)
	}
	s := string(svg)
	n := code.Size + 2*qrQuiet
	if !strings.Contains(s, `viewBox="0 0 `+strconv.Itoa(n)+` `+strconv.Itoa(n)+`"`) || !strings.Contains(s, `aria-label="pay &#34;0.0006&#34; &lt;BTC&gt;"`) || !strings.Contains(s, `fill="#fff"`) {
		t.Fatalf("svg header %.200s", s)
	}
	d := regexp.MustCompile(` d="([^"]*)"`).FindStringSubmatch(s)
	if d == nil {
		t.Fatal("no path")
	}
	drawn := map[[2]int]bool{}
	for _, m := range regexp.MustCompile(`M(\d+) (\d+)h(\d+)v1h-(\d+)z`).FindAllStringSubmatch(d[1], -1) {
		x, _ := strconv.Atoi(m[1])
		y, _ := strconv.Atoi(m[2])
		w, _ := strconv.Atoi(m[3])
		if m[3] != m[4] {
			t.Fatalf("run %q not closed", m[0])
		}
		for i := range w {
			drawn[[2]int{x + i, y}] = true
		}
	}
	for y := range n {
		for x := range n {
			want := x >= qrQuiet && y >= qrQuiet && x < qrQuiet+code.Size && y < qrQuiet+code.Size && code.Black(x-qrQuiet, y-qrQuiet)
			if drawn[[2]int{x, y}] != want {
				t.Fatalf("module %d,%d drawn=%v want %v", x, y, drawn[[2]int{x, y}], want)
			}
		}
	}
}

// The order page shows the buyer a QR code for exactly the amount still due, and nothing once it is covered.
func TestOrderPageQRForAmountDue(t *testing.T) {
	p := newPayEnv(t)
	uri := func(due string) string { return "bitcoin:" + p.addr + "?amount=" + due }
	page := p.page("/order?id="+p.order, p.buyerSess)
	mustContain(t, page, `<figure class="payment-qr"><svg`, uri("0.001"), "Scan to pay exactly <strong>0.001 BTC</strong>",
		"If no code appears, copy the address and amount above.")
	mustNotContain(t, p.page("/order?id="+p.order, p.vendorSess), `class="payment-qr"`)
	// A deposit seen (even unconfirmed) lowers the amount asked for.
	p.fake.Deposit(p.addr, "tx-part", 0, 40000, 1)
	p.poll()
	page = p.page("/order?id="+p.order, p.buyerSess)
	mustContain(t, page, uri("0.0006"), "Scan to pay exactly <strong>0.0006 BTC</strong>")
	mustNotContain(t, page, uri("0.001"))
	svg := strings.TrimPrefix(regexp.MustCompile(`(?s)<figure class="payment-qr"><svg.*?</svg>`).FindString(page), `<figure class="payment-qr">`)
	want, err := qrSVG(uri("0.0006"), "QR code: pay 0.0006 BTC to the deposit address")
	if err != nil || svg != string(want) {
		t.Fatalf("page QR code does not encode the amount due:\npage %.200s\nwant %.200s", svg, want)
	}
	// Once the full amount is seen, no QR code invites a second payment.
	p.fake.Deposit(p.addr, "tx-rest", 0, 60000, 1)
	p.poll()
	mustNotContain(t, p.page("/order?id="+p.order, p.buyerSess), `class="payment-qr"`, "bitcoin:")
}

func TestMoneroRefundSubtractsFee(t *testing.T) {
	ctx := context.Background()
	w, s := newPayMonero(t, payStagenetPrimary, payStagenetSub)
	w.valid[payStagenetSub] = "stagenet"
	p, err := newMoneroProvider(ctx, s.url(""), "", "stagenet", 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.Send(ctx, payStagenetSub, 42); err != nil || strings.Contains(string(s.lastParams("transfer")), "subtract_fee_from_outputs") {
		t.Fatalf("Send (fee on top, unused for payouts) subtracts the fee: %s %v", s.lastParams("transfer"), err)
	}
	if _, err = p.SendFeeFromAmount(ctx, payStagenetSub, 42); err != nil || !strings.Contains(string(s.lastParams("transfer")), `"subtract_fee_from_outputs":[0]`) {
		t.Fatalf("SendFeeFromAmount params %s %v", s.lastParams("transfer"), err)
	}
	var _ feeFromAmountSender = p
	var _ feeFromAmountSender = &bitcoinProvider{}
}

// A restore from backup keeps the large hold behind its own marker: releasing the restored payout still needs
// the review as well as the "no broadcast" wallet check.
func TestRestoreKeepsLargeHold(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	p.setPayoutAddress(p.buyer, "fake-testnet-buyer")
	p.paidByWatcherAmount(350000)
	p.complete(p.order)
	for _, q := range restorePayoutSQL {
		if _, err := p.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if st, errText, _ := p.returnPayout(p.order); st != "held" || errText != restoredHold+" "+largeHold {
		t.Fatalf("restored return %s %q", st, errText)
	}
	id := p.str("SELECT id::text FROM payouts WHERE order_id=$1 AND kind='return'", p.order)
	release := url.Values{"payout_id": {id}, "op": {"release"}, "password": {testPassword}, "not_broadcast": {"confirmed"}}
	if w := p.do("POST", "/admin/payout", p.adminSess, release); w.Code != 409 || !strings.Contains(w.Body.String(), "larger than the order price") {
		t.Fatalf("restored large return released without review: %d", w.Code)
	}
	mustContain(t, p.page("/admin", p.adminSess), `name="large_reviewed" value="confirmed" required`, `name="not_broadcast" value="confirmed" required`)
	release.Set("large_reviewed", "confirmed")
	p.check(p.do("POST", "/admin/payout", p.adminSess, release), 303)
	if st, _, _ := p.returnPayout(p.order); st != "pending" {
		t.Fatalf("state after both checks %s", st)
	}
}
