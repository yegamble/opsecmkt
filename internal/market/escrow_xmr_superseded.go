package market

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strconv"
	"strings"
)

// A superseded transaction that reaches the network after all (A-566, PAY66-3; migration 144). An administrator's
// re-request (escrow_xmr_rerequest.go) supersedes a failed settlement that keeps its stored transaction, and the new
// settlement spends exactly the same key images. The failed transaction may still be mined: the daemon's refusal can
// follow a relay that took effect (monero-wallet-rpc answers -4 for a lost connection too), and a peer that kept it may
// send it back. The re-request can then never be mined, and its coins were not spent by a transaction the market did
// not make: they were spent by the market's own earlier transaction, which pays the stored plan. So before a
// re-request is built, before its stored transaction is relayed, when the wallet's coins no longer match it
// (xmrOutputsWait) and before it is recorded external (markXMRExternal), the watcher looks up the stored transactions of
// the settlements it re-requests; one the daemon has (in its pool or a block) is the outcome: in one transaction under
// the order row lock, the re-request is closed (superseded, never built or relayed again) and that settlement moves
// back to relayed with its deposit claims, so trackXMRRelayed follows it to confirmed and 100 confirmations, relays the
// same stored transaction again should the daemon lose it, and the order shows it as the release or refund. One order
// note (to the parties and staff) and a notice to each party and to the administrator who asked say so, with what it
// pays; nothing is flagged, since nothing is wrong with the escrow. A spend by any other transaction is still external.

// xmrSupersededSent reports whether a settlement re-request s re-requests (directly, or through earlier re-requests)
// has its stored transaction on the network, and then records it as the outcome (xmrAdoptSuperseded). It reports false
// for a settlement that re-requests nothing.
func (a *App) xmrSupersededSent(ctx context.Context, s *xmrSettleRow) (bool, error) {
	if s.Supersedes == 0 {
		return false, nil
	}
	rows, err := a.db.QueryContext(ctx, `WITH RECURSIVE earlier AS (
			SELECT id,supersedes,tx_hash,state FROM xmr_settlements WHERE id=$1
			UNION ALL
			SELECT p.id,p.supersedes,p.tx_hash,p.state FROM xmr_settlements p JOIN earlier e ON p.id=e.supersedes)
		SELECT id,tx_hash FROM earlier WHERE state='superseded' AND tx_hash IS NOT NULL ORDER BY id DESC`, s.Supersedes)
	if err != nil {
		return false, err
	}
	type earlier struct {
		id   int64
		hash string
	}
	var list []earlier
	for rows.Next() {
		var e earlier
		if err = rows.Scan(&e.id, &e.hash); err != nil {
			rows.Close()
			return false, err
		}
		list = append(list, e)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return false, err
	}
	for _, e := range list {
		found, confirmations, err := a.xmrOnNetwork(ctx, &xmrSettleRow{TxHash: e.hash})
		if err != nil {
			return false, err
		}
		if found {
			return true, a.xmrAdoptSuperseded(ctx, s, e.id, e.hash, confirmations)
		}
	}
	return false, nil
}

// xmrAdoptSuperseded records that the stored transaction hash of settlement earlier, which re-request s re-requests, is
// on the network with confirmations (0 in the pool): s is closed and earlier is relayed again (see the file comment).
// A concurrent pass that recorded it first leaves nothing to do.
func (a *App) xmrAdoptSuperseded(ctx context.Context, s *xmrSettleRow, earlier int64, hash string, confirmations int64) error {
	tx, err := a.beginEscrowOrderTx(ctx, s.OrderID)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// The re-request first: migration 144 lets earlier move back to relayed only once every re-request after it is closed.
	var stored bool
	err = tx.QueryRowContext(ctx, "UPDATE xmr_settlements SET state='superseded',send_error='',updated=now() WHERE id=$1 AND state IN ('requested','built','relayed') RETURNING tx_hash IS NOT NULL", s.ID).Scan(&stored)
	if err == sql.ErrNoRows {
		return nil // recorded already, or s moved on meanwhile
	}
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, "UPDATE xmr_settlements SET state='relayed',relayed_at=COALESCE(relayed_at,now()),confirmed_at=NULL,send_error='',updated=now() WHERE id=$1 AND state='superseded'", earlier)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("monero escrow: settlement %d is no longer superseded", earlier)
	}
	if _, err = tx.ExecContext(ctx, "UPDATE payments SET xmr_settlement_id=$2,updated=now() WHERE order_id=$3 AND xmr_settlement_id=$1", s.ID, earlier, s.OrderID); err != nil {
		return err
	}
	paid, fee, err := xmrPlanText(ctx, tx, earlier, s.Kind)
	if err != nil {
		return err
	}
	where := "in its pool"
	if confirmations > 0 {
		where = "in a block"
	}
	what := settlementKind(s.Kind)
	closed := "it is closed before it was built, so nothing more is sent"
	if stored {
		closed = "it is closed: its own stored transaction spends those coins too, so it can never be mined, and the market does not relay it"
	}
	note := fmt.Sprintf("The earlier transaction %s of this order's Monero escrow settlement %d (%s), which the market had recorded as failed, %s: the escrow daemon has it %s (%s network). "+
		"It pays what that settlement planned: %s; network fee %s XMR. The re-request that was to replace it, settlement %d, spends exactly the same coins, so %s. Nothing is paid twice. "+
		"The order page follows transaction %s until it is confirmed.",
		hash, earlier, what, supersededSentPhrase, where, a.xmrEscrowNetwork(), paid, fee, s.ID, closed, hash)
	if err = settleNote(ctx, tx, s.OrderID, note, "its Monero escrow settlement ("+what+") "+supersededSentPhrase+": the market's earlier transaction for it reached the "+
		a.xmrEscrowNetwork()+" network, so its re-request is closed and nothing is paid twice. The order page shows the transaction and its confirmations."); err != nil {
		return err
	}
	var admin sql.NullString
	if err = tx.QueryRowContext(ctx, "SELECT requested_by FROM xmr_settlements WHERE id=$1", s.ID).Scan(&admin); err != nil {
		return err
	}
	if admin.Valid {
		if _, err = insertScopedNotification(ctx, tx, admin.String, "Order "+shortID(s.OrderID)+": "+note, s.OrderID, "admin"); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	log.Printf("escrow: Monero escrow order %s: superseded settlement %d's transaction is on the network; its re-request %d is closed", shortID(s.OrderID), earlier, s.ID)
	return nil
}

// supersededSentPhrase says that a superseded settlement's stored transaction reached the network (A-566).
const supersededSentPhrase = "is on the network after all"

// xmrPlanText lists the stored outputs of settlement id (of kind) by recipient, without addresses (the parties read
// it), and its network fee, in XMR.
func xmrPlanText(ctx context.Context, tx *sql.Tx, id int64, kind string) (string, string, error) {
	var fee int64
	if err := tx.QueryRowContext(ctx, "SELECT fee FROM xmr_settlements WHERE id=$1", id).Scan(&fee); err != nil {
		return "", "", err
	}
	rows, err := tx.QueryContext(ctx, "SELECT role,amount FROM xmr_settlement_outputs WHERE settlement_id=$1 ORDER BY n", id)
	if err != nil {
		return "", "", err
	}
	defer rows.Close()
	var outs []string
	for rows.Next() {
		var role string
		var n int64
		if err = rows.Scan(&role, &n); err != nil {
			return "", "", err
		}
		outs = append(outs, strings.Replace(settlementOutputRole(kind, role), "The ", "the ", 1)+": "+amount(n, 12)+" XMR")
	}
	return strings.Join(outs, "; "), amount(fee, 12), rows.Err()
}

// xmrClosedBy is the order page's status of re-request s, closed because the stored transaction of earlier, a
// settlement it re-requests, reached the network (A-566).
func xmrClosedBy(earlier int64, stored bool) string {
	text := "Closed: the earlier transaction of settlement " + strconv.FormatInt(earlier, 10) + ", which this one re-requests, " + supersededSentPhrase +
		" and spends the same coins, so this settlement was closed"
	if stored {
		return text + ": its own stored transaction spends the same coins, so it can never be mined, and the market does not relay it. Nothing is paid twice."
	}
	return text + " before it was built. Nothing is paid twice."
}
