package market

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// A return needs its release (A-268, migration 073): a return carries the excess of a release back to the buyer, so
// the database refuses, at commit, any order left with a return and no release.

const returnGuard = "payouts_return_needs_release"

// returnGuardRefusal returns the trigger's refusal in err, or nil.
func returnGuardRefusal(err error) *pgconn.PgError {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23514" && pe.ConstraintName == returnGuard {
		return pe
	}
	return nil
}

// refusedFor checks that err is the trigger's refusal naming order (and none of the others), with the fixed text
// pointing to UPGRADING.md and no amount or address.
func refusedFor(t *testing.T, err error, order string, others ...string) {
	t.Helper()
	pe := returnGuardRefusal(err)
	if pe == nil {
		t.Fatalf("not refused by %s: %v", returnGuard, err)
	}
	want := "payouts: order " + order + " has a return but no release. A return sends the excess of a release back to the buyer and is valid only beside the release of the same order, so this transaction was rolled back and nothing was changed. Settle the order as UPGRADING.md describes (A return needs its release, migration 073)."
	if pe.Message != want {
		t.Fatalf("refusal %q, want %q", pe.Message, want)
	}
	for _, o := range others {
		if strings.Contains(pe.Message, o) {
			t.Fatalf("refusal names %s: %q", o, pe.Message)
		}
	}
}

func (p *payEnv) kinds(order string) string {
	p.t.Helper()
	var k string
	if err := p.DB.QueryRow("SELECT coalesce(string_agg(kind, ',' ORDER BY kind), '') FROM payouts WHERE order_id=$1", order).Scan(&k); err != nil {
		p.t.Fatal(err)
	}
	return k
}

// inTx runs stmts (each with the order as $1 where it has a parameter) in one transaction and returns the COMMIT
// error; a statement error fails the test.
func (p *payEnv) inTx(stmts ...string) error {
	p.t.Helper()
	tx, err := p.DB.Begin()
	if err != nil {
		p.t.Fatal(err)
	}
	defer tx.Rollback()
	for _, s := range stmts {
		if _, err = tx.Exec(s); err != nil {
			p.t.Fatalf("%s: %v", s, err)
		}
	}
	return tx.Commit()
}

func payoutInsert(order, kind, recipient string) string {
	return fmt.Sprintf("INSERT INTO payouts(order_id,kind,user_id,currency,amount,address) VALUES('%s','%s','%s','BTC',100000,'fake-testnet-addr')", order, kind, recipient)
}

func TestReturnNeedsRelease(t *testing.T) {
	p := newPayEnv(t)
	// A return alone, in one transaction: the statement is accepted (the check is deferred) and COMMIT refuses it.
	lone, _ := p.newOrder(stateCompleted)
	err := p.inTx(payoutInsert(lone, "return", p.buyer.ID))
	refusedFor(t, err, lone)
	if pe := returnGuardRefusal(err); strings.Contains(pe.Message, "100000") || strings.Contains(pe.Message, "0.001") || strings.Contains(pe.Message, "fake-testnet") {
		t.Fatalf("refusal carries an amount or address: %q", pe.Message)
	}
	// So is one outside an explicit transaction.
	_, err = p.DB.Exec(payoutInsert(lone, "return", p.buyer.ID))
	refusedFor(t, err, lone)
	if n := p.count("SELECT count(*) FROM payouts WHERE order_id=$1", lone); n != 0 {
		t.Fatalf("a refused return left %d payouts", n)
	}

	// A release and its return in one transaction, in either order, as enqueuePayout (release first) or by hand.
	returnFirst, _ := p.newOrder(stateCompleted)
	if err = p.inTx(payoutInsert(returnFirst, "return", p.buyer.ID), payoutInsert(returnFirst, "release", p.vendor.ID)); err != nil {
		t.Fatalf("return then release: %v", err)
	}
	releaseFirst, _ := p.newOrder(stateCompleted)
	if err = p.inTx(payoutInsert(releaseFirst, "release", p.vendor.ID), payoutInsert(releaseFirst, "return", p.buyer.ID)); err != nil {
		t.Fatalf("release then return: %v", err)
	}
	// A return added later beside an existing release is accepted too.
	later, _ := p.newOrder(stateCompleted)
	p.insertPayouts(later, "release")
	if _, err = p.DB.Exec(payoutInsert(later, "return", p.buyer.ID)); err != nil {
		t.Fatalf("return beside a release: %v", err)
	}

	// Deleting a release while its return remains is refused, and nothing is deleted.
	_, err = p.DB.Exec("DELETE FROM payouts WHERE order_id=$1 AND kind='release'", returnFirst)
	refusedFor(t, err, returnFirst)
	if k := p.kinds(returnFirst); k != "release,return" {
		t.Fatalf("refused delete left %q", k)
	}

	// Changing a release's kind while its return remains is refused. Every other kind already clashes with an index
	// (a second return with 064's key, a refund beside the return with 070's), so the index for a refund beside a
	// return is dropped inside the transaction (and restored by its rollback) to reach the trigger.
	var pe *pgconn.PgError
	if _, err = p.DB.Exec("UPDATE payouts SET kind='refund' WHERE order_id=$1 AND kind='release'", releaseFirst); !errors.As(err, &pe) || pe.ConstraintName != "payouts_refund_or_return" {
		t.Fatalf("release turned refund beside its return: %v", err)
	}
	err = p.inTx("DROP INDEX payouts_refund_or_return",
		fmt.Sprintf("UPDATE payouts SET kind='refund' WHERE order_id='%s' AND kind='release'", releaseFirst))
	refusedFor(t, err, releaseFirst)
	if k := p.kinds(releaseFirst); k != "release,return" || p.count("SELECT count(*) FROM pg_indexes WHERE schemaname=current_schema() AND indexname='payouts_refund_or_return'") != 1 {
		t.Fatalf("refused kind change left %q or dropped the index", k)
	}

	// Moving a release to another order leaves its return alone: refused, naming the order it left, not the order
	// it moved to. Moving a return to an order without a release is refused naming that order.
	empty, _ := p.newOrder(stateCompleted)
	_, err = p.DB.Exec("UPDATE payouts SET order_id=$2 WHERE order_id=$1 AND kind='release'", releaseFirst, empty)
	refusedFor(t, err, releaseFirst, empty)
	_, err = p.DB.Exec("UPDATE payouts SET order_id=$2 WHERE order_id=$1 AND kind='return'", releaseFirst, empty)
	refusedFor(t, err, empty, releaseFirst)
	if p.kinds(releaseFirst) != "release,return" || p.kinds(empty) != "" {
		t.Fatalf("refused moves left %q and %q", p.kinds(releaseFirst), p.kinds(empty))
	}
	// A statement touching several orders names the one it leaves with a return and no release, not the valid order
	// it also touches.
	alone, _ := p.newOrder(stateCompleted)
	p.insertPayouts(alone, "release")
	_, err = p.DB.Exec("DELETE FROM payouts WHERE order_id=ANY($1) AND kind='release'", []string{alone, later})
	refusedFor(t, err, later, alone)
	if p.kinds(alone) != "release" || p.kinds(later) != "release,return" {
		t.Fatalf("refused multi-order delete left %q and %q", p.kinds(alone), p.kinds(later))
	}

	// A return deleted alone, or a whole settlement deleted together, is accepted.
	if _, err = p.DB.Exec("DELETE FROM payouts WHERE order_id=$1 AND kind='return'", later); err != nil {
		t.Fatalf("return deleted alone: %v", err)
	}
	if _, err = p.DB.Exec("DELETE FROM payouts WHERE order_id=$1", returnFirst); err != nil {
		t.Fatalf("release and return deleted together: %v", err)
	}
	if p.kinds(later) != "release" || p.kinds(returnFirst) != "" {
		t.Fatalf("after the deletes: %q %q", p.kinds(later), p.kinds(returnFirst))
	}
}

// The commit-time check cannot be raced: two transactions, one adding a return beside a release and the other
// deleting that release, would each pass a check that does not see the other's uncommitted row. The trigger takes an
// advisory lock before checking and keeps it until its transaction ends, so the second waits for the first to commit
// and then sees its row.
func TestReturnGuardSerializesConcurrentCommits(t *testing.T) {
	p := newPayEnv(t)
	order, _ := p.newOrder(stateCompleted)
	p.insertPayouts(order, "release")
	ctx := context.Background()
	adder, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer adder.Rollback()
	// IMMEDIATE runs the adder's check at once (it sees the release) and keeps the lock until the adder ends.
	if _, err = adder.Exec("SET CONSTRAINTS " + returnGuard + " IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	if _, err = adder.Exec(payoutInsert(order, "return", p.buyer.ID)); err != nil {
		t.Fatal(err)
	}
	deleter, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer deleter.Rollback()
	var pid int
	if err = deleter.QueryRow("SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err = deleter.Exec("DELETE FROM payouts WHERE order_id=$1 AND kind='release'", order); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- deleter.Commit() }()
	deadline := time.Now().Add(15 * time.Second)
	for p.count("SELECT count(*) FROM pg_stat_activity WHERE pid=$1 AND wait_event_type='Lock' AND wait_event='advisory'", pid) != 1 {
		select {
		case err = <-done:
			t.Fatalf("the delete committed without waiting for the return's transaction: %v (kinds now %q)", err, p.kinds(order))
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the delete's commit never waited on the guard's advisory lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err = adder.Commit(); err != nil {
		t.Fatalf("the return's transaction: %v", err)
	}
	refusedFor(t, <-done, order)
	if k := p.kinds(order); k != "release,return" {
		t.Fatalf("after the race %q", k)
	}
}

// unguard removes migration 073 (its trigger, function and record) so a test can insert the returns it refuses, and
// returns a function applying it in one transaction as a startup does, after running pre (with args) in that
// transaction; a refused migration rolls everything back.
func (p *payEnv) unguard() func(pre string, args ...any) error {
	p.t.Helper()
	if _, err := p.DB.Exec("DROP TRIGGER " + returnGuard + " ON payouts; DROP FUNCTION " + returnGuard + "(); DELETE FROM schema_migrations WHERE version=73"); err != nil {
		p.t.Fatal(err)
	}
	return p.applyVersions(73)
}

// applyVersions returns a function applying the given embedded migrations in one transaction, after pre.
func (p *payEnv) applyVersions(versions ...int) func(pre string, args ...any) error {
	p.t.Helper()
	list, err := loadMigrations(migrationFiles)
	if err != nil {
		p.t.Fatal(err)
	}
	var chosen []migration
	for _, m := range list {
		if slices.Contains(versions, m.Version) {
			chosen = append(chosen, m)
		}
	}
	if len(chosen) != len(versions) {
		p.t.Fatalf("migrations %v found %+v", versions, chosen)
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
		if err = applyMigrations(context.Background(), tx, chosen); err != nil {
			return err
		}
		return tx.Commit()
	}
}

// returnGuardApplied counts migration 073's trigger, function and record in the test's schema: 3 when applied.
func (p *payEnv) returnGuardApplied() int {
	p.t.Helper()
	return p.count(`SELECT count(*) FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname=current_schema() AND c.relname='payouts' AND t.tgname=$1 AND t.tgdeferrable AND t.tginitdeferred`, returnGuard) +
		p.count("SELECT count(*) FROM pg_proc f JOIN pg_namespace n ON n.oid=f.pronamespace WHERE n.nspname=current_schema() AND f.proname=$1", returnGuard) +
		p.count("SELECT count(*) FROM schema_migrations WHERE version=73")
}

// returnWithoutReleaseSQL is the SQL UPGRADING.md section 18 gives for settling an order migration 073 refuses:
// list the payouts of every order with a return and no release, restore its release held for an administrator,
// record a release paid outside the market as sent, record a sent return of a cancelled or refund-outcome order as
// its refund, delete an unsent return, and check that no order is left with a return and no release.
// TestReturnWithoutReleaseGuide runs it with psql's variables bound as $1 (order_id), $2 (address), $3 (txid) and
// checks that UPGRADING.md carries it verbatim (whitespace aside).
var returnWithoutReleaseSQL = struct{ list, restore, record, relabel, drop, check string }{
	list: `SELECT p.id, p.order_id, o.state AS order_state, d.outcome AS dispute_outcome, o.amount AS price, p.kind, p.state, p.send_ambiguous,
  p.currency, p.amount, p.address, p.user_id, p.txid, p.error, p.created, p.updated
FROM payouts p JOIN orders o ON o.id = p.order_id LEFT JOIN disputes d ON d.order_id = p.order_id
WHERE p.order_id IN (SELECT order_id FROM payouts GROUP BY order_id
  HAVING count(*) FILTER (WHERE kind = 'return') > 0 AND count(*) FILTER (WHERE kind = 'release') = 0)
ORDER BY p.order_id, p.kind;`,
	restore: `INSERT INTO payouts(order_id, kind, user_id, currency, amount, address, state, error)
SELECT o.id, 'release', p.vendor_id, o.currency, o.amount, CASE o.currency WHEN 'XMR' THEN v.payout_xmr ELSE v.payout_btc END, 'held',
  'Restored by hand (UPGRADING.md section 18): check the wallet before releasing it.'
FROM orders o JOIN products p ON p.id = o.product_id JOIN users v ON v.id = p.vendor_id
WHERE o.id = :'order_id' AND EXISTS (SELECT 1 FROM payouts r WHERE r.order_id = o.id AND r.kind = 'return')
RETURNING id, kind, state, amount, address;`,
	record: `INSERT INTO payouts(order_id, kind, user_id, currency, amount, address, state, txid)
SELECT o.id, 'release', p.vendor_id, o.currency, o.amount, :'address', 'sent', :'txid'
FROM orders o JOIN products p ON p.id = o.product_id
WHERE o.id = :'order_id' AND EXISTS (SELECT 1 FROM payouts r WHERE r.order_id = o.id AND r.kind = 'return')
RETURNING id, kind, state, amount, address, txid;`,
	relabel: `UPDATE payouts SET kind = 'refund', updated = now() WHERE order_id = :'order_id' AND kind = 'return'
  AND NOT EXISTS (SELECT 1 FROM payouts l WHERE l.order_id = payouts.order_id AND l.kind = 'release')
RETURNING id, kind, state, amount, txid;`,
	drop: `DELETE FROM payouts WHERE order_id = :'order_id' AND kind = 'return' AND state <> 'sent'
  AND NOT EXISTS (SELECT 1 FROM payouts l WHERE l.order_id = payouts.order_id AND l.kind = 'release')
RETURNING id, kind, state, txid;`,
	check: `SELECT order_id FROM payouts GROUP BY order_id
HAVING count(*) FILTER (WHERE kind = 'return') > 0 AND count(*) FILTER (WHERE kind = 'release') = 0;`,
}

// A database that already holds a return without a release (possible only by hand, for example by settling a 069
// clash as UPGRADING.md section 14 said before migration 073) stops migration 073 with an error naming exactly those
// orders, and nothing is changed or deleted. UPGRADING.md section 18's SQL lists them, settles each (a completed order
// gets its release back, held for an administrator or recorded as paid; a cancelled order's sent return becomes its
// refund, an unsent one is deleted) and its check then finds nothing, after which the migration applies.
func TestReturnWithoutReleaseGuide(t *testing.T) {
	p := newPayEnv(t)
	p.setPayoutAddress(p.vendor, "fake-testnet-vendor")
	p.setPayoutAddress(p.buyer, "fake-testnet-buyer")
	apply := p.unguard()
	if p.returnGuardApplied() != 0 {
		t.Fatal("unguard left the trigger, its function or its record")
	}
	order := func(state string) string { id, _ := p.newOrder(state); return id }
	owed, paid := order(stateCompleted), order(stateCompleted) // the vendor is owed the price / was paid it by hand
	cancelledSent, cancelledUnsent := order(stateCancelled), order(stateCancelled)
	clean, releaseOnly, refundOnly := order(stateCompleted), order(stateCompleted), order(stateCancelled)
	for o, kinds := range map[string][]string{owed: {"return"}, paid: {"return"}, cancelledSent: {"return"}, cancelledUnsent: {"return"},
		clean: {"release", "return"}, releaseOnly: {"release"}, refundOnly: {"refund"}} {
		p.insertPayouts(o, kinds...)
	}
	if _, err := p.DB.Exec("UPDATE payouts SET state='sent', txid='tx-return' WHERE kind='return' AND order_id=ANY($1)", []string{paid, cancelledSent}); err != nil {
		t.Fatal(err)
	}
	total := p.count("SELECT count(*) FROM payouts")
	orphans := []string{owed, paid, cancelledSent, cancelledUnsent}
	sort.Strings(orphans)

	err := apply("")
	if err == nil || !strings.Contains(err.Error(), "migration 073_payout_return_needs_release") {
		t.Fatalf("migration over a return without a release: %v", err)
	}
	m := regexp.MustCompile(`payouts: order\(s\) (.+?) have a return but no release\. Nothing was changed\. Check in the wallet whether each return was sent and settle each order by hand as UPGRADING\.md describes \(A return needs its release, migration 073\), then start the server again\.`).FindStringSubmatch(err.Error())
	if m == nil {
		t.Fatalf("refusal text: %v", err)
	}
	named := strings.Split(m[1], ", ")
	sort.Strings(named)
	if !slices.Equal(named, orphans) {
		t.Fatalf("073 names %v, want %v", named, orphans)
	}
	if p.count("SELECT count(*) FROM payouts") != total || p.returnGuardApplied() != 0 {
		t.Fatal("a refused migration changed payouts or left its trigger, function or record")
	}

	// Step 1 lists exactly the payouts of those orders, with what the wallet check and the decision need.
	rows, err := p.DB.Query(returnWithoutReleaseSQL.list)
	if err != nil {
		t.Fatal(err)
	}
	cols, _ := rows.Columns()
	for _, c := range []string{"id", "order_id", "order_state", "dispute_outcome", "price", "kind", "state", "send_ambiguous", "currency", "amount", "address", "user_id", "txid", "error", "created", "updated"} {
		if !slices.Contains(cols, c) {
			t.Fatalf("listing columns %v lack %s", cols, c)
		}
	}
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
		listed = append(listed, vals[1].(string)+":"+vals[5].(string))
	}
	rows.Close()
	sort.Strings(listed)
	var want []string
	for _, o := range orphans {
		want = append(want, o+":return")
	}
	if !slices.Equal(listed, want) {
		t.Fatalf("listing %v, want %v", listed, want)
	}
	checked := func() []string {
		t.Helper()
		rows, err := p.DB.Query(returnWithoutReleaseSQL.check)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var o string
			if err = rows.Scan(&o); err != nil {
				t.Fatal(err)
			}
			got = append(got, o)
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		sort.Strings(got)
		return got
	}
	if got := checked(); !slices.Equal(got, orphans) {
		t.Fatalf("check lists %v, want %v", got, orphans)
	}

	run := func(stmt string, want int, args ...any) {
		t.Helper()
		stmt = strings.NewReplacer(":'order_id'", "$1", ":'address'", "$2", ":'txid'", "$3").Replace(stmt)
		res, err := p.DB.Exec(stmt, args...)
		if err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
		if n, _ := res.RowsAffected(); n != int64(want) {
			t.Fatalf("%s changed %d payouts, want %d", stmt, n, want)
		}
	}
	// Step 3. The statements refuse the wrong shape: restoring or recording a release for an order without a return,
	// or deleting a sent return, changes nothing.
	run(returnWithoutReleaseSQL.restore, 0, refundOnly)
	run(returnWithoutReleaseSQL.record, 0, refundOnly, "fake-testnet-vendor", "tx-release-by-hand")
	run(returnWithoutReleaseSQL.drop, 0, cancelledSent)
	run(returnWithoutReleaseSQL.drop, 0, clean) // a return beside its release is never touched
	run(returnWithoutReleaseSQL.relabel, 0, clean)
	run(returnWithoutReleaseSQL.restore, 1, owed)
	run(returnWithoutReleaseSQL.record, 1, paid, "fake-testnet-vendor-old", "tx-release-by-hand")
	run(returnWithoutReleaseSQL.relabel, 1, cancelledSent)
	run(returnWithoutReleaseSQL.drop, 1, cancelledUnsent)
	if got := checked(); len(got) != 0 {
		t.Fatalf("check lists %v after the guide", got)
	}
	if err = apply(""); err != nil {
		t.Fatal(err)
	}
	if p.returnGuardApplied() != 3 {
		t.Fatal("migration 073 did not apply once the orders were settled")
	}
	type row struct {
		kind, user, state, address, txid, errText string
		amount                                    int64
	}
	payouts := func(o string) []row {
		t.Helper()
		rows, err := p.DB.Query("SELECT kind,user_id,state,address,txid,error,amount FROM payouts WHERE order_id=$1 ORDER BY kind", o)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []row
		for rows.Next() {
			var r row
			if err = rows.Scan(&r.kind, &r.user, &r.state, &r.address, &r.txid, &r.errText, &r.amount); err != nil {
				t.Fatal(err)
			}
			out = append(out, r)
		}
		return out
	}
	restoredHold := "Restored by hand (UPGRADING.md section 18): check the wallet before releasing it."
	if got := payouts(owed); len(got) != 2 || got[0] != (row{"release", p.vendor.ID, "held", "fake-testnet-vendor", "", restoredHold, 100000}) || got[1].kind != "return" || got[1].state != "pending" {
		t.Fatalf("owed order: %+v", got)
	}
	if got := payouts(paid); len(got) != 2 || got[0] != (row{"release", p.vendor.ID, "sent", "fake-testnet-vendor-old", "tx-release-by-hand", "", 100000}) || got[1].kind != "return" || got[1].state != "sent" {
		t.Fatalf("paid order: %+v", got)
	}
	if got := payouts(cancelledSent); len(got) != 1 || got[0].kind != "refund" || got[0].state != "sent" || got[0].txid != "tx-return" || got[0].user != p.buyer.ID {
		t.Fatalf("cancelled order whose return was sent: %+v", got)
	}
	if p.kinds(cancelledUnsent) != "" || p.kinds(clean) != "release,return" || p.kinds(releaseOnly) != "release" || p.kinds(refundOnly) != "refund" {
		t.Fatalf("after the guide: %q %q %q %q", p.kinds(cancelledUnsent), p.kinds(clean), p.kinds(releaseOnly), p.kinds(refundOnly))
	}
	// The restored release is released on /admin like any held payout, and the watcher sends it once.
	id := p.str("SELECT id::text FROM payouts WHERE order_id=$1 AND kind='release'", owed)
	p.check(p.do("POST", "/admin/payout", p.adminSess, url.Values{"payout_id": {id}, "op": {"release"}, "password": {testPassword}}), 303)
	p.poll()
	if st, txid, _, _ := p.payout(owed); st != "sent" || txid == "" {
		t.Fatalf("restored release after the administrator released it: %s %q", st, txid)
	}
	// The other rows here were inserted without an address, so the restored release is the only send.
	if s := p.fake.Sends(); len(s) != 1 || s[0].To != "fake-testnet-vendor" || s[0].Amount != 100000 {
		t.Fatalf("sends %+v", s)
	}

	doc, err := os.ReadFile("UPGRADING.md")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(string(doc)), " ")
	for _, q := range []string{returnWithoutReleaseSQL.list, returnWithoutReleaseSQL.restore, returnWithoutReleaseSQL.record, returnWithoutReleaseSQL.relabel, returnWithoutReleaseSQL.drop, returnWithoutReleaseSQL.check} {
		if q = strings.Join(strings.Fields(q), " "); !strings.Contains(text, q) {
			t.Fatalf("UPGRADING.md does not carry the section 18 SQL %q", q)
		}
	}
	for _, s := range []string{"## 18. A return needs its release (migration 073, A-268)"} {
		if !strings.Contains(string(doc), s) {
			t.Fatalf("UPGRADING.md lacks %q", s)
		}
	}
}
