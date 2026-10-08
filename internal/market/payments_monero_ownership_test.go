package market

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

func rotatedPooledPrimary() string { return "5A" + payStagenetPrimary[2:] }
func rotatedPooledSub() string     { return "7A" + payStagenetSub[2:] }

func TestMoneroPooledWalletRotationInvalidatesIssuedIndices(t *testing.T) {
	ctx := context.Background()
	w, s := newPayMonero(t, payStagenetPrimary, payStagenetSub)
	p, err := newMoneroProvider(ctx, s.url(""), s.url(""), "stagenet", 10)
	if err != nil {
		t.Fatal(err)
	}
	addr, err := p.NewAddress(ctx, "order-1")
	if err != nil {
		t.Fatal(err)
	}
	w.set(func(m *payMoneroWallet) {
		m.transfers = map[string]any{"in": []map[string]any{{"txid": "known", "amount": 1, "confirmations": 1, "subaddr_index": map[string]any{"major": 0, "minor": 1}}}}
	})
	if _, err := p.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := p.Incoming(ctx, []string{addr}); err != nil || len(got) != 1 || s.called("get_address_index") != 0 {
		t.Fatalf("same primary discarded the issued-address cache: %+v %v, lookups=%d", got, err, s.called("get_address_index"))
	}
	w.set(func(m *payMoneroWallet) { m.primary, m.sub = rotatedPooledPrimary(), rotatedPooledSub() })
	if _, err := p.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if p.primaryAddress() != rotatedPooledPrimary() || len(p.index) != 0 {
		t.Fatalf("new primary did not replace the primary and invalidate indices: primary=%q indices=%v", p.primaryAddress(), p.index)
	}
	reads := s.called("get_transfers")
	got, err := p.Incoming(ctx, []string{addr})
	if got != nil || err == nil || !strings.Contains(err.Error(), "no longer recognizes an issued payment address") ||
		strings.Contains(err.Error(), addr) || s.called("get_address_index") != 1 || s.called("get_transfers") != reads {
		t.Fatalf("disowned address was read as empty or leaked: %+v %v, lookups=%d transfers=%d", got, err, s.called("get_address_index"), s.called("get_transfers"))
	}
}

// Restoring a seed can leave the primary unchanged but forget or reassign its
// created subaddresses. The cached minor must be checked against the wallet on
// every read chunk, even when Check saw no primary change.
func TestMoneroPooledCachedIndexVerifiesSamePrimary(t *testing.T) {
	for _, tc := range []struct {
		name   string
		forget func(*payMoneroWallet)
	}{
		{"index now absent", func(m *payMoneroWallet) { m.nextIndex = 0 }},
		{"index maps to another address", func(m *payMoneroWallet) { m.sub = rotatedPooledSub() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			w, s := newPayMonero(t, payStagenetPrimary, payStagenetSub)
			p, err := newMoneroProvider(ctx, s.url(""), s.url(""), "stagenet", 10)
			if err != nil {
				t.Fatal(err)
			}
			addr, err := p.NewAddress(ctx, "order-1")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := p.Incoming(ctx, []string{addr}); err != nil {
				t.Fatal(err)
			}
			w.set(tc.forget)
			if _, err := p.Check(ctx); err != nil || p.primaryAddress() != payStagenetPrimary || p.index[addr] != 1 {
				t.Fatalf("same-primary wallet state: primary=%q index=%d err=%v", p.primaryAddress(), p.index[addr], err)
			}
			reads := s.called("get_transfers")
			got, err := p.Incoming(ctx, []string{addr})
			if got != nil || err == nil || !strings.Contains(err.Error(), "issued payment address") ||
				strings.Contains(err.Error(), addr) || s.called("get_transfers") != reads {
				t.Fatalf("cached index accepted after same-primary restore: %+v %v, transfers=%d→%d", got, err, reads, s.called("get_transfers"))
			}
		})
	}
}

func TestMoneroPooledWalletBehindDaemonIsSyncing(t *testing.T) {
	ctx := context.Background()
	w, override, s := newGapMonero(t)
	w.set(func(m *payMoneroWallet) { m.height, m.walletHeight = 100, 99 })
	p, err := newMoneroProvider(ctx, s.url(""), s.url(""), "stagenet", 10)
	if err != nil {
		t.Fatal(err)
	}
	if st, err := p.Check(ctx); err != nil || !st.Syncing || st.Why != "wallet height below daemon height" || st.Tip != 99 {
		t.Fatalf("wallet lag: %+v %v", st, err)
	}
	w.set(func(m *payMoneroWallet) { m.walletHeight = 100 })
	if st, err := p.Check(ctx); err != nil || st.Syncing {
		t.Fatalf("wallet caught up: %+v %v", st, err)
	}
	for _, bad := range []struct {
		info    map[string]any
		message string
	}{
		{map[string]any{"nettype": "stagenet", "stagenet": true, "synchronized": true, "outgoing_connections_count": 8}, "daemon height unreadable"},
		{map[string]any{"nettype": "stagenet", "stagenet": true, "synchronized": true, "outgoing_connections_count": 8, "height": "bad"}, "unexpected result shape"},
	} {
		answer := bad.info
		override.answer("get_info", func(string, string, json.RawMessage) (any, int, string) { return answer, 0, "" })
		if _, err := p.Check(ctx); err == nil || !strings.Contains(err.Error(), bad.message) {
			t.Fatalf("unreadable daemon height accepted: %v", err)
		}
	}
}

// A rotated wallet must not cause the pooled watcher to mark old deposits missing or
// expire an old payment window. All four states share the same issued-address read.
func TestMoneroPooledDisownedAddressHoldsLedgerAndExpiry(t *testing.T) {
	for _, tc := range []struct {
		name          string
		fraction      int64 // numerator of 10; 0 means no deposit
		confirmations int64
		wantState     string
		recoverState  string
		samePrimary   bool
	}{
		{"expired unpaid", 0, 0, stateAwaitingPayment, stateCancelled, false},
		{"below threshold", 10, 1, stateAwaitingPayment, statePaid, false},
		{"part paid", 4, 10, stateAwaitingPayment, "", false},
		{"credited", 10, 10, statePaid, "", false},
		{"credited same primary", 10, 10, statePaid, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, s := newPayMonero(t, payStagenetPrimary, payStagenetSub)
			w.set(func(m *payMoneroWallet) {
				m.resolvedIndex = 1
				m.valid[payStagenetSub] = "stagenet"
			})
			setPaymentEnv(t, "", "", s.url(""), s.url(""), "stagenet")
			t.Setenv("PAYMENT_CONFIRMATIONS_XMR", "10")
			e := newTestApp(t)
			buyer, session := e.user("rotate_buyer", "buyer")
			vendor, _ := e.user("rotate_vendor", "vendor")
			order := e.order(buyer, e.product(vendor, "digital"), "XMR", stateDraft)
			e.check(e.do("POST", "/orders/pay", session, url.Values{"order_id": {order}}), 303)
			var amount int64
			if err := e.DB.QueryRow("SELECT amount FROM orders WHERE id=$1", order).Scan(&amount); err != nil {
				t.Fatal(err)
			}
			if tc.fraction > 0 {
				w.set(func(m *payMoneroWallet) {
					m.transfers = map[string]any{"in": []map[string]any{{"txid": "issued-deposit", "amount": amount * tc.fraction / 10, "confirmations": tc.confirmations, "subaddr_index": map[string]any{"major": 0, "minor": 1}}}}
				})
			}
			if err := e.A.pollOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			state := func() string {
				var got string
				if err := e.DB.QueryRow("SELECT state FROM orders WHERE id=$1", order).Scan(&got); err != nil {
					t.Fatal(err)
				}
				return got
			}
			if got := state(); got != tc.wantState {
				t.Fatalf("initial order state %s, want %s", got, tc.wantState)
			}
			if _, err := e.DB.Exec("UPDATE payment_addresses SET created=now()-interval '48 hours' WHERE order_id=$1", order); err != nil {
				t.Fatal(err)
			}
			if tc.name == "part paid" {
				if _, err := e.DB.Exec("UPDATE payments SET created=now()-interval '48 hours' WHERE order_id=$1", order); err != nil {
					t.Fatal(err)
				}
			}
			var beforeConfs int64
			var beforeFlagged, beforeCredited bool
			if tc.fraction > 0 {
				if err := e.DB.QueryRow("SELECT confirmations,flagged,credited FROM payments WHERE order_id=$1", order).Scan(&beforeConfs, &beforeFlagged, &beforeCredited); err != nil {
					t.Fatal(err)
				}
			}
			var beforeEvents, beforeNotices int
			if err := e.DB.QueryRow("SELECT count(*) FROM order_events WHERE order_id=$1", order).Scan(&beforeEvents); err != nil {
				t.Fatal(err)
			}
			if err := e.DB.QueryRow("SELECT count(*) FROM notifications WHERE user_id=$1", buyer).Scan(&beforeNotices); err != nil {
				t.Fatal(err)
			}
			w.set(func(m *payMoneroWallet) {
				if !tc.samePrimary {
					m.primary = rotatedPooledPrimary()
				}
				m.sub = rotatedPooledSub()
				m.transfers = map[string]any{}
			})
			if err := e.A.pollOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "issued payment address") || strings.Contains(err.Error(), payStagenetSub) {
				t.Fatalf("disowned read did not fail safely: %v", err)
			}
			if got := state(); got != tc.wantState {
				t.Fatalf("order changed on unread pass: %s, want %s", got, tc.wantState)
			}
			var afterEvents, afterNotices, payouts, flags int
			if err := e.DB.QueryRow("SELECT count(*) FROM order_events WHERE order_id=$1", order).Scan(&afterEvents); err != nil {
				t.Fatal(err)
			}
			if err := e.DB.QueryRow("SELECT count(*) FROM notifications WHERE user_id=$1", buyer).Scan(&afterNotices); err != nil {
				t.Fatal(err)
			}
			if err := e.DB.QueryRow("SELECT count(*) FROM payouts WHERE order_id=$1", order).Scan(&payouts); err != nil {
				t.Fatal(err)
			}
			if err := e.DB.QueryRow("SELECT count(*) FROM payments WHERE order_id=$1 AND flagged", order).Scan(&flags); err != nil {
				t.Fatal(err)
			}
			if afterEvents != beforeEvents || afterNotices != beforeNotices || payouts != 0 || flags != 0 {
				t.Fatalf("unread pass changed events %d→%d, notices %d→%d, payouts=%d, flags=%d", beforeEvents, afterEvents, beforeNotices, afterNotices, payouts, flags)
			}
			if tc.fraction > 0 {
				var confs int64
				var flagged, credited bool
				if err := e.DB.QueryRow("SELECT confirmations,flagged,credited FROM payments WHERE order_id=$1", order).Scan(&confs, &flagged, &credited); err != nil || confs != beforeConfs || flagged != beforeFlagged || credited != beforeCredited {
					t.Fatalf("ledger changed on unread pass: confirmations=%d flagged=%t credited=%t err=%v, before=%d/%t/%t", confs, flagged, credited, err, beforeConfs, beforeFlagged, beforeCredited)
				}
			}
			if tc.recoverState != "" {
				w.set(func(m *payMoneroWallet) {
					m.primary, m.sub = payStagenetPrimary, payStagenetSub
					if tc.fraction > 0 {
						m.transfers = map[string]any{"in": []map[string]any{{"txid": "issued-deposit", "amount": amount * tc.fraction / 10, "confirmations": 10, "subaddr_index": map[string]any{"major": 0, "minor": 1}}}}
					}
				})
				if err := e.A.pollOnce(context.Background()); err != nil {
					t.Fatal(err)
				}
				if got := state(); got != tc.recoverState {
					t.Fatalf("restored wallet left order %s, want %s", got, tc.recoverState)
				}
			}
		})
	}
}

func TestMoneroPooledWalletLagHoldsExpiredOrder(t *testing.T) {
	w, s := newPayMonero(t, payStagenetPrimary, payStagenetSub)
	w.set(func(m *payMoneroWallet) {
		m.height, m.walletHeight = 100, 100
		m.valid[payStagenetSub] = "stagenet"
	})
	setPaymentEnv(t, "", "", s.url(""), s.url(""), "stagenet")
	e := newTestApp(t)
	buyer, session := e.user("lag_buyer", "buyer")
	vendor, _ := e.user("lag_vendor", "vendor")
	order := e.order(buyer, e.product(vendor, "digital"), "XMR", stateDraft)
	e.check(e.do("POST", "/orders/pay", session, url.Values{"order_id": {order}}), 303)
	if _, err := e.DB.Exec("UPDATE payment_addresses SET created=now()-interval '48 hours' WHERE order_id=$1", order); err != nil {
		t.Fatal(err)
	}
	w.set(func(m *payMoneroWallet) { m.walletHeight = 99 })
	reads := s.called("get_transfers")
	if err := e.A.pollOnce(context.Background()); err != nil {
		t.Fatalf("wallet lag should skip, not fail, the pass: %v", err)
	}
	var state, status string
	if err := e.DB.QueryRow("SELECT state FROM orders WHERE id=$1", order).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if err := e.DB.QueryRow("SELECT last_error FROM payment_status WHERE currency='XMR'").Scan(&status); err != nil {
		t.Fatal(err)
	}
	if state != stateAwaitingPayment || status != syncSkipReason("wallet height below daemon height", "") || s.called("get_transfers") != reads {
		t.Fatalf("wallet lag acted on old order: state=%s status=%q wallet reads=%d→%d", state, status, reads, s.called("get_transfers"))
	}
	w.set(func(m *payMoneroWallet) { m.walletHeight = 100 })
	if err := e.A.pollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := e.DB.QueryRow("SELECT state FROM orders WHERE id=$1", order).Scan(&state); err != nil || state != stateCancelled {
		t.Fatalf("wallet caught up: state=%s err=%v", state, err)
	}
}
