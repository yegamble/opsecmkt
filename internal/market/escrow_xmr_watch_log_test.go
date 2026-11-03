package market

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// A-635, the Monero port of the retired Bitcoin TestLogsContainNoSecretsFromEscrowWatcher (A-170, A-493): the running
// Monero escrow watcher's error line names its labels, the wallet method and the RPC code, never the wallet's error
// text, which may quote an address. Here the escrow wallet-rpc refuses get_transfers with a message carrying a marker,
// an unrelated address and the order's escrow address; nothing of that message, the refund address or the order id
// reaches the log, and the unread escrow leaves its order awaiting payment. DB integration test against the simulated
// Monero wallet RPC.
func TestLogsContainNoSecretsFromXMREscrowWatcher(t *testing.T) {
	x := newXMREscrowEnv(t)
	x.mustPoll()
	order, escrowAddr := x.freshCheckout(x.product)
	refund := agStr(x.testEnv, "SELECT refund_address FROM escrows WHERE order_id=$1", order)
	const marker = "A635-WALLET-MARKER"
	leaked := testXMRAddr("a635 leaked address", true)
	x.fake.handle("get_transfers", func(*fakeXMREscrow, json.RawMessage) (any, int64, string) {
		return nil, -5, marker + " Fixture refused to list " + leaked + " and " + escrowAddr
	})
	logs := captureLog(t)
	ctx, cancel := context.WithCancel(t.Context())
	stopped := make(chan struct{})
	go func() { defer close(stopped); xmrEscrowWatcher(ctx, x.A) }()
	const prefix = "monero escrow watcher: "
	var line string
	waitUntil(t, 10*time.Second, func() bool {
		for l := range strings.SplitSeq(logs.String(), "\n") {
			if strings.HasPrefix(l, prefix) {
				line = l
				return true
			}
		}
		return false
	})
	cancel()
	<-stopped
	wallet := agStr(x.testEnv, "SELECT w.name FROM escrows e JOIN xmr_escrow_slots s ON s.id=e.xmr_slot_id JOIN xmr_escrow_wallets w ON w.id=s.wallet_id WHERE e.order_id=$1", order)
	if want := prefix + "monero escrow wallet " + wallet + ": get_transfers rpc:-5"; line != want {
		t.Fatalf("watcher line %q, want %q; logs %q", line, want, logs.String())
	}
	for _, s := range []string{marker, leaked, escrowAddr, refund, order, shortID(order), "Fixture refused"} {
		if strings.Contains(logs.String(), s) {
			t.Fatalf("log contains %q: %q", s, logs.String())
		}
	}
	if x.state(order) != stateAwaitingPayment {
		t.Fatalf("an unread escrow moved its order to %s", x.state(order))
	}
}
