package market

import (
	"context"
	"net/http"
	"slices"
	"strings"
)

// P4 Inventory: the listing-edit page and vendor-dashboard inventory counts. Public product queries in
// load.go exclude archived listings; the vendor dashboard lists the owner's archived listings with a badge.

func init() {
	registerPage("listing-edit", pageSpec{Protected: true, Roles: []string{"vendor", "admin"}})
	registerLoader("listing-edit", loadListingEdit)
	registerLoader("vendor-dashboard", loadInventory)
	registerPreview("listing-edit", func(d *PageData) {
		d.Title = "Edit listing"
		d.Listing = &ListingView{Updated: "Sample listing", OpenOrders: 1}
	})
	registerPreview("vendor-dashboard", func(d *PageData) {
		if len(d.Products) > 0 {
			d.Products[len(d.Products)-1].Archived = true
		}
		d.Inventory = inventoryCounts(d.Products, "")
	})
}

// autoDeliveryCurrencies lists the currencies whose payments can be confirmed by a configured provider;
// automatic delivery only happens when a payment is confirmed.
func (a *App) autoDeliveryCurrencies() string {
	var list []string
	for currency, p := range a.providers() {
		if p != nil {
			list = append(list, currency)
		}
	}
	slices.Sort(list)
	return strings.Join(list, ", ")
}

func inventoryCounts(products []Product, auto string) *InventoryView {
	v := &InventoryView{AutoDelivery: auto}
	for _, p := range products {
		if p.Archived {
			v.Archived++
		} else {
			v.Active++
		}
	}
	return v
}

func loadInventory(_ context.Context, a *App, _ *http.Request, d *PageData) error {
	d.Inventory = inventoryCounts(d.Products, a.autoDeliveryCurrencies())
	return nil
}

// loadListingEdit loads one listing for its vendor or an administrator; anyone else gets 404.
func loadListingEdit(ctx context.Context, a *App, r *http.Request, d *PageData) error {
	var p Product
	var btc, xmr int64
	v := &ListingView{AutoDelivery: a.autoDeliveryCurrencies()}
	err := a.db.QueryRowContext(ctx, `SELECT p.id,p.title,p.description,p.category,p.region,p.kind,u.handle,p.vendor_id,p.btc,p.xmr,p.stock,p.archived,p.delivery_content,
 to_char(p.updated,'YYYY-MM-DD HH24:MI'),coalesce(to_char(p.archived_at,'YYYY-MM-DD HH24:MI'),''),
 (`+openOrdersSQL+`)
 FROM products p JOIN users u ON u.id=p.vendor_id WHERE p.id=$1 AND (p.vendor_id=$2 OR $3='admin')`,
		r.URL.Query().Get("id"), d.User.ID, d.User.Role).Scan(&p.ID, &p.Title, &p.Description, &p.Category, &p.Region, &p.Kind, &p.Vendor, &p.VendorID, &btc, &xmr, &p.Stock, &p.Archived, &v.DeliveryContent, &v.Updated, &v.ArchivedAt, &v.OpenOrders)
	if err != nil {
		return err // sql.ErrNoRows -> 404
	}
	p.PriceBTC, p.PriceXMR = amount(btc, 8), amount(xmr, 12)
	d.Product, d.Listing, d.Title = &p, v, "Edit listing"
	return nil
}
