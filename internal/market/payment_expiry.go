package market

import "time"

// A-15: an observed but never-confirming deposit may keep an awaiting-payment
// order open only for a fixed confirmation wait. PAYMENT_EXPIRY can lengthen
// this wait, but a short payment window cannot make it shorter than 14 days.
// The clock begins with the first eligible deposit ever recorded for this
// order, including one later marked missing or conflicted. New small deposits
// therefore cannot restart it. This is separate from A-487's review of a
// deposit at an address after its order has already closed.
const minimumConfirmationWait = 14 * 24 * time.Hour

func confirmationWait() time.Duration {
	return max(expiryWindow(), minimumConfirmationWait)
}

func confirmationDeadline(issued, firstSeen time.Time) time.Time {
	addressEnd := issued.Add(expiryWindow())
	firstDepositEnd := firstSeen.Add(confirmationWait())
	if addressEnd.After(firstDepositEnd) {
		return addressEnd
	}
	return firstDepositEnd
}
