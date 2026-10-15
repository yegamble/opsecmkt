package market

import (
	"context"

	"encoding/json"

	"testing"
)

// coSpendFromWallets is a transaction the buyer's and the vendor's own wallets sign together, spending the escrow
// output txid:vout of order to dest: each party's copy carries its own key origin (its descriptor), the two copies are
// combined and finalized. It is not a settlement of the market.
func coSpendFromWallets(t *testing.T, call func(string, string, any, ...any), e *testEnv, order, txid string, vout int64, dest, btc, buyerWallet, vendorWallet string) string {
	t.Helper()
	var unsigned string
	call("", "createpsbt", &unsigned, []map[string]any{{"txid": txid, "vout": vout}}, []map[string]any{{dest: json.Number(btc)}}, 0, true)
	var signed []string
	for party, wallet := range map[string]string{roleBuyer: buyerWallet, roleVendor: vendorWallet} {
		desc, err := escrowDescriptorFor(context.Background(), e.DB, order, party)
		if err != nil {
			t.Fatal(err)
		}
		var updated string
		call("", "utxoupdatepsbt", &updated, unsigned, []string{desc})
		var out struct {
			PSBT string `json:"psbt"`
		}
		call(wallet, "walletprocesspsbt", &out, updated, true, "ALL", true, false)
		signed = append(signed, out.PSBT)
	}
	var combined string
	call("", "combinepsbt", &combined, signed)
	var fin struct {
		Hex      string `json:"hex"`
		Complete bool   `json:"complete"`
	}
	call("", "finalizepsbt", &fin, combined, true)
	if !fin.Complete {
		t.Fatal("the parties' co-spend does not finalize")
	}
	return fin.Hex
}
