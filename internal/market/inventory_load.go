package market

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// P4 Inventory: the listing-edit page and vendor-dashboard inventory counts. Public product queries in
// load.go exclude archived listings; the vendor dashboard lists the owner's archived listings with a badge.

const inventoryPageSize = 50

func init() {
	registerPage("listing-edit", pageSpec{Protected: true, Roles: []string{"vendor", "admin"}})
	registerLoader("listing-edit", loadListingEdit)
	registerLoader("listing-edit", reviewModerationLoader) // takedown and restore forms (A-163)
	registerLoader("vendor-dashboard", loadInventory)
	registerPreview("listing-edit", func(d *PageData) {
		d.Title = "Edit listing"
		d.Listing = &ListingView{Updated: "Sample listing", OpenOrders: 1, VendorEditor: true, DispatchDays: defaultDispatchDays}
	})
	registerPreview("vendor-dashboard", func(d *PageData) {
		if len(d.Products) > 0 {
			d.Products[len(d.Products)-1].Archived = true
		}
		d.Inventory = inventoryCounts(d.Products, "")
		setInventoryURLs(d)
	})
}

// autoDeliveryCurrencies lists the currencies whose payments can be confirmed by a configured provider;
// automatic delivery only happens when a payment is confirmed.
func (a *App) autoDeliveryCurrencies() string {
	var list []string
	if a.escrowConfigured {
		list = append(list, "BTC")
	}
	if a.escrowXMR != nil {
		list = append(list, "XMR")
	}
	return strings.Join(list, ", ")
}

func inventoryCounts(products []Product, auto string) *InventoryView {
	v := &InventoryView{AutoDelivery: auto, Filter: "all"}
	for _, p := range products {
		if p.Archived {
			v.Archived++
		} else {
			v.Active++
		}
	}
	v.Total = len(products)
	return v
}

func inventoryFilter(raw string) string {
	switch raw {
	case "active", "archived":
		return raw
	default:
		return "all"
	}
}

func inventoryPaging(filter, currency string, total int, rawPage string) InventoryPaging {
	p := InventoryPaging{Page: 1, Total: total, Pages: max(1, (total+inventoryPageSize-1)/inventoryPageSize)}
	if n, err := strconv.Atoi(rawPage); err == nil && n > 1 {
		p.Page = min(n, p.Pages)
	} else if errors.Is(err, strconv.ErrRange) && !strings.HasPrefix(rawPage, "-") {
		p.Page = p.Pages
	}
	if total > 0 {
		p.First, p.Last = p.Offset()+1, min(p.Offset()+inventoryPageSize, total)
	}
	link := func(page int) string {
		q := url.Values{"inventory": {filter}}
		if currency == "XMR" {
			q.Set("currency", "XMR")
		}
		if page > 1 {
			q.Set("page", strconv.Itoa(page))
		}
		return "/vendor-dashboard?" + q.Encode()
	}
	if p.Page > 1 {
		p.PrevURL = link(p.Page - 1)
	}
	if p.Page < p.Pages {
		p.NextURL = link(p.Page + 1)
	}
	return p
}

func inventoryFilterURL(filter, currency string) string {
	q := url.Values{"inventory": {filter}}
	if currency == "XMR" {
		q.Set("currency", "XMR")
	}
	return "/vendor-dashboard?" + q.Encode()
}

func loadInventory(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	if d.Inventory == nil {
		d.Inventory = inventoryCounts(d.Products, "")
	}
	d.Inventory.AutoDelivery = a.autoDeliveryCurrencies()
	ids := make([]string, 0, len(d.Products))
	for _, p := range d.Products {
		ids = append(ids, p.ID)
	}
	reservations, err := listingReservations(ctx, a, ids)
	if err != nil {
		return err
	}
	d.Inventory.Reservations = reservations
	setInventoryURLs(d)
	return nil
}

func listingReservations(ctx context.Context, a *App, productIDs []string) (map[string]ListingReservation, error) {
	result := make(map[string]ListingReservation, len(productIDs))
	if len(productIDs) == 0 {
		return result, nil
	}
	rows, err := a.db.QueryContext(ctx, `SELECT o.product_id,
		count(*) FILTER (WHERE o.state NOT IN ('draft','completed','resolved','cancelled')),
		COALESCE(to_char(min(CASE WHEN o.state='awaiting_payment' AND es.order_id IS NOT NULL THEN
			GREATEST(es.created,COALESCE((SELECT max(pm.created) FROM payments pm WHERE pm.order_id=o.id
				AND pm.confirmations>=0 AND NOT pm.locked AND (es.coin='XMR' OR NOT pm.escrow_left_out)),es.created))
				+ make_interval(secs => $2) END),'YYYY-MM-DD HH24:MI "UTC"'),'')
		FROM orders o LEFT JOIN escrows es ON es.order_id=o.id
		WHERE o.product_id=ANY($1) GROUP BY o.product_id`, productIDs, expiryWindow().Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, expiry string
		var reservation ListingReservation
		if err := rows.Scan(&id, &reservation.Units, &expiry); err != nil {
			return nil, err
		}
		reservation.PaymentExpiry = expiry
		result[id] = reservation
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func setInventoryURLs(d *PageData) {
	d.Inventory.AllURL = inventoryFilterURL("all", d.Currency)
	d.Inventory.ActiveURL = inventoryFilterURL("active", d.Currency)
	d.Inventory.ArchivedURL = inventoryFilterURL("archived", d.Currency)
}

// loadListingEdit loads one listing for its vendor or an administrator; anyone else gets 404.
// Automatic delivery content (what every buyer receives) is loaded only for the listing's vendor; an
// administrator editing another vendor's listing learns only whether it is set.
func loadListingEdit(ctx context.Context, a *App, r *http.Request, d *PageData) error {
	var p Product
	var btc, xmr int64
	v := &ListingView{AutoDelivery: a.autoDeliveryCurrencies()}
	err := a.db.QueryRowContext(ctx, `SELECT p.id,p.title,p.description,p.category,p.region,p.kind,u.handle,p.vendor_id,p.btc,p.xmr,p.stock,p.archived,
 CASE WHEN p.vendor_id=$2 THEN p.delivery_content ELSE '' END,p.delivery_content<>'',
 to_char(p.updated,'YYYY-MM-DD HH24:MI "UTC"'),coalesce(to_char(p.archived_at,'YYYY-MM-DD HH24:MI "UTC"'),''),
 (`+openOrdersSQL+`),coalesce(p.admin_archive_reason,''),coalesce(p.dispatch_days,$4)
 FROM products p JOIN users u ON u.id=p.vendor_id WHERE p.id=$1 AND (p.vendor_id=$2 OR $3='admin')`,
		r.URL.Query().Get("id"), d.User.ID, d.User.Role, defaultDispatchDays).Scan(&p.ID, &p.Title, &p.Description, &p.Category, &p.Region, &p.Kind, &p.Vendor, &p.VendorID, &btc, &xmr, &p.Stock, &p.Archived, &v.DeliveryContent, &v.HasDeliveryContent, &v.Updated, &v.ArchivedAt, &v.OpenOrders, &v.TakedownReason, &v.DispatchDays)
	if err != nil {
		return err // sql.ErrNoRows -> 404
	}
	v.VendorEditor = p.VendorID == d.User.ID
	reservations, err := listingReservations(ctx, a, []string{p.ID})
	if err != nil {
		return err
	}
	v.Reservation = reservations[p.ID]
	p.PriceBTC, p.PriceXMR = amount(btc, 8), amount(xmr, 12)
	d.Product, d.Listing, d.Title = &p, v, "Edit listing"
	return nil
}
