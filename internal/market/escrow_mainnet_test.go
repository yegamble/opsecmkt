package market

import (
	"encoding/binary"

	"strings"
	"testing"
	"time"

	"opsecmkt/internal/btc"
)

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
		{"", "", false, false}, {"signet", "stagenet", false, true}, {"main", "", false, true}, {"", "mainnet", true, false},
		{"main", "mainnet", false, true}, {"main", "stagenet", false, true}, {"signet", "mainnet", false, true}, {"test", "", false, true},
	} {
		live, err := marketLiveFromEnv(func(k string) string {
			return map[string]string{"BITCOIN_CHAIN": tc.chain, "MONERO_NETWORK": tc.xmr}[k]
		})
		if live != tc.live || (err != nil) != tc.bad {
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
