package market

import (
	"database/sql"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// A-652a fix rounds (PAY89-6, PAY91-1), migration 150: a Monero settlement's outputs pay only the addresses its order
// recorded, only a release has a market fee or an overpayment return, the market fee is exactly the fee recorded at
// checkout, a re-request repeats its stored plan, a built plan has the shape and sums planXMRSettlement gives
// (TestSettlementPlanFitsAtBuild), and an escrow's refund address, fee destination and fee rate never change. DB
// integration (SQL against the migrated schema, rolled back); the watcher's own builds through these triggers are the
// last step of TestSettlementOutputsMatchPayees and the Monero escrow tests (TestXMREscrowBuildRefusals covers the
// replaced fee address, TestXMREscrowReRequestGuards a re-request of the stored plan). A-666 (PAY92-1, PAY92-2),
// migration 151: an order's amount never changes once it has an escrow (TestOrderAmountFrozenWithEscrow), and a refund
// or stray refund pays only its escrow's refund address (TestRefundDestinationPinned).

const (
	payeeRefusal    = "pays only its recorded address"
	feeKindRefusal  = "only a release pays a market fee or returns an overpayment"
	feeAmtRefusal   = "the fee recorded at checkout"
	payeeOutputSQL  = "INSERT INTO xmr_settlement_outputs(settlement_id,n,role,address,amount) VALUES($1,$2,$3,$4,$5)"
	xmr999          = int64(999_000_000_000_000) // 999 XMR in piconero (the payments seat's probes)
	refusedFeeClass = xmrDestRefused + "-market_fee"
	amountFrozen    = "has an escrow; its amount, which the market fee and any overpayment return follow, never changes" // 151
	refundDestPin   = "pays only the refund address its escrow recorded at checkout"                                     // 151
)

// payeeFixture is a completed order of a market with a 2.5% fee, with a requested release to the vendor's claim
// address, and a cancelled order with its requested refund: the release, its claim address, the refund, the escrows'
// refund and fee addresses, and the release's market fee (marketFeeSats of the order amount at the escrow's rate).
// small is a third order, completed at a price of twice the output floor (its market fee below the floor), with no
// settlement requested.
type payeeFixture struct {
	x                          *xmrEscrowEnv
	release, refund            int64
	claim, refundAddr, feeAddr string
	fee                        int64
	small                      string
}

func newPayeeFixture(t *testing.T) payeeFixture {
	t.Helper()
	x := newXMREscrowEnv(t)
	t.Setenv("ESCROW_XMR_FEE_BPS", "250")
	t.Setenv("ESCROW_XMR_FEE_ADDRESS", testXMRAddr("payee fee", false))
	x.restart()
	x.mustPoll()
	// The orders are funded (each by a pass) before any settlement is requested, so no pass builds them. The small
	// order's price is set on its listing before checkout: an order's amount never changes once it has an escrow (151).
	cancelled, _ := x.paidXMRPhysical(x.physical(x.vendor))
	cheap := x.digital(x.vendor)
	agExec(x.testEnv, "UPDATE products SET xmr=$2 WHERE id=$1", cheap, 2*minPayoutXMR)
	small, smallAddr := x.freshCheckout(cheap)
	x.fake.payAddress(smallAddr, hex64("deposit "+small), 2*minPayoutXMR, 10)
	order, _, _ := x.completed()
	if x.state(small) != stateCompleted || agInt64(t, x.testEnv, "SELECT amount FROM orders WHERE id=$1", small) != 2*minPayoutXMR {
		t.Fatalf("the small order: %s", x.state(small))
	}
	f := payeeFixture{x: x, small: small}
	f.release, f.claim = x.release(order)
	f.refundAddr = agStr(x.testEnv, "SELECT refund_address FROM escrows WHERE order_id=$1", order)
	f.feeAddr = agStr(x.testEnv, "SELECT fee_destination FROM escrows WHERE order_id=$1", order)
	f.fee = marketFeeSats(agInt64(t, x.testEnv, "SELECT amount FROM orders WHERE id=$1", order), agInt64(t, x.testEnv, "SELECT fee_bps FROM escrows WHERE order_id=$1", order))
	if f.feeAddr == "" || f.fee <= 0 {
		t.Fatalf("the escrow recorded no fee: %q, %d", f.feeAddr, f.fee)
	}
	x.forceDeadline(cancelled, "ship_by=now()-interval '1 minute'")
	if code, body := x.xmrBuyerCancel(cancelled, x.sessOf(cancelled, roleBuyer)); code != 303 {
		t.Fatalf("buyer's cancellation: %d %s", code, body)
	}
	f.refund = x.settlementOf(cancelled, "refund").ID
	return f
}

// payeeAccepted inserts one output in a rolled-back transaction and fails if the database refuses it.
func payeeAccepted(t *testing.T, e *testEnv, what string, args ...any) {
	t.Helper()
	tx, err := e.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(payeeOutputSQL, args...); err != nil {
		t.Errorf("%s: refused: %v", what, err)
	}
}

func TestSettlementOutputsMatchPayees(t *testing.T) {
	f := newPayeeFixture(t)
	e := f.x.testEnv
	refundDest := agStr(e, "SELECT destination FROM xmr_settlements WHERE id=$1", f.refund)
	other := testXMRAddr("someone else", false)
	payeeAccepted(t, e, "the release's claimant to the claim address", f.release, 0, "claimant", f.claim, 1000)
	payeeAccepted(t, e, "the release's buyer return to the refund address", f.release, 1, "buyer_return", f.refundAddr, 1000)
	payeeAccepted(t, e, "the release's market fee to the fee destination", f.release, 1, "market_fee", f.feeAddr, f.fee)
	payeeAccepted(t, e, "the refund's claimant to the refund address", f.refund, 0, "claimant", refundDest, 1000)
	for _, c := range []struct {
		what, want string
		args       []any
	}{
		{"the release's claimant to the fee address", payeeRefusal, []any{f.release, 0, "claimant", f.feeAddr, 1000}},
		{"the release's claimant to another address", payeeRefusal, []any{f.release, 0, "claimant", other, 1000}},
		{"the release's buyer return to the claim address", payeeRefusal, []any{f.release, 1, "buyer_return", f.claim, 1000}},
		{"the release's market fee to the refund address", payeeRefusal, []any{f.release, 1, "market_fee", f.refundAddr, f.fee}},
		{"the release's market fee to another address", payeeRefusal, []any{f.release, 1, "market_fee", other, f.fee}},
		{"the refund's claimant to the fee address", payeeRefusal, []any{f.refund, 0, "claimant", f.feeAddr, 1000}},
		{"the refund's claimant to the vendor's claim address", payeeRefusal, []any{f.refund, 0, "claimant", f.claim, 1000}},
		// The payments seat's probes (PAY91-1).
		{"a refund with a 999 XMR market fee to the fee destination", feeKindRefusal, []any{f.refund, 1, "market_fee", f.feeAddr, xmr999}},
		{"a refund with its exact market fee to the fee destination", feeKindRefusal, []any{f.refund, 1, "market_fee", f.feeAddr, f.fee}},
		{"a refund returning an overpayment", feeKindRefusal, []any{f.refund, 1, "buyer_return", refundDest, 1000}},
		{"a release with a 999 XMR market fee", feeAmtRefusal, []any{f.release, 1, "market_fee", f.feeAddr, xmr999}},
	} {
		xmrRefused(t, e, c.what, c.want, payeeOutputSQL, c.args...)
	}
	// The one replacement path (buildXMRSettlement): while the wallet's refusal of the recorded fee address is the
	// settlement's wait class, the market fee may go to the operator's new fee address, which the database cannot name.
	agExec(e, "UPDATE xmr_settlements SET send_error=$2 WHERE id=$1", f.release, refusedFeeClass)
	payeeAccepted(t, e, "a replaced market fee address while the fee address is refused", f.release, 1, "market_fee", other, f.fee)
	xmrRefused(t, e, "a claimant to another address while the fee address is refused", payeeRefusal, payeeOutputSQL, f.release, 0, "claimant", other, 1000)
	// The seat's third probe: the refusal class on a refund opens nothing.
	agExec(e, "UPDATE xmr_settlements SET send_error=$2 WHERE id=$1", f.refund, refusedFeeClass)
	xmrRefused(t, e, "a refund marked fee-refused with a 999 XMR market fee to another address", feeKindRefusal, payeeOutputSQL, f.refund, 1, "market_fee", other, xmr999)

	// The watcher's own builds pass the trigger: the release pays the claim address and the recorded fee to the fee
	// destination, the refund the refund address.
	agExec(e, "UPDATE xmr_settlements SET send_error='' WHERE id IN ($1,$2)", f.release, f.refund)
	f.x.mustPoll()
	for _, c := range []struct {
		id      int64
		outputs []string
	}{{f.release, []string{"claimant:" + f.claim, "market_fee:" + f.feeAddr + ":" + strconv.FormatInt(f.fee, 10)}}, {f.refund, []string{"claimant:" + refundDest}}} {
		got := f.x.outputsOf(c.id)
		got[0] = got[0][:strings.LastIndex(got[0], ":")] // the claimant's amount is less the network fee
		if s := f.x.settlement(c.id); s.State != "relayed" || !slices.Equal(got, c.outputs) {
			t.Fatalf("settlement %d: %+v outputs %v, want relayed paying %v", c.id, s, got, c.outputs)
		}
	}
}

// TestMarketFeeAmountPinned (PAY91-1): a release's market fee output is exactly marketFeeSats(orders.amount,
// escrows.fee_bps), one piconero more or less is refused, and the fee-address replacement path changes the address
// it may pay, never the amount.
func TestMarketFeeAmountPinned(t *testing.T) {
	f := newPayeeFixture(t)
	e := f.x.testEnv
	other := testXMRAddr("replacement fee", false)
	payeeAccepted(t, e, "the recorded fee", f.release, 1, "market_fee", f.feeAddr, f.fee)
	for _, amt := range []int64{f.fee - 1, f.fee + 1, 2 * f.fee, xmr999} {
		xmrRefused(t, e, "a market fee of "+strconv.FormatInt(amt, 10), feeAmtRefusal, payeeOutputSQL, f.release, 1, "market_fee", f.feeAddr, amt)
	}
	agExec(e, "UPDATE xmr_settlements SET send_error=$2 WHERE id=$1", f.release, refusedFeeClass)
	payeeAccepted(t, e, "the recorded fee to the replacement address", f.release, 1, "market_fee", other, f.fee)
	for _, amt := range []int64{f.fee + 1, xmr999} {
		xmrRefused(t, e, "a replaced fee address with a fee of "+strconv.FormatInt(amt, 10), feeAmtRefusal, payeeOutputSQL, f.release, 1, "market_fee", other, amt)
	}
}

func TestEscrowRefundAddressFrozen(t *testing.T) {
	f := newPayeeFixture(t)
	e := f.x.testEnv
	order := agStr(e, "SELECT order_id FROM xmr_settlements WHERE id=$1", f.release)
	other := testXMRAddr("redirected refund", false)
	for _, c := range []struct {
		what, q string
		arg     any
	}{
		{"a changed refund address", "UPDATE escrows SET refund_address=$2 WHERE order_id=$1", other},
		{"a changed fee destination", "UPDATE escrows SET fee_destination=$2 WHERE order_id=$1", other},
		{"a changed fee rate", "UPDATE escrows SET fee_bps=$2 WHERE order_id=$1", 9999},
	} {
		tx, err := e.DB.Begin()
		if err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(c.q, order, c.arg)
		tx.Rollback()
		if code := pgCode(err); code != "23514" {
			t.Errorf("%s: %v (SQLSTATE %q), want check_violation", c.what, err, code)
		}
	}
	xmrRefused(t, e, "a cleared fee destination", "keeps the refund address, fee destination and fee rate", "UPDATE escrows SET fee_destination='' WHERE order_id=$1", order)
	// Unchanged values and the escrow's other columns are written as before.
	agExec(e, "UPDATE escrows SET refund_address=refund_address,fee_destination=fee_destination,fee_bps=fee_bps,reminded_at=now() WHERE order_id=$1", order)
	if got := agStr(e, "SELECT refund_address FROM escrows WHERE order_id=$1", order); got != f.refundAddr {
		t.Fatalf("refund address %s, want %s", got, f.refundAddr)
	}
}

// planOut is one output row of a plan built by hand.
type planOut struct {
	role, addr string
	amt        int64
}

// buildPlan writes outs as settlement id's plan and stores its build (requested to built) with network fee fee, input
// total input and key image keyImage, in tx, as storeXMRBuilt does.
func buildPlan(tx *sql.Tx, id int64, label string, fee, input int64, keyImage string, outs ...planOut) error {
	for i, o := range outs {
		if _, err := tx.Exec(payeeOutputSQL, id, i, o.role, o.addr, o.amt); err != nil {
			return err
		}
	}
	_, err := tx.Exec("UPDATE xmr_settlements SET state='built',tx_hash=$2,tx_metadata='ab01',tx_blob='cd01',fee=$3,input_total=$4,key_images=ARRAY[$5] WHERE id=$1",
		id, txidOf(label), fee, input, keyImage)
	return err
}

// planCase runs steps in a rolled-back transaction: accepted when want is "", else refused with check_violation and want.
func planCase(t *testing.T, e *testEnv, what, want string, steps func(*sql.Tx) error) {
	t.Helper()
	tx, err := e.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	err = steps(tx)
	switch {
	case want == "" && err != nil:
		t.Errorf("%s: refused: %v", what, err)
	case want != "" && (pgCode(err) != "23514" || !strings.Contains(err.Error(), want)):
		t.Errorf("%s: %v (SQLSTATE %q), want check_violation by %q", what, err, pgCode(err), want)
	}
}

// TestSettlementPlanFitsAtBuild (PAY91-1): when a settlement is built, its recorded plan is the one planXMRSettlement
// gives. Every shape it plans is accepted (a release with the market fee, with it folded below the floor, with and
// without an overpayment return, with the return folded; a refund; a fee paid to the replacement address; a re-request
// of the stored plan), and each departure is refused with check_violation: a missing or unplanned market fee, a missing,
// unplanned or wrong return, outputs that do not add up to the input total, and a re-request that does not repeat its
// stored plan. The floor is minPayoutXMR, so the migration's copy of it is pinned here.
func TestSettlementPlanFitsAtBuild(t *testing.T) {
	f := newPayeeFixture(t)
	e := f.x.testEnv
	order := agStr(e, "SELECT order_id FROM xmr_settlements WHERE id=$1", f.release)
	price := agInt64(t, e, "SELECT amount FROM orders WHERE id=$1", order)
	refundDest := agStr(e, "SELECT destination FROM xmr_settlements WHERE id=$1", f.refund)
	const nf = int64(30_000_000) // the network fee, taken from the claimant's output
	ki := txidOf("plan key image")
	fee := planOut{"market_fee", f.feeAddr, f.fee}
	ret := func(n int64) planOut { return planOut{"buyer_return", f.refundAddr, n} }
	// releaseOf(id, claim) builds release id from input, the claimant's (claim's) output what is left after the others
	// and the network fee; release builds f.release.
	releaseOf := func(id int64, claim string) func(int64, ...planOut) func(*sql.Tx) error {
		return func(input int64, others ...planOut) func(*sql.Tx) error {
			return func(tx *sql.Tx) error {
				left := input - nf
				for _, o := range others {
					left -= o.amt
				}
				return buildPlan(tx, id, "plan release", nf, input, ki, append([]planOut{{"claimant", claim, left}}, others...)...)
			}
		}
	}
	release := releaseOf(f.release, f.claim)
	// small builds the release of the small order, whose fee folds below the floor (requested here, unbuilt: no pass
	// runs), and smallRet returns its overpayment to its refund address.
	smallID, smallClaim := f.x.release(f.small)
	small := releaseOf(smallID, smallClaim)
	smallRefund := agStr(e, "SELECT refund_address FROM escrows WHERE order_id=$1", f.small)
	smallRet := func(n int64) planOut { return planOut{"buyer_return", smallRefund, n} }
	smallFee := marketFeeSats(2*minPayoutXMR, 250)
	if f.fee < minPayoutXMR || smallFee >= minPayoutXMR || smallFee == 0 {
		t.Fatalf("fixture fees %d and %d do not straddle the floor %d", f.fee, smallFee, minPayoutXMR)
	}
	const (
		sums     = "add up to its input total"
		feeShape = "has a market fee output exactly when"
		retShape = "returns its overpayment"
	)
	for _, c := range []struct {
		what, want string
		steps      func(*sql.Tx) error
	}{
		// Every shape planXMRSettlement gives.
		{"a release with the market fee", "", release(price, fee)},
		{"a release with the market fee and an overpayment return", "", release(price+5*minPayoutXMR, fee, ret(5*minPayoutXMR))},
		{"a release returning an overpayment of exactly the floor", "", release(price+minPayoutXMR, fee, ret(minPayoutXMR))},
		{"a release with an overpayment below the floor folded", "", release(price+minPayoutXMR-1, fee)},
		{"a release with its fee below the floor folded", "", small(2 * minPayoutXMR)},
		{"a release with its fee folded and an overpayment return", "", small(4*minPayoutXMR, smallRet(2*minPayoutXMR))},
		{"an underpaid release (no return)", "", release(price-minPayoutXMR, fee)},
		{"a refund paying everything to the refund address", "", func(tx *sql.Tx) error {
			return buildPlan(tx, f.refund, "plan refund", nf, price, ki, planOut{"claimant", refundDest, price - nf})
		}},
		{"a replaced market fee address while the fee address is refused", "", func(tx *sql.Tx) error {
			if _, err := tx.Exec("UPDATE xmr_settlements SET send_error=$2 WHERE id=$1", f.release, refusedFeeClass); err != nil {
				return err
			}
			return release(price, planOut{"market_fee", testXMRAddr("replacement fee", false), f.fee})(tx)
		}},
		{"a build stored without outputs (as the tests' bare builds are)", "", func(tx *sql.Tx) error { return buildPlan(tx, f.release, "plan bare", nf, price, ki) }},

		// Departures.
		{"a release without its market fee", feeShape, release(price)},
		{"a release with a market fee that folds below the floor", feeShape, small(2*minPayoutXMR, planOut{"market_fee", f.feeAddr, smallFee})},
		{"a release keeping an overpayment of the floor", retShape, release(price+minPayoutXMR, fee)},
		{"a release returning an overpayment below the floor", retShape, release(price+minPayoutXMR-1, fee, ret(minPayoutXMR-1))},
		{"a release returning one piconero more than the overpayment", retShape, release(price+5*minPayoutXMR, fee, ret(5*minPayoutXMR+1))},
		{"a release returning one piconero less than the overpayment", retShape, release(price+5*minPayoutXMR, fee, ret(5*minPayoutXMR-1))},
		{"a release returning 999 XMR of a 1000 XMR overpayment", retShape, release(price+xmr999+1_000_000_000_000, fee, ret(xmr999))},
		{"a claimant output one piconero above the input total", sums, func(tx *sql.Tx) error {
			return buildPlan(tx, f.release, "plan over", nf, price, ki, planOut{"claimant", f.claim, price - f.fee - nf + 1}, fee)
		}},
		{"a claimant output below the rest of the input total", sums, func(tx *sql.Tx) error {
			return buildPlan(tx, f.release, "plan under", nf, price, ki, planOut{"claimant", f.claim, minPayoutXMR}, fee)
		}},
		{"a plan without a claimant output", sums, func(tx *sql.Tx) error { return buildPlan(tx, f.release, "plan no claimant", nf, f.fee+nf, ki, fee) }},
		{"a refund paying the network fee twice", sums, func(tx *sql.Tx) error {
			return buildPlan(tx, f.refund, "plan refund twice", nf, price, ki, planOut{"claimant", refundDest, price - 2*nf})
		}},
	} {
		planCase(t, e, c.what, c.want, c.steps)
	}

	// A re-request builds the stored plan of the settlement it supersedes: the same other outputs and input total.
	input := price + 5*minPayoutXMR
	// reRequestWith builds f.release from input with the market fee output feeOut and the return (to an address other than
	// the recorded fee destination: the replacement path, the recorded one refused), fails and supersedes it, inserts its
	// re-request next and runs steps; reRequestOf with the recorded fee.
	reRequestWith := func(feeOut planOut, steps func(tx *sql.Tx, next int64) error) func(*sql.Tx) error {
		return func(tx *sql.Tx) error {
			if feeOut.addr != f.feeAddr {
				if _, err := tx.Exec("UPDATE xmr_settlements SET send_error=$2 WHERE id=$1", f.release, refusedFeeClass); err != nil {
					return err
				}
			}
			if err := release(input, feeOut, ret(5*minPayoutXMR))(tx); err != nil {
				return err
			}
			for _, st := range []string{"failed", "superseded"} {
				if _, err := tx.Exec("UPDATE xmr_settlements SET state=$2 WHERE id=$1", f.release, st); err != nil {
					return err
				}
			}
			var next int64
			if err := tx.QueryRow("INSERT INTO xmr_settlements(order_id,kind,destination,supersedes) VALUES($1,'release',$2,$3) RETURNING id", order, f.claim, f.release).Scan(&next); err != nil {
				return err
			}
			return steps(tx, next)
		}
	}
	reRequestOf := func(steps func(tx *sql.Tx, next int64) error) func(*sql.Tx) error { return reRequestWith(fee, steps) }
	again := func(input int64, others ...planOut) func(*sql.Tx, int64) error {
		return func(tx *sql.Tx, next int64) error {
			left := input - 2*nf
			for _, o := range others {
				left -= o.amt
			}
			return buildPlan(tx, next, "plan again", 2*nf, input, ki, append([]planOut{{"claimant", f.claim, left}}, others...)...)
		}
	}
	const (
		storedPlan = "to its stored plan"
		repeats    = "repeats the stored plan"
	)
	other := testXMRAddr("re-request fee", false)
	for _, c := range []struct {
		what, want string
		steps      func(*sql.Tx, int64) error
	}{
		{"a re-request of the stored plan (another network fee)", "", again(input, fee, ret(5*minPayoutXMR))},
		// The order's amount, which a fresh plan would follow, never changes once it has an escrow (151).
		{"a re-request after the order amount changed", amountFrozen, func(tx *sql.Tx, next int64) error {
			if _, err := tx.Exec("UPDATE orders SET amount=amount/2 WHERE id=$1", order); err != nil {
				return err
			}
			return again(input, fee, ret(5*minPayoutXMR))(tx, next)
		}},
		{"a re-request dropping the overpayment return", storedPlan, again(input, fee)},
		{"a re-request with another input total", storedPlan, again(input+minPayoutXMR, fee, ret(5*minPayoutXMR))},
		{"a re-request with another market fee", repeats, again(input, planOut{"market_fee", f.feeAddr, f.fee + 1}, ret(5*minPayoutXMR))},
		{"a re-request paying the market fee elsewhere", repeats, again(input, planOut{"market_fee", other, f.fee}, ret(5*minPayoutXMR))},
		{"a re-request marked fee-refused paying the market fee elsewhere", repeats, func(tx *sql.Tx, next int64) error {
			if _, err := tx.Exec("UPDATE xmr_settlements SET send_error=$2 WHERE id=$1", next, refusedFeeClass); err != nil {
				return err
			}
			return again(input, planOut{"market_fee", other, f.fee}, ret(5*minPayoutXMR))(tx, next)
		}},
		{"a re-request returning more", repeats, again(input, fee, ret(5*minPayoutXMR+1))},
	} {
		planCase(t, e, c.what, c.want, reRequestOf(c.steps))
	}
	// The stored plan, not a fresh one: a release built to the replacement fee address is repeated to that address,
	// which a fresh plan (the escrow's recorded fee destination) would not pay, and not to the recorded one.
	replaced := planOut{"market_fee", other, f.fee}
	planCase(t, e, "a re-request of a stored plan paying the replacement fee address", "", reRequestWith(replaced, again(input, replaced, ret(5*minPayoutXMR))))
	planCase(t, e, "a re-request of that plan paying the recorded fee destination", repeats, reRequestWith(replaced, again(input, fee, ret(5*minPayoutXMR))))
}

// TestOrderAmountFrozenWithEscrow (PAY92-1, migration 151): once an order has an escrow its amount never changes, so the
// market fee and overpayment return that migration 150 derives from it cannot take the vendor's share. The payments
// seat's probe (a completed order with a requested release priced at 19.88 XMR, so that the fee is most of the escrow),
// lowering the amount (the vendor's share returned to the buyer) and one piconero either way are refused with
// check_violation, on orders with a requested release, a requested refund and no settlement. Writing the same amount
// with other columns, a draft's amount and an order without an escrow are accepted, and the watcher then pays the
// release the fee recorded at checkout. Checkout writes the amount while the order is a draft, before it inserts the
// escrow (escrow_xmr_create.go): the fixture's checkouts and TestXMRQuantityCanBuyAllRemainingStock pass through it.
func TestOrderAmountFrozenWithEscrow(t *testing.T) {
	f := newPayeeFixture(t)
	x, e := f.x, f.x.testEnv
	order := agStr(e, "SELECT order_id FROM xmr_settlements WHERE id=$1", f.release)
	cancelled := agStr(e, "SELECT order_id FROM xmr_settlements WHERE id=$1", f.refund)
	price := agInt64(t, e, "SELECT amount FROM orders WHERE id=$1", order)
	set := func(order string, amount int64) func(*sql.Tx) error {
		return func(tx *sql.Tx) error {
			_, err := tx.Exec("UPDATE orders SET amount=$2 WHERE id=$1", order, amount)
			return err
		}
	}
	for _, c := range []struct {
		what   string
		steps  func(*sql.Tx) error
		accept bool
	}{
		{"the probe: a requested release's order priced at 19.88 XMR", set(order, 19_880_000_000_000), false},
		{"a requested release's order at half its price", set(order, price/2), false},
		{"one piconero more", set(order, price+1), false},
		{"one piconero less", set(order, price-1), false},
		{"a cancelled order with its requested refund", set(cancelled, 2*price), false},
		{"a completed order without a settlement", set(f.small, price), false},
		{"in one statement with its state", func(tx *sql.Tx) error {
			_, err := tx.Exec("UPDATE orders SET amount=amount*2,state=state WHERE id=$1", order)
			return err
		}, false},
		{"the same amount with another column", func(tx *sql.Tx) error {
			_, err := tx.Exec("UPDATE orders SET amount=amount,updated=now() WHERE id=$1", order)
			return err
		}, true},
		{"a draft's amount", set(x.draft(x.product), price), true},
		{"an order without an escrow", set(x.order(x.buyer, x.product, "XMR", statePaid), price), true},
	} {
		want := amountFrozen
		if c.accept {
			want = ""
		}
		planCase(t, e, c.what, want, c.steps)
	}
	if got := agInt64(t, e, "SELECT amount FROM orders WHERE id=$1", order); got != price {
		t.Fatalf("the amount changed to %d", got)
	}
	// One watcher pass then relays the release paying the claim address and the fee recorded at checkout.
	x.mustPoll()
	if out := x.outputsOf(f.release); x.settlement(f.release).State != "relayed" || len(out) != 2 ||
		out[1] != "market_fee:"+f.feeAddr+":"+strconv.FormatInt(f.fee, 10) || !strings.HasPrefix(out[0], "claimant:"+f.claim+":") {
		t.Fatalf("the release: %s %v", x.settlement(f.release).State, out)
	}
}

// TestRefundDestinationPinned (PAY92-2, migration 151): a refund or stray refund is inserted only to its escrow's
// refund address, so no build can pay an escrow to another address as a refund. The payments seat's probes are refused
// with check_violation when the row is inserted: (a) a stray refund to an unrelated address, built paying it everything,
// and (b) the requested refund superseded and a refund to that address inserted and built; so are a refund to another
// order's refund address, the fee destination and the vendor's claim address. The legitimate rows are accepted: a stray
// refund and a refund to the refund address, built, and a re-request (the SQL of escrow_xmr_rerequest.go), which copies
// the destination of the refund it supersedes.
func TestRefundDestinationPinned(t *testing.T) {
	f := newPayeeFixture(t)
	e := f.x.testEnv
	order := agStr(e, "SELECT order_id FROM xmr_settlements WHERE id=$1", f.release)
	cancelled := agStr(e, "SELECT order_id FROM xmr_settlements WHERE id=$1", f.refund)
	refundAddr := agStr(e, "SELECT refund_address FROM escrows WHERE order_id=$1", cancelled)
	other := testXMRAddr("unpinned refund", false)
	const nf, input = int64(30_000_000), int64(xmrListingPrice)
	ki := txidOf("pinned refund key image")
	// inserted inserts a kind of settlement of ord to dest (after superseding the requested refund, if supersede) and
	// builds it paying dest everything.
	inserted := func(ord, kind, dest string, supersede bool) func(*sql.Tx) error {
		return func(tx *sql.Tx) error {
			if supersede {
				if _, err := tx.Exec("UPDATE xmr_settlements SET state='superseded' WHERE id=$1", f.refund); err != nil {
					return err
				}
			}
			var id int64
			if err := tx.QueryRow("INSERT INTO xmr_settlements(order_id,kind,destination) VALUES($1,$2,$3) RETURNING id", ord, kind, dest).Scan(&id); err != nil {
				return err
			}
			return buildPlan(tx, id, "pinned refund", nf, input, ki, planOut{"claimant", dest, input - nf})
		}
	}
	for _, c := range []struct {
		what, want string
		steps      func(*sql.Tx) error
	}{
		{"probe (a): a stray refund to an unrelated address, built", refundDestPin, inserted(order, "stray_refund", other, false)},
		{"a stray refund to another order's refund address", refundDestPin, inserted(order, "stray_refund", refundAddr, false)},
		{"a stray refund to the fee destination", refundDestPin, inserted(order, "stray_refund", f.feeAddr, false)},
		{"a stray refund to the vendor's claim address", refundDestPin, inserted(order, "stray_refund", f.claim, false)},
		{"probe (b): the refund superseded and a refund to an unrelated address, built", refundDestPin, inserted(cancelled, "refund", other, true)},
		{"the refund superseded and a refund to another order's refund address", refundDestPin, inserted(cancelled, "refund", f.refundAddr, true)},
		{"a stray refund to an unrelated address beside the requested refund", refundDestPin, inserted(cancelled, "stray_refund", other, false)},

		{"a stray refund to the refund address, built", "", inserted(order, "stray_refund", f.refundAddr, false)},
		{"the refund superseded and a refund to the refund address, built", "", inserted(cancelled, "refund", refundAddr, true)},
		{"a stray refund beside the requested refund, to the refund address", "", inserted(cancelled, "stray_refund", refundAddr, false)},
		{"a re-request of the built refund, failed and superseded", "", func(tx *sql.Tx) error {
			if err := buildPlan(tx, f.refund, "pinned refund", nf, input, ki, planOut{"claimant", refundAddr, input - nf}); err != nil {
				return err
			}
			for _, st := range []string{"failed", "superseded"} {
				if _, err := tx.Exec("UPDATE xmr_settlements SET state=$2 WHERE id=$1", f.refund, st); err != nil {
					return err
				}
			}
			var next int64
			if err := tx.QueryRow(`INSERT INTO xmr_settlements(order_id,kind,destination,requested_by,supersedes) SELECT order_id,kind,destination,NULL,id FROM xmr_settlements WHERE id=$1
				RETURNING id`, f.refund).Scan(&next); err != nil {
				return err
			}
			return buildPlan(tx, next, "pinned refund again", 2*nf, input, ki, planOut{"claimant", refundAddr, input - 2*nf})
		}},
	} {
		planCase(t, e, c.what, c.want, c.steps)
	}
}
