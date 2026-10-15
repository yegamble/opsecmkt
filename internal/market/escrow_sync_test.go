package market

import (
	"html"
	"strings"
	"testing"
	"time"
)

// onSignet makes the fake node a synced signet node (signet_challenge 51, 8 peers, a fresh tip) and the enabled escrow
// one started on it, by hand, so a test can move escrows made on regtest to signet (a real restart refuses that: the
// chain pin); signetEscrowEnv starts escrow on signet the real way (A-443).
func (x *escrowEnv) onSignet() {
	x.t.Helper()
	x.core.mu.Lock()
	x.core.chain, x.core.challenge, x.core.peers, x.core.tipTime = "signet", "51", 8, time.Now().Unix()
	x.core.mu.Unlock()
	e := x.A.escrow.Load()
	e.network, e.chainID = "signet", escrowChainID("signet", "51")
}

func (x *escrowEnv) adminPage() string {
	x.t.Helper()
	_, as := x.user("syncadmin_"+randomToken()[:6], "admin")
	return html.UnescapeString(x.page("/admin", as))
}

// windowPassedWording is the escrow order page's sentence for x.order past its payment window while the escrow node is
// not synced (held), and the start of the usual one (usual), which names the same past time.
func windowPassedWording(t *testing.T, x *escrowEnv) (held, usual string) {
	t.Helper()
	var created time.Time
	if err := x.DB.QueryRow("SELECT created FROM escrows WHERE order_id=$1", x.order).Scan(&created); err != nil {
		t.Fatal(err)
	}
	at := created.Add(expiryWindow()).UTC().Truncate(time.Minute).Format("2006-01-02 15:04 UTC")
	return "The payment window ended at " + at + ", but the order stays open for now: the market is waiting for its Bitcoin node before it checks this escrow again and acts on the payment window.",
		"If nothing has been received by " + at + ", the order is cancelled."
}

const regtestIBDText = "Bitcoin Core reports initial block download at height 0 and after a restart when its tip is older than 24 hours, " +
	"and with no peers waiting does not end it: mine one block (bitcoin-cli -regtest generatetoaddress 1 <address>) or restart bitcoind with a larger -maxtipage."

func withoutInstalled(s []string) []string {
	var out []string
	for _, v := range s {
		if !strings.HasPrefix(v, "installed=") {
			out = append(out, v)
		}
	}
	return out
}
