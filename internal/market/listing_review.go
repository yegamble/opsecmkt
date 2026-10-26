package market

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"
)

// MOD72-6 (owner directive 2026-10-20): listing hold mode with per-vendor auto-approve (migration 145).
//
//   - settings listing_review_mode: "publish" (the default; a new or edited listing is public at once, as before) or
//     "hold". A missing or unknown value holds (fail closed). Only an administrator changes it (/admin/listing-review,
//     password-confirmed and audited).
//   - products.review_state: "approved", "pending" or "rejected". In hold mode a vendor's new listing, and its own edit of an
//     approved listing that changes what buyers see (title, description, category, subcategory, region, additional
//     shipping regions, fulfillment type, price, dispatch window, or a digital listing's switch between vendor and
//     automatic delivery) or adds a photo, is saved "pending". Stock, digital units and automatic delivery content
//     (which staff never see) do not re-hold it, nor does an administrator's edit. An administrator's own listings and the listings of a
//     vendor an administrator marked users.listing_auto_approve (/admin/vendor-auto-approve) are never held. A ban
//     clears that flag and lifting it does not set it again (admin_ban.go, A-650); no other code writes it
//     (TestAutoApproveOnlyWriters). The account page may turn it on by confirming a HIGH_REPUTATION label (A-653).
//   - A listing that is not approved is on no buyer or guest path (catalog, search, categories, vendor page, product page, checkout
//     page, draft and payment request: load.go, actions_orders.go, orders_hooks.go) and its photos are not on the
//     public image route (image_routes.go). Its vendor sees it marked "Awaiting review"; moderators and administrators
//     see it on its product page and on the moderation desk. Orders placed before an edit re-held it continue.
//   - A moderator or administrator approves it, or rejects it with a reason (/moderator/listing-review,
//     password-confirmed, refused when the vendor changed it after the desk was rendered). Rejection is not a
//     takedown (owner decision 2026-10-20): the listing becomes "rejected" with the reason in
//     products.review_rejection_reason, stays hidden and is not archived. Its vendor sees "Rejected — <reason>" and
//     can edit it; the vendor's next change to what buyers see, or a new photo, resubmits it ("pending", reason
//     cleared, audited) whatever the mode, while a stock-only change or an administrator's edit does not. Both
//     accounts are audited and the vendor is notified. A takedown (adminListingAction) stays a separate staff action.
//   - Switching back to publish approves nothing: listings already pending wait for staff. Moderation capture, its
//     overflow and the model path never read or write review_state (TestModerationNeverWritesReviewState).

const (
	settingListingReviewMode = "listing_review_mode"
	listingReviewLimit       = 50
)

func init() {
	registerAction("/moderator/listing-review", actionSpec{Roles: []string{"moderator", "admin"}, OwnTx: true, ConfirmedFormNonce: true, Run: listingReviewAction})
	registerAction("/admin/listing-review", actionSpec{Roles: []string{"admin"}, OwnTx: true, ConfirmedFormNonce: true, Run: listingReviewModeAction})
	registerAction("/admin/vendor-auto-approve", actionSpec{Roles: []string{"admin"}, OwnTx: true, ConfirmedFormNonce: true, Run: vendorAutoApproveAction})
	registerLoader("moderator", listingReviewQueueLoader)
	registerLoader("admin", listingReviewAdminLoader)
	registerPreview("moderator", func(d *PageData) {
		d.ListingReviews = []ListingReview{{Product: Product{ID: "preview-held", Title: "Sample listing awaiting review", Description: "Preview only: a vendor's new listing waits here until a moderator or administrator approves or rejects it.",
			Category: "Hardware", Region: "Worldwide", Kind: "physical", Vendor: "new_seller", PriceXMR: "0.500000000000"}, Since: "2026-01-01 09:00 UTC", Version: "1767258000000000"}}
		d.ListingReviewsTotal = 1
	})
	registerPreview("admin", func(d *PageData) {
		d.ListingReviewAdmin = &ListingReviewAdminView{Hold: true, Pending: 1, AutoApproved: []string{"ghost_circuit"}}
	})
}

// ListingReview is one listing on the moderation desk's review queue.
type ListingReview struct {
	Product
	Since   string // when it entered review (its last change), UTC
	Version string // listingVersionSQL when the desk was rendered: a review of a listing changed since is refused
}

// listingVersionSQL identifies the reviewed revision of listing p: its last change, to the microsecond. A vendor's
// edit sets products.updated, a held save (holdListing) sets it too, and so does any new photo on a pending listing,
// held or not (storeImage, QA72-1), so a moderator never approves or rejects content they have not seen.
const listingVersionSQL = `(extract(epoch from p.updated)*1000000)::bigint::text`

// ListingReviewAdminView is the administrator's listing review panel: the mode, how many listings wait and the
// active vendors whose listings publish without review (at most 100, by handle).
type ListingReviewAdminView struct {
	Hold         bool
	Pending      int
	AutoApproved []string
}

// listingHeldSQL decides, for vendor $1, whether a listing saved now waits for review: the mode is not exactly
// publish (missing or unknown holds) and the vendor is neither an administrator nor auto-approved.
const listingHeldSQL = `SELECT u.role<>'admin' AND NOT u.listing_auto_approve
 AND NOT EXISTS(SELECT 1 FROM settings WHERE key='listing_review_mode' AND value='publish') FROM users u WHERE u.id=$1`

// listingHeld reports whether a listing vendorID saves in tx now waits for staff review. It share-locks the mode
// row first, so an administrator's mode change waits for this save to commit, or this save sees the change.
func listingHeld(ctx context.Context, tx *sql.Tx, vendorID string) (bool, error) {
	if _, err := tx.ExecContext(ctx, `SELECT 1 FROM settings WHERE key='listing_review_mode' FOR SHARE`); err != nil {
		return false, err
	}
	var held bool
	err := tx.QueryRowContext(ctx, listingHeldSQL, vendorID).Scan(&held)
	return held, err
}

// listingHeldNow is listingHeld for a page: whether vendorID's next save would wait for review, read without locks.
func listingHeldNow(ctx context.Context, q rowQuerier, vendorID string) (bool, error) {
	var held bool
	err := q.QueryRowContext(ctx, listingHeldSQL, vendorID).Scan(&held)
	return held, err
}

// holdListing applies the review rule to the vendor's own save of listing id (row-locked by the caller) that changed
// what buyers see: a rejected listing is resubmitted (pending, reason cleared, audited on the vendor) whatever the
// mode, since only staff approve it; otherwise the listing waits for review when the save is held (listingHeld). It
// reports whether the listing now waits for review. It changes no column the moderation capture trigger reads, so
// it queues no capture.
func holdListing(ctx context.Context, tx *sql.Tx, id, vendorID string) (bool, error) {
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT review_state FROM products WHERE id=$1`, id).Scan(&state); err != nil {
		return false, err
	}
	if state == "rejected" {
		if _, err := tx.ExecContext(ctx, `UPDATE products SET review_state='pending',review_rejection_reason=NULL,updated=now() WHERE id=$1`, id); err != nil {
			return false, err
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", vendorID, "Resubmitted listing "+shortID(id)+" for review")
		return err == nil, err
	}
	held, err := listingHeld(ctx, tx, vendorID)
	if err != nil || !held {
		return false, err
	}
	// updated moves even when the listing was already pending, so a review form rendered before this save is stale.
	_, err = tx.ExecContext(ctx, `UPDATE products SET review_state='pending',updated=now() WHERE id=$1`, id)
	return err == nil, err
}

// listingReviewAction approves or rejects a pending listing from the moderation desk. Like a takedown (A-163), the
// listing is looked up before the password is checked, the users rows are locked before the listing, the reason is
// 10–500 characters after trimming, and both accounts are audited with a link-free notification to the vendor.
func listingReviewAction(c *actionCtx) (actionResult, error) {
	choice := c.Form.Get("action")
	approve := choice == "approve"
	if !approve && choice != "reject" {
		return actionResult{}, fail(400, "Choose approve or reject. Nothing was changed.")
	}
	var reason string
	if !approve {
		reason = strings.TrimSpace(strings.ReplaceAll(c.Form.Get("reason"), "\r\n", "\n"))
		if n := utf8.RuneCountInString(reason); !utf8.ValidString(reason) || strings.ContainsRune(reason, 0) || n < minTakedownReason || n > maxTakedownReason {
			return actionResult{}, fail(400, "Give the vendor a reason of 10–500 characters. Nothing was changed.")
		}
	}
	id := c.Form.Get("id")
	var vendorID string
	err := c.A.db.QueryRowContext(c.Ctx(), "SELECT vendor_id FROM products WHERE id=$1", id).Scan(&vendorID)
	if err == sql.ErrNoRows {
		return actionResult{}, fail(404, "Listing not found")
	}
	if err != nil {
		return actionResult{}, err
	}
	if vendorID == c.User.ID {
		return actionResult{}, fail(400, "This is your own listing: another moderator or administrator reviews it. Nothing was changed.")
	}
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		ctx, tx := c.Ctx(), c.Tx
		if err := waitForWalletLocks(ctx, tx); err != nil {
			return actionResult{}, err
		}
		if _, err := tx.ExecContext(ctx, `SELECT 1 FROM users WHERE id=$1 OR id=$2 FOR KEY SHARE`, c.User.ID, vendorID); err != nil {
			return actionResult{}, err
		}
		var title, state, handle string
		var archived bool
		var version string
		if err := tx.QueryRowContext(ctx, `SELECT p.title,p.review_state,p.archived,u.handle,`+listingVersionSQL+` FROM products p JOIN users u ON u.id=p.vendor_id
			WHERE p.id=$1 FOR UPDATE OF p`, id).Scan(&title, &state, &archived, &handle, &version); err != nil {
			return actionResult{}, err
		}
		if state != "pending" || archived {
			return actionResult{}, fail(409, "This listing is not awaiting review: it was approved, rejected or archived meanwhile. Nothing was changed.")
		}
		if version != c.Form.Get("version") {
			return actionResult{}, fail(409, "The vendor changed this listing after you opened the moderation desk. Reload the desk and review it again. Nothing was changed.")
		}
		staff := "moderator"
		if c.User.Role == "admin" {
			staff = "administrator"
		}
		short := shortID(id)
		var audit, vendorAudit, note, notice string
		if approve {
			if _, err := tx.ExecContext(ctx, `UPDATE products SET review_state='approved' WHERE id=$1`, id); err != nil {
				return actionResult{}, err
			}
			audit = "Approved listing " + short + " of " + handle
			vendorAudit = "Listing " + short + " approved in review by " + staff + " " + c.User.Handle
			note = "The market staff approved your listing “" + title + "” (" + short + "). It is now visible in the marketplace and accepts orders."
			notice = "listing-approved"
		} else {
			// Not a takedown: the listing is not archived and needs no administrator; the vendor's next change to
			// what buyers see resubmits it (holdListing).
			if _, err := tx.ExecContext(ctx, `UPDATE products SET review_state='rejected',review_rejection_reason=$2 WHERE id=$1`, id, reason); err != nil {
				return actionResult{}, err
			}
			audit = "Rejected listing " + short + " of " + handle + ": " + reason
			vendorAudit = "Listing " + short + " rejected in review by " + staff + " " + c.User.Handle + ": " + reason
			note = "The market staff rejected your listing “" + title + "” (" + short + ") with the reason “" + reason + "”. It stays hidden from the marketplace and accepts no orders. Edit it to fix the problem: your next change to what buyers see sends it back for review. If you think this is wrong, contact the market staff."
			notice = "listing-rejected"
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", vendorID, vendorAudit); err != nil {
			return actionResult{}, err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,$3)", randomToken(), vendorID, note); err != nil {
			return actionResult{}, err
		}
		return actionResult{Redirect: "/moderator", Notice: notice, Audit: audit}, nil
	})
}

// listingReviewModeLabel names a stored mode value for the audit trail: only "publish" publishes; anything else
// holds, an absent or unknown value included.
func listingReviewModeLabel(stored string) string {
	if stored == "publish" || stored == "hold" {
		return stored
	}
	return "unset (holds)"
}

// listingReviewModeAction sets listing_review_mode. Switching to publish approves nothing; the audit row says how many
// listings still wait for staff.
func listingReviewModeAction(c *actionCtx) (actionResult, error) {
	mode := c.Form.Get(settingListingReviewMode)
	if mode != "publish" && mode != "hold" {
		return actionResult{}, fail(400, "Choose publish at once or hold for review. Nothing was changed.")
	}
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		ctx, tx := c.Ctx(), c.Tx
		// An absent row holds (fail closed), as an empty value does.
		if _, err := tx.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES($1,'') ON CONFLICT(key) DO NOTHING", settingListingReviewMode); err != nil {
			return actionResult{}, err
		}
		var stored string
		if err := tx.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=$1 FOR UPDATE", settingListingReviewMode).Scan(&stored); err != nil {
			return actionResult{}, err
		}
		old := listingReviewModeLabel(stored)
		if stored == mode {
			return actionResult{}, fail(409, "Listing review is already set to "+mode+"; nothing was changed.")
		}
		if err := setSetting(c, settingListingReviewMode, mode); err != nil {
			return actionResult{}, err
		}
		audit := "Listing review mode changed from " + old + " to " + mode
		if mode == "publish" {
			var pending int
			if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM products WHERE review_state='pending' AND NOT archived").Scan(&pending); err != nil {
				return actionResult{}, err
			}
			if pending > 0 {
				audit += "; " + strconv.Itoa(pending) + " listing(s) still await review"
			}
		}
		return actionResult{Redirect: "/admin/vendors", Notice: "listing-review-mode", Audit: audit}, nil
	})
}

// vendorAutoApproveAction lets a vendor's listings publish without review, or makes them wait again, looked up by
// handle (as vendorVerifyAction). Turning it on needs an active vendor; turning it off works on any account. Both
// accounts are audited and the vendor is notified. It changes no listing: approved listings stay approved and
// pending ones still wait for staff. The account page's reputation panel may turn it on with confirm_reputation and the
// digest of the figures it showed (A-653): the label is recomputed under the account's row lock, and anything but
// HIGH_REPUTATION, or figures that changed since, is refused; the administrator's audit row names the label.
func vendorAutoApproveAction(c *actionCtx) (actionResult, error) {
	choice := c.Form.Get("action")
	on := choice == "on"
	if !on && choice != "off" {
		return actionResult{}, fail(400, "Choose publish without review or wait for review")
	}
	handle := strings.TrimSpace(c.Form.Get("handle"))
	if handle == "" {
		return actionResult{}, fail(400, "Enter a vendor handle")
	}
	// A-653: the account page may also confirm the vendor's reputation label; the digest names the figures it showed.
	confirmRep, digest := c.Form.Has("confirm_reputation"), c.Form.Get("reputation_digest")
	if confirmRep && (!on || digest == "") {
		return actionResult{}, fail(400, "A reputation confirmation only turns on publishing without review, from the vendor's account page. Nothing was changed.")
	}
	notFound := fail(404, "No account has the handle \""+handle+"\". Check the spelling: handles are case-sensitive.")
	var target string
	err := c.A.db.QueryRowContext(c.Ctx(), "SELECT id FROM users WHERE handle=$1", handle).Scan(&target)
	if err == sql.ErrNoRows {
		return actionResult{}, notFound
	}
	if err != nil {
		return actionResult{}, err
	}
	return confirmedTx(c, true, func(c *actionCtx) (actionResult, error) {
		ctx, tx, admin := c.Ctx(), c.Tx, c.User
		var seller User // CanSell: the vendor role, or a buyer or moderator with selling approval (PR #111)
		var auto, banned bool
		err := tx.QueryRowContext(ctx, "SELECT role,vendor_approved,listing_auto_approve,banned_at IS NOT NULL FROM users WHERE id=$1 FOR NO KEY UPDATE", target).Scan(&seller.Role, &seller.VendorApproved, &auto, &banned)
		if err == sql.ErrNoRows {
			return actionResult{}, notFound
		}
		if err != nil {
			return actionResult{}, err
		}
		switch {
		case on && banned:
			return actionResult{}, fail(409, handle+" is banned; nothing was changed.")
		case on && (seller.Role == "admin" || !seller.CanSell()):
			// An administrator's own listings are never held, so the flag means nothing for one.
			return actionResult{}, fail(409, handle+" is not a vendor account; nothing was changed.")
		case on && auto:
			return actionResult{}, fail(409, handle+" already publishes listings without review; nothing was changed.")
		case !on && !auto:
			return actionResult{}, fail(409, handle+"'s listings already wait for review; nothing was changed.")
		}
		confirmed := ""
		if confirmRep { // recomputed under the account's row lock, from the rows as they are now
			rep, err := vendorReputation(ctx, tx, target)
			if err != nil {
				return actionResult{}, err
			}
			if rep.Label != repHigh {
				return actionResult{}, fail(409, handle+"'s reputation label is "+rep.Label+", not "+repHigh+". Nothing was changed.")
			}
			if rep.Digest != digest {
				return actionResult{}, fail(409, handle+"'s reputation figures changed since the page was shown: review them again. Nothing was changed.")
			}
			confirmed = "; reputation confirmed as " + rep.Label // the label only; the vendor's own audit row and notice never name it
		}
		if _, err = tx.ExecContext(ctx, "UPDATE users SET listing_auto_approve=$2 WHERE id=$1", target, on); err != nil {
			return actionResult{}, err
		}
		vendorAudit, actor := "Listings publish without review: set by administrator "+admin.Handle, "Let "+handle+" publish listings without review"+confirmed
		body := "An administrator lets your new and edited listings publish at once, without waiting for staff review."
		if !on {
			vendorAudit, actor = "Listings wait for review again: set by administrator "+admin.Handle, "Made "+handle+"'s listings wait for review"
			body = "An administrator removed your permission to publish without review: while the market holds listings for review, your new and edited listings wait for staff approval. Listings already approved stay visible."
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO audit_events(user_id,action) VALUES($1,$2)", target, vendorAudit); err != nil {
			return actionResult{}, err
		}
		if _, err = insertNotification(ctx, tx, target, body, ""); err != nil {
			return actionResult{}, err
		}
		return actionResult{Redirect: adminUserBack(c, handle, "/admin/vendors"), Notice: "vendor-auto-approve", Audit: actor}, nil
	})
}

// listingReviewQueueLoader fills the moderation desk's review queue: listings awaiting review and not archived, the
// longest waiting first, at most listingReviewLimit of ListingReviewsTotal.
func listingReviewQueueLoader(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	if !staffViewer(d.User) {
		return nil
	}
	rows, err := a.db.QueryContext(ctx, `SELECT p.id,p.title,left(p.description,`+strconv.Itoa(excerptRunes+1)+`),p.category,p.region,p.kind,u.handle,p.vendor_id,p.xmr,
		to_char(p.updated,'YYYY-MM-DD HH24:MI "UTC"'),`+listingVersionSQL+`,count(*) OVER () FROM products p JOIN users u ON u.id=p.vendor_id
		WHERE p.review_state='pending' AND NOT p.archived ORDER BY p.updated,p.id LIMIT `+strconv.Itoa(listingReviewLimit))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var l ListingReview
		var xmr int64
		if err = rows.Scan(&l.ID, &l.Title, &l.Description, &l.Category, &l.Region, &l.Kind, &l.Vendor, &l.VendorID, &xmr, &l.Since, &l.Version, &d.ListingReviewsTotal); err != nil {
			return err
		}
		l.PriceXMR, l.Pending = amount(xmr, 12), true
		d.ListingReviews = append(d.ListingReviews, l)
	}
	return rows.Err()
}

// listingReviewAdminLoader fills the listing review panel on the administrator's Vendors section.
func listingReviewAdminLoader(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	if d.User == nil || d.User.Role != "admin" || d.AdminSection != "vendors" {
		return nil
	}
	v := &ListingReviewAdminView{Hold: d.Settings[settingListingReviewMode] != "publish"}
	if err := a.db.QueryRowContext(ctx, "SELECT count(*) FROM products WHERE review_state='pending' AND NOT archived").Scan(&v.Pending); err != nil {
		return err
	}
	rows, err := a.db.QueryContext(ctx, "SELECT u.handle FROM users u WHERE u.listing_auto_approve AND "+activeVendorSQL("u")+" ORDER BY u.handle LIMIT 100")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var h string
		if err = rows.Scan(&h); err != nil {
			return err
		}
		v.AutoApproved = append(v.AutoApproved, h)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	d.ListingReviewAdmin = v
	return nil
}
