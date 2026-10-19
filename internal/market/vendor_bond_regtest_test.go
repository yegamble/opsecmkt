package market

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// TestXMREscrowRegtestBond (A-547; scripts/test-escrow-xmr-regtest.sh): the vendor bond against monerod and
// monero-wallet-rpc 0.18.5.1 on a local fakechain. Under a 0.25 XMR bond with automatic approval, a buyer applies to
// sell; the first pass creates the bond wallet on the escrow wallet-rpc (its seed sealed, and restored from the seal to
// its primary address) and gives the application a real account's address 0. The funder pays the bond: in the pool it
// is recorded at 0 confirmations, one short of the threshold nothing is recorded, at the threshold the bond is recorded
// as received on the Monero network and the account becomes a vendor. pop_blocks then takes the deposit's blocks off
// the chain (the transaction goes back to the pool): the deposit falls below the threshold and the administrators are
// told once, with the application still approved and the account still a vendor; mined again, nothing changes. The
// bond wallet lists no outgoing, pending or failed transfer: the market made no send from it. A bond wallet an earlier run left on the shared wallet-rpc is recorded
// as an orphan and the next name is used.
func TestXMREscrowRegtestBond(t *testing.T) {
	r := newXMRRegtest(t)
	thr := r.A.escrowXMR.confirmations
	setVendorPolicy(r.testEnv, strconv.FormatInt(testBondPico, 10), "automatic", "")
	uid, sess, app := r.bondApplicant()
	r.mustPoll()
	addr := r.mustBondAddress(app)
	var name, primary string
	var height int64
	if err := r.DB.QueryRow("SELECT name,primary_address,restore_height FROM xmr_bond_wallets WHERE state='sealed'").Scan(&name, &primary, &height); err != nil {
		t.Fatal(err)
	}
	seed := r.unsealArmored(agStr(r.testEnv, "SELECT seed_sealed FROM xmr_bond_wallets WHERE name=$1", name))
	var restored struct {
		Address string `json:"address"`
	}
	r.call(r.restore, "restore_deterministic_wallet", map[string]any{"filename": "restored-bond-" + randomToken()[:10], "password": "", "seed": seed,
		"restore_height": height, "language": "English", "autosave_current": false}, &restored)
	if restored.Address != primary {
		t.Fatalf("the restored bond seed gives %s, the row has %s", restored.Address, primary)
	}
	if _, err := parseXMRAddress(addr, "fakechain"); err != nil {
		t.Fatalf("the bond address is not a fakechain address: %v", err)
	}
	t.Logf("bond wallet %s sealed (restores to %s…); application %s has bond address %s…", name, primary[:12], shortID(app), addr[:12])
	mustContain(t, r.accountPage(sess), addr, "never sends coins back")

	tx := r.pay(map[string]int64{addr: testBondPico})
	confs := func() string {
		return strings.Join(agStrings(t, r.testEnv, "SELECT d.confirmations||' '||d.credited||' '||d.reorg_flagged FROM vendor_bond_deposits d JOIN vendor_bond_accounts c ON c.id=d.account_id WHERE c.application_id=$1", app), ",")
	}
	r.mustPoll()
	if got := confs(); got != "0 false false" || r.appState(app) != "awaiting_bond:" {
		t.Fatalf("in the pool: %q %s", got, r.appState(app))
	}
	r.mineTo(tx, thr-1)
	heights := r.synced()
	r.mustPoll()
	if got := confs(); got != strconv.FormatInt(thr-1, 10)+" false false" || r.appState(app) != "awaiting_bond:" {
		t.Fatalf("one short of the threshold (%s): %q %s", heights, got, r.appState(app))
	}
	r.mineTo(tx, thr)
	heights = r.synced()
	r.mustPoll()
	if got := confs(); got != strconv.FormatInt(thr, 10)+" true false" || r.appState(app) != "approved:xmr" || userRole(r.testEnv, uid) != "vendor" {
		t.Fatalf("at the threshold (%s): %q %s, role %s", heights, got, r.appState(app), userRole(r.testEnv, uid))
	}
	t.Logf("deposit %s: at %d confirmations the bond was recorded received and the account approved as a vendor", tx, thr)

	r.popPast(tx)
	r.mine(xmrMineMargin)
	heights = r.synced()
	r.mustPoll()
	r.mustPoll()
	var got int64
	if err := r.DB.QueryRow(`SELECT d.confirmations FROM vendor_bond_deposits d JOIN vendor_bond_accounts c ON c.id=d.account_id
		WHERE c.application_id=$1 AND d.credited AND d.reorg_flagged`, app).Scan(&got); err != nil || got >= thr {
		t.Fatalf("after pop_blocks (%s): the deposit is not flagged below the threshold: %q (%v)", heights, confs(), err)
	}
	if r.appState(app) != "approved:xmr" || userRole(r.testEnv, uid) != "vendor" || r.adminsTold(bondReorgNotice) != 1 {
		t.Fatalf("after the reorganisation: %s, role %s, notices %d", r.appState(app), userRole(r.testEnv, uid), r.adminsTold(bondReorgNotice))
	}
	t.Logf("pop_blocks: deposit %s back at %d confirmations, flagged to the administrators once; the vendor stays approved", tx, got)
	r.mineTo(tx, thr)
	r.synced()
	r.mustPoll()
	if r.appState(app) != "approved:xmr" || r.adminsTold(bondReorgNotice) != 1 || agInt(r.testEnv, "SELECT count(*) FROM vendor_bond_deposits") != 1 {
		t.Fatalf("mined again: %s, notices %d, deposits %q", r.appState(app), r.adminsTold(bondReorgNotice), confs())
	}
	// No send: the bond wallet itself lists no outgoing, pending or failed transfer on any account.
	ctx := context.Background()
	x := r.A.escrowXMR
	if err := x.rpc.openWallet(ctx, name, xmrWalletPassword(testXMRSecret, name)); err != nil {
		t.Fatalf("open_wallet %s: %v", name, err)
	}
	var sent map[string][]json.RawMessage
	if err := x.rpc.walletCall(ctx, "get_transfers", map[string]any{"out": true, "pending": true, "failed": true, "all_accounts": true}, &sent); err != nil {
		t.Fatal(err)
	}
	for kind, list := range sent {
		if len(list) > 0 {
			t.Fatalf("the bond wallet lists %d %s transfer(s)", len(list), kind)
		}
	}
	t.Logf("mined again to %d confirmations: nothing changed; the bond wallet lists no out, pending or failed transfer", thr)
}
