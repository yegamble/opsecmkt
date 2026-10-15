package market

import (
	"net/url"

	"strconv"
	"strings"
)

type bumpRow struct {
	ID                                                  int64
	N                                                   int
	State, Signer, TxID, Raw, Cosigned, Error, BuildErr string
	Fee, Rate                                           int64
}

func (x *escrowEnv) bumpByID(id int64) bumpRow {
	x.t.Helper()
	r := bumpRow{ID: id}
	if err := x.DB.QueryRow(`SELECT n,state,COALESCE(signer,''),COALESCE(txid,''),COALESCE(raw_tx,''),COALESCE(cosigned,''),send_error,build_error,COALESCE(network_fee,0),fee_rate
		FROM settlement_bumps WHERE id=$1`, id).Scan(&r.N, &r.State, &r.Signer, &r.TxID, &r.Raw, &r.Cosigned, &r.Error, &r.BuildErr, &r.Fee, &r.Rate); err != nil {
		x.t.Fatal(err)
	}
	return r
}

// lastBump is the latest fee bump of settlement sid.
func (x *escrowEnv) lastBump(sid int64) bumpRow {
	x.t.Helper()
	var id int64
	if err := x.DB.QueryRow("SELECT id FROM settlement_bumps WHERE settlement_id=$1 ORDER BY n DESC LIMIT 1", sid).Scan(&id); err != nil {
		x.t.Fatalf("no fee bump of settlement %d: %v", sid, err)
	}
	return x.bumpByID(id)
}

// parentRow is every column of a settlement a fee bump must never change (P2-R7), as one string.
func (x *escrowEnv) parentRow(id int64) string {
	x.t.Helper()
	var s string
	if err := x.DB.QueryRow(`SELECT concat_ws('|',kind,psbt,txid,raw_tx,cosigned,final_at,signer,signed_at,fee_rate,network_fee,input_total,destination,broadcast_at)
		FROM settlements WHERE id=$1`, id).Scan(&s); err != nil {
		x.t.Fatal(err)
	}
	return s + "|" + strings.Join(agStrings(x.t, x.testEnv, "SELECT role||' '||address||' '||sats FROM settlement_outputs WHERE settlement_id=$1 ORDER BY n", id), ",")
}

// broadcastClaim is a vendor's claim of a completed escrow order funded with sats, signed by the vendor, co-signed and
// broadcast at the default fee rate (2 sat/vB): the order and the settlement.
func (x *escrowEnv) broadcastClaim(label string, sats ...int64) (string, int64) {
	x.t.Helper()
	order, id := x.built(regtestAddr("refund-"+label), regtestAddr("claim-"+label), sats...)
	x.signAs(order, roleVendor, id)
	x.poll()
	if r := x.settleByID(id); r.State != "broadcast" {
		x.t.Fatalf("%s: %+v", label, r)
	}
	return order, id
}

// broadcastRefund is the refund of an underpaid escrow order (sats) its buyer cancelled, signed by the buyer, co-signed
// and broadcast: the order and the settlement.
func (x *escrowEnv) broadcastRefund(label string, sats int64) (string, int64) {
	x.t.Helper()
	order := x.draft()
	x.mustCheckout(order, regtestAddr("refund-"+label))
	x.fund(order, order, 3, sats)
	w := x.do("POST", "/orders/cancel", x.buyerSess, url.Values{"order_id": {order}, "from": {stateAwaitingPayment}, "amount_seen": {strconv.FormatInt(sats, 10)}, "payout_confirmed": {"confirmed"}})
	if w.Code != 303 || x.state(order) != stateCancelled {
		x.t.Fatalf("%s: cancel %d %s", label, w.Code, x.state(order))
	}
	x.poll()
	id, _, _, _ := x.settlement(order)
	x.signAs(order, roleBuyer, id)
	x.poll()
	if r := x.settleByID(id); r.State != "broadcast" {
		x.t.Fatalf("%s: %+v", label, r)
	}
	return order, id
}

func (x *escrowEnv) requestBump(order, sess string, id int64, rate string) (int, string) {
	w := x.do("POST", "/escrow/bump", sess, url.Values{"order_id": {order}, "settlement_id": {strconv.FormatInt(id, 10)}, "fee_rate": {rate}, "password": {testPassword}})
	return w.Code, w.Body.String()
}

// mustBump requests a fee bump at rate as the claimant and runs a watcher pass, which builds it.
func (x *escrowEnv) mustBump(order, sess string, id int64, rate string) bumpRow {
	x.t.Helper()
	if code, body := x.requestBump(order, sess, id, rate); code != 303 {
		x.t.Fatalf("bump at %s: %d %s", rate, code, body)
	}
	x.poll()
	b := x.lastBump(id)
	if b.State != "awaiting_signature" {
		x.t.Fatalf("bump at %s after a pass: %+v", rate, b)
	}
	return b
}

func (x *escrowEnv) bumpCopy(order, sess string, sid, bid int64) (int, string) {
	w := x.do("GET", "/escrow-psbt?id="+order+"&s="+strconv.FormatInt(sid, 10)+"&b="+strconv.FormatInt(bid, 10), sess, nil)
	return w.Code, strings.TrimSpace(w.Body.String())
}

func (x *escrowEnv) signBump(order, sess string, bid int64, psbt string) (int, string) {
	w := x.do("POST", "/escrow/bump-sign", sess, url.Values{"order_id": {order}, "bump_id": {strconv.FormatInt(bid, 10)}, "psbt": {psbt}})
	return w.Code, refusalText(w.Body.String())
}

// signBumpAs pastes party's good signature of the fee bump (its wallet signing its own downloaded copy).
func (x *escrowEnv) signBumpAs(order, party string, sid, bid int64) {
	x.t.Helper()
	sess, fp := x.partySess(party)
	code, cp := x.bumpCopy(order, sess, sid, bid)
	if code != 200 {
		x.t.Fatalf("%s's copy of the fee bump: %d", party, code)
	}
	if code, body := x.signBump(order, sess, bid, walletSign(x.t, cp, fp, 1)); code != 303 {
		x.t.Fatalf("%s signs the fee bump: %d %s", party, code, body)
	}
}

// bumpPSBT is the stored PSBT of a fee bump.
func (x *escrowEnv) bumpPSBT(bid int64) string {
	x.t.Helper()
	var s string
	if err := x.DB.QueryRow("SELECT psbt FROM settlement_bumps WHERE id=$1", bid).Scan(&s); err != nil {
		x.t.Fatal(err)
	}
	return s
}
