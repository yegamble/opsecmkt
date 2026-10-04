package market

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Escrow Phase 3, slice X-e: the real-chain leg of a physical Monero escrow order (scripts/test-escrow-xmr-regtest.sh;
// not run in CI), on X-c's helpers (newXMRRegtest). The App's escrow wallet-rpc is reached through a proxy that counts
// each JSON-RPC method, so the leg shows that nothing is built or sent before the order's outcome, and (slice X-d)
// that each outcome's settlement is built and sent once after it.

// xmrRPCCounter counts the JSON-RPC methods a proxy passed to the escrow wallet-rpc.
type xmrRPCCounter struct {
	mu sync.Mutex
	n  map[string]int
}

func (c *xmrRPCCounter) count(methods ...string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := 0
	for _, m := range methods {
		total += c.n[m]
	}
	return total
}

// xmrCountingProxy is a reverse proxy to target that counts each JSON-RPC method it forwards.
func xmrCountingProxy(t *testing.T, target string) (*httptest.Server, *xmrRPCCounter) {
	t.Helper()
	u, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	rp := httputil.NewSingleHostReverseProxy(u)
	c := &xmrRPCCounter{n: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &req)
		c.mu.Lock()
		c.n[req.Method]++
		c.mu.Unlock()
		r.Body, r.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, c
}

// refusedAddress is an address with a valid checksum whose keys the wallet refuses (validate_address false), so
// transfer answers -2 before it builds anything. Random keys pass the wallet's check about one time in four and may then
// crash monero-wallet-rpc 0.18.5.1 mid-build (exit 139, seen on this leg; A-417's offline key check is to refuse those),
// so the leg asks the funder's wallet until it finds one it refuses.
func (r *xmrRegtest) refusedAddress(label string) string {
	r.t.Helper()
	for i := range 64 {
		addr := testXMRAddr("refund "+label+" "+strconv.Itoa(i), true)
		var res struct {
			Valid bool `json:"valid"`
		}
		r.call(r.funder, "validate_address", map[string]any{"address": addr, "any_net_type": false}, &res)
		if !res.Valid {
			return addr
		}
	}
	r.t.Fatal("no refused test address in 64 tries")
	return ""
}

// TestXMREscrowRegtestPhysical: four physical Monero escrow orders funded in one real transaction on the fakechain
// (10 blocks), through the real checkout form. While an order is paid and then shipped (the release requested to a
// real subaddress of the funder's wallet), the escrow wallet-rpc gets no transfer, relay_tx or other spending call,
// and the release is not buildable. Then: Confirm receipt completes the first order and its release becomes buildable;
// the release deadline completes the second; a dispute decided for the buyer supersedes the third's release and
// requests its refund; the buyer's no-ship cancel of the fourth requests its refund. Slice X-d: each of the first three
// outcomes' settlements is then built, sent once and confirmed; the fourth buyer's refund address has a valid checksum
// but keys the wallet refuses, so transfer answers -2 and the refund waits, flagged for staff, and is not tried again.
func TestXMREscrowRegtestPhysical(t *testing.T) {
	r := newXMRRegtest(t)
	proxy, calls := xmrCountingProxy(t, os.Getenv("ESCROW_XMR_REGTEST_WALLET_URL"))
	t.Setenv("ESCROW_XMR_WALLET_RPC_URL", proxy.URL)
	r.restart()
	if !r.A.xmrEscrowReady() {
		t.Fatalf("Monero escrow did not start through the counting proxy: %s", r.A.escrowXMR.startState)
	}
	spending := []string{"transfer", "transfer_split", "sweep_all", "sweep_single", "sweep_dust", "relay_tx", "submit_transfer"}
	r.mustPoll()
	physical := r.physical(r.vendor)
	_, adminSess := r.user("xr_padmin_"+randomToken()[:6], "admin")
	type leg struct{ order, buyerSess, addr, refund string }
	var legs [4]leg
	amounts := map[string]int64{}
	for i := range legs {
		buyer, sess := r.user("xr_pb_"+randomToken()[:8], "buyer")
		order := r.order(buyer, physical, "XMR", stateDraft)
		refund := r.funderAddress() // a real subaddress of the funder's wallet: the refund can be sent there
		if i == 3 {
			refund = r.refusedAddress(order)
		}
		if w := r.checkout(sess, order, refund); w.Code != 303 {
			t.Fatalf("checkout: %d %s", w.Code, w.Body.String())
		}
		addr := agStrings(t, r.testEnv, "SELECT address FROM escrows WHERE order_id=$1", order)[0]
		legs[i] = leg{order, sess, addr, refund}
		amounts[addr] = xmrListingPrice
	}
	tx := r.pay(amounts)
	r.mine(10 + xmrMineMargin)
	r.mustPoll()
	for i, l := range legs {
		if r.state(l.order) != statePaid {
			t.Fatalf("order %d after 10 blocks: %s", i, r.state(l.order))
		}
	}
	t.Logf("transaction %s funded 4 physical Monero escrow orders; all paid after 10 blocks", tx)
	claim := func() string {
		var res struct {
			Address string `json:"address"`
		}
		r.call(r.funder, "create_address", map[string]any{"account_index": 0}, &res)
		return res.Address
	}
	for _, l := range legs[:3] {
		if code, body := r.xmrShip(l.order, r.vendorSess, claim(), testPassword); code != 303 {
			t.Fatalf("ship: %d %s", code, body)
		}
	}
	r.mine(2)
	r.mustPoll()
	r.mustPoll()
	for i, l := range legs[:3] {
		if got := r.xmrSettlements(l.order); r.state(l.order) != stateShipped || len(got) != 1 || !strings.HasPrefix(got[0], "release requested ") ||
			xmrSettlementBuildable("release", stateShipped, "") {
			t.Fatalf("order %d after shipping: %s %v", i, r.state(l.order), got)
		}
	}
	if n := calls.count(spending...); n != 0 {
		t.Fatalf("%d spending calls to the escrow wallet-rpc through shipped", n)
	}
	t.Logf("3 orders shipped, releases requested; escrow wallet-rpc spending calls: %d (get_transfers %d)", calls.count(spending...), calls.count("get_transfers"))

	// Confirm receipt: completed, and the release is buildable (the database agrees).
	first := legs[0]
	if code, body := r.xmrConfirm(first.order, first.buyerSess, testPassword); code != 303 {
		t.Fatalf("Confirm receipt: %d %s", code, body)
	}
	var may bool
	r.DB.QueryRow("SELECT escrow_market_may_sign('release',$1)", first.order).Scan(&may)
	if r.state(first.order) != stateCompleted || !may || !xmrSettlementBuildable("release", stateCompleted, "") {
		t.Fatalf("after Confirm receipt: %s, may sign %v", r.state(first.order), may)
	}
	t.Logf("order %s: Confirm receipt completed it; its release is requested and buildable", shortID(first.order))
	// Slice X-d: the next pass builds the release, stores it and relays it; transfer and relay_tx run once each.
	r.mustPoll()
	if got := r.xmrSettlements(first.order); len(got) != 1 || !strings.HasPrefix(got[0], "release relayed ") || calls.count("transfer") != 1 || calls.count("relay_tx") != 1 {
		t.Fatalf("after Confirm receipt and a pass: %v, transfer %d, relay_tx %d", got, calls.count("transfer"), calls.count("relay_tx"))
	}
	t.Logf("order %s: its release was built and relayed by the next pass (transfer 1, relay_tx 1)", shortID(first.order))

	// The deadline.
	second := legs[1]
	r.forceDeadline(second.order, "release_at=now()-interval '1 minute'")
	r.mustPoll()
	if r.state(second.order) != stateCompleted || !anyContains(r.events(second.order), "auto-release deadline passed") {
		t.Fatalf("at the deadline: %s", r.state(second.order))
	}
	t.Logf("order %s: completed at its release deadline by the Monero escrow watcher; release requested and buildable", shortID(second.order))

	// A dispute decided for the buyer.
	third := legs[2]
	if code, body := r.xmrDispute(third.order, third.buyerSess); code != 303 {
		t.Fatalf("dispute: %d %s", code, body)
	}
	dispute := agStrings(t, r.testEnv, "SELECT id FROM disputes WHERE order_id=$1", third.order)[0]
	if code, body := r.xmrPost("/resolve", adminSess, url.Values{"id": {dispute}, "outcome": {"refund"}, "resolution": {"Decided from the evidence both parties sent."},
		"payout_confirmed": {"confirmed"}, "amount_seen": {strconv.FormatInt(xmrListingPrice, 10)}, "reply_seen": {"0"}, "password": {testPassword}}); code != 303 {
		t.Fatalf("resolve: %d %s", code, body)
	}
	if got := r.xmrSettlements(third.order); len(got) != 2 || !strings.HasPrefix(got[0], "release superseded ") || got[1] != "refund requested "+third.refund {
		t.Fatalf("after deciding for the buyer: %v", got)
	}
	t.Logf("order %s: dispute decided for the buyer; the release is superseded and the refund requested", shortID(third.order))

	// The buyer's no-ship cancel.
	fourth := legs[3]
	r.forceDeadline(fourth.order, "ship_by=now()-interval '1 minute'")
	if code, body := r.xmrBuyerCancel(fourth.order, fourth.buyerSess); code != 303 {
		t.Fatalf("no-ship cancel: %d %s", code, body)
	}
	if got := r.xmrSettlements(fourth.order); r.state(fourth.order) != stateCancelled || len(got) != 1 || got[0] != "refund requested "+fourth.refund {
		t.Fatalf("after the no-ship cancel: %s %v", r.state(fourth.order), got)
	}
	t.Logf("order %s: the buyer's no-ship cancel requested the refund", shortID(fourth.order))

	// Slice X-d: the second's release and the third's refund are built and sent; the wallet refuses the fourth's refund
	// address (-2), which validate_address names: the refund waits, flagged once, and is not tried again.
	r.mustPoll()
	r.mustPoll()
	r.mine(10 + xmrMineMargin)
	r.mustPoll()
	for _, c := range []struct{ order, want string }{{first.order, "release confirmed "}, {second.order, "release confirmed "}, {third.order, "refund confirmed " + third.refund}} {
		got := r.xmrSettlements(c.order)
		if last := got[len(got)-1]; !strings.HasPrefix(last, c.want) {
			t.Fatalf("order %s: %v, want %s", shortID(c.order), got, c.want)
		}
	}
	refused := agStrings(t, r.testEnv, "SELECT state||' '||send_error FROM xmr_settlements WHERE order_id=$1", fourth.order)
	flags := agInt(r.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND strpos(note,'not sent: The escrow wallet refuses')>0", fourth.order)
	if len(refused) != 1 || refused[0] != "requested "+xmrDestRefused+"-claimant" || flags != 1 {
		t.Fatalf("the refused refund: %v, staff flag notes %d", refused, flags)
	}
	if calls.count("transfer") != 4 || calls.count("relay_tx") != 3 || calls.count("transfer_split", "sweep_all", "sweep_single", "sweep_dust", "submit_transfer") != 0 {
		t.Fatalf("spending calls: transfer %d, relay_tx %d", calls.count("transfer"), calls.count("relay_tx"))
	}
	t.Logf("three settlements built, sent and confirmed after their outcomes; the refund to an invalid address (order %s) was refused by the wallet once (-2), flagged and not tried again: transfer 4, relay_tx 3", shortID(fourth.order))
}
