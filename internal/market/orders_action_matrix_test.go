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
func TestOrderActionMatrix(t *testing.T) {
	e := newTestApp(t)
	suffix := randomToken()[:8]
	actors := []struct {
		name, role, party string
		id, session       string
	}{
		{name: "buyer", role: "buyer", party: "buyer"},
		{name: "vendor", role: "vendor", party: "vendor"},
		{name: "other buyer", role: "buyer"},
		{name: "other vendor", role: "vendor"},
		{name: "reviewing moderator", role: "moderator"},
		{name: "reviewing administrator", role: "admin"},
		{name: "moderator buyer", role: "moderator", party: "buyer"},
		{name: "administrator buyer", role: "admin", party: "buyer"},
		{name: "moderator vendor", role: "moderator", party: "vendor"},
		{name: "administrator vendor", role: "admin", party: "vendor"},
	}
	for i := range actors {
		actors[i].id, actors[i].session = e.user("a167_"+strconvIndex(i)+"_"+suffix, actors[i].role)
	}
	// Dedicated fixture principals keep the two outsider personas outside both order parties.
	buyerID, _ := e.user("a167_fixture_buyer_"+suffix, "buyer")
	vendorID, _ := e.user("a167_fixture_vendor_"+suffix, "vendor")
	buyerDefault := struct {
		name, role, party string
		id, session       string
	}{name: "fixture buyer", role: "buyer", party: "buyer", id: buyerID}
	vendorDefault := struct {
		name, role, party string
		id, session       string
	}{name: "fixture vendor", role: "vendor", party: "vendor", id: vendorID}
	e.A.payments["BTC"] = &stubProvider{currency: "BTC"}
	actions := []struct {
		name, path, kind string
	}{
		{"request payment", "/orders/pay", "digital"},
		{"cancel", "/orders/cancel", "physical"},
		{"ship", "/orders/ship", "physical"},
		{"deliver", "/orders/deliver", "digital"},
		{"complete", "/orders/complete", "physical"},
		{"open dispute", "/disputes", "physical"},
		{"resolve", "/resolve", "physical"},
	}
	states := []string{stateDraft, stateAwaitingPayment, statePaid, stateShipped, stateDelivered, stateCompleted, stateDisputed, stateResolved, stateCancelled}
	type matrixCase struct {
		actor, action, state        string
		session, orderID, disputeID string
		buyerID, sellerID, kind     string
		status                      int
		resultState                 string
	}
	cases := make([]matrixCase, 0, len(actors)*len(actions)*len(states))
	for _, actor := range actors {
		for _, action := range actions {
			for _, state := range states {
				buyer, seller := buyerDefault, vendorDefault
				if actor.party == "buyer" {
					buyer = actor
				}
				if actor.party == "vendor" {
					seller = actor
				}
				kind := action.kind
				if action.path == "/orders/complete" {
					if state == stateDelivered {
						kind = "digital"
					} else {
						kind = "physical"
					}
				}
				orderID := randomToken()
				caseResult := matrixCase{actor: actor.name, action: action.name, state: state, session: actor.session, orderID: orderID,
					buyerID: buyer.id, sellerID: seller.id, kind: kind, resultState: state}
				if action.path == "/resolve" {
					caseResult.disputeID = randomToken()
				}
				caseResult.status, caseResult.resultState = orderActionMatrixWant(action.path, actor, state)
				cases = append(cases, caseResult)
			}
		}
	}
	if len(cases) != 630 {
		t.Fatalf("constructed %d order-action cases, want 630", len(cases))
	}
	for _, tc := range cases {
		productID := randomToken()
		if _, err := e.DB.Exec(`INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock)
			VALUES($1,$2,'A167 matrix listing','Fixture','Hardware','Worldwide',$3,100000,500000000000,5)`, productID, tc.sellerID, tc.kind); err != nil {
			t.Fatal(err)
		}
		if _, err := e.DB.Exec(`INSERT INTO orders(id,buyer_id,product_id,currency,amount,state) VALUES($1,$2,$3,'BTC',100000,$4)`, tc.orderID, tc.buyerID, productID, tc.state); err != nil {
			t.Fatal(err)
		}
		if tc.disputeID != "" {
			if _, err := e.DB.Exec(`INSERT INTO disputes(id,order_id,reason) VALUES($1,$2,'The item was not as described and the vendor has not replied.')`, tc.disputeID, tc.orderID); err != nil {
				t.Fatal(err)
			}
		}
		form := url.Values{"order_id": {tc.orderID}}
		switch tc.action {
		case "cancel":
			from := tc.state
			if from != stateDraft && from != stateAwaitingPayment && from != statePaid {
				from = statePaid
			}
			form.Set("from", from)
			form.Set("payout_confirmed", "confirmed")
			form.Set("amount_seen", "0")
		case "ship":
			form.Set("note", "Marked shipped by the vendor.")
		case "deliver":
			form.Set("content", "Delivery token A167 MATRIX CONTENT")
		case "complete":
			form.Set("payout_confirmed", "confirmed")
			form.Set("amount_seen", "0")
		case "open dispute":
			form.Set("reason", "The item was not as described and the vendor has not replied.")
		case "resolve":
			form = url.Values{"id": {tc.disputeID}, "outcome": {"refund"}, "resolution": {"The evidence supports refunding the buyer in full."},
				"reply_seen": {"0"}, "password": {testPassword}, "payout_confirmed": {"confirmed"}, "amount_seen": {"0"}}
		}
		response := e.do("POST", actionPath(tc.action), tc.session, form)
		if response.Code != tc.status {
			t.Errorf("%s as %s on %s: got HTTP %d, want %d; body=%q", tc.action, tc.actor, tc.state, response.Code, tc.status, response.Body.String())
			continue
		}
		var got string
		if err := e.DB.QueryRow("SELECT state FROM orders WHERE id=$1", tc.orderID).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != tc.resultState {
			t.Errorf("%s as %s on %s changed order to %s, want %s", tc.action, tc.actor, tc.state, got, tc.resultState)
		}
	}
}

func actionPath(name string) string {
	for _, action := range []struct{ name, path string }{
		{"request payment", "/orders/pay"}, {"cancel", "/orders/cancel"}, {"ship", "/orders/ship"},
		{"deliver", "/orders/deliver"}, {"complete", "/orders/complete"}, {"open dispute", "/disputes"}, {"resolve", "/resolve"},
	} {
		if action.name == name {
			return action.path
		}
	}
	return ""
}

func orderActionMatrixWant(path string, actor struct {
	name, role, party string
	id, session       string
}, state string) (int, string) {
	party := actor.party != ""
	isBuyer := actor.party == "buyer"
	isVendor := actor.party == "vendor"
	success := func(target string) (int, string) { return 303, target }
	if path == "/resolve" {
		if actor.role != "moderator" && actor.role != "admin" {
			return 403, state
		}
		if party {
			return 403, state
		}
		if state != stateDisputed {
			return 409, state
		}
		return success(stateResolved)
	}
	if !party {
		return 404, state
	}
	switch path {
	case "/orders/pay":
		if state != stateDraft {
			return 409, state
		}
		if actor.party == "vendor" && actor.role == "moderator" {
			return 409, state // a moderator cannot own the listing as an eligible seller
		}
		if isBuyer {
			return success(stateAwaitingPayment)
		}
		return 403, state
	case "/orders/cancel":
		switch state {
		case stateDraft:
			if isBuyer {
				return success(stateCancelled)
			}
		case stateAwaitingPayment:
			return success(stateCancelled)
		case statePaid:
			if isVendor {
				return success(stateCancelled)
			}
		}
		if state == stateDraft || state == stateAwaitingPayment || state == statePaid {
			return 403, state
		}
		return 409, state
	case "/orders/ship":
		if state != statePaid {
			return 409, state
		}
		if isVendor {
			return success(stateShipped)
		}
		return 403, state
	case "/orders/deliver":
		if state != statePaid {
			return 409, state
		}
		if isVendor {
			return success(stateDelivered)
		}
		return 403, state
	case "/orders/complete":
		if state != stateShipped && state != stateDelivered {
			return 409, state
		}
		if isBuyer {
			return success(stateCompleted)
		}
		return 403, state
	case "/disputes":
		if state == statePaid || state == stateShipped || state == stateDelivered {
			return success(stateDisputed)
		}
		return 409, state
	}
	return 500, state
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

func strconvIndex(i int) string { return string(rune('0' + i)) }
