package market

import (
	"strings"
	"testing"
)

// BITCOIN_CHAIN, when set beside ESCROW_BTC=on, may name an escrow chain only: testnet3 is refused before anything is
// contacted; main is the mainnet opt-in (A-460, TestEscrowConfigMainnet).
func TestEscrowConfigChains(t *testing.T) {
	for chain, ok := range map[string]bool{"": true, "regtest": true, "signet": true, "testnet4": true, "main": true, "test": false} {
		_, err := escrowConfigFromEnv(func(k string) string {
			return map[string]string{"ESCROW_BTC": "on", "ESCROW_BTC_RPC_URL": "http://a:b@h:1", "BITCOIN_CHAIN": chain}[k]
		})
		if (err == nil) != ok {
			t.Errorf("BITCOIN_CHAIN=%q: %v", chain, err)
		}
	}
}

// QA4W1-4: an address in capitals is lowered by the human-readable part of the escrow node's chain (the chain table),
// so a TB1 address on signet or testnet4 is stored in the one spelling the market compares and pays, as a BCRT1 one is
// on regtest. Another chain's prefix and a base58 address are left as given (the node then refuses or checks them).
func TestEscrowAddressFormChains(t *testing.T) {
	tb := segwitAddress("tb", 0, make([]byte, 20))
	bcrt := segwitAddress("bcrt", 0, make([]byte, 20))
	for _, tc := range []struct{ chain, in, want string }{
		{"regtest", strings.ToUpper(bcrt), bcrt},
		{"signet", strings.ToUpper(tb), tb},
		{"testnet4", strings.ToUpper(tb), tb},
		{"signet", " " + strings.ToUpper(tb) + " ", tb},
		{"regtest", strings.ToUpper(tb), strings.ToUpper(tb)},
		{"signet", strings.ToUpper(bcrt), strings.ToUpper(bcrt)},
		{"signet", "mipcBbFg9gMiCh81Kj8tqqdgoZub1ZJRfn", "mipcBbFg9gMiCh81Kj8tqqdgoZub1ZJRfn"},
	} {
		if got, ok := escrowAddressForm(tc.chain, tc.in); !ok || got != tc.want {
			t.Errorf("%s %q: %q %v, want %q", tc.chain, tc.in, got, ok, tc.want)
		}
	}
	for _, bad := range []string{"", "tb1q", strings.ToUpper(tb) + ")", "TB1Q" + strings.Repeat("Q", 90)} {
		if _, ok := escrowAddressForm("signet", bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}
