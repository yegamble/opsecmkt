package market

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// The pooled wallet's drain panel (A-445, war-room ruling P4-R9). After a coin's cutover to escrow only (A-444) the
// pooled wallet is to be removed by a later release (A-448), planned to refuse a database pooled_drain_blockers() (migration 091)
// still lists anything for. The administrator's panel on /admin shows that list read-only: a count per kind, never
// capped, and the first readinessListLimit of each kind, each linked to where it is resolved. Beside it, the pooled
// wallets' balances as the wallets report them, as advice only: the list decides, not the balance, and the market never
// sweeps the pooled wallet, so the operator moves what remains by hand once the list is empty. A balance that cannot be
// read (no pooled wallet, a node or wallet that is down or refuses getbalances) is a plain "unavailable" line, never an
// error page.

func init() { registerLoader("admin", pooledDrainLoader) }

// drainBalanceTimeout bounds one wallet's balance read while /admin renders.
const drainBalanceTimeout = 5 * time.Second

// drainKinds are the blocker kinds of pooled_drain_blockers, in the panel's order, with what the panel says of each.
var drainKinds = []struct{ kind, title, resolve string }{
	{"open_order", "Open pooled orders",
		"Each is closed by its buyer and vendor, through a dispute, or by the payment watcher (an unpaid order expires). An order links to its page only where you may read it: as a party, in a dispute or with a flagged payment."},
	{"unsent_payout", "Payouts not sent",
		"Resolve each under Payouts: the watcher sends a queued one (not while outbound payments are paused for a restore, nor without its pooled wallet configured); release, requeue or mark sent a held, failed or stuck one. A blocked one is sent once its recipient saves a payout address; to pay it by hand instead, hold it, pay it, then record it as sent."},
	{"flagged_payment", "Flagged pooled payments without a recorded review",
		"Record each review (no action with the reason, or refunded by hand) on the moderation desk; an escalated flag waits for an administrator who is not a party to its order."},
	{"watched_address", "Pooled deposit addresses still watched",
		"The watcher reads each until 24 hours after its order closed or 30 days after it was issued, whichever is later. After that it keeps reading a deposit recorded there until it confirms (then flagged for review), the wallet stops reporting it, or 14 days pass after both the horizon and the deposit's first observation while it remains unconfirmed (then flagged for staff review). Record the review on the moderation desk; a deposit sent after the watch ends is never read."},
	{"recovery_required", "Restore reconciliation not finished",
		"Payouts have been paused since a restore from backup. Reconcile the restored orders and payments with the wallets and clear the lock as the recovery runbook says (docs/testnet-runbook.md); nothing on this page clears it."},
	{"address_collision", "Issued-address warnings not dismissed",
		"Follow \"Restore the wallets\" in docs/testnet-runbook.md, then dismiss the warning under Payment providers."},
	{"mainnet_history", "Pooled history on mainnet",
		"This market issued pooled deposit addresses on mainnet, and no release has mainnet escrow to replace them, so the pooled wallet cannot be removed from it."},
}

func pooledDrainLoader(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	if d.User == nil || d.User.Role != "admin" {
		return nil
	}
	v := &PooledDrainView{Limit: readinessListLimit}
	byKind := map[string]*DrainKind{}
	for _, k := range drainKinds {
		byKind[k.kind] = &DrainKind{Kind: k.kind, Title: k.title, Resolve: k.resolve}
		v.Kinds = append(v.Kinds, byKind[k.kind])
	}
	// readable: the order page opens for this administrator, as paymentsAdminLoader decides it for a pooled order (a
	// party, a dispute, or a flagged payment; staffReviewer's escrow staff alert concerns escrow orders only); resolvable:
	// the payout's resolve form is on this page (a payout that needs attention, as paymentsAdminLoader marks it). Both are
	// read for the listed rows only, after the per-kind cap.
	rows, err := a.db.QueryContext(ctx, `SELECT x.kind,x.ref,x.order_id,x.currency,x.detail,
			COALESCE(o.buyer_id=$1 OR p.vendor_id=$1 OR o.state IN ('disputed','resolved') OR EXISTS(SELECT 1 FROM payments pm WHERE pm.order_id=o.id AND pm.flagged),false),
			COALESCE(po.state IN ('pending','blocked','held','failed') OR (po.state='sending' AND po.updated < now()-interval '5 minutes'),false),x.n
		FROM (SELECT b.kind,b.ref,COALESCE(b.order_id,'') AS order_id,COALESCE(b.currency,'') AS currency,b.detail,
				count(*) OVER (PARTITION BY b.kind) AS n,
				row_number() OVER (PARTITION BY b.kind ORDER BY CASE WHEN b.kind IN ('unsent_payout','flagged_payment') THEN lpad(b.ref,20,'0') ELSE b.ref END) AS rn
			FROM pooled_drain_blockers() b) x
		LEFT JOIN orders o ON o.id=x.order_id LEFT JOIN products p ON p.id=o.product_id
		LEFT JOIN payouts po ON x.kind='unsent_payout' AND po.id::text=x.ref
		WHERE x.rn<=$2 ORDER BY x.kind,x.rn`, d.User.ID, readinessListLimit)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var it DrainItem
		var kind string
		var readable, resolvable bool
		var n int
		if err = rows.Scan(&kind, &it.Ref, &it.Order, &it.Currency, &it.Detail, &readable, &resolvable, &n); err != nil {
			return err
		}
		k := byKind[kind]
		if k == nil { // a kind a later migration adds: counted and listed, under its name
			k = &DrainKind{Kind: kind, Title: kind}
			byKind[kind] = k
			v.Kinds = append(v.Kinds, k)
		}
		if k.Count == 0 {
			k.Count = n
			v.Total += n
		}
		if it.Order != "" {
			it.OrderShort = shortID(it.Order)
		}
		switch {
		case kind == "unsent_payout" && resolvable:
			it.Link = "/admin#resolve-payout-" + it.Ref
		case kind == "unsent_payout":
			it.Link = "/admin#payouts"
		case kind == "flagged_payment":
			it.Link = "/moderator#payment-review-" + it.Ref
		case kind == "recovery_required" || kind == "address_collision":
			it.Link = "/admin#payment-providers"
		case (kind == "open_order" || kind == "watched_address") && readable:
			it.Link = "/order?id=" + it.Order
		}
		k.Items = append(k.Items, it)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	// Both wallets at once, so a wallet that hangs delays the page by drainBalanceTimeout at most.
	v.Balances = make([]DrainBalance, 2)
	var wg sync.WaitGroup
	for i, cur := range []string{"BTC", "XMR"} {
		wg.Go(func() { v.Balances[i] = a.drainBalance(ctx, cur) })
	}
	wg.Wait()
	d.PooledDrain = v
	return nil
}

// drainBalance reads cur's pooled wallet balance for the panel; any failure leaves it unavailable.
func (a *App) drainBalance(ctx context.Context, cur string) DrainBalance {
	b := DrainBalance{Currency: cur, Configured: a.pooledConfigured(cur)}
	p := a.provider(cur)
	r, ok := p.(balanceReporter)
	if !b.Configured || !ok {
		return b
	}
	b.Network = p.Network()
	rctx, cancel := context.WithTimeout(ctx, drainBalanceTimeout)
	defer cancel()
	confirmed, pending, err := r.Balance(rctx)
	if err != nil {
		return b
	}
	dec := currencyDecimals(cur)
	b.Available, b.Confirmed, b.Pending = true, amount(confirmed, dec), amount(pending, dec)
	return b
}
