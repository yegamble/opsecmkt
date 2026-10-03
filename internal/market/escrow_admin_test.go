package market

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// Escrow Phase 1, slice P1-c2: the A-246 carry-overs (the administrator's market fee, the arbiter key range refilled
// before signing, the arbiter wallet dedicated) and A-334's admin copy, against the fake escrow Core, plus the exact
// params of the RPCs this slice added (simulated RPC, not chain evidence).

// The administrator sets the market fee and its destination on /admin (password step-up, audited without the
// address); a later escrow snapshots it, an earlier one keeps its own. A-334: the payments copy no longer says every
// deposit sits in one pooled wallet while escrow is on.
func TestEscrowAdminMarketFee(t *testing.T) {
	x := newEscrowEnv(t)
	adminID, admin := x.user("eadmin_"+randomToken()[:6], "admin")
	_, mod := x.user("emod_"+randomToken()[:6], "moderator")
	before := x.draft()
	x.mustCheckout(before, regtestAddr("fee-refund-0"))
	page := x.page("/admin", admin)
	for _, want := range []string{`action="/admin/escrow-fee"`, "0 basis points", "watch-only descriptor", "Bitcoin escrow orders (regtest only) never pay into that wallet"} {
		if !strings.Contains(page, want) {
			t.Fatalf("admin page lacks %q:\n%s", want, page)
		}
	}
	if strings.Contains(page, "Deposits sit in one pooled wallet per currency.") {
		t.Fatal("A-334: the pooled-wallet sentence is shown while escrow is on")
	}
	fee := regtestAddr("market-fee-admin")
	post := func(sess, bps, addr, password string) (int, string) {
		w := x.do("POST", "/admin/escrow-fee", sess, url.Values{"fee_bps": {bps}, "fee_address": {addr}, "password": {password}})
		return w.Code, w.Body.String()
	}
	for _, c := range []struct {
		name, sess, bps, addr, password string
		code                            int
	}{
		{"a moderator", mod, "250", fee, testPassword, 403},
		{"over 10000", admin, "10001", fee, testPassword, 400},
		{"not a number", admin, "2.5", fee, testPassword, 400},
		{"no address above 0", admin, "250", "", testPassword, 400},
		{"another network's address", admin, "250", "tb1qnotregtest0000000000000000000000000", testPassword, 400},
		{"an escrow's refund address", admin, "250", regtestAddr("fee-refund-0"), testPassword, 409},
		{"a wrong password", admin, "250", fee, "wrong password 321", 401},
		{"no change", admin, "0", "", testPassword, 409},
	} {
		if code, body := post(c.sess, c.bps, c.addr, c.password); code != c.code {
			t.Errorf("%s: %d %s", c.name, code, body)
		}
	}
	if agInt(x.testEnv, "SELECT count(*) FROM settings WHERE key LIKE 'escrow_btc_fee_%'") != 0 {
		t.Fatal("a refused save was stored")
	}
	// A-348: the notice adds its section's fragment itself, so the redirect names it once.
	if w := x.do("POST", "/admin/escrow-fee", admin, url.Values{"fee_bps": {"1500"}, "fee_address": {fee}, "password": {testPassword}}); w.Code != 303 ||
		w.Header().Get("Location") != "/admin#escrow-fee" || x.cookie(w, "notice") != "escrow-fee" {
		t.Fatalf("save: %d %q %s", w.Code, w.Header().Get("Location"), w.Body.String())
	}
	if agInt(x.testEnv, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE 'Changed the Bitcoin escrow market fee (regtest): 0 → 1500 basis points; fee destination changed%(confirmed with password)%'", adminID) != 1 {
		t.Fatal("audit row")
	}
	assertNotInAudit := agInt(x.testEnv, "SELECT count(*) FROM audit_events WHERE action LIKE '%'||$1||'%'", fee)
	if assertNotInAudit != 0 {
		t.Fatal("the fee address is in the audit log")
	}
	if page := x.page("/admin", admin); !strings.Contains(page, "1500 basis points (15% of the price)") || !strings.Contains(page, fee) {
		t.Fatalf("admin page after the save:\n%s", page)
	}
	after := x.draft()
	x.mustCheckout(after, regtestAddr("fee-refund-1"))
	if got := agStrings(t, x.testEnv, "SELECT order_id||' '||fee_bps||' '||fee_destination FROM escrows WHERE order_id IN ($1,$2) ORDER BY id", before, after); len(got) != 2 ||
		got[0] != before+" 0 " || got[1] != after+" 1500 "+fee {
		t.Fatalf("snapshots %q", got)
	}
	// The fee address is refused as a refund or claim address.
	if w := x.checkout(x.draft(), fee); w.Code != 409 {
		t.Fatalf("the fee address as a refund address: %d", w.Code)
	}
	// The claim of the later escrow pays the fee to it.
	x.fund(after, after, 3, 100000)
	if code, body := x.claim(after, x.vendorSess, regtestAddr("fee-claim-1")); code != 303 {
		t.Fatalf("claim: %d %s", code, body)
	}
	x.poll()
	id, _, _, _ := x.settlement(after)
	if n := agInt(x.testEnv, "SELECT count(*) FROM settlement_outputs WHERE settlement_id=$1 AND role='market_fee' AND address=$2 AND sats=15000", id, fee); n != 1 {
		t.Fatal("market fee output")
	}
	if code, _ := x.claim(after, x.vendorSess, fee); code != 409 {
		t.Fatal("the fee address as a claim address")
	}
	// Back to no fee.
	if code, body := post(admin, "0", "", testPassword); code != 303 {
		t.Fatalf("save 0: %d %s", code, body)
	}
}

// Before the arbiter signs, the market child key must be in its key range: if getaddressinfo says it is not, the
// range is refilled to escrowKeypoolAhead past the market index (keypoolrefill) and checked again; if it still is
// not, nothing is signed or sent and the settlement waits with a fixed class.
func TestEscrowKeypoolRefillBeforeSigning(t *testing.T) {
	x := newEscrowEnv(t)
	order, id := x.built(regtestAddr("kp-refund-1"), regtestAddr("kp-claim-1"), 100000)
	x.signAs(order, roleVendor, id)
	var index int64
	x.DB.QueryRow("SELECT market_index FROM escrows WHERE order_id=$1", order).Scan(&index)
	x.core.mu.Lock()
	x.core.arbiterShort, x.core.keypoolStuck = true, true
	x.core.mu.Unlock()
	x.poll()
	if r := x.settleByID(id); r.State != "party_signed" || r.Error != sendMarketKey || x.core.count("walletprocesspsbt") != 0 {
		t.Fatalf("key range short: %+v", r)
	}
	if path, params := x.core.lastCall(t, "keypoolrefill"); path != "/wallet/opsecmkt-arbiter" || params != "["+strconv.FormatInt(index+escrowKeypoolAhead, 10)+"]" {
		t.Fatalf("keypoolrefill %s %s", path, params)
	}
	if page := x.page("/order?id="+order, x.buyerSess); !strings.Contains(page, "escrow key could not sign it yet") {
		t.Fatal("the page does not say why")
	}
	x.core.mu.Lock()
	x.core.keypoolStuck = false
	x.core.mu.Unlock()
	x.poll()
	if r := x.settleByID(id); r.State != "broadcast" {
		t.Fatalf("after the refill: %+v", r)
	}
	_, params := x.core.lastCall(t, "getaddressinfo")
	if !strings.HasPrefix(params, `["bcrt1q`) {
		t.Fatalf("getaddressinfo of the market child address: %s", params)
	}
}

// The arbiter wallet must be dedicated to the escrow key: a wallet with transactions, or an encrypted one, leaves
// escrow disabled with one fixed line, and the market runs.
func TestEscrowArbiterWalletDedicated(t *testing.T) {
	logs := captureLog(t)
	core, srv := newFakeEscrowCore(t)
	setEscrowEnv(t, srv.url(""))
	core.arbiterTxCount = 3
	a, dsn, err := startApp(t, "")
	if err != nil || a.escrow.Load() != nil || !strings.Contains(logs.String(), escrowLogArbiterUse+" (getwalletinfo not-dedicated)") {
		t.Fatalf("arbiter with transactions: %v\n%s", err, logs.String())
	}
	a.Close()
	core.arbiterTxCount, core.arbiterEncrypted = 0, true
	if a, _, err = startApp(t, dsn); err != nil || a.escrow.Load() != nil || !strings.Contains(logs.String(), escrowLogArbiterUse+" (getwalletinfo encrypted)") {
		t.Fatalf("encrypted arbiter: %v\n%s", err, logs.String())
	}
	a.Close()
	core.arbiterEncrypted = false
	if a, _, err = startApp(t, dsn); err != nil || a.escrow.Load() == nil {
		t.Fatalf("dedicated arbiter: %v\n%s", err, logs.String())
	}
	if _, params := core.lastCall(t, "getwalletinfo"); params != "[]" {
		t.Fatalf("getwalletinfo %s", params)
	}
	a.Close()
}

// TestEscrowRPCPinsCosign pins the path and exact params of the RPCs slice P1-c2 added, and how their answers map:
// gettransaction of a settlement in the watch-only wallet (unknown = not found, negative confirmations with the
// conflicts), getaddressinfo and getwalletinfo of the arbiter wallet, and sendrawtransaction's txid and -27 as sent.
func TestEscrowRPCPinsCosign(t *testing.T) {
	ctx := context.Background()
	e, core, _ := testEscrowAdapter(t)
	const arb = "/wallet/opsecmkt-arbiter"
	steps := []struct {
		method, path, params string
		call                 func() error
	}{
		{"gettransaction", fakeWatchWallet, `["` + txidOf("settled") + `",true]`, func() error {
			w, err := e.walletTx(ctx, txidOf("settled"))
			if err == nil && w.Found {
				t.Error("an unknown transaction was found")
			}
			return err
		}},
		{"getaddressinfo", arb, `["bcrt1qmarketchild"]`, func() error { _, err := e.arbiterOwns(ctx, "bcrt1qmarketchild"); return err }},
		{"getwalletinfo", arb, `[]`, func() error { return e.checkArbiterWallet(ctx) }},
		{"walletprocesspsbt", arb, `["` + testCorePSBTUnsigned + `",true,"ALL",true,false]`, func() error { _, err := e.arbiterSign(ctx, testCorePSBTUnsigned); return err }},
		{"testmempoolaccept", "/", `[["00"],0.001]`, func() error { _, _, err := e.testMempoolAccept(ctx, "00", escrowMaxFeeRate); return err }},
		{"sendrawtransaction", "/", `["00",0.001]`, func() error { _, err := sendStored(ctx, e, "00", "txid-broadcast"); return err }},
	}
	for _, s := range steps {
		if err := s.call(); err != nil {
			t.Fatalf("%s: %v", s.method, err)
		}
		path, params := core.lastCall(t, s.method)
		if path != s.path || params != s.params {
			t.Errorf("%s: path %q params %s\nwant path %q params %s", s.method, path, params, s.path, s.params)
		}
	}
	// A known, conflicted transaction.
	core.mu.Lock()
	core.broadcast = map[string]string{txidOf("conflicted"): "00"}
	core.txConfs = map[string]int64{txidOf("conflicted"): -4}
	core.conflicts = map[string][]string{txidOf("conflicted"): {txidOf("winner")}}
	core.mu.Unlock()
	if w, err := e.walletTx(ctx, txidOf("conflicted")); err != nil || !w.Found || w.Confirmations != -4 || firstTxID(w.Conflicts) != txidOf("winner") {
		t.Fatalf("conflicted: %+v %v", w, err)
	}
	// sendrawtransaction: another txid is a bad reply; -27 is sent; -26 and -25 are refusals; others are errors.
	if sent, err := sendStored(ctx, e, "00", "another"); sent || err == nil {
		t.Fatal("another txid accepted")
	}
	for code, want := range map[int]string{-27: "sent", -26: "refused", -25: "refused", -1: "error"} {
		core.mu.Lock()
		core.sendErr = code
		core.mu.Unlock()
		sent, err := sendStored(ctx, e, "00", "txid-broadcast")
		got := "error"
		switch {
		case err == nil && sent:
			got = "sent"
		case err == nil:
			got = "refused"
		}
		if got != want {
			t.Errorf("sendrawtransaction %d: %s, want %s", code, got, want)
		}
	}
}
