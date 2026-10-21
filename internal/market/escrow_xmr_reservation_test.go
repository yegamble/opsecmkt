package market

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// A-636: migration 111's reservation of Monero refund and claim addresses per recipient account: concurrent
// reservations, and its backfill of the addresses already in use. The Bitcoin tests once cited for this were removed
// with Bitcoin support (A-565).

// Two releases of one vendor to the same address, in transactions that overlap (migration 111's trigger reserves the
// address for the vendor in each): the second waits on the first's reservation and is refused by
// escrow_external_addresses_pkey once the first commits, and goes through once the first rolls back, since a failed
// claim's reservation rolls back with it. A refund address and a claim address of one account share the reservation.
// DB integration (fake wallet-rpc fixture, no RPC is made).
func TestXMRAddressReservationConcurrent(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	first, _, _ := x.completed()
	second, _, _ := x.completed()
	release := func(tx *sql.Tx, order, dest string) error {
		_, err := tx.Exec("INSERT INTO xmr_settlements(order_id,kind,destination) VALUES($1,'release',$2)", order, dest)
		return err
	}
	race := func(first, second, dest string, commit bool) error {
		t.Helper()
		tx1, err := x.DB.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if err = release(tx1, first, dest); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			tx2, err := x.DB.Begin()
			if err != nil {
				done <- err
				return
			}
			if err = release(tx2, second, dest); err != nil {
				_ = tx2.Rollback()
				done <- err
				return
			}
			done <- tx2.Commit()
		}()
		x.waitForLockWaits(1) // the second waits on the first's reservation
		if commit {
			err = tx1.Commit()
		} else {
			err = tx1.Rollback()
		}
		if err != nil {
			t.Fatal(err)
		}
		return <-done
	}
	reserved := func(dest string) int {
		return agInt(x.testEnv, "SELECT count(*) FROM escrow_external_addresses WHERE user_id=$1 AND coin='XMR' AND address=$2", x.vendor, dest)
	}

	dest := testXMRAddr("reserve "+first, true)
	var pe *pgconn.PgError
	if err := race(first, second, dest, true); !errors.As(err, &pe) || pe.Code != "23505" || pe.ConstraintName != "escrow_external_addresses_pkey" {
		t.Fatalf("the overlapping second release: %v", err)
	}
	if reserved(dest) != 1 || len(x.xmrSettlements(first)) != 1 || len(x.xmrSettlements(second)) != 0 {
		t.Fatalf("after the race: %d reservations, settlements %v and %v", reserved(dest), x.xmrSettlements(first), x.xmrSettlements(second))
	}
	// The vendor's own claim of the second order to that address is refused as used, before the database is reached.
	if code, body := x.xmrClaim(second, x.vendorSess, dest, testPassword); code != 409 || !strings.Contains(body, "already used on this market by your account") {
		t.Fatalf("the vendor's claim to a reserved address: %d %s", code, body)
	}

	third, _, _ := x.completed()
	other := testXMRAddr("reserve rollback "+third, true)
	if err := race(third, second, other, false); err != nil {
		t.Fatalf("the second release once the first rolled back: %v", err)
	}
	if reserved(other) != 1 || len(x.xmrSettlements(third)) != 0 || len(x.xmrSettlements(second)) != 1 {
		t.Fatalf("after the rollback: %d reservations, settlements %v and %v", reserved(other), x.xmrSettlements(third), x.xmrSettlements(second))
	}

	// Concurrent checkouts through the handlers. One buyer's two drafts with the same refund address never both take
	// it: one succeeds and the other is refused as used (409) or as busy on the buyer's row (503, retryable). Two
	// buyers' drafts with the same refund address both succeed: each account's history is its own.
	checkoutPair := func(a, b [2]string, refund string) []int {
		var codes [2]int
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i, c := range [][2]string{a, b} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				codes[i] = x.checkout(c[1], c[0], refund).Code
			}()
		}
		close(start)
		wg.Wait()
		x.mustPoll() // free slots for the next pair
		got := codes[:]
		slices.Sort(got)
		return got
	}
	for i := range 4 {
		buyer, sess := x.user("xreserve_b_"+randomToken()[:6], "buyer")
		x.enableOrderPGP(buyer)
		refund := testXMRAddrOn(x.refundNet(), "same buyer refund "+randomToken(), true)
		a, b := x.order(buyer, x.digital(x.vendor), "XMR", stateDraft), x.order(buyer, x.digital(x.vendor), "XMR", stateDraft) // one draft per listing
		got := checkoutPair([2]string{a, sess}, [2]string{b, sess}, refund)
		t.Logf("one buyer's concurrent checkouts %d: %v", i, got)
		if !slices.Equal(got, []int{303, 409}) && !slices.Equal(got, []int{303, 503}) {
			t.Fatalf("one buyer's concurrent checkouts %d: %v", i, got)
		}
		if n := agInt(x.testEnv, "SELECT count(*) FROM escrows WHERE refund_address=$1", refund); n != 1 {
			t.Fatalf("one buyer's concurrent checkouts %d: %d escrows pay refunds to the address", i, n)
		}
		if n := agInt(x.testEnv, "SELECT count(*) FROM escrow_external_addresses WHERE user_id=$1 AND address=$2", buyer, refund); n != 1 {
			t.Fatalf("one buyer's concurrent checkouts %d: %d reservations", i, n)
		}
		if n := agInt(x.testEnv, "SELECT count(*) FROM orders WHERE id IN ($1,$2) AND state='draft'", a, b); n != 1 {
			t.Fatalf("one buyer's concurrent checkouts %d: %d drafts left, want 1", i, n)
		}

		other, otherSess := x.user("xreserve_o_"+randomToken()[:6], "buyer")
		x.enableOrderPGP(other)
		shared := testXMRAddrOn(x.refundNet(), "shared refund "+randomToken(), true)
		c := x.order(other, x.digital(x.vendor), "XMR", stateDraft)
		d := x.order(buyer, x.digital(x.vendor), "XMR", stateDraft)
		if got := checkoutPair([2]string{c, otherSess}, [2]string{d, sess}, shared); !slices.Equal(got, []int{303, 303}) {
			t.Fatalf("two buyers' concurrent checkouts %d: %v", i, got)
		}
		if n := agInt(x.testEnv, "SELECT count(*) FROM escrow_external_addresses WHERE address=$1", shared); n != 2 {
			t.Fatalf("two buyers' concurrent checkouts %d: %d reservations, want one each", i, n)
		}
	}
}

// Migration 111 on a database that already holds Monero escrows and settlements: it reserves each buyer's refund
// address and each vendor's release destination for that account (a refund settlement to the refund address adds
// nothing), keeping one account's address apart from another's that happens to be the same, and from then on its
// triggers refuse a second use by the same account while another account may use it.
func TestXMRMigration111Backfill(t *testing.T) {
	db := schemaDB(t)
	ctx := context.Background()
	list, err := loadMigrations(migrationFiles)
	if err != nil {
		t.Fatal(err)
	}
	var before111, through111 []migration
	for _, m := range list {
		if m.Version < 111 {
			before111 = append(before111, m)
		}
		if m.Version == 111 {
			through111 = append(through111, m)
		}
	}
	if len(through111) != 1 {
		t.Fatalf("migration 111: %d files", len(through111))
	}
	runInTx(t, db, func(tx *sql.Tx) error {
		if _, err := tx.Exec(schema); err != nil {
			return err
		}
		return applyMigrations(ctx, tx, before111)
	})
	r1, r2, r3, c1 := xmrAddr("refund-1"), xmrAddr("refund-2"), xmrAddr("refund-3"), xmrAddr("claim-1")
	for _, q := range []string{
		"INSERT INTO users(id,handle,password_hash,role) VALUES('ub','b111','x','buyer'),('ub2','b111b','x','buyer'),('uv','v111','x','vendor')",
		"INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock) VALUES('pd','uv','D','D','Hardware','Worldwide','digital',100000,1,5)",
		`INSERT INTO orders(id,buyer_id,product_id,currency,amount,state) VALUES('o1','ub','pd','XMR',1,'completed'),('o2','ub','pd','XMR',1,'cancelled'),
			('o3','ub2','pd','XMR',1,'completed')`,
		`INSERT INTO xmr_escrow_wallets(name,month,primary_address,restore_height,seed_sealed,seed_key_fpr,sealed_at,state)
			VALUES('escrow-'||to_char(now(),'YYYYMM')||'-01',date_trunc('month',now())::date,'` + xmrAddr("wallet-111") + `',1,'sealed seed',repeat('AB',20),now(),'active')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for i, o := range []string{"o1", "o2", "o3"} {
		addr := xmrAddr("slot-111-" + o)
		if _, err := db.Exec("INSERT INTO xmr_escrow_slots(wallet_id,account_index,address) SELECT id,$1,$2 FROM xmr_escrow_wallets", i+1, addr); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("UPDATE xmr_escrow_slots SET state='claimed',order_id=$1,updated=now() WHERE address=$2", o, addr); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("INSERT INTO escrows(order_id,coin,address,refund_address,fee_bps,fee_destination,xmr_slot_id) SELECT $1,'XMR',$2,$3,0,'',id FROM xmr_escrow_slots WHERE address=$2",
			o, addr, []string{r1, r2, r3}[i]); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range []struct{ order, kind, dest string }{
		{"o1", "release", c1}, // the vendor's claim
		{"o2", "refund", r2},  // the buyer's refund to the refund address: already reserved for the buyer
		{"o3", "release", r1}, // the vendor's claim to an address that is also another account's refund address
	} {
		if _, err := db.Exec("INSERT INTO xmr_settlements(order_id,kind,destination) VALUES($1,$2,$3)", s.order, s.kind, s.dest); err != nil {
			t.Fatalf("%v: %v", s, err)
		}
	}
	runInTx(t, db, func(tx *sql.Tx) error { return applyMigrations(ctx, tx, through111) })

	var got []string
	rows, err := db.Query("SELECT user_id||' '||coin||' '||address FROM escrow_external_addresses ORDER BY 1")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		if err = rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		got = append(got, s)
	}
	rows.Close()
	want := []string{"ub XMR " + r1, "ub XMR " + r2, "ub2 XMR " + r3, "uv XMR " + c1, "uv XMR " + r1}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("backfilled reservations:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// From now on: the buyer cannot use the refund address again, another buyer can.
	for _, q := range []string{
		"INSERT INTO orders(id,buyer_id,product_id,currency,amount,state) VALUES('o4','ub','pd','XMR',1,'awaiting_payment'),('o5','ub2','pd','XMR',1,'awaiting_payment')",
		"INSERT INTO xmr_escrow_slots(wallet_id,account_index,address) SELECT id,4,'" + xmrAddr("slot-111-o4") + "' FROM xmr_escrow_wallets",
		"INSERT INTO xmr_escrow_slots(wallet_id,account_index,address) SELECT id,5,'" + xmrAddr("slot-111-o5") + "' FROM xmr_escrow_wallets",
		"UPDATE xmr_escrow_slots SET state='claimed',order_id='o'||account_index,updated=now() WHERE account_index IN (4,5)",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	escrow := func(order string) error {
		_, err := db.Exec("INSERT INTO escrows(order_id,coin,address,refund_address,fee_bps,fee_destination,xmr_slot_id) SELECT $1,'XMR',address,$2,0,'',id FROM xmr_escrow_slots WHERE order_id=$1", order, r1)
		return err
	}
	var pe *pgconn.PgError
	if err := escrow("o4"); !errors.As(err, &pe) || pe.ConstraintName != "escrow_external_addresses_pkey" {
		t.Fatalf("the buyer's second use of a backfilled refund address: %v", err)
	}
	if err := escrow("o5"); err != nil {
		t.Fatalf("another buyer's use of the same address: %v", err)
	}
}
