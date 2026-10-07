package market

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"html"
	"maps"
	"net/url"
	"slices"
	"strings"
	"testing"

	"opsecmkt/internal/btc"
)

// QA5W3A's findings on A-456a (per-order escrow creation and watching): the sibling addresses' cross-check and their
// place among the addresses the market never reuses (ES-5), the signer's pins and key at checkout and at registration,
// and the watch-only wallet's repair and the orphan sweep after restores. DB integration with the simulated Core of
// escrow_fake_test.go and the fake signer: simulated RPC, not chain evidence.

// QA5W3A-5 (P4-R2's cross-check, the sibling addresses): Core must derive every address of the order wallet's ranged
// descriptors as Go does. A node that returns one wrong address in the receiving range (/0/5 here, a valid P2WSH address
// of other keys) gets the plain retry answer, and nothing is stored or imported.
func TestEscrowShapeBCrossCheckSibling(t *testing.T) {
	logs := captureLog(t)
	x, _ := newPerOrderEnv(t)
	other := sha256.Sum256([]byte("not this order wallet's /0/5"))
	x.core.mu.Lock()
	x.core.rangeSkew = func(body string, k int64, addr string) string {
		if k == 5 && strings.Contains(body, "/0/*") {
			return segwitAddress("bcrt", 0, other[:])
		}
		return addr
	}
	x.core.mu.Unlock()
	w := x.checkout(x.order, regtestAddr("sibling-skew"))
	if w.Code != 503 || !strings.Contains(w.Body.String(), "Nothing was changed; try again later.") {
		t.Fatalf("checkout with one wrong sibling address: %d %s", w.Code, w.Body.String())
	}
	x.nothingWritten(x.order)
	if n := agInt(x.testEnv, "SELECT count(*) FROM escrow_siblings"); n != 0 {
		t.Fatalf("%d sibling rows stored", n)
	}
	if n := x.core.count("importdescriptors"); n != 0 {
		t.Fatalf("%d imports after a wrong sibling address", n)
	}
	if !strings.Contains(logs.String(), "per-order-derivation-mismatch") {
		t.Fatalf("log: %s", logs.String())
	}
}

// QA5W3A-6 (P4-R17's pins, at checkout): a signer that answers with the pinned key's public key and fingerprint but
// another chain code (so other per-order keys) is not the pinned signer: the checkout gets the escrow signer refusal
// and nothing is stored or imported.
func TestEscrowShapeBSignerChainCodeChanged(t *testing.T) {
	x, f := newPerOrderEnv(t)
	pinned := signerPins(t, x.testEnv)
	f.set(func(f *fakeSigner) { f.key.ChainCode[7] ^= 0x10 })
	w := x.checkout(x.order, regtestAddr("signer-chaincode"))
	if body := html.UnescapeString(w.Body.String()); w.Code != 503 || !strings.Contains(body, "need the market's escrow signer") {
		t.Fatalf("checkout with the signer's chain code changed: %d %s", w.Code, body)
	}
	x.nothingWritten(x.order)
	if n := x.core.count("importdescriptors"); n != 0 {
		t.Fatalf("%d imports", n)
	}
	if got := signerPins(t, x.testEnv); !slices.Equal(got, pinned) {
		t.Fatalf("pins changed: %v", got)
	}
}

// siblingInUse is an env with one per-order escrow (whose siblings the market stores) and both parties' keys Bitcoin
// Core again, and that escrow's sibling /0/5.
func siblingInUse(t *testing.T) (*escrowEnv, string) {
	t.Helper()
	x, _ := newPerOrderEnv(t)
	x.mustCheckout(x.order, regtestAddr("sibling-owner"))
	sib := x.sibling(x.order, 0, 5)
	agExec(x.testEnv, "UPDATE user_escrow_keys SET layout='core' WHERE user_id IN ($1,$2)", x.buyer, x.vendor)
	return x, sib
}

// QA5W3A-4 (ES-5): another escrow's sibling address is an address the market uses, so it is never pinned as a refund
// address: 409, nothing written.
func TestEscrowShapeBSiblingNotRefundAddress(t *testing.T) {
	x, sib := siblingInUse(t)
	o := x.draft()
	if w := x.checkout(o, sib); w.Code != 409 || !strings.Contains(w.Body.String(), "already used on this market") {
		t.Fatalf("a sibling as the refund address: %d %s", w.Code, w.Body.String())
	}
	x.nothingWritten(o)
	x.mustCheckout(o, regtestAddr("sibling-fresh-refund"))
}

// QA5W3A-4 (ES-5): nor as a vendor's claim (release) address: 409, nothing requested.
func TestEscrowShapeBSiblingNotClaimAddress(t *testing.T) {
	x, sib := siblingInUse(t)
	order := x.completed(regtestAddr("sibling-claim-refund"), 100000)
	if code, body := x.claim(order, x.vendorSess, sib); code != 409 || !strings.Contains(body, "already used on this market") {
		t.Fatalf("a sibling as the claim address: %d %s", code, body)
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM settlements"); n != 0 {
		t.Fatalf("%d settlements after the refusal", n)
	}
	if code, body := x.claim(order, x.vendorSess, claimAddr); code != 303 {
		t.Fatalf("a fresh address: %d %s", code, body)
	}
}

// QA5W3A-4 (ES-5): nor as the market fee's destination: 409, nothing saved.
func TestEscrowShapeBSiblingNotFeeAddress(t *testing.T) {
	x, sib := siblingInUse(t)
	_, admin := x.user("esibadmin_"+randomToken()[:6], "admin")
	w := x.do("POST", "/admin/escrow-fee", admin, url.Values{"fee_bps": {"250"}, "fee_address": {sib}, "password": {testPassword}})
	if w.Code != 409 || !strings.Contains(w.Body.String(), "already used on this market") {
		t.Fatalf("a sibling as the fee address: %d %s", w.Code, w.Body.String())
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM settings WHERE key LIKE 'escrow_btc_fee_%'"); n != 0 {
		t.Fatalf("%d fee settings saved", n)
	}
}

// signerAsAccount is the escrow signer's rooted account key re-serialized as a BIP48 account key at depth 4
// (/48h/1h/0h/2h) under the declared fingerprint fp: the same public key and chain code, so the same children.
func signerAsAccount(k btc.ExtKey, fp string) string {
	k.Depth, k.ParentFP, k.Child = 4, [4]byte{0xde, 0xad, 0xbe, 0xef}, 0x80000002
	return "[" + fp + "/48h/1h/0h/2h]" + k.Serialize(btc.VersionTpub)
}

// declaringSignerFP is a BIP48 account key no account holds (testBIP48Key with its chain code changed) declaring the
// signer's fingerprint as its origin.
func declaringSignerFP(t *testing.T, k btc.ExtKey) string {
	fp := k.Fingerprint()
	return altEscrowKey(t, testBIP48Key, hex.EncodeToString(fp[:]), 7)
}

// QA5W3A-7 (defence in depth): a party key must never be the escrow signer's, whose seed would then hold two of an
// escrow's three keys. Registration refuses, in either layout, the signer's key re-serialized at depth 4 under another
// fingerprint (its HMAC, computed as the pin's, equals the pinned one) and any key declaring the signer's pinned
// fingerprint, with nothing saved; another key registers.
func TestEscrowSignerKeyRefusedAtRegistration(t *testing.T) {
	x, f := newPerOrderEnv(t)
	for _, layout := range []string{layoutCore, layoutPerOrder} {
		for name, key := range map[string]string{
			"the signer's key at depth 4":              signerAsAccount(f.key, "0badc0de"),
			"a key declaring the signer's fingerprint": declaringSignerFP(t, f.key),
		} {
			uid, sess := x.user("esigkey_"+randomToken()[:6], "buyer")
			code, body := registerKey(x.testEnv, sess, key, "layout", layout)
			if code != 400 || !strings.Contains(body, "cannot be used here") {
				t.Errorf("%s, layout %s: %d %s", name, layout, code, body)
			}
			if n := agInt(x.testEnv, "SELECT count(*) FROM user_escrow_keys WHERE user_id=$1", uid); n != 0 {
				t.Errorf("%s, layout %s: key saved", name, layout)
			}
		}
	}
	_, sess := x.user("esigok_"+randomToken()[:6], "buyer")
	if code, body := registerKey(x.testEnv, sess, altEscrowKey(t, testBIP48Key, "0badc0de", 9), "layout", layoutPerOrder); code != 303 {
		t.Fatalf("another key: %d %s", code, body)
	}
}

// QA5W3A-7: a party key that is the signer's, registered before the signer was pinned (or before this check), is refused
// at checkout with a 409 naming the problem (not the generic retry answer), for the buyer and for the vendor, with nothing
// written or imported.
func TestEscrowShapeBSignerKeyAtCheckout(t *testing.T) {
	x, f := newPerOrderEnv(t)
	for _, c := range []struct {
		name, uid, key, want string
	}{
		{"buyer: the signer's key at depth 4", x.buyer, signerAsAccount(f.key, "0badc0de"), "Your Bitcoin escrow key is the market's own escrow signer key"},
		{"vendor: the signer's key at depth 4", x.vendor, signerAsAccount(f.key, "0badc0de"), "The vendor's Bitcoin escrow key is the market's own escrow signer key"},
		{"buyer: the signer's fingerprint declared", x.buyer, declaringSignerFP(t, f.key), "Your Bitcoin escrow key is the market's own escrow signer key"},
	} {
		x.activeKey(x.buyer, x.buyerKey)
		x.activeKey(x.vendor, x.vendorKey)
		x.activeKey(c.uid, c.key)
		o := x.draft()
		w := x.checkout(o, regtestAddr("signer-party-"+c.name))
		if body := html.UnescapeString(w.Body.String()); w.Code != 409 || !strings.Contains(body, c.want) || !strings.Contains(body, "Nothing was changed") {
			t.Errorf("%s: %d %s", c.name, w.Code, body)
		}
		x.nothingWritten(o)
	}
	if n := x.core.count("importdescriptors"); n != 0 {
		t.Fatalf("%d imports", n)
	}
}

// QA5W3A-3 (the orphan sweep after a restore) with QA5W3A-8 (the watch-only wallet's repair): a per-order escrow whose
// watch-only wallet was restored from before it, repaired on the next start while the signer did not answer (so its
// siblings came back as addr(…) imports only, unsolvable on Core), is then lost by a database restore. The sweep reserves
// the escrow address's three keys, finds nothing to reserve at the unsolvable siblings, and finishes: the restore's
// marker is cleared and a Bitcoin Core escrow can be created again. A second per-order escrow, lost with its ranged
// descriptors in place (its siblings solvable), is swept too.
func TestEscrowShapeBLostAfterWatchRestore(t *testing.T) {
	x, f := newPerOrderEnv(t)
	addr := x.mustCheckout(x.order, regtestAddr("lost-watch-1"))
	lost := x.escrowPubkeys(x.order)
	n := len(x.core.importRequests())
	x.core.forgetWatched()
	f.set(func(f *fakeSigner) { f.raw = `{"error":"unavailable"}` })
	x.restart()
	x.poll()
	f.set(func(f *fakeSigner) { f.raw = "" })
	reqs := x.core.importRequests()[n:]
	if len(reqs) != 30 || reqs[0]["label"] != "escrow:"+x.order || strings.HasPrefix(reqs[0]["desc"].(string), "addr(") {
		t.Fatalf("repair without the signer: %d requests, want the escrow's own descriptor and its 29 siblings: %v", len(reqs), reqs)
	}
	for _, r := range reqs[1:] {
		if !strings.HasPrefix(r["desc"].(string), "addr(") {
			t.Fatalf("repair without the signer imported %v", r)
		}
	}
	last, called := x.marketCounter()
	x.lose(last, called, x.order)
	x.poll()
	if x.sweepRequired() {
		t.Fatal("the sweep did not finish: escrow_sweep_required is still set")
	}
	if got := agStrings(t, x.testEnv, "SELECT pubkey FROM escrow_reserved_keys WHERE address=$1 ORDER BY pubkey", addr); !slices.Equal(got, lost) {
		t.Fatalf("reserved %v, want the lost escrow's keys %v", got, lost)
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM escrow_reserved_keys WHERE address<>$1", addr); n != 0 {
		t.Fatalf("%d keys reserved at unsolvable sibling addresses", n)
	}
	if n := agInt(x.testEnv, "SELECT count(*) FROM escrow_orphans"); n != 30 {
		t.Fatalf("%d orphans listed, want the escrow address and its 29 siblings", n)
	}

	second := x.draft()
	addr2 := x.mustCheckout(second, regtestAddr("lost-watch-2"))
	lost2 := x.escrowPubkeys(second)
	last, called = x.marketCounter()
	x.lose(last, called, second)
	x.poll()
	if x.sweepRequired() {
		t.Fatal("the sweep did not finish over solvable siblings")
	}
	if got := agStrings(t, x.testEnv, "SELECT pubkey FROM escrow_reserved_keys WHERE address=$1 ORDER BY pubkey", addr2); !slices.Equal(got, lost2) {
		t.Fatalf("reserved %v, want %v", got, lost2)
	}

	agExec(x.testEnv, "UPDATE user_escrow_keys SET layout='core' WHERE user_id IN ($1,$2)", x.buyer, x.vendor)
	x.mustCheckout(x.draft(), regtestAddr("lost-watch-core"))
}

// QA5W3A-8: a watch-only wallet restored from before a per-order escrow is repaired on the next start with everything
// checkout imported: the escrow's own labelled bare-key descriptor (rebuilt from its stored keys), the order wallet's two
// ranged descriptors (rebuilt from the parties' account keys and the signer's, each checked against the stored keys) and
// the 29 labelled sibling addresses, in checkout's order and from the escrow's creation time; a deposit made to the
// escrow address meanwhile is seen and credited, and one to a sibling is recorded left out.
func TestEscrowShapeBWatchRestoredBeforeEscrow(t *testing.T) {
	x, _ := newPerOrderEnv(t)
	addr := x.mustCheckout(x.order, regtestAddr("restored-1"))
	created := agInt64(t, x.testEnv, "SELECT extract(epoch FROM created)::bigint FROM escrows WHERE order_id=$1", x.order)
	first := x.core.importRequests()
	price := agInt64(t, x.testEnv, "SELECT amount FROM orders WHERE id=$1", x.order)
	x.poll() // this process's check: the wallet holds everything
	x.core.forgetWatched()
	x.core.deposit(addr, txidOf("restored-main"), 0, price, 6)
	sib := x.sibling(x.order, 0, 3)
	x.core.deposit(sib, txidOf("restored-sib"), 0, 3000, 6)
	x.poll() // the same process, already checked: the wallet lists neither deposit
	if n := agInt(x.testEnv, "SELECT count(*) FROM payments WHERE order_id=$1", x.order); n != 0 {
		t.Fatalf("%d payments seen by a wallet that lost the escrow", n)
	}
	x.restart()
	x.poll()
	reqs := x.core.importRequests()[len(first):]
	if len(reqs) != 32 {
		t.Fatalf("%d repair requests, want the escrow's descriptor, two ranged ones and 29 siblings: %v", len(reqs), reqs)
	}
	for i, r := range reqs {
		want := maps.Clone(first[i])
		want["timestamp"] = float64(created)
		if !mapsEqualJSON(r, want) {
			t.Fatalf("repair request %d %v, want checkout's %v from the escrow's creation time", i, r, want)
		}
	}
	x.core.mu.Lock()
	label, solvable := x.core.labels[addr], x.core.rangedPubkeys[sib] != nil
	x.core.mu.Unlock()
	if label != "escrow:"+x.order || !solvable {
		t.Fatalf("escrow address label %q, sibling solvable %v", label, solvable)
	}
	if got := agStrings(t, x.testEnv, "SELECT (address=$2)||':'||credited||':'||escrow_left_out FROM payments WHERE order_id=$1 ORDER BY amount", x.order, addr); strings.Join(got, ",") != "false:false:true,true:true:false" {
		t.Fatalf("payments after the repair %v", got)
	}
}

// QA5W3A-8: the ranged descriptors are rebuilt only from keys that still derive the escrow's stored keys. After the
// vendor registered another key, a watch-only wallet restored from before the escrow gets the escrow's descriptor and
// its 29 sibling addresses again, never ranged descriptors of the vendor's new key, and the log says so.
func TestEscrowShapeBRepairKeyChanged(t *testing.T) {
	logs := captureLog(t)
	x, _ := newPerOrderEnv(t)
	x.mustCheckout(x.order, regtestAddr("repair-key-changed"))
	n := len(x.core.importRequests())
	x.activeKey(x.vendor, altEscrowKey(t, x.vendorKey, "5eed5eed", 3))
	x.core.forgetWatched()
	x.restart()
	x.poll()
	reqs := x.core.importRequests()[n:]
	if len(reqs) != 30 {
		t.Fatalf("%d repair requests, want the escrow's descriptor and its 29 siblings: %v", len(reqs), reqs)
	}
	for _, r := range reqs {
		if _, ranged := r["range"]; ranged {
			t.Fatalf("a ranged descriptor was imported after the vendor's key changed: %v", r)
		}
	}
	if !strings.Contains(logs.String(), "ranged descriptors of 1 of them were not imported again") {
		t.Fatalf("log: %s", logs.String())
	}
}

// mapsEqualJSON compares two decoded JSON objects (import requests) by their JSON text.
func mapsEqualJSON(a, b map[string]any) bool {
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(ja) == string(jb)
}
