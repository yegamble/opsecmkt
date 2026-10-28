package market

import (
	"context"
	"maps"
	"os"
	"slices"
	"strconv"
	"testing"
	"time"
)

// TestXMREscrowRegtestFeeAndReturn (A-673, A-681; scripts/test-escrow-xmr-regtest.sh, run by the escrow-xmr-regtest CI
// job): a release that returns an overpayment, on a real monero-wallet-rpc. Each case funds a digital order with its
// price and 0.002 XMR more in one real transaction (delivered on funding; completed when its dispute window is ended by
// the deadline step) and requests its release as the vendor's
// claim does; claim, refund and fee addresses are subaddresses of the funder's wallet, so the leg reads what each payee
// received.
//   - "fee and return": the one App that charges a market fee (ESCROW_XMR_FEE_BPS=250). The release has three
//     destinations, built with subtract_fee_from_outputs [0]: the vendor's claim address, the market fee and the buyer's
//     return. monero-wallet-rpc 0.18.5.1 lists their amounts_by_dest as claimant, buyer_return, market_fee, not in the
//     order asked (CI run 40545579717); until A-681 the market refused that build as a mismatch, so a market with a fee
//     could release nothing.
//   - "return only": no market fee; the release has two destinations, the claim address and the buyer's return.
//
// Each case checks the build passed checkXMRBuild and migration 150's plan rules (stored and relayed after one transfer
// call), that the xmr_settlement_outputs rows are exactly the planned roles, addresses and amounts, and, once the release
// is confirmed, that the funder's get_transfers lists exactly the planned amount at each address: the fee address
// marketFeeSats(price, 250), the refund address the excess, and the claim address the input total less the network fee,
// the fee and the excess. A real wallet that disagrees with the plan fails the leg; that is a payments finding, never a
// test to adjust. A case that does not run to its end fails the leg, as the script fails a skipped leg.
func TestXMREscrowRegtestFeeAndReturn(t *testing.T) {
	for _, v := range []string{"ESCROW_XMR_REGTEST_DAEMON_URL", "ESCROW_XMR_REGTEST_WALLET_URL", "ESCROW_XMR_REGTEST_FUNDER_URL", "ESCROW_XMR_REGTEST_RESTORE_URL"} {
		if os.Getenv(v) == "" {
			t.Skip("set ESCROW_XMR_REGTEST_DAEMON_URL, _WALLET_URL, _FUNDER_URL and _RESTORE_URL (scripts/test-escrow-xmr-regtest.sh) to run against monerod --regtest")
		}
	}
	done := 0
	for _, c := range []struct {
		name string
		bps  int64
	}{{"fee and return", 250}, {"return only", 0}} {
		t.Run(c.name, func(t *testing.T) {
			xmrRegtestFeeAndReturn(t, c.bps)
			done++
		})
	}
	if done != 2 {
		t.Fatalf("%d of 2 cases ran to the end", done)
	}
}

// xmrRegtestFeeAndReturn is one case of TestXMREscrowRegtestFeeAndReturn, with a market fee of bps (0: none).
func xmrRegtestFeeAndReturn(t *testing.T, bps int64) {
	const excess = 2000000000 // 0.002 XMR paid above the price
	var feeAddr string
	r, p := newXMRSettleRegtest(t, func(r *xmrRegtest) map[string]string {
		if bps == 0 {
			return nil
		}
		feeAddr = r.funderAddress()
		return map[string]string{"ESCROW_XMR_FEE_BPS": strconv.FormatInt(bps, 10), "ESCROW_XMR_FEE_ADDRESS": feeAddr}
	})
	order, addr := r.checkoutReal()
	var price, recordedBps int64
	var refund, feeDest string
	if err := r.DB.QueryRow("SELECT o.amount,e.fee_bps,e.fee_destination,e.refund_address FROM orders o JOIN escrows e ON e.order_id=o.id WHERE o.id=$1", order).
		Scan(&price, &recordedBps, &feeDest, &refund); err != nil {
		t.Fatal(err)
	}
	fee := marketFeeSats(price, bps)
	// Each extra output must reach the output floor, or the plan folds it into the vendor's and the case proves nothing.
	if price != xmrListingPrice || recordedBps != bps || feeDest != feeAddr || bps > 0 && fee < minPayoutXMR || excess < minPayoutXMR {
		t.Fatalf("the escrow recorded price %d, fee rate %d, fee destination %q (want %q); market fee %d", price, recordedBps, feeDest, feeAddr, fee)
	}
	funding := r.pay(map[string]int64{addr: price + excess})
	r.mine(10 + xmrMineMargin)
	r.mustPoll()
	r.endWindows()
	if r.state(order) != stateCompleted || agInt64(t, r.testEnv, "SELECT COALESCE(sum(amount),0) FROM payments WHERE order_id=$1 AND txid=$2 AND credited", order, funding) != price+excess {
		t.Fatalf("funded with %d piconero: %s (deposits %v)", price+excess, r.state(order), r.depositConfs(order))
	}
	t.Logf("order %s: funded with %s XMR (price %s XMR) by %s, completed", shortID(order), amount(price+excess, 12), amount(price, 12), funding)

	id := r.releaseReal(order)
	claim := agStr(r.testEnv, "SELECT destination FROM xmr_settlements WHERE id=$1", id)
	// The plan, in the order the market asks the wallet: the claimant's output before the network fee, the market fee,
	// the buyer's return.
	planned := []settleOutput{{"claimant", claim, price - fee}}
	if bps > 0 {
		planned = append(planned, settleOutput{"market_fee", feeAddr, fee})
	}
	planned = append(planned, settleOutput{"buyer_return", refund, excess})
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
		var dests []map[string]any
		for _, o := range planned {
			dests = append(dests, map[string]any{"address": o.Address, "amount": o.Sats})
		}
		dryErr := p.wallet.callWithin(context.Background(), 2*time.Minute, "/json_rpc", "transfer", map[string]any{"destinations": dests, "account_index": account,
			"priority": 0, "subtract_fee_from_outputs": []int{0}, "do_not_relay": true}, &dry)
		t.Fatalf("release %+v (wait %q), transfer %d, relay_tx %d, last transfer error %v; events %q; the plan %v built again unrelayed: fee %d, amounts_by_dest %v, error %v",
			s, r.waitClass(id), p.count("transfer"), p.count("relay_tx"), p.transferFailure(), r.events(order), planned, dry.Fee, dry.AmountsByDest.Amounts, dryErr)
	}
	toClaim := s.InputTotal - s.Fee - fee - excess
	var want []string
	arrived := map[string]int64{}
	for i, o := range planned {
		if i == 0 {
			o.Sats = toClaim
		}
		want = append(want, o.Role+":"+o.Address+":"+strconv.FormatInt(o.Sats, 10))
		arrived[o.Address] += o.Sats
	}
	if got := r.outputsOf(id); !slices.Equal(got, want) {
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
	if !maps.Equal(got, arrived) {
		t.Fatalf("the funder's get_transfers for %s (input total %d, network fee %d):\n got %v\nwant %v", s.TxHash, s.InputTotal, s.Fee, got, arrived)
	}
	t.Logf("release %s confirmed with %d outputs: claim address received %s XMR (input %s less network fee %s, market fee %s at %d bps and the excess %s), refund address %s XMR, fee address %s XMR; one transfer, one relay_tx",
		s.TxHash, len(planned), amount(toClaim, 12), amount(s.InputTotal, 12), amount(s.Fee, 12), amount(fee, 12), bps, amount(excess, 12), amount(excess, 12), amount(fee, 12))
}
