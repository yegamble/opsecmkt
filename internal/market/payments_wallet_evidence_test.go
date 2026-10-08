package market

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPayoutWalletEvidenceIsAdminOnDemandAndAdvisory(t *testing.T) {
	_, override, rpc := newGapCore(t, "testnet4")
	p, err := newBitcoinProvider(context.Background(), rpc.url(""), "opsecmkt", "testnet4", 3)
	if err != nil {
		t.Fatal(err)
	}
	e := newTestApp(t)
	e.A.payments["BTC"] = p
	buyer, buyerSession := e.user("evidence_buyer", "buyer")
	vendor, _ := e.user("evidence_vendor", "vendor")
	_, modSession := e.user("evidence_mod", "moderator")
	_, adminSession := e.user("evidence_admin", "admin")
	order := e.order(buyer, e.product(vendor, "physical"), "BTC", stateCompleted)
	address := "tb1qpayoutdestination00000000000000000000"
	var id int64
	if err := e.DB.QueryRow(`INSERT INTO payouts(order_id,kind,user_id,currency,amount,address,state)
		VALUES($1,'release',$2,'BTC',100000,$3,'held') RETURNING id`, order, vendor, address).Scan(&id); err != nil {
		t.Fatal(err)
	}
	otherOrder := e.order(buyer, e.product(vendor, "physical"), "BTC", stateCompleted)
	var otherID int64
	if err := e.DB.QueryRow(`INSERT INTO payouts(order_id,kind,user_id,currency,amount,address,state)
		VALUES($1,'release',$2,'BTC',200000,'tb1qotherdestination00000000000000000','held') RETURNING id`, otherOrder, vendor).Scan(&otherID); err != nil {
		t.Fatal(err)
	}
	path := "/admin?wallet_evidence=" + strconv.FormatInt(id, 10)
	selectedDisclosure := `<details class="payout-actions" id="resolve-payout-` + strconv.FormatInt(id, 10) + `" open>`
	otherDisclosure := `<details class="payout-actions" id="resolve-payout-` + strconv.FormatInt(otherID, 10) + `" open>`
	if page := e.do("GET", "/admin", adminSession, nil); page.Code != 200 || rpc.called("listtransactions") != 0 {
		t.Fatalf("ordinary admin page read wallet: status=%d calls=%d", page.Code, rpc.called("listtransactions"))
	} else if strings.Contains(page.Body.String(), selectedDisclosure) || strings.Contains(page.Body.String(), otherDisclosure) {
		t.Fatal("ordinary admin page opened a payout recovery disclosure")
	}
	for _, session := range []string{buyerSession, modSession} {
		if page := e.do("GET", path, session, nil); page.Code != 403 || rpc.called("listtransactions") != 0 {
			t.Fatalf("non-admin evidence: status=%d calls=%d", page.Code, rpc.called("listtransactions"))
		}
	}
	if page := e.do("GET", "/admin?wallet_evidence=999999999", adminSession, nil); page.Code != 404 || rpc.called("listtransactions") != 0 {
		t.Fatalf("non-visible payout evidence: status=%d calls=%d", page.Code, rpc.called("listtransactions"))
	}
	match, other, recv := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	override.answer("listtransactions", func(_ string, _ string, params json.RawMessage) (any, int, string) {
		if string(params) != `["*",200,0]` {
			t.Errorf("unbounded or wrong Core read: %s", params)
		}
		return []map[string]any{
			{"category": "send", "address": address, "amount": json.Number("-0.00099000"), "confirmations": 0, "txid": match, "comment": "private wallet note"},
			{"category": "send", "address": "tb1qother", "amount": json.Number("-0.5"), "confirmations": 3, "txid": other},
			{"category": "receive", "address": address, "amount": json.Number("0.00099000"), "confirmations": 1, "txid": recv},
			{"category": "send", "address": address, "amount": json.Number("-0.1"), "confirmations": 1, "txid": "not-a-txid"},
			{"category": "send", "address": address, "amount": json.Number("0.1"), "confirmations": 1, "txid": strings.Repeat("9", 64)},
			{"category": "send", "address": address, "amount": json.Number("-0"), "confirmations": 1, "txid": strings.Repeat("8", 64)},
		}, 0, ""
	})
	page := e.do("GET", path, adminSession, nil)
	if page.Code != 200 || rpc.called("listtransactions") != 1 {
		t.Fatalf("selected wallet read: status=%d calls=%d body=%s", page.Code, rpc.called("listtransactions"), page.Body.String())
	}
	body := page.Body.String()
	if !strings.Contains(body, selectedDisclosure) || strings.Contains(body, otherDisclosure) {
		t.Fatal("selected wallet evidence did not open only its own payout recovery disclosure")
	}
	mustContain(t, body, match, "0.00099 BTC", "Unconfirmed", "Limited wallet read", "May be incomplete after a rescan", "none is proven to be this payout")
	mustNotContain(t, body, other, recv, "private wallet note", "not-a-txid", strings.Repeat("9", 64), strings.Repeat("8", 64))
	var state string
	if err := e.DB.QueryRow("SELECT state FROM payouts WHERE id=$1", id).Scan(&state); err != nil || state != "held" {
		t.Fatalf("advisory GET changed payout: %s %v", state, err)
	}
	// The evidence has no bearing on the existing password confirmation.
	e.check(e.do("POST", "/admin/payout", adminSession, url.Values{"payout_id": {strconv.FormatInt(id, 10)}, "op": {"release"}}), 400)
	if err := e.DB.QueryRow("SELECT state FROM payouts WHERE id=$1", id).Scan(&state); err != nil || state != "held" {
		t.Fatalf("recovery action bypassed confirmation: %s %v", state, err)
	}

	override.fail("listtransactions", -1, "SECRET wallet error and credentials")
	page = e.do("GET", path, adminSession, nil)
	if page.Code != 200 || !strings.Contains(page.Body.String(), "Wallet evidence unavailable") || strings.Contains(page.Body.String(), "SECRET") {
		t.Fatalf("wallet failure leaked or failed page: status=%d", page.Code)
	}
	// The non-waiting gate refuses excess simultaneous wallet reads without a node call.
	walletEvidenceSlots <- struct{}{}
	walletEvidenceSlots <- struct{}{}
	page = e.do("GET", path, adminSession, nil)
	<-walletEvidenceSlots
	<-walletEvidenceSlots
	if page.Code != 200 || !strings.Contains(page.Body.String(), "Wallet read busy") || rpc.called("listtransactions") != 2 {
		t.Fatalf("busy evidence: status=%d calls=%d", page.Code, rpc.called("listtransactions"))
	}
	if _, err := e.DB.Exec("UPDATE payouts SET address='' WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	page = e.do("GET", path, adminSession, nil)
	if page.Code != 200 || !strings.Contains(page.Body.String(), "no recorded destination address") || rpc.called("listtransactions") != 2 {
		t.Fatalf("addressless evidence: status=%d calls=%d", page.Code, rpc.called("listtransactions"))
	}
	if _, err := e.DB.Exec("UPDATE payouts SET address=$2 WHERE id=$1", id, address); err != nil {
		t.Fatal(err)
	}
	e.A.payments["BTC"] = newFakeProvider("BTC", 3)
	page = e.do("GET", path, adminSession, nil)
	if page.Code != 200 || !strings.Contains(page.Body.String(), "does not offer wallet evidence") || rpc.called("listtransactions") != 2 {
		t.Fatalf("unsupported provider evidence: status=%d calls=%d", page.Code, rpc.called("listtransactions"))
	}
}

func TestPayoutWalletEvidenceBitcoinMatchCapAndStatuses(t *testing.T) {
	_, override, rpc := newGapCore(t, "testnet4")
	p, err := newBitcoinProvider(context.Background(), rpc.url(""), "opsecmkt", "testnet4", 3)
	if err != nil {
		t.Fatal(err)
	}
	address := "tb1qpayoutdestination00000000000000000000"
	rows := make([]map[string]any, bitcoinEvidenceEntries)
	for i := range rows {
		rows[i] = map[string]any{"category": "send", "address": address, "amount": json.Number("-0.001"), "confirmations": 2,
			"txid": strings.Repeat("a", 63) + strconv.FormatInt(int64(i%10), 10)}
	}
	rows[len(rows)-1]["abandoned"] = true
	rows[len(rows)-2]["confirmations"] = -2
	override.answer("listtransactions", func(_, _ string, _ json.RawMessage) (any, int, string) { return rows, 0, "" })
	read, err := p.ReadWalletEvidence(context.Background(), address)
	if err != nil || !read.Limited || len(read.Entries) != walletEvidenceMaxMatches || read.Entries[0].Status != "Abandoned" || read.Entries[1].Status != "Conflicted" {
		t.Fatalf("bounded Core matches: %+v %v", read, err)
	}
}

func TestPayoutWalletEvidenceTimeoutAndMoneroOmissions(t *testing.T) {
	_, override, rpc := newGapCore(t, "testnet4")
	p, err := newBitcoinProvider(context.Background(), rpc.url(""), "opsecmkt", "testnet4", 3)
	if err != nil {
		t.Fatal(err)
	}
	e := newTestApp(t)
	e.A.payments["BTC"] = p
	buyer, _ := e.user("timeout_buyer", "buyer")
	vendor, _ := e.user("timeout_vendor", "vendor")
	_, adminSession := e.user("timeout_admin", "admin")
	order := e.order(buyer, e.product(vendor, "physical"), "BTC", stateCompleted)
	var id int64
	if err := e.DB.QueryRow(`INSERT INTO payouts(order_id,kind,user_id,currency,amount,address,state)
		VALUES($1,'release',$2,'BTC',100000,'tb1qtimeoutdestination0000000000000000','held') RETURNING id`, order, vendor).Scan(&id); err != nil {
		t.Fatal(err)
	}
	override.stall("listtransactions", true)
	old := walletEvidenceDeadline
	walletEvidenceDeadline = 35 * time.Millisecond
	t.Cleanup(func() { walletEvidenceDeadline = old })
	page := e.do("GET", "/admin?wallet_evidence="+strconv.FormatInt(id, 10), adminSession, nil)
	if page.Code != 200 || !strings.Contains(page.Body.String(), "Wallet evidence unavailable") || rpc.called("listtransactions") != 1 {
		t.Fatalf("timed-out evidence: status=%d calls=%d", page.Code, rpc.called("listtransactions"))
	}

	_, xoverride, xrpc := newGapMonero(t)
	xmr, err := newMoneroProvider(context.Background(), xrpc.url(""), "", "stagenet", 10)
	if err != nil {
		t.Fatal(err)
	}
	xoverride.answer("get_height", func(_, _ string, _ json.RawMessage) (any, int, string) {
		return map[string]any{"height": 100000}, 0, ""
	})
	address := payStagenetSub
	match, omitted := strings.Repeat("d", 64), strings.Repeat("e", 64)
	xoverride.answer("get_transfers", func(_, _ string, params json.RawMessage) (any, int, string) {
		if !strings.Contains(string(params), `"min_height":78400`) || !strings.Contains(string(params), `"failed":true`) || !strings.Contains(string(params), `"pending":true`) {
			t.Errorf("Monero read is not bounded or missed states: %s", params)
		}
		return map[string]any{
			"out": []map[string]any{{"txid": match, "confirmations": 2, "destinations": []map[string]any{{"address": address, "amount": 990000000000}}},
				{"txid": omitted, "confirmations": 2, "note": "private wallet note"}},
			"pending": []map[string]any{{"txid": strings.Repeat("f", 64), "confirmations": 0, "destinations": []map[string]any{{"address": "7other", "amount": 1000000000000}}}},
			"failed":  []map[string]any{{"txid": strings.Repeat("1", 64), "confirmations": 0, "destinations": []map[string]any{{"address": address, "amount": 900000000000}}}},
		}, 0, ""
	})
	read, err := xmr.ReadWalletEvidence(context.Background(), address)
	if err != nil || !read.Limited || len(read.Entries) != 2 || read.Entries[0].Status != "Failed outgoing" || read.Entries[1].TxID != match || read.Entries[1].Amount != "0.99" || !strings.Contains(read.Scope, "omitted their destinations") || xrpc.called("get_transfers") != 1 {
		t.Fatalf("Monero bounded evidence: %+v %v", read, err)
	}
	if strings.Contains(read.Scope, omitted) || strings.Contains(read.Scope, "private wallet note") {
		t.Fatal("Monero wallet metadata leaked")
	}
}
