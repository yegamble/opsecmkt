package market

import (
	"regexp"
	"strings"
)

const (
	heldReason          = "A credited deposit is conflicted or below the confirmation threshold; the payment watcher holds this payout and releases it by itself once the deposit confirms again."
	suspendedHoldPrefix = "Suspended account:"
	largeHoldPrefix     = "Larger than the order price:"
	restoredHoldPrefix  = "Restored from backup:"
	handHoldPrefix      = "Held to be paid by hand:"
	stuckSending        = "(state='sending' AND updated < now()-interval '5 minutes')"
)

func heldLarge(state, payoutErr string) bool {
	return state == "held" && strings.Contains(payoutErr, largeHoldPrefix)
}
func heldForSuspension(state, payoutErr string) bool {
	return state == "held" && strings.Contains(payoutErr, suspendedHoldPrefix)
}
func heldForHand(state, payoutErr string) bool {
	return state == "held" && strings.Contains(payoutErr, handHoldPrefix)
}

var txidPattern = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
