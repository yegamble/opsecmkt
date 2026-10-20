package market

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// settlementFits reports whether a settlement of kind may be completed for an order in state with dispute outcome
// (ES-2: only in a terminal state, and the outcome the order reached): a release on a completed order or one resolved
// for the vendor, a refund on a cancelled order or one resolved for the buyer, a stray refund on any closed order.
func settlementFits(kind, state, outcome string) bool {
	switch kind {
	case "release":
		return state == stateCompleted || state == stateResolved && outcome == "release"
	case "refund":
		return state == stateCancelled || state == stateResolved && outcome == "refund"
	case "stray_refund":
		return isTerminal(state)
	}
	return false
}

// disputeOutcomeSQL is the outcome of the order o's dispute (empty if none; one dispute per order), for a query that has the orders row o.
const disputeOutcomeSQL = "COALESCE((SELECT d.outcome FROM disputes d WHERE d.order_id=o.id),'')"

// isPerOrderKey reports whether an escrow key row is a per-order (shape (b)) key: a three-step suffix, /<i>/0/0 (or a
// sibling's /<i>/<branch>/<k>), where a Bitcoin Core key has two, /0/<i>.
func isPerOrderKey(k escrowKeyRow) bool { return strings.Count(k.Suffix, "/") == 3 }

// settlementParties is the order's buyer and vendor.
func settlementParties(ctx context.Context, q rowQuerier, orderID string) (buyer, vendor, state string, err error) {
	err = q.QueryRowContext(ctx, "SELECT o.buyer_id,p.vendor_id,o.state FROM orders o JOIN products p ON p.id=o.product_id WHERE o.id=$1", orderID).Scan(&buyer, &vendor, &state)
	return
}

// settleNote writes an order note (no state change, the system as actor) and, when body is set, a notice of it to
// both parties.
func settleNote(ctx context.Context, tx *sql.Tx, orderID, note, body string) error {
	buyer, vendor, state, err := settlementParties(ctx, tx, orderID)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,$2,$2,NULL,$3)", orderID, state, note); err != nil {
		return err
	}
	if body == "" {
		return nil
	}
	for _, uid := range []string{buyer, vendor} {
		if _, err = insertNotification(ctx, tx, uid, "Order "+shortID(orderID)+": "+body, orderID); err != nil {
			return err
		}
	}
	return nil
}

// sendStored sends a settlement's stored raw transaction: true when the node has it (it answered its txid) or it is
// already confirmed (-27). While the Tor-mode gate holds broadcasts (broadcastHold, A-441) it sends nothing and answers
// errBroadcastHeld; its callers check the gate first.
func (a *App) sendStored(ctx context.Context, e *escrowBTC, rawHex, txid string) (bool, error) {
	if a.broadcastHold() != "" {
		return false, errBroadcastHeld
	}
	got, err := e.sendRawTransaction(ctx, rawHex, escrowMaxFeeRate)
	var re *escrowRPCError
	switch {
	case err == nil && got == txid:
		return true, nil
	case err == nil:
		return false, &escrowRPCError{Method: "sendrawtransaction", Class: "bad-reply"}
	case errors.As(err, &re) && re.Code == -27:
		return true, nil
	case errors.As(err, &re) && (re.Code == -25 || re.Code == -26):
		return false, nil // refused: the caller checks its inputs
	}
	return false, err
}

func firstTxID(ids []string) string {
	for _, id := range ids {
		if len(id) == 64 && isLowerHexFold(id) && strings.ToLower(id) == id {
			return id
		}
	}
	return ""
}

// escrowReorgDepth is how many confirmations a confirmed settlement's transaction needs before the watcher stops
// following it (trackConfirmeds). A depth, not a time: a reorganisation is a matter of blocks, and a node that was
// down still catches one up when it returns. 100 is Bitcoin's own coinbase maturity, the depth Core treats as final
// for newly mined coins; a deeper reorganisation is not handled here.
const escrowReorgDepth = 100
