package market

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Escrow Phase 3, slice X-d: the real-chain legs of Monero escrow settlements (scripts/test-escrow-xmr-regtest.sh;
// run by the escrow-xmr-regtest CI job), on monerod and monero-wallet-rpc 0.18.5.1 fakechain. Each leg runs a real App and its Monero escrow
// watcher against the escrow wallet-rpc through a counting proxy (xmrRPCProxy), which can also lose a call's answer
// after the wallet acted on it. Claim and refund addresses are subaddresses of the funder's wallet, so every settlement
// pays a real wallet. Each relayed settlement is decoded by the daemon (xmrShape): RingCT type 6 (CLSAG and
// Bulletproofs+), 16 ring members per input and a fee above 0 and within ESCROW_XMR_MAX_FEE.
//
// A transaction a wallet-rpc just relayed may be left out of the next block (monerod holds it in its Dandelion++ stem
// for a moment), so these legs mine xmrMineMargin blocks more than a threshold needs; X-c's Funding leg checks the
// threshold itself.

// xmrMineMargin: blocks mined beyond a confirmation threshold, for a transaction that missed the next block.
const xmrMineMargin = 2

// xmrRPCProxy forwards JSON-RPC requests to the escrow wallet-rpc, counting each method; drop[method] makes the next
// call of method lose its answer after the wallet-rpc answered it (a timeout after the call took effect), and
// around[method] runs around the forwarding of each call of method (it must call forward once).
type xmrRPCProxy struct {
	*httptest.Server
	mu     sync.Mutex
	counts map[string]int
	drop   map[string]bool
	around map[string]func(forward func())
	wallet *rpcClient // the escrow wallet-rpc itself, for calls the proxy must not count

	// transferError is a fakechain-only diagnostic; never retain requests or successful wallet results.
	transferError *rpcError
}

func newXMRRPCProxy(t *testing.T, target string) *xmrRPCProxy {
	t.Helper()
	p := &xmrRPCProxy{counts: map[string]int{}, drop: map[string]bool{}, around: map[string]func(func()){}}
	var err error
	if p.wallet, err = newRPCClient("monero escrow wallet", target, "2.0", true); err != nil {
		t.Fatal(err)
	}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Method string `json:"method"`
		}
		json.Unmarshal(body, &req)
		p.mu.Lock()
		p.counts[req.Method]++
		drop := p.drop[req.Method]
		delete(p.drop, req.Method)
		around := p.around[req.Method]
		p.mu.Unlock()
		var resp *http.Response
		var err error
		forward := func() {
			resp, err = http.Post(strings.TrimRight(target, "/")+r.URL.Path, "application/json", bytes.NewReader(body))
		}
		if around != nil {
			around(forward)
		} else {
			forward()
		}
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		answer, _ := io.ReadAll(resp.Body)
		if req.Method == "transfer" {
			var reply struct {
				Error *rpcError `json:"error"`
			}
			json.Unmarshal(answer, &reply)
			if reply.Error != nil {
				reply.Error.Method = req.Method
			}
			p.mu.Lock()
			p.transferError = reply.Error
			p.mu.Unlock()
		}
		if drop {
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				conn.Close()
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		w.Write(answer)
	}))
	t.Cleanup(p.Close)
	return p
}

func (p *xmrRPCProxy) count(method string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.counts[method]
}

// transferFailure returns the last transfer's raw error for fakechain failure messages only.
func (p *xmrRPCProxy) transferFailure() *rpcError {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.transferError
}

func (p *xmrRPCProxy) dropNext(method string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.drop[method] = true
}

// wrap runs fn around the forwarding of each later call of method (nil removes it).
func (p *xmrRPCProxy) wrap(method string, fn func(forward func())) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.around[method] = fn
}

// newXMRSettleRegtest is newXMRRegtest (with its env) with the App's escrow wallet-rpc behind a counting proxy.
func newXMRSettleRegtest(t *testing.T, env ...func(r *xmrRegtest) map[string]string) (*xmrRegtest, *xmrRPCProxy) {
	t.Helper()
	walletURL := os.Getenv("ESCROW_XMR_REGTEST_WALLET_URL")
	if walletURL == "" {
		t.Skip("set ESCROW_XMR_REGTEST_DAEMON_URL, _WALLET_URL, _FUNDER_URL and _RESTORE_URL (scripts/test-escrow-xmr-regtest.sh) to run against monerod --regtest")
	}
	p := newXMRRPCProxy(t, walletURL)
	t.Setenv("ESCROW_XMR_REGTEST_WALLET_URL", p.URL)
	r := newXMRRegtest(t, env...)
	r.mustPoll()
	return r, p
}

// funderAddress is a new subaddress of the funder's wallet: a real destination for a claim or a refund.
func (r *xmrRegtest) funderAddress() string {
	r.t.Helper()
	var res struct {
		Address string `json:"address"`
	}
	r.call(r.funder, "create_address", map[string]any{"account_index": 0}, &res)
	return res.Address
}

// daemonPost posts body to one of the daemon's plain JSON endpoints (path) and decodes the answer into out.
func (r *xmrRegtest) daemonPost(path string, body, out any) {
	r.t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(strings.TrimRight(os.Getenv("ESCROW_XMR_REGTEST_DAEMON_URL"), "/")+path, "application/json", bytes.NewReader(b))
	if err != nil {
		r.t.Fatalf("%s: %v", path, err)
	}
	defer resp.Body.Close()
	if err = json.NewDecoder(resp.Body).Decode(out); err != nil {
		r.t.Fatalf("%s: %v", path, err)
	}
}

// checkoutReal takes a Monero escrow for a new digital order with a real refund address (a funder subaddress) and
// returns the order and its escrow address.
func (r *xmrRegtest) checkoutReal() (order, addr string) {
	r.t.Helper()
	buyer, sess := r.user("xr_b_"+randomToken()[:8], "buyer")
	r.enableOrderPGP(buyer)
	order = r.order(buyer, r.digital(r.vendor), "XMR", stateDraft)
	if w := r.checkout(sess, order, r.funderAddress()); w.Code != 303 {
		r.t.Fatalf("checkout: %d %s", w.Code, w.Body.String())
	}
	return order, agStrings(r.t, r.testEnv, "SELECT address FROM escrows WHERE order_id=$1", order)[0]
}

// completedReal is a digital order paid its price and mined to 10 confirmations: delivered on funding, then completed
// by the watcher's deadline step when its dispute window ends (endWindows, A-709).
func (r *xmrRegtest) completedReal() (order, addr string) {
	r.t.Helper()
	order, addr = r.checkoutReal()
	r.pay(map[string]int64{addr: xmrListingPrice})
	r.mine(10 + xmrMineMargin)
	r.mustPoll()
	if r.state(order) != stateDelivered {
		r.t.Fatalf("funded order: %s (deposits %v)", r.state(order), r.depositConfs(order))
	}
	r.endWindows()
	if r.state(order) != stateCompleted {
		r.t.Fatalf("after its dispute window: %s", r.state(order))
	}
	return order, addr
}

// depositConfs lists order's deposits as txid prefix:confirmations:left out, for a failure message.
func (r *xmrRegtest) depositConfs(order string) []string {
	return agStrings(r.t, r.testEnv, "SELECT left(txid,12)||':'||confirmations||':'||escrow_left_out FROM payments WHERE order_id=$1 ORDER BY created,id", order)
}

// releaseReal requests order's release to a fresh funder subaddress (as the vendor's claim does, slice X-e). A claim
// follows completion, so a digital order still in its dispute window is completed first (endWindows, A-709).
func (r *xmrRegtest) releaseReal(order string) int64 {
	r.t.Helper()
	r.endWindows()
	var id int64
	if err := r.DB.QueryRow("INSERT INTO xmr_settlements(order_id,kind,destination) VALUES($1,'release',$2) RETURNING id", order, r.funderAddress()).Scan(&id); err != nil {
		r.t.Fatal(err)
	}
	return id
}

// xmrShape decodes settlement s's transaction in the daemon: RingCT type 6, 16 key offsets per input, and a fee above 0
// and within ESCROW_XMR_MAX_FEE that is the stored one.
func (r *xmrRegtest) xmrShape(s xmrSettlement) {
	r.t.Helper()
	var res struct {
		Txs []struct {
			AsJSON string `json:"as_json"`
		} `json:"txs"`
	}
	r.daemonPost("/get_transactions", map[string]any{"txs_hashes": []string{s.TxHash}, "decode_as_json": true}, &res)
	if len(res.Txs) != 1 {
		r.t.Fatalf("the daemon does not have %s", s.TxHash)
	}
	var body struct {
		Vin []struct {
			Key struct {
				KeyOffsets []json.Number `json:"key_offsets"`
			} `json:"key"`
		} `json:"vin"`
		RCT struct {
			Type int64 `json:"type"`
			Fee  int64 `json:"txnFee"`
		} `json:"rct_signatures"`
	}
	if err := json.Unmarshal([]byte(res.Txs[0].AsJSON), &body); err != nil {
		r.t.Fatal(err)
	}
	for _, in := range body.Vin {
		if len(in.Key.KeyOffsets) != 16 {
			r.t.Fatalf("transaction %s: an input has %d ring members", s.TxHash, len(in.Key.KeyOffsets))
		}
	}
	if body.RCT.Type != 6 || len(body.Vin) != len(s.KeyImages) || body.RCT.Fee != s.Fee || s.Fee <= 0 || s.Fee > r.A.escrowXMR.maxFee {
		r.t.Fatalf("transaction %s: rct type %d, %d inputs, fee %d (stored %d, cap %d)", s.TxHash, body.RCT.Type, len(body.Vin), body.RCT.Fee, s.Fee, r.A.escrowXMR.maxFee)
	}
	r.t.Logf("transaction %s: rct type 6, %d input(s) of ring 16, fee %s XMR (cap %s XMR)", s.TxHash, len(body.Vin), amount(s.Fee, 12), amount(r.A.escrowXMR.maxFee, 12))
}

// confirmReal mines past the threshold and checks settlement id is confirmed.
func (r *xmrRegtest) confirmReal(id int64) {
	r.t.Helper()
	r.mine(10 + xmrMineMargin)
	r.mustPoll()
	if s := r.settlement(id); s.State != "confirmed" {
		r.t.Fatalf("settlement %d after 10 blocks: %s", id, s.State)
	}
}

// TestXMREscrowRegtestRestartMidSend: the relay's answer is lost after the wallet relayed it, the daemon's pool is
// flushed, and the App and the escrow wallet-rpc both restart: the next pass relays the same stored transaction (same
// hash) and the proxy saw transfer once.
func TestXMREscrowRegtestRestartMidSend(t *testing.T) {
	r, p := newXMRSettleRegtest(t)
	order, _ := r.completedReal()
	id := r.releaseReal(order)
	p.dropNext("relay_tx")
	if err := r.poll(); err == nil {
		t.Fatal("a lost relay answer passed")
	}
	stored := r.settlement(id)
	if stored.State != "built" || stored.TxHash == "" {
		t.Fatalf("after the lost answer: %+v", stored)
	}
	var flushed map[string]any
	r.call(r.daemon, "flush_txpool", map[string]any{"txids": []string{stored.TxHash}}, &flushed)
	if c := os.Getenv("ESCROW_XMR_REGTEST_WALLET_CONTAINER"); c != "" {
		if out, err := exec.Command("docker", "restart", c).CombinedOutput(); err != nil {
			t.Fatalf("docker restart: %v %s", err, out)
		}
		time.Sleep(5 * time.Second) // the wallet-rpc's RPC server starts
		t.Logf("escrow wallet-rpc container restarted mid-send")
	} else {
		t.Log("ESCROW_XMR_REGTEST_WALLET_CONTAINER unset: the wallet-rpc was not restarted")
	}
	r.restart()
	r.mustPoll()
	s := r.settlement(id)
	if s.State != "relayed" || s.TxHash != stored.TxHash || p.count("transfer") != 1 || p.count("relay_tx") != 2 {
		t.Fatalf("after the restarts: %+v, transfer %d, relay_tx %d", s, p.count("transfer"), p.count("relay_tx"))
	}
	r.xmrShape(s)
	r.confirmReal(id)
	t.Logf("order %s: transfer called once; transaction %s relayed again after the App and the wallet-rpc restarted, then confirmed", shortID(order), s.TxHash)
}

// TestXMREscrowRegtestRelayDropped: the relay's answer is lost after the wallet relayed it; the next pass finds the
// transaction in the daemon's pool and records it relayed without relaying or building anything.
func TestXMREscrowRegtestRelayDropped(t *testing.T) {
	r, p := newXMRSettleRegtest(t)
	order, _ := r.completedReal()
	id := r.releaseReal(order)
	p.dropNext("relay_tx")
	r.poll()
	r.mustPoll()
	s := r.settlement(id)
	if s.State != "relayed" || p.count("transfer") != 1 || p.count("relay_tx") != 1 {
		t.Fatalf("after the lost answer: %+v, transfer %d, relay_tx %d", s, p.count("transfer"), p.count("relay_tx"))
	}
	r.xmrShape(s)
	r.confirmReal(id)
	t.Logf("order %s: transaction %s found in the pool after its relay answer was lost; recorded relayed, not rebuilt", shortID(order), s.TxHash)
}

// TestXMREscrowRegtestTopUp: a top-up still confirming when the release is built is not spent by it; once confirmed it
// is left out and returned by a stray refund to the buyer's refund address.
func TestXMREscrowRegtestTopUp(t *testing.T) {
	r, _ := newXMRSettleRegtest(t)
	order, addr := r.checkoutReal()
	r.pay(map[string]int64{addr: xmrListingPrice})
	r.mine(6 + xmrMineMargin)
	topUp := r.pay(map[string]int64{addr: 20000000000})
	r.mine(4) // the price has at least 10 confirmations, the top-up at most 4
	r.mustPoll()
	r.endWindows()
	if r.state(order) != stateCompleted {
		t.Fatalf("funded: %s (deposits %v)", r.state(order), r.depositConfs(order))
	}
	id := r.releaseReal(order)
	r.mustPoll()
	release := r.settlement(id)
	if release.State != "relayed" || release.InputTotal != xmrListingPrice {
		t.Fatalf("release: %+v", release)
	}
	r.xmrShape(release)
	r.mine(6 + xmrMineMargin)
	r.mustPoll()
	if agInt(r.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND txid=$2 AND escrow_left_out", order, topUp) != 1 {
		t.Fatal("the top-up was not left out")
	}
	r.mustPoll()
	stray := r.settlementOf(order, "stray_refund")
	if stray.State != "relayed" || stray.InputTotal != 20000000000 {
		t.Logf("deposits %v", r.depositConfs(order))
		t.Fatalf("stray refund: %+v", stray)
	}
	r.xmrShape(stray)
	r.confirmReal(stray.ID)
	t.Logf("order %s: release %s spent the price only; top-up %s left out and returned by stray refund %s", shortID(order), release.TxHash, topUp, stray.TxHash)
}

// TestXMREscrowRegtestReorg: a reorganisation takes a confirmed release out of the chain: every block mined since it
// was relayed is popped (pop_blocks), the daemon's pool flushed and new blocks without it mined (so the wallet-rpc stays
// within 10 blocks of the daemon, P3-R2). It goes back to relayed, the same stored transaction is relayed again and
// confirms again.
func TestXMREscrowRegtestReorg(t *testing.T) {
	r, p := newXMRSettleRegtest(t)
	order, _ := r.completedReal()
	id := r.releaseReal(order)
	r.mustPoll()
	r.confirmReal(id)
	s := r.settlement(id)
	r.xmrShape(s)
	var popped map[string]any
	r.daemonPost("/pop_blocks", map[string]any{"nblocks": 10 + xmrMineMargin}, &popped) // every block confirmReal mined
	var flushed map[string]any
	r.call(r.daemon, "flush_txpool", nil, &flushed)
	r.mine(xmrMineMargin)
	r.mustPoll()
	if got := r.settlement(id).State; got != "relayed" {
		t.Fatalf("after the reorganisation: %s", got)
	}
	relays := p.count("relay_tx")
	r.mustPoll()
	if p.count("relay_tx") <= relays || p.count("transfer") != 1 {
		t.Fatalf("not relayed again: relay_tx %d (was %d), transfer %d", p.count("relay_tx"), relays, p.count("transfer"))
	}
	r.confirmReal(id)
	if got := r.settlement(id); got.TxHash != s.TxHash {
		t.Fatalf("another transaction: %+v", got)
	}
	t.Logf("order %s: transaction %s confirmed, taken out by pop_blocks (every block mined since it was relayed) and a flushed pool, relayed again and confirmed again", shortID(order), s.TxHash)
}

// fundedByTwoReal is a digital order funded with its price in two transactions mined in separate blocks, the first
// thr+6 confirmations deep when the second is paid and the second mined to exactly thr+xmrMineMargin confirmations: both
// credited, the order completed when its dispute window ends (endWindows). It returns the order and the two transactions.
func (r *xmrRegtest) fundedByTwoReal() (order, deep, shallow string) {
	r.t.Helper()
	thr := r.A.escrowXMR.confirmations
	order, addr := r.checkoutReal()
	deep = r.pay(map[string]int64{addr: xmrListingPrice / 2})
	r.mineTo(deep, thr+6)
	shallow = r.pay(map[string]int64{addr: xmrListingPrice - xmrListingPrice/2})
	r.mineTo(shallow, thr+xmrMineMargin)
	r.mustPoll()
	r.endWindows()
	if r.state(order) != stateCompleted || agInt(r.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND credited", order) != 2 {
		r.t.Fatalf("funded by two transactions: %s (deposits %v)", r.state(order), r.depositConfs(order))
	}
	return order, deep, shallow
}

// popPast pops every block from the one that mined tx (pop_blocks: its transactions go back to the daemon's pool).
func (r *xmrRegtest) popPast(tx string) {
	r.t.Helper()
	txs, _, err := r.A.escrowXMR.rpc.transactions(context.Background(), []string{tx})
	if err != nil || len(txs) != 1 || txs[0].InPool || txs[0].Confirmations <= 0 {
		r.t.Fatalf("transaction %s is not mined: %+v %v", tx, txs, err)
	}
	var popped map[string]any
	r.daemonPost("/pop_blocks", map[string]any{"nblocks": txs[0].Confirmations}, &popped)
}

// TestXMREscrowRegtestFundingReorg (A-554): a reorganisation takes a credited funding deposit back below the threshold
// after the vendor's release was requested. Kept in the pool, the deposit is mined again: the release is not built
// while it confirms (no transfer call, no stray refund), then spends both funding deposits, the price. Flushed from the
// pool (a deposit that never returns), the release waits 2 × threshold blocks, then is built from the other deposit
// alone with an order note saying so; the missing deposit is never left out for a stray refund.
func TestXMREscrowRegtestFundingReorg(t *testing.T) {
	r, p := newXMRSettleRegtest(t)
	thr := r.A.escrowXMR.confirmations
	noStray := func(order string) {
		t.Helper()
		if n := agInt(r.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND escrow_left_out", order); n != 0 || len(r.xmrSettlements(order)) != 1 {
			t.Fatalf("order %s: %d deposits left out, settlements %v (deposits %v)", shortID(order), n, r.xmrSettlements(order), r.depositConfs(order))
		}
	}

	// Kept in the pool.
	order, _, shallow := r.fundedByTwoReal()
	id := r.releaseReal(order) // requested while the watcher is held: no pass runs before the reorganisation
	r.popPast(shallow)
	r.mine(xmrMineMargin) // the wallet-rpc stays within 10 blocks of the daemon (P3-R2)
	r.mustPoll()
	r.mustPoll()
	if s := r.settlement(id); s.State != "requested" || r.waitClass(id) != xmrWaitConfirm || p.count("transfer") != 0 {
		t.Fatalf("the shallower funding deposit below the threshold again: %+v %q, transfer %d (deposits %v)", s, r.waitClass(id), p.count("transfer"), r.depositConfs(order))
	}
	if agInt(r.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND txid=$2 AND credited AND regress_notice AND confirmations BETWEEN 0 AND $3", order, shallow, thr-1) != 1 {
		t.Fatalf("the regressed deposit is not flagged: %v", r.depositConfs(order))
	}
	noStray(order)
	r.mineTo(shallow, thr+xmrMineMargin)
	r.mustPoll()
	s := r.settlement(id)
	if s.State != "relayed" || s.InputTotal != xmrListingPrice || len(s.KeyImages) != 2 || p.count("transfer") != 1 {
		t.Fatalf("confirmed again: %+v, transfer %d (deposits %v)", s, p.count("transfer"), r.depositConfs(order))
	}
	r.xmrShape(s) // two inputs of ring 16
	r.confirmReal(id)
	noStray(order)
	t.Logf("order %s: release %s held while funding deposit %s was mined again after pop_blocks, then spent both deposits", shortID(order), s.TxHash, shallow)

	// Flushed from the pool: the deposit is missing for good. Last in the leg: the funder's wallet still counts it.
	order2, deep2, shallow2 := r.fundedByTwoReal()
	id2 := r.releaseReal(order2)
	r.popPast(shallow2)
	var flushed map[string]any
	r.call(r.daemon, "flush_txpool", map[string]any{"txids": []string{shallow2}}, &flushed)
	r.mine(xmrMineMargin)
	r.mustPoll()
	transfers := p.count("transfer")
	r.mine(int(2*thr) - xmrMineMargin)
	r.mustPoll()
	if s := r.settlement(id2); s.State != "requested" || r.waitClass(id2) != xmrWaitMissing || p.count("transfer") != transfers {
		t.Fatalf("a funding deposit missing, before the bounded wait ends: %+v %q (deposits %v)", s, r.waitClass(id2), r.depositConfs(order2))
	}
	noStray(order2)
	r.mine(xmrMineMargin + 1)
	r.mustPoll()
	s2 := r.settlement(id2)
	if s2.State != "relayed" || s2.InputTotal != xmrListingPrice/2 || len(s2.KeyImages) != 1 {
		t.Fatalf("after the bounded wait: %+v (deposits %v)", s2, r.depositConfs(order2))
	}
	r.xmrShape(s2)
	if agInt(r.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND strpos(note,$2)>0", order2, "This release was built without deposit "+truncate(shallow2, 20)) != 1 {
		t.Fatalf("no note for the deposit the release was built without: %q", r.events(order2))
	}
	r.confirmReal(id2)
	noStray(order2)
	if agInt(r.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND txid=$2 AND credited AND confirmations<0 AND regress_notice AND xmr_settlement_id IS NULL", order2, shallow2) != 1 {
		t.Fatalf("the missing deposit: %v", r.depositConfs(order2))
	}
	t.Logf("order %s: funding deposit %s popped and flushed; release %s built from deposit %s alone after %d blocks", shortID(order2), shallow2, s2.TxHash, deep2, 2*thr)
}

// TestXMREscrowRegtestExternal: a wallet restored from the sealed seed spends the escrow's output outside the market
// while the market's release is built but not relayed; the daemon refuses the market's transaction and the settlement
// is recorded external and flagged for staff.
func TestXMREscrowRegtestExternal(t *testing.T) {
	r, p := newXMRSettleRegtest(t)
	order, addr := r.completedReal()
	id := r.releaseReal(order)
	p.dropNext("relay_tx")
	var flushed map[string]any
	r.poll()
	built := r.settlement(id)
	r.call(r.daemon, "flush_txpool", map[string]any{"txids": []string{built.TxHash}}, &flushed)
	name := r.walletName(false)
	var height int64
	var account int64
	if err := r.DB.QueryRow("SELECT w.restore_height,s.account_index FROM xmr_escrow_wallets w JOIN xmr_escrow_slots s ON s.wallet_id=w.id WHERE w.name=$1 AND s.address=$2", name, addr).Scan(&height, &account); err != nil {
		t.Fatal(err)
	}
	restored := "outside-" + randomToken()[:10]
	r.call(r.restore, "restore_deterministic_wallet", map[string]any{"filename": restored, "password": "", "seed": r.unseal(name), "restore_height": height,
		"language": "English", "autosave_current": false}, nil)
	for range account {
		r.call(r.restore, "create_account", nil, nil)
	}
	r.call(r.restore, "rescan_blockchain", nil, nil)
	r.call(r.restore, "refresh", nil, nil)
	var sweep struct {
		TxHashes []string `json:"tx_hash_list"`
	}
	r.call(r.restore, "sweep_all", map[string]any{"address": r.funderAddress(), "account_index": account, "ring_size": 16}, &sweep)
	if len(sweep.TxHashes) == 0 {
		t.Fatal("the restored wallet swept nothing")
	}
	r.mustPoll()
	if got := r.settlement(id).State; got != "external" || r.staffFlags(order) == 0 {
		t.Fatalf("after the outside spend: %s, staff flags %d", got, r.staffFlags(order))
	}
	if p.count("transfer") != 1 || p.count("relay_tx") != 2 {
		t.Fatalf("transfer %d, relay_tx %d (the stored transaction relayed again and refused)", p.count("transfer"), p.count("relay_tx"))
	}
	t.Logf("order %s: escrow output swept outside the market by a wallet restored from the sealed seed (%s); the release %s is external and flagged", shortID(order), sweep.TxHashes[0], built.TxHash)
}

// daemonStoppedAround is a proxy hook (xmrRPCProxy.wrap) that stops the monerod container name before the call is
// forwarded and starts it again once the wallet-rpc has answered, then waits up to two minutes until monerod answers at
// the height it had: it must run with --keep-fakechain, or its start deletes the fakechain. Idle connections to it are
// dropped, since it may come back on another address. It runs on the proxy's goroutine, so it reports with Errorf.
//
// A-631: the escrow wallet-rpc's own auto-refresh (every 20 s) is switched off, directly rather than through p, before
// monerod is stopped. An auto-refresh that starts in that window retries the missing daemon (pull_blocks, up to
// three retries) and the wallet-rpc answers no RPC until it gives up; on a CI runner the retries and the forwarded
// call's own attempt were 10 s apart, so the call was refused only about 40 s after the stop, past the market's
// deadline (payoutSendTimeout for relay_tx), and the leg saw "relay_tx: timeout" instead of the refusal. The forwarded
// call alone now meets the stopped daemon. The transcript gives the stop's and the forwarded call's durations.
func (r *xmrRegtest) daemonStoppedAround(p *xmrRPCProxy, name string) func(forward func()) {
	return func(forward func()) {
		ctx := context.Background()
		var before, after struct {
			Height int64 `json:"height"`
		}
		autoRefresh := func(on bool) {
			if err := p.wallet.callWithin(ctx, 2*time.Minute, "/json_rpc", "auto_refresh", map[string]any{"enable": on}, nil); err != nil {
				r.t.Errorf("escrow wallet-rpc auto_refresh %v: %v", on, err)
			}
		}
		// A-641: the market itself turns it off after every open_wallet, so it is not switched back on afterwards; this
		// call still covers a wallet-rpc restarted since its last open_wallet.
		autoRefresh(false)
		err := r.daemon.call(ctx, "/json_rpc", "get_info", nil, &before)
		began := time.Now()
		if out, stop := exec.Command("docker", "stop", "-t", "30", name).CombinedOutput(); err != nil || stop != nil {
			r.t.Errorf("stopping monerod (container %s): %v, %v %s", name, err, stop, out)
		}
		sent := time.Now()
		forward()
		r.t.Logf("monerod (container %s) stopped in %s; the escrow wallet-rpc answered the forwarded call in %s", name,
			sent.Sub(began).Round(time.Millisecond), time.Since(sent).Round(time.Millisecond))
		if out, err := exec.Command("docker", "start", name).CombinedOutput(); err != nil {
			r.t.Errorf("docker start %s: %v %s", name, err, out)
			return
		}
		r.daemon.http.CloseIdleConnections()
		r.A.escrowXMR.rpc.daemon.http.CloseIdleConnections()
		for range 120 {
			if err = r.daemon.call(ctx, "/json_rpc", "get_info", nil, &after); err == nil {
				break
			}
			time.Sleep(time.Second)
		}
		if err != nil || after.Height != before.Height {
			r.t.Errorf("monerod (container %s) came back at height %d, it had %d (%v): run it with --keep-fakechain", name, after.Height, before.Height, err)
		}
	}
}

// TestXMREscrowRegtestReRequest (A-426, A-401): the monerod container (run with --keep-fakechain) is stopped right before
// each relay of a release's stored transaction and started again once the escrow wallet-rpc has answered, so the
// wallet-rpc cannot commit it (-4) while the transaction is stored, the daemon never saw it and its key images are
// unspent. The watcher relays the same stored metadata again on the next passes, never calls transfer, and records the
// release failed on the third refusal in a row. incoming_transfers available still lists the stored key images. An
// administrator re-requests it with the password step-up after a reorganisation took the funding deposit out of the
// chain (A-614): the new settlement waits (deposit-missing), past A-554's bounded wait too, with no transfer. Once the
// same deposit transaction is mined again, the new build spends exactly the stored key images (the stored row's, and
// the inputs the daemon decodes) to the stored plan, is relayed once and confirms.
//
// A-639: the deposit is mined again at another global output index, and the refused build stored a ring for its key
// image that lacks it, so the wallet refuses the build (ring-stale, A-618) unless its decoy picker happens to draw the
// new index on that pass (CI run 39610521956 was refused on 9 passes in a row; its rerun built on the second). Both
// outcomes are asserted, so the leg no longer depends on that draw: on each of the first two passes it is built and
// relayed once, or refused with nothing built or relayed (wallet-refusing ring-stale, then wallet-refused ring-stale,
// flagged to staff). Still refused, the operator guide's stale-ring break-glass is followed: its step-1 query gives the
// stored key images, all unspent, remedy A's precondition query answers 0, remedy A is applied
// (ESCROW_XMR_REGTEST_WALLET_CONTAINER), and the next pass builds and relays it once.
func TestXMREscrowRegtestReRequest(t *testing.T) {
	container, walletC := os.Getenv("ESCROW_XMR_REGTEST_DAEMON_CONTAINER"), os.Getenv("ESCROW_XMR_REGTEST_WALLET_CONTAINER")
	if container == "" || walletC == "" {
		t.Skip("set ESCROW_XMR_REGTEST_DAEMON_CONTAINER and ESCROW_XMR_REGTEST_WALLET_CONTAINER (scripts/test-escrow-xmr-regtest.sh): this leg stops and starts monerod, and applies the stale-ring remedy A to the escrow wallet-rpc when its build is refused")
	}
	// The guide's queries: the stuck key images, remedy A's precondition.
	keyImagesSQL := breakGlassSQL(t, "array_to_string(o.key_images")
	inFlightSQL := breakGlassSQL(t, "SELECT count(*) FROM xmr_settlements")
	r, p := newXMRSettleRegtest(t)
	order, addr := r.completedReal()
	id, failed := r.relayFailedReal(p, container, order)

	// The escrow wallet still lists the refused transaction's outputs as available.
	ctx := context.Background()
	x := r.A.escrowXMR
	name := r.walletName(false)
	if err := x.rpc.openWallet(ctx, name, xmrWalletPassword(x.secret, name)); err != nil {
		t.Fatalf("open_wallet: %s", xmrErrorClass(err))
	}
	var account int64
	if err := r.DB.QueryRow("SELECT account_index FROM xmr_escrow_slots WHERE address=$1", addr).Scan(&account); err != nil {
		t.Fatal(err)
	}
	outs, err := x.rpc.incomingTransfers(ctx, account)
	if err != nil {
		t.Fatalf("incoming_transfers: %s", xmrErrorClass(err))
	}
	var available []string
	for _, o := range outs {
		if !o.Spent {
			available = append(available, o.KeyImage)
		}
	}
	for _, ki := range failed.KeyImages {
		if !slices.Contains(available, ki) {
			t.Fatalf("incoming_transfers available does not list the refused transaction's key image %s (it lists %v): a re-request could not spend it", ki, available)
		}
	}
	t.Logf("incoming_transfers available lists the refused transaction's %d output(s) unspent: %v", len(failed.KeyImages), failed.KeyImages)

	// A-614: a reorganisation takes the funding deposit out of the chain before the re-request is carried out (see
	// reRequestMissingReal); once the same deposit transaction is mined again it is built from the stored key images.
	r.reRequestMissingReal(p, order, id)
	// The deposit is mined again at another global output index, while the escrow wallet's ring database still holds
	// the ring the refused build used for its key image (wallet2::get_outs stores every build's rings and reuses a known
	// one). That ring no longer contains the real output: monero-wallet-rpc 0.18.5.1 logs "Known ring does not include
	// the spent output" and asks the daemon for the new index only when one of the decoys it draws beside the stored
	// ring is that index, so on each pass transfer is either refused (-4, "Daemon response did not include the requested
	// real output", recorded ring-stale, A-618) or built, by chance. TestXMREscrowRegtestStaleRing makes the refusal
	// certain.
	relays := p.count("relay_tx")
	var next xmrSettlement
	refused := 0
	for pass := 1; pass <= 2; pass++ {
		flags := r.staffFlags(order)
		r.synced()
		r.mustPoll()
		if next = r.settlementOf(order, "release"); next.State != "requested" {
			break
		}
		want := xmrWalletRefusing + " " + xmrRingStale
		if pass == 2 {
			want = xmrWalletRefused + " " + xmrRingStale // flagged to staff
		}
		werr := p.transferFailure()
		if r.waitClass(next.ID) != want || next.TxHash != "" || len(next.KeyImages) != 0 || p.count("transfer") != 1+pass || p.count("relay_tx") != relays ||
			werr == nil || werr.Code != -4 || werr.Message != xmrRingStaleText || (r.staffFlags(order) > flags) != (pass == 2) {
			t.Fatalf("re-requested, pass %d: new %+v %q (want %q), transfer %d, relay_tx %d (was %d), staff notices %d (were %d), wallet transfer error %+v",
				pass, next, r.waitClass(next.ID), want, p.count("transfer"), p.count("relay_tx"), relays, r.staffFlags(order), flags, werr)
		}
		refused++
		t.Logf("re-requested, pass %d: transfer refused (%s; wallet transfer error %d %q), nothing built or relayed", pass, want, werr.Code, werr.Message)
	}
	if next.State == "requested" {
		// The operator guide's stale-ring break-glass: step 1 (the superseded settlement's key images, each unspent),
		// remedy A's precondition (no other settlement may be built again) and remedy A.
		var stuck string
		if err := r.DB.QueryRow(keyImagesSQL, next.ID).Scan(&stuck); err != nil ||
			!slices.Equal(slices.Sorted(slices.Values(strings.Fields(stuck))), slices.Sorted(slices.Values(failed.KeyImages))) {
			t.Fatalf("the guide's key image query: %q %v, want the failed release's %v", stuck, err, failed.KeyImages)
		}
		spent, err := x.rpc.keyImagesSpent(ctx, strings.Fields(stuck))
		if err != nil || len(spent) != len(failed.KeyImages) || slices.ContainsFunc(spent, func(s int) bool { return s != 0 }) {
			t.Fatalf("is_key_image_spent of the stuck key images %v: %v %v, want all 0", failed.KeyImages, spent, err)
		}
		var inFlight int
		if err := r.DB.QueryRow(inFlightSQL, next.ID).Scan(&inFlight); err != nil || inFlight != 0 {
			t.Fatalf("remedy A's precondition query: %d %v, want 0", inFlight, err)
		}
		r.moveRingDBAside(walletC)
		r.mustPoll()
		next = r.settlementOf(order, "release")
	}
	if next.Supersedes != id || next.State != "relayed" || r.settlement(id).State != "superseded" || p.count("transfer") != 2+refused || p.count("relay_tx") != relays+1 {
		t.Fatalf("re-requested after %d refused pass(es): old %+v, new %+v %q, transfer %d, relay_tx %d (was %d), wallet transfer error %+v",
			refused, r.settlement(id), next, r.waitClass(next.ID), p.count("transfer"), p.count("relay_tx"), relays, p.transferFailure())
	}
	if refused == 2 {
		t.Logf("re-request %d: refused twice (ring-stale), built and relayed once after the stale-ring remedy A", next.ID)
	} else {
		t.Logf("re-request %d: built and relayed once after %d ring-stale refusal(s): the wallet drew the deposit's new output index", next.ID, refused)
	}
	inputs := r.sameSpend(id, failed, next)
	r.confirmReal(next.ID)
	t.Logf("order %s: release %s refused %d times while monerod was stopped for its relay, failed; re-requested by an administrator, release %s spent the same key images %v to the same plan (fee %d, was %d) and confirmed",
		shortID(order), failed.TxHash, xmrRelayAttempts, next.TxHash, inputs, next.Fee, failed.Fee)
}

// TestXMREscrowRegtestSupersededMined (A-566, PAY66-3): a release's relay is refused until it fails, as in
// TestXMREscrowRegtestReRequest, and an administrator re-requests it. While the watcher builds the re-request (around
// its incoming_transfers call), the refused transaction's stored blob reaches the daemon after all
// (send_raw_transaction, as a peer that kept it would send it back) and is mined. The re-request is never built
// (no second transfer) or relayed: it is closed, and the old release is recorded sent (relayed, then confirmed),
// with its deposit claim back, one order note saying so and none saying a transaction the market did not make spent
// the coins. The daemon has the old transaction, and its inputs are the stored key images.
func TestXMREscrowRegtestSupersededMined(t *testing.T) {
	container := os.Getenv("ESCROW_XMR_REGTEST_DAEMON_CONTAINER")
	if container == "" {
		t.Skip("set ESCROW_XMR_REGTEST_DAEMON_CONTAINER to the monerod container's name (scripts/test-escrow-xmr-regtest.sh): this leg stops and starts it")
	}
	r, p := newXMRSettleRegtest(t)
	order, _ := r.completedReal()
	id, failed := r.relayFailedReal(p, container, order)
	blob := agStr(r.testEnv, "SELECT tx_blob FROM xmr_settlements WHERE id=$1", id)
	ctx := context.Background()
	sent := false
	p.wrap("incoming_transfers", func(forward func()) {
		if !sent {
			sent = true
			var res struct {
				Status string `json:"status"`
				Reason string `json:"reason"`
			}
			// The proxy's goroutine: report with Errorf (daemonPost fails the leg with Fatalf).
			b, _ := json.Marshal(map[string]any{"tx_as_hex": blob, "do_not_relay": false})
			resp, err := http.Post(strings.TrimRight(os.Getenv("ESCROW_XMR_REGTEST_DAEMON_URL"), "/")+"/send_raw_transaction", "application/json", bytes.NewReader(b))
			if err == nil {
				err = json.NewDecoder(resp.Body).Decode(&res)
				resp.Body.Close()
			}
			if err != nil || res.Status != "OK" {
				t.Errorf("send_raw_transaction of the refused release %s: %v %q %q", failed.TxHash, err, res.Status, res.Reason)
			}
			if err := r.daemon.callWithin(ctx, 2*time.Minute, "/json_rpc", "generateblocks", map[string]any{"amount_of_blocks": 1, "wallet_address": r.funderAddr}, nil); err != nil {
				t.Errorf("generateblocks: %v", err)
			}
		}
		forward()
	})
	_, adminSess := r.user("xr566_admin_"+randomToken()[:6], "admin")
	if code := r.reRequest(adminSess, order, id, testPassword); code != 303 {
		t.Fatalf("re-request: %d", code)
	}
	transfers, relays := p.count("transfer"), p.count("relay_tx")
	r.synced()
	r.mustPoll()
	p.wrap("incoming_transfers", nil)
	if !sent || t.Failed() {
		t.Fatal("the refused release was not sent back to the daemon while the re-request was built")
	}
	next := r.settlementOf(order, "release")
	old := r.settlement(id)
	if next.Supersedes != id || next.State != "superseded" || next.TxHash != "" || old.State != "relayed" || old.TxHash != failed.TxHash ||
		p.count("transfer") != transfers || p.count("relay_tx") != relays {
		t.Fatalf("after the superseded release was mined: old %+v, new %+v, transfer %d, relay_tx %d", old, next, p.count("transfer")-transfers, p.count("relay_tx")-relays)
	}
	if agInt(r.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND xmr_settlement_id=$2", order, id) != 1 ||
		agInt(r.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND strpos(note,$2)>0", order, supersededSentPhrase) != 1 ||
		agInt(r.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND strpos(note,'did not make')>0", order) != 0 {
		t.Fatalf("claims or notes wrong: claims on old %d", agInt(r.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND xmr_settlement_id=$2", order, id))
	}
	r.xmrShape(old)
	r.confirmReal(id)
	t.Logf("order %s: release %s refused %d times and failed, re-requested as settlement %d; the refused transaction was mined after all: the re-request was closed unbuilt and the release confirmed",
		shortID(order), failed.TxHash, xmrRelayAttempts, next.ID)
}

// relayFailedReal requests order's release and stops the monerod container (run with --keep-fakechain) right before
// each relay of its stored transaction and starts it again once the escrow wallet-rpc has answered, so the wallet-rpc
// cannot commit it (-4) while the transaction is stored, the daemon never saw it and its key images are unspent. The
// watcher relays the same stored metadata again on the next passes, never calls transfer again, and records the release
// failed on the third refusal in a row; staff are told. It returns the release and its failed row.
func (r *xmrRegtest) relayFailedReal(p *xmrRPCProxy, container, order string) (int64, xmrSettlement) {
	t := r.t
	t.Helper()
	id := r.releaseReal(order)
	// The relay call waits for the daemon's stop and start through the proxy.
	p.wrap("relay_tx", r.daemonStoppedAround(p, container))
	for n := 1; n <= xmrRelayAttempts; n++ {
		heights := r.synced()
		if err := r.poll(); err != nil || t.Failed() {
			t.Fatalf("pass %d (%s): %v", n, heights, err)
		}
		want, class := "built", xmrRelayRefusing+" "+strconv.Itoa(n)
		if n == xmrRelayAttempts {
			want, class = "failed", xmrRelayRefused
		}
		if s := r.settlement(id); s.State != want || r.waitClass(id) != class || p.count("relay_tx") != n || p.count("transfer") != 1 {
			t.Fatalf("relay refused %d time(s): %+v %q, relay_tx %d, transfer %d", n, s, r.waitClass(id), p.count("relay_tx"), p.count("transfer"))
		}
		t.Logf("pass %d: monerod stopped for the relay; relay_tx refused, the settlement is %s (%s)", n, want, class)
	}
	p.wrap("relay_tx", nil)
	failed := r.settlement(id)
	if r.staffFlags(order) == 0 {
		t.Fatal("staff were not told of the failed release")
	}
	if found, _, err := r.A.xmrOnNetwork(context.Background(), &xmrSettleRow{TxHash: failed.TxHash}); err != nil || found {
		t.Fatalf("the daemon has the refused transaction %s (%v)", failed.TxHash, err)
	}
	return id, failed
}

// reRequestMissingReal (A-614): a reorganisation takes every funding deposit of order out of the chain (every block
// since the earliest was mined popped, the pool flushed) before an administrator re-requests failed settlement id (with
// the password step-up). The new settlement takes over the deposits' claims, builds nothing and waits (deposit-missing)
// rather than failing nothing-to-settle, past twice the threshold in blocks too (A-554's bounded wait, which a re-request
// does not have), with no transfer. The same deposit transactions are then sent to the daemon again
// (send_raw_transaction) and mined to the threshold and xmrMineMargin, each at another global output index. It returns
// the deposits.
func (r *xmrRegtest) reRequestMissingReal(p *xmrRPCProxy, order string, id int64) []string {
	t := r.t
	t.Helper()
	deposits := agStrings(t, r.testEnv, "SELECT txid FROM payments WHERE order_id=$1 ORDER BY created,id", order)
	before := r.outputIndices(deposits)
	var raw struct {
		Txs []struct {
			TxHash string `json:"tx_hash"`
			AsHex  string `json:"as_hex"`
		} `json:"txs"`
	}
	r.daemonPost("/get_transactions", map[string]any{"txs_hashes": deposits}, &raw)
	if len(raw.Txs) != len(deposits) {
		t.Fatalf("the daemon gives %d of funding deposits %v", len(raw.Txs), deposits)
	}
	txs, _, err := r.A.escrowXMR.rpc.transactions(context.Background(), deposits)
	if err != nil || len(txs) != len(deposits) {
		t.Fatalf("funding deposits %v: %+v %v", deposits, txs, err)
	}
	deepest := txs[0]
	for _, tx := range txs {
		if tx.InPool || tx.Confirmations <= 0 {
			t.Fatalf("funding deposit %s is not mined: %+v", tx.Hash, tx)
		}
		if tx.Confirmations > deepest.Confirmations {
			deepest = tx
		}
	}
	r.popPast(deepest.Hash) // every later block too, so every deposit
	var flushed map[string]any
	r.call(r.daemon, "flush_txpool", map[string]any{"txids": deposits}, &flushed)
	r.mine(xmrMineMargin)
	r.synced()
	r.mustPoll()
	_, adminSess := r.user("xr_admin_"+randomToken()[:6], "admin")
	if code := r.reRequest(adminSess, order, id, testPassword); code != 303 {
		t.Fatalf("re-request: %d", code)
	}
	r.synced()
	r.mustPoll()
	r.mine(int(3 * r.A.escrowXMR.confirmations)) // past A-554's bounded wait, which a re-request does not have
	r.synced()
	r.mustPoll()
	waiting := r.settlementOf(order, "release")
	if waiting.Supersedes != id || waiting.State != "requested" || r.waitClass(waiting.ID) != xmrWaitMissing || r.settlement(id).State != "superseded" || p.count("transfer") != 1 ||
		agInt(r.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND xmr_settlement_id=$2 AND confirmations<0", order, waiting.ID) != len(deposits) {
		t.Fatalf("re-requested while its deposits are missing: old %+v, new %+v %q, transfer %d (deposits %v)", r.settlement(id), waiting, r.waitClass(waiting.ID), p.count("transfer"), r.depositConfs(order))
	}
	t.Logf("funding deposit(s) %v popped and flushed: re-request %d waits (%s) past %d blocks, no transfer", deposits, waiting.ID, xmrWaitMissing, 3*r.A.escrowXMR.confirmations)
	for _, tx := range raw.Txs {
		var sent struct {
			Status string `json:"status"`
		}
		r.daemonPost("/send_raw_transaction", map[string]any{"tx_as_hex": tx.AsHex, "do_not_relay": false}, &sent)
		if sent.Status != "OK" {
			t.Fatalf("send_raw_transaction of funding deposit %s: %q", tx.TxHash, sent.Status)
		}
	}
	r.mineAllTo(deposits, 10+xmrMineMargin)
	after := r.outputIndices(deposits)
	for _, d := range deposits {
		t.Logf("funding deposit %s output indices before reorg %v, restored %v", d, before[d], after[d])
	}
	return deposits
}

// outputIndices gives each of txs's global output indices, as the daemon has them now.
func (r *xmrRegtest) outputIndices(txs []string) map[string][]uint64 {
	r.t.Helper()
	var res struct {
		Txs []struct {
			TxHash        string   `json:"tx_hash"`
			OutputIndices []uint64 `json:"output_indices"`
		} `json:"txs"`
	}
	r.daemonPost("/get_transactions", map[string]any{"txs_hashes": txs}, &res)
	out := map[string][]uint64{}
	for _, tx := range res.Txs {
		out[tx.TxHash] = tx.OutputIndices
	}
	if len(out) != len(txs) {
		r.t.Fatalf("the daemon gives %d of transactions %v", len(out), txs)
	}
	return out
}

// mineAllTo mines until the newest of txs, each mined in a block, has exactly confs confirmations (as mineTo does
// for one; the others have at least as many).
func (r *xmrRegtest) mineAllTo(txs []string, confs int64) {
	r.t.Helper()
	for range 20 {
		got, _, err := r.A.escrowXMR.rpc.transactions(context.Background(), txs)
		if err != nil {
			r.t.Fatalf("/get_transactions: %v", err)
		}
		least := confs
		for _, tx := range got {
			if tx.InPool {
				least = min(least, 0)
			} else {
				least = min(least, tx.Confirmations)
			}
		}
		if len(got) != len(txs) {
			least = 0
		}
		if least == confs {
			return
		}
		r.mine(max(int(confs-least), 1))
	}
	r.t.Fatalf("transactions %v did not reach %d confirmations", txs, confs)
}

// sameSpend checks that settlement next, which re-requested failed settlement id, spends exactly failed's stored key
// images (the stored rows and the inputs the daemon decodes) to the stored plan (only the fee, and so the claimant's
// output, may differ), with xmrShape's shape. It returns the decoded inputs.
func (r *xmrRegtest) sameSpend(id int64, failed, next xmrSettlement) []string {
	t := r.t
	t.Helper()
	if !slices.Equal(slices.Sorted(slices.Values(next.KeyImages)), slices.Sorted(slices.Values(failed.KeyImages))) || next.InputTotal != failed.InputTotal {
		t.Fatalf("other coins: old %+v, new %+v", failed, next)
	}
	oldOuts, newOuts := r.outputsOf(id), r.outputsOf(next.ID)
	claimant := func(s string, fee int64) (string, int64) {
		f := strings.Split(s, ":")
		n, _ := strconv.ParseInt(f[2], 10, 64)
		return f[0] + ":" + f[1], n + fee
	}
	oldTo, oldGross := claimant(oldOuts[0], failed.Fee)
	newTo, newGross := claimant(newOuts[0], next.Fee)
	if len(newOuts) != len(oldOuts) || oldTo != newTo || oldGross != newGross || !slices.Equal(oldOuts[1:], newOuts[1:]) {
		t.Fatalf("the plan changed:\n old %v (fee %d)\n new %v (fee %d)", oldOuts, failed.Fee, newOuts, next.Fee)
	}
	// The daemon's decoding of the new transaction: its inputs are exactly the stored key images.
	var res struct {
		Txs []struct {
			AsJSON string `json:"as_json"`
		} `json:"txs"`
	}
	r.daemonPost("/get_transactions", map[string]any{"txs_hashes": []string{next.TxHash}, "decode_as_json": true}, &res)
	var body struct {
		Vin []struct {
			Key struct {
				KeyImage string `json:"k_image"`
			} `json:"key"`
		} `json:"vin"`
	}
	if len(res.Txs) != 1 || json.Unmarshal([]byte(res.Txs[0].AsJSON), &body) != nil {
		t.Fatalf("the daemon does not decode %s", next.TxHash)
	}
	var inputs []string
	for _, in := range body.Vin {
		inputs = append(inputs, in.Key.KeyImage)
	}
	if !slices.Equal(slices.Sorted(slices.Values(inputs)), slices.Sorted(slices.Values(failed.KeyImages))) {
		t.Fatalf("the new transaction's inputs %v are not the stored key images %v", inputs, failed.KeyImages)
	}
	r.xmrShape(next)
	return inputs
}

// The stale-ring rehearsal (A-618): xmrStaleRingDeposits deposits fund the order, so the release spends that many key
// images, and the re-request's first build comes xmrStaleRingAge blocks after the deposits were mined again.
// monero-wallet-rpc 0.18.5.1 can rescue a stale ring only when its decoy picker happens to draw the real output's new
// index (59 draws an input on ring 16); the build needs that for every input, and the draws favour recent outputs, so
// several deposits aged past the newest blocks make the refusal certain in practice (a simulation of wallet2's gamma
// picker on a 2,000-block fakechain gives about 0.14 per input at this age: about 1 in 130,000 for all six).
const (
	xmrStaleRingDeposits = 6
	xmrStaleRingAge      = 50
)

// xmrRingDBDir is the escrow wallet-rpc's ring database: --shared-ringdb-dir inside its wallet directory, as
// compose.nodes.yaml, the native installer (internal/operator/render.go) and CI run it.
const xmrRingDBDir = "/home/monero/wallet/.shared-ringdb"

// regtestDocker runs the docker command with args and returns its trimmed output, failing the leg if it fails.
func regtestDocker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v %s", args[0], err, out)
	}
	return strings.TrimSpace(string(out))
}

// walletStopped is the stale-ring break-glass's frame around either remedy (A-618, A-625): it stops the escrow
// wallet-rpc container, runs offline with the container's image and the volume it mounts at /home/monero/wallet, starts
// it again and waits until it answers. The container must mount its wallet directory from a volume and run with
// --shared-ringdb-dir=/home/monero/wallet/.shared-ringdb.
func (r *xmrRegtest) walletStopped(container string, offline func(image, volume string)) {
	t := r.t
	t.Helper()
	image := regtestDocker(t, "inspect", "-f", "{{.Config.Image}}", container)
	volume := regtestDocker(t, "inspect", "-f", `{{range .Mounts}}{{if eq .Destination "/home/monero/wallet"}}{{.Name}}{{end}}{{end}}`, container)
	if volume == "" {
		t.Fatalf("container %s mounts no volume at /home/monero/wallet: run the escrow wallet-rpc as scripts/test-escrow-xmr-regtest.sh says", container)
	}
	regtestDocker(t, "stop", container)
	started := false
	defer func() { // a failure in offline must not leave the wallet-rpc stopped for the next leg
		if !started {
			exec.Command("docker", "start", container).Run()
		}
	}()
	offline(image, volume)
	started = true
	regtestDocker(t, "start", container)
	http.DefaultTransport.(*http.Transport).CloseIdleConnections() // the proxy's connections to the stopped wallet-rpc
	ctx := context.Background()
	for range 120 {
		if r.A.escrowXMR.rpc.wallet.call(ctx, "/json_rpc", "get_version", nil, nil) == nil {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("the escrow wallet-rpc container %s did not answer within 120 seconds of its start", container)
}

// moveRingDBAside applies the operator guide's stale-ring remedy A to the escrow wallet-rpc container (A-618): stop
// it, move its ring database aside to a dated directory with its own image as uid 1000 and no network (the wallet
// files are not touched), and start it again.
func (r *xmrRegtest) moveRingDBAside(container string) {
	t := r.t
	t.Helper()
	aside := xmrRingDBDir + ".stale-" + time.Now().UTC().Format("20060102T150405Z")
	r.walletStopped(container, func(image, volume string) {
		regtestDocker(t, "run", "--rm", "--network", "none", "--user", "1000:1000", "-v", volume+":/home/monero/wallet", "--entrypoint", "mv", image, xmrRingDBDir, aside)
		t.Logf("escrow wallet-rpc container %s stopped, its ring database moved aside to %s (image %s as uid 1000), started again", container, aside, image)
	})
}

// staleRingReal reproduces the stale ring of the break-glass (A-618): a release of a new order funded by
// xmrStaleRingDeposits deposits is built once (its rings stored in the escrow wallet-rpc's ring database), refused at
// relay until it fails (relayFailedReal; monerod container daemonC is stopped for each relay), and re-requested while a
// reorganisation has taken its deposits out of the chain (reRequestMissingReal); mined again, each deposit is at another
// global output index, and xmrStaleRingAge blocks later the re-request's build is refused: transfer -4, "Daemon response
// did not include the requested real output", recorded wallet-refusing ring-stale, nothing built or relayed. It returns
// the order, the failed release and the waiting re-request.
func (r *xmrRegtest) staleRingReal(p *xmrRPCProxy, daemonC string) (order string, id int64, failed, next xmrSettlement) {
	t := r.t
	t.Helper()
	order, addr := r.checkoutReal()
	var deposits []string
	for i := range xmrStaleRingDeposits {
		share := int64(xmrListingPrice / xmrStaleRingDeposits)
		if i == xmrStaleRingDeposits-1 {
			share = xmrListingPrice - share*(xmrStaleRingDeposits-1)
		}
		deposits = append(deposits, r.pay(map[string]int64{addr: share}))
	}
	r.mineAllTo(deposits, 10+xmrMineMargin)
	r.synced()
	r.mustPoll()
	r.endWindows()
	if r.state(order) != stateCompleted || agInt(r.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND credited", order) != xmrStaleRingDeposits {
		t.Fatalf("funded by %d deposits: %s (deposits %v)", xmrStaleRingDeposits, r.state(order), r.depositConfs(order))
	}
	id, failed = r.relayFailedReal(p, daemonC, order)
	if len(failed.KeyImages) != xmrStaleRingDeposits {
		t.Fatalf("the release spends %d key images, want %d: %+v", len(failed.KeyImages), xmrStaleRingDeposits, failed)
	}
	r.reRequestMissingReal(p, order, id)
	r.mine(xmrStaleRingAge)
	relays := p.count("relay_tx")

	// The stale ring: refused, nothing built or relayed.
	r.synced()
	r.mustPoll()
	next = r.settlementOf(order, "release")
	werr := p.transferFailure()
	if next.Supersedes != id || next.State != "requested" || r.waitClass(next.ID) != xmrWalletRefusing+" "+xmrRingStale || next.TxHash != "" || len(next.KeyImages) != 0 ||
		p.count("transfer") != 2 || p.count("relay_tx") != relays ||
		werr == nil || werr.Code != -4 || werr.Message != "Daemon response did not include the requested real output" {
		t.Fatalf("re-requested with a stale ring: new %+v %q, transfer %d, relay_tx %d (was %d), wallet transfer error %+v", next, r.waitClass(next.ID), p.count("transfer"), p.count("relay_tx"), relays, werr)
	}
	t.Logf("re-request %d after the reorganisation: transfer refused (%s; wallet transfer error %d %q), nothing built or relayed", next.ID, r.waitClass(next.ID), werr.Code, werr.Message)
	return order, id, failed, next
}

// TestXMREscrowRegtestStaleRing (A-618): the operator guide's stale-ring break-glass, rehearsed with remedy A. The stale
// ring is reproduced (staleRingReal); remedy A as documented (stop the escrow wallet-rpc, move its .shared-ringdb aside
// as uid 1000, start it) and the next pass builds the stored key images to the stored plan and relays it exactly once;
// it confirms.
func TestXMREscrowRegtestStaleRing(t *testing.T) {
	daemonC, walletC := os.Getenv("ESCROW_XMR_REGTEST_DAEMON_CONTAINER"), os.Getenv("ESCROW_XMR_REGTEST_WALLET_CONTAINER")
	if daemonC == "" || walletC == "" {
		t.Skip("set ESCROW_XMR_REGTEST_DAEMON_CONTAINER and ESCROW_XMR_REGTEST_WALLET_CONTAINER (scripts/test-escrow-xmr-regtest.sh): this leg stops and starts both")
	}
	r, p := newXMRSettleRegtest(t)
	order, id, failed, _ := r.staleRingReal(p, daemonC)
	relays := p.count("relay_tx")

	// Remedy A, as docs/operator-guide.md documents it; the next pass builds and relays it once.
	r.moveRingDBAside(walletC)
	r.mustPoll()
	next := r.settlementOf(order, "release")
	if next.Supersedes != id || next.State != "relayed" || r.settlement(id).State != "superseded" || p.count("transfer") != 3 || p.count("relay_tx") != relays+1 {
		t.Fatalf("after the ring database was moved aside: old %+v, new %+v %q, transfer %d, relay_tx %d (was %d), wallet transfer error %+v",
			r.settlement(id), next, r.waitClass(next.ID), p.count("transfer"), p.count("relay_tx"), relays, p.transferFailure())
	}
	inputs := r.sameSpend(id, failed, next)
	r.confirmReal(next.ID)
	t.Logf("order %s: re-request %d refused with a stale ring for %d key images; after remedy A it spent the same key images %v to the same plan in %s, relayed once, confirmed",
		shortID(order), next.ID, len(inputs), inputs, next.TxHash)
}

// xmrWalletCLIImage runs the official monero-wallet-cli for remedy B (A-625): a glibc build, which the Alpine (musl)
// wallet-rpc image cannot run. It is the base image the Dockerfile pins (tests/test_ci_escrow_xmr_regtest.py checks).
const xmrWalletCLIImage = "debian:bookworm-slim@sha256:7c7b2c966bc9ee8cedfeef67e0e279108992c77681fa595db4a9d65c06ccc587"

// xmrRingUnset is monero-wallet-cli 0.18.5.1's print_ring answer for a key image with no stored ring.
const xmrRingUnset = "Key image either not spent, or spent with ring size 1"

// xmrPrintedRing matches a ring print_ring shows: the key image, "absolute" and the ring's global output indices.
var xmrPrintedRing = regexp.MustCompile(`([0-9a-f]{64}) absolute((?: [0-9]+)+)`)

// walletCLI runs the official monero-wallet-cli (cli, Linux x86-64) offline on escrow wallet name, as the operator guide's
// remedy B does: in xmrWalletCLIImage as uid 1000 with no network, the wallet volume at /home/monero/wallet, the same
// --shared-ringdb-dir as the wallet-rpc, and the wallet's derived password in a file mounted read-only (mode 0640 in a
// mode-0700 directory, readable through --group-add; never on a command line). stdin is its commands; it returns its
// output, which is logged only on a failure (it names the wallet's address; the password is never in it).
func (r *xmrRegtest) walletCLI(cli, volume, name, stdin string) string {
	t := r.t
	t.Helper()
	dir := t.TempDir()
	pw := dir + "/escrow-wallet-pass"
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pw, []byte(xmrWalletPassword(r.A.escrowXMR.secret, name)), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(pw, 0o640); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(pw)
	cmd := exec.Command("docker", "run", "--rm", "-i", "--platform", "linux/amd64", "--network", "none", "--user", "1000:1000",
		"--group-add", strconv.Itoa(os.Getgid()), "-v", volume+":/home/monero/wallet", "-v", cli+":/usr/local/bin/monero-wallet-cli:ro",
		"-v", pw+":/run/escrow-wallet-pass:ro", "--entrypoint", "/usr/local/bin/monero-wallet-cli", xmrWalletCLIImage,
		"--offline", "--wallet-file", "/home/monero/wallet/"+name, "--password-file", "/run/escrow-wallet-pass",
		"--shared-ringdb-dir", xmrRingDBDir, "--log-file", "/dev/null")
	cmd.Stdin = strings.NewReader(stdin)
	b, err := cmd.CombinedOutput()
	password := xmrWalletPassword(r.A.escrowXMR.secret, name)
	if strings.Contains(string(b), password) { // the guide pipes it to the CLI's prompt: it must not be echoed
		t.Fatalf("monero-wallet-cli on %s echoed the wallet password", name)
	}
	out := string(b)
	if err != nil {
		t.Fatalf("monero-wallet-cli on %s: %v\n%s", name, err, out)
	}
	return out
}

// printedRings maps each key image print_ring showed a ring for to the rings shown, in order.
func printedRings(out string) map[string][]string {
	rings := map[string][]string{}
	for _, m := range xmrPrintedRing.FindAllStringSubmatch(out, -1) {
		rings[m[1]] = append(rings[m[1]], strings.TrimSpace(m[2]))
	}
	return rings
}

// breakGlassSQL is the query of the operator guide's stale-ring break-glass (docs/operator-guide.md) that its
// `db_sql -X -At … -c "…"` line containing marker runs, with SETTLEMENT_ID and 'ORDER_ID' as $1: the leg runs the
// guide's own queries.
func breakGlassSQL(t *testing.T, marker string) string {
	t.Helper()
	guide, err := os.ReadFile("docs/operator-guide.md") // the tests run from the repository root (TestMain)
	if err != nil {
		t.Fatal(err)
	}
	_, section, _ := strings.Cut(string(guide), `<a id="stale-ring-break-glass-a-618"></a>`)
	section, _, _ = strings.Cut(section, "\n**Failed and set-aside settlements")
	for line := range strings.Lines(section) {
		line = strings.TrimSpace(line)
		if _, q, ok := strings.Cut(line, ` -c "`); ok && strings.HasPrefix(line, "db_sql -X -At ") && strings.Contains(q, marker) {
			return strings.NewReplacer("SETTLEMENT_ID", "$1", "'ORDER_ID'", "$1").Replace(strings.TrimSuffix(q, `"`))
		}
	}
	t.Fatalf("the operator guide's stale-ring break-glass has no db_sql query with %q", marker)
	return ""
}

// TestXMREscrowRegtestStaleRingRemedyB (A-625): the operator guide's stale-ring break-glass, rehearsed with remedy B, the
// usual path on a market where some other settlement is always in flight. The stale ring is reproduced
// (staleRingReal); another order's release is then built and relayed, so the guide's remedy A precondition query answers
// 1 and the escrow wallet's ring database holds that coin's ring too. Remedy B as documented: the guide's queries give
// the stuck key images and the order's wallet; the escrow wallet-rpc is stopped; the official monero-wallet-cli 0.18.5.1
// (ESCROW_XMR_REGTEST_WALLET_CLI, checked against the signed hashes.txt by scripts/test-escrow-xmr-regtest.sh) opens the
// wallet offline (walletCLI), one piped session per step: the first open answers the background-mining question;
// print_ring shows no ring (with the default ask-password 2 the CLI's ring database key differs from the wallet-rpc's);
// set ask-password 0 (the password on its prompt); print_ring shows a stored ring of 16 for each stuck key image and the
// other coin's; unset_ring for the stuck ones; print_ring answers xmrRingUnset for each and shows the other coin's ring
// unchanged; set ask-password 2, which then reads 2; no file in the volume belongs to another user; the wallet-rpc is
// started. The next pass builds the stored key images to the stored plan and relays it exactly
// once; both settlements confirm.
func TestXMREscrowRegtestStaleRingRemedyB(t *testing.T) {
	daemonC, walletC := os.Getenv("ESCROW_XMR_REGTEST_DAEMON_CONTAINER"), os.Getenv("ESCROW_XMR_REGTEST_WALLET_CONTAINER")
	if daemonC == "" || walletC == "" {
		t.Skip("set ESCROW_XMR_REGTEST_DAEMON_CONTAINER and ESCROW_XMR_REGTEST_WALLET_CONTAINER (scripts/test-escrow-xmr-regtest.sh): this leg stops and starts both")
	}
	cli := os.Getenv("ESCROW_XMR_REGTEST_WALLET_CLI")
	if cli == "" {
		t.Fatal("set ESCROW_XMR_REGTEST_WALLET_CLI to the official monero-wallet-cli 0.18.5.1 for Linux x86-64, as scripts/test-escrow-xmr-regtest.sh does after checking its archive: this leg runs it")
	}
	if out := regtestDocker(t, "run", "--rm", "--platform", "linux/amd64", "--network", "none", "--user", "1000:1000", "-v", cli+":/usr/local/bin/monero-wallet-cli:ro",
		"--entrypoint", "/usr/local/bin/monero-wallet-cli", xmrWalletCLIImage, "--version"); !strings.Contains(out, "(v0.18.5.1-release)") {
		t.Fatalf("ESCROW_XMR_REGTEST_WALLET_CLI is not monero-wallet-cli 0.18.5.1: %q", out)
	}
	// The guide's queries: remedy A's precondition, the stuck key images, the order's wallet.
	inFlightSQL := breakGlassSQL(t, "SELECT count(*) FROM xmr_settlements")
	keyImagesSQL := breakGlassSQL(t, "array_to_string(o.key_images")
	walletSQL := breakGlassSQL(t, "SELECT w.name, s.account_index")
	r, p := newXMRSettleRegtest(t)
	order, id, failed, next := r.staleRingReal(p, daemonC)

	// Another order's release in flight: built (its ring stored) and relayed, not yet confirmed.
	other, _ := r.completedReal()
	otherID := r.releaseReal(other)
	r.synced()
	r.mustPoll()
	inFlight := r.settlement(otherID)
	if next = r.settlement(next.ID); inFlight.State != "relayed" || len(inFlight.KeyImages) == 0 || next.State != "requested" || !strings.HasSuffix(r.waitClass(next.ID), " "+xmrRingStale) {
		t.Fatalf("another release in flight beside the stale re-request: other %+v, re-request %+v %q, wallet transfer error %+v", inFlight, next, r.waitClass(next.ID), p.transferFailure())
	}

	// Remedy A's precondition is not met; the stuck key images; the order's wallet.
	var inFlightN int
	if err := r.DB.QueryRow(inFlightSQL, next.ID).Scan(&inFlightN); err != nil || inFlightN != 1 {
		t.Fatalf("remedy A's precondition query: %d %v, want 1 (release %d in flight)", inFlightN, err, otherID)
	}
	var stuck string
	if err := r.DB.QueryRow(keyImagesSQL, next.ID).Scan(&stuck); err != nil ||
		!slices.Equal(slices.Sorted(slices.Values(strings.Fields(stuck))), slices.Sorted(slices.Values(failed.KeyImages))) {
		t.Fatalf("the guide's key image query: %q %v, want the failed release's %v", stuck, err, failed.KeyImages)
	}
	walletOf := func(order string) string {
		t.Helper()
		var name string
		var account int64
		if err := r.DB.QueryRow(walletSQL, order).Scan(&name, &account); err != nil || account < 1 {
			t.Fatalf("the guide's wallet query for order %s: %q %d %v", shortID(order), name, account, err)
		}
		return name
	}
	name := walletOf(order)
	if walletOf(other) != name {
		t.Fatalf("the in-flight release is in another wallet than %s: the rehearsal needs both coins in one wallet", name)
	}

	// Remedy B, as docs/operator-guide.md documents it: one piped session per step, the password only on the CLI's
	// prompt (stdin) or in the read-only file.
	relays, transfers := p.count("relay_tx"), p.count("transfer")
	printRings := func(kis ...string) string {
		var b strings.Builder
		for _, ki := range kis {
			b.WriteString("print_ring " + ki + "\n")
		}
		return b.String() + "exit\n"
	}
	both := append(slices.Clone(failed.KeyImages), inFlight.KeyImages...)
	pw := xmrWalletPassword(r.A.escrowXMR.secret, name)
	r.walletStopped(walletC, func(image, volume string) {
		run := func(stdin string) string { return r.walletCLI(cli, volume, name, stdin) }
		// The first time monero-wallet-cli opens a wallet it asks whether to set up background mining and reads the
		// answer from stdin, losing the rest of a piped stdin; answered once, the wallet file keeps the answer.
		if out := run("N\n"); !strings.Contains(out, "Background mining not enabled") {
			t.Fatalf("monero-wallet-cli's first open of %s did not settle the background-mining question", name)
		}
		// With ask-password 2 (decrypt, the default) the CLI derives its ring database key from its in-memory-encrypted
		// spend key and sees none of the rings the unattended wallet-rpc stored: unset_ring would remove nothing.
		// The wallet must open and every key image must get the no-ring answer, so a session that failed or lost its stdin
		// cannot pass this check (INFRA127-1).
		if out := run(printRings(both...)); !strings.Contains(out, "Opened wallet") || strings.Count(out, xmrRingUnset) != len(both) || len(printedRings(out)) != 0 {
			t.Fatalf("with ask-password 2 monero-wallet-cli did not open the wallet and answer %q once per key image (%d): the guide's set ask-password 0 step would then be unproven; rings %q", xmrRingUnset, len(both), printedRings(out))
		}
		run("set ask-password 0\n" + pw + "\nexit\n")
		before := printedRings(run(printRings(both...)))
		for _, ki := range both {
			if len(before[ki]) != 1 || len(strings.Fields(before[ki][0])) != 16 {
				t.Fatalf("print_ring of key image %s after set ask-password 0: rings %q, want one stored ring of 16", ki, before[ki])
			}
		}
		run("unset_ring " + strings.Join(failed.KeyImages, " ") + "\nexit\n")
		out := run(printRings(both...))
		after := printedRings(out)
		if n := strings.Count(out, xmrRingUnset); n != len(failed.KeyImages) {
			t.Fatalf("print_ring after unset_ring answered %q %d times, want %d (once per stuck key image); rings %q", xmrRingUnset, n, len(failed.KeyImages), after)
		}
		for _, ki := range failed.KeyImages {
			if len(after[ki]) != 0 {
				t.Fatalf("print_ring after unset_ring of stuck key image %s: %q, want no ring", ki, after[ki])
			}
		}
		for _, ki := range inFlight.KeyImages {
			if len(after[ki]) != 1 || after[ki][0] != before[ki][0] {
				t.Fatalf("print_ring of the in-flight release's key image %s after unset_ring: %q, want its stored ring %q", ki, after[ki], before[ki])
			}
		}
		run("set ask-password 2\n" + pw + "\nexit\n")
		if out := run("set\nexit\n"); !strings.Contains(out, "ask-password = 2 (decrypt)") {
			t.Fatalf("after set ask-password 2 the wallet's settings do not read ask-password = 2 (decrypt)")
		}
		if others := regtestDocker(t, "run", "--rm", "--network", "none", "--user", "1000:1000", "-v", volume+":/home/monero/wallet", "--entrypoint", "find", image,
			"/home/monero/wallet", "!", "-user", "1000"); others != "" {
			t.Fatalf("files in the wallet volume not owned by uid 1000 after monero-wallet-cli: %s", others)
		}
		t.Logf("escrow wallet-rpc container %s stopped; monero-wallet-cli 0.18.5.1 offline on %s as uid 1000: with ask-password 2 print_ring showed no stored ring; after set ask-password 0 it showed the stored rings of the %d stuck key images and of the in-flight release's %d; unset_ring cleared the stuck ones (%q), the in-flight release's ring is unchanged; ask-password restored to 2; started again",
			walletC, name, len(failed.KeyImages), len(inFlight.KeyImages), xmrRingUnset)
	})

	// The next pass builds and relays the re-request once; both settlements confirm.
	r.mustPoll()
	next = r.settlementOf(order, "release")
	if next.Supersedes != id || next.State != "relayed" || r.settlement(id).State != "superseded" || p.count("transfer") != transfers+1 || p.count("relay_tx") != relays+1 {
		t.Fatalf("after unset_ring: old %+v, new %+v %q, transfer %d (was %d), relay_tx %d (was %d), wallet transfer error %+v",
			r.settlement(id), next, r.waitClass(next.ID), p.count("transfer"), transfers, p.count("relay_tx"), relays, p.transferFailure())
	}
	inputs := r.sameSpend(id, failed, next)
	r.confirmReal(next.ID)
	if s := r.settlement(otherID); s.State != "confirmed" || s.TxHash != inFlight.TxHash {
		t.Fatalf("the in-flight release after remedy B: %+v, was %+v", s, inFlight)
	}
	t.Logf("order %s: re-request %d refused with a stale ring for %d key images while release %d of order %s was in flight; after remedy B it spent the same key images %v to the same plan in %s, relayed once, confirmed; release %d confirmed as %s",
		shortID(order), next.ID, len(inputs), otherID, shortID(other), inputs, next.TxHash, otherID, inFlight.TxHash)
}
