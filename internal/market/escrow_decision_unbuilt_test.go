package market

import (
	"strconv"
)

// decisionRows is every release and refund of order, oldest first: kind, state, build error, the value it could spend
// (input_total, 0 when none) and whether it has a PSBT.
func (x *escrowEnv) decisionRows(order string) []string {
	return agStrings(x.t, x.testEnv, `SELECT kind||' '||state||' '||build_error||' '||COALESCE(input_total,0)||' '||(psbt IS NOT NULL) FROM settlements
		WHERE order_id=$1 AND kind IN ('release','refund') ORDER BY id`, order)
}

// decisionDeposits is every deposit of order by amount: amount, flagged, left out, whether a settlement spends it.
func (x *escrowEnv) decisionDeposits(order string) []string {
	return agStrings(x.t, x.testEnv, "SELECT amount||' '||flagged||' '||escrow_left_out||' '||(settlement_id IS NOT NULL) FROM payments WHERE order_id=$1 ORDER BY amount", order)
}

// countedAgain is the order note on n deposits worth sats that a changed decision counts for the order again (A-462).
func countedAgain(n int, sats int64) string {
	count, is, it := "1 deposit", "is", "it"
	if n > 1 {
		count, is, it = strconv.Itoa(n)+" deposits", "are", "them"
	}
	return count + " (" + amount(sats, 8) + " BTC, TESTNET regtest) to this order's escrow, left out for a stray refund when the refund of the earlier decision could not be built because the coins were too few, " +
		is + " counted for the order again: the release to the vendor includes " + it + ", and no stray refund returns " + it + "."
}
