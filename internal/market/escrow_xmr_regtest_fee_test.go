package market

import (
	"context"
	"maps"
	"strconv"
	"testing"
	"time"
)

// TestXMREscrowRegtestFeeAndReturn (A-673; scripts/test-escrow-xmr-regtest.sh, run by the escrow-xmr-regtest CI job): the
// one leg whose App charges a market fee. It runs with ESCROW_XMR_FEE_BPS=250 and ESCROW_XMR_FEE_ADDRESS a subaddress of
// the funder's wallet, so the leg reads what each payee received. A digital order is funded with its price and 0.002 XMR
// more in one real transaction and completes on funding; its release (as the vendor's claim requests it) is the first
// transaction a real monero-wallet-rpc builds with three destinations and subtract_fee_from_outputs [0]: the vendor's
// claim address, the market fee and the buyer's overpayment back to the refund address. The leg checks that the build
// passed checkXMRBuild and migration 150's plan rules (it was stored and relayed after one transfer call), that exactly
// three xmr_settlement_outputs rows were stored (claimant, market_fee, buyer_return), and, once the release is confirmed,
// that the funder's get_transfers lists exactly what the plan says at each address: the fee address marketFeeSats(price,
// 250), the refund address the excess, and the claim address the input total less the network fee, the fee and the
// excess. A real wallet that disagrees with the plan fails this leg; that is a payments finding, never a test to adjust.
func TestXMREscrowRegtestFeeAndReturn(t *testing.T) {
	const feeBps, excess = 250, 2000000000 // 2.5 %; 0.002 XMR paid above the price
	var feeAddr string
	r, p := newXMRSettleRegtest(t, func(r *xmrRegtest) map[string]string {
		feeAddr = r.funderAddress()
		return map[string]string{"ESCROW_XMR_FEE_BPS": strconv.Itoa(feeBps), "ESCROW_XMR_FEE_ADDRESS": feeAddr}
	})
	order, addr := r.checkoutReal()
	var price, bps int64
	var refund, feeDest string
	if err := r.DB.QueryRow("SELECT o.amount,e.fee_bps,e.fee_destination,e.refund_address FROM orders o JOIN escrows e ON e.order_id=o.id WHERE o.id=$1", order).
		Scan(&price, &bps, &feeDest, &refund); err != nil {
		t.Fatal(err)
	}
	fee := marketFeeSats(price, feeBps)
	// Each extra output must reach the output floor, or the plan folds it into the vendor's and the leg proves nothing.
	if price != xmrListingPrice || bps != feeBps || feeDest != feeAddr || fee < minPayoutXMR || excess < minPayoutXMR {
		t.Fatalf("the escrow recorded price %d, fee rate %d, fee destination %q (want %q); market fee %d", price, bps, feeDest, feeAddr, fee)
	}
	funding := r.pay(map[string]int64{addr: price + excess})
	r.mine(10 + xmrMineMargin)
	r.mustPoll()
	if r.state(order) != stateCompleted || agInt64(t, r.testEnv, "SELECT COALESCE(sum(amount),0) FROM payments WHERE order_id=$1 AND txid=$2 AND credited", order, funding) != price+excess {
		t.Fatalf("funded with %d piconero: %s (deposits %v)", price+excess, r.state(order), r.depositConfs(order))
	}
	t.Logf("order %s: funded with %s XMR (price %s XMR) by %s, completed", shortID(order), amount(price+excess, 12), amount(price, 12), funding)

	id := r.releaseReal(order)
	claim := agStr(r.testEnv, "SELECT destination FROM xmr_settlements WHERE id=$1", id)
	r.mustPoll()
	s := r.settlement(id)
	if s.State != "relayed" || s.InputTotal != price+excess || p.count("transfer") != 1 || p.count("relay_tx") != 1 {
		// The market discards a build that disagrees with its plan; build the plan once more, unrelayed, so the failure
		// shows the wallet's own fee and amounts_by_dest (a payments finding: report these amounts, never adjust the leg).
		var dry struct {
			Fee           int64 `json:"fee"`
			AmountsByDest struct {
				Amounts []int64 `json:"amounts"`
			} `json:"amounts_by_dest"`
		}
		account := agInt64(t, r.testEnv, "SELECT s.account_index FROM escrows es JOIN xmr_escrow_slots s ON s.id=es.xmr_slot_id WHERE es.order_id=$1", order)
		dests := []map[string]any{{"address": claim, "amount": price - fee}, {"address": feeAddr, "amount": fee}, {"address": refund, "amount": excess}}
		dryErr := p.wallet.callWithin(context.Background(), 2*time.Minute, "/json_rpc", "transfer", map[string]any{"destinations": dests, "account_index": account,
			"priority": 0, "subtract_fee_from_outputs": []int{0}, "do_not_relay": true}, &dry)
		t.Fatalf("release %+v (wait %q), transfer %d, relay_tx %d, last transfer error %v; events %q; the plan built again unrelayed: fee %d, amounts_by_dest %v (planned %d, %d, %d before the fee), error %v",
			s, r.waitClass(id), p.count("transfer"), p.count("relay_tx"), p.transferFailure(), r.events(order), dry.Fee, dry.AmountsByDest.Amounts, price-fee, fee, excess, dryErr)
	}
	toClaim := s.InputTotal - s.Fee - fee - excess
	want := []string{"claimant:" + claim + ":" + strconv.FormatInt(toClaim, 10), "market_fee:" + feeAddr + ":" + strconv.FormatInt(fee, 10),
		"buyer_return:" + refund + ":" + strconv.FormatInt(excess, 10)}
	if got := r.outputsOf(id); len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("stored outputs of release %d (fee %d):\n got %v\nwant %v", id, s.Fee, got, want)
	}
	r.xmrShape(s)
	r.confirmReal(id)

	// What each funder subaddress received from the release, as the funder's wallet lists it.
	r.call(r.funder, "refresh", nil, nil)
	var res struct {
		In []struct {
			TxID          string `json:"txid"`
			Address       string `json:"address"`
			Amount        int64  `json:"amount"`
			Confirmations int64  `json:"confirmations"`
		} `json:"in"`
	}
	r.call(r.funder, "get_transfers", map[string]any{"in": true, "account_index": 0}, &res)
	got := map[string]int64{}
	for _, in := range res.In {
		if in.TxID != s.TxHash {
			continue
		}
		if in.Confirmations < r.A.escrowXMR.confirmations {
			t.Fatalf("the funder lists %s's output to %s… with %d confirmations", s.TxHash, in.Address[:12], in.Confirmations)
		}
		got[in.Address] += in.Amount
	}
	if arrived := map[string]int64{claim: toClaim, feeAddr: fee, refund: excess}; !maps.Equal(got, arrived) {
		t.Fatalf("the funder's get_transfers for %s (input total %d, network fee %d):\n got %v\nwant %v", s.TxHash, s.InputTotal, s.Fee, got, arrived)
	}
	t.Logf("release %s confirmed: claim address received %s XMR (input %s less network fee %s, market fee %s at %d bps and the excess %s), fee address %s XMR, refund address %s XMR; one transfer, one relay_tx",
		s.TxHash, amount(toClaim, 12), amount(s.InputTotal, 12), amount(s.Fee, 12), amount(fee, 12), feeBps, amount(excess, 12), amount(fee, 12), amount(excess, 12))
}
