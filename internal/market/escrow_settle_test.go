package market

import (
	"crypto/sha256"
	"encoding/hex"

	"net/url"

	"strconv"
	"strings"

	"testing"
)

// txidOf is a 64-hex transaction ID for label, as Core prints one.
func txidOf(label string) string {
	h := sha256.Sum256([]byte("tx:" + label))
	return hex.EncodeToString(h[:])
}

// fund pays order's escrow with one output per amount, all at confs confirmations, and runs a watcher pass.
func (x *escrowEnv) fund(order, label string, confs int64, sats ...int64) string {
	x.t.Helper()
	var addr string
	if err := x.DB.QueryRow("SELECT address FROM escrows WHERE order_id=$1", order).Scan(&addr); err != nil {
		x.t.Fatal(err)
	}
	for i, s := range sats {
		x.core.deposit(addr, txidOf(label), int64(i), s, confs)
	}
	x.poll()
	return addr
}

// completed is a new escrow order, funded with sats and completed on funding.
func (x *escrowEnv) completed(refund string, sats ...int64) string {
	x.t.Helper()
	order := x.draft()
	x.mustCheckout(order, refund)
	x.fund(order, order, 3, sats...)
	if x.state(order) != stateCompleted {
		x.t.Fatalf("order %s after funding", x.state(order))
	}
	return order
}

func (x *escrowEnv) claim(order, sess, addr string) (int, string) {
	w := x.do("POST", "/escrow/claim", sess, url.Values{"order_id": {order}, "address": {addr}, "password": {testPassword}})
	return w.Code, w.Body.String()
}

// settlement is order's release or refund row: id, kind, state, build_error.
func (x *escrowEnv) settlement(order string) (id int64, kind, state, buildErr string) {
	x.t.Helper()
	if err := x.DB.QueryRow("SELECT id,kind,state,build_error FROM settlements WHERE order_id=$1 AND kind IN ('release','refund')", order).Scan(&id, &kind, &state, &buildErr); err != nil {
		x.t.Fatalf("settlement of %s: %v", order, err)
	}
	return
}

// partyCopy downloads the party's copy of the settlement awaiting a signature.
func (x *escrowEnv) partyCopy(order, sess string, id int64) string {
	x.t.Helper()
	w := x.do("GET", "/escrow-psbt?id="+order+"&s="+strconv.FormatInt(id, 10), sess, nil)
	x.check(w, 200)
	return strings.TrimSpace(w.Body.String())
}

// walletSign is a party's wallet in the fake: a signature (sighash sh) on every input from the key whose derivation
// the copy carries at fingerprint fp.
func walletSign(t *testing.T, copyB64, fp string, sh byte) string {
	t.Helper()
	p, err := decodePSBT(copyB64)
	if err != nil {
		t.Fatal(err)
	}
	raw := p.Raw
	for i, in := range p.Inputs {
		var pub []byte
		for _, d := range in.Derivations {
			if hex.EncodeToString(d.Fingerprint[:]) == fp {
				pub = d.PubKey
			}
		}
		if pub == nil {
			t.Fatalf("input %d carries no derivation at %s", i, fp)
		}
		raw = addField(t, raw, i, append([]byte{0x02}, pub...), fakeSig(pub, p.TxID(), sh))
	}
	return encodePSBT(raw)
}

// addField adds one field to input i of a raw PSBT.
func addField(t *testing.T, raw []byte, i int, key, value []byte) []byte {
	t.Helper()
	p, err := parsePSBT(raw)
	if err != nil {
		t.Fatal(err)
	}
	out, err := p.withInputField(i, key, value)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (x *escrowEnv) sign(order, sess string, id int64, psbt string) (int, string) {
	w := x.do("POST", "/escrow/sign", sess, url.Values{"order_id": {order}, "settlement_id": {strconv.FormatInt(id, 10)}, "psbt": {psbt}})
	return w.Code, refusalText(w.Body.String())
}

func (x *escrowEnv) keyHex(order, party string) string {
	x.t.Helper()
	var pub string
	if err := x.DB.QueryRow("SELECT p.pubkey FROM escrow_pubkeys p JOIN escrows e ON e.id=p.escrow_id WHERE e.order_id=$1 AND p.party=$2", order, party).Scan(&pub); err != nil {
		x.t.Fatal(err)
	}
	return pub
}

const (
	claimAddr  = "bcrt1qvendorclaim000000000000000000000001"
	refundAddr = "bcrt1qbuyerrefund00000000000000000000001"
)
