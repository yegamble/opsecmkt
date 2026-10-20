package market

import (
	"context"
	"database/sql"
	"slices"
	"strconv"
	"strings"
)

func collectStrings(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err = rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// flagOrder records a note in order_events (state unchanged) and notifies active administrators.
// With a party message, the buyer and the vendor are sent it instead of the staff
// notification.
func (a *App) flagOrder(ctx context.Context, tx *sql.Tx, o *Order, note, party string) error {
	if _, err := tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,$2,$2,NULL,$3)", o.ID, o.State, note); err != nil {
		return err
	}
	parties := []string{}
	if party != "" {
		parties = []string{o.BuyerID, o.VendorID}
		for _, uid := range parties {
			if _, err := insertNotification(ctx, tx, uid, party, o.ID); err != nil {
				return err
			}
		}
	}
	staff, err := collectStrings(ctx, tx, "SELECT id FROM users WHERE role='admin' AND suspended_at IS NULL AND closed_at IS NULL AND retention_restricted_at IS NULL AND id<>ALL($1)", parties)
	if err != nil {
		return err
	}
	body := "Payment review needed for order " + o.ID[:min(8, len(o.ID))] + ": " + note
	for _, uid := range staff {
		if _, err = insertScopedNotification(ctx, tx, uid, body, o.ID, "admin"); err != nil {
			return err
		}
	}
	return nil
}

// flagLateDeposits flags, on a closed escrow order, the confirmed deposits first recorded after the order closed
// (after_close), which are never counted. They are never credited or refunded automatically (A-211). Each pass
// that flags some writes one order note naming them (the desk dates each flag from it). The buyer (the amount, no
// transaction ID; the vendor is not told) and the staff are notified once per episode: an episode starts when
// the order has no late flag open and lasts while one is, so many small deposits give one notice each, and a deposit
// after staff disposed of every earlier late flag is announced again. A late flag is one open by paymentFlagOpenRule
// (the rule the moderation desk and the review handler read, so staff can always record a flag that holds an episode,
// A-31) of a deposit not credited, not below the floor, not locked and not spent outside the market: a counted escrow
// deposit's flag is never open, so it never holds an episode (QA462C-1), and an open locked, below-floor, regressed or
// spent-outside flag does not silence the notice of a new deposit after the close (QA462D-1: coins spent outside are
// gone, and their own notice told the parties). An uncredited deposit left out of its settlement holds it: the buyer
// and the staff were told of it, and the one stray refund returns a later deposit with it. Only this function flags a deposit after the close of
// an escrow order before it confirms or is announced (xmrSettleWaits leaves such a deposit to it), so each is
// announced once.
func (a *App) flagLateDeposits(ctx context.Context, tx *sql.Tx, o *Order, threshold int64) error {
	var episode bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM payments p WHERE p.order_id=$1 AND "+paymentFlagOpen("p")+" AND NOT p.credited AND NOT p.below_floor AND NOT p.locked AND NOT p.spent_outside)", o.ID).Scan(&episode); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `UPDATE payments SET flagged=true WHERE order_id=$1 AND NOT credited AND NOT flagged AND NOT locked
		AND after_close AND (confirmations>=$2 OR xmr_settlement_id IS NOT NULL)
		RETURNING txid,idx,amount,after_close,EXISTS(SELECT 1 FROM escrow_siblings s WHERE s.address=payments.address)`, o.ID, threshold)
	if err != nil {
		return err
	}
	dec := currencyDecimals(o.Currency)
	var deposits []string
	var sum int64
	atSiblings := 0 // QA5W5A-6: a deposit to a per-order escrow's sibling address is not at its escrow address
	for rows.Next() {
		var txid string
		var idx, amt int64
		var afterClose bool
		var atSibling bool
		if err = rows.Scan(&txid, &idx, &amt, &afterClose, &atSibling); err != nil {
			rows.Close()
			return err
		}
		deposits = append(deposits, lateDepositName(txid, idx, amt, o.Currency, o.Escrow))
		sum += amt
		if atSibling {
			atSiblings++
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil || len(deposits) == 0 {
		return err
	}
	slices.Sort(deposits)
	verb := "was"
	if len(deposits) > 1 {
		verb = "were"
	}
	notePrefix := a.netTag() + " "
	noteSuffix := " arrived after this order was closed and " + verb + " not counted. Moderator review required."
	body := "Order " + o.ID[:min(8, len(o.ID))] + ": " + amount(sum, dec) + " " + o.Currency + " arrived after this escrow closed. Market staff were notified."
	if o.Escrow {
		// An escrow deposit is at the order's escrow address, never in a wallet staff could refund from (QA31-3).
		net, how := a.escrowNetwork(), "the market builds a stray refund returning it to your refund address, which you or the vendor sign on the order page"
		if o.Currency == "XMR" { // a Monero escrow is custodial: the market alone builds and sends its stray refund (P3-R8)
			net, how = a.xmrEscrowNetwork(), "the market returns it, less the network fee, to your refund address by a stray refund from the escrow"
		}
		where := "this order's escrow address"
		switch {
		case atSiblings == len(deposits):
			where = "an address of this order's wallet other than its escrow address"
		case atSiblings > 0:
			where = "this order's escrow address and another address of this order's wallet"
		}
		notePrefix = a.netTag() + " " + net + " "
		noteSuffix = " arrived at " + where + " after the order closed; not counted, it is returned to the buyer's refund address by a stray refund from the escrow. Moderator review required."
		body = "Order " + o.ID[:min(8, len(o.ID))] + ": " + amount(sum, dec) + " " + o.Currency + " (" + a.netTag() + " " + net + ") arrived at " + where + " after the order closed. It is not counted; " + how + ". Market staff were notified."
	}
	notes := splitNamedFlagNotes(notePrefix, deposits, noteSuffix, maxNoteChars)
	for i, chunk := range notes {
		if i == 0 && !episode {
			if err = a.flagOrder(ctx, tx, o, chunk, ""); err != nil {
				return err
			}
			continue
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,$2,$2,NULL,$3)", o.ID, o.State, chunk); err != nil {
			return err
		}
	}
	if !episode {
		_, err = insertNotification(ctx, tx, o.BuyerID, body, o.ID)
	}
	return err
}

// lateDepositName keeps one transaction's outputs distinguishable in generated flag notes. Pooled Monero's idx is its
// subaddress minor index; per-order Monero escrow stores a wallet/account composite, so show its account component.
func lateDepositName(txid string, idx, amt int64, currency string, escrow bool) string {
	which := "output " + strconv.FormatInt(idx, 10)
	if escrow && currency == "XMR" {
		which = "account " + strconv.FormatInt(idx&((1<<32)-1), 10)
	}
	return "deposit " + truncate(txid, 20) + " " + which + " (" + amount(amt, currencyDecimals(currency)) + " " + currency + ")"
}

// splitNamedFlagNotes keeps every deposit label in a history note under the existing 500-character note bound. The
// caller sends one staff notice for the episode; extra chunks only date the remaining flagged rows on the desk.
func splitNamedFlagNotes(prefix string, names []string, suffix string, limit int) []string {
	out := []string{}
	chunk := []string{}
	for _, name := range names {
		candidate := append(slices.Clone(chunk), name)
		if len(prefix)+len(strings.Join(candidate, ", "))+len(suffix) > limit && len(chunk) > 0 {
			out = append(out, prefix+strings.Join(chunk, ", ")+suffix)
			chunk = []string{name}
			continue
		}
		chunk = candidate
	}
	if len(chunk) > 0 {
		out = append(out, prefix+strings.Join(chunk, ", ")+suffix)
	}
	return out
}
