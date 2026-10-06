package market

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"html"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// A-455 (shape (b) slice 3; rulings P4-R2, P4-R18, ES-9): the escrow key's layout (core or per_order, migration 088),
// registering a per-order wallet key, the shape-(b) practice signature at the reserved per-order index 2^31-1 with
// rooted throwaway and unsignable cosigners, and the vendor's key type on the listing and vendor pages. DB integration
// with the simulated Core of escrow_fake_test.go (simulated RPC, not chain evidence: TestEscrowRegtestPerOrderPractice
// in scripts/test-escrow-regtest.sh signs the same practice with Core on regtest).

// Migration 088: layout defaults to core and takes only core or per_order; escrow_pubkeys refuses a per-order suffix
// (three or more steps: /<i>/0/0) whose first step is 0 or 1, and a party's per-order suffix at the practice index,
// while shape (a)'s two-step /0/<i> rows stay valid. Both new constraints are validated.
func TestEscrowKeyLayoutMigration(t *testing.T) {
	e := newTestApp(t)
	a, _ := e.user("mig088_a", "buyer")
	b, _ := e.user("mig088_b", "vendor")
	ins := "INSERT INTO user_escrow_keys(user_id,xpub_sealed,xpub_hmac,origin_fingerprint,origin_path) VALUES($1,'s',$2,'7450f935','/48h/1h/0h/2h')"
	if _, err := e.DB.Exec(ins, a, strings.Repeat("ab", 32)); err != nil {
		t.Fatal(err)
	}
	var layout string
	if err := e.DB.QueryRow("SELECT layout FROM user_escrow_keys WHERE user_id=$1", a).Scan(&layout); err != nil || layout != "core" {
		t.Fatalf("default layout %q: %v", layout, err)
	}
	if _, err := e.DB.Exec("INSERT INTO user_escrow_keys(user_id,xpub_sealed,xpub_hmac,origin_fingerprint,origin_path,layout) VALUES($1,'s',$2,'7450f935','/48h/1h/0h/2h','per_order')", b, strings.Repeat("cd", 32)); err != nil {
		t.Fatalf("per_order layout refused: %v", err)
	}
	for _, bad := range []string{"other", "", "Core", "per-order"} {
		if _, err := e.DB.Exec("UPDATE user_escrow_keys SET layout=$2 WHERE user_id=$1", a, bad); err == nil {
			t.Errorf("layout %q accepted", bad)
		}
	}
	if _, err := e.DB.Exec("UPDATE user_escrow_keys SET layout=NULL WHERE user_id=$1", a); err == nil {
		t.Error("NULL layout accepted")
	}

	order := e.order(a, e.product(b, "digital"), "BTC", "awaiting_payment")
	var id int64
	if err := e.DB.QueryRow("INSERT INTO escrows(order_id,address,market_index,refund_address,fee_bps,fee_destination) VALUES($1,'bcrt1qa088',nextval('escrow_market_index'),'bcrt1qr088',0,'') RETURNING id", order).Scan(&id); err != nil {
		t.Fatal(err)
	}
	// Each attempt runs in a transaction that is rolled back, so an accepted row cannot mask the next attempt.
	try := func(party, suffix string) error {
		tx, err := e.DB.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		var uid, h any
		if party != "market" {
			uid, h = a, strings.Repeat("ab", 32)
		}
		_, err = tx.Exec("INSERT INTO escrow_pubkeys(pubkey,escrow_id,party,user_id,key_hmac,origin_fingerprint,origin_path,derivation_suffix) VALUES('02'||repeat('77',32),$1,$2,$3,$4,'7450f935','/48h/1h/0h/2h',$5)",
			id, party, uid, h, suffix)
		return err
	}
	for _, c := range []struct{ party, suffix string }{
		{"buyer", "/0/0/0"}, {"buyer", "/1/0/0"}, {"vendor", "/0/7/0"}, {"vendor", "/1/0/0/0"}, {"buyer", "/0h/0/0"}, {"buyer", "/1h/0/0"},
		{"market", "/0/0/0"}, {"market", "/1/0/0"}, // the market's per-order key follows the same rule (P4-R2)
		{"buyer", "/2147483647/0/0"}, {"vendor", "/2147483647/0/0"}, // the reserved practice index (ES-9)
	} {
		if try(c.party, c.suffix) == nil {
			t.Errorf("%s derivation suffix %s accepted", c.party, c.suffix)
		}
	}
	for _, c := range []struct{ party, suffix string }{
		{"buyer", "/0/1"}, {"vendor", "/0/2501"}, {"market", "/0/0"}, {"market", "/0/7"}, // shape (a)
		{"buyer", "/2/0/0"}, {"vendor", "/17/0/0"}, {"buyer", "/2147483646/0/0"}, {"market", "/2147483647/0/0"}, // shape (b)
	} {
		if err := try(c.party, c.suffix); err != nil {
			t.Errorf("%s derivation suffix %s refused: %v", c.party, c.suffix, err)
		}
	}
	for _, name := range []string{"escrow_pubkeys_per_order_index", "escrow_pubkeys_per_order_practice"} {
		var valid bool
		if err := e.DB.QueryRow("SELECT convalidated FROM pg_constraint WHERE conname=$1 AND conrelid='escrow_pubkeys'::regclass", name).Scan(&valid); err != nil || !valid {
			t.Errorf("constraint %s: validated=%v %v", name, valid, err)
		}
	}
}

// Registering: the layout is a form choice, core when absent; a per-order wallet key must be on the BIP48 multisig path
// (no "dedicated" escape); an unknown layout is refused; the page and /account show the key type and the copy says that
// Electrum's own seed type cannot make a per-order wallet.
func TestEscrowKeyLayoutRegistration(t *testing.T) {
	e, _, _ := escrowTestApp(t)
	layoutOf := func(uid string) string {
		t.Helper()
		var l string
		if err := e.DB.QueryRow("SELECT layout FROM user_escrow_keys WHERE user_id=$1", uid).Scan(&l); err != nil {
			t.Fatal(err)
		}
		return l
	}
	core, coreSess := e.user("layout_core", "buyer")
	page := html.UnescapeString(e.do("GET", "/escrow-key", coreSess, nil).Body.String())
	for _, want := range []string{`name="layout" value="core" checked`, `name="layout" value="per_order"`, "Electrum's own seed type", "BIP39 seed", "Sparrow"} {
		if !strings.Contains(page, want) {
			t.Errorf("registration form lacks %q", want)
		}
	}
	if code, body := registerKey(e, coreSess, testBIP48Key); code != 303 {
		t.Fatalf("register core: %d %s", code, body)
	}
	if l := layoutOf(core); l != "core" {
		t.Fatalf("layout %q, want core", l)
	}
	if body := e.do("GET", "/escrow-key", coreSess, nil).Body.String(); !strings.Contains(body, "<dt>Key type</dt>\n<dd>Bitcoin Core</dd>") {
		t.Fatalf("key page lacks the key type: %s", body)
	}

	per, perSess := e.user("layout_per", "buyer")
	if code, body := registerKey(e, perSess, testBIP84Key, "layout", "per_order", "dedicated", "1"); code != 400 || !strings.Contains(html.UnescapeString(body), "m/48h/1h/0h/2h") {
		t.Fatalf("per-order key off the BIP48 path: %d %s", code, body)
	}
	if code, body := registerKey(e, perSess, testBIP48Key, "layout", "electrum"); code != 400 || !strings.Contains(body, "Choose") {
		t.Fatalf("unknown layout: %d %s", code, body)
	}
	if n := agInt(e, "SELECT count(*) FROM user_escrow_keys WHERE user_id=$1", per); n != 0 {
		t.Fatal("a refused registration saved a key")
	}
	key := altEscrowKey(t, testBIP48Key, "0badf00d", 2)
	if code, body := registerKey(e, perSess, key, "layout", "per_order"); code != 303 {
		t.Fatalf("register per-order: %d %s", code, body)
	}
	if l := layoutOf(per); l != "per_order" {
		t.Fatalf("layout %q, want per_order", l)
	}
	if agInt(e, "SELECT count(*) FROM user_escrow_keys WHERE user_id=$1 AND active_at IS NULL", per) != 1 {
		t.Fatal("a per-order key started active")
	}
	audit := agStrings(t, e, "SELECT action FROM audit_events WHERE user_id=$1 ORDER BY id", per)
	if audit[len(audit)-1] != "Registered a Bitcoin escrow key (per-order wallet) (confirmed with password)" {
		t.Fatalf("audit %q", audit)
	}
	if body := e.do("GET", "/escrow-key", perSess, nil).Body.String(); !strings.Contains(body, "<dt>Key type</dt>\n<dd>Per-order wallet</dd>") || !strings.Contains(body, `name="layout" value="per_order" checked`) {
		t.Fatalf("key page: %s", body)
	}
	if body := e.do("GET", "/account", perSess, nil).Body.String(); !strings.Contains(body, "Inactive (per-order wallet)") {
		t.Fatalf("account row lacks the key type: %s", body)
	}
}

var (
	cosignerArea = regexp.MustCompile(`<pre class="code-block escrow-cosigner"[^>]*>([^<]+)</pre>`)
	practiceAddr = regexp.MustCompile(`<pre class="code-block escrow-practice-address"[^>]*>([^<]+)</pre>`)
	practicePath = regexp.MustCompile(`<pre class="code-block escrow-practice-path"[^>]*>([^<]+)</pre>`)
)

// perOrderPractice reads the shape-(b) practice material of the viewer's page: the PSBT, the two cosigner keys, the
// practice wallet's first address and the owner's path.
func perOrderPractice(t *testing.T, e *testEnv, sess string) (psbt string, cosigners []string, addr, path string) {
	t.Helper()
	body := e.do("GET", "/escrow-key?practice=1", sess, nil).Body.String()
	for _, m := range cosignerArea.FindAllStringSubmatch(body, -1) {
		cosigners = append(cosigners, html.UnescapeString(m[1]))
	}
	a, p := practiceAddr.FindStringSubmatch(body), practicePath.FindStringSubmatch(body)
	if len(cosigners) != 2 || a == nil || p == nil {
		t.Fatalf("per-order practice material missing: %s", body)
	}
	return practicePSBT(t, e, sess), cosigners, html.UnescapeString(a[1]), html.UnescapeString(p[1])
}

// ES-9 in shape (b): the practice PSBT spends a made-up output of a 2-of-3 of the owner's key at the reserved per-order
// index (account/2147483647/0/0) and the /0/0 children of two cosigner keys served rooted (depth 0, parent fingerprint
// 0, child 0): a throwaway key the server signs with and an unsignable one; never the market's key. A signature from
// the owner's Core-layout practice child (account/0/0) is refused with the other-key message; a paste carrying an
// Electrum-style derivation entry for every key is accepted (the practice check reads only signatures), and the key is
// active.
func TestEscrowPerOrderPractice(t *testing.T) {
	logs := captureLog(t)
	e, core, _ := escrowTestApp(t)
	uid, sess := e.user("per_order_practice", "buyer")
	if code, body := registerKey(e, sess, testBIP48Key, "layout", "per_order"); code != 303 {
		t.Fatalf("register: %d %s", code, body)
	}
	psbt, cosigners, addr, path := perOrderPractice(t, e, sess)
	if path != "[7450f935/48h/1h/0h/2h/2147483647]" {
		t.Fatalf("owner path %q", path)
	}
	p, err := decodePSBT(psbt)
	if err != nil {
		t.Fatal(err)
	}
	in := p.Inputs[0]
	if len(p.Tx.In) != 1 || len(p.Tx.Out) != 1 || in.WitnessUTXO == nil || in.WitnessUTXO.Value != practicePrevSats || p.Tx.Out[0].Value != practiceOutSats {
		t.Fatalf("practice tx %+v", p.Tx)
	}
	ws := in.WitnessScript
	sum := sha256.Sum256(ws)
	if want := segwitAddress("bcrt", 0, sum[:]); addr != want {
		t.Fatalf("first address shown %s, the practice script's %s", addr, want)
	}
	if !bytes.Equal(p.Tx.Out[0].Script, append([]byte{0, 32}, sum[:]...)) {
		t.Fatal("the practice transaction does not pay its own escrow script")
	}
	var want [][]byte
	for _, c := range cosigners {
		k, err := parseBIP32Pub(c, versionVpub)
		if err != nil || k.Depth != 0 || k.ParentFP != [4]byte{} || k.Child != 0 {
			t.Fatalf("cosigner %s… not served rooted: %+v %v", c[:12], k, err)
		}
		c0, _, err := ckdPub(k, 0)
		if err == nil {
			c0, _, err = ckdPub(c0, 0)
		}
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, c0.PubKey[:])
		var found bool
		for _, d := range in.Derivations {
			if bytes.Equal(d.PubKey, c0.PubKey[:]) {
				fp := k.Fingerprint()
				found = bytes.Equal(d.Fingerprint[:], fp[:]) && formatOriginPath(d.Path) == "/0/0"
			}
		}
		if !found {
			t.Fatalf("cosigner %s… has no derivation at its own fingerprint /0/0", c[:12])
		}
	}
	var owner []byte
	for _, d := range in.Derivations {
		if hex.EncodeToString(d.Fingerprint[:]) == fakeArbiterFP {
			t.Fatal("the market's key is in the practice transaction")
		}
		if hex.EncodeToString(d.Fingerprint[:]) == "7450f935" {
			if owner != nil || formatOriginPath(d.Path) != "/48h/1h/0h/2h/2147483647/0/0" {
				t.Fatalf("owner derivation %s", formatOriginPath(d.Path))
			}
			owner = d.PubKey
		}
	}
	keys := [][]byte{ws[2:35], ws[36:69], ws[70:103]}
	want = append(want, owner)
	slices.SortFunc(want, bytes.Compare)
	if owner == nil || !slices.EqualFunc(keys, want, bytes.Equal) {
		t.Fatal("the practice script is not the owner's per-order key and the two cosigners' /0/0 children")
	}
	if again := practicePSBT(t, e, sess); again != psbt {
		t.Fatal("practice PSBT differs between requests")
	}

	// The owner's Core-layout practice child (shape (a)'s account/0/0) signing: refused, never active.
	coreChild := fakeChild(strings.SplitN(testBIP48Key, "]", 2)[1] + "/0/0")
	coreSigned, err := p.withInputField(0, append([]byte{0x02}, coreChild...), fakeSig(coreChild, p.TxID(), 1))
	if err != nil {
		t.Fatal(err)
	}
	if code, body := submitPractice(e, sess, encodePSBT(coreSigned)); code != 400 || !strings.Contains(body, "key other than your registered key") {
		t.Fatalf("Core-layout signature: %d %s", code, body)
	}
	if code, body := submitPractice(e, sess, fakeOwnerSign(t, psbt, "7450f935", 2)); code != 400 || !strings.Contains(body, "sighash type other than ALL") {
		t.Fatalf("SIGHASH_NONE: %d %s", code, body)
	}
	if agInt(e, "SELECT count(*) FROM user_escrow_keys WHERE user_id=$1 AND active_at IS NOT NULL", uid) != 0 {
		t.Fatal("activated by a refused paste")
	}

	// An Electrum-style paste: its own derivation entries for the cosigners (another fingerprint and path) and the
	// owner's signature. Accepted: the practice check takes only the owner's signature from a paste.
	signed, _ := decodePSBT(fakeOwnerSign(t, psbt, "7450f935", 1))
	raw, err := signed.withoutInputFields(func(_ int, key []byte) bool {
		return key[0] == 0x06 && !bytes.Equal(key[1:], owner)
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if bytes.Equal(k, owner) {
			continue
		}
		next, _ := decodePSBT(encodePSBT(raw))
		if raw, err = next.withInputField(0, append([]byte{0x06}, k...), []byte{0, 0, 0, 0, 7, 0, 0, 0, 9, 0, 0, 0}); err != nil {
			t.Fatal(err)
		}
	}
	if code, body := submitPractice(e, sess, encodePSBT(raw)); code != 303 {
		t.Fatalf("per-order practice: %d %s", code, body)
	}
	if agInt(e, "SELECT count(*) FROM user_escrow_keys WHERE user_id=$1 AND active_at IS NOT NULL AND layout='per_order'", uid) != 1 {
		t.Fatal("not active")
	}
	_, params := core.lastCall(t, "descriptorprocesspsbt")
	if !strings.Contains(params, `/2147483647/0/0`) || strings.Count(params, "tprv") != 1 || !strings.Contains(params, `"ALL",true,false]`) {
		t.Fatalf("throwaway signing params %s", params)
	}
	if strings.Contains(logs.String(), "tprv") {
		t.Fatal("a throwaway key reached the log")
	}
}

// P4-R2: the cosigner keys are Go's derivation, cross-checked with Core before anything is served. A node whose
// derivation disagrees gets no practice PSBT (the page says it cannot be built) and nothing is activated.
func TestEscrowPerOrderPracticeCrossCheck(t *testing.T) {
	logs := captureLog(t)
	e, core, _ := escrowTestApp(t)
	_, sess := e.user("per_order_crosscheck", "buyer")
	if code, body := registerKey(e, sess, testBIP48Key, "layout", "per_order"); code != 303 {
		t.Fatalf("register: %d %s", code, body)
	}
	core.mu.Lock()
	core.ckdSkew = true
	core.mu.Unlock()
	body := e.do("GET", "/escrow-key?practice=1", sess, nil).Body.String()
	if !strings.Contains(body, "cannot be built right now") || strings.Contains(body, "escrow-cosigner") || practiceArea.MatchString(body) {
		t.Fatalf("practice served despite a derivation mismatch: %s", body)
	}
	if !strings.Contains(logs.String(), "per-order-derivation-mismatch") {
		t.Fatalf("log: %s", logs.String())
	}
	if core.count("createpsbt") != 0 {
		t.Fatal("a PSBT was built after the cross-check failed")
	}
}

// P4-R18 and ES-9: switching layout means registering again and signing the practice again. The same key registered
// with the other layout is a change (inactive, practice in the new layout); the same key and layout again changes
// nothing; a new key's row takes the layout it was registered with.
func TestEscrowKeyLayoutSwitchNeedsPractice(t *testing.T) {
	e, _, _ := escrowTestApp(t)
	uid, sess := e.user("layout_switch", "buyer")
	state := func() (layout string, active bool) {
		t.Helper()
		if err := e.DB.QueryRow("SELECT layout,active_at IS NOT NULL FROM user_escrow_keys WHERE user_id=$1", uid).Scan(&layout, &active); err != nil {
			t.Fatal(err)
		}
		return
	}
	ownerPath := func(psbt string) string {
		t.Helper()
		p, err := decodePSBT(psbt)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range p.Inputs[0].Derivations {
			if hex.EncodeToString(d.Fingerprint[:]) == "7450f935" {
				return formatOriginPath(d.Path)
			}
		}
		return ""
	}
	if code, _ := registerKey(e, sess, testBIP48Key); code != 303 {
		t.Fatal("register core")
	}
	if p := ownerPath(practicePSBT(t, e, sess)); p != "/48h/1h/0h/2h/0/0" {
		t.Fatalf("core practice owner path %s", p)
	}
	if code, body := submitPractice(e, sess, fakeOwnerSign(t, practicePSBT(t, e, sess), "7450f935", 1)); code != 303 {
		t.Fatalf("core practice: %d %s", code, body)
	}
	if l, active := state(); l != "core" || !active {
		t.Fatalf("after core practice: %s %v", l, active)
	}
	// The same key and layout again: unchanged, still active.
	w := e.do("POST", "/escrow-key", sess, map[string][]string{"key": {testBIP48Key}, "password": {testPassword}, "layout": {"core"}})
	if w.Code != 303 || e.cookie(w, "notice") != "escrow-key-unchanged" {
		t.Fatalf("same key and layout: %d %q", w.Code, e.cookie(w, "notice"))
	}
	if _, active := state(); !active {
		t.Fatal("re-registering the same key and layout deactivated it")
	}
	// The same key as a per-order wallet key: a change, inactive until the shape-(b) practice is signed.
	if code, body := registerKey(e, sess, testBIP48Key, "layout", "per_order"); code != 303 {
		t.Fatalf("switch to per-order: %d %s", code, body)
	}
	if l, active := state(); l != "per_order" || active {
		t.Fatalf("after switching to per_order: %s active=%v", l, active)
	}
	audit := agStrings(t, e, "SELECT action FROM audit_events WHERE user_id=$1 ORDER BY id", uid)
	if audit[len(audit)-1] != "Changed Bitcoin escrow key (per-order wallet) (confirmed with password)" {
		t.Fatalf("audit %q", audit)
	}
	psbt := practicePSBT(t, e, sess)
	if p := ownerPath(psbt); p != "/48h/1h/0h/2h/2147483647/0/0" {
		t.Fatalf("per-order practice owner path %s", p)
	}
	if code, body := submitPractice(e, sess, fakeOwnerSign(t, psbt, "7450f935", 1)); code != 303 {
		t.Fatalf("per-order practice: %d %s", code, body)
	}
	if l, active := state(); l != "per_order" || !active {
		t.Fatalf("after per-order practice: %s %v", l, active)
	}
	// A new key registered with the Core layout: the row takes that layout and is inactive again.
	if code, body := registerKey(e, sess, altEscrowKey(t, testBIP48Key, "0badf00d", 3), "layout", "core"); code != 303 {
		t.Fatalf("new core key: %d %s", code, body)
	}
	if l, active := state(); l != "core" || active {
		t.Fatalf("after a new core key: %s active=%v", l, active)
	}
	if p := ownerPath(practicePSBT(t, e, sess)); p != "" { // the new key has fingerprint 0badf00d
		t.Fatalf("practice still for the old key: %s", p)
	}
}

// A key that is the key of existing escrows keeps their layout: registered again with the other layout it is refused
// (it would be active at once in a layout its practice never proved), whether it is recognised by its HMAC or, after an
// APP_SECRET_KEY rotation, by Core deriving the escrow's child key from it. Nothing changes.
func TestEscrowKeyWithEscrowsKeepsLayout(t *testing.T) {
	for _, rotated := range []bool{false, true} {
		name := "by HMAC"
		if rotated {
			name = "after a rotation"
		}
		t.Run(name, func(t *testing.T) {
			e, _, _ := escrowTestApp(t)
			uid, sess := e.user("layout_locked_"+randomToken()[:6], "buyer")
			if code, _ := registerKey(e, sess, testBIP48Key); code != 303 {
				t.Fatal("register")
			}
			e.DB.Exec("UPDATE user_escrow_keys SET active_at=now() WHERE user_id=$1", uid)
			insertEscrowFor(t, e, uid)
			if rotated { // another HMAC on record; the recorded child key is the one Core derives from the key at /0/1
				child := hex.EncodeToString(fakeChild(strings.SplitN(testBIP48Key, "]", 2)[1] + "/0/1"))
				if _, err := e.DB.Exec("UPDATE escrow_pubkeys SET key_hmac=repeat('ee',32),pubkey=$2 WHERE user_id=$1", uid, child); err != nil {
					t.Fatal(err)
				}
			}
			code, body := registerKey(e, sess, testBIP48Key, "layout", "per_order")
			if code != 409 || !strings.Contains(html.UnescapeString(body), "key of your existing escrows") {
				t.Fatalf("layout change of a key with escrows: %d %s", code, body)
			}
			var layout string
			var active bool
			e.DB.QueryRow("SELECT layout,active_at IS NOT NULL FROM user_escrow_keys WHERE user_id=$1", uid).Scan(&layout, &active)
			if layout != "core" || !active {
				t.Fatalf("after the refusal: %s active=%v", layout, active)
			}
		})
	}
}

// Until per-order escrows exist (A-456), a party whose active key is a per-order wallet key cannot enter an escrow:
// a shape-(a) escrow would use a child key (/0/<i>) the party's per-order wallet cannot sign. The order page says why
// and the checkout is refused with nothing written, whichever party it is.
func TestEscrowPerOrderKeyNotUsedForCheckout(t *testing.T) {
	for _, who := range []string{"buyer", "vendor"} {
		t.Run(who, func(t *testing.T) {
			x := newEscrowEnv(t)
			uid := x.buyer
			if who == "vendor" {
				uid = x.vendor
			}
			if _, err := x.DB.Exec("UPDATE user_escrow_keys SET layout='per_order' WHERE user_id=$1", uid); err != nil {
				t.Fatal(err)
			}
			page := html.UnescapeString(x.page("/order?id="+x.order, x.buyerSess))
			if !strings.Contains(page, "per-order wallet") || strings.Contains(page, "Pay into an escrow</button>") {
				t.Fatalf("order page offers escrow with a per-order key: %s", page)
			}
			w := x.checkout(x.order, "bcrt1qbuyerrefund00000000000000000000001")
			if w.Code != 409 || !strings.Contains(html.UnescapeString(w.Body.String()), "per-order wallet") {
				t.Fatalf("checkout: %d %s", w.Code, w.Body.String())
			}
			x.nothingWritten(x.order)
			if n := x.core.count("createpsbt") + x.core.count("importdescriptors"); n != 0 {
				t.Fatalf("%d node calls for a refused checkout", n)
			}
		})
	}
}

// P4-R18: the listing and vendor pages tell a buyer the vendor's key type before checkout (only while Bitcoin escrow is
// on): Bitcoin Core, a per-order wallet, or no active key.
func TestEscrowVendorKeyTypeShown(t *testing.T) {
	x := newEscrowEnv(t)
	_, other := x.user("keytype_reader", "buyer")
	for _, c := range []struct{ layout, active, want string }{
		{"core", "now()", "Bitcoin Core"},
		{"per_order", "now()", "Per-order wallet"},
		{"core", "NULL", "None active"},
	} {
		if _, err := x.DB.Exec("UPDATE user_escrow_keys SET layout=$2,active_at="+c.active+" WHERE user_id=$1", x.vendor, c.layout); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{"/product?id=" + x.product, "/vendor?id=" + x.vendor} {
			body := html.UnescapeString(x.page(path, other))
			if !strings.Contains(body, "Bitcoin escrow key") || !strings.Contains(body, c.want) {
				t.Errorf("%s with a %s key (active %s) lacks %q", path, c.layout, c.active, c.want)
			}
		}
	}
	if _, err := x.DB.Exec("DELETE FROM user_escrow_keys WHERE user_id=$1", x.vendor); err != nil {
		t.Fatal(err)
	}
	if body := x.page("/product?id="+x.product, other); !strings.Contains(body, "None active") {
		t.Error("product page without any vendor key lacks None active")
	}
	// With escrow off the pages say nothing about escrow keys.
	t.Setenv("ESCROW_BTC", "")
	off := newTestApp(t)
	v, _ := off.user("keytype_off_vendor", "vendor")
	pid := off.product(v, "digital")
	_, reader := off.user("keytype_off_reader", "buyer")
	for _, path := range []string{"/product?id=" + pid, "/vendor?id=" + v} {
		if body := off.do("GET", path, reader, nil).Body.String(); strings.Contains(body, "Bitcoin escrow key") {
			t.Errorf("%s mentions escrow keys with escrow off", path)
		}
	}
}
