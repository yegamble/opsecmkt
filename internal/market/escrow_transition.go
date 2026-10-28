package market

import (
	"context"
	"database/sql"
)

// The escrow transition hook is shared by Bitcoin and Monero escrow (A-565): it sets a paid escrow order's ship-by
// time, and opens a delivered Monero escrow order's dispute window (A-709) or completes a delivered Bitcoin one on
// funding. Monero escrow depends on it, so it lives apart from either chain's watcher. Its Monero effects are pinned by
// TestXMREscrowNoShipCancel and TestXMREscrowWindowAndLateDeposit (ship_by of a physical order),
// TestXMREscrowDigitalManualDeliveryWindow (ship_by of a digital order, the window opened on delivery) and
// TestXMREscrowDigitalDisputeWindow (the window opened on funding). Hooks run in registration order; by file name this
// init still runs after escrow_amounts.go's and before orders_hooks.go's, as it did from escrow_watch.go.

func init() { registerTransitionHook(escrowTransitionHook) }

const (
	// digitalDeliveryHours is a digital Monero escrow order's delivery deadline, from payment (A-709, owner decision
	// 2026-10-28): a digital listing has no dispatch window, so the deadline is fixed. Once it has passed with the order
	// still paid, the buyer may cancel it for a refund, as for a physical order past its ship-by time (noShipCancel).
	digitalDeliveryHours = 72
	// disputeWindowHours is a delivered digital Monero escrow order's dispute window, from delivery (A-709): the buyer
	// may open a dispute or confirm receipt until escrows.release_at; then the watcher completes the order
	// (xmrReleaseDue) and the vendor may claim. A digital escrow has no release_days snapshot (its CHECK is 7 to 30
	// days), so the window is fixed, not the administrator's release deadline setting.
	disputeWindowHours = 48
)

// escrowTransitionHook keeps escrow orders on the moves Phase 1 supports. Cancelling an escrow order that holds coins
// requests its refund, and resolving its dispute requests the settlement of the decision (settleTerminal); only an
// administrator resolves it (resolveAction). A paid physical escrow order gets its ship-by time and a paid digital
// Monero one its delivery deadline (ship_by too). A delivered Monero escrow order gets its dispute window (release_at),
// with an order note and a notice to each party; a delivered Bitcoin escrow order completes at once, by the system
// (the escrow-only edge; ES-4).
func escrowTransitionHook(ctx context.Context, a *App, tx *sql.Tx, o *Order, from, to string) error {
	if !o.Escrow {
		return nil
	}
	switch {
	case to == statePaid && o.Kind == "physical":
		// P2-R5: the buyer may cancel from the paid time plus the escrow's dispatch window plus 48 hours (escrow_deadline.go).
		_, err := tx.ExecContext(ctx, "UPDATE escrows SET ship_by=now()+make_interval(days => dispatch_days)+interval '48 hours' WHERE order_id=$1 AND dispatch_days IS NOT NULL AND ship_by IS NULL", o.ID)
		return err
	case to == statePaid && o.Kind == "digital" && o.Currency == "XMR":
		// A-709: set for every paid digital order; one delivered automatically in this transaction never uses it.
		_, err := tx.ExecContext(ctx, "UPDATE escrows SET ship_by=now()+make_interval(hours => $2) WHERE order_id=$1 AND ship_by IS NULL", o.ID, digitalDeliveryHours)
		return err
	case to == stateDelivered && o.Currency == "XMR":
		return openDisputeWindow(ctx, tx, o)
	case to == stateDelivered:
		next, err := a.transition(ctx, tx, o.ID, stateDelivered, stateCompleted, nil, escrowCompletedNote)
		if err != nil {
			return err
		}
		o.State, o.Status = next.State, next.Status
	}
	return nil
}
