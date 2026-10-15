package market

import (
	"context"
	"encoding/binary"
	"encoding/hex"

	"slices"

	"testing"
)

// coSpend is a segwit transaction spending txid:vout to one output, as the parties' own co-spend would be, recorded
// in the fake node and the watch-only wallet with confs confirmations (its input spent).
func (x *escrowEnv) coSpend(txid string, vout uint32, confs int64) string {
	x.t.Helper()
	id, _ := hex.DecodeString(txid)
	slices.Reverse(id)
	raw := []byte{2, 0, 0, 0, 0, 1, 1}
	raw = append(raw, id...)
	raw = binary.LittleEndian.AppendUint32(raw, vout)
	raw = append(raw, 0, 0xfd, 0xff, 0xff, 0xff, 1)
	raw = binary.LittleEndian.AppendUint64(raw, 90000)
	raw = append(raw, 22, 0, 20)
	raw = append(raw, bytes32(7)[:20]...)
	raw = append(raw, 2, 0, 1, 1) // witness: two items
	raw = append(raw, 0, 0, 0, 0)
	t, err := parseSignedTx(raw)
	if err != nil {
		x.t.Fatal(err)
	}
	spender := t.TxID()
	x.core.mu.Lock()
	if x.core.broadcast == nil {
		x.core.broadcast, x.core.txConfs = map[string]string{}, map[string]int64{}
	}
	x.core.broadcast[spender], x.core.txConfs[spender] = hex.EncodeToString(raw), confs
	x.core.mu.Unlock()
	x.core.spend(txid, int64(vout))
	return spender
}

// The reconciliation RPCs: exact params, and replies read into fixed shapes (a malformed one is a bad-reply class).
func TestEscrowRPCReconciliation(t *testing.T) {
	ctx := context.Background()
	e, core, _ := testEscrowAdapter(t)
	core.deposit("bcrt1qwatcheda", txidOf("fund"), 1, 70000, 3)
	core.labels = map[string]string{"bcrt1qwatcheda": "escrow:abc", "bcrt1qemptyaddress0": "escrow:def"}
	ws, err := e.watchedAddresses(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if path, params := core.lastCall(t, "listreceivedbyaddress"); path != fakeWatchWallet || params != `[0,true,true]` {
		t.Fatalf("listreceivedbyaddress %s %s", path, params)
	}
	if len(ws) != 2 || ws[0] != (escrowWatched{"bcrt1qwatcheda", "escrow:abc", 100000}) || ws[1] != (escrowWatched{"bcrt1qemptyaddress0", "escrow:def", 0}) {
		t.Fatalf("watched %+v", ws)
	}
	x := &escrowEnv{t: t, core: core}
	spender := x.coSpend(txidOf("fund"), 1, 0)
	got, err := e.walletSpenders(ctx, map[string]bool{txidOf("fund") + ":1": true, txidOf("other") + ":0": true})
	if err != nil {
		t.Fatal(err)
	}
	if path, params := core.lastCall(t, "listtransactions"); path != fakeWatchWallet || params != `["*",100,0,true]` {
		t.Fatalf("listtransactions %s %s", path, params)
	}
	if path, params := core.lastCall(t, "gettransaction"); path != fakeWatchWallet || params != `["`+spender+`",true]` {
		t.Fatalf("gettransaction %s %s", path, params)
	}
	if len(got) != 1 || got[txidOf("fund")+":1"] != spender {
		t.Fatalf("spenders %v", got)
	}
	core.listTxErr = -1
	if _, err := e.walletSpenders(ctx, map[string]bool{"x:0": true}); errorClassOf(err) != "listtransactions rpc:-1" {
		t.Fatalf("listtransactions error class %q", errorClassOf(err))
	}
}
