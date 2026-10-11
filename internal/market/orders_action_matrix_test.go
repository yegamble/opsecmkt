package market

import (
	"net/url"
	"strings"
	"testing"
)

// TestOrderActionMatrix pins the public order-action contract through HTTP. Each actor/state/action case starts from
// its own order row so successful actions cannot change the setup for another case. The expected outcomes below are
// the product rules: only parties use party actions; payment is requested by the buyer; the vendor ships/delivers;
// the buyer completes; either party opens a dispute; and an independent staff account resolves one.
func TestHistoricalOrderActionMatrix(t *testing.T) {
	e := newTestApp(t)
	buyer, bs := e.user("matrix_buyer", "buyer")
	vendor, vs := e.user("matrix_vendor", "vendor")
	_, other := e.user("matrix_other", "buyer")
	product := e.product(vendor, "physical")
	draft := e.order(buyer, product, "BTC", stateDraft)
	for _, tc := range []struct {
		session string
		want    int
	}{{vs, 403}, {other, 404}, {bs, 303}} {
		order := draft
		w := e.do("POST", "/orders/cancel", tc.session, url.Values{"order_id": {order}, "from": {stateDraft}})
		if w.Code != tc.want {
			t.Errorf("draft cancel as %q: %d want %d: %s", tc.session, w.Code, tc.want, w.Body)
		}
	}
	paid := e.order(buyer, product, "BTC", statePaid)
	for _, route := range []string{"/orders/pay", "/orders/ship", "/orders/deliver", "/orders/complete", "/orders/cancel", "/disputes", "/resolve"} {
		for _, session := range []string{bs, vs, other} {
			f := url.Values{"order_id": {paid}, "from": {statePaid}, "reason": {"Historical dispute requires operator review."}}
			if w := e.do("POST", route, session, f); w.Code == 303 {
				t.Errorf("retired %s accepted historical paid order for %q", route, session)
			}
		}
		if got := agStr(e, "SELECT state FROM orders WHERE id=$1", paid); got != statePaid {
			t.Fatalf("%s changed historical order to %s", route, got)
		}
	}
}

func TestOrderGetStaffAccessAcrossStatesAndFlags(t *testing.T) {
	e := newTestApp(t)
	suffix := randomToken()[:8]
	actors := []struct {
		name, role  string
		id, session string
	}{}
	for _, tc := range []struct{ name, role string }{{"reviewer", "moderator"}, {"admin", "admin"}, {"otherbuyer", "buyer"}, {"othervendor", "vendor"}} {
		id, session := e.user("a167_get_"+tc.name+"_"+suffix, tc.role)
		actors = append(actors, struct {
			name, role  string
			id, session string
		}{tc.name, tc.role, id, session})
	}
	buyer, _ := e.user("a167_get_buyer_"+suffix, "buyer")
	vendorID, _ := e.user("a167_get_vendor_"+suffix, "vendor")
	states := []string{stateDraft, stateAwaitingPayment, statePaid, stateShipped, stateDelivered, stateCompleted, stateDisputed, stateResolved, stateCancelled}
	for _, state := range states {
		for _, flagged := range []bool{false, true} {
			orderID, productID := randomToken(), randomToken()
			tx, err := e.DB.Begin()
			if err != nil {
				t.Fatal(err)
			}
			if _, err = tx.Exec(`INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock)
				VALUES($1,$2,'A167 GET listing','Fixture','Hardware','Worldwide','physical',100000,500000000000,5)`, productID, vendorID); err != nil {
				tx.Rollback()
				t.Fatal(err)
			}
			if _, err = tx.Exec(`INSERT INTO orders(id,buyer_id,product_id,currency,amount,state) VALUES($1,$2,$3,'BTC',100000,$4)`, orderID, buyer, productID, state); err != nil {
				tx.Rollback()
				t.Fatal(err)
			}
			if state == stateDisputed {
				if _, err = tx.Exec(`INSERT INTO disputes(id,order_id,reason) VALUES($1,$2,'The item was not as described; staff review requested.')`, randomToken(), orderID); err != nil {
					tx.Rollback()
					t.Fatal(err)
				}
			} else if state == stateResolved {
				if _, err = tx.Exec(`INSERT INTO disputes(id,order_id,reason,status,outcome,resolution)
					VALUES($1,$2,'The item was not as described; staff review requested.','Resolved — refund to buyer','refund','The evidence supports refunding the buyer.')`, randomToken(), orderID); err != nil {
					tx.Rollback()
					t.Fatal(err)
				}
			}
			if flagged {
				if _, err = tx.Exec(`INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations,flagged)
					VALUES($1,'BTC',$2,0,'a167-flag-address',5000,3,true)`, orderID, txidOf("a167 flagged "+orderID)); err != nil {
					tx.Rollback()
					t.Fatal(err)
				}
			}
			if err = tx.Commit(); err != nil {
				t.Fatal(err)
			}
			for _, actor := range actors {
				want := 404
				if actor.role == "moderator" || actor.role == "admin" {
					if flagged || state == stateDisputed || state == stateResolved {
						want = 200
					}
				}
				response := e.do("GET", "/order?id="+orderID, actor.session, nil)
				if response.Code != want {
					t.Errorf("GET order as %s: state=%s flagged=%v got %d want %d", actor.name, state, flagged, response.Code, want)
				}
			}
			if !flagged && (state == stateDisputed || state == stateResolved) {
				// A state label alone is not a staff-read reason: A-166 requires the dispute record.
				e.mustExec("DELETE FROM disputes WHERE order_id=$1", orderID)
				for _, actor := range actors {
					if response := e.do("GET", "/order?id="+orderID, actor.session, nil); response.Code != 404 {
						t.Errorf("rowless order as %s: state=%s got %d want 404", actor.name, state, response.Code)
					}
				}
			}
		}
	}
}

func TestDisputeReason5001BytesRefused(t *testing.T) {
	w := newOrderWorld(t)
	product := w.e.product(w.vendorID, "physical")
	order := w.e.order(w.buyerID, product, "BTC", statePaid)
	beforeNotifications := w.count("SELECT count(*) FROM notifications")
	w.expect("/disputes", w.buyer, form("order_id", order, "reason", strings.Repeat("x", 5001)), 400, "20–5000 characters")
	if w.state(order) != statePaid || w.count("SELECT count(*) FROM disputes WHERE order_id=$1", order) != 0 ||
		w.count("SELECT count(*) FROM order_events WHERE order_id=$1", order) != 0 || w.count("SELECT count(*) FROM notifications") != beforeNotifications {
		t.Fatal("a 5001-byte dispute reason changed order, dispute, history or notifications")
	}
}
