package market

import "testing"

// commerceEnv is a provider-free fixture for shared order, account and
// authorization tests. It never issues a payment address or runs a wallet.
type payEnv struct {
	*testEnv
	t                                *testing.T
	buyer, vendor, mod               *User
	buyerSess, vendorSess, adminSess string
	order, productID                 string
}

func newPayEnv(t *testing.T, supplied ...*testEnv) *payEnv {
	var e *testEnv
	if len(supplied) == 0 {
		e = newTestApp(t)
	} else {
		e = supplied[0]
	}
	suffix := randomToken()[:6]
	b, bs := e.user("cbuyer_"+suffix, "buyer")
	v, vs := e.user("cvendor_"+suffix, "vendor")
	m, _ := e.user("cmod_"+suffix, "moderator")
	_, as := e.user("cadmin_"+suffix, "admin")
	_, key := testPGPKey(t, "cvendor_"+suffix)
	e.setKey(v, key)
	p := &payEnv{testEnv: e, t: t, buyer: e.loadUser(b), vendor: e.loadUser(v), mod: e.loadUser(m), buyerSess: bs, vendorSess: vs, adminSess: as}
	p.productID = e.product(v, "physical")
	p.order = e.order(b, p.productID, "BTC", stateDraft)
	return p
}
func (p *payEnv) state(order string) string {
	p.t.Helper()
	var state string
	if err := p.DB.QueryRow("SELECT state FROM orders WHERE id=$1", order).Scan(&state); err != nil {
		p.t.Fatal(err)
	}
	return state
}
func (p *payEnv) count(query string, args ...any) int {
	p.t.Helper()
	var n int
	if err := p.DB.QueryRow(query, args...).Scan(&n); err != nil {
		p.t.Fatal(err)
	}
	return n
}
func (p *payEnv) page(path, session string) string {
	p.t.Helper()
	w := p.do("GET", path, session, nil)
	p.check(w, 200)
	return w.Body.String()
}
func (p *payEnv) newOrder(state string) (string, string) {
	p.t.Helper()
	return p.testEnv.order(p.buyer.ID, p.productID, "BTC", state), ""
}

func (p *payEnv) str(query string, args ...any) string {
	p.t.Helper()
	var value string
	if err := p.DB.QueryRow(query, args...).Scan(&value); err != nil {
		p.t.Fatal(err)
	}
	return value
}
