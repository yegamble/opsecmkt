package market

import (
	"regexp"

	"strings"
)

// escrowAddr is order's Bitcoin escrow address.
func (x *escrowEnv) escrowAddr(order string) string {
	x.t.Helper()
	return agStrings(x.t, x.testEnv, "SELECT address FROM escrows WHERE order_id=$1", order)[0]
}

var flaggedAt = regexp.MustCompile(`^<tr class="payment-review-row">\s*<td>\d{4}-\d{2}-\d{2} \d{2}:\d{2} UTC</td>`)

// deskRow is the moderation desk's payment-review row of output 0 of txid, or "" when it is not listed.
func deskRow(desk, txid string) string {
	i := strings.Index(desk, txid+":0")
	if i < 0 {
		return ""
	}
	lo, hi := strings.LastIndex(desk[:i], `<tr class="payment-review-row">`), strings.Index(desk[i:], "</tr>")
	if lo < 0 || hi < 0 {
		return ""
	}
	return desk[lo : i+hi]
}
