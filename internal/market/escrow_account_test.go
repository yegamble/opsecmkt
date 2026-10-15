package market

import (
	"context"

	"html"

	"net/url"
	"regexp"

	"strings"
	"testing"
)

func setEscrowEnv(t *testing.T, rpcURL string) {
	t.Setenv("ESCROW_BTC", "on")
	t.Setenv("ESCROW_BTC_RPC_URL", rpcURL)
	t.Setenv("ESCROW_BTC_ARBITER_WALLET", "")
	t.Setenv("ESCROW_BTC_WATCH_WALLET", "")
	t.Setenv("ESCROW_BTC_FEE_BPS", "")
	t.Setenv("ESCROW_BTC_FEE_ADDRESS", "")
	t.Setenv("ESCROW_SIGNER_SOCKET", "")
	t.Setenv("PAYMENT_CONFIRMATIONS_BTC", "")
	t.Setenv("PAYMENT_EXPIRY", "")
	t.Setenv("BITCOIN_MAX_TIP_AGE", "")
	t.Setenv("BITCOIN_RPC_URL", "")
	t.Setenv("BITCOIN_CHAIN", "")
	t.Setenv("MONERO_NETWORK", "")
	t.Setenv("MONERO_WALLET_RPC_URL", "")
}

// escrowTestApp starts the test app with escrow on against a fake regtest node; env is extra name, value pairs set
// after setEscrowEnv's (ESCROW_SIGNER_SOCKET, say).
func escrowTestApp(t *testing.T, env ...string) (*testEnv, *fakeEscrowCore, *payRPCServer) {
	t.Helper()
	core, srv := newFakeEscrowCore(t)
	setEscrowEnv(t, srv.url(""))
	for i := 0; i+1 < len(env); i += 2 {
		t.Setenv(env[i], env[i+1])
	}
	e := newTestApp(t)
	if e.A.escrow.Load() == nil {
		t.Fatal("escrow did not start against the fake regtest node")
	}
	return e, core, srv
}

// startApp is newTestApp's New without failing the test, for startup refusals. schema is the DSN to reuse ("" = a
// fresh schema).
func startApp(t *testing.T, dsn string) (*App, string, error) {
	t.Helper()
	if dsn == "" {
		dsn = testSchemaDSN(t)
	}
	t.Setenv("DATABASE_URL", dsn)
	t.Setenv("SETUP_TOKEN", testSetupToken)
	t.Setenv("APP_SECRET_KEY", "")
	t.Setenv("COMPOSE_FILE", "")
	t.Setenv("COOKIE_SECURE", "false")
	a, err := New(context.Background(), false)
	if a != nil {
		t.Cleanup(a.Close)
	}
	return a, dsn, err
}

func registerKey(e *testEnv, sess, key string, extra ...string) (int, string) {
	f := url.Values{"key": {key}, "password": {testPassword}}
	for i := 0; i+1 < len(extra); i += 2 {
		f.Set(extra[i], extra[i+1])
	}
	w := e.do("POST", "/escrow-key", sess, f)
	return w.Code, w.Body.String()
}

func agStrings(t *testing.T, e *testEnv, q string, args ...any) []string {
	t.Helper()
	rows, err := e.DB.Query(q, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// insertEscrowFor hand-inserts an order with an escrow whose buyer key is the user's current key (P1-b builds these;
// none exist in P1-a) and returns the escrow_pubkeys rows as text.
func insertEscrowFor(t *testing.T, e *testEnv, buyerID string) string {
	t.Helper()
	vendorID, _ := e.user("escrow_fixture_vendor_"+randomToken()[:6], "vendor")
	order := e.order(buyerID, e.product(vendorID, "digital"), "BTC", "awaiting_payment")
	var h, fp, path string
	if err := e.DB.QueryRow("SELECT xpub_hmac,origin_fingerprint,origin_path FROM user_escrow_keys WHERE user_id=$1", buyerID).Scan(&h, &fp, &path); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := e.DB.QueryRow("INSERT INTO escrows(order_id,address,market_index,refund_address,fee_bps,fee_destination) VALUES($1,$2,nextval('escrow_market_index'),$3,0,'') RETURNING id",
		order, "bcrt1qfixture"+randomToken()[:20], "bcrt1qrefund"+randomToken()[:20]).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec(`INSERT INTO escrow_pubkeys(pubkey,escrow_id,party,user_id,key_hmac,origin_fingerprint,origin_path,derivation_suffix) VALUES
		('02'||repeat('ab',32),$1,'buyer',$2,$3,$4,$5,'/0/1'),('03'||repeat('cd',32),$1,'market',NULL,NULL,$6,'/84h/1h/0h','/0/0')`, id, buyerID, h, fp, path, fakeArbiterFP); err != nil {
		t.Fatal(err)
	}
	return strings.Join(agStrings(t, e, "SELECT p::text FROM escrow_pubkeys p ORDER BY pubkey"), "\n")
}

var practiceArea = regexp.MustCompile(`<textarea class="escrow-psbt" readonly[^>]*>([^<]+)</textarea>`)

func practicePSBT(t *testing.T, e *testEnv, sess string) string {
	t.Helper()
	w := e.do("GET", "/escrow-key?practice=1", sess, nil)
	m := practiceArea.FindStringSubmatch(w.Body.String())
	if w.Code != 200 || m == nil {
		t.Fatalf("no practice PSBT: %d %s", w.Code, w.Body.String())
	}
	return html.UnescapeString(m[1])
}

func submitPractice(e *testEnv, sess, psbt string) (int, string) {
	w := e.do("POST", "/escrow-key/practice", sess, url.Values{"psbt": {psbt}})
	return w.Code, html.UnescapeString(w.Body.String())
}
