package market

import (
	"strconv"
	"strings"
)

func init() {
	registerAction("/listings", actionSpec{Roles: []string{"vendor", "admin"}, Run: listingAction})
}

func listingAction(c *actionCtx) (actionResult, error) {
	f := c.Form
	btc, e1 := parseAmount(f.Get("price_btc"), 8)
	xmr, e2 := parseAmount(f.Get("price_xmr"), 12)
	stock, e3 := strconv.Atoi(f.Get("stock"))
	kind := f.Get("kind")
	title := strings.TrimSpace(f.Get("title"))
	desc := strings.TrimSpace(f.Get("description"))
	cat := f.Get("category")
	region := f.Get("region")
	if e1 != nil || e2 != nil || e3 != nil || stock < 0 || stock > 1000000 || len(title) < 3 || len(title) > 140 || len(desc) > 10000 || (kind != "digital" && kind != "physical") || (cat != "Hardware" && cat != "Digital" && cat != "Services") || len(region) < 2 || len(region) > 80 {
		return actionResult{}, fail(400, "Invalid listing. Check price precision, stock, category, and required fields.")
	}
	_, err := c.Tx.ExecContext(c.Ctx(), `INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, randomToken(), c.User.ID, title, desc, cat, region, kind, btc, xmr, stock)
	return actionResult{Redirect: "/vendor-dashboard?saved=1", Audit: "Created listing"}, err
}
