package market

import (
	"context"
	"strconv"
	"strings"
)

// The settlements of a Monero escrow order on its order page (WR-escrow Phase 3, slice X-d;
// web/templates/partials/order-xmr-settlement.html): each settlement, newest first, with its kind, its state in honest
// words, its transaction once it was sent, its outputs in XMR and its network fee, how far it was followed, and why it
// waits or failed. An output's address is shown to the party it pays and to staff only. An administrator who is not a
// party is offered the re-request of a failed settlement (escrow_xmr_rerequest.go), and sees a re-request waiting, its
// refusal or the settlement that superseded it. Nothing here makes an RPC.

// XMRSettlementView is one Monero escrow settlement on the order page.
type XMRSettlementView struct {
	ID                               int64
	Title, Status                    string
	TxHash, Fee, InputTotal          string // set once the transaction is stored; TxHash once it was sent
	Confirmations                    string
	Outputs                          []SettlementOutputView
	Supersedes, SupersededBy         int64
	ReRequest, ReRequestPending      bool   // the administrator's form; a re-request waiting for the watcher
	ReRequestRefused                 string // why the watcher refused the last re-request
	reorgSafe, stored                bool
	kind, state, wait, reRequestText string
}

// xmrSettlementViews lists order d.Order's Monero settlements for the panel v.
func (a *App) xmrSettlementViews(ctx context.Context, d *PageData, v *XMREscrowOrderView) ([]*XMRSettlementView, error) {
	o := d.Order
	rows, err := a.db.QueryContext(ctx, `SELECT s.id,s.kind,s.state,COALESCE(s.tx_hash,''),COALESCE(s.fee,0),COALESCE(s.input_total,0),COALESCE(s.supersedes,0),
		COALESCE((SELECT n.id FROM xmr_settlements n WHERE n.supersedes=s.id),0),s.reorg_safe_at IS NOT NULL,
		COALESCE((SELECT value FROM settings WHERE key='escrow_xmr_settlement_'||s.id),''),COALESCE((SELECT value FROM settings WHERE key='escrow_xmr_rerequest_'||s.id),'')
		FROM xmr_settlements s WHERE s.order_id=$1 ORDER BY s.id DESC LIMIT 20`, o.ID)
	if err != nil {
		return nil, err
	}
	var out []*XMRSettlementView
	for rows.Next() {
		s := &XMRSettlementView{}
		var fee, total int64
		if err = rows.Scan(&s.ID, &s.kind, &s.state, &s.TxHash, &fee, &total, &s.Supersedes, &s.SupersededBy, &s.reorgSafe, &s.wait, &s.reRequestText); err != nil {
			rows.Close()
			return nil, err
		}
		if s.stored = s.TxHash != ""; s.stored {
			s.Fee, s.InputTotal = amount(fee, 12), amount(total, 12)
		}
		out = append(out, s)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	outcome, err := escrowDisputeOutcome(ctx, a.db, o.ID)
	if err != nil {
		return nil, err
	}
	staff := d.OrderViewer == roleModerator
	admin := staff && d.User != nil && d.User.Role == "admin" && a.escrowXMR != nil
	for _, s := range out {
		a.xmrSettlementStatus(s, o, outcome, v)
		if s.stored {
			if err = a.xmrSettlementOutputs(ctx, s, d.OrderViewer); err != nil {
				return nil, err
			}
		}
		switch {
		case s.state != "failed" || !s.stored:
		case strings.HasPrefix(s.reRequestText, xmrReRequestRefusedPrefix):
			s.ReRequestRefused = xmrReRequestRefusal(strings.TrimPrefix(s.reRequestText, xmrReRequestRefusedPrefix))
			s.ReRequest = admin
		case s.reRequestText != "":
			s.ReRequestPending = true
		default:
			s.ReRequest = admin
		}
		if s.ReRequest && d.Security == nil {
			if d.Security, _, _, err = a.securityView(ctx, d.User.ID); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// xmrSettlementStatus fills s's title, status and confirmations for order o (dispute outcome outcome) in panel v.
func (a *App) xmrSettlementStatus(s *XMRSettlementView, o *Order, outcome string, v *XMREscrowOrderView) {
	switch {
	case s.kind == "release" && o.Kind == "physical":
		s.Title = "Release to the vendor"
	case s.kind == "release":
		s.Title = "The vendor's claim"
	case s.kind == "refund":
		s.Title = "Refund to the buyer"
	default:
		s.Title = "Return of coins sent after the order closed or left out of its settlement (a stray refund to the buyer)"
	}
	threshold := strconv.FormatInt(v.Threshold, 10)
	why := xmrSettleWaitText(s.wait, v.Threshold)
	sent := "Sent to the " + xmrEscrowNetwork + " network: transaction " + s.TxHash + "."
	switch s.state {
	case "requested":
		s.Status = "Requested: the market builds this transaction from this order's escrow account and sends it on its next pass. Reload in a minute."
		switch {
		case !settlementFits(s.kind, o.State, outcome):
			s.Status = "Requested: the market builds and sends this release only once the order is completed or its dispute is decided for the vendor. Nothing is sent before."
		case v.RestorePaused:
			s.Status = "Paused: " + restoreWhy + ", so the market builds and sends no settlement until the operator has finished. The escrow is still watched."
		case !v.Available:
			s.Status = "Waiting: Monero escrow is unavailable on this market right now, so the market builds this settlement once it is available again."
		case why != "":
			s.Status = "Not built yet. " + why
		}
	case "built":
		s.Status = "Built, checked and stored; the market sends it on its next pass. Nothing has been sent yet."
		switch {
		case restoreHeld(s.wait):
			s.Status = "Built, checked and stored. " + restoreHeldText
		case v.RestorePaused:
			s.Status = "Built, checked and stored. Paused: " + restoreWhy + ", so the market does not send it until the operator has finished."
		}
	case "relayed":
		s.Status, s.Confirmations = sent+" Waiting for "+threshold+" confirmations.", "Fewer than "+threshold
	case "confirmed":
		s.Status, s.Confirmations = "Confirmed on the "+xmrEscrowNetwork+" network: transaction "+s.TxHash+".", "At least "+threshold+"; followed to "+strconv.Itoa(escrowReorgDepth)
		if s.reorgSafe {
			s.Confirmations = "At least " + strconv.Itoa(escrowReorgDepth) + " (no longer followed)"
		}
	case "external":
		s.Status = "Spent outside the market: the coins of this settlement were spent by a transaction this market did not make, so its transaction " + s.TxHash + " can never confirm. Market staff were notified."
	case "failed":
		s.Status = "Not sent. " + why
		if s.stored {
			s.Status = "Not sent: transaction " + s.TxHash + " was built and stored but never mined. " + why
		}
	case "superseded":
		s.Status = "Superseded: a changed dispute decision replaced it before it was built."
		if s.stored {
			s.Status = "Superseded by settlement " + strconv.FormatInt(s.SupersededBy, 10) + " after an administrator's re-request: its stored transaction " + s.TxHash + " was never mined, and spends the same coins as the one that replaced it, so at most one of them can be."
		}
	default:
		s.Status = s.state
	}
	if s.Supersedes != 0 {
		s.Status += " It re-requests settlement " + strconv.FormatInt(s.Supersedes, 10) + ": the same coins, to the same destinations."
	}
	if s.state != "relayed" && s.state != "confirmed" && s.state != "external" && s.state != "failed" && s.state != "superseded" {
		s.TxHash = "" // not sent yet
	}
}

// xmrSettlementOutputs reads s's outputs; an address is shown to the party it pays and to staff only.
func (a *App) xmrSettlementOutputs(ctx context.Context, s *XMRSettlementView, viewer string) error {
	rows, err := a.db.QueryContext(ctx, "SELECT role,address,amount FROM xmr_settlement_outputs WHERE settlement_id=$1 ORDER BY n", s.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var out SettlementOutputView
		var role string
		var n int64
		if err = rows.Scan(&role, &out.Address, &n); err != nil {
			return err
		}
		recipient := roleBuyer
		if role == "market_fee" {
			recipient = roleModerator
		} else if role == "claimant" && s.kind == "release" {
			recipient = roleVendor
		}
		if viewer != recipient && viewer != roleModerator {
			out.Address = "(shown only to its recipient)"
		}
		out.Amount, out.Role = amount(n, 12), settlementOutputRole(s.kind, role)
		s.Outputs = append(s.Outputs, out)
	}
	return rows.Err()
}
