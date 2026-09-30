package market

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Escrow Phase 1, slice P1-b: escrow checkout and creation. DB integration tests with the simulated Core of
// escrow_fake_test.go (simulated RPC, not chain evidence; scripts/test-escrow-regtest.sh runs a real Core leg).

// escrowEnv is a market with escrow on against the fake Core: a buyer and a vendor with active escrow keys, a
// digital listing with automatic delivery and the buyer's BTC draft.
type escrowEnv struct {
	*testEnv
	t                     *testing.T
	core                  *fakeEscrowCore
	srv                   *payRPCServer
	buyer, vendor         string
	buyerSess, vendorSess string
	buyerKey, vendorKey   string
	product, order        string
}

const escrowDelivery = "licence key ESCROW-TEST-0001"

func newEscrowEnv(t *testing.T) *escrowEnv {
	t.Helper()
	e, core, srv := escrowTestApp(t)
	x := &escrowEnv{testEnv: e, t: t, core: core, srv: srv, buyerKey: testBIP48Key, vendorKey: altEscrowKey(t, testBIP48Key, "5eed5eed", 1)}
	s := randomToken()[:6]
	x.buyer, x.buyerSess = e.user("ebuyer_"+s, "buyer")
	x.vendor, x.vendorSess = e.user("evendor_"+s, "vendor")
	x.activeKey(x.buyer, x.buyerKey)
	x.activeKey(x.vendor, x.vendorKey)
	x.product = x.autoListing(x.vendor)
	x.order = e.order(x.buyer, x.product, "BTC", stateDraft)
	return x
}

// draft is a new BTC draft of the buyer's on a new listing that takes escrow.
func (x *escrowEnv) draft() string {
	return x.testEnv.order(x.buyer, x.autoListing(x.vendor), "BTC", stateDraft)
}

// autoListing is a digital listing with automatic delivery content.
func (x *escrowEnv) autoListing(vendor string) string {
	x.t.Helper()
	id := x.testEnv.product(vendor, "digital")
	if _, err := x.DB.Exec("UPDATE products SET delivery_content=$2 WHERE id=$1", id, escrowDelivery); err != nil {
		x.t.Fatal(err)
	}
	return id
}

// activeKey stores expr as uid's escrow key, active (as registration and practice signing leave it).
func (x *escrowEnv) activeKey(uid, expr string) {
	x.t.Helper()
	k, err := parseEscrowKey(expr)
	if err != nil {
		x.t.Fatal(err)
	}
	sealed, err := x.A.seal("escrow-xpub", k.XPub)
	if err != nil {
		x.t.Fatal(err)
	}
	if _, err = x.DB.Exec(`INSERT INTO user_escrow_keys(user_id,xpub_sealed,xpub_hmac,origin_fingerprint,origin_path,active_at) VALUES($1,$2,$3,$4,$5,now())
		ON CONFLICT (user_id) DO UPDATE SET xpub_sealed=EXCLUDED.xpub_sealed,xpub_hmac=EXCLUDED.xpub_hmac,origin_fingerprint=EXCLUDED.origin_fingerprint,
		origin_path=EXCLUDED.origin_path,active_at=now()`, uid, sealed, x.A.escrowKeyHMAC(k), k.Fingerprint, k.PathText); err != nil {
		x.t.Fatal(err)
	}
}

func (x *escrowEnv) state(order string) string {
	x.t.Helper()
	var s string
	if err := x.DB.QueryRow("SELECT state FROM orders WHERE id=$1", order).Scan(&s); err != nil {
		x.t.Fatal(err)
	}
	return s
}

func (x *escrowEnv) page(path, session string) string {
	x.t.Helper()
	w := x.do("GET", path, session, nil)
	x.check(w, 200)
	return w.Body.String()
}

func (x *escrowEnv) checkout(order, refund string) *httptest.ResponseRecorder {
	return x.do("POST", "/orders/escrow", x.buyerSess, url.Values{"order_id": {order}, "refund_address": {refund}})
}

func (x *escrowEnv) mustCheckout(order, refund string) string {
	x.t.Helper()
	w := x.checkout(order, refund)
	if w.Code != 303 || w.Header().Get("Location") != "/order?id="+order || x.cookie(w, "notice") != "escrow-created" {
		x.t.Fatalf("checkout: %d %q %s", w.Code, w.Header().Get("Location"), w.Body.String())
	}
	var addr string
	if err := x.DB.QueryRow("SELECT address FROM escrows WHERE order_id=$1", order).Scan(&addr); err != nil {
		x.t.Fatal(err)
	}
	return addr
}

// nothingWritten asserts that a refused checkout left order still a draft with its stock and no escrow.
func (x *escrowEnv) nothingWritten(order string) {
	x.t.Helper()
	var state string
	var stock int
	x.DB.QueryRow("SELECT o.state,p.stock FROM orders o JOIN products p ON p.id=o.product_id WHERE o.id=$1", order).Scan(&state, &stock)
	if state != stateDraft || stock != 5 {
		x.t.Fatalf("order %s stock %d", state, stock)
	}
	if n := agInt(x.testEnv, "SELECT (SELECT count(*) FROM escrows WHERE order_id=$1)+(SELECT count(*) FROM escrow_pubkeys p JOIN escrows e ON e.id=p.escrow_id WHERE e.order_id=$1)+(SELECT count(*) FROM payment_addresses WHERE order_id=$1)", order); n != 0 {
		x.t.Fatalf("%d escrow rows written", n)
	}
}

// altEscrowKey is key with another chain code (so another key) under origin fingerprint fp.
func altEscrowKey(t *testing.T, key, fp string, n byte) string {
	t.Helper()
	k, err := parseEscrowKey(key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base58CheckDecode(k.XPub)
	if err != nil {
		t.Fatal(err)
	}
	raw[13] ^= n
	return "[" + fp + k.PathText + "]" + base58CheckEncode(raw)
}

func childHex(expr, suffix string) string {
	return hex.EncodeToString(fakeChild(expr[strings.IndexByte(expr, ']')+1:] + suffix))
}

func fakeArbiterExpr() string { return "[" + fakeArbiterFP + "/84h/1h/0h]" + testBIP84Key[20:] }

// A successful escrow checkout (ES-3, ES-5): the refund address pinned, the fee snapshotted, the three child keys
// stored bare with each party's own origin, the bare descriptor imported before the address exists anywhere, and
// the order page and download showing each reader its own origin only.
func TestEscrowCheckoutCreatesEscrow(t *testing.T) {
	x := newEscrowEnv(t)
	t.Setenv("ESCROW_BTC_FEE_BPS", "250")
	t.Setenv("ESCROW_BTC_FEE_ADDRESS", "bcrt1qmarketfee0000000000000000000000000")
	x.restart()
	calls := len(x.core.methods())
	var inTx int
	x.core.beforeImport = func() { inTx = agInt(x.testEnv, "SELECT count(*) FROM escrows WHERE order_id=$1", x.order) }
	refund := "bcrt1qbuyerrefund00000000000000000000001"
	addr := x.mustCheckout(x.order, refund)
	if got := x.core.methods()[calls:]; !slices.Equal(got, []string{"getdescriptorinfo", "deriveaddresses", "createpsbt", "utxoupdatepsbt", "getdescriptorinfo", "deriveaddresses", "importdescriptors"}) {
		t.Fatalf("RPC sequence %v", got)
	}
	if inTx != 0 {
		t.Fatal("the escrow row existed before the import")
	}
	buyerPub, vendorPub, marketPub := childHex(x.buyerKey, "/0/1"), childHex(x.vendorKey, "/0/1"), childHex(fakeArbiterExpr(), "/0/0")
	bare := "wsh(sortedmulti(2," + buyerPub + "," + vendorPub + "," + marketPub + "))"
	if imported := x.core.importedDescriptors(); !slices.Equal(imported, []string{withChecksum(bare)}) {
		t.Fatalf("imported %q, want only the bare descriptor", imported)
	}
	path, params := x.core.lastCall(t, "importdescriptors")
	if path != fakeWatchWallet || !strings.Contains(params, `"active":false`) || !strings.Contains(params, `"timestamp":"now"`) || strings.Contains(params, "tpub") {
		t.Fatalf("import %s %s", path, params)
	}
	if x.core.address(bare) != addr {
		t.Fatalf("stored address %s is not the imported descriptor's", addr)
	}
	var marketIndex, feeBps int64
	var pinned, feeDest string
	if err := x.DB.QueryRow("SELECT market_index,refund_address,fee_bps,fee_destination FROM escrows WHERE order_id=$1", x.order).Scan(&marketIndex, &pinned, &feeBps, &feeDest); err != nil {
		t.Fatal(err)
	}
	if marketIndex != 0 || pinned != refund || feeBps != 250 || feeDest != "bcrt1qmarketfee0000000000000000000000000" {
		t.Fatalf("escrow %d %s %d %s", marketIndex, pinned, feeBps, feeDest)
	}
	bk, _ := parseEscrowKey(x.buyerKey)
	vk, _ := parseEscrowKey(x.vendorKey)
	rows := agStrings(t, x.testEnv, `SELECT party||' '||pubkey||' '||coalesce(user_id,'-')||' '||coalesce(key_hmac,'-')||' '||origin_fingerprint||' '||origin_path||' '||derivation_suffix||' '||coalesce(per_order_xpub,'-')
		FROM escrow_pubkeys p JOIN escrows e ON e.id=p.escrow_id WHERE e.order_id=$1 ORDER BY party`, x.order)
	want := []string{
		"buyer " + buyerPub + " " + x.buyer + " " + x.A.escrowKeyHMAC(bk) + " 7450f935 /48h/1h/0h/2h /0/1 -",
		"market " + marketPub + " - - " + fakeArbiterFP + " /84h/1h/0h /0/0 -",
		"vendor " + vendorPub + " " + x.vendor + " " + x.A.escrowKeyHMAC(vk) + " 5eed5eed /48h/1h/0h/2h /0/1 -",
	}
	if !slices.Equal(rows, want) {
		t.Fatalf("escrow_pubkeys\n%s\nwant\n%s", strings.Join(rows, "\n"), strings.Join(want, "\n"))
	}
	assertNowhereInDB(t, x.testEnv, bk.XPub[4:40])
	if x.state(x.order) != stateAwaitingPayment || agInt(x.testEnv, "SELECT stock FROM products WHERE id=$1", x.product) != 4 {
		t.Fatal("order not awaiting payment with one unit reserved")
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM payment_addresses WHERE order_id=$1", x.order); n != 0 {
		t.Fatalf("%d pooled deposit addresses for an escrow order", n)
	}
	audit := agStrings(t, x.testEnv, "SELECT action FROM audit_events WHERE user_id=$1", x.buyer)
	if !slices.Equal(audit, []string{"Created Bitcoin escrow (regtest) for order " + x.order[:8]}) {
		t.Fatalf("audit %q", audit)
	}
	for _, text := range append(audit, agStrings(t, x.testEnv, "SELECT note FROM order_events WHERE order_id=$1", x.order)...) {
		if strings.Contains(text, "7450f935") || strings.Contains(text, buyerPub) || strings.Contains(text, refund) || strings.Contains(text, addr) {
			t.Fatalf("fixed text carries escrow data: %q", text)
		}
	}
	// Each reader sees its own origin, the other two keys bare, with the checksum, and can download it.
	buyerDesc := "wsh(sortedmulti(2,[7450f935/48h/1h/0h/2h/0/1]" + buyerPub + "," + vendorPub + "," + marketPub + "))"
	vendorDesc := "wsh(sortedmulti(2," + buyerPub + ",[5eed5eed/48h/1h/0h/2h/0/1]" + vendorPub + "," + marketPub + "))"
	for _, c := range []struct{ sess, desc, notOrigin string }{{x.buyerSess, buyerDesc, "5eed5eed"}, {x.vendorSess, vendorDesc, "7450f935"}} {
		body := x.page("/order?id="+x.order, c.sess)
		for _, want := range []string{addr, withChecksum(c.desc), "regtest", "if the key shown as yours is yours", "can see on the blockchain which coins funded it", `href="/escrow-descriptor?id=` + x.order + `"`} {
			if !strings.Contains(body, want) {
				t.Fatalf("order page lacks %q:\n%s", want, body)
			}
		}
		if strings.Contains(body, "["+c.notOrigin) || strings.Contains(body, fakeArbiterFP) || strings.Contains(body, "tpub") {
			t.Fatal("order page shows another party's origin, the market's fingerprint or an extended key")
		}
		w := x.do("GET", "/escrow-descriptor?id="+x.order, c.sess, nil)
		if w.Code != 200 || w.Body.String() != withChecksum(c.desc)+"\n" || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") ||
			w.Header().Get("Content-Disposition") != `attachment; filename="escrow-`+x.order[:8]+`.txt"` {
			t.Fatalf("download %d %q %q", w.Code, w.Header(), w.Body.String())
		}
	}
	if body := x.page("/order?id="+x.order, x.buyerSess); !strings.Contains(body, refund) {
		t.Fatal("the buyer's page does not show the pinned refund address")
	}
	if body := x.page("/order?id="+x.order, x.vendorSess); strings.Contains(body, refund) {
		t.Fatal("the vendor's page shows the buyer's refund address")
	}
	// Anyone else: the download does not exist for them.
	_, other := x.user("eother_"+randomToken()[:6], "buyer")
	unknown := x.do("GET", "/no-such-page", other, nil)
	for _, sess := range []string{other, randomToken()} {
		if w := x.do("GET", "/escrow-descriptor?id="+x.order, sess, nil); w.Code != 404 || w.Body.String() != unknown.Body.String() {
			t.Fatalf("download for a non-party: %d", w.Code)
		}
	}
	// With the escrow node down the page says deposits are not being read, and still shows the escrow to check.
	up := x.A.escrow.Load()
	x.A.escrow.Store(nil)
	if body := x.page("/order?id="+x.order, x.buyerSess); !strings.Contains(body, "deposits to this escrow are not being read") || !strings.Contains(body, addr) {
		t.Fatal("escrow page with the node down")
	}
	x.A.escrow.Store(up)
	// A second escrow for the same parties: fresh child keys (/0/2 each) and the next market index.
	order2 := x.draft()
	x.mustCheckout(order2, "bcrt1qbuyerrefund00000000000000000000002")
	if s := agStrings(t, x.testEnv, "SELECT party||derivation_suffix FROM escrow_pubkeys p JOIN escrows e ON e.id=p.escrow_id WHERE e.order_id=$1 ORDER BY party", order2); !slices.Equal(s, []string{"buyer/0/2", "market/0/1", "vendor/0/2"}) {
		t.Fatalf("second escrow suffixes %v", s)
	}
}

// Checkout refusals (ES-3, ES-5, ES-9): each refuses before anything is written, and those that need no node make
// no RPC.
func TestEscrowCheckoutRefusals(t *testing.T) {
	x := newEscrowEnv(t)
	refund := "bcrt1qbuyerrefund00000000000000000000009"
	refuse := func(name string, order string, form url.Values, sess string, code int, want string, rpc bool) {
		t.Helper()
		calls, imports := len(x.core.methods()), len(x.core.importedDescriptors())
		if form == nil {
			form = url.Values{"order_id": {order}, "refund_address": {refund}}
		}
		w := x.do("POST", "/orders/escrow", sess, form)
		if w.Code != code || !strings.Contains(w.Body.String(), want) {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
		}
		x.nothingWritten(order)
		if !rpc && len(x.core.methods()) != calls {
			t.Fatalf("%s: node called: %v", name, x.core.methods()[calls:])
		}
		if n := len(x.core.importedDescriptors()); n != imports {
			t.Fatalf("%s: %d new imports", name, n-imports)
		}
	}
	refuse("no refund address", x.order, url.Values{"order_id": {x.order}}, x.buyerSess, 400, "refund address", false)
	refuse("refund address with a descriptor in it", x.order, url.Values{"order_id": {x.order}, "refund_address": {"bcrt1q),pk(02aa"}}, x.buyerSess, 400, "refund address", false)
	refuse("refund address the node refuses", x.order, url.Values{"order_id": {x.order}, "refund_address": {"tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx"}}, x.buyerSess, 400, "not a valid regtest Bitcoin address", true)
	refuse("vendor posts", x.order, nil, x.vendorSess, 403, "Only the buyer", false)
	_, stranger := x.user("estranger_"+randomToken()[:6], "buyer")
	refuse("stranger posts", x.order, nil, stranger, 404, "Order not found", false)

	physical := x.testEnv.order(x.buyer, x.testEnv.product(x.vendor, "physical"), "BTC", stateDraft)
	refuse("physical listing", physical, url.Values{"order_id": {physical}, "refund_address": {refund}}, x.buyerSess, 409, "digital listings with automatic delivery", false)
	manual := x.testEnv.order(x.buyer, x.testEnv.product(x.vendor, "digital"), "BTC", stateDraft)
	refuse("digital listing without automatic delivery", manual, url.Values{"order_id": {manual}, "refund_address": {refund}}, x.buyerSess, 409, "digital listings with automatic delivery", false)
	xmr := x.testEnv.order(x.buyer, x.product, "XMR", stateDraft)
	refuse("Monero order", xmr, url.Values{"order_id": {xmr}, "refund_address": {refund}}, x.buyerSess, 409, "Bitcoin orders only", false)
	own := x.testEnv.order(x.vendor, x.product, "BTC", stateDraft)
	refuse("vendor buying its own listing", own, url.Values{"order_id": {own}, "refund_address": {refund}}, x.vendorSess, 409, "your own listing", false)

	x.DB.Exec("UPDATE user_escrow_keys SET active_at=NULL WHERE user_id=$1", x.buyer)
	refuse("buyer key inactive", x.order, nil, x.buyerSess, 409, "Activate your Bitcoin escrow key", false)
	x.DB.Exec("UPDATE user_escrow_keys SET active_at=now() WHERE user_id=$1", x.buyer)
	x.DB.Exec("UPDATE user_escrow_keys SET active_at=NULL WHERE user_id=$1", x.vendor)
	refuse("vendor key inactive", x.order, nil, x.buyerSess, 409, "vendor has no active Bitcoin escrow key", false)
	x.DB.Exec("DELETE FROM user_escrow_keys WHERE user_id=$1", x.vendor)
	refuse("vendor without a key", x.order, nil, x.buyerSess, 409, "vendor has no active Bitcoin escrow key", false)
	x.activeKey(x.vendor, x.vendorKey)
	x.DB.Exec("UPDATE user_escrow_keys SET xpub_sealed='unreadable' WHERE user_id=$1", x.buyer)
	refuse("buyer key unreadable (APP_SECRET_KEY changed)", x.order, nil, x.buyerSess, 409, "can no longer be read", false)
	x.activeKey(x.buyer, x.buyerKey)

	// The refund address may not be one the market already used as an escrow, settlement or fee destination.
	other := x.draft()
	escrowAddr := x.mustCheckout(other, "bcrt1qbuyerrefund0000000000000000000000a")
	for name, used := range map[string]string{
		"another escrow's refund address": "bcrt1qbuyerrefund0000000000000000000000a",
		"the same address in capitals":    "BCRT1QBUYERREFUND0000000000000000000000A",
		"an escrow address":               escrowAddr,
	} {
		refuse(name, x.order, url.Values{"order_id": {x.order}, "refund_address": {used}}, x.buyerSess, 409, "already used on this market", true)
	}
	// A fee destination, snapshotted on an escrow or configured now.
	x.DB.Exec("UPDATE escrows SET fee_bps=100,fee_destination='bcrt1qoldfee000000000000000000000000000' WHERE order_id=$1", other)
	refuse("a snapshotted fee destination", x.order, url.Values{"order_id": {x.order}, "refund_address": {"bcrt1qoldfee000000000000000000000000000"}}, x.buyerSess, 409, "already used on this market", true)
	x.A.escrow.Load().feeAddress = "bcrt1qnowfee000000000000000000000000000"
	refuse("the configured fee destination", x.order, url.Values{"order_id": {x.order}, "refund_address": {"bcrt1qnowfee000000000000000000000000000"}}, x.buyerSess, 409, "already used on this market", true)
	x.A.escrow.Load().feeAddress = ""
	x.DB.Exec("INSERT INTO payment_addresses(order_id,currency,address,provider) VALUES($1,'BTC','bcrt1qpooleddeposit000000000000000000000','btc-regtest')", x.testEnv.order(x.buyer, x.testEnv.product(x.vendor, "physical"), "BTC", stateAwaitingPayment))
	refuse("a pooled deposit address", x.order, url.Values{"order_id": {x.order}, "refund_address": {"bcrt1qpooleddeposit000000000000000000000"}}, x.buyerSess, 409, "already used on this market", true)

	// Not a draft any more (a repeated checkout): 409, nothing new.
	if w := x.checkout(other, "bcrt1qbuyerrefund0000000000000000000000b"); w.Code != 409 {
		t.Fatalf("repeated checkout: %d", w.Code)
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM escrows"); n != 1 {
		t.Fatalf("%d escrows", n)
	}

	// Escrow switched on but down: 409; switched off: the action does not exist (as an unknown action).
	down := x.A.escrow.Load()
	x.A.escrow.Store(nil)
	refuse("escrow down", x.order, nil, x.buyerSess, 409, "Bitcoin escrow is unavailable", false)
	x.A.escrow.Store(down)
	x.A.escrowConfigured = false
	unknown := x.do("POST", "/no-such-action", x.buyerSess, url.Values{"order_id": {x.order}, "refund_address": {refund}})
	w := x.checkout(x.order, refund)
	if w.Code != 404 || w.Body.String() != unknown.Body.String() {
		t.Fatalf("flag off: %d %s", w.Code, w.Body.String())
	}
	x.nothingWritten(x.order)
	x.A.escrowConfigured = true
}

// A failed build writes nothing and shows no address: an import the watch wallet refuses, a node whose bare-key
// address differs from the extended-key one, a witness script with a repeated key, and one with two keys from the
// arbiter's fingerprint. Only the import failure reaches the import; none leaves a row.
func TestEscrowCheckoutBuildFailureWritesNothing(t *testing.T) {
	logs := captureLog(t)
	x := newEscrowEnv(t)
	x.core.importFail = true
	if w := x.checkout(x.order, "bcrt1qbuyerrefund00000000000000000000001"); w.Code != 503 || !strings.Contains(w.Body.String(), "Nothing was changed") {
		t.Fatalf("import failure: %d %s", w.Code, w.Body.String())
	}
	x.nothingWritten(x.order)
	if body := x.page("/order?id="+x.order, x.buyerSess); strings.Contains(body, "Escrow address") {
		t.Fatal("an escrow address is shown after a failed import")
	}
	x.core.importFail = false

	x.core.bareAddrSalt = "ff"
	if w := x.checkout(x.order, "bcrt1qbuyerrefund00000000000000000000001"); w.Code != 503 {
		t.Fatalf("address mismatch: %d %s", w.Code, w.Body.String())
	}
	x.nothingWritten(x.order)
	x.core.bareAddrSalt = ""

	// A node whose checksum for the bare-key descriptor differs from the one the page would show.
	x.core.infoSkew = true
	if w := x.checkout(x.order, "bcrt1qbuyerrefund00000000000000000000001"); w.Code != 503 {
		t.Fatalf("checksum mismatch: %d %s", w.Code, w.Body.String())
	}
	x.nothingWritten(x.order)
	x.core.infoSkew = false

	// The vendor registered the buyer's very key (another HMAC, so the database let it through): the escrow would
	// hold one key twice, which lets one party spend alone.
	bk, _ := parseEscrowKey(x.buyerKey)
	sealed, _ := x.A.seal("escrow-xpub", bk.XPub)
	x.DB.Exec("UPDATE user_escrow_keys SET xpub_sealed=$2,origin_fingerprint='7450f935',xpub_hmac=repeat('0',64) WHERE user_id=$1", x.vendor, sealed)
	if w := x.checkout(x.order, "bcrt1qbuyerrefund00000000000000000000001"); w.Code != 503 {
		t.Fatalf("duplicate key: %d %s", w.Code, w.Body.String())
	}
	x.nothingWritten(x.order)
	// A party key under the arbiter's fingerprint: two keys would come from the market's fingerprint.
	x.activeKey(x.vendor, altEscrowKey(t, x.vendorKey, fakeArbiterFP, 2))
	if w := x.checkout(x.order, "bcrt1qbuyerrefund00000000000000000000001"); w.Code != 503 {
		t.Fatalf("second arbiter key: %d %s", w.Code, w.Body.String())
	}
	x.nothingWritten(x.order)
	if n := len(x.core.importedDescriptors()); n != 0 {
		t.Fatalf("%d descriptors imported by failed builds", n)
	}
	if strings.Contains(logs.String(), "tpub") || strings.Contains(logs.String(), "7450f935") || strings.Contains(logs.String(), "bcrt1qbuyerrefund") {
		t.Fatalf("log carries escrow data: %s", logs.String())
	}
	// Then a good build still works (the market indices burned by failures are skipped, never reused).
	x.activeKey(x.vendor, x.vendorKey)
	x.mustCheckout(x.order, "bcrt1qbuyerrefund00000000000000000000001")
	if n := agInt(x.testEnv, "SELECT count(*) FROM escrows"); n != 1 {
		t.Fatalf("%d escrows", n)
	}
}

// Repeated and concurrent checkouts: one escrow per order, and one refund address per escrow even when two orders
// race for it.
func TestEscrowCheckoutConcurrent(t *testing.T) {
	x := newEscrowEnv(t)
	var wg sync.WaitGroup
	codes := make([]int, 4)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = x.checkout(x.order, fmt.Sprintf("bcrt1qbuyerrefund0000000000000000000000%d", i)).Code
		}()
	}
	wg.Wait()
	slices.Sort(codes)
	if codes[0] != 303 || codes[1] == 303 || agInt(x.testEnv, "SELECT count(*) FROM escrows WHERE order_id=$1", x.order) != 1 {
		t.Fatalf("same order: %v", codes)
	}
	// Two orders, two buyers, one refund address.
	_, sess2 := x.user("ebuyer2_"+randomToken()[:6], "buyer")
	b2 := agStrings(t, x.testEnv, "SELECT user_id FROM sessions WHERE token_hash=$1", digest(sess2))[0]
	x.activeKey(b2, altEscrowKey(t, testBIP48Key, "0dd0dd0d", 3))
	o1, o2 := x.draft(), x.testEnv.order(b2, x.autoListing(x.vendor), "BTC", stateDraft)
	shared := "bcrt1qsharedrefund0000000000000000000000"
	var c1, c2 int
	wg.Add(2)
	go func() { defer wg.Done(); c1 = x.checkout(o1, shared).Code }()
	go func() {
		defer wg.Done()
		c2 = x.do("POST", "/orders/escrow", sess2, url.Values{"order_id": {o2}, "refund_address": {shared}}).Code
	}()
	wg.Wait()
	if (c1 == 303) == (c2 == 303) || agInt(x.testEnv, "SELECT count(*) FROM escrows WHERE refund_address=$1", shared) != 1 {
		t.Fatalf("shared refund address: %d %d", c1, c2)
	}
	loser := o1
	if c1 == 303 {
		loser = o2
	}
	x.nothingWritten(loser)
}

// Everything outside escrow keeps today's pooled flow: with ESCROW_BTC on, a physical order, a digital order without
// automatic delivery and an XMR order render exactly as with it off, and a pooled payment request on an eligible
// listing still issues a pooled deposit address.
func TestEscrowLeavesPooledOrdersUnchanged(t *testing.T) {
	x := newEscrowEnv(t)
	fake := newFakeProvider("BTC", 3)
	x.A.payments["BTC"] = fake
	pages := map[string]string{}
	orders := map[string]string{
		"physical": x.testEnv.order(x.buyer, x.testEnv.product(x.vendor, "physical"), "BTC", stateDraft),
		"manual":   x.testEnv.order(x.buyer, x.testEnv.product(x.vendor, "digital"), "BTC", stateDraft),
		"xmr":      x.testEnv.order(x.buyer, x.product, "XMR", stateDraft),
	}
	for name, id := range orders {
		pages[name] = x.page("/order?id="+id, x.buyerSess)
		if strings.Contains(pages[name], "/orders/escrow") || strings.Contains(pages[name], "escrow") {
			t.Fatalf("%s order page mentions escrow", name)
		}
	}
	x.A.escrowConfigured = false
	x.A.escrow.Store(nil)
	for name, id := range orders {
		if got := x.page("/order?id="+id, x.buyerSess); got != pages[name] {
			t.Fatalf("%s order page differs with escrow off", name)
		}
	}
	// The eligible listing with escrow off: no escrow form; with it on, both the pooled request and escrow.
	off := x.page("/order?id="+x.order, x.buyerSess)
	if strings.Contains(off, "/orders/escrow") {
		t.Fatal("escrow checkout offered with ESCROW_BTC off")
	}
	x.restart()
	x.A.payments["BTC"] = fake
	on := x.page("/order?id="+x.order, x.buyerSess)
	if !strings.Contains(on, `action="/orders/escrow"`) || !strings.Contains(on, `action="/orders/pay"`) || !strings.Contains(on, `name="refund_address"`) {
		t.Fatalf("eligible draft with escrow on: %s", on)
	}
	if w := x.do("POST", "/orders/pay", x.buyerSess, url.Values{"order_id": {x.order}}); w.Code != 303 {
		t.Fatalf("pooled request: %d %s", w.Code, w.Body.String())
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM payment_addresses WHERE order_id=$1", x.order); n != 1 || agInt(x.testEnv, "SELECT count(*) FROM escrows") != 0 {
		t.Fatal("pooled request did not issue a pooled address")
	}
}

// rotatedSecretKey is the APP_SECRET_KEY a rotation test switches to: 64 hex digits derived from a fixed phrase, so
// the source holds no key-shaped literal for secret scanners to flag (A-370).
func rotatedSecretKey() string {
	sum := sha256.Sum256([]byte("escrow test: rotated APP_SECRET_KEY"))
	return hex.EncodeToString(sum[:])
}

// The chair's ruling (a) on QA25's carry-over: an APP_SECRET_KEY rotation never hides an existing escrow or changes
// which key it uses. After a rotation the stored key is unreadable and its HMAC differs; the owner registers the same
// key again: the escrow is still attributed to the account and its rows are untouched, the key is not offered
// practice (it has escrows), Core confirms it derives the escrow's child key so it is active again, and the next
// escrow takes the next child index. Reuse detection across the rotation is best-effort: the old HMAC no longer
// matches (documented in the operator guide).
func TestEscrowSurvivesSecretRotation(t *testing.T) {
	x := newEscrowEnv(t)
	x.mustCheckout(x.order, "bcrt1qbuyerrefund00000000000000000000001")
	before := strings.Join(agStrings(t, x.testEnv, "SELECT p::text FROM escrow_pubkeys p ORDER BY pubkey"), "\n") + strings.Join(agStrings(t, x.testEnv, "SELECT e::text FROM escrows e"), "\n")
	t.Setenv("APP_SECRET_KEY", rotatedSecretKey())
	x.restart()
	body := x.page("/escrow-key", x.buyerSess)
	if !strings.Contains(body, "register it again") || !strings.Contains(body, "This key is used by escrows") || strings.Contains(body, "Show the practice transaction") {
		t.Fatalf("after rotation: %s", body)
	}
	calls := len(x.core.methods())
	if code, body := registerKey(x.testEnv, x.buyerSess, x.buyerKey); code != 303 {
		t.Fatalf("re-register: %d %s", code, body)
	}
	if got := x.core.methods()[calls:]; !slices.Equal(got, []string{"deriveaddresses", "deriveaddresses"}) {
		t.Fatalf("re-registration RPCs %v", got)
	}
	after := strings.Join(agStrings(t, x.testEnv, "SELECT p::text FROM escrow_pubkeys p ORDER BY pubkey"), "\n") + strings.Join(agStrings(t, x.testEnv, "SELECT e::text FROM escrows e"), "\n")
	if after != before {
		t.Fatalf("escrow rows changed by the rotation and re-registration:\n%s\n%s", before, after)
	}
	body = x.page("/escrow-key", x.buyerSess)
	if !strings.Contains(body, "Active since") || !strings.Contains(body, "This key is used by escrows") || strings.Contains(body, "Show the practice transaction") {
		t.Fatalf("re-registered: %s", body)
	}
	audit := agStrings(t, x.testEnv, "SELECT action FROM audit_events WHERE user_id=$1 ORDER BY id DESC LIMIT 1", x.buyer)
	if !strings.Contains(audit[0], "the key of your existing escrows") {
		t.Fatalf("audit %q", audit)
	}
	// Registering another key under the same origin (a wrong fingerprint or path typed for it) is refused.
	if code, body := registerKey(x.testEnv, x.buyerSess, altEscrowKey(t, x.buyerKey, "7450f935", 4)); code != 400 || !strings.Contains(body, "same origin as the key your escrows use") {
		t.Fatalf("other key, same origin: %d %s", code, body)
	}
	// The vendor's key is also unreadable now; the vendor registers it again too, then a new escrow uses /0/2.
	if code, body := registerKey(x.testEnv, x.vendorSess, x.vendorKey, "dedicated", "1"); code != 303 {
		t.Fatalf("vendor re-register: %d %s", code, body)
	}
	order2 := x.draft()
	x.mustCheckout(order2, "bcrt1qbuyerrefund00000000000000000000002")
	if s := agStrings(t, x.testEnv, "SELECT party||derivation_suffix FROM escrow_pubkeys p JOIN escrows e ON e.id=p.escrow_id WHERE e.order_id=$1 AND party<>'market' ORDER BY party", order2); !slices.Equal(s, []string{"buyer/0/2", "vendor/0/2"}) {
		t.Fatalf("after rotation, second escrow suffixes %v", s)
	}
}

// A-314 (QA29-1): once a key has escrows, under any origin, its origin cannot be changed. The same key re-registered
// with another fingerprint or path is refused with 409 and nothing changes, for the buyer's key and the vendor's
// (whose key serves every buyer of its listings); practice is not offered again; the next checkout still works on
// the recorded origin. The same key is recognised by its HMAC, and after an APP_SECRET_KEY rotation (the HMAC no
// longer matches) by Core deriving the recorded child key, never by the origin.
func TestEscrowOriginLockedOnceKeyHasEscrows(t *testing.T) {
	x := newEscrowEnv(t)
	x.mustCheckout(x.order, "bcrt1qbuyerrefund00000000000000000000001")
	keyRows := func() string {
		return strings.Join(agStrings(t, x.testEnv, "SELECT k::text FROM user_escrow_keys k WHERE user_id IN ($1,$2) ORDER BY user_id", x.buyer, x.vendor), "\n")
	}
	before := keyRows()
	moved := func(expr, origin string) string {
		k, err := parseEscrowKey(expr)
		if err != nil {
			t.Fatal(err)
		}
		return "[" + origin + "]" + k.XPub
	}
	refused := func(name, sess, expr string) {
		t.Helper()
		calls := len(x.core.methods())
		code, body := registerKey(x.testEnv, sess, expr)
		if code != 409 || !strings.Contains(body, "This key has escrows, so its origin (fingerprint and path) cannot be changed") ||
			!strings.Contains(body, "keep the origin they were created with") || !strings.Contains(body, "contact the market") {
			t.Fatalf("%s: %d %s", name, code, body)
		}
		if got := keyRows(); got != before {
			t.Fatalf("%s: key rows changed:\n%s\n%s", name, before, got)
		}
		if name != "after rotation" && len(x.core.methods()) != calls {
			t.Fatalf("%s: node called although the HMAC matched: %v", name, x.core.methods()[calls:])
		}
		if body := x.page("/escrow-key", sess); strings.Contains(body, "Show the practice transaction") || !strings.Contains(body, "Its origin (fingerprint and path) can no longer be changed.") {
			t.Fatalf("%s: practice offered for a key with escrows, or no word on the origin: %s", name, body)
		}
	}
	for _, c := range []struct{ name, sess, key, origin string }{
		{"buyer, another fingerprint", x.buyerSess, x.buyerKey, "0000beef/48h/1h/0h/2h"},
		{"buyer, another path", x.buyerSess, x.buyerKey, "7450f935/48h/1h/5h/2h"},
		{"vendor, another fingerprint", x.vendorSess, x.vendorKey, "0000beef/48h/1h/0h/2h"},
		{"vendor, another path", x.vendorSess, x.vendorKey, "5eed5eed/48h/1h/7h/2h"},
	} {
		refused(c.name, c.sess, moved(c.key, c.origin))
	}
	// Checkout still works on the recorded origins, for this buyer and for another buyer of the vendor's listings.
	order2 := x.draft()
	x.mustCheckout(order2, "bcrt1qbuyerrefund00000000000000000000002")
	if s := agStrings(t, x.testEnv, "SELECT party||' '||origin_fingerprint||derivation_suffix FROM escrow_pubkeys p JOIN escrows e ON e.id=p.escrow_id WHERE e.order_id=$1 AND party<>'market' ORDER BY party", order2); !slices.Equal(s, []string{"buyer 7450f935/0/2", "vendor 5eed5eed/0/2"}) {
		t.Fatalf("second escrow %v", s)
	}
	b2, sess2 := x.user("ebuyer2_"+randomToken()[:6], "buyer")
	x.activeKey(b2, altEscrowKey(t, testBIP48Key, "0dd0dd0d", 3))
	o3 := x.testEnv.order(b2, x.autoListing(x.vendor), "BTC", stateDraft)
	if w := x.do("POST", "/orders/escrow", sess2, url.Values{"order_id": {o3}, "refund_address": {"bcrt1qbuyerrefund00000000000000000000003"}}); w.Code != 303 {
		t.Fatalf("another buyer of the vendor: %d %s", w.Code, w.Body.String())
	}
	// A key row moved to another origin by hand (as a database written before A-314 could hold it) still counts its
	// escrows, by the key's HMAC: practice is neither offered nor accepted for it.
	x.DB.Exec("UPDATE user_escrow_keys SET origin_fingerprint='0000beef',active_at=NULL WHERE user_id=$1", x.buyer)
	if body := x.page("/escrow-key", x.buyerSess); !strings.Contains(body, "This key is used by escrows") || strings.Contains(body, "Show the practice transaction") {
		t.Fatalf("moved key row: %s", body)
	}
	if code, body := submitPractice(x.testEnv, x.buyerSess, "cHNidP8BAA=="); code != 409 || !strings.Contains(body, "no escrows") {
		t.Fatalf("practice for a moved key row: %d %s", code, body)
	}
	x.activeKey(x.buyer, x.buyerKey)
	// After an APP_SECRET_KEY rotation the stored HMACs no longer match; Core's derivation still recognises the key.
	t.Setenv("APP_SECRET_KEY", rotatedSecretKey())
	x.restart()
	before = keyRows()
	refused("after rotation", x.buyerSess, moved(x.buyerKey, "0000beef/48h/1h/0h/2h"))
	// At its recorded origin the same key is restored, active at once.
	if code, body := registerKey(x.testEnv, x.buyerSess, x.buyerKey); code != 303 {
		t.Fatalf("restore at the recorded origin: %d %s", code, body)
	}
	if agInt(x.testEnv, "SELECT count(*) FROM user_escrow_keys WHERE user_id=$1 AND active_at IS NOT NULL AND origin_fingerprint='7450f935'", x.buyer) != 1 {
		t.Fatal("not restored")
	}
}

// A-314: an escrow committed for the key between the pre-check and the key change (a checkout racing an origin
// correction) is caught inside the key change's transaction: the origin is not changed.
func TestEscrowOriginLockRecheckedInTransaction(t *testing.T) {
	x := newEscrowEnv(t)
	// The account's escrows so far hold another key (so the pre-check asks the node about it).
	x.activeKey(x.buyer, altEscrowKey(t, testBIP48Key, "0badf00d", 7))
	x.mustCheckout(x.order, "bcrt1qbuyerrefund00000000000000000000001")
	x.activeKey(x.buyer, x.buyerKey)
	bk, _ := parseEscrowKey(x.buyerKey)
	var once sync.Once
	x.core.beforeDerive = func() {
		once.Do(func() { // a checkout with the current key commits while the node is asked
			order := x.testEnv.order(x.buyer, x.autoListing(x.vendor), "BTC", stateAwaitingPayment)
			var id int64
			if err := x.DB.QueryRow("INSERT INTO escrows(order_id,address,market_index,refund_address,fee_bps,fee_destination) VALUES($1,'bcrt1qracedescrow000000000',nextval('escrow_market_index'),'bcrt1qracedrefund000000000',0,'') RETURNING id", order).Scan(&id); err != nil {
				t.Error(err)
				return
			}
			if _, err := x.DB.Exec(`INSERT INTO escrow_pubkeys(pubkey,escrow_id,party,user_id,key_hmac,origin_fingerprint,origin_path,derivation_suffix) VALUES
				($1,$2,'buyer',$3,$4,'7450f935','/48h/1h/0h/2h','/0/2')`, childHex(x.buyerKey, "/0/2"), id, x.buyer, x.A.escrowKeyHMAC(bk)); err != nil {
				t.Error(err)
			}
		})
	}
	before := strings.Join(agStrings(t, x.testEnv, "SELECT k::text FROM user_escrow_keys k WHERE user_id=$1", x.buyer), "")
	code, body := registerKey(x.testEnv, x.buyerSess, "[0000beef"+bk.PathText+"]"+bk.XPub)
	if code != 409 || !strings.Contains(body, "This key has escrows, so its origin") {
		t.Fatalf("origin change racing a checkout: %d %s", code, body)
	}
	if after := strings.Join(agStrings(t, x.testEnv, "SELECT k::text FROM user_escrow_keys k WHERE user_id=$1", x.buyer), ""); after != before {
		t.Fatalf("key row changed:\n%s\n%s", before, after)
	}
}

// A-314 (i) and (iv): a party's next child index is per account, one more than the highest any of the account's
// escrows used under any origin, so a key changed to another origin (or a key found under two origins) never reuses
// a child key; and a child key another escrow already holds is refused with a truthful 409 and a fixed log line.
func TestEscrowChildIndexPerAccount(t *testing.T) {
	logs := captureLog(t)
	x := newEscrowEnv(t)
	x.mustCheckout(x.order, "bcrt1qbuyerrefund00000000000000000000001")
	suffixes := func(order string) []string {
		return agStrings(t, x.testEnv, "SELECT party||' '||origin_fingerprint||derivation_suffix FROM escrow_pubkeys p JOIN escrows e ON e.id=p.escrow_id WHERE e.order_id=$1 AND party<>'market' ORDER BY party", order)
	}
	// Another key under another origin: it continues after the account's highest index.
	x.activeKey(x.buyer, altEscrowKey(t, testBIP48Key, "0badf00d", 7))
	o2 := x.draft()
	x.mustCheckout(o2, "bcrt1qbuyerrefund00000000000000000000002")
	if s := suffixes(o2); !slices.Equal(s, []string{"buyer 0badf00d/0/2", "vendor 5eed5eed/0/2"}) {
		t.Fatalf("new key's escrow %v", s)
	}
	// The same key found under two origins (as a database written before A-314 could hold it): the counter spans
	// both, so the child key is fresh and the checkout succeeds instead of answering 503.
	x.activeKey(x.buyer, x.buyerKey)
	x.DB.Exec("UPDATE escrow_pubkeys SET origin_fingerprint='0000beef' WHERE user_id=$1 AND origin_fingerprint='7450f935'", x.buyer)
	o3 := x.draft()
	x.mustCheckout(o3, "bcrt1qbuyerrefund00000000000000000000003")
	if s := suffixes(o3); !slices.Equal(s, []string{"buyer 7450f935/0/3", "vendor 5eed5eed/0/3"}) {
		t.Fatalf("two-origin escrow %v", s)
	}
	// A child key already held by another escrow: 409 with a truthful message, nothing written, a fixed log line.
	taken := childHex(x.buyerKey, "/0/4")
	other := x.testEnv.order(x.vendor, x.testEnv.product(x.buyer, "digital"), "BTC", stateAwaitingPayment)
	var id int64
	if err := x.DB.QueryRow("INSERT INTO escrows(order_id,address,market_index,refund_address,fee_bps,fee_destination) VALUES($1,'bcrt1qtakenescrow0000000000',nextval('escrow_market_index'),'bcrt1qtakenrefund0000000000',0,'') RETURNING id", other).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := x.DB.Exec("INSERT INTO escrow_pubkeys(pubkey,escrow_id,party,user_id,key_hmac,origin_fingerprint,origin_path,derivation_suffix) VALUES($1,$2,'buyer',$3,repeat('e',64),'0c0c0c0c','/48h/1h/0h/2h','/0/1')", taken, id, x.vendor); err != nil {
		t.Fatal(err)
	}
	x.DB.Exec("UPDATE orders SET state='completed' WHERE buyer_id=$1 AND state='awaiting_payment'", x.buyer) // under the awaiting-payment cap
	o4 := x.draft()
	w := x.checkout(o4, "bcrt1qbuyerrefund00000000000000000000004")
	if w.Code != 409 || !strings.Contains(w.Body.String(), "already holds one of its keys") || strings.Contains(w.Body.String(), "did not confirm") {
		t.Fatalf("taken child key: %d %s", w.Code, w.Body.String())
	}
	x.nothingWritten(o4)
	line := "escrow: escrow for order " + o4[:8] + " not built: a child key is already held by another escrow"
	if !strings.Contains(logs.String(), line) || strings.Contains(logs.String(), taken) || strings.Contains(logs.String(), "23505") || strings.Contains(logs.String(), "duplicate key") {
		t.Fatalf("log:\n%s", logs.String())
	}
}

// N11 (A-317): checkout locks both parties' key rows FOR UPDATE, so two buyers checking out on one vendor at the same
// time take the vendor's child keys in turn (/0/1, /0/2), never the same one. Each build waits (up to a second) for
// the other to reach its build too: with the rows locked only one ever does.
func TestEscrowConcurrentBuyersShareVendor(t *testing.T) {
	x := newEscrowEnv(t)
	b2, sess2 := x.user("ebuyer2_"+randomToken()[:6], "buyer")
	x.activeKey(b2, altEscrowKey(t, testBIP48Key, "0dd0dd0d", 3))
	o2 := x.testEnv.order(b2, x.autoListing(x.vendor), "BTC", stateDraft)
	var building atomic.Int32
	x.core.beforeCreate = func() {
		building.Add(1)
		for end := time.Now().Add(time.Second); building.Load() < 2 && time.Now().Before(end); {
			time.Sleep(10 * time.Millisecond)
		}
	}
	var wg sync.WaitGroup
	var w1, w2 *httptest.ResponseRecorder
	wg.Add(2)
	go func() { defer wg.Done(); w1 = x.checkout(x.order, "bcrt1qbuyerrefund00000000000000000000001") }()
	go func() {
		defer wg.Done()
		w2 = x.do("POST", "/orders/escrow", sess2, url.Values{"order_id": {o2}, "refund_address": {"bcrt1qbuyerrefund00000000000000000000002"}})
	}()
	wg.Wait()
	if w1.Code != 303 || w2.Code != 303 {
		t.Fatalf("concurrent checkouts on one vendor: %d %s / %d %s", w1.Code, w1.Body.String(), w2.Code, w2.Body.String())
	}
	if s := agStrings(t, x.testEnv, "SELECT derivation_suffix FROM escrow_pubkeys WHERE party='vendor' ORDER BY derivation_suffix"); !slices.Equal(s, []string{"/0/1", "/0/2"}) {
		t.Fatalf("vendor child keys %v", s)
	}
}

// N20 (A-317): escrow checkout keeps the buyer's cap of 3 orders awaiting payment: a fourth is refused before any
// RPC, and with one fewer the checkout goes through.
func TestEscrowCheckoutAwaitingCap(t *testing.T) {
	x := newEscrowEnv(t)
	var awaiting []string
	for range maxAwaitingPayment {
		awaiting = append(awaiting, x.testEnv.order(x.buyer, x.autoListing(x.vendor), "BTC", stateAwaitingPayment))
	}
	calls := len(x.core.methods())
	w := x.checkout(x.order, "bcrt1qbuyerrefund00000000000000000000001")
	if w.Code != 409 || !strings.Contains(w.Body.String(), "You already have 3 orders awaiting payment") {
		t.Fatalf("fourth awaiting order: %d %s", w.Code, w.Body.String())
	}
	x.nothingWritten(x.order)
	if len(x.core.methods()) != calls {
		t.Fatalf("node called: %v", x.core.methods()[calls:])
	}
	x.DB.Exec("UPDATE orders SET state='cancelled' WHERE id=$1", awaiting[0])
	x.mustCheckout(x.order, "bcrt1qbuyerrefund00000000000000000000001")
}

// N18b (A-317): a listing priced below the minimum automatic payout cannot be bought with escrow (it could never be
// settled); at the minimum it can.
func TestEscrowCheckoutPriceFloor(t *testing.T) {
	x := newEscrowEnv(t)
	floor := payoutFloor("BTC", "release")
	x.DB.Exec("UPDATE products SET btc=$2 WHERE id=$1", x.product, floor-1)
	calls := len(x.core.methods())
	w := x.checkout(x.order, "bcrt1qbuyerrefund00000000000000000000001")
	if w.Code != 400 || !strings.Contains(w.Body.String(), "below the minimum automatic payout") {
		t.Fatalf("sub-floor listing: %d %s", w.Code, w.Body.String())
	}
	x.nothingWritten(x.order)
	if len(x.core.methods()) != calls {
		t.Fatalf("node called: %v", x.core.methods()[calls:])
	}
	x.DB.Exec("UPDATE products SET btc=$2 WHERE id=$1", x.product, floor)
	x.mustCheckout(x.order, "bcrt1qbuyerrefund00000000000000000000001")
	if got := agInt(x.testEnv, "SELECT amount FROM orders WHERE id=$1", x.order); int64(got) != floor {
		t.Fatalf("amount %d", got)
	}
}

// N24 (A-317): the escrow checkout offer on an eligible draft says why escrow cannot be used, and offers no form,
// when the vendor's key is inactive or missing, when the buyer's is (with a link to the key page), and when escrow
// is down; with both keys active it offers the form.
func TestEscrowCheckoutOfferCopy(t *testing.T) {
	x := newEscrowEnv(t)
	offered := func(name string, form bool, want string) {
		t.Helper()
		body := x.page("/order?id="+x.order, x.buyerSess)
		if strings.Contains(body, `action="/orders/escrow"`) != form || !strings.Contains(body, want) {
			t.Fatalf("%s: form=%v, want %q:\n%s", name, form, want, body)
		}
	}
	offered("both active", true, "Or pay into a Bitcoin escrow")
	x.DB.Exec("UPDATE user_escrow_keys SET active_at=NULL WHERE user_id=$1", x.vendor)
	offered("vendor inactive", false, "<strong>Unavailable.</strong> The vendor has no active Bitcoin escrow key, so this order cannot use escrow.")
	x.DB.Exec("DELETE FROM user_escrow_keys WHERE user_id=$1", x.vendor)
	offered("vendor without a key", false, "<strong>Unavailable.</strong> The vendor has no active Bitcoin escrow key, so this order cannot use escrow.")
	x.activeKey(x.vendor, x.vendorKey)
	x.DB.Exec("UPDATE user_escrow_keys SET active_at=NULL WHERE user_id=$1", x.buyer)
	offered("buyer inactive", false, `Activate your Bitcoin escrow key first: register it and sign its practice transaction. <a class="text-link" href="/escrow-key">Your escrow key</a>`)
	x.activeKey(x.buyer, x.buyerKey)
	up := x.A.escrow.Load()
	x.A.escrow.Store(nil)
	offered("escrow down", false, "Bitcoin escrow is unavailable on this market right now")
	x.A.escrow.Store(up)
	offered("both active again", true, `name="refund_address"`)
}
