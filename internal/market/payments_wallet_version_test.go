package market

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"net/url"
	"strings"
	"testing"
)

// War-room A-174: moneroPreSubmitCodes holds only for the monero-wallet-rpc it was verified against. Check reads
// get_version at startup and on every refresh; any other answer, or none, makes every transfer error ambiguous.

func TestMoneroWalletVersionGatesDefiniteSendErrors(t *testing.T) {
	ctx := context.Background()
	w, ov, s := newGapMonero(t)
	xp, err := newMoneroProvider(ctx, s.url(""), "", "stagenet", 1)
	if err != nil {
		t.Fatal(err)
	}
	send := func() error {
		t.Helper()
		ov.fail("transfer", -17, "not enough money")
		defer ov.clear()
		txid, err := xp.Send(ctx, payStagenetSub, 42)
		var re *rpcError
		if txid != "" || !errors.As(err, &re) || re.Code != -17 || !strings.Contains(err.Error(), "not enough money") {
			t.Fatalf("transfer -17: txid=%q err=%v", txid, err)
		}
		return err
	}
	check := func() {
		t.Helper()
		if _, err := xp.Check(ctx); err != nil {
			t.Fatal(err)
		}
	}
	version := func(wantUnverified bool, want string) {
		t.Helper()
		text, unverified := xp.WalletVersion()
		if unverified != wantUnverified || !strings.Contains(text, want) {
			t.Fatalf("WalletVersion() = %q, %v; want %q, %v", text, unverified, want, wantUnverified)
		}
	}

	// The verified wallet (v0.18.5.1 reports 1.31, a release build): the table applies, as before A-174.
	version(false, "RPC version 1.31")
	if err := send(); !walletRejected("XMR", err) || payoutErrorClass("XMR", err) != "definite/rpc:-17" {
		t.Fatalf("verified wallet: -17 not definite: %v", err)
	}

	unverified := []struct {
		name  string
		setup func()
		want  string
	}{
		{"another RPC version", func() { w.set(func(m *payMoneroWallet) { m.rpcVersion = 1<<16 | 30 }) }, "RPC version 1.30"},
		{"a newer RPC version", func() { w.set(func(m *payMoneroWallet) { m.rpcVersion = 1<<16 | 35 }) }, "RPC version 1.35"},
		{"a development build of 1.31", func() { w.set(func(m *payMoneroWallet) { m.devBuild = true }) }, "not a release build"},
		{"get_version failing", func() { ov.fail("get_version", -32601, "Method not found") }, "get_version failed"},
		{"no release flag", func() {
			ov.answer("get_version", func(string, string, json.RawMessage) (any, int, string) {
				return map[string]any{"version": moneroVerifiedWalletRPC}, 0, ""
			})
		}, "not a release build"},
		{"an unreadable version", func() {
			ov.answer("get_version", func(string, string, json.RawMessage) (any, int, string) {
				return map[string]any{"version": "0.18.5.1", "release": true}, 0, ""
			})
		}, "get_version failed"},
	}
	for _, c := range unverified {
		c.setup()
		check()
		ov.clear()
		w.set(func(m *payMoneroWallet) { m.rpcVersion, m.devBuild = 0, false })
		version(true, c.want)
		err := send()
		if walletRejected("XMR", err) || payoutErrorClass("XMR", err) != "ambiguous/rpc:-17" {
			t.Fatalf("%s: -17 classified definite: %v", c.name, err)
		}
		if !strings.Contains(err.Error(), "not verified") || !strings.Contains(err.Error(), moneroVerifiedWalletRelease) {
			t.Fatalf("%s: error does not say the wallet is unverified: %v", c.name, err)
		}
		// The next check reads the verified wallet again and the table applies again.
		check()
		version(false, "RPC version 1.31")
		if err := send(); !walletRejected("XMR", err) {
			t.Fatalf("%s: verified again but -17 not definite: %v", c.name, err)
		}
	}

	// A check that fails before reading the version leaves the wallet unverified, never the last good answer.
	w.set(func(m *payMoneroWallet) { m.primary = "" })
	if _, err := xp.Check(ctx); err == nil {
		t.Fatal("check with no primary address passed")
	}
	version(true, "not read")
	if err := send(); walletRejected("XMR", err) {
		t.Fatalf("failed check: -17 classified definite: %v", err)
	}
	// Transport failures are ambiguous whatever the version and keep their own text.
	w.set(func(m *payMoneroWallet) { m.primary = payStagenetPrimary })
	check()
	ov.answer("transfer", func(string, string, json.RawMessage) (any, int, string) { return "not an object", 0, "" })
	if _, err := xp.Send(ctx, payStagenetSub, 42); err == nil || walletRejected("XMR", err) || strings.Contains(err.Error(), "not verified") {
		t.Fatalf("unreadable transfer reply: %v", err)
	}
}

func TestBitcoinNodeVersionIsRecorded(t *testing.T) {
	ctx := context.Background()
	_, ov, s := newGapCore(t, "testnet4")
	bp, err := newBitcoinProvider(ctx, s.url(""), "opsecmkt", "testnet4", 1)
	if err != nil {
		t.Fatal(err)
	}
	if text, unverified := bp.WalletVersion(); text != "Bitcoin Core 31.1.0" || unverified {
		t.Fatalf("WalletVersion() = %q, %v", text, unverified)
	}
	// Bitcoin's classification does not depend on the version (it commits before relaying): a failing
	// getnetworkinfo is shown, and the check and every sendtoaddress error stay as they were.
	ov.fail("getnetworkinfo", -32601, "Method not found")
	if _, err := bp.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if text, unverified := bp.WalletVersion(); !strings.Contains(text, "getnetworkinfo failed") || unverified {
		t.Fatalf("WalletVersion() after a failed getnetworkinfo = %q, %v", text, unverified)
	}
	ov.fail("sendtoaddress", -6, "Insufficient funds")
	if _, err := bp.Send(ctx, gapVendorBTC, 1000); !walletRejected("BTC", err) {
		t.Fatalf("bitcoin -6 not definite: %v", err)
	}
}

// Through the watcher and PostgreSQL: with an unverified wallet a -17 is recorded as possibly broadcast, requeueing
// it needs the wallet confirmation, and /admin names the version and the bump checklist; once the verified
// wallet is back a -17 is a definite rejection again.
func TestUnverifiedMoneroWalletRecordsSendFailuresAmbiguous(t *testing.T) {
	p := newPayEnv(t)
	ctx := context.Background()
	g, _ := gapXMR(t, p, 1)
	if _, err := p.DB.Exec("UPDATE users SET payout_xmr=$1 WHERE id=$2", payStagenetSub, p.vendor.ID); err != nil {
		t.Fatal(err)
	}
	g.w.set(func(m *payMoneroWallet) { m.rpcVersion = 1<<16 | 35 }) // a later monero-wallet-rpc
	failed := map[bool]string{}
	for i, verified := range []bool{false, true} {
		if verified {
			g.w.set(func(m *payMoneroWallet) { m.rpcVersion = 0 })
		}
		addr := "7gapxmrversion" + strings.Repeat(string(rune('a'+i)), 81)
		g.subaddress(addr, int64(60+i))
		order := p.gapOrder("XMR", addr)
		g.deposit(addr, "xmr-version-"+string(rune('a'+i)), 100000, 1)
		p.poll()
		p.complete(order)
		g.ov.fail("transfer", -17, "not enough money")
		if err := p.A.pollOnce(ctx); err == nil || !strings.Contains(err.Error(), "not enough money") {
			t.Fatalf("send failure not reported: %v", err)
		}
		g.reset()
		p.failedPayout(order, "not enough money", !verified)
		failed[verified] = order
		admin := html.UnescapeString(p.page("/admin", p.adminSess))
		warning := "XMR wallet version not verified"
		if !verified {
			if !strings.Contains(admin, warning) || !strings.Contains(admin, "RPC version 1.35") || !strings.Contains(admin, "docs/operator-guide.md#node-image-versions") || !strings.Contains(admin, moneroVerifiedWalletRelease) || !strings.Contains(admin, "subtract_fee_from_outputs) is also unverified") {
				t.Fatal("admin page does not name the unverified wallet version and the bump checklist")
			}
			if !strings.Contains(p.str("SELECT error FROM payouts WHERE order_id=$1", order), "RPC version 1.35") {
				t.Fatal("payout error does not name the unverified wallet version")
			}
		} else if strings.Contains(admin, warning) || !strings.Contains(admin, "RPC version 1.31") {
			t.Fatal("admin page warns about the verified wallet")
		}
	}
	amb := p.payoutID(failed[false])
	calls := g.s.called("transfer")
	w := p.do("POST", "/admin/payout", p.adminSess, url.Values{"payout_id": {amb}, "op": {"requeue"}, "password": {testPassword}})
	if w.Code != 400 || !strings.Contains(html.UnescapeString(w.Body.String()), "may have been broadcast") {
		t.Fatalf("unconfirmed requeue of an unverified-wallet failure: %d", w.Code)
	}
	p.poll()
	if st, _, _, _ := p.payout(failed[false]); st != "failed" || g.s.called("transfer") != calls {
		t.Fatalf("unverified-wallet failure resent without confirmation: %s calls=%d", st, g.s.called("transfer"))
	}
	if code, body := p.payoutAction(p.adminSess, p.payoutID(failed[true]), "requeue", "", testPassword); code != 303 {
		t.Fatalf("definite requeue: %d %s", code, body)
	}
}
