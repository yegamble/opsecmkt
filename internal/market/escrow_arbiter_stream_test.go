package market

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// coreArbiterEntry is one listsinceblock entry in Bitcoin Core 31.1's shape for a payment the arbiter wallet received
// that its payer replaced by fee bumps (RBF): walletconflicts names each other replacement Core keeps, so an entry
// grows by about 67 bytes per bump (QA4W6-1). label pads it with one string token of that many bytes.
func coreArbiterEntry(n int, category string, conflicts int, label string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `{"involvesWatchonly":true,"address":"bcrt1qjlxd2f5kejv5aq6w4djsy3j28nnr3adhvd8r2y","parent_descs":["wpkh([d34db33f/84h/1h/0h]tpubD6NzVbkrYhZ4X/0/*)#abcdefgh"],"category":%q,"amount":0.00010000,"label":%q,"vout":0,"abandoned":false,"confirmations":0,"trusted":false,"txid":"%064x","wtxid":"%064x","walletconflicts":[`,
		category, label, n, n+1)
	for i := range conflicts {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"%064x"`, 1<<40+i)
	}
	fmt.Fprintf(&b, `],"mempoolconflicts":[],"replaced_by_txid":"%064x","time":1759800000,"timereceived":1759800000,"bip125-replaceable":"yes"}`, n+2)
	return b.String()
}

// QA4W6-1: rpcStreamMaxValue bounds one JSON token of the arbiter wallet's listsinceblock answer, never a whole entry.
// A buyer who pays the market child's address (the descriptor on the order page shows it) and bumps the fee about 1000
// times made Core list entries whose walletconflicts passed 64 KiB; read whole, every entry was refused as a bad reply,
// at every start and retry, and escrow stayed off. Here, against a fake Core: entries with 5000 walletconflicts each
// (about 335 KB) are read, and a "send" listed after them, or named before its walletconflicts, is still found; an
// object or a list under an unknown key is skipped token by token. One string token past the bound is still refused as
// bad-reply (QA4W5-2's protection). QA4W6-6: the bound is exact: a token of rpcStreamMaxValue bytes (its quotes
// included) passes, one byte more is refused, whether it is an entry's value, a skipped key's value or an object key
// after another (valueBoundReader: a token that opens its object or list has one byte more). Unit test (no database).
func TestEscrowArbiterEntriesStreamByToken(t *testing.T) {
	var body atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"result":` + body.Load().(string) + `,"error":null,"id":"opsecmkt"}`))
	}))
	defer srv.Close()
	e, err := newEscrowBTC(escrowConfig{rpcURL: strings.Replace(srv.URL, "http://", "http://u:p@", 1), arbiterWallet: "opsecmkt-arbiter", watchWallet: "w"})
	if err != nil {
		t.Fatal(err)
	}
	tip := fmt.Sprintf("%064x", 7)
	answer := func(entries ...string) string {
		return `{"transactions":[` + strings.Join(entries, ",") + `],"lastblock":"` + tip + `"}`
	}
	read := func(name, result string, wantSent bool, wantClass string) {
		t.Helper()
		body.Store(result)
		block, sent, err := e.arbiterSends(context.Background(), "")
		var ee *escrowRPCError
		switch {
		case wantClass == "" && err != nil:
			t.Errorf("%s: %v, want a read", name, err)
		case wantClass == "" && (block != tip || sent != wantSent):
			t.Errorf("%s: block %q sent %v, want %q %v", name, block, sent, tip, wantSent)
		case wantClass != "" && (!errors.As(err, &ee) || ee.Class != wantClass):
			t.Errorf("%s: %v, want %s", name, err, wantClass)
		}
	}
	bumps := make([]string, 0, 21)
	for i := range 20 {
		bumps = append(bumps, coreArbiterEntry(i, "receive", 5000, ""))
	}
	if n := len(bumps[0]); n < 5*rpcStreamMaxValue {
		t.Fatalf("an entry of %d bytes does not exceed the bound", n)
	}
	read("20 entries of 5000 walletconflicts", answer(bumps...), false, "")
	read("a send after them", answer(append(bumps, coreArbiterEntry(99, "send", 0, ""))...), true, "")
	// category named after a 5000-entry list in the same entry, and before one
	tail := `{"walletconflicts":[` + strings.Repeat(`"`+tip+`",`, 4999) + `"` + tip + `"],"category":"send"}`
	head := `{"category":"send","walletconflicts":[` + strings.Repeat(`"`+tip+`",`, 4999) + `"` + tip + `"]}`
	read("send named after its walletconflicts", answer(bumps[0], tail), true, "")
	read("send named before its walletconflicts", answer(head, bumps[1]), true, "")
	nested := `{"category":"receive","x":{"a":[1,{"b":[` + strings.Repeat(`"`+tip+`",`, 3000) + `null]}],"c":true},"y":-1.5e3}`
	read("nested unknown values", answer(nested), false, "")
	unknownTop := `{"removed":[` + strings.Join(bumps[:3], ",") + `],"transactions":[],"lastblock":"` + tip + `"}`
	read("an unknown top-level list of large entries", unknownTop, false, "")
	read("a 70 KB string token", answer(coreArbiterEntry(0, "receive", 0, strings.Repeat("x", 70_000))), false, "bad-reply")
	read("a 70 KB category", answer(`{"category":"`+strings.Repeat("s", 70_000)+`"}`), false, "bad-reply")
	read("a non-string category", answer(`{"category":{"a":1}}`), false, "bad-reply")
	read("an entry that is not an object", answer(`"receive"`), false, "bad-reply")

	// QA4W6-6: the bound, exactly: a string token of rpcStreamMaxValue bytes (quotes included) passes; one more is refused.
	at, past := strings.Repeat("x", rpcStreamMaxValue-2), strings.Repeat("x", rpcStreamMaxValue-1)
	read("a label token of exactly the bound", answer(coreArbiterEntry(0, "receive", 0, at)), false, "")
	read("a label token one byte past the bound", answer(coreArbiterEntry(0, "receive", 0, past)), false, "bad-reply")
	read("a skipped top-level token of exactly the bound", `{"junk":"`+at+`","transactions":[],"lastblock":"`+tip+`"}`, false, "")
	read("a skipped top-level token one byte past the bound", `{"junk":"`+past+`","transactions":[],"lastblock":"`+tip+`"}`, false, "bad-reply")
	read("an entry key of exactly the bound", answer(`{"category":"send","`+at+`":1}`), true, "")
	read("an entry key one byte past the bound", answer(`{"category":"send","`+past+`":1}`), false, "bad-reply")
}
