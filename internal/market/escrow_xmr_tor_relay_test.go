package market

import (
	"context"
	"strings"
	"testing"
)

// A-515 (PRIV60-3; Bitcoin's counterpart is A-441's Tor-mode gate): with APP_MODE=tor, monero-wallet-rpc's relay_tx
// would send a Monero escrow settlement through its daemon from the host's IP address. So in Tor mode a settlement is
// built, checked and stored but not relayed unless COMPOSE_FILE has compose.nodes-tor.yaml (the local nodes over Tor)
// or ESCROW_XMR_TOR_RELAY=clearnet-accepted; /admin names the reason and one line is logged. Once either holds (after
// a restart, as both are read at startup) the stored transaction is relayed unchanged: transfer is never called
// again. DB integration tests against the simulated wallet-rpc and monerod (escrow_xmr_fake_test.go): simulated RPC,
// not chain evidence; Monero over Tor itself stays unverified (A-95).

const (
	xmrTorNoOverlay = "compose.yaml:compose.tor.yaml:compose.nodes.yaml:compose.internal-db.yaml"
	xmrTorOverlay   = xmrTorNoOverlay + ":compose.nodes-tor.yaml"
	xmrTorHeldAdmin = "Monero relays held: Tor mode without the node Tor overlay — see operator guide"
	xmrTorHeldLog   = "escrow: APP_MODE=tor: Monero escrow relays held"
)

// torRelayEnv sets the environment the next restart reads: APP_MODE, COMPOSE_FILE and ESCROW_XMR_TOR_RELAY.
func torRelayEnv(t *testing.T, mode, composeFile, ack string) {
	t.Helper()
	t.Setenv("APP_MODE", mode)
	t.Setenv("COMPOSE_FILE", composeFile)
	t.Setenv("ESCROW_XMR_TOR_RELAY", ack)
}

func TestXMREscrowTorModeWithoutNodeProxyHoldsRelay(t *testing.T) {
	// held is a market in Tor mode without the overlay or the acknowledgement, with one completed order whose release
	// was built and stored on two passes but never relayed.
	held := func(t *testing.T) (x *xmrEscrowEnv, logs *lockedBuffer, order string, id int64, stored xmrSettlement) {
		t.Helper()
		x = newXMREscrowEnv(t)
		logs = captureLog(t)
		torRelayEnv(t, "tor", xmrTorNoOverlay, "")
		x.restart()
		x.mustPoll()
		order, _, _ = x.completed()
		id, _ = x.release(order)
		x.mustPoll()
		x.mustPoll()
		stored = x.settlement(id)
		if stored.State != "built" || stored.TxHash == "" || len(stored.KeyImages) == 0 || x.fake.countOf("transfer") != 1 || x.fake.countOf("relay_tx") != 0 ||
			x.fake.countOf("get_transfer_by_txid") != 0 {
			t.Fatalf("held: %+v, transfer %d, relay_tx %d, get_transfer_by_txid %d", stored, x.fake.countOf("transfer"), x.fake.countOf("relay_tx"), x.fake.countOf("get_transfer_by_txid"))
		}
		if _, ok := x.fake.tx(stored.TxHash); ok {
			t.Fatal("the held transaction reached the daemon")
		}
		if got := logs.String(); strings.Count(got, xmrTorHeldLog) != 1 || strings.Contains(got, stored.TxHash) || strings.Contains(got, x.fake.wallet.URL) {
			t.Fatalf("want one hold line, no transaction or address:\n%s", got)
		}
		page := x.adminPage()
		for _, want := range []string{xmrTorHeldAdmin, "compose.nodes-tor.yaml", "ESCROW_XMR_TOR_RELAY=clearnet-accepted"} {
			if !strings.Contains(page, want) {
				t.Fatalf("admin page lacks %q:\n%s", want, page)
			}
		}
		if p := x.panel(order, x.sessOf(order, roleBuyer)); !strings.Contains(p, "Built, checked and stored. The market holds its Monero relays") || strings.Contains(p, "sends it on its next pass") {
			t.Fatalf("the order page while relays are held:\n%s", p)
		}
		return x, logs, order, id, stored
	}
	// resumed checks that the stored transaction of settlement id was relayed unchanged, once, with no new build.
	resumed := func(t *testing.T, x *xmrEscrowEnv, id int64, stored xmrSettlement) {
		t.Helper()
		x.mustPoll()
		s := x.settlement(id)
		if s.State != "relayed" || s.TxHash != stored.TxHash || x.fake.countOf("relay_tx") != 1 || x.fake.countOf("transfer") != 1 {
			t.Fatalf("resumed: %+v (stored %+v), relay_tx %d, transfer %d", s, stored, x.fake.countOf("relay_tx"), x.fake.countOf("transfer"))
		}
		if _, ok := x.fake.tx(stored.TxHash); !ok {
			t.Fatal("the stored transaction is not in the daemon")
		}
		if n := agInt(x.testEnv, "SELECT count(*) FROM xmr_settlements WHERE order_id=(SELECT order_id FROM xmr_settlements WHERE id=$1)", id); n != 1 {
			t.Fatalf("%d settlements for one escrow", n)
		}
		if page := x.adminPage(); strings.Contains(page, "Monero relays held") {
			t.Fatal("the admin line stayed once relays may go out")
		}
	}

	t.Run("held without the overlay or the acknowledgement", func(t *testing.T) {
		x, logs, order, id, stored := held(t)
		// Later passes relay nothing, build nothing again and log nothing new; a deposit after close gets its stray refund
		// requested, which waits for the built release (one unrelayed transaction per escrow).
		x.fake.payAddress(agStrings(t, x.testEnv, "SELECT address FROM escrows WHERE order_id=$1", order)[0], hex64("late "+order), 7000000000, 12)
		x.mustPoll()
		x.mustPoll()
		if s := x.settlement(id); s.State != "built" || s.TxHash != stored.TxHash || x.fake.countOf("relay_tx") != 0 || x.fake.countOf("transfer") != 1 {
			t.Fatalf("later passes: %+v, relay_tx %d, transfer %d", s, x.fake.countOf("relay_tx"), x.fake.countOf("transfer"))
		}
		if st := x.settlementOf(order, "stray_refund"); st.State != "requested" || st.TxHash != "" {
			t.Fatalf("the stray refund did not wait for the held release: %+v", st)
		}
		if got := logs.String(); strings.Count(got, xmrTorHeldLog) != 1 {
			t.Fatalf("the hold was logged again:\n%s", got)
		}
	})

	t.Run("relayed once the overlay is added", func(t *testing.T) {
		x, _, _, id, stored := held(t)
		torRelayEnv(t, "tor", xmrTorOverlay, "")
		x.restart()
		resumed(t, x, id, stored)
	})

	t.Run("relayed once clearnet relay is accepted", func(t *testing.T) {
		x, _, _, id, stored := held(t)
		torRelayEnv(t, "tor", xmrTorNoOverlay, "clearnet-accepted")
		x.restart()
		resumed(t, x, id, stored)
	})

	t.Run("a relayed settlement the daemon lost waits too", func(t *testing.T) {
		x := newXMREscrowEnv(t)
		torRelayEnv(t, "clearnet", "", "")
		x.restart()
		x.mustPoll()
		order, _, _ := x.completed()
		id, _ := x.release(order)
		x.mustPoll()
		s := x.settlement(id)
		if s.State != "relayed" || x.fake.countOf("relay_tx") != 1 {
			t.Fatalf("clearnet relay: %+v", s)
		}
		torRelayEnv(t, "tor", xmrTorNoOverlay, "")
		x.restart()
		x.fake.flushPool(s.TxHash)
		x.mustPoll()
		x.mustPoll()
		if got := x.settlement(id); got.State != "relayed" || x.fake.countOf("relay_tx") != 1 {
			t.Fatalf("relayed again while held: %+v, relay_tx %d", got, x.fake.countOf("relay_tx"))
		}
		torRelayEnv(t, "tor", xmrTorOverlay, "")
		x.restart()
		x.mustPoll()
		if _, ok := x.fake.tx(s.TxHash); !ok || x.fake.countOf("relay_tx") != 2 || x.fake.countOf("transfer") != 1 {
			t.Fatalf("not relayed again with the overlay: relay_tx %d, transfer %d", x.fake.countOf("relay_tx"), x.fake.countOf("transfer"))
		}
	})

	t.Run("clearnet mode unchanged", func(t *testing.T) {
		x := newXMREscrowEnv(t)
		logs := captureLog(t)
		torRelayEnv(t, "clearnet", "", "")
		x.restart()
		x.mustPoll()
		order, _, _ := x.completed()
		id, _ := x.release(order)
		x.mustPoll()
		if s := x.settlement(id); s.State != "relayed" || x.fake.countOf("relay_tx") != 1 {
			t.Fatalf("clearnet: %+v, relay_tx %d", s, x.fake.countOf("relay_tx"))
		}
		if strings.Contains(logs.String(), xmrTorHeldLog) || strings.Contains(x.adminPage(), "Monero relays held") {
			t.Fatal("clearnet mode reports Monero relays held")
		}
	})

	t.Run("an invalid acknowledgement is refused at startup", func(t *testing.T) {
		x := newXMREscrowEnv(t)
		for _, mode := range []string{"tor", "clearnet"} {
			for _, v := range []string{"yes", "clearnet", "Clearnet-Accepted", "clearnet-accepted-please"} {
				torRelayEnv(t, mode, xmrTorNoOverlay, v)
				if mode == "clearnet" {
					t.Setenv("COMPOSE_FILE", "")
				}
				a, err := New(context.Background(), false)
				if err == nil {
					a.Close()
					t.Fatalf("APP_MODE=%s ESCROW_XMR_TOR_RELAY=%q started", mode, v)
				}
				if msg := err.Error(); !strings.Contains(msg, "ESCROW_XMR_TOR_RELAY") || !strings.Contains(msg, "clearnet-accepted") || strings.Contains(msg, `"`+v+`"`) {
					t.Fatalf("APP_MODE=%s ESCROW_XMR_TOR_RELAY=%q: %v", mode, v, err)
				}
			}
		}
		// Blank and the one accepted value start.
		torRelayEnv(t, "tor", xmrTorNoOverlay, " clearnet-accepted ")
		x.restart()
		if x.A.xmrRelayHold() != "" {
			t.Fatal("the acknowledgement did not let relays go out")
		}
	})
}
