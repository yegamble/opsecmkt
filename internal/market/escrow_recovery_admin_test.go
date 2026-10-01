package market

import (
	"html"
	"net/url"
	"strings"
	"testing"
)

// Escrow P1-d3: /admin's escrow recovery section and the administrator's rebroadcast confirmation. Fake escrow Core
// (simulated RPC, not chain evidence).

// A-363: under the restore gate, once the one send an administrator's confirmation allowed has happened, a second
// confirmation is refused and nothing is sent again, for a settlement that was only signed (sent by
// broadcastSettlement) and for one already broadcast (sent again by trackBroadcast); /admin and the order page say it
// was sent once. Once the gate is cleared the market follows and resends it by itself, as for any broadcast settlement.
func TestEscrowRebroadcastNotRepeatedUnderGate(t *testing.T) {
	x := newEscrowEnv(t)
	_, admin := x.user("rradmin", "admin")
	ok := url.Values{"not_sent": {"confirmed"}, "password": {testPassword}}
	// S: co-signed and stored, its broadcast killed; B: broadcast, unconfirmed.
	orderS, idS := x.built(regtestAddr("refund-rr-s"), regtestAddr("claim-rr-s"), 100000)
	x.signAs(orderS, roleVendor, idS)
	x.A.escrowPollOnce(x.killAtSend())
	x.sendNormally()
	s := x.settleByID(idS)
	orderB, idB := x.built(regtestAddr("refund-rr-b"), regtestAddr("claim-rr-b"), 100000)
	x.signAs(orderB, roleVendor, idB)
	x.poll()
	b := x.settleByID(idB)
	if s.State != "signed" || b.State != "broadcast" {
		t.Fatalf("before the restore: S %+v, B %+v", s, b)
	}
	x.setGate(true)
	x.restoreHold(idS, idB)
	sendsS, sendsB := x.sendsOf(s.Raw), x.sendsOf(b.Raw)
	for _, id := range []int64{idS, idB} {
		if code, body := x.rebroadcast(admin, id, ok); code != 303 {
			t.Fatalf("first confirmation of %d: %d %s", id, code, body)
		}
	}
	x.poll()
	x.poll()
	if x.sendsOf(s.Raw) != sendsS+1 || x.sendsOf(b.Raw) != sendsB+1 {
		t.Fatalf("after the confirmations: S %d sends (was %d), B %d (was %d)", x.sendsOf(s.Raw), sendsS, x.sendsOf(b.Raw), sendsB)
	}
	for _, id := range []int64{idS, idB} {
		if r := x.settleByID(id); r.State != "broadcast" {
			t.Fatalf("settlement %d after its confirmed send: %+v", id, r)
		}
		code, body := x.rebroadcast(admin, id, ok)
		if code != 409 || !strings.Contains(body, "Its confirmed rebroadcast was already sent") {
			t.Fatalf("a second confirmation of %d after the send: %d %s", id, code, body)
		}
	}
	x.poll()
	x.poll()
	if x.sendsOf(s.Raw) != sendsS+1 || x.sendsOf(b.Raw) != sendsB+1 {
		t.Fatal("sent again under the gate after the confirmed send")
	}
	page := html.UnescapeString(x.page("/admin", admin))
	_, held, _ := strings.Cut(page, "Escrow settlements waiting for a rebroadcast check")
	held, _, _ = strings.Cut(held, "</table>")
	if strings.Count(held, "Sent once after an administrator's confirmation") != 2 || strings.Contains(held, `action="/admin/escrow-rebroadcast"`) {
		t.Fatalf("/admin after the confirmed sends:\n%s", held)
	}
	if order := html.UnescapeString(x.page("/order?id="+orderS, x.buyerSess)); !strings.Contains(order, "the market broadcast it once; it follows its confirmations but does not send it again until the operator has finished") {
		t.Fatalf("order page after the confirmed send:\n%s", order)
	}
	// The gate cleared: an unconfirmed broadcast settlement is followed and sent again by the market itself.
	x.setGate(false)
	x.poll()
	if x.sendsOf(b.Raw) != sendsB+2 {
		t.Fatalf("after the gate was cleared: B %d sends, want %d", x.sendsOf(b.Raw), sendsB+2)
	}
	x.core.setTxConfs(b.TxID, 3, "")
	x.poll()
	if r := x.settleByID(idB); r.State != "confirmed" || r.Error != "" {
		t.Fatalf("B confirmed: %+v", r)
	}
	x.noPayouts(orderB)
}

// A-360: with ESCROW_BTC off the rebroadcast route is off (404), so /admin lists a held settlement without the
// "Rebroadcast once" form and says escrow must be on to confirm it; with escrow on the form is offered.
func TestEscrowRebroadcastFormOnlyWithEscrowOn(t *testing.T) {
	x := newEscrowEnv(t)
	_, admin := x.user("offadmin", "admin")
	order, id := x.built(regtestAddr("refund-off"), regtestAddr("claim-off"), 100000)
	x.signAs(order, roleVendor, id)
	x.A.escrowPollOnce(x.killAtSend())
	x.sendNormally()
	x.setGate(true)
	x.restoreHold(id)
	const off = "Bitcoin escrow is off on this market (ESCROW_BTC), so no rebroadcast can be confirmed"
	page := html.UnescapeString(x.page("/admin", admin))
	if !strings.Contains(page, `action="/admin/escrow-rebroadcast"`) || !strings.Contains(page, "Rebroadcast once") || strings.Contains(page, off) {
		t.Fatalf("/admin with escrow on:\n%s", page)
	}
	t.Setenv("ESCROW_BTC", "")
	x.restart()
	page = html.UnescapeString(x.page("/admin", admin))
	if !strings.Contains(page, shortID(order)) || strings.Contains(page, `action="/admin/escrow-rebroadcast"`) || strings.Contains(page, "Rebroadcast once") || !strings.Contains(page, off) {
		t.Fatalf("/admin with escrow off:\n%s", page)
	}
	if code, _ := x.rebroadcast(admin, id, url.Values{"not_sent": {"confirmed"}, "password": {testPassword}}); code != 404 {
		t.Fatalf("rebroadcast with escrow off: %d", code)
	}
	if r := x.settleByID(id); r.Error != restoredSettlementHold {
		t.Fatalf("the hold changed with escrow off: %+v", r)
	}
}
