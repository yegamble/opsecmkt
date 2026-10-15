package market

import (
	"context"
	"encoding/hex"
)

// sigsOf is party's good signature, as its own wallet would make it, on every input of settlement id.
func (x *escrowEnv) sigsOf(order string, id int64, party string) partySigs {
	x.t.Helper()
	var stored string
	if err := x.DB.QueryRow("SELECT psbt FROM settlements WHERE id=$1", id).Scan(&stored); err != nil {
		x.t.Fatal(err)
	}
	sp, err := decodePSBT(stored)
	if err != nil {
		x.t.Fatal(err)
	}
	pub, _ := hex.DecodeString(x.keyHex(order, party))
	p := partySigs{Party: party}
	for range sp.Inputs {
		p.Sigs = append(p.Sigs, psbtSig{pub, fakeSig(pub, sp.TxID(), 1)})
	}
	return p
}

// partyCopyAny is party's copy of settlement id as the stored PSBT gives it, whatever the settlement's state (the
// download serves only a settlement awaiting a signature).
func (x *escrowEnv) partyCopyAny(order string, id int64, party string) string {
	x.t.Helper()
	var stored string
	if err := x.DB.QueryRow("SELECT psbt FROM settlements WHERE id=$1", id).Scan(&stored); err != nil {
		x.t.Fatal(err)
	}
	keys, err := escrowKeys(context.Background(), x.DB, order)
	if err != nil {
		x.t.Fatal(err)
	}
	cp, err := settlementCopy(stored, keys[party])
	if err != nil {
		x.t.Fatal(err)
	}
	return cp
}
