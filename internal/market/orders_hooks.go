package market

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// P3 Orders: side effects that must happen in the same transaction as a state change.

func init() {
	registerTransitionHook(stockHook)
	registerTransitionHook(autoDeliveryHook)
}

// checkoutPriceCtx marks a production checkout that has repriced the order from the listing.
// It is set only by the pooled/BTC escrow/XMR escrow payment-request paths, never by a form field.
type checkoutPriceCtx struct{}

func withCheckoutPrice(ctx context.Context) context.Context {
	return context.WithValue(ctx, checkoutPriceCtx{}, true)
}

// stockHook reserves the ordered quantity when a buyer requests payment and returns those units when a reserved order is
// cancelled. Drafts never hold stock.
func stockHook(ctx context.Context, a *App, tx *sql.Tx, o *Order, from, to string) error {
	switch {
	case from == stateDraft && to == stateAwaitingPayment:
		// Reserving stock locks the listing row. Read its terms from that same UPDATE so an
		// edit racing this checkout is either wholly before or wholly after the snapshot.
		var title, description, kind string
		var btc, xmr int64
		// A listing awaiting staff review (MOD72-6, listing_review.go) takes no payment request, even from a draft saved
		// before an edit sent it back to review.
		err := tx.QueryRowContext(ctx, `UPDATE products SET stock=stock-$2 WHERE id=$1 AND stock>=$2 AND NOT archived AND review_state='approved'
			RETURNING title,description,kind,btc,xmr`, o.ProductID, o.Units()).Scan(&title, &description, &kind, &btc, &xmr)
		if err == sql.ErrNoRows {
			return fail(409, "This listing is out of stock or no longer available")
		}
		if err != nil {
			return err
		}
		if kind != o.Kind {
			return fail(409, "This listing changed while payment was requested. Reload and try again; nothing was changed.")
		}
		if ctx.Value(checkoutPriceCtx{}) != nil {
			dec, price := 8, btc
			if o.Currency == "XMR" {
				dec, price = 12, xmr
			}
			recorded, err := parseAmount(o.Amount, dec)
			if err != nil {
				return err
			}
			total, totalErr := checkedOrderTotal(price, o.Units())
			if totalErr != nil {
				return totalErr
			}
			if recorded != total {
				return fail(409, "This listing price changed while payment was requested. Reload and try again; nothing was changed.")
			}
		}
		res, err := tx.ExecContext(ctx, `UPDATE orders SET listing_title=$2,listing_description=$3,listing_kind=$4
			WHERE id=$1 AND state='awaiting_payment' AND listing_title IS NULL`, o.ID, title, description, kind)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errConflict
		}
		o.Title, o.Description, o.Kind, o.KindDisplay, o.ListingRecorded = title, description, kind, kind, true
		if err := reserveDigitalUnits(ctx, tx, o); err != nil {
			return err
		}
	case to == stateCancelled && (from == stateAwaitingPayment || from == statePaid):
		// A checkout of the same listing may hold its row while it waits on the escrow node (escrowCheckoutAction).
		if err := waitForWalletLocks(ctx, tx); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "UPDATE products SET stock=stock+$2 WHERE id=$1", o.ProductID, o.Units())
		if err != nil {
			return err
		}
		return releaseDigitalUnits(ctx, tx, o)
	case from == stateDisputed && (to == stateCancelled || to == stateResolved) && o.Kind == "digital":
		// A-711: a dispute ending in a refund (the vendor's concession, or a decision for the buyer) returns the distinct
		// units the order reserved and never delivered; delivered ones stay consumed. Physical stock is not returned
		// after a dispute (the goods may have shipped).
		return releaseDisputedDigitalUnits(ctx, tx, o, to)
	}
	return nil
}

// autoDeliveryHook releases a digital listing's stored delivery content as soon as the payment watcher
// marks the order paid, by recording the delivery and moving paid -> delivered as the system actor.
// Content that cannot be delivered automatically (blank or over the limit, or a reserved batch no longer available)
// leaves the order paid for the vendor to deliver by hand; it never blocks the payment transition. Neither is a listing
// an administrator took down (A-163) delivered: the order stays paid, for the vendor to deliver or cancel. Each of these
// writes a history note and tells the vendor the cause (notDeliveredAutomatically, A-711).
// Nor is the listing of a banned vendor (A-652e): the order stays paid, no inventory unit is consumed, and the note
// names the closed account and says an administrator can refund the order (admin_ban_orders.go, A-652b).
func autoDeliveryHook(ctx context.Context, a *App, tx *sql.Tx, o *Order, from, to string) error {
	if to != statePaid || o.Kind != "digital" {
		return nil
	}
	var content, mode, handle string
	var takenDown, banned bool
	// FOR KEY SHARE OF p waits for a takedown in progress (it holds the listing row FOR UPDATE) and then reads its
	// result, without blocking stock reservations on the same listing. The vendor's account row is read without a lock:
	// a ban holds it FOR NO KEY UPDATE, which a KEY SHARE lock would not wait for anyway, and a SHARE lock here, taken
	// while this transaction holds the order, would make the payment wait on every write to the vendor's account row (a
	// checkout of theirs as a buyer holds it during the escrow node call). A ban that commits after this read is
	// concurrent with the payment, which is then delivered as one funded just before the ban, and nothing is released
	// to the banned vendor (A-652c).
	if err := tx.QueryRowContext(ctx, `SELECT p.delivery_content,p.digital_delivery_mode,p.admin_archive_reason IS NOT NULL,u.handle,u.banned_at IS NOT NULL
		FROM products p JOIN users u ON u.id=p.vendor_id WHERE p.id=$1 FOR KEY SHARE OF p`, o.ProductID).Scan(&content, &mode, &takenDown, &handle, &banned); err != nil {
		return err
	}
	if mode == "manual" {
		return nil
	}
	if banned {
		note := "Not delivered automatically: the vendor's account " + handle + " is closed. An administrator can refund this order to the buyer's refund address under Orders of this banned account, on that account's page."
		_, err := tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,$2,$2,NULL,$3)", o.ID, statePaid, note)
		return err
	}
	if takenDown {
		note := "Not delivered automatically: an administrator took this listing down. The vendor can deliver by hand or cancel and refund."
		switch {
		case o.Escrow && o.Currency == "XMR": // a Monero escrow's refund is built and sent by the market alone (P3-R8)
			note = "Not delivered automatically: an administrator took this listing down. The vendor can deliver by hand, or cancel: the escrow is then refunded to the buyer's refund address by a transaction the market builds and sends."
		case o.Escrow: // cancelling a funded escrow order requests its refund, a settlement (escrowSettleTerminal)
			note = "Not delivered automatically: an administrator took this listing down. The vendor can deliver by hand, or cancel: the escrow is then refunded to the buyer by a settlement the buyer or the vendor signs."
		}
		return notDeliveredAutomatically(ctx, tx, o, note, "an administrator took this listing down")
	}
	if mode == "reusable" && strings.TrimSpace(content) == "" {
		return notDeliveredAutomatically(ctx, tx, o, "Not delivered automatically: the listing's reusable download content is empty. The vendor must deliver by hand or cancel and refund this order.", "the listing's reusable download content is empty")
	}
	if mode == "reusable" && utf8.RuneCountInString(content) > maxDeliveryChars {
		return notDeliveredAutomatically(ctx, tx, o, "Not delivered automatically: the listing's reusable download content is over the 32000-character delivery limit. The vendor must deliver by hand or cancel and refund this order.", "the listing's reusable download content is over the 32000-character delivery limit")
	}
	if mode == "inventory" {
		var err error
		content, err = reservedDigitalBatch(ctx, tx, o)
		if err != nil {
			// Invalid or expired stock must not prevent recording an observed payment.
			if _, ok := err.(*httpError); !ok {
				return err
			}
			return notDeliveredAutomatically(ctx, tx, o, "Automatic digital batch unavailable: its reserved distinct digital units are no longer available. The vendor must deliver or cancel this order.", "its reserved distinct digital units are no longer available")
		}
	}

	if err := insertDelivery(ctx, tx, o, content); err != nil {
		if !errors.Is(err, errDeliveryBuyerKey) && !errors.Is(err, errDeliveryCiphertext) && !errors.Is(err, errDeliveryEncryption) {
			return err
		}
		// The buyer may have removed their key after checkout, or a listing may contain ciphertext for a
		// different recipient. Count the confirmed payment and keep the order paid for a safe manual delivery.
		note := "Not delivered automatically: digital content could not be encrypted to the buyer's current PGP key. The buyer must save a usable encryption key; the vendor can then deliver content from the order page or cancel and refund."
		if _, err = tx.ExecContext(ctx, "INSERT INTO order_events(order_id,from_state,to_state,actor_id,note) VALUES($1,$2,$2,NULL,$3)", o.ID, statePaid, note); err != nil {
			return err
		}
		for _, uid := range []string{o.BuyerID, o.VendorID} {
			if _, err = insertNotification(ctx, tx, uid, "Order "+shortID(o.ID)+": digital delivery is waiting for a usable buyer PGP key and content encrypted to that key. After the buyer updates their key, the vendor can deliver from the order page.", o.ID); err != nil {
				return err
			}
		}
		return nil
	}
	next, err := a.transition(ctx, tx, o.ID, statePaid, stateDelivered, nil, "Automatic delivery after confirmed payment")
	if err != nil {
		return err
	}
	o.State, o.Status = next.State, next.Status
	return nil
}

var (
	errDeliveryBuyerKey   = fail(409, "Digital content was not delivered: the buyer must save a usable PGP encryption key on their account. Nothing was changed.")
	errDeliveryCiphertext = fail(400, "Digital content was not delivered: the armored PGP message must be encrypted to the buyer's current public key with a visible recipient key ID. Nothing was changed.")
	errDeliveryEncryption = fail(409, "Digital content could not be encrypted to the buyer's PGP key. Ask the buyer to update their encryption key and try again. Nothing was changed.")
)

// deliveryCiphertext binds both original deliveries and replacements to the buyer's current key.
// Existing valid recipient ciphertext is canonicalized without encrypting its packets again.
func deliveryCiphertext(ctx context.Context, tx *sql.Tx, o *Order, content string) (string, string, error) {
	if strings.TrimSpace(content) == "" || utf8.RuneCountInString(content) > maxDeliveryChars {
		return "", "", fail(400, "Delivery content must contain 1–32000 characters")
	}
	var one int
	// The order is locked before this account. NOWAIT avoids a cycle with an account action that already holds
	// the buyer's row, while keeping its key stable through encryption, the delivery and notification inserts.
	if err := tx.QueryRowContext(ctx, "SELECT 1 FROM users WHERE id=$1 FOR SHARE NOWAIT", o.BuyerID).Scan(&one); err != nil {
		return "", "", err
	}
	p, err := loadPGPAccount(ctx, tx, o.BuyerID, false)
	if err != nil {
		return "", "", err
	}
	if !canReceiveEncrypted(p.Key, time.Now()) {
		return "", "", errDeliveryBuyerKey
	}
	var ciphertext string
	if strings.HasPrefix(strings.TrimSpace(content), "-----BEGIN PGP MESSAGE-----") {
		ciphertext, err = canonicalMessage(content)
		if err != nil {
			return "", "", errDeliveryCiphertext
		}
		ids, encrypted, err := inspectEncrypted(ciphertext)
		selected, usable := p.Key.EncryptionKey(time.Now())
		if err != nil || !encrypted || !usable || !slices.Contains(ids, selected.PublicKey.KeyId) || slices.Contains(ids, uint64(0)) {
			return "", "", errDeliveryCiphertext
		}
	} else {
		ciphertext, err = encryptTo(p.Key, content)
		if err != nil {
			return "", "", errDeliveryEncryption
		}
	}
	return ciphertext, p.Fingerprint, nil
}

// insertDelivery stores recipient-specific ciphertext and records a buyer-only linked notification.
func insertDelivery(ctx context.Context, tx *sql.Tx, o *Order, content string) error {
	ciphertext, fingerprint, err := deliveryCiphertext(ctx, tx, o, content)
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO deliveries(order_id,content,encrypted,recipient_fingerprint)
		VALUES($1,$2,true,$3) ON CONFLICT (order_id) DO NOTHING`, o.ID, ciphertext, fingerprint)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fail(409, "Content was already delivered for this order")
	}
	if _, err = tx.ExecContext(ctx, "UPDATE digital_units SET delivered_at=now() WHERE order_id=$1 AND delivered_at IS NULL", o.ID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO notifications(id,user_id,body,order_id,delivery_order_id)
		VALUES($1,$2,$3,$4,$4)`, randomToken(), o.BuyerID, "Order "+shortID(o.ID)+": encrypted digital delivery is available. Decrypt it with your PGP private key.", o.ID)
	return err
}
