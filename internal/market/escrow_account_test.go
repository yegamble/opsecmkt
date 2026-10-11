package market

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"html"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Escrow Phase 1, slice P1-a: DB integration tests with the simulated Core of escrow_fake_test.go (simulated RPC,
// not chain evidence; scripts/test-escrow-regtest.sh runs the same journey against a real regtest node).

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

// ESCROW_BTC is off by default: no RPC, no account row, and /escrow-key and its actions answer exactly as an unknown
// page or action does, signed in or not, so nobody can tell that the route exists (QA-P1a-4, the chair's ruling).
func TestEscrowOffByDefault(t *testing.T) {
	core, srv := newFakeEscrowCore(t)
	t.Setenv("ESCROW_BTC", "")
	t.Setenv("ESCROW_BTC_RPC_URL", srv.url(""))
	e := newTestApp(t)
	if e.A.escrowConfigured || e.A.escrow.Load() != nil || len(core.methods()) != 0 {
		t.Fatalf("escrow started or called the node: %v", core.methods())
	}
	uid, sess := e.user("escrow_off_buyer", "buyer")
	answer := func(w *httptest.ResponseRecorder) string {
		return fmt.Sprintf("%d %q %q %q", w.Code, w.Header().Get("Location"), w.Header().Get("Content-Type"), w.Body.String())
	}
	for _, who := range []struct{ name, sess string }{{"signed in", sess}, {"anonymous", randomToken()}} {
		unknownPage := answer(e.do("GET", "/no-such-page", who.sess, nil))
		unknownAction := answer(e.do("POST", "/no-such-action", who.sess, url.Values{"key": {testBIP48Key}, "password": {testPassword}}))
		if !strings.HasPrefix(unknownPage, "404 ") || !strings.HasPrefix(unknownAction, "404 ") {
			t.Fatalf("%s: unknown page %s, unknown action %s", who.name, unknownPage, unknownAction)
		}
		for _, path := range []string{"/escrow-key", "/escrow-key?practice=1"} {
			if got := answer(e.do("GET", path, who.sess, nil)); got != unknownPage {
				t.Errorf("%s GET %s: %s, an unknown page: %s", who.name, path, got, unknownPage)
			}
		}
		for _, path := range []string{"/escrow-key", "/escrow-key/practice"} {
			if got := answer(e.do("POST", path, who.sess, url.Values{"key": {testBIP48Key}, "password": {testPassword}})); got != unknownAction {
				t.Errorf("%s POST %s: %s, an unknown action: %s", who.name, path, got, unknownAction)
			}
		}
		// Nor does the sign-in page offer to return to it.
		login, control := e.do("GET", "/login?next=%2Fescrow-key", who.sess, nil), e.do("GET", "/login?next=%2Fno-such-page", who.sess, nil)
		if login.Body.String() != control.Body.String() || strings.Contains(login.Body.String(), "escrow-key") || strings.Contains(login.Body.String(), "Escrow Key") {
			t.Errorf("%s: /login?next=/escrow-key differs from an unknown next: %d", who.name, login.Code)
		}
	}
	if body := e.do("GET", "/account", sess, nil).Body.String(); strings.Contains(body, "Bitcoin escrow key") || strings.Contains(body, "/escrow-key") {
		t.Fatal("/account shows the escrow row with ESCROW_BTC off")
	}
	if n := agInt(e, "SELECT count(*) FROM user_escrow_keys WHERE user_id=$1", uid); n != 0 {
		t.Fatalf("%d key rows", n)
	}
	if len(core.methods()) != 0 {
		t.Fatalf("the node was called: %v", core.methods())
	}
}

// An unreachable node or an unreadable arbiter wallet leaves escrow disabled with a fixed line; the market starts,
// the page says escrow is unavailable and the actions refuse.
func TestEscrowStartupDisabled(t *testing.T) {
	logs := captureLog(t)
	core, srv := newFakeEscrowCore(t)
	srv.down.Store(true)
	setEscrowEnv(t, srv.url(""))
	a, dsn, err := startApp(t, "")
	if err != nil || a.escrow.Load() != nil || !a.escrowConfigured {
		t.Fatalf("unreachable: %v %v", err, a)
	}
	if !strings.Contains(logs.String(), escrowLogUnchecked+" (getblockchaininfo unreachable)") {
		t.Fatalf("log: %s", logs.String())
	}
	a.Close()
	srv.down.Store(false)
	core.arbiterCode = -4
	a, _, err = startApp(t, dsn)
	if err != nil || a.escrow.Load() != nil || !strings.Contains(logs.String(), escrowLogArbiter+" (getrawchangeaddress rpc:-4)") {
		t.Fatalf("arbiter: %v\n%s", err, logs.String())
	}
	e := &testEnv{t: t, A: a, DB: a.db}
	_, sess := e.user("escrow_down_buyer", "buyer")
	if body := e.do("GET", "/escrow-key", sess, nil).Body.String(); !strings.Contains(body, "Bitcoin escrow unavailable") || strings.Contains(body, `action="/escrow-key"`) {
		t.Fatalf("page: %s", body)
	}
	if body := e.do("GET", "/account", sess, nil).Body.String(); !strings.Contains(body, "Bitcoin escrow key") || !strings.Contains(body, "Unavailable") {
		t.Fatal("/account row")
	}
	if code, _ := registerKey(e, sess, testBIP48Key); code != 409 {
		t.Fatalf("register while down: %d", code)
	}
	if strings.Contains(logs.String(), "has no available keys") || strings.Contains(logs.String(), "127.0.0.1") {
		t.Fatalf("log carries node text: %s", logs.String())
	}
	// A key registered while escrow was up, then a start with the node down: the page, practice request included,
	// says escrow is unavailable and builds nothing (QA25-3, A-309: practice is offered only while escrow is up).
	a.Close()
	core.mu.Lock()
	core.arbiterCode = 0
	core.mu.Unlock()
	if a, _, err = startApp(t, dsn); err != nil || a.escrow.Load() == nil {
		t.Fatalf("escrow up: %v", err)
	}
	e = &testEnv{t: t, A: a, DB: a.db}
	uid, sess2 := e.user("escrow_down_registered", "buyer")
	if code, body := registerKey(e, sess2, testBIP48Key); code != 303 {
		t.Fatalf("register while up: %d %s", code, body)
	}
	a.Close()
	srv.down.Store(true)
	if a, _, err = startApp(t, dsn); err != nil || a.escrow.Load() != nil {
		t.Fatalf("node down: %v", err)
	}
	e = &testEnv{t: t, A: a, DB: a.db}
	calls := len(core.methods())
	w := e.do("GET", "/escrow-key?practice=1", sess2, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Bitcoin escrow unavailable") || strings.Contains(w.Body.String(), "escrow-psbt") || len(core.methods()) != calls {
		t.Fatalf("practice while down: %d %s", w.Code, w.Body.String())
	}
	if agInt(e, "SELECT count(*) FROM user_escrow_keys WHERE user_id=$1 AND active_at IS NULL", uid) != 1 {
		t.Fatal("key row")
	}
}

// The arbiter fingerprint is pinned at the first start; a different one later disables escrow with a fixed line
// that names no fingerprint.
func TestEscrowArbiterFingerprintPinned(t *testing.T) {
	logs := captureLog(t)
	core, srv := newFakeEscrowCore(t)
	setEscrowEnv(t, srv.url(""))
	a, dsn, err := startApp(t, "")
	if err != nil || a.escrow.Load() == nil || a.escrow.Load().fingerprint != fakeArbiterFP {
		t.Fatalf("first start: %v", err)
	}
	var pinned string
	a.db.QueryRow("SELECT value FROM settings WHERE key=$1", escrowFingerprintPin).Scan(&pinned)
	if pinned != fakeArbiterFP || !strings.Contains(logs.String(), escrowLogOn) {
		t.Fatalf("pin %q", pinned)
	}
	a.Close()
	// Same wallet again: still on.
	if a, _, err = startApp(t, dsn); err != nil || a.escrow.Load() == nil {
		t.Fatalf("restart: %v", err)
	}
	a.Close()
	// Another wallet (another seed) under the same name: disabled, pin unchanged.
	core.arbiterFP = "0badf00d"
	core.arbiterDesc = "wpkh([0badf00d/84h/1h/0h]" + testBIP84Key[20:] + "/1/*)#x"
	a, _, err = startApp(t, dsn)
	if err != nil || a.escrow.Load() != nil || !strings.Contains(logs.String(), escrowLogFingerprint) {
		t.Fatalf("mismatch: %v\n%s", err, logs.String())
	}
	a.db.QueryRow("SELECT value FROM settings WHERE key=$1", escrowFingerprintPin).Scan(&pinned)
	if pinned != fakeArbiterFP {
		t.Fatalf("pin changed to %q", pinned)
	}
	if strings.Contains(logs.String(), "0badf00d") || strings.Contains(logs.String(), fakeArbiterFP) || strings.Contains(logs.String(), "tpub") {
		t.Fatalf("log names a fingerprint or key: %s", logs.String())
	}
}

// Registration (ES-3): sealed key, HMAC, origin, inactive, a notification and a fixed audit line; none of them holds
// the key, its fingerprint or path.
func TestEscrowKeyRegister(t *testing.T) {
	e, _, _ := escrowTestApp(t)
	uid, sess := e.user("escrow_reg_buyer", "buyer")
	if body := e.do("GET", "/escrow-key", sess, nil).Body.String(); !strings.Contains(body, "No key registered") || !strings.Contains(body, `action="/escrow-key"`) {
		t.Fatalf("empty page: %s", body)
	}
	if code, body := registerKey(e, sess, testBIP48Key); code != 303 {
		t.Fatalf("register: %d %s", code, body)
	}
	var sealed, h, fp, path string
	var active bool
	if err := e.DB.QueryRow("SELECT xpub_sealed,xpub_hmac,origin_fingerprint,origin_path,active_at IS NOT NULL FROM user_escrow_keys WHERE user_id=$1", uid).Scan(&sealed, &h, &fp, &path, &active); err != nil {
		t.Fatal(err)
	}
	k, _ := parseEscrowKey(testBIP48Key)
	if fp != "7450f935" || path != "/48h/1h/0h/2h" || active || h != e.A.escrowKeyHMAC(k) {
		t.Fatalf("row %s %s %v %s", fp, path, active, h)
	}
	if opened, err := e.A.open("escrow-xpub", sealed); err != nil || opened != k.XPub {
		t.Fatalf("sealed value: %v", err)
	}
	assertNowhereInDB(t, e, k.XPub[4:40])
	audit := agStrings(t, e, "SELECT action FROM audit_events WHERE user_id=$1 ORDER BY id", uid)
	if !slices.Equal(audit, []string{"Registered a Bitcoin escrow key (confirmed with password)"}) {
		t.Fatalf("audit %q", audit)
	}
	notes := agStrings(t, e, "SELECT body FROM notifications WHERE user_id=$1", uid)
	if len(notes) != 1 || !strings.Contains(notes[0], "Bitcoin escrow key was registered") {
		t.Fatalf("notifications %q", notes)
	}
	for _, text := range append(audit, notes...) {
		if strings.Contains(text, "7450f935") || strings.Contains(text, "48h") || strings.Contains(text, "tpub") {
			t.Fatalf("text carries key data: %q", text)
		}
	}
	body := e.do("GET", "/escrow-key", sess, nil).Body.String()
	if !strings.Contains(body, "Inactive: sign the practice transaction below") || !strings.Contains(body, "[7450f935/48h/1h/0h/2h]") ||
		!strings.Contains(body, "Show the practice transaction") || strings.Contains(body, k.XPub) {
		t.Fatalf("registered page: %s", body)
	}
	if body := e.do("GET", "/account", sess, nil).Body.String(); !strings.Contains(body, "Bitcoin escrow key") || !strings.Contains(body, "Inactive") {
		t.Fatal("/account row")
	}
	// The same key again changes nothing and asks for no password.
	w := e.do("POST", "/escrow-key", sess, url.Values{"key": {testBIP48Key + "/0/*"}})
	if w.Code != 303 || agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1", uid) != 1 {
		t.Fatalf("unchanged: %d", w.Code)
	}
	// The same key with a corrected fingerprint or path is a change, not "unchanged": it needs the password, updates
	// the origin and is audited (QA25-3, A-309).
	for _, in := range []string{"[0000beef/48h/1h/0h/2h]" + k.XPub, "[0000beef/48h/1h/5h/2h]" + k.XPub} {
		if code, body := registerKey(e, sess, in); code != 303 {
			t.Fatalf("%q: %d %s", in, code, body)
		}
		var origin string
		e.DB.QueryRow("SELECT '['||origin_fingerprint||origin_path||']' FROM user_escrow_keys WHERE user_id=$1", uid).Scan(&origin)
		if origin != in[:strings.IndexByte(in, ']')+1] {
			t.Fatalf("origin %q after %q", origin, in)
		}
		if audit := agStrings(t, e, "SELECT action FROM audit_events WHERE user_id=$1 ORDER BY id DESC LIMIT 1", uid); audit[0] != "Changed Bitcoin escrow key (confirmed with password)" {
			t.Fatalf("audit %q", audit)
		}
		// Without escrows the corrected key is offered practice as before (A-314 (iii)).
		if body := e.do("GET", "/escrow-key", sess, nil).Body.String(); !strings.Contains(body, "Show the practice transaction") {
			t.Fatalf("practice not offered after an origin correction: %s", body)
		}
	}
	if n := agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1", uid); n != 3 {
		t.Fatalf("%d audit lines", n)
	}
	// A wrong password changes nothing (a fresh account: failures spend its confirmation budget).
	uid2, sess2 := e.user("escrow_reg_wrongpw", "vendor")
	if w := e.do("POST", "/escrow-key", sess2, url.Values{"key": {testBIP84Key}, "dedicated": {"1"}, "password": {"not-the-password"}}); w.Code != 401 {
		t.Fatalf("wrong password: %d", w.Code)
	}
	if agInt(e, "SELECT count(*) FROM user_escrow_keys WHERE user_id=$1", uid2) != 0 {
		t.Fatal("stored despite a wrong password")
	}
}

// An account with an authenticator app (TOTP) gets the code field on /escrow-key, and the form posted as rendered
// with a current code registers the key (QA-P1a-1: the field was missing, so the post was refused with 401).
func TestEscrowKeyRegisterWithTOTP(t *testing.T) {
	e, _, _ := escrowTestApp(t)
	uid, sess := e.user("escrow_totp_buyer", "buyer")
	agEnableTOTP(e, uid)
	body := e.do("GET", "/escrow-key", sess, nil).Body.String()
	form := regexp.MustCompile(`(?s)<form class="stack" method="post" action="/escrow-key">(.*?)</form>`).FindStringSubmatch(body)
	if form == nil {
		t.Fatalf("no key form: %s", body)
	}
	names := map[string]bool{}
	for _, m := range regexp.MustCompile(`<input[^>]* name="([a-z]+)"`).FindAllStringSubmatch(form[1], -1) {
		names[m[1]] = true
	}
	if !names["code"] || !names["password"] || !names["key"] || !names["csrf"] || !names["layout"] {
		t.Fatalf("form fields %v", names)
	}
	code, _ := e.totpCodeFor(uid, 0)
	f := url.Values{"key": {testBIP48Key}, "password": {testPassword}, "code": {code}, "layout": {"core"}} // layout: the checked radio (A-455)
	for name := range names {
		if f.Get(name) == "" && name != "csrf" && name != "dedicated" {
			t.Fatalf("the form has a field %q this test does not fill", name)
		}
	}
	if w := e.do("POST", "/escrow-key", sess, f); w.Code != 303 {
		t.Fatalf("register with TOTP: %d %s", w.Code, w.Body.String())
	}
	audit := agStrings(t, e, "SELECT action FROM audit_events WHERE user_id=$1 ORDER BY id", uid)
	if !slices.Equal(audit, []string{"Registered a Bitcoin escrow key (confirmed with password and authenticator code)"}) {
		t.Fatalf("audit %q", audit)
	}
	if n := agInt(e, "SELECT count(*) FROM user_escrow_keys WHERE user_id=$1", uid); n != 1 {
		t.Fatalf("%d key rows", n)
	}
	// The change form keeps the code field once a key is registered.
	if body := e.do("GET", "/escrow-key", sess, nil).Body.String(); !strings.Contains(body, "Change your key") || !strings.Contains(body, `name="code"`) {
		t.Fatal("change form without the code field")
	}
}

// ES-3 refusals through the form: each is refused before the password is checked (a wrong password still gets the
// refusal, not 401) and before any RPC, stores nothing and never echoes the paste.
func TestEscrowKeyRefusedThroughForm(t *testing.T) {
	logs := captureLog(t)
	e, core, _ := escrowTestApp(t)
	uid, sess := e.user("escrow_refuse_buyer", "buyer")
	tprv := strings.TrimPrefix(strings.Split(testPrivDescriptor, "/")[0], "wpkh(")
	calls := len(core.methods())
	for name, c := range map[string]struct{ in, want string }{
		"private key":        {tprv, "this key is exposed, replace it"},
		"private descriptor": {testPrivDescriptor, "this key is exposed, replace it"},
		"WIF":                {testWIF(true, true), "this key is exposed, replace it"},
		"recovery words":     {"abandon ability able about above absent absorb abstract absurd abuse access accident", "this key is exposed, replace it"},
		"empty payload":      {"3QJmnh", "needs its origin"},
		"empty key payload":  {"[7450f935/48h/1h/0h/2h]3QJmnh", "not a valid extended public key"},
		"JSON words":         {`["abandon","ability","able","about","above","absent","absorb","abstract","absurd","abuse","access","accident"]`, "this key is exposed, replace it"},
		"numbered words":     {"1. Abandon 2. Ability 3. Able 4. About 5. Above 6. Absent 7. Absorb 8. Abstract 9. Absurd 10. Abuse 11. Access 12. Accident.", "this key is exposed, replace it"},
		"comma":              {testBIP48Key + "," + testBIP84Key, "Paste exactly one key"},
		"parentheses":        {"wsh(" + testBIP48Key + ")", "not a whole descriptor"},
		"hash":               {testBIP48Key + "#20gdjfqv", "not a whole descriptor"},
		"whitespace":         {testBIP48Key[:30] + " " + testBIP48Key[30:], "no spaces or line breaks"},
		"no origin":          {testBIP48Key[23:], "needs its origin"},
		"mainnet":            {"[7450f935/48h/1h/0h/2h]" + reversion(t, testBIP48Key[23:], 0x0488b21e), "mainnet key"},
		"SLIP-132 vpub":      {"[7450f935/48h/1h/0h/2h]" + reversion(t, testBIP48Key[23:], 0x045f1cf6), "single-signature"},
		"not BIP48, no ack":  {testBIP84Key, "not on the standard multisig path"},
		"arbiter's own key":  {"[" + fakeArbiterFP + "/48h/1h/0h/2h]" + testBIP48Key[23:], "cannot be used here"},
	} {
		t.Run(name, func(t *testing.T) {
			w := e.do("POST", "/escrow-key", sess, url.Values{"key": {c.in}, "password": {"wrong-password-here"}})
			body := html.UnescapeString(w.Body.String())
			if w.Code != 400 || !strings.Contains(body, c.want) {
				t.Fatalf("%d %s", w.Code, body)
			}
			if len(c.in) > 20 && strings.Contains(body, c.in[len(c.in)-20:]) {
				t.Fatal("the response echoes the paste")
			}
		})
	}
	if n := agInt(e, "SELECT count(*) FROM user_escrow_keys WHERE user_id=$1", uid); n != 0 || len(core.methods()) != calls {
		t.Fatalf("stored %d rows, %d node calls", n, len(core.methods())-calls)
	}
	assertNowhereInDB(t, e, tprv[4:40])
	if strings.Contains(logs.String(), tprv[4:40]) {
		t.Fatal("the private key reached the log")
	}
	if strings.Contains(logs.String(), "panic") {
		t.Fatalf("a refusal panicked: %s", logs.String())
	}
	// The refusals spent no confirmation budget: the right password still works at once.
	if code, body := registerKey(e, sess, testBIP84Key, "dedicated", "1"); code != 303 {
		t.Fatalf("after refusals: %d %s", code, body)
	}
	if audit := agStrings(t, e, "SELECT action FROM audit_events WHERE user_id=$1", uid); len(audit) != 1 || audit[0] != "Registered a Bitcoin escrow key (path outside BIP48; wallet used only for this market acknowledged) (confirmed with password)" {
		t.Fatalf("audit %q", audit)
	}
}

// Cross-account reuse is refused by UNIQUE(xpub_hmac), whatever version bytes or origin the second account uses, and
// the refusal does not say that another account has the key.
func TestEscrowKeyReuseRefused(t *testing.T) {
	e, _, _ := escrowTestApp(t)
	_, sessA := e.user("escrow_reuse_a", "buyer")
	uidB, sessB := e.user("escrow_reuse_b", "vendor")
	if code, _ := registerKey(e, sessA, testBIP48Key); code != 303 {
		t.Fatal("A")
	}
	vpub := "[7450f935/48h/1h/0h/2h]" + reversion(t, testBIP48Key[23:], 0x02575483)
	// The same chain code and public key serialized with another parent fingerprint, or at another depth under a
	// matching origin: a different extended-key string, the same key (QA-P1a-6, M7c).
	raw, err := base58CheckDecode(testBIP48Key[23:])
	if err != nil {
		t.Fatal(err)
	}
	reencode := func(f func(b []byte)) string {
		b := bytes.Clone(raw)
		f(b)
		return base58CheckEncode(b)
	}
	otherParent := "[7450f935/48h/1h/0h/2h]" + reencode(func(b []byte) { copy(b[5:9], []byte{0xde, 0xad, 0xbe, 0xef}) })
	otherDepth := "[7450f935/48h/1h/0h/9/2h]" + reencode(func(b []byte) { b[4] = 5 })
	for _, in := range []string{otherParent, otherDepth} {
		if k, err := parseEscrowKey(in); err != nil || k.XPub == testBIP48Key[23:] {
			t.Fatalf("re-encoded fixture %q: %+v %v", in, k, err)
		}
	}
	for _, in := range []string{testBIP48Key, vpub, "[0000beef/48h/1h/0h/2h]" + testBIP48Key[23:], otherParent, otherDepth} {
		code, body := registerKey(e, sessB, in, "dedicated", "1")
		if code != 409 || !strings.Contains(body, "cannot be used here") || strings.Contains(body, "another account") {
			t.Fatalf("%q: %d %s", in, code, body)
		}
	}
	if agInt(e, "SELECT count(*) FROM user_escrow_keys WHERE user_id=$1", uidB) != 0 {
		t.Fatal("B stored a key")
	}
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

// Changing the key needs the password, resets activation and never changes an existing escrow's keys.
func TestEscrowKeyChangeLeavesEscrows(t *testing.T) {
	e, _, _ := escrowTestApp(t)
	uid, sess := e.user("escrow_change_buyer", "buyer")
	if code, _ := registerKey(e, sess, testBIP48Key); code != 303 {
		t.Fatal("register")
	}
	e.DB.Exec("UPDATE user_escrow_keys SET active_at=now() WHERE user_id=$1", uid)
	before := insertEscrowFor(t, e, uid)
	if body := e.do("GET", "/escrow-key", sess, nil).Body.String(); !strings.Contains(body, "This key is used by escrows") || strings.Contains(body, "Show the practice transaction") {
		t.Fatalf("page with escrows: %s", body)
	}
	if code, body := registerKey(e, sess, testBIP84Key, "dedicated", "1"); code != 303 {
		t.Fatalf("change: %d %s", code, body)
	}
	if after := strings.Join(agStrings(t, e, "SELECT p::text FROM escrow_pubkeys p ORDER BY pubkey"), "\n"); after != before {
		t.Fatalf("escrow keys changed:\n%s\n%s", before, after)
	}
	var fp string
	var active bool
	e.DB.QueryRow("SELECT origin_fingerprint,active_at IS NOT NULL FROM user_escrow_keys WHERE user_id=$1", uid).Scan(&fp, &active)
	if active || fp != "7450f935" {
		t.Fatalf("after change: active=%v", active)
	}
	audit := agStrings(t, e, "SELECT action FROM audit_events WHERE user_id=$1 ORDER BY id", uid)
	if audit[len(audit)-1] != "Changed Bitcoin escrow key (path outside BIP48; wallet used only for this market acknowledged) (confirmed with password)" {
		t.Fatalf("audit %q", audit)
	}
	// The new key has no escrows, so practice is offered for it.
	if body := e.do("GET", "/escrow-key", sess, nil).Body.String(); !strings.Contains(body, "Show the practice transaction") {
		t.Fatal("practice not offered for the new key")
	}
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

// ES-9: the practice PSBT uses the owner's reserved child, one throwaway server key and one unsignable key, never the
// market key; its made-up outpoint is checked absent; a correct signature activates the key and records only
// active_at; practice is then never offered again.
func TestEscrowPracticeActivates(t *testing.T) {
	logs := captureLog(t)
	e, core, _ := escrowTestApp(t)
	uid, sess := e.user("escrow_practice_buyer", "buyer")
	if code, _ := registerKey(e, sess, testBIP48Key); code != 303 {
		t.Fatal("register")
	}
	psbt := practicePSBT(t, e, sess)
	p, err := decodePSBT(psbt)
	if err != nil {
		t.Fatal(err)
	}
	in := p.Inputs[0]
	if len(p.Tx.In) != 1 || len(p.Tx.Out) != 1 || in.WitnessUTXO == nil || in.WitnessUTXO.Value != practicePrevSats || p.Tx.Out[0].Value != practiceOutSats {
		t.Fatalf("practice tx %+v", p.Tx)
	}
	var owner int
	for _, d := range in.Derivations {
		if formatOriginPath(d.Path) == "/48h/1h/0h/2h/0/0" {
			owner++
		}
		if strings.HasPrefix(formatOriginPath(d.Path), "/84h") {
			t.Fatal("the market's key is in the practice transaction")
		}
	}
	if owner != 1 {
		t.Fatalf("owner derivations: %d", owner)
	}
	if _, params := core.lastCall(t, "gettxout"); !strings.HasSuffix(params, `,0,true]`) {
		t.Fatalf("gettxout %s", params)
	}
	for _, m := range core.methods() {
		if m == "walletprocesspsbt" || m == "listdescriptors" {
			t.Fatalf("%s called while building practice", m)
		}
	}
	// The PSBT is rebuilt identically: nothing about it is stored.
	if again := practicePSBT(t, e, sess); again != psbt {
		t.Fatal("practice PSBT differs between requests")
	}
	var before string
	e.DB.QueryRow("SELECT xpub_sealed||xpub_hmac||origin_fingerprint||origin_path||created::text FROM user_escrow_keys WHERE user_id=$1", uid).Scan(&before)
	signed := fakeOwnerSign(t, psbt, "7450f935", 1)
	if code, body := submitPractice(e, sess, signed); code != 303 {
		t.Fatalf("practice: %d %s", code, body)
	}
	var after string
	var active bool
	e.DB.QueryRow("SELECT xpub_sealed||xpub_hmac||origin_fingerprint||origin_path||created::text,active_at IS NOT NULL FROM user_escrow_keys WHERE user_id=$1", uid).Scan(&after, &active)
	if !active || after != before {
		t.Fatalf("activation changed more than active_at: active=%v", active)
	}
	if _, params := core.lastCall(t, "descriptorprocesspsbt"); !strings.Contains(params, `"ALL",true,false]`) || strings.Count(params, "tprv") != 1 {
		t.Fatalf("throwaway signing params %s", params)
	}
	if _, params := core.lastCall(t, "testmempoolaccept"); !strings.HasSuffix(params, `,0.001]`) {
		t.Fatalf("testmempoolaccept params %s", params)
	}
	audit := agStrings(t, e, "SELECT action FROM audit_events WHERE user_id=$1 ORDER BY id", uid)
	if audit[len(audit)-1] != "Activated Bitcoin escrow key (practice transaction signed)" {
		t.Fatalf("audit %q", audit)
	}
	body := e.do("GET", "/escrow-key?practice=1", sess, nil).Body.String()
	if !strings.Contains(body, "Active since ") || strings.Contains(body, "escrow-psbt") || strings.Contains(body, "Show the practice transaction") {
		t.Fatalf("active page: %s", body)
	}
	if code, _ := submitPractice(e, sess, signed); code != 409 {
		t.Fatalf("second practice: %d", code)
	}
	raw, _ := base64.StdEncoding.DecodeString(signed)
	sigTail := base64.StdEncoding.EncodeToString(raw[len(raw)-60:])
	assertNowhereInDB(t, e, sigTail[:40])
	if strings.Contains(logs.String(), sigTail[:40]) || strings.Contains(logs.String(), "tprv") {
		t.Fatal("the paste or a throwaway key reached the log")
	}
}

// ES-9 refusal table: every paste below is refused with its fixed message and leaves the key inactive.
func TestEscrowPracticeRefusals(t *testing.T) {
	e, core, _ := escrowTestApp(t)
	uid, sess := e.user("escrow_practice_refuse", "buyer")
	if code, _ := registerKey(e, sess, testBIP48Key); code != 303 {
		t.Fatal("register")
	}
	psbt := practicePSBT(t, e, sess)
	p, _ := decodePSBT(psbt)
	sp, _ := decodePSBT(fakeOwnerSign(t, psbt, "7450f935", 1))
	ownerSig := sp.Inputs[0].PartialSigs[0]
	add := func(base string, key, val []byte) string {
		pk, err := decodePSBT(base)
		if err != nil {
			t.Fatal(err)
		}
		out, err := pk.withInputField(0, key, val)
		if err != nil {
			t.Fatal(err)
		}
		return encodePSBT(out)
	}
	corrupt := append([]byte(nil), ownerSig.Sig...)
	corrupt[10] ^= 1
	var server []byte
	ws := p.Inputs[0].WitnessScript
	for _, k := range [][]byte{ws[2:35], ws[36:69], ws[70:103]} {
		if string(k) != string(ownerSig.PubKey) && k[0] == 3 {
			server = k // the fake's throwaway key has prefix 03; the unsignable key 02
		}
	}
	// The same PSBT with one byte of the spent txid changed: another transaction.
	otherRaw := append([]byte(nil), p.Raw...)
	otherRaw[bytes.Index(otherRaw, p.UnsignedTx)+5] ^= 1
	otherTx := fakeOwnerSign(t, encodePSBT(otherRaw), "7450f935", 1)
	cases := map[string]struct {
		paste       string
		code        int
		want        string
		mempoolOK   bool
		prevoutSeen bool
	}{
		"not base64":         {"%%%", 400, "could not be read", false, false},
		"unsigned":           {psbt, 400, "no signature from your registered key", false, false},
		"corrupt signature":  {add(psbt, append([]byte{0x02}, ownerSig.PubKey...), corrupt), 400, "did not verify", false, false},
		"SIGHASH_NONE":       {fakeOwnerSign(t, psbt, "7450f935", 2), 400, "sighash type other than ALL", false, false},
		"SINGLE|ANYONE":      {fakeOwnerSign(t, psbt, "7450f935", 0x83), 400, "sighash type other than ALL", false, false},
		"sighash field NONE": {add(fakeOwnerSign(t, psbt, "7450f935", 1), []byte{0x03}, []byte{2, 0, 0, 0}), 400, "sighash type other than ALL", false, false},
		"server key entry":   {add(fakeOwnerSign(t, psbt, "7450f935", 1), append([]byte{0x02}, server...), fakeSig(server, p.TxID(), 1)), 400, "key other than your registered key", false, false},
		"other key only":     {add(psbt, append([]byte{0x02}, fakeChild("other")...), fakeSig(fakeChild("other"), p.TxID(), 1)), 400, "key other than your registered key", false, false},
		"finalized":          {add(fakeOwnerSign(t, psbt, "7450f935", 1), []byte{0x08}, []byte{0}), 400, "already finalized", false, false},
		"other transaction":  {otherTx, 400, "not the practice transaction", false, false},
		"mempool accepts":    {fakeOwnerSign(t, psbt, "7450f935", 1), 409, "could not be confirmed as unbroadcastable", true, false},
		"prevout exists":     {fakeOwnerSign(t, psbt, "7450f935", 1), 409, "could not be confirmed as unbroadcastable", false, true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			core.mu.Lock()
			core.allowed, core.prevoutExists = c.mempoolOK, c.prevoutSeen
			core.mu.Unlock()
			code, body := submitPractice(e, sess, c.paste)
			if code != c.code || !strings.Contains(body, c.want) {
				t.Fatalf("%d %s", code, body)
			}
			if agInt(e, "SELECT count(*) FROM user_escrow_keys WHERE user_id=$1 AND active_at IS NOT NULL", uid) != 0 {
				t.Fatal("activated")
			}
		})
	}
	core.mu.Lock()
	core.allowed, core.prevoutExists = false, false
	core.mu.Unlock()
	if code, body := submitPractice(e, sess, fakeOwnerSign(t, psbt, "7450f935", 1)); code != 303 {
		t.Fatalf("the correct paste after the refusals: %d %s", code, body)
	}
}

// A node that does not answer: the page says so and the check answers 503, nothing changed.
func TestEscrowPracticeNodeDown(t *testing.T) {
	e, core, srv := escrowTestApp(t)
	uid, sess := e.user("escrow_practice_down", "buyer")
	if code, _ := registerKey(e, sess, testBIP48Key); code != 303 {
		t.Fatal("register")
	}
	psbt := practicePSBT(t, e, sess)
	srv.down.Store(true)
	if body := e.do("GET", "/escrow-key?practice=1", sess, nil).Body.String(); !strings.Contains(body, "cannot be built right now") {
		t.Fatalf("page: %s", body)
	}
	if code, _ := submitPractice(e, sess, fakeOwnerSign(t, psbt, "7450f935", 1)); code != 503 {
		t.Fatalf("check: %d", code)
	}
	// A node whose throwaway signature alone completes the practice transaction is not trusted either.
	srv.down.Store(false)
	core.mu.Lock()
	core.throwawayDone = true
	core.mu.Unlock()
	if code, _ := submitPractice(e, sess, fakeOwnerSign(t, psbt, "7450f935", 1)); code != 503 {
		t.Fatalf("complete server copy: %d", code)
	}
	if agInt(e, "SELECT count(*) FROM user_escrow_keys WHERE user_id=$1 AND active_at IS NOT NULL", uid) != 0 {
		t.Fatal("activated")
	}
}

// Practice is refused for a key with escrows, even when inactive (ES-9: offered only while the key has no escrows).
func TestEscrowPracticeRefusedWithEscrows(t *testing.T) {
	e, _, _ := escrowTestApp(t)
	uid, sess := e.user("escrow_practice_escrows", "buyer")
	if code, _ := registerKey(e, sess, testBIP48Key); code != 303 {
		t.Fatal("register")
	}
	psbt := practicePSBT(t, e, sess)
	insertEscrowFor(t, e, uid)
	if code, body := submitPractice(e, sess, fakeOwnerSign(t, psbt, "7450f935", 1)); code != 409 || !strings.Contains(body, "no escrows") {
		t.Fatalf("%d %s", code, body)
	}
	if body := e.do("GET", "/escrow-key?practice=1", sess, nil).Body.String(); strings.Contains(body, "escrow-psbt") {
		t.Fatal("practice offered for a key with escrows")
	}
}

// The activation update is a compare-and-set on what the practice check used (QA-P1a-6, M8b and M5c): a key whose
// stored origin or layout changed, or that gained an escrow, while the node checked the paste is not activated (A-455:
// a Core practice signature never activates the same key re-registered as a per-order wallet key meanwhile).
func TestEscrowPracticeKeyChangedDuringCheck(t *testing.T) {
	e, core, _ := escrowTestApp(t)
	for name, change := range map[string]func(uid string, escrowID int64) error{
		"origin fingerprint changed": func(uid string, _ int64) error {
			_, err := e.DB.Exec("UPDATE user_escrow_keys SET origin_fingerprint='0000beef' WHERE user_id=$1", uid)
			return err
		},
		"origin path changed": func(uid string, _ int64) error {
			_, err := e.DB.Exec("UPDATE user_escrow_keys SET origin_path='/48h/1h/1h/2h' WHERE user_id=$1", uid)
			return err
		},
		"layout changed": func(uid string, _ int64) error {
			_, err := e.DB.Exec("UPDATE user_escrow_keys SET layout='per_order' WHERE user_id=$1", uid)
			return err
		},
		"escrow created": func(uid string, escrowID int64) error {
			_, err := e.DB.Exec(`INSERT INTO escrow_pubkeys(pubkey,escrow_id,party,user_id,key_hmac,origin_fingerprint,origin_path,derivation_suffix)
				SELECT '02'||repeat('ab',32),$2,'buyer',user_id,xpub_hmac,origin_fingerprint,origin_path,'/0/1' FROM user_escrow_keys WHERE user_id=$1`, uid, escrowID)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			uid, sess := e.user("escrow_race_"+randomToken()[:8], "buyer")
			if code, _ := registerKey(e, sess, testBIP48Key); code != 303 {
				t.Fatal("register")
			}
			// An order and escrow row without keys: the key has no escrows until the change inserts one.
			vendorID, _ := e.user("escrow_race_vendor_"+randomToken()[:6], "vendor")
			var escrowID int64
			if err := e.DB.QueryRow("INSERT INTO escrows(order_id,address,market_index,refund_address,fee_bps,fee_destination) VALUES($1,$2,nextval('escrow_market_index'),$3,0,'') RETURNING id",
				e.order(uid, e.product(vendorID, "digital"), "BTC", "awaiting_payment"), "bcrt1qrace"+randomToken()[:20], "bcrt1qrefund"+randomToken()[:20]).Scan(&escrowID); err != nil {
				t.Fatal(err)
			}
			signed := fakeOwnerSign(t, practicePSBT(t, e, sess), "7450f935", 1)
			var changeErr error
			core.mu.Lock()
			core.beforeMempool = func() { changeErr = change(uid, escrowID) }
			core.mu.Unlock()
			code, body := submitPractice(e, sess, signed)
			core.mu.Lock()
			core.beforeMempool = nil
			core.mu.Unlock()
			if changeErr != nil {
				t.Fatal(changeErr)
			}
			if code != 409 || !strings.Contains(body, "changed or activated while the practice transaction was checked") {
				t.Fatalf("%d %s", code, body)
			}
			if agInt(e, "SELECT count(*) FROM user_escrow_keys WHERE user_id=$1 AND active_at IS NOT NULL", uid) != 0 {
				t.Fatal("activated")
			}
		})
		// Delete the fixture escrow keys: each subtest registers the same key on a new account.
		e.DB.Exec("DELETE FROM escrow_pubkeys")
		e.DB.Exec("DELETE FROM user_escrow_keys")
	}
}

// A mainnet market (a payment provider configured for mainnet) with ESCROW_BTC=on stops startup before any escrow
// RPC, even with BITCOIN_CHAIN blank (QA-P1a-6, M4c): escrow runs on test networks only.

// A party-key row sealed under another root is unreadable: /escrow-key says so with a fixed message instead of
// blaming the node, the practice check refuses the same way, and registering the key again replaces that row.
// The running app root and immutable arbiter/signer pins are unchanged.
func TestEscrowKeyUnreadablePartyRowCanBeReRegistered(t *testing.T) {
	logs := captureLog(t)
	e, core, _ := escrowTestApp(t)
	uid, sess := e.user("escrow_unreadable_buyer", "buyer")
	if code, _ := registerKey(e, sess, testBIP48Key); code != 303 {
		t.Fatal("register")
	}
	signed := fakeOwnerSign(t, practicePSBT(t, e, sess), "7450f935", 1)
	makePartyEscrowKeyUnreadable(t, &escrowEnv{testEnv: e, t: t, core: core}, uid, testBIP48Key)
	e.restart() // same key root and pins; only this user's party-key row is unreadable.
	calls := len(core.methods())
	body := html.UnescapeString(e.do("GET", "/escrow-key?practice=1", sess, nil).Body.String())
	if !strings.Contains(body, errEscrowKeyUnreadable) || strings.Contains(body, "did not answer") || strings.Contains(body, "escrow-psbt") {
		t.Fatalf("page with unreadable party row: %s", body)
	}
	if code, body := submitPractice(e, sess, signed); code != 409 || !strings.Contains(body, errEscrowKeyUnreadable) {
		t.Fatalf("practice with unreadable party row: %d %s", code, body)
	}
	if n := len(core.methods()) - calls; n != 0 {
		t.Fatalf("%d node calls for an unreadable key", n)
	}
	if !strings.Contains(logs.String(), "escrow: stored escrow key cannot be opened") || strings.Contains(logs.String(), "7450f935") {
		t.Fatalf("log: %s", logs.String())
	}
	// Registering the key again (with the password) replaces the unreadable copy and practice works again.
	if code, body := registerKey(e, sess, testBIP48Key); code != 303 {
		t.Fatalf("re-register: %d %s", code, body)
	}
	if code, body := submitPractice(e, sess, fakeOwnerSign(t, practicePSBT(t, e, sess), "7450f935", 1)); code != 303 {
		t.Fatalf("practice after re-registering: %d %s", code, body)
	}
	if agInt(e, "SELECT count(*) FROM user_escrow_keys WHERE user_id=$1 AND active_at IS NOT NULL", uid) != 1 {
		t.Fatal("not active")
	}
}

// Migration 074's constraints: one key per HMAC, no practice child in an escrow, the market party has no account,
// one release or refund per order (stray refunds apart), and payments.settlement_id.
func TestEscrowMigrationConstraints(t *testing.T) {
	e := newTestApp(t)
	a, _ := e.user("mig_escrow_a", "buyer")
	b, _ := e.user("mig_escrow_b", "vendor")
	h := strings.Repeat("ab", 32)
	ins := "INSERT INTO user_escrow_keys(user_id,xpub_sealed,xpub_hmac,origin_fingerprint,origin_path) VALUES($1,'s',$2,'7450f935','/48h/1h/0h/2h')"
	if _, err := e.DB.Exec(ins, a, h); err != nil {
		t.Fatal(err)
	}
	mustFail := func(what, q string, args ...any) {
		t.Helper()
		if _, err := e.DB.Exec(q, args...); err == nil {
			t.Errorf("%s accepted", what)
		}
	}
	mustFail("a second account with the same key", ins, b, h)
	mustFail("a bad origin path", "INSERT INTO user_escrow_keys(user_id,xpub_sealed,xpub_hmac,origin_fingerprint,origin_path) VALUES($1,'s',$2,'7450f935','48h')", b, strings.Repeat("cd", 32))
	order := e.order(a, e.product(b, "digital"), "BTC", "awaiting_payment")
	var id int64
	if err := e.DB.QueryRow("INSERT INTO escrows(order_id,address,market_index,refund_address,fee_bps,fee_destination) VALUES($1,'bcrt1qa',nextval('escrow_market_index'),'bcrt1qr',250,'bcrt1qfee') RETURNING id", order).Scan(&id); err != nil {
		t.Fatal(err)
	}
	esc := "INSERT INTO escrows(order_id,address,market_index,refund_address,fee_bps,fee_destination) VALUES($1,$2,$3,$4,$5,$6)"
	other := e.order(b, e.product(a, "digital"), "BTC", "awaiting_payment")
	mustFail("a second escrow at the same address", esc, other, "bcrt1qa", 99, "bcrt1qr2", 0, "")
	// P1-b: the refund address is pinned once (ES-5), never the escrow's own address; the fee is snapshotted.
	mustFail("a refund address pinned twice", esc, other, "bcrt1qb", 99, "bcrt1qr", 0, "")
	mustFail("the escrow address as its own refund address", esc, other, "bcrt1qb", 99, "bcrt1qb", 0, "")
	mustFail("an empty refund address", esc, other, "bcrt1qb", 99, "", 0, "")
	mustFail("a market fee without a destination", esc, other, "bcrt1qb", 99, "bcrt1qr2", 100, "")
	mustFail("a market fee above 100%", esc, other, "bcrt1qb", 99, "bcrt1qr2", 10001, "bcrt1qfee")
	mustFail("a negative market fee", esc, other, "bcrt1qb", 99, "bcrt1qr2", -1, "bcrt1qfee")
	// No payouts row for an escrow order (ES-1, invariant 4), inserted or moved there. Migration 108 refuses every new
	// payouts row (A-492); 074's escrow refusal beneath it is checked with 108's triggers off, rolled back.
	payout := "INSERT INTO payouts(order_id,kind,user_id,currency,amount,address,state) VALUES($1,'release',$2,'BTC',1000,'x','pending')"
	mustFail("a payout for an escrow order", payout, order, b)
	mustFail("a payout for an order without escrow", payout, other, a)
	withPooledHistoryWritable(t, e, func(tx *sql.Tx) {
		if _, err := tx.Exec(payout, order, b); err == nil || !strings.Contains(err.Error(), "is an escrow order") {
			t.Errorf("a payout for an escrow order, 108 off: %v", err)
		}
	})
	withPooledHistoryWritable(t, e, func(tx *sql.Tx) {
		if _, err := tx.Exec(payout, other, a); err != nil {
			t.Fatalf("a payout for an order without escrow, 108 off: %v", err)
		}
		if _, err := tx.Exec("UPDATE payouts SET order_id=$1 WHERE order_id=$2", order, other); err == nil || !strings.Contains(err.Error(), "is an escrow order") {
			t.Errorf("a payout moved to an escrow order, 108 off: %v", err)
		}
	})
	pk := "INSERT INTO escrow_pubkeys(pubkey,escrow_id,party,user_id,key_hmac,origin_fingerprint,origin_path,derivation_suffix) VALUES($1,$2,$3,$4,$5,'7450f935','/48h/1h/0h/2h',$6)"
	mustFail("the practice child in an escrow", pk, "02"+strings.Repeat("11", 32), id, "buyer", a, h, "/0/0")
	// Paths are canonical (no leading zeros), so no other spelling of /0/0 passes that CHECK (QA-P1a-3).
	// Each attempt runs in a transaction that is rolled back, so an accepted row cannot mask the next attempt.
	inTx := func(q string, args ...any) error {
		tx, err := e.DB.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		_, err = tx.Exec(q, args...)
		return err
	}
	vendorPK := "INSERT INTO escrow_pubkeys(pubkey,escrow_id,party,user_id,key_hmac,origin_fingerprint,origin_path,derivation_suffix) VALUES('02'||repeat('66',32),$1,'vendor',$2,$3,'7450f935',$4,$5)"
	keyRow := "INSERT INTO user_escrow_keys(user_id,xpub_sealed,xpub_hmac,origin_fingerprint,origin_path) VALUES($1,'s',$2,'7450f935',$3)"
	for _, suffix := range []string{"/0/00", "/00/0", "/048h", "/0/01", "/0/00000000000"} {
		if inTx(vendorPK, id, b, strings.Repeat("cd", 32), "/48h/1h/0h/2h", suffix) == nil {
			t.Errorf("derivation suffix %s accepted", suffix)
		}
	}
	for _, path := range []string{"/048h/1h/0h/2h", "/48h/01h/0h/2h", "/48h/1h/00h/2h"} {
		if inTx(keyRow, b, strings.Repeat("cd", 32), path) == nil {
			t.Errorf("account key origin path %s accepted", path)
		}
		if inTx(vendorPK, id, b, strings.Repeat("cd", 32), path, "/0/3") == nil {
			t.Errorf("escrow key origin path %s accepted", path)
		}
	}
	// The canonical spellings, including multi-digit steps, are accepted.
	if err := inTx(vendorPK, id, b, strings.Repeat("cd", 32), "/48h/1h/10h/2h", "/0/10"); err != nil {
		t.Errorf("canonical escrow key paths refused: %v", err)
	}
	if err := inTx(keyRow, b, strings.Repeat("cd", 32), "/48h/1h/2147483647h/2h"); err != nil {
		t.Errorf("canonical account key path refused: %v", err)
	}
	mustFail("a market key with an account", pk, "02"+strings.Repeat("22", 32), id, "market", a, nil, "/0/5")
	mustFail("a party key without an account", pk, "02"+strings.Repeat("33", 32), id, "vendor", nil, nil, "/0/5")
	if _, err := e.DB.Exec(pk, "02"+strings.Repeat("44", 32), id, "buyer", a, h, "/0/1"); err != nil {
		t.Fatal(err)
	}
	mustFail("the same child key in two escrow rows", pk, "02"+strings.Repeat("44", 32), id, "vendor", b, strings.Repeat("ef", 32), "/0/1")
	mustFail("a second buyer key in one escrow", pk, "02"+strings.Repeat("55", 32), id, "buyer", a, h, "/0/2")
	st := "INSERT INTO settlements(order_id,kind) VALUES($1,$2)"
	if _, err := e.DB.Exec(st, order, "release"); err != nil {
		t.Fatal(err)
	}
	mustFail("a refund beside a release", st, order, "refund")
	mustFail("a second release", st, order, "release")
	mustFail("an unknown kind", st, order, "return")
	// Migration 076: one stray refund in progress per order at a time, and another once it has ended.
	if _, err := e.DB.Exec(st, order, "stray_refund"); err != nil {
		t.Fatalf("stray refund: %v", err)
	}
	mustFail("a second stray refund in progress", st, order, "stray_refund")
	if _, err := e.DB.Exec("UPDATE settlements SET state='failed' WHERE order_id=$1 AND kind='stray_refund'", order); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec(st, order, "stray_refund"); err != nil {
		t.Fatalf("a stray refund after one ended: %v", err)
	}
	mustFail("an unknown state", "UPDATE settlements SET state='sent' WHERE order_id=$1", order)
	if n := agInt(e, "SELECT count(*) FROM payments WHERE settlement_id IS NOT NULL"); n != 0 {
		t.Fatal("payments.settlement_id")
	}
}
