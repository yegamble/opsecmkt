package market

import (
	"testing"
)

func TestBitcoinPriceLabelUsesKnownEscrowNetworkWithoutPooledBTC(t *testing.T) {
	knownNetwork := "main"
	tests := []struct {
		name       string
		configured bool
		network    *string
		live       bool
		want       string
	}{
		{name: "escrow mainnet", configured: true, network: &knownNetwork, want: "Price (BTC, main network)"},
		{name: "escrow network not yet known", configured: true, want: "Price (BTC, not yet known)"},
		{name: "no BTC even if XMR is live", live: true, want: "Price (BTC, not configured)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := &App{escrowConfigured: tc.configured,
				live: tc.live,
			}
			if tc.network != nil {
				a.escrowNet.Store(tc.network)
			}
			if got := a.bitcoinPriceLabel(); got != tc.want {
				t.Fatalf("bitcoinPriceLabel() = %q, want %q", got, tc.want)
			}
		})
	}
}
