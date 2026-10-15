package market

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// mineBlocks never takes generatetoaddress's silent [] (Core's answer on signet when maxtries runs out) for a block, and
// on signet it passes maxtries 2000000000 and asks again for the blocks still missing. No database; a scripted
// JSON-RPC server.
func TestEscrowRealChainMineRefusesEmpty(t *testing.T) {
	type call struct {
		Params []any `json:"params"`
	}
	serve := func(answers ...string) (*rpcClient, *[]call) {
		var calls []call
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			var c call
			json.Unmarshal(body, &c)
			calls = append(calls, c)
			if len(calls) > 5 { // a helper that keeps asking after an empty answer: stop it, the call count tells
				io.WriteString(w, `{"result":null,"error":{"code":-1,"message":"too many calls"},"id":"x"}`)
				return
			}
			result := "[]"
			if len(calls) <= len(answers) {
				result = answers[len(calls)-1]
			}
			io.WriteString(w, `{"result":`+result+`,"error":null,"id":"x"}`)
		}))
		t.Cleanup(srv.Close)
		u, _ := url.Parse(srv.URL)
		u.User = url.UserPassword("u", "p")
		c, err := newRPCClient("mine test", u.String(), "1.0", false)
		if err != nil {
			t.Fatal(err)
		}
		return c, &calls
	}
	h := func(b byte) string { return `"` + strings.Repeat(string(b), 64) + `"` }
	ctx := context.Background()
	for _, chain := range []string{"regtest", "signet"} {
		c, calls := serve("[]")
		if mined, err := mineBlocks(ctx, c, chain, 3, "addr"); err == nil || len(mined) != 0 {
			t.Errorf("%s: an empty answer was taken for blocks: %v %v", chain, mined, err)
		}
		if len(*calls) != 1 {
			t.Errorf("%s: %d calls after an empty answer", chain, len(*calls))
		}
	}
	// Signet: 2 of 3 blocks, then the missing one, each call with maxtries 2000000000.
	c, calls := serve("["+h('a')+","+h('b')+"]", "["+h('c')+"]")
	mined, err := mineBlocks(ctx, c, "signet", 3, "addr")
	if err != nil || len(mined) != 3 || len(*calls) != 2 {
		t.Fatalf("signet in two calls: %v %v (%d calls)", mined, err, len(*calls))
	}
	for i, want := range []float64{3, 1} {
		p := (*calls)[i].Params
		if len(p) != 3 || p[0] != want || p[1] != "addr" || p[2] != float64(signetMaxTries) {
			t.Errorf("signet call %d params %v", i, p)
		}
	}
	// Signet: a call that mines blocks then one that mines none fails, keeping what was mined.
	c, _ = serve("["+h('a')+"]", "[]")
	if mined, err = mineBlocks(ctx, c, "signet", 2, "addr"); err == nil || len(mined) != 1 {
		t.Fatalf("signet, then empty: %v %v", mined, err)
	}
	// Regtest: one call with no maxtries; fewer blocks than asked for fails, more than asked for fails.
	c, calls = serve("[" + h('a') + "," + h('b') + "]")
	if mined, err = mineBlocks(ctx, c, "regtest", 2, "addr"); err != nil || len(mined) != 2 || len((*calls)[0].Params) != 2 {
		t.Fatalf("regtest: %v %v %v", mined, err, *calls)
	}
	c, _ = serve("[" + h('a') + "]")
	if _, err = mineBlocks(ctx, c, "regtest", 2, "addr"); err == nil {
		t.Fatal("regtest: 1 of 2 blocks accepted")
	}
	c, _ = serve("[" + h('a') + "," + h('b') + "]")
	if _, err = mineBlocks(ctx, c, "signet", 1, "addr"); err == nil {
		t.Fatal("signet: 2 blocks for 1 accepted")
	}
}

// TestEscrowPrivateSignetPrepare readies node A of the private signet (ESCROW_REGTEST_MINER_RPC_URL; run by
// scripts/test-escrow-regtest.sh --private-signet before the legs, never in CI): it loads or creates the miner wallet,
// mines to it until at least 150 BTC are mature (the first time: about 110 blocks, each checked by mineBlocks), and
// mines one more block so the tip is fresh (a cached datadir's tip is old: the sync guard's tip age and Core's initial
// block download).
func TestEscrowPrivateSignetPrepare(t *testing.T) {
	if realChainNet() != "signet" || os.Getenv("ESCROW_REGTEST_MINER_RPC_URL") == "" {
		t.Skip("set ESCROW_REGTEST_CHAIN=signet and ESCROW_REGTEST_MINER_RPC_URL (scripts/test-escrow-regtest.sh --private-signet)")
	}
	ctx := context.Background()
	miner := privateSignetMiner(t, ctx)
	var version struct {
		Subversion string `json:"subversion"`
	}
	if err := miner.call(ctx, "", "getnetworkinfo", []any{}, &version); err != nil {
		t.Fatal(err)
	}
	t.Logf("%s: node A %s, signet_challenge %s", privateSignetLabel, version.Subversion, privateSignetChallenge)
	wallet := "/wallet/" + privateSignetMinerWallet
	if err := miner.call(ctx, "", "loadwallet", []any{privateSignetMinerWallet, true}, nil); err != nil {
		if err := miner.call(ctx, wallet, "getwalletinfo", []any{}, nil); err != nil {
			if err := miner.call(ctx, "", "createwallet", []any{privateSignetMinerWallet, false, false, "", false, true, true}, nil); err != nil {
				t.Fatalf("createwallet: %v", err)
			}
		}
	}
	var addr string
	if err := miner.call(ctx, wallet, "getnewaddress", []any{}, &addr); err != nil {
		t.Fatal(err)
	}
	balance := func() float64 {
		var b struct {
			Mine struct {
				Trusted float64 `json:"trusted"`
			} `json:"mine"`
		}
		if err := miner.call(ctx, wallet, "getbalances", []any{}, &b); err != nil {
			t.Fatal(err)
		}
		return b.Mine.Trusted
	}
	start := time.Now()
	mined := 0
	for b := balance(); b < 150; b = balance() {
		if mined >= 300 {
			t.Fatalf("%s: %d blocks mined and only %.8f BTC mature", privateSignetLabel, mined, b)
		}
		mustMine(t, ctx, miner, 10, addr)
		mined += 10
		t.Logf("%s: mined %d blocks so far (%s), %.8f BTC mature", privateSignetLabel, mined, time.Since(start).Round(time.Second), balance())
	}
	mustMine(t, ctx, miner, 1, addr)
	var info struct {
		Blocks int64 `json:"blocks"`
		IBD    bool  `json:"initialblockdownload"`
	}
	if err := miner.call(ctx, "", "getblockchaininfo", []any{}, &info); err != nil || info.IBD {
		t.Fatalf("node A after a fresh block: %+v %v", info, err)
	}
	t.Logf("%s: node A at height %d, initial block download over, miner wallet %.8f BTC mature (%d blocks mined to mature it in %s, plus one fresh block)",
		privateSignetLabel, info.Blocks, balance(), mined, time.Since(start).Round(time.Second))
}
