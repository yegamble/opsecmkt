package market

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
)

// Contacts: the PGP keys people need to encrypt to each other (vendor page, order page, messages), the
// /messages?to=<handle> recipient prefill, and the unread-notification count shown in the navigation.
// Shipping addresses are never stored: buyers send them as messages encrypted to the vendor's key.

func init() {
	registerLoader("*", unreadNotificationsLoader)
	registerLoader("vendor", func(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
		if len(d.Users) == 0 {
			return nil
		}
		k, err := contactKey(ctx, a.db, d.Users[0].ID, d.Users[0].Handle, "vendor")
		if err == nil {
			d.Contacts = []ContactKey{k}
		}
		return err
	})
	registerLoader("messages", messageRecipientLoader)
	registerLoader("order", disputeStaffLoader)

	registerPreview("*", func(d *PageData) {
		if d.User != nil {
			d.UnreadNotifications = 1
		}
	})
	registerPreview("notifications", func(d *PageData) {
		d.Notifications = []Notification{{ID: "sample-notification", Body: "Sample notification for the read-only preview. No order exists.", Created: "Sample"}}
	})
	registerPreview("vendor", func(d *PageData) {
		if len(d.Users) > 0 {
			d.Contacts = []ContactKey{previewContactKey(d.Users[0].Handle, "vendor")}
		}
	})
	registerPreview("order", func(d *PageData) {
		if d.Order != nil {
			d.OrderViewer = roleBuyer
			d.Contacts = []ContactKey{{Handle: d.Order.Vendor, Relation: "vendor"}}
		}
	})
	registerPreview("messages", func(d *PageData) {
		d.MessageTo = "ghost_circuit"
		d.Contacts = []ContactKey{previewContactKey("ghost_circuit", "")}
	})
}

// Only order parties need a directory of independent staff to contact about a dispute.
// Staff roles and saved keys are read live; role labels never imply key ownership verification.
// Suspended staff are left out: they cannot sign in to read an encrypted message (A-118).
func disputeStaffLoader(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	o, u := d.Order, d.User
	if o == nil || u == nil || (o.State != stateDisputed && o.State != stateResolved) ||
		(u.ID != o.BuyerID && u.ID != o.VendorID) {
		return nil
	}
	rows, err := a.db.QueryContext(ctx, `SELECT id,handle,role FROM users WHERE role IN ('moderator','admin') AND suspended_at IS NULL AND id<>$1 AND id<>$2 ORDER BY handle`, o.BuyerID, o.VendorID)
	if err != nil {
		return err
	}
	var staff [][3]string
	for rows.Next() {
		var person [3]string
		if err := rows.Scan(&person[0], &person[1], &person[2]); err != nil {
			rows.Close()
			return err
		}
		staff = append(staff, person)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, person := range staff {
		key, err := contactKey(ctx, a.db, person[0], person[1], person[2])
		if err != nil {
			return err
		}
		d.StaffContacts = append(d.StaffContacts, key)
	}
	return nil
}

func previewContactKey(handle, relation string) ContactKey {
	return ContactKey{Handle: handle, Relation: relation, Fingerprint: formatFingerprint("0000000000000000000000000000000000000000"),
		Armored: "-----BEGIN PGP PUBLIC KEY BLOCK-----\n(sample preview — not a real key)\n-----END PGP PUBLIC KEY BLOCK-----"}
}

// contactKey reads a user's saved key. Verified comes from loadPGPAccount: an ownership proof recorded for
// exactly the saved key. An unparseable key is reported, never shown as usable.
func contactKey(ctx context.Context, db *sql.DB, userID, handle, relation string) (ContactKey, error) {
	p, err := loadPGPAccount(ctx, db, userID, false)
	if err != nil {
		return ContactKey{}, err
	}
	k := ContactKey{Handle: handle, Relation: relation}
	switch {
	case p.Key != nil:
		// Others see the canonical key (A-79) and the proof date only; the owner's pages keep the minute.
		verifiedOn, _, _ := strings.Cut(p.VerifiedAt, " ")
		k.Armored, k.Fingerprint, k.Verified, k.VerifiedAt = p.Canonical, formatFingerprint(p.Fingerprint), p.Verified, verifiedOn
	case p.Armored != "":
		k.Unreadable = true
	}
	return k, nil
}

// orderContacts returns the key of the viewer's counterparty, or both parties' keys for a dispute reviewer.
func orderContacts(ctx context.Context, a *App, o *Order, viewer string) ([]ContactKey, error) {
	var parties [][3]string // id, handle, relation
	if viewer != roleVendor {
		parties = append(parties, [3]string{o.VendorID, o.Vendor, roleVendor})
	}
	if viewer != roleBuyer {
		parties = append(parties, [3]string{o.BuyerID, o.Buyer, roleBuyer})
	}
	var out []ContactKey
	for _, p := range parties {
		k, err := contactKey(ctx, a.db, p[0], p[1], p[2])
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, nil
}

// messageRecipientLoader prefills the recipient from ?to=<handle> (ignored unless it matches the handle
// pattern) and shows that user's key when the handle exists and is not the viewer.
func messageRecipientLoader(ctx context.Context, a *App, r *http.Request, d *PageData) error {
	to := r.URL.Query().Get("to")
	if !handlePattern.MatchString(to) || d.User == nil {
		return nil
	}
	d.MessageTo = to
	var id, handle string
	err := a.db.QueryRowContext(ctx, "SELECT id,handle FROM users WHERE handle=$1", to).Scan(&id, &handle)
	if err == sql.ErrNoRows || err == nil && id == d.User.ID {
		return nil
	}
	if err != nil {
		return err
	}
	k, err := contactKey(ctx, a.db, id, handle, "")
	if err == nil {
		d.Contacts = []ContactKey{k}
	}
	return err
}

// unreadNotificationsLoader counts unread notifications for the navigation (notifications_user index).
func unreadNotificationsLoader(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	if d.User == nil {
		return nil
	}
	return a.db.QueryRowContext(ctx, "SELECT count(*) FROM notifications WHERE user_id=$1 AND NOT is_read", d.User.ID).Scan(&d.UnreadNotifications)
}
