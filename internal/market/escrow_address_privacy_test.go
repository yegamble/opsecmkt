package market

import (
	"context"
	"database/sql"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// A checkout must not reveal another account's external address history, or let
// the first account to paste an address prevent its owner from using it.
func TestEscrowExternalRefundAddressIsPrivatePerAccount(t *testing.T) {
	t.Run("Monero", func(t *testing.T) {
		x := newXMREscrowEnv(t)
		x.mustPoll()
		addr := testXMRAddr("private external refund", true)
		victim := x.draft(x.digital(x.vendor))
		if w := x.checkout(x.buyerSess, victim, addr); w.Code != 303 {
			t.Fatalf("first checkout: %d %s", w.Code, w.Body.String())
		}
		other, sess := x.user("address_privacy_"+randomToken()[:6], "buyer")
		order := x.order(other, x.digital(x.vendor), "XMR", stateDraft)
		if w := x.do("GET", "/order?id="+victim, sess, nil); w.Code != 404 {
			t.Fatalf("unrelated order read: %d", w.Code)
		}
		if w := x.checkout(sess, order, addr); w.Code != 303 {
			t.Fatalf("another account's address history disclosed: %d %s", w.Code, w.Body.String())
		}
		if w := x.checkout(sess, x.order(other, x.digital(x.vendor), "XMR", stateDraft), addr); w.Code != 409 {
			t.Fatalf("own address reuse: %d", w.Code)
		}
	})
	t.Run("Bitcoin", func(t *testing.T) {
		x := newEscrowEnv(t)
		addr := regtestAddr("private external refund")
		victim := x.draft()
		x.mustCheckout(victim, addr)
		other, sess := x.user("address_privacy_"+randomToken()[:6], "buyer")
		x.activeKey(other, altEscrowKey(t, testBIP48Key, "0dd0dd0d", 3))
		order := x.testEnv.order(other, x.autoListing(x.vendor), "BTC", stateDraft)
		if w := x.do("GET", "/order?id="+victim, sess, nil); w.Code != 404 {
			t.Fatalf("unrelated order read: %d", w.Code)
		}
		post := func(order string) int {
			return x.do("POST", "/orders/escrow", sess, url.Values{"order_id": {order}, "refund_address": {addr}}).Code
		}
		if code := post(order); code != 303 {
			t.Fatalf("another account's address history disclosed: %d", code)
		}
		if code := post(x.testEnv.order(other, x.autoListing(x.vendor), "BTC", stateDraft)); code != 409 {
			t.Fatalf("own address reuse: %d", code)
		}
	})
}

func TestEscrowExternalAddressReservationsAtomic(t *testing.T) {
	e := newTestApp(t)
	owner, _ := e.user("address_owner", "vendor")
	other, _ := e.user("address_other", "buyer")
	seller, _ := e.user("address_seller", "vendor")
	product := e.product(seller, "digital")
	addr := regtestAddr("atomic external refund")
	insert := `INSERT INTO escrows(order_id,address,market_index,refund_address,fee_bps,fee_destination)
		VALUES($1,$2,nextval('escrow_market_index'),$3,0,'')`
	orders := []string{e.order(owner, product, "BTC", stateAwaitingPayment), e.order(owner, product, "BTC", stateAwaitingPayment)}
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range orders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = e.DB.Exec(insert, orders[i], regtestAddr(orders[i]), addr)
		}()
	}
	wg.Wait()
	success, refused := 0, 0
	for _, err := range errs {
		if err == nil {
			success++
			continue
		}
		pe, ok := err.(*pgconn.PgError)
		if !ok || pe.Code != "23505" || pe.ConstraintName != "escrow_external_addresses_pkey" {
			t.Fatalf("unexpected reservation failure: %v", err)
		}
		refused++
	}
	if success != 1 || refused != 1 {
		t.Fatalf("same-account reservation race: %v", errs)
	}
	order := e.order(other, product, "BTC", stateAwaitingPayment)
	if _, err := e.DB.Exec(insert, order, regtestAddr(order), addr); err != nil {
		t.Fatalf("different account poisoned by first reservation: %v", err)
	}
	// An account selling elsewhere cannot reuse its own buyer refund as a claim.
	claimOrder := e.order(other, e.product(owner, "digital"), "BTC", stateCompleted)
	if _, err := e.DB.Exec("INSERT INTO settlements(order_id,kind,destination) VALUES($1,'release',$2)", claimOrder, addr); err == nil {
		t.Fatal("same-account cross-role address reuse accepted")
	}
	otherSeller, _ := e.user("address_other_seller", "vendor")
	otherClaim := e.order(owner, e.product(otherSeller, "digital"), "BTC", stateCompleted)
	if _, err := e.DB.Exec("INSERT INTO settlements(order_id,kind,destination) VALUES($1,'release',$2)", otherClaim, addr); err != nil {
		t.Fatalf("different vendor's external destination blocked: %v", err)
	}
	// Reservations belong to the same transaction as the financial record.
	rollbackAddr := regtestAddr("rolled-back external refund")
	rollbackOrder := e.order(owner, product, "BTC", stateAwaitingPayment)
	tx, err := e.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(insert, rollbackOrder, regtestAddr(rollbackOrder), rollbackAddr); err != nil {
		t.Fatal(err)
	}
	tx.Rollback()
	if n := agInt(e, "SELECT count(*) FROM escrow_external_addresses WHERE address=$1", rollbackAddr); n != 0 {
		t.Fatal("failed transaction retained an address reservation")
	}
}

func TestEscrowExternalAddressMigration111BackfillsRecipients(t *testing.T) {
	db := schemaDB(t)
	list, err := loadMigrations(migrationFiles)
	if err != nil {
		t.Fatal(err)
	}
	var before, after []migration
	for _, m := range list {
		if m.Version < 111 {
			before = append(before, m)
		} else {
			after = append(after, m)
		}
	}
	runInTx(t, db, func(tx *sql.Tx) error {
		if _, err := tx.Exec(schema); err != nil {
			return err
		}
		return applyMigrations(context.Background(), tx, before)
	})
	e := &testEnv{t: t, DB: db}
	buyer, _ := e.user("address_upgrade_buyer", "buyer")
	vendor, _ := e.user("address_upgrade_vendor", "vendor")
	staff, _ := e.user("address_upgrade_staff", "admin")
	product := e.product(vendor, "digital")
	btc := e.order(buyer, product, "BTC", stateCompleted)
	btcRefund, btcClaim := regtestAddr("upgrade refund"), regtestAddr("upgrade claim")
	agExec(e, "INSERT INTO escrows(order_id,address,market_index,refund_address,fee_bps,fee_destination) VALUES($1,$2,nextval('escrow_market_index'),$3,0,'')", btc, regtestAddr(btc), btcRefund)
	agExec(e, "INSERT INTO settlements(order_id,kind,destination,requested_by) VALUES($1,'release',$2,$3)", btc, btcClaim, staff)
	xmr, _, _ := xmrEscrow(t, e, xmrWallet(t, e, 1, "active"), 1, buyer, product, stateCompleted)
	xmrClaim := xmrAddr("upgrade claim")
	agExec(e, "INSERT INTO xmr_settlements(order_id,kind,destination,requested_by) VALUES($1,'release',$2,$3)", xmr, xmrClaim, staff)
	runInTx(t, db, func(tx *sql.Tx) error { return applyMigrations(context.Background(), tx, after) })
	for _, tc := range []struct{ owner, coin, address string }{
		{buyer, "BTC", btcRefund}, {vendor, "BTC", btcClaim},
		{buyer, "XMR", xmrAddr("refund-" + xmr)}, {vendor, "XMR", xmrClaim},
	} {
		if n := agInt(e, "SELECT count(*) FROM escrow_external_addresses WHERE user_id=$1 AND coin=$2 AND address=$3", tc.owner, tc.coin, tc.address); n != 1 {
			t.Fatalf("missing recipient reservation: %+v", tc)
		}
	}
	if n := agInt(e, "SELECT count(*) FROM escrow_external_addresses WHERE user_id=$1", staff); n != 0 {
		t.Fatal("staff requester incorrectly owns a financial recipient's address")
	}
}

// Older migration tests explicitly reconstruct the pre-upgrade schema.
func restoreSchema110(t *testing.T, e *testEnv) {
	t.Helper()
	if _, err := e.DB.Exec(`DELETE FROM schema_migrations WHERE version=111;
		DROP TRIGGER escrow_refund_address_reserve ON escrows;
		DROP TRIGGER escrow_claim_address_reserve ON settlements;
		DROP TRIGGER xmr_escrow_claim_address_reserve ON xmr_settlements;
		DROP FUNCTION escrow_refund_address_reserve(); DROP FUNCTION escrow_claim_address_reserve();
		DROP TABLE escrow_external_addresses;
		ALTER TABLE escrows ADD CONSTRAINT escrows_refund_address_key UNIQUE (refund_address);
		CREATE UNIQUE INDEX settlements_release_destination ON settlements(destination) WHERE kind='release';
		CREATE UNIQUE INDEX xmr_settlements_release_destination ON xmr_settlements(destination) WHERE kind='release' AND supersedes IS NULL`); err != nil {
		t.Fatal(err)
	}
}

// Infrastructure addresses and this order's buyer refund remain forbidden to a
// claimant, while another account's external refund is not a public oracle.
func TestEscrowExternalClaimAddressIsPrivatePerAccount(t *testing.T) {
	t.Run("Monero", func(t *testing.T) {
		x := newXMREscrowEnv(t)
		x.mustPoll()
		victim, _ := x.fundedXMR(x.product)
		other, _ := x.fundedXMR(x.product)
		ownRefund := testXMRAddr("refund "+other, true)
		if code, _ := x.xmrClaim(other, x.vendorSess, ownRefund, testPassword); code != 409 {
			t.Fatalf("own order buyer refund: %d", code)
		}
		addr := testXMRAddr("refund "+victim, true)
		if code, body := x.xmrClaim(other, x.vendorSess, addr, testPassword); code != 303 {
			t.Fatalf("unrelated buyer refund leaked: %d %s", code, body)
		}
		third, _ := x.fundedXMR(x.digital(x.vendor))
		if code, body := x.xmrClaim(third, x.vendorSess, addr, testPassword); code != 409 || !strings.Contains(body, "already used") {
			t.Fatalf("own claim reuse: %d %s", code, body)
		}
	})
	t.Run("Bitcoin", func(t *testing.T) {
		x := newEscrowEnv(t)
		addr := regtestAddr("another buyer refund")
		x.mustCheckout(x.draft(), addr)
		other := x.completed(regtestAddr("own order buyer refund"), 100000)
		if code, _ := x.claim(other, x.vendorSess, regtestAddr("own order buyer refund")); code != 409 {
			t.Fatalf("own order buyer refund: %d", code)
		}
		if code, body := x.claim(other, x.vendorSess, addr); code != 303 {
			t.Fatalf("unrelated buyer refund leaked: %d %s", code, body)
		}
		third := x.completed(regtestAddr("third buyer refund"), 100000)
		if code, body := x.claim(third, x.vendorSess, addr); code != 409 || !strings.Contains(body, "already used") {
			t.Fatalf("own claim reuse: %d %s", code, body)
		}
	})
}
