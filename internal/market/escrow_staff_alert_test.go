package market

import (
	"html"

	"strings"
)

// outsiders is an unrelated account and the buyer and the vendor of another order: none may read an escrow order.
func (x *escrowEnv) outsiders() []string {
	x.t.Helper()
	s := randomToken()[:6]
	_, stranger := x.user("estranger_"+s, "buyer")
	otherBuyer, otherBuyerSess := x.user("eotherbuyer_"+s, "buyer")
	otherVendor, otherVendorSess := x.user("eothervendor_"+s, "vendor")
	x.testEnv.order(otherBuyer, x.testEnv.product(otherVendor, "digital"), "BTC", statePaid)
	return []string{stranger, otherBuyerSess, otherVendorSess}
}

// exec runs a fixture write, failing the test on error.
func (x *escrowEnv) exec(query string, args ...any) {
	x.t.Helper()
	if _, err := x.DB.Exec(query, args...); err != nil {
		x.t.Fatal(err)
	}
}

// deskAlert is the moderation desk's escrow-alerts entry for order as sess sees it, or "" when it is not listed.
func (x *escrowEnv) deskAlert(order, sess string) string {
	x.t.Helper()
	desk := x.page("/moderator", sess)
	_, section, ok := strings.Cut(desk, `<section id="escrow-alerts"`)
	if !ok {
		return ""
	}
	section, _, _ = strings.Cut(section, "</section>")
	_, entry, ok := strings.Cut(section, `<li><a class="text-link mono" href="/order?id=`+order+`">`)
	if !ok {
		return ""
	}
	entry, _, _ = strings.Cut(entry, "</li>")
	return entry
}

// orderCodes is the status of GET /order?id=order for each session. Each response's status and first 300 bytes are
// logged with it (shown only when the test fails), so an unexpected status explains itself on a recurrence.
func (x *escrowEnv) orderCodes(order string, sessions ...string) []int {
	x.t.Helper()
	var out []int
	for i, s := range sessions {
		w := x.do("GET", "/order?id="+order, s, nil)
		body := w.Body.String()
		x.t.Logf("GET /order?id=%s as session %d of %d: status %d, body starts %q", order, i+1, len(sessions), w.Code, body[:min(len(body), 300)])
		out = append(out, w.Code)
	}
	return out
}

// staffHelp is the help line the order page shows staff who read it (order.html), or "" when it carries none.
func staffHelp(page string) string {
	_, help, ok := strings.Cut(html.UnescapeString(page), "You are viewing this order as ")
	if !ok {
		return ""
	}
	help, _, _ = strings.Cut(help, "</p>")
	return help
}

// staffReadOnly fails when the order page staff are shown offers any form that changes something.
func (x *escrowEnv) staffReadOnly(order, sess string) {
	x.t.Helper()
	if page := x.page("/order?id="+order, sess); strings.Contains(page, `method="post"`) || strings.Contains(page, escrowDelivery) {
		x.t.Fatalf("the staff order page offers an action or shows the delivery:\n%s", page)
	}
}
