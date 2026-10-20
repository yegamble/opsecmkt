package market

import (
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

func init() {
	registerAction("/listings", actionSpec{Roles: []string{"vendor", "admin"}, FormNonce: true, Run: listingAction})
}

// maxDeliveryContent matches the manual delivery limit (characters).
const maxDeliveryContent = 32000

// listingInput is a validated listing form shared by create (/listings) and edit (/listings/update).
type listingInput struct {
	Title, Description, Category, Region, Kind, DeliveryContent string
	Subcategory                                                 string
	ShippingRegions                                             []string
	BTC, XMR                                                    int64
	Stock                                                       int
	// DispatchDays is the physical listing's dispatch window in days (1 to 30; WR-escrow P2-R5); 0 = the form sent
	// none (a digital listing, or an omitted field), which create stores as defaultDispatchDays and update keeps.
	DispatchDays int
}

// defaultDispatchDays is a physical listing's dispatch window when its vendor gives none; maxDispatchDays is the
// longest window (migration 079's CHECK).
const (
	defaultDispatchDays = 3
	maxDispatchDays     = 30
)

// validateListing checks every listing field. Only physical and digital fulfillment exist: the order
// state machine has no fulfillment step for any other kind. Automatic delivery content is digital-only.
func validateListing(f url.Values) (listingInput, error) {
	xmr, e2 := parseAmount(f.Get("price_xmr"), 12)
	stock, e3 := strconv.Atoi(f.Get("stock"))
	in := listingInput{Title: normalizedFormText(f.Get("title")), Description: normalizedFormText(f.Get("description")), Category: f.Get("category"), Region: f.Get("region"), Kind: f.Get("kind"), DeliveryContent: f.Get("delivery_content"), XMR: xmr, Stock: stock}
	in.Subcategory = f.Get("subcategory")
	for _, r := range f["shipping_regions"] {
		if !slices.Contains(shippingRegions, r) {
			return listingInput{}, fail(400, "Additional shipping regions: choose a listed region.")
		}
		if !slices.Contains(in.ShippingRegions, r) {
			in.ShippingRegions = append(in.ShippingRegions, r)
		}
	}
	if !validSubcategory(in.Category, in.Subcategory) {
		return listingInput{}, fail(400, "Subcategory: choose an option within the selected category, or leave it unspecified.")
	}
	// A-514: each field in error is named, by its form label, in form order.
	var problems []string
	for _, p := range []struct {
		bad bool
		msg string
	}{
		{!textLength(in.Title, 3, 140), "Listing title: use 3 to 140 characters."},
		{!textLength(in.Description, 0, 10000), "Description: use at most 10000 characters."},
		{in.Category != "Hardware" && in.Category != "Digital" && in.Category != "Services", "Category: choose Hardware, Digital or Services."},
		{!textLength(in.Region, 2, 80), "Region: use 2 to 80 characters."},
		{in.Kind != "digital" && in.Kind != "physical", "Fulfillment type: choose physical or digital."},
		{e3 != nil || stock < 0 || stock > 1000000, "Stock: use a whole number from 0 to 1000000."},
		{e2 != nil, "Monero price: use a positive amount with at most 12 decimal places."},
	} {
		if p.bad {
			problems = append(problems, p.msg)
		}
	}
	if len(problems) > 0 {
		return listingInput{}, fail(400, strings.Join(problems, " "))
	}
	// A payout below its currency's floor is never sent (A-99, A-232), so no order may be priced below it.
	if in.XMR < minPayoutXMR {
		return listingInput{}, fail(400, "The Monero price must be at least "+amount(minPayoutXMR, 12)+" XMR: the network fee is deducted from every payout and would take too large a share of a smaller one.")
	}
	if in.Kind == "physical" {
		// Digital listings ignore the field, whatever it holds.
		if v := strings.TrimSpace(f.Get("dispatch_days")); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > maxDispatchDays {
				return listingInput{}, fail(400, "Dispatch within must be a whole number of days from 1 to "+strconv.Itoa(maxDispatchDays)+".")
			}
			in.DispatchDays = n
		}
	}
	if strings.TrimSpace(in.DeliveryContent) == "" {
		in.DeliveryContent = ""
	}
	if in.DeliveryContent != "" && in.Kind != "digital" {
		return listingInput{}, fail(400, "Automatic delivery content is only available for digital listings.")
	}
	if !utf8.ValidString(in.DeliveryContent) || utf8.RuneCountInString(in.DeliveryContent) > maxDeliveryContent {
		return listingInput{}, fail(400, "Automatic delivery content must be valid text of at most 32000 characters.")
	}
	return in, nil
}

func listingAction(c *actionCtx) (actionResult, error) {
	in, err := validateListing(c.Form)
	if err != nil {
		return actionResult{}, err
	}
	// The request's role snapshot can predate a concurrent demotion. Keep this
	// share lock through creation so demotion either wins first or archives the
	// newly committed listing in its subsequent archive statement.
	var current User
	var active bool
	if err = c.Tx.QueryRowContext(c.Ctx(), "SELECT role,vendor_approved,suspended_at IS NULL AND closed_at IS NULL AND retention_restricted_at IS NULL FROM users WHERE id=$1 FOR SHARE", c.User.ID).Scan(&current.Role, &current.VendorApproved, &active); err != nil {
		return actionResult{}, err
	}
	if !current.CanSell() || !active {
		return actionResult{}, fail(403, "Vendor access required")
	}
	mode := c.Form.Get("digital_delivery_mode")
	if mode == "" {
		mode = "manual"
	}
	if mode != "manual" && mode != "inventory" && mode != "reusable" {
		return actionResult{}, fail(400, "Choose a digital delivery method.")
	}
	if in.Kind != "digital" {
		mode = "manual"
	}
	if in.Kind == "digital" && mode == "inventory" {
		in.Stock = 0
	}
	id := randomToken()
	if in.Kind == "physical" && in.DispatchDays == 0 {
		in.DispatchDays = defaultDispatchDays
	}
	_, err = c.Tx.ExecContext(c.Ctx(), `INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock,delivery_content,dispatch_days,subcategory,shipping_regions,digital_delivery_mode) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,NULLIF($12,0),$13,coalesce($14::text[],'{}'),$15)`, id, c.User.ID, in.Title, in.Description, in.Category, in.Region, in.Kind, in.BTC, in.XMR, in.Stock, in.DeliveryContent, in.DispatchDays, in.Subcategory, in.ShippingRegions, mode)
	// A-188: a new listing lands on its edit page's Photos panel, where its photos are added.
	return actionResult{Redirect: "/listing-edit?id=" + id, Notice: "listing-created", Audit: "Created listing"}, err
}
