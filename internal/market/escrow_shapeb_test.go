package market

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"html"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A-456a (shape (b) slice 4, first half; rulings P4-R2, P4-R17, P4-R18; ADDENDUM QA4W5K-6): checkout picks the escrow's
// shape from both parties' active keys, refuses a mixed pair, takes a per-order index i >= 2 from the account's one
// shared counter, derives every per-order key in Go and cross-checks it with Core before anything is stored, imports
// the per-order wallet's ranged descriptors into the watch-only wallet and routes a deposit to a sibling address to the
// stray refund. (A-456b removed the checkout switch A-456a kept off.) DB integration with the simulated Core of
// escrow_fake_test.go (real BIP32 public derivation for per-order keys) and the fake signer of escrow_signer_test.go:
// simulated RPC, not chain evidence (scripts/test-escrow-regtest.sh has the real Core leg).

// newShapeBEnv is newEscrowEnv with a fake escrow signer co-signing as cmd/escrow-signer does: how every shape-(b) test
// that builds per-order escrows on the fake node starts. The fake derives each order wallet's keys in Go (math/big,
// slow under -race on a loaded host), so its RPC calls get a generous rpcTimeout instead of the production 10 s, set
// before the app starts and restored after it stops (a call inside a form post is still bounded by ServeHTTP's request
// deadline: what keeps a checkout within it is the fake's derivation memo, ckdMemo). No test in this package uses
// t.Parallel (testenv_test.go), so the package variable is safe to set; a test exercising a timeout sets its own
// smaller value after this.
func newShapeBEnv(t *testing.T) (*escrowEnv, *fakeSigner) {
	t.Helper()
	prev := rpcTimeout
	rpcTimeout = 2 * time.Minute
	t.Cleanup(func() { rpcTimeout = prev })
	f := newFakeSigner(t, signerKey(t, 7))
	f.cosign = true // A-456b: it co-signs as cmd/escrow-signer does, refusing what that signer refuses
	x := signerApp(t, f.path)
	return x, f
}

// newPerOrderEnv is newShapeBEnv with both parties' active keys per-order wallet keys.
func newPerOrderEnv(t *testing.T) (*escrowEnv, *fakeSigner) {
	t.Helper()
	x, f := newShapeBEnv(t)
	agExec(x.testEnv, "UPDATE user_escrow_keys SET layout='per_order' WHERE user_id IN ($1,$2)", x.buyer, x.vendor)
	return x, f
}

// perOrderChild is Go's per-order child <i> of an extended key (the order wallet's key, served rooted) and its
// receiving key /0/0.
func perOrderChild(t *testing.T, k bip32Key, i uint32) (child, leaf bip32Key) {
	t.Helper()
	child, used, err := ckdPub(k, i)
	if err != nil || used != i {
		t.Fatalf("per-order child %d: used %d, %v", i, used, err)
	}
	return child, siblingKey(t, child, 0, 0)
}

// siblingKey is a per-order wallet's key /branch/k.
func siblingKey(t *testing.T, child bip32Key, branch, k uint32) bip32Key {
	t.Helper()
	c, _, err := ckdPub(child, branch)
	if err == nil {
		c, _, err = ckdPub(c, k)
	}
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// accountKey is a party's registered account key as an extended key (escrowKey), for Go's derivation.
func accountKey(t *testing.T, expr string) bip32Key {
	t.Helper()
	k := mustParseKey(t, expr)
	return bip32Key{Depth: byte(len(k.Path)), ChainCode: k.ChainCode, PubKey: k.PubKey}
}

// p2wshScript is the scriptPubKey paying wsh(sortedmulti(2, keys)).
func p2wshScript(keys ...[]byte) []byte {
	sum := sha256.Sum256(sortedMultisigScript(keys...))
	return append([]byte{0, 32}, sum[:]...)
}

// shapeBRow is one escrow_pubkeys row of an order.
type shapeBRow struct{ party, suffix, fp, origin, pubkey string }

func (x *escrowEnv) escrowRows(order string) map[string]shapeBRow {
	x.t.Helper()
	rows, err := x.DB.Query(`SELECT p.party,p.derivation_suffix,p.origin_fingerprint,p.origin_path,p.pubkey,p.per_order_xpub IS NULL FROM escrow_pubkeys p
		JOIN escrows e ON e.id=p.escrow_id WHERE e.order_id=$1`, order)
	if err != nil {
		x.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]shapeBRow{}
	for rows.Next() {
		var r shapeBRow
		var noXpub bool
		if err = rows.Scan(&r.party, &r.suffix, &r.fp, &r.origin, &r.pubkey, &noXpub); err != nil {
			x.t.Fatal(err)
		}
		// QA5W4A-5, QA5W5A-1: every per-order key's order wallet key is stored with it (the market's too), and nothing
		// else has one.
		if perOrder := strings.Count(r.suffix, "/") == 3; noXpub == perOrder {
			x.t.Fatalf("%s %s: per_order_xpub stored %v", r.party, r.suffix, !noXpub)
		}
		out[r.party] = r
	}
	return out
}

// checkoutAside creates an escrow for a new draft and sets its order aside (cancelled in SQL), so a test can create more
// than maxAwaitingPayment escrows for one buyer.
func (x *escrowEnv) checkoutAside(label string) string {
	x.t.Helper()
	o := x.draft()
	x.mustCheckout(o, regtestAddr(label))
	agExec(x.testEnv, "UPDATE orders SET state='cancelled' WHERE id=$1", o)
	return o
}

func (x *escrowEnv) marketIndex(order string) int64 {
	x.t.Helper()
	return agInt64(x.t, x.testEnv, "SELECT market_index FROM escrows WHERE order_id=$1", order)
}

// sibling is the stored sibling address /branch/k of order's escrow.
func (x *escrowEnv) sibling(order string, branch, k int) string {
	x.t.Helper()
	got := agStrings(x.t, x.testEnv, "SELECT s.address FROM escrow_siblings s JOIN escrows e ON e.id=s.escrow_id WHERE e.order_id=$1 AND s.branch=$2 AND s.idx=$3", order, branch, k)
	if len(got) != 1 {
		x.t.Fatalf("sibling /%d/%d of %s: %v", branch, k, order, got)
	}
	return got[0]
}

// P4-R18: an escrow is shape (b) only when both parties' active keys are per-order wallet keys and shape (a) only when
// both are Bitcoin Core keys. A mixed pair is refused, both ways, with a plain message naming the vendor's key type (as
// the listing shows it), on the order page and at checkout, with nothing written and no node call, whether an escrow
// signer is configured (per-order escrows offered, A-456b) or not.
func TestEscrowShapeBMixedPairRefused(t *testing.T) {
	for _, c := range []struct {
		name, perOrder, want string
	}{
		{"vendor per-order, buyer Core", "vendor", "The vendor's Bitcoin escrow key is for a per-order wallet and yours is for Bitcoin Core"},
		{"vendor Core, buyer per-order", "buyer", "The vendor's Bitcoin escrow key is for Bitcoin Core and yours is for a per-order wallet"},
	} {
		for _, signer := range []bool{false, true} {
			t.Run(c.name+" signer "+strconv.FormatBool(signer), func(t *testing.T) {
				var x *escrowEnv
				if signer {
					x = signerApp(t, newFakeSigner(t, signerKey(t, 7)).path)
				} else {
					x = newEscrowEnv(t)
				}
				uid := map[string]string{"buyer": x.buyer, "vendor": x.vendor}[c.perOrder]
				agExec(x.testEnv, "UPDATE user_escrow_keys SET layout='per_order' WHERE user_id=$1", uid)
				page := html.UnescapeString(x.page("/order?id="+x.order, x.buyerSess))
				if !strings.Contains(page, c.want) || strings.Contains(page, "Pay into an escrow</button>") {
					t.Fatalf("order page with a mixed pair:\n%s", page)
				}
				w := x.checkout(x.order, regtestAddr("mixed"))
				if body := html.UnescapeString(w.Body.String()); w.Code != 409 || !strings.Contains(body, c.want) || !strings.Contains(body, "Nothing was changed.") {
					t.Fatalf("mixed checkout: %d %s", w.Code, body)
				}
				x.nothingWritten(x.order)
				if n := x.core.count("createpsbt") + x.core.count("importdescriptors") + x.core.count("deriveaddresses"); n != 0 {
					t.Fatalf("%d node calls for a refused mixed checkout", n)
				}
			})
		}
	}
}

// A-456b removed A-456a's per-order switch: a buyer and a vendor whose active keys are both per-order wallet keys are
// offered escrow and get a per-order escrow, now that the market co-signs its settlements through the escrow signer
// (TestEscrowShapeBCosignedBySigner). Without an escrow signer configured, no per-order escrow is offered or created:
// nothing could co-sign it (and, under escrow-only intake, TestEscrowOnlyPerOrderVendorNeedsSigner).
func TestEscrowShapeBOpenToUsers(t *testing.T) {
	f := newFakeSigner(t, signerKey(t, 7))
	x := signerApp(t, f.path)
	agExec(x.testEnv, "UPDATE user_escrow_keys SET layout='per_order' WHERE user_id IN ($1,$2)", x.buyer, x.vendor)
	page := html.UnescapeString(x.page("/order?id="+x.order, x.buyerSess))
	if !strings.Contains(page, "Pay into an escrow</button>") || strings.Contains(page, "not available on this market") {
		t.Fatalf("order page with two per-order keys does not offer escrow:\n%s", page)
	}
	x.mustCheckout(x.order, regtestAddr("open-1"))
	if rows := x.escrowRows(x.order); rows["market"].suffix != "/"+strconv.FormatInt(x.marketIndex(x.order), 10)+"/0/0" {
		t.Fatalf("not a per-order escrow: %+v", rows)
	}

	n := newEscrowEnv(t) // no ESCROW_SIGNER_SOCKET
	agExec(n.testEnv, "UPDATE user_escrow_keys SET layout='per_order' WHERE user_id IN ($1,$2)", n.buyer, n.vendor)
	const want = "Escrows with per-order wallets need the market's escrow signer, which is not available on this market right now."
	page = html.UnescapeString(n.page("/order?id="+n.order, n.buyerSess))
	if !strings.Contains(page, want) || strings.Contains(page, "Pay into an escrow</button>") {
		t.Fatalf("order page without a signer:\n%s", page)
	}
	w := n.checkout(n.order, regtestAddr("open-nosigner"))
	if body := html.UnescapeString(w.Body.String()); w.Code != 503 || !strings.Contains(body, "escrow signer") {
		t.Fatalf("checkout without a signer: %d %s", w.Code, body)
	}
	n.nothingWritten(n.order)
}

// Shape (b): each party's key is its account key's child <i>/0/0 with i from the account's one shared counter (2 for a
// fresh account, never 0 or 1), the market's the escrow signer's account key's child <m>/0/0 with m >= 2 from the market
// counter; the address is the 2-of-3 of the keys Go derived; per_order_xpub holds the buyer's and the vendor's order
// wallet key (TestEscrowShapeBOrderXpubStored), the market's none; the watch-only wallet imports
// the bare-key descriptor (labelled) and the per-order wallet's ranged descriptors /0/0..19 and /1/0..9 from the rooted
// per-order keys only; the 29 sibling addresses stored are Go's. Two Bitcoin Core keys still make shape (a).
func TestEscrowShapeBChosen(t *testing.T) {
	x, f := newPerOrderEnv(t)
	order := x.order
	addr := x.mustCheckout(order, regtestAddr("shapeb1"))
	rows := x.escrowRows(order)
	m := x.marketIndex(order)
	if m < 2 || m >= escrowPracticeIndex {
		t.Fatalf("market per-order index %d", m)
	}
	signerFP := f.key.Fingerprint()
	want := map[string]shapeBRow{
		"buyer":  {"buyer", "/2/0/0", "7450f935", "/48h/1h/0h/2h", ""},
		"vendor": {"vendor", "/2/0/0", "5eed5eed", "/48h/1h/0h/2h", ""},
		"market": {"market", "/" + strconv.FormatInt(m, 10) + "/0/0", hex.EncodeToString(signerFP[:]), "", ""},
	}
	children := map[string]bip32Key{}
	var leaves [][]byte
	for party, key := range map[string]bip32Key{"buyer": accountKey(t, x.buyerKey), "vendor": accountKey(t, x.vendorKey), "market": f.key} {
		i := uint32(2)
		if party == "market" {
			i = uint32(m)
		}
		child, leaf := perOrderChild(t, key, i)
		children[party] = child
		leaves = append(leaves, leaf.PubKey[:])
		w := want[party]
		w.pubkey = hex.EncodeToString(leaf.PubKey[:])
		if rows[party] != w {
			t.Errorf("%s row %+v, want %+v", party, rows[party], w)
		}
	}
	if spk, err := escrowAddressScript("regtest", addr); err != nil || !bytes.Equal(spk, p2wshScript(leaves...)) {
		t.Fatalf("escrow address %s is not the 2-of-3 of the keys Go derived (%v)", addr, err)
	}
	reqs := x.core.importRequests()
	if len(reqs) != 32 || x.core.count("importdescriptors") != 1 {
		t.Fatalf("%d import requests in %d calls, want the bare-key descriptor, two ranged ones and 29 sibling addresses in one: %v", len(reqs), x.core.count("importdescriptors"), reqs)
	}
	// Each sibling address labelled with the escrow's label: Core lists a deposit by listreceivedbyaddress only for an
	// address in the address book, where an unlabelled ranged import puts none.
	stored := agStrings(t, x.testEnv, "SELECT s.address FROM escrow_siblings s JOIN escrows e ON e.id=s.escrow_id WHERE e.order_id=$1 ORDER BY s.branch,s.idx", order)
	for n, r := range reqs[3:] {
		if r["desc"] != withChecksum("addr("+stored[n]+")") || r["label"] != "escrow:"+order || r["timestamp"] != "now" {
			t.Fatalf("sibling import %d: %v", n, r)
		}
	}
	if reqs[0]["label"] != "escrow:"+order || strings.Contains(reqs[0]["desc"].(string), "tpub") {
		t.Fatalf("first import %v", reqs[0])
	}
	for n, c := range []struct {
		branch string
		to     float64
	}{{"/0/*", 19}, {"/1/*", 9}} {
		r := reqs[n+1]
		desc, _ := r["desc"].(string)
		rng, _ := r["range"].([]any)
		if _, labelled := r["label"]; labelled || len(rng) != 2 || rng[0] != float64(0) || rng[1] != c.to || r["active"] != false {
			t.Fatalf("ranged import %v", r)
		}
		for _, party := range []string{"buyer", "vendor", "market"} {
			if !strings.Contains(desc, children[party].Rooted(versionTpub)+c.branch) {
				t.Fatalf("ranged import %s lacks the %s's rooted per-order key", desc, party)
			}
		}
		if strings.Contains(desc, "[") || strings.Contains(desc, mustParseKey(t, x.buyerKey).XPub) || strings.Contains(desc, f.key.Rooted(versionTpub)) {
			t.Fatalf("ranged import %s names an account key or an origin", desc)
		}
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM escrow_siblings s JOIN escrows e ON e.id=s.escrow_id WHERE e.order_id=$1", order); n != 29 {
		t.Fatalf("%d sibling addresses stored, want 29", n)
	}
	for _, s := range []struct{ branch, k int }{{0, 1}, {0, 19}, {1, 0}, {1, 3}, {1, 9}} {
		var keys [][]byte
		for _, party := range []string{"buyer", "vendor", "market"} {
			sk := siblingKey(t, children[party], uint32(s.branch), uint32(s.k))
			keys = append(keys, sk.PubKey[:])
		}
		if spk, err := escrowAddressScript("regtest", x.sibling(order, s.branch, s.k)); err != nil || !bytes.Equal(spk, p2wshScript(keys...)) {
			t.Fatalf("sibling /%d/%d is not Go's (%v)", s.branch, s.k, err)
		}
	}

	// The next escrow of the same accounts takes the next index of the shared counter.
	second := x.draft()
	x.mustCheckout(second, regtestAddr("shapeb2"))
	if r := x.escrowRows(second); r["buyer"].suffix != "/3/0/0" || r["vendor"].suffix != "/3/0/0" || x.marketIndex(second) <= m {
		t.Fatalf("second escrow %+v, market index %d", r, x.marketIndex(second))
	}
}

// The one shared counter (P4-R2): an account's per-order index continues past the highest index its escrows used in
// either shape, never below the floor a restore set, and never below 2.
func TestEscrowShapeBSharedCounter(t *testing.T) {
	x, _ := newShapeBEnv(t)
	for i := range 2 { // two shape-(a) escrows: /0/1 and /0/2
		x.checkoutAside("shared-a-" + strconv.Itoa(i))
	}
	agExec(x.testEnv, "UPDATE user_escrow_keys SET layout='per_order' WHERE user_id IN ($1,$2)", x.buyer, x.vendor)
	b := x.checkoutAside("shared-b")
	if r := x.escrowRows(b); r["buyer"].suffix != "/3/0/0" || r["vendor"].suffix != "/3/0/0" {
		t.Fatalf("per-order index after two shape-(a) escrows: %+v", r)
	}
	agExec(x.testEnv, "INSERT INTO escrow_index_floors(user_id,next_index) VALUES($1,60)", x.buyer)
	c := x.checkoutAside("shared-c")
	if r := x.escrowRows(c); r["buyer"].suffix != "/60/0/0" || r["vendor"].suffix != "/4/0/0" {
		t.Fatalf("per-order index above a floor: %+v", r)
	}
	// Both keys Bitcoin Core again: shape (a), counting on from the same counter.
	agExec(x.testEnv, "UPDATE user_escrow_keys SET layout='core' WHERE user_id IN ($1,$2)", x.buyer, x.vendor)
	d := x.checkoutAside("shared-d")
	if r := x.escrowRows(d); r["buyer"].suffix != "/0/61" || r["vendor"].suffix != "/0/5" || r["market"].origin != "/84h/1h/0h" {
		t.Fatalf("shape (a) after shape (b): %+v", r)
	}
}

// The reserved practice index 2^31-1 is never served to an escrow, for a party or for the market, and no per-order index
// reaches 2^31: the checkout is refused with nothing written.
func TestEscrowShapeBNeverPractice(t *testing.T) {
	x, _ := newPerOrderEnv(t)
	agExec(x.testEnv, "INSERT INTO escrow_index_floors(user_id,next_index) VALUES($1,2147483646)", x.buyer)
	x.mustCheckout(x.order, regtestAddr("practice-1")) // the last index below the practice index
	if r := x.escrowRows(x.order); r["buyer"].suffix != "/2147483646/0/0" {
		t.Fatalf("buyer suffix %q", r["buyer"].suffix)
	}
	next := x.draft()
	if w := x.checkout(next, regtestAddr("practice-2")); w.Code == 303 {
		t.Fatalf("the practice index was served: %s", agStrings(t, x.testEnv, "SELECT derivation_suffix FROM escrow_pubkeys WHERE party='buyer' ORDER BY escrow_id"))
	}
	x.nothingWritten(next)

	// The market counter: 2147483647 is skipped, and 2147483648 is past every unhardened index.
	y, _ := newPerOrderEnv(t)
	agExec(y.testEnv, "SELECT setval('escrow_market_index',2147483646,true)")
	imports := len(y.core.importRequests())
	w := y.checkout(y.order, regtestAddr("practice-m"))
	if w.Code != 503 || !strings.Contains(w.Body.String(), "try again later") {
		t.Fatalf("market index past 2^31-2: %d %s", w.Code, w.Body.String())
	}
	y.nothingWritten(y.order)
	if len(y.core.importRequests()) != imports {
		t.Fatal("an escrow at the practice index was imported")
	}
}

// P4-R2's cross-check: Core must derive every per-order key and address Go derived. A node whose derivation disagrees
// (ckdSkew flips a chain-code bit before each of its public derivations) gets the plain retry answer, and nothing is
// stored or imported.
func TestEscrowShapeBCrossCheckMismatch(t *testing.T) {
	logs := captureLog(t)
	x, _ := newPerOrderEnv(t)
	x.core.mu.Lock()
	x.core.ckdSkew = true
	x.core.mu.Unlock()
	w := x.checkout(x.order, regtestAddr("skew"))
	if w.Code != 503 || !strings.Contains(w.Body.String(), "Nothing was changed; try again later.") {
		t.Fatalf("checkout with a disagreeing node: %d %s", w.Code, w.Body.String())
	}
	x.nothingWritten(x.order)
	if n := agInt(x.testEnv, "SELECT count(*) FROM escrow_siblings"); n != 0 {
		t.Fatalf("%d sibling rows stored", n)
	}
	if n := x.core.count("importdescriptors"); n != 0 {
		t.Fatalf("%d imports after a failed cross-check", n)
	}
	if !strings.Contains(logs.String(), "per-order-derivation-mismatch") {
		t.Fatalf("log: %s", logs.String())
	}
}

// Shape (b) needs the escrow signer: with none configured the checkout is refused with a plain retry answer.
func TestEscrowShapeBNeedsSigner(t *testing.T) {
	x := newEscrowEnv(t)
	agExec(x.testEnv, "UPDATE user_escrow_keys SET layout='per_order' WHERE user_id IN ($1,$2)", x.buyer, x.vendor)
	w := x.checkout(x.order, regtestAddr("nosigner"))
	if body := html.UnescapeString(w.Body.String()); w.Code != 503 || !strings.Contains(body, "escrow signer") || !strings.Contains(body, "try again later") {
		t.Fatalf("checkout without a signer: %d %s", w.Code, body)
	}
	x.nothingWritten(x.order)
}

// A deposit to a sibling address of the per-order wallet (/0/1 and /1/3 here) is recorded for the order but never counts:
// not credited, left out (payments.escrow_left_out) and, once the order is closed, returned by the stray refund, never
// by a release. An order paid only at siblings is cancelled at the end of its payment window as unpaid; one paid at its
// address completes and its sibling deposit goes to a stray refund. The orphan sweep does not list sibling addresses.
func TestEscrowShapeBSiblingDeposits(t *testing.T) {
	x, _ := newPerOrderEnv(t)
	x.mustCheckout(x.order, regtestAddr("sib-1"))
	price := agInt64(t, x.testEnv, "SELECT amount FROM orders WHERE id=$1", x.order)
	s01, s13 := x.sibling(x.order, 0, 1), x.sibling(x.order, 1, 3)
	x.core.deposit(s01, txidOf("sib01"), 0, price, 6)
	x.core.deposit(s13, txidOf("sib13"), 1, 5000, 6)
	x.poll()
	rows := agStrings(t, x.testEnv, "SELECT address||':'||escrow_left_out||':'||credited FROM payments WHERE order_id=$1 ORDER BY amount", x.order)
	if strings.Join(rows, ",") != s13+":true:false,"+s01+":true:false" {
		t.Fatalf("sibling deposits %v", rows)
	}
	if s := x.state(x.order); s != stateAwaitingPayment {
		t.Fatalf("an order paid only at sibling addresses moved to %s", s)
	}
	agExec(x.testEnv, "UPDATE escrows SET created=now()-interval '10 days' WHERE order_id=$1", x.order)
	agExec(x.testEnv, "UPDATE payments SET created=now()-interval '10 days' WHERE order_id=$1", x.order)
	x.poll()
	if s := x.state(x.order); s != stateCancelled {
		t.Fatalf("order paid only at siblings after its window: %s", s)
	}
	if kinds := agStrings(t, x.testEnv, "SELECT kind FROM settlements WHERE order_id=$1", x.order); strings.Join(kinds, ",") != "stray_refund" {
		t.Fatalf("settlements %v, want one stray refund", kinds)
	}

	// Paid at its address: completed on funding; the sibling deposit is left out for a stray refund.
	paid := x.draft()
	addr := x.mustCheckout(paid, regtestAddr("sib-2"))
	x.core.deposit(addr, txidOf("sib-main"), 0, agInt64(t, x.testEnv, "SELECT amount FROM orders WHERE id=$1", paid), 6)
	x.core.deposit(x.sibling(paid, 0, 1), txidOf("sib-top"), 0, 7000, 6)
	x.poll()
	x.poll()
	if s := x.state(paid); s != stateCompleted {
		t.Fatalf("order paid at its address: %s", s)
	}
	if got := agStrings(t, x.testEnv, "SELECT (address=$2)||':'||credited||':'||escrow_left_out FROM payments WHERE order_id=$1 ORDER BY amount", paid, addr); strings.Join(got, ",") != "false:false:true,true:true:false" {
		t.Fatalf("payments %v", got)
	}
	if kinds := agStrings(t, x.testEnv, "SELECT kind FROM settlements WHERE order_id=$1", paid); strings.Join(kinds, ",") != "stray_refund" {
		t.Fatalf("settlements %v, want one stray refund", kinds)
	}

	agExec(x.testEnv, "INSERT INTO settings(key,value) VALUES('escrow_sweep_required','true') ON CONFLICT (key) DO UPDATE SET value='true'")
	x.poll()
	if n := agInt(x.testEnv, "SELECT count(*) FROM escrow_orphans"); n != 0 {
		t.Fatalf("%d orphans listed: %v", n, agStrings(t, x.testEnv, "SELECT address FROM escrow_orphans"))
	}
}

// The sibling coverage survives a restart and its re-import is idempotent: after a restart the first watcher pass asks
// the watch-only wallet whether it holds each shape-(b) escrow's address and siblings and imports nothing when it does; a
// watch-only wallet that lost them (restored from before the escrow) gets the escrow's descriptor, the ranged ones and
// the siblings as labelled addr(…) descriptors again from the escrow's creation time, once; a sibling deposit is then
// routed as before.
func TestEscrowShapeBRestartReimport(t *testing.T) {
	x, _ := newPerOrderEnv(t)
	x.mustCheckout(x.order, regtestAddr("restart-1"))
	created := agInt64(t, x.testEnv, "SELECT extract(epoch FROM created)::bigint FROM escrows WHERE order_id=$1", x.order)
	n := len(x.core.importRequests())
	x.restart()
	x.poll()
	if got := len(x.core.importRequests()); got != n {
		t.Fatalf("%d imports after a restart with the siblings watched", got-n)
	}
	if x.core.countWith("getaddressinfo", x.sibling(x.order, 1, 9)) == 0 {
		t.Fatal("the restart did not check the sibling coverage")
	}
	x.core.forgetWatched()
	x.restart()
	x.poll()
	reqs := x.core.importRequests()[n:]
	if len(reqs) != 32 {
		t.Fatalf("%d re-import requests, want the escrow's own descriptor, the two ranged ones and the 29 sibling addresses (QA5W3A-8)", len(reqs))
	}
	for i, r := range reqs {
		desc, _ := r["desc"].(string)
		_, ranged := r["range"]
		if r["timestamp"] != float64(created) || i == 0 && (r["label"] != "escrow:"+x.order || strings.HasPrefix(desc, "addr(")) ||
			(i == 1 || i == 2) != ranged || i > 2 && (!strings.HasPrefix(desc, "addr(") || r["label"] != "escrow:"+x.order) {
			t.Fatalf("re-import %d: %v", i, r)
		}
	}
	for _, s := range []struct{ branch, k int }{{0, 1}, {1, 9}} {
		x.core.mu.Lock()
		held := x.core.watched[x.sibling(x.order, s.branch, s.k)]
		x.core.mu.Unlock()
		if !held {
			t.Fatalf("sibling /%d/%d not watched again", s.branch, s.k)
		}
	}
	n = len(x.core.importRequests())
	x.restart()
	x.poll()
	x.poll()
	if got := len(x.core.importRequests()); got != n {
		t.Fatalf("the re-import ran again: %d more requests", got-n)
	}
	x.core.deposit(x.sibling(x.order, 1, 9), txidOf("restart-sib"), 0, 4000, 6)
	x.poll()
	if got := agStrings(t, x.testEnv, "SELECT escrow_left_out||':'||credited FROM payments WHERE order_id=$1", x.order); strings.Join(got, ",") != "true:false" {
		t.Fatalf("sibling deposit after restarts %v", got)
	}
}

// The restore floor (scripts/restore.sh's escrow-key-margin block, run as written, and the same statement in UPGRADING.md
// section 6) reads an account's highest index in both shapes: after a per-order escrow at /3/0/0 (and shape-(a) ones at
// /0/1 and /0/2) the account's next index is 3+1+50.
func TestEscrowShapeBRestoreFloor(t *testing.T) {
	x, _ := newShapeBEnv(t)
	for i := range 2 {
		x.checkoutAside("floor-a-" + strconv.Itoa(i))
	}
	agExec(x.testEnv, "UPDATE user_escrow_keys SET layout='per_order' WHERE user_id IN ($1,$2)", x.buyer, x.vendor)
	if r := x.escrowRows(x.checkoutAside("floor-b")); r["buyer"].suffix != "/3/0/0" {
		t.Fatalf("buyer suffix %q", r["buyer"].suffix)
	}
	block := restoreMarginSQL(t)
	if _, err := x.DB.Exec(block); err != nil {
		t.Fatal(err)
	}
	if got := agStrings(t, x.testEnv, "SELECT user_id||'='||next_index FROM escrow_index_floors WHERE user_id IN ($1,$2) ORDER BY user_id=$1 DESC", x.buyer, x.vendor); strings.Join(got, ",") != x.buyer+"=54,"+x.vendor+"=54" {
		t.Fatalf("floors %v, want 54 for both", got)
	}
	upgrading, err := os.ReadFile("UPGRADING.md")
	if err != nil {
		t.Fatal(err)
	}
	floor, _, _ := strings.Cut(block, "DO $$")
	if !strings.Contains(string(upgrading), floor) {
		t.Fatalf("UPGRADING.md section 6 lacks scripts/restore.sh's floor statement:\n%s", floor)
	}
}

// P4-R2: no per-order key served to a wallet (the account key's child <i>, or the signer's child <m>) is an ancestor of
// another key of the same origin: not of another escrow's key in either shape, nor of the account's receiving and change
// branches (/0, /1) a Bitcoin Core wallet of the same account uses, nor of the practice key.
func TestEscrowShapeBNoServedKeyIsAncestor(t *testing.T) {
	x, _ := newShapeBEnv(t)
	for i := range 2 {
		x.checkoutAside("anc-a-" + strconv.Itoa(i))
	}
	agExec(x.testEnv, "UPDATE user_escrow_keys SET layout='per_order' WHERE user_id IN ($1,$2)", x.buyer, x.vendor)
	for i := range 3 {
		x.checkoutAside("anc-b-" + strconv.Itoa(i))
	}
	type key struct{ fp, path, suffix string }
	var keys []key
	rows, err := x.DB.Query("SELECT origin_fingerprint,origin_path,derivation_suffix FROM escrow_pubkeys")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k key
		if err = rows.Scan(&k.fp, &k.path, &k.suffix); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k)
	}
	rows.Close()
	served := 0
	for _, s := range keys {
		if suffixLayout(s.suffix) != layoutPerOrder {
			continue
		}
		served++
		i := strings.Split(s.suffix, "/")[1]
		node := s.path + "/" + i + "/" // the served key's path, as a prefix of its descendants
		for _, other := range []string{s.path + "/0/0", s.path + "/0/1", s.path + "/1/0", s.path + "/2147483647/0/0"} {
			if strings.HasPrefix(other+"/", node) {
				t.Errorf("served per-order key %s%s is an ancestor of %s", s.fp, node, other)
			}
		}
		for _, o := range keys {
			if o == s || o.fp != s.fp {
				continue
			}
			if strings.HasPrefix(o.path+o.suffix+"/", node) {
				t.Errorf("served per-order key %s%s is an ancestor of escrow key %s%s%s", s.fp, node, o.fp, o.path, o.suffix)
			}
		}
	}
	if served != 9 {
		t.Fatalf("%d per-order keys checked, want 9", served)
	}
}

// Migration 089 (ADDENDUM QA4W5K-6, P4-R2, QA5W3A-11): a per-order derivation suffix is exactly /<i>/0/0 with
// 2 <= i < 2^31-1: /2147483648/0/0 and /2147483649/0/0 (aliases of /0h/0/0 and /1h/0/0), an i of 1 and, for the market
// too, the practice index are refused; no step of a shape-(a) suffix is hardened or reaches 2^31 either (/0/2147483648,
// /0/9999999999), for any party. The market's origin may be a rooted signer key's (no path), a party's may not. The
// constraint is validated.
func TestEscrowMigration089(t *testing.T) {
	e := newTestApp(t)
	a, _ := e.user("mig089_a", "buyer")
	b, _ := e.user("mig089_b", "vendor")
	order := e.order(a, e.product(b, "digital"), "BTC", "awaiting_payment")
	var id int64
	if err := e.DB.QueryRow("INSERT INTO escrows(order_id,address,market_index,refund_address,fee_bps,fee_destination) VALUES($1,'bcrt1qa089',nextval('escrow_market_index'),'bcrt1qr089',0,'') RETURNING id", order).Scan(&id); err != nil {
		t.Fatal(err)
	}
	try := func(party, origin, suffix string) error {
		tx, err := e.DB.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		var uid, h any
		if party != "market" {
			uid, h = a, strings.Repeat("ab", 32)
		}
		_, err = tx.Exec("INSERT INTO escrow_pubkeys(pubkey,escrow_id,party,user_id,key_hmac,origin_fingerprint,origin_path,derivation_suffix) VALUES('02'||repeat('89',32),$1,$2,$3,$4,'7450f935',$5,$6)",
			id, party, uid, h, origin, suffix)
		return err
	}
	const acct = "/48h/1h/0h/2h"
	for _, c := range []struct{ party, origin, suffix string }{
		{"buyer", acct, "/2147483648/0/0"}, {"vendor", acct, "/2147483649/0/0"}, {"market", "", "/2147483648/0/0"}, {"market", "", "/2147483649/0/0"},
		{"buyer", acct, "/9999999999/0/0"}, {"buyer", acct, "/1/0/0"}, {"market", "", "/1/0/0"},
		{"buyer", acct, "/2/0/1"}, {"buyer", acct, "/2/1/0"}, {"buyer", acct, "/2/0/0/0"}, {"buyer", acct, "/2h/0/0"}, {"buyer", acct, "/2/0/2147483648"},
		{"buyer", "", "/2/0/0"}, {"market", "", "/0/7"}, // no origin path: only the market's per-order key
		// QA5W3A-11: the market never takes the practice index; no step of any suffix, in either shape, reaches 2^31.
		{"market", "", "/2147483647/0/0"},
		{"buyer", acct, "/0/2147483648"}, {"vendor", acct, "/0/9999999999"}, {"market", "/84h/1h/0h", "/0/2147483648"}, {"market", "/84h/1h/0h", "/0/9999999999"},
		{"buyer", acct, "/2147483648/7"}, {"buyer", acct, "/0/7h"}, {"market", "/84h/1h/0h", "/0h/7"},
	} {
		if try(c.party, c.origin, c.suffix) == nil {
			t.Errorf("%s origin %q suffix %s accepted", c.party, c.origin, c.suffix)
		}
	}
	for _, c := range []struct{ party, origin, suffix string }{
		{"buyer", acct, "/2/0/0"}, {"vendor", acct, "/2147483646/0/0"}, {"market", "", "/2/0/0"}, {"market", "", "/2147483646/0/0"},
		{"buyer", acct, "/0/1"}, {"market", "/84h/1h/0h", "/0/0"}, // shape (a)
		{"vendor", acct, "/0/2147483647"}, {"market", "/84h/1h/0h", "/0/2147483647"},
	} {
		if err := try(c.party, c.origin, c.suffix); err != nil {
			t.Errorf("%s origin %q suffix %s refused: %v", c.party, c.origin, c.suffix, err)
		}
	}
	for _, name := range []string{"escrow_pubkeys_suffix_steps", "escrow_pubkeys_origin_path", "escrow_pubkeys_per_order_xpub"} {
		var valid bool
		if err := e.DB.QueryRow("SELECT convalidated FROM pg_constraint WHERE conname=$1 AND conrelid='escrow_pubkeys'::regclass", name).Scan(&valid); err != nil || !valid {
			t.Errorf("constraint %s: validated=%v %v", name, valid, err)
		}
	}
}

// Migration 089 on a database holding an escrow key row it refuses (a historical /2147483648/0/0, a shape-(a)
// /0/2147483648 or a market key at the practice index /2147483647/0/0, one at a time): it stops with a message naming
// the row's suffix, records nothing and changes nothing; with the row gone it applies.
func TestEscrowMigration089BadHistoricalRow(t *testing.T) {
	db := schemaDB(t)
	ctx := context.Background()
	list, err := loadMigrations(migrationFiles)
	if err != nil {
		t.Fatal(err)
	}
	upTo088 := slices.DeleteFunc(slices.Clone(list), func(m migration) bool { return m.Version >= 89 })
	runInTx(t, db, func(tx *sql.Tx) error {
		if _, err := tx.Exec(schema); err != nil {
			return err
		}
		return applyMigrations(ctx, tx, upTo088)
	})
	for _, q := range []string{
		"INSERT INTO users(id,handle,password_hash,role) VALUES('ub','up_buyer','x','buyer'),('uv','up_vendor','x','vendor')",
		"INSERT INTO products(id,vendor_id,title,description,category,region,kind,btc,xmr,stock) VALUES('pd','uv','D','D','Hardware','Worldwide','digital',100000,1,5)",
		"INSERT INTO orders(id,buyer_id,product_id,currency,amount,state) VALUES('o1','ub','pd','BTC',100000,'awaiting_payment')",
		"INSERT INTO escrows(order_id,address,market_index,refund_address,fee_bps,fee_destination) VALUES('o1','bcrt1qbad089',nextval('escrow_market_index'),'bcrt1qbadr089',0,'')",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// A per-order step of 2^31, a shape-(a) step of 2^31 (QA5W3A-11) and the market at the practice index (QA5W3A-11).
	for _, bad := range []struct{ party, uid, hmac, origin, suffix string }{
		{"buyer", "ub", strings.Repeat("a", 64), "/48h/1h/0h/2h", "/2147483648/0/0"},
		{"vendor", "uv", strings.Repeat("b", 64), "/48h/1h/0h/2h", "/0/2147483648"},
		{"market", "", "", "/84h/1h/0h", "/2147483647/0/0"}, // with a path: 074's origin rule, which 089 relaxes
	} {
		var uid, h any
		if bad.uid != "" {
			uid, h = bad.uid, bad.hmac
		}
		if _, err := db.Exec(`INSERT INTO escrow_pubkeys(pubkey,escrow_id,party,user_id,key_hmac,origin_fingerprint,origin_path,derivation_suffix)
			SELECT '02'||repeat('b8',32),id,$1,$2,$3,'a1b2c3d4',$4,$5 FROM escrows`, bad.party, uid, h, bad.origin, bad.suffix); err != nil {
			t.Fatalf("%s: %v", bad.suffix, err)
		}
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		err = applyMigrations(ctx, tx, list)
		tx.Rollback()
		if err == nil || !strings.Contains(err.Error(), "migration 089") || !strings.Contains(err.Error(), "(such as "+bad.suffix+")") || !strings.Contains(err.Error(), "UPGRADING.md section 44") {
			t.Fatalf("migration 089 over a bad row %s: %v", bad.suffix, err)
		}
		var n int
		if err = db.QueryRow("SELECT count(*) FROM schema_migrations WHERE version=89").Scan(&n); err != nil || n != 0 {
			t.Fatalf("089 recorded: %d %v", n, err)
		}
		if _, err = db.Exec("DELETE FROM escrow_pubkeys"); err != nil {
			t.Fatal(err)
		}
	}
	runInTx(t, db, func(tx *sql.Tx) error { return applyMigrations(ctx, tx, list) })
}

// Go's per-order origin check (checkEscrowOrigin) for shape (b): /<i>/0/0 with 2 <= i < 2^31-1, unhardened and
// canonical.
func TestEscrowPerOrderOrigin(t *testing.T) {
	k := mustParseKey(t, testBIP48Key)
	for suffix, ok := range map[string]bool{
		"/2/0/0": true, "/17/0/0": true, "/2147483646/0/0": true,
		"/0/0/0": false, "/1/0/0": false, "/2147483647/0/0": false, "/2147483648/0/0": false, "/2147483649/0/0": false,
		"/2/0/1": false, "/2/1/0": false, "/2h/0/0": false, "/02/0/0": false, "/2/0/0/0": false, "/2/0": false,
	} {
		if err := checkEscrowOrigin(k, k.PathText, suffix); (err == nil) != ok {
			t.Errorf("suffix %s: %v, want ok=%v", suffix, err, ok)
		}
	}
	if perOrderSuffix(5) != "/5/0/0" {
		t.Fatalf("perOrderSuffix(5) = %q", perOrderSuffix(5))
	}
}
