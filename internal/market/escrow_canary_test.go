package market

import (
	"html"
	"strings"
	"testing"
)

// A-333 (ES-10): while ESCROW_BTC is on, /canary states the escrow mode truthfully: it never calls the market's
// holding "custodial holding, not escrow" or lists multisig escrow as unavailable, and it says escrow runs on regtest
// only. /canary#records lists the escrow records, what each keeps and for how long. With escrow off the page keeps its
// earlier wording. DB integration with the simulated Core (not chain evidence).
func TestCanaryStatesEscrowMode(t *testing.T) {
	x := newEscrowEnv(t)
	for _, sess := range []string{"", x.buyerSess} {
		page := strings.ReplaceAll(html.UnescapeString(oneSpace(x.page("/canary", sess))), "’", "'")
		for _, bad := range []string{"this is custodial holding, not escrow", "multisig escrow, commission", "Multisig escrow, commission"} {
			if strings.Contains(page, bad) {
				t.Fatalf("/canary says %q with escrow on:\n%s", bad, page)
			}
		}
		_, records, _ := strings.Cut(page, `id="records"`)
		records, _, _ = strings.Cut(records, "</ul>")
		_, entry, ok := strings.Cut(records, "<li><strong>Bitcoin escrow.</strong>")
		entry, _, _ = strings.Cut(entry, "</li>")
		if !ok {
			t.Fatalf("/canary#records has no Bitcoin escrow entry:\n%s", records)
		}
		for _, want := range []string{
			"extended public key, sealed",
			"the escrow address, the three child public keys",
			"refund address",
			"the unsigned transaction",
			"each signing party's signature on each input",
			"the pasted PSBT is not kept",
			"the final signed transaction",
			"the market-signed PSBT is never kept",
			"watch-only wallet",
			"labelled with the order ID",
			"no age limit",
			"kept until the operator removes",
			"addresses the watch-only wallet holds with no escrow",
			"the release deadline length and dispatch window it was created with", // P2-a, P2-c: migration 079
			"spent by a transaction this market did not make",
			"A fee bump of a broadcast settlement",                              // P2-d: settlement_bumps, migration 080
			"with their public keys kept so that no new escrow uses them again", // P2-e: escrow_reserved_keys, migration 083
			"each account's lowest next escrow child key is kept too",           // P2-e: escrow_index_floors, migration 083
		} {
			if !strings.Contains(entry, want) {
				t.Errorf("/canary#records Bitcoin escrow entry lacks %q:\n%s", want, entry)
			}
		}
		for _, want := range []string{
			"Bitcoin escrow (regtest only)",
			"2-of-3 multisig escrow",
			// A-361 (ES-10): the claim holds only if the key each party's page shows as theirs is theirs, as the order
			// page's escrow panel says.
			"so the market cannot spend it alone, if the key each party's order page shows as theirs is theirs: each party checks the escrow descriptor in their own wallet",
			"not verified on any public network",
			"or a physical listing with a dispatch window, can be paid into its own 2-of-3 multisig escrow", // Phase 2
		} {
			if !strings.Contains(page, want) {
				t.Errorf("/canary lacks %q with escrow on:\n%s", want, page)
			}
		}
	}
	t.Setenv("ESCROW_BTC", "")
	x.restart()
	if page := html.UnescapeString(oneSpace(x.page("/canary", ""))); !strings.Contains(page, "multisig escrow, commission") || strings.Contains(page, "2-of-3 multisig escrow") {
		t.Fatalf("/canary with escrow off:\n%s", page)
	}
}
