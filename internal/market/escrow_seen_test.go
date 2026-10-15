package market

import (
	"encoding/hex"
	"slices"
	"strconv"
	"strings"
)

// partiesBroadcast puts the settlement's final transaction, finalized from the given signatures on every input, in the
// fake node and wallet with confs confirmations, its inputs spent: what the parties' own broadcast would do.
func (x *escrowEnv) partiesBroadcast(id int64, confs int64, sigs ...partySigs) (txid, raw string) {
	x.t.Helper()
	var stored string
	if err := x.DB.QueryRow("SELECT psbt FROM settlements WHERE id=$1", id).Scan(&stored); err != nil {
		x.t.Fatal(err)
	}
	sp, err := decodePSBT(stored)
	if err != nil {
		x.t.Fatal(err)
	}
	both, err := withPartySigs(sp, sigs...)
	if err != nil {
		x.t.Fatal(err)
	}
	pk, err := decodePSBT(both)
	if err != nil {
		x.t.Fatal(err)
	}
	raw, ok := fakeFinalTx(pk)
	if !ok {
		x.t.Fatal("the signatures do not finalize")
	}
	b, _ := hex.DecodeString(raw)
	final, err := parseSignedTx(b)
	if err != nil {
		x.t.Fatal(err)
	}
	txid = final.TxID()
	x.core.mu.Lock()
	defer x.core.mu.Unlock()
	if x.core.broadcast == nil {
		x.core.broadcast, x.core.txConfs = map[string]string{}, map[string]int64{}
	}
	if x.core.spent == nil {
		x.core.spent = map[string]bool{}
	}
	x.core.broadcast[txid], x.core.txConfs[txid] = raw, confs
	for _, in := range final.In {
		id := in.TxID
		slices.Reverse(id[:])
		x.core.spent[hex.EncodeToString(id[:])+":"+strconv.FormatInt(int64(in.Vout), 10)] = true
	}
	return txid, raw
}

// storedSigs reads party's stored signatures on settlement id.
func (x *escrowEnv) storedSigs(id int64, party string) partySigs {
	x.t.Helper()
	p := partySigs{Party: party}
	for _, r := range agStrings(x.t, x.testEnv, "SELECT pubkey||' '||signature FROM settlement_signatures WHERE settlement_id=$1 AND party=$2 ORDER BY input_n", id, party) {
		f := strings.Fields(r)
		pub, _ := hex.DecodeString(f[0])
		sig, _ := hex.DecodeString(f[1])
		p.Sigs = append(p.Sigs, psbtSig{pub, sig})
	}
	return p
}
