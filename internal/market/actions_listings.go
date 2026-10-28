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

// A-711: a reusable download without content would be published as delivered automatically, while checkout refuses
// it (escrowEligible). The create and edit forms and the digital delivery method form refuse it with these.
const (
	reusableContentRequired     = "Reusable download content: enter the content every buyer receives, or choose another digital delivery method."
	reusableContentRequiredEdit = "Reusable download content: enter the content every buyer receives. To stop sending it, choose another digital delivery method under Digital unit inventory first."
	reusableModeNeedsContent    = "Digital delivery method: save the reusable download content in the listing form first, then choose a reusable download. Nothing was changed."
)

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
// A refused form's error is listingProblems: one message per condition, naming its field by its form label, every
// field in error in form order (A-514, A-59).
func validateListing(f url.Values) (listingInput, error) {
	xmr, e2 := parseAmount(f.Get("price_xmr"), 12)
	stock, e3 := strconv.Atoi(f.Get("stock"))
	in := listingInput{Title: normalizedFormText(f.Get("title")), Description: normalizedFormText(f.Get("description")), Category: f.Get("category"), Region: f.Get("region"), Kind: f.Get("kind"), DeliveryContent: f.Get("delivery_content"), XMR: xmr, Stock: stock}
	in.Subcategory = f.Get("subcategory")
	var problems listingProblems
	add := func(field, msg string) { problems = append(problems, listingProblem{field, msg}) }
	if msg := textProblem("Listing title", in.Title, 3, 140); msg != "" {
		add("title", msg)
	}
	if msg := textProblem("Description", in.Description, 0, 10000); msg != "" {
		add("description", msg)
	}
	categoryOK := in.Category == "Hardware" || in.Category == "Digital" || in.Category == "Services"
	if !categoryOK {
		add("category", "Category: choose Hardware, Digital or Services.")
	}
	if categoryOK && !validSubcategory(in.Category, in.Subcategory) {
		add("subcategory", "Subcategory: choose an option within the selected category, or leave it unspecified.")
	}
	if in.Region == "" {
		add("region", "Region: choose a region.")
	} else if msg := textProblem("Region", in.Region, 2, 80); msg != "" {
		add("region", msg)
	}
	if in.Kind != "digital" && in.Kind != "physical" {
		add("kind", "Fulfillment type: choose physical or digital.")
	}
	switch {
	case e3 != nil:
		add("stock", "Stock: enter a whole number, such as 1.")
	case stock < 0 || stock > 1000000:
		add("stock", "Stock: use a whole number from 0 to 1000000.")
	}
	if in.Kind == "physical" {
		// Digital listings ignore the field, whatever it holds.
		if v := strings.TrimSpace(f.Get("dispatch_days")); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > maxDispatchDays {
				add("dispatch_days", "Dispatch within: use a whole number of days from 1 to "+strconv.Itoa(maxDispatchDays)+".")
			}
			in.DispatchDays = n
		}
	}
	switch {
	case e2 != nil:
		add("price_xmr", "Monero price: use a positive amount with at most 12 decimal places.")
	case in.XMR < minPayoutXMR:
		// A payout below its currency's floor is never sent (A-99, A-232), so no order may be priced below it.
		add("price_xmr", "Monero price: use at least "+amount(minPayoutXMR, 12)+" XMR. The network fee is deducted from every payout and would take too large a share of a smaller one.")
	}
	for _, r := range f["shipping_regions"] {
		if !slices.Contains(shippingRegions, r) {
			add("shipping_regions", "Additional shipping regions: choose only listed regions.")
			break
		}
		if !slices.Contains(in.ShippingRegions, r) {
			in.ShippingRegions = append(in.ShippingRegions, r)
		}
	}
	if strings.TrimSpace(in.DeliveryContent) == "" {
		in.DeliveryContent = ""
	}
	switch {
	case !utf8.ValidString(in.DeliveryContent) || strings.ContainsRune(in.DeliveryContent, 0):
		add("delivery_content", "Reusable download content: remove the characters that are not valid text.")
	case utf8.RuneCountInString(in.DeliveryContent) > maxDeliveryContent:
		add("delivery_content", "Reusable download content: use at most "+strconv.Itoa(maxDeliveryContent)+" characters.")
	case in.DeliveryContent != "" && in.Kind == "physical":
		add("delivery_content", "Reusable download content: only a digital listing can have it. Clear it, or choose Digital as the fulfillment type.")
	case in.DeliveryContent == "" && in.Kind == "digital" && f.Get("digital_delivery_mode") == "reusable":
		// The create form chooses the method; the edit form leaves it to the method form (listingUpdateAction).
		add("delivery_content", reusableContentRequired)
	}
	if len(problems) > 0 {
		return listingInput{}, problems
	}
	return in, nil
}

// textProblem is label's message when s is not valid text of minChars to maxChars characters (textLength), "" when
// it is.
func textProblem(label, s string, minChars, maxChars int) string {
	n := utf8.RuneCountInString(s)
	switch {
	case !utf8.ValidString(s) || strings.ContainsRune(s, 0):
		return label + ": remove the characters that are not valid text."
	case n == 0 && minChars > 0:
		return label + ": enter at least " + strconv.Itoa(minChars) + " characters."
	case n < minChars:
		return label + ": use at least " + strconv.Itoa(minChars) + " characters."
	case n > maxChars:
		return label + ": use at most " + strconv.Itoa(maxChars) + " characters."
	}
	return ""
}

func listingAction(c *actionCtx) (actionResult, error) {
	in, err := validateListing(c.Form)
	if err != nil {
		return actionResult{}, listingFormRefusal(c, err)
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
		return actionResult{}, listingFormRefusal(c, listingProblems{{"digital_delivery_mode", "Digital delivery method: choose one of the listed methods."}})
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
	// MOD72-6: in hold mode a vendor's new listing waits for staff review (listing_review.go).
	held, err := listingHeld(c.Ctx(), c.Tx, c.User.ID)
	if err != nil {
		return actionResult{}, err
	}
	state := "approved"
	notice := "listing-created"
	if held {
		state = "pending"
		notice = "listing-created-held"
	}
	_, err = c.Tx.ExecContext(c.Ctx(), `INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock,delivery_content,dispatch_days,subcategory,shipping_regions,digital_delivery_mode,review_state) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,NULLIF($12,0),$13,coalesce($14::text[],'{}'),$15,$16)`, id, c.User.ID, in.Title, in.Description, in.Category, in.Region, in.Kind, in.BTC, in.XMR, in.Stock, in.DeliveryContent, in.DispatchDays, in.Subcategory, in.ShippingRegions, mode, state)
	// A-188: a new listing lands on its edit page's Photos panel, where its photos are added.
	return actionResult{Redirect: "/listing-edit?id=" + id, Notice: notice, Audit: "Created listing"}, err
}
