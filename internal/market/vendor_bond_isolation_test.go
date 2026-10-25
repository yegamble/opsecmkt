package market

import (
	"context"
	"html"
	"strconv"
	"strings"
	"testing"
)

func bondIsolationQuote(e *testEnv, vendor, address string) {
	e.t.Helper()
	agExec(e, `INSERT INTO vendor_bond_quotes(id,vendor_id,reference_currency,amount,coin,crypto_amount,address,provider_reference)
		VALUES($1,$2,'BTC',100000,'BTC',100000,$3,$4)`, randomToken(), vendor, address, randomToken())
}

// Buyer escrow payouts must not be directed to the operator's bond receiving
// addresses. Existing funded orders remain fulfillable when the bond is absent.
func TestVendorBondBTCClaimAndShippingRejectBondDestinations(t *testing.T) {
	for _, leg := range []string{"claim", "ship"} {
		t.Run(leg, func(t *testing.T) {
			x := newEscrowEnv(t)
			var order string
			if leg == "claim" {
				order = x.completed(regtestAddr("bond-isolation-refund-claim"), 100000)
			} else {
				order = x.paidPhysical(regtestAddr("bond-isolation-refund-ship"), 100000)
			}
			bond := regtestAddr("operator bond receiving " + leg)
			bondIsolationQuote(x.testEnv, x.vendor, bond)
			bondPolicyTest(x.testEnv, 100000, "BTC")
			var code int
			var body string
			if leg == "claim" {
				code, body = x.claim(order, x.vendorSess, bond)
			} else {
				code, body = x.shipEscrow(order, x.vendorSess, bond, testPassword)
			}
			if code != 409 || !strings.Contains(html.UnescapeString(body), errClaimAddressUsed.Error()) {
				t.Fatalf("bond %s destination accepted: %d %s", leg, code, body)
			}
			if agInt(x.testEnv, "SELECT count(*) FROM settlements WHERE order_id=$1", order) != 0 {
				t.Fatal("refused bond destination wrote a settlement")
			}
			fresh := regtestAddr("vendor own wallet " + leg)
			if leg == "claim" {
				code, body = x.claim(order, x.vendorSess, fresh)
			} else {
				code, body = x.shipEscrow(order, x.vendorSess, fresh, testPassword)
			}
			if code != 303 {
				t.Fatalf("existing funded order blocked by absent bond: %d %s", code, body)
			}
			if agStr(x.testEnv, "SELECT destination FROM settlements WHERE order_id=$1", order) != fresh {
				t.Fatal("funded order lost its correct payout destination")
			}
		})
	}
}

func TestVendorBondBTCFeeDescriptorSkipsBondDestination(t *testing.T) {
	desc := "wpkh(" + testBIP84Key + "/0/*)"
	x := newEscrowEnv(t, "ESCROW_BTC_FEE_BPS", "250", "ESCROW_BTC_FEE_ADDRESS", desc)
	k, err := parseFeeDescriptor(desc, keyNetTest)
	if err != nil {
		t.Fatal(err)
	}
	x.core.mu.Lock()
	x.core.deriveHook = func(body string) (string, bool) {
		rest, ok := strings.CutPrefix(body, "wpkh("+k.Expression()+"/0/")
		if !ok {
			return "", false
		}
		index, err := strconv.ParseUint(strings.TrimSuffix(rest, ")"), 10, 32)
		if err != nil {
			return "", false
		}
		address, err := feeDescriptorAddress(k, "regtest", uint32(index))
		return address, err == nil
	}
	x.core.mu.Unlock()
	bond, err := feeDescriptorAddress(k, "regtest", 0)
	if err != nil {
		t.Fatal(err)
	}
	bondIsolationQuote(x.testEnv, x.vendor, bond)
	order := x.draft()
	x.mustCheckout(order, regtestAddr("bond-isolation-fee-refund"))
	got, err := x.A.escrowFeeAddress(context.Background(), x.A.escrow.Load(), order)
	want, _ := feeDescriptorAddress(k, "regtest", 1)
	if err != nil || got != want || got == bond {
		t.Fatalf("fee destination %q %v, want unused %q", got, err, want)
	}
}
