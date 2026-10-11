package market

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Incoming is one received output from an escrow wallet.
type Incoming struct {
	Address, TxID string
	Index         int64
	Amount        int64
	Confirmations int64
	Locked        bool
}

const fundedOpenStates = "'paid','shipped','delivered','disputed'"

type ledgerKey struct {
	txid string
	idx  int64
}

var watcherPassTimeout = 5 * time.Minute

// watchError labels a watcher error. Error() keeps the wrapped text (payment_status, the admin pages), but the
// watcher's log line prints only the label and the class (watcherCause), never the wallet's or the driver's
// text, which can hold a payout address or the database user, name, host and port (A-170). A label holds
// only a currency, "order <short id>", "payout <id>" and fixed words; an empty label adds nothing to Error().
type watchError struct {
	label string
	class string // logged instead of watcherCause(err) when set
	err   error
}

func (e *watchError) Error() string {
	if e.label == "" {
		return e.err.Error()
	}
	return e.label + ": " + e.err.Error()
}

func (e *watchError) Unwrap() error { return e.err }

// watcherCause is the one-line log form of a watcher error: each joined error in turn ("; ") with all its
// labels, then the JSON-RPC code of a wallet error (rpc:<code>) or errorCause's class.
func watcherCause(err error) string { return strings.Join(watcherCauses(err), "; ") }

func watcherCauses(err error) []string {
	switch e := err.(type) {
	case interface{ Unwrap() []error }:
		var parts []string
		for _, c := range e.Unwrap() {
			parts = append(parts, watcherCauses(c)...)
		}
		return parts
	case *watchError:
		parts := []string{e.class}
		if e.class == "" {
			parts = watcherCauses(e.err)
		}
		if e.label != "" {
			for i := range parts {
				parts[i] = e.label + ": " + parts[i]
			}
		}
		return parts
	}
	var re *rpcError
	if errors.As(err, &re) {
		return []string{fmt.Sprintf("rpc:%d", re.Code)}
	}
	return []string{errorCause(err)}
}

func currencyDecimals(currency string) int {
	if currency == "XMR" {
		return 12
	}
	return 8
}

// applyIncoming writes what a wallet reported for addresses to the payments ledger, one row per output; ledger rows
// the wallet no longer reports (replaced, conflicted or evicted transactions) are marked -1 confirmations. A new row
// on a funded, non-terminal order is marked late_notice for settleOrder to announce; one on a closed order is marked
// after_close: it is never counted toward the order's settlement (payoutBasis), and settleOrder flags it. The pooled
// watcher and the escrow watcher (escrow_watch.go) both write the ledger through it.
func (a *App) applyIncoming(ctx context.Context, cur string, incoming []Incoming, addresses []string, orderOf map[string]string) error {
	seen := map[ledgerKey]bool{}
	for _, in := range incoming {
		order, ok := orderOf[in.Address]
		if !ok || in.TxID == "" || len(in.TxID) > 128 || in.Amount <= 0 {
			continue
		}
		seen[ledgerKey{in.TxID, in.Index}] = true
		if _, err := a.upsertIncoming(ctx, cur, order, in); err != nil {
			return err
		}
	}
	return a.markMissing(ctx, cur, addresses, seen)
}

// upsertIncoming writes one output to the payments ledger for order (applyIncoming's row) and reports whether the row
// was inserted or changed. The Monero escrow watcher calls it row by row, so a row the database refuses is caught for
// that row alone (escrow_xmr_watch.go). An output to a sibling address of a shape-(b) Bitcoin escrow's order wallet
// (escrow_siblings, A-456a) is recorded left out from the start: it never counts for the order and the stray refund
// returns it once the order is closed.
func (a *App) upsertIncoming(ctx context.Context, cur, order string, in Incoming) (bool, error) {
	res, err := a.db.ExecContext(ctx, `INSERT INTO payments(order_id,currency,txid,idx,address,amount,confirmations,locked,late_notice,after_close,escrow_left_out)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,EXISTS(SELECT 1 FROM orders WHERE id=$1 AND state IN (`+fundedOpenStates+`)),
			EXISTS(SELECT 1 FROM orders WHERE id=$1 AND state IN ('cancelled','resolved','completed')),
			$2='BTC' AND EXISTS(SELECT 1 FROM escrow_siblings WHERE address=$5))
		ON CONFLICT (currency,txid,idx) DO UPDATE SET confirmations=excluded.confirmations, locked=excluded.locked, updated=now()
		WHERE (payments.confirmations,payments.locked) IS DISTINCT FROM (excluded.confirmations,excluded.locked)`, order, cur, in.TxID, in.Index, in.Address, in.Amount, in.Confirmations, in.Locked)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// markMissing marks -1 confirmations on the ledger rows at addresses that the wallet no longer reports (not in seen).
func (a *App) markMissing(ctx context.Context, cur string, addresses []string, seen map[ledgerKey]bool) error {
	rows, err := a.db.QueryContext(ctx, "SELECT id,txid,idx FROM payments WHERE currency=$1 AND address=ANY($2) AND confirmations>=0", cur, addresses)
	if err != nil {
		return err
	}
	var missing []int64
	for rows.Next() {
		var id, idx int64
		var txid string
		if err = rows.Scan(&id, &txid, &idx); err != nil {
			rows.Close()
			return err
		}
		if !seen[ledgerKey{txid, idx}] {
			missing = append(missing, id)
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for _, id := range missing {
		if _, err = a.db.ExecContext(ctx, "UPDATE payments SET confirmations=-1, updated=now() WHERE id=$1", id); err != nil {
			return err
		}
	}
	return nil
}

func isTerminal(state string) bool {
	return state == stateCompleted || state == stateResolved || state == stateCancelled
}
