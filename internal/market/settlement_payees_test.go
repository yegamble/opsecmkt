package market

import (
	"slices"
	"strings"
	"testing"
)

// A-652a fix round (PAY89-6), migration 150: a Monero settlement's outputs pay only the addresses its order recorded,
// and an escrow's refund address and fee destination never change. DB integration (SQL against the migrated schema);
// the watcher's own builds through these triggers are the Monero escrow tests (TestXMREscrowBuildRefusals covers the
// replaced fee address).

const payeeRefusal = "pays only its recorded address"

// payeeFixture is a completed order of a market with a 2.5% fee, with a requested release to the vendor's claim
// address, and a cancelled order with its requested refund: the release, its claim address, the refund, and the
// escrows' refund and fee addresses.
type payeeFixture struct {
	x                          *xmrEscrowEnv
	release, refund            int64
	claim, refundAddr, feeAddr string
}

func newPayeeFixture(t *testing.T) payeeFixture {
	t.Helper()
	x := newXMREscrowEnv(t)
	t.Setenv("ESCROW_XMR_FEE_BPS", "250")
	t.Setenv("ESCROW_XMR_FEE_ADDRESS", testXMRAddr("payee fee", false))
	x.restart()
	x.mustPoll()
	// Both orders are funded (each by a pass) before either settlement is requested, so no pass builds them.
	cancelled, _ := x.paidXMRPhysical(x.physical(x.vendor))
	order, _, _ := x.completed()
	f := payeeFixture{x: x}
	f.release, f.claim = x.release(order)
	f.refundAddr = agStr(x.testEnv, "SELECT refund_address FROM escrows WHERE order_id=$1", order)
	f.feeAddr = agStr(x.testEnv, "SELECT fee_destination FROM escrows WHERE order_id=$1", order)
	if f.feeAddr == "" {
		t.Fatal("the escrow recorded no fee destination")
	}
	x.forceDeadline(cancelled, "ship_by=now()-interval '1 minute'")
	if code, body := x.xmrBuyerCancel(cancelled, x.sessOf(cancelled, roleBuyer)); code != 303 {
		t.Fatalf("buyer's cancellation: %d %s", code, body)
	}
	f.refund = x.settlementOf(cancelled, "refund").ID
	return f
}

func TestSettlementOutputsMatchPayees(t *testing.T) {
	f := newPayeeFixture(t)
	e := f.x.testEnv
	refundDest := agStr(e, "SELECT destination FROM xmr_settlements WHERE id=$1", f.refund)
	other := testXMRAddr("someone else", false)
	out := "INSERT INTO xmr_settlement_outputs(settlement_id,n,role,address,amount) VALUES($1,$2,$3,$4,$5)"
	accepted := func(what string, args ...any) {
		t.Helper()
		tx, err := e.DB.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err = tx.Exec(out, args...); err != nil {
			t.Errorf("%s: refused: %v", what, err)
		}
	}
	accepted("the release's claimant to the claim address", f.release, 0, "claimant", f.claim, 1000)
	accepted("the release's buyer return to the refund address", f.release, 1, "buyer_return", f.refundAddr, 1000)
	accepted("the release's market fee to the fee destination", f.release, 1, "market_fee", f.feeAddr, 1000)
	accepted("the refund's claimant to the refund address", f.refund, 0, "claimant", refundDest, 1000)
	for _, c := range []struct {
		what string
		args []any
	}{
		{"the release's claimant to the fee address", []any{f.release, 0, "claimant", f.feeAddr, 1000}},
		{"the release's claimant to another address", []any{f.release, 0, "claimant", other, 1000}},
		{"the release's buyer return to the claim address", []any{f.release, 1, "buyer_return", f.claim, 1000}},
		{"the release's market fee to the refund address", []any{f.release, 1, "market_fee", f.refundAddr, 1000}},
		{"the release's market fee to another address", []any{f.release, 1, "market_fee", other, 1000}},
		{"the refund's claimant to the fee address", []any{f.refund, 0, "claimant", f.feeAddr, 1000}},
		{"the refund's claimant to the vendor's claim address", []any{f.refund, 0, "claimant", f.claim, 1000}},
	} {
		xmrRefused(t, e, c.what, payeeRefusal, out, c.args...)
	}
	// The one replacement path (buildXMRSettlement): while the wallet's refusal of the recorded fee address is the
	// settlement's wait class, the market fee may go to the operator's new fee address, which the database cannot name.
	agExec(e, "UPDATE xmr_settlements SET send_error=$2 WHERE id=$1", f.release, xmrDestRefused+"-market_fee")
	accepted("a replaced market fee address while the fee address is refused", f.release, 1, "market_fee", other, 1000)
	xmrRefused(t, e, "a claimant to another address while the fee address is refused", payeeRefusal, out, f.release, 0, "claimant", other, 1000)

	// The watcher's own builds pass the trigger: the release pays the claim address and the fee destination, the
	// refund the refund address.
	agExec(e, "UPDATE xmr_settlements SET send_error='' WHERE id=$1", f.release)
	f.x.mustPoll()
	for _, c := range []struct {
		id    int64
		roles []string
	}{{f.release, []string{"claimant:" + f.claim, "market_fee:" + f.feeAddr}}, {f.refund, []string{"claimant:" + refundDest}}} {
		var got []string
		for _, o := range f.x.outputsOf(c.id) {
			got = append(got, o[:strings.LastIndex(o, ":")])
		}
		if s := f.x.settlement(c.id); s.State != "relayed" || !slices.Equal(got, c.roles) {
			t.Fatalf("settlement %d: %+v outputs %v, want relayed paying %v", c.id, s, got, c.roles)
		}
	}
}

func TestEscrowRefundAddressFrozen(t *testing.T) {
	f := newPayeeFixture(t)
	e := f.x.testEnv
	order := agStr(e, "SELECT order_id FROM xmr_settlements WHERE id=$1", f.release)
	other := testXMRAddr("redirected refund", false)
	for _, c := range []struct{ what, q string }{
		{"a changed refund address", "UPDATE escrows SET refund_address=$2 WHERE order_id=$1"},
		{"a changed fee destination", "UPDATE escrows SET fee_destination=$2 WHERE order_id=$1"},
	} {
		tx, err := e.DB.Begin()
		if err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(c.q, order, other)
		tx.Rollback()
		if code := pgCode(err); code != "23514" {
			t.Errorf("%s: %v (SQLSTATE %q), want check_violation", c.what, err, code)
		}
	}
	xmrRefused(t, e, "a cleared fee destination", "keeps the refund address and fee destination", "UPDATE escrows SET fee_destination='' WHERE order_id=$1", order)
	// Unchanged values and the escrow's other columns are written as before.
	agExec(e, "UPDATE escrows SET refund_address=refund_address,fee_destination=fee_destination,reminded_at=now() WHERE order_id=$1", order)
	if got := agStr(e, "SELECT refund_address FROM escrows WHERE order_id=$1", order); got != f.refundAddr {
		t.Fatalf("refund address %s, want %s", got, f.refundAddr)
	}
}
