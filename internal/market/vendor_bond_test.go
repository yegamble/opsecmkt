package market

import (
	"context"
	"errors"
	"html"
	"net/url"
	"strings"
	"testing"
)

type fakeBondProvider struct {
	coin               string
	confirmed, pending int64
	err                error
	allocations        int
}

func (p *fakeBondProvider) Currency() string { return p.coin }
func (p *fakeBondProvider) Allocate(_ context.Context, id string) (string, string, error) {
	p.allocations++
	return "bond-address-" + id, id, p.err
}
func (p *fakeBondProvider) Observe(context.Context, string, string) (int64, int64, error) {
	return p.confirmed, p.pending, p.err
}

func bondPolicyTest(e *testEnv, n int64, cur string) {
	e.t.Helper()
	agExec(e, `UPDATE settings SET value=json_build_object('amount',$1::bigint,'currency',$2::text)::text WHERE key=$3`, n, cur, vendorBondSetting)
}

func TestVendorBondAdminPolicyAndReceipt(t *testing.T) {
	e := newTestApp(t)
	adminID, admin := e.user("bond_admin", "admin")
	_, vendor := e.user("bond_vendor", "vendor")
	page := e.do("GET", "/admin", admin, nil)
	e.check(page, 200)
	f := url.Values{"amount": {"250.00"}, "currency": {"GBP"}, "password": {testPassword}, formNonceField: {renderedFormNonce(t, page.Body.String(), "/admin/vendor-bond")}}
	e.check(e.do("POST", "/admin/vendor-bond", vendor, f), 403)
	wrong := copyFormValues(f)
	wrong.Set("password", "wrong")
	e.check(e.do("POST", "/admin/vendor-bond", admin, wrong), 401)
	e.check(e.do("POST", "/admin/vendor-bond", admin, f), 303)
	p, err := vendorBondPolicy(context.Background(), e.DB, false)
	if err != nil || p.Amount != 25000 || p.Currency != "GBP" {
		t.Fatalf("policy=%+v err=%v", p, err)
	}
	page = e.do("GET", "/admin", admin, nil)
	f2 := copyFormValues(f)
	f2.Set("amount", "300")
	f2.Set(formNonceField, renderedFormNonce(t, page.Body.String(), "/admin/vendor-bond"))
	e.check(e.do("POST", "/admin/vendor-bond", admin, f2), 303)
	e.check(e.do("POST", "/admin/vendor-bond", admin, f), 303)
	p, _ = vendorBondPolicy(context.Background(), e.DB, false)
	if p.Amount != 30000 || agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE 'Changed vendor bond requirement%'", adminID) != 2 {
		t.Fatal("receipt reverted new policy or repeated audit")
	}
	for _, cur := range []string{"EURO", "DOGE", "usd"} {
		bad := url.Values{"amount": {"5"}, "currency": {cur}, "password": {testPassword}}
		e.check(e.do("POST", "/admin/vendor-bond", admin, bad), 400)
	}
}

func TestVendorBondAutomaticFundingFreshnessAndRenewal(t *testing.T) {
	e := newTestApp(t)
	id, vendor := e.user("bond_auto", "vendor")
	bondPolicyTest(e, 10000, "USD")
	p := &fakeBondProvider{coin: "BTC"}
	e.A.bondProviders = map[string]bondProvider{"BTC": p}
	e.A.fiatRates = &fiatRateService{} // enabled shared cache; provider HTTP is tested separately
	agExec(e, `INSERT INTO fiat_rates(fiat,coin,rate,as_of,sources,checked_at,available) VALUES('USD','BTC','50000',now(),ARRAY['kraken','coingecko','coinpaprika'],now(),true)`)
	e.check(e.do("POST", "/vendor/bond", vendor, url.Values{"currency": {"BTC"}}), 303)
	quoteID := agStr(e, "SELECT id FROM vendor_bond_quotes WHERE vendor_id=$1", id)
	if agInt(e, "SELECT crypto_amount FROM vendor_bond_quotes WHERE id=$1", quoteID) != 200000 {
		t.Fatal("fiat conversion was incorrect")
	}
	p.pending = 200000
	if err := e.A.pollVendorBonds(context.Background()); err != nil {
		t.Fatal(err)
	}
	ok, err := vendorBondSatisfied(context.Background(), e.DB, id, false)
	if err != nil || ok {
		t.Fatal("pending deposit authorized selling")
	}
	p.pending = 0
	p.confirmed = 200000
	if err = e.A.pollVendorBonds(context.Background()); err != nil {
		t.Fatal(err)
	}
	ok, err = vendorBondSatisfied(context.Background(), e.DB, id, false)
	if err != nil || !ok {
		t.Fatal("confirmed deposit did not authorize selling")
	}
	agExec(e, "UPDATE fiat_rates SET rate='100000',available=false")
	ok, _ = vendorBondSatisfied(context.Background(), e.DB, id, false)
	if !ok {
		t.Fatal("funded bond changed with crypto price")
	}
	agExec(e, "UPDATE vendor_bond_quotes SET checked_at=now()-interval '3 minutes'")
	ok, _ = vendorBondSatisfied(context.Background(), e.DB, id, false)
	if ok {
		t.Fatal("stale balance authorized selling")
	}
	p.err = errors.New("provider failed")
	if err = e.A.pollVendorBonds(context.Background()); err != nil {
		t.Fatal(err)
	}
	if agInt(e, "SELECT count(*) FROM vendor_bond_quotes WHERE observation_ok") != 0 {
		t.Fatal("failed provider retained trusted observation")
	}
	p.err = nil
	p.confirmed = 0
	if err = e.A.pollVendorBonds(context.Background()); err != nil {
		t.Fatal(err)
	}
	ok, _ = vendorBondSatisfied(context.Background(), e.DB, id, false)
	if ok {
		t.Fatal("withdrawn bond authorized selling")
	}
	bondPolicyTest(e, 10000, "EUR")
	ok, _ = vendorBondSatisfied(context.Background(), e.DB, id, false)
	if ok {
		t.Fatal("old reference currency satisfied changed policy")
	}
}

func TestVendorBondExpiredQuoteCanRenewWithoutLosingDeposit(t *testing.T) {
	e := newTestApp(t)
	id, vendor := e.user("bond_renew", "vendor")
	bondPolicyTest(e, 100000, "BTC")
	p := &fakeBondProvider{coin: "BTC"}
	e.A.bondProviders = map[string]bondProvider{"BTC": p}
	e.check(e.do("POST", "/vendor/bond", vendor, url.Values{"currency": {"BTC"}}), 303)
	agExec(e, "UPDATE vendor_bond_quotes SET expires_at=now()-interval '1 minute'")
	p.confirmed = 100000
	if err := e.A.pollVendorBonds(context.Background()); err != nil {
		t.Fatal(err)
	}
	ok, _ := vendorBondSatisfied(context.Background(), e.DB, id, false)
	if ok {
		t.Fatal("late funding accepted an expired quote")
	}
	e.check(e.do("POST", "/vendor/bond", vendor, url.Values{"currency": {"BTC"}}), 303)
	if p.allocations != 1 || agInt(e, "SELECT count(*) FROM vendor_bond_quotes") != 1 {
		t.Fatal("renewal discarded existing destination")
	}
	if err := e.A.pollVendorBonds(context.Background()); err != nil {
		t.Fatal(err)
	}
	ok, _ = vendorBondSatisfied(context.Background(), e.DB, id, false)
	if !ok {
		t.Fatal("renewed quote did not recognize existing deposit")
	}
}

func TestVendorBondBlocksNewIntakeAndPublication(t *testing.T) {
	x := newEscrowEnv(t)
	order := x.draft()
	bondPolicyTest(x.testEnv, 100000, "BTC")
	page := x.page("/order?id="+order, x.buyerSess)
	if !strings.Contains(html.UnescapeString(page), sellerBondRequired) || strings.Contains(page, `action="/orders/escrow"`) {
		t.Fatalf("draft offered funding without bond: %s", page)
	}
	before := len(x.core.calls)
	w := x.checkout(order, regtestAddr("bond refund"))
	if w.Code != 409 || !strings.Contains(html.UnescapeString(w.Body.String()), sellerBondRequired) {
		t.Fatalf("checkout=%d %s", w.Code, w.Body)
	}
	x.nothingWritten(order)
	if len(x.core.calls) != before {
		t.Fatal("bond refusal made order RPC")
	}
	x.check(x.do("POST", "/orders", x.buyerSess, url.Values{"product_id": {x.product}, "currency": {"BTC"}}), 409)
	input := url.Values{"title": {"Bond listing"}, "description": {"test"}, "category": {"Digital"}, "region": {"Worldwide"}, "kind": {"digital"}, "price_btc": {"0.001"}, "price_xmr": {"0.1"}, "stock": {"1"}}
	x.check(x.do("POST", "/listings", x.vendorSess, input), 409)
	_, admin := x.user("bond_seller_admin", "admin")
	x.check(x.do("POST", "/listings", admin, input), 409)
	page = x.page("/vendor-dashboard", x.vendorSess)
	if !strings.Contains(page, "Required vendor bond") || strings.Contains(page, `action="/listings"`) {
		t.Fatal("vendor desk hides bond or offers publishing")
	}
}

func TestVendorBondXMRBlocksBeforeSlotClaim(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order := x.draft(x.digital(x.vendor))
	bondPolicyTest(x.testEnv, 100000, "BTC")
	page := x.page("/order?id="+order, x.buyerSess)
	if !strings.Contains(html.UnescapeString(page), sellerBondRequired) || strings.Contains(page, `action="/orders/escrow-xmr"`) {
		t.Fatal("XMR draft still offered funding")
	}
	before := x.fake.count()
	w := x.checkout(x.buyerSess, order, testXMRAddr("bond XMR refund", true))
	if w.Code != 409 || x.fake.count() != before || x.state(order) != stateDraft || agInt(x.testEnv, "SELECT count(*) FROM xmr_escrow_slots WHERE state='claimed'") != 0 {
		t.Fatalf("XMR bond gate changed state or made RPC: %d", w.Code)
	}
}

func TestVendorBondPreservesPaidOrdersAndBlocksRestore(t *testing.T) {
	x := newEscrowEnv(t)
	paid := x.paidPhysical(regtestAddr("bond existing refund"), 100000)
	bondPolicyTest(x.testEnv, 100000, "BTC")
	if code, body := x.shipEscrow(paid, x.vendorSess, regtestAddr("bond existing claim"), testPassword); code != 303 {
		t.Fatalf("existing order could not ship: %d %s", code, body)
	}
	x.check(x.do("POST", "/listings/archive", x.vendorSess, url.Values{"id": {x.product}}), 303)
	x.check(x.do("POST", "/listings/restore", x.vendorSess, url.Values{"id": {x.product}}), 409)
	if agInt(x.testEnv, "SELECT count(*) FROM products WHERE id=$1 AND archived", x.product) != 1 {
		t.Fatal("unbonded listing was restored")
	}
}

func TestVendorBondQuoteReceiptAndPolicyTopUp(t *testing.T) {
	e := newTestApp(t)
	id, vendor := e.user("bond_receipt", "vendor")
	bondPolicyTest(e, 100000, "BTC")
	p := &fakeBondProvider{coin: "BTC", confirmed: 100000}
	e.A.bondProviders = map[string]bondProvider{"BTC": p}
	f := url.Values{"currency": {"BTC"}, formNonceField: {formNonceFor("/vendor/bond")}}
	lost := loseNextActionCommit(t, e)
	w := e.do("POST", "/vendor/bond", vendor, f)
	if lost.Load() || w.Code != 500 || !strings.Contains(w.Body.String(), "outcome unknown") {
		t.Fatalf("lost ack: %d %s", w.Code, w.Body)
	}
	e.check(e.do("POST", "/vendor/bond", vendor, f), 303)
	if p.allocations != 1 || agInt(e, "SELECT count(*) FROM vendor_bond_quotes") != 1 {
		t.Fatal("quote replay allocated twice")
	}
	if err := e.A.pollVendorBonds(context.Background()); err != nil {
		t.Fatal(err)
	}
	bondPolicyTest(e, 100050, "BTC")
	ok, _ := vendorBondSatisfied(context.Background(), e.DB, id, false)
	if ok {
		t.Fatal("policy increase did not require top-up")
	}
	e.check(e.do("POST", "/vendor/bond", vendor, url.Values{"currency": {"BTC"}}), 303)
	if agInt(e, "SELECT max(amount) FROM vendor_bond_quotes") != 100000 || agInt(e, "SELECT count(*) FROM vendor_bond_quotes") != 2 {
		t.Fatal("top-up did not preserve old bond credit")
	}
	if agInt(e, "SELECT crypto_amount FROM vendor_bond_quotes WHERE amount=50") != 1000 {
		t.Fatal("small top-up did not use the Bitcoin receiving minimum")
	}
	if err := e.A.pollVendorBonds(context.Background()); err != nil {
		t.Fatal(err)
	}
	ok, _ = vendorBondSatisfied(context.Background(), e.DB, id, false)
	if !ok {
		t.Fatal("confirmed top-up did not satisfy changed policy")
	}
	agExec(e, "INSERT INTO settings(key,value) VALUES('payments_recovery_required','true') ON CONFLICT(key) DO UPDATE SET value='true'")
	ok, _ = vendorBondSatisfied(context.Background(), e.DB, id, false)
	if ok {
		t.Fatal("restore gate trusted cached bonds")
	}
}

func TestVendorBondDisabledRateServiceRefusesCachedQuote(t *testing.T) {
	e := newTestApp(t)
	_, vendor := e.user("bond_disabled_rates", "vendor")
	bondPolicyTest(e, 10000, "USD")
	p := &fakeBondProvider{coin: "BTC"}
	e.A.bondProviders = map[string]bondProvider{"BTC": p}
	e.A.fiatRates = nil
	agExec(e, `INSERT INTO fiat_rates(fiat,coin,rate,as_of,sources,checked_at,available) VALUES('USD','BTC','50000',now(),ARRAY['kraken','coingecko','coinpaprika'],now(),true)`)
	e.check(e.do("POST", "/vendor/bond", vendor, url.Values{"currency": {"BTC"}}), 503)
	if p.allocations != 0 || agInt(e, "SELECT count(*) FROM vendor_bond_quotes") != 0 {
		t.Fatal("disabled rate service issued a deposit address")
	}
}

func TestVendorBondPolicyChangeSerializesWithNewIntake(t *testing.T) {
	x := newEscrowEnv(t)
	order := x.draft()
	tx, err := x.DB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = lockSetting(context.Background(), tx, vendorBondSetting); err != nil {
		t.Fatal(err)
	}
	w := x.checkout(order, regtestAddr("bond policy race refund"))
	if w.Code != 503 {
		t.Fatalf("checkout did not refuse busy policy: %d %s", w.Code, w.Body)
	}
	x.nothingWritten(order)
}
