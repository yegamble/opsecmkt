package market

import (
	"strings"
)

const keysUnavailableText = "cannot be signed right now"

// siblingStray is a completed per-order escrow order with a deposit to its sibling /0/<k>, and that deposit's stray
// refund, built and awaiting a signature.
func (x *escrowEnv) siblingStray(label string, k int) (string, int64) {
	x.t.Helper()
	order := x.completed(regtestAddr(label+"-refund"), 100000)
	x.core.deposit(x.sibling(order, 0, k), txidOf(label), 0, 30000, 6)
	x.poll() // recorded and the stray refund requested
	x.poll() // built
	ids, states := x.strayRefunds(order)
	if len(ids) != 1 || states[0] != "awaiting_signature" {
		x.t.Fatalf("stray refunds %v %v, want one awaiting a signature", ids, states)
	}
	return order, ids[0]
}

// hasSignForm reports whether page offers the settlement sign form.
func hasSignForm(page string) bool { return strings.Contains(page, `action="/escrow/sign"`) }
