package market

import (
	"crypto/sha256"
	"encoding/hex"

	"net/http/httptest"
	"net/url"

	"strings"

	"testing"
)

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

func newEscrowEnv(t *testing.T, env ...string) *escrowEnv {
	t.Helper()
	e, core, srv := escrowTestApp(t, env...)
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
	k, err := parseEscrowKey(expr, keyNetTest)
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
	k, err := parseEscrowKey(key, keyNetTest)
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

// rotatedSecretKey provides a deterministic foreign key root for unreadable-row fixtures: 64 hex digits derived from
// a fixed phrase, so the source holds no key-shaped literal for secret scanners to flag (A-370).
func rotatedSecretKey() string {
	sum := sha256.Sum256([]byte("escrow test: rotated APP_SECRET_KEY"))
	return hex.EncodeToString(sum[:])
}

// makePartyEscrowKeyUnreadable simulates a party-key row sealed and HMACed under a different historical key root,
// without rotating this app's root or changing the immutable arbiter/signer pins.
func makePartyEscrowKeyUnreadable(t *testing.T, x *escrowEnv, uid, expression string) {
	t.Helper()
	key, err := parseEscrowKey(expression, keyNetTest)
	if err != nil {
		t.Fatal(err)
	}
	foreignRoot := &App{keyRoot: rotatedSecretKey()}
	sealed, err := foreignRoot.seal("escrow-xpub", key.XPub)
	if err != nil {
		t.Fatal(err)
	}
	foreignHMAC := foreignRoot.escrowKeyHMAC(key)
	if foreignHMAC == x.A.escrowKeyHMAC(key) {
		t.Fatal("foreign party-key fixture unexpectedly uses the running key root")
	}
	if _, err := x.DB.Exec("UPDATE user_escrow_keys SET xpub_sealed=$2,xpub_hmac=$3 WHERE user_id=$1", uid, sealed, foreignHMAC); err != nil {
		t.Fatal(err)
	}
	// A real key-root change also makes each stored escrow-party HMAC differ from the one recomputed by this app.
	if _, err := x.DB.Exec("UPDATE escrow_pubkeys SET key_hmac=$2 WHERE user_id=$1 AND party IN ('buyer','vendor')", uid, foreignHMAC); err != nil {
		t.Fatal(err)
	}
}
