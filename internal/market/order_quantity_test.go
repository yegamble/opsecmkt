package market

import (
	"context"
	"database/sql"
	"math"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestOrderQuantityMigrationPreservesLegacyOrders(t *testing.T) {
	db := schemaDB(t)
	ctx := t.Context()
	list, err := loadMigrations(migrationFiles)
	if err != nil {
		t.Fatal(err)
	}
	var previous []migration
	for _, m := range list {
		if m.Version < 133 {
			previous = append(previous, m)
		}
	}
	runInTx(t, db, func(tx *sql.Tx) error {
		if _, err := tx.Exec(schema); err != nil {
			return err
		}
		return applyMigrations(ctx, tx, previous)
	})
	for _, q := range []string{
		"INSERT INTO users(id,handle,password_hash,role) VALUES('qb','quantity_buyer','x','buyer'),('qv','quantity_vendor','x','vendor')",
		"INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock,dispatch_days) VALUES('qp','qv','Quantity upgrade','Original description','Hardware','Worldwide','physical',100000,500000000000,5,3)",
		"INSERT INTO orders(id,buyer_id,product_id,currency,amount,state) VALUES('qd','qb','qp','XMR',500000000000,'draft'),('qa','qb','qp','XMR',500000000000,'paid'),('qc','qb','qp','XMR',500000000000,'cancelled')",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	runInTx(t, db, func(tx *sql.Tx) error { return applyMigrations(ctx, tx, list) })
	var preserved int
	if err := db.QueryRow("SELECT count(*) FROM orders WHERE quantity=1 AND amount=500000000000").Scan(&preserved); err != nil || preserved != 3 {
		t.Fatalf("legacy orders changed by migration: %d %v", preserved, err)
	}
	for _, q := range []int64{0, -1} {
		if _, err := db.Exec("UPDATE orders SET quantity=$1 WHERE id='qd'", q); err == nil {
			t.Fatalf("invalid draft quantity %d accepted by schema", q)
		}
	}
	if _, err := db.Exec("UPDATE orders SET quantity=2 WHERE id='qd'"); err != nil {
		t.Fatal(err)
	}
	runInTx(t, db, func(tx *sql.Tx) error { return applyMigrations(ctx, tx, list) })
}

func TestOrderQuantityDraftValidationAndTotal(t *testing.T) {
	p := newPayEnv(t)
	f := url.Values{"product_id": {p.productID}, "currency": {"XMR"}, "quantity": {"3"}, formNonceField: {randomToken()[:32]}}
	w := p.do("POST", "/orders", p.buyerSess, f)
	p.check(w, 303)
	order := strings.TrimPrefix(w.Header().Get("Location"), "/order?id=")
	if p.count("SELECT quantity FROM orders WHERE id=$1", order) != 3 || p.str("SELECT amount::text FROM orders WHERE id=$1", order) != "1500000000000" || p.count("SELECT stock FROM products WHERE id=$1", p.productID) != 5 {
		t.Fatal("draft lost quantity, total, or reserved stock")
	}
	p.check(p.do("POST", "/orders", p.buyerSess, f), 303)
	if p.count("SELECT count(*) FROM orders WHERE buyer_id=$1 AND product_id=$2", p.buyer.ID, p.productID) != 1 {
		t.Fatal("nonce replay duplicated draft")
	}
	for _, bad := range []string{"", "0", "-1", "1.5", "+2", " 2 ", "6", "99999999999999999999999999"} {
		f := url.Values{"product_id": {p.productID}, "currency": {"XMR"}, "quantity": {bad}}
		p.check(p.do("POST", "/orders", p.buyerSess, f), 400)
		if p.count("SELECT quantity FROM orders WHERE id=$1", order) != 3 {
			t.Fatalf("bad quantity %q modified existing draft", bad)
		}
	}
	p.check(p.do("POST", "/orders", p.buyerSess, url.Values{"product_id": {p.productID}, "currency": {"XMR"}, "quantity": {"5"}}), 303)
	if p.count("SELECT quantity FROM orders WHERE id=$1", order) != 5 || p.str("SELECT amount::text FROM orders WHERE id=$1", order) != "2500000000000" {
		t.Fatal("existing draft quantity did not update")
	}
	page := p.page("/checkout?id="+p.productID, p.buyerSess)
	mustContain(t, page, `name="quantity"`, `max="5"`)
	agExec(p.testEnv, "UPDATE products SET stock=0 WHERE id=$1", p.productID)
	page = p.page("/checkout?id="+p.productID, p.buyerSess)
	mustContain(t, page, "Out of stock")
	mustNotContain(t, page, `action="/orders"`)
	p.check(p.do("POST", "/orders", p.buyerSess, url.Values{"product_id": {p.productID}, "currency": {"XMR"}, "quantity": {"1"}}), 400)
	agExec(p.testEnv, "UPDATE products SET stock=5 WHERE id=$1", p.productID)
	p.check(p.do("POST", "/orders/cancel", p.buyerSess, url.Values{"order_id": {order}, "from": {stateDraft}}), 303)
	if p.count("SELECT stock FROM products WHERE id=$1", p.productID) != 5 {
		t.Fatal("cancelling an unfunded quantity draft returned unreserved units")
	}
	agExec(p.testEnv, "UPDATE products SET xmr=$2 WHERE id=$1", p.productID, int64(math.MaxInt64))
	p.check(p.do("POST", "/orders", p.buyerSess, url.Values{"product_id": {p.productID}, "currency": {"XMR"}, "quantity": {"2"}}), 400)
	if p.count("SELECT count(*) FROM orders WHERE buyer_id=$1 AND product_id=$2 AND state='draft'", p.buyer.ID, p.productID) != 0 {
		t.Fatal("overflow created a draft")
	}
}

func TestXMRQuantityCheckoutRepricesReservesAndReturnsExactlyOnce(t *testing.T) {
	x := newXMREscrowEnv(t)
	product := x.physical(x.vendor)
	x.mustPoll()
	w := x.do("POST", "/orders", x.buyerSess, url.Values{"product_id": {product}, "currency": {"XMR"}, "quantity": {"3"}})
	x.check(w, 303)
	order := strings.TrimPrefix(w.Header().Get("Location"), "/order?id=")
	agExec(x.testEnv, "UPDATE products SET xmr=700000000000 WHERE id=$1", product)
	page := x.page("/order?id="+order, x.buyerSess)
	mustContain(t, page, "Current checkout total for 3 units:", "2.1 XMR")
	before := x.fake.count()
	checkout := url.Values{"order_id": {order}, "quantity": {"999"}, "refund_address": {testXMRAddr("quantity refund", true)}, formNonceField: {renderedFormNonce(t, page, "/orders/escrow-xmr")}}
	x.check(x.do("POST", "/orders/escrow-xmr", x.buyerSess, checkout), 303)
	x.check(x.do("POST", "/orders/escrow-xmr", x.buyerSess, checkout), 303)
	if x.fake.count() != before || agInt(x.testEnv, "SELECT stock FROM products WHERE id=$1", product) != 2 || agInt64(t, x.testEnv, "SELECT amount FROM orders WHERE id=$1", order) != 2100000000000 {
		t.Fatal("checkout did not use current unit price times saved quantity or reserve exact units")
	}
	o, err := scanOrder(x.DB.QueryRow(orderQuery+" WHERE o.id=$1", order))
	if err != nil {
		t.Fatal(err)
	}
	if agInt(x.testEnv, "SELECT quantity FROM orders WHERE id=$1", order) != 3 || o.Amount != "2.1" {
		t.Fatalf("order hydration: %+v", o)
	}
	reservations, err := listingReservations(t.Context(), x.A, []string{product})
	if err != nil || reservations[product].Units != 3 {
		t.Fatalf("reserved units: %+v %v", reservations, err)
	}
	cancel := url.Values{"order_id": {order}, "from": {stateAwaitingPayment}, formNonceField: {randomToken()[:32]}}
	x.check(x.do("POST", "/orders/cancel", x.buyerSess, cancel), 303)
	x.check(x.do("POST", "/orders/cancel", x.buyerSess, cancel), 303)
	if agInt(x.testEnv, "SELECT stock FROM products WHERE id=$1", product) != 5 {
		t.Fatal("cancel did not restore exactly three units once")
	}
}

func TestXMRQuantityStaleStockAndOverflowLeaveNoEscrow(t *testing.T) {
	x := newXMREscrowEnv(t)
	product := x.physical(x.vendor)
	x.mustPoll()
	for _, tc := range []struct {
		name  string
		stock int
		price int64
		code  int
	}{{"stock fell", 2, 500000000000, 409}, {"price overflow", 5, math.MaxInt64, 400}} {
		t.Run(tc.name, func(t *testing.T) {
			buyer, session := x.user("quantity_stale_"+randomToken()[:8], "buyer")
			order := x.order(buyer, product, "XMR", stateDraft)
			agExec(x.testEnv, "UPDATE orders SET quantity=3 WHERE id=$1", order)
			agExec(x.testEnv, "UPDATE products SET stock=$2,xmr=$3 WHERE id=$1", product, tc.stock, tc.price)
			page := x.page("/order?id="+order, session)
			mustNotContain(t, page, `action="/orders/escrow-xmr"`)
			before := x.fake.count()
			slots := agInt(x.testEnv, "SELECT count(*) FROM xmr_escrow_slots WHERE state='claimed'")
			x.check(x.checkout(session, order, testXMRAddr("quantity refusal "+order, true)), tc.code)
			if x.fake.count() != before || agInt(x.testEnv, "SELECT count(*) FROM escrows WHERE order_id=$1", order) != 0 || agInt(x.testEnv, "SELECT count(*) FROM xmr_escrow_slots WHERE state='claimed'") != slots || agInt(x.testEnv, "SELECT stock FROM products WHERE id=$1", product) != tc.stock || agStr(x.testEnv, "SELECT state FROM orders WHERE id=$1", order) != stateDraft {
				t.Fatal("refused quantity checkout changed stock, slot, order or wallet")
			}
		})
	}
}

func TestXMRQuantityPaidCancellationAndFrozenQuantity(t *testing.T) {
	x := newXMREscrowEnv(t)
	product := x.physical(x.vendor)
	x.mustPoll()
	order := x.draft(product)
	agExec(x.testEnv, "UPDATE orders SET quantity=3 WHERE id=$1", order)
	addr := x.mustCheckout(order)
	for _, quantity := range []int64{0, -1, 4} {
		if _, err := x.DB.Exec("UPDATE orders SET quantity=$2 WHERE id=$1", order, quantity); err == nil {
			t.Fatalf("checked-out quantity changed to %d", quantity)
		}
	}
	paid := int64(3 * xmrListingPrice)
	x.fake.payAddress(addr, hex64("quantity full payment "+order), paid, 10)
	x.mustPoll()
	if x.state(order) != statePaid {
		t.Fatalf("multi-unit payment state %s", x.state(order))
	}
	// Restocking while a large order is reserved must not overflow the old int32 stock column on return.
	agExec(x.testEnv, "UPDATE products SET stock=$2 WHERE id=$1", product, int64(math.MaxInt32))
	form := url.Values{"order_id": {order}, "from": {statePaid}, "payout_confirmed": {"confirmed"}, "amount_seen": {strconv.FormatInt(paid, 10)}, formNonceField: {randomToken()[:32]}}
	x.check(x.do("POST", "/orders/cancel", x.vendorSess, form), 303)
	x.check(x.do("POST", "/orders/cancel", x.vendorSess, form), 303)
	if got := agInt64(t, x.testEnv, "SELECT stock FROM products WHERE id=$1", product); got != int64(math.MaxInt32)+3 {
		t.Fatalf("paid cancellation returned incorrect quantity: %d", got)
	}
	if agInt(x.testEnv, "SELECT count(*) FROM xmr_settlements WHERE order_id=$1 AND kind='refund'", order) != 1 {
		t.Fatal("quantity order did not keep one refund settlement")
	}
}

func TestXMRQuantityConcurrentCheckoutCannotOversell(t *testing.T) {
	x := newXMREscrowEnv(t)
	product := x.physical(x.vendor)
	x.mustPoll()
	type request struct{ order, session string }
	requests := make([]request, 2)
	for i := range requests {
		id, s := x.user("quantity_race_"+randomToken()[:8], "buyer")
		order := x.order(id, product, "XMR", stateDraft)
		agExec(x.testEnv, "UPDATE orders SET quantity=3 WHERE id=$1", order)
		requests[i] = request{order, s}
	}
	before := x.fake.count()
	codes := make([]int, 2)
	var wg sync.WaitGroup
	for i, r := range requests {
		wg.Go(func() { codes[i] = x.checkout(r.session, r.order, testXMRAddr("quantity race "+r.order, true)).Code })
	}
	wg.Wait()
	if !((codes[0] == 303 && codes[1] == 409) || (codes[1] == 303 && codes[0] == 409)) {
		t.Fatalf("race statuses: %v", codes)
	}
	if agInt(x.testEnv, "SELECT stock FROM products WHERE id=$1", product) != 2 || agInt(x.testEnv, "SELECT sum(quantity) FROM orders WHERE product_id=$1 AND state='awaiting_payment'", product) != 3 || agInt(x.testEnv, "SELECT count(*) FROM escrows WHERE order_id=ANY($1)", []string{requests[0].order, requests[1].order}) != 1 || x.fake.count() != before {
		t.Fatal("concurrent orders oversold stock or took extra escrow slots")
	}
}

func TestXMRQuantityCanBuyAllRemainingStock(t *testing.T) {
	x := newXMREscrowEnv(t)
	product := x.physical(x.vendor)
	x.mustPoll()
	order := x.draft(product)
	agExec(x.testEnv, "UPDATE orders SET quantity=5 WHERE id=$1", order)
	before := x.fake.count()
	x.mustCheckout(order)
	if agInt(x.testEnv, "SELECT stock FROM products WHERE id=$1", product) != 0 || agInt64(t, x.testEnv, "SELECT amount FROM orders WHERE id=$1", order) != 5*xmrListingPrice || x.fake.count() != before {
		t.Fatal("checkout did not permit the full remaining quantity")
	}
	page := x.page("/checkout?id="+product, x.buyerSess)
	mustContain(t, page, "Out of stock")
	mustNotContain(t, page, `action="/orders"`)
	page = x.page("/product?id="+product, x.buyerSess)
	mustContain(t, page, "Out of stock")
	mustNotContain(t, page, "Review order draft")
}

func TestXMRQuantityCheckoutRejectsPriceChangeBeforeStockLock(t *testing.T) {
	x := newXMREscrowEnv(t)
	product := x.physical(x.vendor)
	x.mustPoll()
	order := x.draft(product)
	agExec(x.testEnv, "UPDATE orders SET quantity=3 WHERE id=$1", order)
	// Commit a vendor edit after the preliminary price read, before stockHook
	// locks the listing. The same transaction must refuse the stale total.
	saved := transitionHooks
	transitionHooks = append([]transitionHook{func(ctx context.Context, _ *App, _ *sql.Tx, o *Order, from, to string) error {
		if o.ID == order && from == stateDraft && to == stateAwaitingPayment {
			_, err := x.DB.ExecContext(ctx, "UPDATE products SET xmr=700000000000 WHERE id=$1", product)
			return err
		}
		return nil
	}}, saved...)
	t.Cleanup(func() { transitionHooks = saved })
	before := x.fake.count()
	x.check(x.checkout(x.buyerSess, order, testXMRAddr("quantity edited during checkout", true)), 409)
	if x.state(order) != stateDraft || agInt(x.testEnv, "SELECT stock FROM products WHERE id=$1", product) != 5 || agInt(x.testEnv, "SELECT count(*) FROM escrows WHERE order_id=$1", order) != 0 || x.fake.count() != before {
		t.Fatal("concurrent price edit left an underpriced or partially reserved order")
	}
}
