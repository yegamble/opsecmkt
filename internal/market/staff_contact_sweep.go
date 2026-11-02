package market

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"
)

// PM109-5: the panels warn about the staff contact only when opened (A-734), so each running process also checks it once
// a day and when it starts, and notifies the administrators when its health has changed, including when a warning clears
// and when its key comes within keyExpiryWarning of expiring. The settings row holds the last state notified, as fixed
// codes with no handle, so a pass, a restart or a second process repeats nothing, and one change is at most one notice
// per administrator. Every warning is fixed text naming no one.

func init() { registerBackground(staffContactHealthSweeper) }

const settingStaffContactHealth = "staff_contact_health_notice"

// staffContactHealthOK is the stored state of a contact with no warning; a missing or empty row means no pass stored one.
const staffContactHealthOK = "ok"

// staffContactHealthSweepInterval is how often the contact is checked. A variable only so tests can shorten it.
var staffContactHealthSweepInterval = 24 * time.Hour

func staffContactHealthSweeper(ctx context.Context, a *App) {
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := a.sweepStaffContactHealth(ctx); err != nil && ctx.Err() == nil {
			log.Printf("staff contact health sweep: %s", errorCause(err)) // a class, never the driver's text (A-170)
		}
		t.Reset(staffContactHealthSweepInterval)
	}
}

// sweepStaffContactHealth runs one pass. The state's row is locked first, then the health is read, so concurrent passes
// serialise and the second finds the first's state. With no active administrator to tell it stores nothing, so the first
// one to arrive is told. A panic in the OpenPGP library on the contact's stored key fails the pass, not the process
// (A-266): this runs in a background goroutine.
func (a *App) sweepStaffContactHealth(ctx context.Context) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("the staff contact's key could not be read")
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// The row is created first so that two first passes serialise on it: FOR UPDATE locks nothing that is absent.
	if _, err = tx.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES($1,'') ON CONFLICT(key) DO NOTHING", settingStaffContactHealth); err != nil {
		return err
	}
	var was, contact string
	var open, admins bool
	if err = tx.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=$1 FOR UPDATE", settingStaffContactHealth).Scan(&was); err != nil {
		return err
	}
	// As signedOutStaffContactRefusal reads them: a missing registrations row counts as closed.
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT value FROM settings WHERE key=$1),''), COALESCE((SELECT value FROM settings WHERE key=$2),'')='open',
		EXISTS(SELECT 1 FROM users WHERE role='admin' AND suspended_at IS NULL AND closed_at IS NULL AND retention_restricted_at IS NULL)`,
		settingStaffContact, settingRegistrations).Scan(&contact, &open, &admins); err != nil {
		return err
	}
	if !admins {
		return nil
	}
	warnings, err := staffContactHealth(ctx, tx, contact, open)
	if err != nil {
		return err
	}
	state := staffContactHealthState(warnings)
	if state == was {
		return nil
	}
	if body := staffContactHealthNotice(warnings); was != "" || len(warnings) > 0 { // a first pass that finds nothing wrong says nothing
		if err = notifyAdmins(ctx, tx, body); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, "UPDATE settings SET value=$2 WHERE key=$1", settingStaffContactHealth, state); err != nil {
		return err
	}
	return tx.Commit()
}

// staffContactHealthState is the stored form of warnings: their codes, which a reworded warning leaves as they are.
func staffContactHealthState(warnings []StaffContactWarning) string {
	if len(warnings) == 0 {
		return staffContactHealthOK
	}
	codes := make([]string, len(warnings))
	for i, w := range warnings {
		codes[i] = w.Code
	}
	return strings.Join(codes, ",")
}

func staffContactHealthNotice(warnings []StaffContactWarning) string {
	if len(warnings) == 0 {
		return "The staff contact no longer has a warning."
	}
	names := make([]string, len(warnings))
	for i, w := range warnings {
		names[i] = w.Name
	}
	return "The staff contact needs attention. " + strings.Join(names, " ") + " Site & branding says what to do."
}
