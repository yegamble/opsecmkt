package market

import (
	"net/url"
	"strconv"
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

// TestEscrowOrderActionMatrix pins every order action on live Bitcoin escrow orders (A-493, absorbing A-63's staff
// refusal pin) through HTTP: ship (the escrow form and the retired order form), complete, cancel, dispute, resolve and
// either party's concession, for the buyer, the vendor, an unrelated account, an independent moderator and an
// independent administrator, on a physical escrow order that is a draft, awaiting payment, paid, shipped or disputed.
// The expected outcomes are the product rules: only the parties reach party actions (anyone else, staff included, is
// told the order does not exist); the vendor ships a paid order with the escrow form only; the buyer confirms receipt by
// signing the release, never with Complete; a draft is cancelled by its buyer, an unpaid escrow by either party and a
// paid one by its vendor (by the buyer only after the ship-by time); either party disputes a paid or shipped order; only
// an independent administrator decides an escrow dispute; and each party concedes only to the other. Every refusal runs
// against one order per state and must leave its state, history, settlements, dispute, notifications and form receipts
// exactly as they were; every success runs on a fresh order of its own and records exactly one move. Fake escrow Core:
// simulated RPC, not chain evidence.
func TestEscrowOrderActionMatrix(t *testing.T) {
	x := newEscrowEnv(t)
	s := randomToken()[:6]
	_, other := x.user("ematrix_other_"+s, "buyer")
	_, mod := x.user("ematrix_mod_"+s, "moderator")
	_, admin := x.user("ematrix_admin_"+s, "admin")
	_, pub := testPGPKey(t, "ematrixvendor"+s)
	x.setKey(x.vendor, pub) // a physical escrow needs a vendor who can receive the buyer's encrypted address
	actors := []struct{ name, session string }{{"buyer", x.buyerSess}, {"vendor", x.vendorSess}, {"non-party", other}, {"moderator", mod}, {"admin", admin}}
	n := 0
	// fresh is a new physical escrow order of the buyer's in state, and its dispute when disputed.
	fresh := func(state string) (string, string) {
		t.Helper()
		n++
		label := "ematrix-" + strconv.Itoa(n) + "-" + s
		order := x.testEnv.order(x.buyer, x.physicalListing(x.vendor, 3), "BTC", stateDraft)
		if state == stateDraft {
			return order, ""
		}
		x.mustCheckout(order, regtestAddr(label+"-refund"))
		if state == stateAwaitingPayment {
			return order, ""
		}
		x.fund(order, label, 3, 100000)
		if got := x.state(order); got != statePaid {
			t.Fatalf("funded matrix order is %s", got)
		}
		switch state {
		case stateShipped:
			if code, body := x.shipEscrow(order, x.vendorSess, regtestAddr(label+"-claim"), testPassword); code != 303 {
				t.Fatalf("ship matrix order: %d %s", code, body)
			}
		case stateDisputed:
			w := x.do("POST", "/disputes", x.buyerSess, url.Values{"order_id": {order}, "reason": {"The parcel has not arrived and the vendor does not answer."}})
			if w.Code != 303 || x.state(order) != stateDisputed {
				t.Fatalf("dispute matrix order: %d %s", w.Code, w.Body.String())
			}
			return order, agStr(x.testEnv, "SELECT id FROM disputes WHERE order_id=$1", order)
		}
		return order, ""
	}
	formFor := func(action, state, order, dispute string) url.Values {
		switch action {
		case "ship":
			return url.Values{"order_id": {order}, "address": {regtestAddr("ematrix-claim-" + randomToken())}, "password": {testPassword}, "note": {"Sent by tracked post."}}
		case "ship with the order form":
			return url.Values{"order_id": {order}, "note": {"Sent by tracked post."}}
		case "complete":
			return payoutConfirmed(url.Values{"order_id": {order}}, 100000)
		case "cancel":
			from := state
			if from != stateDraft && from != stateAwaitingPayment && from != statePaid {
				from = statePaid // a form shown before the order moved on
			}
			return payoutConfirmed(url.Values{"order_id": {order}, "from": {from}}, 100000)
		case "dispute":
			return url.Values{"order_id": {order}, "reason": {"The parcel has not arrived and the vendor does not answer."}}
		case "resolve":
			return resolveConfirmed(url.Values{"id": {dispute}, "outcome": {"refund"}, "resolution": {"The evidence supports refunding the buyer in full."}, "reply_seen": {"0"}}, 100000)
		case "concede release":
			return concessionForm(dispute, "release", randomToken()[:32], 100000)
		case "concede refund":
			return concessionForm(dispute, "refund", randomToken()[:32], 100000)
		}
		t.Fatalf("unknown action %s", action)
		return nil
	}
	// snapshot is everything a refusal must leave unchanged.
	snapshot := func(order string) string {
		t.Helper()
		return agStr(x.testEnv, `SELECT o.state||' events='||(SELECT count(*) FROM order_events WHERE order_id=o.id)||
			' settlements='||(SELECT count(*) FROM settlements WHERE order_id=o.id)||
			' disputes='||coalesce((SELECT string_agg(status||':'||outcome,',') FROM disputes WHERE order_id=o.id),'')||
			' notifications='||(SELECT count(*) FROM notifications)||' receipts='||(SELECT count(*) FROM form_submissions)||
			' release_at='||coalesce((SELECT release_at::text FROM escrows WHERE order_id=o.id),'-')
			FROM orders o WHERE o.id=$1`, order)
	}
	actions := []struct {
		name, path string
		dispute    bool // needs a dispute row: one is planted on the refusal order of a state that has none
	}{
		{"ship", "/escrow/ship", false}, {"ship with the order form", "/orders/ship", false}, {"complete", "/orders/complete", false},
		{"cancel", "/orders/cancel", false}, {"dispute", "/disputes", false},
		{"resolve", "/resolve", true}, {"concede release", "/disputes/concede", true}, {"concede refund", "/disputes/concede", true},
	}
	successes := 0
	for _, state := range []string{stateDraft, stateAwaitingPayment, statePaid, stateShipped, stateDisputed} {
		shared, sharedDispute := fresh(state)
		for _, action := range actions {
			if action.dispute && sharedDispute == "" {
				// As TestHistoricalOrderActionMatrix's predecessor did: an Open dispute row on an order that is not disputed,
				// so that the state rule, not a missing dispute, is what refuses resolve and concede.
				sharedDispute = randomToken()
				agExec(x.testEnv, "INSERT INTO disputes(id,order_id,reason) VALUES($1,$2,'The parcel has not arrived and the vendor does not answer.')", sharedDispute, shared)
			}
			for _, actor := range actors {
				want, to := escrowMatrixWant(action.name, actor.name, state)
				name := action.name + " as " + actor.name + " on " + state
				if want == 303 {
					successes++
					order, dispute := fresh(state)
					w := x.do("POST", action.path, actor.session, formFor(action.name, state, order, dispute))
					if w.Code != 303 {
						t.Errorf("%s: %d, want 303: %s", name, w.Code, w.Body.String())
						continue
					}
					if got := x.state(order); got != to {
						t.Errorf("%s moved the order to %s, want %s", name, got, to)
					}
					if got := agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND from_state=$2 AND to_state=$3", order, state, to); got != 1 {
						t.Errorf("%s recorded %d %s>%s events, want 1", name, got, state, to)
					}
					continue
				}
				before := snapshot(shared)
				w := x.do("POST", action.path, actor.session, formFor(action.name, state, shared, sharedDispute))
				if w.Code != want {
					t.Errorf("%s: %d, want %d: %s", name, w.Code, want, w.Body.String())
				}
				if after := snapshot(shared); after != before {
					t.Errorf("refused %s changed the order:\n before %s\n after  %s", name, before, after)
				}
			}
		}
	}
	if successes != 12 {
		t.Fatalf("the matrix allows %d moves, want 12", successes)
	}
}

// escrowMatrixWant is the status a physical Bitcoin escrow order's action answers actor in state, and the state a
// success moves the order to.
func escrowMatrixWant(action, actor, state string) (int, string) {
	switch action {
	case "resolve":
		switch {
		case actor != "moderator" && actor != "admin":
			return 403, "" // staff only
		case actor == "moderator" && state != stateDraft:
			return 403, "" // only an administrator decides a dispute on an escrow order (a draft has no escrow yet)
		case state == stateDisputed:
			return 303, stateResolved
		}
		return 409, ""
	case "concede release", "concede refund":
		switch {
		case action == "concede release" && actor != "buyer", action == "concede refund" && actor != "vendor":
			return 403, "" // each party concedes only to the other; staff and outsiders never
		case state != stateDisputed:
			return 409, ""
		case actor == "buyer":
			return 303, stateCompleted
		}
		return 303, stateCancelled
	}
	if actor != "buyer" && actor != "vendor" {
		return 404, "" // party actions do not reveal an order to anyone else, staff included
	}
	switch action {
	case "ship":
		switch {
		case state == stateDraft:
			return 404, "" // no escrow yet: the escrow form does not know the order
		case actor == "buyer":
			return 403, ""
		case state == statePaid:
			return 303, stateShipped
		}
	case "ship with the order form":
		// The retired form: an escrow order is shipped with its claim address; a draft is not paid.
	case "complete":
		if actor == "vendor" && state == stateShipped {
			return 403, ""
		}
		// The buyer of a shipped escrow order confirms receipt by signing the release (errEscrowComplete).
	case "cancel":
		switch {
		case state == stateDraft && actor == "buyer", state == stateAwaitingPayment, state == statePaid && actor == "vendor":
			return 303, stateCancelled
		case state == stateDraft:
			return 403, ""
		}
		// The buyer of a paid escrow order cancels only after its ship-by time; later states cannot be cancelled.
	case "dispute":
		if state == statePaid || state == stateShipped {
			return 303, stateDisputed
		}
	}
	return 409, ""
}
