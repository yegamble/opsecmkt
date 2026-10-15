package market

import (
	"context"

	"fmt"

	"testing"
)

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
		{"getwalletinfo", arb, `[]`, func() error {
			if state, class, err := (&App{}).checkArbiterWallet(ctx, e); state != "" || err != nil { // txcount 0: nothing read, no database
				return fmt.Errorf("%s (%s) %v", state, class, err)
			}
			return nil
		}},
		{"walletprocesspsbt", arb, `["` + testCorePSBTUnsigned + `",true,"ALL",true,false]`, func() error { _, err := e.arbiterSign(ctx, testCorePSBTUnsigned); return err }},
		{"testmempoolaccept", "/", `[["00"],0.001]`, func() error { _, _, err := e.testMempoolAccept(ctx, "00", escrowMaxFeeRate); return err }},
		{"sendrawtransaction", "/", `["00",0.001]`, func() error { _, err := (&App{}).sendStored(ctx, e, "00", "txid-broadcast"); return err }},
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
	if sent, err := (&App{}).sendStored(ctx, e, "00", "another"); sent || err == nil {
		t.Fatal("another txid accepted")
	}
	for code, want := range map[int]string{-27: "sent", -26: "refused", -25: "refused", -1: "error"} {
		core.mu.Lock()
		core.sendErr = code
		core.mu.Unlock()
		sent, err := (&App{}).sendStored(ctx, e, "00", "txid-broadcast")
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
