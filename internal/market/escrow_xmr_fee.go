package market

import (
	"context"
	"net/http"
	"strconv"
	"strings"
)

// The Monero escrow market fee is host configuration only (A-550, owner directive 2026-10-18): ESCROW_XMR_FEE_BPS and
// ESCROW_XMR_FEE_ADDRESS, read and checked at startup (escrowXMRConfigFromEnv, startEscrowXMR). No action, form or
// settings row sets it, so an administrator web account, taken over or not, cannot redirect the fee; Admin → Payments
// shows it read-only (xmrFeeLoader). Each escrow snapshots the fee and its address at checkout (escrow_xmr_create.go).
//
// So that a change made on the host is seen by staff, each start with Monero escrow on compares the configured fee with
// the one it last recorded (xmrFeeSeenSetting) and, when they differ, records the new one, writes one audit row naming
// both and notifies every administrator once (recordXMRFee). The record is never read as the fee: editing it or
// restoring an older database only makes the next start announce the host's value again.

// xmrFeeSeenSetting holds the fee the last start saw, "BPS ADDRESS" (ADDRESS empty at 0 basis points).
const xmrFeeSeenSetting = "escrow_xmr_fee_seen"

func init() {
	registerLoader("admin", xmrFeeLoader)
}

// xmrFeeWords names a fee in an audit row or notice: its rate and, above 0, the full address it is paid to.
func xmrFeeWords(bps int64, addr string) string {
	if bps == 0 {
		return "no market fee (0 basis points)"
	}
	return strconv.FormatInt(bps, 10) + " basis points (" + xmrFeePercent(bps) + ") paid to " + addr
}

func xmrFeePercent(bps int64) string {
	return strconv.FormatFloat(float64(bps)/100, 'f', -1, 64) + "%"
}

// recordXMRFee compares the fee this start configured with the one last recorded and, when they differ (or none was
// recorded), records it, writes one audit row with the old and new rate and address and notifies every administrator
// who is not suspended, in one transaction under the setting's lock (lockSetting), so concurrent starts announce a
// change once. An unchanged fee writes nothing.
func (a *App) recordXMRFee(ctx context.Context) error {
	x := a.escrowXMR
	bps, addr := x.feeBps, x.feeAddress
	if bps == 0 {
		addr = ""
	}
	seen := strconv.FormatInt(bps, 10) + " " + addr
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = lockSetting(ctx, tx, xmrFeeSeenSetting); err != nil {
		return err
	}
	var old string
	if err = tx.QueryRowContext(ctx, "SELECT COALESCE((SELECT value FROM settings WHERE key=$1),'')", xmrFeeSeenSetting).Scan(&old); err != nil {
		return err
	}
	if old == seen {
		return nil
	}
	was := "none recorded before"
	if b, ad, ok := strings.Cut(old, " "); ok {
		if n, err := strconv.ParseInt(b, 10, 64); err == nil && n >= 0 && n <= 10000 {
			was = xmrFeeWords(n, ad)
		} else {
			was = "an unreadable record"
		}
	} else if old != "" {
		was = "an unreadable record"
	}
	now := xmrFeeWords(bps, addr)
	if _, err = tx.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT (key) DO UPDATE SET value=excluded.value", xmrFeeSeenSetting, seen); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES(NULL,$1)",
		"Monero escrow market fee changed on the server (seen at startup): "+was+" → "+now); err != nil {
		return err
	}
	if err = notifyAdmins(ctx, tx, "The Monero escrow market fee set on the server changed, as this start found: it was "+was+"; it is now "+now+
		". It applies to escrows created from now on; existing escrows keep the fee of their checkout. Administrators cannot change the fee on /admin: if no operator meant to change it, check ESCROW_XMR_FEE_BPS and ESCROW_XMR_FEE_ADDRESS on the app host now (Admin → Payments, Market fee)."); err != nil {
		return err
	}
	return tx.Commit()
}

// xmrFeeLoader shows the market fee in force on Admin → Payments, read-only (XMRFeeView).
func xmrFeeLoader(_ context.Context, a *App, _ *http.Request, d *PageData) error {
	if d.User == nil || d.User.Role != "admin" {
		return nil
	}
	v := &XMRFeeView{}
	if x := a.escrowXMR; x != nil {
		v.On, v.Bps, v.Network = true, strconv.FormatInt(x.feeBps, 10), a.xmrEscrowNetShort()
		if x.feeBps > 0 {
			v.Percent, v.Address = xmrFeePercent(x.feeBps), x.feeAddress
		}
	}
	d.XMRFee = v
	return nil
}
