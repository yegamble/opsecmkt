package market

import (
	"context"
	"database/sql"
)

// The escrow transition hook is shared by Bitcoin and Monero escrow (A-565): it sets a paid physical escrow order's
// ship-by time and completes a delivered one on funding. Monero escrow depends on it, so it lives apart from either
// chain's watcher. Its Monero effects are pinned by TestXMREscrowNoShipCancel and TestXMREscrowWindowAndLateDeposit
// (ship_by), and TestXMREscrowFundingThreshold and TestXMREscrowDigitalClaim (completion on funding, Monero note).
// Hooks run in registration order; by file name this init still runs after escrow_amounts.go's and before
// orders_hooks.go's, as it did from escrow_watch.go.

func init() { registerTransitionHook(escrowTransitionHook) }

// escrowTransitionHook keeps escrow orders on the moves Phase 1 supports. Cancelling an escrow order that holds coins
// requests its refund, and resolving its dispute requests the settlement of the decision (settleTerminal); only an
// administrator resolves it (resolveAction). A delivered escrow order completes at once, by the system (the
// escrow-only edge; ES-4: digital orders complete on funding). A paid physical escrow order gets its ship-by time.
func escrowTransitionHook(ctx context.Context, a *App, tx *sql.Tx, o *Order, from, to string) error {
	if !o.Escrow {
		return nil
	}
	switch {
	case to == statePaid && o.Kind == "physical":
		// P2-R5: the buyer may cancel from the paid time plus the escrow's dispatch window plus 48 hours (escrow_deadline.go).
		_, err := tx.ExecContext(ctx, "UPDATE escrows SET ship_by=now()+make_interval(days => dispatch_days)+interval '48 hours' WHERE order_id=$1 AND dispatch_days IS NOT NULL AND ship_by IS NULL", o.ID)
		return err
	case to == stateDelivered:
		note := escrowCompletedNote
		if o.Currency == "XMR" {
			note = xmrEscrowCompletedNote
		}
		next, err := a.transition(ctx, tx, o.ID, stateDelivered, stateCompleted, nil, note)
		if err != nil {
			return err
		}
		o.State, o.Status = next.State, next.Status
	}
	return nil
}
