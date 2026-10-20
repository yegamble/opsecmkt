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

	// transferError is a fakechain-only diagnostic; never retain requests or successful wallet results.
	transferError *rpcError
}

func newXMRRPCProxy(t *testing.T, target string) *xmrRPCProxy {
	t.Helper()
	p := &xmrRPCProxy{counts: map[string]int{}, drop: map[string]bool{}, around: map[string]func(func()){}}
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

// newXMRSettleRegtest is newXMRRegtest with the App's escrow wallet-rpc behind a counting proxy.
func newXMRSettleRegtest(t *testing.T) (*xmrRegtest, *xmrRPCProxy) {
	t.Helper()
	walletURL := os.Getenv("ESCROW_XMR_REGTEST_WALLET_URL")
	if walletURL == "" {
		t.Skip("set ESCROW_XMR_REGTEST_DAEMON_URL, _WALLET_URL, _FUNDER_URL and _RESTORE_URL (scripts/test-escrow-xmr-regtest.sh) to run against monerod --regtest")
	}
	p := newXMRRPCProxy(t, walletURL)
	t.Setenv("ESCROW_XMR_REGTEST_WALLET_URL", p.URL)
	r := newXMRRegtest(t)
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

// completedReal is a digital order paid its price and mined to 10 confirmations: completed on funding.
func (r *xmrRegtest) completedReal() (order, addr string) {
	r.t.Helper()
	order, addr = r.checkoutReal()
	r.pay(map[string]int64{addr: xmrListingPrice})
	r.mine(10 + xmrMineMargin)
	r.mustPoll()
	if r.state(order) != stateCompleted {
		r.t.Fatalf("funded order: %s (deposits %v)", r.state(order), r.depositConfs(order))
	}
	return order, addr
}

// depositConfs lists order's deposits as txid prefix:confirmations:left out, for a failure message.
func (r *xmrRegtest) depositConfs(order string) []string {
	return agStrings(r.t, r.testEnv, "SELECT left(txid,12)||':'||confirmations||':'||escrow_left_out FROM payments WHERE order_id=$1 ORDER BY created,id", order)
}

// releaseReal requests order's release to a fresh funder subaddress (as the vendor's claim does, slice X-e).
func (r *xmrRegtest) releaseReal(order string) int64 {
	r.t.Helper()
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
// credited, the order completed. It returns the order and the two transactions.
func (r *xmrRegtest) fundedByTwoReal() (order, deep, shallow string) {
	r.t.Helper()
	thr := r.A.escrowXMR.confirmations
	order, addr := r.checkoutReal()
	deep = r.pay(map[string]int64{addr: xmrListingPrice / 2})
	r.mineTo(deep, thr+6)
	shallow = r.pay(map[string]int64{addr: xmrListingPrice - xmrListingPrice/2})
	r.mineTo(shallow, thr+xmrMineMargin)
	r.mustPoll()
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
func (r *xmrRegtest) daemonStoppedAround(name string) func(forward func()) {
	return func(forward func()) {
		ctx := context.Background()
		var before, after struct {
			Height int64 `json:"height"`
		}
		err := r.daemon.call(ctx, "/json_rpc", "get_info", nil, &before)
		if out, stop := exec.Command("docker", "stop", "-t", "30", name).CombinedOutput(); err != nil || stop != nil {
			r.t.Errorf("stopping monerod (container %s): %v, %v %s", name, err, stop, out)
		}
		forward()
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
// the inputs the daemon decodes) to the stored plan, is relayed and confirms.
func TestXMREscrowRegtestReRequest(t *testing.T) {
	container := os.Getenv("ESCROW_XMR_REGTEST_DAEMON_CONTAINER")
	if container == "" {
		t.Skip("set ESCROW_XMR_REGTEST_DAEMON_CONTAINER to the monerod container's name (scripts/test-escrow-xmr-regtest.sh): this leg stops and starts it")
	}
	r, p := newXMRSettleRegtest(t)
	// The relay call waits for the daemon's stop and start through the proxy.
	order, addr := r.completedReal()
	id := r.releaseReal(order)
	p.wrap("relay_tx", r.daemonStoppedAround(container))
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

	// A-614: a reorganisation takes the funding deposit out of the chain (every block since it was mined popped, the
	// pool flushed) before the re-request is carried out. The new settlement takes over the deposit's claim, builds
	// nothing and waits (deposit-missing) rather than failing nothing-to-settle; once the same deposit transaction is
	// mined again it is built from the stored key images as below.
	deposit := agStrings(t, r.testEnv, "SELECT txid FROM payments WHERE order_id=$1", order)[0]
	var raw struct {
		Txs []struct {
			AsHex         string   `json:"as_hex"`
			OutputIndices []uint64 `json:"output_indices"`
		} `json:"txs"`
	}
	r.daemonPost("/get_transactions", map[string]any{"txs_hashes": []string{deposit}}, &raw)
	if len(raw.Txs) != 1 || raw.Txs[0].AsHex == "" {
		t.Fatalf("the daemon does not give funding deposit %s", deposit)
	}
	r.popPast(deposit)
	var flushed map[string]any
	r.call(r.daemon, "flush_txpool", map[string]any{"txids": []string{deposit}}, &flushed)
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
		agInt(r.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1 AND xmr_settlement_id=$2 AND confirmations<0", order, waiting.ID) != 1 {
		t.Fatalf("re-requested while its deposit is missing: old %+v, new %+v %q, transfer %d (deposits %v)", r.settlement(id), waiting, r.waitClass(waiting.ID), p.count("transfer"), r.depositConfs(order))
	}
	t.Logf("funding deposit %s popped and flushed: re-request %d waits (%s) past %d blocks, no transfer", deposit, waiting.ID, xmrWaitMissing, 3*r.A.escrowXMR.confirmations)
	var sent struct {
		Status string `json:"status"`
	}
	r.daemonPost("/send_raw_transaction", map[string]any{"tx_as_hex": raw.Txs[0].AsHex, "do_not_relay": false}, &sent)
	if sent.Status != "OK" {
		t.Fatalf("send_raw_transaction of funding deposit %s: %q", deposit, sent.Status)
	}
	r.mineTo(deposit, 10+xmrMineMargin)
	var restored struct {
		Txs []struct {
			OutputIndices []uint64 `json:"output_indices"`
		} `json:"txs"`
	}
	r.daemonPost("/get_transactions", map[string]any{"txs_hashes": []string{deposit}}, &restored)
	if len(restored.Txs) != 1 {
		t.Fatalf("the daemon does not give restored funding deposit %s", deposit)
	}
	t.Logf("funding deposit output indices before reorg %v, restored %v", raw.Txs[0].OutputIndices, restored.Txs[0].OutputIndices)
	r.synced()
	r.mustPoll()
	next := r.settlementOf(order, "release")
	if next.Supersedes != id || next.State != "relayed" || r.settlement(id).State != "superseded" || p.count("transfer") != 2 ||
		!slices.Equal(slices.Sorted(slices.Values(next.KeyImages)), slices.Sorted(slices.Values(failed.KeyImages))) || next.InputTotal != failed.InputTotal {
		t.Fatalf("re-requested: old %+v, new %+v, transfer %d, wallet transfer error %+v", r.settlement(id), next, p.count("transfer"), p.transferFailure())
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
	r.confirmReal(next.ID)
	t.Logf("order %s: release %s refused %d times while monerod was stopped for its relay, failed; re-requested by an administrator, release %s spent the same key images %v to the same plan (fee %d, was %d) and confirmed",
		shortID(order), failed.TxHash, xmrRelayAttempts, next.TxHash, inputs, next.Fee, failed.Fee)
}
