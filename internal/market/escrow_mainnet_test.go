package market

import (
	"context"
	"encoding/binary"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"opsecmkt/internal/btc"
)

// A-460: Bitcoin escrow on mainnet as the operator's opt-in (BITCOIN_CHAIN=main). Pure-function tests use published
// vectors; the startup and fee-index tests run against the simulated Core (escrow_fake_test.go: simulated RPC, never a
// mainnet node or real funds). No test here contacts a network.

// bip84Vector is BIP 84's test vector (mnemonic "abandon … about"): the account m/84'/0'/0' as an xpub (its zpub with
// xpub version bytes), the master fingerprint, and its first two receiving addresses.
const (
	bip84VectorZpub  = "zpub6rFR7y4Q2AijBEqTUquhVz398htDFrtymD9xYYfG1m4wAcvPhXNfE3EfH1r1ADqtfSdVCToUG868RvUUkgDKf31mGDtKsAYz2oz2AGutZYs"
	bip84VectorFP    = "73c5da0a"
	bip84VectorAddr0 = "bc1qcr8te4kr609gcawutmrza0j4xv80jy8z306fyu"
	bip84VectorAddr1 = "bc1qnjg0jd8228aq7egyzacy8cys3knf9xvrerkf9g"
)

// withVersion re-encodes an extended key with other version bytes.
func withVersion(t *testing.T, key string, version uint32) string {
	t.Helper()
	raw, err := base58CheckDecode(key)
	if err != nil || len(raw) != 78 {
		t.Fatalf("decode %q: %v", key, err)
	}
	binary.BigEndian.PutUint32(raw[:4], version)
	return base58CheckEncode(raw)
}

// mainnetExpr is a test-network key expression [fp/path]tpub… as a mainnet one: xpub version bytes and coin type 0h.
func mainnetExpr(t *testing.T, expr string) string {
	t.Helper()
	end := strings.IndexByte(expr, ']')
	origin := strings.Replace(expr[:end+1], "h/1h/", "h/0h/", 1)
	return origin + withVersion(t, expr[end+1:], btc.VersionXpub)
}

func bip84VectorExpr(t *testing.T) string {
	return "[" + bip84VectorFP + "/84h/0h/0h]" + withVersion(t, bip84VectorZpub, btc.VersionXpub)
}

func TestMarketLiveFromEnv(t *testing.T) {
	for _, tc := range []struct {
		chain, xmr string
		live, bad  bool
	}{
		{"", "", false, false}, {"signet", "stagenet", false, false}, {"main", "", true, false}, {"", "mainnet", true, false},
		{"main", "mainnet", true, false}, {"main", "stagenet", false, true}, {"signet", "mainnet", false, true}, {"test", "", false, false},
	} {
		live, err := marketLiveFromEnv(func(k string) string {
			return map[string]string{"BITCOIN_CHAIN": tc.chain, "MONERO_NETWORK": tc.xmr}[k]
		})
		if live != tc.live || (err != nil) != tc.bad || tc.bad && !errors.Is(err, errMixedNetworks) {
			t.Errorf("BITCOIN_CHAIN=%q MONERO_NETWORK=%q: live %v, %v", tc.chain, tc.xmr, live, err)
		}
	}
}

// A mainnet market takes xpub and Zpub keys on m/48h/0h/<account>h/2h, stores them as xpubs, and refuses a test-network
// key naming it; a test-network market refuses a mainnet key as before.
func TestEscrowKeyClasses(t *testing.T) {
	main := mainnetExpr(t, testBIP48Key)
	k, err := parseEscrowKey(main, keyNetMain)
	if err != nil || !k.BIP48 || !strings.HasPrefix(k.XPub, "xpub") || k.PathText != "/48h/0h/0h/2h" {
		t.Fatalf("mainnet key: %+v %v", k, err)
	}
	end := strings.IndexByte(main, ']')
	zpub := main[:end+1] + withVersion(t, main[end+1:], btc.VersionZpub)
	if z, err := parseEscrowKey(zpub, keyNetMain); err != nil || z.XPub != k.XPub {
		t.Fatalf("Zpub: %+v %v", z, err)
	}
	if _, err := parseEscrowKey(testBIP48Key, keyNetMain); err != errKeyTestnet {
		t.Fatalf("tpub on mainnet: %v", err)
	}
	if _, err := parseEscrowKey(main, keyNetTest); err != errKeyMainnet {
		t.Fatalf("xpub on a test network: %v", err)
	}
	// The test network's coin type is not BIP48 on mainnet (and the reverse).
	coin1 := "[7450f935/48h/1h/0h/2h]" + main[end+1:]
	if k, err := parseEscrowKey(coin1, keyNetMain); err != nil || k.BIP48 {
		t.Fatalf("coin type 1 on mainnet: %+v %v", k, err)
	}
	ypub := main[:end+1] + withVersion(t, main[end+1:], 0x049d7cb2)
	if _, err := parseEscrowKey(ypub, keyNetMain); err != errKeySingleSig || !strings.Contains(keyRefusalText(err, true), "ypub or zpub") {
		t.Fatalf("ypub: %v", err)
	}
	if got := keyRefusalText(errKeyPerOrderPath, true); !strings.Contains(got, "m/48h/0h/0h/2h") {
		t.Fatalf("mainnet path text: %s", got)
	}
	if got := keyRefusalText(errKeyPerOrderPath, false); !strings.Contains(got, "m/48h/1h/0h/2h") {
		t.Fatalf("test path text: %s", got)
	}
}

// Mainnet addresses: bc1 segwit, base58 versions 0x00 and 0x05; test-network ones are refused there.
func TestEscrowAddressScriptMainnet(t *testing.T) {
	if spk, err := escrowAddressScript("main", bip84VectorAddr0); err != nil || len(spk) != 22 || spk[0] != 0 {
		t.Fatalf("bc1: %x %v", spk, err)
	}
	if spk, err := escrowAddressScript("main", "1BvBMSEYstWetqTFn5Au4m4GFg7xJaNVN2"); err != nil || spk[0] != 0x76 {
		t.Fatalf("P2PKH: %x %v", spk, err)
	}
	if spk, err := escrowAddressScript("main", "3J98t1WpEZ73CNmQviecrnyiWrnqRhWNLy"); err != nil || spk[0] != 0xa9 {
		t.Fatalf("P2SH: %x %v", spk, err)
	}
	for _, addr := range []string{"mipcBbFg9gMiCh81Kj8tqqdgoZub1ZJRfn", segwitAddress("tb", 0, make([]byte, 20))} {
		if _, err := escrowAddressScript("main", addr); err == nil {
			t.Errorf("%s accepted on mainnet", addr)
		}
	}
	if _, err := escrowAddressScript("signet", "1BvBMSEYstWetqTFn5Au4m4GFg7xJaNVN2"); err == nil {
		t.Fatal("a mainnet P2PKH address accepted on signet")
	}
}

func TestEscrowChainAllowed(t *testing.T) {
	test, live := &App{}, &App{live: true}
	for _, chain := range []string{"regtest", "signet", "testnet4"} {
		if !test.escrowChainAllowed(chain) || live.escrowChainAllowed(chain) {
			t.Errorf("%s", chain)
		}
	}
	if test.escrowChainAllowed("main") || !live.escrowChainAllowed("main") || live.escrowChainAllowed("test") || test.escrowChainAllowed("test") {
		t.Fatal("main/test")
	}
}

// The fee descriptor's addresses are BIP 84's: the account key's /0/<index>, on the chain's human-readable part.
func TestFeeDescriptorAddressVector(t *testing.T) {
	k, err := parseFeeDescriptor("wpkh("+bip84VectorExpr(t)+"/0/*)", keyNetMain)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{bip84VectorAddr0, bip84VectorAddr1} {
		if got, err := feeDescriptorAddress(k, "main", uint32(i)); err != nil || got != want {
			t.Errorf("index %d: %s %v", i, got, err)
		}
	}
	if _, err := feeDescriptorAddress(k, "test", 0); err == nil {
		t.Fatal("testnet3 gave an address")
	}
	// The forms: with its checksum (which must be right), the bare key, and nothing else.
	desc := "wpkh(" + bip84VectorExpr(t) + "/0/*)"
	for in, ok := range map[string]bool{
		withChecksum(desc):                     true,
		desc + "#qqqqqqqq":                     false,
		bip84VectorExpr(t):                     true,
		"wpkh(" + bip84VectorExpr(t) + "/1/*)": false,
		"wsh(" + bip84VectorExpr(t) + "/0/*)":  false,
		bip84VectorAddr0:                       false,
	} {
		if _, err := parseFeeDescriptor(in, keyNetMain); (err == nil) != ok {
			t.Errorf("%q: %v", in, err)
		}
	}
	if _, err := parseFeeDescriptor(desc, keyNetTest); err == nil {
		t.Fatal("a mainnet descriptor accepted on a test network")
	}
}

// ESCROW_BTC's configuration on a mainnet market: BITCOIN_CHAIN=main is required beside MONERO_NETWORK=mainnet, and the
// market fee must go to a descriptor.
func TestEscrowConfigMainnet(t *testing.T) {
	desc := "wpkh(" + bip84VectorExpr(t) + "/0/*)"
	for _, tc := range []struct {
		env map[string]string
		ok  bool
	}{
		{map[string]string{"BITCOIN_CHAIN": "main"}, true},
		{map[string]string{"MONERO_NETWORK": "mainnet"}, false},
		{map[string]string{"BITCOIN_CHAIN": "main", "ESCROW_BTC_FEE_BPS": "250", "ESCROW_BTC_FEE_ADDRESS": bip84VectorAddr0}, false},
		{map[string]string{"BITCOIN_CHAIN": "main", "ESCROW_BTC_FEE_BPS": "250", "ESCROW_BTC_FEE_ADDRESS": desc}, true},
		{map[string]string{"BITCOIN_CHAIN": "signet", "ESCROW_BTC_FEE_BPS": "250", "ESCROW_BTC_FEE_ADDRESS": desc}, false},
		{map[string]string{"BITCOIN_CHAIN": "signet", "ESCROW_BTC_FEE_BPS": "250", "ESCROW_BTC_FEE_ADDRESS": "wpkh(" + testBIP84Key + "/0/*)"}, true},
		{map[string]string{"BITCOIN_CHAIN": "test"}, false},
	} {
		c, err := escrowConfigFromEnv(func(k string) string {
			if v, ok := tc.env[k]; ok {
				return v
			}
			return map[string]string{"ESCROW_BTC": "on", "ESCROW_BTC_RPC_URL": "http://a:b@h:1"}[k]
		})
		if (err == nil) != tc.ok {
			t.Errorf("%v: %v", tc.env, err)
		}
		if err == nil && tc.env["ESCROW_BTC_FEE_ADDRESS"] != "" && c.feeAddress != tc.env["ESCROW_BTC_FEE_ADDRESS"] {
			t.Errorf("%v: fee %q", tc.env, c.feeAddress)
		}
	}
}

// mainnetCore makes the simulated Core a mainnet node with a mainnet arbiter wallet (an xpub on 84h/0h/0h), peers
// and a fresh tip.
func mainnetCore(t *testing.T, c *fakeEscrowCore) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.chain, c.peers, c.tipTime = "main", 8, time.Now().Unix()
	c.arbiterDesc = "wpkh(" + mainnetExpr(t, fakeArbiterExpr()) + "/1/*)#fakechk0"
}

// Startup: a mainnet node runs escrow only with BITCOIN_CHAIN=main, and then with mainnet keys and the chain pinned
// main; BITCOIN_CHAIN=main with a test-network node is refused; a database holding test-network escrows refuses a
// mainnet market, before any node is contacted.
func TestEscrowStartupMainnet(t *testing.T) {
	t.Run("opted-in", func(t *testing.T) {
		core, srv := newFakeEscrowCore(t)
		mainnetCore(t, core)
		setEscrowEnv(t, srv.url(""))
		t.Setenv("BITCOIN_CHAIN", "main")
		a, _, err := startApp(t, "")
		if err != nil {
			t.Fatal(err)
		}
		e := a.escrow.Load()
		if !a.live || e == nil || e.network != "main" || !strings.HasPrefix(e.arbiterKey.XPub, "xpub") || e.arbiterKey.PathText != "/84h/0h/0h" {
			t.Fatalf("live %v escrow %+v", a.live, e)
		}
		var pin string
		if err := a.db.QueryRow("SELECT value FROM settings WHERE key=$1", escrowChainPin).Scan(&pin); err != nil || pin != "main" {
			t.Fatalf("pin %q %v", pin, err)
		}
		if a.netTag() != "MAINNET" {
			t.Fatalf("tag %s", a.netTag())
		}
	})
	t.Run("node-on-test-chain", func(t *testing.T) {
		core, srv := newFakeEscrowCore(t)
		_ = core
		setEscrowEnv(t, srv.url(""))
		t.Setenv("BITCOIN_CHAIN", "main")
		if _, _, err := startApp(t, ""); !errors.Is(err, errEscrowChain) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("test-network-database", func(t *testing.T) {
		_, srv := newFakeEscrowCore(t)
		setEscrowEnv(t, srv.url(""))
		a, dsn, err := startApp(t, "")
		if err != nil {
			t.Fatal(err)
		}
		a.Close()
		t.Setenv("BITCOIN_CHAIN", "main")
		// A pin alone (no escrow yet) does not hold the class: the market may still move to mainnet (here with Bitcoin
		// escrow off, as the node is still on regtest).
		t.Setenv("ESCROW_BTC", "off")
		b, _, err := startApp(t, dsn)
		if err != nil {
			t.Fatalf("pin without escrows: %v", err)
		}
		b.Close()
		t.Setenv("BITCOIN_CHAIN", "")
		t.Setenv("ESCROW_BTC", "on")
		c, _, err := startApp(t, dsn)
		if err != nil {
			t.Fatal(err)
		}
		e := &testEnv{t: t, A: c, DB: c.db}
		buyer, _ := e.user("class_buyer", "buyer")
		vendor, _ := e.user("class_vendor", "vendor")
		order := e.order(buyer, e.product(vendor, "physical"), "BTC", stateDraft)
		if _, err := c.db.Exec("INSERT INTO escrows(order_id,address,market_index,refund_address,fee_bps,fee_destination) VALUES($1,'bcrt1qclass',nextval('escrow_market_index'),'bcrt1qclassrefund',0,'')", order); err != nil {
			t.Fatal(err)
		}
		c.Close()
		t.Setenv("BITCOIN_CHAIN", "main")
		if _, _, err := startApp(t, dsn); err == nil || !strings.Contains(err.Error(), "escrows on a test network") {
			t.Fatalf("got %v", err)
		}
	})
}

// The fee index (A-460): a descriptor fee escrow takes one index at its first release build and keeps it on every
// rebuild; the next escrow takes the next; the address is Go's and Core's agreed derivation, stored on the escrow; an
// address Core does not confirm stores nothing. On regtest (a descriptor is allowed on any chain).
func TestEscrowFeeDescriptorIndex(t *testing.T) {
	desc := "wpkh(" + testBIP84Key + "/0/*)"
	x := newEscrowEnv(t, "ESCROW_BTC_FEE_BPS", "250", "ESCROW_BTC_FEE_ADDRESS", desc)
	e := x.A.escrow.Load()
	if e == nil || !isSealedFeeDescriptor(e.feeAddress) {
		t.Fatalf("fee destination not sealed: %v", e)
	}
	k, err := parseFeeDescriptor(desc, keyNetTest)
	if err != nil {
		t.Fatal(err)
	}
	agree := true
	x.core.mu.Lock()
	x.core.deriveHook = func(body string) (string, bool) {
		rest, ok := strings.CutPrefix(body, "wpkh("+k.Expression()+"/0/")
		if !ok {
			return "", false
		}
		var i uint32
		for _, c := range strings.TrimSuffix(rest, ")") {
			i = i*10 + uint32(c-'0')
		}
		addr, _ := feeDescriptorAddress(k, "regtest", i)
		if !agree {
			addr = segwitAddress("bcrt", 0, make([]byte, 20))
		}
		return addr, true
	}
	x.core.mu.Unlock()
	order := x.draft()
	x.mustCheckout(order, x.netAddr("refund-1"))
	var sealed, dest string
	if err := x.DB.QueryRow("SELECT COALESCE(fee_descriptor,''),fee_destination FROM escrows WHERE order_id=$1", order).Scan(&sealed, &dest); err != nil || !isSealedFeeDescriptor(sealed) || dest != "" {
		t.Fatalf("checkout snapshot %q %q %v", sealed, dest, err)
	}
	ctx := context.Background()
	first, err := x.A.escrowFeeAddress(ctx, e, order)
	want, _ := feeDescriptorAddress(k, "regtest", 0)
	if err != nil || first != want {
		t.Fatalf("first build: %q %v, want %q", first, err, want)
	}
	if again, err := x.A.escrowFeeAddress(ctx, e, order); err != nil || again != first {
		t.Fatalf("rebuild: %q %v", again, err)
	}
	other := x.draft()
	x.mustCheckout(other, x.netAddr("refund-2"))
	agree = false
	if _, err := x.A.escrowFeeAddress(ctx, e, other); !errors.Is(err, errFeeDescUnusable) {
		t.Fatalf("disagreement: %v", err)
	}
	var stored string
	var index int64
	if err := x.DB.QueryRow("SELECT fee_destination,fee_index FROM escrows WHERE order_id=$1", other).Scan(&stored, &index); err != nil || stored != "" || index != 1 {
		t.Fatalf("after a disagreement: %q %d %v", stored, index, err)
	}
	agree = true
	second, err := x.A.escrowFeeAddress(ctx, e, other)
	want1, _ := feeDescriptorAddress(k, "regtest", 1)
	if err != nil || second != want1 {
		t.Fatalf("second escrow: %q %v", second, err)
	}
	if high, err := highestFeeIndex(ctx, x.DB); err != nil || high != 1 {
		t.Fatalf("highest %d %v", high, err)
	}
	// The admin page shows the descriptor's fingerprint and the highest index, never the sealed value.
	_, admin := x.user("feedesc_admin", "admin")
	body := x.page("/admin", admin)
	if !strings.Contains(body, k.Fingerprint) || !strings.Contains(body, "Highest fee index used: <strong>1</strong>") || strings.Contains(body, escrowFeeDescPrefix) {
		t.Fatalf("admin page:\n%s", body)
	}
}

// On a mainnet market the admin fee form refuses a plain address and saves a descriptor sealed; the audit row and the
// admin page never show the descriptor, only its fingerprint.
func TestEscrowFeeActionMainnet(t *testing.T) {
	core, srv := newFakeEscrowCore(t)
	mainnetCore(t, core)
	setEscrowEnv(t, srv.url(""))
	t.Setenv("BITCOIN_CHAIN", "main")
	a, _, err := startApp(t, "")
	if err != nil || a.escrow.Load() == nil {
		t.Fatalf("mainnet escrow: %v", err)
	}
	e := &testEnv{t: t, A: a, DB: a.db}
	_, admin := e.user("mainfee_admin", "admin")
	plain := e.do("POST", "/admin/escrow-fee", admin, url.Values{"fee_bps": {"250"}, "fee_address": {bip84VectorAddr0}, "password": {testPassword}})
	if plain.Code != 400 || !strings.Contains(plain.Body.String(), "watch-only descriptor") {
		t.Fatalf("plain address: %d %s", plain.Code, plain.Body.String())
	}
	tpub := e.do("POST", "/admin/escrow-fee", admin, url.Values{"fee_bps": {"250"}, "fee_address": {"wpkh(" + testBIP84Key + "/0/*)"}, "password": {testPassword}})
	if tpub.Code != 400 {
		t.Fatalf("test-network descriptor: %d", tpub.Code)
	}
	if agInt(e, "SELECT count(*) FROM settings WHERE key LIKE 'escrow_btc_fee_%'") != 0 {
		t.Fatal("a refused save was stored")
	}
	desc := withChecksum("wpkh(" + bip84VectorExpr(t) + "/0/*)")
	if w := e.do("POST", "/admin/escrow-fee", admin, url.Values{"fee_bps": {"250"}, "fee_address": {desc}, "password": {testPassword}}); w.Code != 303 {
		t.Fatalf("descriptor: %d %s", w.Code, w.Body.String())
	}
	var stored string
	if err := a.db.QueryRow("SELECT value FROM settings WHERE key=$1", escrowFeeAddrSetting).Scan(&stored); err != nil || !isSealedFeeDescriptor(stored) || strings.Contains(stored, "xpub") {
		t.Fatalf("stored %q %v", stored, err)
	}
	if agInt(e, "SELECT count(*) FROM audit_events WHERE action LIKE '%xpub%' OR action LIKE '%wpkh%'") != 0 {
		t.Fatal("the descriptor is in the audit log")
	}
	if w := e.do("POST", "/admin/escrow-fee", admin, url.Values{"fee_bps": {"250"}, "fee_address": {bip84VectorExpr(t)}, "password": {testPassword}}); w.Code != 409 {
		t.Fatalf("the same key again: %d", w.Code)
	}
	page := e.do("GET", "/admin", admin, nil).Body.String()
	if !strings.Contains(page, bip84VectorFP) || strings.Contains(page, "xpub6") || !strings.Contains(page, "must go to a watch-only descriptor") {
		t.Fatalf("admin page:\n%s", page)
	}
}

// A fee index whose address the market already used (here a settlement's destination) is skipped: the escrow moves to
// the next index. The arbiter's own key is refused as the fee descriptor, at startup and on the admin form.
func TestEscrowFeeDescriptorSkipsUsedAndArbiter(t *testing.T) {
	desc := "wpkh(" + testBIP84Key + "/0/*)"
	x := newEscrowEnv(t, "ESCROW_BTC_FEE_BPS", "250", "ESCROW_BTC_FEE_ADDRESS", desc)
	k, _ := parseFeeDescriptor(desc, keyNetTest)
	x.core.mu.Lock()
	x.core.deriveHook = func(body string) (string, bool) {
		rest, ok := strings.CutPrefix(body, "wpkh("+k.Expression()+"/0/")
		if !ok {
			return "", false
		}
		var i uint32
		for _, c := range strings.TrimSuffix(rest, ")") {
			i = i*10 + uint32(c-'0')
		}
		addr, _ := feeDescriptorAddress(k, "regtest", i)
		return addr, true
	}
	x.core.mu.Unlock()
	used, _ := feeDescriptorAddress(k, "regtest", 0)
	// Index 0's address is already a refund address on this market.
	prior := x.draft()
	x.mustCheckout(prior, used)
	order := x.draft()
	x.mustCheckout(order, x.netAddr("skip-refund"))
	got, err := x.A.escrowFeeAddress(context.Background(), x.A.escrow.Load(), order)
	want, _ := feeDescriptorAddress(k, "regtest", 1)
	if err != nil || got != want {
		t.Fatalf("fee address %q %v, want index 1's %q", got, err, want)
	}

	// The arbiter's key as the fee descriptor: refused at startup.
	core, srv := newFakeEscrowCore(t)
	_ = core
	setEscrowEnv(t, srv.url(""))
	t.Setenv("ESCROW_BTC_FEE_BPS", "250")
	t.Setenv("ESCROW_BTC_FEE_ADDRESS", "wpkh("+fakeArbiterExpr()+"/0/*)")
	if _, _, err := startApp(t, ""); !errors.Is(err, errEscrowFeeAddress) {
		t.Fatalf("arbiter key at startup: %v", err)
	}
	// And on the admin form.
	_, admin := x.user("feearb_admin", "admin")
	w := x.do("POST", "/admin/escrow-fee", admin, url.Values{"fee_bps": {"250"}, "fee_address": {"wpkh(" + fakeArbiterExpr() + "/0/*)"}, "password": {testPassword}})
	if w.Code != 400 || !strings.Contains(w.Body.String(), "arbiter key") {
		t.Fatalf("arbiter key on the admin form: %d %s", w.Code, w.Body.String())
	}
}
