package market

import (
	"database/sql"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// P4 Inventory: vendors edit, archive and restore their own listings; administrators may manage any listing
// except its automatic delivery content, which only the listing's vendor can read or change. An administrator
// archives another vendor's listing only by taking it down with a reason (A-163), which the vendor cannot undo.

func init() {
	managers := []string{"vendor", "admin"}
	registerAction("/listings/update", actionSpec{Roles: managers, FormNonce: true, Run: listingUpdateAction})
	registerAction("/listings/archive", actionSpec{Roles: managers, FormNonce: true, Run: func(c *actionCtx) (actionResult, error) { return listingArchiveAction(c, true) }})
	registerAction("/listings/restore", actionSpec{Roles: managers, FormNonce: true, Run: func(c *actionCtx) (actionResult, error) { return listingArchiveAction(c, false) }})
	registerAction("/admin/listing", actionSpec{Roles: []string{"admin"}, OwnTx: true, ConfirmedFormNonce: true, Run: adminListingAction})
}

type managedListing struct {
	ID, VendorID, Kind string
	Stock              int
	Archived           bool
	Takedown           string // an administrator's takedown reason; "" when not taken down
}

// lockListing row-locks a listing the actor may manage (its vendor, or any administrator).
// Anything else is reported as not found so listing ids of other vendors are not confirmed.
//
// Lock order: the users rows of the actor and the listing's vendor (FOR KEY SHARE), then the listing. An
// administrator's role change locks the vendor's users row FOR UPDATE and then archives that vendor's listings; a
// listing action that locked the listing first and then took a users row, which the foreign keys of its audit and
// notification inserts do, would deadlock with it (PR #24 review R24-1; TestListingActionsDoNotDeadlockWithRoleChange,
// TestPhotoRemoveDoesNotDeadlockWithRoleChange). Taken first, the users rows make the action wait for the change
// instead. The vendor's row is locked only when the actor may manage the listing, so another vendor's id is not
// held up by (and cannot probe) a change to that vendor's account. Every caller takes users rows only after this;
// the administrator's takedown (adminListingAction) keeps the same order.
func lockListing(c *actionCtx, id string) (managedListing, error) {
	l := managedListing{ID: id}
	if err := waitForWalletLocks(c.Ctx(), c.Tx); err != nil { // a checkout may hold the row (escrowCheckoutAction)
		return l, err
	}
	if _, err := c.Tx.ExecContext(c.Ctx(), `SELECT 1 FROM users WHERE id=$1 OR id=(SELECT vendor_id FROM products WHERE id=$2 AND (vendor_id=$1 OR $3='admin')) FOR KEY SHARE`, c.User.ID, id, c.User.Role); err != nil {
		return l, err
	}
	err := c.Tx.QueryRowContext(c.Ctx(), `SELECT vendor_id,kind,stock,archived,coalesce(admin_archive_reason,'') FROM products WHERE id=$1 AND (vendor_id=$2 OR $3='admin') FOR UPDATE`, id, c.User.ID, c.User.Role).Scan(&l.VendorID, &l.Kind, &l.Stock, &l.Archived, &l.Takedown)
	if err == sql.ErrNoRows {
		return l, fail(404, "Listing not found")
	}
	return l, err
}

// listingAudit names the listing by a short id and flags administrator changes to another vendor's listing.
func listingAudit(action string, c *actionCtx, l managedListing) string {
	s := action + " listing " + l.ID[:min(8, len(l.ID))]
	if l.VendorID != c.User.ID {
		s += " as administrator"
	}
	return s
}

// openOrdersSQL counts orders that still depend on the listing's fulfillment type. A fund-free draft
// is repriced and revalidated at checkout, so it must not let an abandoned checkout freeze the listing.
// Keep the guard for an anomalous historical draft with a deposit or escrow record.
const openOrdersSQL = `SELECT count(*) FROM orders o WHERE product_id=$1 AND state NOT IN ('completed','resolved','cancelled')
 AND (state<>'draft' OR EXISTS(SELECT 1 FROM payments pm WHERE pm.order_id=o.id) OR EXISTS(SELECT 1 FROM escrows es WHERE es.order_id=o.id))`

func listingUpdateAction(c *actionCtx) (actionResult, error) {
	ctx := c.Ctx()
	in, err := validateListing(c.Form)
	if err != nil {
		return actionResult{}, err
	}
	seen, err := strconv.Atoi(c.Form.Get("stock_seen"))
	if err != nil {
		return actionResult{}, fail(400, "Reload the edit form and try again.")
	}
	l, err := lockListing(c, c.Form.Get("id"))
	if err != nil {
		return actionResult{}, err
	}
	// was is the listing before an administrator's edit of another vendor's listing, for the vendor's notice (A-516).
	// It also tells whether the vendor's own edit changed what buyers see, which re-holds it for review (MOD72-6).
	was := listingInput{Kind: l.Kind, Stock: l.Stock}
	var storedDelivery string
	if err = c.Tx.QueryRowContext(ctx, `SELECT delivery_content,title,description,category,region,btc,xmr,coalesce(dispatch_days,0) FROM products WHERE id=$1`, l.ID).Scan(
		&storedDelivery, &was.Title, &was.Description, &was.Category, &was.Region, &was.BTC, &was.XMR, &was.DispatchDays); err != nil {
		return actionResult{}, err
	}
	if l.VendorID != c.User.ID {
		// Automatic delivery content is what every buyer receives in the vendor's name. Other editors
		// (administrators) never see it, so their saves keep the stored content whatever the form sends.
		in.DeliveryContent = storedDelivery
		if in.DeliveryContent != "" && in.Kind != "digital" {
			return actionResult{}, fail(400, "This listing has automatic delivery content that only its vendor can remove, so it must stay digital.")
		}
	}
	if in.Kind != l.Kind {
		// Orders read the fulfillment type from the listing; changing it would change their shipping/delivery path.
		var open int
		if err = c.Tx.QueryRowContext(ctx, openOrdersSQL, l.ID).Scan(&open); err != nil {
			return actionResult{}, err
		}
		if open > 0 {
			return actionResult{}, fail(409, "The fulfillment type cannot change while this listing has open orders.")
		}
	}
	// Stock is only written when the editor changed it. Order payment requests reserve stock concurrently,
	// so a change based on a stale value is refused instead of silently overwriting a reservation.
	stock := l.Stock
	if in.Stock != seen {
		if l.Stock != seen {
			return actionResult{}, fail(409, "Stock changed to "+strconv.Itoa(l.Stock)+" while you were editing (orders reserve stock). Reload and try again.")
		}
		stock = in.Stock
	}
	// The dispatch window (P2-R5): a physical listing keeps its stored window when the form sends none, and takes the
	// default when it has none (a listing saved before migration 079, or one that was digital); a digital one has none.
	// Orders and escrows snapshot the window, so changing it affects only later checkouts.
	saved := in
	saved.Stock = stock
	err = c.Tx.QueryRowContext(ctx, `UPDATE products SET title=$2,description=$3,category=$4,region=$5,kind=$6,btc=$7,xmr=$8,stock=$9,delivery_content=$10,
		dispatch_days=CASE WHEN $6<>'physical' THEN NULL ELSE COALESCE(NULLIF($11,0),dispatch_days,$12) END,private_expired_at=NULL,updated=now() WHERE id=$1
		RETURNING coalesce(dispatch_days,0)`,
		l.ID, in.Title, in.Description, in.Category, in.Region, in.Kind, in.BTC, in.XMR, stock, in.DeliveryContent, in.DispatchDays, defaultDispatchDays).Scan(&saved.DispatchDays)
	if err != nil {
		return actionResult{}, err
	}
	changed := changedListingFields(was, saved)
	notice := "listing-saved"
	// MOD72-6: in hold mode the vendor's own change to what buyers see sends an approved listing back to staff review
	// (listing_review.go); a stock-only change, automatic delivery content (staff never see it) and an
	// administrator's edit do not.
	if l.VendorID == c.User.ID && slices.ContainsFunc(changed, func(f string) bool { return f != "stock" }) {
		held, err := holdListing(ctx, c.Tx, l.ID, l.VendorID)
		if err != nil {
			return actionResult{}, err
		}
		if held {
			notice = "listing-saved-held"
		}
	}
	// An administrator's change to another vendor's listing is told to the vendor, naming what changed, and kept in
	// the vendor's audit trail too (A-516), as an administrator's photo change is (A-190). A save that changed
	// nothing tells them nothing.
	if l.VendorID != c.User.ID && len(changed) > 0 {
		short, fields := shortID(l.ID), strings.Join(changed, ", ")
		if _, err = c.Tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", l.VendorID, "Listing "+short+" edited by administrator "+c.User.Handle+": changed "+fields); err != nil {
			return actionResult{}, err
		}
		note := "An administrator edited your listing “" + was.Title + "” (" + short + ") and changed: " + fields + ". Check it on your vendor desk; if you think this is wrong, contact the market staff."
		if _, err = c.Tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), l.VendorID, note); err != nil {
			return actionResult{}, err
		}
	}
	return actionResult{Redirect: "/listing-edit?id=" + l.ID, Notice: notice, Audit: listingAudit("Updated", c, l)}, nil
}

// changedListingFields names, in form order, the fields an edit changed (automatic delivery content excepted: an
// administrator never changes it).
func changedListingFields(was, now listingInput) []string {
	var out []string
	for _, f := range []struct {
		name    string
		changed bool
	}{
		{"title", was.Title != now.Title},
		{"description", was.Description != now.Description},
		{"category", was.Category != now.Category},
		{"region", was.Region != now.Region},
		{"fulfillment type", was.Kind != now.Kind},
		{"Monero price", was.XMR != now.XMR},
		{"stock", was.Stock != now.Stock},
		{"dispatch window", was.DispatchDays != now.DispatchDays},
	} {
		if f.changed {
			out = append(out, f.name)
		}
	}
	return out
}

// listingArchiveAction archives (hides from the catalog and refuses new drafts) or restores a listing.
// Existing orders are not changed. An administrator archives another vendor's listing only with a takedown, and a
// taken-down listing is restored only with adminListingAction (A-163).
func listingArchiveAction(c *actionCtx, archive bool) (actionResult, error) {
	l, err := lockListing(c, c.Form.Get("id"))
	if err != nil {
		return actionResult{}, err
	}
	if archive && l.VendorID != c.User.ID {
		return actionResult{}, fail(400, "Taking down another vendor's listing needs a reason and your password: use Take down listing on its edit page. Nothing was changed.")
	}
	if !archive && l.Takedown != "" {
		if c.User.Role == "admin" {
			return actionResult{}, fail(409, "An administrator took this listing down. Restore it with Restore listing on its edit page, confirmed with your password. Nothing was changed.")
		}
		// The staff contact (A-119, A-279) is the path to an administrator.
		contact, err := staffContactRefusal(c.Ctx(), c.Tx)
		if err != nil {
			return actionResult{}, err
		}
		return actionResult{}, fail(409, "An administrator took this listing down with the reason “"+l.Takedown+"”. You can still edit it, but only an administrator can restore it. If you think this is wrong, ask an administrator to review it. "+contact)
	}
	if l.Archived == archive {
		if archive {
			return actionResult{}, fail(409, "This listing is already archived.")
		}
		return actionResult{}, fail(409, "This listing is not archived.")
	}
	if !archive {
		if ok, err := sellerIsVendorLocked(c, l.ID); err != nil || !ok {
			if err == nil {
				err = fail(409, "This listing's owner is no longer a vendor or is closing their account, so it cannot be restored.")
			}
			return actionResult{}, err
		}
	}
	if _, err = c.Tx.ExecContext(c.Ctx(), `UPDATE products SET archived=$2,archived_at=CASE WHEN $2 THEN now() END,private_expired_at=NULL,updated=now() WHERE id=$1`, l.ID, archive); err != nil {
		return actionResult{}, err
	}
	action, res := "Restored", actionResult{Redirect: "/vendor-dashboard", Notice: "listing-restored"}
	if archive {
		action = "Archived"
		res.Notice = "listing-archived"
	}
	if l.VendorID != c.User.ID {
		res.Redirect = "/listing-edit?id=" + l.ID
	}
	res.Audit = listingAudit(action, c, l)
	return res, nil
}

// Takedown reasons are shown to the vendor, so they must say something and fit a notification.
const minTakedownReason, maxTakedownReason = 10, 500

// adminListingAction takes down another vendor's listing or restores a taken-down one (A-163), from the listing's
// edit page. A takedown archives the listing with the administrator's reason (10–500 characters after trimming,
// stored unencrypted in products.admin_archive_reason and shown to the vendor, never on public pages). The vendor
// can still edit it but not restore it, and it is not delivered automatically while taken down (autoDeliveryHook).
// Only this restore clears the reason, and like any restore it needs the owner to still be a vendor. The
// administrator confirms with their own password (and authenticator code when enrolled); both accounts are
// audited, with the reason, and the vendor gets a link-free notification.
func adminListingAction(c *actionCtx) (actionResult, error) {
	choice := c.Form.Get("action")
	takedown := choice == "takedown"
	if !takedown && choice != "restore" {
		return actionResult{}, fail(400, "Choose take down or restore")
	}
	var reason string
	if takedown {
		reason = strings.TrimSpace(strings.ReplaceAll(c.Form.Get("reason"), "\r\n", "\n"))
		// PostgreSQL text refuses NUL, so one is the reason's 400 here rather than a failed update (PR #27 NIT-1).
		if n := utf8.RuneCountInString(reason); !utf8.ValidString(reason) || strings.ContainsRune(reason, 0) || n < minTakedownReason || n > maxTakedownReason {
			return actionResult{}, fail(400, "Give the vendor a reason of 10–500 characters. Nothing was changed.")
		}
	}
	id := c.Form.Get("id")
	// The listing is looked up before the password is checked, so a stale form does not use a confirmation attempt.
	var vendorID string
	err := c.A.db.QueryRowContext(c.Ctx(), "SELECT vendor_id FROM products WHERE id=$1", id).Scan(&vendorID)
	if err == sql.ErrNoRows {
		return actionResult{}, fail(404, "Listing not found")
	}
	if err != nil {
		return actionResult{}, err
	}
	if takedown && vendorID == c.User.ID {
		return actionResult{}, fail(400, "This is your own listing: archive it from the vendor desk. Nothing was changed.")
	}
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		ctx, tx := c.Ctx(), c.Tx
		if err := waitForWalletLocks(ctx, tx); err != nil { // a checkout may hold the row (escrowCheckoutAction)
			return actionResult{}, err
		}
		// The users rows before the listing, as lockListing takes them: a role change holds the vendor's row and then
		// archives the listings (PR #24 re-check N-1; TestAdminTakedownDoesNotDeadlockWithRoleChange).
		if _, err := tx.ExecContext(ctx, `SELECT 1 FROM users WHERE id=$1 OR id=$2 FOR KEY SHARE`, c.User.ID, vendorID); err != nil {
			return actionResult{}, err
		}
		var title, current, handle string
		var reviewState string
		err := tx.QueryRowContext(ctx, `SELECT p.title,coalesce(p.admin_archive_reason,''),u.handle,p.review_state FROM products p JOIN users u ON u.id=p.vendor_id
			WHERE p.id=$1 FOR UPDATE OF p`, id).Scan(&title, &current, &handle, &reviewState)
		if err != nil {
			return actionResult{}, err
		}
		short := shortID(id)
		var audit, vendorAudit, note string
		if takedown {
			if current != "" {
				return actionResult{}, fail(409, "This listing is already taken down; nothing was changed.")
			}
			if _, err = tx.ExecContext(ctx, "UPDATE products SET archived=true,archived_at=now(),admin_archive_reason=$2,private_expired_at=NULL,updated=now() WHERE id=$1", id, reason); err != nil {
				return actionResult{}, err
			}
			audit = "Took down listing " + short + " of " + handle + ": " + reason
			vendorAudit = "Listing " + short + " taken down by administrator " + c.User.Handle + ": " + reason
			note = "An administrator took down your listing “" + title + "” (" + short + ") with the reason “" + reason + "”. It is hidden from the marketplace and accepts no new orders. Existing orders continue, but paid orders on it are not delivered automatically: deliver or cancel them from the order page. You can still edit the listing; only an administrator can restore it. If you think this is wrong, contact the market staff."
		} else {
			if current == "" {
				return actionResult{}, fail(409, "This listing was not taken down by an administrator; nothing was changed.")
			}
			if ok, err := sellerIsVendorLocked(c, id); err != nil || !ok {
				if err == nil {
					err = fail(409, "This listing's owner is no longer a vendor or is closing their account, so it cannot be restored. Nothing was changed.")
				}
				return actionResult{}, err
			}
			if _, err = tx.ExecContext(ctx, "UPDATE products SET archived=false,archived_at=NULL,admin_archive_reason=NULL,private_expired_at=NULL,updated=now() WHERE id=$1", id); err != nil {
				return actionResult{}, err
			}
			audit = "Restored listing " + short + " of " + handle + " after a takedown"
			vendorAudit = "Listing " + short + " restored by administrator " + c.User.Handle
			note = "An administrator restored your listing “" + title + "” (" + short + "). It is visible in the marketplace again and accepts new orders."
			// MOD72-6: a listing taken down while not approved stays hidden: a pending one until staff approve it, a
			// rejected one until its vendor edits it to resubmit it.
			switch reviewState {
			case "pending":
				audit += "; it is not approved for buyers"
				note = "An administrator restored your listing “" + title + "” (" + short + "). Buyers still cannot see it until the market staff approve it in review."
			case "rejected":
				audit += "; it is not approved for buyers"
				note = "An administrator restored your listing “" + title + "” (" + short + "). It is still rejected in review, so buyers cannot see it: edit it to fix the problem, and your next change to what buyers see sends it back for review."
			}
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", vendorID, vendorAudit); err != nil {
			return actionResult{}, err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), vendorID, note); err != nil {
			return actionResult{}, err
		}
		notice := "listing-restored"
		if takedown {
			notice = "listing-taken-down"
		}
		return actionResult{Redirect: "/listing-edit?id=" + id, Notice: notice, Audit: audit}, nil
	})
}
