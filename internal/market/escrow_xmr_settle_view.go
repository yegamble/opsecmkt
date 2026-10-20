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
// refusal or the settlement that superseded it; the retry of a settlement set aside because building it made the
// escrow wallet stop responding; and, for one that failed before any transaction was built (other than a refund too
// few), marking it settled by hand (escrow_xmr_hand_settled.go, A-613), whose record staff alone see. Nothing here
// makes an RPC.

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
	RetryBuild                       bool   // the administrator's form for a settlement set aside (xmrBuildSetAside)
	SettledByHand                    bool   // the administrator's form closing a failed one the market never sends (A-613)
	HandSettled                      string // to staff only: who dismissed it or marked it settled by hand, when, and their note
	reorgSafe, stored, handSettled   bool
	claims                           bool   // it claims a deposit (payments.xmr_settlement_id; PAY613-4)
	kind, state, wait, reRequestText string // reRequestText: the newest re-request's state and reason
}

// xmrSettlementViews lists order d.Order's Monero settlements for the panel v.
func (a *App) xmrSettlementViews(ctx context.Context, d *PageData, v *XMREscrowOrderView) ([]*XMRSettlementView, error) {
	o := d.Order
	rows, err := a.db.QueryContext(ctx, `SELECT s.id,s.kind,s.state,COALESCE(s.tx_hash,''),COALESCE(s.fee,0),COALESCE(s.input_total,0),COALESCE(s.supersedes,0),
		COALESCE((SELECT n.id FROM xmr_settlements n WHERE n.supersedes=s.id),0),s.reorg_safe_at IS NOT NULL,
		s.send_error,COALESCE((SELECT r.state||' '||r.reason FROM xmr_settlement_rerequests r WHERE r.settlement_id=s.id ORDER BY r.id DESC LIMIT 1),''),
		h.settlement_id IS NOT NULL,COALESCE(h.kind,''),COALESCE(to_char(h.settled_at,'YYYY-MM-DD HH24:MI "UTC"')||' by '||COALESCE(u.handle,'an administrator')||': '||h.note,''),
		EXISTS(SELECT 1 FROM payments p WHERE p.xmr_settlement_id=s.id)
		FROM xmr_settlements s LEFT JOIN xmr_settlement_hand_settled h ON h.settlement_id=s.id LEFT JOIN users u ON u.id=h.settled_by
		WHERE s.order_id=$1 ORDER BY s.id DESC LIMIT 20`, o.ID)
	if err != nil {
		return nil, err
	}
	staff := d.OrderViewer == roleModerator
	var out []*XMRSettlementView
	for rows.Next() {
		s := &XMRSettlementView{}
		var fee, total int64
		var recordKind, record string
		if err = rows.Scan(&s.ID, &s.kind, &s.state, &s.TxHash, &fee, &total, &s.Supersedes, &s.SupersededBy, &s.reorgSafe, &s.wait, &s.reRequestText, &s.handSettled, &recordKind, &record, &s.claims); err != nil {
			rows.Close()
			return nil, err
		}
		switch {
		case !staff || !s.handSettled:
		case recordKind == xmrNothingOwed:
			s.HandSettled = "Dismissed (nothing owed now) " + record
		default:
			s.HandSettled = "Marked settled by hand " + record
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
	staffAdmin := staff && d.User != nil && d.User.Role == "admin"
	admin := staffAdmin && a.escrowXMR != nil
	// offered: the kinds of administrator form on the page, in xmrSettlementActions' order, for the staff help.
	var offered [4]bool
	for _, s := range out {
		a.xmrSettlementStatus(s, o, outcome, v)
		if s.stored {
			if err = a.xmrSettlementOutputs(ctx, s, d.OrderViewer); err != nil {
				return nil, err
			}
		}
		// The newest re-request of a failed settlement: waiting, or refused (it may be asked again), or none yet.
		last, reason, _ := strings.Cut(s.reRequestText, " ")
		// One marked settled by hand is never re-requested again (A-613).
		switch {
		case s.state != "failed" || !s.stored:
		case last == "requested":
			s.ReRequestPending = true
		case last == "refused":
			s.ReRequestRefused, s.ReRequest = xmrReRequestRefusal(reason), admin && !s.handSettled
		default:
			s.ReRequest = admin && !s.handSettled
		}
		s.RetryBuild = admin && s.state == "requested" && s.wait == xmrBuildSetAside
		// A-613: as xmrSettledByHandAction accepts (never built, other than a refund too few and one still claiming a
		// deposit, dismissed as nothing owed; or its stored transaction's last re-request refused for good, marked settled
		// by hand); it needs no wallet, so it is offered while Monero escrow is off too.
		s.SettledByHand = staffAdmin && s.state == "failed" && !s.handSettled &&
			(!s.stored && !s.claims && !(s.kind == "refund" && s.wait == xmrTooSmall) || s.stored && last == "refused" && xmrReRequestFinal(reason))
		if !s.ReRequest && !s.RetryBuild && !s.SettledByHand {
			continue
		}
		offered[0] = offered[0] || s.RetryBuild
		offered[1] = offered[1] || s.ReRequest
		offered[2] = offered[2] || s.SettledByHand && !s.stored
		offered[3] = offered[3] || s.SettledByHand && s.stored
		if d.XMRSettlementForm == 0 {
			d.XMRSettlementForm = s.ID
		}
		if d.Security == nil {
			if d.Security, _, _, err = a.securityView(ctx, d.User.ID); err != nil {
				return nil, err
			}
		}
	}
	d.XMRSettlementActions = xmrSettlementActions(offered)
	return out, nil
}

// xmrSettlementActions names, for the order page's staff help, only the administrator's settlement forms the page
// offers (offered: build again, re-request, dismiss, mark settled by hand), as "a", "a or b" or "a, b or c".
func xmrSettlementActions(offered [4]bool) string {
	var names []string
	for i, n := range []string{"build it again", "re-request it", "dismiss it as nothing owed", "mark it settled by hand"} {
		if offered[i] {
			names = append(names, n)
		}
	}
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " or " + names[len(names)-1]
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
		s.Title = "Return of coins that do not count for the order: sent after it closed, left out of its settlement, or not counted when it was cancelled (a stray refund to the buyer)"
	}
	threshold := strconv.FormatInt(v.Threshold, 10)
	why := xmrSettleWaitText(s.wait, s.kind, v.Threshold)
	sent := "Sent to the " + v.Net + " network: transaction " + s.TxHash + "."
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
		case a.xmrRelayHold() != "":
			s.Status = "Built, checked and stored. The market holds its Monero relays until the operator has finished setting up its node; it then sends this transaction unchanged. Nothing has been sent yet."
		case s.wait == sendRebroadcastOK:
			s.Status = "Built, checked and stored. " + xmrRelayConfirmedText
		case v.RestorePaused && !xmrRelayConfirmed(s.wait):
			s.Status = "Built, checked and stored. Paused: " + restoreWhy + ", so the market does not send it until the operator has finished."
		case why != "":
			s.Status = "Built, checked and stored. " + why
		}
	case "relayed":
		s.Status, s.Confirmations = sent+" Waiting for "+threshold+" confirmations.", "Fewer than "+threshold
		switch {
		case restoreHeld(s.wait):
			s.Status += " " + restoreHeldText
		case s.wait == sendRebroadcastOK:
			s.Status += " " + xmrRelayConfirmedText
		case why != "":
			s.Status += " " + why
		}
	case "confirmed":
		s.Status, s.Confirmations = "Confirmed on the "+v.Net+" network: transaction "+s.TxHash+".", "At least "+threshold+"; followed to "+strconv.Itoa(escrowReorgDepth)
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

// xmrRelayConfirmedText follows a held settlement's status once an administrator confirmed its relay.
const xmrRelayConfirmedText = "An administrator checked that this transaction was not already relayed or replaced; the market relays it once on its next pass if the daemon does not have it."

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
