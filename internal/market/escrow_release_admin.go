package market

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"
	"strings"
)

// The Monero escrow release deadline length (WR-escrow Phase 2, slice P2-a; P2-R3, owner decision ES-11): how many
// days after a physical order is marked shipped the market may release the vendor's claim when the buyer has not
// confirmed receipt and no dispute is open. An administrator sets it on /admin (setting escrow_release_days: 7 to 30 whole days,
// default 14; password step-up; the audit row names the old and the new days only). Each physical escrow snapshots the
// days in force at checkout (escrows.release_days, frozen by migration 079's trigger), so a change affects only later
// escrows. The deadline itself, the reminder and the release are later slices.

const (
	escrowReleaseDaysSetting = "escrow_release_days"
	defaultReleaseDays       = 14
	minReleaseDays           = 7
	maxReleaseDays           = 30
)

func init() {
	registerAction("/admin/escrow-release-days", actionSpec{Roles: []string{"admin"}, OwnTx: true, ConfirmedFormNonce: true, Off: physicalEscrowOff, Run: escrowReleaseDaysAction})
	registerLoader("admin", escrowReleaseDaysLoader)
}

// lockSetting serialises concurrent saves of a settings key until the transaction ends (A-379). SELECT … FOR UPDATE
// on a row that was never saved locks nothing, so two first saves would both read the default as the old value and
// the audit rows would not chain; the transaction advisory lock is taken whether or not the row exists (the hash
// names the key, so it cannot collide with another subsystem's lock keys), and the row is locked as well when it does.
// The old value is read after this, in a new statement, so it sees the other save once that has committed.
func lockSetting(ctx context.Context, tx *sql.Tx, key string) error {
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtextextended('setting:'||$1, 0))", key); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "SELECT 1 FROM settings WHERE key=$1 FOR UPDATE", key)
	return err
}

// escrowReleaseDays is the release deadline length in force: the administrator's setting, or the default when none
// was saved. A stored value outside the range is refused rather than guessed (503).
func escrowReleaseDays(ctx context.Context, q rowQuerier) (int, error) {
	var v sql.NullString
	if err := q.QueryRowContext(ctx, `SELECT (SELECT value FROM settings WHERE key=$1)`, escrowReleaseDaysSetting).Scan(&v); err != nil {
		return 0, err
	}
	if !v.Valid {
		return defaultReleaseDays, nil
	}
	n, err := strconv.Atoi(v.String)
	if err != nil || n < minReleaseDays || n > maxReleaseDays {
		return 0, fail(503, "The release deadline setting is unreadable. Nothing was changed.")
	}
	return n, nil
}

// escrowReleaseDaysLoader shows the length in force and its form on /admin while Monero escrow is on.
func escrowReleaseDaysLoader(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	if physicalEscrowOff(a) || d.User == nil || d.User.Role != "admin" {
		return nil
	}
	n, err := escrowReleaseDays(ctx, a.db)
	if err != nil {
		return err
	}
	d.EscrowReleaseDays = &EscrowReleaseDaysView{Days: strconv.Itoa(n), Min: minReleaseDays, Max: maxReleaseDays}
	return nil
}

// escrowReleaseDaysAction saves the release deadline length: a whole number of days from 7 to 30, confirmed with the
// administrator's password (and authenticator code when enrolled). The audit row names the old and new days only.
func escrowReleaseDaysAction(c *actionCtx) (actionResult, error) {
	s := strings.TrimSpace(c.Form.Get("release_days"))
	days, err := strconv.Atoi(s)
	if err != nil || days < minReleaseDays || days > maxReleaseDays {
		return actionResult{}, fail(400, "Enter the release deadline as a whole number of days from "+strconv.Itoa(minReleaseDays)+" to "+strconv.Itoa(maxReleaseDays)+". Nothing was changed.")
	}
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		ctx := c.Ctx()
		if err := lockSetting(ctx, c.Tx, escrowReleaseDaysSetting); err != nil {
			return actionResult{}, err
		}
		old, err := escrowReleaseDays(ctx, c.Tx)
		if err != nil {
			return actionResult{}, err
		}
		if old == days {
			return actionResult{}, fail(409, "The release deadline is already that many days; nothing was changed.")
		}
		if err = setSetting(c, escrowReleaseDaysSetting, strconv.Itoa(days)); err != nil {
			return actionResult{}, err
		}
		network := c.A.xmrEscrowNetKnown()
		if network == "" {
			network = "network not verified"
		}
		audit := "Changed the Monero escrow release deadline (" + network + "): " + strconv.Itoa(old) + " → " + strconv.Itoa(days) + " days"
		return actionResult{Redirect: "/admin/payments", Notice: "escrow-release-days", Audit: audit}, nil
	})
}
