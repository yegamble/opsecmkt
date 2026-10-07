package market

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// A-449 (P4-R15): the address tags follow the network the escrow daemon was verified on, and the Monero sync guard's
// rule. Unit tests (no database).

// Each network type accepts exactly its own standard and subaddress tags: stagenet 5…/7…, fakechain 4…/8… (the mainnet
// format); integrated addresses are refused as such on their own network; mainnet and testnet are no escrow network,
// and an unverified network ("") accepts nothing.
func TestXMRAddressTagsPerNettype(t *testing.T) {
	addr := func(net string, kind int) string {
		return testXMRAddressWith(testXMRTags[net][kind], testXMRKey("tbl/s"), testXMRKey("tbl/v"), nil, false)
	}
	for _, c := range []struct {
		escrowNet, addrNet string
		kind               int // 0 standard, 1 integrated (network byte, 95 characters), 2 subaddress
		want               error
		first              byte
	}{
		{"stagenet", "stagenet", 0, nil, '5'},
		{"stagenet", "stagenet", 2, nil, '7'},
		{"stagenet", "stagenet", 1, errXMRAddressIntegrated, '5'},
		{"stagenet", "fakechain", 0, errXMRAddressNetwork, '4'},
		{"stagenet", "fakechain", 2, errXMRAddressNetwork, '8'},
		{"stagenet", "testnet", 0, errXMRAddressNetwork, '9'},
		{"stagenet", "testnet", 2, errXMRAddressNetwork, 'B'},
		{"fakechain", "fakechain", 0, nil, '4'},
		{"fakechain", "fakechain", 2, nil, '8'},
		{"fakechain", "fakechain", 1, errXMRAddressIntegrated, '4'},
		{"fakechain", "stagenet", 0, errXMRAddressNetwork, '5'},
		{"fakechain", "stagenet", 2, errXMRAddressNetwork, '7'},
		{"fakechain", "stagenet", 1, errXMRAddressNetwork, '5'},
		{"fakechain", "testnet", 2, errXMRAddressNetwork, 'B'},
		{"mainnet", "mainnet", 0, errXMRAddressNetwork, '4'},
		{"mainnet", "mainnet", 2, errXMRAddressNetwork, '8'},
		{"testnet", "testnet", 0, errXMRAddressNetwork, '9'},
		{"testnet", "testnet", 2, errXMRAddressNetwork, 'B'},
		{"", "fakechain", 0, errXMRAddressNetwork, '4'},
		{"", "stagenet", 2, errXMRAddressNetwork, '7'},
	} {
		s := addr(c.addrNet, c.kind)
		if s[0] != c.first {
			t.Fatalf("test vector %s/%d starts with %c", c.addrNet, c.kind, s[0])
		}
		a, err := parseXMRAddress(s, c.escrowNet)
		if !errors.Is(err, c.want) {
			t.Errorf("escrow on %q, %s address kind %d: %v, want %v", c.escrowNet, c.addrNet, c.kind, err, c.want)
		}
		if c.want == nil && (a.Kind != map[int]string{0: xmrStandard, 2: xmrSubaddress}[c.kind]) {
			t.Errorf("escrow on %q, %s kind %d: parsed as %q", c.escrowNet, c.addrNet, c.kind, a.Kind)
		}
		if err != nil && strings.Contains(err.Error(), s[:12]) {
			t.Errorf("the refusal quotes the address: %v", err)
		}
	}
	// The refusal names the escrow's own prefixes.
	if _, err := parseXMRAddress(addr("fakechain", 2), "stagenet"); err == nil || !strings.Contains(err.Error(), "start with 5 or 7") {
		t.Fatalf("stagenet refusal: %v", err)
	}
	if _, err := parseXMRAddress(addr("stagenet", 2), "fakechain"); err == nil || !strings.Contains(err.Error(), "start with 4 or 8") {
		t.Fatalf("fakechain refusal: %v", err)
	}
	// A 106-character integrated address of the escrow's network is refused as integrated by the form check.
	integrated := testXMRAddressWith(25, testXMRKey("tbl/s"), testXMRKey("tbl/v"), testXMRKey("pid")[:8], false)
	if err := checkXMRFormAddress(integrated, "stagenet"); !errors.Is(err, errXMRAddressIntegrated) {
		t.Fatalf("stagenet integrated form address: %v", err)
	}
}

// Real stagenet addresses, from a disposable monero-wallet-rpc 0.18.5.1 --stagenet wallet created for this test (its
// primary address, a create_account subaddress and make_integrated_address; no funds, public keys only), and the
// stagenet entries of Monero's tests/functional_tests/validate_address.py.
func TestXMRAddressStagenetVectors(t *testing.T) {
	for _, c := range []struct {
		addr, kind string
		err        error
	}{
		{"51v8eaPQVU3QGCHyM5xcH9geZCJvso4drBmjc2bbKWo7MsyWYeQNGAb12Uv22dwmcGgroVK4W7Fc2CDPKupxGpnEFG7acys", xmrStandard, nil},
		{"77HGaYh7ceyiopq1QWCzLUUj87iRwNC57N1sPEvwCuCTTmfR4CpDzN255JYP4GSSnZ23SZgpZZ8dgWNeCc8hKrrHFs2C1iU", xmrSubaddress, nil},
		{"5BcofPCu6jZQGCHyM5xcH9geZCJvso4drBmjc2bbKWo7MsyWYeQNGAb12Uv22dwmcGgroVK4W7Fc2CDPKupxGpnEN7fwnNmss8aD1j5p1r", "", errXMRAddressLength},
		{"53teqCAESLxeJ1REzGMAat1ZeHvuajvDiXqboEocPaDRRmqWoVPzy46GLo866qRFjbNhfkNckyhST3WEvBviDwpUDd7DSzB", xmrStandard, nil},
		{"73LhUiix4DVFMcKhsPRG51QmCsv8dYYbL6GcQoLwEEFvPvkVvc7BhebfA4pnEFF9Lq66hwvLqBvpHjTcqvpJMHmmNjPPBqa", xmrSubaddress, nil},
		{"7A1Hr63MfgUa8pkWxueD5xBqhQczkusYiCMYMnJGcGmuQxa7aDBxN1G7iCuLCNB3VPeb2TW7U9FdxB27xKkWKfJ8VhUZthF", xmrSubaddress, nil},
		{"5K8mwfjumVseCcQEjNbf59Um6R9NfVUNkHTLhhPCmNvgDLVS88YW5tScnm83rw9mfgYtchtDDTW5jEfMhygi27j1QYphX38hg6m4VMtN29", "", errXMRAddressLength},
	} {
		a, err := parseXMRAddress(c.addr, "stagenet")
		if !errors.Is(err, c.err) || a.Kind != c.kind {
			t.Errorf("%s… on stagenet: %q %v, want %q %v", c.addr[:8], a.Kind, err, c.kind, c.err)
		}
		if _, err := parseXMRAddress(c.addr, "fakechain"); err == nil {
			t.Errorf("%s… accepted on the fakechain", c.addr[:8])
		}
	}
	if err := checkXMRFormAddress("5BcofPCu6jZQGCHyM5xcH9geZCJvso4drBmjc2bbKWo7MsyWYeQNGAb12Uv22dwmcGgroVK4W7Fc2CDPKupxGpnEN7fwnNmss8aD1j5p1r", "stagenet"); !errors.Is(err, errXMRAddressIntegrated) {
		t.Fatalf("real stagenet integrated address: %v", err)
	}
}

// The Monero not-synced rule (stateless): synchronized false or target_height above height on every network type;
// on stagenet (any public network type) also no connections or a tip older than the bound; the fakechain is exempt from
// those two.
func TestMoneroNodeNotSynced(t *testing.T) {
	now := time.Now()
	fresh := now.Add(-5 * time.Minute).Unix()
	for _, c := range []struct {
		n    moneroNode
		want string
	}{
		{moneroNode{NetType: "stagenet", Synchronized: true, Height: 100, Connections: 3, TipTime: fresh}, ""},
		{moneroNode{NetType: "stagenet", Synchronized: true, Height: 100, TargetHeight: 100, Connections: 3, TipTime: fresh}, ""},
		{moneroNode{NetType: "stagenet", Synchronized: false, Height: 100, Connections: 3, TipTime: fresh}, xmrNotSyncedFlag},
		{moneroNode{NetType: "stagenet", Synchronized: true, Height: 100, TargetHeight: 101, Connections: 3, TipTime: fresh}, xmrNotSyncedTarget},
		{moneroNode{NetType: "stagenet", Synchronized: true, Height: 100, Connections: 0, TipTime: fresh}, notSyncedPeers},
		{moneroNode{NetType: "stagenet", Synchronized: true, Height: 100, Connections: 3, TipTime: now.Add(-61 * time.Minute).Unix()}, xmrNotSyncedTip},
		{moneroNode{NetType: "testnet", Synchronized: true, Height: 100, Connections: 0, TipTime: fresh}, notSyncedPeers},
		{moneroNode{NetType: "mainnet", Synchronized: true, Height: 100, Connections: 3, TipTime: 0}, xmrNotSyncedTip},
		{moneroNode{NetType: "stagenet", Synchronized: true, Height: 100, Connections: 3, Offline: true, TipTime: fresh}, notSyncedPeers},
		// A restricted RPC reports 0 connections whatever its peers: offline and the tip's age decide.
		{moneroNode{NetType: "stagenet", Synchronized: true, Height: 100, Restricted: true, Connections: 0, TipTime: fresh}, ""},
		{moneroNode{NetType: "stagenet", Synchronized: true, Height: 100, Restricted: true, Offline: true, TipTime: fresh}, notSyncedPeers},
		{moneroNode{NetType: "stagenet", Synchronized: true, Height: 100, Restricted: true, TipTime: now.Add(-61 * time.Minute).Unix()}, xmrNotSyncedTip},
		{moneroNode{NetType: "stagenet", Synchronized: true, Height: 100, Restricted: true, TipTime: -1}, xmrNotSyncedTipBad},
		{moneroNode{NetType: "stagenet", Synchronized: true, Height: 100, Connections: 2, TipTime: -1}, xmrNotSyncedTipBad},
		{moneroNode{NetType: "fakechain", Synchronized: true, Height: 100, Connections: 0, TipTime: 0}, ""},
		{moneroNode{NetType: "fakechain", Synchronized: true, Height: 100, Offline: true, TipTime: 0}, ""},
		{moneroNode{NetType: "fakechain", Synchronized: false, Height: 100}, xmrNotSyncedFlag},
		{moneroNode{NetType: "fakechain", Synchronized: true, Height: 100, TargetHeight: 140}, xmrNotSyncedTarget},
	} {
		if got := c.n.notSynced(now, time.Hour); got != c.want {
			t.Errorf("%+v: %q, want %q", c.n, got, c.want)
		}
	}
}

// MONERO_MAX_TIP_AGE: a Go duration from 20m to 24h, 1h when blank, as BITCOIN_MAX_TIP_AGE is parsed.
func TestParseMoneroMaxTipAge(t *testing.T) {
	for in, want := range map[string]time.Duration{"": time.Hour, " ": time.Hour, "20m": 20 * time.Minute, "90m": 90 * time.Minute, "24h": 24 * time.Hour} {
		if got, err := parseMoneroMaxTipAge(in); err != nil || got != want {
			t.Errorf("%q: %v %v, want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"19m59s", "0", "-1h", "25h", "1 hour", "abc"} {
		if _, err := parseMoneroMaxTipAge(in); err == nil || !strings.Contains(err.Error(), "MONERO_MAX_TIP_AGE") {
			t.Errorf("%q accepted: %v", in, err)
		}
	}
}
