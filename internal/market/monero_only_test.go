package market

import (
	"context"
	"net/url"
	"strings"
	"testing"
)

func TestMoneroOnlyRefusesBitcoinConfigurationBeforeDatabase(t *testing.T) {
	for _, name := range []string{"ESCROW_BTC", "BITCOIN_RPC_URL", "BITCOIN_WALLET", "BITCOIN_CHAIN", "BITCOIN_RPC_PASSWORD", "BITCOIN_RPC_AUTH",
		"ESCROW_BTC_RPC_URL", "ESCROW_BTC_ARBITER_WALLET", "ESCROW_BTC_WATCH_WALLET", "ESCROW_BTC_FEE_BPS", "ESCROW_BTC_FEE_ADDRESS", "ESCROW_BTC_FEE_RATE",
		"ESCROW_SIGNER_SOCKET", "ESCROW_SIGNER_RPC_URL", "ESCROW_SIGNER_SEED_FILE", "COMPOSE_PROFILES"} {
		t.Run(name, func(t *testing.T) {
			values := map[string]string{"APP_SECRET_KEY": testSetupToken}
			secret := "http://user:never-echo-this-secret@node.invalid:1234"
			values[name] = secret
			if name == "ESCROW_BTC" {
				values[name] = "on"
			}
			if name == "COMPOSE_PROFILES" {
				values[name] = "monero,escrow-signer"
			}
			_, err := loadStartupConfig(func(k string) string { return values[k] }, "clearnet")
			if err == nil || !strings.Contains(err.Error(), "Monero (XMR) only") || !strings.Contains(err.Error(), name) || strings.Contains(err.Error(), secret) {
				t.Fatalf("Bitcoin configuration did not fail safely before missing DATABASE_URL: %v", err)
			}
		})
	}
	t.Setenv("APP_MODE", "clearnet")
	t.Setenv("APP_SECRET_KEY", testSetupToken)
	t.Setenv("DATABASE_URL", "")
	t.Setenv("ESCROW_BTC", "on")
	if app, err := New(context.Background(), false); app != nil || err == nil || !strings.Contains(err.Error(), "Bitcoin is removed") {
		t.Fatalf("New did not reject Bitcoin before opening the database: (%v, %v)", app, err)
	}
}

func TestMoneroOnlyIgnoresPassiveRetiredBitcoinDefaults(t *testing.T) {
	for _, disabled := range []string{"", "off", "false"} {
		values := map[string]string{"ESCROW_BTC": disabled, "BITCOIN_IMAGE": "retired-image", "BITCOIN_PRUNE": "1000", "PAYMENT_CONFIRMATIONS_BTC": "3", "BITCOIN_MAX_TIP_AGE": "2h"}
		if err := refuseBitcoinEnv(func(k string) string { return values[k] }); err != nil {
			t.Fatalf("inert retired settings refused: %v", err)
		}
	}
}

func TestMoneroOnlyBitcoinRoutesAreUnregistered(t *testing.T) {
	for _, path := range []string{"/orders/escrow", "/escrow-key", "/escrow-key/practice", "/escrow-setup", "/escrow-descriptor", "/escrow-psbt", "/escrow/claim", "/escrow/sign", "/escrow/hardware-sign", "/escrow/bump", "/escrow/bump-sign", "/escrow/hardware-bump-sign", "/escrow/decision", "/escrow/ship", "/escrow/recover", "/admin/escrow-fee", "/admin/escrow-rebroadcast"} {
		if _, ok := actions[path]; ok {
			t.Errorf("retired action registered: %s", path)
		}
		if _, ok := raws[path]; ok {
			t.Errorf("retired download registered: %s", path)
		}
		if _, ok := pages[strings.TrimPrefix(path, "/")]; ok {
			t.Errorf("retired page registered: %s", path)
		}
	}
	if _, ok := actions["/orders/escrow-xmr"]; !ok {
		t.Fatal("Monero checkout is missing")
	}
	if _, ok := actions["/admin/escrow-release-days"]; !ok {
		t.Fatal("shared Monero physical-order deadline action is missing")
	}
}

func TestMoneroOnlyListingsOrdersAndIntake(t *testing.T) {
	e := newTestApp(t)
	vendor, vendorSession := e.user("monero_vendor", "vendor")
	buyer, buyerSession := e.user("monero_buyer", "buyer")
	e.enableOrderPGP(vendor, buyer)
	_, adminSession := e.user("monero_admin", "admin")
	form := url.Values{"title": {"Monero listing"}, "description": {"Fixture"}, "category": {"Digital"}, "region": {"Worldwide"}, "kind": {"digital"}, "stock": {"5"}, "price_xmr": {"0.5"}, "delivery_content": {"Licence"}}
	created := e.do("POST", "/listings", vendorSession, form)
	e.check(created, 303)
	location, err := url.Parse(created.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	product := location.Query().Get("id")
	var btc int64
	if err := e.DB.QueryRow("SELECT btc FROM products WHERE id=$1", product).Scan(&btc); err != nil || btc != 0 {
		t.Fatalf("Monero listing has a fabricated Bitcoin quote %d: %v", btc, err)
	}
	if got := e.do("POST", "/orders", buyerSession, url.Values{"product_id": {product}, "currency": {"BTC"}}); got.Code != 400 {
		t.Fatalf("crafted Bitcoin draft status = %d", got.Code)
	}
	if got := e.do("POST", "/orders", buyerSession, url.Values{"product_id": {product}, "currency": {"XMR"}}); got.Code != 303 {
		t.Fatalf("Monero draft status = %d: %s", got.Code, got.Body.String())
	}
	var currency string
	if err := e.DB.QueryRow("SELECT currency FROM orders WHERE buyer_id=$1 AND product_id=$2", buyer, product).Scan(&currency); err != nil || currency != "XMR" {
		t.Fatalf("stored draft currency = %q: %v", currency, err)
	}
	if got := e.do("POST", "/admin/payment-intake", adminSession, url.Values{"currency": {"BTC"}, "enabled": {intakeEscrow}, "password": {testPassword}}); got.Code != 400 {
		t.Fatalf("crafted Bitcoin intake change status = %d", got.Code)
	}
	for _, path := range []string{"/orders/escrow", "/escrow/sign", "/admin/escrow-fee"} {
		e.check(e.do("POST", path, adminSession, url.Values{"order_id": {product}}), 404)
	}
	for _, path := range []string{"/escrow-key", "/escrow-setup", "/escrow-psbt?id=" + product} {
		e.check(e.do("GET", path, buyerSession, nil), 404)
	}
	for _, probe := range []struct {
		statement, want string
		args            []any
	}{
		{"UPDATE settings SET value='escrow' WHERE key='payment_intake_BTC'", "only Monero payment intake", nil},
		{"UPDATE orders SET currency='BTC' WHERE buyer_id=$1", "orders_monero_only", []any{buyer}},
	} {
		if _, err := e.DB.Exec(probe.statement, probe.args...); err == nil || !strings.Contains(err.Error(), probe.want) {
			t.Fatalf("database failed to reject Bitcoin mutation with its currency guard: %v", err)
		}
	}
	page := e.do("GET", "/checkout?id="+product+"&currency=BTC", buyerSession, nil)
	e.check(page, 200)
	if got := page.Body.String(); strings.Contains(got, `value="BTC"`) || strings.Contains(got, `price_btc`) || !strings.Contains(got, "XMR") {
		t.Fatal("crafted currency query did not retain Monero-only checkout")
	}
}
