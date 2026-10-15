package market

import (
	"crypto/sha256"

	"strings"
	"testing"
	"time"
)

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
