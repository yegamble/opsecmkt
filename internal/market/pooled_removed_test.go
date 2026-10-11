package market

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// A-492: pooled payments are removed (owner directive 2026-10-11). No non-test Go source or template may write the
// pooled tables (migration 108 refuses their rows), call a pooled wallet RPC, or bring back a pooled identifier or
// function. The only exceptions are config.go's refusal of the obsolete pooled settings (refuseObsoletePooledEnv) and
// the immutable numbered migrations, which are SQL and not scanned.
var pooledRemovedPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(INSERT\s+INTO|UPDATE)\s+(payouts|payment_addresses)\b`),
	regexp.MustCompile(`\b(sendtoaddress|getnewaddress|create_address|sendmany)\b`),
	regexp.MustCompile(`Pooled[A-Z]`),
	regexp.MustCompile(`\b(enqueuePayout|sendPayouts|settleOrder|payAction)\b`),
	// A-508: the pooled payout preview (a release/return split with an administrator's review and a minimum automatic
	// payout, paid to the saved payout address) and every reader of the saved payout addresses. Escrow settlements pay
	// the claim and refund addresses (escrowExcess, escrowDisputePreview); users.payout_btc and payout_xmr stay as
	// columns only (migrations are immutable).
	regexp.MustCompile(`\b(orderPayouts|payoutStatement|splitPayout|payoutPart|subFloorPriceExcess|countedDeposits|OverpaidView|PayoutView|NoAddress)\b|\bbelowFloor\(`),
	regexp.MustCompile(`\bpayout_(btc|xmr)\b`),
	// QA59b: pooled readers A-492 deleted (pooledOnly is now buyerCompleteRefused, payoutBasis countedDeposits, itself
	// deleted by A-508).
	regexp.MustCompile(`\b(paymentsOrderLoader|unsentPayouts|unsentPayoutsLoader|payoutStateLabel|orderPayoutLabel|recipientPayout|PayoutRow|payoutBasis|pooledOnly|hidePooled)\b`),
}

// pooledRemovedAllowed: the one pattern a file may match, by path.
var pooledRemovedAllowed = map[string]string{"internal/market/config.go": `Pooled[A-Z]`}

// pooledRemovedFindings reports each line of files (path to content) that matches a pattern its path is not allowed.
func pooledRemovedFindings(files map[string]string) []string {
	var found []string
	for path, text := range files {
		for n, line := range strings.Split(text, "\n") {
			for _, p := range pooledRemovedPatterns {
				if pooledRemovedAllowed[path] != p.String() && p.MatchString(line) {
					found = append(found, path+":"+strconv.Itoa(n+1)+": "+p.FindString(line))
				}
			}
		}
	}
	return found
}

// pooledRemovedSources reads every non-test Go file under cmd and internal (migrations excluded) and every template.
func pooledRemovedSources(t *testing.T) map[string]string {
	t.Helper()
	files := map[string]string{}
	for _, root := range []string{"cmd", "internal", "web/templates"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if path == filepath.Join("internal", "market", "migrations") || d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			goSource := strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go")
			if !goSource && !strings.HasSuffix(path, ".html") {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			files[filepath.ToSlash(path)] = string(b)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(files) < 100 || files["internal/market/config.go"] == "" || files["web/templates/layout.html"] == "" {
		t.Fatalf("scanned %d files; the walk missed the sources", len(files))
	}
	return files
}

func TestPooledPaymentsStayRemoved(t *testing.T) {
	for _, f := range pooledRemovedFindings(pooledRemovedSources(t)) {
		t.Errorf("pooled payments are removed (A-492); found %s", f)
	}
}

// The guard itself: each forbidden form is caught in any scanned file, and config.go may name only its refusal.
func TestPooledRemovedGuardCatchesEachPattern(t *testing.T) {
	for _, line := range []string{
		`tx.Exec("INSERT INTO payouts(order_id) VALUES($1)")`,
		`tx.Exec("update payouts SET state='sent'")`,
		`q := "INSERT  INTO payment_addresses(order_id) VALUES($1)"`,
		`tx.Exec("UPDATE payment_addresses SET created=now()")`,
		`c.call(ctx, "sendtoaddress", addr, amt)`,
		`c.call(ctx, "getnewaddress")`,
		`w.call(ctx, "create_address", nil)`,
		`c.call(ctx, "sendmany", "", outs)`,
		`d.PooledDrain = v`,
		`{{with .PooledDrain}}`,
		`a.enqueuePayout(ctx, tx, o)`,
		`a.sendPayouts(ctx)`,
		`a.settleOrder(ctx, o)`,
		`registerAction("/orders/pay", actionSpec{Run: payAction})`,
		`parts := splitPayout("release", o, sum, required)`,
		`Confirm: payoutStatement("refund", o.Buyer, "the buyer", a.netName(), o, sum)`,
		`if err = a.orderPayouts(ctx, d); err != nil {`,
		`sum, err := countedDeposits(ctx, a.db, o)`,
		`v.Held, v.Reason = true, subFloorPriceExcess(o.Currency)`,
		`if below := belowFloor(o.Currency, kind, sum); below != "" {`,
		`Overpaid *OverpaidView`,
		`{{if .NoAddress}}`,
		`d.Payout = &PayoutView{}`,
		`a.db.QueryRowContext(ctx, "SELECT payout_btc,payout_xmr FROM users WHERE id=$1", id)`,
		`column := "payout_xmr"`,
		`registerLoader("order", paymentsOrderLoader)`,
		`rows, err := unsentPayouts(ctx, a.db, u.ID)`,
		`registerLoader("account", unsentPayoutsLoader)`,
		`v.State = payoutStateLabel(p.State)`,
		`{{orderPayoutLabel .}}`,
		`p := recipientPayout(o)`,
		`var rows []PayoutRow`,
		`sum, err := payoutBasis(ctx, tx, o)`,
		`if pooledOnly[[3]string{o.State, to, r}] {`,
		`if hidePooled(ctx, a, o) {`,
	} {
		for _, path := range []string{"internal/market/payments.go", "web/templates/pages/admin.html", "internal/market/config.go"} {
			got := pooledRemovedFindings(map[string]string{path: "ok\n" + line})
			if path == "internal/market/config.go" && regexp.MustCompile(pooledRemovedAllowed[path]).MatchString(line) {
				if len(got) != 0 { // config.go's one allowed pattern
					t.Errorf("%s in %s: %v, want none", line, path, got)
				}
			} else if len(got) != 1 || !strings.HasPrefix(got[0], path+":2: ") {
				t.Errorf("%s in %s: %v, want one finding on line 2", line, path, got)
			}
		}
	}
	for _, clean := range []string{
		`if err := refuseObsoletePooledEnv(getenv); err != nil {`, // config.go only
		`a.xmrSettleOrders(ctx, read, touched)`,
		`SELECT count(*) FROM payouts`,
		`UPDATE payments SET flagged=true`,
		`errShipPlainForm`,
		`var credited, locked, regressed, belowFloor, escrow, outside, counted, leftOut bool`,
		`ADD COLUMN IF NOT EXISTS payout_btc_changed timestamptz`,
		`back := a.escrowExcess(o, sum-left)`,
		`floor := payoutFloor("XMR", s.Kind)`,
	} {
		path := "internal/market/escrow_watch.go"
		if strings.Contains(clean, "refuseObsoletePooledEnv") {
			path = "internal/market/config.go"
		}
		if got := pooledRemovedFindings(map[string]string{path: clean}); len(got) != 0 {
			t.Errorf("%q in %s reported %v", clean, path, got)
		}
	}
}
