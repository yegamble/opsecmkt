package market

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

const maxDigitalImportUnits = 1000

type DigitalInventoryUnit struct{ ID, Content string }
type DigitalInventoryView struct {
	Mode                string
	Owner               bool
	Available, Reserved int
	Units               []DigitalInventoryUnit
	ModeError           string // a refused method form's problem (digitalModeRefusal), shown next to the method field
}

func init() {
	roles := []string{"vendor", "admin"}
	registerAction("/listings/digital-mode", actionSpec{Roles: roles, FormNonce: true, Run: digitalModeAction})
	registerAction("/listings/digital-units", actionSpec{Roles: roles, FormNonce: true, Run: digitalUnitImportAction})
	registerAction("/listings/digital-units/remove", actionSpec{Roles: roles, FormNonce: true, Run: digitalUnitRemoveAction})
	registerAction("/listings/digital-units/clear", actionSpec{Roles: roles, FormNonce: true, Run: digitalUnitClearAction})
}

func digitalOwnerListing(c *actionCtx) (managedListing, error) {
	l, err := lockListing(c, c.Form.Get("id"))
	if err != nil {
		return l, err
	}
	if l.VendorID != c.User.ID {
		return l, fail(404, "Listing not found")
	}
	if l.Kind != "digital" {
		return l, fail(409, "Digital inventory is available only for digital listings.")
	}
	return l, nil
}

func digitalModeAction(c *actionCtx) (actionResult, error) {
	l, err := digitalOwnerListing(c)
	if err != nil {
		return actionResult{}, err
	}
	mode := c.Form.Get("mode")
	if mode != "manual" && mode != "reusable" && mode != "inventory" {
		return actionResult{}, fail(400, "Choose manual fulfillment, distinct inventory or a reusable download.")
	}
	var open int
	if err = c.Tx.QueryRowContext(c.Ctx(), openOrdersSQL, l.ID).Scan(&open); err != nil {
		return actionResult{}, err
	}
	if open != 0 {
		return actionResult{}, fail(409, "Finish or cancel this listing's open orders before changing its digital delivery method.")
	}
	var existing, content string
	if err = c.Tx.QueryRowContext(c.Ctx(), "SELECT digital_delivery_mode,delivery_content FROM products WHERE id=$1", l.ID).Scan(&existing, &content); err != nil {
		return actionResult{}, err
	}
	if mode == "reusable" && strings.TrimSpace(content) == "" {
		return actionResult{}, digitalModeRefusal(c, reusableModeNeedsContent)
	}
	if existing == "inventory" && mode != "inventory" {
		var remaining int
		if err = c.Tx.QueryRowContext(c.Ctx(), "SELECT count(*) FROM digital_units WHERE product_id=$1 AND order_id IS NULL", l.ID).Scan(&remaining); err != nil {
			return actionResult{}, err
		}
		if remaining != 0 {
			return actionResult{}, fail(409, "Clear or remove the remaining distinct inventory before changing delivery method.")
		}
	}
	if _, err = c.Tx.ExecContext(c.Ctx(), `UPDATE products SET digital_delivery_mode=$2,stock=CASE WHEN $2='inventory' THEN (SELECT count(*) FROM digital_units WHERE product_id=$1 AND order_id IS NULL) ELSE stock END,updated=now() WHERE id=$1`, l.ID, mode); err != nil {
		return actionResult{}, err
	}
	// MOD72-6: the product page tells buyers whether a digital listing is delivered by the vendor or automatically, so
	// the vendor's change between the two re-holds an approved listing, or resubmits a rejected one
	// (listing_review.go). Switching between the two automatic methods changes nothing buyers see.
	if l.VendorID == c.User.ID && (existing == "manual") != (mode == "manual") {
		if _, err = holdListing(c.Ctx(), c.Tx, l.ID, l.VendorID); err != nil {
			return actionResult{}, err
		}
	}
	return actionResult{Redirect: "/listing-edit?id=" + l.ID + "#digital-inventory", Audit: "Changed digital delivery method for listing " + shortID(l.ID) + " to " + mode}, nil
}

// digitalModeRefusal answers a refused digital delivery method form with 400 and the listing's edit page again, after
// the action's rollback: msg in its error alert and next to the method field (A-59). When the page cannot be loaded,
// the shared refusal page says msg instead.
func digitalModeRefusal(c *actionCtx, msg string) error {
	return &httpError{Code: http.StatusBadRequest, Msg: msg, Render: func(w http.ResponseWriter) {
		a := c.A
		r := c.R.Clone(c.R.Context())
		r.URL.RawQuery = url.Values{"id": {c.Form.Get("id")}}.Encode()
		d := a.pageData(r, "listing-edit", c.Token, c.User)
		if err := a.load(r, &d); err != nil || d.DigitalInventory == nil {
			a.refuse(w, c.R, c.User, http.StatusBadRequest, msg)
			return
		}
		d.DigitalInventory.ModeError, d.Error = msg, msg
		a.renderStatus(w, c.R, d, http.StatusBadRequest)
	}}
}

func digitalInventoryListing(c *actionCtx) (managedListing, error) {
	l, err := digitalOwnerListing(c)
	if err != nil {
		return l, err
	}
	var mode string
	if err = c.Tx.QueryRowContext(c.Ctx(), "SELECT digital_delivery_mode FROM products WHERE id=$1", l.ID).Scan(&mode); err != nil {
		return l, err
	}
	if mode != "inventory" {
		return l, fail(409, "Choose distinct inventory before adding or removing digital units.")
	}
	return l, nil
}

func digitalUnitImportAction(c *actionCtx) (actionResult, error) {
	l, err := digitalInventoryListing(c)
	if err != nil {
		return actionResult{}, err
	}
	if l.Archived || l.Takedown != "" {
		return actionResult{}, fail(409, "Restore this listing before adding digital inventory.")
	}
	content := strings.ReplaceAll(c.Form.Get("contents"), "\r\n", "\n")
	if strings.Contains(content, "-----BEGIN PGP MESSAGE-----") {
		return actionResult{}, fail(400, encryptedInventoryUnitReason)
	}
	if !utf8.ValidString(content) || utf8.RuneCountInString(content) > maxDeliveryChars {
		return actionResult{}, fail(400, "Import at most 32000 characters of valid text; put one distinct item on each line.")
	}
	var units []string
	seen := map[string]bool{}
	if c.Form.Get("format") == "lines" {
		for _, line := range strings.Split(content, "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				units = append(units, line)
			}
		}
	} else if strings.TrimSpace(content) != "" {
		units = []string{strings.TrimSpace(content)}
	}
	for _, unit := range units {
		if strings.ContainsRune(unit, '\x00') {
			return actionResult{}, fail(400, "Digital units must not contain NUL bytes.")
		}
		hash := digest(unit)
		if seen[hash] {
			return actionResult{}, fail(409, "The import repeats a digital unit. Nothing was added.")
		}
		seen[hash] = true
	}
	if len(units) == 0 || len(units) > maxDigitalImportUnits {
		return actionResult{}, fail(400, "Import between 1 and 1000 distinct items, one per line.")
	}
	if l.Stock+len(units) > 1000000 {
		return actionResult{}, fail(400, "This import would exceed the listing stock limit.")
	}
	for _, unit := range units {
		hash := digest(unit)
		res, err := c.Tx.ExecContext(c.Ctx(), `INSERT INTO digital_units(id,product_id,content,fingerprint) VALUES($1,$2,$3,$4) ON CONFLICT(product_id,fingerprint) DO NOTHING`, randomToken(), l.ID, unit, hash)
		if err != nil {
			return actionResult{}, err
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return actionResult{}, fail(409, "A digital unit is already retained for this listing. Nothing was added.")
		}

	}
	_, err = c.Tx.ExecContext(c.Ctx(), `UPDATE products SET stock=(SELECT count(*) FROM digital_units WHERE product_id=$1 AND order_id IS NULL),private_expired_at=NULL,updated=now() WHERE id=$1`, l.ID)
	return actionResult{Redirect: "/listing-edit?id=" + l.ID + "#digital-inventory", Audit: "Added " + strconv.Itoa(len(units)) + " distinct digital unit(s) to listing " + shortID(l.ID)}, err
}

func digitalUnitRemoveAction(c *actionCtx) (actionResult, error) {
	l, err := digitalInventoryListing(c)
	if err != nil {
		return actionResult{}, err
	}
	res, err := c.Tx.ExecContext(c.Ctx(), `DELETE FROM digital_units WHERE id=$1 AND product_id=$2 AND order_id IS NULL`, c.Form.Get("unit_id"), l.ID)
	if err != nil {
		return actionResult{}, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return actionResult{}, fail(404, "Available digital unit not found")
	}
	_, err = c.Tx.ExecContext(c.Ctx(), `UPDATE products SET stock=(SELECT count(*) FROM digital_units WHERE product_id=$1 AND order_id IS NULL),updated=now() WHERE id=$1`, l.ID)
	return actionResult{Redirect: "/listing-edit?id=" + l.ID + "#digital-inventory", Audit: "Removed one undisclosed digital unit from listing " + shortID(l.ID)}, err
}

func digitalUnitClearAction(c *actionCtx) (actionResult, error) {
	l, err := digitalInventoryListing(c)
	if err != nil {
		return actionResult{}, err
	}
	if c.Form.Get("confirm_clear") != "yes" {
		return actionResult{}, fail(400, "Confirm that you saved any unused digital stock you need before clearing it.")
	}
	if _, err = c.Tx.ExecContext(c.Ctx(), `DELETE FROM digital_units WHERE product_id=$1 AND order_id IS NULL`, l.ID); err != nil {
		return actionResult{}, err
	}
	_, err = c.Tx.ExecContext(c.Ctx(), "UPDATE products SET stock=0,updated=now() WHERE id=$1", l.ID)
	return actionResult{Redirect: "/listing-edit?id=" + l.ID + "#digital-inventory", Audit: "Cleared undisclosed digital stock from listing " + shortID(l.ID)}, err
}

func loadDigitalInventory(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	if d.Product == nil || d.Listing == nil || d.Product.Kind != "digital" {
		return nil
	}
	v := &DigitalInventoryView{Owner: d.Product.VendorID == d.User.ID}
	if err := a.db.QueryRowContext(ctx, `SELECT digital_delivery_mode,(SELECT count(*) FROM digital_units WHERE product_id=$1 AND order_id IS NULL),(SELECT count(*) FROM digital_units WHERE product_id=$1 AND order_id IS NOT NULL AND delivered_at IS NULL) FROM products WHERE id=$1`, d.Product.ID).Scan(&v.Mode, &v.Available, &v.Reserved); err != nil {
		return err
	}
	if v.Owner && v.Mode == "inventory" {
		rows, err := a.db.QueryContext(ctx, `SELECT id,content FROM digital_units WHERE product_id=$1 AND order_id IS NULL ORDER BY created,id LIMIT 50`, d.Product.ID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var unit DigitalInventoryUnit
			if err = rows.Scan(&unit.ID, &unit.Content); err != nil {
				return err
			}
			v.Units = append(v.Units, unit)
		}
		if err = rows.Err(); err != nil {
			return err
		}
	}
	d.DigitalInventory = v
	return nil
}

const encryptedInventoryUnitReason = "Distinct inventory accepts text and links before the buyer is known. Use manual delivery from the order page for content already encrypted to that buyer's PGP key. Nothing was changed."

// appendDigitalUnit checks the final rendered batch size before retaining each unit.
func appendDigitalUnit(b *strings.Builder, count *int, content string, index, units int64) error {
	if strings.Contains(content, "-----BEGIN PGP MESSAGE-----") {
		return fail(409, encryptedInventoryUnitReason)
	}
	if content == "" {
		return fail(409, "Reserved digital content is no longer available; contact the vendor.")
	}
	prefix := ""
	if units > 1 {
		prefix = fmt.Sprintf("Item %d of %d\n", index, units)
	}
	separator := ""
	if index > 1 {
		separator = "\n\n"
	}
	n := *count + len(separator) + len(prefix) + utf8.RuneCountInString(content)
	if n > maxDeliveryChars {
		return fail(409, "This digital batch exceeds the 32000-character delivery limit. Choose a smaller quantity; nothing was charged.")
	}
	b.WriteString(separator)
	b.WriteString(prefix)
	b.WriteString(content)
	*count = n
	return nil
}

func digitalBatch(contents []string) (string, error) {
	var b strings.Builder
	count := 0
	for i, content := range contents {
		if err := appendDigitalUnit(&b, &count, content, int64(i+1), int64(len(contents))); err != nil {
			return "", err
		}
	}
	return b.String(), nil
}

// Read at most one bounded payload beyond the delivery limit, stopping as soon as
// the batch is too large. This also bounds reads during an unlocked availability
// preview racing an inventory import.
func scanDigitalBatch(rows *sql.Rows, units int64) (string, error) {
	defer rows.Close()
	var b strings.Builder
	count := 0
	var n int64
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			return "", err
		}
		n++
		if n > units {
			return "", fail(409, "The reserved digital batch does not match the order quantity.")
		}
		if err := appendDigitalUnit(&b, &count, content, n, units); err != nil {
			return "", err
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if n != units {
		return "", fail(409, "There are not enough distinct digital units left. Choose a smaller quantity; nothing was charged.")
	}
	return b.String(), nil
}

func availableDigitalBatch(ctx context.Context, q rowQuerier, product string, units int64) (string, error) {
	if units < 1 || units > 1000000 {
		return "", fail(400, "Invalid order quantity")
	}
	rows, err := q.QueryContext(ctx, `SELECT content FROM digital_units WHERE product_id=$1 AND order_id IS NULL ORDER BY created,id LIMIT $2`, product, units)
	if err != nil {
		return "", err
	}
	return scanDigitalBatch(rows, units)
}

// reserveDigitalUnits must be called while checkout owns the product row lock.
func reserveDigitalUnits(ctx context.Context, tx *sql.Tx, o *Order) error {
	var mode string
	if err := tx.QueryRowContext(ctx, "SELECT digital_delivery_mode FROM products WHERE id=$1", o.ProductID).Scan(&mode); err != nil {
		return err
	}
	if mode != "inventory" || o.Kind != "digital" {
		return nil
	}
	units := o.Units()
	if _, err := availableDigitalBatch(ctx, tx, o.ProductID, units); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE digital_units SET order_id=$2,reserved_at=now() WHERE id IN(SELECT id FROM digital_units WHERE product_id=$1 AND order_id IS NULL ORDER BY created,id LIMIT $3 FOR UPDATE)`, o.ProductID, o.ID, units)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != units {
		return fail(409, "Digital stock changed. Nothing was charged.")
	}
	return nil
}

// Only undisclosed units can return to stock. Delivered rows remain consumed
// even after their content and fingerprint expire under order retention.
func releaseDigitalUnits(ctx context.Context, tx *sql.Tx, o *Order) error {
	if o.Kind != "digital" {
		return nil
	}
	var mode string
	var privateExpired bool
	// Checkout/cancellation already owns this product lock. Keep the helper's
	// marker read serialized with listing expiry and restore as well.
	if err := tx.QueryRowContext(ctx, "SELECT digital_delivery_mode,private_expired_at IS NOT NULL FROM products WHERE id=$1 FOR UPDATE", o.ProductID).Scan(&mode, &privateExpired); err != nil {
		return err
	}
	if mode != "inventory" {
		return nil
	}
	if !privateExpired {
		if _, err := tx.ExecContext(ctx, `UPDATE digital_units SET order_id=NULL,reserved_at=NULL WHERE order_id=$1 AND delivered_at IS NULL AND content<>''`, o.ID); err != nil {
			return err
		}
	}
	// An expired listing must not acquire private stock again through cancellation.
	// Keep its undisclosed claims assigned and unavailable: the canceled order's
	// own retention deadline and preservation duties still govern those payloads.
	_, err := tx.ExecContext(ctx, `UPDATE products SET stock=(SELECT count(*) FROM digital_units WHERE product_id=$1 AND order_id IS NULL) WHERE id=$1`, o.ProductID)
	return err
}

// releaseDisputedDigitalUnits returns to stock the distinct units of o, a digital order leaving a dispute for to, when
// the dispute ends in a refund (cancelled by the vendor's concession, or resolved for the buyer) and no delivery was
// recorded for the order (A-711). The listing row is locked only when such a unit exists.
func releaseDisputedDigitalUnits(ctx context.Context, tx *sql.Tx, o *Order, to string) error {
	var undelivered bool
	var outcome string
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM digital_units WHERE order_id=$1 AND delivered_at IS NULL)
		AND NOT EXISTS(SELECT 1 FROM deliveries WHERE order_id=$1),COALESCE((SELECT outcome FROM disputes WHERE order_id=$1),'')`, o.ID).Scan(&undelivered, &outcome); err != nil {
		return err
	}
	if !undelivered || to == stateResolved && outcome != "refund" {
		return nil
	}
	// As for a cancellation (stockHook): a checkout of the same listing may hold its row while it waits on the escrow node.
	if err := waitForWalletLocks(ctx, tx); err != nil {
		return err
	}
	return releaseDigitalUnits(ctx, tx, o)
}

func reservedDigitalBatch(ctx context.Context, tx *sql.Tx, o *Order) (string, error) {
	units := o.Units()
	rows, err := tx.QueryContext(ctx, `SELECT content FROM digital_units WHERE order_id=$1 ORDER BY created,id FOR UPDATE`, o.ID)
	if err != nil {
		return "", err
	}
	return scanDigitalBatch(rows, units)
}

// notDeliveredAutomatically keeps a paid order paid when its automatic delivery cannot be made: note goes in its
// history and the vendor is told why (cause) and that they deliver by hand or cancel (A-711).
func notDeliveredAutomatically(ctx context.Context, tx *sql.Tx, o *Order, note, cause string) error {
	if _, err := tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,$2,$2,NULL,$3)", o.ID, statePaid, note); err != nil {
		return err
	}
	_, err := insertNotification(ctx, tx, o.VendorID, "Order "+shortID(o.ID)+" is paid but was not delivered automatically: "+cause+". Deliver it by hand from the order page, or cancel it and refund the buyer.", o.ID)
	return err
}
