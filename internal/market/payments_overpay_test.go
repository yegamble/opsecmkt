package market

import "testing"

func TestSplitPayoutRules(t *testing.T) {
	o := &Order{BuyerID: "b", VendorID: "v", Currency: "BTC"}
	const price = 100000
	const xmrPrice = 500000000000 // 0.5 XMR
	for _, tc := range []struct {
		name, kind, currency string
		sum                  int64
		want                 []payoutPart
	}{
		{"exact release", "release", "BTC", price, []payoutPart{{"release", "v", "vendor", price, false}}},
		{"underpaid release", "release", "BTC", 90000, []payoutPart{{"release", "v", "vendor", 90000, false}}},
		{"excess below the floor stays with the vendor", "release", "BTC", price + minPayoutBTC - 1, []payoutPart{{"release", "v", "vendor", price + minPayoutBTC - 1, false}}},
		{"excess at the floor is returned", "release", "BTC", price + minPayoutBTC, []payoutPart{{"release", "v", "vendor", price, false}, {"return", "b", "buyer", minPayoutBTC, false}}},
		{"excess equal to the price is returned", "release", "BTC", 2 * price, []payoutPart{{"release", "v", "vendor", price, false}, {"return", "b", "buyer", price, false}}},
		{"excess above the price is held", "release", "BTC", 2*price + 1, []payoutPart{{"release", "v", "vendor", price, false}, {"return", "b", "buyer", price + 1, true}}},
		{"refund up to the price", "refund", "BTC", price, []payoutPart{{"refund", "b", "buyer", price, false}}},
		{"refund above the price is held", "refund", "BTC", price + 1, []payoutPart{{"refund", "b", "buyer", price + 1, true}}},
		{"XMR excess below its floor stays with the vendor", "release", "XMR", xmrPrice + minPayoutXMR - 1, []payoutPart{{"release", "v", "vendor", xmrPrice + minPayoutXMR - 1, false}}},
		{"XMR excess at its floor is returned", "release", "XMR", xmrPrice + minPayoutXMR, []payoutPart{{"release", "v", "vendor", xmrPrice, false}, {"return", "b", "buyer", minPayoutXMR, false}}},
		{"XMR excess above the price is held", "release", "XMR", 2*xmrPrice + 1, []payoutPart{{"release", "v", "vendor", xmrPrice, false}, {"return", "b", "buyer", xmrPrice + 1, true}}},
		{"XMR refund above the price is held", "refund", "XMR", xmrPrice + 1, []payoutPart{{"refund", "b", "buyer", xmrPrice + 1, true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o.Currency = tc.currency
			required := int64(price)
			if tc.currency == "XMR" {
				required = xmrPrice
			}
			got := splitPayout(tc.kind, o, tc.sum, required)
			if len(got) != len(tc.want) {
				t.Fatalf("got %+v", got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("part %d: got %+v want %+v", i, got[i], tc.want[i])
				}
			}
			var total int64
			for _, part := range got {
				total += part.amount
			}
			if total != tc.sum {
				t.Fatalf("parts pay %d of %d", total, tc.sum)
			}
		})
	}
}
