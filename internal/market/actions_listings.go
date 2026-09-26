package market

import (
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

func init() {
	registerAction("/listings", actionSpec{Roles: []string{"vendor", "admin"}, Run: listingAction})
}

// maxDeliveryContent matches the manual delivery limit (characters).
const maxDeliveryContent = 32000

// listingInput is a validated listing form shared by create (/listings) and edit (/listings/update).
type listingInput struct {
	Title, Description, Category, Region, Kind, DeliveryContent string
	BTC, XMR                                                    int64
	Stock                                                       int
}

// validateListing checks every listing field. Only physical and digital fulfillment exist: the order
// state machine has no fulfillment step for any other kind. Automatic delivery content is digital-only.
func validateListing(f url.Values) (listingInput, error) {
	btc, e1 := parseAmount(f.Get("price_btc"), 8)
	xmr, e2 := parseAmount(f.Get("price_xmr"), 12)
	stock, e3 := strconv.Atoi(f.Get("stock"))
	in := listingInput{Title: strings.TrimSpace(f.Get("title")), Description: strings.TrimSpace(f.Get("description")), Category: f.Get("category"), Region: f.Get("region"), Kind: f.Get("kind"), DeliveryContent: f.Get("delivery_content"), BTC: btc, XMR: xmr, Stock: stock}
	if e1 != nil || e2 != nil || e3 != nil || stock < 0 || stock > 1000000 || len(in.Title) < 3 || len(in.Title) > 140 || len(in.Description) > 10000 || (in.Kind != "digital" && in.Kind != "physical") || (in.Category != "Hardware" && in.Category != "Digital" && in.Category != "Services") || len(in.Region) < 2 || len(in.Region) > 80 {
		return listingInput{}, fail(400, "Invalid listing. Check price precision, stock, category, and required fields.")
	}
	// A Bitcoin payout below minPayoutBTC is never sent (A-99), so no order may be priced below it.
	if in.BTC < minPayoutBTC {
		return listingInput{}, fail(400, "The Bitcoin price must be at least "+amount(minPayoutBTC, 8)+" BTC: a smaller Bitcoin payout cannot cover the network fee.")
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
	var role string
	if err = c.Tx.QueryRowContext(c.Ctx(), "SELECT role FROM users WHERE id=$1 FOR SHARE", c.User.ID).Scan(&role); err != nil {
		return actionResult{}, err
	}
	if role != "vendor" && role != "admin" {
		return actionResult{}, fail(403, "Vendor access required")
	}
	_, err = c.Tx.ExecContext(c.Ctx(), `INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock,delivery_content) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, randomToken(), c.User.ID, in.Title, in.Description, in.Category, in.Region, in.Kind, in.BTC, in.XMR, in.Stock, in.DeliveryContent)
	return actionResult{Redirect: "/vendor-dashboard?saved=1", Audit: "Created listing"}, err
}
