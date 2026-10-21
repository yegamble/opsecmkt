package market

import (
	"context"
	"net/http"
)

// The escrow watcher (WR-escrow Phase 1, slice P1-b). One per deployment (its own PostgreSQL advisory lock): it reads what the watch-only wallet received at each watched escrow address and writes it
// to the payments ledger, one row per output (applyIncoming; payments.settlement_id stays NULL until a settlement
// claims the output, P1-c). An escrow order awaiting payment whose confirmed outputs reach the price moves to paid;
// autoDeliveryHook delivers the content and escrowTransitionHook completes the order on funding through the
// escrow-only system edge delivered -> completed. Each move goes through transition (row lock, compare-and-set,
// order_events).
//
// No payouts row is ever written for an escrow order (settleTerminal; migration 074 refuses one). What returns coins
// from an escrow is a settlement (escrow_settle.go): the watcher builds each requested claim or refund at the end of
// its pass (buildSettlements), then co-signs party-signed ones, stores and broadcasts them and follows them to their
// confirmation (escrow_cosign.go, slice P1-c2). A dispute on an escrow order is decided by an administrator and its
// decision is paid by a settlement (escrow_decision.go). An escrow order that has received nothing when its payment
// window ends is cancelled; one underpaid then is cancelled too and its refund requested. A confirmed deposit
// first recorded after an escrow order closed is flagged for payment review (flagLateDeposits); one that did not count
// when the order was cancelled and has confirmed since is left out for the stray refund (leaveOutUncounted, A-428).
//
// While escrow is switched on but not enabled (its node was down at startup, or on another chain than the one pinned for
// the database's escrows, say) the watcher retries the startup checks each interval (retryEscrow) and does nothing else
// but apply the release deadlines, which need no node (escrow_deadline.go): no deposit is read and no order expires
// until they pass. The same holds for each pass while the node is not synced (A-439, bitcoin_sync.go).

const escrowWatcherLock = 782495

// xmrEscrowWatcherLock is the Monero escrow watcher's session advisory lock (escrow_xmr_watch.go), apart from the Bitcoin escrow watcher, so
// a slow Monero wallet pass never holds up the Bitcoin escrow watcher.
const xmrEscrowWatcherLock = 782496

// escrowNetwork is the chain the escrow node reported at startup, for the network word in escrow notes (regtest, signet
// or testnet4). While escrow is not enabled it is the chain the escrow node last reported in this process, else the
// chain this database's escrows are pinned to (notePinnedNet, A-443), else "regtest" (no node has answered and nothing
// is pinned or held: a market that has never run escrow, which has no escrow to write a note about). Pages name a chain
// only while escrowNetKnown.
func (a *App) escrowNetwork() string {
	if e := a.escrow.Load(); e != nil && e.network != "" {
		return e.network
	}
	if net := a.escrowNet.Load(); net != nil {
		return *net
	}
	return "regtest"
}

// escrowNetKnown reports whether escrowNetwork's chain is one an escrow node reported or this database's escrows are on
// (QA4W6-8): not while a market that has never run escrow waits for its node's first answer. Until then PageData's
// EscrowNet is "" and the pages say the chain is not verified yet, as Monero escrow's do (QA4W4B-1); on a mainnet
// market it is "main" (paymentsGlobalLoader, A-540), the only chain escrow can start on there.
func (a *App) escrowNetKnown() bool {
	return a.escrow.Load() != nil || a.escrowNet.Load() != nil
}

func init() {
	registerLoader("admin", escrowNodeLoader)
}

// escrowCompletedNote is the order history note of the completion on funding.
const escrowCompletedNote = "Completed on funding (Bitcoin escrow): the content was delivered. The vendor claims the escrowed coins from the order page."

// escrowNodeLoader shows on /admin, under Payment providers, why the Bitcoin escrow node is not used: the reason class
// it is not synced for (with syncRemedy's advice when there is one), the pinned chain and the node's when
// startEscrow refused it, or why the arbiter wallet's
// entries are not read (QA4W4A-1); in APP_MODE=tor why the
// Tor-mode gate holds broadcasts; and the settlements queued or stuck (A-441, escrow_stall.go).
func escrowNodeLoader(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	if d.User == nil || d.User.Role != "admin" {
		return nil
	}
	v := &EscrowNodeView{Configured: a.escrowConfigured, Mismatch: a.escrowMismatch.Load(), StallN: escrowStallBlocks}
	var err error
	if v.Health, err = escrowHealthView(ctx, a.db); err != nil {
		return err
	}
	if !a.escrowConfigured {
		if v.Health != nil {
			d.EscrowNode = v
		}
		return nil
	}
	if why := a.escrowArbiterRead.Load(); why != nil {
		v.ArbiterRead = *why
		if *why != "waiting" {
			v.ArbiterNext = a.arbiterNextCheck() // A-479: the next read waits after a slow one
		}
	}
	if e := a.escrow.Load(); e != nil {
		v.NotSynced, _ = a.escrowSync.Load().(string)
		v.Remedy = syncRemedy(e.network, v.NotSynced)
		v.TorHold = a.broadcastHold()
	}
	if v.Stalled, err = a.escrowStalls(ctx); err != nil {
		return err
	}
	if v.Health != nil || v.NotSynced != "" || v.Mismatch != nil || v.ArbiterRead != "" || v.TorHold != "" || len(v.Stalled) > 0 {
		d.EscrowNode = v
	}
	return nil
}
