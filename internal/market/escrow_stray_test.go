package market

import (
	"net/url"
)

func (x *escrowEnv) recover(order, sess, password string) (int, string) {
	w := x.do("POST", "/escrow/recover", sess, url.Values{"order_id": {order}, "password": {password}})
	return w.Code, w.Body.String()
}

// stray is order's newest stray refund (0 when none).
func (x *escrowEnv) stray(order string) (int64, string) {
	x.t.Helper()
	var id int64
	var state string
	x.DB.QueryRow("SELECT id,state FROM settlements WHERE order_id=$1 AND kind='stray_refund' ORDER BY id DESC LIMIT 1", order).Scan(&id, &state)
	return id, state
}

// uncounted counts the A-428 notes on order (leaveOutUncounted's "not spent by any refund" wording): order notes, the buyer's notices and the
// staff's notices.
func (x *escrowEnv) uncounted(order string) (notes, buyer, staff int) {
	x.t.Helper()
	const part = "not spent by any refund of this order"
	notes = agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND strpos(note,$2)>0", order, part)
	buyer = agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1 AND strpos(body,$2)>0 AND strpos(body,$3)>0", x.buyer, shortID(order), part)
	staff = agInt(x.testEnv, "SELECT count(*) FROM notifications n JOIN users u ON u.id=n.user_id WHERE u.role IN ('moderator','admin') AND strpos(n.body,$1)>0 AND strpos(n.body,$2)>0",
		"Payment review needed for order "+shortID(order), part)
	return
}

const (
	heldWant = "held at the escrow address, because the coins there are too few to return after the network fee"
	heldHelp = "tries the stray refund once more"
)

// strayTail is what a cancelled order's status says once a stray refund returns its coins (A-435), after "It is" (a
// deposit) or "The coins in this escrow are" (a refund that failed too-small), by the stray refund's state.
func strayTail(state, it string) string {
	switch state {
	case "broadcast":
		return "being returned to the buyer's refund address by the stray refund below, which is broadcast and waits for confirmations."
	case "confirmed":
		return "back at the buyer's refund address: the stray refund below that returned " + it + " is confirmed."
	}
	return "held at the escrow address until the stray refund below, which returns " + it + " to the buyer's refund address, is signed and broadcast."
}

const tooFewStatus = "Cancelled; the market could not build the refund. The coins in this escrow are "
