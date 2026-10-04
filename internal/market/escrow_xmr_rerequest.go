package market

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strconv"
	"strings"
)

// The administrator's re-request of a failed Monero escrow settlement (WR-escrow Phase 3, slice X-d; owner decision
// O-1, A-401, P3-R6). A settlement fails when the daemon refused its stored transaction for good and no other
// transaction spent its key images (escrow_xmr_settle.go); it is never rebuilt automatically. An administrator who is
// not a party may ask, from the order page, with a password step-up and an audit row, that it be built again: the
// request is recorded (xmr_settlement_rerequests, migration 085) and the Monero escrow watcher decides it under its
// lock, because the checks need the daemon. It is carried out only while the stored transaction is in neither the daemon's pool nor its chain
// and every stored key image is unspent (is_key_image_spent all 0); otherwise it is refused with a fixed reason, shown
// on the order page, and may be asked again later. Carried out, in one transaction: the failed settlement becomes
// superseded (keeping its stored transaction), a new requested settlement names it in supersedes (the same order, kind
// and destination; migration 084), and the deposits it claimed move to the new one. The new settlement is then built
// like any other, but it spends exactly the stored key images (every other output of the account frozen) to the stored
// plan: the same destinations and amounts, only the network fee and so the claimant's output may differ. Migration
// 084 refuses any other key images when its transaction is stored, so at most one of the two transactions can ever be
// mined; the plan is checked in Go (checkXMRBuild).

func init() {
	registerAction("/admin/escrow-xmr-rerequest", actionSpec{Roles: []string{"admin"}, OwnTx: true, Off: escrowXMROff, Run: xmrReRequestAction})
	registerAction("/admin/escrow-xmr-retry-build", actionSpec{Roles: []string{"admin"}, OwnTx: true, Off: escrowXMROff, Run: xmrRetryBuildAction})
}

// Refusal reasons of a re-request (xmr_settlement_rerequests.reason).
const (
	xmrReRequestInPool  = "in-pool"
	xmrReRequestOnChain = "on-chain"
	xmrReRequestSpent   = "key-image-spent"
)

// xmrReRequestRefusal is what the order page says about a refused re-request.
func xmrReRequestRefusal(class string) string {
	switch class {
	case xmrReRequestInPool:
		return "its stored transaction is in the Monero daemon's pool, so it may still be mined."
	case xmrReRequestOnChain:
		return "its stored transaction is in the chain, so it was sent after all."
	case xmrReRequestSpent:
		return "a coin its stored transaction spends is already spent, by that transaction or another."
	}
	return ""
}

var (
	errReRequestNone    = fail(404, "Settlement not found")
	errReRequestParty   = fail(403, "You are a party to this order, so you cannot re-request its settlement.")
	errReRequestState   = fail(409, "Only a Monero escrow settlement that failed after its transaction was built can be re-requested. Reload the page. Nothing was changed.")
	errReRequestPending = fail(409, "A re-request of this settlement is already waiting for the market's next pass. Reload the page in a minute. Nothing was changed.")
	errRetryBuildState  = fail(409, "Only a Monero escrow settlement set aside because building it made the escrow wallet stop responding can be built again this way. Reload the page. Nothing was changed.")
)

// xmrReRequestAction records an administrator's re-request of a failed Monero escrow settlement (see the file comment).
func xmrReRequestAction(c *actionCtx) (actionResult, error) {
	id, err := strconv.ParseInt(c.Form.Get("settlement_id"), 10, 64)
	if err != nil || id <= 0 {
		return actionResult{}, errReRequestNone
	}
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		ctx := c.Ctx()
		var orderID, kind, state string
		var stored bool
		err := c.Tx.QueryRowContext(ctx, "SELECT order_id,kind,state,tx_hash IS NOT NULL FROM xmr_settlements WHERE id=$1 FOR UPDATE", id).Scan(&orderID, &kind, &state, &stored)
		if err == sql.ErrNoRows {
			return actionResult{}, errReRequestNone
		}
		if err != nil {
			return actionResult{}, err
		}
		o, err := scanOrder(c.Tx.QueryRowContext(ctx, orderQuery+" WHERE o.id=$1", orderID))
		if err != nil {
			return actionResult{}, err
		}
		switch {
		case c.User.ID == o.BuyerID || c.User.ID == o.VendorID:
			return actionResult{}, errReRequestParty
		case state != "failed" || !stored:
			return actionResult{}, errReRequestState
		}
		// The settlement row is locked: of two requests one inserts, and the other finds it open.
		var open bool
		if err = c.Tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM xmr_settlement_rerequests WHERE settlement_id=$1 AND state='requested')", id).Scan(&open); err != nil {
			return actionResult{}, err
		}
		if open {
			return actionResult{}, errReRequestPending
		}
		if _, err = c.Tx.ExecContext(ctx, "INSERT INTO xmr_settlement_rerequests(settlement_id,requested_by) VALUES($1,$2)", id, c.User.ID); err != nil {
			return actionResult{}, err
		}
		if _, err = c.Tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,$2,$2,$3,$4)", o.ID, o.State, c.User.ID,
			"An administrator asked the market to re-request the failed Monero escrow settlement "+strconv.FormatInt(id, 10)+" ("+settlementKind(kind)+"). On its next pass the market checks that its stored transaction is in neither the daemon's pool nor the chain and that none of its coins is spent; only then does it build it again, from the same coins to the same destinations (only the network fee may differ), and send it."); err != nil {
			return actionResult{}, err
		}
		return actionResult{Redirect: orderPage(o.ID) + "#xmr-settlement-" + strconv.FormatInt(id, 10), Notice: "escrow-xmr-rerequest",
			Audit: "Re-requested the failed Monero escrow settlement " + strconv.FormatInt(id, 10) + " (" + settlementKind(kind) + ") of order " + shortID(o.ID) + " (" + xmrEscrowNetwork + ")"}, nil
	})
}

// xmrRetryBuildAction is an administrator's request that a set-aside settlement be built again: a settlement whose
// builds twice in a row made the escrow wallet-rpc stop responding is skipped by the watcher (xmrBuildSetAside,
// escrow_xmr_settle.go) until an administrator who is not a party, with a password step-up and an audit row, clears it.
// Nothing was built for it, so nothing needs the daemon: the next pass builds it like any requested settlement.
func xmrRetryBuildAction(c *actionCtx) (actionResult, error) {
	id, err := strconv.ParseInt(c.Form.Get("settlement_id"), 10, 64)
	if err != nil || id <= 0 {
		return actionResult{}, errReRequestNone
	}
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		ctx := c.Ctx()
		var orderID, kind, state, wait string
		err := c.Tx.QueryRowContext(ctx, "SELECT order_id,kind,state,send_error FROM xmr_settlements WHERE id=$1 FOR UPDATE", id).Scan(&orderID, &kind, &state, &wait)
		if err == sql.ErrNoRows {
			return actionResult{}, errReRequestNone
		}
		if err != nil {
			return actionResult{}, err
		}
		o, err := scanOrder(c.Tx.QueryRowContext(ctx, orderQuery+" WHERE o.id=$1", orderID))
		if err != nil {
			return actionResult{}, err
		}
		switch {
		case c.User.ID == o.BuyerID || c.User.ID == o.VendorID:
			return actionResult{}, errReRequestParty
		case state != "requested" || wait != xmrBuildSetAside:
			return actionResult{}, errRetryBuildState
		}
		if _, err = c.Tx.ExecContext(ctx, "UPDATE xmr_settlements SET send_error='',updated=now() WHERE id=$1", id); err != nil {
			return actionResult{}, err
		}
		if _, err = c.Tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,$2,$2,$3,$4)", o.ID, o.State, c.User.ID,
			"An administrator asked the market to build the Monero escrow settlement "+strconv.FormatInt(id, 10)+" ("+settlementKind(kind)+") again; it had been set aside because building it made the escrow wallet stop responding. The market builds it on its next pass; if the wallet stops responding twice more, it is set aside again."); err != nil {
			return actionResult{}, err
		}
		return actionResult{Redirect: orderPage(o.ID) + "#xmr-settlement-" + strconv.FormatInt(id, 10), Notice: "escrow-xmr-retry",
			Audit: "Asked the market to build the set-aside Monero escrow settlement " + strconv.FormatInt(id, 10) + " (" + settlementKind(kind) + ") of order " + shortID(o.ID) + " again (" + xmrEscrowNetwork + ")"}, nil
	})
}

// xmrReRequests answers the administrators' re-requests of the failed settlements of wallet w (see the file comment).
// The watcher calls it only while the restore gate is not set.
func (a *App) xmrReRequests(ctx context.Context, w xmrWalletRow) error {
	rows, err := a.db.QueryContext(ctx, `SELECT r.id,r.requested_by,s.id,s.order_id,s.kind,s.tx_hash,array_to_string(s.key_images,',') FROM xmr_settlement_rerequests r
		JOIN xmr_settlements s ON s.id=r.settlement_id JOIN escrows es ON es.order_id=s.order_id AND es.coin='XMR' JOIN xmr_escrow_slots sl ON sl.id=es.xmr_slot_id
		WHERE sl.wallet_id=$1 AND r.state='requested' AND s.state='failed' AND s.tx_hash IS NOT NULL ORDER BY r.id`, w.id)
	if err != nil {
		return err
	}
	type request struct {
		id    int64
		admin string
		s     xmrSettleRow
	}
	var due []request
	for rows.Next() {
		var r request
		var keyImages string
		if err = rows.Scan(&r.id, &r.admin, &r.s.ID, &r.s.OrderID, &r.s.Kind, &r.s.TxHash, &keyImages); err != nil {
			rows.Close()
			return err
		}
		r.s.KeyImages = strings.Split(keyImages, ",")
		due = append(due, r)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	var errs []error
	for _, r := range due {
		if err = a.xmrReRequest(ctx, r.id, &r.s, r.admin); err != nil {
			errs = append(errs, &watchError{label: "monero escrow settlement " + strconv.FormatInt(r.s.ID, 10), class: "re-request: " + xmrErrorClass(err), err: err})
		}
	}
	return errors.Join(errs...)
}

// xmrReRequest checks re-request id of settlement s against the daemon and refuses it or carries it out.
func (a *App) xmrReRequest(ctx context.Context, id int64, s *xmrSettleRow, admin string) error {
	found, confirmations, err := a.xmrOnNetwork(ctx, s)
	if err != nil {
		return err
	}
	switch {
	case found && confirmations == 0:
		return a.refuseXMRReRequest(ctx, id, s, admin, xmrReRequestInPool)
	case found:
		return a.refuseXMRReRequest(ctx, id, s, admin, xmrReRequestOnChain)
	}
	spent, err := a.escrowXMR.rpc.keyImagesSpent(ctx, s.KeyImages)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(spent, func(n int) bool { return n != 0 }) {
		return a.refuseXMRReRequest(ctx, id, s, admin, xmrReRequestSpent)
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state, open string
	if err = tx.QueryRowContext(ctx, "SELECT s.state,r.state FROM xmr_settlements s JOIN xmr_settlement_rerequests r ON r.settlement_id=s.id WHERE r.id=$1 FOR UPDATE",
		id).Scan(&state, &open); err != nil {
		return err
	}
	if state != "failed" || open != "requested" {
		return nil // changed meanwhile
	}
	if _, err = tx.ExecContext(ctx, "UPDATE xmr_settlements SET state='superseded',updated=now() WHERE id=$1 AND state='failed'", s.ID); err != nil {
		return err
	}
	var next int64
	if err = tx.QueryRowContext(ctx, `INSERT INTO xmr_settlements(order_id,kind,destination,requested_by,supersedes) SELECT order_id,kind,destination,$2,id FROM xmr_settlements WHERE id=$1
		RETURNING id`, s.ID, admin).Scan(&next); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE payments SET xmr_settlement_id=$2,updated=now() WHERE xmr_settlement_id=$1", s.ID, next); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE xmr_settlement_rerequests SET state='done',new_settlement_id=$2,decided_at=now() WHERE id=$1 AND state='requested'", id, next); err != nil {
		return err
	}
	what := settlementKind(s.Kind)
	note := "Re-request carried out: the failed Monero escrow settlement " + strconv.FormatInt(s.ID, 10) + " (" + what + ") is superseded by settlement " + strconv.FormatInt(next, 10) +
		". Its stored transaction is in neither the daemon's pool nor the chain and none of its coins is spent, so the market builds it again from the same coins to the same destinations (only the network fee may differ) and sends it."
	if _, err = tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) SELECT id,state,state,NULL,$2 FROM orders WHERE id=$1", s.OrderID, note); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), admin, "Order "+shortID(s.OrderID)+": "+note); err != nil {
		return err
	}
	return tx.Commit()
}

// refuseXMRReRequest records a refused re-request with its class, an order note and a notice to the administrator who
// asked; it may be asked again.
func (a *App) refuseXMRReRequest(ctx context.Context, id int64, s *xmrSettleRow, admin, class string) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, "UPDATE xmr_settlement_rerequests SET state='refused',reason=$2,decided_at=now() WHERE id=$1 AND state='requested'", id, class)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil
	}
	note := "Re-request of the failed Monero escrow settlement " + strconv.FormatInt(s.ID, 10) + " (" + settlementKind(s.Kind) + ") refused: " + xmrReRequestRefusal(class) + " Nothing was built or sent."
	if _, err = tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) SELECT id,state,state,NULL,$2 FROM orders WHERE id=$1", s.OrderID, note); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), admin, "Order "+shortID(s.OrderID)+": "+note); err != nil {
		return err
	}
	return tx.Commit()
}
