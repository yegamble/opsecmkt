package market

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// A-59 (PA-6): a refused listing create or edit is the same form again, filled in with what was sent and each problem
// next to its field, so a browser without JavaScript (Tor Browser's Firefox restores an empty form on Back) loses
// nothing the vendor typed. The reusable download content is never filled in again: it is a delivery secret.

// listingProblem is one field's problem; field is the form field's name.
type listingProblem struct{ field, msg string }

// listingProblems is every problem of a refused listing form, in form order. Its Error is the messages joined, which
// the shared refusal page shows when the form itself cannot be loaded again.
type listingProblems []listingProblem

func (p listingProblems) Error() string {
	msgs := make([]string, len(p))
	for i, x := range p {
		msgs[i] = x.msg
	}
	return strings.Join(msgs, " ")
}

// ListingFormView is what the listing create (vendor desk) and edit forms show: the stored listing, the create form's
// defaults, or a refused post as it was sent (ListingFields). Every value is text as typed; html/template escapes it.
type ListingFormView struct {
	Title, Description, Category, Subcategory, Region, Kind string
	Stock, DispatchDays, PriceXMR, DeliveryMode, StockSeen  string
	ShippingRegions                                         []string
	Errors                                                  map[string]string // field name → its problem
	// DeliveryNote says that the reusable download content sent was not filled in again; "" when none was changed.
	DeliveryNote string
}

// Err is field's problem, "" when it has none.
func (f ListingFormView) Err(field string) string { return f.Errors[field] }

// Ship reports whether region is one of the additional shipping regions.
func (f ListingFormView) Ship(region string) bool { return slices.Contains(f.ShippingRegions, region) }

// KnownRegion reports whether Region is an option of the form's Region list.
func (f ListingFormView) KnownRegion() bool {
	return f.Region == "Digital" || slices.Contains(shippingRegions, f.Region)
}

// ListingFields is the listing form's content on this page: a refused post's (ListingForm), else the listing being
// edited, else the create form's defaults.
func (d PageData) ListingFields() ListingFormView {
	if d.ListingForm != nil {
		return *d.ListingForm
	}
	if p := d.Product; p != nil && d.Page == "listing-edit" {
		f := ListingFormView{Title: p.Title, Description: p.Description, Category: p.Category, Subcategory: p.Subcategory,
			Region: p.Region, Kind: p.Kind, Stock: strconv.Itoa(p.Stock), StockSeen: strconv.Itoa(p.Stock), PriceXMR: p.PriceXMR,
			ShippingRegions: p.ShippingRegions}
		if d.Listing != nil {
			f.DispatchDays = strconv.Itoa(d.Listing.DispatchDays)
		}
		return f
	}
	return ListingFormView{Category: "Hardware", Kind: "physical", Stock: "1", DispatchDays: strconv.Itoa(defaultDispatchDays), DeliveryMode: "manual"}
}

// typedText is a sent form value to show again: valid UTF-8 without NUL characters, at most limit characters (so a
// hostile post cannot make the page huge; the form's own limits are far lower).
func typedText(v string, limit int) string {
	v = strings.ReplaceAll(strings.ToValidUTF8(v, "�"), "\x00", "")
	if utf8.RuneCountInString(v) > limit {
		v = string([]rune(v)[:limit])
	}
	return v
}

// listingFormFromPost is the listing form as f sent it, with problems, never its reusable download content. Text
// over a field's limit is kept up to twice the limit, so the vendor can shorten it rather than lose its end.
func listingFormFromPost(f url.Values, problems listingProblems) *ListingFormView {
	v := &ListingFormView{
		Title: typedText(f.Get("title"), 280), Description: typedText(f.Get("description"), 20000),
		Category: typedText(f.Get("category"), 80), Subcategory: typedText(f.Get("subcategory"), 80),
		Region: typedText(f.Get("region"), 160), Kind: typedText(f.Get("kind"), 16), Stock: typedText(f.Get("stock"), 20),
		DispatchDays: typedText(f.Get("dispatch_days"), 20), PriceXMR: typedText(f.Get("price_xmr"), 40),
		DeliveryMode: typedText(f.Get("digital_delivery_mode"), 16), StockSeen: typedText(f.Get("stock_seen"), 20),
		Errors: map[string]string{},
	}
	for _, r := range f["shipping_regions"] {
		if slices.Contains(shippingRegions, r) && !slices.Contains(v.ShippingRegions, r) {
			v.ShippingRegions = append(v.ShippingRegions, r)
		}
	}
	for _, p := range problems {
		if v.Errors[p.field] == "" {
			v.Errors[p.field] = p.msg
		}
	}
	return v
}

// listingFormRefusal answers a listing create (/listings) or edit (/listings/update) refused with err, when err is
// listingProblems, with 400 and the same form again, after the action's rollback: a summary in the error alert, each problem next to its field and every
// value as sent, except the reusable download content (a delivery secret), which the edit form shows as stored and
// the create form leaves empty, saying so. The CSRF token and a new form nonce are the page's own, as on any load.
// When the page cannot be loaded (the listing is gone, or is not the editor's), the shared refusal page names the
// problems instead.
func listingFormRefusal(c *actionCtx, err error) error {
	var problems listingProblems
	if !errors.As(err, &problems) {
		return err
	}
	edit := c.R.URL.Path == "/listings/update"
	summary := "The listing was not published; nothing was saved. "
	if edit {
		summary = "The changes were not saved. "
	}
	msg := summary + problems.Error()
	return &httpError{Code: http.StatusBadRequest, Msg: problems.Error(), Render: func(w http.ResponseWriter) {
		a := c.A
		r := c.R.Clone(c.R.Context())
		page := "vendor-dashboard"
		r.URL.RawQuery = ""
		if edit {
			page = "listing-edit"
			r.URL.RawQuery = url.Values{"id": {c.Form.Get("id")}}.Encode()
		}
		d := a.pageData(r, page, c.Token, c.User)
		if err := a.load(r, &d); err != nil {
			a.refuse(w, c.R, c.User, http.StatusBadRequest, problems.Error())
			return
		}
		form := listingFormFromPost(c.Form, problems)
		sent := strings.ReplaceAll(c.Form.Get("delivery_content"), "\r\n", "\n")
		switch {
		case edit && d.Listing != nil && d.Listing.VendorEditor:
			if sent != strings.ReplaceAll(d.Listing.DeliveryContent, "\r\n", "\n") {
				form.DeliveryNote = "Reusable download content is never filled in again after a refused form. The saved content is shown; enter your change again."
			}
		case !edit && strings.TrimSpace(sent) != "":
			form.DeliveryNote = "Reusable download content is never filled in again after a refused form. Enter it again."
		}
		d.ListingForm, d.Error = form, msg
		a.renderStatus(w, c.R, d, http.StatusBadRequest)
	}}
}
